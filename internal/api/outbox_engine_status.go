package api

// =============================================================================
// OUTBOX ENGINE STATUS — GET /api/mailing/outbox/engine-status
// =============================================================================
// One payload that answers the operator's standing questions about the sending
// engine: "are we sending, are we at capacity, are we backlogged, are we in a
// deferral storm, and is the wave manager alive?".
//
// Five independent sections (queue / waves / throughput / storm / workers),
// each failing soft: a broken query nulls out its own section (ok=false +
// error string) and never 500s the whole payload. The dashboard polls every
// ~10s, so — following the outbox_admin.go precedent where raw aggregates on
// the 1M+-row queue table took 15s in prod — the handler NEVER queries the DB
// on the request path. A background refresher recomputes the snapshot every
// fastRefreshInterval; the one genuinely heavy query (24h per-domain deferral
// baseline over partitioned mailing_tracking_events) runs on its own slower
// cadence and is merged into each fast refresh.
//
// The refresher is started lazily by the first call to
// HandleOutboxEngineStatus (i.e. at route-registration time), so no main.go
// wiring is required.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// VersionOutboxEngineStatus is bumped on every behavior change so operators
// can match the payload against deploy logs.
//
// 1.1: waves section gains completed_recipients_24h (SUM of planned_recipients
//      over completed-24h waves) so "waves done" can be read next to a message
//      figure; new /outbox/isp-pipes per-VMTA drill-down endpoint.
const VersionOutboxEngineStatus = "1.1"

const (
	engineFastRefreshInterval = 12 * time.Second
	engineSlowRefreshInterval = 5 * time.Minute
	engineQueryTimeout        = 15 * time.Second

	// Storm thresholds: an ISP is flagged when its last-60m deferral rate is
	// BOTH above stormMinRate AND at least stormMultiplierFloor× its 24h
	// baseline, with at least stormMinEvents events in the window so a handful
	// of deferrals on a tiny ISP can't page anyone.
	stormMinRate         = 0.10
	stormMultiplierFloor = 2.0
	stormMinEvents       = 50
	// baselineRateFloor protects the multiplier from divide-by-zero when an
	// ISP had a spotless 24h baseline (0.5% assumed noise floor).
	baselineRateFloor = 0.005
)

// ---------------------------------------------------------------------------
// Payload types
// ---------------------------------------------------------------------------

// EngineISPQueueDepth is one row of the per-ISP queued breakdown.
type EngineISPQueueDepth struct {
	ISP    string `json:"isp"`
	Queued int64  `json:"queued"`
}

// EngineQueueSection summarizes mailing_campaign_queue for the live board.
type EngineQueueSection struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	Queued     int64  `json:"queued"`
	Processing int64  `json:"processing"`
	// Sent24h counts per-recipient 'sent' tracking events over 24h. It
	// includes click-drip direct sends and any other non-wave send paths, so
	// it legitimately EXCEEDS the wave section's completed_recipients_24h —
	// do not treat the two as a contradiction.
	Sent24h             int64                 `json:"sent_24h"`
	Failed24h           int64                 `json:"failed_24h"`
	DeadLetter24h       int64                 `json:"dead_letter_24h"`
	OldestQueuedSeconds int64                 `json:"oldest_queued_seconds"`
	ByISP               []EngineISPQueueDepth `json:"by_isp"`
}

// EngineUpcomingWave is one of the next-5 planned waves.
type EngineUpcomingWave struct {
	WaveID            string    `json:"wave_id"`
	CampaignID        string    `json:"campaign_id"`
	CampaignName      string    `json:"campaign_name"`
	WaveNumber        int       `json:"wave_number"`
	ScheduledAt       time.Time `json:"scheduled_at"`
	PlannedRecipients int64     `json:"planned_recipients"`
}

// EngineWaveSection summarizes mailing_campaign_waves.
type EngineWaveSection struct {
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	PlannedDue    int64  `json:"planned_due"`
	PlannedFuture int64  `json:"planned_future"`
	Enqueuing     int64  `json:"enqueuing"`
	Sending       int64  `json:"sending"`
	// Completed24h counts wave EXECUTIONS, not messages.
	Completed24h int64 `json:"completed_24h"`
	// CompletedRecipients24h is the SUM of planned_recipients across those
	// completed waves — the message-scale companion to Completed24h. It is a
	// *planned* figure and covers wave-path sends only; the queue section's
	// Sent24h (per-recipient tracking events) also counts click-drip direct
	// sends + other non-wave paths, so Sent24h exceeding this is expected.
	CompletedRecipients24h int64                `json:"completed_recipients_24h"`
	Failed24h              int64                `json:"failed_24h"`
	Cancelled24h           int64                `json:"cancelled_24h"`
	Upcoming               []EngineUpcomingWave `json:"upcoming"`
}

