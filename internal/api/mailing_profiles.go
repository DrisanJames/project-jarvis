package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

func (svc *MailingService) HandleGetProfiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse query params
	search := r.URL.Query().Get("search")
	isp := r.URL.Query().Get("isp")
	tier := r.URL.Query().Get("tier")
	sortField := r.URL.Query().Get("sort")
	sortOrder := r.URL.Query().Get("order")
	pageStr := r.URL.Query().Get("page")
	limitStr := r.URL.Query().Get("limit")

	page := 1
	limit := 50
	if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
		page = p
	}
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 200 {
		limit = l
	}

	// Build WHERE clause
	var conditions []string
	var args []interface{}
	argIdx := 1

	if search != "" {
		conditions = append(conditions, fmt.Sprintf("(email ILIKE $%d OR email_hash ILIKE $%d)", argIdx, argIdx))
		args = append(args, "%"+search+"%")
		argIdx++
	}

	if isp != "" {
		ispDomains := map[string][]string{
			"gmail":     {"gmail.com"},
			"yahoo":     {"yahoo.com", "ymail.com", "yahoo.co.uk", "yahoo.co.jp"},
			"microsoft": {"outlook.com", "hotmail.com", "live.com", "msn.com"},
			"aol":       {"aol.com"},
			"apple":     {"icloud.com", "me.com", "mac.com"},
			"comcast":   {"comcast.net", "xfinity.com"},
		}
		if domains, ok := ispDomains[strings.ToLower(isp)]; ok {
			placeholders := make([]string, len(domains))
			for i, d := range domains {
				placeholders[i] = fmt.Sprintf("$%d", argIdx)
				args = append(args, d)
				argIdx++
			}
			conditions = append(conditions, fmt.Sprintf("domain IN (%s)", strings.Join(placeholders, ",")))
		}
	}

	if tier != "" {
		switch tier {
		case "high":
			conditions = append(conditions, "engagement_score >= 0.70")
		case "medium":
			conditions = append(conditions, "engagement_score >= 0.40 AND engagement_score < 0.70")
		case "low":
			conditions = append(conditions, "engagement_score > 0 AND engagement_score < 0.40")
		case "inactive":
			conditions = append(conditions, "engagement_score = 0")
		}
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Sort
	validSorts := map[string]string{
		"engagement": "engagement_score",
		"sent":       "total_sends",
		"opens":      "total_opens",
		"clicks":     "total_clicks",
		"recent":     "updated_at",
	}
	orderBy := "updated_at DESC"
	if col, ok := validSorts[sortField]; ok {
		dir := "DESC"
		if sortOrder == "asc" {
			dir = "ASC"
		}
		orderBy = col + " " + dir
	}

	offset := (page - 1) * limit

	// Count query
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM mailing_inbox_profiles %s", whereClause)
	var total int
	svc.db.QueryRowContext(ctx, countQuery, args...).Scan(&total)

	dataQuery := fmt.Sprintf(`
		SELECT COALESCE(email, email_hash), domain, COALESCE(total_sends,0), COALESCE(total_opens,0), COALESCE(total_clicks,0),
		       COALESCE(total_bounces,0), COALESCE(total_complaints,0),
		       COALESCE(engagement_score,0), COALESCE(optimal_send_hour,10), COALESCE(optimal_send_day,2),
		       last_send_at, last_open_at, last_click_at,
		       updated_at
		FROM mailing_inbox_profiles %s
		ORDER BY %s
		LIMIT $%d OFFSET $%d
	`, whereClause, orderBy, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := svc.db.QueryContext(ctx, dataQuery, args...)
	if err != nil {
		log.Printf("HandleGetProfiles query error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"profiles": []interface{}{}, "total": 0, "page": page, "limit": limit})
		return
	}
	defer rows.Close()

	var profiles []map[string]interface{}
	for rows.Next() {
		var email, domain string
		var totalSent, totalOpens, totalClicks, totalBounces, totalComplaints, bestHour, bestDay int
		var score float64
		var lastSent, lastOpen, lastClick, updated *time.Time
		err := rows.Scan(&email, &domain, &totalSent, &totalOpens, &totalClicks,
			&totalBounces, &totalComplaints,
			&score, &bestHour, &bestDay,
			&lastSent, &lastOpen, &lastClick,
			&updated)
		if err != nil {
			log.Printf("HandleGetProfiles scan error: %v", err)
			continue
		}

		// Detect ISP from domain
		ispVal := detectISP(domain)

		// Calculate rates
		openRate := 0.0
		clickRate := 0.0
		if totalSent > 0 {
			openRate = float64(totalOpens) / float64(totalSent) * 100
			clickRate = float64(totalClicks) / float64(totalSent) * 100
		}

		// Determine tier
		engagementTier := "inactive"
		if score >= 70 {
			engagementTier = "high"
		} else if score >= 40 {
			engagementTier = "medium"
		} else if score > 0 {
			engagementTier = "low"
		}

		// Derive engagement trend from recency
		trend := "stable"
		if lastOpen != nil {
			daysSinceOpen := int(time.Since(*lastOpen).Hours() / 24)
			if daysSinceOpen < 7 {
				trend = "rising"
			} else if daysSinceOpen > 30 {
				trend = "falling"
			}
		} else if totalOpens == 0 {
			trend = "falling"
		}

		profile := map[string]interface{}{
			"email":            email,
			"domain":           domain,
			"isp":              ispVal,
			"total_sent":       totalSent,
			"total_opens":      totalOpens,
			"total_clicks":     totalClicks,
			"total_bounces":    totalBounces,
			"total_complaints": totalComplaints,
			"engagement_score": score,
			"engagement_tier":  engagementTier,
			"engagement_trend": trend,
			"best_send_hour":   bestHour,
			"best_send_day":    bestDay,
			"open_rate":        round2(openRate),
			"click_rate":       round2(clickRate),
		}
		if lastSent != nil {
			profile["last_sent_at"] = lastSent.Format(time.RFC3339)
		}
		if lastOpen != nil {
			profile["last_open_at"] = lastOpen.Format(time.RFC3339)
		}
		if lastClick != nil {
			profile["last_click_at"] = lastClick.Format(time.RFC3339)
		}
		if updated != nil {
			profile["first_seen_at"] = updated.Format(time.RFC3339) // fallback to updated_at
			profile["updated_at"] = updated.Format(time.RFC3339)
		}

		profiles = append(profiles, profile)
	}
	if profiles == nil {
		profiles = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"profiles":    profiles,
		"total":       total,
		"page":        page,
		"limit":       limit,
		"total_pages": (total + limit - 1) / limit,
	})
}

// detectISP returns a friendly ISP name from a domain
func detectISP(domain string) string {
	d := strings.ToLower(domain)
	switch {
	case d == "gmail.com":
		return "Gmail"
	case d == "yahoo.com" || d == "ymail.com" || strings.HasPrefix(d, "yahoo."):
		return "Yahoo"
	case d == "outlook.com" || d == "hotmail.com" || d == "live.com" || d == "msn.com":
		return "Microsoft"
	case d == "aol.com":
		return "AOL"
	case d == "icloud.com" || d == "me.com" || d == "mac.com":
		return "Apple"
	case d == "comcast.net" || d == "xfinity.com":
		return "Comcast"
	case d == "protonmail.com" || d == "proton.me":
		return "Proton"
	default:
		return "Other"
	}
}

// HandleGetProfileStats returns aggregate inbox profiling statistics
func (svc *MailingService) HandleGetProfileStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Total profiles
	var total int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles").Scan(&total)

	// Average engagement
	var avgEngagement float64
	svc.db.QueryRowContext(ctx, "SELECT COALESCE(AVG(engagement_score), 0) FROM mailing_inbox_profiles").Scan(&avgEngagement)

	// Tier distribution
	var highCount, medCount, lowCount, inactiveCount int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score >= 70").Scan(&highCount)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score >= 40 AND engagement_score < 70").Scan(&medCount)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score > 0 AND engagement_score < 40").Scan(&lowCount)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE engagement_score = 0").Scan(&inactiveCount)

	// Recently active (opened in last 30 days)
	var recentlyActive int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE last_open_at > NOW() - INTERVAL '30 days'").Scan(&recentlyActive)

	// Total sends/opens/clicks across all profiles
	var totalSent, totalOpens, totalClicks int
	svc.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(total_sent),0), COALESCE(SUM(total_opens),0), COALESCE(SUM(total_clicks),0) FROM mailing_inbox_profiles").Scan(&totalSent, &totalOpens, &totalClicks)

	// ISP distribution (top 10)
	ispRows, _ := svc.db.QueryContext(ctx, `
		SELECT domain, COUNT(*) as cnt FROM mailing_inbox_profiles
		GROUP BY domain ORDER BY cnt DESC LIMIT 10
	`)
	ispDist := map[string]int{}
	if ispRows != nil {
		defer ispRows.Close()
		for ispRows.Next() {
			var dom string
			var cnt int
			ispRows.Scan(&dom, &cnt)
			isp := detectISP(dom)
			ispDist[isp] += cnt
		}
	}

	// Profiles added recently (last 7 days)
	var newProfilesWeek int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE updated_at > NOW() - INTERVAL '7 days'").Scan(&newProfilesWeek)

	// Average open rate
	avgOpenRate := 0.0
	if totalSent > 0 {
		avgOpenRate = float64(totalOpens) / float64(totalSent) * 100
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_profiles":   total,
		"recently_active":  recentlyActive,
		"avg_engagement":   round2(avgEngagement),
		"avg_open_rate":    round2(avgOpenRate),
		"new_this_week":    newProfilesWeek,
		"total_sends":      totalSent,
		"total_opens":      totalOpens,
		"total_clicks":     totalClicks,
		"tier_distribution": map[string]int{
			"high":     highCount,
			"medium":   medCount,
			"low":      lowCount,
			"inactive": inactiveCount,
		},
		"isp_distribution": ispDist,
	})
}

