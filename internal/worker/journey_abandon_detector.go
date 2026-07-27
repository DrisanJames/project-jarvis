package worker

// JourneyAbandonDetector — the ABANDON trigger of the converter journey
// (operator spec 2026-07-27).
//
// Inlet: mailing_journey_events, fed by the WCL leadgen funnel through
// POST /api/mailing/journey/events (internal/api/journey_events_bridge.go):
// 'session_progress' on step transitions, 'lead_accepted' on a West Capital
// accept.
//
// Every tick (30 min) it detects abandoned sessions:
//   - session has >=1 session_progress event and its FIRST event is older
//     than JOURNEY_ABANDON_HOURS (default 4) but younger than 14 days
//     (late-arrival cap: ancient sessions never enroll),
//   - the session is attributable: sub1 (subscriber uuid from the send link)
//     or an email captured by the funnel,
//   - NO lead_accepted exists for the session's transid,
//
// and records them ONCE per session (PK session_id, ON CONFLICT DO NOTHING)
// in mailing_journey_abandon_state with status='pending'. It then:
//   - resolves email from sub1 for rows the funnel couldn't identify, and
//   - marks rows 'converted' whose transid gained a lead_accepted AFTER
//     detection — a converted session is NEVER abandon-touched (the future
//     sender additionally re-checks status='pending' at send time).
//
// SENDS: none. The abandon recovery touch + vertical follow-ups are
// config-only and DISABLED pending operator copy review
// (agents/journeys/converter_config.py ABANDON_JOURNEY); this worker only
// maintains the durable detection state.

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultAbandonInterval is the sweep cadence — abandonment is an hours-scale
// signal; 30 min keeps the state fresh without load.
const DefaultAbandonInterval = 30 * time.Minute

// DefaultAbandonHours: a session with no activity acceptance after this many
// hours from its FIRST event is abandoned. Env override JOURNEY_ABANDON_HOURS.
const DefaultAbandonHours = 4

// abandonDetectSQL inserts newly-abandoned sessions. The decision predicate
// lives here, in one statement, so a concurrent double-fire (two ECS tasks)
// collapses on the PK.
const abandonDetectSQL = `
	INSERT INTO mailing_journey_abandon_state
		(session_id, transid, email, sub1, loan_purpose, first_event_at)
	SELECT s.session_id, s.transid, s.email, s.sub1, s.loan_purpose, s.first_event_at
	FROM (
		SELECT session_id,
		       MAX(transid)                                        AS transid,
		       COALESCE(MAX(NULLIF(email, '')), '')                AS email,
		       COALESCE(MAX(NULLIF(sub1, '')), '')                 AS sub1,
		       COALESCE(MAX(NULLIF(form_data->>'loan_purpose','')), '') AS loan_purpose,
		       MIN(received_at)                                    AS first_event_at
		FROM mailing_journey_events
		WHERE event_type = 'session_progress' AND session_id <> ''
		GROUP BY session_id
	) s
	WHERE s.first_event_at < NOW() - make_interval(hours => $1)
	  AND s.first_event_at > NOW() - INTERVAL '14 days'
	  AND (s.sub1 <> '' OR s.email <> '')
	  AND NOT EXISTS (
	      SELECT 1 FROM mailing_journey_events c
	      WHERE c.event_type = 'lead_accepted' AND c.transid = s.transid)
	ON CONFLICT (session_id) DO NOTHING`

// abandonResolveEmailSQL fills email from sub1 (the subscriber uuid minted
// into send links as sub1={{subscriber.id}}) for rows the funnel couldn't
// identify directly. The ::text comparison sidesteps invalid-uuid cast errors
// on garbage sub1 values; the regex guard keeps the join sargable-sane.
const abandonResolveEmailSQL = `
	UPDATE mailing_journey_abandon_state a
	SET email = lower(s.email)
	FROM mailing_subscribers s
	WHERE a.status = 'pending' AND a.email = '' AND a.sub1 <> ''
	  AND a.sub1 ~ '^[0-9a-fA-F-]{36}$'
	  AND s.id = a.sub1::uuid`

