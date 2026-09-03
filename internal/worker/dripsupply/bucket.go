package dripsupply

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brandident"
	"github.com/lib/pq"
)

// NoLimit marks a governor ceiling (or a plan/supply term) that does not bind.
// It is deliberately negative so a forgotten zero can never read as "unbounded".
const NoLimit = -1

// dayOf truncates an instant to midnight in ITS OWN location. Like contracts.go's
// dayWindow, this package never loads tzdata and never guesses which day the
// caller meant: pass a Denver-anchored day and every boundary here is Denver's.
func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// dayKey renders a day for a `$n::date` bind. Passing a time.Time as timestamptz
// and letting Postgres cast it to date resolves in the SESSION time zone, so a
// Denver midnight becomes the PREVIOUS day on a UTC server. Every day parameter
// in this package goes through this function.
func dayKey(day time.Time) string { return dayOf(day).Format("2006-01-02") }

// -----------------------------------------------------------------------------
// Window — the domain contract's pacing shape (§1.1 drip_domain_contracts)
// -----------------------------------------------------------------------------

// Window is active_window_start / active_window_end / interval_minutes /
// max_burst_intervals from a domain contract, as offsets from local midnight.
type Window struct {
	Start             time.Duration // offset from local midnight, e.g. 1h
	End               time.Duration // offset from local midnight, e.g. 20h
	Interval          time.Duration // e.g. 15m
	MaxBurstIntervals int           // e.g. 2
}

// DefaultWindow is the §1.1 default: 01:00–20:00, 15-minute intervals, 2-interval burst.
func DefaultWindow() Window {
	return Window{Start: time.Hour, End: 20 * time.Hour, Interval: 15 * time.Minute, MaxBurstIntervals: 2}
}

// WindowOf reads the pacing shape off an active domain contract. It is the only
// place the contract's "HH:MM" strings become durations.
func WindowOf(c *DomainContract) (Window, error) {
	if c == nil {
		return Window{}, errors.New("dripsupply: WindowOf called with a nil domain contract")
	}
	startMin, err := parseClock(c.ActiveWindowStart)
	if err != nil {
		return Window{}, fmt.Errorf("dripsupply: domain %s active_window_start: %w", c.SendingDomain, err)
	}
	endMin, err := parseClock(c.ActiveWindowEnd)
	if err != nil {
		return Window{}, fmt.Errorf("dripsupply: domain %s active_window_end: %w", c.SendingDomain, err)
	}
	w := Window{
		Start:             time.Duration(startMin) * time.Minute,
		End:               time.Duration(endMin) * time.Minute,
		Interval:          time.Duration(c.IntervalMinutes) * time.Minute,
		MaxBurstIntervals: c.MaxBurstIntervals,
	}
	if err := w.Validate(); err != nil {
		return Window{}, fmt.Errorf("dripsupply: domain %s: %w", c.SendingDomain, err)
	}
	return w, nil
}

// ActiveIntervals is (end-start)/interval — 76 for the 01:00–20:00 @ 15m default.
// Never returns 0: a zero would turn the refill divisor into +Inf tokens.
func (w Window) ActiveIntervals() int {
	if w.Interval <= 0 || w.End <= w.Start {
		return 1
	}
	n := int((w.End - w.Start) / w.Interval)
	if n < 1 {
		return 1
	}
	return n
}

// Hours is the length of the active window, used to turn a per-hour governor
// rate into a per-day ceiling.
func (w Window) Hours() float64 {
	if w.End <= w.Start {
		return 0
	}
	return (w.End - w.Start).Hours()
}

// BurstIntervals clamps max_burst_intervals to at least 1: a 0 would make the
// token cap 0 and wedge the domain silently for the whole day.
func (w Window) BurstIntervals() int {
	if w.MaxBurstIntervals < 1 {
		return 1
	}
	return w.MaxBurstIntervals
}

// Bounds returns the [start, end) instants of the window on a day, in the day's
// own location.
func (w Window) Bounds(day time.Time) (time.Time, time.Time) {
	d := dayOf(day)
	return d.Add(w.Start), d.Add(w.End)
}

// Contains reports whether now is inside the day's active window, [start, end).
func (w Window) Contains(day, now time.Time) bool {
	start, end := w.Bounds(day)
	t := now.In(day.Location())
	return !t.Before(start) && t.Before(end)
}

