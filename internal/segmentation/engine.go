package segmentation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Engine is the main segmentation engine
type Engine struct {
	store *Store
	db    *sql.DB
}

// NewEngine creates a new segmentation engine
func NewEngine(db *sql.DB) *Engine {
	return &Engine{
		store: NewStore(db),
		db:    db,
	}
}

// Store returns the underlying store for direct access
func (e *Engine) Store() *Store {
	return e.store
}

// ==========================================
// SEGMENT EXECUTION
// ==========================================

// ExecuteSegment calculates subscribers matching a segment
func (e *Engine) ExecuteSegment(ctx context.Context, segmentID uuid.UUID) (*SegmentResult, error) {
	startTime := time.Now()

	// Get segment
	segment, err := e.store.GetSegment(ctx, uuid.Nil, segmentID)
	if err != nil {
		return nil, fmt.Errorf("get segment: %w", err)
	}
	if segment == nil {
		return nil, fmt.Errorf("segment not found")
	}

	// Get conditions
	conditions, err := e.store.GetSegmentConditions(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("get conditions: %w", err)
	}
	if conditions == nil {
		conditions = &ConditionGroupBuilder{LogicOperator: LogicAnd}
	}

	// Parse global exclusions
	var globalExclusions []ConditionBuilder
	if len(segment.GlobalExclusionRules) > 0 {
		json.Unmarshal(segment.GlobalExclusionRules, &globalExclusions)
	}

	// Build and execute query
	qb := NewQueryBuilder()
	qb.SetOrganizationID(segment.OrganizationID.String())
	if segment.ListID != nil {
		qb.SetListID(segment.ListID.String())
	}
	qb.SetIncludeSuppressed(segment.IncludeSuppressed)

	query, args, err := qb.BuildQuery(*conditions, globalExclusions)
	if err != nil {
		return nil, fmt.Errorf("build query: %w", err)
	}

	// Execute query
	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}
	defer rows.Close()

	var subscriberIDs []uuid.UUID
	for rows.Next() {
		var sub struct {
			ID uuid.UUID
		}
		// We only need the ID for the result, scan the rest into throwaway variables
		err := rows.Scan(&sub.ID, new(uuid.UUID), new(uuid.UUID), new(string), new(sql.NullString),
			new(sql.NullString), new(string), new(float64), new(int), new(int),
			new(sql.NullTime), new(sql.NullTime), new(sql.NullInt32), new(sql.NullString),
			new(time.Time), new(json.RawMessage), new([]string))
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		subscriberIDs = append(subscriberIDs, sub.ID)
	}

	// Update segment count
	if err := e.store.UpdateSegmentCount(ctx, segmentID, len(subscriberIDs)); err != nil {
		// Non-fatal error, just log
	}

	return &SegmentResult{
		SegmentID:       segmentID,
		SubscriberCount: len(subscriberIDs),
		SubscriberIDs:   subscriberIDs,
		QueryHash:       HashQuery(*conditions, globalExclusions, segment.OrganizationID.String(), ""),
		CalculatedAt:    time.Now(),
		DurationMs:      time.Since(startTime).Milliseconds(),
	}, nil
}

// PreviewSegment returns a quick preview of segment results
func (e *Engine) PreviewSegment(ctx context.Context, orgID uuid.UUID, listID *uuid.UUID, conditions ConditionGroupBuilder, exclusions []ConditionBuilder, limit int) (*SegmentPreview, error) {
	if limit <= 0 {
		limit = 10
	}

	// Build query builder
	qb := NewQueryBuilder()
	qb.SetOrganizationID(orgID.String())
	if listID != nil {
		qb.SetListID(listID.String())
	}

	// Get count first
	countQuery, countArgs, err := qb.BuildCountQuery(conditions, exclusions)
	if err != nil {
		return nil, fmt.Errorf("build count query: %w", err)
	}

	var count int
	if err := e.db.QueryRowContext(ctx, countQuery, countArgs...).Scan(&count); err != nil {
		return nil, fmt.Errorf("count query: %w", err)
	}

	// Get sample subscribers
	// Reset query builder for the main query
	qb = NewQueryBuilder()
	qb.SetOrganizationID(orgID.String())
	if listID != nil {
		qb.SetListID(listID.String())
	}

	query, args, err := qb.BuildQuery(conditions, exclusions)
	if err != nil {
		return nil, fmt.Errorf("build query: %w", err)
	}

	// Add limit
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}
	defer rows.Close()

	var samples []SubscriberPreview
	for rows.Next() {
		var sub SubscriberPreview
		var listID, orgID uuid.UUID
		var status string
		var totalOpens, totalClicks int
		var lastOpenAt, lastClickAt sql.NullTime
		var optimalHour sql.NullInt32
		var timezone sql.NullString
		var subscribedAt time.Time
		var customFields json.RawMessage
		var tags []string
		var firstName, lastName sql.NullString

		err := rows.Scan(&sub.ID, &orgID, &listID, &sub.Email, &firstName,
			&lastName, &status, &sub.EngagementScore, &totalOpens, &totalClicks,
			&lastOpenAt, &lastClickAt, &optimalHour, &timezone,
			&subscribedAt, &customFields, &tags)
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		if firstName.Valid {
			sub.FirstName = firstName.String
		}
		if lastName.Valid {
			sub.LastName = lastName.String
		}

		samples = append(samples, sub)
	}

	return &SegmentPreview{
		EstimatedCount:    count,
		SampleSubscribers: samples,
		CalculatedAt:      time.Now(),
	}, nil
}

