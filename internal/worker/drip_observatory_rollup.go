package worker

// Drip Observatory rollup worker — Vector B plan rev 4.2 §6 (P1/P2).
//
// Recomputes the partner-drip fact tables (partner_drip_send_cohort_daily +
// partner_drip_event_daily) for the lanes enabled by DRIP_OBSERVATORY_LANES,
// under the run/scope/cursor/staged-swap generation protocol:
//
//   - ONE reserved *sql.Conn holds the advisory lock for the whole run AND
//     carries every scope INSERT, staged-fact write, and the swap transaction
//     (§6.1). Ownership is re-verified on that conn immediately before each
//     publish step; a lost session cancels the run with NO swap.
//   - Per-(source_pass × dataset) cursors (§5.3/§6.2): no cursor row ⇒ the
//     lane gets a FULL historical backfill [dataset.created_at, run start);
//     otherwise incremental [watermark_to − 1h, run start). Cursors advance
//     only on completion.
//   - Two-stage recompute (§6.3): stage 1 DISCOVERY finds WHAT changed in the
//     window (never what to count) and maps every changed fact to BOTH exact
//     logical keys — its event-day key AND its campaign's cohort key (the
//     FIRST-dispatch Denver day, §3.1); stage 2 recomputes every scoped key
//     from its COMPLETE source population.
//   - Per-kind atomic swap (§6.4): staged rows land under this run_id; one
//     transaction deletes prior-generation rows for EXACTLY the scoped keys
//     of THIS pass's fact kinds ('cohort'/'event'/'hygiene' — hygiene rows
//     themselves land in P3; the swap already scopes the kind).
//
// Identity (interim until the §7.0 meta stamp) is the rev-4.2 §6.6 chain:
// positional name parse (brand token = ORCHESTRATOR CODE, never an apex) →
// resolveBrandSendingDomain (mailing_brand_metadata overlay preferred,
// compiled map fallback) → strip the leading em./m. label → sending_apex →
// brandident.CodeForApex → canonical brand_code. Any link failing →
// brand_unknown / name_parse_error quarantine, never a guess.
//
// Kill switch: DRIP_OBSERVATORY_ROLLUP_DISABLED=1. Lane gate:
// DRIP_OBSERVATORY_LANES ('' = worker runs, processes NOTHING; UUID list =
// those lanes; 'all' = every lane). First run 120s after boot, then every 6h.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brandident"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// dripObservatoryLockID is the session advisory lock for every observatory
// pass. Verified unused across internal/ + cmd/ (existing IDs: 8675309,
// 867530901, 987654331, 728104407, 728104408, hashtext-derived keys).
const dripObservatoryLockID int64 = 0x0B5E_0001

const dripObsRollupPass = "rollup"

// dripObsIndexNames — the §4 supporting indexes. The heavy pass REFUSES to
// run unless all five exist, valid and ready (a failed CONCURRENTLY build
// leaves an INVALID index that IF NOT EXISTS skips forever — §4 runbook).
var dripObsIndexNames = []string{
	"idx_pcq_mailed_at",
	"idx_pcq_dataset_mailed_at",
	"idx_pcq_mailed_campaign",
	"idx_pcq_validated_at",
	"idx_mc_partner_dataset_sched",
}

// ---------------------------------------------------------------------------
// Denver time helpers — bounds computed in Go, NEVER tz-cast in a WHERE
// clause (Denver tz-cast seq-scan footgun). AddDate through the location
// handles the 23h/25h DST days (§11 #20).
// ---------------------------------------------------------------------------

var (
	dripObsDenverOnce sync.Once
	dripObsDenverLoc  *time.Location
)

func dripObsDenver() *time.Location {
	dripObsDenverOnce.Do(func() {
		loc, err := time.LoadLocation("America/Denver")
		if err != nil {
			log.Printf("[DripObservatory] LoadLocation America/Denver failed (%v) — falling back to UTC", err)
			loc = time.UTC
		}
		dripObsDenverLoc = loc
	})
	return dripObsDenverLoc
}

func dripObsDenverDay(t time.Time) string {
	return t.In(dripObsDenver()).Format("2006-01-02")
}

// dripObsDayBounds returns the UTC-comparable [start, end) instants of a
// Denver calendar day ("2006-01-02").
func dripObsDayBounds(day string) (time.Time, time.Time, error) {
	lo, err := time.ParseInLocation("2006-01-02", day, dripObsDenver())
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return lo, lo.AddDate(0, 0, 1), nil
}

// ---------------------------------------------------------------------------
// ISP vocabulary — AllGroups() ∪ {'other'} (rev-4.1 STOP-2 ruling).
// ---------------------------------------------------------------------------

var dripObsISPVocab = func() map[string]bool {
	m := make(map[string]bool, len(isp.AllGroups())+1)
	for _, g := range isp.AllGroups() {
		m[g] = true
	}
	m[isp.Other] = true
	return m
}()

// dripObsNormalizeISPFamily normalizes a partner_clean_queue isp_family value
// into the observatory vocabulary, mirroring the orchestrator's own claim-time
// fallback (LOWER(COALESCE(NULLIF(isp_family,''),'other')), bucket-to-'other'
// when unrecognized — partner_drip_orchestrator.go:2679-2687).
func dripObsNormalizeISPFamily(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || !dripObsISPVocab[v] {
		return isp.Other
	}
	return v
}

func dripObsISPFromEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return isp.Other
	}
	return isp.GroupFromDomain(email[at+1:])
}

// ---------------------------------------------------------------------------
// §6.6 identity chain (interim until the §7.0 meta stamp)
// ---------------------------------------------------------------------------

const dripObsNamePrefix = "[partner-drip] "

var (
	dripObsTouchRE     = regexp.MustCompile(`\[t(\d+)\]`)
	dripObsTimestampRE = regexp.MustCompile(`^\d{8}T\d{6}$`)
)

// parseDripCampaignName parses positionally against the orchestrator's name
// literal fmt.Sprintf("[partner-drip] %s %s %s %s %s", v.vertical, brand, ts,
// sha4, nonce) (partner_drip_orchestrator.go:3642), then regex-scans for the
// OPTIONAL bracket suffixes [tN] (touch; absent → 1) and [ses:xxxxxxxx]
// (appended at :1267). NEVER assumes an exact field count.
func parseDripCampaignName(name string) (vertical, brandToken string, touch int, ok bool) {
	if !strings.HasPrefix(name, dripObsNamePrefix) {
		return "", "", 0, false
	}
	fields := strings.Fields(strings.TrimPrefix(name, dripObsNamePrefix))
	if len(fields) < 5 {
		return "", "", 0, false
	}
	// Positional validation: the 3rd token after the prefix is the timestamp.
	if !dripObsTimestampRE.MatchString(fields[2]) {
		return "", "", 0, false
	}
	touch = 1
	if m := dripObsTouchRE.FindStringSubmatch(name); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 0 {
			return "", "", 0, false
		}
		touch = n
	}
	return fields[0], fields[1], touch, true
}

// stripSendingLabel derives the sending APEX from a sending domain by
// stripping the leading em./m. label (§6.6 step 3: em.discountblog.com →
// discountblog.com, m.wcl-heloc.com → wcl-heloc.com). Domains carrying
// neither label pass through unchanged — brandident then rules.
func stripSendingLabel(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if rest, found := strings.CutPrefix(d, "em."); found {
		return rest
	}
	if rest, found := strings.CutPrefix(d, "m."); found {
		return rest
	}
	return d
}

// dripObsGovernedApexes derives the governed (Kumo) property apex set from
// the orchestrator's governedBrands token codes (:101) via the compiled
// brand→domain map, so BOTH identity paths (meta rows carry canonical
// brandident codes; name parse carries orchestrator tokens) agree on the
// governed flag through the one stable dimension they share: the apex.
var (
	dripObsGovernedOnce   sync.Once
	dripObsGovernedApexes map[string]bool
)

func dripObsGovernedApex(apex string) bool {
	dripObsGovernedOnce.Do(func() {
		m := map[string]bool{}
		for code := range governedBrands {
			if dom, ok := resolveBrandSendingDomain(code); ok {
				m[stripSendingLabel(dom)] = true
			}
		}
		dripObsGovernedApexes = m
	})
	return dripObsGovernedApexes[strings.ToLower(strings.TrimSpace(apex))]
}

// resolveObsBrand runs §6.6 chain links 2–4 for an orchestrator brand-code
// token. overlay is THIS CYCLE's fresh mailing_brand_metadata load (preferred,
// exactly like the orchestrator's own loadBrandDomains overlay — the worker
// keeps a cycle-local copy instead of mutating the orchestrator's shared map);
// resolveBrandSendingDomain (same package, :332-343) is the fallback and
// itself prefers the shared overlay before the compiled map. NEVER a direct
// brandident lookup on the token: orchestrator codes ≠ brandident codes for
// 11/27 brands (bwp/hws/rru/tot/yih/mrd/lpl/wfy/mpf/pmd/trb).
func resolveObsBrand(token string, overlay map[string]string) (apex, brandCode string, ok bool) {
	t := strings.ToLower(strings.TrimSpace(token))
	if t == "" {
		return "", "", false
	}
	dom := overlay[t]
	if dom == "" {
		if d, found := resolveBrandSendingDomain(t); found {
			dom = d
		}
	}
	if strings.TrimSpace(dom) == "" {
		return "", "", false
	}
	apex = stripSendingLabel(dom)
	code, found := brandident.CodeForApex(apex)
	if !found {
		return "", "", false
	}
	return apex, code, true
}

