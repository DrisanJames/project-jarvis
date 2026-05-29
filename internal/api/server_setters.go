package api

import (
	"database/sql"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/agent"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/datainjections"
	"github.com/ignite/sparkpost-monitor/internal/datanorm"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
	"github.com/ignite/sparkpost-monitor/internal/financial"
	"github.com/ignite/sparkpost-monitor/internal/intelligence"
	"github.com/ignite/sparkpost-monitor/internal/kanban"
	"github.com/ignite/sparkpost-monitor/internal/mailgun"
	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/redis/go-redis/v9"
)

// SetMailgunCollector sets the Mailgun collector on the server
func (s *Server) SetMailgunCollector(collector *mailgun.Collector) {
	if s.handlers != nil {
		s.handlers.SetMailgunCollector(collector)
	}
}

// SetSESCollector sets the SES collector on the server
func (s *Server) SetSESCollector(collector *ses.Collector) {
	if s.handlers != nil {
		s.handlers.SetSESCollector(collector)
	}
}

// SetOngageCollector sets the Ongage collector on the server
func (s *Server) SetOngageCollector(collector *ongage.Collector) {
	if s.handlers != nil {
		s.handlers.SetOngageCollector(collector)
	}
}

// GetOngageCollector returns the Ongage collector from the server
func (s *Server) GetOngageCollector() *ongage.Collector {
	if s.handlers != nil {
		return s.handlers.GetOngageCollector()
	}
	return nil
}

// SetEverflowCollector sets the Everflow collector on the server
func (s *Server) SetEverflowCollector(collector *everflow.Collector) {
	if s.handlers != nil {
		s.handlers.SetEverflowCollector(collector)
	}
}

// SetNetworkIntelligenceCollector sets the network intelligence collector on the server
func (s *Server) SetNetworkIntelligenceCollector(collector *everflow.NetworkIntelligenceCollector) {
	if s.handlers != nil {
		s.handlers.SetNetworkIntelligenceCollector(collector)
	}
}

// SetEnrichmentService sets the Everflow enrichment service on the server
func (s *Server) SetEnrichmentService(service *everflow.EnrichmentService) {
	if s.handlers != nil {
		s.handlers.SetEnrichmentService(service)
	}
}

// SetOpenAIAgent sets the OpenAI-powered conversational agent
func (s *Server) SetOpenAIAgent(openaiAgent *agent.OpenAIAgent) {
	if s.handlers != nil {
		s.handlers.SetOpenAIAgent(openaiAgent)
	}
}

// SetOpenAIConfig stores the OpenAI configuration for AI services
func (s *Server) SetOpenAIConfig(cfg config.OpenAIConfig) {
	s.openAIConfig = cfg
}

// SetAgenticLoop sets the self-learning agentic loop
func (s *Server) SetAgenticLoop(loop *agent.AgenticLoop) {
	if s.handlers != nil {
		s.handlers.SetAgenticLoop(loop)
	}
}

// GetMailingDB returns the mailing database
func (s *Server) GetMailingDB() *sql.DB {
	return s.mailingDB
}

// SetImageCDNConfig sets the S3 and CDN configuration for image hosting.
// Routes are registered in SetMailingDB; this just stores the S3/CDN config
// so that routes registered there have the S3 client available.
func (s *Server) SetImageCDNConfig(s3Client *s3.Client, bucket, cdnDomain, region string) {
	s.s3Client = s3Client
	s.imageBucket = bucket
	s.cdnDomain = cdnDomain
	s.awsRegion = region
}

// SetRedisClient sets the Redis client for rate limiting and throttling
func (s *Server) SetRedisClient(client *redis.Client) {
	s.redisClient = client

	// Throttle routes are registered inside the /api/mailing group by SetMailingDB.
	// Do NOT mount a separate /api/mailing here — it would shadow the apiRouter routes.
}

// GetRedisClient returns the Redis client
func (s *Server) GetRedisClient() *redis.Client {
	return s.redisClient
}

// SetDataInjectionsService sets the data injections monitoring service
func (s *Server) SetDataInjectionsService(service *datainjections.Service) {
	if s.handlers != nil {
		s.handlers.SetDataInjectionsService(service)
	}
}

// SetKanbanService sets the Kanban service
func (s *Server) SetKanbanService(service *kanban.Service) {
	if s.handlers != nil {
		s.handlers.SetKanbanService(service)
	}
}

// SetKanbanAIAnalyzer sets the Kanban AI analyzer
func (s *Server) SetKanbanAIAnalyzer(analyzer *kanban.AIAnalyzer) {
	if s.handlers != nil {
		s.handlers.SetKanbanAIAnalyzer(analyzer)
	}
}

// SetKanbanArchival sets the Kanban archival service
func (s *Server) SetKanbanArchival(archival *kanban.ArchivalService) {
	if s.handlers != nil {
		s.handlers.SetKanbanArchival(archival)
	}
}

// SetRevenueModelService sets the revenue model service for financial dashboard
func (s *Server) SetRevenueModelService(service *financial.RevenueModelService) {
	if s.handlers != nil {
		s.handlers.SetRevenueModelService(service)
	}
}

// SetIntelligenceService sets the intelligence service on the server
func (s *Server) SetIntelligenceService(service *intelligence.Service) {
	if s.handlers != nil {
		s.handlers.SetIntelligenceService(service)
	}
}

// SetConfig sets the full application config for handlers that need it
func (s *Server) SetConfig(cfg *config.Config) {
	if s.handlers != nil {
		s.handlers.SetConfig(cfg)
	}
}