// ==========================================
// REAL-TIME EVALUATION
// ==========================================

// EvaluateSubscriber checks if a subscriber matches a segment (for real-time triggers)
func (e *Engine) EvaluateSubscriber(ctx context.Context, subscriberID, segmentID uuid.UUID) (bool, error) {
	// Get segment and conditions
	segment, err := e.store.GetSegment(ctx, uuid.Nil, segmentID)
	if err != nil {
		return false, err
	}
	if segment == nil {
		return false, fmt.Errorf("segment not found")
	}

	conditions, err := e.store.GetSegmentConditions(ctx, segmentID)
	if err != nil {
		return false, err
	}
	if conditions == nil {
		return true, nil // No conditions = all match
	}

	var globalExclusions []ConditionBuilder
	if len(segment.GlobalExclusionRules) > 0 {
		json.Unmarshal(segment.GlobalExclusionRules, &globalExclusions)
	}

	// Build query with subscriber ID filter
	qb := NewQueryBuilder()
	qb.SetOrganizationID(segment.OrganizationID.String())
	if segment.ListID != nil {
		qb.SetListID(segment.ListID.String())
	}
	qb.SetIncludeSuppressed(segment.IncludeSuppressed)

	// Build query and check if subscriber exists in the result
	fullQuery, fullArgs, err := qb.BuildQuery(*conditions, globalExclusions)
	if err != nil {
		return false, err
	}

	checkQuery := fmt.Sprintf(`
		SELECT EXISTS (
			SELECT 1 FROM (%s) matched WHERE matched.id = $%d
		)
	`, fullQuery, len(fullArgs)+1)

	fullArgs = append(fullArgs, subscriberID)

	var matches bool
	if err := e.db.QueryRowContext(ctx, checkQuery, fullArgs...).Scan(&matches); err != nil {
		return false, err
	}

	return matches, nil
}

// ==========================================
// SNAPSHOTS
// ==========================================