// abandonLateConversionSQL retires pending rows whose session converted after
// detection. Idempotency + conversion safety: a converted session is never
// abandon-touched.
const abandonLateConversionSQL = `
	UPDATE mailing_journey_abandon_state a
	SET status = 'converted'
	WHERE a.status = 'pending'
	  AND EXISTS (
	      SELECT 1 FROM mailing_journey_events c
	      WHERE c.event_type = 'lead_accepted' AND c.transid = a.transid)`

// JourneyAbandonDetector is the worker handle.
type JourneyAbandonDetector struct {
	db           *sql.DB
	interval     time.Duration
	abandonHours int
	stopChan     chan struct{}
	stopOnce     sync.Once

	totalDetected  int64
	totalResolved  int64
	totalConverted int64
	totalErrors    int64
}

// NewJourneyAbandonDetector wires the worker.
func NewJourneyAbandonDetector(db *sql.DB) *JourneyAbandonDetector {
	return &JourneyAbandonDetector{
		db:           db,
		interval:     DefaultAbandonInterval,
		abandonHours: abandonHoursFromEnv(),
		stopChan:     make(chan struct{}),
	}
}

// abandonHoursFromEnv reads JOURNEY_ABANDON_HOURS; unset/invalid/<=0 → 4.
func abandonHoursFromEnv() int {
	v, err := strconv.Atoi(os.Getenv("JOURNEY_ABANDON_HOURS"))
	if err != nil || v <= 0 {
		return DefaultAbandonHours
	}
	return v
}

// WithInterval overrides the sweep cadence (tests).
func (w *JourneyAbandonDetector) WithInterval(d time.Duration) *JourneyAbandonDetector {
	if d > 0 {
		w.interval = d
	}
	return w
}

// WithAbandonHours overrides the N-hour threshold (tests).
func (w *JourneyAbandonDetector) WithAbandonHours(h int) *JourneyAbandonDetector {
	if h > 0 {
		w.abandonHours = h
	}
	return w
}

// Start runs the sweep loop until ctx cancels or Stop() is called.
func (w *JourneyAbandonDetector) Start(ctx context.Context) {
	go func() {
		log.Printf("JourneyAbandonDetector: started (interval=%s, abandon_hours=%d)", w.interval, w.abandonHours)
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-w.stopChan:
				log.Println("JourneyAbandonDetector: stopped")
				return
			case <-ctx.Done():
				log.Println("JourneyAbandonDetector: context cancelled, stopping")
				return
			}
		}
	}()
}

// Stop terminates the loop. Idempotent.
func (w *JourneyAbandonDetector) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
}

// Stats returns lifetime counters for observability.
func (w *JourneyAbandonDetector) Stats() (detected, resolved, converted, errors int64) {
	return atomic.LoadInt64(&w.totalDetected),
		atomic.LoadInt64(&w.totalResolved),
		atomic.LoadInt64(&w.totalConverted),
		atomic.LoadInt64(&w.totalErrors)
}

// tick runs the three statements. Order matters: late-conversion retirement
// runs LAST so a lead_accepted that raced the detect INSERT still retires the
// row in the same tick.
func (w *JourneyAbandonDetector) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	detected, err := w.exec(tickCtx, abandonDetectSQL, w.abandonHours)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyAbandonDetector: detect: %v", err)
		return
	}
	atomic.AddInt64(&w.totalDetected, detected)

	resolved, err := w.exec(tickCtx, abandonResolveEmailSQL)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyAbandonDetector: resolve email: %v", err)
	} else {
		atomic.AddInt64(&w.totalResolved, resolved)
	}

	converted, err := w.exec(tickCtx, abandonLateConversionSQL)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyAbandonDetector: late-conversion sweep: %v", err)
	} else {
		atomic.AddInt64(&w.totalConverted, converted)
	}

	if detected > 0 || converted > 0 {
		log.Printf("JourneyAbandonDetector: tick detected=%d email_resolved=%d late_converted=%d",
			detected, resolved, converted)
	}
}

func (w *JourneyAbandonDetector) exec(ctx context.Context, q string, args ...interface{}) (int64, error) {
	res, err := w.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
