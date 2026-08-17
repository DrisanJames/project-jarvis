package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/auth"


	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// apiAuthMiddleware enforces session-or-admin-key on the /api group AND stamps
// the trusted actor identity (Vector A plan rev4, Step 13).
//
// X-User-Email was a browser-spoofable header: every consumer
// (actorFromRequest, creatives_registry, fresh_broadcast_handlers,
// stream_broadcast_config) trusted the raw value, so a session-authenticated
// caller could forge any actor. Now:
//   - admin-key branch: trusted server-to-server — X-User-Email passes through
//     unchanged (callers stamp their own service identity).
//   - session branch: the inbound header is ALWAYS stripped, then re-set from
//     the server-side session's email — spoof-proof with zero per-handler
//     changes.
//
// Dev mode (middleware skipped in SetupRoutes) keeps today's behavior: the
// actor falls back to ip:/unknown.
func apiAuthMiddleware(authManager *auth.AuthManager, adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if adminKey != "" && req.Header.Get("X-Admin-Key") == adminKey {
				next.ServeHTTP(w, req)
				return
			}
			if !authManager.IsAuthenticated(req) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			// Session-authenticated: never trust the inbound header.
			req.Header.Del("X-User-Email")
			if s := authManager.GetSession(req); s != nil && s.Email != "" {
				req.Header.Set("X-User-Email", s.Email)
			}
			next.ServeHTTP(w, req)
		})
	}
}

