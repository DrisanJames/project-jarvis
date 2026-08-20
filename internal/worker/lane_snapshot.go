package worker

// LaneSnapshotWorker — a timed Athena → JSON → S3 snapshot of TODAY's lane
// activity. Deliberately simple: snapshot in, file out, endpoint reads the
// file.
//
// WHAT THIS REPLACES (and what it must NOT become)
// ------------------------------------------------
// The prior accelerator, LaneStatsRollupWorker, precomputed the same scoreboard
// into Postgres. It ran on the SAME RDS instance as the request path and
// starved it: GET /property-ledger/stats went from "slow but converging" to
// failing on every call with "campaign resolution failed: canceling statement
// due to user request". It was switched off (LANE_STATS_ROLLUP_DISABLED,
// commit fc7fec3) and stays on disk behind that flag.
//
// So the standing constraints on THIS worker are:
//   * the heavy scan is on the LAKE, never on Postgres;
//   * exactly ONE small Postgres query per tick (the campaign→lane mapping);
//   * NO read-through tiers, NO freshness arbitration, NO cache layers, NO
//     stale-serve logic. The endpoint reads whatever the last snapshot was and
//     says how old it is. Anything more is rebuilding what was removed.
//
// IN-PROCESS ON PURPOSE — NOT A CRON. This repo has a documented history of
// cron jobs that were written, documented, and never armed (welcome_saturation,
// drip_lane_release, the kumo newsletter stage; CLAUDE.md §13.1). An in-process
// ticker cannot be un-armed: if the server is up, the snapshot is running.
//
// EACH TICK
//   1. ONE Athena query for today's Denver day (analytics.LaneSnapshot →
//      BuildLaneSnapshotSQL). Present-tense 'open'/'click'; dt pinned to the
//      two UTC partitions a Denver day spans.
//   2. ONE Postgres query resolving campaign_id → (org, vertical, brand) for
//      partner-drip campaigns created in the last laneSnapshotCampaignDays
//      days. created_at LEADS; the name regex is a residual filter. Measured on
//      prod 2026-08-19: Index Scan on idx_campaigns_org_status_created,
//      254 ms, 27,845 rows for a 10-day window.
//   3. Aggregate in memory to (org, vertical, brand, isp, source).
//   4. Publish to the in-process latest-snapshot slot, then PUT ONE JSON object
//      to S3. Memory is written FIRST so an S3 outage still leaves the endpoint
//      a fresh answer.
//
// HONESTY RULES BAKED IN (these are not cosmetic)
//   * A campaign_id the mapping query does not cover is DROPPED and COUNTED
//     (UnmappedCampaigns / UnmappedEvents) — never attributed to a lane it does
//     not belong to, and never silently discarded.
//   * source survives into every row. source='kumo' carries NO open/click rows
//     in the lake (CLAUDE.md §5.4), so a kumo row's engagement fields are NULL
//     with engagement_available=false — ABSENT, not zero. If kumo ever does
//     emit engagement the fields populate on their own.
//   * source='app' is the PG→lake mirror and DOUBLE-COUNTS deliveries against
//     ses/pmta (verified 2026-07-28). Rows stay split by source so the
//     double-count is visible instead of being summed away.
//   * open_uniq/click_uniq are SUMS OF PER-CAMPAIGN uniques (the lake query
//     groups by campaign_id, which is the only path to vertical/brand), so a
//     subscriber active in two campaigns of one lane counts twice. Stated in
//     the payload's notes, never presented as a lane-level distinct count.
//
// NEVER DELETES. The only write is PutObject on today's own key, overwritten
// each tick.
//
// Kill switch: LANE_SNAPSHOT_DISABLED (a disabled tick emits one heartbeat with
// status 'disabled' and runs nothing else).
// Env: LANE_SNAPSHOT_BUCKET (default JARVIS_S3_BUCKET), LANE_SNAPSHOT_PREFIX
// (default "lane-snapshots/"), LANE_SNAPSHOT_REGION (default JARVIS_S3_REGION,
// then us-west-2).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	laneSnapshotWorkerName = "lane_snapshot"
	laneSnapshotLockKey    = "lane_snapshot"

	// DefaultLaneSnapshotInterval — the operator's cadence: "a snapshot from
	// athena that is ran on a per 5 min basis".
	DefaultLaneSnapshotInterval = 5 * time.Minute

	// laneSnapshotCampaignDays bounds the campaign-mapping query. The drip
	// reminder ladder is ~72h, so a campaign created several days ago can still
	// emit events today; 10 days is generous cover at 254 ms.
	// 10 -> 4 days. The snapshot only covers TODAY, and a campaign can only emit
	// events today if it mailed recently; 4 days keeps the ladder's ~72h tail.
	// Measured: 10d = 27,835 campaigns, 4d = 12,619 — less than half the rows to
	// transfer on every tick, for the same answer.
	laneSnapshotCampaignDays = 4

	// laneSnapshotLakeBudget / laneSnapshotPGBudget bound the two queries.
	// The lake query measured 9.6s for a full Denver day; the PG query 254 ms.
	// Both sit well inside their budgets, and the PG one sits under the prod
	// 30s statement_timeout by construction.
	laneSnapshotLakeBudget = 5 * time.Minute
	// RAISED 25s -> 120s (2026-08-19, measured on prod). The mapping query runs
	// in ~1.1s standalone, but it failed on TWO consecutive live ticks with
	// "canceling statement due to user request" — a CONTEXT cancellation, not a
	// statement timeout. The tick ctx has no deadline, so the 25s was genuinely
	// elapsing: w.db is the pool shared by the server's ~60 workers, and the
	// budget was being spent WAITING FOR A CONNECTION before the query even ran.
	// This worker is off the request path, so waiting is free; failing is not.
	laneSnapshotPGBudget  = 120 * time.Second
	laneSnapshotPutBudget = 60 * time.Second

	// laneSnapshotDefaultPrefix is the S3 key prefix. Key =
	// <prefix><denver-day>.json, overwritten every tick.
	laneSnapshotDefaultPrefix = "lane-snapshots/"

	// laneSnapshotCampaignPrefix is the partner-drip campaign-name prefix.
	// Names are `[partner-drip] <vertical> <brand> <stamp> …` (verified against
	// prod 2026-08-19).
	laneSnapshotCampaignPrefix = "[partner-drip] "

	// laneSnapshotSource labels the payload so no screen can mistake these
	// numbers for the PG tracking-events path.
	laneSnapshotSource = "athena_lake_snapshot"
)

