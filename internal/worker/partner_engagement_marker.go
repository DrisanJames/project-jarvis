package worker

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"
)

// PartnerEngagementMarker stamps partner_clean_queue.engaged_at when a record's
// subscriber CLICKS one of that record's own data-partner-drip campaigns (matched
// by mailing_campaigns.partner_dataset_id = partner_clean_queue.dataset_id).
//
// Why this exists (2026-06-22): the engaged_at column was added 2026-06-11 and its
// READERS were wired immediately — the drip's next-due query only mails records
// WHERE engaged_at IS NULL (so an engaged record should EXIT the drip), and every
// data-partner Activation/Churn metric counts COUNT(*) FILTER (WHERE engaged_at IS
// NOT NULL). But the WRITER — this "engagement marker" — was never built, so
// engaged_at was permanently NULL. Effect: (1) the drip never let an engaged record
// exit → it pushed every record through all four touches (over-mailing proven
// clickers); (2) every Activation metric read 0 / Churn read ~100% regardless of
// real performance (e.g. Spicy Clickers showed Engaged 0 despite ~2,123 real
// clickers). This worker closes the loop.
//
// Signal = CLICKS ONLY. Opens are ~90% Apple-MPP/machine; marking on opens would
// falsely "engage" nearly everyone after one touch and gut the drip. A click is the
// reliable human re-engagement signal this drip is designed to detect. Scoped to the
// dataset's own campaigns (partner_dataset_id) so cross-lane clicks don't leak — the
// same accuracy rule the Previous Activations v2 endpoint uses. Idempotent via the
// engaged_at IS NULL guard.
//
// Kill switch: DISABLE_PARTNER_ENGAGEMENT_MARKER=1 disables it entirely.
type PartnerEngagementMarker struct {
	db       *sql.DB
	interval time.Duration
}

func NewPartnerEngagementMarker(db *sql.DB) *PartnerEngagementMarker {
	return &PartnerEngagementMarker{db: db, interval: 3 * time.Minute}
}

// Start runs a one-time all-history backfill ~2min after boot, then a windowed
// sweep on each tick. Runs until ctx is cancelled.
func (m *PartnerEngagementMarker) Start(ctx context.Context) {
	if m.db == nil {
		log.Printf("[PartnerEngagementMarker] disabled (db missing)")
		return
	}
	if os.Getenv("DISABLE_PARTNER_ENGAGEMENT_MARKER") == "1" {
		log.Printf("[PartnerEngagementMarker] disabled via DISABLE_PARTNER_ENGAGEMENT_MARKER")
		return
	}
	log.Printf("[PartnerEngagementMarker] started interval=%s (clicks->engaged_at)", m.interval)

	// Startup delay so migrations/boot settle before the heavy backfill.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}
	m.markOnce(ctx, 0) // backfill over all history

	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[PartnerEngagementMarker] stopping")
			return
		case <-t.C:
			// Ongoing: only scan recent clicks. 30m overlaps the 3m interval
			// generously so no engagement is dropped across run boundaries.
			m.markOnce(ctx, 30)
		}
	}
}

// markOnce stamps engaged_at on records whose subscriber clicked one of that
// dataset's drip campaigns. lookbackMins <= 0 means all history (backfill).
func (m *PartnerEngagementMarker) markOnce(ctx context.Context, lookbackMins int) {
	timeFilter := ""
	var args []interface{}
	if lookbackMins > 0 {
		timeFilter = "AND te.event_at > NOW() - make_interval(mins => $1)"
		args = append(args, lookbackMins)
	}
	q := `
		UPDATE partner_clean_queue q
		SET engaged_at = e.first_click, updated_at = NOW()
		FROM (
			SELECT te.subscriber_id, c.partner_dataset_id, MIN(te.event_at) AS first_click
			FROM mailing_tracking_events te
			JOIN mailing_campaigns c ON c.id = te.campaign_id
			WHERE te.event_type = 'clicked'
			  AND c.partner_dataset_id IS NOT NULL
			  ` + timeFilter + `
			GROUP BY te.subscriber_id, c.partner_dataset_id
		) e
		WHERE q.subscriber_id = e.subscriber_id
		  AND q.dataset_id = e.partner_dataset_id
		  AND q.engaged_at IS NULL`
	res, err := m.db.ExecContext(ctx, q, args...)
	if err != nil {
		log.Printf("[PartnerEngagementMarker] mark err (lookback=%dm): %v", lookbackMins, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[PartnerEngagementMarker] marked %d records engaged (clicks, lookback=%dm)", n, lookbackMins)
	}
}
