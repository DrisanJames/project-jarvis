package api

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// AdvancedMailingService provides production-ready mailing features
type AdvancedMailingService struct {
	db *sql.DB
}

func NewAdvancedMailingService(db *sql.DB) *AdvancedMailingService {
	return &AdvancedMailingService{db: db}
}

// getOrgIDFromRequest extracts the organization ID from request via dynamic context
func getOrgIDFromRequest(r *http.Request) string {
	orgID := r.Header.Get("X-Organization-ID")
	if orgID == "" {
		// Use dynamic org context extraction
		orgIDStr, err := GetOrgIDStringFromRequest(r)
		if err != nil {
			return "" // Will be caught by handler validation
		}
		return orgIDStr
	}
	return orgID
}

// RegisterAdvancedMailingRoutes registers all advanced mailing routes
func (s *AdvancedMailingService) RegisterRoutes(r chi.Router) {
	// Bounce/Complaint Webhooks (auto-suppression to Global Suppression List)
	r.Post("/webhooks/sparkpost", s.HandleSparkPostWebhook)
	r.Post("/webhooks/ses", s.HandleSESWebhook)
	r.Post("/webhooks/mailgun", s.HandleMailgunWebhook)
	
	// A/B Testing
	r.Get("/ab-tests", s.HandleGetABTests)
	r.Post("/ab-tests", s.HandleCreateABTest)
	r.Get("/ab-tests/{testId}", s.HandleGetABTest)
	r.Post("/ab-tests/{testId}/start", s.HandleStartABTest)
	r.Post("/ab-tests/{testId}/pick-winner", s.HandlePickWinner)
	
	// Campaign Management
	r.Put("/campaigns/{campaignId}", s.HandleUpdateCampaign)
	r.Post("/campaigns/{campaignId}/clone", s.HandleCloneCampaign)
	r.Post("/campaigns/{campaignId}/schedule", s.HandleScheduleCampaign)
	r.Delete("/campaigns/{campaignId}", s.HandleDeleteCampaign)
	
	// Subscriber Import
	r.Post("/lists/{listId}/import", s.HandleImportSubscribers)
	r.Get("/imports", s.HandleGetImportJobs)
	r.Get("/imports/{jobId}", s.HandleGetImportJob)
	
	// Segments
	r.Get("/segments", s.HandleGetSegments)
	r.Post("/segments", s.HandleCreateSegment)
	r.Get("/segments/{segmentId}", s.HandleGetSegment)
	r.Put("/segments/{segmentId}", s.HandleUpdateSegment)
	r.Get("/segments/{segmentId}/preview", s.HandlePreviewSegment)
	r.Delete("/segments/{segmentId}", s.HandleDeleteSegment)
	
	// Automation Workflows
	r.Get("/automations", s.HandleGetAutomations)
	r.Post("/automations", s.HandleCreateAutomation)
	r.Get("/automations/{workflowId}", s.HandleGetAutomation)
	r.Put("/automations/{workflowId}", s.HandleUpdateAutomation)
	r.Post("/automations/{workflowId}/activate", s.HandleActivateAutomation)
	r.Post("/automations/{workflowId}/pause", s.HandlePauseAutomation)
	r.Get("/automations/{workflowId}/enrollments", s.HandleGetEnrollments)
	
	// Templates
	r.Get("/templates", s.HandleGetTemplates)
	r.Post("/templates", s.HandleCreateTemplate)
	r.Get("/templates/{templateId}", s.HandleGetTemplate)
	r.Put("/templates/{templateId}", s.HandleUpdateTemplate)
	r.Post("/templates/{templateId}/clone", s.HandleCloneTemplate)
	r.Delete("/templates/{templateId}", s.HandleDeleteTemplate)
	
	// Tags
	r.Get("/tags", s.HandleGetTags)
	r.Post("/tags", s.HandleCreateTag)
	r.Post("/subscribers/{subscriberId}/tags", s.HandleAssignTags)
	
	// Enhanced Analytics
	r.Get("/analytics/campaigns/{campaignId}/timeline", s.HandleCampaignTimeline)
	r.Get("/analytics/campaigns/{campaignId}/domains", s.HandleCampaignByDomain)
	r.Get("/analytics/campaigns/{campaignId}/devices", s.HandleCampaignByDevice)
	r.Get("/analytics/overview", s.HandleAnalyticsOverview)
}

// Webhook handlers: mailing_webhooks.go
// A/B test handlers: mailing_ab_test.go
// Campaign ops handlers: mailing_campaign_ops.go
// Import handlers: mailing_import.go