// laneSnapshotUniqNote and friends ride in the payload. They exist so a reader
// of the JSON cannot arrive at a wrong number by summing the obvious way.
const (
	laneSnapshotUniqNote = "open_uniq/click_uniq are SUMS OF PER-CAMPAIGN distinct subscribers (the lake query groups by campaign_id, the only path to vertical/brand). A subscriber active in two campaigns of one lane counts twice. They are not lane-level distinct counts."
	laneSnapshotSrcNote  = "Rows are split by source and MUST NOT be summed blindly: source='app' is the PG→lake mirror and double-counts deliveries against ses/pmta. source='kumo' carries no open/click rows in the lake, so its engagement is null (absent), never zero."
	laneSnapshotMapNote  = "Rows come only from campaigns resolvable to a lane via mailing_campaigns. Lake activity from campaigns outside that set is counted in `unmapped`, never attributed to a lane."
)

// laneSnapshotNoEngagementSources are transports the lake carries no
// open/click rows for (CLAUDE.md §5.4, verified 2026-08-11). Their engagement
// fields are reported ABSENT rather than zero — unless the snapshot actually
// observes engagement for them, in which case the real numbers win and this
// list stops mattering for that tick.
var laneSnapshotNoEngagementSources = map[string]bool{"kumo": true}

func laneSnapshotDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LANE_SNAPSHOT_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// ── payload ─────────────────────────────────────────────────────────────────

// LaneSnapshotRow is one (vertical, brand, isp, source) cell of a Denver day.
//
// The engagement fields are POINTERS on purpose: nil marshals to JSON null,
// which is how "this transport does not report engagement" is distinguished
// from "this transport reported zero engagement". Collapsing the two is exactly
// the silent-zero failure this file exists to avoid.
type LaneSnapshotRow struct {
	OrganizationID string `json:"organization_id"`
	Vertical       string `json:"vertical"`
	Brand          string `json:"brand"`
	ISP            string `json:"isp"`
	Source         string `json:"source"`

	Attempted int64 `json:"attempted"`
	Delivered int64 `json:"delivered"`
	Bounced   int64 `json:"bounced"`

	OpenUniq    *int64 `json:"open_uniq"`
	ClickUniq   *int64 `json:"click_uniq"`
	OpenEvents  *int64 `json:"open_events"`
	ClickEvents *int64 `json:"click_events"`

	// EngagementAvailable is false when this source reports no open/click into
	// the lake at all. When false the four fields above are null.
	EngagementAvailable bool `json:"engagement_available"`

	// Campaigns is how many distinct campaigns fed this cell.
	Campaigns int `json:"campaigns"`
}

// LaneSnapshotUnmapped records lake activity that could not be attributed to a
// lane. Reported, never dropped silently and never guessed at.
type LaneSnapshotUnmapped struct {
	Campaigns int   `json:"campaigns"`
	Events    int64 `json:"events"`
}

// LaneSnapshot is the whole file: one Denver day, every lane.
type LaneSnapshot struct {
	Day        string `json:"day"`         // Denver calendar day, YYYY-MM-DD
	CapturedAt string `json:"captured_at"` // RFC3339 UTC, when the tick finished aggregating
	Source     string `json:"source"`

	Rows     []LaneSnapshotRow    `json:"rows"`
	Unmapped LaneSnapshotUnmapped `json:"unmapped"`

	// LakeRows / MappedCampaigns are provenance: how much the lake returned and
	// how much of it landed in a lane.
	LakeRows        int `json:"lake_rows"`
	MappedCampaigns int `json:"mapped_campaigns"`

	Notes []string `json:"notes"`
}

// LaneSnapshotCampaign is one resolved campaign→lane mapping.
type LaneSnapshotCampaign struct {
	OrganizationID string
	Vertical       string
	Brand          string
}

// ── seams (tests inject these; nothing here touches AWS in a test) ──────────

// LaneSnapshotLakeReader is the worker's seam over the Athena reader.
type LaneSnapshotLakeReader interface {
	LaneSnapshot(ctx context.Context, denverDay string) ([]analytics.LaneSnapshotLakeRow, error)
}

// LaneSnapshotStore is the worker's seam over object storage.
type LaneSnapshotStore interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// analyticsLakeReader adapts the package-level analytics reader.
type analyticsLakeReader struct{}

func (analyticsLakeReader) LaneSnapshot(ctx context.Context, denverDay string) ([]analytics.LaneSnapshotLakeRow, error) {
	return analytics.LaneSnapshot(ctx, denverDay)
}

// s3LaneSnapshotStore is the real store: PutObject/GetObject on one key.
type s3LaneSnapshotStore struct {
	client *s3.Client
	bucket string
}