// Validate rejects a window that cannot pace anything.
func (w Window) Validate() error {
	if w.Interval <= 0 {
		return fmt.Errorf("window interval must be > 0, got %s", w.Interval)
	}
	if w.End <= w.Start {
		return fmt.Errorf("window end (%s) must be after start (%s)", w.End, w.Start)
	}
	if w.MaxBurstIntervals < 1 {
		return fmt.Errorf("max_burst_intervals must be >= 1, got %d", w.MaxBurstIntervals)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Governors — read, never stored; they REDUCE effective capacity, never raise it
// -----------------------------------------------------------------------------

// GovernorCeiling is one governor's opinion of a domain×ISP daily ceiling.
// Limit == NoLimit means "does not bind". Limit == 0 means STOP.
type GovernorCeiling struct {
	Name  string
	Limit int
}

// GovernorReader yields the ceilings that apply to one domain×ISP on one day.
// §2.3: governors reduce; a governor value above the contract is ignored.
type GovernorReader interface {
	Ceilings(ctx context.Context, day time.Time, domain, isp string, w Window) ([]GovernorCeiling, error)
}

// Governors composes readers. A reader that errors is NOT silently dropped: the
// error is returned so the caller can fail closed rather than mail at contract.
type Governors []GovernorReader

func (g Governors) Ceilings(ctx context.Context, day time.Time, domain, isp string, w Window) ([]GovernorCeiling, error) {
	out := make([]GovernorCeiling, 0, len(g))
	for _, r := range g {
		if r == nil {
			continue
		}
		cs, err := r.Ceilings(ctx, day, domain, isp, w)
		if err != nil {
			return nil, fmt.Errorf("governor read failed for %s/%s: %w", domain, isp, err)
		}
		out = append(out, cs...)
	}
	return out, nil
}

// ApplyGovernors returns effective = min(contracted, governors…) plus the name
// of the governor that bound (empty when the contract itself is the ceiling).
func ApplyGovernors(contracted int, ceilings []GovernorCeiling) (effective int, boundBy string) {
	if contracted < 0 {
		contracted = 0
	}
	effective, boundBy = contracted, ""
	for _, c := range ceilings {
		if c.Limit == NoLimit || c.Limit < 0 {
			continue
		}
		if c.Limit < effective {
			effective, boundBy = c.Limit, c.Name
		}
	}
	return effective, boundBy
}

// ThrottleGovernor is the REAL governor: it reads mailing_isp_throttle_state
// (created in cmd/server/main.go:5322 — isp TEXT PK, msgs_per_hour DOUBLE
// PRECISION, updated_at). That table is estate-wide per ISP, not per sending
// domain, so it can only ever express "this ISP pipe is collapsed".
//
// Semantics (§2.3 "mailing_isp_throttle_state.msgs_per_hour (0 ⇒ 0)"):
//
//	no row                   → no ceiling
//	rate <= BlockAtOrBelow   → ceiling 0 (hard stop)
//	rate >  BlockAtOrBelow   → ceiling floor(rate × active-window hours)
//
// NOTE for WP5: the OLD chain (partner_drip_orchestrator.fetchThrottledISPs,
// :4127) defers on msgs_per_hour < ThrottledISPRateThreshold (default 50) — a
// strictly harder gate than rate<=0. Set BlockAtOrBelow to the same threshold if
// the executor must keep the old deferral behaviour when the flag is on.
type ThrottleGovernor struct {
	DB Queryer
	// BlockAtOrBelow is the msgs_per_hour at or under which the lane is stopped.
	// Zero (the default) implements the design doc literally: only rate 0 blocks.
	BlockAtOrBelow float64
}

func (t ThrottleGovernor) Ceilings(ctx context.Context, day time.Time, domain, isp string, w Window) ([]GovernorCeiling, error) {
	if t.DB == nil {
		return nil, nil
	}
	var rate float64
	err := t.DB.QueryRowContext(ctx, `
		SELECT msgs_per_hour
		FROM mailing_isp_throttle_state
		WHERE lower(btrim(isp)) = lower(btrim($1))
	`, isp).Scan(&rate)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		// Missing TABLE = no throttling, matching fetchThrottledISPs so a fresh
		// database does not fail every reservation closed. A missing COLUMN is a
		// real fault and is surfaced.
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mailing_isp_throttle_state for isp=%s: %w", isp, err)
	}
	if rate <= t.BlockAtOrBelow {
		return []GovernorCeiling{{Name: "throttle", Limit: 0}}, nil
	}
	limit := int(math.Floor(rate * w.Hours()))
	if limit < 0 {
		limit = 0
	}
	return []GovernorCeiling{{Name: "throttle", Limit: limit}}, nil
}

// -----------------------------------------------------------------------------
// Brand resolution — the reverse of the executor's brand → domain
// -----------------------------------------------------------------------------

// BrandForDomain resolves a sending domain to its canonical brand_code.
// ok=false means the domain could not be identified — never a guessed code.
type BrandForDomain func(ctx context.Context, sendingDomain string) (string, bool)

// DefaultBrandForDomain is the platform's existing mapping, and is a deliberate
// mirror of api.ispBanBrandCode (internal/api/isp_bans.go): brand.Root (owned
// domain roots, the union of the Go slice and mailing_owned_domains) then
// brandident.CodeForApex (the ONE brand_code↔apex normalizer). Nothing here
// re-hardcodes a brand map.
//
// mailing_sending_profiles was the suggested source and carries NO brand column
// (only sending_domain / pool_prefix / from_name), so it cannot answer this;
// brandident is the canonical reverse and needs no query at all.
//
// brand.Root returns its input unchanged on a miss (CLAUDE.md §7), so the second
// attempt strips the sending label ("em.warrantyforyou.com" → "warrantyforyou.com")
// exactly as isp_bans.go does.
func DefaultBrandForDomain(_ context.Context, sendingDomain string) (string, bool) {
	d := strings.ToLower(strings.TrimSpace(sendingDomain))
	if d == "" {
		return "", false
	}
	apex := brand.Root(d)
	if code, ok := brandident.CodeForApex(apex); ok {
		return code, true
	}
	if i := strings.Index(apex, "."); i > 0 {
		if code, ok := brandident.CodeForApex(apex[i+1:]); ok {
			return code, true
		}
	}
	return "", false
}

// -----------------------------------------------------------------------------
// GmailHoldGovernor — REAL (REQ-083 bans + the mature-brand allow-list)
// -----------------------------------------------------------------------------

// GmailAllowEnv is the per-ISP allow-list override the orchestrator reads.
const GmailAllowEnv = "PARTNER_DRIP_GMAIL_NEW_BRANDS"

// gmailAllowDefault is the mature-4 default (operator 2026-06-13): the
// engagement-priced, reputation-sensitive ISPs ship only from the warmed
// domains that own the isolated per-ISP IP pools.
const gmailAllowDefault = "db,ht,mh,qf"

// GmailAllowedBrands duplicates the gmail slice of
// worker.DefaultNewRecordISPBrandAllow (partner_drip_orchestrator.go:685).
// It is duplicated rather than imported because internal/worker will import
// THIS package at WP5 and the edge would be a cycle; the duplication is pinned
// by TestGmailAllowListMatchesOrchestrator in the external test package.
func GmailAllowedBrands() map[string]bool {
	v := strings.TrimSpace(os.Getenv(GmailAllowEnv))
	if v == "" {
		v = gmailAllowDefault
	}
	m := map[string]bool{}
	for _, b := range strings.Split(v, ",") {
		if b = strings.ToLower(strings.TrimSpace(b)); b != "" {
			m[b] = true
		}
	}
	return m
}

// GmailHoldGovernor stops gmail for a brand that is either banned in
// mailing_isp_bans (REQ-083, the 2026-08-30 8-brand ruling) or outside the
// mature-brand allow-list. It is the only governor that fails CLOSED.
//
// Why closed: isp_bans.go's own doctrine is "a ban that fails open re-creates
// exactly the leak this exists to close" — 3,416 gmail messages shipped on
// banned brands on 2026-09-01 because the enforcement ran somewhere the rebind
// could undo. An unreadable ban table, or a domain whose brand cannot be
// identified, therefore yields ceiling 0 for gmail.
//
// The one exception is a MISSING table: a database that has never run the
// REQ-083 migration is a fresh boot, not a policy failure, and is treated as
// "no bans" so a new environment is not wedged. Same split as ThrottleGovernor.
type GmailHoldGovernor struct {
	DB       Queryer
	OrgID    string         // "" = the cross-org union, which can only ban MORE
	BrandFor BrandForDomain // nil = DefaultBrandForDomain
	// ISP is the class this governor guards. Defaults to "gmail"; overridable so
	// the same body can guard another banned class without a second type.
	ISP      string
	CacheTTL time.Duration // 0 = 60s, matching api.ispBans

	mu      sync.RWMutex
	cached  map[string]map[string]bool // brand_code -> isp -> banned
	fetched time.Time
}

// NewGmailHoldGovernor builds the gmail hold against the live ban registry.
func NewGmailHoldGovernor(db Queryer, orgID string) *GmailHoldGovernor {
	return &GmailHoldGovernor{DB: db, OrgID: orgID, ISP: "gmail", CacheTTL: 60 * time.Second}
}

func (g *GmailHoldGovernor) isp() string {
	if s := normISP(g.ISP); s != "" {
		return s
	}
	return "gmail"
}

func (g *GmailHoldGovernor) brandFor() BrandForDomain {
	if g.BrandFor != nil {
		return g.BrandFor
	}
	return DefaultBrandForDomain
}

// bans returns the cached ban table, reloading when stale. A load error is
// never cached: a transient blip must not pin a refusal for a whole TTL.
// (nil, nil) means "the table does not exist" — no bans, not a failure.
func (g *GmailHoldGovernor) bans(ctx context.Context) (map[string]map[string]bool, error) {
	ttl := g.CacheTTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	g.mu.RLock()
	if g.cached != nil && time.Since(g.fetched) < ttl {
		c := g.cached
		g.mu.RUnlock()
		return c, nil
	}
	g.mu.RUnlock()

	if g.DB == nil {
		return nil, nil
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The whole table is 8 rows in production — a config table, read once and
	// scoped in memory. The org predicate mirrors api.ispBanTable.forOrg: an
	// empty org asks for the cross-org union.
	rows, err := g.DB.QueryContext(qctx, `
		SELECT lower(btrim(brand_code)), lower(btrim(isp))
		FROM mailing_isp_bans
		WHERE $1::text = '' OR lower(btrim(organization_id::text)) = lower(btrim($1))
	`, strings.TrimSpace(g.OrgID))
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load mailing_isp_bans: %w", err)
	}
	defer rows.Close()
	tbl := map[string]map[string]bool{}
	for rows.Next() {
		var code, isp string
		if err := rows.Scan(&code, &isp); err != nil {
			return nil, fmt.Errorf("scan mailing_isp_bans: %w", err)
		}
		if code == "" || isp == "" {
			return nil, fmt.Errorf("mailing_isp_bans row has an empty brand_code or isp")
		}
		if tbl[code] == nil {
			tbl[code] = map[string]bool{}
		}
		tbl[code][isp] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mailing_isp_bans rows: %w", err)
	}
	g.mu.Lock()
	g.cached, g.fetched = tbl, time.Now()
	g.mu.Unlock()
	return tbl, nil
}

func (g *GmailHoldGovernor) Ceilings(ctx context.Context, _ time.Time, domain, isp string, _ Window) ([]GovernorCeiling, error) {
	if normISP(isp) != g.isp() {
		return nil, nil // this governor has no opinion about other classes
	}
	stop := []GovernorCeiling{{Name: "gmail_hold", Limit: 0}}

	code, ok := g.brandFor()(ctx, domain)
	if !ok {
		log.Printf("[DripSupply] gmail_hold: cannot identify a brand for %q — holding gmail at 0 (a domain we cannot name cannot be proven allowed)", domain)
		return stop, nil
	}

	tbl, err := g.bans(ctx)
	if err != nil {
		log.Printf("[DripSupply] gmail_hold: ban registry unreadable (%v) — holding %s/gmail at 0; a ban that fails OPEN is the bug REQ-083 exists to prevent", err, domain)
		return stop, nil
	}
	if tbl[code][g.isp()] {
		return stop, nil
	}
	if !GmailAllowedBrands()[code] {
		return stop, nil
	}
	return nil, nil
}

// -----------------------------------------------------------------------------
// SESQuotaGovernor — REAL (remaining 24h account quota)
// -----------------------------------------------------------------------------

// SESQuotaFunc reads the account's 24-hour sending quota. It is a function, not
// an *ses.Client, so this package carries no AWS dependency and main.go needs no
// edit. Wire it in one line next to the orchestrator:
//
//	dripsupply.NewSESQuotaGovernor(func(ctx context.Context) (float64, float64, error) {
//	    out, err := sesClient.GetAccountStatistics(ctx)   // internal/ses/client.go:269
//	    if err != nil || out == nil || out.SendQuota == nil {
//	        return 0, 0, err
//	    }
//	    return out.SendQuota.Max24HourSend, out.SendQuota.SentLast24Hours, nil
//	})
type SESQuotaFunc func(ctx context.Context) (max24h, sent24h float64, err error)

// SESRouteAllEnv is the orchestrator's route-everything-through-SES switch.
const SESRouteAllEnv = "PARTNER_DRIP_ROUTE_ALL_SES"

// SESDoctrineISPs are the classes that route SES by standing doctrine
// (CLAUDE.md §5.3: "Gmail, Apple and the yahoo-family lanes route SES").
var SESDoctrineISPs = []string{"gmail", "apple"}

// SESRouteAll mirrors worker.dripRouteAllSES (partner_drip_orchestrator.go:1323):
// default ON, meaning the WHOLE drip defaults to the SES relay. Duplicated for
// the same import-cycle reason as the gmail allow-list and pinned by
// TestSESRouteAllMatchesOrchestrator.
func SESRouteAll() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SESRouteAllEnv))) {
	case "false", "0", "off", "no":
		return false
	}
	return true
}

