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
	
	rows, _ := s.db.QueryContext(ctx, `
		SELECT id, name, description, segment_type, subscriber_count, status, created_at
		FROM mailing_segments WHERE organization_id = $1 ORDER BY created_at DESC
	`, orgID)
	defer rows.Close()
	
	var segments []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, desc, segType, status string
		var subCount int
		var createdAt time.Time
		rows.Scan(&id, &name, &desc, &segType, &subCount, &status, &createdAt)
		segments = append(segments, map[string]interface{}{
			"id": id.String(), "name": name, "description": desc, "segment_type": segType,
			"subscriber_count": subCount, "status": status, "created_at": createdAt,
		})
	}
	if segments == nil { segments = []map[string]interface{}{} }
	
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

// calculateSegmentCount calculates the actual subscriber count for a segment based on its conditions
func (s *AdvancedMailingService) calculateSegmentCount(ctx context.Context, segmentID uuid.UUID, listID interface{}, conditions []struct {
	Group    int    `json:"group"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}) int {
	// Build WHERE clause from conditions
	whereClauses := []string{"status = 'confirmed'"}
	args := []interface{}{}
	argNum := 1
	
	// Add list filter if specified
	if listID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("list_id = $%d", argNum))
		args = append(args, listID)
		argNum++
	}
	
	// Build condition clauses
	for _, c := range conditions {
		field := c.Field
		value := c.Value
		
		// Map common field names to database columns
		dbField := mapFieldToColumn(field)
		
		var clause string
		switch c.Operator {
		case "equals", "is":
			clause = fmt.Sprintf("%s = $%d", dbField, argNum)
			args = append(args, value)
			argNum++
		case "not_equals", "is_not":
			clause = fmt.Sprintf("%s != $%d", dbField, argNum)
			args = append(args, value)
			argNum++
		case "contains":
			clause = fmt.Sprintf("%s ILIKE $%d", dbField, argNum)
			args = append(args, "%"+value+"%")
			argNum++
		case "not_contains":
			clause = fmt.Sprintf("%s NOT ILIKE $%d", dbField, argNum)
			args = append(args, "%"+value+"%")
			argNum++
		case "starts_with":
			clause = fmt.Sprintf("%s ILIKE $%d", dbField, argNum)
			args = append(args, value+"%")
			argNum++
		case "ends_with":
			clause = fmt.Sprintf("%s ILIKE $%d", dbField, argNum)
			args = append(args, "%"+value)
			argNum++
		case "greater_than", "gt":
			clause = fmt.Sprintf("%s > $%d", dbField, argNum)
			args = append(args, value)
			argNum++
		case "less_than", "lt":
			clause = fmt.Sprintf("%s < $%d", dbField, argNum)
			args = append(args, value)
			argNum++
		case "greater_than_or_equal", "gte":
			clause = fmt.Sprintf("%s >= $%d", dbField, argNum)
			args = append(args, value)
			argNum++
		case "less_than_or_equal", "lte":
			clause = fmt.Sprintf("%s <= $%d", dbField, argNum)
			args = append(args, value)
			argNum++
		case "is_empty", "is_null":
			clause = fmt.Sprintf("(%s IS NULL OR %s = '')", dbField, dbField)
		case "is_not_empty", "is_not_null":
			clause = fmt.Sprintf("(%s IS NOT NULL AND %s != '')", dbField, dbField)
		case "in_last_days":
			// For date fields like last_open_at, subscribed_at
			clause = fmt.Sprintf("%s >= NOW() - INTERVAL '%s days'", dbField, value)
		case "more_than_days_ago":
			clause = fmt.Sprintf("%s < NOW() - INTERVAL '%s days'", dbField, value)
		default:
			continue
		}
		
		if clause != "" {
			whereClauses = append(whereClauses, clause)
		}
	}
	
	// Build final query
	query := fmt.Sprintf("SELECT COUNT(*) FROM mailing_subscribers WHERE %s", strings.Join(whereClauses, " AND "))
	
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
		"email":              "email",
		"first_name":         "first_name",
		"last_name":          "last_name",
		"status":             "status",
		"engagement_score":   "engagement_score",
		"total_opens":        "total_opens",
		"total_clicks":       "total_clicks",
		"last_open_at":       "last_open_at",
		"last_click_at":      "last_click_at",
		"last_email_at":      "last_email_at",
		"subscribed_at":      "subscribed_at",
		"created_at":         "created_at",
		"source":             "source",
		"timezone":           "timezone",
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
	
	var id, listID uuid.UUID
	var name, desc, segType, status string
	var subCount int
	
	err := s.db.QueryRowContext(ctx, `
		SELECT id, list_id, name, description, segment_type, subscriber_count, status
		FROM mailing_segments WHERE id = $1
	`, segmentID).Scan(&id, &listID, &name, &desc, &segType, &subCount, &status)
	
	if err != nil {
		http.Error(w, `{"error":"segment not found"}`, http.StatusNotFound)
		return
	}
	
	// Get conditions
	rows, _ := s.db.QueryContext(ctx, `
		SELECT condition_group, field, operator, value FROM mailing_segment_conditions WHERE segment_id = $1 ORDER BY condition_group
	`, segmentID)
	defer rows.Close()
	
	var conditions []map[string]interface{}
	for rows.Next() {
		var group int
		var field, operator, value string
		rows.Scan(&group, &field, &operator, &value)
		conditions = append(conditions, map[string]interface{}{
			"group": group, "field": field, "operator": operator, "value": value,
		})
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id.String(), "list_id": listID.String(), "name": name, "description": desc,
		"segment_type": segType, "subscriber_count": subCount, "status": status, "conditions": conditions,
	})
}

func (s *AdvancedMailingService) HandleUpdateSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID := chi.URLParam(r, "segmentId")
	
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Conditions  []struct {
			Group    int    `json:"group"`
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    string `json:"value"`
		} `json:"conditions"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	
	s.db.ExecContext(ctx, `UPDATE mailing_segments SET name = $2, description = $3, updated_at = NOW() WHERE id = $1`,
		segmentID, input.Name, input.Description)
	
	// Replace conditions
	s.db.ExecContext(ctx, `DELETE FROM mailing_segment_conditions WHERE segment_id = $1`, segmentID)
	for _, c := range input.Conditions {
		s.db.ExecContext(ctx, `
			INSERT INTO mailing_segment_conditions (id, segment_id, condition_group, field, operator, value)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, uuid.New(), segmentID, c.Group, c.Field, c.Operator, c.Value)
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": segmentID, "updated": true})
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
	
	// Calculate count using the shared function
	count := s.calculateSegmentCount(ctx, segmentUUID, listID, conditions)
	
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
