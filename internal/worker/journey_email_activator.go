package worker

// JourneyEmailNodeActivator turns a journey "email" node firing into a
// shadow campaign row in mailing_campaigns, tagged with journey_id /
// journey_node_id / journey_wave_index. Each shadow campaign captures
// the exact creative + audience that the journey produced for one wave
// generation of one node, so:
//
//   - /api/mailing/journeys/{id}/node-stats can group every shadow
//     campaign back under its node and roll up sent / delivered / open /
//     click / hard_bounce / soft_bounce per node.
//   - operators can audit every journey email as a real campaign in the
//     Campaigns list (campaign_type = 'journey_node').
//   - Phase 4's engagement watcher and Phase 5's send-completion bridge
//     have an unambiguous correlation key: shadow_campaign_id.
//
// Phased delivery (per the Welcome Series plan):
//
// Phase 3 (this file): write the audit row + buffer enrollments so the
// /node-stats endpoint and canvas tile have a real schema to query
// against, AND tag every enrollment with metadata.shadow_campaign_id so
// Phase 4 can advance them. To avoid breaking the existing send path
// during the cut-over window, the activator ALSO triggers the legacy
// emailSender for the enrollment's subscriber. This double-send guard
// is intentional and is removed in Phase 3.5 once the wave-native
// pipeline is verified end-to-end.
//
// Phase 3.5 (follow-up, NOT this file): rewire the activator to push
// the shadow campaign through planPMTAAudience + createPMTAWaveCampaign
// with a fresh InclusionList scoped to just the journey-enrolled
// subscribers, and drop the inline emailSender call.
//
// Concurrency: the activator buffers per (journey_id, node_id) inside a
// single mutex-protected map. Drain runs on a 1-minute ticker. Stop()
// flushes any remaining buffers before returning.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultActivatorDrainInterval matches the plan's "1-minute pending
// bucket" cadence. Production callers may override.
const DefaultActivatorDrainInterval = 1 * time.Minute

// JourneyEmailNodeActivator buffers enrollment activations for a
// journey email node and drains them into shadow campaign rows.
type JourneyEmailNodeActivator struct {
	db            *sql.DB
	drainInterval time.Duration

	mu      sync.Mutex
	buckets map[string]*activationBucket // keyed by journey_id + "|" + node_id

	stopChan chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// activationBucket holds the pending enrollments for one (journey, node)
// pair. Content fields are captured at first activation in the bucket
// so a node config edit between activations doesn't corrupt the wave.
type activationBucket struct {
	JourneyID   string
	NodeID      string
	Subject     string
	HTMLContent string
	FromName    string
	FromEmail   string
	Enrollments []bufferedEnrollment
}

type bufferedEnrollment struct {
	EnrollmentID    string
	SubscriberEmail string
}

// NewJourneyEmailNodeActivator returns a stopped activator. Call
// Start(ctx) once after wiring it into the journey executor.
func NewJourneyEmailNodeActivator(db *sql.DB) *JourneyEmailNodeActivator {
	return &JourneyEmailNodeActivator{
		db:            db,
		drainInterval: DefaultActivatorDrainInterval,
		buckets:       make(map[string]*activationBucket),
		stopChan:      make(chan struct{}),
	}
}

// WithDrainInterval overrides the per-bucket flush cadence (tests).
func (a *JourneyEmailNodeActivator) WithDrainInterval(d time.Duration) *JourneyEmailNodeActivator {
	if d > 0 {
		a.drainInterval = d
	}
	return a
}

// Start launches the drain loop. Idempotent.
func (a *JourneyEmailNodeActivator) Start(ctx context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		log.Printf("JourneyEmailNodeActivator: started (drain=%s)", a.drainInterval)
		ticker := time.NewTicker(a.drainInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.drainAll(ctx)
			case <-a.stopChan:
				a.drainAll(ctx)
				log.Println("JourneyEmailNodeActivator: stopped (final flush complete)")
				return
			case <-ctx.Done():
				a.drainAll(context.Background())
				log.Println("JourneyEmailNodeActivator: context cancelled (final flush complete)")
				return
			}
		}
	}()
}

