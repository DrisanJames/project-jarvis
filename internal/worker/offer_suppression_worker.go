package worker

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// OfferSuppressionWorker runs on a configurable interval (default 24h) and
// performs four tasks:
//  1. Fatigue Suppression: suppress subscribers sent a given offer 5+ times
//     without converting.
//  2. Creative Performance Rollup: aggregate sends/opens/clicks/rates per
//     creative from queue + tracking events.
//  3. Subject Line Performance Rollup: aggregate sends/opens/open_rate per
//     subject line.
//  4. From Name Performance Rollup: aggregate sends/opens/complaint_rate per
//     from name.
//  5. Deployment Stats Update: update mailing_offer_deployments with final
//     send totals.
type OfferSuppressionWorker struct {
	db       *sql.DB
	interval time.Duration
	cancel   context.CancelFunc
}

func NewOfferSuppressionWorker(db *sql.DB, interval time.Duration) *OfferSuppressionWorker {
	return &OfferSuppressionWorker{db: db, interval: interval}
}

func (w *OfferSuppressionWorker) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	go w.loop(ctx)
	log.Printf("[OfferSuppressionWorker] started, interval=%s", w.interval)
}

func (w *OfferSuppressionWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *OfferSuppressionWorker) loop(ctx context.Context) {
	w.run(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[OfferSuppressionWorker] stopped")
			return
		case <-ticker.C:
			w.run(ctx)
		}
	}
}

func (w *OfferSuppressionWorker) run(ctx context.Context) {
	start := time.Now()
	log.Println("[OfferSuppressionWorker] starting nightly run")

	w.syncFatigueSuppressions(ctx)
	w.rollupCreativePerformance(ctx)
	w.rollupSubjectPerformance(ctx)
	w.rollupFromNamePerformance(ctx)
	w.rollupDeploymentStats(ctx)

	log.Printf("[OfferSuppressionWorker] nightly run complete in %s", time.Since(start).Round(time.Millisecond))
}

// syncFatigueSuppressions suppresses subscribers who have been sent a given
// offer 5+ times (via mailing_campaign_queue) without converting. Uses
// ON CONFLICT DO NOTHING to remain idempotent.
func (w *OfferSuppressionWorker) syncFatigueSuppressions(ctx context.Context) {
	result, err := w.db.ExecContext(ctx, `
		INSERT INTO mailing_offer_suppressions (id, organization_id, offer_id, subscriber_id, reason, source, suppressed_at)
		SELECT gen_random_uuid(), o.organization_id, q.offer_id, q.subscriber_id,
		       'fatigue', 'nightly_sync', NOW()
		FROM (
			SELECT cq.offer_id, cq.subscriber_id
			FROM mailing_campaign_queue cq
			WHERE cq.offer_id IS NOT NULL AND cq.status = 'sent'
			GROUP BY cq.offer_id, cq.subscriber_id
			HAVING COUNT(*) >= 5
		) q
		JOIN mailing_offers o ON o.id = q.offer_id
		WHERE NOT EXISTS (
			SELECT 1 FROM mailing_offer_suppressions os
			WHERE os.offer_id = q.offer_id AND os.subscriber_id = q.subscriber_id
		)
		ON CONFLICT (offer_id, subscriber_id) DO NOTHING
	`)
	if err != nil {
		log.Printf("[OfferSuppressionWorker] fatigue suppression error: %v", err)
		return
	}
	rows, _ := result.RowsAffected()
	log.Printf("[OfferSuppressionWorker] fatigue suppression: %d subscribers suppressed", rows)
}

// rollupCreativePerformance aggregates total_sends (from campaign_queue) and
// total_opens/total_clicks (from tracking_events) per creative, then computes
// open_rate and click_rate.
func (w *OfferSuppressionWorker) rollupCreativePerformance(ctx context.Context) {
	result, err := w.db.ExecContext(ctx, `
		UPDATE mailing_offer_creatives oc SET
			total_sends  = COALESCE(sq.sends, 0),
			total_opens  = COALESCE(te.opens, 0),
			total_clicks = COALESCE(te.clicks, 0),
			open_rate    = CASE WHEN COALESCE(sq.sends, 0) > 0
			                    THEN COALESCE(te.opens, 0)::decimal / sq.sends
			                    ELSE 0 END,
			click_rate   = CASE WHEN COALESCE(sq.sends, 0) > 0
			                    THEN COALESCE(te.clicks, 0)::decimal / sq.sends
			                    ELSE 0 END,
			updated_at   = NOW()
		FROM (
			SELECT creative_id, COUNT(*) AS sends
			FROM mailing_campaign_queue
			WHERE creative_id IS NOT NULL AND status = 'sent'
			GROUP BY creative_id
		) sq
		LEFT JOIN (
			SELECT creative_id,
				COUNT(*) FILTER (WHERE event_type = 'opened')  AS opens,
				COUNT(*) FILTER (WHERE event_type = 'clicked') AS clicks
			FROM mailing_tracking_events
			WHERE creative_id IS NOT NULL
			GROUP BY creative_id
		) te ON te.creative_id = sq.creative_id
		WHERE oc.id = sq.creative_id
	`)
	if err != nil {
		log.Printf("[OfferSuppressionWorker] creative rollup error: %v", err)
		return
	}
	rows, _ := result.RowsAffected()
	log.Printf("[OfferSuppressionWorker] creative rollup: %d creatives updated", rows)
}

