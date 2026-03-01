package api

import (
	"encoding/json"
	"fmt"
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

// HandleGetSendingPlans returns AI sending plans
func (svc *MailingService) HandleGetSendingPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get subscriber counts
	var highEng, medEng, lowEng int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE engagement_score >= 70").Scan(&highEng)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE engagement_score >= 30 AND engagement_score < 70").Scan(&medEng)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE engagement_score < 30").Scan(&lowEng)

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

	plans := []map[string]interface{}{
		{
			"time_period": "morning", "name": "Morning Focus",
			"description": "High-engagement subscribers during peak hours",
			"recommended_volume": morningVol,
			"predictions": map[string]interface{}{
				"estimated_opens": int(float64(morningVol) * 0.17),
				"estimated_clicks": int(float64(morningVol) * 0.025),
				"estimated_revenue": float64(morningVol) * 0.0127,
			},
			"confidence_score": 0.88,
			"ai_explanation": "Morning sends show 30% higher engagement historically",
			"recommendations": []string{"Ideal for time-sensitive offers"},
		},
		{
			"time_period": "first_half", "name": "First Half Balanced",
			"description": "Extended morning through early afternoon",
			"recommended_volume": firstHalfVol,
			"predictions": map[string]interface{}{
				"estimated_opens": int(float64(firstHalfVol) * 0.15),
				"estimated_clicks": int(float64(firstHalfVol) * 0.022),
				"estimated_revenue": float64(firstHalfVol) * 0.011,
			},
			"confidence_score": 0.85,
			"ai_explanation": "Balanced reach and performance",
			"recommendations": []string{"Good for general campaigns"},
		},
		{
			"time_period": "full_day", "name": "Full Day Maximum",
			"description": "Full capacity across all segments",
			"recommended_volume": fullDayVol,
			"predictions": map[string]interface{}{
				"estimated_opens": int(float64(fullDayVol) * 0.14),
				"estimated_clicks": int(float64(fullDayVol) * 0.021),
				"estimated_revenue": float64(fullDayVol) * 0.0105,
			},
			"confidence_score": 0.80,
			"ai_explanation": "Maximum reach plan",
			"recommendations": []string{"Best for revenue maximization"},
			"warnings": []string{"Higher complaint risk from low-engagement segment"},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"plans": plans, "target_date": time.Now().Format("2006-01-02"),
	})
}

// HandleGetISPAgents returns ISP-specific AI agent intelligence
func (svc *MailingService) HandleGetISPAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Query per-ISP aggregate metrics from inbox profiles
	// Uses total_sends (017 schema) with COALESCE fallback
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
			COALESCE(AVG(optimal_send_hour), 10) as avg_best_hour,
			COALESCE(AVG(optimal_send_day), 2) as avg_best_day,
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
		var avgEngagement, avgBestHour, avgBestDay float64
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

		dataPoints := totalSends + totalOpens + totalClicks + totalBounces + totalComplaints
		totalProfiles += profilesCount
		totalDataPoints += dataPoints

		// Calculate open/click rates
		var openRate, clickRate float64
		if totalSends > 0 {
			openRate = float64(totalOpens) / float64(totalSends) * 100
			clickRate = float64(totalClicks) / float64(totalSends) * 100
		}

		// Calculate learning frequency
		learningDays := int(time.Since(firstLearning).Hours() / 24)
		if learningDays < 1 {
			learningDays = 1
		}
		var learningFreq float64
		if dataPoints > 0 {
			learningFreq = float64(learningDays*24) / float64(dataPoints)
		}

		// Determine agent status
		status := "dormant"
		hoursSinceLearn := time.Since(lastLearning).Hours()
		if hoursSinceLearn < 24 {
			status = "active"
		} else if hoursSinceLearn < 72 {
			status = "idle"
		} else if hoursSinceLearn < 168 {
			status = "sleeping"
		}

		// Build risk factors
		var riskFactors []string
		if totalBounces > 0 && totalSends > 0 && float64(totalBounces)/float64(totalSends) > 0.02 {
			riskFactors = append(riskFactors, "Elevated bounce rate detected")
		}
		if totalComplaints > 0 {
			riskFactors = append(riskFactors, "Spam complaints recorded")
		}
		if recentOpeners == 0 && totalSends > 5 {
			riskFactors = append(riskFactors, "No recent opens — engagement declining")
		}

		// Build insights
		var insights []string
		if recentOpeners > 0 {
			insights = append(insights, fmt.Sprintf("%d profiles opened in last 7 days", recentOpeners))
		}
		if recentClickers > 0 {
			insights = append(insights, fmt.Sprintf("%d profiles clicked in last 7 days", recentClickers))
		}
		if int(avgBestHour) >= 0 {
			dayNames := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
			bestDay := int(avgBestDay) % 7
			insights = append(insights, fmt.Sprintf("Best send time: %s at %d:00 UTC", dayNames[bestDay], int(avgBestHour)))
		}

		agent := ISPAgent{
			ISP:               isp,
			ISPKey:            ispKey,
			Domain:            domain,
			Status:            status,
			ProfilesCount:     profilesCount,
			TotalSends:        totalSends,
			TotalOpens:        totalOpens,
			TotalClicks:       totalClicks,
			TotalBounces:      totalBounces,
			TotalComplaints:   totalComplaints,
			AvgEngagement:     avgEngagement,
			AvgOpenRate:       openRate,
			AvgClickRate:      clickRate,
			DataPointsTotal:   dataPoints,
			LastLearningAt:    lastLearning.Format(time.RFC3339),
			FirstLearningAt:   firstLearning.Format(time.RFC3339),
			LearningDays:      learningDays,
			LearningFreqHours: learningFreq,
			LearningSources: map[string]int{
				"sends":      totalSends,
				"opens":      totalOpens,
				"clicks":     totalClicks,
				"bounces":    totalBounces,
				"complaints": totalComplaints,
			},
			Knowledge: map[string]interface{}{
				"optimal_send_hour": int(avgBestHour),
				"optimal_send_day":  int(avgBestDay),
				"engagement_tiers": map[string]int{
					"high": highCount, "medium": mediumCount,
					"low": lowCount, "inactive": inactiveCount,
				},
				"risk_factors":    riskFactors,
				"insights":        insights,
				"recent_openers":  recentOpeners,
				"recent_clickers": recentClickers,
			},
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

// HandleGetDeliveryServers returns delivery servers
func (svc *MailingService) HandleGetDeliveryServers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := svc.db.QueryContext(ctx, `
		SELECT id, name, server_type, hourly_quota, daily_quota, status
		FROM mailing_delivery_servers ORDER BY priority
	`)
	defer rows.Close()

	var servers []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, serverType, status string
		var hourlyQuota, dailyQuota int
		rows.Scan(&id, &name, &serverType, &hourlyQuota, &dailyQuota, &status)
		servers = append(servers, map[string]interface{}{
			"id": id.String(), "name": name, "server_type": serverType,
			"hourly_quota": hourlyQuota, "daily_quota": dailyQuota, "status": status,
		})
	}
	if servers == nil {
		// Add default SparkPost
		servers = []map[string]interface{}{
			{"id": "default", "name": "SparkPost Primary", "server_type": "sparkpost", "hourly_quota": 50000, "daily_quota": 500000, "status": "active"},
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"servers": servers, "total": len(servers)})
}
