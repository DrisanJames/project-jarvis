package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (s *AdvancedMailingService) HandleGetSegments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgIDFromRequest(r)

	empty := map[string]interface{}{"segments": []map[string]interface{}{}}
	if orgID == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(empty)
		return
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.description, s.segment_type, s.subscriber_count, s.status, s.created_at,
		       s.list_id, COALESCE(l.name, ''), COALESCE(s.conditions::text, '[]')
		FROM mailing_segments s
		LEFT JOIN mailing_lists l ON l.id = s.list_id
		WHERE s.organization_id = $1 ORDER BY s.created_at DESC
	`, orgID)
	if err != nil {
		log.Printf("[HandleGetSegments] query error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(empty)
		return
	}
	defer rows.Close()

	var segments []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var listID *uuid.UUID
		var name, desc, segType, status, listName, conditionsRaw string
		var subCount int
		var createdAt time.Time
		rows.Scan(&id, &name, &desc, &segType, &subCount, &status, &createdAt, &listID, &listName, &conditionsRaw)

		seg := map[string]interface{}{
			"id": id.String(), "name": name, "description": desc, "segment_type": segType,
			"subscriber_count": subCount, "status": status, "created_at": createdAt,
			"list_name": listName,
		}
		if listID != nil {
			seg["list_id"] = listID.String()
		}
		var conditions []interface{}
		if err := json.Unmarshal([]byte(conditionsRaw), &conditions); err == nil && len(conditions) > 0 {
			seg["conditions"] = conditions
		}
		segments = append(segments, seg)
	}
	if segments == nil {
		segments = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"segments": segments})
}

func (s *AdvancedMailingService) HandleCreateSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ListID      string `json:"list_id"`
		SegmentType string `json:"segment_type"`
		Conditions  []struct {
			Group    int    `json:"group"`
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    string `json:"value"`
		} `json:"conditions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		log.Printf("Error decoding request body: %v", err)
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	
	if input.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	
	segmentID := uuid.New()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	
	// Handle optional list_id - use NULL if not provided
	var listID interface{}
	if input.ListID != "" {
		parsedListID, err := uuid.Parse(input.ListID)
		if err != nil {
			http.Error(w, `{"error":"invalid list_id"}`, http.StatusBadRequest)
			return
		}
		listID = parsedListID
	} else {
		listID = nil
	}
	
	// Default segment type to dynamic if not specified
	segmentType := input.SegmentType
	if segmentType == "" {
		segmentType = "dynamic"
	}
	
	// Build conditions JSONB for storage in the main table
	conditionsJSON := buildConditionsJSON(input.Conditions)
	
	log.Printf("Creating segment: id=%s, org=%s, list=%v, name=%s, type=%s, conditions=%s",
		segmentID, orgID, listID, input.Name, segmentType, conditionsJSON)
	
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_segments (id, organization_id, list_id, name, description, segment_type, conditions, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', NOW(), NOW())
	`, segmentID, orgID, listID, input.Name, input.Description, segmentType, conditionsJSON)
	
	if err != nil {
		log.Printf("ERROR creating segment - SQL error: %v", err)
		log.Printf("  - segmentID: %s", segmentID)
		log.Printf("  - orgID: %s", orgID)
		log.Printf("  - listID: %v (type: %T)", listID, listID)
		log.Printf("  - name: %s", input.Name)
		log.Printf("  - description: %s", input.Description)
		log.Printf("  - segmentType: %s", segmentType)
		log.Printf("  - conditionsJSON: %s", conditionsJSON)
		http.Error(w, `{"error":"Failed to create segment"}`, http.StatusInternalServerError)
		return
	}
	
	log.Printf("Successfully created segment: %s", segmentID)
	
	// Also add to segment_conditions table for compatibility (with valid operators only)
	if segmentType == "dynamic" && len(input.Conditions) > 0 {
		for i, c := range input.Conditions {
			// Map operators to database-allowed values
			operator := mapOperatorForDB(c.Operator)
			if operator == "" {
				continue // Skip operators not supported by the conditions table
			}
			
			_, condErr := s.db.ExecContext(ctx, `
				INSERT INTO mailing_segment_conditions (id, segment_id, condition_group, field, operator, value)
				VALUES ($1, $2, $3, $4, $5, $6)
			`, uuid.New(), segmentID, i, c.Field, operator, c.Value)
			if condErr != nil {
				log.Printf("Error adding condition to table: %v", condErr)
				// Don't fail the whole request - conditions are also stored in JSONB
			}
		}
	}
	
	// Calculate the actual subscriber count for this segment
	subscriberCount := s.calculateSegmentCount(ctx, segmentID, listID, input.Conditions)
	
	// Update the segment with the calculated count
	s.db.ExecContext(ctx, `UPDATE mailing_segments SET subscriber_count = $2, updated_at = NOW() WHERE id = $1`, segmentID, subscriberCount)
	
	log.Printf("Segment %s created with %d subscribers", segmentID, subscriberCount)
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":               segmentID.String(),
		"name":             input.Name,
		"segment_type":     segmentType,
		"status":           "active",
		"subscriber_count": subscriberCount,
	})
}

// buildConditionsJSON converts input conditions to JSONB format for storage
func buildConditionsJSON(conditions []struct {
	Group    int    `json:"group"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}) string {
	if len(conditions) == 0 {
		return "[]"
	}
	
	// Build a structured conditions array
	result := make([]map[string]interface{}, len(conditions))
	for i, c := range conditions {
		result[i] = map[string]interface{}{
			"group":    c.Group,
			"field":    c.Field,
			"operator": c.Operator,
			"value":    c.Value,
		}
	}
	
	jsonBytes, err := json.Marshal(result)
	if err != nil {
		return "[]"
	}
	return string(jsonBytes)
}