// SESRoutedISP reports whether an ISP's mail goes through the SES relay, and so
// whether the account quota is a real ceiling for it.
//
// NOTE: with SESRouteAll on (the default) this is EVERY class, not just
// gmail/apple. The orchestrator's `sesPins` (dripBrandISPSESProfiles) is not a
// set of SES-routed ISPs at all — it is a brand×ISP → tenant-profile map whose
// default is the single entry ht=microsoft, layered ON TOP of the route-all
// default. Treating sesPins as the routed set would have under-counted the
// governed surface to one cell.
func SESRoutedISP(isp string) bool {
	if SESRouteAll() {
		return true
	}
	n := normISP(isp)
	for _, d := range SESDoctrineISPs {
		if n == d {
			return true
		}
	}
	return false
}

// SESQuotaGovernor caps SES-routed ISPs at the account's remaining 24-hour
// quota. Doctrine: an SES 454 is CAPACITY, not deliverability (JAOS core §5) —
// this reduces capacity and must never colour an ISP health band.
//
// A read error yields NO ceiling and increments ErrorCount. Failing closed here
// would stop the estate on an AWS blip, and the quota is a ceiling on the whole
// account rather than a per-cell policy.
type SESQuotaGovernor struct {
	read     SESQuotaFunc
	ttl      time.Duration
	routed   func(isp string) bool
	errors   atomic.Int64
	mu       sync.RWMutex
	cached   int
	hasCache bool
	fetched  time.Time
	warnOnce sync.Once
}

