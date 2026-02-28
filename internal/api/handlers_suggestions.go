package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/storage"
)

// ========================================
// Suggestion/Improvement Handlers
// ========================================

// SuggestionRequest represents a request to create a suggestion
type SuggestionRequest struct {
	Area               string `json:"area"`
	AreaContext        string `json:"area_context,omitempty"`
	OriginalSuggestion string `json:"original_suggestion"`
}

// UpdateSuggestionStatusRequest represents a request to update suggestion status
type UpdateSuggestionStatusRequest struct {
	Status          string `json:"status"` // "resolved" or "denied"
	ResolutionNotes string `json:"resolution_notes,omitempty"`
}

// GetSuggestions returns all suggestions
func (h *Handlers) GetSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	// Check for status filter
	statusFilter := r.URL.Query().Get("status")
	
	var suggestions []storage.Suggestion
	var err error
	
	if statusFilter != "" {
		suggestions, err = awsStorage.GetSuggestionsByStatus(ctx, storage.SuggestionStatus(statusFilter))
	} else {
		suggestions, err = awsStorage.GetAllSuggestions(ctx)
	}
	
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to get suggestions")
		return
	}

	if suggestions == nil {
		suggestions = []storage.Suggestion{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"suggestions": suggestions,
	})
}

// GetSuggestion returns a specific suggestion by ID
func (h *Handlers) GetSuggestion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	suggestionID := chi.URLParam(r, "id")

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	suggestion, err := awsStorage.GetSuggestion(ctx, suggestionID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to get suggestion")
		return
	}

	if suggestion == nil {
		respondError(w, http.StatusNotFound, "Suggestion not found")
		return
	}

	respondJSON(w, http.StatusOK, suggestion)
}

// CreateSuggestion creates a new suggestion and generates AI requirements
func (h *Handlers) CreateSuggestion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	var req SuggestionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.OriginalSuggestion == "" {
		respondError(w, http.StatusBadRequest, "Suggestion text is required")
		return
	}

	// Get user info from auth cookie/session (we'll extract from request context)
	userEmail := r.Header.Get("X-User-Email")
	userName := r.Header.Get("X-User-Name")
	
	// If not in headers, try to get from context/cookie
	if userEmail == "" {
		// Default to anonymous if no auth
		userEmail = "anonymous@unknown.com"
		userName = "Anonymous User"
	}

	// Generate AI requirements if OpenAI agent is available
	requirements := ""
	if h.openaiAgent != nil {
		prompt := fmt.Sprintf(`You are a product requirements analyst. Convert the following user suggestion into structured requirements.

Area/Component: %s
Additional Context: %s
User Suggestion: %s

Please provide:
1. A clear, concise summary of the requirement
2. Acceptance criteria (bullet points)
3. Technical considerations
4. Priority recommendation (Low/Medium/High)

Format your response in clear markdown.`, req.Area, req.AreaContext, req.OriginalSuggestion)

		aiResponse, _, err := h.openaiAgent.Chat(ctx, prompt, nil)
		if err == nil {
			requirements = aiResponse
		}
	}

	suggestion := &storage.Suggestion{
		SubmittedByEmail:   userEmail,
		SubmittedByName:    userName,
		Area:               req.Area,
		AreaContext:        req.AreaContext,
		OriginalSuggestion: req.OriginalSuggestion,
		Requirements:       requirements,
		Status:             storage.SuggestionStatusPending,
	}

	if err := awsStorage.SaveSuggestion(ctx, suggestion); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to save suggestion")
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"success":    true,
		"suggestion": suggestion,
	})
}

