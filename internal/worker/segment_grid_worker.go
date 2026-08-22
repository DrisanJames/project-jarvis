package worker

// SegmentGridWorker — the daily in-platform build of the per-sending-domain
// engagement grid ('<CODE> <N>D (Openers|Clickers)', N ∈ {7,14,30,60} — 27
// domains × 8 cells), ported out of the Fargate sidecar
// (s3://ignite-analytics-lake/_tools/refresh_engagement_segments_from_lake.py)
// per the operator mandate: "I don't want no cron job. I don't want some
// sidecar. I need this baked into the software."
//
// WHAT ONE PASS DOES
//  1. Enumerate the grid from mailing_segments WHERE status='active' by NAME
//     pattern (never by ledger alone — a ledger-only enumeration counts
//     archived corpses; 2026-08-21 finding).
//  2. Parse each segment's legacy conditions FAIL-CLOSED (the 332b3bfe rule:
//     an inexpressible condition refuses the whole segment rather than build
//     a silently BROADER audience; those rows stay with MaterializeSegment).
//     Expressible predicates mirror the fixed sidecar: event window,
//     sending_domain, exclude_list_pattern (seeds), created_at in_last_days,
//     exclude_never_clickers gte (8670ba68).
//  3. Skip segments already built ok in the last segmentGridFreshSkip — this
//     is the restart/resume contract: an ECS bounce mid-pass re-fires the
//     pass, and already-built segments are skipped from the LEDGER, so a
//     double-fire costs nothing (no Athena re-scan, no member churn).
//  4. Fetch one Athena bucket per needed (event, window) — at most 8 —
//     of distinct (subscriber_id, brand) pairs
//     (analytics.BuildEngagementGridBucket; Athena spend identical to the
//     sidecar's UNLOADs).
//  5. Resolve the union of needed subscriber ids → (email, seed flag,
//     created_at, sends, clicks) with ONE bounded temp-table hash join over
//     mailing_subscribers (the sidecar's resolve_emails(): COPY ids into a
//     TEMP table, enable_nestloop=off, single sequential scan — never 200k
//     random PK reads on a strained primary).
//  6. Rebuild each segment SEQUENTIALLY (one at a time — DB-safety mandate):
//     directional delta guard (d8c4067c: drops past 50% are the lake-gap
//     signal and SKIP; growth only past 500%; segments under 200 current
//     members exempt), transactional member swap under
//     pg_advisory_xact_lock(hashtext(segment_id)) (the same key the sidecar
//     and the api lake builder take, so builders can never interleave), and
//     a ledger row for EVERY outcome — 'ok' | 'skipped_delta' | 'error'.
//     A skip that writes no ledger row is the documented footgun (the
//     symmetric-guard freeze went unnoticed for 4 days because SKIP lines
//     lived only in a Fargate log).
//  7. CIRCUIT: segmentGridCircuitLimit consecutive build failures abort the
//     pass and heartbeat 'degraded' instead of grinding a browned-out
//     primary (the 12 kumo 600s-timeout loops of 2026-08-20 burned ~2h of
//     RDS for nothing).
//
// ON-DEMAND QUEUE: every tick the worker drains
// mailing_segment_refresh_requests (written by POST
// /api/mailing/segments/refresh — the UI Refresh button). Claims are
// single-statement FOR UPDATE SKIP LOCKED so a double-fire can never
// double-build; rows stuck 'running' past segmentGridRequestStaleAfter are
// re-queued (crash recovery). Requests finish 'done' or 'failed' with an
// operator-facing reason.
//
// SINGLE-WRITER: distlock (Redis preferred, PG advisory fallback — the
// PMTAWaveScheduler contract). ECS runs desired=2; without the lock every
// segment worker used to run doubled on both instances (2026-08-21 SEV-2).
// The lock is taken per tick and held for the duration of the work.
//
// SCHEDULE: in-process, daily target segmentGridTargetUTC (05:30Z = 23:30
// America/Denver during MDT) so the grid is fresh before the ~06:30Z board
// build. Denver-day aware: the pass runs once per UTC day at the target and
// the per-segment 20h ledger skip absorbs deploy re-fires; no EventBridge,
// no cron, no sidecar.
//
// Kill switch: DISABLE_SEGMENT_GRID_WORKER=true.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	segmentGridWorkerName = "segment_grid"
	segmentGridLockKey    = "segment-grid-worker"

	// Daily target: 05:30 UTC = 23:30 America/Denver (MDT) — after the send
	// day, before the ~06:30Z board build the grid must be fresh for.
	segmentGridTargetHourUTC = 5
	segmentGridTargetMinUTC  = 30

	// segmentGridTick paces the loop: each tick drains the on-demand queue
	// and (once per day, past the target) runs the daily pass.
	segmentGridTick = 45 * time.Second

	// segmentGridFreshSkip: a segment whose ledger shows a successful build
	// newer than this is skipped by the daily pass. 20h < the 24h cadence so
	// a drifting pass start never skips a whole day, and > any plausible
	// same-morning double-fire window (deploy bounces, the still-armed
	// Fargate job at 06:30Z).
	segmentGridFreshSkip = 20 * time.Hour

	// segmentGridPassCooldown throttles daily-pass retries after a failed
	// attempt so a persistent fault (Athena down, DB browned out) is retried
	// on a calm cadence instead of every tick.
	segmentGridPassCooldown = 30 * time.Minute

	// segmentGridCircuitLimit: consecutive per-segment build failures that
	// abort the pass with a 'degraded' heartbeat.
	segmentGridCircuitLimit = 3

	// Directional delta guard (d8c4067c — the sidecar's semantics exactly).
	segmentGridMaxDropPct   = 50.0
	segmentGridMaxGrowthPct = 500.0
	segmentGridMinGuardSize = 200

	// segmentGridResolveTimeoutMs bounds the one-per-pass subscriber resolve
	// hash join (sidecar parity: --db-timeout-ms default 900000).
	segmentGridResolveTimeoutMs = 900000

	// segmentGridSwapTimeoutMs bounds each per-segment member swap tx.
	segmentGridSwapTimeoutMs = 600000

	// segmentGridRunningStaleAfter mirrors api/segment_ledger.go
	// ledgerRunningStaleAfter: a 'running' ledger row older than this is a
	// crashed build and must not block a new one.
	segmentGridRunningStaleAfter = 30 * time.Minute

	// segmentGridRequestStaleAfter: an on-demand request stuck 'running'
	// longer than this was claimed by a process that died — re-queue it.
	segmentGridRequestStaleAfter = 30 * time.Minute

	// segmentGridClaimBatch bounds how many queued requests one tick claims.
	segmentGridClaimBatch = 10
)

// ---------------------------------------------------------------------------
// Schema (registered in cmd/server/main.go runStartupMigrations — each const
// is its OWN migration entry: migrationSkipProbe classifies by LEADING
// keyword, so a single string holding CREATE TABLE then CREATE INDEX would be
// probed as CREATE TABLE and the index would silently never land).
// ---------------------------------------------------------------------------

// SegmentGridLockTimeoutDDL bounds the lock wait for this slice's entries so
// a busy table can never barricade the boot path (2026-08-20 lesson: a no-op
// ALTER queued 216s behind a hot table). MUST be paired with
// SegmentGridLockTimeoutResetDDL after the last entry — runStartupMigrations
// holds ONE dedicated connection, so an un-reset SET re-scopes every later
// migration.
const SegmentGridLockTimeoutDDL = `SET lock_timeout = '2s'`

// SegmentGridLockTimeoutResetDDL restores the connection default.
const SegmentGridLockTimeoutResetDDL = `RESET lock_timeout`

// SegmentRefreshRequestsDDL creates the on-demand refresh queue written by
// POST /api/mailing/segments/refresh and drained by SegmentGridWorker.
// New empty table — instant; ids are generated app-side (no pgcrypto dep).
const SegmentRefreshRequestsDDL = `
CREATE TABLE IF NOT EXISTS mailing_segment_refresh_requests (
	id              UUID        PRIMARY KEY,
	segment_id      UUID        NOT NULL,
	organization_id UUID,
	requested_by    TEXT        NOT NULL DEFAULT '',
	source          TEXT        NOT NULL DEFAULT 'ui',
	status          TEXT        NOT NULL DEFAULT 'queued',
	reason          TEXT,
	requested_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	started_at      TIMESTAMPTZ,
	finished_at     TIMESTAMPTZ
)`

