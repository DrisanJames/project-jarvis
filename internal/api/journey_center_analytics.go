package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// HandleJourneyMetrics returns detailed metrics for a specific journey
// GET /api/journey-center/journeys/{id}/metrics
func (jc *JourneyCenter) HandleJourneyMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	journeyID := chi.URLParam(r, "journeyId")

	// Get journey info
	var metrics JourneyMetrics
	var nodesJSON sql.NullString
	err := jc.db.QueryRowContext(ctx, `
		SELECT id, name, status, nodes FROM mailing_journeys WHERE id = $1
	`, journeyID).Scan(&metrics.JourneyID, &metrics.JourneyName, &metrics.Status, &nodesJSON)
	if err != nil {
		http.Error(w, `{"error":"journey not found"}`, http.StatusNotFound)
		return
	}

	// Get enrollment stats
	jc.db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*) as total,
			COUNT(*) FILTER (WHERE status = 'active') as active,
			COUNT(*) FILTER (WHERE status IN ('completed', 'converted')) as completed,
			COUNT(*) FILTER (WHERE status = 'converted') as converted,
			COUNT(*) FILTER (WHERE status = 'exited') as exited
		FROM mailing_journey_enrollments
		WHERE journey_id = $1
	`, journeyID).Scan(&metrics.TotalEnrollments, &metrics.ActiveEnrollments, 
		&metrics.CompletedCount, &metrics.ConvertedCount, &metrics.ExitedCount)

	// Calculate rates
	if metrics.TotalEnrollments > 0 {
		metrics.CompletionRate = float64(metrics.CompletedCount) / float64(metrics.TotalEnrollments)
		metrics.ConversionRate = float64(metrics.ConvertedCount) / float64(metrics.TotalEnrollments)
	}

	// Calculate average time to complete
	var avgSeconds sql.NullFloat64
	jc.db.QueryRowContext(ctx, `
		SELECT AVG(EXTRACT(EPOCH FROM (completed_at - enrolled_at)))
		FROM mailing_journey_enrollments
		WHERE journey_id = $1 AND completed_at IS NOT NULL
	`, journeyID).Scan(&avgSeconds)
	if avgSeconds.Valid {
		metrics.AverageTimeToComplete = formatJourneyDuration(time.Duration(avgSeconds.Float64) * time.Second)
	}

	// Get email metrics
	jc.db.QueryRowContext(ctx, `
		SELECT 
			COALESCE(SUM((details->>'sent')::int), 0),
			COALESCE(SUM((details->>'opens')::int), 0),
			COALESCE(SUM((details->>'unique_opens')::int), 0),
			COALESCE(SUM((details->>'clicks')::int), 0),
			COALESCE(SUM((details->>'unique_clicks')::int), 0),
			COALESCE(SUM((details->>'bounces')::int), 0),
			COALESCE(SUM((details->>'unsubscribes')::int), 0)
		FROM mailing_journey_executions
		WHERE journey_id = $1 AND node_type = 'email'
	`, journeyID).Scan(
		&metrics.EmailMetrics.TotalSent,
		&metrics.EmailMetrics.TotalOpens,
		&metrics.EmailMetrics.UniqueOpens,
		&metrics.EmailMetrics.TotalClicks,
		&metrics.EmailMetrics.UniqueClicks,
		&metrics.EmailMetrics.Bounces,
		&metrics.EmailMetrics.Unsubscribes,
	)

	if metrics.EmailMetrics.TotalSent > 0 {
		metrics.EmailMetrics.OpenRate = float64(metrics.EmailMetrics.UniqueOpens) / float64(metrics.EmailMetrics.TotalSent)
		metrics.EmailMetrics.ClickRate = float64(metrics.EmailMetrics.UniqueClicks) / float64(metrics.EmailMetrics.TotalSent)
		metrics.EmailMetrics.BounceRate = float64(metrics.EmailMetrics.Bounces) / float64(metrics.EmailMetrics.TotalSent)
	}
	if metrics.EmailMetrics.UniqueOpens > 0 {
		metrics.EmailMetrics.ClickToOpen = float64(metrics.EmailMetrics.UniqueClicks) / float64(metrics.EmailMetrics.UniqueOpens)
	}

	// Get node metrics
	metrics.NodeMetrics = []NodeMetric{}
	if nodesJSON.Valid {
		var nodes []JourneyNode
		json.Unmarshal([]byte(nodesJSON.String), &nodes)

		for _, node := range nodes {
			nodeMetric := NodeMetric{
				NodeID:   node.ID,
				NodeType: node.Type,
				NodeName: getNodeName(node),
			}

			// Get execution stats for this node
			jc.db.QueryRowContext(ctx, `
				SELECT 
					COUNT(*) FILTER (WHERE action = 'entered'),
					COUNT(*) FILTER (WHERE action = 'completed'),
					COUNT(*) FILTER (WHERE action IN ('exited', 'failed'))
				FROM mailing_journey_executions
				WHERE journey_id = $1 AND node_id = $2
			`, journeyID, node.ID).Scan(&nodeMetric.Entered, &nodeMetric.Completed, &nodeMetric.Exited)

			// Get avg time spent
			var avgNodeSeconds sql.NullFloat64
			jc.db.QueryRowContext(ctx, `
				SELECT AVG(EXTRACT(EPOCH FROM (completed_at - entered_at)))
				FROM mailing_journey_executions
				WHERE journey_id = $1 AND node_id = $2 AND completed_at IS NOT NULL
			`, journeyID, node.ID).Scan(&avgNodeSeconds)
			if avgNodeSeconds.Valid {
				nodeMetric.AvgTimeSpent = formatJourneyDuration(time.Duration(avgNodeSeconds.Float64) * time.Second)
			}

			metrics.NodeMetrics = append(metrics.NodeMetrics, nodeMetric)
		}
	}

	// Get hourly distribution
	metrics.HourlyDistribution = []HourlyMetric{}
	hourlyRows, _ := jc.db.QueryContext(ctx, `
		SELECT 
			EXTRACT(HOUR FROM enrolled_at)::int as hour,
			COUNT(*) FILTER (WHERE enrolled_at IS NOT NULL) as enrollments,
			COUNT(*) FILTER (WHERE completed_at IS NOT NULL) as completions
		FROM mailing_journey_enrollments
		WHERE journey_id = $1 AND enrolled_at >= NOW() - INTERVAL '7 days'
		GROUP BY EXTRACT(HOUR FROM enrolled_at)
		ORDER BY hour
	`, journeyID)
	if hourlyRows != nil {
		defer hourlyRows.Close()
		for hourlyRows.Next() {
			var h HourlyMetric
			hourlyRows.Scan(&h.Hour, &h.Enrollments, &h.Completions)
			metrics.HourlyDistribution = append(metrics.HourlyDistribution, h)
		}
	}

	respondJSON(w, http.StatusOK, metrics)
}

// HandleJourneyFunnel returns node-by-node funnel analysis
// GET /api/journey-center/journeys/{id}/funnel
func (jc *JourneyCenter) HandleJourneyFunnel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	journeyID := chi.URLParam(r, "journeyId")

	// Get journey info
	var response JourneyFunnelResponse
	var nodesJSON sql.NullString
	err := jc.db.QueryRowContext(ctx, `
		SELECT id, name, nodes FROM mailing_journeys WHERE id = $1
	`, journeyID).Scan(&response.JourneyID, &response.JourneyName, &nodesJSON)
	if err != nil {
		http.Error(w, `{"error":"journey not found"}`, http.StatusNotFound)
		return
	}

	// Get total started
	jc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_journey_enrollments WHERE journey_id = $1
	`, journeyID).Scan(&response.TotalStart)

	// Build funnel from nodes
	response.FunnelSteps = []FunnelStep{}
	if nodesJSON.Valid {
		var nodes []JourneyNode
		json.Unmarshal([]byte(nodesJSON.String), &nodes)

		stepNum := 1
		for _, node := range nodes {
			if node.Type == "trigger" {
				continue // Skip trigger node in funnel
			}

			step := FunnelStep{
				StepNumber: stepNum,
				NodeID:     node.ID,
				NodeType:   node.Type,
				NodeName:   getNodeName(node),
			}

			// Get funnel metrics for this node
			jc.db.QueryRowContext(ctx, `
				SELECT 
					COUNT(*) FILTER (WHERE action = 'entered'),
					COUNT(*) FILTER (WHERE action = 'completed')
				FROM mailing_journey_executions
				WHERE journey_id = $1 AND node_id = $2
			`, journeyID, node.ID).Scan(&step.Entered, &step.Completed)

			step.DroppedOff = step.Entered - step.Completed
			if step.Entered > 0 {
				step.DropOffRate = float64(step.DroppedOff) / float64(step.Entered)
			}
			if response.TotalStart > 0 {
				step.ConversionRate = float64(step.Completed) / float64(response.TotalStart)
			}

			response.FunnelSteps = append(response.FunnelSteps, step)
			stepNum++
		}
	}

	respondJSON(w, http.StatusOK, response)
}

