package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// PersonalizationService handles template preview and merge tag APIs
type PersonalizationService struct {
	db             *sql.DB
	templateSvc    *mailing.TemplateService
	contextBuilder *mailing.ContextBuilder
}

// NewPersonalizationService creates a new personalization service
func NewPersonalizationService(db *sql.DB) *PersonalizationService {
	return &PersonalizationService{
		db:             db,
		templateSvc:    mailing.NewTemplateService(),
		contextBuilder: mailing.NewContextBuilder(db, "https://track.ignite.media", "signing-key-placeholder"),
	}
}

// RegisterRoutes registers personalization API routes
func (ps *PersonalizationService) RegisterRoutes(r chi.Router) {
	// Merge Tags
	r.Get("/merge-tags", ps.HandleGetMergeTags)
	r.Get("/merge-tags/custom", ps.HandleGetCustomMergeTags)
	r.Get("/filters", ps.HandleGetFilters)

	// Template Preview & Validation
	r.Post("/preview", ps.HandlePreviewTemplate)
	r.Post("/validate", ps.HandleValidateTemplate)
	r.Post("/preview/subscriber/{subscriberID}", ps.HandlePreviewForSubscriber)

	// Sample Data
	r.Get("/sample-context", ps.HandleGetSampleContext)
}

// PreviewRequest represents a template preview request
type PreviewRequest struct {
	Subject      string `json:"subject"`
	HTMLContent  string `json:"html_content"`
	TextContent  string `json:"text_content,omitempty"`
	SubscriberID string `json:"subscriber_id,omitempty"`
	CampaignID   string `json:"campaign_id,omitempty"`
	UseSample    bool   `json:"use_sample,omitempty"`
}

// PreviewResponse represents a template preview response
type PreviewResponse struct {
	RenderedSubject string                              `json:"rendered_subject"`
	RenderedHTML    string                              `json:"rendered_html"`
	RenderedText    string                              `json:"rendered_text,omitempty"`
	Context         map[string]interface{}              `json:"context,omitempty"`
	Warnings        []mailing.TemplateValidationError   `json:"warnings,omitempty"`
	Success         bool                                `json:"success"`
}

// HandleGetMergeTags returns all available merge tags
func (ps *PersonalizationService) HandleGetMergeTags(w http.ResponseWriter, r *http.Request) {
	tags := mailing.GetAvailableMergeTags()

	// Group by category
	grouped := make(map[string][]mailing.MergeTagDefinition)
	for _, tag := range tags {
		grouped[tag.Category] = append(grouped[tag.Category], tag)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tags":    tags,
		"grouped": grouped,
		"categories": []string{
			"profile",
			"custom",
			"engagement",
			"computed",
			"system",
			"logic",
		},
	})
}

// HandleGetCustomMergeTags returns organization-specific custom field merge tags
func (ps *PersonalizationService) HandleGetCustomMergeTags(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrganizationID(r)

	// Load custom field definitions from mailing_contact_fields
	rows, err := ps.db.QueryContext(ctx, `
		SELECT field_key, field_label, field_type, category, description
		FROM mailing_contact_fields
		WHERE organization_id = $1 AND is_visible = true
		ORDER BY display_order, field_label
	`, orgID)

	if err != nil {
		log.Printf("Error loading custom fields: %v", err)
		http.Error(w, "Failed to load custom fields", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var customTags []mailing.MergeTagDefinition
	for rows.Next() {
		var key, label, fieldType, category string
		var description sql.NullString

		if err := rows.Scan(&key, &label, &fieldType, &category, &description); err != nil {
			continue
		}

		tag := mailing.MergeTagDefinition{
			Key:      "custom." + key,
			Label:    label,
			Category: "custom",
			DataType: fieldType,
			Syntax:   "{{ custom." + key + " }}",
		}

		// Add appropriate filter suggestion based on type
		switch fieldType {
		case "date", "datetime":
			tag.Syntax = "{{ custom." + key + " | date: '%B %d, %Y' }}"
		case "number", "decimal":
			tag.Syntax = "{{ custom." + key + " | number_with_delimiter }}"
		}

		customTags = append(customTags, tag)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tags":  customTags,
		"count": len(customTags),
	})
}

// HandleGetFilters returns all available Liquid filters
func (ps *PersonalizationService) HandleGetFilters(w http.ResponseWriter, r *http.Request) {
	filters := ps.templateSvc.GetAvailableFilters()

	// Group by category
	grouped := make(map[string][]mailing.FilterInfo)
	for _, f := range filters {
		grouped[f.Category] = append(grouped[f.Category], f)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"filters": filters,
		"grouped": grouped,
		"categories": []string{
			"string",
			"number",
			"date",
			"email",
			"boolean",
		},
	})
}