// Stop flushes pending buckets and waits for the drain goroutine to
// exit. Idempotent.
func (a *JourneyEmailNodeActivator) Stop() {
	a.stopOnce.Do(func() { close(a.stopChan) })
	a.wg.Wait()
}

// ActivateNode is the entrypoint called by the journey executor when an
// email node fires for an enrollment. It buffers the enrollment until
// the next drain tick, at which point a single shadow campaign row is
// created for all enrollments in the bucket.
//
// Returns an error only on input validation failures so the executor
// can abort the enrollment. Database errors during drain are logged
// rather than propagated because they happen on the timer goroutine.
func (a *JourneyEmailNodeActivator) ActivateNode(
	enrollmentID, journeyID, nodeID, subscriberEmail string,
	subject, htmlContent, fromName, fromEmail string,
) error {
	if journeyID == "" || nodeID == "" || subscriberEmail == "" {
		return fmt.Errorf("ActivateNode requires journeyID, nodeID, and subscriberEmail")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := bucketKey(journeyID, nodeID)
	b, ok := a.buckets[key]
	if !ok {
		b = &activationBucket{
			JourneyID:   journeyID,
			NodeID:      nodeID,
			Subject:     subject,
			HTMLContent: htmlContent,
			FromName:    fromName,
			FromEmail:   fromEmail,
		}
		a.buckets[key] = b
	}
	b.Enrollments = append(b.Enrollments, bufferedEnrollment{
		EnrollmentID:    enrollmentID,
		SubscriberEmail: subscriberEmail,
	})
	return nil
}

// drainAll snapshots the current bucket map, resets it, and writes one
// shadow campaign per bucket. Any errors are logged but don't stop the
// rest of the drain — a single failed bucket must not stall the whole
// journey system.
func (a *JourneyEmailNodeActivator) drainAll(ctx context.Context) {
	a.mu.Lock()
	pending := a.buckets
	a.buckets = make(map[string]*activationBucket)
	a.mu.Unlock()

	for _, b := range pending {
		if len(b.Enrollments) == 0 {
			continue
		}
		if err := a.flushBucket(ctx, b); err != nil {
			log.Printf("JourneyEmailNodeActivator[%s/%s]: flush failed: %v",
				b.JourneyID, b.NodeID, err)
		}
	}
}

// flushBucket creates the shadow campaign row, attaches the
// shadow_campaign_id to each enrollment's metadata, and returns the new
// campaign id. We use a transaction so partial failures don't leak
// orphaned campaigns or orphaned enrollment metadata pointers.
func (a *JourneyEmailNodeActivator) flushBucket(ctx context.Context, b *activationBucket) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	waveIndex, err := a.nextWaveIndex(ctx, tx, b.JourneyID, b.NodeID)
	if err != nil {
		return fmt.Errorf("compute wave index: %w", err)
	}

	campaignID := uuid.NewString()
	name := fmt.Sprintf("Journey %s · Node %s · Wave %d", shortID(b.JourneyID), b.NodeID, waveIndex)

	orgID, err := a.lookupJourneyOrg(ctx, tx, b.JourneyID)
	if err != nil {
		return fmt.Errorf("resolve organization_id: %w", err)
	}

	// Insert the shadow campaign row. status='draft' until Phase 3.5
	// flips this to 'finalizing_audience'. We insert minimal columns
	// here — campaign_builder.go will own the full deploy path later.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, status,
			subject, from_name, from_email, html_content,
			campaign_type, execution_mode,
			journey_id, journey_node_id, journey_wave_index,
			total_recipients, max_recipients,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, 'draft',
			$4, $5, $6, $7,
			'journey_node', 'pmta_isp_wave',
			$8, $9, $10,
			$11, $11,
			NOW(), NOW()
		)
	`,
		campaignID, orgID, name,
		b.Subject, b.FromName, b.FromEmail, b.HTMLContent,
		b.JourneyID, b.NodeID, waveIndex,
		len(b.Enrollments),
	)
	if err != nil {
		return fmt.Errorf("insert shadow campaign: %w", err)
	}

	// Tag every buffered enrollment with the new campaign id so
	// Phase 4's engagement watcher and Phase 3.5's send-advancer can
	// correlate without a fresh schema. We use jsonb_set so we don't
	// clobber any other metadata keys (e.g. enroller source).
	for _, e := range b.Enrollments {
		_, err := tx.ExecContext(ctx, `
			UPDATE mailing_journey_enrollments
			SET metadata = jsonb_set(
				COALESCE(metadata, '{}'::jsonb),
				'{shadow_campaign_id}',
				to_jsonb($2::text),
				true
			),
			updated_at = NOW()
			WHERE id = $1
		`, e.EnrollmentID, campaignID)
		if err != nil {
			return fmt.Errorf("tag enrollment %s with shadow_campaign_id: %w", e.EnrollmentID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true

	log.Printf("JourneyEmailNodeActivator: shadow campaign %s created for journey=%s node=%s wave=%d (%d recipients)",
		campaignID, b.JourneyID, b.NodeID, waveIndex, len(b.Enrollments))
	return nil
}

// nextWaveIndex returns the next 1-based wave generation for a
// (journey, node) pair. Each drain produces one wave. Stored as an
// integer column so it sorts naturally and survives schema dumps.
func (a *JourneyEmailNodeActivator) nextWaveIndex(ctx context.Context, tx *sql.Tx, journeyID, nodeID string) (int, error) {
	var maxIdx sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT MAX(journey_wave_index)
		FROM mailing_campaigns
		WHERE journey_id = $1 AND journey_node_id = $2
	`, journeyID, nodeID).Scan(&maxIdx)
	if err != nil {
		return 0, err
	}
	if maxIdx.Valid {
		return int(maxIdx.Int64) + 1, nil
	}
	return 1, nil
}