// ---------------------------------------------------------------------------
// §3.3 click classification — money-HOST match / wrapper path / other
// ---------------------------------------------------------------------------

// classifyClickURL classifies a tracking-event link_url. Money-host matching
// applies dripMoneyHostHrefRE itself (the ONE money-host source, §7.2) but
// anchored at position 0 so a wrapper URL that merely EMBEDS a money URL in
// its query is not misread as money. Wrapper = the emailed t.em…/o/… and
// /track/click/ forms (written only by the SES webhook —
// handlers_ses_events.go:556); everything else (incl. parse failures and the
// /integration/ unsub links) is "other".
func classifyClickURL(raw string) string {
	if loc := dripMoneyHostHrefRE.FindStringIndex(raw); loc != nil && loc[0] == 0 {
		return "money"
	}
	if u, err := url.Parse(raw); err == nil {
		p := u.EscapedPath()
		if strings.HasPrefix(p, "/o/") || strings.HasPrefix(p, "/track/click/") {
			return "wrapper"
		}
	}
	return "other"
}

// ---------------------------------------------------------------------------
// Worker
// ---------------------------------------------------------------------------

type DripObservatoryRollup struct {
	db       *sql.DB
	disabled bool

	firstDelay time.Duration
	interval   time.Duration

	nowFn func() time.Time

	// lockPID is the backend pid of the reserved lock session for the
	// current run (diagnostics + the lock-loss integration scenario).
	lockPID int

	// hookAfterDiscovery, when non-nil, runs after stage-1 discovery of each
	// lane (test seam for §11 #10 — lock-session loss mid-run). Nil in prod.
	hookAfterDiscovery func(datasetID string)
}

func NewDripObservatoryRollup(db *sql.DB) *DripObservatoryRollup {
	w := &DripObservatoryRollup{
		db:         db,
		firstDelay: 120 * time.Second,
		interval:   6 * time.Hour,
		nowFn:      time.Now,
	}
	if v := os.Getenv("DRIP_OBSERVATORY_ROLLUP_DISABLED"); v == "1" || strings.EqualFold(v, "true") {
		log.Println("[DripObservatory] DRIP_OBSERVATORY_ROLLUP_DISABLED set — rollup worker disabled")
		w.disabled = true
	}
	return w
}

// Start runs the rollup loop: first run 120s after boot, then every 6h.
// Single goroutine; honors ctx.Done().
func (w *DripObservatoryRollup) Start(ctx context.Context) {
	if w.disabled {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(w.firstDelay):
	}
	w.runCycleLogged(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runCycleLogged(ctx)
		}
	}
}

func (w *DripObservatoryRollup) runCycleLogged(ctx context.Context) {
	if err := w.runCycle(ctx); err != nil {
		log.Printf("[DripObservatory] cycle error: %v", err)
	}
}

// observatoryLaneGate is the parsed DRIP_OBSERVATORY_LANES value.
type observatoryLaneGate struct {
	all bool
	ids []string
}

func parseObservatoryLanes(v string) observatoryLaneGate {
	v = strings.TrimSpace(v)
	if v == "" {
		return observatoryLaneGate{}
	}
	if strings.EqualFold(v, "all") {
		return observatoryLaneGate{all: true}
	}
	var ids []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			ids = append(ids, p)
		}
	}
	return observatoryLaneGate{ids: ids}
}

func (g observatoryLaneGate) empty() bool { return !g.all && len(g.ids) == 0 }

// ---------------------------------------------------------------------------
// Cycle plumbing: lock, index gate, janitor, run row
// ---------------------------------------------------------------------------

func (w *DripObservatoryRollup) acquireLock(ctx context.Context, conn *sql.Conn) (bool, error) {
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, dripObservatoryLockID).Scan(&got); err != nil {
		return false, err
	}
	if got {
		// Record the session pid for diagnostics (and the §11 #10 scenario).
		if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&w.lockPID); err != nil {
			w.lockPID = 0
		}
	}
	return got, nil
}

// verifyLockOwnership re-checks, ON THE RESERVED CONN, that this session
// still holds the advisory lock (§6.1 — run immediately before every publish
// step). false ⇒ the caller cancels the run: no swap.
func (w *DripObservatoryRollup) verifyLockOwnership(ctx context.Context, conn *sql.Conn) bool {
	var owned bool
	err := conn.QueryRowContext(ctx, `
		SELECT count(*) = 1 FROM pg_locks
		WHERE locktype = 'advisory' AND objid = $1 AND pid = pg_backend_pid() AND granted
	`, dripObservatoryLockID).Scan(&owned)
	if err != nil {
		log.Printf("[DripObservatory] lock-ownership check failed: %v", err)
		return false
	}
	return owned
}