func (s *s3LaneSnapshotStore) Put(ctx context.Context, key string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("s3 put s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

func (s *s3LaneSnapshotStore) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get s3://%s/%s: %w", s.bucket, key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// ── the in-process latest slot ──────────────────────────────────────────────
//
// The endpoint serves from here first. It exists so an S3 read failure cannot
// blank the screen — NOT as a cache tier: there is no TTL, no arbitration, no
// stale/fresh decision. It is simply "the last thing this process computed",
// and the payload's captured_at lets the UI say how old that is.

var (
	laneSnapshotMu     sync.RWMutex
	laneSnapshotLatest *LaneSnapshot
	laneSnapshotStore  LaneSnapshotStore
	laneSnapshotPrefix = laneSnapshotDefaultPrefix
	laneSnapshotLoc    = laneSnapshotLocation()
)

func laneSnapshotLocation() *time.Location {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[LaneSnapshot] America/Denver tz unavailable (%v) — falling back to UTC day buckets", err)
		return time.UTC
	}
	return loc
}

// LaneSnapshotDenverDay returns t's Denver calendar day as YYYY-MM-DD.
func LaneSnapshotDenverDay(t time.Time) string {
	return t.In(laneSnapshotLoc).Format("2006-01-02")
}

// LaneSnapshotObjectKey is the S3 key for a Denver day. One key per day,
// overwritten on every tick.
func LaneSnapshotObjectKey(prefix, day string) string {
	if prefix == "" {
		prefix = laneSnapshotDefaultPrefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + day + ".json"
}

// publishLaneSnapshot installs the newest snapshot in the in-process slot.
func publishLaneSnapshot(s *LaneSnapshot) {
	laneSnapshotMu.Lock()
	laneSnapshotLatest = s
	laneSnapshotMu.Unlock()
}

// registerLaneSnapshotStore lets LoadLaneSnapshot fall back to S3.
func registerLaneSnapshotStore(store LaneSnapshotStore, prefix string) {
	laneSnapshotMu.Lock()
	laneSnapshotStore = store
	if prefix != "" {
		laneSnapshotPrefix = prefix
	}
	laneSnapshotMu.Unlock()
}

// SetLaneSnapshotForTest installs a snapshot (and optional store) into the
// in-process slot and restores the previous state via t.Cleanup-style undo.
// Exported for the API package's handler tests, which must be able to exercise
// both the "no snapshot" and "snapshot present" paths without AWS.
func SetLaneSnapshotForTest(s *LaneSnapshot, store LaneSnapshotStore) func() {
	laneSnapshotMu.Lock()
	prevSnap, prevStore := laneSnapshotLatest, laneSnapshotStore
	laneSnapshotLatest, laneSnapshotStore = s, store
	laneSnapshotMu.Unlock()
	return func() {
		laneSnapshotMu.Lock()
		laneSnapshotLatest, laneSnapshotStore = prevSnap, prevStore
		laneSnapshotMu.Unlock()
	}
}

// LoadLaneSnapshot returns the newest snapshot this process can reach and the
// storage it came from ("memory" or "s3"). It returns (nil, "") when no
// snapshot exists anywhere — the caller MUST render that as an explicit
// "no snapshot yet" state, never as an empty row list.
//
// This is a two-step lookup, not a cache tier: memory is the thing the worker
// just wrote; S3 covers a task that has booted but not yet ticked (or one whose
// Athena query is failing while another task's snapshot is on disk).
func LoadLaneSnapshot(ctx context.Context) (*LaneSnapshot, string) {
	laneSnapshotMu.RLock()
	snap, store, prefix := laneSnapshotLatest, laneSnapshotStore, laneSnapshotPrefix
	laneSnapshotMu.RUnlock()

	if snap != nil {
		return snap, "memory"
	}
	if store == nil {
		return nil, ""
	}
	key := LaneSnapshotObjectKey(prefix, LaneSnapshotDenverDay(time.Now()))
	body, err := store.Get(ctx, key)
	if err != nil {
		log.Printf("[LaneSnapshot] S3 read %s failed: %v (endpoint will report no snapshot)", key, err)
		return nil, ""
	}
	var out LaneSnapshot
	if err := json.Unmarshal(body, &out); err != nil {
		log.Printf("[LaneSnapshot] S3 object %s is not a snapshot: %v", key, err)
		return nil, ""
	}
	return &out, "s3"
}

// ── the worker ──────────────────────────────────────────────────────────────

// LaneSnapshotWorker owns the 5-minute snapshot pass. Construct via
// NewLaneSnapshotWorker, then Start(ctx) once at boot.
type LaneSnapshotWorker struct {
	db       *sql.DB
	redis    *redis.Client
	interval time.Duration
	loc      *time.Location

	bucket string
	prefix string
	region string

	lake  LaneSnapshotLakeReader
	store LaneSnapshotStore

	lakeAvailable func() bool
}

// NewLaneSnapshotWorker wires the worker. redisClient may be nil — the distlock
// falls back to a PG advisory lock (the sibling contract, see
// property_intro_rollup.go). Bucket/prefix/region come from env so the deploy
// can retarget the snapshot without a code change.
func NewLaneSnapshotWorker(db *sql.DB, redisClient *redis.Client) *LaneSnapshotWorker {
	bucket := strings.TrimSpace(os.Getenv("LANE_SNAPSHOT_BUCKET"))
	if bucket == "" {
		bucket = strings.TrimSpace(os.Getenv("JARVIS_S3_BUCKET"))
	}
	prefix := strings.TrimSpace(os.Getenv("LANE_SNAPSHOT_PREFIX"))
	if prefix == "" {
		prefix = laneSnapshotDefaultPrefix
	}
	region := strings.TrimSpace(os.Getenv("LANE_SNAPSHOT_REGION"))
	if region == "" {
		region = strings.TrimSpace(os.Getenv("JARVIS_S3_REGION"))
	}
	if region == "" {
		region = "us-west-2"
	}
	return &LaneSnapshotWorker{
		db:            db,
		redis:         redisClient,
		interval:      DefaultLaneSnapshotInterval,
		loc:           laneSnapshotLoc,
		bucket:        bucket,
		prefix:        prefix,
		region:        region,
		lake:          analyticsLakeReader{},
		lakeAvailable: analytics.ReaderEnabled,
	}
}

// WithInterval overrides the tick cadence (tests). Call before Start.
func (w *LaneSnapshotWorker) WithInterval(d time.Duration) *LaneSnapshotWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

// SetLakeReader injects the Athena seam (tests). Call before Start.
func (w *LaneSnapshotWorker) SetLakeReader(r LaneSnapshotLakeReader) *LaneSnapshotWorker {
	w.lake = r
	w.lakeAvailable = func() bool { return true }
	return w
}

// SetStore injects the object-storage seam (tests). Call before Start.
func (w *LaneSnapshotWorker) SetStore(s LaneSnapshotStore) *LaneSnapshotWorker {
	w.store = s
	return w
}

// Start runs the tick loop until ctx is cancelled. Non-blocking; no-op if db
// is nil.
func (w *LaneSnapshotWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[LaneSnapshot] disabled (db missing)")
		return
	}
	go func() {
		log.Printf("Lane snapshot worker started (interval=%s, day=Denver, lake=athena, bucket=%s, prefix=%s)",
			w.interval, w.bucket, w.prefix)
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-ctx.Done():
				log.Printf("[LaneSnapshot] context cancelled, stopping")
				return
			}
		}
	}()
}

