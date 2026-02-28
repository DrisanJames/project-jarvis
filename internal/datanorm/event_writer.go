package datanorm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// EventWriter inserts rows into the subscriber_events partitioned table.
// Used by PMTA collector, tracking service, site events, and data normalizer.
type EventWriter struct {
	db *sql.DB
}

func NewEventWriter(db *sql.DB) *EventWriter {
	return &EventWriter{db: db}
}

// notifyEventTypes are event types worth pushing to the WebSocket hub (H1).
var notifyEventTypes = map[string]bool{
	"open": true, "click": true, "conversion": true,
	"hard_bounce": true, "complaint": true, "unsubscribe": true,
}

// WriteEvent inserts a single event. If notify is true AND the event type
// is high-value, it also fires pg_notify for the WebSocket hub.
func (ew *EventWriter) WriteEvent(ctx context.Context, emailHash, eventType string, campaignID *uuid.UUID, variantID *uuid.UUID, source string, metadata map[string]interface{}, notify bool) error {
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		metaJSON = []byte("{}")
	}

	var id int64
	err = ew.db.QueryRowContext(ctx,
		`INSERT INTO subscriber_events (email_hash, event_type, campaign_id, variant_id, source, metadata, event_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW()) RETURNING id`,
		emailHash, eventType, campaignID, variantID, source, metaJSON,
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("write event: %w", err)
	}

	if notify && notifyEventTypes[eventType] {
		payload := fmt.Sprintf(`{"id":%d,"email_hash":"%s","event_type":"%s","source":"%s"}`,
			id, emailHash, eventType, source)
		_, _ = ew.db.ExecContext(ctx, "SELECT pg_notify('sub_events', $1)", payload)
	}

	return nil
}

// WriteBatch inserts multiple events in a single multi-row INSERT for
// high-throughput paths like PMTA accounting file processing.
func (ew *EventWriter) WriteBatch(ctx context.Context, events []SubscriberEvent) error {
	if len(events) == 0 {
		return nil
	}

	const batchSize = 500
	for i := 0; i < len(events); i += batchSize {
		end := i + batchSize
		if end > len(events) {
			end = len(events)
		}
		if err := ew.insertBatch(ctx, events[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (ew *EventWriter) insertBatch(ctx context.Context, batch []SubscriberEvent) error {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO subscriber_events (email_hash, event_type, campaign_id, variant_id, source, metadata, event_at) VALUES `)

	args := make([]interface{}, 0, len(batch)*7)
	for i, e := range batch {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * 7
		fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7)

		metaJSON, _ := json.Marshal(e.Metadata)
		if metaJSON == nil {
			metaJSON = []byte("{}")
		}
		args = append(args, e.EmailHash, e.EventType, e.CampaignID, e.VariantID, e.Source, metaJSON, e.EventAt)
	}

	_, err := ew.db.ExecContext(ctx, sb.String(), args...)
	return err
}

// QueryByEmail returns events for a subscriber by email hash.
func (ew *EventWriter) QueryByEmail(ctx context.Context, emailHash string, limit int, offset int) ([]SubscriberEvent, error) {
	rows, err := ew.db.QueryContext(ctx,
		`SELECT id, email_hash, event_type, campaign_id, variant_id, source, metadata, event_at
		FROM subscriber_events WHERE email_hash = $1 ORDER BY event_at DESC LIMIT $2 OFFSET $3`,
		emailHash, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanEvents(rows)
}

// QueryByCampaign returns the count of events of a given type for a campaign.
func (ew *EventWriter) QueryByCampaign(ctx context.Context, campaignID uuid.UUID, eventType string) (int, error) {
	var count int
	err := ew.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriber_events WHERE campaign_id = $1 AND event_type = $2`,
		campaignID, eventType).Scan(&count)
	return count, err
}

func scanEvents(rows *sql.Rows) ([]SubscriberEvent, error) {
	var events []SubscriberEvent
	for rows.Next() {
		var e SubscriberEvent
		var metaBytes []byte
		if err := rows.Scan(&e.ID, &e.EmailHash, &e.EventType, &e.CampaignID,
			&e.VariantID, &e.Source, &metaBytes, &e.EventAt); err != nil {
			continue
		}
		if metaBytes != nil {
			_ = json.Unmarshal(metaBytes, &e.Metadata)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
