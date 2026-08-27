package worker

// ClickFunnelSnapshotWorker — the materializer behind the Click Funnels screen.
//
// WHY IT EXISTS: the screen used to do all of its work on the request path —
// measured on prod 2026-08-25 at 4.0s for the lane list (six correlated
// subqueries plus a mailing_message_log join) and ~13s more per lane click
// (4.9s of execution-log flow, plus two live Athena passes at 3.0s each). No
// caching anywhere. This worker moves every one of those reads off the request
// path, exactly as LaneSnapshotWorker did for the property ledger after a
// Postgres rollup starved the request path (commit fc7fec3).
//
// OUTPUT: S3, never Postgres. A catalog object plus one object per lane, so
// opening ONE funnel never requires loading every funnel.
//
//	<prefix>manifest.json
//	<prefix>catalog/current.json
//	<prefix>lanes/<offer>/current.json
//
// CADENCE IS SPLIT, because the data has different freshness needs:
//
//	engagement — floor of ~6h no matter what we do. Measured ingest lag for
//	             source='app' (the ONLY source of open/click) on 2026-08-25:
//	             27.7% <1h, 60.8% 6-24h, 8.6% 1-2d. A "live" badge would lie.
//	operational — a retry loop or a revoked proof must surface in minutes.
//
// So every tick refreshes configuration, flow, state and alerts from Postgres,
// while the lake pass runs on a 3-DAY ROLLING INCREMENTAL (app-source
// completeness) and a 7-DAY FULL RECONCILIATION once per hour (the ses tail
// reaches 2-7d). Day-grain rows are merged, never summed, so a re-run replaces
// a day rather than doubling it.
//
// RE-RUN SAFE BY CONSTRUCTION: the pass has no accumulating state. It
// recomputes from scratch, merges lake days by (node, dt) with replace
// semantics, and overwrites a fixed set of S3 keys. Running it twice produces
// the same objects.
//
// Kill switch: CLICK_FUNNEL_SNAPSHOT_DISABLED.
// Env: CLICK_FUNNEL_SNAPSHOT_BUCKET (default JARVIS_S3_BUCKET),
//      CLICK_FUNNEL_SNAPSHOT_PREFIX (default "click-funnel-snapshots/"),
//      CLICK_FUNNEL_SNAPSHOT_REGION (default JARVIS_S3_REGION, then us-west-2).

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
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
	clickFunnelSnapshotWorkerName = "click_funnel_snapshot"
	clickFunnelSnapshotLockKey    = "click_funnel_snapshot"

	// DefaultClickFunnelSnapshotInterval — operational freshness. Config, flow,
	// state and alerts refresh at this cadence; the lake pass is throttled
	// separately below because its data cannot be fresher than ~6h anyway.
	DefaultClickFunnelSnapshotInterval = 5 * time.Minute

	// clickFunnelLakeInterval throttles the Athena pass. Engagement has a ~6h
	// floor, so scanning every 5 minutes buys nothing and costs 12x.
	clickFunnelLakeInterval = 15 * time.Minute

	// clickFunnelReconcileInterval is the FULL-window pass that catches late
	// arrivals the incremental window missed (ses tail runs 2-7d).
	clickFunnelReconcileInterval = 1 * time.Hour

	// clickFunnelIncrementalDays — 3 days, NOT 2. Measured 2026-08-25: 8.6% of
	// app-source engagement lands in the 1-2d bucket, so a 2-day window sits
	// exactly on the edge with zero margin.
	clickFunnelIncrementalDays = 3

	// clickFunnelReconcileDays covers the ses late tail.
	clickFunnelReconcileDays = 7

	// clickFunnelRetentionDays bounds the day series carried in a lane object.
	clickFunnelRetentionDays = 90

	clickFunnelDefaultPrefix = "click-funnel-snapshots/"

	// clickFunnelStuckRetryRatio — attempts per affected enrollment above which
	// a node is a stuck-retry condition rather than ordinary transient failure
	// (METRIC_CONTRACT §10.9). Offer 420 ran at ~6,727.
	clickFunnelStuckRetryRatio = 20

	// clickFunnelAdminExitAlertPct — a lane whose administrative exits exceed
	// this share of enrollments must disclose it (§10.3).
	clickFunnelAdminExitAlertPct = 10.0

	clickFunnelLakeLagNote = "open/click ride source='app', whose ingest lag measured 27.7% <1h, 60.8% 6-24h, 8.6% 1-2d on 2026-08-25. Engagement here can never be fresher than roughly 6 hours; read metrics_through, not wall-clock."

	clickFunnelPGBudget   = 120 * time.Second
	clickFunnelLakeBudget = 5 * time.Minute
	clickFunnelPutBudget  = 60 * time.Second
)

func clickFunnelSnapshotDisabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("CLICK_FUNNEL_SNAPSHOT_DISABLED")))
	return v == "1" || v == "true" || v == "yes"
}

// ── storage seam ────────────────────────────────────────────────────────────

// ClickFunnelStore is the object-storage seam. Same two-method shape as
// LaneSnapshotStore so tests can share a fake.
type ClickFunnelStore interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

type s3ClickFunnelStore struct {
	client *s3.Client
	bucket string
}

func (s *s3ClickFunnelStore) Put(ctx context.Context, key string, body []byte) error {
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

func (s *s3ClickFunnelStore) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("s3 get s3://%s/%s: %w", s.bucket, key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// ── keys ────────────────────────────────────────────────────────────────────

func cfNormPrefix(p string) string {
	if p == "" {
		p = clickFunnelDefaultPrefix
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// ClickFunnelCatalogKey / ClickFunnelLaneKey are the published object keys.
func ClickFunnelCatalogKey(prefix string) string {
	return cfNormPrefix(prefix) + "catalog/current.json"
}
func ClickFunnelLaneKey(prefix, offerID string) string {
	return cfNormPrefix(prefix) + "lanes/" + cfSafeOffer(offerID) + "/current.json"
}

// cfSafeOffer keeps an offer id usable as a path segment. Offer ids are
// numeric-ish everywhere in production, but a key is built from data and must
// never be able to escape its prefix.
func cfSafeOffer(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "unknown"
	}
	return out
}

// ── in-process latest slot ──────────────────────────────────────────────────
//
// The endpoint serves from here first. NOT a cache tier: no TTL, no
// arbitration. It is "the last thing this process computed", and S3 covers a
// task that has booted but not yet ticked.

var (
	cfMu      sync.RWMutex
	cfCatalog *ClickFunnelCatalog
	cfLanes   = map[string]*ClickFunnelLane{}
	cfStore   ClickFunnelStore
	cfPrefix  = clickFunnelDefaultPrefix
)

func publishClickFunnelSnapshot(cat *ClickFunnelCatalog, lanes map[string]*ClickFunnelLane) {
	cfMu.Lock()
	cfCatalog = cat
	cfLanes = lanes
	cfMu.Unlock()
}

func registerClickFunnelStore(store ClickFunnelStore, prefix string) {
	cfMu.Lock()
	cfStore = store
	if prefix != "" {
		cfPrefix = prefix
	}
	cfMu.Unlock()
}

// SetClickFunnelSnapshotForTest installs a snapshot into the in-process slot
// and returns the undo. Exported for the API package's handler tests.
func SetClickFunnelSnapshotForTest(cat *ClickFunnelCatalog, lanes map[string]*ClickFunnelLane, store ClickFunnelStore) func() {
	cfMu.Lock()
	pc, pl, ps := cfCatalog, cfLanes, cfStore
	cfCatalog, cfStore = cat, store
	if lanes != nil {
		cfLanes = lanes
	} else {
		cfLanes = map[string]*ClickFunnelLane{}
	}
	cfMu.Unlock()
	return func() {
		cfMu.Lock()
		cfCatalog, cfLanes, cfStore = pc, pl, ps
		cfMu.Unlock()
	}
}

// LoadClickFunnelCatalog returns the newest catalog this process can reach and
// where it came from ("memory" or "s3"). (nil, "") means no snapshot exists
// anywhere — the caller MUST render that as an explicit "no snapshot yet"
// state, never as an empty lane list.
func LoadClickFunnelCatalog(ctx context.Context) (*ClickFunnelCatalog, string) {
	cfMu.RLock()
	cat, store, prefix := cfCatalog, cfStore, cfPrefix
	cfMu.RUnlock()
	if cat != nil {
		return cat, "memory"
	}
	if store == nil {
		return nil, ""
	}
	body, err := store.Get(ctx, ClickFunnelCatalogKey(prefix))
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] S3 catalog read failed: %v", err)
		return nil, ""
	}
	var out ClickFunnelCatalog
	if err := json.Unmarshal(body, &out); err != nil {
		log.Printf("[ClickFunnelSnapshot] S3 catalog is not a snapshot: %v", err)
		return nil, ""
	}
	if out.SchemaVersion > ClickFunnelSchemaVersion {
		// A newer writer's payload may mean something different. Refuse rather
		// than misread it.
		log.Printf("[ClickFunnelSnapshot] catalog schema v%d > supported v%d — refusing", out.SchemaVersion, ClickFunnelSchemaVersion)
		return nil, ""
	}
	return &out, "s3"
}

// LoadClickFunnelLane returns one lane's snapshot.
func LoadClickFunnelLane(ctx context.Context, offerID string) (*ClickFunnelLane, string) {
	cfMu.RLock()
	lane, store, prefix := cfLanes[offerID], cfStore, cfPrefix
	cfMu.RUnlock()
	if lane != nil {
		return lane, "memory"
	}
	if store == nil {
		return nil, ""
	}
	body, err := store.Get(ctx, ClickFunnelLaneKey(prefix, offerID))
	if err != nil {
		return nil, ""
	}
	var out ClickFunnelLane
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, ""
	}
	if out.SchemaVersion > ClickFunnelSchemaVersion {
		return nil, ""
	}
	return &out, "s3"
}

