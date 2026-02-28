package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// AISendTimeHandlers provides HTTP handlers for AI send time optimization
type AISendTimeHandlers struct {
	service *mailing.AISendTimeService
	db      *sql.DB
}

// NewAISendTimeHandlers creates a new AISendTimeHandlers instance
func NewAISendTimeHandlers(db *sql.DB) *AISendTimeHandlers {
	return &AISendTimeHandlers{
		service: mailing.NewAISendTimeService(db),
		db:      db,
	}
}

// RegisterRoutes registers the AI send time routes
func (h *AISendTimeHandlers) RegisterRoutes(r chi.Router) {
	r.Route("/ai/send-time", func(r chi.Router) {
		// Get optimal send time for a single subscriber
		r.Get("/{subscriber_id}", h.HandleGetOptimalSendTime)

		// Get optimal send times for multiple subscribers (bulk)
		r.Post("/bulk", h.HandleGetBulkOptimalTimes)

		// Get audience-level optimal times for a list
		r.Get("/audience/{list_id}", h.HandleGetAudienceOptimalTimes)

		// Schedule a campaign with per-subscriber optimal times
		r.Post("/schedule-campaign/{campaign_id}", h.HandleScheduleCampaignOptimally)

		// Recalculate optimal time for a subscriber
		r.Post("/recalculate/{subscriber_id}", h.HandleRecalculateSubscriberTime)

		// Get timezone distribution for a list
		r.Get("/timezones/{list_id}", h.HandleGetTimezoneDistribution)

		// Update subscriber timezone
		r.Put("/timezone/{subscriber_id}", h.HandleUpdateTimezone)

		// Get send time history for a subscriber
		r.Get("/history/{subscriber_id}", h.HandleGetSendTimeHistory)

		// Get campaign scheduled times
		r.Get("/campaign/{campaign_id}/schedule", h.HandleGetCampaignScheduledTimes)
	})
}

// HandleGetOptimalSendTime returns the optimal send time for a subscriber
// GET /api/mailing/ai/send-time/{subscriber_id}
func (h *AISendTimeHandlers) HandleGetOptimalSendTime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subscriberID := chi.URLParam(r, "subscriber_id")

	if subscriberID == "" {
		jsonError(w, "subscriber_id is required", http.StatusBadRequest)
		return
	}

	recommendation, err := h.service.GetOptimalSendTime(ctx, subscriberID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, recommendation)
}