// checkSupportingIndexes is the §4 heavy-pass gate: all five supporting
// indexes exist AND indisvalid AND indisready.
func (w *DripObservatoryRollup) checkSupportingIndexes(ctx context.Context) (bool, error) {
	var n int
	var valid, ready bool
	err := w.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(bool_and(i.indisvalid), false), COALESCE(bool_and(i.indisready), false)
		FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
		WHERE c.relname = ANY($1::text[])
	`, pq.Array(dripObsIndexNames)).Scan(&n, &valid, &ready)
	if err != nil {
		return false, err
	}
	return n == len(dripObsIndexNames) && valid && ready, nil
}

// janitor deletes fact rows whose run_id belongs to rollup runs that are
// 'failed' or stuck 'running' >6h, then marks the stuck runs failed
// (§6.2). Generation-scoped by run_id, source_pass-scoped to 'rollup' — it
// cannot touch other passes' facts. Failed/stuck runs never own published
// rows (the swap and the status flip commit in one transaction), so this
// only clears orphaned staged generations.
func (w *DripObservatoryRollup) janitor(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT run_id FROM partner_drip_observatory_runs
		WHERE source_pass = $1
		  AND (status = 'failed' OR (status = 'running' AND started_at < NOW() - interval '6 hours'))
	`, dripObsRollupPass)
	if err != nil {
		return fmt.Errorf("janitor list: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("janitor scan: %w", err)
		}
		stale = append(stale, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("janitor rows: %w", err)
	}
	if len(stale) == 0 {
		return nil
	}
	for _, q := range []string{
		`DELETE FROM partner_drip_send_cohort_daily WHERE run_id = ANY($1::uuid[])`,
		`DELETE FROM partner_drip_event_daily WHERE run_id = ANY($1::uuid[])`,
		`DELETE FROM partner_drip_hygiene_daily WHERE run_id = ANY($1::uuid[])`,
	} {
		if _, err := conn.ExecContext(ctx, q, pq.Array(stale)); err != nil {
			return fmt.Errorf("janitor delete: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE partner_drip_observatory_runs
		SET status = 'failed', completed_at = NOW(),
		    error = COALESCE(error, 'janitor: stale running run (>6h)')
		WHERE run_id = ANY($1::uuid[]) AND status = 'running'
	`, pq.Array(stale)); err != nil {
		return fmt.Errorf("janitor mark failed: %w", err)
	}
	log.Printf("[DripObservatory] janitor cleared %d stale generation(s)", len(stale))
	return nil
}

// markRunFailed records a run failure THROUGH THE POOL — the reserved conn
// may be the thing that died (lock-session loss), and the failure record must
// land regardless.
func (w *DripObservatoryRollup) markRunFailed(runID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := w.db.ExecContext(ctx, `
		UPDATE partner_drip_observatory_runs
		SET status = 'failed', completed_at = NOW(), error = $2
		WHERE run_id = $1 AND status = 'running'
	`, runID, truncateObsSample(reason, 500)); err != nil {
		log.Printf("[DripObservatory] markRunFailed(%s): %v", runID, err)
	}
}

func truncateObsSample(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------------------------------------------------------------------
// Cycle data model
// ---------------------------------------------------------------------------

type obsDataset struct {
	id        string
	vertical  string
	createdAt time.Time
}

type obsCampaign struct {
	id              string
	orgID           string
	name            string
	totalRecipients int

	tokenCode   string // orchestrator code from the name (governed lookup key)
	brandCode   string // canonical brandident code (fact-row column)
	sendingApex string
	touch       int
	governed    bool

	cohortDay     string // "" = no dispatch evidence → no cohort row (§3.1)
	dispatchBasis string // pcq | message_log | acct_terminal | unavailable
}

type obsKey struct {
	org string
	day string
}

// obsCellKey is the fact-row grain under one (org, day, dataset).
type obsCellKey struct {
	org   string
	day   string
	touch int
	brand string
	isp   string // "" never appears here; lane rows are derived at flush
}

type obsCell struct {
	apex     string
	governed bool

	dispatchPcq    int
	dispatchMsgLog int

	delivered    int
	relayedToSES int
	hardBounced  int
	softBounced  int
	repBlocked   int
	validation   int
	complained   int
	hasAcct      bool

	opens         int
	clicksWrapper int
	clicksMoney   int
	actionsW      int // click actions attributed on the wrapper basis
	actionsR      int // click actions attributed on the resolved-only basis
	humanClicks   int
	unsubs        int
}

// obsLaneExtras carries the lane-scope-only measures per (org, day, touch,
// brand): planning (§3.2) and campaign-grain conversions (§6.6).
type obsLaneExtras struct {
	recipientsPlanned int
	conversions       int
	revenue           float64
	basisSet          map[string]bool // contributing campaigns' dispatch bases
	apex              string
	governed          bool
}

type obsLaneResult struct {
	dataset obsDataset
	orgID   string // org of the lane's campaigns (cursor row); "" = none seen

	scopeCohort map[obsKey]bool
	scopeEvent  map[obsKey]bool

	cohortCells  map[obsCellKey]*obsCell
	eventCells   map[obsCellKey]*obsCell
	cohortExtras map[obsCellKey]*obsLaneExtras // isp field = ""
	eventExtras  map[obsCellKey]*obsLaneExtras

	// familyFailed marks measure families whose source slice errored:
	// their statuses publish as 'failed' (§3.4).
	familyFailed map[string]bool // dispatch|delivery|engagement|conversion

	windowLo, windowHi time.Time
	bootstrap          bool
}

// ---------------------------------------------------------------------------
// The cycle
// ---------------------------------------------------------------------------

func (w *DripObservatoryRollup) runCycle(ctx context.Context) error {
	gate := parseObservatoryLanes(os.Getenv("DRIP_OBSERVATORY_LANES"))
	if gate.empty() {
		log.Println("[DripObservatory] DRIP_OBSERVATORY_LANES empty — worker running, processing nothing")
		return nil
	}

	// §6.5 run deadline.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	// §6.1: ONE reserved session for the whole run.
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve conn: %w", err)
	}
	defer conn.Close()
	got, err := w.acquireLock(ctx, conn)
	if err != nil || !got {
		log.Printf("[DripObservatory] lock held elsewhere or errored (%v) — skipping cycle", err)
		return nil
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer rcancel()
		_, _ = conn.ExecContext(rctx, `SELECT pg_advisory_unlock($1)`, dripObservatoryLockID)
	}()

	// §4 heavy-pass gate.
	if ok, err := w.checkSupportingIndexes(ctx); err != nil || !ok {
		log.Printf("[DripObservatory] supporting-index gate failed (ok=%v err=%v) — refusing heavy pass; see §4 invalid-index recovery", ok, err)
		return nil
	}

	// §6.2 janitor (on the reserved conn — it is a write).
	if err := w.janitor(ctx, conn); err != nil {
		return err
	}

	// §5.9: brandident loaded per cycle (union-never-replacement).
	if err := brandident.RefreshFromDB(ctx, w.db); err != nil {
		log.Printf("[DripObservatory] brandident refresh failed (%v) — compile-time literal stands", err)
	}
	overlay, err := w.loadBrandOverlay(ctx)
	if err != nil {
		log.Printf("[DripObservatory] brand overlay load failed (%v) — resolveBrandSendingDomain fallback only", err)
		overlay = map[string]string{}
	}

	runStart := w.nowFn().UTC()

	// Open the run row (on conn).
	var runID string
	if err := conn.QueryRowContext(ctx, `
		INSERT INTO partner_drip_observatory_runs (operational_day, source_pass)
		VALUES ($1, $2) RETURNING run_id
	`, dripObsDenverDay(runStart), dripObsRollupPass).Scan(&runID); err != nil {
		return fmt.Errorf("open run: %w", err)
	}

	lanes, err := w.loadLanes(ctx, gate)
	if err != nil {
		w.markRunFailed(runID, "load lanes: "+err.Error())
		return err
	}

	q := newObsQuarantine(runID, dripObsRollupPass)
	var (
		results       []*obsLaneResult
		expectedUnits int
		hadErrors     bool
	)

	for _, ds := range lanes {
		res, laneErr := w.processLane(ctx, conn, runID, ds, overlay, q)
		if laneErr != nil {
			log.Printf("[DripObservatory] lane %s failed: %v", ds.id, laneErr)
			hadErrors = true
			q.add(ctx, conn, "lane_failed", ds.id, laneErr.Error())
			// Remove this lane's scope rows so the swap cannot delete
			// prior-generation rows it will not replace.
			if _, derr := conn.ExecContext(ctx, `
				DELETE FROM partner_drip_observatory_run_scope
				WHERE run_id = $1 AND dataset_id = $2
			`, runID, ds.id); derr != nil {
				w.markRunFailed(runID, "lane cleanup failed: "+derr.Error())
				return fmt.Errorf("lane %s cleanup: %w", ds.id, derr)
			}
			continue
		}
		expectedUnits += len(res.scopeCohort) + len(res.scopeEvent)
		if len(res.familyFailed) > 0 {
			hadErrors = true
		}
		results = append(results, res)
	}

	// Staged-fact writes (on conn; §6.1 ownership check per publish step).
	processedUnits := 0
	for _, res := range results {
		if !w.verifyLockOwnership(ctx, conn) {
			w.markRunFailed(runID, "advisory-lock ownership lost before staged write")
			return fmt.Errorf("lock ownership lost (run %s)", runID)
		}
		n, err := w.writeStagedFacts(ctx, conn, runID, res)
		if err != nil {
			w.markRunFailed(runID, "staged write: "+err.Error())
			return fmt.Errorf("staged write lane %s: %w", res.dataset.id, err)
		}
		processedUnits += n
	}

	if q.total > 0 {
		hadErrors = true
	}
	final := "complete"
	if hadErrors {
		final = "complete_with_errors"
	}

	// §6.4 per-kind atomic swap (on conn, ownership check inside the tx).
	if err := w.swapPublish(ctx, conn, runID, final, expectedUnits, processedUnits, q.total); err != nil {
		w.markRunFailed(runID, "swap: "+err.Error())
		return fmt.Errorf("swap: %w", err)
	}

	// Cursors advance only on completion (§6.2).
	for _, res := range results {
		if res.orgID == "" {
			// No campaigns seen ⇒ no org context (§3.6 forbids fallback
			// literals). The empty lane re-bootstraps trivially next cycle.
			continue
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO partner_drip_observatory_cursor
				(source_pass, dataset_id, organization_id, bootstrapped_at, watermark_to)
			VALUES ($1, $2, $3, NOW(), $4)
			ON CONFLICT (source_pass, dataset_id) DO UPDATE
			SET watermark_to = EXCLUDED.watermark_to,
			    organization_id = EXCLUDED.organization_id,
			    bootstrapped_at = COALESCE(partner_drip_observatory_cursor.bootstrapped_at, EXCLUDED.bootstrapped_at)
		`, dripObsRollupPass, res.dataset.id, res.orgID, res.windowHi); err != nil {
			log.Printf("[DripObservatory] cursor advance lane %s failed: %v", res.dataset.id, err)
		}
	}

	log.Printf("[DripObservatory] run %s %s: lanes=%d keys=%d staged_rows=%d quarantined=%d",
		runID, final, len(results), expectedUnits, processedUnits, q.total)
	return nil
}

// loadBrandOverlay mirrors the orchestrator's loadBrandDomains query
// (partner_drip_orchestrator.go:2308-2315) into a CYCLE-LOCAL map — the
// worker never mutates the orchestrator's shared overlay.
func (w *DripObservatoryRollup) loadBrandOverlay(ctx context.Context) (map[string]string, error) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, `
		SELECT lower(btrim(brand_code)), btrim(sending_domain)
		FROM mailing_brand_metadata
		WHERE COALESCE(status, 'active') = 'active'
		  AND COALESCE(btrim(sending_domain), '') <> ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var code, dom string
		if err := rows.Scan(&code, &dom); err != nil {
			return nil, err
		}
		if code != "" {
			out[code] = dom
		}
	}
	return out, rows.Err()
}

func (w *DripObservatoryRollup) loadLanes(ctx context.Context, gate observatoryLaneGate) ([]obsDataset, error) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var (
		rows *sql.Rows
		err  error
	)
	if gate.all {
		rows, err = w.db.QueryContext(qctx, `
			SELECT id, COALESCE(vertical, ''), created_at FROM partner_datasets ORDER BY created_at`)
	} else {
		rows, err = w.db.QueryContext(qctx, `
			SELECT id, COALESCE(vertical, ''), created_at FROM partner_datasets
			WHERE id = ANY($1::uuid[]) ORDER BY created_at`, pq.Array(gate.ids))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []obsDataset
	for rows.Next() {
		var d obsDataset
		if err := rows.Scan(&d.id, &d.vertical, &d.createdAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Per-lane processing: roster+identity, stage-1 discovery, stage-2 recompute
// ---------------------------------------------------------------------------

const dripObsChunk = 8000 // ≤8,000-id chunks (standing caveat)

func (w *DripObservatoryRollup) processLane(ctx context.Context, conn *sql.Conn, runID string,
	ds obsDataset, overlay map[string]string, q *obsQuarantine) (*obsLaneResult, error) {

	res := &obsLaneResult{
		dataset:      ds,
		scopeCohort:  map[obsKey]bool{},
		scopeEvent:   map[obsKey]bool{},
		cohortCells:  map[obsCellKey]*obsCell{},
		eventCells:   map[obsCellKey]*obsCell{},
		cohortExtras: map[obsCellKey]*obsLaneExtras{},
		eventExtras:  map[obsCellKey]*obsLaneExtras{},
		familyFailed: map[string]bool{},
	}
	runStart := w.nowFn().UTC()

	// Window (§6.2): no cursor row ⇒ FULL historical backfill.
	var watermark sql.NullTime
	err := w.db.QueryRowContext(ctx, `
		SELECT watermark_to FROM partner_drip_observatory_cursor
		WHERE source_pass = $1 AND dataset_id = $2
	`, dripObsRollupPass, ds.id).Scan(&watermark)
	switch {
	case err == sql.ErrNoRows:
		res.bootstrap = true
		res.windowLo = ds.createdAt.UTC()
	case err != nil:
		return nil, fmt.Errorf("cursor read: %w", err)
	default:
		res.windowLo = watermark.Time.UTC().Add(-1 * time.Hour)
	}
	res.windowHi = runStart

	// Roster: per-dataset iteration with BOTH bounds (§6.5 — leading
	// equality seeks idx_mc_partner_dataset_sched; the roster is always the
	// lane's FULL campaign history so late events find old campaigns).
	campaigns, err := w.loadRoster(ctx, ds, overlay, q, conn, runID)
	if err != nil {
		return nil, err
	}
	if len(campaigns) > 0 {
		res.orgID = campaigns[0].orgID
	}
	if err := w.resolveFirstDispatch(ctx, ds, campaigns); err != nil {
		return nil, fmt.Errorf("first-dispatch: %w", err)
	}

	byID := make(map[string]*obsCampaign, len(campaigns))
	var rosterIDs []string
	for _, c := range campaigns {
		byID[c.id] = c
		rosterIDs = append(rosterIDs, c.id)
	}

	// -------- Stage 1: DISCOVERY (window finds WHAT changed, §6.3) --------
	if err := w.discover(ctx, ds, res, byID, rosterIDs); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}

	if w.hookAfterDiscovery != nil {
		w.hookAfterDiscovery(ds.id)
	}

	// Scope rows (on conn; §6.1 ownership check first — a publish step).
	if !w.verifyLockOwnership(ctx, conn) {
		return nil, fmt.Errorf("advisory-lock ownership lost before scope write")
	}
	if err := w.writeScope(ctx, conn, runID, ds.id, res); err != nil {
		return nil, fmt.Errorf("scope write: %w", err)
	}

	// -------- Stage 2: full-key recomputation (§6.3) --------
	if err := w.recompute(ctx, ds, res, byID, rosterIDs); err != nil {
		return nil, fmt.Errorf("recompute: %w", err)
	}
	// Planning grain (§3.2): cohort lane rows only, never a denominator.
	fillObsPlanning(res, byID)
	return res, nil
}

// loadRoster enumerates the lane's campaigns and resolves identity: the §7.0
// meta row when present, else the §6.6 chain. Chain failures quarantine.
func (w *DripObservatoryRollup) loadRoster(ctx context.Context, ds obsDataset, overlay map[string]string,
	q *obsQuarantine, conn *sql.Conn, runID string) ([]*obsCampaign, error) {

	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, `
		SELECT id, COALESCE(organization_id::text, ''), name, COALESCE(total_recipients, 0)
		FROM mailing_campaigns
		WHERE partner_dataset_id = $1 AND scheduled_at >= $2 AND scheduled_at < $3
	`, ds.id, ds.createdAt.UTC(), w.nowFn().UTC())
	if err != nil {
		return nil, fmt.Errorf("roster: %w", err)
	}
	defer rows.Close()

	var out []*obsCampaign
	var raw []*obsCampaign
	var rawIDs []string
	for rows.Next() {
		c := &obsCampaign{}
		if err := rows.Scan(&c.id, &c.orgID, &c.name, &c.totalRecipients); err != nil {
			return nil, err
		}
		raw = append(raw, c)
		rawIDs = append(rawIDs, c.id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Meta rows preferred once the §7.0 writer exists (D3b).
	meta, err := w.loadCampaignMeta(ctx, rawIDs)
	if err != nil {
		return nil, err
	}

	for _, c := range raw {
		if c.orgID == "" {
			q.add(ctx, conn, "mapping_error", c.id, "campaign has no organization_id: "+truncateObsSample(c.name, 200))
			continue
		}
		if m, ok := meta[c.id]; ok {
			c.brandCode, c.sendingApex, c.touch = m.brandCode, m.sendingApex, m.touch
			c.tokenCode = m.brandCode
			c.governed = dripObsGovernedApex(m.sendingApex)
			out = append(out, c)
			continue
		}
		vertical, token, touch, ok := parseDripCampaignName(c.name)
		if !ok {
			q.add(ctx, conn, "name_parse_error", c.id, truncateObsSample(c.name, 200))
			continue
		}
		if vertical != ds.vertical {
			q.add(ctx, conn, "name_parse_error", c.id,
				fmt.Sprintf("vertical %q != dataset vertical %q: %s", vertical, ds.vertical, truncateObsSample(c.name, 150)))
			continue
		}
		apex, code, ok := resolveObsBrand(token, overlay)
		if !ok {
			q.add(ctx, conn, "brand_unknown", c.id,
				fmt.Sprintf("token %q unresolvable: %s", token, truncateObsSample(c.name, 150)))
			continue
		}
		c.tokenCode = strings.ToLower(token)
		c.brandCode, c.sendingApex, c.touch = code, apex, touch
		c.governed = dripObsGovernedApex(apex)
		out = append(out, c)
	}
	return out, nil
}

type obsMetaRow struct {
	brandCode, sendingApex string
	touch                  int
}

func (w *DripObservatoryRollup) loadCampaignMeta(ctx context.Context, ids []string) (map[string]obsMetaRow, error) {
	out := map[string]obsMetaRow{}
	for _, chunk := range chunkObsIDs(ids) {
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rows, err := w.db.QueryContext(qctx, `
			SELECT campaign_id, brand_code, sending_apex, touch_number
			FROM partner_drip_campaign_meta WHERE campaign_id = ANY($1::uuid[])
		`, pq.Array(chunk))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("campaign meta: %w", err)
		}
		for rows.Next() {
			var id string
			var m obsMetaRow
			if err := rows.Scan(&id, &m.brandCode, &m.sendingApex, &m.touch); err != nil {
				rows.Close()
				cancel()
				return nil, err
			}
			out[id] = m
		}
		err = rows.Err()
		rows.Close()
		cancel()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// resolveFirstDispatch fills cohortDay + dispatchBasis per §3.1 precedence:
// (1) pcq MIN(mailed_at) → 'pcq'; (2) message_log MIN(sent_at) →
// 'message_log'; (3) MIN(summary_date) → 'acct_terminal' (UTC receipt day,
// disclosed ≤1-day skew); (4) none → no cohort row, basis 'unavailable'.
func (w *DripObservatoryRollup) resolveFirstDispatch(ctx context.Context, ds obsDataset, campaigns []*obsCampaign) error {
	byID := map[string]*obsCampaign{}
	var ids []string
	for _, c := range campaigns {
		byID[c.id] = c
		ids = append(ids, c.id)
		c.dispatchBasis = "unavailable"
	}
	for _, chunk := range chunkObsIDs(ids) {
		// (1) pcq — rides idx_pcq_mailed_campaign.
		if err := w.scanFirstDispatch(ctx, `
			SELECT mailed_campaign_id, MIN(mailed_at) FROM partner_clean_queue
			WHERE mailed_campaign_id = ANY($1::uuid[]) AND mailed_at IS NOT NULL
			GROUP BY 1`, chunk, byID, "pcq", false); err != nil {
			return err
		}
		// (2) message_log (t0 journey sends) — partner_dataset_id equality
		// rides idx_message_log_partner_dataset.
		if err := w.scanFirstDispatchDS(ctx, `
			SELECT campaign_id, MIN(sent_at) FROM mailing_message_log
			WHERE partner_dataset_id = $1 AND campaign_id = ANY($2::uuid[])
			GROUP BY 1`, ds.id, chunk, byID, "message_log"); err != nil {
			return err
		}
		// (3) acct summary — summary_date is a UTC receipt DAY.
		if err := w.scanFirstDispatch(ctx, `
			SELECT campaign_id, MIN(summary_date)::timestamptz FROM pmta_acct_daily_summary
			WHERE campaign_id = ANY($1::uuid[])
			GROUP BY 1`, chunk, byID, "acct_terminal", true); err != nil {
			return err
		}
	}
	return nil
}

func (w *DripObservatoryRollup) scanFirstDispatch(ctx context.Context, query string, chunk []string,
	byID map[string]*obsCampaign, basis string, dateIsDay bool) error {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, query, pq.Array(chunk))
	if err != nil {
		return fmt.Errorf("first-dispatch %s: %w", basis, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return err
		}
		applyFirstDispatch(byID[id], t, basis, dateIsDay)
	}
	return rows.Err()
}

func (w *DripObservatoryRollup) scanFirstDispatchDS(ctx context.Context, query, dsID string, chunk []string,
	byID map[string]*obsCampaign, basis string) error {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, query, dsID, pq.Array(chunk))
	if err != nil {
		return fmt.Errorf("first-dispatch %s: %w", basis, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return err
		}
		applyFirstDispatch(byID[id], t, basis, false)
	}
	return rows.Err()
}

// applyFirstDispatch applies §3.1 precedence: pcq > message_log >
// acct_terminal. Callers run in precedence order; a later (weaker) source
// never overwrites an earlier hit.
func applyFirstDispatch(c *obsCampaign, t time.Time, basis string, dateIsDay bool) {
	if c == nil || c.cohortDay != "" {
		return
	}
	if dateIsDay {
		// summary_date is a calendar DAY (UTC receipt) — use it verbatim,
		// with the §3.1 disclosed ≤1-day skew for t≥2 cohorts.
		c.cohortDay = t.UTC().Format("2006-01-02")
	} else {
		c.cohortDay = dripObsDenverDay(t)
	}
	c.dispatchBasis = basis
}

// discover is stage 1 (§6.3): the window finds WHAT changed — never what to
// count. Every changed fact maps to BOTH its event-day key and its campaign's
// cohort key. Event discovery is roster-constrained (never a global tracking
// scan); tracking + pcq walk the window in Denver-day chunks; acct rides
// last_updated_at over the whole window (that is exactly how out-of-window
// corrections re-enter scope, §11 #17); conversions ride converted_at.
func (w *DripObservatoryRollup) discover(ctx context.Context, ds obsDataset, res *obsLaneResult,
	byID map[string]*obsCampaign, rosterIDs []string) error {

	scopeBoth := func(c *obsCampaign, eventDay string) {
		if c == nil {
			return
		}
		if eventDay != "" {
			res.scopeEvent[obsKey{org: c.orgID, day: eventDay}] = true
		}
		if c.cohortDay != "" {
			res.scopeCohort[obsKey{org: c.orgID, day: c.cohortDay}] = true
		}
	}

	// Day-chunked walks (tracking + pcq).
	for day := dripObsDenverDay(res.windowLo); ; {
		lo, hi, err := dripObsDayBounds(day)
		if err != nil {
			return err
		}
		if !lo.Before(res.windowHi) {
			break
		}
		qLo, qHi := maxObsTime(lo, res.windowLo), minObsTime(hi, res.windowHi)

		for _, chunk := range chunkObsIDs(rosterIDs) {
			qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			rows, err := w.db.QueryContext(qctx, `
				SELECT DISTINCT campaign_id FROM mailing_tracking_events
				WHERE campaign_id = ANY($1::uuid[]) AND event_at >= $2 AND event_at < $3
			`, pq.Array(chunk), qLo, qHi)
			if err != nil {
				cancel()
				return fmt.Errorf("tracking discovery: %w", err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					cancel()
					return err
				}
				scopeBoth(byID[id], day)
			}
			err = rows.Err()
			rows.Close()
			cancel()
			if err != nil {
				return err
			}
		}

		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rows, err := w.db.QueryContext(qctx, `
			SELECT DISTINCT mailed_campaign_id FROM partner_clean_queue
			WHERE dataset_id = $1 AND mailed_at >= $2 AND mailed_at < $3
			  AND mailed_campaign_id IS NOT NULL
		`, ds.id, qLo, qHi)
		if err != nil {
			cancel()
			return fmt.Errorf("pcq discovery: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				cancel()
				return err
			}
			scopeBoth(byID[id], day)
		}
		err = rows.Err()
		rows.Close()
		cancel()
		if err != nil {
			return err
		}

		day = hi.In(dripObsDenver()).Format("2006-01-02")
	}

	// acct change discovery: last_updated_at is stamped on every upsert
	// (summary_builder.go:223) — the delivery change-discovery cursor (§6.3).
	for _, chunk := range chunkObsIDs(rosterIDs) {
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rows, err := w.db.QueryContext(qctx, `
			SELECT DISTINCT campaign_id, summary_date FROM pmta_acct_daily_summary
			WHERE campaign_id = ANY($1::uuid[]) AND last_updated_at >= $2 AND last_updated_at < $3
		`, pq.Array(chunk), res.windowLo, res.windowHi)
		if err != nil {
			cancel()
			return fmt.Errorf("acct discovery: %w", err)
		}
		for rows.Next() {
			var id string
			var sd time.Time
			if err := rows.Scan(&id, &sd); err != nil {
				rows.Close()
				cancel()
				return err
			}
			// Delivery event-day basis = summary_date verbatim (UTC receipt
			// day, §3.5) — labeled by the column COMMENTs.
			scopeBoth(byID[id], sd.UTC().Format("2006-01-02"))
		}
		err = rows.Err()
		rows.Close()
		cancel()
		if err != nil {
			return err
		}

		qctx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
		rows2, err := w.db.QueryContext(qctx2, `
			SELECT campaign_id, converted_at FROM mailing_everflow_conversions
			WHERE campaign_id = ANY($1::uuid[]) AND converted_at >= $2 AND converted_at < $3
		`, pq.Array(chunk), res.windowLo, res.windowHi)
		if err != nil {
			cancel2()
			return fmt.Errorf("conversion discovery: %w", err)
		}
		for rows2.Next() {
			var id string
			var at time.Time
			if err := rows2.Scan(&id, &at); err != nil {
				rows2.Close()
				cancel2()
				return err
			}
			scopeBoth(byID[id], dripObsDenverDay(at))
		}
		err = rows2.Err()
		rows2.Close()
		cancel2()
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *DripObservatoryRollup) writeScope(ctx context.Context, conn *sql.Conn, runID, dsID string, res *obsLaneResult) error {
	write := func(kind string, keys map[obsKey]bool) error {
		for k := range keys {
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO partner_drip_observatory_run_scope (run_id, fact_kind, organization_id, day, dataset_id)
				VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING
			`, runID, kind, k.org, k.day, dsID); err != nil {
				return err
			}
		}
		return nil
	}
	if err := write("cohort", res.scopeCohort); err != nil {
		return err
	}
	return write("event", res.scopeEvent)
}