// ── the worker ──────────────────────────────────────────────────────────────

// ClickFunnelLakeReader is the Athena seam.
type ClickFunnelLakeReader interface {
	ClickFunnelDaily(ctx context.Context, fromDt, toDt string, campaignIDs []string) ([]analytics.ClickFunnelLakeRow, error)
}

type clickFunnelLakeReader struct{}

func (clickFunnelLakeReader) ClickFunnelDaily(ctx context.Context, fromDt, toDt string, ids []string) ([]analytics.ClickFunnelLakeRow, error) {
	return analytics.ClickFunnelDaily(ctx, fromDt, toDt, ids)
}

// ClickFunnelSnapshotWorker owns the materialization pass.
type ClickFunnelSnapshotWorker struct {
	db       *sql.DB
	redis    *redis.Client
	interval time.Duration

	bucket, prefix, region string

	lake  ClickFunnelLakeReader
	store ClickFunnelStore

	lakeAvailable func() bool
	now           func() time.Time

	// days accumulates lake rows across ticks so an incremental pass does not
	// discard history it did not re-read. Keyed node -> dt -> day.
	daysMu sync.Mutex
	days   map[cfFlowKey]map[string]ClickFunnelNodeDay

	lastLakeAt      time.Time
	lastReconcileAt time.Time
}

// NewClickFunnelSnapshotWorker wires the worker. redisClient may be nil — the
// distlock falls back to a PG advisory lock on a dedicated connection.
func NewClickFunnelSnapshotWorker(db *sql.DB, redisClient *redis.Client) *ClickFunnelSnapshotWorker {
	bucket := strings.TrimSpace(os.Getenv("CLICK_FUNNEL_SNAPSHOT_BUCKET"))
	if bucket == "" {
		bucket = strings.TrimSpace(os.Getenv("JARVIS_S3_BUCKET"))
	}
	prefix := strings.TrimSpace(os.Getenv("CLICK_FUNNEL_SNAPSHOT_PREFIX"))
	if prefix == "" {
		prefix = clickFunnelDefaultPrefix
	}
	region := strings.TrimSpace(os.Getenv("CLICK_FUNNEL_SNAPSHOT_REGION"))
	if region == "" {
		region = strings.TrimSpace(os.Getenv("JARVIS_S3_REGION"))
	}
	if region == "" {
		region = "us-west-2"
	}
	return &ClickFunnelSnapshotWorker{
		db:            db,
		redis:         redisClient,
		interval:      DefaultClickFunnelSnapshotInterval,
		bucket:        bucket,
		prefix:        cfNormPrefix(prefix),
		region:        region,
		lake:          clickFunnelLakeReader{},
		lakeAvailable: analytics.ReaderEnabled,
		now:           time.Now,
		days:          map[cfFlowKey]map[string]ClickFunnelNodeDay{},
	}
}

