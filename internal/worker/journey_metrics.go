package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// JourneyMetrics provides real-time and historical journey analytics
type JourneyMetrics struct {
	db         *sql.DB
	mu         sync.RWMutex
	
	// Cached metrics (updated periodically)
	cache      map[string]*JourneyStats
	lastUpdate time.Time
	
	// Cache configuration
	cacheTTL   time.Duration
}

// JourneyStats contains comprehensive journey statistics
type JourneyStats struct {
	JourneyID             string        `json:"journey_id"`
	Name                  string        `json:"name"`
	Description           string        `json:"description,omitempty"`
	Status                string        `json:"status"`
	
	// Enrollment stats
	TotalEnrolled         int64         `json:"total_enrolled"`
	ActiveEnrollments     int64         `json:"active_enrollments"`
	CompletedEnrollments  int64         `json:"completed_enrollments"`
	ConvertedEnrollments  int64         `json:"converted_enrollments"`
	
	// Time-based stats
	EnrolledToday         int64         `json:"enrolled_today"`
	EnrolledThisWeek      int64         `json:"enrolled_this_week"`
	CompletedToday        int64         `json:"completed_today"`
	
	// Node-level stats
	NodeStats             []NodeStat    `json:"node_stats"`
	
	// Performance metrics
	AvgCompletionTime     time.Duration `json:"avg_completion_time"`
	AvgCompletionTimeSecs float64       `json:"avg_completion_time_secs"` // For JSON serialization
	ConversionRate        float64       `json:"conversion_rate"`
	DropOffRate           float64       `json:"drop_off_rate"`
	
	// Email metrics (aggregated from journey emails)
	EmailsSent            int64         `json:"emails_sent"`
	EmailsOpened          int64         `json:"emails_opened"`
	EmailsClicked         int64         `json:"emails_clicked"`
	EmailsBounced         int64         `json:"emails_bounced"`
	OpenRate              float64       `json:"open_rate"`
	ClickRate             float64       `json:"click_rate"`
	
	// Segment breakdown
	SegmentStats          []SegmentStat `json:"segment_stats,omitempty"`
	
	// Journey metadata
	CreatedAt             time.Time     `json:"created_at"`
	ActivatedAt           *time.Time    `json:"activated_at,omitempty"`
	UpdatedAt             time.Time     `json:"updated_at"`
}

// NodeStat contains statistics for a single journey node
type NodeStat struct {
	NodeID        string        `json:"node_id"`
	NodeType      string        `json:"node_type"`
	NodeName      string        `json:"node_name"`
	EntriesCount  int64         `json:"entries_count"`
	ExitsCount    int64         `json:"exits_count"`
	AvgTimeInNode time.Duration `json:"avg_time_in_node"`
	AvgTimeSecs   float64       `json:"avg_time_in_node_secs"` // For JSON serialization
	ErrorCount    int64         `json:"error_count"`
	SuccessRate   float64       `json:"success_rate"`
	Position      int           `json:"position"` // Order in the funnel
}

// SegmentStat contains performance metrics for a segment within a journey
type SegmentStat struct {
	SegmentID      string  `json:"segment_id"`
	SegmentName    string  `json:"segment_name"`
	EnrolledCount  int64   `json:"enrolled_count"`
	ActiveCount    int64   `json:"active_count"`
	CompletedCount int64   `json:"completed_count"`
	ConvertedCount int64   `json:"converted_count"`
	ConversionRate float64 `json:"conversion_rate"`
	AvgTimeToConvert float64 `json:"avg_time_to_convert_hours"`
}

// DailyTrend contains daily metrics for trend analysis
type DailyTrend struct {
	Date               time.Time `json:"date"`
	DateStr            string    `json:"date_str"`
	Enrolled           int64     `json:"enrolled"`
	Completed          int64     `json:"completed"`
	Converted          int64     `json:"converted"`
	EmailsSent         int64     `json:"emails_sent"`
	EmailsOpened       int64     `json:"emails_opened"`
	EmailsClicked      int64     `json:"emails_clicked"`
	ConversionRate     float64   `json:"conversion_rate"`
	OpenRate           float64   `json:"open_rate"`
	ClickRate          float64   `json:"click_rate"`
}

