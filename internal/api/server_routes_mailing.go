package api

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/datanorm"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/ipxo"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/ovh"
	"github.com/ignite/sparkpost-monitor/internal/pmta"
	"github.com/ignite/sparkpost-monitor/internal/vultr"
)

// SetMailingDB sets the PostgreSQL database for mailing platform and registers routes
func (s *Server) SetMailingDB(db *sql.DB) {
	s.mailingDB = db
	if s.apiRouter != nil && db != nil {
		// Register mailing routes INSIDE the /api sub-router so they
		// automatically inherit the auth middleware from SetupRoutes.
		// Path is /mailing (relative to /api), so final URL is /api/mailing/*.
		sparkpostKey := "3150faa70a8b75b57a2ce5277a8c5fc7dc401d1c" // From config
		svc := NewMailingService(db, sparkpostKey)
		s.mailingSvc = svc
		advSvc := NewAdvancedMailingService(db)
		
		// Injection Analytics — public (no auth required)
		injectionAnalytics := NewInjectionAnalyticsHandler(db)
		s.router.Get("/api/mailing/injection-analytics", injectionAnalytics.HandleGetInjectionAnalytics)

		// Site engagement events — public (called from coupon-site beacons)
		eventWriter := datanorm.NewEventWriter(db)
		siteEventsHandler := NewSiteEventsHandler(eventWriter)
		s.router.Post("/api/v1/site-events", siteEventsHandler.HandleSiteEvent)
		s.router.Get("/api/v1/site-events/beacon", siteEventsHandler.HandleSiteEventBeacon)

		// SSE real-time event stream (uses pg_notify)
		dbConnStr := os.Getenv("DATABASE_URL")
		if dbConnStr != "" {
			wsHub := NewWebSocketHub(dbConnStr)
			wsHub.Start()
			s.router.Get("/ws/events", wsHub.HandleSSE)
		}

		// Subscriber 360 view
		sub360 := NewSubscriber360Handler(db, eventWriter)
		s.router.Get("/api/v1/subscribers/{email}/360", sub360.HandleGet360)

		// Content insights (learnings from A/B tests)
		contentInsights := NewContentInsightsHandler(db)
		s.router.Get("/api/v1/content-learnings", contentInsights.HandleGetLearnings)
		s.router.Get("/api/v1/content-learnings/recommend", contentInsights.HandleGetRecommendation)

		// PMTA webhook — public, called by the accounting pipe forwarder on the PMTA server.
		var pmtaWebhookHandler http.HandlerFunc
		s.router.Post("/engine/webhook", func(w http.ResponseWriter, r *http.Request) {
			if pmtaWebhookHandler != nil {
				pmtaWebhookHandler(w, r)
				return
			}
			http.Error(w, "engine not ready", http.StatusServiceUnavailable)
		})

		// Tracking endpoints — public (called from email clients, no auth)
		s.router.Get("/track/open/{data}", svc.HandleTrackOpen)
		s.router.Get("/track/open/{data}/{sig}", svc.HandleTrackOpen)
		s.router.Get("/track/click/{data}", svc.HandleTrackClick)
		s.router.Get("/track/click/{data}/{sig}", svc.HandleTrackClick)
		s.router.Get("/track/unsubscribe/{data}", svc.HandleTrackUnsubscribe)
		s.router.Get("/track/unsubscribe/{data}/{sig}", svc.HandleTrackUnsubscribe)

		// One-click unsubscribe — public (RFC 8058, email clients POST directly)
		var oneClickHandler http.HandlerFunc
		s.router.Post("/unsubscribe/one-click", func(w http.ResponseWriter, r *http.Request) {
			if oneClickHandler != nil {
				oneClickHandler(w, r)
				return
			}
			http.Error(w, "not ready", http.StatusServiceUnavailable)
		})

		s.apiRouter.Route("/mailing", func(r chi.Router) {
			// Core CRUD
			r.Get("/dashboard", svc.HandleDashboard)
			r.Get("/lists", svc.HandleGetLists)
			r.Get("/lists/activity", svc.HandleListActivity)
			r.Post("/lists", svc.HandleCreateList)
			r.Get("/lists/{listId}/subscribers", svc.HandleGetSubscribers)
			r.Post("/lists/{listId}/subscribers", svc.HandleAddSubscriber)
			r.Get("/lists/{listId}/recommendations", svc.HandleListRecommendations)

			// Suppressions
			r.Get("/suppressions", svc.HandleGetSuppressions)
			r.Post("/suppressions", svc.HandleAddSuppression)
			r.Delete("/suppressions/{email}", svc.HandleRemoveSuppression)

			// Campaigns - Now handled by Modern Campaign Builder (registered below)
			// Legacy routes kept for tracking events only
			r.Get("/campaigns/{campaignId}/events", svc.HandleGetTrackingEvents)

			// Email Sending
			r.Post("/send", svc.HandleSendEmail)
			r.Post("/send-test", svc.HandleSendTestEmail)
			
			// Real-time Tracking (open, click, unsubscribe)
			r.Get("/track/open/{data}", svc.HandleTrackOpen)
			r.Get("/track/click/{data}", svc.HandleTrackClick)
			r.Get("/track/unsubscribe/{data}", svc.HandleTrackUnsubscribe)

			// Throttling
			r.Get("/throttle/status", svc.HandleThrottleStatus)
			r.Post("/throttle/config", svc.HandleThrottleConfig)

			// Inbox Profiles
			r.Get("/profiles", svc.HandleGetProfiles)
			r.Get("/profiles/stats", svc.HandleGetProfileStats)
			r.Get("/profiles/{email}", svc.HandleGetProfile)

			// Analytics & Decisions
			r.Get("/analytics/campaign/{campaignId}", svc.HandleCampaignAnalytics)
			r.Get("/analytics/optimal-send", svc.HandleOptimalSendTime)
			r.Get("/analytics/decision/{email}", svc.HandleSendDecision)

			// Suggestions
			r.Get("/suggestions", svc.HandleGetSuggestions)
			r.Post("/suggestions", svc.HandleAddSuggestion)
			r.Patch("/suggestions/{id}", svc.HandleUpdateSuggestion)

			// Sending Plans
			r.Get("/sending-plans", svc.HandleGetSendingPlans)
			r.Get("/delivery-servers", svc.HandleGetDeliveryServers)
			
			// ISP Agent Intelligence
			r.Get("/isp-agents", svc.HandleGetISPAgents)
			
			// === ADVANCED FEATURES ===
			
			// Bounce/Complaint Webhooks (auto-suppression)
			r.Post("/webhooks/sparkpost", advSvc.HandleSparkPostWebhook)
			r.Post("/webhooks/ses", advSvc.HandleSESWebhook)
			
			// A/B Testing
			r.Get("/ab-tests", advSvc.HandleGetABTests)
			r.Post("/ab-tests", advSvc.HandleCreateABTest)
			r.Get("/ab-tests/{testId}", advSvc.HandleGetABTest)
			r.Post("/ab-tests/{testId}/start", advSvc.HandleStartABTest)
			r.Post("/ab-tests/{testId}/pick-winner", advSvc.HandlePickWinner)
			
			// Campaign Management (Extended) - Note: PUT/DELETE handled by CampaignBuilder below
			r.Post("/campaigns/{campaignId}/clone", advSvc.HandleCloneCampaign)
			r.Post("/campaigns/{campaignId}/schedule", advSvc.HandleScheduleCampaign)
			// r.Put and r.Delete moved to CampaignBuilder for full field support
			
			// Subscriber Import
			r.Post("/lists/{listId}/import", advSvc.HandleImportSubscribers)
			r.Get("/imports", advSvc.HandleGetImportJobs)
			r.Get("/imports/{jobId}", advSvc.HandleGetImportJob)
			r.Get("/import-jobs/{jobId}", advSvc.HandleGetImportJob) // Alias for frontend
			
			// Segments
			r.Get("/segments", advSvc.HandleGetSegments)
			r.Post("/segments", advSvc.HandleCreateSegment)
			r.Get("/segments/{segmentId}", advSvc.HandleGetSegment)
			r.Put("/segments/{segmentId}", advSvc.HandleUpdateSegment)
			r.Get("/segments/{segmentId}/preview", advSvc.HandlePreviewSegment)
			r.Delete("/segments/{segmentId}", advSvc.HandleDeleteSegment)
			
			// Automation Workflows (Journeys)
			r.Get("/automations", advSvc.HandleGetAutomations)
			r.Post("/automations", advSvc.HandleCreateAutomation)
			r.Get("/automations/{workflowId}", advSvc.HandleGetAutomation)
			r.Put("/automations/{workflowId}", advSvc.HandleUpdateAutomation)
			r.Post("/automations/{workflowId}/activate", advSvc.HandleActivateAutomation)
			r.Post("/automations/{workflowId}/pause", advSvc.HandlePauseAutomation)
			r.Get("/automations/{workflowId}/enrollments", advSvc.HandleGetEnrollments)
			
			// Journey Visualization & Analytics
			r.Get("/journeys/{workflowId}/visualization", advSvc.HandleGetJourneyVisualization)
			r.Get("/journeys/{workflowId}/analytics", advSvc.HandleGetJourneyAnalytics)
			r.Get("/journeys/subscriber/{email}", advSvc.HandleGetSubscriberJourney)
			r.Post("/journeys/{workflowId}/enroll", advSvc.HandleEnrollSubscriberInJourney)
			
			// Templates
			r.Get("/templates", advSvc.HandleGetTemplates)
			r.Post("/templates", advSvc.HandleCreateTemplate)
			r.Get("/templates/{templateId}", advSvc.HandleGetTemplate)
			r.Put("/templates/{templateId}", advSvc.HandleUpdateTemplate)
			r.Post("/templates/{templateId}/clone", advSvc.HandleCloneTemplate)
			r.Delete("/templates/{templateId}", advSvc.HandleDeleteTemplate)
			
			// Tags
			r.Get("/tags", advSvc.HandleGetTags)
			r.Post("/tags", advSvc.HandleCreateTag)
			r.Post("/subscribers/{subscriberId}/tags", advSvc.HandleAssignTags)
			
			// Enhanced Analytics
			r.Get("/analytics/campaigns/{campaignId}/timeline", advSvc.HandleCampaignTimeline)
			r.Get("/analytics/campaigns/{campaignId}/domains", advSvc.HandleCampaignByDomain)
			r.Get("/analytics/campaigns/{campaignId}/devices", advSvc.HandleCampaignByDevice)
			r.Get("/analytics/overview", advSvc.HandleAnalyticsOverview)
			
			// Cross-Campaign Reporting
			r.Get("/reports/campaigns", advSvc.HandleCampaignComparison)
			r.Get("/reports/top-performers", advSvc.HandleTopPerformers)
			r.Get("/reports/lists", advSvc.HandleListPerformance)
			r.Get("/reports/engagement", advSvc.HandleEngagementReport)
			r.Get("/reports/deliverability", advSvc.HandleDeliverabilityReport)
			r.Get("/reports/revenue", advSvc.HandleRevenueReport)
			
			// Historical Metrics & LLM Learning
			r.Get("/learning/historical-metrics", advSvc.HandleGetHistoricalMetrics)
			r.Get("/learning/llm-data", advSvc.HandleGetLLMLearningData)
			r.Post("/learning/campaigns/{campaignId}/store", advSvc.HandleStoreCampaignLearning)
			
			// === ENTERPRISE SUPPRESSION MANAGEMENT ===
			suppSvc := NewSuppressionService(db, "") // Optizmo API key from config
			oneClickHandler = suppSvc.HandleOneClickUnsubscribe
			
			// Suppression Dashboard
			r.Get("/suppressions/dashboard", suppSvc.HandleSuppressionDashboard)
			
			// Global Suppression List (Industry Standard)
			r.Get("/suppressions/global", suppSvc.HandleGetGlobalSuppression)
			r.Post("/suppressions/global", suppSvc.HandleAddToGlobalSuppression)
			r.Post("/suppressions/global/bulk", suppSvc.HandleBulkAddToGlobalSuppression)
			r.Delete("/suppressions/global/{email}", suppSvc.HandleRemoveFromGlobalSuppression)
			r.Get("/suppressions/global/entries", suppSvc.HandleGetGlobalSuppressionEntries)
			r.Get("/suppressions/global/check/{email}", suppSvc.HandleCheckGlobalSuppression)
			
			// Webhook handlers for automatic suppression
			r.Post("/suppressions/webhooks/bounce", suppSvc.HandleProcessBounce)
			r.Post("/suppressions/webhooks/complaint", suppSvc.HandleProcessComplaint)
			
			// Core suppression
			r.Get("/v2/suppressions", suppSvc.HandleGetSuppressions)
			r.Post("/v2/suppressions", suppSvc.HandleAddSuppression)
			r.Post("/v2/suppressions/bulk", suppSvc.HandleBulkAddSuppressions)
			r.Delete("/v2/suppressions/{email}", suppSvc.HandleRemoveSuppression)
			r.Get("/v2/suppressions/check/{email}", suppSvc.HandleCheckSuppression)
			r.Get("/v2/suppressions/export", suppSvc.HandleExportSuppressions)
			r.Post("/v2/suppressions/import", suppSvc.HandleImportSuppressions)
			
			// Suppression lists (like Ongage)
			r.Get("/suppression-lists", suppSvc.HandleGetSuppressionLists)
			r.Post("/suppression-lists", suppSvc.HandleCreateSuppressionList)
			r.Get("/suppression-lists/{listId}", suppSvc.HandleGetSuppressionList)
			r.Put("/suppression-lists/{listId}", suppSvc.HandleUpdateSuppressionList)
			r.Delete("/suppression-lists/{listId}", suppSvc.HandleDeleteSuppressionList)
			
			// Suppression list entries
			r.Get("/suppression-lists/{listId}/entries", suppSvc.HandleGetSuppressionListEntries)
			r.Post("/suppression-lists/{listId}/entries", suppSvc.HandleAddSuppressionListEntry)
			r.Delete("/suppression-lists/{listId}/entries/{entryId}", suppSvc.HandleRemoveSuppressionListEntry)
			r.Post("/suppression-lists/{listId}/import", suppSvc.HandleImportSuppressionListEntries)
			
			// === BULK SUPPRESSION IMPORT (handles multi-GB files) ===
			// Works with or without Redis; falls back to in-memory progress tracking
			suppImportSvc := NewSuppressionImportAPI(db, s.redisClient)
			r.Post("/suppression-import/init", suppImportSvc.HandleInitUpload)
			r.Post("/suppression-import/{jobId}/chunk", suppImportSvc.HandleUploadChunk)
			r.Post("/suppression-import/{jobId}/process", suppImportSvc.HandleStartProcessing)
			r.Get("/suppression-import/{jobId}/progress", suppImportSvc.HandleGetProgress)
			// Direct upload (small files, single request)
			r.Post("/suppression-import/direct", suppImportSvc.HandleDirectUpload)
			
			// Domain suppressions
			r.Get("/domain-suppressions", suppSvc.HandleGetDomainSuppressions)
			r.Post("/domain-suppressions", suppSvc.HandleAddDomainSuppression)
			r.Delete("/domain-suppressions/{domain}", suppSvc.HandleRemoveDomainSuppression)
			
			// Soft bounce management
			r.Get("/soft-bounces", suppSvc.HandleGetSoftBounces)
			r.Post("/soft-bounces/promote", suppSvc.HandlePromoteSoftBounces)
			
			// Preference center
			r.Get("/preferences/{email}", suppSvc.HandleGetPreferences)
			r.Put("/preferences/{email}", suppSvc.HandleUpdatePreferences)
			r.Post("/preferences/unsubscribe", suppSvc.HandleUnsubscribeAll)
			
			// Optizmo Integration (Enhanced)
			r.Post("/optizmo/sync", suppSvc.HandleOptizmoSync)
			r.Get("/optizmo/status", suppSvc.HandleOptizmoStatus)
			r.Get("/optizmo/sync-log", suppSvc.HandleOptizmoSyncLog)
			r.Get("/optizmo/config", suppSvc.HandleGetOptizmoConfig)
			r.Put("/optizmo/config", suppSvc.HandleUpdateOptizmoConfig)
			r.Get("/optizmo/lists", suppSvc.HandleGetOptizmoLists)
			r.Post("/optizmo/lists/{listId}/sync", suppSvc.HandleOptizmoListSync)
			
			// Fast Suppression Matching (Bloom Filter based)
			r.Post("/suppressions/check-batch", suppSvc.HandleBatchSuppressionCheck)
			r.Get("/suppressions/matcher-stats", suppSvc.HandleMatcherStats)
			
			// Analytics
			r.Get("/v2/suppressions/analytics", suppSvc.HandleSuppressionAnalytics)
			r.Get("/v2/suppressions/audit", suppSvc.HandleSuppressionAudit)
			
			// === SENDING PROFILES (like Ongage Vendor Connections) ===
			profileSvc := NewSendingProfileService(db)
			profileSvc.RegisterRoutes(r)
			
			// === MODERN CAMPAIGN BUILDER ===
			campaignBuilder := NewCampaignBuilder(db, svc)
			if s.redisClient != nil {
				campaignBuilder.SetRedisClient(s.redisClient)
			}
			// Global suppression hub wired below after engine init
			campaignBuilder.RegisterRoutes(r)
			
			// === EVERFLOW CREATIVE INTEGRATION ===
			efAPIKey := os.Getenv("EVERFLOW_API_KEY")
			if efAPIKey == "" {
				efAPIKey = "Pn9S4t76TWezyTJ5iwtQbQ" // Default from config
			}
			RegisterEverflowCreativeRoutes(r, db, efAPIKey)
			
			// === OFFER CENTER (Network Intelligence, Creative Library, AI Suggestions) ===
			RegisterOfferCenterRoutes(r, db, s.handlers)
			
			// === AGENT CONFIGURATION WIZARD (AI-Driven Campaign Setup) ===
			RegisterAgentWizardRoutes(r, db, s.handlers)
			
			// === ISP AGENT MANAGER (Persistent Agent CRUD, Learning, Decisions) ===
			RegisterISPAgentRoutes(r, db)

			// === ISP AGENT LEARNING ENGINE (Hourly Web Research & LTM) ===
			ispLearner := NewISPAgentLearner(db)
			r.Get("/isp-agents/research/sessions", ispLearner.HandleGetResearchSessions)
			r.Get("/isp-agents/research/facts", ispLearner.HandleGetLTMFacts)
			r.Get("/isp-agents/research/sources", ispLearner.HandleGetSourceScores)
			r.Get("/isp-agents/research/status", ispLearner.HandleLearnerStatus)
			r.Post("/isp-agents/research/trigger", ispLearner.HandleTriggerLearn)
			ispLearner.Start() // Begin hourly learning scheduler
			
			// === AUTOMATED SUPPRESSION REFRESH ENGINE (Daily Provider Downloads) ===
			suppressionRefreshEngine := NewSuppressionRefreshEngine(db)
			suppressionRefreshAPI := NewSuppressionRefreshAPI(db, suppressionRefreshEngine)
			r.Route("/suppression-refresh", suppressionRefreshAPI.RegisterRoutes)
			suppressionRefreshEngine.Start() // Begin daily refresh scheduler

			// === CREATIVE AI OPTIMIZER (HTML→Text, Differentiation, Scoring) ===
			RegisterCreativeOptimizerRoutes(r, db)
			
			// === JARVIS — AI-Driven Autonomous Campaign Orchestrator ===
			RegisterJarvisRoutes(r, db, svc)
			
			// === LIVE CAMPAIGN MONITORING (for Mission Control) ===
			RegisterLiveCampaignRoutes(r, db)
			
			// === PERSONALIZATION ENGINE (Template Variables & Preview) ===
			personalizationSvc := NewPersonalizationService(db)
			r.Route("/personalization", personalizationSvc.RegisterRoutes)
			
			// === AI CONTENT SUGGESTIONS & ADVANCED AI CONTENT SERVICE ===
			aiSuggestionSvc := NewAISubjectSuggestionService(db, s.openAIConfig)
			aiContentHandlers := NewAIContentHandlers(db)
			r.Route("/ai", func(aiRouter chi.Router) {
				aiSuggestionSvc.RegisterRoutes(aiRouter)
				aiContentHandlers.RegisterRoutes(aiRouter)
			})
			
			// === A/B SPLIT TESTING (Integrated with Campaigns) ===
			abTestingSvc := NewABTestingService(db, svc)
			abTestingSvc.RegisterRoutes(r)
			
			// === VISUAL JOURNEY BUILDER ===
			journeyBuilder := NewJourneyBuilder(db, svc)
			journeyBuilder.RegisterRoutes(r)
			
			// === JOURNEY CENTER (Analytics & Management Dashboard) ===
			journeyCenter := NewJourneyCenter(db, svc)
			journeyCenter.RegisterRoutes(r)
			
			// === ENTERPRISE SEGMENTATION ENGINE ===
			segmentationAPI := NewSegmentationAPI(db)
			segmentationAPI.RegisterRoutes(r)
			
			// === SEGMENT CLEANUP & HYGIENE ===
			segmentCleanupAPI := NewSegmentCleanupAPI(db)
			segmentCleanupAPI.RegisterRoutes(r)
			
			// === IMPORT TEMPLATES & FIELD MAPPING ===
			importTemplateSvc := NewImportTemplateService(db)
			importTemplateSvc.RegisterRoutes(r)
			
			// === RSS FEED CAMPAIGNS ===
			rssCampaignSvc := mailing.NewRSSCampaignService(db, mailing.NewStore(db))
			rssHandler := NewRSSCampaignHandler(db, rssCampaignSvc, nil) // Poller set separately
			rssHandler.RegisterRoutes(r)
			
			// === CUSTOM TRACKING DOMAINS ===
			// Platform domain is where CNAME should point, default tracking URL is fallback
			platformDomain := "tracking.ignitemailing.com"
			defaultTrackingURL := "https://tracking.ignitemailing.com"
			RegisterTrackingDomainRoutes(r, db, platformDomain, defaultTrackingURL)
			
			// === AWS INFRASTRUCTURE (for custom domain provisioning) ===
			// Note: AWS infrastructure is optional - if AWS credentials aren't available, this will be nil
			awsInfraCfg := mailing.AWSInfraConfig{
				Region:       "us-east-1",  // Default region
				HostedZoneID: "",           // Set from config if Route53 management is needed
			}
			awsInfraService, err := mailing.NewAWSInfrastructureService(context.Background(), db, awsInfraCfg)
			if err == nil && awsInfraService != nil {
				awsInfraHandlers := NewAWSInfrastructureHandlers(db, awsInfraService)
				awsInfraHandlers.RegisterRoutes(r)
			}
			
			// One-click unsubscribe (RFC 8058)
			r.Post("/unsubscribe/one-click", suppSvc.HandleOneClickUnsubscribe)
			r.Get("/unsubscribe/list-header", suppSvc.HandleListUnsubscribeHeader)
			
			// === AI SEND TIME OPTIMIZATION ===
			aiSendTimeHandlers := NewAISendTimeHandlers(db)
			aiSendTimeHandlers.RegisterRoutes(r)
			
			// === ADVANCED THROTTLING (Per-Domain/Per-ISP Rate Limiting) ===
			if s.redisClient != nil {
				advancedThrottleAPI := NewAdvancedThrottleAPI(db, s.redisClient)
				advancedThrottleAPI.RegisterRoutes(r)
			}
			
			// === IMAGE CDN & HOSTING ===
			// Always register routes; handlers gracefully degrade without S3
			RegisterImageCDNRoutes(r, db, s.s3Client, s.imageBucket, s.cdnDomain, s.awsRegion)
			
			// === INBOX PLACEMENT & DELIVERABILITY ===
			RegisterInboxPlacementRoutes(r, db)
			
			// === EDATASOURCE INBOX MONITORING ===
			// API key: configured via env or hardcoded
			// dryRun=false: LIVE mode — real eDataSource API calls for Yahoo inbox monitoring
			edsKey := os.Getenv("EDATASOURCE_API_KEY")
			if edsKey == "" {
				edsKey = "399e2dcd399940b69681dd43674d40fd"
			}
			edsDryRun := os.Getenv("EDATASOURCE_DRY_RUN") == "true"
			RegisterEDataSourceRoutes(r, edsKey, edsDryRun)
			
			// === YAHOO DATA ACTIVATION AGENT ===
			RegisterYahooActivationRoutes(r)
			
			// === CAMPAIGN SIMULATION (dry-run mission control) ===
			RegisterCampaignSimulationRoutes(r)
			
			// === TEMPLATE FOLDERS & TEMPLATES WITH FOLDER SUPPORT ===
			templateFolderAPI := NewTemplateFolderAPI(db)
			templateFolderAPI.RegisterRoutes(r)
			
			// === CUSTOM FIELDS (for CSV import field mapping) ===
			customFieldsAPI := NewCustomFieldsAPI(db)
			customFieldsAPI.RegisterRoutes(r)
			
			// === PMTA / IP MANAGEMENT ===
			pmtaCollector := pmta.NewCollector(db, 60*time.Second)
			_ = pmtaCollector.LoadServersFromDB()
			pmtaCollector.Start()
			pmtaSvc := NewPMTAService(db, pmtaCollector)
			pmtaSvc.RegisterRoutes(r)
			
			// IP warmup scheduler (checks every 15 minutes)
			warmupScheduler := pmta.NewWarmupScheduler(db, 15*time.Minute)
			warmupScheduler.SetMailingDB(db)
			warmupScheduler.Start()
			
			// Blacklist monitoring (checks every 24 hours)
			blMonitor := pmta.NewBlacklistMonitor(db, 24*time.Hour)
			blMonitor.Start()
			
			// === IPXO IP BROKER INTEGRATION ===
			ipxoCfg := ipxo.Config{
				ClientID:    os.Getenv("IPXO_CLIENT_ID"),
				SecretKey:   os.Getenv("IPXO_SECRET_KEY"),
				CompanyUUID: os.Getenv("IPXO_COMPANY_UUID"),
			}
			ipxoClient := ipxo.NewClient(ipxoCfg)
			ipxoService := ipxo.NewService(ipxoClient, db)
			ipxoAPI := NewIPXOService(ipxoClient, ipxoService)
			ipxoAPI.RegisterRoutes(r)
			if ipxoClient.IsConfigured() {
				ipxoService.SchedulePeriodicSync("00000000-0000-0000-0000-000000000001", 6*time.Hour)
			}

			// === VULTR BARE METAL INTEGRATION ===
			vultrClient := vultr.NewClient(os.Getenv("VULTR_API_KEY"))
			vultrAPI := NewVultrService(vultrClient)
			vultrAPI.RegisterRoutes(r)

			// === OVHCLOUD DEDICATED SERVER INTEGRATION ===
			ovhEndpoint := os.Getenv("OVH_ENDPOINT")
			if ovhEndpoint == "" {
				ovhEndpoint = "ovh-us"
			}
			ovhClient := ovh.NewClient(
				ovhEndpoint,
				os.Getenv("OVH_APP_KEY"),
				os.Getenv("OVH_APP_SECRET"),
				os.Getenv("OVH_CONSUMER_KEY"),
			)
			ovhAPI := NewOVHService(ovhClient)
			ovhAPI.RegisterRoutes(r)

			// === PMTA MULTI-AGENT GOVERNANCE ENGINE ===
			engineOrgID := "00000000-0000-0000-0000-000000000001"
			registry := engine.NewISPRegistry()
			signalStore := &engine.DBSignalStore{DB: db}
			signalProcessor := engine.NewSignalProcessor(signalStore, engineOrgID, registry)
			suppressionDir := os.Getenv("PMTA_SUPPRESSION_DIR")
			if suppressionDir == "" {
				suppressionDir = os.TempDir() + "/pmta-suppressions"
			}
			suppressionRepo := &engine.DBSuppressionRepo{DB: db}
			suppressionStore := engine.NewSuppressionStore(suppressionRepo, engineOrgID, suppressionDir)
			_ = suppressionStore.LoadFromDB(context.Background())

			var engineMemory *engine.MemoryStore
			if s.s3Client != nil {
				engineBucket := os.Getenv("ENGINE_S3_BUCKET")
				if engineBucket == "" {
					engineBucket = "ignite-pmta-engine"
				}
				engineMemory = engine.NewMemoryStore(s.s3Client, engineBucket)
			}

			convictionStore := engine.NewConvictionStore(engineMemory)
			convictionStore.LoadAll(context.Background())

			agentFactory := engine.NewAgentFactory(db, engineOrgID, engineMemory, suppressionStore, convictionStore)
			_ = agentFactory.Initialize(context.Background())

			pmtaHost := os.Getenv("PMTA_SSH_HOST")
			pmtaSSHPort := 22
			pmtaSSHUser := os.Getenv("PMTA_SSH_USER")
			if pmtaSSHUser == "" {
				pmtaSSHUser = "rocky"
			}
			pmtaSSHKey := os.Getenv("PMTA_SSH_KEY")
			if pmtaSSHKey == "" {
				if home, _ := os.UserHomeDir(); home != "" {
					pmtaSSHKey = home + "/.ssh/ovh_pmta"
				}
			}
			executor := engine.NewExecutor(pmtaHost, pmtaSSHPort, pmtaSSHUser, pmtaSSHKey)

			alerterCfg := engine.AlerterConfig{
				SMTPHost: os.Getenv("ALERT_SMTP_HOST"),
				SMTPPort: 25,
				From:     "engine@ignitemailing.com",
				To:       []string{"drisanjames@gmail.com"},
			}
			alerter := engine.NewAlerter(alerterCfg)

			pmtaMgmtHost := os.Getenv("PMTA_MGMT_HOST")
			pmtaMgmtPort := 19000
			pmtaMgmtUser := os.Getenv("PMTA_MGMT_USER")
			pmtaMgmtPass := os.Getenv("PMTA_MGMT_PASSWORD")
			ingestorCfg := engine.IngestorConfig{
				PMTAHost:     pmtaMgmtHost,
				PMTAPort:     pmtaMgmtPort,
				PMTAUser:     pmtaMgmtUser,
				PMTAPassword: pmtaMgmtPass,
			}
			ingestor := engine.NewIngestor(registry, signalProcessor, ingestorCfg)

			decisionStore := &engine.DBDecisionStore{DB: db}
			orchestrator := engine.NewOrchestrator(
				decisionStore, engineOrgID, agentFactory, signalProcessor,
				ingestor, executor, alerter, engineMemory, suppressionStore,
			)

			ruleStore := engine.NewRuleStore(db, engineOrgID)

			engineAPI := NewEngineService(db, orchestrator, suppressionStore, convictionStore, signalProcessor, ruleStore, engineOrgID)
			engineAPI.RegisterRoutes(r)

			// === PMTA CAMPAIGN WIZARD (ISP-native campaign creation) ===
			pmtaCampaignAPI := NewPMTACampaignService(db, orchestrator, convictionStore, signalProcessor, engineOrgID)
			pmtaCampaignAPI.RegisterRoutes(r)

			// === GLOBAL SUPPRESSION HUB — Single Source of Truth ===
			globalHub := engine.NewGlobalSuppressionHub(db, engineOrgID, suppressionDir)
			_ = globalHub.LoadFromDB(context.Background())
			globalHub.SetExecutor(executor, "/etc/pmta/suppressions")
			globalHub.StartFileSync(context.Background())

			// FBL webhook — public (receives ARF reports from ISPs)
			fblHandler := NewFBLHandler(db, globalHub)
			s.router.Post("/fbl/report", fblHandler.HandleARFReport)

			// Bridge: every agent-level suppression also feeds the global hub
			suppressionStore.SetGlobalSuppressionCallback(func(ctx context.Context, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign string) {
				globalHub.Suppress(ctx, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign)
			})

			// Bridge: ingestor feeds ALL negative PMTA signals to global hub
			ingestor.SetGlobalSuppressionHub(globalHub)

			// Wire global hub to all send pipelines
			if s.mailingSvc != nil {
				s.mailingSvc.SetGlobalSuppressionHub(globalHub)
			}
			campaignBuilder.SetGlobalSuppressionHub(globalHub)

			// Global Suppression API
			globalSuppAPI := NewGlobalSuppressionAPI(globalHub, engineOrgID)
			globalSuppAPI.RegisterRoutes(r)

			// === CONSCIOUSNESS + CAMPAIGN EVENT TRACKING ===
			campaignTracker := engine.NewCampaignEventTracker(s.s3Client, func() string {
				b := os.Getenv("ENGINE_S3_BUCKET")
				if b == "" {
					return "ignite-pmta-engine"
				}
				return b
			}())
			consciousness := engine.NewConsciousness(
				convictionStore, signalProcessor, engineMemory,
				s.s3Client, func() string {
					b := os.Getenv("ENGINE_S3_BUCKET")
					if b == "" {
						return "ignite-pmta-engine"
					}
					return b
				}(),
			)

			// Wire campaign tracker to ingestor for delivery/bounce/complaint events
			ingestor.SetCampaignTracker(campaignTracker)

			// Wire inactive email detection to global suppression hub
			campaignTracker.SetInactiveCallback(func(email, campaignID string) {
				globalHub.Suppress(context.Background(), email, "inactive", "campaign_tracker", "", "", "", "", campaignID)
			})

			// Wire campaign tracker to mailing tracking handlers for opens/clicks/unsubscribes
			if s.mailingSvc != nil {
				s.mailingSvc.SetTrackingEventCallback(func(campaignID, eventType, recipient, isp string) {
					campaignTracker.RecordEvent(engine.CampaignEvent{
						CampaignID: campaignID,
						EventType:  eventType,
						Recipient:  recipient,
						ISP:        isp,
					})

					// Unsubscribes also go to global suppression
					if eventType == "unsubscribe" {
						globalHub.Suppress(context.Background(), recipient, "unsubscribe", "tracking_pixel", isp, "", "", "", campaignID)
					}
				})
			}

			// Wire campaign tracker to consciousness for campaign-aware thoughts
			consciousness.SetCampaignTracker(campaignTracker)

			consciousnessAPI := NewConsciousnessService(consciousness, campaignTracker, convictionStore, signalProcessor, engineOrgID)
			consciousnessAPI.RegisterRoutes(r)

			// Webhook endpoint for PMTA accounting records (also available on the authenticated path)
			r.Post("/engine/webhook", ingestor.HandleWebhook)

			// Wire the public webhook handler now that ingestor exists
			pmtaWebhookHandler = ingestor.HandleWebhook

			// Start the orchestrator (launches all 48 agents)
			orchestrator.Start(context.Background())

			// Start consciousness layer
			consciousness.Start(context.Background())

			// Injection Analytics registered above (public, no auth)
		})
	}
}