// WithInterval overrides the tick cadence (tests). Call before Start.
func (w *ClickFunnelSnapshotWorker) WithInterval(d time.Duration) *ClickFunnelSnapshotWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

// SetLakeReader injects the Athena seam (tests). Call before Start.
func (w *ClickFunnelSnapshotWorker) SetLakeReader(r ClickFunnelLakeReader) *ClickFunnelSnapshotWorker {
	w.lake = r
	w.lakeAvailable = func() bool { return true }
	return w
}

// SetStore injects the object-storage seam (tests). Call before Start.
func (w *ClickFunnelSnapshotWorker) SetStore(s ClickFunnelStore) *ClickFunnelSnapshotWorker {
	w.store = s
	return w
}

// SetNow injects the clock (tests). Call before Start.
func (w *ClickFunnelSnapshotWorker) SetNow(f func() time.Time) *ClickFunnelSnapshotWorker {
	if f != nil {
		w.now = f
	}
	return w
}

// Start runs the tick loop until ctx is cancelled. Non-blocking; no-op if db is
// nil.
func (w *ClickFunnelSnapshotWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[ClickFunnelSnapshot] disabled (db missing)")
		return
	}
	go func() {
		log.Printf("Click funnel snapshot worker started (interval=%s, lake=%s, reconcile=%s, bucket=%s, prefix=%s)",
			w.interval, clickFunnelLakeInterval, clickFunnelReconcileInterval, w.bucket, w.prefix)
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-ctx.Done():
				log.Printf("[ClickFunnelSnapshot] context cancelled, stopping")
				return
			}
		}
	}()
}

func (w *ClickFunnelSnapshotWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if clickFunnelSnapshotDisabled() {
		EmitHeartbeat(ctx, w.db, clickFunnelSnapshotWorkerName, int(w.interval.Seconds()), "disabled", "CLICK_FUNNEL_SNAPSHOT_DISABLED set")
		return
	}
	lock := distlock.NewLock(w.redis, w.db, clickFunnelSnapshotLockKey, w.interval)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] lock acquire error: %v", err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[ClickFunnelSnapshot] lock release error: %v", err)
		}
	}()
	w.RunOnce(ctx)
}