// NewSESQuotaGovernor builds the quota governor. A nil read function makes it
// permanently inert (and says so once), never a source of zeroes.
func NewSESQuotaGovernor(read SESQuotaFunc) *SESQuotaGovernor {
	return &SESQuotaGovernor{read: read, ttl: 5 * time.Minute, routed: SESRoutedISP}
}

// WithTTL overrides the 5-minute cache (tests, and a tighter watch during a
// quota-pressure incident).
func (g *SESQuotaGovernor) WithTTL(d time.Duration) *SESQuotaGovernor {
	if d > 0 {
		g.ttl = d
	}
	return g
}

// ErrorCount is the number of failed quota reads since boot. Rising means the
// estate is running WITHOUT the quota ceiling — visible, not silent.
func (g *SESQuotaGovernor) ErrorCount() int64 { return g.errors.Load() }

func (g *SESQuotaGovernor) Ceilings(ctx context.Context, _ time.Time, _, isp string, _ Window) ([]GovernorCeiling, error) {
	if g.read == nil {
		g.warnOnce.Do(func() {
			log.Printf("[DripSupply] governor \"ses_quota\" has no reader — SES-routed capacity is NOT quota-bound")
		})
		return nil, nil
	}
	routed := g.routed
	if routed == nil {
		routed = SESRoutedISP
	}
	if !routed(isp) {
		return nil, nil
	}

	g.mu.RLock()
	if g.hasCache && time.Since(g.fetched) < g.ttl {
		c := g.cached
		g.mu.RUnlock()
		return []GovernorCeiling{{Name: "ses_quota", Limit: c}}, nil
	}
	g.mu.RUnlock()

	max24h, sent24h, err := g.read(ctx)
	if err != nil {
		n := g.errors.Add(1)
		log.Printf("[DripSupply] ses_quota read failed (%v, total=%d) — NO quota ceiling this tick; capacity is contract-bound only", err, n)
		return nil, nil
	}
	// SES reports -1 for an account with no 24-hour cap.
	if max24h < 0 {
		g.store(NoLimit)
		return []GovernorCeiling{{Name: "ses_quota", Limit: NoLimit}}, nil
	}
	remaining := int(math.Floor(max24h - sent24h))
	if remaining < 0 {
		remaining = 0 // the quota is genuinely spent; that IS a ceiling of 0
	}
	g.store(remaining)
	return []GovernorCeiling{{Name: "ses_quota", Limit: remaining}}, nil
}

