package worker

import (
	"context"
	"database/sql"
	"log"
	"time"
)

type GhostVisitorWorker struct {
	db       *sql.DB
	interval time.Duration
	cancel   context.CancelFunc
}

func NewGhostVisitorWorker(db *sql.DB, interval time.Duration) *GhostVisitorWorker {
	return &GhostVisitorWorker{db: db, interval: interval}
}

func (w *GhostVisitorWorker) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	go w.loop(ctx)
}

func (w *GhostVisitorWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *GhostVisitorWorker) loop(ctx context.Context) {
	w.runOnce(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *GhostVisitorWorker) runOnce(ctx context.Context) {
	start := time.Now()

	ghostQuery := `
	SELECT DISTINCT se.subscriber_id
	FROM subscriber_events se
	WHERE se.source IN ('site','site_beacon')
		AND se.event_type = 'page_view'
		AND se.subscriber_id IS NOT NULL
		AND se.subscriber_email IS NOT NULL AND se.subscriber_email != ''
		AND se.event_at > NOW() - INTERVAL '30 days'
		AND NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events te
			WHERE te.subscriber_id = se.subscriber_id
				AND te.event_type = 'clicked'
				AND te.event_at > NOW() - INTERVAL '45 days'
		)
	LIMIT 5000
	`

	rows, err := w.db.QueryContext(ctx, ghostQuery)
	if err != nil {
		log.Printf("[GhostVisitorWorker] query error: %v", err)
		return
	}
	defer rows.Close()

	var ghostIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ghostIDs = append(ghostIDs, id)
		}
	}

	tagged, untagged := 0, 0
	batchSize := 1000

	for i := 0; i < len(ghostIDs); i += batchSize {
		end := i + batchSize
		if end > len(ghostIDs) {
			end = len(ghostIDs)
		}
		batch := ghostIDs[i:end]

		tx, err := w.db.BeginTx(ctx, nil)
		if err != nil {
			log.Printf("[GhostVisitorWorker] tx begin error: %v", err)
			continue
		}

		for _, id := range batch {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO mailing_subscriber_tags (subscriber_id, tag) VALUES ($1, 'ghost_visitor') ON CONFLICT DO NOTHING`,
				id)
			if err == nil {
				tagged++
			}
		}

		if err := tx.Commit(); err != nil {
			log.Printf("[GhostVisitorWorker] tx commit error: %v", err)
			tx.Rollback()
		}
	}

	_, err = w.db.ExecContext(ctx, `
		DELETE FROM mailing_subscriber_tags
		WHERE tag = 'ghost_visitor'
			AND subscriber_id NOT IN (
				SELECT DISTINCT se.subscriber_id
				FROM subscriber_events se
				WHERE se.source IN ('site','site_beacon')
					AND se.event_type = 'page_view'
					AND se.subscriber_id IS NOT NULL
					AND se.event_at > NOW() - INTERVAL '30 days'
					AND NOT EXISTS (
						SELECT 1 FROM mailing_tracking_events te
						WHERE te.subscriber_id = se.subscriber_id
							AND te.event_type = 'clicked'
							AND te.event_at > NOW() - INTERVAL '45 days'
					)
			)
	`)
	if err != nil {
		log.Printf("[GhostVisitorWorker] untag error: %v", err)
	} else {
		untagged++
	}

	w.db.ExecContext(ctx, `
		UPDATE mailing_segments
		SET subscriber_count = (
			SELECT COUNT(*) FROM mailing_subscriber_tags WHERE tag = 'ghost_visitor'
		), updated_at = NOW()
		WHERE name = 'Ghost Visitors (System)'
	`)

	log.Printf("[GhostVisitorWorker] complete in %v: %d ghost IDs found, %d tagged, cleanup pass ran",
		time.Since(start).Round(time.Millisecond), len(ghostIDs), tagged)
}
