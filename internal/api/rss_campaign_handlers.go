// Package api provides REST API handlers for RSS feed campaigns.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// RSSCampaignHandler handles RSS campaign API requests
type RSSCampaignHandler struct {
	db        *sql.DB
	rssSvc    *mailing.RSSCampaignService
	rssPoller *worker.RSSPoller
}

// NewRSSCampaignHandler creates a new RSS campaign handler
func NewRSSCampaignHandler(db *sql.DB, rssSvc *mailing.RSSCampaignService, rssPoller *worker.RSSPoller) *RSSCampaignHandler {
	return &RSSCampaignHandler{
		db:        db,
		rssSvc:    rssSvc,
		rssPoller: rssPoller,
	}
}

// RegisterRoutes registers RSS campaign routes on the provided router
func (h *RSSCampaignHandler) RegisterRoutes(r chi.Router) {
	r.Route("/rss-campaigns", func(r chi.Router) {
		r.Get("/", h.HandleGetRSSCampaigns)
		r.Post("/", h.HandleCreateRSSCampaign)
		r.Get("/{id}", h.HandleGetRSSCampaign)
		r.Put("/{id}", h.HandleUpdateRSSCampaign)
		r.Delete("/{id}", h.HandleDeleteRSSCampaign)
		r.Post("/{id}/preview", h.HandlePreviewItems)
		r.Post("/{id}/poll", h.HandleManualPoll)
		r.Get("/{id}/history", h.HandleGetPollHistory)
		r.Get("/{id}/items", h.HandleGetSentItems)
		r.Get("/poller/stats", h.HandleGetPollerStats)
	})
}

// HandleGetRSSCampaigns returns all RSS campaigns for an organization
func (h *RSSCampaignHandler) HandleGetRSSCampaigns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get organization from dynamic context
	orgID, err := GetOrgIDStringFromRequest(r)
	if err != nil {
		h.jsonError(w, "organization context required", http.StatusUnauthorized)
		return
	}

	campaigns, err := h.rssSvc.GetRSSCampaigns(ctx, orgID)
	if err != nil {
		h.jsonError(w, "Failed to fetch RSS campaigns", http.StatusInternalServerError)
		return
	}

	h.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"campaigns": campaigns,
		"total":     len(campaigns),
	})
}