// SetupRoutes configures all API routes.
// Returns the top-level mux AND the /api sub-router so that late-registered
// route groups (e.g. mailing) can be mounted inside /api and inherit its
// auth middleware.
func SetupRoutes(h *Handlers, authManager *auth.AuthManager) (*chi.Mux, chi.Router) {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)

	// Server identity header - distinguishes real server from stub API
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Server-Identity", "ignite-server-v1.0")
			w.Header().Set("X-Server-Binary", "cmd/server")
			next.ServeHTTP(w, req)
		})
	})

	// CORS - allow credentials for auth cookies (H8: explicit origins)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{
			"https://projectjarvis.io",
			"http://localhost:5173", "http://localhost:8080",
			"https://discountblog.com", "https://www.discountblog.com",
			"https://quizfiesta.com", "https://www.quizfiesta.com",
			"https://historythinking.com", "https://www.historythinking.com",
			"https://myownhealth.net", "https://www.myownhealth.net",
			"https://myownhealth.com", "https://www.myownhealth.com",
		},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Admin-Key", "X-Organization-ID"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Health check (no auth required)
	r.Get("/health", h.HealthCheck)

	// SES event webhook — registered HERE, at construction, not inside the
	// async SetMailingDB block.
	//
	// It used to be registered only once SetMailingDB ran, which happens
	// asynchronously after boot. Until then a POST from SNS fell through to the
	// auth-protected router and got 401, so EVERY task start burned SNS
	// delivery attempts. Measured 2026-08-12 during an unplanned task
	// replacement: 953 NumberOfNotificationsFailed in a single 5-minute bucket,
	// all inside the registration window, while the endpoint served 100% 200s
	// before and after.
	//
	// Nothing was lost then — SNS retried and the DLQ stayed empty — but it
	// spends the subscription's 3-attempt budget on a self-inflicted outage
	// during exactly the window a task is least healthy. Registering early and
	// answering 503 makes the "not ready yet" honest and retryable instead of
	// looking like an auth failure.
	r.Post("/api/mailing/webhooks/ses-events", serveSESEventsWhenReady)

	// Auth routes (no auth required)
	if authManager != nil {
		r.Get("/auth/login", authManager.HandleLogin)
		r.Get("/auth/callback", authManager.HandleCallback)
		r.Get("/auth/logout", authManager.HandleLogout)
		r.Get("/auth/user", authManager.HandleUserInfo)
		// Off-by-default UI-test access (404 unless TEST_ACCESS_TOKEN is set on
		// the task); mints a single test@<domain> session. See HandleTestLogin.
		r.Get("/auth/test-login", authManager.HandleTestLogin)
		r.Post("/auth/test-login", authManager.HandleTestLogin)
	} else {
		r.Get("/auth/user", func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusOK, map[string]interface{}{
				"authenticated": true,
				"user": map[string]interface{}{
					"id": "admin", "email": "admin@jamesventurescorp.com",
					"name": "Admin", "domain": "jamesventurescorp.com",
				},
				"organization": map[string]interface{}{
					"id": "00000000-0000-0000-0000-000000000001",
					"name": "Jarvis", "domain": "jamesventurescorp.com",
				},
			})
		})
	}

	// API routes (protected by auth middleware)
	var apiRouter chi.Router
	devMode := os.Getenv("DEV_MODE") == "true" || os.Getenv("ENVIRONMENT") == "development"

	r.Route("/api", func(r chi.Router) {
		apiRouter = r // capture so late-registered groups can use it
		// Apply auth middleware to all API routes (skip in dev mode)
		adminKey := os.Getenv("ADMIN_API_KEY")
		if authManager != nil && !devMode {
			r.Use(apiAuthMiddleware(authManager, adminKey))
		}
		// Dashboard - all data in one call
		r.Get("/dashboard", h.GetDashboard)
		r.Get("/dashboard/combined", h.GetCombinedDashboard)
		
		// Unified ISP metrics from all providers
		r.Get("/isp/unified", h.GetUnifiedISPMetrics)
		
		// Unified IP metrics from all providers
		r.Get("/ip/unified", h.GetUnifiedIPMetrics)
		
		// Unified domain metrics from all providers
		r.Get("/domain/unified", h.GetUnifiedDomainMetrics)

		// Data Partner Analytics routes registered outside auth group (see below)

		// Data Activation Intelligence
		r.Get("/activation/intelligence", h.GetDataActivationIntelligence)

		// Mailing Platform routes — registered via SetMailingDB() in server_routes_mailing.go
		// Tracking domain routes registered under /api/mailing/tracking-domains via same

		// Agent routes
		r.Route("/agent", func(r chi.Router) {
			// Alerts
			r.Get("/alerts", h.GetAlerts)
			r.Post("/alerts/acknowledge", h.AcknowledgeAlert)
			r.Post("/alerts/clear", h.ClearAlerts)
			
			// Insights
			r.Get("/insights", h.GetInsights)
			
			// Learned data
			r.Get("/baselines", h.GetBaselines)
			r.Get("/correlations", h.GetCorrelations)
			
			// Chat
			r.Post("/chat", h.Chat)
			
			// Agentic Self-Learning System
			r.Get("/agentic/status", h.HandleAgenticStatus)
			r.Get("/agentic/actions", h.HandleAgenticActions)
		})

		// System routes
		r.Route("/system", func(r chi.Router) {
			r.Get("/status", h.GetSystemStatus)
			r.Get("/cache", h.GetCacheStats)
			r.Post("/fetch", h.TriggerFetch)
		})
	})

	// Data Partner Analytics — public (no auth required)
	r.Get("/api/data-partners/analytics", h.GetDataPartnerAnalytics)
	r.Post("/api/data-partners/refresh", h.RefreshDataPartnerCache)

	// Unmatched routes: while /api/mailing/* registration is still running in
	// its background goroutine, answer /api/* with a retryable 503 instead of
	// a misleading 404 (see mailing_routes_readiness.go — this silently ate 25
	// board-deploy POSTs on 2026-08-06).
	r.NotFound(handleUnmatchedRoute)

	// Serve static files for React frontend (SPA with fallback to index.html)
	spaHandler(r, "./web/dist")

	return r, apiRouter
}

// spaHandler serves static files and falls back to index.html for SPA routing
func spaHandler(r chi.Router, staticPath string) {
	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		// Get the path
		path := req.URL.Path
		
		// Skip API routes. The SPA catch-all is registered at router-build
		// time, so it shadows /api GETs whose real handler has not been
		// registered yet — route them through the same startup-aware path.
		if strings.HasPrefix(path, "/api") || strings.HasPrefix(path, "/health") {
			handleUnmatchedRoute(w, req)
			return
		}
		
		// Try to serve the file directly
		filePath := filepath.Join(staticPath, path)
		
		// Check if file exists
		if _, err := os.Stat(filePath); err == nil {
			http.ServeFile(w, req, filePath)
			return
		}

		// Static asset requests (JS/CSS/images with hashed filenames) that don't exist
		// should 404 instead of falling back to index.html, which would cause MIME mismatch.
		if strings.HasPrefix(path, "/assets/") {
			http.NotFound(w, req)
			return
		}
		
		// For SPA routing, serve index.html for unknown paths
		indexPath := filepath.Join(staticPath, "index.html")
		http.ServeFile(w, req, indexPath)
	})
}
