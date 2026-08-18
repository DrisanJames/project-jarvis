package worker

// SESVDMSnapshotWorker (Vector A plan rev4, Step 16) — UTC-day SES VDM
// identity telemetry snapshots into ses_vdm_daily.
//
// Every 6 hours, under a distlock lease SIZED FOR THE WORST-CASE 14-day
// catch-up (estimate computed and logged at run start):
//
//  1. WORK SET = missing/unfinalized UTC days generated from the LAST
//     COMPLETED run day in ses_vdm_snapshot_runs (a fully-absent day is
//     discoverable ONLY through the run table — row-based detection can't see
//     a day with zero rows), walked forward to yesterday, UNIONed with days
//     that still carry unfinalized rows, bounded to the most recent 14; plus
//     today-so-far. Finalized rows are excluded from work sets up front AND
//     protected at the write (I-7).
//  2. Per day: a run row (I-8; expected_cells = |identities| × |canonical
//     groups queried|); for each identity × raw AWS ISP name ∈
//     (isp.VDMISPs() ∪ SES_VDM_ISPS env), fetch via
//     ses.GetMetricsForIdentityISP with a 300ms pause; per-cell errors are
//     tolerated and recorded, never fatal to the day.
//  3. ALIAS-COLLISION FIX: raw results are collected in memory, grouped by
//     isp.GroupFromVDMISP(raw), and written as ONE summed row per canonical
//     group with raw_isps[] listing every contributing raw name — two raw
//     names mapping to one group can never overwrite each other.
//  4. Upsert guarded by immutability: ... ON CONFLICT DO UPDATE ... WHERE
//     ses_vdm_daily.finalized_at IS NULL.
//  5. FINALIZE (off-by-one fixed, rev4): finalized_at = NOW() WHERE complete
//     AND source_window_end <= NOW() - 48h — never a day-arithmetic predicate.
//
// VDM data lives on UTC days (the API rejects non-midnight-UTC daily bounds)
// and NEVER mixes into budget judgments (I-1). Kill switch:
// SES_VDM_SNAPSHOT_DISABLED (disabled tick = one 'disabled' heartbeat).

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	sespkg "github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	sesVDMSnapshotWorkerName = "ses_vdm_snapshot"
	sesVDMSnapshotLockKey    = "ses_vdm_snapshot"

	// DefaultSESVDMSnapshotInterval — VDM daily buckets move slowly; 6h keeps
	// today-so-far fresh without hammering BatchGetMetricData.
	DefaultSESVDMSnapshotInterval = 6 * time.Hour

	// sesVDMCatchupMaxDays bounds one run's historical work set.
	sesVDMCatchupMaxDays = 14

	// sesVDMPerCallPause is the inter-call breath (AWS rate limits).
	sesVDMPerCallPause = 300 * time.Millisecond

	// sesVDMPerCallEstimate is the assumed per-call latency used ONLY for the
	// lease-TTL estimate (call + pause).
	sesVDMPerCallEstimate = 700 * time.Millisecond
)

func sesVDMSnapshotDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SES_VDM_SNAPSHOT_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// VDMFetcher is the worker's seam over the SES client (tests inject a fake).
type VDMFetcher interface {
	GetMetricsForIdentityISP(ctx context.Context, identity, rawISP string, from, to time.Time) (*sespkg.IdentityISPMetrics, error)
}

// SESVDMSnapshotWorker owns the snapshot pass.
type SESVDMSnapshotWorker struct {
	db       *sql.DB
	redis    *redis.Client
	region   string
	interval time.Duration
	pause    time.Duration
	fetcher  VDMFetcher // lazily built from region unless injected
}

// NewSESVDMSnapshotWorker wires the worker. redisClient may be nil (PG
// advisory fallback). region comes from SES_REGION (default us-west-1 at the
// boot site).
func NewSESVDMSnapshotWorker(db *sql.DB, redisClient *redis.Client, region string) *SESVDMSnapshotWorker {
	return &SESVDMSnapshotWorker{
		db:       db,
		redis:    redisClient,
		region:   region,
		interval: DefaultSESVDMSnapshotInterval,
		pause:    sesVDMPerCallPause,
	}
}