// CreateSegmentSnapshot creates a point-in-time snapshot of a segment
func (e *Engine) CreateSegmentSnapshot(ctx context.Context, segmentID uuid.UUID, purpose string, createdBy *uuid.UUID) (*SegmentSnapshot, error) {
	// Execute segment to get current results
	result, err := e.ExecuteSegment(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("execute segment: %w", err)
	}

	// Get segment for metadata
	segment, err := e.store.GetSegment(ctx, uuid.Nil, segmentID)
	if err != nil {
		return nil, fmt.Errorf("get segment: %w", err)
	}

	// Get conditions for snapshot
	conditions, err := e.store.GetSegmentConditions(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("get conditions: %w", err)
	}

	conditionsJSON, _ := json.Marshal(conditions)

	snapshot := &SegmentSnapshot{
		SegmentID:          segmentID,
		OrganizationID:     segment.OrganizationID,
		Name:               fmt.Sprintf("%s - Snapshot %s", segment.Name, time.Now().Format("2006-01-02 15:04")),
		ConditionsSnapshot: conditionsJSON,
		SubscriberCount:    result.SubscriberCount,
		QueryHash:          result.QueryHash,
		Purpose:            purpose,
		CreatedBy:          createdBy,
	}

	// For small segments, store subscriber IDs directly
	if result.SubscriberCount <= 10000 {
		snapshot.SubscriberIDs = result.SubscriberIDs
	}

	if err := e.store.CreateSnapshot(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	return snapshot, nil
}

// GetSnapshotSubscribers retrieves subscribers from a snapshot
func (e *Engine) GetSnapshotSubscribers(ctx context.Context, snapshotID uuid.UUID) ([]uuid.UUID, error) {
	snapshot, err := e.store.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, fmt.Errorf("snapshot not found")
	}

	// If we have stored IDs, return them
	if len(snapshot.SubscriberIDs) > 0 {
		return snapshot.SubscriberIDs, nil
	}

	// Otherwise, re-execute with the frozen conditions
	var conditions ConditionGroupBuilder
	if err := json.Unmarshal(snapshot.ConditionsSnapshot, &conditions); err != nil {
		return nil, fmt.Errorf("unmarshal conditions: %w", err)
	}

	qb := NewQueryBuilder()
	qb.SetOrganizationID(snapshot.OrganizationID.String())

	query, args, err := qb.BuildQuery(conditions, nil)
	if err != nil {
		return nil, fmt.Errorf("build query: %w", err)
	}

	rows, err := e.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id, new(uuid.UUID), new(uuid.UUID), new(string), new(sql.NullString),
			new(sql.NullString), new(string), new(float64), new(int), new(int),
			new(sql.NullTime), new(sql.NullTime), new(sql.NullInt32), new(sql.NullString),
			new(time.Time), new(json.RawMessage), new([]string)); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	return ids, nil
}

// ==========================================
// EVENTS
// ==========================================

// TrackEvent records a custom event and triggers real-time segment evaluation
func (e *Engine) TrackEvent(ctx context.Context, event *CustomEvent) error {
	// Record the event
	if err := e.store.RecordCustomEvent(ctx, event); err != nil {
		return fmt.Errorf("record event: %w", err)
	}

	// Update computed fields for this subscriber
	if err := e.store.UpdateComputedFields(ctx, event.SubscriberID); err != nil {
		// Non-fatal, just log
	}

	// TODO: Trigger real-time segment evaluation for automation triggers
	// This would notify any listening automation workflows

	return nil
}

// ==========================================
// HELPERS
// ==========================================

// ValidateConditions validates a condition tree
func (e *Engine) ValidateConditions(conditions ConditionGroupBuilder) []string {
	var errors []string

	for _, cond := range conditions.Conditions {
		// Validate operator is appropriate for field type
		meta := getOperatorMeta(cond.Operator)
		if meta == nil {
			errors = append(errors, fmt.Sprintf("unknown operator: %s", cond.Operator))
			continue
		}

		// Check if value is required but missing
		if meta.RequiresValue && cond.Value == "" {
			errors = append(errors, fmt.Sprintf("operator %s requires a value for field %s", cond.Operator, cond.Field))
		}

		// Check if secondary value is required but missing
		if meta.RequiresSecondary && cond.ValueSecondary == "" {
			errors = append(errors, fmt.Sprintf("operator %s requires a secondary value for field %s", cond.Operator, cond.Field))
		}

		// Check if array is required but missing
		if meta.RequiresArray && len(cond.ValuesArray) == 0 {
			errors = append(errors, fmt.Sprintf("operator %s requires an array of values for field %s", cond.Operator, cond.Field))
		}

		// Check event conditions
		if cond.ConditionType == ConditionEvent && cond.EventName == "" {
			errors = append(errors, "event conditions require an event name")
		}
	}

	// Recursively validate child groups
	for _, group := range conditions.Groups {
		childErrors := e.ValidateConditions(group)
		errors = append(errors, childErrors...)
	}

	return errors
}

func getOperatorMeta(op Operator) *OperatorMetadata {
	for _, meta := range GetOperatorMetadata() {
		if meta.Operator == op {
			return &meta
		}
	}
	return nil
}

// GetAvailableOperators returns operators available for a field type
func GetAvailableOperators(fieldType FieldType) []OperatorMetadata {
	var operators []OperatorMetadata
	for _, meta := range GetOperatorMetadata() {
		for _, ft := range meta.ApplicableTypes {
			if ft == fieldType {
				operators = append(operators, meta)
				break
			}
		}
	}
	return operators
}