// JourneyNode represents a node definition (for parsing journey config)
type JourneyNodeDef struct {
	ID          string                 `json:"id"`
	Type        string                 `json:"type"`
	Config      map[string]interface{} `json:"config"`
	Connections []string               `json:"connections"`
}

// NewJourneyMetrics creates a new journey metrics service
func NewJourneyMetrics(db *sql.DB) *JourneyMetrics {
	return &JourneyMetrics{
		db:       db,
		cache:    make(map[string]*JourneyStats),
		cacheTTL: 5 * time.Minute,
	}
}

// SetCacheTTL sets the cache time-to-live duration
func (jm *JourneyMetrics) SetCacheTTL(ttl time.Duration) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	jm.cacheTTL = ttl
}

// GetJourneyStats returns comprehensive statistics for a single journey
func (jm *JourneyMetrics) GetJourneyStats(ctx context.Context, journeyID string) (*JourneyStats, error) {
	// Check cache first
	jm.mu.RLock()
	if stats, ok := jm.cache[journeyID]; ok && time.Since(jm.lastUpdate) < jm.cacheTTL {
		jm.mu.RUnlock()
		return stats, nil
	}
	jm.mu.RUnlock()
	
	stats := &JourneyStats{
		JourneyID: journeyID,
	}
	
	// Get journey basic info
	var description sql.NullString
	var activatedAt sql.NullTime
	var nodesJSON, connectionsJSON sql.NullString
	err := jm.db.QueryRowContext(ctx, `
		SELECT 
			name, description, status, nodes, connections,
			created_at, updated_at, activated_at
		FROM mailing_journeys
		WHERE id = $1
	`, journeyID).Scan(
		&stats.Name, &description, &stats.Status,
		&nodesJSON, &connectionsJSON,
		&stats.CreatedAt, &stats.UpdatedAt, &activatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("journey not found: %w", err)
	}
	stats.Description = description.String
	if activatedAt.Valid {
		stats.ActivatedAt = &activatedAt.Time
	}
	
	// Get enrollment statistics
	err = jm.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) as total_enrolled,
			COUNT(*) FILTER (WHERE status = 'active') as active,
			COUNT(*) FILTER (WHERE status = 'completed') as completed,
			COUNT(*) FILTER (WHERE status = 'converted') as converted,
			COUNT(*) FILTER (WHERE enrolled_at >= CURRENT_DATE) as enrolled_today,
			COUNT(*) FILTER (WHERE enrolled_at >= CURRENT_DATE - INTERVAL '7 days') as enrolled_this_week,
			COUNT(*) FILTER (WHERE completed_at >= CURRENT_DATE AND status IN ('completed', 'converted')) as completed_today
		FROM mailing_journey_enrollments
		WHERE journey_id = $1
	`, journeyID).Scan(
		&stats.TotalEnrolled,
		&stats.ActiveEnrollments,
		&stats.CompletedEnrollments,
		&stats.ConvertedEnrollments,
		&stats.EnrolledToday,
		&stats.EnrolledThisWeek,
		&stats.CompletedToday,
	)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("JourneyMetrics: Error getting enrollment stats for %s: %v", journeyID, err)
	}
	
	// Calculate performance metrics
	if stats.TotalEnrolled > 0 {
		stats.ConversionRate = float64(stats.ConvertedEnrollments) / float64(stats.TotalEnrolled) * 100
		
		// Drop-off rate: enrollments that neither completed nor converted (excluding active)
		finishedCount := stats.CompletedEnrollments + stats.ConvertedEnrollments
		if stats.TotalEnrolled > stats.ActiveEnrollments {
			notFinished := stats.TotalEnrolled - stats.ActiveEnrollments - finishedCount
			stats.DropOffRate = float64(notFinished) / float64(stats.TotalEnrolled-stats.ActiveEnrollments) * 100
		}
	}
	
	// Get average completion time
	var avgCompletionSeconds sql.NullFloat64
	err = jm.db.QueryRowContext(ctx, `
		SELECT AVG(EXTRACT(EPOCH FROM (completed_at - enrolled_at)))
		FROM mailing_journey_enrollments
		WHERE journey_id = $1 AND status IN ('completed', 'converted') AND completed_at IS NOT NULL
	`, journeyID).Scan(&avgCompletionSeconds)
	if err == nil && avgCompletionSeconds.Valid {
		stats.AvgCompletionTime = time.Duration(avgCompletionSeconds.Float64) * time.Second
		stats.AvgCompletionTimeSecs = avgCompletionSeconds.Float64
	}
	
	// Get node-level statistics
	stats.NodeStats, _ = jm.GetNodeFunnelStats(ctx, journeyID)
	
	// Get email metrics from execution log and message tracking
	jm.populateEmailMetrics(ctx, stats, journeyID)
	
	// Update cache
	jm.mu.Lock()
	jm.cache[journeyID] = stats
	jm.lastUpdate = time.Now()
	jm.mu.Unlock()
	
	return stats, nil
}

// populateEmailMetrics fills in email-related metrics for a journey
func (jm *JourneyMetrics) populateEmailMetrics(ctx context.Context, stats *JourneyStats, journeyID string) {
	// Count emails sent through journey executions
	err := jm.db.QueryRowContext(ctx, `
		SELECT COUNT(*) 
		FROM mailing_journey_execution_log 
		WHERE journey_id = $1 AND node_type = 'email' AND result = 'success'
	`, journeyID).Scan(&stats.EmailsSent)
	if err != nil {
		log.Printf("JourneyMetrics: Error getting email sent count: %v", err)
	}
	
	// Get email engagement metrics from tracking events
	// This joins on subscriber email from enrollments to find related tracking events
	err = jm.db.QueryRowContext(ctx, `
		WITH journey_emails AS (
			SELECT DISTINCT e.subscriber_email
			FROM mailing_journey_enrollments e
			WHERE e.journey_id = $1
		)
		SELECT
			COUNT(*) FILTER (WHERE te.event_type = 'opened') as opens,
			COUNT(*) FILTER (WHERE te.event_type = 'clicked') as clicks,
			COUNT(*) FILTER (WHERE te.event_type = 'bounced') as bounces
		FROM mailing_tracking_events te
		JOIN mailing_subscribers s ON s.id = te.subscriber_id
		JOIN journey_emails je ON LOWER(s.email) = LOWER(je.subscriber_email)
		WHERE te.event_at >= (
			SELECT MIN(enrolled_at) FROM mailing_journey_enrollments WHERE journey_id = $1
		)
	`, journeyID).Scan(&stats.EmailsOpened, &stats.EmailsClicked, &stats.EmailsBounced)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("JourneyMetrics: Error getting email engagement: %v", err)
	}
	
	// Calculate rates
	if stats.EmailsSent > 0 {
		stats.OpenRate = float64(stats.EmailsOpened) / float64(stats.EmailsSent) * 100
		stats.ClickRate = float64(stats.EmailsClicked) / float64(stats.EmailsSent) * 100
	}
}

// GetAllJourneysStats returns statistics for all journeys in an organization
func (jm *JourneyMetrics) GetAllJourneysStats(ctx context.Context, orgID string) ([]*JourneyStats, error) {
	// Get all journey IDs for the organization
	rows, err := jm.db.QueryContext(ctx, `
		SELECT id FROM mailing_journeys 
		WHERE ($1 = '' OR organization_id = $1::uuid)
		ORDER BY updated_at DESC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch journeys: %w", err)
	}
	defer rows.Close()
	
	var journeyIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		journeyIDs = append(journeyIDs, id)
	}
	
	// Get stats for each journey
	var allStats []*JourneyStats
	for _, id := range journeyIDs {
		stats, err := jm.GetJourneyStats(ctx, id)
		if err != nil {
			log.Printf("JourneyMetrics: Error getting stats for journey %s: %v", id, err)
			continue
		}
		allStats = append(allStats, stats)
	}
	
	return allStats, nil
}