// EngineMinutePoint is one per-minute throughput bucket.
type EngineMinutePoint struct {
	Minute   time.Time `json:"minute"`
	Sent     int64     `json:"sent"`
	Deferred int64     `json:"deferred"`
}

// EngineHourPoint is one per-hour throughput bucket.
type EngineHourPoint struct {
	Hour time.Time `json:"hour"`
	Sent int64     `json:"sent"`
}

// EngineThroughputSection carries send-rate series + the capacity estimate.
type EngineThroughputSection struct {
	OK                bool                `json:"ok"`
	Error             string              `json:"error,omitempty"`
	PerMinute         []EngineMinutePoint `json:"per_minute"`
	PerHour           []EngineHourPoint   `json:"per_hour"`
	CurrentRatePerMin float64             `json:"current_rate_per_min"`
	PeakRatePerMin24h float64             `json:"peak_rate_per_min_24h"`
	UtilizationPct    float64             `json:"utilization_pct"`
}

// EngineISPDeferral is the per-ISP deferral picture (recent vs baseline).
type EngineISPDeferral struct {
	ISP          string  `json:"isp"`
	Sent60m      int64   `json:"sent_60m"`
	Deferred60m  int64   `json:"deferred_60m"`
	RecentRate   float64 `json:"recent_rate"`
	BaselineRate float64 `json:"baseline_rate"`
	Multiplier   float64 `json:"multiplier"`
	Sent24h      int64   `json:"sent_24h"`
	Storm        bool    `json:"storm"`
}

// EngineStormSection flags ISPs deferring well above their own baseline.
type EngineStormSection struct {
	OK     bool                `json:"ok"`
	Error  string              `json:"error,omitempty"`
	Active bool                `json:"active"`
	ISPs   []EngineISPDeferral `json:"isps"`
}

// EngineWorkerBeat is one row from mailing_worker_heartbeats.
type EngineWorkerBeat struct {
	Name                    string    `json:"name"`
	LastBeatAt              time.Time `json:"last_beat_at"`
	SecondsSinceBeat        int64     `json:"seconds_since_beat"`
	ExpectedIntervalSeconds int       `json:"expected_interval_seconds"`
	Stalled                 bool      `json:"stalled"`
	LastStatus              string    `json:"last_status"`
	LastError               string    `json:"last_error,omitempty"`
}

// EngineWorkerSection lists background-worker heartbeats. If the heartbeat
// table is missing (fresh env) the section reports ok=false and the frontend
// omits the panel.
type EngineWorkerSection struct {
	OK      bool               `json:"ok"`
	Error   string             `json:"error,omitempty"`
	Workers []EngineWorkerBeat `json:"workers"`
}

// OutboxEngineStatusResponse is the full payload.
type OutboxEngineStatusResponse struct {
	GeneratedAt     time.Time               `json:"generated_at"`
	APIVersion      string                  `json:"api_version"`
	CacheAgeSeconds int64                   `json:"cache_age_seconds"`
	Queue           EngineQueueSection      `json:"queue"`
	Waves           EngineWaveSection       `json:"waves"`
	Throughput      EngineThroughputSection `json:"throughput"`
	Storm           EngineStormSection      `json:"storm"`
	Workers         EngineWorkerSection     `json:"workers"`
}

// ---------------------------------------------------------------------------
// Cache + refresher plumbing
// ---------------------------------------------------------------------------

type outboxEngineCache struct {
	mu  sync.RWMutex
	set bool
	d   OutboxEngineStatusResponse
}

func (c *outboxEngineCache) snapshot() (OutboxEngineStatusResponse, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.d, c.set
}

func (c *outboxEngineCache) store(resp OutboxEngineStatusResponse) {
	c.mu.Lock()
	c.d = resp
	c.set = true
	c.mu.Unlock()
}

// engineBaseline holds the slow-cadence per-ISP 24h deferral baseline plus
// the other 24h-window aggregates (sent_24h count, hourly series) so the
// fast 12s tick never rescans a full day of tracking events (QA finding H1).
type engineBaseline struct {
	mu          sync.Mutex
	refreshedAt time.Time
	// per normalized ISP: events in the 25h→60m-ago window.
	sentBase map[string]int64
	defBase  map[string]int64
	// slow-cadence 24h aggregates reused between fast ticks.
	sent24h    int64
	hourly     []EngineHourPoint
	aggRefresh time.Time
}

var (
	engineStatusOnce  sync.Once
	engineStatusCache = &outboxEngineCache{}
)

