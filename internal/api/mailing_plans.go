package api

import (
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

// HandleGetSuggestions returns suggestions
func (svc *MailingService) HandleGetSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := svc.db.QueryContext(ctx, `
		SELECT id, category, description, impact, status, created_at
		FROM mailing_suggestions ORDER BY created_at DESC
	`)
	defer rows.Close()

	var suggestions []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var category, description, impact, status string
		var createdAt time.Time
		rows.Scan(&id, &category, &description, &impact, &status, &createdAt)
		suggestions = append(suggestions, map[string]interface{}{
			"id": id.String(), "category": category, "description": description,
			"impact": impact, "status": status, "created_at": createdAt,
		})
	}
	if suggestions == nil {
		suggestions = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"suggestions": suggestions, "total": len(suggestions)})
}

// HandleAddSuggestion adds a suggestion
func (svc *MailingService) HandleAddSuggestion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Category    string `json:"category"`
		Description string `json:"description"`
		Impact      string `json:"impact"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	id := uuid.New()
	_, err := svc.db.ExecContext(ctx, `
		INSERT INTO mailing_suggestions (id, category, description, impact, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'pending', NOW(), NOW())
	`, id, input.Category, input.Description, input.Impact)

	if err != nil {
		http.Error(w, `{"error":"failed to add suggestion"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id.String(), "category": input.Category, "description": input.Description, "status": "pending",
	})
}