// GetNodeFunnelStats returns funnel statistics for each node in a journey
func (jm *JourneyMetrics) GetNodeFunnelStats(ctx context.Context, journeyID string) ([]NodeStat, error) {
	// First, get the journey nodes definition
	var nodesJSON sql.NullString
	err := jm.db.QueryRowContext(ctx, `
		SELECT nodes FROM mailing_journeys WHERE id = $1
	`, journeyID).Scan(&nodesJSON)
	if err != nil {
		return nil, fmt.Errorf("journey not found: %w", err)
	}
	
	var nodes []JourneyNodeDef
	if nodesJSON.Valid {
		json.Unmarshal([]byte(nodesJSON.String), &nodes)
	}
	
	// Build node ID to name/type mapping
	nodeInfo := make(map[string]JourneyNodeDef)
	for _, node := range nodes {
		nodeInfo[node.ID] = node
	}
	
	// Query execution log for node-level stats
	rows, err := jm.db.QueryContext(ctx, `
		SELECT 
			node_id,
			node_type,
			COUNT(*) as entries,
			COUNT(*) FILTER (WHERE result = 'success') as exits,
			COUNT(*) FILTER (WHERE result = 'error') as errors,
			AVG(CASE WHEN result = 'success' THEN 1.0 ELSE 0.0 END) * 100 as success_rate
		FROM mailing_journey_execution_log
		WHERE journey_id = $1
		GROUP BY node_id, node_type
		ORDER BY MIN(executed_at) ASC
	`, journeyID)
	if err != nil {
		return nil, fmt.Errorf("failed to get node stats: %w", err)
	}
	defer rows.Close()
	
	var nodeStats []NodeStat
	position := 0
	for rows.Next() {
		var stat NodeStat
		var successRate sql.NullFloat64
		err := rows.Scan(&stat.NodeID, &stat.NodeType, &stat.EntriesCount, &stat.ExitsCount, &stat.ErrorCount, &successRate)
		if err != nil {
			continue
		}
		
		// Get node name from config
		if info, ok := nodeInfo[stat.NodeID]; ok {
			if name, ok := info.Config["name"].(string); ok {
				stat.NodeName = name
			} else if subject, ok := info.Config["subject"].(string); ok {
				stat.NodeName = subject
			} else {
				stat.NodeName = fmt.Sprintf("%s Node", stat.NodeType)
			}
		}
		
		if successRate.Valid {
			stat.SuccessRate = successRate.Float64
		}
		
		stat.Position = position
		position++
		
		nodeStats = append(nodeStats, stat)
	}
	
	// Calculate average time in each node
	for i := range nodeStats {
		var avgTimeSecs sql.NullFloat64
		jm.db.QueryRowContext(ctx, `
			WITH node_times AS (
				SELECT 
					enrollment_id,
					executed_at as enter_time,
					LEAD(executed_at) OVER (PARTITION BY enrollment_id ORDER BY executed_at) as exit_time
				FROM mailing_journey_execution_log
				WHERE journey_id = $1 AND node_id = $2
			)
			SELECT AVG(EXTRACT(EPOCH FROM (exit_time - enter_time)))
			FROM node_times
			WHERE exit_time IS NOT NULL
		`, journeyID, nodeStats[i].NodeID).Scan(&avgTimeSecs)
		
		if avgTimeSecs.Valid {
			nodeStats[i].AvgTimeInNode = time.Duration(avgTimeSecs.Float64) * time.Second
			nodeStats[i].AvgTimeSecs = avgTimeSecs.Float64
		}
	}
	
	return nodeStats, nil
}