// SegmentRefreshRequestsPendingIdxDDL drives the worker's queue poll.
const SegmentRefreshRequestsPendingIdxDDL = `CREATE INDEX IF NOT EXISTS idx_seg_refresh_req_pending
	ON mailing_segment_refresh_requests (status, requested_at)
	WHERE status IN ('queued','running')`

// SegmentRefreshRequestsOpenSlotDDL is the idempotency rail: at most ONE
// open (queued/running) request per segment. The API inserts with
// ON CONFLICT DO NOTHING against this index, so a duplicate Refresh click is
// reported as already-queued, never double-inserted.
const SegmentRefreshRequestsOpenSlotDDL = `CREATE UNIQUE INDEX IF NOT EXISTS uq_seg_refresh_req_open
	ON mailing_segment_refresh_requests (segment_id)
	WHERE status IN ('queued','running')`

// SegmentRefreshRequestsDoneIdxDDL serves the freshness endpoint's
// latest-request-per-segment read.
const SegmentRefreshRequestsDoneIdxDDL = `CREATE INDEX IF NOT EXISTS idx_seg_refresh_req_segment
	ON mailing_segment_refresh_requests (segment_id, requested_at DESC)`

// ---------------------------------------------------------------------------
// Grid spec parsing (fail-closed — the 332b3bfe rule)
// ---------------------------------------------------------------------------

// gridNameRe selects the grid by NAME: '<CODE> <N>D (Openers|Clickers)'.
// Codes are 2-4 uppercase letters (DB … BCC, USF, HLJ, FTH, HTM, AAD, HFC).
var gridNameRe = regexp.MustCompile(`^[A-Z]{2,4} (7|14|30|60)D (Openers|Clickers)$`)

// gridBrandPrefixes mirrors the sidecar's _BRAND_PREFIXES: how a segment's
// sending_domain ('em.foo.com') normalizes to the apex the lake stores in
// email_events.brand.
var gridBrandPrefixes = []string{"em.", "m.", "mail.", "e.", "mx."}

// gridEventFieldMap mirrors the sidecar's EVENT_FIELD_MAP.
var gridEventFieldMap = map[string]string{
	"email_opened":  "open",
	"email_clicked": "click",
	"opened":        "open",
	"clicked":       "click",
}

// gridSpec is one parsed grid segment.
type gridSpec struct {
	ID           string
	Name         string
	Event        string // "open" | "click"
	WindowDays   int
	BrandApex    string
	ExcludeSeeds bool
	// 0 = predicate absent (both predicates are meaningless at 0).
	CreatedWithinDays int
	NeverClickersGte  int
}

func gridBrandApex(sendingDomain string) string {
	d := strings.ToLower(strings.TrimSpace(sendingDomain))
	for _, pre := range gridBrandPrefixes {
		if strings.HasPrefix(d, pre) {
			return d[len(pre):]
		}
	}
	return d
}

// gridRawCondition is one legacy condition entry.
type gridRawCondition struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
}

// gridIterConditions accepts both the legacy `[...]` array form and the V2-ish
// `{"conditions":[...]}` object form (the sidecar's _iter_conditions). Any
// other shape (including {"lake_spec":...}) yields no conditions.
func gridIterConditions(raw string) []gridRawCondition {
	raw = strings.TrimSpace(raw)
	var arr []gridRawCondition
	if json.Unmarshal([]byte(raw), &arr) == nil {
		return arr
	}
	var obj struct {
		Conditions []gridRawCondition `json:"conditions"`
	}
	if json.Unmarshal([]byte(raw), &obj) == nil {
		return obj.Conditions
	}
	return nil
}

func gridCondInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// parseGridConditions translates a grid segment's conditions into a gridSpec,
// FAIL-CLOSED: any condition field/operator this builder cannot express
// returns an error, and the segment is left to MaterializeSegment (which
// honours the full condition set). Building a wider audience than the
// segment's own definition — the pre-332b3bfe behavior — is the failure this
// exists to prevent (live case: all 16 '<CODE> 60D Openers' carry
// exclude_never_clickers gte 15; the old parse dropped it silently).
func parseGridConditions(name, conditionsRaw string) (*gridSpec, error) {
	conds := gridIterConditions(conditionsRaw)
	if len(conds) == 0 {
		return nil, fmt.Errorf("no parseable conditions")
	}
	spec := &gridSpec{Name: name}
	for _, c := range conds {
		field := strings.ToLower(strings.TrimSpace(c.Field))
		op := strings.ToLower(strings.TrimSpace(c.Operator))
		switch {
		case gridEventFieldMap[field] != "" && (op == "in_last_days" || op == "within_last"):
			n, ok := gridCondInt(c.Value)
			if !ok || n < 1 {
				return nil, fmt.Errorf("bad window value %v", c.Value)
			}
			spec.Event = gridEventFieldMap[field]
			spec.WindowDays = n
		case field == "sending_domain" && (op == "equals" || op == "is"):
			s, _ := c.Value.(string)
			spec.BrandApex = gridBrandApex(s)
		case field == "exclude_list_pattern":
			if s, _ := c.Value.(string); s != "" {
				spec.ExcludeSeeds = true
			}
		case field == "created_at" && (op == "in_last_days" || op == "within_last"):
			n, ok := gridCondInt(c.Value)
			if !ok || n < 1 {
				return nil, fmt.Errorf("bad created_at value %v", c.Value)
			}
			spec.CreatedWithinDays = n
		case field == "exclude_never_clickers" && (op == "gte" || op == "greater_than_or_equal"):
			n, ok := gridCondInt(c.Value)
			if !ok || n < 1 {
				return nil, fmt.Errorf("bad exclude_never_clickers value %v", c.Value)
			}
			spec.NeverClickersGte = n
		default:
			return nil, fmt.Errorf("condition not expressible: field=%q operator=%q", field, op)
		}
	}
	if spec.Event == "" || spec.WindowDays == 0 {
		return nil, fmt.Errorf("no event/window condition")
	}
	if spec.BrandApex == "" {
		// The grid is per sending domain by definition; a name-matching row
		// with no domain scope is not a grid cell.
		return nil, fmt.Errorf("no sending_domain condition")
	}
	return spec, nil
}

// gridDeltaGuardTrips is the DIRECTIONAL membership guard — the sidecar's
// delta_guard_trips() arm for arm (d8c4067c). The guard protects against
// lake GAPS, which under-count, i.e. a DROP; the original symmetric guard
// also blocked growth, which was self-perpetuating and froze all 17 Clicker
// segments by 2026-08-20 (DB 30D Clickers pinned at 1,534 vs a true 2,846).
//
//   - drop   → blocked past maxDropPct (the genuine lake-gap signal)
//   - growth → blocked only past maxGrowthPct (gross inflation)
//   - tiny   → segments under minGuardSize current members are exempt
func gridDeltaGuardTrips(curN, newN int64, maxDropPct, maxGrowthPct float64, minGuardSize int64) bool {
	if curN < minGuardSize {
		return false
	}
	if curN <= 0 {
		return false
	}
	delta := float64(newN-curN) / float64(curN) * 100.0
	if delta < -maxDropPct {
		return true
	}
	if maxGrowthPct > 0 && delta > maxGrowthPct {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Worker status (read by GET /api/mailing/segments/freshness)
// ---------------------------------------------------------------------------

// SegmentGridWorkerState is the in-process view of the worker on THIS
// instance. LastPassAt/outcome reflect the last daily pass this instance ran
// (cluster-wide truth lives in mailing_worker_heartbeats / _runs).
type SegmentGridWorkerState struct {
	Running         bool
	Leader          bool
	Degraded        bool
	LastPassAt      time.Time
	LastPassOutcome string
}

var (
	gridStateMu sync.Mutex
	gridState   SegmentGridWorkerState
)

// SegmentGridState returns a copy of the worker's in-process state.
func SegmentGridState() SegmentGridWorkerState {
	gridStateMu.Lock()
	defer gridStateMu.Unlock()
	return gridState
}

func setGridState(mut func(*SegmentGridWorkerState)) {
	gridStateMu.Lock()
	defer gridStateMu.Unlock()
	mut(&gridState)
}

// ---------------------------------------------------------------------------
// Worker
// ---------------------------------------------------------------------------

// gridSubscriber is one resolved subscriber row from the temp-table hash
// join (the sidecar's resolve_emails() attrs).
type gridSubscriber struct {
	Email     string
	IsSeed    bool
	CreatedAt sql.NullTime
	Sends     int
	Clicks    int
}

// gridMember is one member row to write.
type gridMember struct {
	SubscriberID string
	Email        string
}

// SegmentGridWorker rebuilds the engagement grid daily and serves the
// on-demand refresh queue.
type SegmentGridWorker struct {
	db    *sql.DB
	redis *redis.Client

	// Test seams — default to the real implementations in NewSegmentGridWorker.
	bucketFn        func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error)
	resolveFn       func(ctx context.Context, ids []string, seedListIDs []string) (map[string]gridSubscriber, error)
	swapFn          func(ctx context.Context, segmentID string, members []gridMember) error
	nowFn           func() time.Time
	readerEnabledFn func() bool

	// Delta-path seams (phase 2 — segment_grid_delta.go). All default to the
	// real implementations; tests stub them to prove routing without S3,
	// Athena, or a DB.
	deltaSupportedFn func() bool
	ensureSnapFn     func(ctx context.Context) error
	unloadFn         func(ctx context.Context, event string, windowDays int, dt string) (int64, error)
	diffFn           func(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]analytics.GridDelta, error)
	mergeFn          func(ctx context.Context, segmentID string, adds []gridMember, removes []string) (int64, error)
	snapExistsFn     func(ctx context.Context, event string, windowDays int, dt string) bool
	recordSnapFn     func(ctx context.Context, event string, windowDays int, dt string, pairs int64)
	deleteSnapshotFn func(ctx context.Context, event string, windowDays int, dt string) error
	pruneFn          func(ctx context.Context, event string, windowDays int, todayDt string)
	loadDeltaStateFn func(ctx context.Context, ids []string) (map[string]gridDeltaState, bool)

	lastDailyDay     string    // UTC day of the last COMPLETED daily pass
	lastDailyAttempt time.Time // start of the last daily-pass attempt (cooldown)
}

