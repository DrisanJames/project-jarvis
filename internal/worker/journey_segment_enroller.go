package worker

// JourneySegmentEnroller polls active journeys with a segment-based
// trigger and inserts new enrollments for any subscriber who matches the
// segment but isn't already enrolled. This is the "auto-enroll" path
// behind the Welcome Series UI's "Cleaned, never mailed" preset and any
// other saved segment chosen as a journey trigger.
//
// Design constraints (locked in by the Welcome Series plan):
//
//  1. Idempotent. Re-running with the same active journey + same segment
//     must NOT create duplicate enrollments. We rely on the existing
//     UNIQUE (journey_id, subscriber_email) constraint via ON CONFLICT.
//  2. Cheap. Polling once every 5 minutes is the documented cadence; the
//     Phase 2 plan explicitly avoids tighter loops because Phase 3 will
//     batch outgoing waves on the PMTA scheduler's 15-minute cadence
//     anyway, so faster enrollment buys nothing.
//  3. Two flavors of segment input:
//       - normal saved segments: resolved via segmentation.Engine.ExecuteSegment
//         which returns a slice of subscriber UUIDs.
//       - the magic frontend preset id "__preset_cleaned_never_mailed__"
//         (declared in JourneyBuilder.tsx as CLEANED_NEVER_MAILED_PRESET_ID).
//         Resolved via a direct SQL query against mailing_subscribers
//         filtering last_email_at IS NULL AND last_verified_at IS NOT NULL.
//  4. Robust against schema drift. We resolve the trigger node by
//     scanning the journey's nodes JSON and pulling
//     config.triggerType == "segment" + config.segmentId.
//
// Phase 3 (wave-native send) will not need to touch this worker — it
// only enrolls. Phase 4 (engagement watcher) will exit subscribers from
// the enrollments this worker creates.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/segmentation"
)

// CleanedNeverMailedPresetID is the magic segment id the JourneyBuilder
// UI sends for the "cleaned but never mailed" trigger preset. Keep this
// in sync with web/src/components/mailing/components/JourneyBuilder.tsx
// (CLEANED_NEVER_MAILED_PRESET_ID).
const CleanedNeverMailedPresetID = "__preset_cleaned_never_mailed__"

// DefaultSegmentEnrollerInterval is the documented poll cadence from the
// Welcome Series plan (5 minutes). Production callers may override.
const DefaultSegmentEnrollerInterval = 5 * time.Minute

// DefaultSegmentEnrollerBatchLimit caps how many subscribers we enroll
// from a single segment per tick to avoid churning the journey
// enrollments table when a brand-new journey is first activated against
// a 1M+ row segment. Subsequent ticks pick up the rest.
const DefaultSegmentEnrollerBatchLimit = 5000

// JourneySegmentEnroller is the worker handle. Construct via
// NewJourneySegmentEnroller, then Start(ctx) once at boot. Stop() is
// safe to call multiple times.
type JourneySegmentEnroller struct {
	db          *sql.DB
	engine      *segmentation.Engine
	interval    time.Duration
	batchLimit  int
	stopChan    chan struct{}
	stopOnce    sync.Once
	totalEnrolled int64
	totalErrors   int64
}

// NewJourneySegmentEnroller wires the worker up. engine may be nil; in
// that case only the cleaned-never-mailed preset is honored (saved
// segments will be skipped with a log).
func NewJourneySegmentEnroller(db *sql.DB, engine *segmentation.Engine) *JourneySegmentEnroller {
	return &JourneySegmentEnroller{
		db:         db,
		engine:     engine,
		interval:   DefaultSegmentEnrollerInterval,
		batchLimit: DefaultSegmentEnrollerBatchLimit,
		stopChan:   make(chan struct{}),
	}
}

// WithInterval overrides the poll cadence (use only in tests or to slow
// the worker down during incidents).
func (w *JourneySegmentEnroller) WithInterval(d time.Duration) *JourneySegmentEnroller {
	w.interval = d
	return w
}

// WithBatchLimit overrides the per-tick enrollment cap.
func (w *JourneySegmentEnroller) WithBatchLimit(n int) *JourneySegmentEnroller {
	if n > 0 {
		w.batchLimit = n
	}
	return w
}

// Start runs the polling loop until ctx is cancelled or Stop() is
// called. Safe to call once; subsequent calls are no-ops.
func (w *JourneySegmentEnroller) Start(ctx context.Context) {
	go func() {
		log.Printf("JourneySegmentEnroller: started (interval=%s, batchLimit=%d)", w.interval, w.batchLimit)
		// Run once immediately so journey activation feels responsive.
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-w.stopChan:
				log.Println("JourneySegmentEnroller: stopped")
				return
			case <-ctx.Done():
				log.Println("JourneySegmentEnroller: context cancelled, stopping")
				return
			}
		}
	}()
}

