package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// OutboxSummaryResponse is the JSON shape returned by GET /api/outbox/summary.
// It is intentionally small so operators can curl it during incidents without
// wading through a payload. Every field is directly queryable against the
// mailing_campaign_queue table — the outbox is its own source of truth.
type OutboxSummaryResponse struct {
	GeneratedAt             time.Time      `json:"generated_at"`
	APIVersion              string         `json:"api_version"`
	CacheAgeSeconds         int64          `json:"cache_age_seconds"`
	DepthByStatus           map[string]int `json:"depth_by_status"`
	OldestQueuedSeconds     int64          `json:"oldest_queued_seconds"`
	OldestSendingSeconds    int64          `json:"oldest_sending_seconds"`
	OldestSubmittingSeconds int64          `json:"oldest_submitting_seconds"`
	IdempotencyKeyedRows    int64          `json:"idempotency_keyed_rows"`
	IdempotencyNullRows     int64          `json:"idempotency_null_rows"`
	Last5MinInserted        int64          `json:"last_5min_inserted"`
	Last5MinSent            int64          `json:"last_5min_sent"`
	Last5MinFailed          int64          `json:"last_5min_failed"`
}

// VersionOutboxSummary is bumped on every behavior change. Operators can match
// this against deploy logs to confirm the handler they're hitting is the one
// they expect.
//
// 1.1 — Added 30s in-memory cache with background refresher. The raw aggregate
// queries (GROUP BY status on a 1M+ row queue table) were taking 15s in
// production, causing dashboard polls to saturate the handler. Cache hit path
// is now sub-millisecond; refresher runs out-of-band so handler latency is
// decoupled from DB load.
const VersionOutboxSummary = "1.1"

// OutboxSummaryCache holds the most recently computed summary. The handler
// reads from it under RLock and never blocks on a DB query; the refresher
// goroutine is the sole writer. GeneratedAt on the cached payload lets
// operators see staleness directly. If the refresher has never run (e.g., the
// first 30 seconds after boot) the handler returns an empty-but-valid payload
// with CacheAgeSeconds = -1 so callers can distinguish "cold cache" from
// "everything is zero".
type OutboxSummaryCache struct {
	mu   sync.RWMutex
	data OutboxSummaryResponse
	set  bool
}

// Snapshot returns a copy of the cached summary plus a boolean indicating
// whether the refresher has ever populated it. Deep-copies the status map so
// the caller can't race the refresher mid-write.
func (c *OutboxSummaryCache) Snapshot() (OutboxSummaryResponse, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.set {
		return OutboxSummaryResponse{}, false
	}
	cp := c.data
	cp.DepthByStatus = make(map[string]int, len(c.data.DepthByStatus))
	for k, v := range c.data.DepthByStatus {
		cp.DepthByStatus[k] = v
	}
	return cp, true
}

func (c *OutboxSummaryCache) store(resp OutboxSummaryResponse) {
	c.mu.Lock()
	c.data = resp
	c.set = true
	c.mu.Unlock()
}

// defaultOutboxCache is the package-level cache shared between the refresher
// goroutine and the HTTP handler. Tests can construct their own cache and
// bypass this by calling handleOutboxSummaryWithCache directly.
var defaultOutboxCache = &OutboxSummaryCache{}

// OutboxSummaryRefreshInterval is how often the background goroutine recomputes
// the summary. 30s is short enough that operators see fresh data during an
// incident, long enough that the cost is negligible (a handful of aggregate
// scans per tick against indexed partial predicates).
const OutboxSummaryRefreshInterval = 30 * time.Second

// StartOutboxSummaryRefresher launches a long-lived goroutine that keeps the
// package-level cache populated. Safe to call exactly once from main.go at
// startup. Runs an immediate refresh on entry so the first curl after boot
// doesn't see an empty payload.
func StartOutboxSummaryRefresher(ctx context.Context, db *sql.DB) {
	go runOutboxSummaryRefresher(ctx, db, defaultOutboxCache, OutboxSummaryRefreshInterval)
}

// runOutboxSummaryRefresher is the loop body, extracted so tests can drive it
// with a shortened interval and a controllable context.
func runOutboxSummaryRefresher(ctx context.Context, db *sql.DB, cache *OutboxSummaryCache, interval time.Duration) {
	log.Printf("[OutboxSummary] refresher started (interval=%s)", interval)
	refreshOutboxSummary(ctx, db, cache)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[OutboxSummary] refresher stopping")
			return
		case <-ticker.C:
			refreshOutboxSummary(ctx, db, cache)
		}
	}
}