// RunOnce executes one full pass. Exported so tests and operational tooling can
// drive a pass directly; the caller owns locking.
func (w *ClickFunnelSnapshotWorker) RunOnce(ctx context.Context) {
	started := w.now()

	pgCtx, cancel := context.WithTimeout(ctx, clickFunnelPGBudget)
	defer cancel()

	// Repair first: a node with no shadow campaign cannot be measured at all,
	// so fixing attribution before gathering means this pass already reflects
	// it. Idempotent — once repaired the statement matches zero rows.
	w.repairOrphanNodeStamps(pgCtx)

	lanes, err := w.gatherLanes(pgCtx)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] %v", err)
		EmitHeartbeat(ctx, w.db, clickFunnelSnapshotWorkerName, int(w.interval.Seconds()), "error", err.Error())
		return
	}
	graphs, err := w.gatherGraphs(pgCtx)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] %v", err)
		EmitHeartbeat(ctx, w.db, clickFunnelSnapshotWorkerName, int(w.interval.Seconds()), "error", err.Error())
		return
	}

	// Per-lane maturity and conversion lookback both derive from the lane's own
	// ladder, so they are computed once from the graph and passed down.
	maturity := map[string]float64{}
	lookback := map[string]float64{}
	for _, l := range lanes {
		lh := ladderHours(graphs[l.JourneyID])
		if lh <= 0 {
			lh = clickFunnelDefaultLookbackHours
		}
		maturity[l.OfferID] = lh + clickFunnelMaturityGraceHours
		lookback[l.OfferID] = lh
	}

	flow, err := w.gatherFlow(pgCtx)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] %v", err)
		EmitHeartbeat(ctx, w.db, clickFunnelSnapshotWorkerName, int(w.interval.Seconds()), "error", err.Error())
		return
	}
	awaiting, err := w.gatherAwaiting(pgCtx)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] %v", err)
		return
	}
	cohorts, err := w.gatherCohort(pgCtx, maturity)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] %v", err)
		return
	}
	convs, medFirstSend, err := w.gatherConversions(pgCtx)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] %v", err)
		return
	}
	nodeConvs, err := w.gatherNodeConversions(pgCtx, lookback)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] %v", err)
		return
	}
	copyBy, err := w.gatherCopy(pgCtx)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] %v", err)
		return
	}
	nodeCampaigns, err := w.gatherNodeCampaigns(pgCtx)
	if err != nil {
		log.Printf("[ClickFunnelSnapshot] %v", err)
		return
	}
	orphans := w.gatherOrphanInlets(pgCtx)

	journeyThrough := w.now()

	// ── the lake pass, throttled and merged ─────────────────────────────────
	marks := ClickFunnelWatermarks{
		JourneyThrough: journeyThrough,
		LakeLagNote:    clickFunnelLakeLagNote,
	}
	w.refreshLake(ctx, nodeCampaigns, &marks)

	// ── assemble ────────────────────────────────────────────────────────────
	snapID := cfSnapshotID(started)
	cat := &ClickFunnelCatalog{
		SnapshotID:    snapID,
		SchemaVersion: ClickFunnelSchemaVersion,
		GeneratedAt:   started,
		Watermarks:    marks,
		DataQuality:   cfDataQuality(marks),
		Lanes:         make([]ClickFunnelCatalogRow, 0, len(lanes)),
		OrphanInlets:  orphans,
	}
	laneObjs := make(map[string]*ClickFunnelLane, len(lanes))

	for _, l := range lanes {
		lane := w.assembleLane(l, graphs[l.JourneyID], maturity, lookback,
			flow, awaiting, cohorts[l.OfferID], convs[l.OfferID], medFirstSend[l.OfferID],
			nodeConvs, copyBy, nodeCampaigns, marks, snapID, started)
		cat.Lanes = append(cat.Lanes, lane.ClickFunnelCatalogRow)
		laneObjs[l.OfferID] = lane
	}

	// Navigator order: enabled first, then by live activity, then by id — the
	// lane an operator is most likely to want is first without a click.
	sort.SliceStable(cat.Lanes, func(i, j int) bool {
		a, b := cat.Lanes[i], cat.Lanes[j]
		if a.Enabled != b.Enabled {
			return a.Enabled
		}
		if a.ActiveNow != b.ActiveNow {
			return a.ActiveNow > b.ActiveNow
		}
		return a.OfferID < b.OfferID
	})

	publishClickFunnelSnapshot(cat, laneObjs)
	w.publishToStore(ctx, cat, laneObjs)

	EmitHeartbeat(ctx, w.db, clickFunnelSnapshotWorkerName, int(w.interval.Seconds()), "ok",
		fmt.Sprintf("lanes=%d lake_rows=%d quality=%s took=%s",
			len(cat.Lanes), marks.LakeRowCount, cat.DataQuality, w.now().Sub(started).Truncate(time.Millisecond)))
}