// recompute is stage 2 (§6.3): every scoped key recomputes from its COMPLETE
// source population. One Denver-day walk from the earliest scoped day to now
// covers both fact kinds: each day's engagement/dispatch/conversion facts are
// attributed to that day's EVENT key (when scoped) AND to the owning
// campaign's COHORT key (when scoped — first-dispatch day, possibly weeks
// earlier). The per-campaign-day granularity of this walk is also what the
// §3.3 canonical click computation requires. The acct pass attributes by
// summary_date (event) and campaign (cohort) in one bounded scan.
func (w *DripObservatoryRollup) recompute(ctx context.Context, ds obsDataset, res *obsLaneResult,
	byID map[string]*obsCampaign, rosterIDs []string) error {

	minDay := ""
	for k := range res.scopeCohort {
		if minDay == "" || k.day < minDay {
			minDay = k.day
		}
	}
	for k := range res.scopeEvent {
		if minDay == "" || k.day < minDay {
			minDay = k.day
		}
	}
	if minDay == "" {
		return nil // nothing scoped — cursor still advances (empty window)
	}
	today := dripObsDenverDay(w.nowFn())

	for day := minDay; day <= today; {
		lo, hi, err := dripObsDayBounds(day)
		if err != nil {
			return err
		}
		if err := w.recomputeDay(ctx, ds, res, byID, rosterIDs, day, lo, minObsTime(hi, res.windowHi)); err != nil {
			return err
		}
		day = hi.In(dripObsDenver()).Format("2006-01-02")
	}

	return w.recomputeAcct(ctx, res, byID, rosterIDs, minDay)
}

