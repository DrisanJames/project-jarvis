package worker

// JourneySendAdvancer is the bridge between the PMTA send pipeline and
// the journey state machine. When a shadow campaign produced by
// JourneyEmailNodeActivator finishes injecting (i.e. records a
// mailing_tracking_events row with event_type IN ('sent','bounced',
// 'failed')), this worker advances the corresponding enrollment from
// "wait_for_send" to the next node in the journey graph.
//
// Why a polling worker instead of inline coupling?
//
//   - The send pipeline (send_worker.go -> PMTAAPISender.Send ->
//     markSent) is shared by every campaign in the system. Adding a
//     hook there for journey advancement would couple the dispatcher
//     to the journey domain; the advancer keeps that boundary clean.
//   - mailing_tracking_events is the canonical event log; polling it
//     means we don't miss events even if a process crashes between the
//     send and the in-memory advancement. Restart safety.
//   - 1-minute polling cadence is well within the 15-minute wave
//     cadence; subscribers don't perceive lag.
//
// Phase scope:
//
// Phase 3 (this file): correlate by metadata->>'shadow_campaign_id' on
// mailing_journey_enrollments. Walk forward to the next node id by
// reading the journey's nodes JSON and following the first edge from
// the current node. The full graph traversal helper (findNextNode)
// will be promoted out of journey_executor.go in Phase 3.5; for now
// we reimplement the simple "first connection from current node" rule
// to avoid a breaking API change.
//
// Phase 4 (engagement watcher) and Phase 5 (production validation) do
// not need to change this worker.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultSendAdvancerInterval matches the plan's "1-minute polling
// interval" and tracking-event lag tolerance.
const DefaultSendAdvancerInterval = 1 * time.Minute

// JourneySendAdvancer is the worker handle. Construct via
// NewJourneySendAdvancer, then Start(ctx) once at boot.
type JourneySendAdvancer struct {
	db        *sql.DB
	interval  time.Duration
	stopChan  chan struct{}
	stopOnce  sync.Once
	advanced  int64
	exited    int64
	errors    int64
	lastSeen  time.Time
	lastSeenM sync.Mutex
}

// NewJourneySendAdvancer wires the worker. interval defaults to 1m.
func NewJourneySendAdvancer(db *sql.DB) *JourneySendAdvancer {
	return &JourneySendAdvancer{
		db:       db,
		interval: DefaultSendAdvancerInterval,
		stopChan: make(chan struct{}),
		// Bootstrapping: only advance enrollments based on events from
		// the last 24h on first run, to avoid replaying an entire
		// historical event log on cold start. Subsequent ticks update
		// lastSeen as events are processed.
		lastSeen: time.Now().Add(-24 * time.Hour),
	}
}

// WithInterval overrides the poll interval (tests).
func (a *JourneySendAdvancer) WithInterval(d time.Duration) *JourneySendAdvancer {
	if d > 0 {
		a.interval = d
	}
	return a
}

// Start launches the polling loop.
func (a *JourneySendAdvancer) Start(ctx context.Context) {
	go func() {
		log.Printf("JourneySendAdvancer: started (interval=%s)", a.interval)
		ticker := time.NewTicker(a.interval)
		defer ticker.Stop()
		// Run once immediately so initial activations don't wait a tick.
		a.tick(ctx)
		for {
			select {
			case <-ticker.C:
				a.tick(ctx)
			case <-a.stopChan:
				log.Println("JourneySendAdvancer: stopped")
				return
			case <-ctx.Done():
				log.Println("JourneySendAdvancer: context cancelled, stopping")
				return
			}
		}
	}()
}

// Stop terminates the polling loop. Idempotent.
func (a *JourneySendAdvancer) Stop() {
	a.stopOnce.Do(func() { close(a.stopChan) })
}

// Stats exposes lifetime counters.
func (a *JourneySendAdvancer) Stats() (advanced, exited, errors int64) {
	return atomic.LoadInt64(&a.advanced),
		atomic.LoadInt64(&a.exited),
		atomic.LoadInt64(&a.errors)
}

// pendingEvent is one row from the join of tracking events to shadow
// campaigns: enough to identify which enrollment to advance and which
// outcome to record.
type pendingEvent struct {
	EventID         string
	EventType       string
	OccurredAt      time.Time
	CampaignID      string
	JourneyID       string
	JourneyNodeID   string
	SubscriberEmail string
}

func (a *JourneySendAdvancer) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	a.lastSeenM.Lock()
	since := a.lastSeen
	a.lastSeenM.Unlock()

	events, newWatermark, err := a.fetchPendingEvents(tickCtx, since)
	if err != nil {
		log.Printf("JourneySendAdvancer: fetch events: %v", err)
		atomic.AddInt64(&a.errors, 1)
		return
	}
	if len(events) == 0 {
		return
	}

	for _, ev := range events {
		if err := a.processEvent(tickCtx, ev); err != nil {
			log.Printf("JourneySendAdvancer: process event %s: %v", ev.EventID, err)
			atomic.AddInt64(&a.errors, 1)
		}
	}

	a.lastSeenM.Lock()
	if newWatermark.After(a.lastSeen) {
		a.lastSeen = newWatermark
	}
	a.lastSeenM.Unlock()
}

