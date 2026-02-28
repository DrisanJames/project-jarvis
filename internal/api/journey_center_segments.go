package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleJourneySegments lists segments available for journey enrollment
// GET /api/journey-center/segments
func (jc *JourneyCenter) HandleJourneySegments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := jc.db.QueryContext(ctx, `
		SELECT 
			id, name, description, subscriber_count, last_calculated_at
		FROM mailing_segments
		WHERE status = 'active'
		ORDER BY name ASC
	`)
	if err != nil {
		// Try alternative table
		rows, err = jc.db.QueryContext(ctx, `
			SELECT 
				id::text, name, COALESCE(description, '') as description, 
				COALESCE(subscriber_count, 0), COALESCE(last_calculated_at, NOW())
			FROM segmentation_segments
			ORDER BY name ASC
		`)
	}

	segments := []SegmentForEnrollment{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var seg SegmentForEnrollment
			var description sql.NullString
			rows.Scan(&seg.ID, &seg.Name, &description, &seg.SubscriberCount, &seg.LastCalculated)
			seg.Description = description.String
			segments = append(segments, seg)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"segments": segments,
	})
}

// HandleSegmentEnrollment enrolls an entire segment into a journey
// POST /api/journey-center/journeys/{id}/segment-enroll
// Body: { segment_id: "", batch_size: 1000 }
func (jc *JourneyCenter) HandleSegmentEnrollment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	journeyID := chi.URLParam(r, "journeyId")

	var req struct {
		SegmentID string `json:"segment_id"`
		BatchSize int    `json:"batch_size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.SegmentID == "" {
		http.Error(w, `{"error":"segment_id is required"}`, http.StatusBadRequest)
		return
	}

	if req.BatchSize <= 0 {
		req.BatchSize = 1000
	}

	// Verify journey is active
	var status string
	err := jc.db.QueryRowContext(ctx, "SELECT status FROM mailing_journeys WHERE id = $1", journeyID).Scan(&status)
	if err != nil {
		http.Error(w, `{"error":"journey not found"}`, http.StatusNotFound)
		return
	}
	if status != "active" {
		http.Error(w, `{"error":"journey must be active to enroll subscribers"}`, http.StatusBadRequest)
		return
	}

	// Get first node ID
	var nodesJSON sql.NullString
	jc.db.QueryRowContext(ctx, "SELECT nodes FROM mailing_journeys WHERE id = $1", journeyID).Scan(&nodesJSON)
	firstNodeID := ""
	if nodesJSON.Valid {
		var nodes []JourneyNode
		json.Unmarshal([]byte(nodesJSON.String), &nodes)
		for _, node := range nodes {
			if node.Type != "trigger" {
				firstNodeID = node.ID
				break
			}
		}
	}

	// Get emails from segment
	emails := jc.getEmailsFromSegment(ctx, req.SegmentID)
	if len(emails) == 0 {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success":  true,
			"enrolled": 0,
			"message":  "No subscribers found in segment",
		})
		return
	}

	// Start background enrollment process
	jobID := fmt.Sprintf("segment-enroll-%s", uuid.New().String()[:8])

	// For now, do synchronous enrollment with batching
	// In production, this would be a background job
	enrolled := 0
	skipped := 0

	for i := 0; i < len(emails); i += req.BatchSize {
		end := i + req.BatchSize
		if end > len(emails) {
			end = len(emails)
		}
		batch := emails[i:end]

		for _, email := range batch {
			enrollmentID := fmt.Sprintf("enroll-%s", uuid.New().String()[:8])
			
			result, err := jc.db.ExecContext(ctx, `
				INSERT INTO mailing_journey_enrollments 
				(id, journey_id, subscriber_email, current_node_id, status, enrolled_at)
				VALUES ($1, $2, $3, $4, 'active', NOW())
				ON CONFLICT (journey_id, subscriber_email) DO NOTHING
			`, enrollmentID, journeyID, email, firstNodeID)

			if err == nil {
				rowsAffected, _ := result.RowsAffected()
				if rowsAffected > 0 {
					enrolled++
				} else {
					skipped++
				}
			} else {
				skipped++
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":    true,
		"job_id":     jobID,
		"enrolled":   enrolled,
		"skipped":    skipped,
		"total":      len(emails),
		"segment_id": req.SegmentID,
	})
}

// getEmailsFromSegment retrieves subscriber emails matching a segment's conditions
func (jc *JourneyCenter) getEmailsFromSegment(ctx interface{}, segmentID string) []string {
	emails := []string{}

	// Get segment conditions
	var conditionsJSON sql.NullString
	var listID sql.NullString
	err := jc.db.QueryRow(`
		SELECT conditions, list_id FROM mailing_segments WHERE id = $1
	`, segmentID).Scan(&conditionsJSON, &listID)
	if err != nil {
		return emails
	}

	// Parse conditions
	var conditions []struct {
		Field    string `json:"field"`
		Operator string `json:"operator"`
		Value    string `json:"value"`
		Group    int    `json:"group"`
	}
	if conditionsJSON.Valid {
		json.Unmarshal([]byte(conditionsJSON.String), &conditions)
	}

	// Build dynamic query based on conditions
	query := "SELECT DISTINCT email FROM mailing_subscribers WHERE status = 'confirmed'"
	args := []interface{}{}
	argIdx := 1

	// Add list filter if segment is tied to a list
	if listID.Valid && listID.String != "" {
		query += fmt.Sprintf(" AND list_id = $%d", argIdx)
		args = append(args, listID.String)
		argIdx++
	}

	// Build WHERE clause from conditions
	for _, cond := range conditions {
		var clause string
		switch cond.Operator {
		case "equals", "=":
			clause = fmt.Sprintf(" AND %s = $%d", sanitizeFieldName(cond.Field), argIdx)
			args = append(args, cond.Value)
		case "not_equals", "!=":
			clause = fmt.Sprintf(" AND %s != $%d", sanitizeFieldName(cond.Field), argIdx)
			args = append(args, cond.Value)
		case "contains":
			clause = fmt.Sprintf(" AND %s ILIKE $%d", sanitizeFieldName(cond.Field), argIdx)
			args = append(args, "%"+cond.Value+"%")
		case "not_contains":
			clause = fmt.Sprintf(" AND %s NOT ILIKE $%d", sanitizeFieldName(cond.Field), argIdx)
			args = append(args, "%"+cond.Value+"%")
		case "starts_with":
			clause = fmt.Sprintf(" AND %s ILIKE $%d", sanitizeFieldName(cond.Field), argIdx)
			args = append(args, cond.Value+"%")
		case "ends_with":
			clause = fmt.Sprintf(" AND %s ILIKE $%d", sanitizeFieldName(cond.Field), argIdx)
			args = append(args, "%"+cond.Value)
		case "is_empty":
			clause = fmt.Sprintf(" AND (%s IS NULL OR %s = '')", sanitizeFieldName(cond.Field), sanitizeFieldName(cond.Field))
			argIdx-- // No argument needed
		case "is_not_empty":
			clause = fmt.Sprintf(" AND %s IS NOT NULL AND %s != ''", sanitizeFieldName(cond.Field), sanitizeFieldName(cond.Field))
			argIdx-- // No argument needed
		default:
			continue
		}
		query += clause
		argIdx++
	}

	rows, err := jc.db.Query(query, args...)
	if err != nil {
		return emails
	}
	defer rows.Close()

	for rows.Next() {
		var email string
		rows.Scan(&email)
		emails = append(emails, email)
	}

	return emails
}

// sanitizeFieldName ensures only valid column names are used
func sanitizeFieldName(field string) string {
	// Whitelist of allowed fields
	allowedFields := map[string]string{
		"email":      "email",
		"first_name": "first_name",
		"last_name":  "last_name",
		"status":     "status",
		"created_at": "created_at",
		"updated_at": "updated_at",
	}
	if sanitized, ok := allowedFields[field]; ok {
		return sanitized
	}
	// For custom fields, use JSONB extraction
	return fmt.Sprintf("custom_fields->>'%s'", field)
}