// mapOperatorForDB maps frontend operator names to database-allowed values
// Returns empty string if the operator isn't supported by the conditions table CHECK constraint
func mapOperatorForDB(op string) string {
	// These are the operators allowed by the CHECK constraint:
	// ('equals', 'not_equals', 'contains', 'not_contains', 'starts_with', 'ends_with', 'gt', 'lt', 'gte', 'lte', 'is_null', 'is_not_null', 'in', 'not_in')
	mapping := map[string]string{
		"equals":                 "equals",
		"not_equals":             "not_equals",
		"contains":               "contains",
		"not_contains":           "not_contains",
		"starts_with":            "starts_with",
		"ends_with":              "ends_with",
		"greater_than":           "gt",
		"less_than":              "lt",
		"greater_than_or_equal":  "gte",
		"less_than_or_equal":     "lte",
		"gt":                     "gt",
		"lt":                     "lt",
		"gte":                    "gte",
		"lte":                    "lte",
		"is_null":                "is_null",
		"is_not_null":            "is_not_null",
		"is_empty":               "is_null",      // Map to is_null
		"is_not_empty":           "is_not_null",  // Map to is_not_null
		"in":                     "in",
		"not_in":                 "not_in",
	}
	if mapped, ok := mapping[op]; ok {
		return mapped
	}
	// Operators like 'in_last_days', 'more_than_days_ago' aren't in the constraint
	// They're stored in JSONB but skipped for the conditions table
	return ""
}

