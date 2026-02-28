package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleJourneyEnrollments lists enrollments with status and progress
// GET /api/journey-center/journeys/{id}/enrollments
// Query params: status, page, limit, search
func (jc *JourneyCenter) HandleJourneyEnrollments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	journeyID := chi.URLParam(r, "journeyId")
	query := r.URL.Query()

	status := query.Get("status")
	search := query.Get("search")
	page, _ := strconv.Atoi(query.Get("page"))
	limit, _ := strconv.Atoi(query.Get("limit"))

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	offset := (page - 1) * limit

	// Build query
	baseQuery := `
		SELECT 
			e.id, e.subscriber_email, e.current_node_id, e.status, 
			e.enrolled_at, e.completed_at, e.metadata
		FROM mailing_journey_enrollments e
		WHERE e.journey_id = $1
	`
	args := []interface{}{journeyID}
	argIdx := 2

	if status != "" {
		baseQuery += fmt.Sprintf(" AND e.status = $%d", argIdx)
		args = append(args, status)
		argIdx++
	}

	if search != "" {
		baseQuery += fmt.Sprintf(" AND e.subscriber_email ILIKE $%d", argIdx)
		args = append(args, "%"+search+"%")
		argIdx++
	}

	// Count total
	countQuery := "SELECT COUNT(*) FROM (" + baseQuery + ") sub"
	var total int
	jc.db.QueryRowContext(ctx, countQuery, args...).Scan(&total)

	// Get journey nodes for progress calculation
	var nodesJSON sql.NullString
	var totalNodes int
	jc.db.QueryRowContext(ctx, "SELECT nodes FROM mailing_journeys WHERE id = $1", journeyID).Scan(&nodesJSON)
	if nodesJSON.Valid {
		var nodes []interface{}
		json.Unmarshal([]byte(nodesJSON.String), &nodes)
		totalNodes = len(nodes)
	}

	// Add sorting and pagination
	baseQuery += fmt.Sprintf(" ORDER BY e.enrolled_at DESC LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := jc.db.QueryContext(ctx, baseQuery, args...)
	if err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"enrollments": []EnrollmentListItem{},
			"total":       0,
			"page":        page,
			"limit":       limit,
		})
		return
	}
	defer rows.Close()

	enrollments := []EnrollmentListItem{}
	for rows.Next() {
		var item EnrollmentListItem
		var completedAt sql.NullTime
		var metadataJSON sql.NullString

		err := rows.Scan(
			&item.ID, &item.Email, &item.CurrentNodeID, &item.Status,
			&item.EnrolledAt, &completedAt, &metadataJSON,
		)
		if err != nil {
			continue
		}

		if completedAt.Valid {
			item.CompletedAt = &completedAt.Time
		}

		if metadataJSON.Valid {
			json.Unmarshal([]byte(metadataJSON.String), &item.Metadata)
		}

		// Calculate progress
		if totalNodes > 0 {
			var completedNodes int
			jc.db.QueryRowContext(ctx, `
				SELECT COUNT(DISTINCT node_id) 
				FROM mailing_journey_executions
				WHERE journey_id = $1 AND enrollment_id = $2 AND action = 'completed'
			`, journeyID, item.ID).Scan(&completedNodes)
			item.Progress = float64(completedNodes) / float64(totalNodes) * 100
		}

		// Get node name
		item.CurrentNodeName = jc.getNodeNameByID(ctx, journeyID, item.CurrentNodeID)

		enrollments = append(enrollments, item)
	}

	totalPages := (total + limit - 1) / limit

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"enrollments": enrollments,
		"total":       total,
		"page":        page,
		"limit":       limit,
		"total_pages": totalPages,
	})
}