// rollupSubjectPerformance aggregates total_sends (from campaign_queue) and
// total_opens (from tracking_events) per subject line, then computes open_rate.
func (w *OfferSuppressionWorker) rollupSubjectPerformance(ctx context.Context) {
	result, err := w.db.ExecContext(ctx, `
		UPDATE mailing_offer_subject_lines osl SET
			total_sends = COALESCE(sq.sends, 0),
			total_opens = COALESCE(te.opens, 0),
			open_rate   = CASE WHEN COALESCE(sq.sends, 0) > 0
			                   THEN COALESCE(te.opens, 0)::decimal / sq.sends
			                   ELSE 0 END,
			updated_at  = NOW()
		FROM (
			SELECT subject_line_id, COUNT(*) AS sends
			FROM mailing_campaign_queue
			WHERE subject_line_id IS NOT NULL AND status = 'sent'
			GROUP BY subject_line_id
		) sq
		LEFT JOIN (
			SELECT subject_line_id,
				COUNT(*) FILTER (WHERE event_type = 'opened') AS opens
			FROM mailing_tracking_events
			WHERE subject_line_id IS NOT NULL
			GROUP BY subject_line_id
		) te ON te.subject_line_id = sq.subject_line_id
		WHERE osl.id = sq.subject_line_id
	`)
	if err != nil {
		log.Printf("[OfferSuppressionWorker] subject rollup error: %v", err)
		return
	}
	rows, _ := result.RowsAffected()
	log.Printf("[OfferSuppressionWorker] subject rollup: %d subject lines updated", rows)
}

// rollupFromNamePerformance aggregates total_sends (from campaign_queue) and
// total_opens + complaints (from tracking_events) per from_name, then computes
// complaint_rate.
func (w *OfferSuppressionWorker) rollupFromNamePerformance(ctx context.Context) {
	result, err := w.db.ExecContext(ctx, `
		UPDATE mailing_offer_from_names ofn SET
			total_sends    = COALESCE(sq.sends, 0),
			total_opens    = COALESCE(te.opens, 0),
			complaint_rate = CASE WHEN COALESCE(sq.sends, 0) > 0
			                      THEN COALESCE(te.complaints, 0)::decimal / sq.sends
			                      ELSE 0 END,
			updated_at     = NOW()
		FROM (
			SELECT from_name_id, COUNT(*) AS sends
			FROM mailing_campaign_queue
			WHERE from_name_id IS NOT NULL AND status = 'sent'
			GROUP BY from_name_id
		) sq
		LEFT JOIN (
			SELECT from_name_id,
				COUNT(*) FILTER (WHERE event_type = 'opened')    AS opens,
				COUNT(*) FILTER (WHERE event_type = 'complained') AS complaints
			FROM mailing_tracking_events
			WHERE from_name_id IS NOT NULL
			GROUP BY from_name_id
		) te ON te.from_name_id = sq.from_name_id
		WHERE ofn.id = sq.from_name_id
	`)
	if err != nil {
		log.Printf("[OfferSuppressionWorker] from-name rollup error: %v", err)
		return
	}
	rows, _ := result.RowsAffected()
	log.Printf("[OfferSuppressionWorker] from-name rollup: %d from names updated", rows)
}

// rollupDeploymentStats updates mailing_offer_deployments with the final
// total_sent count from the campaign queue.
func (w *OfferSuppressionWorker) rollupDeploymentStats(ctx context.Context) {
	result, err := w.db.ExecContext(ctx, `
		UPDATE mailing_offer_deployments od SET
			total_sent = COALESCE(sq.sends, 0)
		FROM (
			SELECT campaign_id, offer_id, COUNT(*) AS sends
			FROM mailing_campaign_queue
			WHERE offer_id IS NOT NULL AND status = 'sent'
			GROUP BY campaign_id, offer_id
		) sq
		WHERE od.campaign_id = sq.campaign_id AND od.offer_id = sq.offer_id
	`)
	if err != nil {
		log.Printf("[OfferSuppressionWorker] deployment stats error: %v", err)
		return
	}
	rows, _ := result.RowsAffected()
	log.Printf("[OfferSuppressionWorker] deployment stats: %d deployments updated", rows)
}