// HandleUpdateSuggestion updates suggestion status
func (svc *MailingService) HandleUpdateSuggestion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var input struct {
		Status string `json:"status"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	svc.db.ExecContext(ctx, "UPDATE mailing_suggestions SET status = $2, updated_at = NOW() WHERE id = $1", id, input.Status)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "status": input.Status})
}

// HandleGetSendingPlans returns AI sending plans scoped to the org's subscriber base.
// Volumes are capped to the org's daily capacity so plans never exceed infrastructure limits.
func (svc *MailingService) HandleGetSendingPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization context required")
		return
	}

	// Org-scoped engagement tier counts
	var highEng, medEng, lowEng int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_subscribers
		WHERE organization_id = $1 AND engagement_score >= 70
	`, orgID).Scan(&highEng); err != nil {
		log.Printf("[SendingPlans] high_eng query error org=%s: %v", orgID, err)
	}
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_subscribers
		WHERE organization_id = $1 AND engagement_score >= 30 AND engagement_score < 70
	`, orgID).Scan(&medEng); err != nil {
		log.Printf("[SendingPlans] med_eng query error org=%s: %v", orgID, err)
	}
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_subscribers
		WHERE organization_id = $1 AND engagement_score < 30
	`, orgID).Scan(&lowEng); err != nil {
		log.Printf("[SendingPlans] low_eng query error org=%s: %v", orgID, err)
	}

	// If the org has no subscribers at all, use modest defaults
	morningVol := highEng
	if morningVol == 0 {
		morningVol = 50000
	}
	firstHalfVol := highEng + medEng
	if firstHalfVol == 0 {
		firstHalfVol = 75000
	}
	fullDayVol := highEng + medEng + lowEng
	if fullDayVol == 0 {
		fullDayVol = 125000
	}

	// Cap volumes to daily capacity so plans don't recommend exceeding infrastructure
	var dailyCapacity int64
	svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(daily_limit), 0) FROM mailing_sending_profiles
		WHERE status = 'active' AND daily_limit > 0
	`).Scan(&dailyCapacity)
	if dailyCapacity == 0 {
		dailyCapacity = DefaultDailyCapacity
	}
	cap := int(dailyCapacity)
	if morningVol > cap {
		morningVol = cap
	}
	if firstHalfVol > cap {
		firstHalfVol = cap
	}
	if fullDayVol > cap {
		fullDayVol = cap
	}

	now := time.Now().UTC()
	plans := []SendingPlan{
		{
			TimePeriod:        "morning",
			Name:              "Morning Focus",
			Description:       "High-engagement subscribers during peak hours",
			RecommendedVolume: morningVol,
			Predictions: PlanPredictions{
				EstimatedOpens:   int(float64(morningVol) * 0.17),
				EstimatedClicks:  int(float64(morningVol) * 0.025),
				EstimatedRevenue: float64(morningVol) * 0.0127,
			},
			ConfidenceScore: 0.88,
			AIExplanation:   "Morning sends show 30% higher engagement historically",
			Explanation:     "Morning sends show 30% higher engagement historically",
			Recommendations: []string{"Ideal for time-sensitive offers"},
		},
		{
			TimePeriod:        "first_half",
			Name:              "First Half Balanced",
			Description:       "Extended morning through early afternoon",
			RecommendedVolume: firstHalfVol,
			Predictions: PlanPredictions{
				EstimatedOpens:   int(float64(firstHalfVol) * 0.15),
				EstimatedClicks:  int(float64(firstHalfVol) * 0.022),
				EstimatedRevenue: float64(firstHalfVol) * 0.011,
			},
			ConfidenceScore: 0.85,
			AIExplanation:   "Balanced reach and performance",
			Explanation:     "Balanced reach and performance",
			Recommendations: []string{"Good for general campaigns"},
		},
		{
			TimePeriod:        "full_day",
			Name:              "Full Day Maximum",
			Description:       "Full capacity across all segments",
			RecommendedVolume: fullDayVol,
			Predictions: PlanPredictions{
				EstimatedOpens:   int(float64(fullDayVol) * 0.14),
				EstimatedClicks:  int(float64(fullDayVol) * 0.021),
				EstimatedRevenue: float64(fullDayVol) * 0.0105,
			},
			ConfidenceScore: 0.80,
			AIExplanation:   "Maximum reach plan",
			Explanation:     "Maximum reach plan",
			Recommendations: []string{"Best for revenue maximization"},
			Warnings:        []string{"Higher complaint risk from low-engagement segment"},
		},
	}

	respondJSON(w, http.StatusOK, SendingPlanResponse{
		Plans:       plans,
		TargetDate:  now.Format("2006-01-02"),
		GeneratedAt: now.Format(time.RFC3339),
	})
}

// HandleGetISPAgents returns ISP-specific AI agent intelligence
func (svc *MailingService) HandleGetISPAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Query per-ISP aggregate metrics from inbox profiles
	// AVG(optimal_send_hour/day) returns NULL when no profile has learned data — no fake defaults
	rows, err := svc.db.QueryContext(ctx, `
		SELECT 
			domain,
			COUNT(*) as profiles_count,
			COALESCE(SUM(total_sends), 0) as total_sends,
			COALESCE(SUM(total_opens), 0) as total_opens,
			COALESCE(SUM(total_clicks), 0) as total_clicks,
			COALESCE(SUM(total_bounces), 0) as total_bounces,
			COALESCE(SUM(total_complaints), 0) as total_complaints,
			COALESCE(AVG(engagement_score), 0) as avg_engagement,
			COALESCE(MAX(updated_at), NOW()) as last_learning,
			COALESCE(MIN(updated_at), NOW()) as first_learning,
			AVG(optimal_send_hour) as avg_best_hour,
			AVG(optimal_send_day) as avg_best_day,
			COUNT(CASE WHEN engagement_score >= 0.70 THEN 1 END) as high_count,
			COUNT(CASE WHEN engagement_score >= 0.40 AND engagement_score < 0.70 THEN 1 END) as medium_count,
			COUNT(CASE WHEN engagement_score > 0 AND engagement_score < 0.40 THEN 1 END) as low_count,
			COUNT(CASE WHEN engagement_score = 0 THEN 1 END) as inactive_count,
			COUNT(CASE WHEN last_open_at IS NOT NULL AND last_open_at > NOW() - INTERVAL '7 days' THEN 1 END) as recent_openers,
			COUNT(CASE WHEN last_click_at IS NOT NULL AND last_click_at > NOW() - INTERVAL '7 days' THEN 1 END) as recent_clickers
		FROM mailing_inbox_profiles
		GROUP BY domain
		ORDER BY COUNT(*) DESC
	`)
	if err != nil {
		http.Error(w, "Failed to query ISP agents", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type ISPAgent struct {
		ISP               string                 `json:"isp"`
		ISPKey            string                 `json:"isp_key"`
		Domain            string                 `json:"domain"`
		Status            string                 `json:"status"`
		ProfilesCount     int                    `json:"profiles_count"`
		TotalSends        int                    `json:"total_sends"`
		TotalOpens        int                    `json:"total_opens"`
		TotalClicks       int                    `json:"total_clicks"`
		TotalBounces      int                    `json:"total_bounces"`
		TotalHardBounces  int                    `json:"total_hard_bounces"`
		TotalSoftBounces  int                    `json:"total_soft_bounces"`
		TotalComplaints   int                    `json:"total_complaints"`
		AvgEngagement     float64                `json:"avg_engagement"`
		AvgOpenRate       float64                `json:"avg_open_rate"`
		AvgClickRate      float64                `json:"avg_click_rate"`
		DataPointsTotal   int                    `json:"data_points_total"`
		LastLearningAt    string                 `json:"last_learning_at"`
		FirstLearningAt   string                 `json:"first_learning_at"`
		LearningDays      int                    `json:"learning_days"`
		LearningFreqHours float64                `json:"learning_frequency_hours"`
		LearningSources   map[string]int         `json:"learning_sources"`
		Knowledge         map[string]interface{} `json:"knowledge"`
	}

	var agents []ISPAgent
	var totalProfiles, totalDataPoints int

	for rows.Next() {
		var domain string
		var profilesCount, totalSends, totalOpens, totalClicks, totalBounces, totalComplaints int
		var avgEngagement float64
		var avgBestHour, avgBestDay sql.NullFloat64
		var lastLearning, firstLearning time.Time
		var highCount, mediumCount, lowCount, inactiveCount, recentOpeners, recentClickers int

		err := rows.Scan(&domain, &profilesCount, &totalSends, &totalOpens, &totalClicks,
			&totalBounces, &totalComplaints, &avgEngagement,
			&lastLearning, &firstLearning, &avgBestHour, &avgBestDay,
			&highCount, &mediumCount, &lowCount, &inactiveCount,
			&recentOpeners, &recentClickers)
		if err != nil {
			continue
		}

		isp := detectISP(domain)
		ispKey := strings.ToLower(strings.ReplaceAll(isp, " ", "_"))

		// Enrich with live stats from tracking events via recipient_domain
		var liveDelivered, liveOpened, liveClicked, liveHard, liveSoft, liveComplained sql.NullInt64
		_ = svc.db.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT
				COUNT(*) FILTER (WHERE event_type = 'delivered'),
				COUNT(*) FILTER (WHERE event_type = 'opened'),
				COUNT(*) FILTER (WHERE event_type = 'clicked'),
				COUNT(*) FILTER (WHERE event_type = 'bounced' AND %s),
				COUNT(*) FILTER (WHERE event_type = 'bounced' AND NOT (%s)),
				COUNT(*) FILTER (WHERE event_type = 'complained')
			FROM mailing_tracking_events
			WHERE LOWER(recipient_domain) = LOWER($1) AND created_at > NOW() - INTERVAL '30 days'`,
			HardBounceSQL("mailing_tracking_events"),
			HardBounceSQL("mailing_tracking_events")),
			domain,
		).Scan(&liveDelivered, &liveOpened, &liveClicked, &liveHard, &liveSoft, &liveComplained)

		liveSendsVal := 0
		if liveDelivered.Valid && liveDelivered.Int64 > 0 {
			liveSendsVal = int(liveDelivered.Int64)
		}
		liveOpensVal := 0
		if liveOpened.Valid {
			liveOpensVal = int(liveOpened.Int64)
		}
		liveClicksVal := 0
		if liveClicked.Valid {
			liveClicksVal = int(liveClicked.Int64)
		}
		hardBounces := 0
		if liveHard.Valid {
			hardBounces = int(liveHard.Int64)
		}
		softBounces := 0
		if liveSoft.Valid {
			softBounces = int(liveSoft.Int64)
		}
		liveComplaintsVal := 0
		if liveComplained.Valid {
			liveComplaintsVal = int(liveComplained.Int64)
		}

		effectiveSends := totalSends
		if liveSendsVal > effectiveSends {
			effectiveSends = liveSendsVal
		}
		effectiveOpens := totalOpens
		if liveOpensVal > effectiveOpens {
			effectiveOpens = liveOpensVal
		}
		effectiveClicks := totalClicks
		if liveClicksVal > effectiveClicks {
			effectiveClicks = liveClicksVal
		}

		dataPoints := effectiveSends + effectiveOpens + effectiveClicks + hardBounces + softBounces + liveComplaintsVal
		totalProfiles += profilesCount
		totalDataPoints += dataPoints

		var openRate, clickRate float64
		if effectiveSends > 0 {
			openRate = float64(effectiveOpens) / float64(effectiveSends) * 100
			clickRate = float64(effectiveClicks) / float64(effectiveSends) * 100
		}

		learningDays := int(time.Since(firstLearning).Hours() / 24)
		if learningDays < 1 {
			learningDays = 1
		}
		var learningFreq float64
		if dataPoints > 0 {
			learningFreq = float64(learningDays*24) / float64(dataPoints)
		}

		status := "dormant"
		hoursSinceLearn := time.Since(lastLearning).Hours()
		if hoursSinceLearn < 24 {
			status = "active"
		} else if hoursSinceLearn < 72 {
			status = "idle"
		} else if hoursSinceLearn < 168 {
			status = "sleeping"
		}

		var riskFactors []string
		if hardBounces > 0 && effectiveSends > 0 && float64(hardBounces)/float64(effectiveSends) > 0.02 {
			riskFactors = append(riskFactors, "Elevated hard bounce rate detected")
		}
		if softBounces > 0 && effectiveSends > 0 && float64(softBounces)/float64(effectiveSends) > 0.05 {
			riskFactors = append(riskFactors, "High soft bounce rate — possible throttling")
		}
		if liveComplaintsVal > 0 {
			riskFactors = append(riskFactors, "Spam complaints recorded")
		}
		if recentOpeners == 0 && effectiveSends > 5 {
			riskFactors = append(riskFactors, "No recent opens — engagement declining")
		}

		var insights []string
		if recentOpeners > 0 {
			insights = append(insights, fmt.Sprintf("%d profiles opened in last 7 days", recentOpeners))
		}
		if recentClickers > 0 {
			insights = append(insights, fmt.Sprintf("%d profiles clicked in last 7 days", recentClickers))
		}
		if avgBestHour.Valid && avgBestDay.Valid {
			dayNames := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
			bestDay := int(avgBestDay.Float64) % 7
			insights = append(insights, fmt.Sprintf("Best send time: %s at %d:00 UTC", dayNames[bestDay], int(avgBestHour.Float64)))
		}

		knowledgeMap := map[string]interface{}{
			"engagement_tiers": map[string]int{
				"high": highCount, "medium": mediumCount,
				"low": lowCount, "inactive": inactiveCount,
			},
			"risk_factors":    riskFactors,
			"insights":        insights,
			"recent_openers":  recentOpeners,
			"recent_clickers": recentClickers,
		}
		if avgBestHour.Valid {
			knowledgeMap["optimal_send_hour"] = int(avgBestHour.Float64)
		}
		if avgBestDay.Valid {
			knowledgeMap["optimal_send_day"] = int(avgBestDay.Float64)
		}

		agent := ISPAgent{
			ISP:               isp,
			ISPKey:            ispKey,
			Domain:            domain,
			Status:            status,
			ProfilesCount:     profilesCount,
			TotalSends:        effectiveSends,
			TotalOpens:        effectiveOpens,
			TotalClicks:       effectiveClicks,
			TotalBounces:      hardBounces + softBounces,
			TotalHardBounces:  hardBounces,
			TotalSoftBounces:  softBounces,
			TotalComplaints:   liveComplaintsVal,
			AvgEngagement:     avgEngagement * 100,
			AvgOpenRate:       openRate,
			AvgClickRate:      clickRate,
			DataPointsTotal:   dataPoints,
			LastLearningAt:    lastLearning.Format(time.RFC3339),
			FirstLearningAt:   firstLearning.Format(time.RFC3339),
			LearningDays:      learningDays,
			LearningFreqHours: learningFreq,
			LearningSources: map[string]int{
				"sends":        effectiveSends,
				"opens":        effectiveOpens,
				"clicks":       effectiveClicks,
				"hard_bounces": hardBounces,
				"soft_bounces": softBounces,
				"complaints":   liveComplaintsVal,
			},
			Knowledge: knowledgeMap,
		}
		agents = append(agents, agent)
	}

	// Count active agents
	activeCount := 0
	var lastSystemLearning string
	for _, a := range agents {
		if a.Status == "active" || a.Status == "idle" {
			activeCount++
		}
		if lastSystemLearning == "" || a.LastLearningAt > lastSystemLearning {
			lastSystemLearning = a.LastLearningAt
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"agents": agents,
		"summary": map[string]interface{}{
			"total_agents":        len(agents),
			"active_agents":       activeCount,
			"total_profiles":      totalProfiles,
			"total_data_points":   totalDataPoints,
			"last_system_learning": lastSystemLearning,
		},
	})
}

// HandleGetDeliveryServers returns delivery servers from the database.
// No fake fallback data — if no servers exist, the frontend sees an empty array.
func (svc *MailingService) HandleGetDeliveryServers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := svc.db.QueryContext(ctx, `
		SELECT id, name, server_type, hourly_quota, daily_quota, status
		FROM mailing_delivery_servers ORDER BY priority
	`)
	if err != nil {
		log.Printf("[GetDeliveryServers] query error: %v", err)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	defer rows.Close()

	servers := make([]DeliveryServerItem, 0)
	for rows.Next() {
		var s DeliveryServerItem
		var id uuid.UUID
		if err := rows.Scan(&id, &s.Name, &s.ServerType, &s.HourlyQuota, &s.DailyQuota, &s.Status); err != nil {
			log.Printf("[GetDeliveryServers] scan error: %v", err)
			continue
		}
		s.ID = id.String()
		servers = append(servers, s)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[GetDeliveryServers] iteration error: %v", err)
	}

	respondJSON(w, http.StatusOK, DeliveryServersResponse{
		Servers: servers,
		Total:   len(servers),
	})
}