// tick is one leased pass.
func (w *LaneSnapshotWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if laneSnapshotDisabled() {
		EmitHeartbeat(ctx, w.db, laneSnapshotWorkerName, int(w.interval.Seconds()), "disabled", "LANE_SNAPSHOT_DISABLED set")
		return
	}
	lock := distlock.NewLock(w.redis, w.db, laneSnapshotLockKey, w.interval)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[LaneSnapshot] lock acquire error: %v", err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[LaneSnapshot] lock release error: %v", err)
		}
	}()
	w.RunOnce(ctx)
}

// RunOnce executes one full pass. Exported so tests and operational tooling can
// drive a pass directly; the caller owns locking.
//
// RE-RUN SAFE BY CONSTRUCTION: the pass has no accumulating state. It
// recomputes today from scratch, replaces the in-memory slot, and overwrites
// exactly one S3 key. Running it twice produces the same file, never doubled
// numbers and never a second object.
func (w *LaneSnapshotWorker) RunOnce(ctx context.Context) {
	if w.lakeAvailable != nil && !w.lakeAvailable() {
		log.Printf("[LaneSnapshot] lake reader disabled (ANALYTICS_ATHENA_OUTPUT unset) — nothing to snapshot")
		EmitHeartbeat(ctx, w.db, laneSnapshotWorkerName, int(w.interval.Seconds()), "disabled", "lake reader not configured")
		return
	}

	day := LaneSnapshotDenverDay(time.Now())

	// (1) THE LAKE — one query, off the request path entirely.
	lctx, lcancel := context.WithTimeout(ctx, laneSnapshotLakeBudget)
	lakeRows, err := w.lake.LaneSnapshot(lctx, day)
	lcancel()
	if err != nil {
		if analytics.IsDisabledErr(err) {
			EmitHeartbeat(ctx, w.db, laneSnapshotWorkerName, int(w.interval.Seconds()), "disabled", "lake reader not configured")
			return
		}
		log.Printf("[LaneSnapshot] athena query failed for %s: %v", day, err)
		EmitHeartbeat(ctx, w.db, laneSnapshotWorkerName, int(w.interval.Seconds()), "error", "athena: "+err.Error())
		return
	}

	// (2) THE ONE POSTGRES QUERY. Keep it that way — PG contention is what
	// killed the design this replaces.
	pctx, pcancel := context.WithTimeout(ctx, laneSnapshotPGBudget)
	mapping, err := w.loadCampaignLanes(pctx)
	pcancel()
	if err != nil {
		log.Printf("[LaneSnapshot] campaign mapping failed: %v", err)
		EmitHeartbeat(ctx, w.db, laneSnapshotWorkerName, int(w.interval.Seconds()), "error", "campaign mapping: "+err.Error())
		return
	}

	// (3) Aggregate in memory.
	snap := BuildLaneSnapshot(day, time.Now(), lakeRows, mapping)

	// (4) Memory FIRST — an S3 failure must not cost us a fresh answer.
	publishLaneSnapshot(snap)

	status, detail := "ok", ""
	if err := w.writeSnapshot(ctx, snap); err != nil {
		log.Printf("[LaneSnapshot] %v", err)
		status, detail = "error", err.Error()
	}
	log.Printf("[LaneSnapshot] %s: %d lake rows → %d lane cells (%d campaigns mapped, %d unmapped / %d events) write=%s",
		day, snap.LakeRows, len(snap.Rows), snap.MappedCampaigns,
		snap.Unmapped.Campaigns, snap.Unmapped.Events, status)
	EmitHeartbeat(ctx, w.db, laneSnapshotWorkerName, int(w.interval.Seconds()), status, detail)
}

