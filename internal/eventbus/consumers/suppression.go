package consumers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// SuppressionMode selects how the projector treats consumed suppression ADDs.
type SuppressionMode string

const (
	// SuppressionModeShadow (DEFAULT) writes ONLY the suppression_shadow parity
	// ledger and NEVER touches the live hub. This is the dark default.
	SuppressionModeShadow SuppressionMode = "shadow"

	// SuppressionModeAuthoritative ALSO calls the provided add func (which the
	// integration step backs with GlobalSuppressionHub.Suppress). ADD-ONLY:
	// there is no unsuppress consumer by design.
	SuppressionModeAuthoritative SuppressionMode = "authoritative"
)

// suppressionShadowInsert mirrors the shadow dedup: ON CONFLICT (email,
// brand_root) DO NOTHING. brand_root carries '' for a global suppression so the
// UNIQUE key collapses on reprocessing instead of duplicating.
const suppressionShadowInsert = `
INSERT INTO suppression_shadow (email, reason, brand_root, source, kafka_event_uid)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (email, brand_root) DO NOTHING`

// suppressionPayload is the JSON the suppression tap publishes (see
// engine/global_suppression.go): {email, reason, brand_root, source}.
type suppressionPayload struct {
	Email     string `json:"email"`
	Reason    string `json:"reason"`
	BrandRoot string `json:"brand_root"`
	Source    string `json:"source"`
}

// SuppressionAddFunc is the live-hub ADD seam. The integration step backs it
// with GlobalSuppressionHub.Suppress(email, reason, brand, source). Only called
// in authoritative mode.
type SuppressionAddFunc func(email, reason, brand, source string)

// SuppressionProjector projects suppression.state ADDs.
//
// PER-INSTANCE BROADCAST consumer: every ECS task needs EVERY suppression in
// its in-memory hub, so this must read ALL partitions on EVERY instance. The
// integration step MUST give it a UNIQUE consumer-group id per instance (see
// GroupID) so it fans out rather than work-splits across the fleet.
type SuppressionProjector struct {
	db   ingestExecer
	cfg  eventbus.Config
	mode SuppressionMode
	add  SuppressionAddFunc

	consumer *eventbus.Consumer
	dlqProd  eventbus.Producer
}

// NewSuppressionProjector constructs the projector. mode defaults to
// SuppressionModeShadow when empty. add may be nil in shadow mode; in
// authoritative mode it is REQUIRED (Handle errors if it is missing) so a
// misconfiguration fails loudly rather than silently skipping the live hub.
func NewSuppressionProjector(db ingestExecer, cfg eventbus.Config, mode SuppressionMode, add SuppressionAddFunc) *SuppressionProjector {
	if mode == "" {
		mode = SuppressionModeShadow
	}
	return &SuppressionProjector{db: db, cfg: cfg, mode: mode, add: add}
}

// GroupID derives a UNIQUE consumer-group id per instance from a base group and
// the instance id (e.g. the ECS task id / hostname). The integration step MUST
// use this so the broadcast consumer fans out — each instance reads all
// partitions independently — instead of work-splitting one group across tasks.
//
//	cfg.Group = consumers.SuppressionGroupID(baseGroup, instanceID)
func SuppressionGroupID(base, instanceID string) string {
	if base == "" {
		base = "ignite-suppression-projector"
	}
	if instanceID == "" {
		return base
	}
	return base + "." + instanceID
}

// Handle is the eventbus.Handler. Shadow mode writes only the parity ledger;
// authoritative mode ALSO applies to the live hub via add.
func (p *SuppressionProjector) Handle(ctx context.Context, _, value []byte) error {
	var s suppressionPayload
	if err := json.Unmarshal(value, &s); err != nil {
		return fmt.Errorf("suppression-projector: unmarshal: %w", err)
	}
	if s.Email == "" {
		return fmt.Errorf("suppression-projector: empty email")
	}

	// kafka_event_uid: the suppression tap keys on email and carries no separate
	// uid; email+brand IS the dedup identity, so we record email as the uid for
	// traceability.
	if _, err := p.db.ExecContext(ctx, suppressionShadowInsert,
		s.Email,            // email
		nullStr(s.Reason),  // reason
		s.BrandRoot,        // brand_root ('' for global; NOT NULL by schema default)
		nullStr(s.Source),  // source
		nullStr(s.Email),   // kafka_event_uid
	); err != nil {
		return fmt.Errorf("suppression-projector: shadow insert: %w", err)
	}

	if p.mode == SuppressionModeAuthoritative {
		if p.add == nil {
			return fmt.Errorf("suppression-projector: authoritative mode requires an add func")
		}
		// ADD-only. Hub.Suppress is idempotent, so a Kafka replay is safe.
		p.add(s.Email, s.Reason, s.BrandRoot, s.Source)
	}
	return nil
}

// Start constructs the real group consumer and runs it. No-op when the bus is
// disabled. The integration step MUST set cfg.Group via SuppressionGroupID so
// the broadcast semantics hold. DLQ parks dead records on suppression.state.dlq.
func (p *SuppressionProjector) Start(ctx context.Context) error {
	if !p.cfg.Enabled() {
		return nil
	}
	cfg := p.cfg
	if len(cfg.Topics) == 0 {
		cfg.Topics = []string{eventbus.TopicSuppression}
	}

	prod, err := eventbus.NewKgoProducer(cfg)
	if err != nil {
		return fmt.Errorf("suppression-projector: dlq producer: %w", err)
	}
	p.dlqProd = prod

	con, err := eventbus.NewConsumer(cfg, p.Handle, NewProducerDLQ(prod), eventbus.ConsumerOptions{})
	if err != nil {
		_ = prod.Close()
		return fmt.Errorf("suppression-projector: consumer: %w", err)
	}
	p.consumer = con
	go func() { _ = con.Run(ctx) }()
	return nil
}

// Stop closes the consumer and its DLQ producer. Safe when never started.
func (p *SuppressionProjector) Stop() {
	if p.consumer != nil {
		_ = p.consumer.Close()
	}
	if p.dlqProd != nil {
		_ = p.dlqProd.Close()
	}
}