func (w *DripObservatoryRollup) recomputeDay(ctx context.Context, ds obsDataset, res *obsLaneResult,
	byID map[string]*obsCampaign, rosterIDs []string, day string, lo, hi time.Time) error {

	if !lo.Before(hi) {
		return nil
	}

	// Per-key row guard (§6.5): pre-count > 5M ⇒ the day's keys fail closed.
	for _, chunk := range chunkObsIDs(rosterIDs) {
		var n int
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := w.db.QueryRowContext(qctx, `
			SELECT COUNT(*) FROM mailing_tracking_events
			WHERE campaign_id = ANY($1::uuid[]) AND event_at >= $2 AND event_at < $3
		`, pq.Array(chunk), lo, hi).Scan(&n)
		cancel()
		if err != nil {
			res.familyFailed["engagement"] = true
			return nil
		}
		if n > 5_000_000 {
			res.familyFailed["engagement"] = true
			log.Printf("[DripObservatory] lane %s day %s exceeds 5M-row guard (%d) — engagement marked failed", ds.id, day, n)
			return nil
		}
	}

	// Engagement (§6.6 canonical SQL — past-tense types; both event_at
	// bounds; ISP from recipient_domain via isp.GroupFromDomain in Go;
	// never is_machine_*). Runs as a single-query SET LOCAL transaction
	// (§6.5 timeout contract; the scorecard idiom, scorecard.go:76).
	type clickAgg struct {
		wrapper, money           map[string]int // isp -> count
		wrapperHuman, moneyHuman map[string]int
	}
	clicks := map[string]*clickAgg{} // campaign -> agg (this Denver day)

	for _, chunk := range chunkObsIDs(rosterIDs) {
		err := w.withHeavyTx(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `
				SELECT campaign_id, event_type, COALESCE(recipient_domain, ''), COALESCE(bounce_type, ''), COALESCE(link_url, ''),
				       COUNT(*) AS events,
				       COUNT(*) FILTER (WHERE event_type = 'clicked'
				         AND ignite_verdict_is_human(COALESCE(click_verdict, ignite_event_verdict(user_agent, ip_address)))) AS human
				FROM mailing_tracking_events
				WHERE campaign_id = ANY($1::uuid[]) AND event_at >= $2 AND event_at < $3
				  AND event_type IN ('opened','clicked','unsubscribed','bounced')
				GROUP BY 1,2,3,4,5
			`, pq.Array(chunk), lo, hi)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var campID, etype, rdom, btype, link string
				var events, human int
				if err := rows.Scan(&campID, &etype, &rdom, &btype, &link, &events, &human); err != nil {
					return err
				}
				c := byID[campID]
				if c == nil {
					continue
				}
				ispGroup := isp.GroupFromDomain(rdom)
				switch etype {
				case "opened":
					w.bump(res, c, day, ispGroup, func(cell *obsCell) { cell.opens += events })
				case "unsubscribed":
					w.bump(res, c, day, ispGroup, func(cell *obsCell) { cell.unsubs += events })
				case "bounced":
					// hard/soft/reputation come ONLY from acct (§3.8);
					// tracking feeds validation_blocked alone.
					if btype == "validation" {
						w.bump(res, c, day, ispGroup, func(cell *obsCell) { cell.validation += events })
					}
				case "clicked":
					kind := classifyClickURL(link)
					agg := clicks[campID]
					if agg == nil {
						agg = &clickAgg{wrapper: map[string]int{}, money: map[string]int{},
							wrapperHuman: map[string]int{}, moneyHuman: map[string]int{}}
						clicks[campID] = agg
					}
					switch kind {
					case "wrapper":
						agg.wrapper[ispGroup] += events
						agg.wrapperHuman[ispGroup] += human
						w.bump(res, c, day, ispGroup, func(cell *obsCell) { cell.clicksWrapper += events })
					case "money":
						agg.money[ispGroup] += events
						agg.moneyHuman[ispGroup] += human
						w.bump(res, c, day, ispGroup, func(cell *obsCell) { cell.clicksMoney += events })
					}
					// "other" click rows (unsub /integration/ links etc.)
					// join neither raw column nor any basis (§3.3).
				}
			}
			return rows.Err()
		})
		if err != nil {
			log.Printf("[DripObservatory] engagement slice failed lane %s day %s: %v", ds.id, day, err)
			res.familyFailed["engagement"] = true
		}
	}

	// §3.3 canonical actions, PER CAMPAIGN-DAY: wrapper count if >0, else
	// money count (resolved-only fallback). human_clicks over the same rows
	// as the campaign-day's chosen basis.
	for campID, agg := range clicks {
		c := byID[campID]
		wrapperTotal := 0
		for _, n := range agg.wrapper {
			wrapperTotal += n
		}
		if wrapperTotal > 0 {
			for g, n := range agg.wrapper {
				n, g := n, g
				w.bump(res, c, day, g, func(cell *obsCell) { cell.actionsW += n; cell.humanClicks += agg.wrapperHuman[g] })
			}
		} else {
			for g, n := range agg.money {
				n, g := n, g
				w.bump(res, c, day, g, func(cell *obsCell) { cell.actionsR += n; cell.humanClicks += agg.moneyHuman[g] })
			}
		}
	}

	// Dispatch, pcq basis (t1): rides idx_pcq_dataset_mailed_at.
	{
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rows, err := w.db.QueryContext(qctx, `
			SELECT mailed_campaign_id, COALESCE(isp_family, ''), COUNT(*)
			FROM partner_clean_queue
			WHERE dataset_id = $1 AND mailed_at >= $2 AND mailed_at < $3
			  AND mailed_campaign_id IS NOT NULL
			GROUP BY 1, 2
		`, ds.id, lo, hi)
		if err != nil {
			cancel()
			log.Printf("[DripObservatory] pcq dispatch slice failed lane %s day %s: %v", ds.id, day, err)
			res.familyFailed["dispatch"] = true
		} else {
			for rows.Next() {
				var campID, fam string
				var n int
				if err := rows.Scan(&campID, &fam, &n); err != nil {
					rows.Close()
					cancel()
					return err
				}
				add := n
				w.bump(res, byID[campID], day, dripObsNormalizeISPFamily(fam), func(cell *obsCell) { cell.dispatchPcq += add })
			}
			err = rows.Err()
			rows.Close()
			cancel()
			if err != nil {
				return err
			}
		}
	}

	// Dispatch, message_log basis (t0): ISP via isp.GroupFromDomain in Go.
	{
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rows, err := w.db.QueryContext(qctx, `
			SELECT campaign_id, COALESCE(email, '') FROM mailing_message_log
			WHERE partner_dataset_id = $1 AND sent_at >= $2 AND sent_at < $3 AND campaign_id IS NOT NULL
		`, ds.id, lo, hi)
		if err != nil {
			cancel()
			log.Printf("[DripObservatory] message_log dispatch slice failed lane %s day %s: %v", ds.id, day, err)
			res.familyFailed["dispatch"] = true
		} else {
			for rows.Next() {
				var campID, email string
				if err := rows.Scan(&campID, &email); err != nil {
					rows.Close()
					cancel()
					return err
				}
				w.bump(res, byID[campID], day, dripObsISPFromEmail(email), func(cell *obsCell) { cell.dispatchMsgLog++ })
			}
			err = rows.Err()
			rows.Close()
			cancel()
			if err != nil {
				return err
			}
		}
	}

	// Conversions (campaign grain → lane rows only, §6.6).
	for _, chunk := range chunkObsIDs(rosterIDs) {
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rows, err := w.db.QueryContext(qctx, `
			SELECT campaign_id, COUNT(*), COALESCE(SUM(payout), 0)
			FROM mailing_everflow_conversions
			WHERE campaign_id = ANY($1::uuid[]) AND converted_at >= $2 AND converted_at < $3
			GROUP BY 1
		`, pq.Array(chunk), lo, hi)
		if err != nil {
			cancel()
			log.Printf("[DripObservatory] conversion slice failed lane %s day %s: %v", ds.id, day, err)
			res.familyFailed["conversion"] = true
			continue
		}
		for rows.Next() {
			var campID string
			var n int
			var revenue float64
			if err := rows.Scan(&campID, &n, &revenue); err != nil {
				rows.Close()
				cancel()
				return err
			}
			c := byID[campID]
			if c == nil {
				continue
			}
			addN, addRev := n, revenue
			w.bumpExtras(res, c, day, func(x *obsLaneExtras) { x.conversions += addN; x.revenue += addRev })
		}
		err = rows.Err()
		rows.Close()
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

// recomputeAcct attributes acct daily-summary rows: event day = summary_date
// verbatim (UTC receipt day, §3.5); cohort = the campaign's first-dispatch
// day. Delivered follows metrics.go semantics — SUM(delivered) +
// SUM(ses_delivered), never another formula. hard/soft/reputation are three
// DISTINCT classes, never summed.
func (w *DripObservatoryRollup) recomputeAcct(ctx context.Context, res *obsLaneResult,
	byID map[string]*obsCampaign, rosterIDs []string, minDay string) error {

	today := w.nowFn().UTC().Format("2006-01-02")
	for _, chunk := range chunkObsIDs(rosterIDs) {
		err := w.withHeavyTx(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `
				SELECT summary_date, campaign_id, COALESCE(recipient_isp, ''),
				       COALESCE(SUM(delivered),0) + COALESCE(SUM(ses_delivered),0) AS delivered,
				       COALESCE(SUM(relayed_to_ses),0)     AS relayed_to_ses,
				       COALESCE(SUM(hard_bounced),0)       AS hard_bounced,
				       COALESCE(SUM(soft_bounced),0)       AS soft_bounced,
				       COALESCE(SUM(reputation_blocked),0) AS reputation_blocked,
				       COALESCE(SUM(complained),0)         AS complained
				FROM pmta_acct_daily_summary
				WHERE campaign_id = ANY($1::uuid[]) AND summary_date >= $2 AND summary_date <= $3
				GROUP BY 1, 2, 3
			`, pq.Array(chunk), minDay, today)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var sd time.Time
				var campID, rispRaw string
				var delivered, relayed, hard, soft, rep, complained int
				if err := rows.Scan(&sd, &campID, &rispRaw, &delivered, &relayed, &hard, &soft, &rep, &complained); err != nil {
					return err
				}
				c := byID[campID]
				if c == nil {
					continue
				}
				day := sd.UTC().Format("2006-01-02")
				// recipient_isp is written by isp.GroupFromDomain
				// (summary_builder.go:156) — normalize defensively anyway.
				risp := dripObsNormalizeISPFamily(rispRaw)
				w.bump(res, c, day, risp, func(cell *obsCell) {
					cell.delivered += delivered
					cell.relayedToSES += relayed
					cell.hardBounced += hard
					cell.softBounced += soft
					cell.repBlocked += rep
					cell.complained += complained
					cell.hasAcct = true
				})
			}
			return rows.Err()
		})
		if err != nil {
			log.Printf("[DripObservatory] acct slice failed: %v", err)
			res.familyFailed["delivery"] = true
		}
	}
	return nil
}