// GetJourneyTrends returns daily trend data for a journey over a specified number of days
func (jm *JourneyMetrics) GetJourneyTrends(ctx context.Context, journeyID string, days int) ([]DailyTrend, error) {
	if days <= 0 {
		days = 30
	}
	if days > 365 {
		days = 365
	}
	
	rows, err := jm.db.QueryContext(ctx, `
		WITH date_series AS (
			SELECT generate_series(
				CURRENT_DATE - INTERVAL '1 day' * $2,
				CURRENT_DATE,
				INTERVAL '1 day'
			)::date as date
		),
		enrollment_stats AS (
			SELECT 
				enrolled_at::date as date,
				COUNT(*) as enrolled,
				COUNT(*) FILTER (WHERE status IN ('completed', 'converted') AND completed_at::date = enrolled_at::date) as completed_same_day,
				COUNT(*) FILTER (WHERE status = 'converted' AND completed_at::date = enrolled_at::date) as converted_same_day
			FROM mailing_journey_enrollments
			WHERE journey_id = $1 AND enrolled_at >= CURRENT_DATE - INTERVAL '1 day' * $2
			GROUP BY enrolled_at::date
		),
		completion_stats AS (
			SELECT 
				completed_at::date as date,
				COUNT(*) FILTER (WHERE status IN ('completed', 'converted')) as completed,
				COUNT(*) FILTER (WHERE status = 'converted') as converted
			FROM mailing_journey_enrollments
			WHERE journey_id = $1 
				AND completed_at IS NOT NULL 
				AND completed_at >= CURRENT_DATE - INTERVAL '1 day' * $2
			GROUP BY completed_at::date
		),
		email_stats AS (
			SELECT 
				executed_at::date as date,
				COUNT(*) FILTER (WHERE node_type = 'email' AND result = 'success') as sent
			FROM mailing_journey_execution_log
			WHERE journey_id = $1 AND executed_at >= CURRENT_DATE - INTERVAL '1 day' * $2
			GROUP BY executed_at::date
		)
		SELECT 
			d.date,
			COALESCE(e.enrolled, 0) as enrolled,
			COALESCE(c.completed, 0) as completed,
			COALESCE(c.converted, 0) as converted,
			COALESCE(em.sent, 0) as emails_sent
		FROM date_series d
		LEFT JOIN enrollment_stats e ON e.date = d.date
		LEFT JOIN completion_stats c ON c.date = d.date
		LEFT JOIN email_stats em ON em.date = d.date
		ORDER BY d.date ASC
	`, journeyID, days)
	
	if err != nil {
		return nil, fmt.Errorf("failed to get journey trends: %w", err)
	}
	defer rows.Close()
	
	var trends []DailyTrend
	for rows.Next() {
		var trend DailyTrend
		err := rows.Scan(
			&trend.Date,
			&trend.Enrolled,
			&trend.Completed,
			&trend.Converted,
			&trend.EmailsSent,
		)
		if err != nil {
			continue
		}
		
		trend.DateStr = trend.Date.Format("2006-01-02")
		
		// Calculate rates
		if trend.Enrolled > 0 {
			trend.ConversionRate = float64(trend.Converted) / float64(trend.Enrolled) * 100
		}
		
		trends = append(trends, trend)
	}
	
	// Fill in email engagement metrics from tracking events
	jm.populateTrendEmailEngagement(ctx, trends, journeyID)
	
	return trends, nil
}