func NewSegmentGridWorker(db *sql.DB, redisClient *redis.Client) *SegmentGridWorker {
	w := &SegmentGridWorker{
		db:    db,
		redis: redisClient,
		nowFn: time.Now,
	}
	w.bucketFn = analytics.BuildEngagementGridBucket
	w.resolveFn = w.resolveSubscribers
	w.swapFn = w.writeSegmentMembers
	w.readerEnabledFn = analytics.ReaderEnabled
	w.deltaSupportedFn = analytics.GridSnapshotsSupported
	w.ensureSnapFn = analytics.EnsureGridSnapshotTable
	w.unloadFn = func(ctx context.Context, event string, windowDays int, dt string) (int64, error) {
		cctx, cancel := context.WithTimeout(ctx, segmentGridUnloadTimeout)
		defer cancel()
		return analytics.UnloadGridSnapshot(cctx, event, windowDays, dt)
	}
	w.diffFn = func(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]analytics.GridDelta, error) {
		cctx, cancel := context.WithTimeout(ctx, segmentGridDiffTimeout)
		defer cancel()
		return analytics.DiffGridSnapshots(cctx, event, windowDays, baseDt, todayDt)
	}
	w.mergeFn = w.mergeSegmentDelta
	w.snapExistsFn = w.snapshotRecorded
	w.recordSnapFn = w.recordSnapshot
	w.deleteSnapshotFn = analytics.DeleteGridSnapshot
	w.pruneFn = w.pruneSnapshotsDefault
	w.loadDeltaStateFn = w.loadDeltaState
	return w
}

// Start runs the tick loop until ctx is cancelled. Call as `go w.Start(ctx)`
// from cmd/server/main.go behind dbReachable.
func (w *SegmentGridWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[SegmentGrid] disabled (db missing)")
		return
	}
	if os.Getenv("DISABLE_SEGMENT_GRID_WORKER") == "true" {
		log.Printf("[SegmentGrid] disabled via DISABLE_SEGMENT_GRID_WORKER")
		return
	}
	setGridState(func(s *SegmentGridWorkerState) { s.Running = true })
	log.Printf("[SegmentGrid] started (daily %02d:%02dZ grid build + %s on-demand queue; kill: DISABLE_SEGMENT_GRID_WORKER)",
		segmentGridTargetHourUTC, segmentGridTargetMinUTC, segmentGridTick)

	ticker := time.NewTicker(segmentGridTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			setGridState(func(s *SegmentGridWorkerState) { s.Running = false; s.Leader = false })
			log.Printf("[SegmentGrid] stopping")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick takes the single-writer lock once and, while holding it, runs the
// daily pass (when due) and drains the on-demand queue. A non-leader
// instance does NOTHING — not the pass, not the queue (claims are
// additionally FOR UPDATE SKIP LOCKED as defense in depth).
func (w *SegmentGridWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	lock := distlock.NewLock(w.redis, w.db, segmentGridLockKey, 2*time.Hour)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[SegmentGrid] lock acquire error: %v", err)
		return
	}
	if !acquired {
		setGridState(func(s *SegmentGridWorkerState) { s.Leader = false })
		return
	}
	setGridState(func(s *SegmentGridWorkerState) { s.Leader = true })
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[SegmentGrid] lock release error: %v", err)
		}
	}()

	if w.dailyDue() {
		w.runDailyPass(ctx)
	}
	w.drainQueue(ctx)
}

// dailyDue reports whether the daily pass should run now: past today's UTC
// target, not already completed today, and past the retry cooldown.
func (w *SegmentGridWorker) dailyDue() bool {
	now := w.nowFn().UTC()
	target := time.Date(now.Year(), now.Month(), now.Day(),
		segmentGridTargetHourUTC, segmentGridTargetMinUTC, 0, 0, time.UTC)
	if now.Before(target) {
		return false
	}
	if w.lastDailyDay == now.Format("2006-01-02") {
		return false
	}
	if !w.lastDailyAttempt.IsZero() && now.Sub(w.lastDailyAttempt) < segmentGridPassCooldown {
		return false
	}
	return true
}

// gridSegmentRow is one enumerated grid segment.
type gridSegmentRow struct {
	spec  gridSpec
	orgID string
}

// enumerateGridSegments loads the active grid by name pattern and parses
// conditions fail-closed. Unparseable rows are logged and left to the
// materializer — they are NOT ledgered by this worker (the materializer owns
// their ledger rows; overwriting them here would misreport its builds).
func (w *SegmentGridWorker) enumerateGridSegments(ctx context.Context) ([]gridSegmentRow, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id::text, name, COALESCE(conditions::text, '[]'), organization_id::text
		FROM mailing_segments
		WHERE status = 'active'
		  AND name ~ '^[A-Z]{2,4} (7|14|30|60)D (Openers|Clickers)$'
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []gridSegmentRow
	for rows.Next() {
		var id, name, conds, org string
		if err := rows.Scan(&id, &name, &conds, &org); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if !gridNameRe.MatchString(name) { // defense in depth vs regex-dialect drift
			continue
		}
		spec, perr := parseGridConditions(name, conds)
		if perr != nil {
			log.Printf("[SegmentGrid] skip %q (%s): %v — left to MaterializeSegment", name, id[:8], perr)
			continue
		}
		spec.ID = id
		out = append(out, gridSegmentRow{spec: *spec, orgID: org})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("enumerate read truncated: %w", err)
	}
	return out, nil
}

// gridLedgerRow is the slice of the build ledger the pass consults.
type gridLedgerRow struct {
	count     int64
	builtAt   sql.NullTime
	status    string
	updatedAt time.Time
}