// HandleGetBulkOptimalTimes returns optimal send times for multiple subscribers
// POST /api/mailing/ai/send-time/bulk
func (h *AISendTimeHandlers) HandleGetBulkOptimalTimes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var request struct {
		SubscriberIDs []string `json:"subscriber_ids"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(request.SubscriberIDs) == 0 {
		jsonError(w, "subscriber_ids is required", http.StatusBadRequest)
		return
	}

	if len(request.SubscriberIDs) > 1000 {
		jsonError(w, "maximum 1000 subscriber IDs per request", http.StatusBadRequest)
		return
	}

	recommendations, err := h.service.GetBulkOptimalTimes(ctx, request.SubscriberIDs)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"recommendations": recommendations,
		"total":           len(recommendations),
	})
}

// HandleGetAudienceOptimalTimes returns optimal times for an entire list/audience
// GET /api/mailing/ai/send-time/audience/{list_id}
func (h *AISendTimeHandlers) HandleGetAudienceOptimalTimes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "list_id")

	if listID == "" {
		jsonError(w, "list_id is required", http.StatusBadRequest)
		return
	}

	optimalTimes, err := h.service.CalculateAudienceOptimalTimes(ctx, listID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, optimalTimes)
}

// HandleScheduleCampaignOptimally schedules a campaign with per-subscriber times
// POST /api/mailing/ai/send-time/schedule-campaign/{campaign_id}
func (h *AISendTimeHandlers) HandleScheduleCampaignOptimally(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaign_id")

	if campaignID == "" {
		jsonError(w, "campaign_id is required", http.StatusBadRequest)
		return
	}

	scheduledTimes, err := h.service.ScheduleCampaignOptimally(ctx, campaignID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"campaign_id":     campaignID,
		"scheduled_times": scheduledTimes,
		"total_scheduled": len(scheduledTimes),
	})
}

// HandleRecalculateSubscriberTime recalculates optimal time for a subscriber
// POST /api/mailing/ai/send-time/recalculate/{subscriber_id}
func (h *AISendTimeHandlers) HandleRecalculateSubscriberTime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subscriberID := chi.URLParam(r, "subscriber_id")

	if subscriberID == "" {
		jsonError(w, "subscriber_id is required", http.StatusBadRequest)
		return
	}

	optimalTime, err := h.service.RecalculateSubscriberTime(ctx, subscriberID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, optimalTime)
}

// HandleGetTimezoneDistribution returns timezone distribution for a list
// GET /api/mailing/ai/send-time/timezones/{list_id}
func (h *AISendTimeHandlers) HandleGetTimezoneDistribution(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "list_id")

	if listID == "" {
		jsonError(w, "list_id is required", http.StatusBadRequest)
		return
	}

	distribution, err := h.service.GetTimezoneDistribution(ctx, listID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Calculate total and format response
	total := 0
	for _, count := range distribution {
		total += count
	}

	jsonResponse(w, map[string]interface{}{
		"list_id":      listID,
		"distribution": distribution,
		"total":        total,
	})
}

// HandleUpdateTimezone updates a subscriber's timezone
// PUT /api/mailing/ai/send-time/timezone/{subscriber_id}
func (h *AISendTimeHandlers) HandleUpdateTimezone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subscriberID := chi.URLParam(r, "subscriber_id")

	if subscriberID == "" {
		jsonError(w, "subscriber_id is required", http.StatusBadRequest)
		return
	}

	var request struct {
		Timezone string `json:"timezone"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.service.UpdateSubscriberTimezone(ctx, subscriberID, request.Timezone); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"subscriber_id": subscriberID,
		"timezone":      request.Timezone,
		"updated":       true,
	})
}

