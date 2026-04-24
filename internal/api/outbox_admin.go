package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// OutboxSummaryResponse is the JSON shape returned by GET /api/outbox/summary.
// It is intentionally small so operators can curl it during incidents without
// wading through a payload. Every field is directly queryable against the
// mailing_campaign_queue table — the outbox is its own source of truth.
type OutboxSummaryResponse struct {
	GeneratedAt             time.Time      `json:"generated_at"`
	APIVersion              string         `json:"api_version"`
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
const VersionOutboxSummary = "1.0"

// HandleOutboxSummary returns live counts of the injection outbox state. It
// runs a handful of cheap aggregate queries; each one uses the partial indexes
// we created in runStartupMigrations so no sequential scans are performed in
// the steady state.
func HandleOutboxSummary(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		resp := OutboxSummaryResponse{
			GeneratedAt:   time.Now().UTC(),
			APIVersion:    VersionOutboxSummary,
			DepthByStatus: map[string]int{},
		}

		rows, err := db.QueryContext(ctx, `
			SELECT status, COUNT(*)::int
			FROM mailing_campaign_queue
			GROUP BY status
		`)
		if err != nil {
			log.Printf("[outbox_summary] depth query failed: %v", err)
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var status string
			var count int
			if scanErr := rows.Scan(&status, &count); scanErr != nil {
				rows.Close()
				log.Printf("[outbox_summary] depth scan failed: %v", scanErr)
				http.Error(w, "scan failed", http.StatusInternalServerError)
				return
			}
			resp.DepthByStatus[status] = count
		}
		rows.Close()

		// Oldest-in-state probes. NULL coalesce to zero-age so the JSON is
		// always well-formed even when a given state has zero rows.
		ageQuery := `
			SELECT
				COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(scheduled_at)))::bigint, 0)
			FROM mailing_campaign_queue
			WHERE status = $1
		`
		db.QueryRowContext(ctx, ageQuery, "queued").Scan(&resp.OldestQueuedSeconds)
		db.QueryRowContext(ctx, `
			SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(locked_at)))::bigint, 0)
			FROM mailing_campaign_queue
			WHERE status = 'sending' AND locked_at IS NOT NULL
		`).Scan(&resp.OldestSendingSeconds)
		db.QueryRowContext(ctx, `
			SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(locked_at)))::bigint, 0)
			FROM mailing_campaign_queue
			WHERE status = 'submitting' AND locked_at IS NOT NULL
		`).Scan(&resp.OldestSubmittingSeconds)

		// Idempotency key coverage. Useful during the rollout to confirm new
		// wave enqueues are producing keys while legacy rows tail off.
		db.QueryRowContext(ctx, `
			SELECT COUNT(*)::bigint FROM mailing_campaign_queue WHERE idempotency_key IS NOT NULL
		`).Scan(&resp.IdempotencyKeyedRows)
		db.QueryRowContext(ctx, `
			SELECT COUNT(*)::bigint FROM mailing_campaign_queue WHERE idempotency_key IS NULL
		`).Scan(&resp.IdempotencyNullRows)

		// Rolling 5-minute throughput probes so operators can see activity
		// during a live campaign without leaving the summary endpoint.
		db.QueryRowContext(ctx, `
			SELECT COUNT(*)::bigint FROM mailing_campaign_queue
			WHERE created_at > NOW() - INTERVAL '5 minutes'
		`).Scan(&resp.Last5MinInserted)
		db.QueryRowContext(ctx, `
			SELECT COUNT(*)::bigint FROM mailing_campaign_queue
			WHERE sent_at > NOW() - INTERVAL '5 minutes'
		`).Scan(&resp.Last5MinSent)
		db.QueryRowContext(ctx, `
			SELECT COUNT(*)::bigint FROM mailing_campaign_queue
			WHERE last_attempt_at > NOW() - INTERVAL '5 minutes'
			  AND status IN ('failed','failed_retryable','failed_permanent','dead_letter','dead_letter_strict')
		`).Scan(&resp.Last5MinFailed)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
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