// SegmentConditionInput is the input shape for segment conditions from JSON.
type SegmentConditionInput struct {
	Group    int    `json:"group"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

// trackingEventTypeMap maps frontend event field names to DB event_type values.
var trackingEventTypeMap = map[string]string{
	"email_sent":         "sent",
	"email_opened":       "opened",
	"email_clicked":      "clicked",
	"email_bounced":      "bounced",
	"email_delivered":    "delivered",
	"email_unsubscribed": "unsubscribed",
	"email_complained":   "complained",
}

// isEventField returns true if the field maps to mailing_tracking_events.
func isEventField(field string) bool {
	_, ok := trackingEventTypeMap[field]
	return ok
}

// BuildSegmentWhereClause builds a SQL WHERE clause from segment conditions.
// Returns the clause string (without "WHERE") and positional args.
// Callers can prefix their own SELECT and append this.
func BuildSegmentWhereClause(listID interface{}, conditions []SegmentConditionInput) (string, []interface{}) {
	whereClauses := []string{"status IN ('active','confirmed')"}
	args := []interface{}{}
	argNum := 1

	if listID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("list_id = $%d", argNum))
		args = append(args, listID)
		argNum++
	}

	for _, c := range conditions {
		if isEventField(c.Field) {
			clause, newArgs, newArgNum := buildEventWhereClause(c, argNum)
			if clause != "" {
				whereClauses = append(whereClauses, clause)
				args = append(args, newArgs...)
				argNum = newArgNum
			}
			continue
		}

		dbField := mapFieldToColumn(c.Field)
		var clause string
		switch c.Operator {
		case "equals", "is":
			clause = fmt.Sprintf("%s = $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "not_equals", "is_not":
			clause = fmt.Sprintf("%s != $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "contains":
			clause = fmt.Sprintf("%s ILIKE $%d", dbField, argNum)
			args = append(args, "%"+c.Value+"%")
			argNum++
		case "not_contains":
			clause = fmt.Sprintf("%s NOT ILIKE $%d", dbField, argNum)
			args = append(args, "%"+c.Value+"%")
			argNum++
		case "starts_with":
			clause = fmt.Sprintf("%s ILIKE $%d", dbField, argNum)
			args = append(args, c.Value+"%")
			argNum++
		case "ends_with":
			clause = fmt.Sprintf("%s ILIKE $%d", dbField, argNum)
			args = append(args, "%"+c.Value)
			argNum++
		case "greater_than", "gt":
			clause = fmt.Sprintf("%s > $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "less_than", "lt":
			clause = fmt.Sprintf("%s < $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "greater_than_or_equal", "gte":
			clause = fmt.Sprintf("%s >= $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "less_than_or_equal", "lte":
			clause = fmt.Sprintf("%s <= $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "is_empty", "is_null":
			clause = fmt.Sprintf("(%s IS NULL OR %s = '')", dbField, dbField)
		case "is_not_empty", "is_not_null":
			clause = fmt.Sprintf("(%s IS NOT NULL AND %s != '')", dbField, dbField)
		case "in_last_days":
			clause = fmt.Sprintf("%s >= NOW() - INTERVAL '%s days'", dbField, c.Value)
		case "more_than_days_ago":
			clause = fmt.Sprintf("%s < NOW() - INTERVAL '%s days'", dbField, c.Value)
		default:
			continue
		}
		if clause != "" {
			whereClauses = append(whereClauses, clause)
		}
	}

	return strings.Join(whereClauses, " AND "), args
}

// buildEventWhereClause generates an EXISTS/NOT EXISTS subquery against
// mailing_tracking_events for event-based segment conditions.
func buildEventWhereClause(c SegmentConditionInput, argNum int) (string, []interface{}, int) {
	eventType := trackingEventTypeMap[c.Field]
	var args []interface{}

	switch c.Operator {
	case "equals", "is":
		// "has this event" → EXISTS
		args = append(args, eventType)
		clause := fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
		)`, argNum)
		return clause, args, argNum + 1

	case "not_equals", "is_not":
		// "does not have this event" → NOT EXISTS
		args = append(args, eventType)
		clause := fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
		)`, argNum)
		return clause, args, argNum + 1

	case "in_last_days":
		// "event occurred in last N days" → EXISTS with time filter
		args = append(args, eventType)
		clause := fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
			  AND e.event_at >= NOW() - INTERVAL '%s days'
		)`, argNum, c.Value)
		return clause, args, argNum + 1

	case "not_in_last_days", "more_than_days_ago":
		// "event did NOT occur in last N days" → NOT EXISTS with time filter
		args = append(args, eventType)
		clause := fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
			  AND e.event_at >= NOW() - INTERVAL '%s days'
		)`, argNum, c.Value)
		return clause, args, argNum + 1

	default:
		return "", nil, argNum
	}
}

// BuildSegmentSubscriberQuery returns a SELECT query for subscriber id+email
// matching a segment's conditions. Used by deploy-time pre-computation.
func BuildSegmentSubscriberQuery(listID interface{}, conditions []SegmentConditionInput) (string, []interface{}) {
	where, args := BuildSegmentWhereClause(listID, conditions)
	return fmt.Sprintf("SELECT id::text, email FROM mailing_subscribers WHERE %s", where), args
}

// calculateSegmentCount calculates the actual subscriber count for a segment based on its conditions
func (s *AdvancedMailingService) calculateSegmentCount(ctx context.Context, segmentID uuid.UUID, listID interface{}, conditions []struct {
	Group    int    `json:"group"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}) int {
	converted := make([]SegmentConditionInput, len(conditions))
	for i, c := range conditions {
		converted[i] = SegmentConditionInput{Group: c.Group, Field: c.Field, Operator: c.Operator, Value: c.Value}
	}
	where, args := BuildSegmentWhereClause(listID, converted)
	query := fmt.Sprintf("SELECT COUNT(*) FROM mailing_subscribers WHERE %s", where)

	var count int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	if err != nil {
		log.Printf("Error calculating segment count: %v (query: %s)", err, query)
		return 0
	}
	return count
}