// SetNormalizer wires the S3 data normalizer and registers its API routes.
func (s *Server) SetNormalizer(normalizer *datanorm.Normalizer) {
	handler := NewDataNormHandler(normalizer, s.mailingDB)
	s.dataNormHandler = handler

	if s.apiRouter != nil {
		s.apiRouter.Route("/mailing/data-normalizer", func(r chi.Router) {
			r.Post("/trigger", handler.HandleTrigger)
			r.Get("/status", handler.HandleStatus)
			r.Get("/logs", handler.HandleLogs)
			r.Get("/quality-breakdown", handler.HandleQualityBreakdown)
			r.Post("/skip", handler.HandleSkip)
			r.Post("/retry", handler.HandleRetry)
		})
	}
}

// SetSuppressionS3Client configures the S3 client for offer suppression files
// and initializes the OfferSuppressionManager with it.
func (s *Server) SetSuppressionS3Client(s3Client *SuppressionS3Client) {
	s.suppressionS3 = s3Client
	s.OfferSuppMgr = NewOfferSuppressionManager(s.mailingDB, s3Client)
}

// GetOfferSuppressionManager returns the offer suppression manager.
func (s *Server) GetOfferSuppressionManager() *OfferSuppressionManager {
	return s.OfferSuppMgr
}

// SetPartnerIngestS3Client configures the S3 client for the data partner
// ingestion pipeline. Reads from PARTNER_INGEST_S3_BUCKET / REGION env vars
// (defaults: jarvis-partner-ingest / us-west-2).
func (s *Server) SetPartnerIngestS3Client(client *PartnerIngestS3Client) {
	s.partnerIngestS3 = client
}

// GetPartnerIngestS3Client returns the partner ingestion S3 client (may be
// nil during early boot or in environments where the bucket is not provisioned).
func (s *Server) GetPartnerIngestS3Client() *PartnerIngestS3Client {
	return s.partnerIngestS3
}

// SetPMTACampaignService stores the PMTA campaign service so the drip
// orchestrator can invoke HandleDeployCampaign in-process at wave time.
func (s *Server) SetPMTACampaignService(svc *PMTACampaignService) {
	s.pmtaCampaignSvc = svc
}

// GetPMTACampaignService returns the registered PMTA campaign service. May
// be nil if SetMailingDB has not yet completed.
func (s *Server) GetPMTACampaignService() *PMTACampaignService {
	return s.pmtaCampaignSvc
}

// SetPartnerSlicer / SetPartnerValidator / SetPartnerDripOrchestrator are
// thin setters used by main.go to wire the workers for graceful shutdown.
func (s *Server) SetPartnerSlicer(stopper interface{ Stop() })            { s.partnerSlicer = stopper }
func (s *Server) SetPartnerValidator(stopper interface{ Stop() })         { s.partnerValidator = stopper }
func (s *Server) SetPartnerDripOrchestrator(stopper interface{ Stop() })  { s.partnerDripOrchestrator = stopper }

// RegisterWaveProcessorStatusRoute swaps in the wave processor throughput
// provider so the GET /api/wave-processor/status handler (registered early
// in SetMailingDB) can serve real data. Safe to call at any time after
// SendWorkerPool is constructed in main.go. Mutex-protected so the late
// wiring is race-free against in-flight requests.
//
// Pure observability — see SA-7 in PER_DOMAIN_ENGAGEMENT_ENGINE_SPEC.md.
// Until this is called the handler returns empty per-domain throughput
// (DB-derived queue depth still works), so the route is never 404 / 500.
func (s *Server) RegisterWaveProcessorStatusRoute(provider ThroughputProvider) {
	s.waveProcessorMu.Lock()
	s.waveProcessorProvider = provider
	s.waveProcessorMu.Unlock()
}

// getWaveProcessorProvider returns the currently-wired throughput provider
// (may be nil during early boot before main.go has constructed the worker
// pool). Used by the /api/wave-processor/status handler closure.
func (s *Server) getWaveProcessorProvider() ThroughputProvider {
	s.waveProcessorMu.RLock()
	defer s.waveProcessorMu.RUnlock()
	return s.waveProcessorProvider
}

// SetStorageGuard wires the storage guard worker for /health/storage.
func (s *Server) SetStorageGuard(g *worker.StorageGuard) {
	s.storageGuard = g
}

// HandleStorage returns the latest storage guard snapshot.
func (s *Server) HandleStorage(w http.ResponseWriter, r *http.Request) {
	if s.storageGuard == nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status":  "unavailable",
			"message": "storage guard not initialized",
		})
		return
	}
	respondJSON(w, http.StatusOK, s.storageGuard.Snapshot())
}

// RegisterHealthRoutes creates a HealthChecker from the server's dependencies
// and registers comprehensive health routes on the router. Call this after all
// Set* methods (SetMailingDB, SetRedisClient, SetImageCDNConfig) have been invoked
// so the checker has access to every available dependency.
func (s *Server) RegisterHealthRoutes() {
	hc := NewHealthChecker(s.mailingDB, s.redisClient, s.s3Client, s.imageBucket)
	s.router.Get("/health", hc.HandleHealth)
	s.router.Get("/health/live", hc.HandleLiveness)
	s.router.Get("/health/ready", hc.HandleReadiness)
	s.router.Get("/health/detailed", hc.HandleDetailed)
	s.router.Get("/health/storage", s.HandleStorage)
	s.router.Get("/health/db-stats", hc.HandleDBStats)
	s.router.Get("/version", hc.HandleVersion)
}
