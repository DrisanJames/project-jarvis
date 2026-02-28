package mailing

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Handlers provides HTTP handlers for mailing API
type Handlers struct {
	store       *Store
	sender      *Sender
	campSender  *CampaignSender
	tracking    *TrackingService
	webhook     *WebhookHandler
	ai          *AIService
}

// NewHandlers creates new mailing handlers
func NewHandlers(store *Store, sender *Sender, tracking *TrackingService, ai *AIService) *Handlers {
	return &Handlers{
		store:      store,
		sender:     sender,
		campSender: NewCampaignSender(store, sender),
		tracking:   tracking,
		webhook:    NewWebhookHandler(store),
		ai:         ai,
	}
}

// Response helpers
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{"error": message})
}

func getOrgID(r *http.Request) uuid.UUID {
	// In production, extract from JWT claims
	orgIDStr := r.Header.Get("X-Organization-ID")
	if orgIDStr == "" {
		return uuid.Nil
	}
	id, _ := uuid.Parse(orgIDStr)
	return id
}

// List Handlers

// HandleGetLists returns all lists for an organization
func (h *Handlers) HandleGetLists(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	if orgID == uuid.Nil {
		respondError(w, http.StatusUnauthorized, "organization not found")
		return
	}

	lists, err := h.store.GetLists(r.Context(), orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get lists")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"lists": lists,
		"total": len(lists),
	})
}

// HandleCreateList creates a new list
func (h *Handlers) HandleCreateList(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	if orgID == uuid.Nil {
		respondError(w, http.StatusUnauthorized, "organization not found")
		return
	}

	var list List
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	list.OrganizationID = orgID
	if err := h.store.CreateList(r.Context(), &list); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create list")
		return
	}

	respondJSON(w, http.StatusCreated, list)
}

// HandleGetList returns a single list
func (h *Handlers) HandleGetList(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	listIDStr := r.PathValue("listId")
	listID, err := uuid.Parse(listIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid list ID")
		return
	}

	list, err := h.store.GetList(r.Context(), orgID, listID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get list")
		return
	}
	if list == nil {
		respondError(w, http.StatusNotFound, "list not found")
		return
	}

	respondJSON(w, http.StatusOK, list)
}

// Subscriber Handlers

// HandleGetSubscribers returns subscribers for a list
func (h *Handlers) HandleGetSubscribers(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	listIDStr := r.PathValue("listId")
	listID, err := uuid.Parse(listIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid list ID")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	// Verify list belongs to org
	list, err := h.store.GetList(r.Context(), orgID, listID)
	if err != nil || list == nil {
		respondError(w, http.StatusNotFound, "list not found")
		return
	}

	subscribers, total, err := h.store.GetSubscribers(r.Context(), listID, limit, offset)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get subscribers")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"subscribers": subscribers,
		"total":       total,
		"limit":       limit,
		"offset":      offset,
	})
}

// HandleCreateSubscriber creates a new subscriber
func (h *Handlers) HandleCreateSubscriber(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	listIDStr := r.PathValue("listId")
	listID, err := uuid.Parse(listIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid list ID")
		return
	}

	var sub Subscriber
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if !ValidateEmail(sub.Email) {
		respondError(w, http.StatusBadRequest, "invalid email address")
		return
	}

	sub.OrganizationID = orgID
	sub.ListID = listID
	if err := h.store.CreateSubscriber(r.Context(), &sub); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create subscriber")
		return
	}

	respondJSON(w, http.StatusCreated, sub)
}

// Campaign Handlers

// HandleGetCampaigns returns campaigns for an organization
func (h *Handlers) HandleGetCampaigns(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}

	campaigns, err := h.store.GetCampaigns(r.Context(), orgID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get campaigns")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"campaigns": campaigns,
		"total":     len(campaigns),
	})
}

// HandleCreateCampaign creates a new campaign
func (h *Handlers) HandleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	var campaign Campaign
	if err := json.NewDecoder(r.Body).Decode(&campaign); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	campaign.OrganizationID = orgID
	if err := h.store.CreateCampaign(r.Context(), &campaign); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create campaign")
		return
	}

	respondJSON(w, http.StatusCreated, campaign)
}