// writeSnapshot marshals and PUTs the one object for the day.
func (w *LaneSnapshotWorker) writeSnapshot(ctx context.Context, snap *LaneSnapshot) error {
	store, err := w.resolveStore(ctx)
	if err != nil {
		return err
	}
	if store == nil {
		// No bucket configured: memory-only is a legitimate degraded mode, and
		// the endpoint still answers. Say so rather than pretending it wrote.
		return fmt.Errorf("no snapshot bucket configured (set LANE_SNAPSHOT_BUCKET or JARVIS_S3_BUCKET) — snapshot is in-memory only")
	}
	// Arm the endpoint's S3 read BEFORE attempting the write: a task whose PUT
	// keeps failing must still be able to read a sibling task's object, and a
	// task that has not ticked yet gets the store the moment one does.
	registerLaneSnapshotStore(store, w.prefix)

	body, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	key := LaneSnapshotObjectKey(w.prefix, snap.Day)
	pctx, cancel := context.WithTimeout(ctx, laneSnapshotPutBudget)
	defer cancel()
	return store.Put(pctx, key, body)
}

// resolveStore returns the injected store, or lazily builds the S3 one.
func (w *LaneSnapshotWorker) resolveStore(ctx context.Context) (LaneSnapshotStore, error) {
	if w.store != nil {
		return w.store, nil
	}
	if w.bucket == "" {
		return nil, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(w.region))
	if err != nil {
		return nil, fmt.Errorf("aws config (region=%s): %w", w.region, err)
	}
	w.store = &s3LaneSnapshotStore{client: s3.NewFromConfig(cfg), bucket: w.bucket}
	return w.store, nil
}

