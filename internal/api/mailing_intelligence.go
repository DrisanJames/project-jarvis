package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// HandleListRecommendations provides AI recommendations based on an uploaded list
func (svc *MailingService) HandleListRecommendations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "listId")
	
	// Get list stats
	var listName string
	var subscriberCount int
	err := svc.db.QueryRowContext(ctx, `
		SELECT name, subscriber_count FROM mailing_lists WHERE id = $1
	`, listID).Scan(&listName, &subscriberCount)
	
	if err != nil {
		http.Error(w, `{"error":"list not found"}`, http.StatusNotFound)
		return
	}
	
	// Analyze subscriber engagement distribution
	var highEng, medEng, lowEng, noEng int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND engagement_score >= 70", listID).Scan(&highEng)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND engagement_score >= 40 AND engagement_score < 70", listID).Scan(&medEng)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND engagement_score > 0 AND engagement_score < 40", listID).Scan(&lowEng)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND (engagement_score = 0 OR engagement_score IS NULL)", listID).Scan(&noEng)
	
	// Analyze domain distribution
	domainRows, _ := svc.db.QueryContext(ctx, `
		SELECT SPLIT_PART(email, '@', 2) as domain, COUNT(*) as cnt
		FROM mailing_subscribers WHERE list_id = $1
		GROUP BY SPLIT_PART(email, '@', 2)
		ORDER BY cnt DESC LIMIT 10
	`, listID)
	defer domainRows.Close()
	
	var topDomains []map[string]interface{}
	for domainRows.Next() {
		var domain string
		var count int
		domainRows.Scan(&domain, &count)
		topDomains = append(topDomains, map[string]interface{}{
			"domain": domain, "count": count, "percentage": round2(float64(count)/float64(subscriberCount)*100),
		})
	}
	
	// Get optimal send time based on list engagement
	var avgBestHour float64
	svc.db.QueryRowContext(ctx, `
		SELECT AVG(ip.best_send_hour) 
		FROM mailing_inbox_profiles ip
		JOIN mailing_subscribers s ON LOWER(s.email) = ip.email
		WHERE s.list_id = $1
	`, listID).Scan(&avgBestHour)
	
	// Calculate health score
	healthScore := 100.0
	if subscriberCount > 0 {
		highEngPct := float64(highEng) / float64(subscriberCount) * 100
		noEngPct := float64(noEng) / float64(subscriberCount) * 100
		healthScore = (highEngPct * 2) + (float64(medEng)/float64(subscriberCount)*100) - (noEngPct * 0.5)
		if healthScore > 100 { healthScore = 100 }
		if healthScore < 0 { healthScore = 0 }
	}
	
	// Generate recommendations
	recommendations := []map[string]interface{}{}
	
	// Segmentation recommendations
	if highEng > 0 {
		recommendations = append(recommendations, map[string]interface{}{
			"type":        "segment",
			"priority":    "high",
			"title":       "Create VIP Segment",
			"description": fmt.Sprintf("You have %d high-engagement subscribers. Create a VIP segment for exclusive offers and early access.", highEng),
			"action":      "Create segment with engagement_score >= 70",
		})
	}
	
	if lowEng+noEng > subscriberCount/4 {
		recommendations = append(recommendations, map[string]interface{}{
			"type":        "re-engagement",
			"priority":    "high",
			"title":       "Run Re-engagement Campaign",
			"description": fmt.Sprintf("%d subscribers have low/no engagement. Run a re-engagement campaign before they go stale.", lowEng+noEng),
			"action":      "Create automation for subscribers with engagement_score < 40",
		})
	}
	
	// Send time recommendation
	optimalHour := int(avgBestHour)
	if optimalHour == 0 { optimalHour = 10 } // Default
	recommendations = append(recommendations, map[string]interface{}{
		"type":        "timing",
		"priority":    "medium",
		"title":       "Optimal Send Time",
		"description": fmt.Sprintf("Based on subscriber engagement patterns, send around %d:00 UTC (Tuesday-Thursday) for best results.", optimalHour),
		"action":      fmt.Sprintf("Schedule campaigns for %d:00 UTC", optimalHour),
	})
	
	// Domain-specific recommendations
	if len(topDomains) > 0 {
		topDomain := topDomains[0]["domain"].(string)
		domainPct := topDomains[0]["percentage"].(float64)
		if domainPct > 50 {
			recommendations = append(recommendations, map[string]interface{}{
				"type":        "deliverability",
				"priority":    "medium",
				"title":       "High Domain Concentration",
				"description": fmt.Sprintf("%.0f%% of your list is %s. Monitor deliverability closely for this provider.", domainPct, topDomain),
				"action":      "Set up inbox placement monitoring",
			})
		}
	}
	
	// Volume recommendation
	safeVolume := subscriberCount
	if healthScore < 70 {
		safeVolume = int(float64(subscriberCount) * 0.7) // Reduce volume for lower health lists
		recommendations = append(recommendations, map[string]interface{}{
			"type":        "volume",
			"priority":    "high",
			"title":       "Throttle Send Volume",
			"description": fmt.Sprintf("List health is %.0f%%. Consider sending to engaged subscribers first (%d of %d).", healthScore, safeVolume, subscriberCount),
			"action":      "Create segment: engagement_score >= 40",
		})
	}
	
	// A/B test recommendation
	if subscriberCount >= 1000 {
		recommendations = append(recommendations, map[string]interface{}{
			"type":        "testing",
			"priority":    "medium",
			"title":       "Run A/B Tests",
			"description": "List size supports meaningful A/B testing. Test subject lines and send times.",
			"action":      "Create A/B test with 20% sample size",
		})
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"list_id":   listID,
		"list_name": listName,
		"analysis": map[string]interface{}{
			"subscriber_count": subscriberCount,
			"health_score":     round2(healthScore),
			"engagement_distribution": map[string]int{
				"high":     highEng,
				"medium":   medEng,
				"low":      lowEng,
				"inactive": noEng,
			},
			"top_domains":       topDomains,
			"optimal_send_hour": optimalHour,
		},
		"recommendations":      recommendations,
		"recommended_volume":   safeVolume,
		"confidence_score":     0.85,
	})
}