// HandleGetCampaign returns a single campaign
func (h *Handlers) HandleGetCampaign(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	campaign, err := h.store.GetCampaign(r.Context(), orgID, campaignID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get campaign")
		return
	}
	if campaign == nil {
		respondError(w, http.StatusNotFound, "campaign not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"campaign": campaign,
		"stats":    campaign.CalculateStats(),
	})
}

// HandleSendCampaign initiates campaign sending
func (h *Handlers) HandleSendCampaign(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	campaign, err := h.store.GetCampaign(r.Context(), orgID, campaignID)
	if err != nil || campaign == nil {
		respondError(w, http.StatusNotFound, "campaign not found")
		return
	}

	if campaign.Status != StatusDraft {
		respondError(w, http.StatusBadRequest, "campaign must be in draft status to send")
		return
	}

	// Prepare and start sending
	if err := h.campSender.PrepareCampaign(r.Context(), campaign); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to prepare campaign: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message":          "campaign sending initiated",
		"campaign_id":      campaign.ID,
		"total_recipients": campaign.TotalRecipients,
	})
}

// Delivery Server Handlers

// HandleGetDeliveryServers returns delivery servers
func (h *Handlers) HandleGetDeliveryServers(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	servers, err := h.store.GetDeliveryServers(r.Context(), orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get delivery servers")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"servers": servers,
		"total":   len(servers),
	})
}

// Sending Plan Handlers

// HandleGetSendingPlans generates sending plan options
func (h *Handlers) HandleGetSendingPlans(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	dateStr := r.URL.Query().Get("date")
	targetDate := time.Now()
	if dateStr != "" {
		parsed, err := time.Parse("2006-01-02", dateStr)
		if err == nil {
			targetDate = parsed
		}
	}

	plans, err := h.ai.GenerateSendingPlans(r.Context(), orgID, targetDate)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate plans: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"plans":       plans,
		"target_date": targetDate.Format("2006-01-02"),
		"generated_at": time.Now(),
	})
}

// Tracking Handlers

// HandleTrackOpen handles open tracking pixel
func (h *Handlers) HandleTrackOpen(w http.ResponseWriter, r *http.Request) {
	encoded := r.PathValue("data")
	signature := r.PathValue("sig")

	if err := h.tracking.HandleOpen(r.Context(), encoded, signature, r); err != nil {
		// Don't expose errors, still return pixel
	}

	// Return 1x1 transparent GIF
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	pixel := []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00,
		0x80, 0x00, 0x00, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x2c,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02,
		0x02, 0x44, 0x01, 0x00, 0x3b}
	w.Write(pixel)
}