// HandlePreviewTemplate renders a template with sample or real subscriber data
func (ps *PersonalizationService) HandlePreviewTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req PreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Build render context
	var renderCtx mailing.RenderContext
	var campaign *mailing.Campaign

	if req.CampaignID != "" {
		campaignUUID, _ := uuid.Parse(req.CampaignID)
		campaign = &mailing.Campaign{ID: campaignUUID}
	}

	if req.SubscriberID != "" && !req.UseSample {
		// Use real subscriber data
		subUUID, err := uuid.Parse(req.SubscriberID)
		if err != nil {
			http.Error(w, "Invalid subscriber ID", http.StatusBadRequest)
			return
		}

		renderCtx, err = ps.contextBuilder.BuildContextFromSubscriberID(ctx, subUUID, campaign)
		if err != nil {
			log.Printf("Failed to build context for subscriber %s: %v", req.SubscriberID, err)
			// Fall back to sample
			renderCtx = ps.contextBuilder.BuildSampleContext()
		}
	} else {
		// Use sample data
		renderCtx = ps.contextBuilder.BuildSampleContext()
	}

	// Render with strict mode to catch all warnings
	response := PreviewResponse{
		Success:  true,
		Context:  renderCtx,
		Warnings: []mailing.TemplateValidationError{},
	}

	// Render subject
	if req.Subject != "" {
		result, err := ps.templateSvc.RenderWithMode(req.Subject, renderCtx, mailing.RenderModeStrict)
		if err != nil {
			response.Warnings = append(response.Warnings, mailing.TemplateValidationError{
				Variable: "subject",
				Message:  err.Error(),
			})
			response.RenderedSubject = req.Subject
		} else {
			response.RenderedSubject = result.Output
			response.Warnings = append(response.Warnings, result.Warnings...)
		}
	}

	// Render HTML
	if req.HTMLContent != "" {
		result, err := ps.templateSvc.RenderWithMode(req.HTMLContent, renderCtx, mailing.RenderModeStrict)
		if err != nil {
			response.Warnings = append(response.Warnings, mailing.TemplateValidationError{
				Variable: "html_content",
				Message:  err.Error(),
			})
			response.RenderedHTML = req.HTMLContent
		} else {
			response.RenderedHTML = result.Output
			response.Warnings = append(response.Warnings, result.Warnings...)
		}
	}

	// Render text
	if req.TextContent != "" {
		result, err := ps.templateSvc.RenderWithMode(req.TextContent, renderCtx, mailing.RenderModeLax)
		if err == nil {
			response.RenderedText = result.Output
		} else {
			response.RenderedText = req.TextContent
		}
	}

	// Mark as not fully successful if there are warnings
	if len(response.Warnings) > 0 {
		response.Success = false
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// HandleValidateTemplate validates template syntax without rendering
func (ps *PersonalizationService) HandleValidateTemplate(w http.ResponseWriter, r *http.Request) {
	var req PreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	response := map[string]interface{}{
		"valid":    true,
		"errors":   []map[string]string{},
		"warnings": []mailing.TemplateValidationError{},
	}

	var errors []map[string]string

	// Validate subject syntax
	if req.Subject != "" {
		if err := ps.templateSvc.Parse(req.Subject); err != nil {
			errors = append(errors, map[string]string{
				"field":   "subject",
				"message": err.Error(),
			})
		}
	}

	// Validate HTML syntax
	if req.HTMLContent != "" {
		if err := ps.templateSvc.Parse(req.HTMLContent); err != nil {
			errors = append(errors, map[string]string{
				"field":   "html_content",
				"message": err.Error(),
			})
		}
	}

	// Validate text syntax
	if req.TextContent != "" {
		if err := ps.templateSvc.Parse(req.TextContent); err != nil {
			errors = append(errors, map[string]string{
				"field":   "text_content",
				"message": err.Error(),
			})
		}
	}

	// Check for undefined variables (warnings, not errors)
	sampleCtx := ps.contextBuilder.BuildSampleContext()
	var warnings []mailing.TemplateValidationError

	if req.Subject != "" {
		warnings = append(warnings, ps.templateSvc.ValidateVariables(req.Subject, sampleCtx)...)
	}
	if req.HTMLContent != "" {
		warnings = append(warnings, ps.templateSvc.ValidateVariables(req.HTMLContent, sampleCtx)...)
	}
	if req.TextContent != "" {
		warnings = append(warnings, ps.templateSvc.ValidateVariables(req.TextContent, sampleCtx)...)
	}

	response["errors"] = errors
	response["warnings"] = warnings
	response["valid"] = len(errors) == 0

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// HandlePreviewForSubscriber renders template for a specific subscriber
func (ps *PersonalizationService) HandlePreviewForSubscriber(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subscriberID := chi.URLParam(r, "subscriberID")

	subUUID, err := uuid.Parse(subscriberID)
	if err != nil {
		http.Error(w, "Invalid subscriber ID", http.StatusBadRequest)
		return
	}

	var req PreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Build context for this specific subscriber
	var campaign *mailing.Campaign
	if req.CampaignID != "" {
		campaignUUID, _ := uuid.Parse(req.CampaignID)
		campaign = &mailing.Campaign{ID: campaignUUID}
	}

	renderCtx, err := ps.contextBuilder.BuildContextFromSubscriberID(ctx, subUUID, campaign)
	if err != nil {
		http.Error(w, "Subscriber not found", http.StatusNotFound)
		return
	}

	response := PreviewResponse{
		Success: true,
		Context: renderCtx,
	}

	// Render
	if req.Subject != "" {
		response.RenderedSubject, _ = ps.templateSvc.Render("", req.Subject, renderCtx)
	}
	if req.HTMLContent != "" {
		response.RenderedHTML, _ = ps.templateSvc.Render("", req.HTMLContent, renderCtx)
	}
	if req.TextContent != "" {
		response.RenderedText, _ = ps.templateSvc.Render("", req.TextContent, renderCtx)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// HandleGetSampleContext returns the sample context for testing
func (ps *PersonalizationService) HandleGetSampleContext(w http.ResponseWriter, r *http.Request) {
	sampleCtx := ps.contextBuilder.BuildSampleContext()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"context": sampleCtx,
		"description": "Sample data used for template preview when no subscriber is selected",
	})
}