// HandleCreateRSSCampaign creates a new RSS campaign
func (h *RSSCampaignHandler) HandleCreateRSSCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		Name             string  `json:"name"`
		FeedURL          string  `json:"feed_url"`
		TemplateID       string  `json:"template_id"`
		ListID           string  `json:"list_id"`
		SegmentID        *string `json:"segment_id,omitempty"`
		SendingProfileID string  `json:"sending_profile_id"`
		PollInterval     string  `json:"poll_interval"`
		AutoSend         bool    `json:"auto_send"`
		MaxItemsPerPoll  int     `json:"max_items_per_poll"`
		SubjectTemplate  string  `json:"subject_template"`
		FromName         string  `json:"from_name"`
		FromEmail        string  `json:"from_email"`
		ReplyTo          string  `json:"reply_to"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		h.jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validation
	if input.Name == "" {
		h.jsonError(w, "Name is required", http.StatusBadRequest)
		return
	}
	if input.FeedURL == "" {
		h.jsonError(w, "Feed URL is required", http.StatusBadRequest)
		return
	}
	if input.ListID == "" && input.SegmentID == nil {
		h.jsonError(w, "Either list_id or segment_id is required", http.StatusBadRequest)
		return
	}

	// Get organization from dynamic context
	orgID, err := GetOrgIDStringFromRequest(r)
	if err != nil {
		h.jsonError(w, "organization context required", http.StatusUnauthorized)
		return
	}

	config := mailing.RSSCampaign{
		OrgID:            orgID,
		Name:             input.Name,
		FeedURL:          input.FeedURL,
		TemplateID:       input.TemplateID,
		ListID:           input.ListID,
		SegmentID:        input.SegmentID,
		SendingProfileID: input.SendingProfileID,
		PollInterval:     input.PollInterval,
		AutoSend:         input.AutoSend,
		MaxItemsPerPoll:  input.MaxItemsPerPoll,
		SubjectTemplate:  input.SubjectTemplate,
		FromName:         input.FromName,
		FromEmail:        input.FromEmail,
		ReplyTo:          input.ReplyTo,
		Active:           true,
	}

	campaign, err := h.rssSvc.CreateRSSCampaign(ctx, config)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.jsonResponse(w, http.StatusCreated, campaign)
}

// HandleGetRSSCampaign returns a single RSS campaign by ID
func (h *RSSCampaignHandler) HandleGetRSSCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	campaign, err := h.rssSvc.GetRSSCampaign(ctx, id)
	if err != nil {
		h.jsonError(w, "Failed to fetch RSS campaign", http.StatusInternalServerError)
		return
	}
	if campaign == nil {
		h.jsonError(w, "RSS campaign not found", http.StatusNotFound)
		return
	}

	// Include recent sent items
	sentItems, _ := h.rssSvc.GetSentItems(ctx, id, 10)
	pollHistory, _ := h.rssSvc.GetPollHistory(ctx, id, 5)

	h.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"campaign":     campaign,
		"sent_items":   sentItems,
		"poll_history": pollHistory,
	})
}

// HandleUpdateRSSCampaign updates an existing RSS campaign
func (h *RSSCampaignHandler) HandleUpdateRSSCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	// First get existing campaign
	existing, err := h.rssSvc.GetRSSCampaign(ctx, id)
	if err != nil {
		h.jsonError(w, "Failed to fetch RSS campaign", http.StatusInternalServerError)
		return
	}
	if existing == nil {
		h.jsonError(w, "RSS campaign not found", http.StatusNotFound)
		return
	}

	var input struct {
		Name             *string `json:"name,omitempty"`
		FeedURL          *string `json:"feed_url,omitempty"`
		TemplateID       *string `json:"template_id,omitempty"`
		ListID           *string `json:"list_id,omitempty"`
		SegmentID        *string `json:"segment_id,omitempty"`
		SendingProfileID *string `json:"sending_profile_id,omitempty"`
		PollInterval     *string `json:"poll_interval,omitempty"`
		AutoSend         *bool   `json:"auto_send,omitempty"`
		MaxItemsPerPoll  *int    `json:"max_items_per_poll,omitempty"`
		SubjectTemplate  *string `json:"subject_template,omitempty"`
		FromName         *string `json:"from_name,omitempty"`
		FromEmail        *string `json:"from_email,omitempty"`
		ReplyTo          *string `json:"reply_to,omitempty"`
		Active           *bool   `json:"active,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		h.jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Apply updates
	if input.Name != nil {
		existing.Name = *input.Name
	}
	if input.FeedURL != nil {
		existing.FeedURL = *input.FeedURL
	}
	if input.TemplateID != nil {
		existing.TemplateID = *input.TemplateID
	}
	if input.ListID != nil {
		existing.ListID = *input.ListID
	}
	if input.SegmentID != nil {
		existing.SegmentID = input.SegmentID
	}
	if input.SendingProfileID != nil {
		existing.SendingProfileID = *input.SendingProfileID
	}
	if input.PollInterval != nil {
		existing.PollInterval = *input.PollInterval
	}
	if input.AutoSend != nil {
		existing.AutoSend = *input.AutoSend
	}
	if input.MaxItemsPerPoll != nil {
		existing.MaxItemsPerPoll = *input.MaxItemsPerPoll
	}
	if input.SubjectTemplate != nil {
		existing.SubjectTemplate = *input.SubjectTemplate
	}
	if input.FromName != nil {
		existing.FromName = *input.FromName
	}
	if input.FromEmail != nil {
		existing.FromEmail = *input.FromEmail
	}
	if input.ReplyTo != nil {
		existing.ReplyTo = *input.ReplyTo
	}
	if input.Active != nil {
		existing.Active = *input.Active
	}

	if err := h.rssSvc.UpdateRSSCampaign(ctx, *existing); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Fetch updated campaign
	updated, _ := h.rssSvc.GetRSSCampaign(ctx, id)
	h.jsonResponse(w, http.StatusOK, updated)
}

