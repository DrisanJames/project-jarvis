package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/agent"
	"github.com/ignite/sparkpost-monitor/internal/auth"
	"github.com/ignite/sparkpost-monitor/internal/buildinfo"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/ignite/sparkpost-monitor/internal/storage"
	"github.com/redis/go-redis/v9"
)

// Server represents the API server
type Server struct {
	config       config.ServerConfig
	openAIConfig config.OpenAIConfig
	handler      http.Handler
	handlers     *Handlers
	server       *http.Server
	authManager  *auth.AuthManager
	router       *chi.Mux
	apiRouter    chi.Router // sub-router for /api (carries auth middleware)
	mailingDB    *sql.DB
	startedAt    time.Time
	// Image CDN configuration
	s3Client     *s3.Client
	imageBucket  string
	cdnDomain    string
	awsRegion    string
	// Redis client for rate limiting and throttling
	redisClient    *redis.Client
	// Mailing service reference for wiring callbacks
	mailingSvc     *MailingService
	// Global suppression hub — exported so main.go can wire it to the send worker pool
	GlobalHub      interface{ IsSuppressed(email string) bool }
	// ISP rate registry — shared between engine agents and send worker pool
	rateRegistry    *engine.ISPRateRegistry
	ispConfigs      map[engine.ISP]engine.ISPConfig
	convictionStore *engine.ConvictionStore
	agentFactory    *engine.AgentFactory
	// S3 data normalizer for operational API
	dataNormHandler *DataNormHandler
	// Server lifecycle context — propagated to engine persist goroutines
	shutdownCtx context.Context
	// Optizmo Delta Sync Worker — stored for graceful shutdown
	deltaSyncWorker *OptizmoDeltaSyncWorker
	// S3 client for offer suppression files (separate from image CDN)
	suppressionS3 *SuppressionS3Client
	// Offer suppression Bloom filter manager — O(1) send-time checks
	OfferSuppMgr *OfferSuppressionManager
	// S3 client for partner data partner ingestion (separate bucket)
	partnerIngestS3 *PartnerIngestS3Client
	// PMTA campaign service — exposed so the drip orchestrator can call
	// HandleDeployCampaign in-process.
	pmtaCampaignSvc *PMTACampaignService
	// Partner ingestion workers (slicer/validator/drip orchestrator). Stored
	// for graceful shutdown plus admin endpoints that need to surface their
	// status. Type is interface{} to avoid a hard dep on internal/worker.
	partnerSlicer            interface{ Stop() }
	partnerValidator         interface{ Stop() }
	partnerDripOrchestrator  interface{ Stop() }
	// partnerDripStarter is invoked from SetMailingDB immediately after
	// SetPMTACampaignService. Route registration can take >10 minutes; polling
	// from main.go timed out before PMTA was wired (May 30 incident).
	partnerDripStarter   func(db *sql.DB, pmta *PMTACampaignService) interface{ Stop() }
	partnerDripStartOnce sync.Once
	// Data pipeline reference — exported so main.go can wire it
	DataPipeline *worker.DataPipeline
	// PMTA accounting — public POST /engine/webhook; set when ingestor + workers are ready
	pmtaAccountingWebhook http.HandlerFunc
	// Wave processor throughput provider (SA-7). Set after sendWorkerPool
	// is constructed in main.go via SetWaveProcessorThroughputProvider.
	// Read by the /api/wave-processor/status handler registered in
	// SetMailingDB. Mutex-protected so the late wiring is race-free.
	waveProcessorMu       sync.RWMutex
	waveProcessorProvider ThroughputProvider
	// Storage guard snapshot provider for GET /health/storage
	storageGuard *worker.StorageGuard
}

// NewServer creates a new API server
func NewServer(
	cfg config.ServerConfig,
	client *sparkpost.Client,
	store *storage.Storage,
	agent *agent.Agent,
	collector *sparkpost.Collector,
) *Server {
	handlers := NewHandlers(collector, agent, store)
	router, apiRouter := SetupRoutes(handlers, nil) // No auth initially

	return &Server{
		config:    cfg,
		handler:   router,
		handlers:  handlers,
		router:    router,
		apiRouter: apiRouter,
		startedAt: time.Now(),
	}
}

// NewServerWithAuth creates a new API server with authentication
func NewServerWithAuth(
	cfg config.ServerConfig,
	client *sparkpost.Client,
	store *storage.Storage,
	agent *agent.Agent,
	collector *sparkpost.Collector,
	authManager *auth.AuthManager,
) *Server {
	handlers := NewHandlers(collector, agent, store)
	router, apiRouter := SetupRoutes(handlers, authManager)

	return &Server{
		config:      cfg,
		handler:     router,
		handlers:    handlers,
		authManager: authManager,
		router:      router,
		apiRouter:   apiRouter,
		startedAt:   time.Now(),
	}
}

// ListenAndServe starts the HTTP server
func (s *Server) ListenAndServe(addr string) error {
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.handler,
		// Timeouts are generous to support large file uploads (multi-GB suppression files).
		// Individual endpoints use http.TimeoutHandler or context deadlines for tighter control.
		ReadTimeout:       5 * time.Minute,
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	return s.server.ListenAndServe()
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// Handler returns the HTTP handler for testing
func (s *Server) Handler() http.Handler {
	return s.handler
}

// GetRateRegistry returns the ISP rate registry created during engine startup.
// Returns nil if the engine has not been initialized yet.
func (s *Server) GetRateRegistry() *engine.ISPRateRegistry {
	return s.rateRegistry
}

// SetShutdownContext stores the server lifecycle context. Must be called
// before SetMailingDB so the engine can propagate it to persist goroutines.
func (s *Server) SetShutdownContext(ctx context.Context) {
	s.shutdownCtx = ctx
}

// StopDeltaSyncWorker gracefully stops the Optizmo nightly delta sync worker.
func (s *Server) StopDeltaSyncWorker() {
	if s.deltaSyncWorker != nil {
		s.deltaSyncWorker.Stop()
	}
}

// HandleVersion returns build info and deployment timestamp for the UI sidebar.
func (s *Server) HandleVersion(w http.ResponseWriter, r *http.Request) {
	bi := buildinfo.Current()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"version":     bi.Version,
		"git_sha":     bi.GitSHA,
		"build_time":  bi.BuildTime,
		"go_version":  bi.GoVersion,
		"deployed_at": s.startedAt.UTC().Format(time.RFC3339),
	})
}
