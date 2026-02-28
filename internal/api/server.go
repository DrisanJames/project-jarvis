package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/agent"
	"github.com/ignite/sparkpost-monitor/internal/auth"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
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
	// Image CDN configuration
	s3Client     *s3.Client
	imageBucket  string
	cdnDomain    string
	awsRegion    string
	// Redis client for rate limiting and throttling
	redisClient    *redis.Client
	// Mailing service reference for wiring callbacks
	mailingSvc     *MailingService
	// S3 data normalizer for operational API
	dataNormHandler *DataNormHandler
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
