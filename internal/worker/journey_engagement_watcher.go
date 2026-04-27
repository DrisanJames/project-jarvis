package worker

// JourneyEngagementWatcher implements the locked-in user decision:
// "any open or click anywhere in the system flips the subscriber to
// engager and exits all active enrollments." This worker polls
// mailing_tracking_events for opened/clicked events since a watermark
// and exits any active enrollments belonging to journeys that have
// exit_on_open or exit_on_click set.
//
// Design notes (per the Welcome Series plan):
//
//   - "Anywhere in the system" means the engagement does NOT have to
//     be on a journey email. Even an open on an unrelated newsletter
//     exits the subscriber from every active welcome-series journey
//     they're enrolled in. The user explicitly chose this.
//   - The journey-level toggles (exit_on_open / exit_on_click on
//     mailing_journeys) gate this per-journey. Journeys with both
//     toggles off behave as before.
//   - Enrollments correlated by subscriber_email (the existing schema
//     key). Phase 3.5 may add subscriber_id once threaded through.
//   - Idempotent: status NOT IN ('exited','completed','failed') filter
//     prevents double-exit.
//   - Watermark stored in-memory; on cold start we look back 1h to
//     avoid replaying the entire historical event log. Acceptable
//     behavior because a missed engagement window of <=1h on a fresh
//     boot is far smaller than the 15-min wave cadence anyway.
//
// Test coverage: see journey_engagement_watcher_test.go.

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

// DefaultEngagementWatcherInterval matches the plan's "30s" cadence.
const DefaultEngagementWatcherInterval = 30 * time.Second

// JourneyEngagementWatcher exits enrollments on opens/clicks anywhere.
type JourneyEngagementWatcher struct {
	db        *sql.DB
	interval  time.Duration
	stopChan  chan struct{}
	stopOnce  sync.Once
	exited    int64
	errors    int64
	lastSeen  time.Time
	lastSeenM sync.Mutex
}

// NewJourneyEngagementWatcher constructs the worker.
func NewJourneyEngagementWatcher(db *sql.DB) *JourneyEngagementWatcher {
	return &JourneyEngagementWatcher{
		db:       db,
		interval: DefaultEngagementWatcherInterval,
		stopChan: make(chan struct{}),
		// Look back 1h on cold start so a recent engagement (e.g.
		// during a deploy rollout) still produces an exit.
		lastSeen: time.Now().Add(-1 * time.Hour),
	}
}

// WithInterval overrides the poll cadence (tests).
func (w *JourneyEngagementWatcher) WithInterval(d time.Duration) *JourneyEngagementWatcher {
	if d > 0 {
		w.interval = d
	}
	return w
}

// Start launches the polling loop.
func (w *JourneyEngagementWatcher) Start(ctx context.Context) {
	go func() {
		log.Printf("JourneyEngagementWatcher: started (interval=%s)", w.interval)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		w.tick(ctx)
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-w.stopChan:
				log.Println("JourneyEngagementWatcher: stopped")
				return
			case <-ctx.Done():
				log.Println("JourneyEngagementWatcher: context cancelled, stopping")
				return
			}
		}
	}()
}

// Stop halts the worker. Idempotent.
func (w *JourneyEngagementWatcher) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
}

// Stats exposes lifetime counters.
func (w *JourneyEngagementWatcher) Stats() (exited, errors int64) {
	return atomic.LoadInt64(&w.exited), atomic.LoadInt64(&w.errors)
}

// tick runs one poll cycle. Pulled out as a method so tests can call
// it directly without spinning a goroutine.
func (w *JourneyEngagementWatcher) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	w.lastSeenM.Lock()
	since := w.lastSeen
	w.lastSeenM.Unlock()

	openEmails, clickEmails, newWatermark, err := w.fetchEngagedEmails(tickCtx, since)
	if err != nil {
		log.Printf("JourneyEngagementWatcher: fetch events: %v", err)
		atomic.AddInt64(&w.errors, 1)
		return
	}

	if len(openEmails) > 0 {
		exited, err := w.exitForEngagement(tickCtx, openEmails, "exit_on_open", "engaged_open")
		if err != nil {
			log.Printf("JourneyEngagementWatcher: exit on open: %v", err)
			atomic.AddInt64(&w.errors, 1)
		} else if exited > 0 {
			atomic.AddInt64(&w.exited, exited)
			log.Printf("JourneyEngagementWatcher: exited %d enrollments on open", exited)
		}
	}
	if len(clickEmails) > 0 {
		exited, err := w.exitForEngagement(tickCtx, clickEmails, "exit_on_click", "engaged_click")
		if err != nil {
			log.Printf("JourneyEngagementWatcher: exit on click: %v", err)
			atomic.AddInt64(&w.errors, 1)
		} else if exited > 0 {
			atomic.AddInt64(&w.exited, exited)
			log.Printf("JourneyEngagementWatcher: exited %d enrollments on click", exited)
		}
	}

	w.lastSeenM.Lock()
	if newWatermark.After(w.lastSeen) {
		w.lastSeen = newWatermark
	}
	w.lastSeenM.Unlock()
}

// fetchEngagedEmails returns the distinct subscriber emails that
// generated 'opened' / 'clicked' events since `since`, plus the new
// watermark. We bucket by event type so we can run two narrower exit
// queries against the appropriate journey toggle.
func (w *JourneyEngagementWatcher) fetchEngagedEmails(ctx context.Context, since time.Time) ([]string, []string, time.Time, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT event_type, COALESCE(recipient_email, ''), occurred_at
		FROM mailing_tracking_events
		WHERE event_type IN ('opened', 'clicked')
		  AND occurred_at > $1
		ORDER BY occurred_at ASC
		LIMIT 10000
	`, since)
	if err != nil {
		return nil, nil, since, err
	}
	defer rows.Close()

	openSet := make(map[string]struct{})
	clickSet := make(map[string]struct{})
	maxSeen := since
	for rows.Next() {
		var et, email string
		var ts time.Time
		if err := rows.Scan(&et, &email, &ts); err != nil {
			continue
		}
		if email == "" {
			continue
		}
		switch et {
		case "opened":
			openSet[email] = struct{}{}
		case "clicked":
			clickSet[email] = struct{}{}
		}
		if ts.After(maxSeen) {
			maxSeen = ts
		}
	}
	return setToSlice(openSet), setToSlice(clickSet), maxSeen, rows.Err()
}

// exitForEngagement updates active enrollments for journeys that have
// the matching exit_* flag set. Returns the number of enrollments
// transitioned to 'exited'. Done in one UPDATE so the pq.Array bind
// fans out to a single index lookup on subscriber_email.
func (w *JourneyEngagementWatcher) exitForEngagement(ctx context.Context, emails []string, flagColumn, exitReason string) (int64, error) {
	if len(emails) == 0 {
		return 0, nil
	}
	// flagColumn is hard-coded by the caller (exit_on_open /
	// exit_on_click), so substituting it into the SQL is safe and
	// avoids dynamic parameterization that would force sql.Null types.
	query := `
		UPDATE mailing_journey_enrollments e
		SET status = 'exited',
		    exited_at = NOW(),
		    exit_reason = $2,
		    updated_at = NOW(),
		    next_execute_at = NULL
		FROM mailing_journeys j
		WHERE e.journey_id = j.id
		  AND e.subscriber_email = ANY($1)
		  AND e.status NOT IN ('exited', 'completed', 'failed')
		  AND j.` + flagColumn + ` = true
	`
	res, err := w.db.ExecContext(ctx, query, pq.Array(emails), exitReason)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func setToSlice(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
