package dripsupply

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

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
		// Missing table = no throttling, matching fetchThrottledISPs so a fresh
		// database does not fail every reservation closed.
		if strings.Contains(strings.ToLower(err.Error()), "does not exist") {
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

// unconfiguredGovernor is the shared body of the three governors WP3 stubs. It
// never invents a ceiling — it returns NoLimit and logs ONCE, so an operator
// grepping for "NOT WIRED" can see which inputs `effective` is missing instead
// of trusting a number that silently ignores them.
type unconfiguredGovernor struct {
	name   string
	source string
	once   sync.Once
}

func (u *unconfiguredGovernor) ceilings() ([]GovernorCeiling, error) {
	u.once.Do(func() {
		log.Printf("[DripSupply] governor %q NOT WIRED — effective capacity ignores it. TODO source: %s", u.name, u.source)
	})
	return []GovernorCeiling{{Name: u.name, Limit: NoLimit}}, nil
}

// SESQuotaGovernor caps SES-routed ISPs at the remaining SES daily quota.
//
// TODO(WP5/WP7): source is the SESv2 account sending quota (AWS SDK sesv2
// GetAccount → Max24HourSend minus SentLast24Hours, via internal/ses), divided
// across the SES-routed domain×ISP cells for the rest of the window. Doctrine:
// an SES 454 is CAPACITY, not deliverability (JAOS core §5) — this governor must
// reduce capacity and must NOT feed an ISP health band.
type SESQuotaGovernor struct{ u unconfiguredGovernor }

func NewSESQuotaGovernor() *SESQuotaGovernor {
	return &SESQuotaGovernor{u: unconfiguredGovernor{
		name:   "ses_quota",
		source: "SESv2 GetAccount Max24HourSend/SentLast24Hours via internal/ses",
	}}
}

func (g *SESQuotaGovernor) Ceilings(context.Context, time.Time, string, string, Window) ([]GovernorCeiling, error) {
	return g.u.ceilings()
}

// HealthBandGovernor caps a domain by its deliverability health band.
//
// TODO(WP5/WP7): source is the sending-domain card health band — Go side
// internal/domainagent/scorecard.go; operator artifact
// .scratch/reports/sending_domain_cards/. Map band → fraction of contract
// (red ⇒ 0, amber ⇒ 0.5×, green ⇒ NoLimit) with the mapping in the contract's
// notes, never hard-coded here.
type HealthBandGovernor struct{ u unconfiguredGovernor }

func NewHealthBandGovernor() *HealthBandGovernor {
	return &HealthBandGovernor{u: unconfiguredGovernor{
		name:   "health_band",
		source: "sending_domain_cards / internal/domainagent/scorecard.go",
	}}
}

func (g *HealthBandGovernor) Ceilings(context.Context, time.Time, string, string, Window) ([]GovernorCeiling, error) {
	return g.u.ceilings()
}

// GmailHoldGovernor enforces the standing gmail holds.
//
// TODO(WP5/WP7): source is `mailing_isp_bans` (REQ-083, enforced in the planner
// at internal/api/pmta_campaign_planner.go:1078) plus the 8-brand gmail ban
// (WFY RB RRU TOT CP LPL YIH CI). Ceiling 0 for a banned brand×gmail cell.
// Until this is wired, a domain contract's gmail value is the ONLY thing between
// a banned brand and a gmail send — which is exactly why §1.1 requires notes
// naming the operator ruling whenever gmail > 0.
type GmailHoldGovernor struct{ u unconfiguredGovernor }

func NewGmailHoldGovernor() *GmailHoldGovernor {
	return &GmailHoldGovernor{u: unconfiguredGovernor{
		name:   "gmail_hold",
		source: "mailing_isp_bans (REQ-083) + the 8-brand gmail ban",
	}}
}

func (g *GmailHoldGovernor) Ceilings(context.Context, time.Time, string, string, Window) ([]GovernorCeiling, error) {
	return g.u.ceilings()
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