// mapFieldToColumn maps frontend field names to database column names
func mapFieldToColumn(field string) string {
	// Direct column mappings
	columnMap := map[string]string{
		"email":                  "email",
		"first_name":             "first_name",
		"last_name":              "last_name",
		"status":                 "status",
		"engagement_score":       "engagement_score",
		"total_emails_received":  "total_emails_received",
		"total_opens":            "total_opens",
		"total_clicks":           "total_clicks",
		"last_email_at":          "last_email_at",
		"last_open_at":           "last_open_at",
		"last_click_at":          "last_click_at",
		"subscribed_at":          "subscribed_at",
		"created_at":             "created_at",
		"source":                 "source",
		"timezone":               "timezone",
	}
	
	if col, ok := columnMap[field]; ok {
		return col
	}
	
	// Custom fields are stored in JSONB - access them with ->> operator
	if strings.HasPrefix(field, "custom.") {
		jsonKey := strings.TrimPrefix(field, "custom.")
		return fmt.Sprintf("custom_fields->>'%s'", jsonKey)
	}
	
	// For fields like 'city', 'company', etc. that are in custom_fields
	customFieldAliases := map[string]bool{
		"city": true, "state": true, "country": true, "postal_code": true,
		"company": true, "job_title": true, "industry": true, "phone": true,
		"language": true,
	}
	if customFieldAliases[field] {
		return fmt.Sprintf("custom_fields->>'%s'", field)
	}
	
	// Default: assume it's a direct column
	return field
}