// HandleTrackClick handles click tracking
func (h *Handlers) HandleTrackClick(w http.ResponseWriter, r *http.Request) {
	encoded := r.PathValue("data")
	signature := r.PathValue("sig")

	redirectURL, err := h.tracking.HandleClick(r.Context(), encoded, signature, r)
	if err != nil || redirectURL == "" {
		http.Error(w, "invalid link", http.StatusBadRequest)
		return
	}

	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// HandleTrackUnsubscribe handles unsubscribe requests
func (h *Handlers) HandleTrackUnsubscribe(w http.ResponseWriter, r *http.Request) {
	encoded := r.PathValue("data")
	signature := r.PathValue("sig")

	if err := h.tracking.HandleUnsubscribe(r.Context(), encoded, signature, r); err != nil {
		respondError(w, http.StatusBadRequest, "invalid unsubscribe link")
		return
	}

	// Return unsubscribe confirmation page
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Unsubscribed</title></head>
<body style="font-family: sans-serif; text-align: center; padding: 50px;">
<h1>You've been unsubscribed</h1>
<p>You will no longer receive emails from us.</p>
</body>
</html>`))
}

// Webhook Handlers

// HandleSparkPostWebhook handles SparkPost webhook events
func (h *Handlers) HandleSparkPostWebhook(w http.ResponseWriter, r *http.Request) {
	var events []map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		respondError(w, http.StatusBadRequest, "invalid webhook payload")
		return
	}

	if err := h.webhook.HandleSparkPostWebhook(r.Context(), events); err != nil {
		respondError(w, http.StatusInternalServerError, "webhook processing failed")
		return
	}

	w.WriteHeader(http.StatusOK)
}

// HandleSESWebhook handles AWS SES webhook events
func (h *Handlers) HandleSESWebhook(w http.ResponseWriter, r *http.Request) {
	var notification map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
		respondError(w, http.StatusBadRequest, "invalid webhook payload")
		return
	}

	if err := h.webhook.HandleSESWebhook(r.Context(), notification); err != nil {
		respondError(w, http.StatusInternalServerError, "webhook processing failed")
		return
	}

	w.WriteHeader(http.StatusOK)
}

// Dashboard/Stats Handlers

// HandleGetDashboard returns dashboard statistics
func (h *Handlers) HandleGetDashboard(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	// Get recent campaigns
	campaigns, err := h.store.GetCampaigns(r.Context(), orgID, 10)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get dashboard data")
		return
	}

	// Calculate aggregate stats
	var totalSent, totalOpens, totalClicks int
	var totalRevenue float64
	for _, c := range campaigns {
		totalSent += c.SentCount
		totalOpens += c.OpenCount
		totalClicks += c.ClickCount
		totalRevenue += c.Revenue
	}

	// Get lists
	lists, _ := h.store.GetLists(r.Context(), orgID)
	var totalSubscribers int
	for _, l := range lists {
		totalSubscribers += l.SubscriberCount
	}

	// Get delivery servers
	servers, _ := h.store.GetDeliveryServers(r.Context(), orgID)
	var dailyCapacity, dailyUsed int
	for _, s := range servers {
		dailyCapacity += s.DailyQuota
		dailyUsed += s.UsedDaily
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"overview": map[string]interface{}{
			"total_subscribers": totalSubscribers,
			"total_lists":       len(lists),
			"total_campaigns":   len(campaigns),
			"daily_capacity":    dailyCapacity,
			"daily_used":        dailyUsed,
		},
		"performance": map[string]interface{}{
			"total_sent":    totalSent,
			"total_opens":   totalOpens,
			"total_clicks":  totalClicks,
			"total_revenue": totalRevenue,
			"open_rate":     safeRate(totalOpens, totalSent),
			"click_rate":    safeRate(totalClicks, totalSent),
		},
		"recent_campaigns": campaigns,
	})
}

func safeRate(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator) * 100
}

// RegisterRoutes registers all mailing routes
func (h *Handlers) RegisterRoutes(mux *http.ServeMux) {
	// Dashboard
	mux.HandleFunc("GET /api/mailing/dashboard", h.HandleGetDashboard)

	// Lists
	mux.HandleFunc("GET /api/mailing/lists", h.HandleGetLists)
	mux.HandleFunc("POST /api/mailing/lists", h.HandleCreateList)
	mux.HandleFunc("GET /api/mailing/lists/{listId}", h.HandleGetList)

	// Subscribers
	mux.HandleFunc("GET /api/mailing/lists/{listId}/subscribers", h.HandleGetSubscribers)
	mux.HandleFunc("POST /api/mailing/lists/{listId}/subscribers", h.HandleCreateSubscriber)

	// Campaigns
	mux.HandleFunc("GET /api/mailing/campaigns", h.HandleGetCampaigns)
	mux.HandleFunc("POST /api/mailing/campaigns", h.HandleCreateCampaign)
	mux.HandleFunc("GET /api/mailing/campaigns/{campaignId}", h.HandleGetCampaign)
	mux.HandleFunc("POST /api/mailing/campaigns/{campaignId}/send", h.HandleSendCampaign)

	// Delivery Servers
	mux.HandleFunc("GET /api/mailing/delivery-servers", h.HandleGetDeliveryServers)

	// AI Sending Plans
	mux.HandleFunc("GET /api/mailing/sending-plans", h.HandleGetSendingPlans)

	// Tracking
	mux.HandleFunc("GET /track/open/{data}/{sig}", h.HandleTrackOpen)
	mux.HandleFunc("GET /track/click/{data}/{sig}", h.HandleTrackClick)
	mux.HandleFunc("GET /track/unsubscribe/{data}/{sig}", h.HandleTrackUnsubscribe)

	// Webhooks
	mux.HandleFunc("POST /webhooks/sparkpost", h.HandleSparkPostWebhook)
	mux.HandleFunc("POST /webhooks/ses", h.HandleSESWebhook)
}