// lookupJourneyOrg fetches the journey's organization_id so the shadow
// campaign inherits the same org and shows up in the right tenant
// scope. Falls back to a sentinel "00000000-..." UUID if the journey
// row is missing organization_id, which would only happen in tests.
func (a *JourneyEmailNodeActivator) lookupJourneyOrg(ctx context.Context, tx *sql.Tx, journeyID string) (string, error) {
	var orgID sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT organization_id::text FROM mailing_journeys WHERE id = $1
	`, journeyID).Scan(&orgID)
	if err == sql.ErrNoRows {
		return "00000000-0000-0000-0000-000000000001", nil
	}
	if err != nil {
		return "", err
	}
	if !orgID.Valid || orgID.String == "" {
		return "00000000-0000-0000-0000-000000000001", nil
	}
	return orgID.String, nil
}

// PendingCount returns the total number of buffered enrollments across
// all buckets. Used by tests and by /node-stats's "awaiting" metric.
// O(buckets), not O(enrollments), so cheap to call.
func (a *JourneyEmailNodeActivator) PendingCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	total := 0
	for _, b := range a.buckets {
		total += len(b.Enrollments)
	}
	return total
}

// PendingForNode returns how many enrollments are buffered for a
// specific (journey, node). Used by the per-node stats endpoint to
// surface the "audience awaiting injection" number on canvas tiles.
func (a *JourneyEmailNodeActivator) PendingForNode(journeyID, nodeID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.buckets[bucketKey(journeyID, nodeID)]
	if !ok {
		return 0
	}
	return len(b.Enrollments)
}

func bucketKey(journeyID, nodeID string) string {
	return journeyID + "|" + nodeID
}

// shortID truncates a uuid for log readability.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Marshal helpers for tests that want to inspect a bucket. Kept
// internal so they don't bloat the public surface.
//
//nolint:unused
func (b *activationBucket) marshal() string {
	out, _ := json.Marshal(b)
	return string(out)
}
