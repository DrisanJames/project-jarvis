package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// HandleJourneyCenterOverview returns dashboard overview with all journey stats
// GET /api/journey-center/overview
func (jc *JourneyCenter) HandleJourneyCenterOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	overview := JourneyCenterOverview{
		TopJourneys:    []JourneyOverviewItem{},
		RecentActivity: []JourneyActivityItem{},
	}

	// Get journey counts by status
	err := jc.db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*) as total,
			COUNT(*) FILTER (WHERE status = 'active') as active,
			COUNT(*) FILTER (WHERE status = 'draft') as draft,
			COUNT(*) FILTER (WHERE status = 'paused') as paused
		FROM mailing_journeys
	`).Scan(&overview.TotalJourneys, &overview.ActiveJourneys, &overview.DraftJourneys, &overview.PausedJourneys)
	if err != nil {
		// Tables might not exist yet
		overview.TotalJourneys = 0
	}

	// Get enrollment stats
	jc.db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*) FILTER (WHERE status = 'active') as active_enrollments,
			COUNT(*) FILTER (WHERE DATE(enrolled_at) = CURRENT_DATE) as enrollments_today,
			COUNT(*) FILTER (WHERE DATE(completed_at) = CURRENT_DATE) as completions_today,
			COUNT(*) FILTER (WHERE status = 'converted' AND DATE(completed_at) = CURRENT_DATE) as conversions_today
		FROM mailing_journey_enrollments
	`).Scan(&overview.TotalActiveEnrollments, &overview.EnrollmentsToday, &overview.CompletionsToday, &overview.ConversionsToday)

	// Calculate overall conversion rate
	var totalCompleted, totalConverted int
	jc.db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*) FILTER (WHERE status IN ('completed', 'converted')),
			COUNT(*) FILTER (WHERE status = 'converted')
		FROM mailing_journey_enrollments
	`).Scan(&totalCompleted, &totalConverted)

	if totalCompleted > 0 {
		overview.OverallConversionRate = float64(totalConverted) / float64(totalCompleted)
	}

	// Get top performing journeys
	rows, err := jc.db.QueryContext(ctx, `
		SELECT 
			j.id, j.name, j.status,
			COUNT(e.id) FILTER (WHERE e.status = 'active') as active_enrolled,
			COUNT(e.id) FILTER (WHERE e.status IN ('completed', 'converted')) as completed,
			COUNT(e.id) FILTER (WHERE e.status = 'converted') as converted
		FROM mailing_journeys j
		LEFT JOIN mailing_journey_enrollments e ON j.id = e.journey_id
		WHERE j.status = 'active'
		GROUP BY j.id, j.name, j.status
		ORDER BY converted DESC, completed DESC
		LIMIT 5
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var item JourneyOverviewItem
			rows.Scan(&item.ID, &item.Name, &item.Status, &item.ActiveEnrolled, &item.Completed, &item.Converted)
			if item.Completed > 0 {
				item.ConversionRate = float64(item.Converted) / float64(item.Completed)
			}
			overview.TopJourneys = append(overview.TopJourneys, item)
		}
	}

	// Get recent activity
	activityRows, err := jc.db.QueryContext(ctx, `
		SELECT 
			CASE 
				WHEN e.status = 'converted' THEN 'conversion'
				WHEN e.completed_at IS NOT NULL THEN 'completion'
				ELSE 'enrollment'
			END as type,
			e.journey_id,
			j.name as journey_name,
			e.subscriber_email,
			e.current_node_id,
			COALESCE(e.completed_at, e.enrolled_at) as timestamp
		FROM mailing_journey_enrollments e
		JOIN mailing_journeys j ON j.id = e.journey_id
		ORDER BY COALESCE(e.completed_at, e.enrolled_at) DESC
		LIMIT 20
	`)
	if err == nil {
		defer activityRows.Close()
		for activityRows.Next() {
			var item JourneyActivityItem
			var email, nodeID sql.NullString
			activityRows.Scan(&item.Type, &item.JourneyID, &item.JourneyName, &email, &nodeID, &item.Timestamp)
			item.Email = email.String
			item.NodeID = nodeID.String
			overview.RecentActivity = append(overview.RecentActivity, item)
		}
	}

	respondJSON(w, http.StatusOK, overview)
}