func (w *SegmentGridWorker) loadLedger(ctx context.Context, ids []string) (map[string]gridLedgerRow, error) {
	out := make(map[string]gridLedgerRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := w.db.QueryContext(ctx, `
		SELECT segment_id::text, COALESCE(subscriber_count, 0), last_built_at,
		       COALESCE(last_build_status, ''), COALESCE(updated_at, NOW())
		FROM mailing_segment_build_ledger
		WHERE segment_id = ANY($1::uuid[])
	`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var lr gridLedgerRow
		if err := rows.Scan(&id, &lr.count, &lr.builtAt, &lr.status, &lr.updatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out[id] = lr
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger read truncated: %w", err)
	}
	return out, nil
}

// runDailyPass executes one grid build pass. Caller holds the distlock.
func (w *SegmentGridWorker) runDailyPass(ctx context.Context) {
	now := w.nowFn().UTC()
	w.lastDailyAttempt = now
	start := time.Now()

	built, skipped, failed := 0, 0, 0
	hbStatus, hbErr := "ok", ""
	outcome := "ok"
	defer func() {
		EmitHeartbeat(ctx, w.db, segmentGridWorkerName, int((24 * time.Hour).Seconds()), hbStatus, hbErr)
		runStatus := "ok"
		switch {
		case hbStatus == "error" || hbStatus == "degraded":
			runStatus = "failed"
		case failed > 0:
			runStatus = "partial"
		}
		RecordWorkerRun(ctx, w.db, segmentGridWorkerName, start, runStatus, built+skipped, failed,
			truncateRunDetail(fmt.Sprintf("grid pass: %d built, %d skipped, %d failed (%s)", built, skipped, failed, outcome)))
		setGridState(func(s *SegmentGridWorkerState) {
			s.LastPassAt = time.Now().UTC()
			s.LastPassOutcome = runStatus
			s.Degraded = hbStatus == "degraded"
		})
	}()

	if !w.readerEnabledFn() {
		hbStatus, hbErr = "error", "analytics lake reader disabled"
		outcome = hbErr
		log.Printf("[SegmentGrid] daily pass aborted: %s", hbErr)
		return
	}

	segs, err := w.enumerateGridSegments(ctx)
	if err != nil {
		hbStatus, hbErr = "error", "enumerate: "+err.Error()
		outcome = hbErr
		log.Printf("[SegmentGrid] %s", hbErr)
		return
	}
	if len(segs) == 0 {
		outcome = "no grid segments"
		w.lastDailyDay = now.Format("2006-01-02")
		return
	}

	ids := make([]string, len(segs))
	for i, s := range segs {
		ids[i] = s.spec.ID
	}
	ledger, err := w.loadLedger(ctx, ids)
	if err != nil {
		hbStatus, hbErr = "error", "ledger: "+err.Error()
		outcome = hbErr
		log.Printf("[SegmentGrid] %s", hbErr)
		return
	}

	// Resume/dedupe: skip segments built ok recently, and segments another
	// builder has freshly in flight.
	var need []gridSegmentRow
	for _, s := range segs {
		lr, ok := ledger[s.spec.ID]
		if ok && lr.builtAt.Valid && now.Sub(lr.builtAt.Time) < segmentGridFreshSkip {
			continue
		}
		if ok && lr.status == "running" && now.Sub(lr.updatedAt) < segmentGridRunningStaleAfter {
			log.Printf("[SegmentGrid] %q has a fresh 'running' build elsewhere — skipping", s.spec.Name)
			continue
		}
		need = append(need, s)
	}
	if len(need) == 0 {
		outcome = fmt.Sprintf("all %d grid segments already fresh", len(segs))
		w.lastDailyDay = now.Format("2006-01-02")
		log.Printf("[SegmentGrid] %s", outcome)
		return
	}
	log.Printf("[SegmentGrid] daily pass: %d/%d grid segments need building", len(need), len(segs))

	degraded, b, sk, f := w.buildSegments(ctx, need, ledger)
	built, skipped, failed = b, sk, f
	if degraded {
		hbStatus = "degraded"
		hbErr = fmt.Sprintf("circuit open after %d consecutive build failures", segmentGridCircuitLimit)
		outcome = hbErr
		return
	}
	if ctx.Err() != nil {
		outcome = "cancelled"
		return
	}
	outcome = "completed"
	w.lastDailyDay = now.Format("2006-01-02")
	log.Printf("[SegmentGrid] daily pass complete: %d built, %d skipped, %d failed in %s",
		built, skipped, failed, time.Since(start).Round(time.Second))
}

// buildSegments runs the shared build core over a set of parsed grid
// segments. Phase 2: each segment is routed down the DELTA path (Athena
// snapshot diff → one-TX PG merge — quick and IO-light) when its state
// allows, and down the phase-1 FULL path (bucket query → transactional
// member swap) otherwise: bootstrap (no prior snapshot), the 7-day
// reconcile, snapshot infra unavailable, an intraday refresh, or the
// DISABLE_SEGMENT_GRID_DELTA kill switch. A failed diff applies NOTHING
// (error ledger row — never a stale partial, never a surprise full swap).
// Returns (circuitTripped, built, skipped, failed).
func (w *SegmentGridWorker) buildSegments(ctx context.Context, need []gridSegmentRow, ledger map[string]gridLedgerRow) (bool, int, int, int) {
	built, skipped, failed := 0, 0, 0
	consecFails := 0
	passStart := time.Now()
	now := w.nowFn().UTC()
	todayDt := now.Format("2006-01-02")
	totalAdds, totalRemoves := 0, 0

	type bucketKey struct {
		event  string
		window int
	}
	needBuckets := map[bucketKey]bool{}
	for _, s := range need {
		needBuckets[bucketKey{s.spec.Event, s.spec.WindowDays}] = true
	}

	// ---- Path routing -----------------------------------------------------
	deltaOn := os.Getenv("DISABLE_SEGMENT_GRID_DELTA") != "true" && w.deltaSupportedFn()
	var deltaState map[string]gridDeltaState
	if deltaOn {
		ids := make([]string, len(need))
		for i, s := range need {
			ids[i] = s.spec.ID
		}
		var ok bool
		deltaState, ok = w.loadDeltaStateFn(ctx, ids)
		if !ok {
			deltaOn = false // logged inside; every segment goes full (phase-1)
		}
	}
	if deltaOn {
		if err := w.ensureSnapFn(ctx); err != nil {
			log.Printf("[SegmentGrid] snapshot table unavailable (delta path disabled this pass): %v", err)
			deltaOn = false
		}
	}

	type segPath struct {
		delta  bool
		baseDt string
		source string
		why    string
	}
	paths := make(map[string]segPath, len(need))
	for _, s := range need {
		p := segPath{source: segmentGridLedgerSourceFull}
		st := deltaState[s.spec.ID]
		switch {
		case !deltaOn:
			p.why = "delta unavailable"
		case st.snapshotDt == "":
			p.why = "bootstrap (no prior snapshot)"
		case !analytics.GridSnapshotWindowOK(s.spec.WindowDays):
			p.why = "window has no snapshot partition"
		case !st.fullBuiltAt.Valid || now.Sub(st.fullBuiltAt.Time) >= segmentGridReconcileEvery:
			p.why = "reconcile due — forcing full rebuild to correct snapshot/PG drift"
		case st.snapshotDt >= todayDt:
			// The base IS today's snapshot: the daily pass has nothing to
			// diff, and an operator's intraday Refresh wants CURRENT lake
			// truth, not "no change since the morning snapshot".
			p.why = "base snapshot is today's (intraday refresh) — full rebuild"
		case gridSnapshotBaseTooOld(st.snapshotDt, todayDt):
			p.why = fmt.Sprintf("base snapshot %s older than %dd", st.snapshotDt, segmentGridDeltaMaxBaseAgeDays)
		default:
			p.delta, p.baseDt, p.source = true, st.snapshotDt, segmentGridLedgerSourceDelta
		}
		if !p.delta && deltaOn {
			log.Printf("[SegmentGrid] %q full path: %s", s.spec.Name, p.why)
		}
		paths[s.spec.ID] = p
	}
	srcOf := func(segID string) string {
		if p, ok := paths[segID]; ok {
			return p.source
		}
		return segmentGridLedgerSourceFull
	}

	fail := func(segID, name, msg string) bool {
		failed++
		consecFails++
		w.ledgerUpsert(ctx, segID, ledgerCountOf(ledger, segID), srcOf(segID), "error", 0, 0, msg)
		log.Printf("[SegmentGrid] %q failed: %s", name, msg)
		return consecFails >= segmentGridCircuitLimit
	}

	// ---- Snapshot phase: UNLOAD today's snapshot per needed bucket --------
	// Needed as tomorrow's diff base regardless of today's path, and as the
	// diff's "today" side. Idempotent within a day via the PG bookkeeping
	// row (a crash between UNLOAD and record re-UNLOADs after a prefix
	// cleanup — partial S3 files are never trusted). An UNLOAD failure
	// demotes the bucket's delta segments to the full path: that is the
	// phase-1-approved complete rebuild, not a stale partial.
	snapshotted := map[bucketKey]bool{}
	if deltaOn {
		for bk := range needBuckets {
			if ctx.Err() != nil {
				return false, built, skipped, failed
			}
			if w.snapExistsFn(ctx, bk.event, bk.window, todayDt) {
				snapshotted[bk] = true
				continue
			}
			snapStart := time.Now()
			pairs, err := w.unloadFn(ctx, bk.event, bk.window, todayDt)
			if err != nil {
				log.Printf("[SegmentGrid] snapshot %s/%dd/%s failed (bucket's delta segments demoted to full): %v",
					bk.event, bk.window, todayDt, err)
				continue
			}
			w.recordSnapFn(ctx, bk.event, bk.window, todayDt, pairs)
			snapshotted[bk] = true
			log.Printf("[SegmentGrid] snapshot %s/%dd/%s: %d pairs in %s",
				bk.event, bk.window, todayDt, pairs, time.Since(snapStart).Round(time.Millisecond))
			w.pruneFn(ctx, bk.event, bk.window, todayDt)
		}
		for _, s := range need {
			p := paths[s.spec.ID]
			if !p.delta {
				continue
			}
			bk := bucketKey{s.spec.Event, s.spec.WindowDays}
			switch {
			case !snapshotted[bk]:
				p.delta, p.source, p.why = false, segmentGridLedgerSourceFull, "today's snapshot unavailable — full fallback"
			case !w.snapExistsFn(ctx, bk.event, bk.window, p.baseDt):
				// A base whose UNLOAD never completed must not be diffed
				// against: an empty/partial base reads as mass adds/removes.
				p.delta, p.source, p.why = false, segmentGridLedgerSourceFull, "base snapshot record missing — full fallback"
			}
			if !p.delta {
				log.Printf("[SegmentGrid] %q full path: %s", s.spec.Name, p.why)
				paths[s.spec.ID] = p
			}
		}
	}

	// ---- FULL-path bucket fetches — one Athena query per distinct bucket
	// still needed by at least one full-path segment.
	fullBuckets := map[bucketKey]bool{}
	for _, s := range need {
		if !paths[s.spec.ID].delta {
			fullBuckets[bucketKey{s.spec.Event, s.spec.WindowDays}] = true
		}
	}
	buckets := map[bucketKey]map[string][]string{} // key → brandApex → subscriber ids
	for bk := range fullBuckets {
		if ctx.Err() != nil {
			return false, built, skipped, failed
		}
		pairs, err := w.bucketFn(ctx, bk.event, bk.window)
		if err != nil {
			// The bucket's FULL segments all fail; the circuit counts ONE
			// fault per bucket (an Athena outage must not instantly trip on
			// the first bucket when others may succeed).
			for _, s := range need {
				if !paths[s.spec.ID].delta && s.spec.Event == bk.event && s.spec.WindowDays == bk.window {
					failed++
					w.ledgerUpsert(ctx, s.spec.ID, ledgerCountOf(ledger, s.spec.ID), srcOf(s.spec.ID), "error", 0, 0,
						"lake bucket query: "+err.Error())
				}
			}
			consecFails++
			log.Printf("[SegmentGrid] bucket %s/%dd failed: %v", bk.event, bk.window, err)
			if consecFails >= segmentGridCircuitLimit {
				return true, built, skipped, failed
			}
			continue
		}
		byBrand := map[string][]string{}
		for _, p := range pairs {
			byBrand[p.BrandApex] = append(byBrand[p.BrandApex], p.SubscriberID)
		}
		buckets[bucketKey{bk.event, bk.window}] = byBrand
	}

	// ---- DELTA diffs, one Athena anti-join per (bucket, base day) --------
	type diffKey struct {
		bk     bucketKey
		baseDt string
	}
	type segDiff struct {
		adds    []string
		removes []string
	}
	diffGroups := map[diffKey][]int{} // → indexes into need
	for i, s := range need {
		p := paths[s.spec.ID]
		if p.delta {
			dk := diffKey{bucketKey{s.spec.Event, s.spec.WindowDays}, p.baseDt}
			diffGroups[dk] = append(diffGroups[dk], i)
		}
	}
	diffs := map[string]segDiff{} // segment_id → its adds/removes
	for dk, idxs := range diffGroups {
		if ctx.Err() != nil {
			return false, built, skipped, failed
		}
		deltas, err := w.diffFn(ctx, dk.bk.event, dk.bk.window, dk.baseDt, todayDt)
		if err != nil {
			// THE RAIL: a failed Athena diff applies NOTHING — error ledger
			// rows, never a stale partial.
			for _, i := range idxs {
				s := need[i]
				failed++
				w.ledgerUpsert(ctx, s.spec.ID, ledgerCountOf(ledger, s.spec.ID), srcOf(s.spec.ID), "error", 0, 0,
					"lake diff query: "+err.Error())
			}
			consecFails++
			log.Printf("[SegmentGrid] diff %s/%dd %s→%s failed: %v", dk.bk.event, dk.bk.window, dk.baseDt, todayDt, err)
			if consecFails >= segmentGridCircuitLimit {
				return true, built, skipped, failed
			}
			continue
		}
		addsByBrand := map[string][]string{}
		removesByBrand := map[string][]string{}
		for _, d := range deltas {
			switch d.Op {
			case "add":
				addsByBrand[d.BrandApex] = append(addsByBrand[d.BrandApex], d.SubscriberID)
			case "del":
				removesByBrand[d.BrandApex] = append(removesByBrand[d.BrandApex], d.SubscriberID)
			}
		}
		for _, i := range idxs {
			s := need[i]
			diffs[s.spec.ID] = segDiff{
				adds:    addsByBrand[s.spec.BrandApex],
				removes: removesByBrand[s.spec.BrandApex],
			}
		}
	}

	// Resolve the union of subscriber ids actually needed — full segments'
	// whole bucket-brand slices plus delta segments' ADDS (removes are
	// deleted by id and need no attributes) — in ONE bounded hash join.
	wantSeeds := false
	for _, s := range need {
		if s.spec.ExcludeSeeds {
			wantSeeds = true
		}
	}
	unionSet := map[string]bool{}
	for _, s := range need {
		if p := paths[s.spec.ID]; p.delta {
			if d, ok := diffs[s.spec.ID]; ok {
				for _, id := range d.adds {
					unionSet[id] = true
				}
			}
			continue
		}
		byBrand, ok := buckets[bucketKey{s.spec.Event, s.spec.WindowDays}]
		if !ok {
			continue
		}
		for _, id := range byBrand[s.spec.BrandApex] {
			unionSet[id] = true
		}
	}
	union := make([]string, 0, len(unionSet))
	for id := range unionSet {
		union = append(union, id)
	}

	var seedListIDs []string
	if wantSeeds {
		var err error
		seedListIDs, err = w.fetchSeedListIDs(ctx)
		if err != nil {
			log.Printf("[SegmentGrid] seed list fetch failed (seed exclusion skipped this pass would WIDEN audiences — aborting): %v", err)
			// Fail-closed: without the seed set, exclude_list_pattern
			// segments would build BROADER than defined.
			for _, s := range need {
				failed++
				w.ledgerUpsert(ctx, s.spec.ID, ledgerCountOf(ledger, s.spec.ID), srcOf(s.spec.ID), "error", 0, 0,
					"seed-list fetch failed: "+err.Error())
			}
			return false, built, skipped, failed
		}
	}

	resolved := map[string]gridSubscriber{}
	if len(union) > 0 {
		var err error
		resolved, err = w.resolveFn(ctx, union, seedListIDs)
		if err != nil {
			for _, s := range need {
				failed++
				w.ledgerUpsert(ctx, s.spec.ID, ledgerCountOf(ledger, s.spec.ID), srcOf(s.spec.ID), "error", 0, 0,
					"subscriber resolve: "+err.Error())
			}
			log.Printf("[SegmentGrid] subscriber resolve failed: %v", err)
			return false, built, skipped, failed
		}
	}

	// Converted-subscriber exemption for the never-clicker rule (both
	// sources, fetched once — sidecar's fetch_converted_ids).
	var converted map[string]bool
	for _, s := range need {
		if s.spec.NeverClickersGte > 0 {
			var err error
			converted, err = w.fetchConvertedIDs(ctx)
			if err != nil {
				log.Printf("[SegmentGrid] converted-ids fetch failed (never-clicker exemption unavailable): %v", err)
				// Fail-closed for the affected segments only: applying the
				// exclusion WITHOUT the exemption would wrongly drop
				// converters, and skipping the exclusion would widen the
				// audience. Neither is acceptable — error those segments.
				converted = nil
			}
			break
		}
	}

	// Sequential per-segment rebuild (bounded concurrency = 1 by design).
	nowUTC := w.nowFn().UTC()
	for _, s := range need {
		if ctx.Err() != nil {
			return false, built, skipped, failed
		}
		spec := s.spec
		p := paths[spec.ID]

		// Candidate ids to filter: the whole bucket-brand slice (full path)
		// or just the diff's adds (delta path). Removes bypass the filters —
		// deleting a member the filters would also have dropped is a no-op.
		var rawIDs, removes []string
		if p.delta {
			d, ok := diffs[spec.ID]
			if !ok {
				continue // diff failed; already ledgered above
			}
			rawIDs, removes = d.adds, d.removes
		} else {
			byBrand, ok := buckets[bucketKey{spec.Event, spec.WindowDays}]
			if !ok {
				continue // bucket failed; already ledgered above
			}
			rawIDs = byBrand[spec.BrandApex]
		}
		if spec.NeverClickersGte > 0 && converted == nil {
			if fail(spec.ID, spec.Name, "never-clicker exemption set unavailable — refusing to build a wrong audience") {
				return true, built, skipped, failed
			}
			continue
		}

		buildStart := time.Now()
		w.ledgerMarkRunning(ctx, spec.ID, p.source)

		members := make([]gridMember, 0, len(rawIDs))
		for _, sub := range rawIDs {
			r, ok := resolved[sub]
			if !ok {
				continue // no current subscriber row / no email — dropped (sidecar parity)
			}
			if spec.ExcludeSeeds && r.IsSeed {
				continue
			}
			if spec.CreatedWithinDays > 0 {
				if !r.CreatedAt.Valid || nowUTC.Sub(r.CreatedAt.Time) > time.Duration(spec.CreatedWithinDays)*24*time.Hour {
					continue
				}
			}
			if spec.NeverClickersGte > 0 {
				// NOT(sends >= N AND clicks = 0 AND never converted) —
				// mailing_segments.go buildSegmentWhereClause arm for arm.
				if r.Sends >= spec.NeverClickersGte && r.Clicks == 0 && !converted[sub] {
					continue
				}
			}
			members = append(members, gridMember{SubscriberID: sub, Email: strings.ToLower(r.Email)})
		}

		prior := ledgerCountOf(ledger, spec.ID)
		durMs := func() int { return int(time.Since(buildStart).Milliseconds()) }

		if p.delta {
			// ---- DELTA path: guards on the DIFF SIZES, then one-TX merge.
			addN, remN := int64(len(members)), int64(len(removes))
			projected := prior - remN + addN
			if projected < 0 {
				projected = 0
			}
			deltaPct := 0.0
			if prior > 0 {
				deltaPct = float64(projected-prior) / float64(prior) * 100.0
			}
			switch {
			case addN == 0 && remN == 0:
				// No churn: 'ok' advances freshness without touching a row.
				built++
				consecFails = 0
				w.ledgerUpsert(ctx, spec.ID, prior, p.source, "ok", durMs(), 0, "")
				w.ledgerStampDeltaState(ctx, spec.ID, p.source, todayDt)
				ledger[spec.ID] = gridLedgerRow{count: prior, builtAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}, status: "ok", updatedAt: time.Now().UTC()}
				log.Printf("[SegmentGrid] %q delta: no change (%d members) in %dms", spec.Name, prior, durMs())
				continue
			case prior >= segmentGridMinGuardSize && float64(remN) > float64(prior)*segmentGridMaxDropPct/100.0:
				// The directional guard applied to the diff itself: a diff
				// that would remove the majority of a big segment is the
				// lake-gap / lost-snapshot signature, refused the same way.
				skipped++
				consecFails = 0
				w.ledgerUpsert(ctx, spec.ID, prior, p.source, "skipped_delta", durMs(), deltaPct,
					fmt.Sprintf("diff guard: %d removes against %d members (drop limit -%.0f%%)", remN, prior, segmentGridMaxDropPct))
				log.Printf("[SegmentGrid] %q SKIP(diff): %d removes vs %d members", spec.Name, remN, prior)
				continue
			case projected == 0 && prior > 0:
				skipped++
				consecFails = 0
				w.ledgerUpsert(ctx, spec.ID, prior, p.source, "skipped_delta", durMs(), deltaPct,
					fmt.Sprintf("empty result — refusing to wipe %d members via delta", prior))
				log.Printf("[SegmentGrid] %q SKIP(empty-delta): %d → 0", spec.Name, prior)
				continue
			case gridDeltaGuardTrips(prior, projected, segmentGridMaxDropPct, segmentGridMaxGrowthPct, segmentGridMinGuardSize):
				skipped++
				consecFails = 0
				w.ledgerUpsert(ctx, spec.ID, prior, p.source, "skipped_delta", durMs(), deltaPct,
					fmt.Sprintf("delta guard: %d → %d (%+.1f%%; drop limit -%.0f%%, growth limit +%.0f%%)",
						prior, projected, deltaPct, segmentGridMaxDropPct, segmentGridMaxGrowthPct))
				log.Printf("[SegmentGrid] %q SKIP(delta): %d → %d (%+.1f%%)", spec.Name, prior, projected, deltaPct)
				continue
			}
			count, err := w.mergeFn(ctx, spec.ID, members, removes)
			if err != nil {
				if fail(spec.ID, spec.Name, "delta merge: "+err.Error()) {
					return true, built, skipped, failed
				}
				continue
			}
			built++
			consecFails = 0
			totalAdds += int(addN)
			totalRemoves += int(remN)
			finalPct := 0.0
			if prior > 0 {
				finalPct = float64(count-prior) / float64(prior) * 100.0
			}
			w.ledgerUpsert(ctx, spec.ID, count, p.source, "ok", durMs(), finalPct, "")
			w.ledgerStampDeltaState(ctx, spec.ID, p.source, todayDt)
			ledger[spec.ID] = gridLedgerRow{count: count, builtAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}, status: "ok", updatedAt: time.Now().UTC()}
			log.Printf("[SegmentGrid] %q delta-merged: +%d/-%d → %d members (prior %d, %+.1f%%) in %dms",
				spec.Name, addN, remN, count, prior, finalPct, durMs())
			continue
		}

		// ---- FULL path (phase-1 semantics, unchanged).
		newN := int64(len(members))
		deltaPct := 0.0
		if prior > 0 {
			deltaPct = float64(newN-prior) / float64(prior) * 100.0
		}

		// Empty rail: never wipe an existing membership to zero (sidecar
		// --allow-empty semantics; a 0-member grid cell on an active brand
		// is an upstream fault, not intent).
		if newN == 0 && prior > 0 {
			skipped++
			consecFails = 0
			w.ledgerUpsert(ctx, spec.ID, prior, p.source, "skipped_delta", durMs(), deltaPct,
				fmt.Sprintf("empty result — refusing to wipe %d members", prior))
			log.Printf("[SegmentGrid] %q SKIP(empty): %d → 0", spec.Name, prior)
			continue
		}
		if newN == 0 && prior == 0 {
			// Nothing to write and nothing at risk: genuinely-empty warmup
			// cell. Ledger it so the skip is visible (the no-ledger-row skip
			// is the documented footgun).
			skipped++
			consecFails = 0
			w.ledgerUpsert(ctx, spec.ID, 0, p.source, "skipped_delta", durMs(), 0, "empty result on empty segment")
			continue
		}
		if gridDeltaGuardTrips(prior, newN, segmentGridMaxDropPct, segmentGridMaxGrowthPct, segmentGridMinGuardSize) {
			skipped++
			consecFails = 0
			w.ledgerUpsert(ctx, spec.ID, prior, p.source, "skipped_delta", durMs(), deltaPct,
				fmt.Sprintf("delta guard: %d → %d (%+.1f%%; drop limit -%.0f%%, growth limit +%.0f%%)",
					prior, newN, deltaPct, segmentGridMaxDropPct, segmentGridMaxGrowthPct))
			log.Printf("[SegmentGrid] %q SKIP(delta): %d → %d (%+.1f%%)", spec.Name, prior, newN, deltaPct)
			continue
		}

		if err := w.swapFn(ctx, spec.ID, members); err != nil {
			if fail(spec.ID, spec.Name, "member swap: "+err.Error()) {
				return true, built, skipped, failed
			}
			continue
		}
		built++
		consecFails = 0
		w.ledgerUpsert(ctx, spec.ID, newN, p.source, "ok", durMs(), deltaPct, "")
		// Stamp which snapshot this membership corresponds to (tomorrow's
		// diff base) — or CLEAR it when the build ran without a snapshot, so
		// a stale base can never be diffed against.
		snapDt := ""
		if snapshotted[bucketKey{spec.Event, spec.WindowDays}] {
			snapDt = todayDt
		}
		w.ledgerStampDeltaState(ctx, spec.ID, p.source, snapDt)
		ledger[spec.ID] = gridLedgerRow{count: newN, builtAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}, status: "ok", updatedAt: time.Now().UTC()}
		log.Printf("[SegmentGrid] %q built (full: %s): %d members (prior %d, %+.1f%%) in %dms",
			spec.Name, p.why, newN, prior, deltaPct, durMs())
	}
	log.Printf("[SegmentGrid] build core: %d built, %d skipped, %d failed; delta rows shipped +%d/-%d in %s",
		built, skipped, failed, totalAdds, totalRemoves, time.Since(passStart).Round(time.Millisecond))
	return false, built, skipped, failed
}