// HandleJourneyTrends returns historical trends over time
// GET /api/journey-center/journeys/{id}/trends
// Query params: days (7, 30, 90)
func (jc *JourneyCenter) HandleJourneyTrends(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	journeyID := chi.URLParam(r, "journeyId")

	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days != 7 && days != 30 && days != 90 {
		days = 30 // Default to 30 days
	}

	var response JourneyTrendsResponse
	response.JourneyID = journeyID
	response.Period = fmt.Sprintf("%dd", days)
	response.DataPoints = []TrendDataPoint{}

	// Get journey name
	var name string
	jc.db.QueryRowContext(ctx, "SELECT name FROM mailing_journeys WHERE id = $1", journeyID).Scan(&name)

	// Get daily data points
	rows, err := jc.db.QueryContext(ctx, `
		WITH dates AS (
			SELECT generate_series(
				CURRENT_DATE - ($2::int || ' days')::interval,
				CURRENT_DATE,
				'1 day'::interval
			)::date as date
		),
		enrollments AS (
			SELECT DATE(enrolled_at) as date, COUNT(*) as count
			FROM mailing_journey_enrollments
			WHERE journey_id = $1 AND enrolled_at >= CURRENT_DATE - ($2::int || ' days')::interval
			GROUP BY DATE(enrolled_at)
		),
		completions AS (
			SELECT DATE(completed_at) as date, COUNT(*) as count
			FROM mailing_journey_enrollments
			WHERE journey_id = $1 AND completed_at >= CURRENT_DATE - ($2::int || ' days')::interval
			GROUP BY DATE(completed_at)
		),
		conversions AS (
			SELECT DATE(completed_at) as date, COUNT(*) as count
			FROM mailing_journey_enrollments
			WHERE journey_id = $1 AND status = 'converted' 
			  AND completed_at >= CURRENT_DATE - ($2::int || ' days')::interval
			GROUP BY DATE(completed_at)
		)
		SELECT 
			d.date,
			COALESCE(e.count, 0) as enrollments,
			COALESCE(c.count, 0) as completions,
			COALESCE(cv.count, 0) as conversions
		FROM dates d
		LEFT JOIN enrollments e ON e.date = d.date
		LEFT JOIN completions c ON c.date = d.date
		LEFT JOIN conversions cv ON cv.date = d.date
		ORDER BY d.date
	`, journeyID, days)
	if err != nil {
		respondJSON(w, http.StatusOK, response)
		return
	}
	defer rows.Close()

	var totalEnrollments, totalCompletions, totalConversions int
	for rows.Next() {
		var dp TrendDataPoint
		var date time.Time
		rows.Scan(&date, &dp.Enrollments, &dp.Completions, &dp.Conversions)
		dp.Date = date.Format("2006-01-02")
		
		totalEnrollments += dp.Enrollments
		totalCompletions += dp.Completions
		totalConversions += dp.Conversions

		response.DataPoints = append(response.DataPoints, dp)
	}

	// Build summary
	response.Summary = TrendSummary{
		TotalEnrollments: totalEnrollments,
		TotalCompletions: totalCompletions,
		TotalConversions: totalConversions,
	}
	if days > 0 {
		response.Summary.AvgDailyEnrollments = float64(totalEnrollments) / float64(days)
	}

	// Calculate trend percentages (compare to previous period)
	var prevEnrollments, prevCompletions, prevConversions int
	jc.db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*) FILTER (WHERE enrolled_at >= CURRENT_DATE - ($2::int * 2 || ' days')::interval 
				AND enrolled_at < CURRENT_DATE - ($2::int || ' days')::interval),
			COUNT(*) FILTER (WHERE completed_at >= CURRENT_DATE - ($2::int * 2 || ' days')::interval 
				AND completed_at < CURRENT_DATE - ($2::int || ' days')::interval),
			COUNT(*) FILTER (WHERE status = 'converted' 
				AND completed_at >= CURRENT_DATE - ($2::int * 2 || ' days')::interval 
				AND completed_at < CURRENT_DATE - ($2::int || ' days')::interval)
		FROM mailing_journey_enrollments
		WHERE journey_id = $1
	`, journeyID, days).Scan(&prevEnrollments, &prevCompletions, &prevConversions)

	if prevEnrollments > 0 {
		response.Summary.EnrollmentTrend = (float64(totalEnrollments) - float64(prevEnrollments)) / float64(prevEnrollments) * 100
	}
	if prevCompletions > 0 {
		response.Summary.CompletionTrend = (float64(totalCompletions) - float64(prevCompletions)) / float64(prevCompletions) * 100
	}
	if prevConversions > 0 {
		response.Summary.ConversionTrend = (float64(totalConversions) - float64(prevConversions)) / float64(prevConversions) * 100
	}

	respondJSON(w, http.StatusOK, response)
}

// HandleJourneyPerformanceComparison returns cross-journey performance comparison
// GET /api/journey-center/performance
func (jc *JourneyCenter) HandleJourneyPerformanceComparison(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := jc.db.QueryContext(ctx, `
		SELECT 
			j.id, j.name, j.status,
			COUNT(e.id) as total_enrollments,
			COUNT(e.id) FILTER (WHERE e.status IN ('completed', 'converted')) as completed,
			COUNT(e.id) FILTER (WHERE e.status = 'converted') as converted,
			AVG(EXTRACT(EPOCH FROM (e.completed_at - e.enrolled_at))) FILTER (WHERE e.completed_at IS NOT NULL) as avg_time_seconds
		FROM mailing_journeys j
		LEFT JOIN mailing_journey_enrollments e ON j.id = e.journey_id
		WHERE j.status IN ('active', 'paused')
		GROUP BY j.id, j.name, j.status
		HAVING COUNT(e.id) > 0
		ORDER BY converted DESC, completed DESC
	`)

	performances := []JourneyPerformanceItem{}
	if err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"performances": performances,
			"total":        0,
		})
		return
	}
	defer rows.Close()

	for rows.Next() {
		var item JourneyPerformanceItem
		var completed, converted int
		var avgTimeSeconds sql.NullFloat64

		rows.Scan(
			&item.JourneyID, &item.JourneyName, &item.Status,
			&item.TotalEnrollments, &completed, &converted, &avgTimeSeconds,
		)

		if item.TotalEnrollments > 0 {
			item.CompletionRate = float64(completed) / float64(item.TotalEnrollments)
			item.ConversionRate = float64(converted) / float64(item.TotalEnrollments)
		}

		if avgTimeSeconds.Valid {
			item.AvgTimeToComplete = formatJourneyDuration(time.Duration(avgTimeSeconds.Float64) * time.Second)
		}

		// Get email metrics
		jc.db.QueryRowContext(ctx, `
			SELECT 
				COALESCE(SUM((details->>'sent')::int), 0),
				COALESCE(SUM((details->>'unique_opens')::int), 0),
				COALESCE(SUM((details->>'unique_clicks')::int), 0)
			FROM mailing_journey_executions
			WHERE journey_id = $1 AND node_type = 'email'
		`, item.JourneyID).Scan(&item.EmailsSent, &item.OpenRate, &item.ClickRate)

		if item.EmailsSent > 0 {
			opens := int(item.OpenRate)
			clicks := int(item.ClickRate)
			item.OpenRate = float64(opens) / float64(item.EmailsSent)
			item.ClickRate = float64(clicks) / float64(item.EmailsSent)
		}

		performances = append(performances, item)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"performances": performances,
		"total":        len(performances),
	})
}
