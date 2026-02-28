package segmentation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Store provides database operations for segmentation
type Store struct {
	db *sql.DB
}

// NewStore creates a new segmentation store
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// ==========================================
// SEGMENT OPERATIONS
// ==========================================

// CreateSegment creates a new segment with conditions
func (s *Store) CreateSegment(ctx context.Context, segment *Segment, rootGroup *ConditionGroupBuilder) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Create segment
	segment.ID = uuid.New()
	segment.CreatedAt = time.Now()
	segment.UpdatedAt = time.Now()
	if segment.Status == "" {
		segment.Status = "active"
	}
	if segment.SegmentType == "" {
		segment.SegmentType = "dynamic"
	}
	if segment.CalculationMode == "" {
		segment.CalculationMode = "batch"
	}

	// Marshal conditions for storage
	conditionsJSON, err := json.Marshal(rootGroup)
	if err != nil {
		return fmt.Errorf("marshal conditions: %w", err)
	}

	query := `
		INSERT INTO mailing_segments (
			id, organization_id, list_id, name, description, segment_type,
			conditions, calculation_mode, refresh_interval_minutes, include_suppressed,
			global_exclusion_rules, status, created_by, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`

	_, err = tx.ExecContext(ctx, query,
		segment.ID, segment.OrganizationID, segment.ListID, segment.Name, segment.Description,
		segment.SegmentType, conditionsJSON, segment.CalculationMode, segment.RefreshIntervalMin,
		segment.IncludeSuppressed, segment.GlobalExclusionRules, segment.Status,
		segment.CreatedBy, segment.CreatedAt, segment.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert segment: %w", err)
	}

	// Create condition groups and conditions
	if rootGroup != nil {
		if err := s.createConditionGroup(ctx, tx, segment.ID, nil, rootGroup, 0); err != nil {
			return fmt.Errorf("create condition groups: %w", err)
		}
	}

	return tx.Commit()
}