// HandleEnrollmentDetail returns single enrollment detail with execution history
// GET /api/journey-center/journeys/{id}/enrollments/{enrollmentId}
func (jc *JourneyCenter) HandleEnrollmentDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	journeyID := chi.URLParam(r, "journeyId")
	enrollmentID := chi.URLParam(r, "enrollmentId")

	var detail EnrollmentDetail
	var completedAt sql.NullTime
	var metadataJSON sql.NullString

	err := jc.db.QueryRowContext(ctx, `
		SELECT 
			e.id, e.subscriber_email, e.current_node_id, e.status,
			e.enrolled_at, e.completed_at, e.metadata,
			j.id, j.name
		FROM mailing_journey_enrollments e
		JOIN mailing_journeys j ON j.id = e.journey_id
		WHERE e.journey_id = $1 AND e.id = $2
	`, journeyID, enrollmentID).Scan(
		&detail.ID, &detail.Email, &detail.CurrentNodeID, &detail.Status,
		&detail.EnrolledAt, &completedAt, &metadataJSON,
		&detail.JourneyID, &detail.JourneyName,
	)
	if err != nil {
		http.Error(w, `{"error":"enrollment not found"}`, http.StatusNotFound)
		return
	}

	if completedAt.Valid {
		detail.CompletedAt = &completedAt.Time
		if detail.Status == "converted" {
			detail.ConvertedAt = &completedAt.Time
		}
	}

	if metadataJSON.Valid {
		json.Unmarshal([]byte(metadataJSON.String), &detail.Metadata)
	}

	detail.CurrentNodeName = jc.getNodeNameByID(ctx, journeyID, detail.CurrentNodeID)

	// Get execution history
	detail.ExecutionHistory = []ExecutionHistoryItem{}
	historyRows, err := jc.db.QueryContext(ctx, `
		SELECT 
			node_id, node_type, action, entered_at, completed_at, details
		FROM mailing_journey_executions
		WHERE journey_id = $1 AND enrollment_id = $2
		ORDER BY entered_at ASC
	`, journeyID, enrollmentID)
	if err == nil {
		defer historyRows.Close()
		for historyRows.Next() {
			var item ExecutionHistoryItem
			var completedAt sql.NullTime
			var detailsJSON sql.NullString

			historyRows.Scan(
				&item.NodeID, &item.NodeType, &item.Action,
				&item.EnteredAt, &completedAt, &detailsJSON,
			)

			item.NodeName = jc.getNodeNameByID(ctx, journeyID, item.NodeID)
			if completedAt.Valid {
				item.CompletedAt = &completedAt.Time
				item.Duration = formatJourneyDuration(completedAt.Time.Sub(item.EnteredAt))
			}
			if detailsJSON.Valid {
				json.Unmarshal([]byte(detailsJSON.String), &item.Details)
			}

			detail.ExecutionHistory = append(detail.ExecutionHistory, item)
		}
	}

	// Get emails received
	detail.EmailsReceived = []EnrollmentEmailItem{}
	emailRows, err := jc.db.QueryContext(ctx, `
		SELECT 
			id, subject, sent_at, opened_at, clicked_at,
			CASE 
				WHEN clicked_at IS NOT NULL THEN 'clicked'
				WHEN opened_at IS NOT NULL THEN 'opened'
				ELSE 'sent'
			END as status
		FROM mailing_journey_emails
		WHERE enrollment_id = $1
		ORDER BY sent_at ASC
	`, enrollmentID)
	if err == nil {
		defer emailRows.Close()
		for emailRows.Next() {
			var email EnrollmentEmailItem
			var openedAt, clickedAt sql.NullTime
			emailRows.Scan(&email.EmailID, &email.Subject, &email.SentAt, &openedAt, &clickedAt, &email.Status)
			if openedAt.Valid {
				email.OpenedAt = &openedAt.Time
			}
			if clickedAt.Valid {
				email.ClickedAt = &clickedAt.Time
			}
			detail.EmailsReceived = append(detail.EmailsReceived, email)
		}
	}

	// Get subscriber info
	detail.SubscriberInfo = map[string]interface{}{}
	var firstName, lastName sql.NullString
	var customFieldsJSON sql.NullString
	err = jc.db.QueryRowContext(ctx, `
		SELECT first_name, last_name, custom_fields
		FROM mailing_subscribers
		WHERE email = $1
		LIMIT 1
	`, detail.Email).Scan(&firstName, &lastName, &customFieldsJSON)
	if err == nil {
		if firstName.Valid {
			detail.SubscriberInfo["first_name"] = firstName.String
		}
		if lastName.Valid {
			detail.SubscriberInfo["last_name"] = lastName.String
		}
		if customFieldsJSON.Valid {
			var customFields map[string]interface{}
			if json.Unmarshal([]byte(customFieldsJSON.String), &customFields) == nil {
				detail.SubscriberInfo["custom_fields"] = customFields
			}
		}
	}

	respondJSON(w, http.StatusOK, detail)
}