// HandleOutboxEngineStatus returns the cached engine snapshot. The first call
// (route registration) starts the background refresher; the request path is a
// pure cache read so a 10s dashboard poll can never pile up DB load.
func HandleOutboxEngineStatus(db *sql.DB) http.HandlerFunc {
	engineStatusOnce.Do(func() {
		go runOutboxEngineRefresher(context.Background(), db, engineStatusCache, engineFastRefreshInterval)
	})
	return func(w http.ResponseWriter, r *http.Request) {
		snap, populated := engineStatusCache.snapshot()
		if !populated {
			snap = OutboxEngineStatusResponse{
				GeneratedAt:     time.Now().UTC(),
				APIVersion:      VersionOutboxEngineStatus,
				CacheAgeSeconds: -1,
			}
		} else {
			snap.CacheAgeSeconds = int64(time.Since(snap.GeneratedAt).Seconds())
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			log.Printf("[engine_status] encode failed: %v", err)
		}
	}
}

func runOutboxEngineRefresher(ctx context.Context, db *sql.DB, cache *outboxEngineCache, interval time.Duration) {
	log.Printf("[EngineStatus] refresher started (fast=%s, baseline=%s)", interval, engineSlowRefreshInterval)
	baseline := &engineBaseline{}
	refreshOutboxEngineStatus(ctx, db, cache, baseline)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[EngineStatus] refresher stopping")
			return
		case <-t.C:
			refreshOutboxEngineStatus(ctx, db, cache, baseline)
		}
	}
}

// refreshOutboxEngineStatus recomputes every section and swaps the snapshot
// in. Sections are independent; an error in one is recorded on that section
// only.
func refreshOutboxEngineStatus(ctx context.Context, db *sql.DB, cache *outboxEngineCache, baseline *engineBaseline) {
	resp := OutboxEngineStatusResponse{
		GeneratedAt: time.Now().UTC(),
		APIVersion:  VersionOutboxEngineStatus,
	}
	refreshEngine24hAggregates(ctx, db, baseline)
	resp.Queue = engineQueueSection(ctx, db, baseline)
	resp.Waves = engineWaveSection(ctx, db)
	resp.Throughput = engineThroughputSection(ctx, db, baseline)
	resp.Storm = engineStormSection(ctx, db, baseline)
	resp.Workers = engineWorkerSection(ctx, db)
	cache.store(resp)
}

// ---------------------------------------------------------------------------
// Section: queue
// ---------------------------------------------------------------------------

func engineQueueSection(ctx context.Context, db *sql.DB, baseline *engineBaseline) EngineQueueSection {
	sec := EngineQueueSection{ByISP: []EngineISPQueueDepth{}}

	qctx, cancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer cancel()

	// Active depths only — bounded by idx_queue_status; never a full GROUP BY
	// over the whole partitioned table (that took 15s in prod, see
	// outbox_admin.go VersionOutboxSummary 1.1 note).
	err := db.QueryRowContext(qctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'queued'),
			COUNT(*) FILTER (WHERE status IN ('claimed','processing','sending','submitting'))
		FROM mailing_campaign_queue
		WHERE status IN ('queued','claimed','processing','sending','submitting')
	`).Scan(&sec.Queued, &sec.Processing)
	if err != nil {
		sec.Error = "depth query: " + err.Error()
		return sec
	}

	// Failed/dead-letter in the last 24h: status predicate first so the
	// status index (and the dead-letter partial index) bound the scan.
	fctx, fcancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer fcancel()
	if err := db.QueryRowContext(fctx, `
		SELECT COUNT(*) FROM mailing_campaign_queue
		WHERE status IN ('failed','failed_retryable','failed_permanent')
		  AND COALESCE(last_attempt_at, created_at) > NOW() - INTERVAL '24 hours'
	`).Scan(&sec.Failed24h); err != nil {
		log.Printf("[EngineStatus] failed_24h probe: %v", err)
	}
	dctx, dcancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer dcancel()
	if err := db.QueryRowContext(dctx, `
		SELECT COUNT(*) FROM mailing_campaign_queue
		WHERE status IN ('dead_letter','dead_letter_strict')
		  AND COALESCE(last_attempt_at, created_at) > NOW() - INTERVAL '24 hours'
	`).Scan(&sec.DeadLetter24h); err != nil {
		log.Printf("[EngineStatus] dead_letter_24h probe: %v", err)
	}

	// Oldest queued age — partial index idx_queue_scheduled (status='queued').
	octx, ocancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer ocancel()
	if err := db.QueryRowContext(octx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(scheduled_at)))::bigint, 0)
		FROM mailing_campaign_queue WHERE status = 'queued'
	`).Scan(&sec.OldestQueuedSeconds); err != nil {
		log.Printf("[EngineStatus] oldest queued probe: %v", err)
	}

	// Per-ISP queued breakdown (top 10).
	ictx, icancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer icancel()
	rows, err := db.QueryContext(ictx, `
		SELECT COALESCE(NULLIF(LOWER(recipient_isp), ''), 'other'), COUNT(*)
		FROM mailing_campaign_queue
		WHERE status = 'queued'
		GROUP BY 1
		ORDER BY 2 DESC
		LIMIT 10
	`)
	if err != nil {
		log.Printf("[EngineStatus] per-ISP queued probe: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var d EngineISPQueueDepth
			if scanErr := rows.Scan(&d.ISP, &d.Queued); scanErr == nil {
				sec.ByISP = append(sec.ByISP, d)
			}
		}
	}

	// sent_24h is a full-day event count — refreshed on the slow cadence by
	// refreshEngine24hAggregates and read from the baseline cache here, so
	// the fast tick never rescans 24h of tracking events (QA finding H1).
	baseline.mu.Lock()
	sec.Sent24h = baseline.sent24h
	baseline.mu.Unlock()

	sec.OK = true
	return sec
}