// SetFetcher injects the SES seam (tests). Call before Start.
func (w *SESVDMSnapshotWorker) SetFetcher(f VDMFetcher) *SESVDMSnapshotWorker {
	w.fetcher = f
	return w
}

// WithInterval overrides the tick cadence (tests). Call before Start.
func (w *SESVDMSnapshotWorker) WithInterval(d time.Duration) *SESVDMSnapshotWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

// sesVDMExtraIdentities are NON-LEDGER SES identities worth the VDM
// scoreboard (QA fix increment 2026-08-17). The ledger reverse map only
// yields the 16 drip-roster domains, so identities outside the roster must be
// listed here (or via SES_VDM_EXTRA_IDENTITIES, comma-separated env).
//   - m.wcl-heloc.com — the WCL HELOC funnel's live SES identity (verified
//     mailing_sending_profiles 2026-08-17: the profile is m.*, there is no
//     em.wcl profile — the m.* SES identity convention).
var sesVDMExtraIdentities = []string{"m.wcl-heloc.com"}

// vdmIdentities returns the ledger properties' sending domains (the 16 drip
// roster brands via the orchestrator's canonical map) plus the explicit
// non-ledger extras (const above ∪ SES_VDM_EXTRA_IDENTITIES env), deduped and
// sorted.
func vdmIdentities() []string {
	set := map[string]bool{}
	for _, b := range DripIntroBrands() {
		if d, ok := BrandSendingDomain(b); ok && d != "" {
			set[d] = true
		}
	}
	for _, d := range sesVDMExtraIdentities {
		set[d] = true
	}
	for _, d := range strings.Split(os.Getenv("SES_VDM_EXTRA_IDENTITIES"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			set[d] = true
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// vdmRawISPs returns VDMISPs() ∪ SES_VDM_ISPS (comma-separated env), deduped
// and sorted.
func vdmRawISPs() []string {
	set := map[string]bool{}
	for _, r := range isppkg.VDMISPs() {
		set[r] = true
	}
	for _, r := range strings.Split(os.Getenv("SES_VDM_ISPS"), ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			set[r] = true
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// vdmCanonicalGroups returns the distinct canonical groups the raw set maps
// onto (isp.GroupFromVDMISP; unmapped raws canonicalize to lower(raw)).
func vdmCanonicalGroups(raws []string) []string {
	set := map[string]bool{}
	for _, r := range raws {
		g, _ := isppkg.GroupFromVDMISP(r)
		set[g] = true
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// vdmLeaseTTL sizes the distlock lease for the worst-case catch-up (plan
// Step 16: 14 days × per-day fetch time + margin).
func vdmLeaseTTL(identities, raws int) time.Duration {
	perDay := time.Duration(identities*raws) * (sesVDMPerCallPause + sesVDMPerCallEstimate)
	est := time.Duration(sesVDMCatchupMaxDays+1) * perDay
	return est + 30*time.Minute
}

// vdmWorkSet computes the historical days to (re)fetch — see the package
// comment. All inputs/outputs are UTC midnights. Pure (unit-tested).
func vdmWorkSet(lastCompleted *time.Time, unfinalized []time.Time, todayUTC time.Time, maxDays int) []time.Time {
	set := map[time.Time]bool{}
	yesterday := todayUTC.AddDate(0, 0, -1)

	// Gap walk from the last COMPLETED run day (run-table detection — the only
	// way to see a fully-absent day). No completed run ever → bootstrap the
	// full bounded window.
	start := todayUTC.AddDate(0, 0, -maxDays)
	if lastCompleted != nil && lastCompleted.AddDate(0, 0, 1).After(start) {
		start = lastCompleted.AddDate(0, 0, 1)
	}
	for d := start; !d.After(yesterday); d = d.AddDate(0, 0, 1) {
		set[d] = true
	}

	// Days still carrying unfinalized rows re-fetch until they finalize.
	for _, d := range unfinalized {
		if !d.After(yesterday) {
			set[d] = true
		}
	}

	out := make([]time.Time, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	if len(out) > maxDays {
		out = out[len(out)-maxDays:]
	}
	return out
}

// vdmCanonicalRow is one summed (identity, canonical isp) cell ready to write.
type vdmCanonicalRow struct {
	isp        string
	rawISPs    []string
	values     map[string]int64
	missing    []string
	fetchError string
	complete   bool
}

// sumVDMRawResults groups one identity's raw fetches by canonical group and
// sums them (the alias-collision fix). fetchErrs is raw name → error text for
// raws whose call failed outright. Pure (unit-tested).
func sumVDMRawResults(results []*sespkg.IdentityISPMetrics, fetchErrs map[string]string) map[string]*vdmCanonicalRow {
	out := map[string]*vdmCanonicalRow{}
	get := func(raw string) *vdmCanonicalRow {
		g, _ := isppkg.GroupFromVDMISP(raw)
		row := out[g]
		if row == nil {
			row = &vdmCanonicalRow{isp: g, values: map[string]int64{}, complete: true}
			out[g] = row
		}
		row.rawISPs = append(row.rawISPs, raw)
		return row
	}
	missingSet := map[string]map[string]bool{}
	for _, r := range results {
		row := get(r.RawISP)
		for m, v := range r.Values {
			row.values[m] += v
		}
		for _, m := range r.MissingMetrics {
			if missingSet[row.isp] == nil {
				missingSet[row.isp] = map[string]bool{}
			}
			missingSet[row.isp][m] = true
			row.complete = false
		}
	}
	for raw, errText := range fetchErrs {
		row := get(raw)
		row.complete = false
		if row.fetchError != "" {
			row.fetchError += "; "
		}
		row.fetchError += raw + ": " + errText
	}
	for g, row := range out {
		for m := range missingSet[g] {
			row.missing = append(row.missing, m)
		}
		sort.Strings(row.missing)
		sort.Strings(row.rawISPs)
	}
	return out
}

// Start runs the tick loop until ctx is cancelled.
func (w *SESVDMSnapshotWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[SESVDMSnapshot] disabled (db missing)")
		return
	}
	go func() {
		log.Printf("SES VDM snapshot worker started (interval=%s, region=%s, catch-up ≤%dd, finalize at 48h)",
			w.interval, w.region, sesVDMCatchupMaxDays)
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-ctx.Done():
				log.Printf("[SESVDMSnapshot] context cancelled, stopping")
				return
			}
		}
	}()
}

func (w *SESVDMSnapshotWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if sesVDMSnapshotDisabled() {
		EmitHeartbeat(ctx, w.db, sesVDMSnapshotWorkerName, int(w.interval.Seconds()), "disabled", "SES_VDM_SNAPSHOT_DISABLED set")
		return
	}
	identities := vdmIdentities()
	raws := vdmRawISPs()
	ttl := vdmLeaseTTL(len(identities), len(raws))
	lock := distlock.NewLock(w.redis, w.db, sesVDMSnapshotLockKey, ttl)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[SESVDMSnapshot] lock acquire error: %v", err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[SESVDMSnapshot] lock release error: %v", err)
		}
	}()
	w.RunOnce(ctx)
}

// RunOnce executes one full snapshot pass. The caller owns locking.
func (w *SESVDMSnapshotWorker) RunOnce(ctx context.Context) {
	identities := vdmIdentities()
	raws := vdmRawISPs()
	groups := vdmCanonicalGroups(raws)
	ttl := vdmLeaseTTL(len(identities), len(raws))
	log.Printf("[SESVDMSnapshot] run start: %d identities × %d raw ISPs (→ %d canonical groups); worst-case catch-up estimate %s (lease sized ≥ that)",
		len(identities), len(raws), len(groups), ttl-30*time.Minute)

	if w.fetcher == nil {
		client, err := sespkg.NewClientForRegion(ctx, w.region)
		if err != nil {
			log.Printf("[SESVDMSnapshot] SES client init failed (region=%s): %v", w.region, err)
			EmitHeartbeat(ctx, w.db, sesVDMSnapshotWorkerName, int(w.interval.Seconds()), "error", "ses client: "+err.Error())
			return
		}
		w.fetcher = client
	}

	todayUTC := utcMidnight(time.Now())
	lastCompleted, unfinalized, err := w.loadWorkSetInputs(ctx, todayUTC)
	if err != nil {
		log.Printf("[SESVDMSnapshot] work-set discovery failed: %v", err)
		EmitHeartbeat(ctx, w.db, sesVDMSnapshotWorkerName, int(w.interval.Seconds()), "error", "work set: "+err.Error())
		return
	}
	days := vdmWorkSet(lastCompleted, unfinalized, todayUTC, sesVDMCatchupMaxDays)
	days = append(days, todayUTC) // today-so-far, always

	hbStatus, hbErr := "ok", ""
	for _, day := range days {
		if ctx.Err() != nil {
			return
		}
		if err := w.runOneDay(ctx, day, identities, raws, groups); err != nil {
			log.Printf("[SESVDMSnapshot] day %s failed: %v", day.Format("2006-01-02"), err)
			hbStatus, hbErr = "error", err.Error()
		}
	}

	// Finalize (I-7, rev4 predicate): 48h after the source window closed.
	if _, err := w.db.ExecContext(ctx, `
		UPDATE ses_vdm_daily SET finalized_at = NOW()
		WHERE finalized_at IS NULL AND complete
		  AND source_window_end <= NOW() - INTERVAL '48 hours'`); err != nil {
		log.Printf("[SESVDMSnapshot] finalize failed: %v", err)
		if hbStatus == "ok" {
			hbStatus, hbErr = "error", "finalize: "+err.Error()
		}
	}

	EmitHeartbeat(ctx, w.db, sesVDMSnapshotWorkerName, int(w.interval.Seconds()), hbStatus, hbErr)
}

// loadWorkSetInputs reads the last COMPLETED run day and the days still
// carrying unfinalized rows (both region-scoped, both before today).
func (w *SESVDMSnapshotWorker) loadWorkSetInputs(ctx context.Context, todayUTC time.Time) (*time.Time, []time.Time, error) {
	var last sql.NullTime
	if err := w.db.QueryRowContext(ctx, `
		SELECT MAX(day) FROM ses_vdm_snapshot_runs
		WHERE region = $1 AND status = 'completed'`, w.region).Scan(&last); err != nil {
		return nil, nil, fmt.Errorf("last completed run: %w", err)
	}
	var lastPtr *time.Time
	if last.Valid {
		d := utcMidnight(last.Time)
		lastPtr = &d
	}
	rows, err := w.db.QueryContext(ctx, `
		SELECT DISTINCT day FROM ses_vdm_daily
		WHERE region = $1 AND finalized_at IS NULL AND day < $2::date`,
		w.region, todayUTC.Format("2006-01-02"))
	if err != nil {
		return nil, nil, fmt.Errorf("unfinalized days: %w", err)
	}
	defer rows.Close()
	var unfinalized []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, nil, err
		}
		unfinalized = append(unfinalized, utcMidnight(d))
	}
	return lastPtr, unfinalized, rows.Err()
}