// HandleManualEnrollment manually enrolls subscribers into a journey
// POST /api/journey-center/journeys/{id}/enrollments
// Body: { emails: [], segment_id: "" }
func (jc *JourneyCenter) HandleManualEnrollment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	journeyID := chi.URLParam(r, "journeyId")

	var req struct {
		Emails    []string `json:"emails"`
		SegmentID string   `json:"segment_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Verify journey exists and is active
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

	// If segment_id provided, get emails from segment
	if req.SegmentID != "" {
		segmentEmails := jc.getEmailsFromSegment(ctx, req.SegmentID)
		req.Emails = append(req.Emails, segmentEmails...)
	}

	// Dedupe emails
	emailSet := make(map[string]bool)
	for _, email := range req.Emails {
		emailSet[strings.ToLower(strings.TrimSpace(email))] = true
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

	// Enroll each email
	enrolled := 0
	skipped := 0
	errors := []string{}

	for email := range emailSet {
		enrollmentID := fmt.Sprintf("enroll-%s", uuid.New().String()[:8])
		
		_, err := jc.db.ExecContext(ctx, `
			INSERT INTO mailing_journey_enrollments 
			(id, journey_id, subscriber_email, current_node_id, status, enrolled_at)
			VALUES ($1, $2, $3, $4, 'active', NOW())
			ON CONFLICT (journey_id, subscriber_email) DO NOTHING
		`, enrollmentID, journeyID, email, firstNodeID)

		if err != nil {
			errors = append(errors, fmt.Sprintf("failed to enroll %s: %v", email, err))
		} else {
			// Check if actually inserted
			var count int
			jc.db.QueryRowContext(ctx, 
				"SELECT COUNT(*) FROM mailing_journey_enrollments WHERE id = $1", 
				enrollmentID).Scan(&count)
			if count > 0 {
				enrolled++
			} else {
				skipped++ // Already enrolled
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"enrolled": enrolled,
		"skipped":  skipped,
		"total":    len(emailSet),
		"errors":   errors,
	})
}

// HandleTestJourney tests a journey with a sample subscriber
// POST /api/journey-center/journeys/{id}/test
// Body: { email: "", test_mode: true }
func (jc *JourneyCenter) HandleTestJourney(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	journeyID := chi.URLParam(r, "journeyId")

	var req struct {
		Email    string `json:"email"`
		TestMode bool   `json:"test_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.Email == "" {
		http.Error(w, `{"error":"email is required"}`, http.StatusBadRequest)
		return
	}

	// Verify journey exists
	var name, nodesJSON string
	err := jc.db.QueryRowContext(ctx, 
		"SELECT name, nodes FROM mailing_journeys WHERE id = $1", 
		journeyID).Scan(&name, &nodesJSON)
	if err != nil {
		http.Error(w, `{"error":"journey not found"}`, http.StatusNotFound)
		return
	}

	// Parse nodes
	var nodes []JourneyNode
	json.Unmarshal([]byte(nodesJSON), &nodes)

	// Create test enrollment
	testEnrollmentID := fmt.Sprintf("test-%s", uuid.New().String()[:8])
	firstNodeID := ""
	for _, node := range nodes {
		if node.Type != "trigger" {
			firstNodeID = node.ID
			break
		}
	}

	// Insert test enrollment with metadata marking it as test
	metadata := map[string]interface{}{
		"test_mode":    req.TestMode,
		"test_started": time.Now(),
	}
	metadataJSON, _ := json.Marshal(metadata)

	_, err = jc.db.ExecContext(ctx, `
		INSERT INTO mailing_journey_enrollments 
		(id, journey_id, subscriber_email, current_node_id, status, enrolled_at, metadata)
		VALUES ($1, $2, $3, $4, 'active', NOW(), $5)
	`, testEnrollmentID, journeyID, req.Email, firstNodeID, string(metadataJSON))

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to create test enrollment: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// If test_mode, simulate journey execution
	simulatedSteps := []map[string]interface{}{}
	if req.TestMode {
		for i, node := range nodes {
			if node.Type == "trigger" {
				continue
			}

			step := map[string]interface{}{
				"step":      i,
				"node_id":   node.ID,
				"node_type": node.Type,
				"node_name": getNodeName(node),
				"action":    "would_execute",
			}

			switch node.Type {
			case "email":
				step["details"] = map[string]interface{}{
					"subject":  node.Config["subject"],
					"template": node.Config["templateId"],
				}
			case "delay":
				step["details"] = map[string]interface{}{
					"delay_type":  node.Config["delayType"],
					"delay_value": node.Config["delayValue"],
				}
			case "condition":
				step["details"] = map[string]interface{}{
					"condition_type": node.Config["conditionType"],
					"branches":       len(node.Connections),
				}
			}

			simulatedSteps = append(simulatedSteps, step)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"enrollment_id": testEnrollmentID,
		"journey_name":  name,
		"email":         req.Email,
		"test_mode":     req.TestMode,
		"simulated_steps": simulatedSteps,
		"message":       "Test enrollment created. Check enrollment details for progress.",
	})
}
