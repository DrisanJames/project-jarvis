package api

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// MODERN CAMPAIGN BUILDER - Simplified from Ongage's complex approach
// =============================================================================
// Key simplifications:
// - Single unified campaign type (handles marketing, drip, transactional)
// - Simple sending profile selection (no complex ESP routing UI)
// - Preset throttling speeds instead of complex hour/day configurations
// - Smart send (AI-optimized timing) as a simple toggle
// - Automatic timezone handling based on subscriber data
// =============================================================================

// CampaignBuilder handles modern campaign creation and management
type CampaignBuilder struct {
	db          *sql.DB
	mailingSvc  *MailingService
	redisClient *redis.Client // optional; nil falls back to PG advisory locks
	globalHub   GlobalSuppressionChecker
}

// GlobalSuppressionChecker is the interface the send pipeline uses to check
// the single source of truth before sending. Implemented by engine.GlobalSuppressionHub.
type GlobalSuppressionChecker interface {
	IsSuppressed(email string) bool
}

// NewCampaignBuilder creates a new campaign builder
func NewCampaignBuilder(db *sql.DB, mailingSvc *MailingService) *CampaignBuilder {
	cb := &CampaignBuilder{db: db, mailingSvc: mailingSvc}
	cb.ensureSchema()
	return cb
}

// SetGlobalSuppressionHub connects the campaign builder to the global
// suppression single source of truth for pre-send scrubbing.
func (cb *CampaignBuilder) SetGlobalSuppressionHub(hub GlobalSuppressionChecker) {
	cb.globalHub = hub
}

// SetRedisClient sets the Redis client for distributed locking.
func (cb *CampaignBuilder) SetRedisClient(client *redis.Client) {
	cb.redisClient = client
}

// getOrganizationID extracts the organization ID from the request using the dynamic org context
func getOrganizationID(r *http.Request) string {
	orgID, err := GetOrgIDStringFromRequest(r)
	if err != nil {
		return "" // Return empty string on error - caller should handle
	}
	return orgID
}

// getOrganizationUUID extracts the organization ID from the request and parses it as UUID
func getOrganizationUUID(r *http.Request) uuid.UUID {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		return uuid.Nil // Return nil UUID on error - caller should handle
	}
	return orgID
}

// ensureSchema ensures the campaign table has the correct constraints
func (cb *CampaignBuilder) ensureSchema() {
	// Drop old restrictive constraint if it exists
	cb.db.Exec(`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`)
	
	// Add updated constraint with all valid statuses (including 'preparing' for edit lock window)
	cb.db.Exec(`
		ALTER TABLE mailing_campaigns 
		ADD CONSTRAINT mailing_campaigns_status_check 
		CHECK (status IN ('draft', 'scheduled', 'preparing', 'sending', 'paused', 'completed', 'completed_with_errors', 'cancelled', 'failed', 'deleted', 'sent'))
	`)
	
	// Ensure queued_count column exists for tracking enqueue progress
	cb.db.Exec(`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS queued_count INTEGER DEFAULT 0`)
	
	log.Println("CampaignBuilder: Schema constraints updated (includes 'preparing' status)")
}

// RegisterRoutes registers campaign builder routes
func (cb *CampaignBuilder) RegisterRoutes(r chi.Router) {
	r.Route("/campaigns", func(r chi.Router) {
		// Core CRUD
		r.Get("/", cb.HandleListCampaigns)
		r.Post("/", cb.HandleCreateCampaign)
		r.Get("/{id}", cb.HandleGetCampaign)
		r.Put("/{id}", cb.HandleUpdateCampaign)
		r.Delete("/{id}", cb.HandleDeleteCampaign)
		
		// Bulk operations (must be before /{id} routes)
		r.Get("/scheduled", cb.HandleGetScheduledCampaigns)
		r.Post("/cancel-all-scheduled", cb.HandleCancelAllScheduled)
		
		// Actions
		r.Post("/{id}/send", cb.HandleSendCampaign)
		r.Post("/{id}/schedule", cb.HandleScheduleCampaign)
		r.Post("/{id}/pause", cb.HandlePauseCampaign)
		r.Post("/{id}/resume", cb.HandleResumeCampaign)
		r.Post("/{id}/cancel", cb.HandleCancelCampaign)
		r.Post("/{id}/reset", cb.HandleResetCampaign)
		r.Post("/{id}/duplicate", cb.HandleDuplicateCampaign)
		r.Post("/{id}/throttle", cb.HandleSetThrottle)
		r.Post("/{id}/send-async", cb.HandleSendCampaignAsync)
		
		// Test & Preview
		r.Post("/{id}/test", cb.HandleSendTestCampaign)
		r.Get("/{id}/preview", cb.HandlePreviewCampaign)
		r.Get("/{id}/estimate", cb.HandleEstimateAudience)
		
		// Analytics
		r.Get("/{id}/stats", cb.HandleCampaignStats)
		r.Get("/{id}/timeline", cb.HandleCampaignTimeline)
	})
}