// fetchPendingEvents joins mailing_tracking_events to mailing_campaigns
// where the campaign is journey-tagged, since the last watermark. We
// only consider terminal-injection events (sent, bounced, failed) so
// open/click events flow through the engagement watcher's path
// instead.
func (a *JourneySendAdvancer) fetchPendingEvents(ctx context.Context, since time.Time) ([]pendingEvent, time.Time, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT
			e.id::text,
			e.event_type,
			e.occurred_at,
			c.id::text,
			c.journey_id::text,
			c.journey_node_id,
			COALESCE(e.recipient_email, '')
		FROM mailing_tracking_events e
		JOIN mailing_campaigns c ON e.campaign_id = c.id
		WHERE c.journey_id IS NOT NULL
		  AND e.occurred_at > $1
		  AND e.event_type IN ('sent', 'bounced', 'failed')
		ORDER BY e.occurred_at ASC
		LIMIT 5000
	`, since)
	if err != nil {
		return nil, since, err
	}
	defer rows.Close()

	var events []pendingEvent
	maxSeen := since
	for rows.Next() {
		var ev pendingEvent
		if err := rows.Scan(
			&ev.EventID, &ev.EventType, &ev.OccurredAt,
			&ev.CampaignID, &ev.JourneyID, &ev.JourneyNodeID, &ev.SubscriberEmail,
		); err != nil {
			continue
		}
		events = append(events, ev)
		if ev.OccurredAt.After(maxSeen) {
			maxSeen = ev.OccurredAt
		}
	}
	return events, maxSeen, rows.Err()
}

// processEvent finds the enrollment by (journey_id, subscriber_email,
// metadata.shadow_campaign_id) and either advances to the next node on
// 'sent', or marks the enrollment failed on 'bounced'/'failed'.
func (a *JourneySendAdvancer) processEvent(ctx context.Context, ev pendingEvent) error {
	if ev.EventType == "sent" {
		return a.advanceEnrollment(ctx, ev)
	}
	return a.failEnrollment(ctx, ev)
}

// advanceEnrollment moves the enrollment from wait_for_send to the next
// node id (or completes the journey if there is none). The lookup is
// guarded by status filters so a duplicate event doesn't double-advance.
func (a *JourneySendAdvancer) advanceEnrollment(ctx context.Context, ev pendingEvent) error {
	nextNodeID, err := a.resolveNextNodeID(ctx, ev.JourneyID, ev.JourneyNodeID)
	if err != nil {
		return err
	}

	if nextNodeID == "" {
		res, err := a.db.ExecContext(ctx, `
			UPDATE mailing_journey_enrollments
			SET status = 'completed',
			    completed_at = NOW(),
			    updated_at = NOW(),
			    next_execute_at = NULL
			WHERE journey_id = $1
			  AND subscriber_email = $2
			  AND metadata->>'shadow_campaign_id' = $3
			  AND status IN ('active', 'wait_for_send', 'processing', 'waiting')
		`, ev.JourneyID, ev.SubscriberEmail, ev.CampaignID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		atomic.AddInt64(&a.advanced, n)
		return nil
	}

	res, err := a.db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments
		SET current_node_id = $4,
		    status = 'active',
		    next_execute_at = NOW(),
		    updated_at = NOW()
		WHERE journey_id = $1
		  AND subscriber_email = $2
		  AND metadata->>'shadow_campaign_id' = $3
		  AND status IN ('active', 'wait_for_send', 'processing', 'waiting')
	`, ev.JourneyID, ev.SubscriberEmail, ev.CampaignID, nextNodeID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	atomic.AddInt64(&a.advanced, n)
	return nil
}

// failEnrollment marks the enrollment failed on a bounce or send-time
// failure. We don't auto-advance because the user shouldn't get the
// next email if the previous one didn't deliver.
func (a *JourneySendAdvancer) failEnrollment(ctx context.Context, ev pendingEvent) error {
	res, err := a.db.ExecContext(ctx, `
		UPDATE mailing_journey_enrollments
		SET status = 'failed',
		    exit_reason = $4,
		    exited_at = NOW(),
		    updated_at = NOW(),
		    next_execute_at = NULL
		WHERE journey_id = $1
		  AND subscriber_email = $2
		  AND metadata->>'shadow_campaign_id' = $3
		  AND status NOT IN ('completed', 'failed', 'exited')
	`, ev.JourneyID, ev.SubscriberEmail, ev.CampaignID, "send_"+ev.EventType)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	atomic.AddInt64(&a.exited, n)
	return nil
}

// resolveNextNodeID parses the journey's nodes + connections JSON and
// returns the destination of the first connection out of fromNodeID, or
// "" if there is no outbound edge (= journey complete).
func (a *JourneySendAdvancer) resolveNextNodeID(ctx context.Context, journeyID, fromNodeID string) (string, error) {
	var connsJSON sql.NullString
	err := a.db.QueryRowContext(ctx, `
		SELECT connections::text FROM mailing_journeys WHERE id = $1
	`, journeyID).Scan(&connsJSON)
	if err == sql.ErrNoRows || !connsJSON.Valid {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	type edge struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	var edges []edge
	if err := json.Unmarshal([]byte(connsJSON.String), &edges); err != nil {
		return "", err
	}
	for _, e := range edges {
		if e.From == fromNodeID {
			return e.To, nil
		}
	}
	return "", nil
}