// populateTrendEmailEngagement fills in email open/click data for trends
func (jm *JourneyMetrics) populateTrendEmailEngagement(ctx context.Context, trends []DailyTrend, journeyID string) {
	if len(trends) == 0 {
		return
	}
	
	startDate := trends[0].Date
	endDate := trends[len(trends)-1].Date.Add(24 * time.Hour)
	
	// Create a map for quick lookup
	trendMap := make(map[string]*DailyTrend)
	for i := range trends {
		trendMap[trends[i].DateStr] = &trends[i]
	}
	
	// Query engagement events
	rows, err := jm.db.QueryContext(ctx, `
		WITH journey_subscribers AS (
			SELECT DISTINCT subscriber_email
			FROM mailing_journey_enrollments
			WHERE journey_id = $1
		)
		SELECT 
			te.event_at::date as date,
			COUNT(*) FILTER (WHERE te.event_type = 'opened') as opens,
			COUNT(*) FILTER (WHERE te.event_type = 'clicked') as clicks
		FROM mailing_tracking_events te
		JOIN mailing_subscribers s ON s.id = te.subscriber_id
		JOIN journey_subscribers js ON LOWER(s.email) = LOWER(js.subscriber_email)
		WHERE te.event_at >= $2 AND te.event_at < $3
		GROUP BY te.event_at::date
	`, journeyID, startDate, endDate)
	
	if err != nil {
		log.Printf("JourneyMetrics: Error getting trend engagement: %v", err)
		return
	}
	defer rows.Close()
	
	for rows.Next() {
		var date time.Time
		var opens, clicks int64
		if err := rows.Scan(&date, &opens, &clicks); err != nil {
			continue
		}
		
		dateStr := date.Format("2006-01-02")
		if trend, ok := trendMap[dateStr]; ok {
			trend.EmailsOpened = opens
			trend.EmailsClicked = clicks
			
			if trend.EmailsSent > 0 {
				trend.OpenRate = float64(opens) / float64(trend.EmailsSent) * 100
				trend.ClickRate = float64(clicks) / float64(trend.EmailsSent) * 100
			}
		}
	}
}