// ── the ONE Postgres query ──────────────────────────────────────────────────

// laneSnapshotCampaignSQL resolves campaign_id → (org, vertical, brand).
//
// created_at LEADS and the anchored name regex is a RESIDUAL filter — leading
// with the regex cannot use an index (property_lane_stats.go carries the same
// note from lane_performance_ledger.series(), which timed out at 852s that
// way). VERIFIED on prod 2026-08-19:
//
//	Index Scan using idx_campaigns_org_status_created … Index Cond:
//	(created_at >= (now() - '10 days'::interval))
//	Filter: (name ~ '^\[partner-drip\] ') … Execution Time: 254.612 ms
//
// organization_id is SELECTED (not filtered) because the worker is not
// request-scoped; the endpoint filters rows by the caller's org.
//
// $1 created_at floor.
const laneSnapshotCampaignSQL = `
	SELECT id::text, organization_id::text, name
	FROM mailing_campaigns
	WHERE created_at >= $1
	  AND name ~ '^\[partner-drip\] '`

// loadCampaignLanes runs the single PG query and parses names into lanes.
func (w *LaneSnapshotWorker) loadCampaignLanes(ctx context.Context) (map[string]LaneSnapshotCampaign, error) {
	floor := time.Now().AddDate(0, 0, -laneSnapshotCampaignDays)
	rows, err := w.db.QueryContext(ctx, laneSnapshotCampaignSQL, floor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]LaneSnapshotCampaign, 32768)
	for rows.Next() {
		var id, org, name string
		if err := rows.Scan(&id, &org, &name); err != nil {
			return nil, err
		}
		vertical, brand, ok := ParseLaneCampaignName(name)
		if !ok {
			continue
		}
		out[id] = LaneSnapshotCampaign{OrganizationID: org, Vertical: vertical, Brand: brand}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseLaneCampaignName splits `[partner-drip] <vertical> <brand> <stamp> …`.
// Verified against live names 2026-08-19, e.g.
// "[partner-drip] term_life rru 20260820T050519 8deb78a3 …".
// A name that does not carry both tokens returns ok=false and is DROPPED —
// never guessed into a lane.
func ParseLaneCampaignName(name string) (vertical, brand string, ok bool) {
	rest := strings.TrimSpace(name)
	if !strings.HasPrefix(rest, laneSnapshotCampaignPrefix) {
		return "", "", false
	}
	fields := strings.Fields(strings.TrimPrefix(rest, laneSnapshotCampaignPrefix))
	if len(fields) < 2 {
		return "", "", false
	}
	vertical = strings.ToLower(fields[0])
	brand = strings.ToLower(fields[1])
	if vertical == "" || brand == "" {
		return "", "", false
	}
	return vertical, brand, true
}

// ── aggregation (pure — this is what the tests drive) ───────────────────────

type laneSnapshotKey struct {
	Org      string
	Vertical string
	Brand    string
	ISP      string
	Source   string
}

type laneSnapshotAcc struct {
	attempted, delivered, bounced int64
	openUniq, clickUniq           int64
	openEvents, clickEvents       int64
	sawOpen, sawClick             bool
	campaigns                     map[string]struct{}
}

// BuildLaneSnapshot folds lake buckets into lane cells.
//
// A campaign_id absent from `mapping` is DROPPED and counted in Unmapped —
// there is no "unknown" lane, no fallback vertical, and no attribution guess.
// That is honesty rule #4: never silently attributed to the wrong lane.
func BuildLaneSnapshot(day string, capturedAt time.Time, lakeRows []analytics.LaneSnapshotLakeRow, mapping map[string]LaneSnapshotCampaign) *LaneSnapshot {
	acc := make(map[laneSnapshotKey]*laneSnapshotAcc, 1024)
	mapped := make(map[string]struct{}, len(lakeRows))
	unmappedCampaigns := make(map[string]struct{})
	var unmappedEvents int64

	for _, lr := range lakeRows {
		lane, ok := mapping[lr.CampaignID]
		if !ok {
			if lr.CampaignID != "" {
				unmappedCampaigns[lr.CampaignID] = struct{}{}
			}
			unmappedEvents += lr.Events
			continue
		}
		mapped[lr.CampaignID] = struct{}{}

		isp := strings.ToLower(strings.TrimSpace(lr.ISPGroup))
		if isp == "" {
			isp = "other"
		}
		src := strings.ToLower(strings.TrimSpace(lr.Source))
		if src == "" {
			src = "unknown"
		}
		k := laneSnapshotKey{
			Org: lane.OrganizationID, Vertical: lane.Vertical, Brand: lane.Brand,
			ISP: isp, Source: src,
		}
		a := acc[k]
		if a == nil {
			a = &laneSnapshotAcc{campaigns: make(map[string]struct{})}
			acc[k] = a
		}
		a.campaigns[lr.CampaignID] = struct{}{}

		switch lr.EventType {
		case "attempted":
			a.attempted += lr.Events
		case "delivered":
			a.delivered += lr.Events
		case "bounced":
			a.bounced += lr.Events
		case "open":
			a.sawOpen = true
			a.openEvents += lr.Events
			a.openUniq += lr.Uniques
		case "click":
			a.sawClick = true
			a.clickEvents += lr.Events
			a.clickUniq += lr.Uniques
		}
	}

	rows := make([]LaneSnapshotRow, 0, len(acc))
	for k, a := range acc {
		row := LaneSnapshotRow{
			OrganizationID: k.Org,
			Vertical:       k.Vertical,
			Brand:          k.Brand,
			ISP:            k.ISP,
			Source:         k.Source,
			Attempted:      a.attempted,
			Delivered:      a.delivered,
			Bounced:        a.bounced,
			Campaigns:      len(a.campaigns),
		}
		// ENGAGEMENT ABSENT vs ZERO. A transport the lake carries no open/click
		// rows for reports null — unless this very snapshot saw engagement from
		// it, in which case the observation wins over the doctrine list.
		if laneSnapshotNoEngagementSources[k.Source] && !a.sawOpen && !a.sawClick {
			row.EngagementAvailable = false
		} else {
			row.EngagementAvailable = true
			ou, cu, oe, ce := a.openUniq, a.clickUniq, a.openEvents, a.clickEvents
			row.OpenUniq, row.ClickUniq, row.OpenEvents, row.ClickEvents = &ou, &cu, &oe, &ce
		}
		rows = append(rows, row)
	}

	// Deterministic order so two runs of the same tick produce byte-identical
	// files (and so a diff of two snapshots is readable).
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Vertical != b.Vertical {
			return a.Vertical < b.Vertical
		}
		if a.Brand != b.Brand {
			return a.Brand < b.Brand
		}
		if a.ISP != b.ISP {
			return a.ISP < b.ISP
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.OrganizationID < b.OrganizationID
	})

	return &LaneSnapshot{
		Day:             day,
		CapturedAt:      capturedAt.UTC().Format(time.RFC3339),
		Source:          laneSnapshotSource,
		Rows:            rows,
		Unmapped:        LaneSnapshotUnmapped{Campaigns: len(unmappedCampaigns), Events: unmappedEvents},
		LakeRows:        len(lakeRows),
		MappedCampaigns: len(mapped),
		Notes:           []string{laneSnapshotUniqNote, laneSnapshotSrcNote, laneSnapshotMapNote},
	}
}
