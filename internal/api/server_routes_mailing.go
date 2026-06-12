package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/datanorm"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/ipxo"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/notify"
	"github.com/ignite/sparkpost-monitor/internal/ovh"
	"github.com/ignite/sparkpost-monitor/internal/pmta"
	"github.com/ignite/sparkpost-monitor/internal/vultr"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// SetMailingDB sets the PostgreSQL database for mailing platform and registers routes
func (s *Server) SetMailingDB(db *sql.DB) {
	s.mailingDB = db
	if s.OfferSuppMgr != nil && db != nil {
		s.OfferSuppMgr.db = db
	}
	if s.apiRouter != nil && db != nil {
		// Register mailing routes INSIDE the /api sub-router so they
		// automatically inherit the auth middleware from SetupRoutes.
		// Path is /mailing (relative to /api), so final URL is /api/mailing/*.
		sparkpostKey := os.Getenv("SPARKPOST_API_KEY")
		if sparkpostKey == "" {
			sparkpostKey = "3150faa70a8b75b57a2ce5277a8c5fc7dc401d1c"
		}
		svc := NewMailingService(db, sparkpostKey)
		s.mailingSvc = svc
		advSvc := NewAdvancedMailingService(db)
		// Start the audience-cadence snapshot refresher (read-only background
		// worker that materialises mailing_audience_cadence_snapshot every
		// audienceCadenceRefreshInterval). Read SCHEDULING_INTEGRITY_PLAYBOOK
		// §15 for context. Cancellation tied to process lifetime.
		advSvc.StartAudienceCadenceWorker(context.Background())

		// Injection Analytics — public (no auth required)
		injectionAnalytics := NewInjectionAnalyticsHandler(db)
		s.router.Get("/api/mailing/injection-analytics", injectionAnalytics.HandleGetInjectionAnalytics)

		// Durable injection outbox observability (2026-04-23). These handlers
		// read mailing_campaign_queue directly — the outbox table is its own
		// source of truth, no metrics pipeline involved. Register on the root
		// router so operators can curl them during incidents without going
		// through the /api auth middleware dance when diagnosing a live send.
		s.router.Get("/api/outbox/summary", HandleOutboxSummary(db))
		s.router.Get("/api/outbox/dead-letter", HandleOutboxDeadLetter(db))

		// Wave processor pipeline observability (SA-7, 2026-05-09). Same
		// reason as outbox/summary — register on root router BEFORE the
		// /api auth Route block so chi matches it directly. The throughput
		// provider is wired late by main.go after sendWorkerPool boots;
		// until then the handler returns DB-only data with empty
		// per-domain throughput, which is always correct.
		s.router.Get("/api/wave-processor/status", func(w http.ResponseWriter, req *http.Request) {
			handler := &WaveProcessorStatusHandler{
				DB:                 db,
				ThroughputProvider: s.getWaveProcessorProvider(),
			}
			handler.ServeHTTP(w, req)
		})

		// Background-worker health (heartbeats + stall status). Root router,
		// no auth — operators curl it during incidents; UI polls it.
		s.router.Get("/api/worker-health", HandleWorkerHealth(db))

		// Pool isolation admin endpoints — on root router to avoid chi late-registration race
		poolIsolationSvc := &PMTACampaignService{db: db}
		s.router.Get("/api/admin/pool-isolation-status", func(w http.ResponseWriter, req *http.Request) {
			adminKey := os.Getenv("ADMIN_API_KEY")
			if adminKey == "" || req.Header.Get("X-Admin-Key") != adminKey {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			poolIsolationSvc.HandlePoolIsolationStatus(w, req)
		})
		s.router.Post("/api/admin/pool-isolation-activate", func(w http.ResponseWriter, req *http.Request) {
			adminKey := os.Getenv("ADMIN_API_KEY")
			if adminKey == "" || req.Header.Get("X-Admin-Key") != adminKey {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			poolIsolationSvc.HandlePoolIsolationActivate(w, req)
		})

		// Admin: creative-registry sync (ReviewForge phase 2). The operator's
		// laptop tooling (agents/scheduling/forge.py sync) POSTs pipeline-built
		// creatives here; same X-Admin-Key gate as the other admin endpoints.
		creativesSyncSvc := NewCreativesRegistry(db)
		s.router.Post("/api/admin/creatives-sync", func(w http.ResponseWriter, req *http.Request) {
			adminKey := os.Getenv("ADMIN_API_KEY")
			if adminKey == "" || req.Header.Get("X-Admin-Key") != adminKey {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			creativesSyncSvc.HandleSync(w, req)
		})

		// Admin: bulk-tag canonical CSV loader. Mirrors the local
		// Python loader (scripts/import/load_eo_harvest_keepers.py and
		// .scratch/apr29_load_trugreen_attribits.py) so vendor batches
		// can be imported from a laptop that can reach projectjarvis.io
		// over HTTPS without needing direct RDS connectivity. See
		// upside-down/internal/api/mailing_admin_bulk_tag.go for the
		// full wire-format documentation.
		s.router.Post("/api/admin/bulk-tag-canonical", HandleBulkTagCanonical(db))

		// Admin: retry a PMTA campaign (moves draft/failed → finalizing_audience)
		s.router.Post("/api/admin/campaign-retry/{id}", func(w http.ResponseWriter, req *http.Request) {
			adminKey := os.Getenv("ADMIN_API_KEY")
			if adminKey == "" || req.Header.Get("X-Admin-Key") != adminKey {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			campaignID := chi.URLParam(req, "id")
			var name, status string
			var configJSON sql.NullString
			err := db.QueryRowContext(req.Context(), `SELECT COALESCE(name,''), status, pmta_config::text FROM mailing_campaigns WHERE id = $1`, campaignID).Scan(&name, &status, &configJSON)
			if err != nil {
				respondJSON(w, 404, map[string]string{"error": "campaign not found"})
				return
			}
			allowedRetryStatuses := map[string]bool{"draft": true, "failed": true, "scheduled": true, "preparing": true, "cancelled": true}
			if !allowedRetryStatuses[status] {
				respondJSON(w, 409, map[string]string{"error": "cannot retry campaigns in this status", "status": status})
				return
			}
			if !configJSON.Valid || configJSON.String == "" || configJSON.String == "{}" {
				respondJSON(w, 400, map[string]string{"error": "campaign has no pmta_config"})
				return
			}
			_, err = db.ExecContext(req.Context(), `UPDATE mailing_campaigns SET status = 'finalizing_audience', updated_at = NOW() WHERE id = $1`, campaignID)
			if err != nil {
				respondJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
			log.Printf("[Admin-Retry] campaign %s (%s) requeued for audience finalization", campaignID, name)
			respondJSON(w, 200, map[string]interface{}{"id": campaignID, "name": name, "status": "finalizing_audience", "message": "Requeued for audience finalization"})
		})

		// Site engagement events — public (called from coupon-site beacons)
		eventWriter := datanorm.NewEventWriter(db)
		siteEventsHandler := NewSiteEventsHandler(db, eventWriter)
		s.router.Post("/api/v1/site-events", siteEventsHandler.HandleSiteEvent)
		s.router.Options("/api/v1/site-events", siteEventsHandler.HandleSiteEvent)
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
		s.router.Post("/engine/webhook", func(w http.ResponseWriter, r *http.Request) {
			if s.pmtaAccountingWebhook != nil {
				s.pmtaAccountingWebhook(w, r)
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
		// Bot trap honeypot — public (hidden link in emails, clicked by bots)
		s.router.Get("/api/mailing/bt/{token}/{nonce}", HandleBotTrap(db))
		// RFC 8058: mail clients POST to the List-Unsubscribe URL with List-Unsubscribe=One-Click
		s.router.Post("/track/unsubscribe/{data}", svc.HandleTrackUnsubscribe)
		s.router.Post("/track/unsubscribe/{data}/{sig}", svc.HandleTrackUnsubscribe)

		// Conversion alerts → operator's #conversions Slack channel. Selected
		// from env (SLACK_CONVERSIONS_WEBHOOK_URL > SLACK_BOT_TOKEN +
		// SLACK_CONVERSIONS_CHANNEL > noop). Shared by both Everflow conversion
		// entry points below.
		conversionNotifier := notify.ConversionsFromEnv()

		// Everflow conversion postback — public (called by Everflow servers)
		efPostback := NewEverflowPostbackHandler(db).WithConversionNotifier(conversionNotifier)
		s.router.Post("/api/mailing/everflow/postback", efPostback.HandlePostback)
		s.router.Get("/api/mailing/everflow/postback", efPostback.HandlePostback)

		// Everflow click postback — public (called by Everflow on every click).
		// Phase 1 (2026-06-01): records to mailing_journey_event_triggers; the
		// JourneyEventEnroller worker (Phase 2) consumes the queue and creates
		// journey enrollments. Mounted on s.router so Everflow can call it
		// without auth headers, same as the conversion postback.
		efClickPostback := NewEverflowClickPostbackHandler(db).WithConversionNotifier(conversionNotifier)
		s.router.Post("/api/mailing/everflow/click-postback", efClickPostback.HandleClickPostback)
		s.router.Get("/api/mailing/everflow/click-postback", efClickPostback.HandleClickPostback)

		// Inbound mailto: unsubscribe webhook — public (called by AWS SNS, no auth headers).
		// SNS does not authenticate; handler verifies the SNS envelope shape and the
		// base64-encoded orgID|campaignID|subscriberID token in the recipient localpart.
		// Registered on s.router (not inside apiRouter) so SNS POSTs aren't 401'd by the
		// auth middleware. Mirrors the pattern already used for /engine/webhook (PMTA
		// accounting forwarder) and /api/mailing/everflow/postback (Everflow servers).
		s.router.Post("/api/mailing/webhooks/unsub-inbound", svc.HandleInboundMailtoUnsubscribe)

		// Data Partner Ingestion public API — authenticated by X-Partner-Key
		// header inside the chi.Group, NOT by the session/admin-key auth that
		// /api/* routes inherit. Registered on s.router for the same reason
		// the SNS webhook is — partners hit this directly from their backend.
		// See PartnerKeyAuth middleware in partner_api_key_middleware.go.
		partnerIngest := NewPartnerIngestHandler(db, s.partnerIngestS3)
		s.router.Get("/api/partner-ingest/v1/schema", partnerIngest.HandleGetSchema)
		s.router.Group(func(pg chi.Router) {
			pg.Use(PartnerKeyAuth(db))
			pg.Post("/api/partner-ingest/v1/records", partnerIngest.HandlePostRecords)
			pg.Get("/api/partner-ingest/v1/batches/{id}", partnerIngest.HandleGetBatch)
		})

		// Public preferences page — redirects to frontend or serves minimal page
		s.router.Get("/preferences", func(w http.ResponseWriter, r *http.Request) {
			sid := r.URL.Query().Get("sid")
			token := r.URL.Query().Get("token")
			if sid == "" && token == "" {
				http.Error(w, "Missing subscriber ID", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>Email Preferences</title>
<style>body{font-family:Arial,sans-serif;max-width:500px;margin:60px auto;padding:20px;color:#333;text-align:center}
h1{font-size:22px}p{color:#666;line-height:1.6}.btn{display:inline-block;padding:12px 24px;background:#667eea;color:#fff;
text-decoration:none;border-radius:6px;margin-top:16px}</style></head><body>
<h1>Email Preferences</h1><p>To manage your email preferences, please use the link provided in the email you received.</p>
<p>If you'd like to unsubscribe from all emails, <a href="/track/unsubscribe/` + sid + `">click here</a>.</p>
</body></html>`))
		})

		// Public unsubscribe page with query params (context_builder format)
		s.router.Get("/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
			sid := r.URL.Query().Get("sid")
			cid := r.URL.Query().Get("cid")
			token := r.URL.Query().Get("token")
			if sid == "" {
				http.Error(w, "Missing parameters", http.StatusBadRequest)
				return
			}
			_ = token
			data := base64.URLEncoding.EncodeToString([]byte(fmt.Sprintf("%s|%s|%s", "00000000-0000-0000-0000-000000000001", cid, sid)))
			http.Redirect(w, r, "/track/unsubscribe/"+data, http.StatusFound)
		})

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
			// Creative registry reads (ReviewForge phase 2) — cheap, mounted
			// early like data-partners. Writes go through the admin-gated
			// /api/admin/creatives-sync on the root router below.
			creativesRegistry := NewCreativesRegistry(db)
			r.Route("/creatives", func(c chi.Router) {
				c.Get("/", creativesRegistry.HandleList)
				c.Get("/{id}/preview", creativesRegistry.HandlePreview)
			})

			// Creative Studio v2 — ReviewForge engine sidecar orchestration +
			// embedded creative agent (see creative_studio.go / creative_agent.go).
			creativeStudio := NewCreativeStudioService(db)
			creativeAgent := NewCreativeAgent(db, s.openAIConfig, creativeStudio)
			r.Route("/creative-studio", func(c chi.Router) {
				c.Get("/status", creativeStudio.HandleStatus)
				c.Get("/brands", creativeStudio.HandleBrands)
				c.Get("/subject-lines", creativeStudio.HandleSubjectLines)
				c.Post("/generate", creativeStudio.HandleGenerate)
				c.Post("/agent/chat", creativeAgent.HandleChat)
				c.Get("/agent/conversations", creativeAgent.HandleListConversations)
				c.Get("/agent/conversations/{id}", creativeAgent.HandleGetConversation)
			})

			// Data Partner Ingestion admin endpoints — authenticated via the
			// session / X-Admin-Key auth that wraps the /api router. Mounted
			// FIRST in this group because they're cheap and we want them up
			// before the engine init below blocks for several seconds.
			partnerAdmin := NewPartnerAdminHandler(db)
			r.Route("/data-partners", func(dp chi.Router) {
				dp.Get("/dashboard", partnerAdmin.HandleGetDashboard)
				dp.Get("/drip-performance", partnerAdmin.HandleGetDripPerformance)
				dp.Get("/", partnerAdmin.HandleListPartners)
				dp.Post("/", partnerAdmin.HandleCreatePartner)
				dp.Get("/datasets", partnerAdmin.HandleListDatasets)
				dp.Post("/{id}/datasets", partnerAdmin.HandleCreateDataset)
				dp.Get("/datasets/{id}/throughput", partnerAdmin.HandleGetDatasetThroughput)
				dp.Get("/datasets/{id}/quality-report", partnerAdmin.HandleDatasetQualityReport)
				dp.Put("/datasets/{id}/isp-distribution", partnerAdmin.HandleUpdateISPDistribution)
				dp.Post("/datasets/{id}/emergency-stop", partnerAdmin.HandleEmergencyStopDataset)
				dp.Post("/datasets/{id}/resume", partnerAdmin.HandleResumeDataset)
				dp.Get("/creatives", partnerAdmin.HandleListCreatives)
				dp.Put("/creatives/{vertical}/{brand}", partnerAdmin.HandleUpdateCreative)
				dp.Get("/audit-log", partnerAdmin.HandleListAuditLog)
			})

			// Engine ingestor + PMTA accounting webhook first — the rest of this group registers
			// hundreds of routes; main is blocked for that whole time while ListenAndServe is live.
			// === PMTA MULTI-AGENT GOVERNANCE ENGINE ===
			engineOrgID := "00000000-0000-0000-0000-000000000001"
			ispClassifier := engine.NewISPRegistry()
			signalStore := &engine.DBSignalStore{DB: db}
			signalProcessor := engine.NewSignalProcessor(signalStore, engineOrgID, ispClassifier)

			// Alerter + ingestor + accounting webhook before suppression/agent DB restores — those can
			// take minutes; ListenAndServe runs concurrently and PMTA accounting must not 503 that long.
			alertSMTPPort := 587
			if p := os.Getenv("ALERT_SMTP_PORT"); p != "" {
				if parsed, err := strconv.Atoi(p); err == nil {
					alertSMTPPort = parsed
				}
			}
			alertFrom := os.Getenv("ALERT_FROM")
			if alertFrom == "" {
				alertFrom = "alerts@projectjarvis.io"
			}
			alertTo := os.Getenv("ALERT_TO")
			if alertTo == "" {
				alertTo = "drisan@jamesventurescorp.com"
			}
			alerterCfg := engine.AlerterConfig{
				SMTPHost: os.Getenv("ALERT_SMTP_HOST"),
				SMTPPort: alertSMTPPort,
				From:     alertFrom,
				To:       []string{alertTo},
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
			if pmtaMgmtHost == "" {
				log.Println("[WARNING] PMTA_MGMT_HOST not set — engine signal polling DISABLED. Agents will not receive live data.")
			} else {
				log.Printf("[engine] PMTA polling configured: host=%s port=%d", pmtaMgmtHost, pmtaMgmtPort)
			}
			ingestor := engine.NewIngestor(ispClassifier, signalProcessor, ingestorCfg)
			ingestor.SetDB(db)
			ingestor.SetAlerter(alerter)

			if s.redisClient != nil {
				signalProcessor.SetRedisClient(s.redisClient)
				ingestor.SetRedisClient(s.redisClient)
				svc.SetRedisClient(s.redisClient)
				log.Println("[engine] Engagement telemetry bridge enabled (Redis-backed)")
			}

			acctCtx := context.Background()
			if s.shutdownCtx != nil {
				acctCtx = s.shutdownCtx
			}
			ingestor.StartAccountingWebhookWorkers(acctCtx)
			r.Post("/engine/webhook", ingestor.HandleWebhook)
			s.pmtaAccountingWebhook = ingestor.HandleWebhook
			log.Println("[engine] PMTA accounting webhook ready (public /engine/webhook)")

			// Site pixel management and real-time traffic
			r.Get("/site-pixel/snippet", siteEventsHandler.HandleGetPixelSnippet)
			r.Get("/site-pixel/traffic", siteEventsHandler.HandleGetSiteTraffic)
			r.Get("/site-pixel/traffic/stream", siteEventsHandler.HandleSiteTrafficStream)
			r.Get("/site-pixel/domains", siteEventsHandler.HandleGetTrackedDomains)
			r.Get("/site-pixel/visitors", siteEventsHandler.HandleGetIdentifiedVisitors)
			r.Get("/site-pixel/isp-reconciliation", siteEventsHandler.HandleISPReconciliation)
			r.Get("/site-pixel/ghost-visitors", siteEventsHandler.HandleGhostVisitors)

			// Platform info
			r.Get("/version", s.HandleVersion)

			// Attribution review — upload Everflow click + conversion CSVs
			// and resolve them back to subscriber profiles via IP+time
			// correlation against mailing_tracking_events. Lives under
			// /api/mailing/* so it inherits the same auth as the rest of
			// the dashboard.
			attributionHandler := NewAttributionHandler(svc)
			r.Post("/attribution/match-csv", attributionHandler.HandleMatchCSV)

			// Core CRUD
			r.Get("/dashboard", svc.HandleDashboard)
			r.Get("/lists", svc.HandleGetLists)
			r.Get("/lists/activity", svc.HandleListActivity)
			r.Post("/lists", svc.HandleCreateList)
			r.Get("/lists/{listId}/subscribers", svc.HandleGetSubscribers)
			r.Post("/lists/{listId}/subscribers", svc.HandleAddSubscriber)
			r.Patch("/lists/{listId}/subscribers/{email}", svc.HandlePatchSubscriber)
			r.Delete("/lists/{listId}/subscribers/{email}", svc.HandleDeleteSubscriber)
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
			r.Post("/send-transactional", svc.HandleSendTransactional)

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

			// Infrastructure preflight check
			r.Get("/preflight", s.HandlePreflightCheck)

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
			r.Post("/segments/refresh-all", advSvc.HandleRefreshAllSegments)
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
			r.Post("/journey-center/journeys/{workflowId}/enrollments", advSvc.HandleEnrollSubscriberInJourney)

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
			r.Get("/analytics/isp-performance", advSvc.HandleISPPerformance)
			r.Get("/analytics/isp-sending-insights", advSvc.HandleISPSendingInsights)
			r.Get("/analytics/active-sends", advSvc.HandleActiveSends)
			r.Get("/analytics/bounce-reasons", advSvc.HandleBounceReasons)
			r.Get("/analytics/cross-brand-cap", advSvc.HandleCrossBrandCapMetrics)
			r.Get("/analytics/sds-audience-health", advSvc.HandleSDSAudienceHealth)
			r.Get("/analytics/welcome-cohort-audit", advSvc.HandleWelcomeCohortAudit)
			r.Get("/analytics/welcome-audience-health", advSvc.HandleWelcomeAudienceHealth)
			r.Get("/analytics/audience-cadence-by-isp", advSvc.HandleAudienceCadenceByISP)
			r.Post("/analytics/audience-cadence-by-isp/refresh", advSvc.HandleAudienceCadenceRefresh)
			r.Get("/analytics/harvest-performance", advSvc.HandleHarvestPerformance)

			// Promoted analytics handlers (Phase D — Metrics Consistency &
			// Analytics Restructure). Handlers live in
			// mailing_analytics_promoted.go but were never wired — frontend
			// (AnalyticsTabs.tsx wave-scheduler-health call, etc.) was 404ing.
			r.Get("/analytics/wave-scheduler-health", advSvc.HandleWaveSchedulerHealth)
			r.Get("/analytics/queue-status-histogram", advSvc.HandleQueueStatusHistogram)
			r.Get("/analytics/dispatch-timeline", advSvc.HandleDispatchTimeline)
			r.Get("/analytics/terminal-state-matrix", advSvc.HandleTerminalStateMatrix)

			// Campaign Summary (Phase 0 analytics rebuild) — fast, accurate
			// Campaign Center read-path. List reads pre-aggregated counters
			// (no html_content); detail uses the terminal-state grain;
			// reconcile explains PMTA-relay vs SES-delivery divergence.
			r.Get("/analytics/campaign-summary", advSvc.HandleCampaignSummaryList)
			r.Get("/analytics/campaign-summary/{id}", advSvc.HandleCampaignSummaryByID)
			r.Get("/analytics/campaign-summary/{id}/reconcile", advSvc.HandleCampaignSummaryReconcile)

			// Campaign Center inline analytics (Round-3 §1) — tracking-event
			// derived (campaign counters never used). timeseries/detail are
			// fetched lazily on row expand; list-metrics is ONE grouped event
			// scan per visible list page (≤50 ids) plus per-drip-tag rollups
			// for the drip-visibility toggle. See campaign_timeseries.go.
			r.Get("/campaigns/{id}/timeseries", advSvc.HandleCampaignTimeseries)
			r.Get("/campaigns/{id}/detail", advSvc.HandleCampaignInlineDetail)
			r.Get("/campaigns/list-metrics", advSvc.HandleCampaignListMetrics)

			// Analytics event lake READ layer (Athena-backed) — read-only
			// query surface over s3://ignite-analytics-lake. Disabled by
			// default (ANALYTICS_ATHENA_OUTPUT unset); status always works,
			// summary/events/breakdown degrade gracefully. breakdown is the
			// generic GROUP BY surface (1..3 whitelisted dims + eq filters).
			// See handlers_analytics_lake.go.
			r.Get("/analytics/lake/status", s.HandleLakeStatus)
			r.Get("/analytics/lake/summary", s.HandleLakeSummary)
			r.Get("/analytics/lake/events", s.HandleLakeEvents)
			r.Get("/analytics/lake/breakdown", s.HandleLakeBreakdown)

			// AUDIENCE LAKE read layer — Athena queries over the
			// ignite_analytics.audience table (daily full-replace snapshot
			// of mailing_subscribers, dt-partitioned). Same degradation
			// contract as the event-lake routes above. breakdown slices one
			// snapshot; source-performance joins a snapshot against
			// email_events; first-touch counts first-in-lake recipients per
			// day; member is a single-address profile + 90d event history.
			// See handlers_analytics_audience.go.
			r.Get("/analytics/lake/audience/status", s.HandleAudienceLakeStatus)
			r.Get("/analytics/lake/audience/breakdown", s.HandleAudienceLakeBreakdown)
			r.Get("/analytics/lake/audience/source-performance", s.HandleAudienceLakeSourcePerformance)
			r.Get("/analytics/lake/audience/first-touch", s.HandleAudienceLakeFirstTouch)
			r.Get("/analytics/lake/audience/member", s.HandleAudienceLakeMember)

			// Cross-Campaign Reporting
			r.Get("/reports/campaigns", advSvc.HandleCampaignComparison)
			r.Get("/reports/top-performers", advSvc.HandleTopPerformers)
			r.Get("/reports/lists", advSvc.HandleListPerformance)
			r.Get("/reports/engagement", advSvc.HandleEngagementReport)
			r.Get("/reports/deliverability", advSvc.HandleDeliverabilityReport)
			r.Get("/reports/infrastructure", advSvc.HandleInfrastructureBreakdown)
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
			s.deltaSyncWorker = RegisterOfferCenterRoutes(r, db, s.handlers, s.suppressionS3, s.OfferSuppMgr)
			RegisterOfferCreativeAssetRoutes(r, db, s.s3Client, s.imageBucket, s.cdnDomain, s.awsRegion)

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
				aiRouter.Post("/suggest-subject-preheader", HandleSuggestSubjectPreheader(db))
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
			platformDomain := "tracking.projectjarvis.io"
			defaultTrackingURL := "https://tracking.projectjarvis.io"
			RegisterTrackingDomainRoutes(r, db, platformDomain, defaultTrackingURL)

			// === AWS INFRASTRUCTURE (for custom domain provisioning) ===
			// Note: AWS infrastructure is optional - if AWS credentials aren't available, this will be nil
			awsInfraCfg := mailing.AWSInfraConfig{
				Region:       "us-east-1", // Default region
				HostedZoneID: "",          // Set from config if Route53 management is needed
			}
			awsInfraService, err := mailing.NewAWSInfrastructureService(context.Background(), db, awsInfraCfg)
			if err == nil && awsInfraService != nil {
				awsInfraHandlers := NewAWSInfrastructureHandlers(db, awsInfraService)
				awsInfraHandlers.RegisterRoutes(r)
			}

			// One-click unsubscribe (RFC 8058)
			r.Post("/unsubscribe/one-click", suppSvc.HandleOneClickUnsubscribe)
			r.Get("/unsubscribe/list-header", suppSvc.HandleListUnsubscribeHeader)

			// NOTE: /webhooks/unsub-inbound is registered on s.router (public, line ~170)
			// so AWS SNS POSTs aren't blocked by the apiRouter auth middleware.

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

			// PMTA accounting summary builder — processes pmta_acct_raw into
			// pmta_acct_daily_summary. Throughput tunable via env.
			acctBatch := 10000
			if v := os.Getenv("ACCT_SUMMARY_BATCH_SIZE"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					acctBatch = n
				}
			}
			acctTick := 15 * time.Second
			if v := os.Getenv("ACCT_SUMMARY_TICK_SECONDS"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					acctTick = time.Duration(n) * time.Second
				}
			}
			acctSummary := pmta.NewAcctSummaryBuilderWithConfig(db, acctBatch, acctTick)
			acctSummary.Start()
			log.Printf("[AcctSummary] configured batch=%d tick=%s", acctBatch, acctTick)

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

			suppressionDir := os.Getenv("PMTA_SUPPRESSION_DIR")
			if suppressionDir == "" {
				suppressionDir = os.TempDir() + "/pmta-suppressions"
			}
			suppressionRepo := &engine.DBSuppressionRepo{DB: db}
			suppressionStore := engine.NewSuppressionStore(suppressionRepo, engineOrgID, suppressionDir)
			suppLoadCtx, suppLoadCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = suppressionStore.LoadFromDB(suppLoadCtx)
			suppLoadCancel()

			var engineMemory *engine.MemoryStore
			if s.s3Client != nil {
				engineBucket := os.Getenv("ENGINE_S3_BUCKET")
				if engineBucket == "" {
					engineBucket = "ignite-pmta-engine"
				}
				engineMemory = engine.NewMemoryStore(s.s3Client, engineBucket)
			}

			// ENGINE_DECISION_PERSIST_DISABLED decommissions the DB-side of the
			// convictions/decisions engine: it stops INSERTs into the two large
			// append-only logs (mailing_engine_convictions ~37 GB,
			// mailing_engine_decisions ~19 GB) that were the dominant RDS write-IO
			// source and were never read by the send path (only by display/analytics
			// surfaces). In-memory agents, S3 archival of convictions, the throttle
			// rate registry, and mailing_engine_throttle_agent_state are all
			// unaffected. Fully reversible: unset the env var and redeploy.
			enginePersistDisabled := strings.EqualFold(os.Getenv("ENGINE_DECISION_PERSIST_DISABLED"), "true")

			convictionStore := engine.NewConvictionStore(engineMemory)
			convictionStore.SetDB(db)
			if enginePersistDisabled {
				convictionStore.DisableDBPersist()
				log.Printf("[engine] DB persistence DISABLED for convictions/decisions (ENGINE_DECISION_PERSIST_DISABLED=true) — S3 archival continues, RDS write-IO relieved")
			}
			// Restore convictions from S3 + DB in the background. This work
			// can take 10–20 minutes under DB load (60 ISP × agent-type
			// combinations × ~10–60s per query). Blocking SetMailingDB on
			// it means /api/mailing/* routes stay 404 for the entire window
			// — which makes every deploy a Sev-2 incident for daily ops.
			//
			// Convictions are an OPTIMIZATION: they short-circuit redundant
			// reputation/throttle decisions. Engine agents tolerate an
			// empty conviction ring on startup — they refill as decisions
			// happen. So async-loading is correct.
			//
			// 2026-05-30: bundled with the SES-relay routing fix as the
			// "make boot survivable" change after the SetMailingDB hang
			// during the sending_profile_id deploy.
			go func() {
				start := time.Now()
				log.Printf("[conviction] async LoadAll starting (S3 + DB restore for 10 ISPs × 6 agent types)")
				convictionStore.LoadAll(context.Background())
				log.Printf("[conviction] async LoadAll completed in %s", time.Since(start).Round(time.Second))
			}()

			agentFactory := engine.NewAgentFactory(db, engineOrgID, engineMemory, suppressionStore, convictionStore)
			_ = agentFactory.Initialize(context.Background())

			rateRegistry := engine.NewISPRateRegistry()
			configRates := make(map[engine.ISP]float64)
			for isp, cfg := range agentFactory.GetConfigs() {
				rateRegistry.SetRate(isp, float64(cfg.MaxMsgRate))
				configRates[isp] = float64(cfg.MaxMsgRate)
			}
			rateRegistry.SetDB(db)
			if s.shutdownCtx != nil {
				rateRegistry.SetShutdownContext(s.shutdownCtx)
			}
			restored := rateRegistry.RestoreFromDB(configRates)
			restoredIP := rateRegistry.RestoreIPRatesFromDB()
			agentFactory.SetRateRegistry(rateRegistry)
			if s.shutdownCtx != nil {
				agentFactory.SetShutdownContext(s.shutdownCtx)
			}
			throttleRestored := agentFactory.RestoreThrottleState()
			s.rateRegistry = rateRegistry
			s.ispConfigs = agentFactory.GetConfigs()
			s.convictionStore = convictionStore
			s.agentFactory = agentFactory
			log.Printf("[engine] ISPRateRegistry initialized with %d ISP rates (%d restored from DB, %d per-IP rates, %d throttle states restored)",
				len(agentFactory.GetConfigs()), restored, restoredIP, throttleRestored)

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
			executor.SetDB(db)

			decisionStore := &engine.DBDecisionStore{DB: db, PersistDisabled: enginePersistDisabled}
			orchestrator := engine.NewOrchestrator(
				decisionStore, engineOrgID, agentFactory, signalProcessor,
				ingestor, executor, alerter, engineMemory, suppressionStore,
			)

			ruleStore := engine.NewRuleStore(db, engineOrgID)

			engineAPI := NewEngineService(db, orchestrator, suppressionStore, convictionStore, signalProcessor, ruleStore, engineOrgID)
			engineAPI.RegisterRoutes(r)

			r.Get("/engine/throttle-analytics", (&throttleAnalyticsHandler{
				registry:        s.rateRegistry,
				configs:         s.ispConfigs,
				db:              db,
				convictionStore: convictionStore,
				factory:         s.agentFactory,
				orgID:           engineOrgID,
			}).ServeHTTP)

			r.Get("/engine/audience-analytics", (&throttleAnalyticsHandler{
				registry:        s.rateRegistry,
				configs:         s.ispConfigs,
				db:              db,
				convictionStore: convictionStore,
				factory:         s.agentFactory,
				orgID:           engineOrgID,
			}).handleAudienceAnalytics)

			// === ENGINE-TO-UI BRIDGE (managed agent ↔ engine data) ===
			bridge := &ISPAgentEngineBridge{
				db:              db,
				rateRegistry:    s.rateRegistry,
				ispConfigs:      s.ispConfigs,
				convictionStore: convictionStore,
				agentFactory:    s.agentFactory,
				signalProcessor: signalProcessor,
			}
			r.Get("/isp-agents/managed/{id}/engine", bridge.HandleAgentEngine)
			r.Get("/isp-agents/engine/bridge-summary", bridge.HandleEngineSummary)

			// === DELIVERABILITY CONTROL (ISP config CRUD + hot-reload) ===
			delivH := &deliverabilityHandler{
				db:           db,
				rateRegistry: rateRegistry,
				agentFactory: agentFactory,
				ispConfigs:   s.ispConfigs,
			}
			r.Get("/deliverability/config", delivH.HandleGetConfig)
			r.Patch("/deliverability/config/{isp}", delivH.HandlePatchConfig)
			r.Post("/deliverability/config/{isp}/reset-throttle", delivH.HandleResetThrottle)
			r.Get("/deliverability/throughput", delivH.HandleGetThroughput)

			// === ISP HEALTH CENTER (v2.0) — read-only views over pmta_acct_raw ===
			r.Get("/deliverability/timeseries", delivH.HandleGetTimeSeries)
			r.Get("/deliverability/matrix", delivH.HandleGetMatrix)
			r.Get("/deliverability/deferrals", delivH.HandleGetDeferrals)
			r.Get("/deliverability/bounces", delivH.HandleGetBounces)
			r.Get("/deliverability/fbl", delivH.HandleGetFBL)
			r.Get("/deliverability/ip-activity", delivH.HandleGetIPActivity)

			// === PMTA CAMPAIGN WIZARD (ISP-native campaign creation) ===
			pmtaCampaignAPI := NewPMTACampaignService(db, orchestrator, convictionStore, signalProcessor, engineOrgID)
			if s.OfferSuppMgr != nil {
				pmtaCampaignAPI.SetOfferSuppressionManager(s.OfferSuppMgr)
			}
			pmtaCampaignAPI.RegisterRoutes(r)
			// Expose the campaign service so the data partner drip orchestrator
			// can invoke HandleDeployCampaign in-process at wave time.
			s.SetPMTACampaignService(pmtaCampaignAPI)
			// Start partner drip as soon as deploy hook exists — do not wait for
			// the rest of route registration (consciousness, pipeline, etc.).
			s.startPartnerDripOrchestrator(db, pmtaCampaignAPI)

			// === SEND-DAY PLANNER (canvas) — Phase 1 endpoints ===
			// Each endpoint backs one of the six pre-deploy gates from
			// .cursor/rules/send-day-process.mdc or the canvas's
			// per-cell creative drawer. Implementations live in
			// send_day_handlers.go + send_day_creative_resolve.go.
			r.Get("/send-day/volume-reconciliation", advSvc.HandleSendDayVolumeReconciliation)
			r.Get("/send-day/banned-creatives", advSvc.HandleSendDayBannedCreatives)
			r.Post("/send-day/preflight-batch", pmtaCampaignAPI.HandleSendDayPreflightBatch)
			r.Post("/send-day/creative-resolve", advSvc.HandleSendDayCreativeResolve)
			r.Get("/send-day/host-health", advSvc.HandleSendDayHostHealth)
			r.Post("/send-day/host-health/attest", advSvc.HandleSendDayHostHealthAttest)

			// === DOMAIN AGENT (per-domain scorecard → briefing → plan → deploy) ===
			// Scorecard rollups are maintained by domainagent.ScorecardWorker
			// (wired in cmd/server/main.go); plan approval deploys each compiled
			// payload in-process via pmtaCampaignAPI.DeployFromInput. Routes live
			// under /api/mailing/domain-agent/*; see domain_agent_handlers.go.
			domainAgentAPI := NewDomainAgentAPI(db, pmtaCampaignAPI)
			domainAgentAPI.RegisterRoutes(r)

			// === SEND BASELINES & SCORECARDS (volume governance per domain × ISP) ===
			// GET /api/mailing/send-baselines, /send-baselines/today, /send-scorecards.
			// Baselines read mailing_domain_agent_scorecard; see send_baselines.go.
			sendBaselinesAPI := NewSendBaselinesAPI(db)
			sendBaselinesAPI.RegisterRoutes(r)

			// Sending-engine live status (queue depth, wave manager, throughput,
			// deferral storms, worker heartbeats). Cached snapshot — safe to poll
			// every 10s. Authed: the payload carries campaign names + worker
			// errors (QA finding H2).
			r.Get("/outbox/engine-status", HandleOutboxEngineStatus(db))
			r.Get("/outbox/isp-pipes", HandleOutboxISPPipes(db))

			// === AUDIENCE CADENCE v3 — messages-to-engage/convert KPIs per ISP +
			// the ISP doctrine registry (audience_cadence_kpis.go).
			cadenceKPISvc := NewAudienceCadenceKPIService(db)
			r.Get("/audience-cadence/kpis", cadenceKPISvc.HandleAudienceCadenceKPIs)
			r.Get("/audience-cadence/doctrines", cadenceKPISvc.HandleListISPDoctrines)
			r.Put("/audience-cadence/doctrines/{isp}", cadenceKPISvc.HandleUpsertISPDoctrine)

			// === DOMAIN CENTER: DNS / REPUTATION HEALTH (live SPF/DKIM/DMARC/NS/Spamhaus) ===
			r.Get("/domain-center/dns-health", NewDomainDNSHealthHandler(db).Handle)

			// === AUDIENCE ARCHITECTURE: Background workers ===
			workerCtx := context.Background()
			segMaterializer := NewSegmentMaterializer(db, "04:00")
			segMaterializer.Start(workerCtx)
			// Populate the canonical master-list segments (Master List,
			// Engaged Openers, Engaged Clickers) right away so the UI
			// shows non-zero counts on first boot after the phase21 seed
			// migrations run, instead of waiting up to 24h for the
			// nightly cycle. Runs in its own goroutine — large subscriber
			// tables can take minutes to scan and we don't want to block
			// HTTP server startup.
			go segMaterializer.MaterializeCanonicalSegments(workerCtx)
			// Hydrate any engagement_brand/engagement_global segment that
			// currently has zero rows in mailing_segment_members. Targets
			// the 10 stragglers from the May 29 direct-API build (5 Tier 1
			// reactivated Clickers + 4 globals + HWS 30D Openers) plus any
			// new engagement segment a future operator creates where the
			// inline POST goroutine times out before MaterializeSegment
			// completes. Idempotent — segments with members are skipped.
			go segMaterializer.MaterializeEngagementCatalog(workerCtx)
			pmtaCampaignAPI.StartAudienceWorker(workerCtx)

			// SDS Graduation Job (per-domain engagement engine, SA-1).
			// Nightly cleanup at 02:00 UTC: cold pass on
			// mailing_subscriber_domain_state + cross-engaged graduation
			// on mailing_subscribers. See
			// internal/worker/sds_graduation_job.go for the design notes
			// and the explicit omission of any cold-decay re-probe pass.
			sdsGraduationJob := worker.NewSDSGraduationJob(db)
			if err := sdsGraduationJob.Start(); err != nil {
				log.Printf("SDS Graduation Job failed to start: %v", err)
			} else {
				log.Println("SDS Graduation Job started (daily cold + cross-engaged passes at 02:00 UTC)")
			}

			// === BLOG CAMPAIGN INGEST (minimal JSON → full engaged campaign) ===
			r.Post("/blog-campaign", pmtaCampaignAPI.HandleBlogCampaign)

			// === CLICK-DRIP ADMIN CRUD (operator UI for Phase 4) ===
			// Reminder subjects + offer→journey map. Lives under the
			// authenticated /api/mailing prefix so the operator dashboard
			// can edit them; the click-postback handler reads the journey
			// map on every postback (no cache) so enabled=false here halts
			// new enrollments immediately. See click_drip_admin_handlers.go.
			RegisterClickDripAdminRoutes(r, db)

			// === CAMPAIGN COPILOT — AI Campaign Management Chatbot ===
			campaignCopilot := NewCampaignCopilot(db, s.openAIConfig, pmtaCampaignAPI, segmentationAPI)
			r.Post("/copilot/chat", campaignCopilot.HandleChat)

			// === DOMAIN AGENT CHAT — conversational scheduling copilot ===
			// Full-power chat over the Domain Agent lifecycle (scorecard →
			// briefing → plan → approve-to-deploy) + the Copilot toolset
			// (clone/deploy/stop). See domain_agent_chat.go.
			domainAgentChat := NewDomainAgentChat(db, s.openAIConfig, domainAgentAPI, campaignCopilot)
			domainAgentChat.RegisterRoutes(r)

			// === CPM PLANNER — deal pricing, pacing & capacity (cpm_planner_handlers.go) ===
			cpmPlanner := NewCpmPlannerHandlers(db)
			r.Route("/cpm-planner", func(cp chi.Router) {
				cp.Get("/deals", cpmPlanner.HandleListDeals)
				cp.Post("/deals", cpmPlanner.HandleCreateDeal)
				cp.Put("/deals/{id}", cpmPlanner.HandleUpdateDeal)
				cp.Delete("/deals/{id}", cpmPlanner.HandleDeleteDeal)
				cp.Get("/deals/{id}/insights", cpmPlanner.HandleDealInsights)
				cp.Get("/deals/{id}/offer-performance", cpmPlanner.HandleDealOfferPerformance)
				// Manual conversions — operator-uploaded conversion ground truth
				// (Everflow CSV exports / quick-adds) blended into deal pacing.
				cp.Post("/deals/{id}/conversions", cpmPlanner.HandleAddDealConversions)
				cp.Get("/deals/{id}/conversions", cpmPlanner.HandleListDealConversions)
				cp.Delete("/deals/{id}/conversions/{convID}", cpmPlanner.HandleDeleteDealConversion)
				cp.Get("/capacity", cpmPlanner.HandleCapacity)
				cp.Get("/offers-lite", cpmPlanner.HandleOffersLite)
			})

			// === EMAIL MARKETING AGENT — Standalone AI strategist ===
			ensureAgentTables(db)
			marketingAgent := NewEmailMarketingAgent(db, s.openAIConfig, pmtaCampaignAPI, segmentationAPI)
			r.Route("/agent", func(ar chi.Router) {
				ar.Post("/chat", marketingAgent.HandleChat)
				ar.Get("/conversations", marketingAgent.HandleListConversations)
				ar.Get("/conversations/{id}", marketingAgent.HandleGetConversation)
				ar.Delete("/conversations/{id}", marketingAgent.HandleDeleteConversation)
				ar.Get("/memory", marketingAgent.HandleListMemory)
				ar.Delete("/memory/{id}", marketingAgent.HandleDeleteMemory)
				ar.Get("/strategies", marketingAgent.HandleListStrategies)
				ar.Post("/strategies", marketingAgent.HandleSaveStrategy)
				ar.Put("/strategies/{id}", marketingAgent.HandleUpdateStrategy)
				ar.Delete("/strategies/{id}", marketingAgent.HandleDeleteStrategy)
				ar.Get("/calendar/forecast", marketingAgent.HandleGetForecast)
				ar.Get("/calendar/recommendations", marketingAgent.HandleListRecommendations)
				ar.Post("/calendar/recommendations", marketingAgent.HandleCreateRecommendation)
				ar.Get("/calendar/recommendations/{id}", marketingAgent.HandleGetRecommendation)
				ar.Patch("/calendar/recommendations/{id}", marketingAgent.HandleUpdateRecommendation)
				ar.Post("/calendar/recommendations/{id}/approve", marketingAgent.HandleApproveRecommendation)
				ar.Post("/calendar/recommendations/{id}/unapprove", marketingAgent.HandleUnapproveRecommendation)
				ar.Post("/calendar/recommendations/{id}/reject", marketingAgent.HandleRejectRecommendation)
				ar.Delete("/calendar/recommendations/{id}", marketingAgent.HandleDeleteRecommendation)
				ar.Delete("/calendar/recommendations", marketingAgent.HandleBulkDeleteRecommendations)
				ar.Get("/calendar/compute-quotas", marketingAgent.HandleComputeQuotas)
				ar.Post("/calendar/generate", marketingAgent.HandleGenerateForecast)
				ar.Post("/calendar/clear-forecasts", marketingAgent.HandleClearForecasts)
				ar.Post("/calendar/cancel-tomorrow", marketingAgent.HandleCancelTomorrowCampaigns)
				ar.Post("/calendar/clone-day", marketingAgent.HandleCloneDay)
				ar.Post("/calendar/campaigns/{campaignId}/generate-variants", marketingAgent.HandleGenerateVariants)
				ar.Get("/calendar/day/{date}/variants", marketingAgent.HandleGetDayVariants)
			})

			// === PMTA SEND-TIME RECOMMENDATIONS ===
			sendTimeHandler := NewPMTASendTimeHandler(db)
			sendTimeHandler.RegisterRoutes(r)

			// === GLOBAL SUPPRESSION HUB — Single Source of Truth ===
			globalHub := engine.NewGlobalSuppressionHub(db, engineOrgID, suppressionDir)
			loadCtx, loadCancel := context.WithTimeout(context.Background(), 20*time.Second)
			if err := globalHub.LoadFromDB(loadCtx); err != nil {
				log.Printf("[global-suppression] LoadFromDB error (will rely on real-time feed): %v", err)
			}
			loadCancel()
			globalHub.SetExecutor(executor, "/etc/pmta/suppressions")
			globalHub.StartFileSync(context.Background())

			// FBL webhook — public (receives ARF reports from ISPs)
			fblHandler := NewFBLHandler(db, globalHub)
			s.router.Post("/fbl/report", fblHandler.HandleARFReport)

			// SES events webhook — public (receives SNS-wrapped SES event-publishing
			// notifications: Bounce/Complaint -> globalHub.Suppress; Open/Click/Send/
			// Delivery/Reject/DeliveryDelay -> log only). Registered on s.router (not
			// inside the apiRouter auth-protected Route block) so SNS HTTPS POSTs
			// aren't 401'd. Mirrors the pattern used for unsub-inbound and FBL.
			sesEventsHandler := NewSESEventsHandler(db, globalHub, engineOrgID)
			s.router.Post("/api/mailing/webhooks/ses-events", sesEventsHandler.ServeHTTP)

			// Bridge: every agent-level suppression also feeds the global hub
			suppressionStore.SetGlobalSuppressionCallback(func(ctx context.Context, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign string) {
				globalHub.Suppress(ctx, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign)
			})

			// Bridge: ingestor feeds ALL negative PMTA signals to global hub
			ingestor.SetGlobalSuppressionHub(globalHub)

			// Wire global hub to SuppressionService (consolidates all global suppression)
			suppSvc.SetGlobalSuppressionHub(globalHub)

			// Wire global hub to all send pipelines
			if s.mailingSvc != nil {
				s.mailingSvc.SetGlobalSuppressionHub(globalHub)
			}
			campaignBuilder.SetGlobalSuppressionHub(globalHub)
			pmtaCampaignAPI.SetGlobalSuppressionHub(globalHub)
			pmtaCampaignAPI.SetExecutor(executor)

			// Wire global hub to bulk import service for in-memory cache reload after global imports
			suppImportSvc.svc.SetGlobalSuppressionReloader(globalHub)

			// Export for main.go to wire to the send worker pool
			s.GlobalHub = globalHub

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

					// Unsubscribes and hard bounces go to global suppression
					switch eventType {
					case "unsubscribe":
						globalHub.Suppress(context.Background(), recipient, "unsubscribe", "tracking_pixel", isp, "", "", "", campaignID)
					case "bounced":
						globalHub.Suppress(context.Background(), recipient, "hard_bounce", "sync_send", isp, "", "", "", campaignID)
					}
				})
			}

			// Wire campaign tracker to consciousness for campaign-aware thoughts
			consciousness.SetCampaignTracker(campaignTracker)

			consciousnessAPI := NewConsciousnessService(consciousness, campaignTracker, convictionStore, signalProcessor, engineOrgID)
			SetConsciousnessDB(db) // campaign-name enrichment + hourly assessment (handlers_consciousness.go)
			consciousnessAPI.RegisterRoutes(r)

			// Start the orchestrator (launches all 48 agents)
			orchestrator.Start(context.Background())

			// Start consciousness layer
			consciousness.Start(context.Background())

			// Injection Analytics registered above (public, no auth)

			// Data Pipeline dashboard endpoints — registered unconditionally;
			// handlers check for nil pipeline at request time. s.DataPipeline
			// is set in main.go after SetMailingDB returns.
			pipelineH := NewDataPipelineHandlers(db, nil)
			pipelineH.server = s
			r.Get("/pipeline/stats", pipelineH.HandleGetPipelineStats)
			r.Get("/pipeline/runs", pipelineH.HandleGetPipelineRuns)
			r.Get("/pipeline/health", pipelineH.HandleGetDomainHealth)
			r.Get("/pipeline/chart", pipelineH.HandleGetPipelineChart)
			r.Post("/pipeline/trigger", pipelineH.HandleTriggerPipeline)
			r.Post("/pipeline/validate-existing", pipelineH.HandleValidateExisting)
		})
	}
}