// GetTopPerformingJourneys returns the top performing journeys by conversion rate
func (jm *JourneyMetrics) GetTopPerformingJourneys(ctx context.Context, orgID string, limit int) ([]*JourneyStats, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	
	rows, err := jm.db.QueryContext(ctx, `
		WITH journey_metrics AS (
			SELECT 
				j.id,
				j.name,
				j.status,
				COUNT(e.id) as total_enrolled,
				COUNT(e.id) FILTER (WHERE e.status = 'converted') as converted,
				CASE 
					WHEN COUNT(e.id) > 0 
					THEN COUNT(e.id) FILTER (WHERE e.status = 'converted')::float / COUNT(e.id) * 100
					ELSE 0 
				END as conversion_rate
			FROM mailing_journeys j
			LEFT JOIN mailing_journey_enrollments e ON e.journey_id = j.id
			WHERE ($1 = '' OR j.organization_id = $1::uuid)
			  AND j.status IN ('active', 'completed')
			GROUP BY j.id, j.name, j.status
			HAVING COUNT(e.id) >= 10  -- Minimum enrollments for statistical significance
		)
		SELECT id
		FROM journey_metrics
		ORDER BY conversion_rate DESC, total_enrolled DESC
		LIMIT $2
	`, orgID, limit)
	
	if err != nil {
		return nil, fmt.Errorf("failed to get top journeys: %w", err)
	}
	defer rows.Close()
	
	var journeyIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		journeyIDs = append(journeyIDs, id)
	}
	
	// Get full stats for each journey
	var topJourneys []*JourneyStats
	for _, id := range journeyIDs {
		stats, err := jm.GetJourneyStats(ctx, id)
		if err != nil {
			continue
		}
		topJourneys = append(topJourneys, stats)
	}
	
	return topJourneys, nil
}