func (g *SESQuotaGovernor) store(v int) {
	g.mu.Lock()
	g.cached, g.hasCache, g.fetched = v, true, time.Now()
	g.mu.Unlock()
}

// -----------------------------------------------------------------------------
// HealthBandGovernor — REAL, and it is CONTRACT POLICY, not an inferred verdict
// -----------------------------------------------------------------------------

// AmberShare is the fraction of contracted capacity an amber domain may use.
// The band constants and HealthBands() live in contracts.go (WP2) — the
// vocabulary belongs to the contract, this file only prices it.
const AmberShare = 0.5

// HealthBandCeiling turns a band into a ceiling on one domain×ISP cell:
//
//	red         → 0
//	amber       → floor(0.5 × contracted)
//	green, ""   → no ceiling
//
// ok=false means "no opinion" — the caller must not append a ceiling at all.
//
// This is a PURE function of the contract. The band is operator policy carried
// on drip_domain_contracts (§1.1), which is why it is versioned, approved and
// audited like every other number there — rather than a verdict this governor
// infers from traffic. The earlier WP3 report blocked on exactly that
// distinction; the operator ruled the band is policy, and this is the
// implementation of that ruling.
//
// An UNRECOGNISED band yields no ceiling and logs once: WP2's Validate() is the
// gate on the vocabulary, and a value that slips past it is drift, not a licence
// for this function to invent a number.
func HealthBandCeiling(band string, contracted int) (GovernorCeiling, bool) {
	b := strings.ToLower(strings.TrimSpace(band))
	switch b {
	case "", HealthBandGreen:
		return GovernorCeiling{}, false
	case HealthBandRed:
		return GovernorCeiling{Name: "health_band:" + HealthBandRed, Limit: 0}, true
	case HealthBandAmber:
		if contracted < 0 {
			contracted = 0
		}
		limit := int(math.Floor(AmberShare * float64(contracted)))
		if limit > contracted {
			// Unreachable for AmberShare <= 1, and asserted by
			// TestHealthBand_AmberNeverExceedsContracted. Kept because a
			// governor that RAISED capacity would be the one bug ApplyGovernors
			// cannot catch on its own if the share were ever mis-set.
			limit = contracted
		}
		return GovernorCeiling{Name: "health_band:" + HealthBandAmber, Limit: limit}, true
	default:
		unknownBandOnce.Do(func() {
			log.Printf("[DripSupply] health_band %q is not one of %v — applying NO ceiling; the contract's Validate() is the gate on this vocabulary", band, HealthBands())
		})
		return GovernorCeiling{}, false
	}
}