// HandleListJourneyCenterJourneys lists journeys with filtering, sorting, pagination
// GET /api/journey-center/journeys
// Query params: status, sort_by, order, page, limit, search
func (jc *JourneyCenter) HandleListJourneyCenterJourneys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	// Parse query params
	status := query.Get("status")
	sortBy := query.Get("sort_by")
	order := query.Get("order")
	page, _ := strconv.Atoi(query.Get("page"))
	limit, _ := strconv.Atoi(query.Get("limit"))
	search := query.Get("search")

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := (page - 1) * limit

	// Build query
	baseQuery := `
		SELECT 
			j.id, j.name, j.description, j.status, j.nodes, j.created_at, j.updated_at,
			COUNT(e.id) as total_enrollments,
			COUNT(e.id) FILTER (WHERE e.status = 'active') as active_enrollments,
			COUNT(e.id) FILTER (WHERE e.status IN ('completed', 'converted')) as completed,
			COUNT(e.id) FILTER (WHERE e.status = 'converted') as converted,
			MAX(e.enrolled_at) as last_enrollment_at
		FROM mailing_journeys j
		LEFT JOIN mailing_journey_enrollments e ON j.id = e.journey_id
		WHERE 1=1
	`

	args := []interface{}{}
	argIdx := 1

	if status != "" {
		baseQuery += fmt.Sprintf(" AND j.status = $%d", argIdx)
		args = append(args, status)
		argIdx++
	}

	if search != "" {
		baseQuery += fmt.Sprintf(" AND (j.name ILIKE $%d OR j.description ILIKE $%d)", argIdx, argIdx+1)
		args = append(args, "%"+search+"%", "%"+search+"%")
		argIdx += 2
	}

	baseQuery += " GROUP BY j.id, j.name, j.description, j.status, j.nodes, j.created_at, j.updated_at"

	// Sort
	validSortFields := map[string]string{
		"name":               "j.name",
		"status":             "j.status",
		"created_at":         "j.created_at",
		"updated_at":         "j.updated_at",
		"total_enrollments":  "total_enrollments",
		"active_enrollments": "active_enrollments",
		"completion_rate":    "completed",
		"conversion_rate":    "converted",
	}

	sortField := validSortFields[sortBy]
	if sortField == "" {
		sortField = "j.updated_at"
	}
	sortOrder := "DESC"
	if strings.ToUpper(order) == "ASC" {
		sortOrder = "ASC"
	}
	baseQuery += fmt.Sprintf(" ORDER BY %s %s", sortField, sortOrder)

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM (%s) sub", baseQuery)
	var total int
	jc.db.QueryRowContext(ctx, countQuery, args...).Scan(&total)

	// Add pagination
	baseQuery += fmt.Sprintf(" LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := jc.db.QueryContext(ctx, baseQuery, args...)
	if err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"journeys":   []JourneyListItem{},
			"total":      0,
			"page":       page,
			"limit":      limit,
			"total_pages": 0,
		})
		return
	}
	defer rows.Close()

	journeys := []JourneyListItem{}
	for rows.Next() {
		var item JourneyListItem
		var description sql.NullString
		var nodesJSON sql.NullString
		var lastEnrollment sql.NullTime
		var completed, converted int

		err := rows.Scan(
			&item.ID, &item.Name, &description, &item.Status, &nodesJSON,
			&item.CreatedAt, &item.UpdatedAt, &item.TotalEnrollments,
			&item.ActiveEnrollments, &completed, &converted, &lastEnrollment,
		)
		if err != nil {
			continue
		}

		item.Description = description.String
		if lastEnrollment.Valid {
			item.LastEnrollmentAt = &lastEnrollment.Time
		}

		// Parse nodes to count
		if nodesJSON.Valid {
			var nodes []interface{}
			json.Unmarshal([]byte(nodesJSON.String), &nodes)
			item.NodeCount = len(nodes)
		}

		// Calculate rates
		if item.TotalEnrollments > 0 {
			item.CompletionRate = float64(completed) / float64(item.TotalEnrollments)
			item.ConversionRate = float64(converted) / float64(item.TotalEnrollments)
		}

		// Get email metrics for this journey (from execution history)
		jc.db.QueryRowContext(ctx, `
			SELECT 
				COALESCE(SUM((details->>'sent')::int), 0),
				COALESCE(SUM((details->>'opens')::int), 0),
				COALESCE(SUM((details->>'clicks')::int), 0)
			FROM mailing_journey_executions
			WHERE journey_id = $1 AND node_type = 'email'
		`, item.ID).Scan(&item.EmailsSent, &item.OpenRate, &item.ClickRate)

		if item.EmailsSent > 0 {
			// OpenRate and ClickRate stored as counts, convert to rate
			opens := int(item.OpenRate)
			clicks := int(item.ClickRate)
			item.OpenRate = float64(opens) / float64(item.EmailsSent)
			item.ClickRate = float64(clicks) / float64(item.EmailsSent)
		}

		journeys = append(journeys, item)
	}

	totalPages := (total + limit - 1) / limit

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"journeys":    journeys,
		"total":       total,
		"page":        page,
		"limit":       limit,
		"total_pages": totalPages,
	})
}