// refreshLake runs the Athena pass when due and merges its day rows into the
// accumulator. Merge is REPLACE-by-(node,dt): re-reading a day overwrites it
// rather than adding to it, which is what makes the pass re-run safe.
func (w *ClickFunnelSnapshotWorker) refreshLake(ctx context.Context, nodeCampaigns map[cfFlowKey]string, marks *ClickFunnelWatermarks) {
	if w.lakeAvailable != nil && !w.lakeAvailable() {
		marks.LakeError = "lake reader not configured"
		w.fillMarksFromAccumulator(marks)
		return
	}
	if len(nodeCampaigns) == 0 {
		marks.LakeError = "no node-attributed campaigns"
		return
	}

	now := w.now()
	reconcile := now.Sub(w.lastReconcileAt) >= clickFunnelReconcileInterval
	if !reconcile && now.Sub(w.lastLakeAt) < clickFunnelLakeInterval {
		// Not due. Serve the accumulated days unchanged — stale, and SAID to be
		// stale via the watermarks, never silently zeroed.
		w.fillMarksFromAccumulator(marks)
		return
	}

	days := clickFunnelIncrementalDays
	if reconcile {
		days = clickFunnelReconcileDays
	}
	toDt := now.UTC().Format("2006-01-02")
	fromDt := now.UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")

	// campaign -> node, inverted for attribution after the scan.
	byCampaign := make(map[string]cfFlowKey, len(nodeCampaigns))
	ids := make([]string, 0, len(nodeCampaigns))
	for k, id := range nodeCampaigns {
		byCampaign[id] = k
		ids = append(ids, id)
	}
	sort.Strings(ids)

	lakeCtx, cancel := context.WithTimeout(ctx, clickFunnelLakeBudget)
	defer cancel()
	rows, err := w.lake.ClickFunnelDaily(lakeCtx, fromDt, toDt, ids)
	if err != nil {
		// A lake failure means engagement is STALE, not zero. The accumulator
		// keeps the last good days and the watermark says what happened.
		log.Printf("[ClickFunnelSnapshot] lake pass failed (%s..%s): %v", fromDt, toDt, err)
		marks.LakeError = err.Error()
		w.fillMarksFromAccumulator(marks)
		return
	}

	fresh := map[cfFlowKey]map[string]ClickFunnelNodeDay{}
	for _, r := range rows {
		node, ok := byCampaign[r.CampaignID]
		if !ok {
			continue
		}
		if fresh[node] == nil {
			fresh[node] = map[string]ClickFunnelNodeDay{}
		}
		d := fresh[node][r.Dt]
		d.Dt = r.Dt
		applyLakeRow(&d, r)
		fresh[node][r.Dt] = d
	}

	w.daysMu.Lock()
	for node, byDt := range fresh {
		if w.days[node] == nil {
			w.days[node] = map[string]ClickFunnelNodeDay{}
		}
		for dt, d := range byDt {
			w.days[node][dt] = d // REPLACE, never accumulate
		}
	}
	// Bound retention so a long-lived process cannot grow without limit.
	cutoff := now.UTC().AddDate(0, 0, -clickFunnelRetentionDays).Format("2006-01-02")
	for node, byDt := range w.days {
		for dt := range byDt {
			if dt < cutoff {
				delete(byDt, dt)
			}
		}
		if len(byDt) == 0 {
			delete(w.days, node)
		}
	}
	w.daysMu.Unlock()

	w.lastLakeAt = now
	if reconcile {
		w.lastReconcileAt = now
		marks.Reconciled = true
		marks.ReconciledAt = now
	}
	marks.MetricsFrom = fromDt
	marks.MetricsThrough = toDt
	marks.LakeRowCount = len(rows)
}

// fillMarksFromAccumulator reports the window the accumulator actually covers
// when this tick did not (or could not) re-read the lake.
func (w *ClickFunnelSnapshotWorker) fillMarksFromAccumulator(marks *ClickFunnelWatermarks) {
	w.daysMu.Lock()
	defer w.daysMu.Unlock()
	lo, hi, n := "", "", 0
	for _, byDt := range w.days {
		for dt := range byDt {
			n++
			if lo == "" || dt < lo {
				lo = dt
			}
			if hi == "" || dt > hi {
				hi = dt
			}
		}
	}
	marks.MetricsFrom, marks.MetricsThrough, marks.LakeRowCount = lo, hi, n
}