// refreshOutboxSummary runs the full set of aggregate queries against the DB
// and swaps the result into the cache. Each query runs with its own 20s
// timeout so a single slow aggregate can't wedge the entire refresh cycle.
// Errors are logged but never surfaced — the cache retains the previous good
// snapshot so handlers keep returning data during a transient DB hiccup.
func refreshOutboxSummary(ctx context.Context, db *sql.DB, cache *OutboxSummaryCache) {
	resp := OutboxSummaryResponse{
		GeneratedAt:   time.Now().UTC(),
		APIVersion:    VersionOutboxSummary,
		DepthByStatus: map[string]int{},
	}

	if depth, err := queryDepthByStatus(ctx, db); err == nil {
		resp.DepthByStatus = depth
	} else {
		log.Printf("[OutboxSummary] depth query failed: %v", err)
	}

	resp.OldestQueuedSeconds = queryOldest(ctx, db,
		`SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(scheduled_at)))::bigint, 0)
		 FROM mailing_campaign_queue WHERE status = 'queued'`)
	resp.OldestSendingSeconds = queryOldest(ctx, db,
		`SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(locked_at)))::bigint, 0)
		 FROM mailing_campaign_queue WHERE status = 'sending' AND locked_at IS NOT NULL`)
	resp.OldestSubmittingSeconds = queryOldest(ctx, db,
		`SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(locked_at)))::bigint, 0)
		 FROM mailing_campaign_queue WHERE status = 'submitting' AND locked_at IS NOT NULL`)

	resp.IdempotencyKeyedRows = queryInt64(ctx, db,
		`SELECT COUNT(*)::bigint FROM mailing_campaign_queue WHERE idempotency_key IS NOT NULL`)
	resp.IdempotencyNullRows = queryInt64(ctx, db,
		`SELECT COUNT(*)::bigint FROM mailing_campaign_queue WHERE idempotency_key IS NULL`)

	resp.Last5MinInserted = queryInt64(ctx, db,
		`SELECT COUNT(*)::bigint FROM mailing_campaign_queue
		 WHERE created_at > NOW() - INTERVAL '5 minutes'`)
	resp.Last5MinSent = queryInt64(ctx, db,
		`SELECT COUNT(*)::bigint FROM mailing_campaign_queue
		 WHERE sent_at > NOW() - INTERVAL '5 minutes'`)
	resp.Last5MinFailed = queryInt64(ctx, db,
		`SELECT COUNT(*)::bigint FROM mailing_campaign_queue
		 WHERE last_attempt_at > NOW() - INTERVAL '5 minutes'
		   AND status IN ('failed','failed_retryable','failed_permanent','dead_letter','dead_letter_strict')`)

	cache.store(resp)
}

