package worker

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// ListRefreshWorker periodically updates subscriber_count and mailed_to
// on mailing_lists. Without this, list counts become stale as subscribers
// are added/removed and campaigns send.
type ListRefreshWorker struct {
	db       *sql.DB
	interval time.Duration
}

func NewListRefreshWorker(db *sql.DB, interval time.Duration) *ListRefreshWorker {
	return &ListRefreshWorker{db: db, interval: interval}
}

func (w *ListRefreshWorker) Start(ctx context.Context) {
	go func() {
		time.Sleep(45 * time.Second)
		w.refresh(ctx)

		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.refresh(ctx)
			case <-ctx.Done():
				log.Println("[list-refresh] stopped")
				return
			}
		}
	}()
}

func (w *ListRefreshWorker) refresh(ctx context.Context) {
	start := time.Now()

	// The four columns come from runStartupMigrations (add_list_*,
	// cmd/server/main.go, catalog-probed). Re-issuing them here queued an
	// ACCESS EXCLUSIVE lock on mailing_lists every tick (2026-09-11: one
	// waited behind a 190s segment insert with sessions queued behind it).

	// Update subscriber_count (total confirmed/active subscribers per list)
	res, err := w.db.ExecContext(ctx, `
		UPDATE mailing_lists l
		SET subscriber_count = sub.cnt,
		    active_count = sub.active_cnt,
		    last_refreshed_at = NOW()
		FROM (
			SELECT list_id,
			       COUNT(*) AS cnt,
			       COUNT(*) FILTER (WHERE status IN ('active', 'confirmed')) AS active_cnt
			FROM mailing_subscribers
			WHERE list_id IS NOT NULL
			GROUP BY list_id
		) sub
		WHERE l.id = sub.list_id
	`)
	if err != nil {
		log.Printf("[list-refresh] subscriber_count update failed: %v", err)
	} else {
		rows, _ := res.RowsAffected()
		log.Printf("[list-refresh] updated subscriber_count for %d lists", rows)
	}

	// Update mailed_to (count of distinct subscribers who received at least one send)
	res, err = w.db.ExecContext(ctx, `
		UPDATE mailing_lists l
		SET mailed_to = sub.cnt
		FROM (
			SELECT s.list_id, COUNT(DISTINCT q.subscriber_id) AS cnt
			FROM mailing_campaign_queue q
			JOIN mailing_subscribers s ON s.id = q.subscriber_id
			WHERE q.status = 'sent'
			  AND s.list_id IS NOT NULL
			GROUP BY s.list_id
		) sub
		WHERE l.id = sub.list_id
	`)
	if err != nil {
		log.Printf("[list-refresh] mailed_to update failed: %v", err)
	} else {
		rows, _ := res.RowsAffected()
		log.Printf("[list-refresh] updated mailed_to for %d lists", rows)
	}

	log.Printf("[list-refresh] completed in %s", time.Since(start).Round(time.Millisecond))
}