var unknownBandOnce sync.Once

// HealthBandGovernor applies the domain contract's band.
//
// It is NOT a GovernorReader: that interface is handed (day, domain, isp,
// window) and never the contract, so a band read through it would need a DB
// round-trip per cell for a value the caller is already holding. RefillDomain
// applies it directly from the *DomainContract it already has (see
// RefillDomain), which is also what keeps the band a pure contract read with no
// query and no cache to go stale.
type HealthBandGovernor struct{}

// NewHealthBandGovernor is retained for symmetry with the other governors and
// for any caller that wants the band applied explicitly.
func NewHealthBandGovernor() *HealthBandGovernor { return &HealthBandGovernor{} }

// CeilingFor is the contract-aware entry point. A nil contract has no opinion.
func (g *HealthBandGovernor) CeilingFor(c *DomainContract, contracted int) (GovernorCeiling, bool) {
	if c == nil {
		return GovernorCeiling{}, false
	}
	return HealthBandCeiling(c.Band(), contracted)
}

// -----------------------------------------------------------------------------
// Refill — the token math (§2.3)
// -----------------------------------------------------------------------------

// RefillResult reports what one Refill did, for the tick-outcome surface.
type RefillResult struct {
	RefillPerInterval float64
	IntervalsElapsed  int
	TokensBefore      float64
	TokensAfter       float64
	Capped            bool // the burst ceiling bound this refill
	DayRolled         bool // now is past the balance's day
	InWindow          bool
}