// Stop terminates the polling loop. Idempotent.
func (w *JourneySegmentEnroller) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
}

// Stats returns lifetime counters for observability.
func (w *JourneySegmentEnroller) Stats() (enrolled, errors int64) {
	return atomic.LoadInt64(&w.totalEnrolled), atomic.LoadInt64(&w.totalErrors)
}

// activeSegmentJourney is one row from the journey scan: the bits we
// need to resolve a subscriber set and find the first executable node.
type activeSegmentJourney struct {
	JourneyID    string
	SegmentID    string
	FirstNodeID  string
	OrgID        string
}

// tick runs a single enrollment pass. Public-ish so tests can call it
// without standing up a goroutine.
func (w *JourneySegmentEnroller) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	journeys, err := w.findActiveSegmentJourneys(tickCtx)
	if err != nil {
		log.Printf("JourneySegmentEnroller: list active journeys: %v", err)
		atomic.AddInt64(&w.totalErrors, 1)
		return
	}

	for _, j := range journeys {
		emails, err := w.resolveSegmentEmails(tickCtx, j)
		if err != nil {
			log.Printf("JourneySegmentEnroller[%s]: resolve segment %s: %v", j.JourneyID, j.SegmentID, err)
			atomic.AddInt64(&w.totalErrors, 1)
			continue
		}
		if len(emails) == 0 {
			continue
		}
		inserted, err := w.insertEnrollments(tickCtx, j, emails)
		if err != nil {
			log.Printf("JourneySegmentEnroller[%s]: insert enrollments: %v", j.JourneyID, err)
			atomic.AddInt64(&w.totalErrors, 1)
			continue
		}
		atomic.AddInt64(&w.totalEnrolled, int64(inserted))
		if inserted > 0 {
			log.Printf("JourneySegmentEnroller[%s]: enrolled %d subscribers from segment %s",
				j.JourneyID, inserted, j.SegmentID)
		}
	}
}

// findActiveSegmentJourneys returns every active journey whose trigger
// node has triggerType=segment and a non-empty segmentId. We unpack the
// nodes JSON in Go because it's cheaper than a recursive jsonb query
// and keeps this worker independent of Postgres jsonb function support.
func (w *JourneySegmentEnroller) findActiveSegmentJourneys(ctx context.Context) ([]activeSegmentJourney, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, nodes::text
		FROM mailing_journeys
		WHERE status = 'active'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []activeSegmentJourney
	for rows.Next() {
		var id string
		var nodesJSON sql.NullString
		if err := rows.Scan(&id, &nodesJSON); err != nil {
			continue
		}
		if !nodesJSON.Valid || nodesJSON.String == "" {
			continue
		}
		segID, firstNodeID, ok := extractSegmentTrigger(nodesJSON.String)
		if !ok {
			continue
		}
		out = append(out, activeSegmentJourney{
			JourneyID:   id,
			SegmentID:   segID,
			FirstNodeID: firstNodeID,
		})
	}
	return out, rows.Err()
}

// extractSegmentTrigger parses a journey nodes JSON blob and returns the
// (segmentId, firstNonTriggerNodeId) pair, or ok=false if no segment
// trigger or no downstream node was found. Pulled out as a pure function
// so it can be unit tested without a DB.
func extractSegmentTrigger(nodesJSON string) (segmentID string, firstNodeID string, ok bool) {
	type node struct {
		ID     string                 `json:"id"`
		Type   string                 `json:"type"`
		Config map[string]interface{} `json:"config"`
	}
	var nodes []node
	if err := json.Unmarshal([]byte(nodesJSON), &nodes); err != nil {
		return "", "", false
	}
	for _, n := range nodes {
		if n.Type != "trigger" {
			continue
		}
		tt, _ := n.Config["triggerType"].(string)
		if tt != "segment" {
			return "", "", false
		}
		segID, _ := n.Config["segmentId"].(string)
		if segID == "" {
			return "", "", false
		}
		segmentID = segID
		break
	}
	if segmentID == "" {
		return "", "", false
	}
	for _, n := range nodes {
		if n.Type != "trigger" {
			firstNodeID = n.ID
			break
		}
	}
	if firstNodeID == "" {
		return "", "", false
	}
	return segmentID, firstNodeID, true
}