// runOneDay fetches and writes one UTC day under a run row (I-8).
func (w *SESVDMSnapshotWorker) runOneDay(ctx context.Context, day time.Time, identities, raws, groups []string) error {
	expected := len(identities) * len(groups)
	dayStr := day.Format("2006-01-02")
	winStart := day
	winEnd := day.AddDate(0, 0, 1)

	var runID string
	if err := w.db.QueryRowContext(ctx, `
		INSERT INTO ses_vdm_snapshot_runs (day, region, expected_cells, status)
		VALUES ($1::date, $2, $3, 'running') RETURNING run_id::text`,
		dayStr, w.region, expected).Scan(&runID); err != nil {
		return fmt.Errorf("run row: %w", err)
	}

	completed := 0
	var dayErr error
	for _, identity := range identities {
		if ctx.Err() != nil {
			dayErr = ctx.Err()
			break
		}
		results := make([]*sespkg.IdentityISPMetrics, 0, len(raws))
		fetchErrs := map[string]string{}
		for _, raw := range raws {
			m, err := w.fetcher.GetMetricsForIdentityISP(ctx, identity, raw, winStart, winEnd)
			if err != nil {
				fetchErrs[raw] = err.Error()
			} else {
				results = append(results, m)
			}
			if w.pause > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(w.pause):
				}
			}
		}
		for _, row := range sumVDMRawResults(results, fetchErrs) {
			n, err := w.upsertCell(ctx, dayStr, identity, row, winStart, winEnd)
			if err != nil {
				dayErr = err
				continue
			}
			completed += int(n)
		}
	}

	if dayErr != nil {
		if _, uerr := w.db.ExecContext(ctx, `
			UPDATE ses_vdm_snapshot_runs SET status='failed', completed_cells=$2,
			       finished_at=NOW(), error=$3 WHERE run_id=$1::uuid`,
			runID, completed, truncateRunDetail(dayErr.Error())); uerr != nil {
			log.Printf("[SESVDMSnapshot] run %s fail-stamp error: %v", runID, uerr)
		}
		return dayErr
	}
	if _, err := w.db.ExecContext(ctx, `
		UPDATE ses_vdm_snapshot_runs SET status='completed', completed_cells=$2,
		       finished_at=NOW() WHERE run_id=$1::uuid`, runID, completed); err != nil {
		return fmt.Errorf("run complete-stamp: %w", err)
	}
	return nil
}