// GetSegmentPerformance returns performance breakdown by segment for a journey
func (jm *JourneyMetrics) GetSegmentPerformance(ctx context.Context, journeyID string) ([]SegmentStat, error) {
	// Get journey's associated segment if any
	var segmentID sql.NullString
	jm.db.QueryRowContext(ctx, `
		SELECT segment_id FROM mailing_journeys WHERE id = $1
	`, journeyID).Scan(&segmentID)
	
	// Query segment-based enrollment stats
	// This joins enrollments with subscribers and their segment memberships
	rows, err := jm.db.QueryContext(ctx, `
		WITH enrollment_segments AS (
			SELECT 
				e.id as enrollment_id,
				e.subscriber_email,
				e.status,
				e.enrolled_at,
				e.completed_at,
				s.id as segment_id,
				s.name as segment_name
			FROM mailing_journey_enrollments e
			JOIN mailing_subscribers sub ON LOWER(sub.email) = LOWER(e.subscriber_email)
			LEFT JOIN mailing_segments s ON s.list_id = sub.list_id
			WHERE e.journey_id = $1
		)
		SELECT 
			COALESCE(segment_id::text, 'no_segment') as segment_id,
			COALESCE(segment_name, 'No Segment') as segment_name,
			COUNT(*) as enrolled,
			COUNT(*) FILTER (WHERE status = 'active') as active,
			COUNT(*) FILTER (WHERE status IN ('completed', 'converted')) as completed,
			COUNT(*) FILTER (WHERE status = 'converted') as converted,
			CASE 
				WHEN COUNT(*) > 0 
				THEN COUNT(*) FILTER (WHERE status = 'converted')::float / COUNT(*) * 100
				ELSE 0 
			END as conversion_rate,
			AVG(
				CASE 
					WHEN status = 'converted' AND completed_at IS NOT NULL 
					THEN EXTRACT(EPOCH FROM (completed_at - enrolled_at)) / 3600.0
					ELSE NULL 
				END
			) as avg_hours_to_convert
		FROM enrollment_segments
		GROUP BY segment_id, segment_name
		ORDER BY conversion_rate DESC
	`, journeyID)
	
	if err != nil {
		return nil, fmt.Errorf("failed to get segment performance: %w", err)
	}
	defer rows.Close()
	
	var segmentStats []SegmentStat
	for rows.Next() {
		var stat SegmentStat
		var avgHoursToConvert sql.NullFloat64
		err := rows.Scan(
			&stat.SegmentID,
			&stat.SegmentName,
			&stat.EnrolledCount,
			&stat.ActiveCount,
			&stat.CompletedCount,
			&stat.ConvertedCount,
			&stat.ConversionRate,
			&avgHoursToConvert,
		)
		if err != nil {
			continue
		}
		if avgHoursToConvert.Valid {
			stat.AvgTimeToConvert = avgHoursToConvert.Float64
		}
		segmentStats = append(segmentStats, stat)
	}
	
	return segmentStats, nil
}

// RefreshCache forces a refresh of all cached journey statistics
func (jm *JourneyMetrics) RefreshCache() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	
	// Get all journey IDs
	rows, err := jm.db.QueryContext(ctx, `SELECT id FROM mailing_journeys`)
	if err != nil {
		return fmt.Errorf("failed to fetch journeys for cache refresh: %w", err)
	}
	defer rows.Close()
	
	var journeyIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		journeyIDs = append(journeyIDs, id)
	}
	
	// Clear existing cache
	jm.mu.Lock()
	jm.cache = make(map[string]*JourneyStats)
	jm.mu.Unlock()
	
	// Refresh stats for each journey
	for _, id := range journeyIDs {
		_, err := jm.GetJourneyStats(ctx, id)
		if err != nil {
			log.Printf("JourneyMetrics: Error refreshing cache for journey %s: %v", id, err)
		}
	}
	
	log.Printf("JourneyMetrics: Cache refreshed for %d journeys", len(journeyIDs))
	return nil
}

// GetJourneyComparison compares multiple journeys side by side
func (jm *JourneyMetrics) GetJourneyComparison(ctx context.Context, journeyIDs []string) ([]*JourneyStats, error) {
	var comparison []*JourneyStats
	
	for _, id := range journeyIDs {
		stats, err := jm.GetJourneyStats(ctx, id)
		if err != nil {
			continue
		}
		comparison = append(comparison, stats)
	}
	
	return comparison, nil
}