// HandleDeleteRSSCampaign deletes an RSS campaign
func (h *RSSCampaignHandler) HandleDeleteRSSCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	// Verify it exists
	existing, err := h.rssSvc.GetRSSCampaign(ctx, id)
	if err != nil {
		h.jsonError(w, "Failed to fetch RSS campaign", http.StatusInternalServerError)
		return
	}
	if existing == nil {
		h.jsonError(w, "RSS campaign not found", http.StatusNotFound)
		return
	}

	if err := h.rssSvc.DeleteRSSCampaign(ctx, id); err != nil {
		h.jsonError(w, "Failed to delete RSS campaign", http.StatusInternalServerError)
		return
	}

	h.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"message": "RSS campaign deleted successfully",
		"id":      id,
	})
}

// HandlePreviewItems returns a preview of the next items that would be processed
func (h *RSSCampaignHandler) HandlePreviewItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	// Parse limit from query string
	limit := 5
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 20 {
			limit = parsed
		}
	}

	items, err := h.rssSvc.PreviewNextItems(ctx, id, limit)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"items": items,
		"count": len(items),
	})
}

// HandleManualPoll triggers a manual poll of an RSS campaign
func (h *RSSCampaignHandler) HandleManualPoll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	// Verify campaign exists
	campaign, err := h.rssSvc.GetRSSCampaign(ctx, id)
	if err != nil {
		h.jsonError(w, "Failed to fetch RSS campaign", http.StatusInternalServerError)
		return
	}
	if campaign == nil {
		h.jsonError(w, "RSS campaign not found", http.StatusNotFound)
		return
	}

	var result *worker.PollResult
	
	// Use the poller if available
	if h.rssPoller != nil {
		result, err = h.rssPoller.PollSingleFeed(ctx, id)
	} else {
		// Fallback: just poll and return items
		items, pollErr := h.rssSvc.PollFeed(ctx, id)
		err = pollErr
		if err == nil {
			result = &worker.PollResult{
				CampaignID: id,
				NewItems:   len(items),
			}
		}
	}

	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"message": "Poll completed successfully",
		"result":  result,
	})
}

// HandleGetPollHistory returns the polling history for an RSS campaign
func (h *RSSCampaignHandler) HandleGetPollHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	// Parse limit
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}

	history, err := h.rssSvc.GetPollHistory(ctx, id, limit)
	if err != nil {
		h.jsonError(w, "Failed to fetch poll history", http.StatusInternalServerError)
		return
	}

	h.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"history": history,
		"total":   len(history),
	})
}

// HandleGetSentItems returns items that have been processed for an RSS campaign
func (h *RSSCampaignHandler) HandleGetSentItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	// Parse limit
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 200 {
			limit = parsed
		}
	}

	items, err := h.rssSvc.GetSentItems(ctx, id, limit)
	if err != nil {
		h.jsonError(w, "Failed to fetch sent items", http.StatusInternalServerError)
		return
	}

	h.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"items": items,
		"total": len(items),
	})
}

// HandleGetPollerStats returns statistics from the RSS poller
func (h *RSSCampaignHandler) HandleGetPollerStats(w http.ResponseWriter, r *http.Request) {
	if h.rssPoller == nil {
		h.jsonError(w, "RSS poller not initialized", http.StatusServiceUnavailable)
		return
	}

	stats := h.rssPoller.Stats()
	h.jsonResponse(w, http.StatusOK, map[string]interface{}{
		"running": h.rssPoller.IsRunning(),
		"stats":   stats,
	})
}

// Helper methods

func (h *RSSCampaignHandler) jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func (h *RSSCampaignHandler) jsonError(w http.ResponseWriter, message string, status int) {
	h.jsonResponse(w, status, map[string]string{"error": message})
}