// UpdateSuggestionStatus updates the status of a suggestion (resolve/deny)
func (h *Handlers) UpdateSuggestionStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	suggestionID := chi.URLParam(r, "id")

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	var req UpdateSuggestionStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Validate status
	if req.Status != "resolved" && req.Status != "denied" {
		respondError(w, http.StatusBadRequest, "Status must be 'resolved' or 'denied'")
		return
	}

	// Get the suggestion first to get user email for notification
	suggestion, err := awsStorage.GetSuggestion(ctx, suggestionID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to get suggestion")
		return
	}
	if suggestion == nil {
		respondError(w, http.StatusNotFound, "Suggestion not found")
		return
	}

	// Update the status
	status := storage.SuggestionStatus(req.Status)
	if err := awsStorage.UpdateSuggestionStatus(ctx, suggestionID, status, req.ResolutionNotes); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to update suggestion status")
		return
	}

	// Send email notification if SES is configured
	if h.sesCollector != nil && suggestion.SubmittedByEmail != "" && suggestion.SubmittedByEmail != "anonymous@unknown.com" {
		go h.sendSuggestionNotificationEmail(suggestion, status, req.ResolutionNotes)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
	})
}

// sendSuggestionNotificationEmail sends email notification about suggestion status change
func (h *Handlers) sendSuggestionNotificationEmail(suggestion *storage.Suggestion, status storage.SuggestionStatus, notes string) {
	// This would use AWS SES to send the email
	// For now, just log it
	var subject, body string
	
	if status == storage.SuggestionStatusResolved {
		subject = "Your suggestion has been implemented!"
		body = fmt.Sprintf(`Hi %s,

Great news! Your suggestion has been implemented.

Your Original Suggestion:
%s

Area: %s

Resolution Notes:
%s

Thank you for helping us improve!

Best regards,
Ignite Email Monitoring Team`, suggestion.SubmittedByName, suggestion.OriginalSuggestion, suggestion.Area, notes)
	} else {
		subject = "Update on your suggestion"
		body = fmt.Sprintf(`Hi %s,

We've reviewed your suggestion and unfortunately we're unable to implement it at this time.

Your Original Suggestion:
%s

Area: %s

Reason:
%s

We appreciate your feedback and encourage you to continue sharing your ideas!

Best regards,
Ignite Email Monitoring Team`, suggestion.SubmittedByName, suggestion.OriginalSuggestion, suggestion.Area, notes)
	}

	// Log for now (would integrate with SES)
	fmt.Printf("Would send email to %s:\nSubject: %s\nBody: %s\n", suggestion.SubmittedByEmail, subject, body)
}

// DeleteSuggestion removes a suggestion
func (h *Handlers) DeleteSuggestion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	suggestionID := chi.URLParam(r, "id")

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	if err := awsStorage.DeleteSuggestion(ctx, suggestionID); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to delete suggestion")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
	})
}

// RegenerateRequirements regenerates AI requirements for a suggestion
func (h *Handlers) RegenerateRequirements(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	suggestionID := chi.URLParam(r, "id")

	awsStorage := h.storage.GetAWSStorage()
	if awsStorage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not available")
		return
	}

	suggestion, err := awsStorage.GetSuggestion(ctx, suggestionID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to get suggestion")
		return
	}
	if suggestion == nil {
		respondError(w, http.StatusNotFound, "Suggestion not found")
		return
	}

	if h.openaiAgent == nil {
		respondError(w, http.StatusServiceUnavailable, "AI service not available")
		return
	}

	prompt := fmt.Sprintf(`You are a product requirements analyst. Convert the following user suggestion into structured requirements.

Area/Component: %s
Additional Context: %s
User Suggestion: %s

Please provide:
1. A clear, concise summary of the requirement
2. Acceptance criteria (bullet points)
3. Technical considerations
4. Priority recommendation (Low/Medium/High)

Format your response in clear markdown.`, suggestion.Area, suggestion.AreaContext, suggestion.OriginalSuggestion)

	aiResponse, _, err := h.openaiAgent.Chat(ctx, prompt, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to generate requirements")
		return
	}

	suggestion.Requirements = aiResponse
	if err := awsStorage.SaveSuggestion(ctx, suggestion); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to save updated suggestion")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":      true,
		"requirements": aiResponse,
	})
}