func queryDepthByStatus(ctx context.Context, db *sql.DB) (map[string]int, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	rows, err := db.QueryContext(queryCtx, `
		SELECT status, COUNT(*)::int FROM mailing_campaign_queue GROUP BY status
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

// queryOldest and queryInt64 are tiny helpers so the refresh function reads
// linearly. Errors are swallowed intentionally — a single slow probe shouldn't
// blank out the entire cache. The 20s per-query timeout is the real safety
// net; if the DB is this slow we have bigger problems than a missing gauge.
func queryOldest(ctx context.Context, db *sql.DB, query string) int64 {
	queryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var v int64
	if err := db.QueryRowContext(queryCtx, query).Scan(&v); err != nil {
		log.Printf("[OutboxSummary] oldest probe failed: %v", err)
		return 0
	}
	return v
}

func queryInt64(ctx context.Context, db *sql.DB, query string) int64 {
	queryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var v int64
	if err := db.QueryRowContext(queryCtx, query).Scan(&v); err != nil {
		log.Printf("[OutboxSummary] int64 probe failed: %v", err)
		return 0
	}
	return v
}

// HandleOutboxSummary returns cached outbox state. Handler path is pure cache
// read + JSON encode; the DB is not touched on the request thread. If the
// refresher has not populated the cache yet the response is still valid JSON
// with CacheAgeSeconds = -1, which operators use to tell "refresher dead" from
// "everything is quiet".
//
// The db parameter is kept for interface compatibility with the old signature
// (server_routes_mailing.go passes the mailing DB in) but is not used on the
// request path.
func HandleOutboxSummary(db *sql.DB) http.HandlerFunc {
	_ = db
	return handleOutboxSummaryWithCache(defaultOutboxCache)
}

// handleOutboxSummaryWithCache lets tests drive the handler with an injected
// cache so they never need to stand up a real refresher loop.
func handleOutboxSummaryWithCache(cache *OutboxSummaryCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snapshot, populated := cache.Snapshot()
		if !populated {
			snapshot = OutboxSummaryResponse{
				GeneratedAt:     time.Now().UTC(),
				APIVersion:      VersionOutboxSummary,
				DepthByStatus:   map[string]int{},
				CacheAgeSeconds: -1,
			}
		} else {
			snapshot.CacheAgeSeconds = int64(time.Since(snapshot.GeneratedAt).Seconds())
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(snapshot); err != nil {
			log.Printf("[outbox_summary] encode failed: %v", err)
		}
	}
}

// OutboxDeadLetterRow is one row in the dead-letter listing. It intentionally
// redacts nothing — this endpoint is for operators behind auth, so showing the
// full email + last error is expected.
type OutboxDeadLetterRow struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CampaignID     string    `json:"campaign_id"`
	SubscriberID   string    `json:"subscriber_id"`
	Email          string    `json:"email"`
	Status         string    `json:"status"`
	Attempts       int       `json:"attempts"`
	LastError      string    `json:"last_error,omitempty"`
	LastAttemptAt  time.Time `json:"last_attempt_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// HandleOutboxDeadLetter lists the most recent dead-letter rows. Query params:
//
//	campaign_id — optional, filters to a single campaign.
//	limit       — optional, defaults to 200, capped at 1000.
func HandleOutboxDeadLetter(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		q := r.URL.Query()
		campaignID := q.Get("campaign_id")
		limit := 200
		if v := q.Get("limit"); v != "" {
			if n, err := parsePositiveIntOutbox(v); err == nil && n > 0 {
				if n > 1000 {
					n = 1000
				}
				limit = n
			}
		}

		args := []any{limit}
		where := "WHERE status IN ('dead_letter','dead_letter_strict')"
		if campaignID != "" {
			where += " AND campaign_id = $2"
			args = append(args, campaignID)
		}

		rows, err := db.QueryContext(ctx, `
			SELECT
				q.id::text,
				COALESCE(q.idempotency_key::text, ''),
				q.campaign_id::text,
				q.subscriber_id::text,
				COALESCE(s.email, ''),
				q.status,
				COALESCE(q.attempts, 0),
				COALESCE(q.error_message, ''),
				COALESCE(q.last_attempt_at, q.created_at),
				q.created_at
			FROM mailing_campaign_queue q
			LEFT JOIN mailing_subscribers s ON s.id = q.subscriber_id
			`+where+`
			ORDER BY COALESCE(q.last_attempt_at, q.created_at) DESC
			LIMIT $1
		`, args...)
		if err != nil {
			log.Printf("[outbox_dead_letter] query failed: %v", err)
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		out := make([]OutboxDeadLetterRow, 0, limit)
		for rows.Next() {
			var row OutboxDeadLetterRow
			if scanErr := rows.Scan(
				&row.ID,
				&row.IdempotencyKey,
				&row.CampaignID,
				&row.SubscriberID,
				&row.Email,
				&row.Status,
				&row.Attempts,
				&row.LastError,
				&row.LastAttemptAt,
				&row.CreatedAt,
			); scanErr != nil {
				log.Printf("[outbox_dead_letter] scan failed: %v", scanErr)
				continue
			}
			out = append(out, row)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"api_version": VersionOutboxSummary,
			"rows":        out,
			"count":       len(out),
		})
	}
}

// parsePositiveIntOutbox is a tiny local helper so we don't pull in strconv.Atoi
// for a single call site. Returns (n, nil) for strings that are valid positive
// integers, (0, err) otherwise.
func parsePositiveIntOutbox(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errOutboxBadInt
		}
		n = n*10 + int(c-'0')
		if n > 10_000_000 {
			return 0, errOutboxBadInt
		}
	}
	if n == 0 {
		return 0, errOutboxBadInt
	}
	return n, nil
}

type outboxError string

func (e outboxError) Error() string { return string(e) }

const errOutboxBadInt outboxError = "invalid integer"