// GetNodeDropOffAnalysis returns detailed drop-off analysis for each node
func (jm *JourneyMetrics) GetNodeDropOffAnalysis(ctx context.Context, journeyID string) ([]NodeStat, error) {
	nodeStats, err := jm.GetNodeFunnelStats(ctx, journeyID)
	if err != nil {
		return nil, err
	}
	
	// Calculate drop-off between consecutive nodes
	for i := 1; i < len(nodeStats); i++ {
		prevEntries := nodeStats[i-1].EntriesCount
		currEntries := nodeStats[i].EntriesCount
		
		if prevEntries > 0 {
			dropOff := float64(prevEntries-currEntries) / float64(prevEntries) * 100
			// Store drop-off in success rate as inverse (retention rate)
			nodeStats[i].SuccessRate = 100 - dropOff
		}
	}
	
	return nodeStats, nil
}

// GetActiveEnrollmentsByNode returns count of currently active enrollments at each node
func (jm *JourneyMetrics) GetActiveEnrollmentsByNode(ctx context.Context, journeyID string) (map[string]int64, error) {
	rows, err := jm.db.QueryContext(ctx, `
		SELECT current_node_id, COUNT(*) as count
		FROM mailing_journey_enrollments
		WHERE journey_id = $1 AND status = 'active'
		GROUP BY current_node_id
	`, journeyID)
	if err != nil {
		return nil, fmt.Errorf("failed to get active enrollments by node: %w", err)
	}
	defer rows.Close()
	
	result := make(map[string]int64)
	for rows.Next() {
		var nodeID string
		var count int64
		if err := rows.Scan(&nodeID, &count); err != nil {
			continue
		}
		result[nodeID] = count
	}
	
	return result, nil
}

// GetRecentErrors returns recent execution errors for a journey
func (jm *JourneyMetrics) GetRecentErrors(ctx context.Context, journeyID string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 50
	}
	
	rows, err := jm.db.QueryContext(ctx, `
		SELECT 
			l.id, l.enrollment_id, l.node_id, l.node_type, 
			l.action, l.error_message, l.executed_at,
			e.subscriber_email
		FROM mailing_journey_execution_log l
		LEFT JOIN mailing_journey_enrollments e ON e.id = l.enrollment_id
		WHERE l.journey_id = $1 AND l.result = 'error'
		ORDER BY l.executed_at DESC
		LIMIT $2
	`, journeyID, limit)
	
	if err != nil {
		return nil, fmt.Errorf("failed to get recent errors: %w", err)
	}
	defer rows.Close()
	
	var errors []map[string]interface{}
	for rows.Next() {
		var id, enrollmentID, nodeID, nodeType, action string
		var errorMessage, subscriberEmail sql.NullString
		var executedAt time.Time
		
		err := rows.Scan(&id, &enrollmentID, &nodeID, &nodeType, &action, &errorMessage, &executedAt, &subscriberEmail)
		if err != nil {
			continue
		}
		
		errorInfo := map[string]interface{}{
			"id":            id,
			"enrollment_id": enrollmentID,
			"node_id":       nodeID,
			"node_type":     nodeType,
			"action":        action,
			"error_message": errorMessage.String,
			"executed_at":   executedAt,
		}
		if subscriberEmail.Valid {
			errorInfo["subscriber_email"] = subscriberEmail.String
		}
		errors = append(errors, errorInfo)
	}
	
	return errors, nil
}

// StartBackgroundRefresh starts periodic cache refresh
func (jm *JourneyMetrics) StartBackgroundRefresh(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		
		for range ticker.C {
			if err := jm.RefreshCache(); err != nil {
				log.Printf("JourneyMetrics: Background refresh error: %v", err)
			}
		}
	}()
}