// HandleCampaignAnalytics returns campaign analytics
func (svc *MailingService) HandleCampaignAnalytics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")

	var name, subject, status string
	var sent, opens, clicks, bounces int
	var revenue float64
	var startedAt *time.Time

	err := svc.db.QueryRowContext(ctx, `
		SELECT name, subject, status, sent_count, open_count, click_count, bounce_count, revenue, started_at
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&name, &subject, &status, &sent, &opens, &clicks, &bounces, &revenue, &startedAt)

	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}

	openRate := 0.0
	clickRate := 0.0
	bounceRate := 0.0
	ctor := 0.0
	if sent > 0 {
		openRate = float64(opens) / float64(sent) * 100
		clickRate = float64(clicks) / float64(sent) * 100
		bounceRate = float64(bounces) / float64(sent) * 100
	}
	if opens > 0 {
		ctor = float64(clicks) / float64(opens) * 100
	}

	// Generate recommendations
	recs := []string{}
	if openRate < 10 {
		recs = append(recs, "⚠️ Open rate below industry average (15-25%). Consider: shorter subject lines, personalization, urgency")
	} else if openRate < 15 {
		recs = append(recs, "📊 Open rate slightly below average. Test: emojis in subject, sender name variations")
	} else if openRate > 25 {
		recs = append(recs, "✅ Excellent open rate! Scale this subject line pattern to other campaigns")
	}

	if clickRate < 1 {
		recs = append(recs, "⚠️ Low click rate. Consider: clearer CTAs, above-the-fold placement")
	} else if clickRate > 4 {
		recs = append(recs, "✅ Strong click rate! Analyze what content resonated")
	}

	if ctor < 10 {
		recs = append(recs, "📧 Low click-to-open ratio - content may not match subject line promise")
	} else if ctor > 20 {
		recs = append(recs, "✅ Strong CTOR - subject line and content alignment is excellent")
	}

	recs = append(recs, "💡 Industry insight: Tuesday-Thursday 9-11am typically shows highest engagement")

	analytics := map[string]interface{}{
		"campaign_id":         campaignID,
		"name":                name,
		"subject":             subject,
		"status":              status,
		"sent":                sent,
		"opens":               opens,
		"clicks":              clicks,
		"bounces":             bounces,
		"revenue":             revenue,
		"open_rate":           openRate,
		"click_rate":          clickRate,
		"bounce_rate":         bounceRate,
		"click_to_open_rate":  ctor,
		"recommendations":     recs,
	}
	if startedAt != nil {
		analytics["started_at"] = startedAt.Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(analytics)
}

// HandleOptimalSendTime returns optimal send parameters
func (svc *MailingService) HandleOptimalSendTime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := r.URL.Query().Get("list_id")

	dayNames := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

	// Default industry best practices
	optimalHour := 10
	optimalDay := 2 // Tuesday
	confidence := 0.5
	reasoning := []string{
		"Using industry best practices for B2C email",
		"Tuesday-Thursday 9-11am typically optimal",
		"Consider A/B testing once you have baseline data",
	}

	if listID != "" {
		// Analyze past campaigns for this list
		var avgOpenRate float64
		var totalCampaigns int
		var bestHour, bestDay int

		svc.db.QueryRowContext(ctx, `
			SELECT COUNT(*), AVG(open_count::float / NULLIF(sent_count, 0) * 100),
				   MODE() WITHIN GROUP (ORDER BY EXTRACT(HOUR FROM started_at)),
				   MODE() WITHIN GROUP (ORDER BY EXTRACT(DOW FROM started_at))
			FROM mailing_campaigns WHERE list_id = $1 AND sent_count > 0
		`, listID).Scan(&totalCampaigns, &avgOpenRate, &bestHour, &bestDay)

		if totalCampaigns > 0 {
			optimalHour = bestHour
			optimalDay = bestDay
			confidence = float64(totalCampaigns) / 10.0
			if confidence > 0.9 {
				confidence = 0.9
			}
			reasoning = []string{
				fmt.Sprintf("Based on %d past campaigns for this list", totalCampaigns),
				fmt.Sprintf("Average open rate: %.1f%%", avgOpenRate),
				fmt.Sprintf("Best performing send time: %s at %d:00", dayNames[optimalDay], optimalHour),
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"optimal_hour":     optimalHour,
		"optimal_day":      optimalDay,
		"optimal_day_name": dayNames[optimalDay],
		"optimal_time":     fmt.Sprintf("%s at %d:00", dayNames[optimalDay], optimalHour),
		"confidence":       confidence,
		"reasoning":        reasoning,
	})
}

// HandleSendDecision returns AI decision for sending to an email
func (svc *MailingService) HandleSendDecision(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawEmail := chi.URLParam(r, "email")
	email, _ := url.PathUnescape(rawEmail)
	if email == "" {
		email = rawEmail
	}

	dayNames := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

	decision := map[string]interface{}{
		"email":        email,
		"should_send":  true,
		"optimal_hour": 10,
		"optimal_day":  2,
		"confidence":   0.5,
		"reasoning":    []string{},
		"risk_factors": []string{},
	}

	// Check suppression
	var suppressed bool
	svc.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mailing_suppressions WHERE email = $1 AND active = true)", strings.ToLower(email)).Scan(&suppressed)
	if suppressed {
		decision["should_send"] = false
		decision["reasoning"] = []string{"Email is on suppression list"}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(decision)
		return
	}

	// Get profile
	var score float64
	var bounces, complaints, bestHour, bestDay int
	err := svc.db.QueryRowContext(ctx, `
		SELECT engagement_score, total_bounces, total_complaints, best_send_hour, best_send_day
		FROM mailing_inbox_profiles WHERE email = $1
	`, strings.ToLower(email)).Scan(&score, &bounces, &complaints, &bestHour, &bestDay)

	reasoning := []string{}
	riskFactors := []string{}

	if err == sql.ErrNoRows {
		// New email - use domain defaults
		domain := ""
		if parts := strings.Split(email, "@"); len(parts) == 2 {
			domain = parts[1]
		}
		
		switch {
		case strings.Contains(domain, "gmail"):
			bestHour, bestDay = 10, 2
			reasoning = append(reasoning, "Gmail domain - using Gmail optimal times")
		case strings.Contains(domain, "yahoo"):
			bestHour, bestDay = 11, 3
			reasoning = append(reasoning, "Yahoo domain - using Yahoo optimal times")
		default:
			bestHour, bestDay = 10, 2
			reasoning = append(reasoning, "New inbox - using industry defaults")
		}
	} else {
		decision["optimal_hour"] = bestHour
		decision["optimal_day"] = bestDay

		if bounces > 0 {
			decision["should_send"] = false
			reasoning = append(reasoning, "Previous bounce detected - email may be invalid")
		}
		if complaints > 0 {
			decision["should_send"] = false
			reasoning = append(reasoning, "Previous complaint - auto-suppressed")
		}
		if score < 20 {
			riskFactors = append(riskFactors, fmt.Sprintf("Low engagement score (%.1f) - consider re-engagement", score))
		}
		if score > 70 {
			reasoning = append(reasoning, fmt.Sprintf("High engagement score (%.1f) - prioritize for sends", score))
		}
		reasoning = append(reasoning, fmt.Sprintf("Optimal send time: %s at %d:00", dayNames[bestDay], bestHour))
	}

	decision["reasoning"] = reasoning
	decision["risk_factors"] = riskFactors
	decision["optimal_time"] = fmt.Sprintf("%s at %d:00", dayNames[bestDay], bestHour)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(decision)
}