// createConditionGroup recursively creates condition groups and conditions
func (s *Store) createConditionGroup(ctx context.Context, tx *sql.Tx, segmentID uuid.UUID, parentGroupID *uuid.UUID, group *ConditionGroupBuilder, sortOrder int) error {
	groupID := uuid.New()

	// Insert group
	groupQuery := `
		INSERT INTO mailing_segment_condition_groups (
			id, segment_id, parent_group_id, logic_operator, is_negated, sort_order, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	_, err := tx.ExecContext(ctx, groupQuery, groupID, segmentID, parentGroupID,
		group.LogicOperator, group.IsNegated, sortOrder, time.Now())
	if err != nil {
		return fmt.Errorf("insert condition group: %w", err)
	}

	// Insert conditions for this group
	for i, cond := range group.Conditions {
		condID := uuid.New()
		valuesJSON, _ := json.Marshal(cond.ValuesArray)

		condQuery := `
			INSERT INTO mailing_segment_conditions (
				id, segment_id, group_id, condition_type, field, field_type, operator,
				value, value_secondary, values_array, event_name, event_time_window_days,
				event_min_count, event_max_count, event_property_path, sort_order, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		`
		_, err := tx.ExecContext(ctx, condQuery,
			condID, segmentID, groupID, cond.ConditionType, cond.Field, cond.FieldType,
			cond.Operator, cond.Value, cond.ValueSecondary, valuesJSON, cond.EventName,
			cond.EventTimeWindowDays, cond.EventMinCount, cond.EventMaxCount,
			cond.EventPropertyPath, i, time.Now())
		if err != nil {
			return fmt.Errorf("insert condition: %w", err)
		}
	}

	// Recursively create child groups
	for i, childGroup := range group.Groups {
		if err := s.createConditionGroup(ctx, tx, segmentID, &groupID, &childGroup, i); err != nil {
			return err
		}
	}

	return nil
}

// GetSegment retrieves a segment by ID
func (s *Store) GetSegment(ctx context.Context, orgID, segmentID uuid.UUID) (*Segment, error) {
	query := `
		SELECT id, organization_id, list_id, name, description, segment_type, conditions,
			calculation_mode, refresh_interval_minutes, include_suppressed,
			global_exclusion_rules, subscriber_count, last_calculated_at, status,
			created_by, last_edited_by, last_edited_at, created_at, updated_at
		FROM mailing_segments
		WHERE id = $1 AND organization_id = $2
	`

	segment := &Segment{}
	var conditions json.RawMessage
	err := s.db.QueryRowContext(ctx, query, segmentID, orgID).Scan(
		&segment.ID, &segment.OrganizationID, &segment.ListID, &segment.Name, &segment.Description,
		&segment.SegmentType, &conditions, &segment.CalculationMode, &segment.RefreshIntervalMin,
		&segment.IncludeSuppressed, &segment.GlobalExclusionRules, &segment.SubscriberCount,
		&segment.LastCalculatedAt, &segment.Status, &segment.CreatedBy, &segment.LastEditedBy,
		&segment.LastEditedAt, &segment.CreatedAt, &segment.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return segment, nil
}

// GetSegmentConditions retrieves the condition tree for a segment
func (s *Store) GetSegmentConditions(ctx context.Context, segmentID uuid.UUID) (*ConditionGroupBuilder, error) {
	// Get root group (no parent)
	groupQuery := `
		SELECT id, logic_operator, is_negated, sort_order
		FROM mailing_segment_condition_groups
		WHERE segment_id = $1 AND parent_group_id IS NULL
		ORDER BY sort_order
		LIMIT 1
	`
	
	var rootGroupID uuid.UUID
	var rootGroup ConditionGroupBuilder
	err := s.db.QueryRowContext(ctx, groupQuery, segmentID).Scan(
		&rootGroupID, &rootGroup.LogicOperator, &rootGroup.IsNegated, new(int))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Load conditions for this group
	conditions, err := s.loadGroupConditions(ctx, segmentID, rootGroupID)
	if err != nil {
		return nil, err
	}
	rootGroup.Conditions = conditions

	// Load child groups recursively
	childGroups, err := s.loadChildGroups(ctx, segmentID, rootGroupID)
	if err != nil {
		return nil, err
	}
	rootGroup.Groups = childGroups

	return &rootGroup, nil
}

// loadGroupConditions loads conditions for a specific group
func (s *Store) loadGroupConditions(ctx context.Context, segmentID, groupID uuid.UUID) ([]ConditionBuilder, error) {
	query := `
		SELECT condition_type, field, field_type, operator, value, value_secondary,
			values_array, event_name, event_time_window_days, event_min_count,
			event_max_count, event_property_path
		FROM mailing_segment_conditions
		WHERE segment_id = $1 AND group_id = $2
		ORDER BY sort_order
	`

	rows, err := s.db.QueryContext(ctx, query, segmentID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conditions []ConditionBuilder
	for rows.Next() {
		var cond ConditionBuilder
		var valuesJSON []byte
		var fieldType sql.NullString

		err := rows.Scan(
			&cond.ConditionType, &cond.Field, &fieldType, &cond.Operator,
			&cond.Value, &cond.ValueSecondary, &valuesJSON, &cond.EventName,
			&cond.EventTimeWindowDays, &cond.EventMinCount, &cond.EventMaxCount,
			&cond.EventPropertyPath)
		if err != nil {
			return nil, err
		}

		if fieldType.Valid {
			cond.FieldType = FieldType(fieldType.String)
		}

		if len(valuesJSON) > 0 {
			json.Unmarshal(valuesJSON, &cond.ValuesArray)
		}

		conditions = append(conditions, cond)
	}

	return conditions, nil
}

// loadChildGroups recursively loads child groups
func (s *Store) loadChildGroups(ctx context.Context, segmentID, parentGroupID uuid.UUID) ([]ConditionGroupBuilder, error) {
	query := `
		SELECT id, logic_operator, is_negated, sort_order
		FROM mailing_segment_condition_groups
		WHERE segment_id = $1 AND parent_group_id = $2
		ORDER BY sort_order
	`

	rows, err := s.db.QueryContext(ctx, query, segmentID, parentGroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []ConditionGroupBuilder
	for rows.Next() {
		var groupID uuid.UUID
		var group ConditionGroupBuilder
		var sortOrder int

		err := rows.Scan(&groupID, &group.LogicOperator, &group.IsNegated, &sortOrder)
		if err != nil {
			return nil, err
		}

		// Load conditions for this group
		conditions, err := s.loadGroupConditions(ctx, segmentID, groupID)
		if err != nil {
			return nil, err
		}
		group.Conditions = conditions

		// Recursively load child groups
		childGroups, err := s.loadChildGroups(ctx, segmentID, groupID)
		if err != nil {
			return nil, err
		}
		group.Groups = childGroups

		groups = append(groups, group)
	}

	return groups, nil
}

// UpdateSegmentCount updates the subscriber count for a segment
func (s *Store) UpdateSegmentCount(ctx context.Context, segmentID uuid.UUID, count int) error {
	query := `
		UPDATE mailing_segments
		SET subscriber_count = $1, last_calculated_at = NOW(), updated_at = NOW()
		WHERE id = $2
	`
	_, err := s.db.ExecContext(ctx, query, count, segmentID)
	return err
}

// ListSegments lists all segments for an organization
func (s *Store) ListSegments(ctx context.Context, orgID uuid.UUID, listID *uuid.UUID) ([]*Segment, error) {
	query := `
		SELECT id, organization_id, list_id, name, description, segment_type,
			subscriber_count, last_calculated_at, status, created_at, updated_at
		FROM mailing_segments
		WHERE organization_id = $1 AND status != 'deleted'
	`
	args := []interface{}{orgID}

	if listID != nil {
		query += " AND list_id = $2"
		args = append(args, listID)
	}

	query += " ORDER BY name"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var segments []*Segment
	for rows.Next() {
		seg := &Segment{}
		err := rows.Scan(&seg.ID, &seg.OrganizationID, &seg.ListID, &seg.Name,
			&seg.Description, &seg.SegmentType, &seg.SubscriberCount,
			&seg.LastCalculatedAt, &seg.Status, &seg.CreatedAt, &seg.UpdatedAt)
		if err != nil {
			return nil, err
		}
		segments = append(segments, seg)
	}

	return segments, nil
}

// DeleteSegment soft-deletes a segment
func (s *Store) DeleteSegment(ctx context.Context, orgID, segmentID uuid.UUID) error {
	query := `
		UPDATE mailing_segments
		SET status = 'deleted', updated_at = NOW()
		WHERE id = $1 AND organization_id = $2
	`
	_, err := s.db.ExecContext(ctx, query, segmentID, orgID)
	return err
}

// ==========================================
// SNAPSHOT OPERATIONS
// ==========================================

// CreateSnapshot creates a segment snapshot
func (s *Store) CreateSnapshot(ctx context.Context, snapshot *SegmentSnapshot) error {
	snapshot.ID = uuid.New()
	snapshot.SnapshotAt = time.Now()
	snapshot.CreatedAt = time.Now()

	subscriberIDsJSON, _ := json.Marshal(snapshot.SubscriberIDs)

	query := `
		INSERT INTO mailing_segment_snapshots (
			id, segment_id, organization_id, name, description, conditions_snapshot,
			subscriber_count, subscriber_ids, query_hash, purpose, campaign_id,
			created_by, snapshot_at, expires_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`

	_, err := s.db.ExecContext(ctx, query,
		snapshot.ID, snapshot.SegmentID, snapshot.OrganizationID, snapshot.Name,
		snapshot.Description, snapshot.ConditionsSnapshot, snapshot.SubscriberCount,
		subscriberIDsJSON, snapshot.QueryHash, snapshot.Purpose, snapshot.CampaignID,
		snapshot.CreatedBy, snapshot.SnapshotAt, snapshot.ExpiresAt, snapshot.CreatedAt)

	return err
}

// GetSnapshot retrieves a snapshot by ID
func (s *Store) GetSnapshot(ctx context.Context, snapshotID uuid.UUID) (*SegmentSnapshot, error) {
	query := `
		SELECT id, segment_id, organization_id, name, description, conditions_snapshot,
			subscriber_count, subscriber_ids, query_hash, purpose, campaign_id,
			created_by, snapshot_at, expires_at, created_at
		FROM mailing_segment_snapshots
		WHERE id = $1
	`

	snapshot := &SegmentSnapshot{}
	var subscriberIDsJSON []byte
	err := s.db.QueryRowContext(ctx, query, snapshotID).Scan(
		&snapshot.ID, &snapshot.SegmentID, &snapshot.OrganizationID, &snapshot.Name,
		&snapshot.Description, &snapshot.ConditionsSnapshot, &snapshot.SubscriberCount,
		&subscriberIDsJSON, &snapshot.QueryHash, &snapshot.Purpose, &snapshot.CampaignID,
		&snapshot.CreatedBy, &snapshot.SnapshotAt, &snapshot.ExpiresAt, &snapshot.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if len(subscriberIDsJSON) > 0 {
		json.Unmarshal(subscriberIDsJSON, &snapshot.SubscriberIDs)
	}

	return snapshot, nil
}

// ==========================================
// CUSTOM EVENTS
// ==========================================

// RecordCustomEvent records a custom behavioral event
func (s *Store) RecordCustomEvent(ctx context.Context, event *CustomEvent) error {
	event.ID = uuid.New()
	if event.EventAt.IsZero() {
		event.EventAt = time.Now()
	}
	event.CreatedAt = time.Now()

	query := `
		INSERT INTO mailing_custom_events (
			id, organization_id, subscriber_id, event_name, event_category, properties,
			source, ip_address, user_agent, device_type, geo_country, geo_region,
			geo_city, session_id, event_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	`

	_, err := s.db.ExecContext(ctx, query,
		event.ID, event.OrganizationID, event.SubscriberID, event.EventName,
		event.EventCategory, event.Properties, event.Source, event.IPAddress,
		event.UserAgent, event.DeviceType, event.GeoCountry, event.GeoRegion,
		event.GeoCity, event.SessionID, event.EventAt, event.CreatedAt)

	return err
}

// ==========================================
// COMPUTED FIELDS
// ==========================================

// UpdateComputedFields updates computed fields for a subscriber
func (s *Store) UpdateComputedFields(ctx context.Context, subscriberID uuid.UUID) error {
	// This calls the database function we created in the migration
	_, err := s.db.ExecContext(ctx, "SELECT update_subscriber_computed($1)", subscriberID)
	return err
}

// GetComputedFields retrieves computed fields for a subscriber
func (s *Store) GetComputedFields(ctx context.Context, subscriberID uuid.UUID) (*ComputedFields, error) {
	query := `
		SELECT subscriber_id, organization_id, total_purchases, total_revenue,
			average_order_value, first_email_at, last_active_at, last_purchase_at,
			last_login_at, opens_7d, opens_30d, opens_90d, clicks_7d, clicks_30d,
			clicks_90d, engagement_velocity, propensity_to_buy, next_purchase_days,
			calculated_at
		FROM mailing_subscriber_computed
		WHERE subscriber_id = $1
	`

	cf := &ComputedFields{}
	err := s.db.QueryRowContext(ctx, query, subscriberID).Scan(
		&cf.SubscriberID, &cf.OrganizationID, &cf.TotalPurchases, &cf.TotalRevenue,
		&cf.AverageOrderValue, &cf.FirstEmailAt, &cf.LastActiveAt, &cf.LastPurchaseAt,
		&cf.LastLoginAt, &cf.Opens7d, &cf.Opens30d, &cf.Opens90d, &cf.Clicks7d,
		&cf.Clicks30d, &cf.Clicks90d, &cf.EngagementVelocity, &cf.PropensityToBuy,
		&cf.NextPurchaseDays, &cf.CalculatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return cf, nil
}

// BatchUpdateComputedFields updates computed fields for multiple subscribers
func (s *Store) BatchUpdateComputedFields(ctx context.Context, orgID uuid.UUID, limit int) (int, error) {
	// Find subscribers that need updating (no computed record or stale)
	query := `
		WITH to_update AS (
			SELECT s.id
			FROM mailing_subscribers s
			LEFT JOIN mailing_subscriber_computed c ON s.id = c.subscriber_id
			WHERE s.organization_id = $1
			AND s.status = 'confirmed'
			AND (c.subscriber_id IS NULL OR c.calculated_at < NOW() - INTERVAL '1 hour')
			LIMIT $2
		)
		SELECT update_subscriber_computed(id) FROM to_update
	`

	result, err := s.db.ExecContext(ctx, query, orgID, limit)
	if err != nil {
		return 0, err
	}

	count, _ := result.RowsAffected()
	return int(count), nil
}

// ==========================================
// CONSENT & SUPPRESSION
// ==========================================

// AddToSuppressionList adds an email to the suppression list
func (s *Store) AddToSuppressionList(ctx context.Context, record *SuppressionRecord) error {
	record.ID = uuid.New()
	record.SuppressedAt = time.Now()
	record.CreatedAt = time.Now()

	query := `
		INSERT INTO mailing_suppression_list (
			id, organization_id, email, email_hash, reason, scope, notes,
			source, original_list_id, suppressed_at, expires_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (organization_id, email_hash, scope) DO UPDATE SET
			reason = EXCLUDED.reason,
			notes = EXCLUDED.notes,
			suppressed_at = EXCLUDED.suppressed_at
	`

	_, err := s.db.ExecContext(ctx, query,
		record.ID, record.OrganizationID, record.Email, record.EmailHash,
		record.Reason, record.Scope, record.Notes, record.Source,
		record.OriginalListID, record.SuppressedAt, record.ExpiresAt, record.CreatedAt)

	return err
}

// IsEmailSuppressed checks if an email is suppressed
func (s *Store) IsEmailSuppressed(ctx context.Context, orgID uuid.UUID, emailHash string) (bool, error) {
	query := `
		SELECT EXISTS (
			SELECT 1 FROM mailing_suppression_list
			WHERE organization_id = $1 AND email_hash = $2
			AND (expires_at IS NULL OR expires_at > NOW())
		)
	`

	var suppressed bool
	err := s.db.QueryRowContext(ctx, query, orgID, emailHash).Scan(&suppressed)
	return suppressed, err
}

// RecordConsent records a consent action
func (s *Store) RecordConsent(ctx context.Context, record *ConsentRecord) error {
	record.ID = uuid.New()
	record.ConsentedAt = time.Now()
	record.CreatedAt = time.Now()

	query := `
		INSERT INTO mailing_consent_records (
			id, organization_id, subscriber_id, email, email_hash, consent_type,
			status, legal_basis, consent_text, consent_version, source,
			ip_address, user_agent, consented_at, expires_at, withdrawn_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
	`

	_, err := s.db.ExecContext(ctx, query,
		record.ID, record.OrganizationID, record.SubscriberID, record.Email,
		record.EmailHash, record.ConsentType, record.Status, record.LegalBasis,
		record.ConsentText, record.ConsentVersion, record.Source, record.IPAddress,
		record.UserAgent, record.ConsentedAt, record.ExpiresAt, record.WithdrawnAt,
		record.CreatedAt)

	return err
}

// ==========================================
// CONTACT FIELDS (Schema Registry)
// ==========================================

// GetContactFields retrieves all contact field definitions
func (s *Store) GetContactFields(ctx context.Context, orgID uuid.UUID) ([]*ContactField, error) {
	query := `
		SELECT id, organization_id, field_key, field_label, field_type,
			description, category, is_system, is_pii, is_required,
			validation_rules, default_value, allowed_values, display_order,
			is_visible, created_at, updated_at
		FROM mailing_contact_fields
		WHERE organization_id = $1
		ORDER BY is_system DESC, display_order, field_label
	`

	rows, err := s.db.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var fields []*ContactField
	for rows.Next() {
		field := &ContactField{}
		var allowedValuesJSON []byte
		err := rows.Scan(
			&field.ID, &field.OrganizationID, &field.FieldKey, &field.FieldLabel,
			&field.FieldType, &field.Description, &field.Category, &field.IsSystem,
			&field.IsPII, &field.IsRequired, &field.ValidationRules, &field.DefaultValue,
			&allowedValuesJSON, &field.DisplayOrder, &field.IsVisible,
			&field.CreatedAt, &field.UpdatedAt)
		if err != nil {
			return nil, err
		}

		if len(allowedValuesJSON) > 0 {
			json.Unmarshal(allowedValuesJSON, &field.AllowedValues)
		}

		fields = append(fields, field)
	}

	return fields, nil
}

// CreateContactField creates a new custom field definition
func (s *Store) CreateContactField(ctx context.Context, field *ContactField) error {
	field.ID = uuid.New()
	field.CreatedAt = time.Now()
	field.UpdatedAt = time.Now()

	allowedValuesJSON, _ := json.Marshal(field.AllowedValues)

	query := `
		INSERT INTO mailing_contact_fields (
			id, organization_id, field_key, field_label, field_type, description,
			category, is_system, is_pii, is_required, validation_rules,
			default_value, allowed_values, display_order, is_visible,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
	`

	_, err := s.db.ExecContext(ctx, query,
		field.ID, field.OrganizationID, field.FieldKey, field.FieldLabel,
		field.FieldType, field.Description, field.Category, field.IsSystem,
		field.IsPII, field.IsRequired, field.ValidationRules, field.DefaultValue,
		allowedValuesJSON, field.DisplayOrder, field.IsVisible,
		field.CreatedAt, field.UpdatedAt)

	return err
}