// bump applies fn to the campaign's COHORT cell (if its cohort key is
// scoped) and to the day's EVENT cell (if that event key is scoped) — the
// §6.3 dual attribution.
func (w *DripObservatoryRollup) bump(res *obsLaneResult, c *obsCampaign, eventDay, ispGroup string, fn func(*obsCell)) {
	if c == nil {
		return
	}
	if c.cohortDay != "" && res.scopeCohort[obsKey{org: c.orgID, day: c.cohortDay}] {
		fn(obsCellFor(res.cohortCells, res.cohortExtras, c, c.cohortDay, ispGroup))
	}
	if res.scopeEvent[obsKey{org: c.orgID, day: eventDay}] {
		fn(obsCellFor(res.eventCells, res.eventExtras, c, eventDay, ispGroup))
	}
}

func (w *DripObservatoryRollup) bumpExtras(res *obsLaneResult, c *obsCampaign, eventDay string, fn func(*obsLaneExtras)) {
	if c == nil {
		return
	}
	if c.cohortDay != "" && res.scopeCohort[obsKey{org: c.orgID, day: c.cohortDay}] {
		fn(obsExtrasFor(res.cohortExtras, c, c.cohortDay))
	}
	if res.scopeEvent[obsKey{org: c.orgID, day: eventDay}] {
		fn(obsExtrasFor(res.eventExtras, c, eventDay))
	}
}