// sesVDMUpsertSQL writes one canonical cell; finalized rows are IMMUTABLE
// (I-7) — protected at the write on top of the up-front work-set exclusion.
const sesVDMUpsertSQL = `
	INSERT INTO ses_vdm_daily
		(day, identity, isp, region, raw_isps, send, delivery, permanent_bounce,
		 transient_bounce, complaint, open, click, complete, missing_metrics,
		 fetch_error, source_window_start, source_window_end, fetched_at)
	VALUES ($1::date, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, NOW())
	ON CONFLICT (day, identity, isp, region) DO UPDATE SET
		raw_isps            = EXCLUDED.raw_isps,
		send                = EXCLUDED.send,
		delivery            = EXCLUDED.delivery,
		permanent_bounce    = EXCLUDED.permanent_bounce,
		transient_bounce    = EXCLUDED.transient_bounce,
		complaint           = EXCLUDED.complaint,
		open                = EXCLUDED.open,
		click               = EXCLUDED.click,
		complete            = EXCLUDED.complete,
		missing_metrics     = EXCLUDED.missing_metrics,
		fetch_error         = EXCLUDED.fetch_error,
		source_window_start = EXCLUDED.source_window_start,
		source_window_end   = EXCLUDED.source_window_end,
		fetched_at          = NOW()
	WHERE ses_vdm_daily.finalized_at IS NULL`

func (w *SESVDMSnapshotWorker) upsertCell(ctx context.Context, dayStr, identity string, row *vdmCanonicalRow, winStart, winEnd time.Time) (int64, error) {
	var fetchErr interface{}
	if row.fetchError != "" {
		fetchErr = truncateRunDetail(row.fetchError)
	}
	missing := row.missing
	if missing == nil {
		missing = []string{}
	}
	res, err := w.db.ExecContext(ctx, sesVDMUpsertSQL,
		dayStr, identity, row.isp, w.region, pq.Array(row.rawISPs),
		row.values[sespkg.MetricSend], row.values[sespkg.MetricDelivery],
		row.values[sespkg.MetricPermanentBounce], row.values[sespkg.MetricTransientBounce],
		row.values[sespkg.MetricComplaint], row.values[sespkg.MetricOpen], row.values[sespkg.MetricClick],
		row.complete, pq.Array(missing), fetchErr, winStart, winEnd)
	if err != nil {
		return 0, fmt.Errorf("upsert %s/%s: %w", identity, row.isp, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// utcMidnight truncates an instant to its UTC calendar date.
func utcMidnight(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