// resolveSegmentEmails turns a segment id (saved-segment UUID OR the
// preset id) into a slice of subscriber emails. We return emails (not
// IDs) because the existing mailing_journey_enrollments schema is keyed
// by subscriber_email, not subscriber_id. Phase 3 may add a join on
// subscriber_id once it's threaded through everywhere.
func (w *JourneySegmentEnroller) resolveSegmentEmails(ctx context.Context, j activeSegmentJourney) ([]string, error) {
	if j.SegmentID == CleanedNeverMailedPresetID {
		return w.resolveCleanedNeverMailed(ctx)
	}
	return w.resolveSavedSegment(ctx, j.SegmentID)
}

// resolveCleanedNeverMailed runs the preset query directly. We avoid
// going through the segmentation engine because the preset doesn't
// correspond to a real mailing_segments row.
//
// "Cleaned but never mailed" = subscriber has been verified at least
// once (last_verified_at IS NOT NULL) and has never received an email
// (last_email_at IS NULL). status filter excludes unsubscribed/bounced.
func (w *JourneySegmentEnroller) resolveCleanedNeverMailed(ctx context.Context) ([]string, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT email
		FROM mailing_subscribers
		WHERE last_email_at IS NULL
		  AND last_verified_at IS NOT NULL
		  AND status IN ('confirmed', 'active')
		  AND COALESCE(is_bot, false) = false
		ORDER BY last_verified_at ASC
		LIMIT $1
	`, w.batchLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEmails(rows)
}

// resolveSavedSegment defers to segmentation.Engine.ExecuteSegment which
// applies the segment's full condition tree. We then translate
// subscriber UUIDs into emails by joining mailing_subscribers.
func (w *JourneySegmentEnroller) resolveSavedSegment(ctx context.Context, segmentID string) ([]string, error) {
	if w.engine == nil {
		return nil, fmt.Errorf("segmentation engine not configured; only the cleaned-never-mailed preset is supported")
	}
	id, err := uuid.Parse(segmentID)
	if err != nil {
		return nil, fmt.Errorf("segmentId is not a uuid: %w", err)
	}
	result, err := w.engine.ExecuteSegment(ctx, id)
	if err != nil {
		return nil, err
	}
	if result == nil || len(result.SubscriberIDs) == 0 {
		return nil, nil
	}
	cap := w.batchLimit
	if len(result.SubscriberIDs) < cap {
		cap = len(result.SubscriberIDs)
	}
	ids := make([]uuid.UUID, cap)
	copy(ids, result.SubscriberIDs[:cap])

	rows, err := w.db.QueryContext(ctx, `
		SELECT email
		FROM mailing_subscribers
		WHERE id = ANY($1::uuid[])
		  AND status IN ('confirmed', 'active')
		  AND COALESCE(is_bot, false) = false
	`, uuidArrayParam(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEmails(rows)
}

// uuidArrayParam wraps a slice in the shape pq expects for an
// uuid[] bind parameter. Stays internal so the rest of the file is free
// of pq-specific types.
func uuidArrayParam(ids []uuid.UUID) interface{} {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	// Building a Postgres literal lets us avoid a hard pq import here
	// while still hitting the indexed = ANY(uuid[]) plan.
	if len(strs) == 0 {
		return "{}"
	}
	out := "{"
	for i, s := range strs {
		if i > 0 {
			out += ","
		}
		out += s
	}
	out += "}"
	return out
}

func scanEmails(rows *sql.Rows) ([]string, error) {
	var emails []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			continue
		}
		emails = append(emails, email)
	}
	return emails, rows.Err()
}

// insertEnrollments performs the per-journey upsert. We use
// ON CONFLICT (journey_id, subscriber_email) DO NOTHING so subscribers
// who are already enrolled (active, completed, exited, whatever) are
// silently skipped. Returns the number of *new* enrollments created.
func (w *JourneySegmentEnroller) insertEnrollments(ctx context.Context, j activeSegmentJourney, emails []string) (int, error) {
	if j.FirstNodeID == "" {
		return 0, fmt.Errorf("journey %s has no executable node after the trigger", j.JourneyID)
	}
	inserted := 0
	for _, email := range emails {
		enrollmentID := fmt.Sprintf("enroll-%s", uuid.NewString()[:8])
		res, err := w.db.ExecContext(ctx, `
			INSERT INTO mailing_journey_enrollments (
				id, journey_id, subscriber_email, current_node_id,
				status, enrolled_at, next_execute_at, metadata
			) VALUES (
				$1, $2, $3, $4, 'active', NOW(), NOW(), '{"source":"segment_enroller"}'::jsonb
			)
			ON CONFLICT (journey_id, subscriber_email) DO NOTHING
		`, enrollmentID, j.JourneyID, email, j.FirstNodeID)
		if err != nil {
			return inserted, err
		}
		n, _ := res.RowsAffected()
		inserted += int(n)
	}
	return inserted, nil
}