// HandleGetProfile returns comprehensive individual mailbox analytics
func (svc *MailingService) HandleGetProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawEmail := chi.URLParam(r, "email")
	decodedEmail, _ := url.PathUnescape(rawEmail)
	if decodedEmail == "" {
		decodedEmail = rawEmail
	}
	emailLower := strings.ToLower(decodedEmail)

	var domain string
	var totalSent, totalOpens, totalClicks, totalBounces, totalComplaints, bestHour, bestDay int
	var score float64
	var avgOpenDelay float64 = 0
	var lastSent, lastOpen, lastClick *time.Time

	err := svc.db.QueryRowContext(ctx, `
		SELECT domain, total_sent, total_opens, total_clicks, COALESCE(total_bounces,0), COALESCE(total_complaints,0),
			   engagement_score, best_send_hour, best_send_day,
			   last_sent_at, last_open_at, last_click_at
		FROM mailing_inbox_profiles WHERE email = $1
	`, emailLower).Scan(&domain, &totalSent, &totalOpens, &totalClicks, &totalBounces, &totalComplaints,
		&score, &bestHour, &bestDay, &lastSent, &lastOpen, &lastClick)

	if err != nil {
		log.Printf("HandleGetProfile: raw=%q decoded=%q lower=%q err=%v", logger.RedactEmail(rawEmail), logger.RedactEmail(decodedEmail), logger.RedactEmail(emailLower), err)
		if err == sql.ErrNoRows {
			http.Error(w, `{"error":"profile not found"}`, http.StatusNotFound)
		} else {
			http.Error(w, `{"error":"profile query error"}`, http.StatusInternalServerError)
		}
		return
	}

	dayNames := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	
	// Calculate metrics
	openRate := 0.0
	clickRate := 0.0
	clickToOpenRate := 0.0
	if totalSent > 0 {
		openRate = float64(totalOpens) / float64(totalSent) * 100
		clickRate = float64(totalClicks) / float64(totalSent) * 100
	}
	if totalOpens > 0 {
		clickToOpenRate = float64(totalClicks) / float64(totalOpens) * 100
	}
	
	// Determine engagement tier
	var engagementTier string
	if score >= 70 {
		engagementTier = "high"
	} else if score >= 40 {
		engagementTier = "medium"
	} else if score > 0 {
		engagementTier = "low"
	} else {
		engagementTier = "inactive"
	}
	
	// Calculate recency (days since last engagement)
	var daysSinceOpen, daysSinceClick int
	if lastOpen != nil {
		daysSinceOpen = int(time.Since(*lastOpen).Hours() / 24)
	} else {
		daysSinceOpen = -1
	}
	if lastClick != nil {
		daysSinceClick = int(time.Since(*lastClick).Hours() / 24)
	} else {
		daysSinceClick = -1
	}

	profile := map[string]interface{}{
		"email":              emailLower,
		"domain":             domain,
		"engagement_tier":    engagementTier,
		"engagement_score":   score,
		"metrics": map[string]interface{}{
			"total_sent":        totalSent,
			"total_opens":       totalOpens,
			"total_clicks":      totalClicks,
			"total_bounces":     totalBounces,
			"total_complaints":  totalComplaints,
			"open_rate":         round2(openRate),
			"click_rate":        round2(clickRate),
			"click_to_open_rate": round2(clickToOpenRate),
			"avg_open_delay_mins": avgOpenDelay,
		},
		"optimal_send": map[string]interface{}{
			"hour_utc":  bestHour,
			"day":       bestDay,
			"day_name":  dayNames[bestDay],
			"formatted": fmt.Sprintf("%s at %d:00 UTC", dayNames[bestDay], bestHour),
		},
		"recency": map[string]interface{}{
			"days_since_open":  daysSinceOpen,
			"days_since_click": daysSinceClick,
			"last_sent_at":     formatTimePtr(lastSent),
			"last_open_at":     formatTimePtr(lastOpen),
			"last_click_at":    formatTimePtr(lastClick),
		},
	}
	
	// Get recent engagement history from tracking events
	historyRows, _ := svc.db.QueryContext(ctx, `
		SELECT e.event_type, e.event_at, c.name as campaign_name
		FROM mailing_tracking_events e
		LEFT JOIN mailing_subscribers s ON e.subscriber_id = s.id
		LEFT JOIN mailing_campaigns c ON e.campaign_id = c.id
		WHERE LOWER(s.email) = $1
		ORDER BY e.event_at DESC LIMIT 10
	`, emailLower)
	
	var history []map[string]interface{}
	if historyRows != nil {
		defer historyRows.Close()
		for historyRows.Next() {
			var eventType, campaignName string
			var eventAt time.Time
			historyRows.Scan(&eventType, &eventAt, &campaignName)
			history = append(history, map[string]interface{}{
				"event":    eventType,
				"time":     eventAt.Format(time.RFC3339),
				"campaign": campaignName,
			})
		}
	}
	if history == nil { history = []map[string]interface{}{} }
	profile["engagement_history"] = history

	// Generate AI recommendations
	recs := []string{}
	if score >= 70 {
		recs = append(recs, "🌟 High value subscriber - prioritize for exclusive offers and early access")
		recs = append(recs, "Consider for VIP segment and loyalty programs")
	} else if score >= 40 {
		recs = append(recs, "📊 Active subscriber - maintain regular engagement cadence")
		recs = append(recs, "Test personalized subject lines to boost opens")
	} else if score > 0 {
		recs = append(recs, "⚠️ Low engagement - consider re-engagement sequence")
		recs = append(recs, "Try different send times or content types")
	} else {
		recs = append(recs, "🔄 Inactive subscriber - run win-back campaign")
		recs = append(recs, "Consider sunset policy if no engagement after re-engagement")
	}
	
	if daysSinceOpen > 30 && daysSinceOpen >= 0 {
		recs = append(recs, fmt.Sprintf("⏰ No opens in %d days - consider re-engagement", daysSinceOpen))
	}
	
	if totalBounces > 0 {
		recs = append(recs, "⚠️ Has bounced before - verify address validity")
	}
	if totalComplaints > 0 {
		recs = append(recs, "🚨 Previous complaint - proceed with caution or suppress")
	}
	
	recs = append(recs, fmt.Sprintf("⏱️ Best time to send: %s at %d:00 UTC", dayNames[bestDay], bestHour))
	
	profile["recommendations"] = recs
	profile["risk_assessment"] = map[string]interface{}{
		"bounce_risk":    totalBounces > 0,
		"complaint_risk": totalComplaints > 0,
		"inactivity_risk": daysSinceOpen > 60 || engagementTier == "inactive",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(profile)
}

// Helper functions
func round2(f float64) float64 {
	return float64(int(f*100)) / 100
}

func formatTimePtr(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339)
}