// applyLakeRow folds one lake bucket into a day, applying METRIC_CONTRACT §1's
// app-stream rule: delivery comes from the real transports only (the `app`
// mirror duplicates it), engagement comes from `app` only (the transports carry
// no open/click at all).
func applyLakeRow(d *ClickFunnelNodeDay, r analytics.ClickFunnelLakeRow) {
	transport := r.Source == "pmta" || r.Source == "ses" || r.Source == "kumo"
	switch r.EventType {
	case "delivered":
		if transport {
			d.Delivered += int(r.Events)
		}
	case "relayed_to_ses":
		if transport {
			d.Relayed += int(r.Events)
		}
	case "hard_bounce":
		if transport {
			d.HardBounce += int(r.Events)
		}
	case "soft_bounce":
		if transport {
			d.SoftBounce += int(r.Events)
		}
	case "delivery_delay":
		// Deferrals are counted by DISTINCT MAILBOX: delay notices are
		// per-retry and event-counting them overstates held mail ~2.6x.
		if transport {
			d.Deferred += int(r.Mailboxes)
		}
	case "open":
		if r.Source == "app" {
			d.Opens += int(r.Recipients)
		}
	case "click":
		if r.Source == "app" {
			d.ClicksRaw += int(r.Recipients)
			d.ClicksClassified += int(r.Classified)
			d.ClicksMachine += int(r.Machine)
		}
	case "unsubscribe":
		if r.Source == "app" {
			d.Unsubs += int(r.Recipients)
		}
	case "complaint":
		if r.Source == "app" {
			d.Complaints += int(r.Recipients)
		}
	}
}

func cfSnapshotID(t time.Time) string {
	sum := sha1.Sum([]byte(t.UTC().Format(time.RFC3339Nano)))
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(sum[:4])
}

// cfDataQuality grades the payload so a screen can badge it without re-deriving
// the rule.
func cfDataQuality(m ClickFunnelWatermarks) string {
	if m.LakeError != "" {
		return "degraded"
	}
	if m.MetricsThrough == "" {
		return "stale"
	}
	return "ok"
}

// publishToStore writes the manifest, the catalog and every lane object.
// Manifest LAST: it names the snapshot id a reader should expect, so writing it
// after the objects it describes means a reader never sees a manifest pointing
// at content that is not there yet.
func (w *ClickFunnelSnapshotWorker) publishToStore(ctx context.Context, cat *ClickFunnelCatalog, lanes map[string]*ClickFunnelLane) {
	store, err := w.ensureStore(ctx)
	if err != nil || store == nil {
		if err != nil {
			log.Printf("[ClickFunnelSnapshot] store unavailable: %v (serving from memory only)", err)
		}
		return
	}
	putCtx, cancel := context.WithTimeout(ctx, clickFunnelPutBudget)
	defer cancel()

	for offer, lane := range lanes {
		body, err := json.Marshal(lane)
		if err != nil {
			continue
		}
		if err := store.Put(putCtx, ClickFunnelLaneKey(w.prefix, offer), body); err != nil {
			log.Printf("[ClickFunnelSnapshot] put lane %s: %v", offer, err)
		}
	}
	if body, err := json.Marshal(cat); err == nil {
		if err := store.Put(putCtx, ClickFunnelCatalogKey(w.prefix), body); err != nil {
			log.Printf("[ClickFunnelSnapshot] put catalog: %v", err)
		}
	}
	manifest := map[string]interface{}{
		"snapshot_id":    cat.SnapshotID,
		"schema_version": ClickFunnelSchemaVersion,
		"generated_at":   cat.GeneratedAt,
		"lane_count":     len(lanes),
		"data_quality":   cat.DataQuality,
		"watermarks":     cat.Watermarks,
	}
	if body, err := json.Marshal(manifest); err == nil {
		if err := store.Put(putCtx, cfNormPrefix(w.prefix)+"manifest.json", body); err != nil {
			log.Printf("[ClickFunnelSnapshot] put manifest: %v", err)
		}
	}
}

func (w *ClickFunnelSnapshotWorker) ensureStore(ctx context.Context) (ClickFunnelStore, error) {
	if w.store != nil {
		registerClickFunnelStore(w.store, w.prefix)
		return w.store, nil
	}
	if w.bucket == "" {
		return nil, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(w.region))
	if err != nil {
		return nil, fmt.Errorf("aws config (region=%s): %w", w.region, err)
	}
	w.store = &s3ClickFunnelStore{client: s3.NewFromConfig(cfg), bucket: w.bucket}
	registerClickFunnelStore(w.store, w.prefix)
	return w.store, nil
}