// HandleGetSendTimeHistory returns send time history for a subscriber
// GET /api/mailing/ai/send-time/history/{subscriber_id}
func (h *AISendTimeHandlers) HandleGetSendTimeHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subscriberID := chi.URLParam(r, "subscriber_id")

	if subscriberID == "" {
		jsonError(w, "subscriber_id is required", http.StatusBadRequest)
		return
	}

	rows, err := h.db.QueryContext(ctx, `
		SELECT 
			sth.id,
			sth.campaign_id,
			c.name as campaign_name,
			sth.sent_at,
			sth.sent_hour,
			sth.sent_day,
			sth.sent_local_hour,
			sth.timezone,
			sth.opened,
			sth.clicked,
			sth.open_delay_seconds,
			sth.created_at
		FROM mailing_send_time_history sth
		LEFT JOIN mailing_campaigns c ON c.id = sth.campaign_id
		WHERE sth.subscriber_id = $1
		ORDER BY sth.sent_at DESC
		LIMIT 100
	`, subscriberID)
	if err != nil {
		jsonError(w, "failed to fetch history", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var history []map[string]interface{}
	for rows.Next() {
		var (
			id, campaignID, campaignName, timezone sql.NullString
			sentAt                                  sql.NullTime
			sentHour, sentDay                       int
			sentLocalHour, openDelaySeconds         sql.NullInt64
			opened, clicked                         bool
			createdAt                               sql.NullTime
		)

		if err := rows.Scan(&id, &campaignID, &campaignName, &sentAt, &sentHour, &sentDay,
			&sentLocalHour, &timezone, &opened, &clicked, &openDelaySeconds, &createdAt); err != nil {
			continue
		}

		entry := map[string]interface{}{
			"id":           id.String,
			"sent_hour":    sentHour,
			"sent_day":     sentDay,
			"day_name":     dayNames[sentDay],
			"opened":       opened,
			"clicked":      clicked,
		}

		if campaignID.Valid {
			entry["campaign_id"] = campaignID.String
		}
		if campaignName.Valid {
			entry["campaign_name"] = campaignName.String
		}
		if sentAt.Valid {
			entry["sent_at"] = sentAt.Time
		}
		if sentLocalHour.Valid {
			entry["sent_local_hour"] = sentLocalHour.Int64
		}
		if timezone.Valid {
			entry["timezone"] = timezone.String
		}
		if openDelaySeconds.Valid {
			entry["open_delay_seconds"] = openDelaySeconds.Int64
		}
		if createdAt.Valid {
			entry["created_at"] = createdAt.Time
		}

		history = append(history, entry)
	}

	if history == nil {
		history = []map[string]interface{}{}
	}

	jsonResponse(w, map[string]interface{}{
		"subscriber_id": subscriberID,
		"history":       history,
		"total":         len(history),
	})
}

// HandleGetCampaignScheduledTimes returns the scheduled send times for a campaign
// GET /api/mailing/ai/send-time/campaign/{campaign_id}/schedule
func (h *AISendTimeHandlers) HandleGetCampaignScheduledTimes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaign_id")

	if campaignID == "" {
		jsonError(w, "campaign_id is required", http.StatusBadRequest)
		return
	}

	// Get campaign info
	var campaignName string
	var aiOptimization bool
	err := h.db.QueryRowContext(ctx, `
		SELECT name, ai_send_time_optimization FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&campaignName, &aiOptimization)
	if err != nil {
		jsonError(w, "campaign not found", http.StatusNotFound)
		return
	}

	// Get scheduled times
	rows, err := h.db.QueryContext(ctx, `
		SELECT 
			cst.subscriber_id,
			s.email,
			cst.scheduled_time,
			cst.local_time,
			cst.timezone,
			cst.confidence,
			cst.reasoning,
			cst.status,
			cst.sent_at
		FROM mailing_campaign_scheduled_times cst
		JOIN mailing_subscribers s ON s.id = cst.subscriber_id
		WHERE cst.campaign_id = $1
		ORDER BY cst.scheduled_time ASC
		LIMIT 1000
	`, campaignID)
	if err != nil {
		jsonError(w, "failed to fetch scheduled times", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var scheduledTimes []map[string]interface{}
	statusCounts := map[string]int{"pending": 0, "sent": 0, "skipped": 0, "failed": 0}
	hourDistribution := make(map[int]int)

	for rows.Next() {
		var (
			subscriberID, email, timezone, reasoning, status sql.NullString
			scheduledTime, localTime, sentAt                 sql.NullTime
			confidence                                       float64
		)

		if err := rows.Scan(&subscriberID, &email, &scheduledTime, &localTime, &timezone,
			&confidence, &reasoning, &status, &sentAt); err != nil {
			continue
		}

		entry := map[string]interface{}{
			"subscriber_id": subscriberID.String,
			"confidence":    confidence,
		}

		if email.Valid {
			entry["email"] = email.String
		}
		if scheduledTime.Valid {
			entry["scheduled_time"] = scheduledTime.Time
			hourDistribution[scheduledTime.Time.Hour()]++
		}
		if localTime.Valid {
			entry["local_time"] = localTime.Time
		}
		if timezone.Valid {
			entry["timezone"] = timezone.String
		}
		if reasoning.Valid {
			entry["reasoning"] = reasoning.String
		}
		if status.Valid {
			entry["status"] = status.String
			statusCounts[status.String]++
		}
		if sentAt.Valid {
			entry["sent_at"] = sentAt.Time
		}

		scheduledTimes = append(scheduledTimes, entry)
	}

	if scheduledTimes == nil {
		scheduledTimes = []map[string]interface{}{}
	}

	jsonResponse(w, map[string]interface{}{
		"campaign_id":        campaignID,
		"campaign_name":      campaignName,
		"ai_optimization":    aiOptimization,
		"scheduled_times":    scheduledTimes,
		"total":              len(scheduledTimes),
		"status_counts":      statusCounts,
		"hour_distribution":  hourDistribution,
	})
}

// dayNames maps day numbers to names (duplicated here for the handler file)
var dayNames = map[int]string{
	0: "Sunday",
	1: "Monday",
	2: "Tuesday",
	3: "Wednesday",
	4: "Thursday",
	5: "Friday",
	6: "Saturday",
}

// jsonResponse writes a JSON response
func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// jsonError writes a JSON error response
func jsonError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