// Refill advances b's token bucket to now, in place:
//
//	refill = effective / active_intervals
//	tokens = min(tokens + refill × intervals_elapsed, refill × max_burst_intervals)
//
// Three details are load-bearing:
//
//  1. last_refill_tick advances by WHOLE intervals only. Advancing it to `now`
//     discards the sub-interval remainder every tick, and a scheduler ticking
//     faster than the interval (a 15 s poll against a 15 min interval) would then
//     never accumulate a single token — the bucket would read as "paced" while
//     granting zero forever.
//  2. Both endpoints are clamped into [window_start, window_end], so closed hours
//     never mint tokens and an overnight gap cannot mint 24 h of them.
//  3. A balance row whose day is already over resets to 0 rather than
//     accumulating: tokens do not survive the day boundary (§2.3).
func Refill(b *Balance, w Window, now time.Time) RefillResult {
	res := RefillResult{InWindow: w.Contains(b.Day, now)}
	res.TokensBefore = b.Tokens

	refill := 0.0
	if b.Effective > 0 {
		refill = float64(b.Effective) / float64(w.ActiveIntervals())
	}
	res.RefillPerInterval = refill
	ceiling := refill * float64(w.BurstIntervals())

	balDay := dayOf(b.Day)
	nowDay := dayOf(now.In(b.Day.Location()))
	switch {
	case nowDay.After(balDay):
		// The day rolled. This row is history; it never accumulates again.
		_, end := w.Bounds(b.Day)
		b.Tokens, b.LastRefillTick = 0, end
		res.DayRolled, res.TokensAfter = true, 0
		return res
	case nowDay.Before(balDay):
		// Clock skew, or a pre-seeded tomorrow row: do nothing.
		res.TokensAfter = b.Tokens
		return res
	}

	start, end := w.Bounds(b.Day)
	clamp := func(t time.Time) time.Time {
		t = t.In(b.Day.Location())
		if t.Before(start) {
			return start
		}
		if t.After(end) {
			return end
		}
		return t
	}
	from, to := clamp(b.LastRefillTick), clamp(now)
	if to.After(from) && w.Interval > 0 {
		res.IntervalsElapsed = int(to.Sub(from) / w.Interval)
	}
	if res.IntervalsElapsed > 0 {
		b.Tokens += refill * float64(res.IntervalsElapsed)
		b.LastRefillTick = from.Add(time.Duration(res.IntervalsElapsed) * w.Interval)
	}
	// The clamp runs unconditionally: tokens handed back by Commit/Release/Expire
	// can push the balance over the burst ceiling between ticks, and this is where
	// that overshoot is taken back.
	if b.Tokens > ceiling {
		b.Tokens, res.Capped = ceiling, true
	}
	if b.Tokens < 0 {
		b.Tokens = 0
	}
	res.TokensAfter = b.Tokens
	return res
}

// -----------------------------------------------------------------------------
// RefillDomain — the persisting wrapper (§2.8 "refill buckets")
// -----------------------------------------------------------------------------

// RefillDomain recomputes `effective` from the contract + governors and advances
// the token bucket for every ISP of one domain contract, one row per short
// transaction under SELECT … FOR UPDATE.
//
// The row lock is not optional: Reserve decrements `tokens` in its own
// transaction, so a read-modify-write here without the lock is a lost-update
// window that hands the decremented tokens straight back.
//
// Lock scope is the domain balance row ONLY — never the lane row — so a refill
// running concurrently with a reservation (domain → lane) cannot deadlock.
//
// A governor that cannot be read leaves that ISP's `effective` UNCHANGED (fail
// closed) rather than defaulting to the contract.
//
// The binding governor's name is written to drip_capacity_balance.effective_reason
// so Reserve, the other ECS instance and the §3 API all read ONE label off the
// row instead of each holding their own guess.
func (s *Service) RefillDomain(ctx context.Context, day time.Time, c *DomainContract) (map[string]RefillResult, error) {
	if c == nil {
		return nil, errors.New("dripsupply: RefillDomain called with a nil contract")
	}
	w, err := WindowOf(c)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make(map[string]RefillResult, len(c.DailyMaxByISP))
	for _, isp := range sortedKeys(c.DailyMaxByISP) {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("dripsupply: RefillDomain cancelled after %d isps: %w", len(out), err)
		}
		n := normISP(isp)
		contracted := c.DailyMaxByISP[isp]
		ceilings, gerr := s.governorCeilings(ctx, day, c.SendingDomain, n, w)
		if gerr != nil {
			log.Printf("[DripSupply] refill %s/%s: %v — leaving effective UNCHANGED (fail closed)", c.SendingDomain, n, gerr)
			continue
		}
		// The health band comes off the contract we are already holding — no
		// query, no cache, no inference (operator ruling: the band is POLICY on
		// drip_domain_contracts, not a verdict derived from traffic).
		// c.Band() resolves an empty band to green exactly as the column's
		// NOT NULL DEFAULT does, so this reads the same value on both sides of
		// a round trip.
		if bandCeiling, ok := HealthBandCeiling(c.Band(), contracted); ok {
			ceilings = append(ceilings, bandCeiling)
		}
		effective, boundBy := ApplyGovernors(contracted, ceilings)
		r, rerr := s.refillOne(ctx, day, c.SendingDomain, n, contracted, effective, boundBy, w, now)
		if rerr != nil {
			return out, rerr
		}
		out[n] = r
	}
	return out, nil
}