// gridSnapshotBaseTooOld reports whether a diff base day is beyond the
// allowed lookback. Unparseable inputs read as too old (full path).
func gridSnapshotBaseTooOld(baseDt, todayDt string) bool {
	b, err1 := time.Parse("2006-01-02", baseDt)
	t, err2 := time.Parse("2006-01-02", todayDt)
	if err1 != nil || err2 != nil {
		return true
	}
	return t.Sub(b) > time.Duration(segmentGridDeltaMaxBaseAgeDays)*24*time.Hour
}

func ledgerCountOf(ledger map[string]gridLedgerRow, id string) int64 {
	if lr, ok := ledger[id]; ok && lr.builtAt.Valid {
		return lr.count
	}
	return 0
}

// ---------------------------------------------------------------------------
// DB helpers
// ---------------------------------------------------------------------------

// fetchSeedListIDs mirrors the sidecar's fetch_seed_list_ids.
func (w *SegmentGridWorker) fetchSeedListIDs(ctx context.Context) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(cctx, `SELECT id::text FROM mailing_lists WHERE name ILIKE '%seed%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// fetchConvertedIDs mirrors the sidecar's fetch_converted_ids: subscriber
// ids with a recorded conversion from BOTH sources (a converter did engage;
// their conversion click may simply not be logged).
func (w *SegmentGridWorker) fetchConvertedIDs(ctx context.Context) (map[string]bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out := map[string]bool{}
	for _, q := range []string{
		`SELECT DISTINCT subscriber_id::text FROM mailing_offer_suppressions
		  WHERE reason = 'converted' AND subscriber_id IS NOT NULL`,
		`SELECT DISTINCT sub1 FROM mailing_cpm_manual_conversions
		  WHERE sub1 IS NOT NULL AND sub1 <> ''`,
	} {
		rows, err := w.db.QueryContext(cctx, q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// resolveSubscribers is the sidecar's resolve_emails(): COPY the engaged ids
// into a TEMP table and force a hash join so Postgres does ONE sequential
// scan of mailing_subscribers instead of ~200k random PK heap reads. The
// seed flag and the attribute columns (created_at, sends, clicks) ride the
// same scan — no extra pass.
func (w *SegmentGridWorker) resolveSubscribers(ctx context.Context, ids []string, seedListIDs []string) (map[string]gridSubscriber, error) {
	out := make(map[string]gridSubscriber, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(segmentGridResolveTimeoutMs)*time.Millisecond+time.Minute)
	defer cancel()

	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit

	if _, err := tx.ExecContext(cctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", segmentGridResolveTimeoutMs)); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(cctx, "CREATE TEMP TABLE _grid_need (id uuid) ON COMMIT DROP"); err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(cctx, pq.CopyIn("_grid_need", "id"))
	if err != nil {
		return nil, fmt.Errorf("prepare copy: %w", err)
	}
	for _, id := range ids {
		if _, perr := uuid.Parse(id); perr != nil {
			continue // defensive: a malformed lake id must not abort the COPY
		}
		if _, err := stmt.ExecContext(cctx, id); err != nil {
			_ = stmt.Close()
			return nil, fmt.Errorf("copy id: %w", err)
		}
	}
	if _, err := stmt.ExecContext(cctx); err != nil { // flush
		_ = stmt.Close()
		return nil, fmt.Errorf("flush copy: %w", err)
	}
	if err := stmt.Close(); err != nil {
		return nil, fmt.Errorf("close copy: %w", err)
	}
	if _, err := tx.ExecContext(cctx, "ANALYZE _grid_need"); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(cctx, "SET LOCAL enable_nestloop = off"); err != nil {
		return nil, err
	}

	seedArr := seedListIDs
	if seedArr == nil {
		seedArr = []string{}
	}
	rows, err := tx.QueryContext(cctx, `
		SELECT s.id::text, s.email, (s.list_id = ANY($1::uuid[])),
		       s.created_at, COALESCE(s.total_emails_received, 0), COALESCE(s.total_clicks, 0)
		FROM mailing_subscribers s
		JOIN _grid_need n ON n.id = s.id
		WHERE s.email IS NOT NULL AND s.email <> ''
	`, pq.Array(seedArr))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var r gridSubscriber
		if err := rows.Scan(&id, &r.Email, &r.IsSeed, &r.CreatedAt, &r.Sends, &r.Clicks); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan: %w", err)
		}
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		// A truncated resolve is a WRONG resolve: absent subscribers read as
		// "no email row" and silently shrink every audience. Abort.
		return nil, fmt.Errorf("resolve read truncated: %w", err)
	}
	rows.Close()
	if err := tx.Commit(); err != nil { // drops the temp table
		return nil, err
	}
	return out, nil
}

// writeSegmentMembers swaps one segment's membership in ONE transaction under
// pg_advisory_xact_lock(hashtext(segment_id)) — the same per-segment key the
// nightly Fargate chain and internal/api's writeLakeSegmentMembers take, so
// no two builders can interleave a DELETE/INSERT on the same segment. KEEP
// IN SYNC with internal/api/lake_segment_builder.go writeLakeSegmentMembers
// (this lives in the worker package because internal/api already imports
// internal/worker — same reason the partner-drip ledger SQL is inlined).
func (w *SegmentGridWorker) writeSegmentMembers(ctx context.Context, segmentID string, members []gridMember) error {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(segmentGridSwapTimeoutMs)*time.Millisecond+time.Minute)
	defer cancel()

	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit

	if _, err := tx.ExecContext(cctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", segmentGridSwapTimeoutMs)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(cctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, segmentID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	if _, err := tx.ExecContext(cctx, `DELETE FROM mailing_segment_members WHERE segment_id = $1::uuid`, segmentID); err != nil {
		return fmt.Errorf("delete old members: %w", err)
	}

	stmt, err := tx.PrepareContext(cctx, pq.CopyIn(
		"mailing_segment_members", "segment_id", "subscriber_id", "email", "materialized_at"))
	if err != nil {
		return fmt.Errorf("prepare copy: %w", err)
	}
	now := time.Now().UTC()
	seen := make(map[string]bool, len(members))
	written := 0
	for _, m := range members {
		if m.SubscriberID == "" || m.Email == "" || seen[m.SubscriberID] {
			continue
		}
		seen[m.SubscriberID] = true
		if _, err := stmt.ExecContext(cctx, segmentID, m.SubscriberID, m.Email, now); err != nil {
			_ = stmt.Close()
			return fmt.Errorf("copy member: %w", err)
		}
		written++
	}
	if _, err := stmt.ExecContext(cctx); err != nil { // flush
		_ = stmt.Close()
		return fmt.Errorf("flush copy: %w", err)
	}
	if err := stmt.Close(); err != nil {
		return fmt.Errorf("close copy: %w", err)
	}

	if _, err := tx.ExecContext(cctx, `
		UPDATE mailing_segments
		   SET subscriber_count = $1, last_calculated_at = NOW(), updated_at = NOW()
		 WHERE id = $2::uuid
	`, written, segmentID); err != nil {
		return fmt.Errorf("update cached count: %w", err)
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Ledger writes (inlined SQL — KEEP IN SYNC with internal/api/segment_ledger.go
// UpsertSegmentLedger / MarkSegmentBuildRunning; this package cannot import
// internal/api). subscriber_count and last_built_at advance ONLY on 'ok' — a
// skipped/errored build must neither claim freshly-built nor overwrite the
// last good count. Best-effort: errors are logged, never control flow.
// ---------------------------------------------------------------------------

func (w *SegmentGridWorker) ledgerMarkRunning(ctx context.Context, segmentID, source string) {
	wctx, cancel := gridLedgerCtx(ctx)
	defer cancel()
	if _, err := w.db.ExecContext(wctx, `
		INSERT INTO mailing_segment_build_ledger
			(segment_id, build_source, last_build_status, updated_at)
		VALUES ($1::uuid, $2, 'running', NOW())
		ON CONFLICT (segment_id) DO UPDATE SET
			build_source      = EXCLUDED.build_source,
			last_build_status = 'running',
			last_error        = NULL,
			updated_at        = NOW()
	`, segmentID, source); err != nil {
		log.Printf("[SegmentGrid] ledger mark-running failed for %s: %v", segmentID, err)
	}
}

func (w *SegmentGridWorker) ledgerUpsert(ctx context.Context, segmentID string, count int64, source, status string, durMs int, deltaPct float64, errMsg string) {
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	wctx, cancel := gridLedgerCtx(ctx)
	defer cancel()
	if _, err := w.db.ExecContext(wctx, `
		INSERT INTO mailing_segment_build_ledger
			(segment_id, subscriber_count, last_built_at, build_source,
			 last_build_status, last_build_ms, last_delta_pct, last_error, updated_at)
		VALUES ($1::uuid, $2,
		        CASE WHEN $4::text = 'ok' THEN NOW() END,
		        $3, $4, $5, $6, NULLIF($7, ''), NOW())
		ON CONFLICT (segment_id) DO UPDATE SET
			subscriber_count  = CASE WHEN EXCLUDED.last_build_status = 'ok'
			                         THEN EXCLUDED.subscriber_count
			                         ELSE mailing_segment_build_ledger.subscriber_count END,
			last_built_at     = CASE WHEN EXCLUDED.last_build_status = 'ok'
			                         THEN NOW()
			                         ELSE mailing_segment_build_ledger.last_built_at END,
			build_source      = EXCLUDED.build_source,
			last_build_status = EXCLUDED.last_build_status,
			last_build_ms     = EXCLUDED.last_build_ms,
			last_delta_pct    = EXCLUDED.last_delta_pct,
			last_error        = EXCLUDED.last_error,
			updated_at        = NOW()
	`, segmentID, count, source, status, durMs, deltaPct, errMsg); err != nil {
		log.Printf("[SegmentGrid] ledger upsert(%s) failed for %s: %v", status, segmentID, err)
	}
}

// gridLedgerCtx detaches from an already-cancelled caller context so a
// timed-out build can still record its terminal status.
func gridLedgerCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil || ctx.Err() != nil {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}
	return context.WithTimeout(ctx, 10*time.Second)
}

// ---------------------------------------------------------------------------
// On-demand queue
// ---------------------------------------------------------------------------

// drainQueue serves mailing_segment_refresh_requests: re-queues stale claims,
// claims a bounded batch atomically, and builds each request through the
// same guarded core the daily pass uses. Caller holds the distlock.
func (w *SegmentGridWorker) drainQueue(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// Crash recovery: a claim whose owner died mid-build goes back to queued.
	if _, err := w.db.ExecContext(ctx, `
		UPDATE mailing_segment_refresh_requests
		   SET status = 'queued', started_at = NULL
		 WHERE status = 'running' AND started_at < NOW() - ($1 * INTERVAL '1 second')
	`, int(segmentGridRequestStaleAfter.Seconds())); err != nil {
		log.Printf("[SegmentGrid] stale-claim requeue failed: %v", err)
	}

	rows, err := w.db.QueryContext(ctx, `
		UPDATE mailing_segment_refresh_requests
		   SET status = 'running', started_at = NOW()
		 WHERE id IN (
			SELECT id FROM mailing_segment_refresh_requests
			 WHERE status = 'queued'
			 ORDER BY requested_at
			 LIMIT $1
			 FOR UPDATE SKIP LOCKED
		 )
		RETURNING id::text, segment_id::text
	`, segmentGridClaimBatch)
	if err != nil {
		log.Printf("[SegmentGrid] queue claim failed: %v", err)
		return
	}
	type claim struct{ id, segmentID string }
	var claims []claim
	for rows.Next() {
		var c claim
		if err := rows.Scan(&c.id, &c.segmentID); err != nil {
			continue
		}
		claims = append(claims, c)
	}
	claimReadErr := rows.Err()
	rows.Close()
	if claimReadErr != nil {
		log.Printf("[SegmentGrid] queue claim read error: %v", claimReadErr)
	}
	if len(claims) == 0 {
		return
	}
	log.Printf("[SegmentGrid] draining %d refresh request(s)", len(claims))

	// Load + parse the requested segments.
	segIDs := make([]string, len(claims))
	for i, c := range claims {
		segIDs[i] = c.segmentID
	}
	segRows, err := w.db.QueryContext(ctx, `
		SELECT id::text, name, COALESCE(conditions::text, '[]'), organization_id::text, status
		FROM mailing_segments WHERE id = ANY($1::uuid[])
	`, pq.Array(segIDs))
	if err != nil {
		log.Printf("[SegmentGrid] request segment load failed: %v", err)
		for _, c := range claims {
			w.finishRequest(ctx, c.id, "failed", "segment load failed: "+err.Error())
		}
		return
	}
	type segMeta struct {
		row    gridSegmentRow
		status string
		name   string
		ok     bool
		reason string
	}
	metas := map[string]segMeta{}
	for segRows.Next() {
		var id, name, conds, org, status string
		if err := segRows.Scan(&id, &name, &conds, &org, &status); err != nil {
			continue
		}
		m := segMeta{name: name, status: status}
		if status != "active" {
			m.reason = "segment is not active (status=" + status + ")"
		} else if spec, perr := parseGridConditions(name, conds); perr != nil {
			m.reason = "not a lake-expressible grid segment (" + perr.Error() + ") — use POST /v2/segments/{id}/recalculate"
		} else {
			spec.ID = id
			m.row = gridSegmentRow{spec: *spec, orgID: org}
			m.ok = true
		}
		metas[id] = m
	}
	segReadErr := segRows.Err()
	segRows.Close()
	if segReadErr != nil {
		log.Printf("[SegmentGrid] request segment read error: %v", segReadErr)
	}

	var need []gridSegmentRow
	for _, c := range claims {
		m, found := metas[c.segmentID]
		switch {
		case !found:
			w.finishRequest(ctx, c.id, "failed", "segment not found")
		case !m.ok:
			w.finishRequest(ctx, c.id, "failed", m.reason)
		default:
			need = append(need, m.row)
		}
	}
	if len(need) == 0 {
		return
	}
	if !w.readerEnabledFn() {
		for _, c := range claims {
			if m, found := metas[c.segmentID]; found && m.ok {
				w.finishRequest(ctx, c.id, "failed", "analytics lake reader disabled")
			}
		}
		return
	}

	ids := make([]string, len(need))
	for i, s := range need {
		ids[i] = s.spec.ID
	}
	ledger, err := w.loadLedger(ctx, ids)
	if err != nil {
		for _, c := range claims {
			if m, found := metas[c.segmentID]; found && m.ok {
				w.finishRequest(ctx, c.id, "failed", "ledger read failed: "+err.Error())
			}
		}
		return
	}

	w.buildSegments(ctx, need, ledger)

	// Report each request from its segment's ledger outcome.
	final, err := w.loadLedger(ctx, ids)
	if err != nil {
		final = map[string]gridLedgerRow{}
	}
	for _, c := range claims {
		m, found := metas[c.segmentID]
		if !found || !m.ok {
			continue // already finished above
		}
		lr, ok := final[c.segmentID]
		switch {
		case ok && lr.status == "ok":
			w.finishRequest(ctx, c.id, "done", fmt.Sprintf("rebuilt: %d members", lr.count))
		case ok && lr.status == "skipped_delta":
			w.finishRequest(ctx, c.id, "failed", "refused by safety guard — see the segment's build ledger (last_build_status=skipped_delta)")
		case ok && lr.status == "error":
			w.finishRequest(ctx, c.id, "failed", "build failed — see the segment's build ledger last_error")
		default:
			w.finishRequest(ctx, c.id, "failed", "build did not complete")
		}
	}
}

func (w *SegmentGridWorker) finishRequest(ctx context.Context, requestID, status, reason string) {
	wctx, cancel := gridLedgerCtx(ctx)
	defer cancel()
	if len(reason) > 500 {
		reason = reason[:500]
	}
	if _, err := w.db.ExecContext(wctx, `
		UPDATE mailing_segment_refresh_requests
		   SET status = $2, reason = $3, finished_at = NOW()
		 WHERE id = $1::uuid
	`, requestID, status, reason); err != nil {
		log.Printf("[SegmentGrid] request finish(%s) failed for %s: %v", status, requestID, err)
	}
}