func obsCellFor(cells map[obsCellKey]*obsCell, extras map[obsCellKey]*obsLaneExtras, c *obsCampaign, day, ispGroup string) *obsCell {
	k := obsCellKey{org: c.orgID, day: day, touch: c.touch, brand: c.brandCode, isp: ispGroup}
	cell := cells[k]
	if cell == nil {
		cell = &obsCell{apex: c.sendingApex, governed: c.governed}
		cells[k] = cell
	}
	// Ensure the lane-extras row exists so basis/status carry even for
	// groups with no conversions/planning yet.
	obsExtrasFor(extras, c, day)
	return cell
}

func obsExtrasFor(extras map[obsCellKey]*obsLaneExtras, c *obsCampaign, day string) *obsLaneExtras {
	k := obsCellKey{org: c.orgID, day: day, touch: c.touch, brand: c.brandCode, isp: ""}
	x := extras[k]
	if x == nil {
		x = &obsLaneExtras{basisSet: map[string]bool{}, apex: c.sendingApex, governed: c.governed}
		extras[k] = x
	}
	x.basisSet[c.dispatchBasis] = true
	return x
}

// withHeavyTx runs fn inside a single-query transaction with SET LOCAL
// statement_timeout='300s' (§6.5; SET LOCAL requires a tx — the scorecard
// idiom, internal/domainagent/scorecard.go:76).
func (w *DripObservatoryRollup) withHeavyTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '300s'`); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Flush: cells → fact rows (writer sets EVERY status/basis explicitly, §3.4)
// ---------------------------------------------------------------------------

type obsFactRow struct {
	org, day, vertical      string
	touch                   int
	brand, apex             string
	scope, ispVal           string // dimension_scope, isp
	recipientsPlanned       sql.NullInt64
	dispatched              sql.NullInt64
	dispatchBasis           string
	delivered               sql.NullInt64
	hard, soft, rep         sql.NullInt64
	validation, complained  sql.NullInt64
	opens                   sql.NullInt64
	clicksWrapper           sql.NullInt64
	clicksMoney             sql.NullInt64
	actionsW, actionsR      int
	humanClicks             int
	clickBasis              string
	unsubs                  sql.NullInt64
	conversions             sql.NullInt64
	revenue                 sql.NullFloat64
	stDispatch, stDelivery  string
	stBounce, stEngagement  string
	stConversion            string
}

func obsInt(n int) sql.NullInt64 { return sql.NullInt64{Int64: int64(n), Valid: true} }

// dispatchGroupBasis resolves a (touch, brand) group's dispatch basis by
// §3.1 precedence over its contributing campaigns' bases. Mixed bases inside
// one group publish the strongest basis with dispatch_status='partial'.
func dispatchGroupBasis(basisSet map[string]bool) (basis string, mixed bool) {
	order := []string{"pcq", "message_log", "acct_terminal"}
	seen := 0
	for _, b := range order {
		if basisSet[b] {
			seen++
			if basis == "" {
				basis = b
			}
		}
	}
	if basis == "" {
		return "unavailable", false
	}
	// Mixed bases — or a real basis alongside evidence-less campaigns —
	// publish the strongest basis with dispatch_status='partial' (§3.4).
	return basis, seen > 1 || basisSet["unavailable"]
}

func clickBasisLabel(actionsW, actionsR int) string {
	switch {
	case actionsW == 0 && actionsR == 0:
		return "none"
	case actionsR == 0:
		return "wrapper"
	case actionsW == 0:
		return "resolved-only"
	default:
		return "mixed"
	}
}

// buildFactRows converts one fact kind's cells into rows: lane_isp rows plus
// ONE lane row per (org, day, touch, brand) — the lane row is the ISP-row sum
// and additionally carries planning (§3.2, cohort only) and conversions
// (§6.6, lane grain).
func buildFactRows(kind string, ds obsDataset, cells map[obsCellKey]*obsCell,
	extras map[obsCellKey]*obsLaneExtras, familyFailed map[string]bool) []obsFactRow {

	laneAgg := map[obsCellKey]*obsCell{}
	var keys []obsCellKey
	for k := range cells {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.day != b.day {
			return a.day < b.day
		}
		if a.brand != b.brand {
			return a.brand < b.brand
		}
		if a.touch != b.touch {
			return a.touch < b.touch
		}
		return a.isp < b.isp
	})

	var rows []obsFactRow
	makeRow := func(k obsCellKey, cell *obsCell, scope string, x *obsLaneExtras) obsFactRow {
		basis, mixed := "unavailable", false
		if x != nil {
			basis, mixed = dispatchGroupBasis(x.basisSet)
		}
		r := obsFactRow{
			org: k.org, day: k.day, vertical: ds.vertical, touch: k.touch,
			brand: k.brand, apex: cell.apex, scope: scope, ispVal: k.isp,
			dispatchBasis: basis,
			actionsW:      cell.actionsW, actionsR: cell.actionsR,
			humanClicks: cell.humanClicks,
			clickBasis:  clickBasisLabel(cell.actionsW, cell.actionsR),
			opens:       obsInt(cell.opens),
			clicksWrapper: obsInt(cell.clicksWrapper), clicksMoney: obsInt(cell.clicksMoney),
			unsubs:     obsInt(cell.unsubs),
			validation: obsInt(cell.validation),
		}
		// Dispatch count per the group's basis; 'acct_terminal' = delivered +
		// relayed_to_ses + hard + reputation + soft + complained — NEVER
		// total_records (records ≠ messages, §15.3).
		switch basis {
		case "pcq":
			r.dispatched = obsInt(cell.dispatchPcq)
			r.stDispatch = "available"
		case "message_log":
			r.dispatched = obsInt(cell.dispatchMsgLog)
			r.stDispatch = "available"
		case "acct_terminal":
			r.dispatched = obsInt(cell.delivered + cell.relayedToSES + cell.hardBounced + cell.repBlocked + cell.softBounced + cell.complained)
			r.stDispatch = "available"
		default:
			r.stDispatch = "unavailable"
		}
		if mixed {
			r.stDispatch = "partial"
		}
		// Delivery + bounce (§3.4 standing rules): governed/kumo lanes have
		// no acct coverage → NULL + 'unavailable'; SES-routed lanes carry
		// PMTA-direct bounce classes only → bounce 'partial'.
		if cell.governed {
			r.stDelivery, r.stBounce = "unavailable", "unavailable"
		} else {
			r.delivered = obsInt(cell.delivered)
			r.hard = obsInt(cell.hardBounced)
			r.soft = obsInt(cell.softBounced)
			r.rep = obsInt(cell.repBlocked)
			r.complained = obsInt(cell.complained)
			r.stDelivery, r.stBounce = "available", "partial"
		}
		r.stEngagement = "available"
		r.stConversion = "available"
		if scope == "lane" && x != nil {
			r.conversions = obsInt(x.conversions)
			r.revenue = sql.NullFloat64{Float64: x.revenue, Valid: true}
			if kind == "cohort" {
				r.recipientsPlanned = obsInt(x.recipientsPlanned)
			}
		}
		// Failed source slices publish as 'failed' for the whole family
		// (§3.4) — statuses are honest before counts are convenient.
		if familyFailed["dispatch"] {
			r.stDispatch = "failed"
		}
		if familyFailed["delivery"] && !cell.governed {
			r.stDelivery, r.stBounce = "failed", "failed"
		}
		if familyFailed["engagement"] {
			r.stEngagement = "failed"
		}
		if familyFailed["conversion"] {
			r.stConversion = "failed"
		}
		return r
	}

	for _, k := range keys {
		cell := cells[k]
		rows = append(rows, makeRow(k, cell, "lane_isp", extras[obsCellKey{org: k.org, day: k.day, touch: k.touch, brand: k.brand}]))
		lk := obsCellKey{org: k.org, day: k.day, touch: k.touch, brand: k.brand}
		agg := laneAgg[lk]
		if agg == nil {
			agg = &obsCell{apex: cell.apex, governed: cell.governed}
			laneAgg[lk] = agg
		}
		addObsCell(agg, cell)
	}
	// Lane rows for groups that ONLY have lane-grain facts (e.g. conversions
	// with no ISP-grain events) still publish.
	for k, x := range extras {
		if _, ok := laneAgg[k]; !ok {
			laneAgg[k] = &obsCell{apex: x.apex, governed: x.governed}
		}
	}
	var laneKeys []obsCellKey
	for k := range laneAgg {
		laneKeys = append(laneKeys, k)
	}
	sort.Slice(laneKeys, func(i, j int) bool {
		a, b := laneKeys[i], laneKeys[j]
		if a.day != b.day {
			return a.day < b.day
		}
		if a.brand != b.brand {
			return a.brand < b.brand
		}
		return a.touch < b.touch
	})
	for _, k := range laneKeys {
		rows = append(rows, makeRow(k, laneAgg[k], "lane", extras[k]))
	}
	return rows
}

func addObsCell(dst, src *obsCell) {
	dst.dispatchPcq += src.dispatchPcq
	dst.dispatchMsgLog += src.dispatchMsgLog
	dst.delivered += src.delivered
	dst.relayedToSES += src.relayedToSES
	dst.hardBounced += src.hardBounced
	dst.softBounced += src.softBounced
	dst.repBlocked += src.repBlocked
	dst.validation += src.validation
	dst.complained += src.complained
	dst.opens += src.opens
	dst.clicksWrapper += src.clicksWrapper
	dst.clicksMoney += src.clicksMoney
	dst.actionsW += src.actionsW
	dst.actionsR += src.actionsR
	dst.humanClicks += src.humanClicks
	dst.unsubs += src.unsubs
	dst.hasAcct = dst.hasAcct || src.hasAcct
}

// writeStagedFacts stages this generation's rows under run_id (on the
// reserved conn). recipients_planned is filled per campaign group before the
// build (cohort lane rows only, §3.2).
func (w *DripObservatoryRollup) writeStagedFacts(ctx context.Context, conn *sql.Conn, runID string, res *obsLaneResult) (int, error) {
	cohortRows := buildFactRows("cohort", res.dataset, res.cohortCells, res.cohortExtras, res.familyFailed)
	eventRows := buildFactRows("event", res.dataset, res.eventCells, res.eventExtras, res.familyFailed)

	n := 0
	for _, r := range cohortRows {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO partner_drip_send_cohort_daily
				(run_id, organization_id, cohort_day, dataset_id, vertical, touch_number,
				 brand_code, sending_apex, dimension_scope, isp,
				 recipients_planned, messages_dispatched_actual, dispatch_basis,
				 delivered, hard_bounced, soft_bounced, reputation_blocked,
				 validation_blocked, complained, opens, clicks_wrapper, clicks_money,
				 click_actions_wrapper_basis, click_actions_resolved_basis, click_actions_total,
				 human_clicks, click_basis, unsubs, conversions, revenue,
				 dispatch_status, delivery_status, bounce_status, engagement_status, conversion_status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,
			        $23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35)
		`, runID, r.org, r.day, res.dataset.id, r.vertical, r.touch,
			r.brand, r.apex, r.scope, r.ispVal,
			r.recipientsPlanned, r.dispatched, r.dispatchBasis,
			r.delivered, r.hard, r.soft, r.rep,
			r.validation, r.complained, r.opens, r.clicksWrapper, r.clicksMoney,
			r.actionsW, r.actionsR, r.actionsW+r.actionsR,
			r.humanClicks, r.clickBasis, r.unsubs, r.conversions, r.revenue,
			r.stDispatch, r.stDelivery, r.stBounce, r.stEngagement, r.stConversion); err != nil {
			return n, fmt.Errorf("cohort insert (%s %s t%d %s %q): %w", r.day, r.brand, r.touch, r.scope, r.ispVal, err)
		}
		n++
	}
	for _, r := range eventRows {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO partner_drip_event_daily
				(run_id, organization_id, event_day, dataset_id, vertical, touch_number,
				 brand_code, sending_apex, dimension_scope, isp,
				 messages_dispatched_actual, dispatch_basis,
				 delivered, hard_bounced, soft_bounced, reputation_blocked,
				 validation_blocked, complained, opens, clicks_wrapper, clicks_money,
				 click_actions_wrapper_basis, click_actions_resolved_basis, click_actions_total,
				 human_clicks, click_basis, unsubs, conversions, revenue,
				 dispatch_status, delivery_status, bounce_status, engagement_status, conversion_status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,
			        $22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34)
		`, runID, r.org, r.day, res.dataset.id, r.vertical, r.touch,
			r.brand, r.apex, r.scope, r.ispVal,
			r.dispatched, r.dispatchBasis,
			r.delivered, r.hard, r.soft, r.rep,
			r.validation, r.complained, r.opens, r.clicksWrapper, r.clicksMoney,
			r.actionsW, r.actionsR, r.actionsW+r.actionsR,
			r.humanClicks, r.clickBasis, r.unsubs, r.conversions, r.revenue,
			r.stDispatch, r.stDelivery, r.stBounce, r.stEngagement, r.stConversion); err != nil {
			return n, fmt.Errorf("event insert (%s %s t%d %s %q): %w", r.day, r.brand, r.touch, r.scope, r.ispVal, err)
		}
		n++
	}
	return n, nil
}

// fillPlanning sums mailing_campaigns.total_recipients into the cohort lane
// extras — plan-grain, NEVER a denominator (§3.2).
func fillObsPlanning(res *obsLaneResult, campaigns map[string]*obsCampaign) {
	for _, c := range campaigns {
		if c.cohortDay == "" || !res.scopeCohort[obsKey{org: c.orgID, day: c.cohortDay}] {
			continue
		}
		obsExtrasFor(res.cohortExtras, c, c.cohortDay).recipientsPlanned += c.totalRecipients
	}
}

// ---------------------------------------------------------------------------
// §6.4 swap — one transaction, ON the reserved conn, this pass's kinds only
// ---------------------------------------------------------------------------

func (w *DripObservatoryRollup) swapPublish(ctx context.Context, conn *sql.Conn, runID, final string,
	expected, processed, quarantined int) error {

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// §6.1: ownership verified INSIDE the swap tx, on this conn.
	var owned bool
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) = 1 FROM pg_locks
		WHERE locktype = 'advisory' AND objid = $1 AND pid = pg_backend_pid() AND granted
	`, dripObservatoryLockID).Scan(&owned); err != nil || !owned {
		return fmt.Errorf("advisory-lock ownership lost before swap (owned=%v err=%v)", owned, err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE partner_drip_observatory_runs
		SET status = $2, completed_at = NOW(),
		    expected_units = $3, processed_units = $4, quarantined_units = $5
		WHERE run_id = $1
	`, runID, final, expected, processed, quarantined); err != nil {
		return fmt.Errorf("swap run update: %w", err)
	}

	swaps := []struct{ sql string }{
		{`DELETE FROM partner_drip_send_cohort_daily d
			USING partner_drip_observatory_run_scope s
			WHERE s.run_id = $1 AND s.fact_kind = 'cohort' AND d.run_id <> $1
			  AND d.organization_id = s.organization_id AND d.cohort_day = s.day AND d.dataset_id = s.dataset_id`},
		{`DELETE FROM partner_drip_event_daily d
			USING partner_drip_observatory_run_scope s
			WHERE s.run_id = $1 AND s.fact_kind = 'event' AND d.run_id <> $1
			  AND d.organization_id = s.organization_id AND d.event_day = s.day AND d.dataset_id = s.dataset_id`},
		{`DELETE FROM partner_drip_hygiene_daily d
			USING partner_drip_observatory_run_scope s
			WHERE s.run_id = $1 AND s.fact_kind = 'hygiene' AND d.run_id <> $1
			  AND d.organization_id = s.organization_id AND d.cohort_day = s.day AND d.dataset_id = s.dataset_id`},
	}
	for _, s := range swaps {
		if _, err := tx.ExecContext(ctx, s.sql, runID); err != nil {
			return fmt.Errorf("swap delete: %w", err)
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Quarantine (§5.10b): ≤100 sample rows per (run, reason); overflow counts.
// ---------------------------------------------------------------------------

type obsQuarantine struct {
	runID, pass string
	perReason   map[string]int
	total       int
}

func newObsQuarantine(runID, pass string) *obsQuarantine {
	return &obsQuarantine{runID: runID, pass: pass, perReason: map[string]int{}}
}

func (q *obsQuarantine) add(ctx context.Context, conn *sql.Conn, reason, key, sample string) {
	q.total++
	if q.perReason[reason] >= 100 {
		return // overflow counts in runs.quarantined_units only
	}
	q.perReason[reason]++
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO partner_drip_observatory_quarantine (run_id, source_pass, reason, quarantine_key, sample)
		VALUES ($1, $2, $3, $4, $5)
	`, q.runID, q.pass, reason, key, truncateObsSample(sample, 500)); err != nil {
		log.Printf("[DripObservatory] quarantine write failed (%s/%s): %v", reason, key, err)
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func chunkObsIDs(ids []string) [][]string {
	if len(ids) == 0 {
		return nil
	}
	var out [][]string
	for len(ids) > dripObsChunk {
		out = append(out, ids[:dripObsChunk])
		ids = ids[dripObsChunk:]
	}
	return append(out, ids)
}

func maxObsTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minObsTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