func (s *Service) refillOne(ctx context.Context, day time.Time, domain, isp string, contracted, effective int, effectiveReason string, w Window, now time.Time) (RefillResult, error) {
	var res RefillResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		b := Balance{Day: dayOf(day), SendingDomain: domain, ISP: isp}
		// last_refill_tick is NULLABLE in the WP1 schema, so a row seeded by any
		// path other than EnsureDayBalances can carry NULL. Scanning that into a
		// time.Time errors out and would wedge the whole domain's refill, so it
		// is read through NullTime and defaults to the window start — which
		// makes the day accrue from open, not from an arbitrary zero time.
		var lastRefill sql.NullTime
		err := tx.QueryRowContext(ctx, `
			SELECT contracted, effective, tokens, reserved, committed, released, last_refill_tick
			FROM drip_capacity_balance
			WHERE day = $1::date AND sending_domain = $2 AND isp = $3
			FOR UPDATE
		`, dayKey(day), domain, isp).Scan(&b.Contracted, &b.Effective, &b.Tokens, &b.Reserved, &b.Committed, &b.Released, &lastRefill)
		if errors.Is(err, sql.ErrNoRows) {
			// No balance row = this domain×ISP is not open for business today.
			// EnsureDayBalances owns creation; a refill never invents capacity.
			return nil
		}
		if err != nil {
			return fmt.Errorf("select balance: %w", err)
		}
		if lastRefill.Valid {
			b.LastRefillTick = lastRefill.Time
		} else {
			b.LastRefillTick, _ = w.Bounds(day)
		}
		b.Contracted, b.Effective, b.EffectiveReason = contracted, effective, effectiveReason
		res = Refill(&b, w, now)
		// effective_reason is PERSISTED, not cached in this process: the API (§3)
		// and the other orchestrator instance must read the same label off the
		// same row. Empty string means the contract itself was the ceiling.
		if _, err := tx.ExecContext(ctx, `
			UPDATE drip_capacity_balance
			SET contracted = $4, effective = $5, effective_reason = $6, tokens = $7, last_refill_tick = $8
			WHERE day = $1::date AND sending_domain = $2 AND isp = $3
		`, dayKey(day), domain, isp, b.Contracted, b.Effective, b.EffectiveReason, b.Tokens, b.LastRefillTick); err != nil {
			return fmt.Errorf("update balance: %w", err)
		}
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("dripsupply: refill %s/%s on %s: %w", domain, isp, dayKey(day), err)
	}
	return res, nil
}

// isUndefinedTable reports whether err is Postgres 42P01 (undefined_table) —
// the table has never been created, i.e. a fresh database rather than a broken
// one. It deliberately does NOT match a bare "does not exist" substring: 42703
// (undefined_column) carries the same words, and treating a schema drift on an
// EXISTING policy table as "no policy" is how a governor fails open by accident.
func isUndefinedTable(err error) bool {
	if err == nil {
		return false
	}
	var pe *pq.Error
	if errors.As(err, &pe) {
		return string(pe.Code) == "42P01"
	}
	// Drivers that lose the SQLSTATE still name the relation in the message.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "relation") && strings.Contains(msg, "does not exist")
}

// isStatementTimeout reports whether err is a statement/lock timeout or a
// cancelled context — the class §2.2 says the caller treats as granted=0,
// binding_reason='reserve_timeout' rather than as a failure.
//
//	57014 query_canceled     — statement_timeout fired
//	55P03 lock_not_available — lock_timeout fired
func isStatementTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pe *pq.Error
	if errors.As(err, &pe) {
		switch string(pe.Code) {
		case "57014", "55P03":
			return true
		}
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "statement timeout") || strings.Contains(msg, "canceling statement due to")
}