func (s *AdvancedMailingService) HandleGetSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID := chi.URLParam(r, "segmentId")

	var id uuid.UUID
	var listID *uuid.UUID
	var name, desc, segType, status, conditionsRaw string
	var subCount int

	err := s.db.QueryRowContext(ctx, `
		SELECT id, list_id, name, description, segment_type, subscriber_count, status,
		       COALESCE(conditions::text, '[]')
		FROM mailing_segments WHERE id = $1
	`, segmentID).Scan(&id, &listID, &name, &desc, &segType, &subCount, &status, &conditionsRaw)

	if err != nil {
		http.Error(w, `{"error":"segment not found"}`, http.StatusNotFound)
		return
	}

	var conditions []interface{}
	json.Unmarshal([]byte(conditionsRaw), &conditions)

	// Fallback: if JSONB conditions are empty, read from mailing_segment_conditions table
	if len(conditions) == 0 {
		rows, qErr := s.db.QueryContext(ctx, `
			SELECT condition_group, field, operator, value
			FROM mailing_segment_conditions
			WHERE segment_id = $1
			ORDER BY condition_group
		`, segmentID)
		if qErr == nil {
			defer rows.Close()
			for rows.Next() {
				var group int
				var field, operator, value string
				if err := rows.Scan(&group, &field, &operator, &value); err == nil {
					conditions = append(conditions, map[string]interface{}{
						"group": group, "field": field, "operator": operator, "value": value,
					})
				}
			}
		}
	}

	resp := map[string]interface{}{
		"id": id.String(), "name": name, "description": desc,
		"segment_type": segType, "subscriber_count": subCount, "status": status, "conditions": conditions,
	}
	if listID != nil {
		resp["list_id"] = listID.String()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *AdvancedMailingService) HandleUpdateSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID := chi.URLParam(r, "segmentId")

	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ListID      string `json:"list_id"`
		Status      string `json:"status"`
		Conditions  []struct {
			Group    int    `json:"group"`
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    string `json:"value"`
		} `json:"conditions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	var listID interface{}
	if input.ListID != "" {
		parsed, err := uuid.Parse(input.ListID)
		if err != nil {
			http.Error(w, `{"error":"invalid list_id"}`, http.StatusBadRequest)
			return
		}
		listID = parsed
	}

	status := input.Status
	if status == "" {
		status = "active"
	}

	conditionsJSON := buildConditionsJSON(input.Conditions)

	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_segments
		SET name = $2, description = $3, list_id = $4, status = $5, conditions = $6, updated_at = NOW()
		WHERE id = $1
	`, segmentID, input.Name, input.Description, listID, status, conditionsJSON)
	if err != nil {
		log.Printf("[HandleUpdateSegment] update error: %v", err)
		http.Error(w, `{"error":"failed to update segment"}`, http.StatusInternalServerError)
		return
	}

	s.db.ExecContext(ctx, `DELETE FROM mailing_segment_conditions WHERE segment_id = $1`, segmentID)
	for i, c := range input.Conditions {
		operator := mapOperatorForDB(c.Operator)
		if operator == "" {
			continue
		}
		s.db.ExecContext(ctx, `
			INSERT INTO mailing_segment_conditions (id, segment_id, condition_group, field, operator, value)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, uuid.New(), segmentID, i, c.Field, operator, c.Value)
	}

	segUUID, _ := uuid.Parse(segmentID)
	subscriberCount := s.calculateSegmentCount(ctx, segUUID, listID, input.Conditions)
	s.db.ExecContext(ctx, `UPDATE mailing_segments SET subscriber_count = $2 WHERE id = $1`, segmentID, subscriberCount)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": segmentID, "updated": true, "subscriber_count": subscriberCount,
	})
}

func (s *AdvancedMailingService) HandlePreviewSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID := chi.URLParam(r, "segmentId")
	segmentUUID, _ := uuid.Parse(segmentID)
	
	// Get segment with conditions from JSONB column
	var listID *uuid.UUID
	var conditionsJSON sql.NullString
	
	err := s.db.QueryRowContext(ctx, `SELECT list_id, conditions FROM mailing_segments WHERE id = $1`, segmentID).Scan(&listID, &conditionsJSON)
	if err != nil {
		http.Error(w, `{"error":"segment not found"}`, http.StatusNotFound)
		return
	}
	
	// Parse conditions from JSONB
	var conditions []struct {
		Group    int    `json:"group"`
		Field    string `json:"field"`
		Operator string `json:"operator"`
		Value    string `json:"value"`
	}
	
	if conditionsJSON.Valid && conditionsJSON.String != "" && conditionsJSON.String != "[]" {
		json.Unmarshal([]byte(conditionsJSON.String), &conditions)
	}

	// Fallback: if JSONB conditions column is empty, read from mailing_segment_conditions table
	if len(conditions) == 0 {
		rows, qErr := s.db.QueryContext(ctx, `
			SELECT condition_group, field, operator, value
			FROM mailing_segment_conditions
			WHERE segment_id = $1
			ORDER BY condition_group
		`, segmentID)
		if qErr == nil {
			defer rows.Close()
			for rows.Next() {
				var c struct {
					Group    int    `json:"group"`
					Field    string `json:"field"`
					Operator string `json:"operator"`
					Value    string `json:"value"`
				}
				if err := rows.Scan(&c.Group, &c.Field, &c.Operator, &c.Value); err == nil {
					conditions = append(conditions, c)
				}
			}
		}
	}

	// Convert *uuid.UUID to interface{} correctly: a nil *uuid.UUID wrapped in
	// interface{} is non-nil, which would cause BuildSegmentWhereClause to add
	// a broken "list_id = NULL" equality clause. Pass a true nil instead.
	var listIDArg interface{}
	if listID != nil {
		listIDArg = *listID
	}

	count := s.calculateSegmentCount(ctx, segmentUUID, listIDArg, conditions)
	
	// Update segment count
	s.db.ExecContext(ctx, `UPDATE mailing_segments SET subscriber_count = $2, updated_at = NOW() WHERE id = $1`, segmentID, count)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"subscriber_count": count,
		"segment_id":       segmentID,
	})
}

func (s *AdvancedMailingService) HandleDeleteSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID := chi.URLParam(r, "segmentId")
	
	s.db.ExecContext(ctx, `DELETE FROM mailing_segments WHERE id = $1`, segmentID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"deleted": segmentID})
}