// refreshEngine24hAggregates recomputes the 24h sent count + hourly series at
// most every engineSlowRefreshInterval.
func refreshEngine24hAggregates(ctx context.Context, db *sql.DB, baseline *engineBaseline) {
	baseline.mu.Lock()
	fresh := time.Since(baseline.aggRefresh) <= engineSlowRefreshInterval && baseline.hourly != nil
	baseline.mu.Unlock()
	if fresh {
		return
	}

	var sent24h int64
	sctx, scancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer scancel()
	if err := db.QueryRowContext(sctx, `
		SELECT COUNT(*) FROM mailing_tracking_events
		WHERE event_type = 'sent' AND event_at > NOW() - INTERVAL '24 hours'
	`).Scan(&sent24h); err != nil {
		log.Printf("[EngineStatus] sent_24h probe: %v", err)
		return
	}

	hourly := []EngineHourPoint{}
	hctx, hcancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer hcancel()
	hrows, err := db.QueryContext(hctx, `
		SELECT DATE_TRUNC('hour', event_at) AS hour, COUNT(*)
		FROM mailing_tracking_events
		WHERE event_at > NOW() - INTERVAL '24 hours'
		  AND event_type = 'sent'
		GROUP BY 1
		ORDER BY 1
	`)
	if err != nil {
		log.Printf("[EngineStatus] per-hour probe: %v", err)
		return
	}
	func() {
		defer hrows.Close()
		for hrows.Next() {
			var p EngineHourPoint
			if scanErr := hrows.Scan(&p.Hour, &p.Sent); scanErr == nil {
				hourly = append(hourly, p)
			}
		}
	}()

	baseline.mu.Lock()
	baseline.sent24h = sent24h
	baseline.hourly = hourly
	baseline.aggRefresh = time.Now()
	baseline.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Section: waves
// ---------------------------------------------------------------------------

func engineWaveSection(ctx context.Context, db *sql.DB) EngineWaveSection {
	sec := EngineWaveSection{Upcoming: []EngineUpcomingWave{}}

	qctx, cancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer cancel()

	// created_at bound keeps this off months of historical wave rows; any
	// wave still relevant to the live board was created within 14 days.
	err := db.QueryRowContext(qctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'planned' AND scheduled_at <= NOW()),
			COUNT(*) FILTER (WHERE status = 'planned' AND scheduled_at >  NOW()),
			COUNT(*) FILTER (WHERE status IN ('dispatched','enqueuing')),
			COUNT(*) FILTER (WHERE status IN ('sending','running')),
			COUNT(*) FILTER (WHERE status = 'completed' AND COALESCE(completed_at, updated_at) > NOW() - INTERVAL '24 hours'),
			COALESCE(SUM(COALESCE(planned_recipients, 0)) FILTER (WHERE status = 'completed' AND COALESCE(completed_at, updated_at) > NOW() - INTERVAL '24 hours'), 0),
			COUNT(*) FILTER (WHERE status = 'failed'    AND COALESCE(failed_at, updated_at)    > NOW() - INTERVAL '24 hours'),
			COUNT(*) FILTER (WHERE status = 'cancelled' AND updated_at > NOW() - INTERVAL '24 hours')
		FROM mailing_campaign_waves
		WHERE created_at > NOW() - INTERVAL '14 days'
	`).Scan(&sec.PlannedDue, &sec.PlannedFuture, &sec.Enqueuing, &sec.Sending,
		&sec.Completed24h, &sec.CompletedRecipients24h, &sec.Failed24h, &sec.Cancelled24h)
	if err != nil {
		sec.Error = "wave counts: " + err.Error()
		return sec
	}

	uctx, ucancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer ucancel()
	rows, err := db.QueryContext(uctx, `
		SELECT w.id::text, w.campaign_id::text, COALESCE(c.name, ''),
		       w.wave_number, w.scheduled_at, COALESCE(w.planned_recipients, 0)
		FROM mailing_campaign_waves w
		LEFT JOIN mailing_campaigns c ON c.id = w.campaign_id
		WHERE w.status = 'planned'
		ORDER BY w.scheduled_at ASC
		LIMIT 5
	`)
	if err != nil {
		log.Printf("[EngineStatus] upcoming waves probe: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var u EngineUpcomingWave
			if scanErr := rows.Scan(&u.WaveID, &u.CampaignID, &u.CampaignName,
				&u.WaveNumber, &u.ScheduledAt, &u.PlannedRecipients); scanErr == nil {
				sec.Upcoming = append(sec.Upcoming, u)
			}
		}
	}

	sec.OK = true
	return sec
}

// ---------------------------------------------------------------------------
// Section: throughput
// ---------------------------------------------------------------------------

func engineThroughputSection(ctx context.Context, db *sql.DB, baseline *engineBaseline) EngineThroughputSection {
	sec := EngineThroughputSection{PerMinute: []EngineMinutePoint{}, PerHour: []EngineHourPoint{}}

	mctx, mcancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer mcancel()
	rows, err := db.QueryContext(mctx, `
		SELECT DATE_TRUNC('minute', event_at) AS minute,
		       COUNT(*) FILTER (WHERE event_type = 'sent'),
		       COUNT(*) FILTER (WHERE event_type IN ('deferred','deferral'))
		FROM mailing_tracking_events
		WHERE event_at > NOW() - INTERVAL '60 minutes'
		  AND event_type IN ('sent','deferred','deferral')
		GROUP BY 1
		ORDER BY 1
	`)
	if err != nil {
		sec.Error = "per-minute query: " + err.Error()
		return sec
	}
	func() {
		defer rows.Close()
		for rows.Next() {
			var p EngineMinutePoint
			if scanErr := rows.Scan(&p.Minute, &p.Sent, &p.Deferred); scanErr == nil {
				sec.PerMinute = append(sec.PerMinute, p)
			}
		}
	}()

	// 24h hourly buckets come from the slow-cadence cache (QA finding H1) —
	// the fast tick only runs the 60m per-minute query above.
	baseline.mu.Lock()
	sec.PerHour = append(sec.PerHour, baseline.hourly...)
	baseline.mu.Unlock()

	sec.CurrentRatePerMin, sec.PeakRatePerMin24h = engineRates(sec.PerMinute, sec.PerHour, time.Now().UTC())
	if sec.PeakRatePerMin24h > 0 {
		sec.UtilizationPct = 100 * sec.CurrentRatePerMin / sec.PeakRatePerMin24h
		if sec.UtilizationPct > 100 {
			sec.UtilizationPct = 100
		}
	}
	sec.OK = true
	return sec
}

// engineRates derives (currentRatePerMin, peakRatePerMin) from the series.
// Current = mean of the last 5 COMPLETE minutes (the in-progress minute would
// always read artificially low). Peak = max of the 60m per-minute buckets and
// the 24h hourly buckets normalized to per-minute. Exported logic kept pure
// for testability.
func engineRates(perMinute []EngineMinutePoint, perHour []EngineHourPoint, now time.Time) (current, peak float64) {
	currentMinute := now.Truncate(time.Minute)

	var complete []EngineMinutePoint
	for _, p := range perMinute {
		if p.Minute.Before(currentMinute) {
			complete = append(complete, p)
		}
		if p.Minute.Before(currentMinute) && float64(p.Sent) > peak {
			peak = float64(p.Sent)
		}
	}
	// Mean over the trailing 5-minute wall-clock window (missing buckets count
	// as zero — an idle engine should read 0/min, not the rate of its last
	// active minute).
	windowStart := currentMinute.Add(-5 * time.Minute)
	var sum int64
	for _, p := range complete {
		if !p.Minute.Before(windowStart) {
			sum += p.Sent
		}
	}
	current = float64(sum) / 5.0

	currentHour := now.Truncate(time.Hour)
	for _, h := range perHour {
		if h.Hour.Equal(currentHour) {
			continue // partial hour
		}
		if r := float64(h.Sent) / 60.0; r > peak {
			peak = r
		}
	}
	return current, peak
}

// ---------------------------------------------------------------------------
// Section: storm (per-ISP deferral rate, recent vs 24h baseline)
// ---------------------------------------------------------------------------

func engineStormSection(ctx context.Context, db *sql.DB, baseline *engineBaseline) EngineStormSection {
	sec := EngineStormSection{ISPs: []EngineISPDeferral{}}

	// Slow-cadence baseline: per-domain sent/deferred over the 25h→60m-ago
	// window. This is the single heavy query of the endpoint, so it refreshes
	// at most every engineSlowRefreshInterval and is reused between fast
	// ticks.
	baseline.mu.Lock()
	if time.Since(baseline.refreshedAt) > engineSlowRefreshInterval || baseline.sentBase == nil {
		bctx, bcancel := context.WithTimeout(ctx, 60*time.Second)
		rows, err := db.QueryContext(bctx, `
			SELECT COALESCE(NULLIF(LOWER(recipient_domain), ''), 'unknown'),
			       COUNT(*) FILTER (WHERE event_type = 'sent'),
			       COUNT(*) FILTER (WHERE event_type IN ('deferred','deferral'))
			FROM mailing_tracking_events
			WHERE event_at > NOW() - INTERVAL '25 hours'
			  AND event_at <= NOW() - INTERVAL '60 minutes'
			  AND event_type IN ('sent','deferred','deferral')
			GROUP BY 1
		`)
		if err != nil {
			log.Printf("[EngineStatus] deferral baseline probe: %v", err)
			// keep previous baseline (possibly nil → handled below)
		} else {
			sent := map[string]int64{}
			def := map[string]int64{}
			for rows.Next() {
				var domain string
				var s, d int64
				if scanErr := rows.Scan(&domain, &s, &d); scanErr == nil {
					isp := engineISPForDomain(domain)
					sent[isp] += s
					def[isp] += d
				}
			}
			rows.Close()
			baseline.sentBase = sent
			baseline.defBase = def
			baseline.refreshedAt = time.Now()
		}
		bcancel()
	}
	sentBase := baseline.sentBase
	defBase := baseline.defBase
	baseline.mu.Unlock()

	// Fast 60m window — partition-pruned to the current month, tiny range.
	rctx, rcancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer rcancel()
	rows, err := db.QueryContext(rctx, `
		SELECT COALESCE(NULLIF(LOWER(recipient_domain), ''), 'unknown'),
		       COUNT(*) FILTER (WHERE event_type = 'sent'),
		       COUNT(*) FILTER (WHERE event_type IN ('deferred','deferral'))
		FROM mailing_tracking_events
		WHERE event_at > NOW() - INTERVAL '60 minutes'
		  AND event_type IN ('sent','deferred','deferral')
		GROUP BY 1
	`)
	if err != nil {
		sec.Error = "recent deferral query: " + err.Error()
		return sec
	}
	sentRecent := map[string]int64{}
	defRecent := map[string]int64{}
	func() {
		defer rows.Close()
		for rows.Next() {
			var domain string
			var s, d int64
			if scanErr := rows.Scan(&domain, &s, &d); scanErr == nil {
				isp := engineISPForDomain(domain)
				sentRecent[isp] += s
				defRecent[isp] += d
			}
		}
	}()

	// Union of ISPs seen recently or in the baseline.
	ispSet := map[string]struct{}{}
	for isp := range sentRecent {
		ispSet[isp] = struct{}{}
	}
	for isp := range defRecent {
		ispSet[isp] = struct{}{}
	}
	for isp := range sentBase {
		ispSet[isp] = struct{}{}
	}

	for isp := range ispSet {
		row := EngineISPDeferral{
			ISP:         isp,
			Sent60m:     sentRecent[isp],
			Deferred60m: defRecent[isp],
			Sent24h:     sentBase[isp] + sentRecent[isp],
		}
		recentTotal := row.Sent60m + row.Deferred60m
		if recentTotal > 0 {
			row.RecentRate = float64(row.Deferred60m) / float64(recentTotal)
		}
		baseTotal := sentBase[isp] + defBase[isp]
		if baseTotal > 0 {
			row.BaselineRate = float64(defBase[isp]) / float64(baseTotal)
		}
		effectiveBaseline := row.BaselineRate
		if effectiveBaseline < baselineRateFloor {
			effectiveBaseline = baselineRateFloor
		}
		row.Multiplier = row.RecentRate / effectiveBaseline
		row.Storm = recentTotal >= stormMinEvents &&
			row.RecentRate > stormMinRate &&
			row.Multiplier >= stormMultiplierFloor
		if row.Storm {
			sec.Active = true
		}
		sec.ISPs = append(sec.ISPs, row)
	}

	// Worst offenders first: storms, then by recent deferral rate, then 24h
	// volume so the table reads top-down by importance.
	sort.Slice(sec.ISPs, func(i, j int) bool {
		a, b := sec.ISPs[i], sec.ISPs[j]
		if a.Storm != b.Storm {
			return a.Storm
		}
		if a.RecentRate != b.RecentRate {
			return a.RecentRate > b.RecentRate
		}
		return a.Sent24h > b.Sent24h
	})
	// Cap the payload — only the meaningful rows.
	if len(sec.ISPs) > 12 {
		sec.ISPs = sec.ISPs[:12]
	}

	sec.OK = true
	return sec
}

// engineISPForDomain folds a recipient domain into the platform's ISP
// families (matches the VMTA pool taxonomy: gmail/yahoo/microsoft/aol/
// comcast/charter/att/apple/cox/other). Kept local to this file to avoid an
// api→worker import for one map.
func engineISPForDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	switch d {
	case "gmail.com", "googlemail.com":
		return "gmail"
	case "ymail.com", "rocketmail.com", "verizon.net", "frontier.com", "frontiernet.net":
		return "yahoo"
	case "aol.com", "aim.com", "netscape.net", "cs.com":
		return "aol"
	case "msn.com":
		return "microsoft"
	case "comcast.net", "xfinity.com":
		return "comcast"
	case "charter.net", "rr.com", "twc.com", "roadrunner.com", "brighthouse.com", "spectrum.net":
		return "charter"
	case "att.net", "sbcglobal.net", "bellsouth.net", "pacbell.net", "ameritech.net", "swbell.net", "prodigy.net", "currently.com":
		return "att"
	case "icloud.com", "me.com", "mac.com":
		return "apple"
	case "cox.net":
		return "cox"
	}
	switch {
	case strings.HasPrefix(d, "yahoo."):
		return "yahoo"
	case strings.HasPrefix(d, "hotmail."), strings.HasPrefix(d, "outlook."), strings.HasPrefix(d, "live."):
		return "microsoft"
	}
	return "other"
}

// ---------------------------------------------------------------------------
// Section: workers
// ---------------------------------------------------------------------------

func engineWorkerSection(ctx context.Context, db *sql.DB) EngineWorkerSection {
	sec := EngineWorkerSection{Workers: []EngineWorkerBeat{}}

	qctx, cancel := context.WithTimeout(ctx, engineQueryTimeout)
	defer cancel()
	rows, err := db.QueryContext(qctx, `
		SELECT worker_name, last_beat_at,
		       COALESCE(expected_interval_seconds, 0),
		       COALESCE(stalled, FALSE),
		       COALESCE(last_status, ''),
		       COALESCE(last_error, '')
		FROM mailing_worker_heartbeats
		ORDER BY worker_name
	`)
	if err != nil {
		// Table may not exist in fresh environments — omit gracefully.
		sec.Error = "heartbeats: " + err.Error()
		return sec
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		var b EngineWorkerBeat
		if scanErr := rows.Scan(&b.Name, &b.LastBeatAt, &b.ExpectedIntervalSeconds,
			&b.Stalled, &b.LastStatus, &b.LastError); scanErr == nil {
			b.SecondsSinceBeat = int64(now.Sub(b.LastBeatAt).Seconds())
			sec.Workers = append(sec.Workers, b)
		}
	}
	sec.OK = true
	return sec
}

// ---------------------------------------------------------------------------
// ISP → VMTA pipes drill-down — GET /api/mailing/outbox/isp-pipes?isp=<family>
// ---------------------------------------------------------------------------
// Per-VMTA delivery breakdown for one ISP family over the last 24h, sourced
// from pmta_acct_raw (the raw PMTA accounting firehose — NOT
// mailing_tracking_events, which has the multi-row-per-recipient grain bug).
// Classification mirrors handlers_deliverability.go exactly by reusing its
// centralized filter constants (attempted/delivered/deferred/hard-bounce), so
// this drill-down can never disagree with the ISP Health Center on what a
// hard bounce is.
//
// ISP matching: recipient_isp holds lowercase isppkg.GroupFromDomain values
// (gmail/yahoo/aol/microsoft/...), but it is only stamped once the acct
// summary builder processes a row — the freshest rows still have it NULL. So
// we match recipient_isp first and fall back to recipient_domain ∈ the
// family's canonical domain list for unstamped rows. NOTE: the engine-status
// per-ISP table folds sbcglobal.net into 'att' (engineISPForDomain), while
// recipient_isp keeps 'sbcglobal' separate — drill into both families if
// reconciling AT&T totals exactly.
//
// The 24h scan can touch millions of rows (bounded by idx_acct_raw_received),
// so responses are cached per-family for ispPipesCacheTTL in a package-level
// map; on query failure a stale cached payload is served over a 500.

const (
	ispPipesCacheTTL     = 120 * time.Second
	ispPipesQueryTimeout = 30 * time.Second
	ispPipesMaxRows      = 50
)

// ISPPipeRow is one VMTA pipe's 24h scorecard within the requested family.
type ISPPipeRow struct {
	VMTA        string `json:"vmta"`
	Attempted   int64  `json:"attempted"`
	Delivered   int64  `json:"delivered"`
	Deferred    int64  `json:"deferred"`
	HardBounces int64  `json:"hard_bounces"`
	// Rates are fractions of attempted (0–1); 0 when attempted is 0.
	DeliveredRate  float64 `json:"delivered_rate"`
	DeferralRate   float64 `json:"deferral_rate"`
	HardBounceRate float64 `json:"hard_bounce_rate"`
}

// ISPPipesResponse is the full isp-pipes payload.
type ISPPipesResponse struct {
	ISP             string       `json:"isp"`
	WindowHours     int          `json:"window_hours"`
	GeneratedAt     time.Time    `json:"generated_at"`
	CacheAgeSeconds int64        `json:"cache_age_seconds"`
	Rows            []ISPPipeRow `json:"rows"`
}

var (
	ispPipesMu    sync.Mutex
	ispPipesCache = map[string]ISPPipesResponse{}
)

// HandleOutboxISPPipes serves GET /api/mailing/outbox/isp-pipes?isp=<family>.
func HandleOutboxISPPipes(db *sql.DB) http.HandlerFunc {
	// Valid families = every canonical recipient_isp value the summary
	// builder can write (isppkg.GroupFromDomain output) plus 'other'.
	valid := map[string]struct{}{isppkg.Other: {}}
	for _, g := range isppkg.AllGroups() {
		valid[g] = struct{}{}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		family := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("isp")))
		if _, ok := valid[family]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown isp family"})
			return
		}

		ispPipesMu.Lock()
		cached, hit := ispPipesCache[family]
		ispPipesMu.Unlock()
		if hit && time.Since(cached.GeneratedAt) < ispPipesCacheTTL {
			cached.CacheAgeSeconds = int64(time.Since(cached.GeneratedAt).Seconds())
			_ = json.NewEncoder(w).Encode(cached)
			return
		}

		resp, err := queryISPPipes(r.Context(), db, family)
		if err != nil {
			log.Printf("[isp_pipes] query failed for %q: %v", family, err)
			if hit {
				// Serve stale over a 500 — operator boards prefer old data.
				cached.CacheAgeSeconds = int64(time.Since(cached.GeneratedAt).Seconds())
				_ = json.NewEncoder(w).Encode(cached)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "query failed"})
			return
		}

		ispPipesMu.Lock()
		ispPipesCache[family] = resp
		ispPipesMu.Unlock()
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// queryISPPipes runs the 24h per-VMTA aggregate for one ISP family.
func queryISPPipes(ctx context.Context, db *sql.DB, family string) (ISPPipesResponse, error) {
	resp := ISPPipesResponse{
		ISP:         family,
		WindowHours: 24,
		GeneratedAt: time.Now().UTC(),
		Rows:        []ISPPipeRow{},
	}

	qctx, cancel := context.WithTimeout(ctx, ispPipesQueryTimeout)
	defer cancel()

	// Filter fragments are the shared constants from handlers_deliverability.go.
	rows, err := db.QueryContext(qctx, `
		SELECT COALESCE(NULLIF(vmta, ''), 'unknown') AS vmta,
		       COUNT(*) FILTER (WHERE `+attemptedFilterSQL+`)  AS attempted,
		       COUNT(*) FILTER (WHERE `+deliveredFilterSQL+`)  AS delivered,
		       COUNT(*) FILTER (WHERE `+deferredFilterSQL+`)   AS deferred,
		       COUNT(*) FILTER (WHERE `+hardBounceFilterSQL+`) AS hard_bounces
		FROM pmta_acct_raw
		WHERE received_at > NOW() - INTERVAL '24 hours'
		  AND (LOWER(COALESCE(NULLIF(recipient_isp, ''), 'other')) = $1
		       OR (NULLIF(recipient_isp, '') IS NULL
		           AND LOWER(COALESCE(recipient_domain, '')) = ANY($2)))
		GROUP BY 1
		ORDER BY 2 DESC
		LIMIT $3
	`, family, pq.Array(isppkg.DomainsForGroup(family)), ispPipesMaxRows)
	if err != nil {
		return resp, err
	}
	defer rows.Close()

	for rows.Next() {
		var p ISPPipeRow
		if scanErr := rows.Scan(&p.VMTA, &p.Attempted, &p.Delivered, &p.Deferred, &p.HardBounces); scanErr != nil {
			continue
		}
		if p.Attempted > 0 {
			p.DeliveredRate = float64(p.Delivered) / float64(p.Attempted)
			p.DeferralRate = float64(p.Deferred) / float64(p.Attempted)
			p.HardBounceRate = float64(p.HardBounces) / float64(p.Attempted)
		}
		resp.Rows = append(resp.Rows, p)
	}
	return resp, rows.Err()
}
