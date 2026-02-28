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
		AllowedOrigins:   []string{"https://projectjarvis.io", "http://localhost:5173", "http://localhost:8080"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Health check (no auth required)
	r.Get("/health", h.HealthCheck)

	// Auth routes (no auth required)
	if authManager != nil {
		r.Get("/auth/login", authManager.HandleLogin)
		r.Get("/auth/callback", authManager.HandleCallback)
		r.Get("/auth/logout", authManager.HandleLogout)
		r.Get("/auth/user", authManager.HandleUserInfo)
	}

	// API routes (protected by auth middleware)
	var apiRouter chi.Router
	devMode := os.Getenv("DEV_MODE") == "true" || os.Getenv("ENVIRONMENT") == "development"

	r.Route("/api", func(r chi.Router) {
		apiRouter = r // capture so late-registered groups can use it
		// Apply auth middleware to all API routes (skip in dev mode)
		if authManager != nil && !devMode {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if !authManager.IsAuthenticated(req) {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusUnauthorized)
						w.Write([]byte(`{"error":"unauthorized"}`))
						return
					}
					next.ServeHTTP(w, req)
				})
			})
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

		// SparkPost metrics routes (default)
		r.Route("/metrics", func(r chi.Router) {
			r.Get("/summary", h.GetSummary)
			
			// ISP metrics
			r.Get("/isp", h.GetISPMetrics)
			r.Get("/isp/details", h.GetISPMetricsByProvider)
			
			// IP metrics
			r.Get("/ip", h.GetIPMetrics)
			
			// Domain metrics
			r.Get("/domain", h.GetDomainMetrics)
			
			// Signals (bounce, delay, rejection reasons)
			r.Get("/signals", h.GetSignals)
		})

		// Mailgun routes
		r.Route("/mailgun", func(r chi.Router) {
			r.Get("/dashboard", h.GetMailgunDashboard)
			r.Get("/summary", h.GetMailgunSummary)
			r.Get("/isp", h.GetMailgunISPMetrics)
			r.Get("/domains", h.GetMailgunDomainMetrics)
			r.Get("/signals", h.GetMailgunSignals)
			r.Post("/fetch", h.TriggerMailgunFetch)
		})

		// SES routes
		r.Route("/ses", func(r chi.Router) {
			r.Get("/dashboard", h.GetSESDashboard)
			r.Get("/summary", h.GetSESSummary)
			r.Get("/isp", h.GetSESISPMetrics)
			r.Get("/signals", h.GetSESSignals)
			r.Post("/fetch", h.TriggerSESFetch)
		})

		// Ongage routes
		r.Route("/ongage", func(r chi.Router) {
			r.Get("/dashboard", h.GetOngageDashboard)
			r.Get("/health", h.GetOngageHealth)
			r.Get("/campaigns", h.GetOngageCampaigns)
			r.Get("/subjects", h.GetOngageSubjectAnalysis)
			r.Get("/schedule", h.GetOngageScheduleAnalysis)
			r.Get("/esp-performance", h.GetOngageESPPerformance)
			r.Get("/audience", h.GetOngageAudienceAnalysis)
			r.Get("/pipeline", h.GetOngagePipelineMetrics)
		})

		// Everflow routes (revenue tracking)
		r.Route("/everflow", func(r chi.Router) {
			r.Get("/dashboard", h.GetEverflowDashboard)
			r.Get("/health", h.GetEverflowHealth)
			r.Get("/daily", h.GetEverflowDailyPerformance)
			r.Get("/weekly", h.GetEverflowWeeklyPerformance)
			r.Get("/monthly", h.GetEverflowMonthlyPerformance)
			r.Get("/offers", h.GetEverflowOfferPerformance)
			r.Get("/properties", h.GetEverflowPropertyPerformance)
			r.Get("/campaigns/{mailingId}", h.GetEverflowCampaignDetails)
			r.Get("/campaigns", h.GetEverflowCampaignRevenue)
			r.Get("/conversions", h.GetEverflowRecentConversions)
			r.Get("/clicks", h.GetEverflowRecentClicks)
			r.Get("/revenue-breakdown", h.GetEverflowRevenueBreakdown)
			r.Get("/esp-revenue", h.GetEverflowESPRevenue)
			r.Get("/esp-contracts", h.GetEverflowESPContracts)
			
			// Campaign offer selection with AI recommendations
			r.Get("/affiliates", h.GetAvailableAffiliates)
			r.Get("/campaign-offers", h.GetCampaignOffers)
			r.Get("/offer-details", h.GetOfferDetails)
			
			// Network-wide intelligence (no affiliate filter)
			r.Get("/network-top-offers", h.GetNetworkTopOffers)
		})

		// Data Partner Analytics routes registered outside auth group (see below)

		// Data Activation Intelligence
		r.Get("/activation/intelligence", h.GetDataActivationIntelligence)

		// Data Injections routes (partner data monitoring)
		r.Route("/data-injections", func(r chi.Router) {
			r.Get("/dashboard", h.DataInjectionsDashboard)
			r.Get("/health", h.DataInjectionsHealth)
			r.Get("/ingestion", h.DataInjectionsIngestion)
			r.Get("/validation", h.DataInjectionsValidation)
			r.Get("/imports", h.DataInjectionsImports)
			r.Post("/refresh", h.DataInjectionsRefresh)
		})

		// Kanban routes (AI-driven task management)
		r.Route("/kanban", func(r chi.Router) {
			r.Get("/board", h.GetKanbanBoard)
			r.Put("/board", h.UpdateKanbanBoard)
			r.Post("/cards", h.CreateKanbanCard)
			r.Put("/cards/{cardId}", h.UpdateKanbanCard)
			r.Put("/cards/{cardId}/move", h.MoveKanbanCard)
			r.Post("/cards/{cardId}/complete", h.CompleteKanbanCard)
			r.Delete("/cards/{cardId}", h.DeleteKanbanCard)
			r.Get("/due", h.GetKanbanDueTasks)
			r.Post("/ai/trigger", h.TriggerKanbanAIAnalysis)
			r.Get("/reports/{month}", h.GetKanbanVelocityReport)
			r.Get("/reports/current", h.GetKanbanCurrentStats)
		})

		// Financial routes (Revenue Model & P&L)
		r.Route("/financial", func(r chi.Router) {
			r.Get("/dashboard", h.GetFinancialDashboard)
			r.Get("/costs", h.GetCostBreakdown)
			r.Get("/costs/categories", h.GetCostsByCategory)
			r.Get("/forecast", h.GetAnnualForecast)
			r.Get("/pl/current", h.GetCurrentMonthPL)
			r.Get("/scenarios", h.GetScenarioPlanning)
			r.Post("/scenarios", h.PostScenarioPlanning)
			r.Get("/growth-drivers", h.GetGrowthDrivers)
			
			// Cost configuration persistence routes
			r.Get("/config/costs", h.GetCostConfigs)
			r.Post("/config/costs", h.SaveCostConfig)
			r.Put("/config/costs", h.SaveAllCostConfigs)
			r.Delete("/config/costs/{type}", h.DeleteCostConfig)
			r.Post("/config/costs/reset", h.ResetCostConfigs)
		})

		// Intelligence routes (AI Learning & Recommendations)
		r.Route("/intelligence", func(r chi.Router) {
			r.Get("/dashboard", h.GetIntelligenceDashboard)
			r.Get("/recommendations", h.GetIntelligenceRecommendations)
			r.Patch("/recommendations/{id}", h.UpdateRecommendationStatus)
			r.Get("/property-offer", h.GetPropertyOfferIntelligence)
			r.Get("/timing", h.GetTimingIntelligence)
			r.Get("/esp-isp", h.GetESPISPIntelligence)
			r.Get("/strategy", h.GetStrategyIntelligence)
			r.Post("/learn", h.TriggerLearningCycle)
		})

		// Planning routes (Volume Planning Dashboard)
		r.Route("/planning", func(r chi.Router) {
			r.Get("/dashboard", h.GetPlanningDashboard)
			
			// Routing plans (saved configurations)
			r.Get("/plans", h.GetRoutingPlans)
			r.Get("/plans/active", h.GetActiveRoutingPlan)
			r.Post("/plans", h.SaveRoutingPlan)
			r.Get("/plans/{id}", h.GetRoutingPlan)
			r.Put("/plans/{id}", h.SaveRoutingPlan)
			r.Delete("/plans/{id}", h.DeleteRoutingPlan)
			r.Post("/plans/{id}/activate", h.SetActiveRoutingPlan)
		})

		// Suggestions/Improvements routes
		r.Route("/suggestions", func(r chi.Router) {
			r.Get("/", h.GetSuggestions)
			r.Post("/", h.CreateSuggestion)
			r.Get("/{id}", h.GetSuggestion)
			r.Patch("/{id}/status", h.UpdateSuggestionStatus)
			r.Post("/{id}/regenerate", h.RegenerateRequirements)
			r.Delete("/{id}", h.DeleteSuggestion)
		})

		// Mailing Platform routes - registered via RegisterMailingRoutes in server.go
		// Note: Tracking domain routes are registered under /api/mailing/tracking-domains
		// via RegisterTrackingDomainRoutes in server.go

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

	// Serve static files for React frontend (SPA with fallback to index.html)
	spaHandler(r, "./web/dist")

	return r, apiRouter
}

// spaHandler serves static files and falls back to index.html for SPA routing
func spaHandler(r chi.Router, staticPath string) {
	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		// Get the path
		path := req.URL.Path
		
		// Skip API routes
		if strings.HasPrefix(path, "/api") || strings.HasPrefix(path, "/health") {
			http.NotFound(w, req)
			return
		}
		
		// Try to serve the file directly
		filePath := filepath.Join(staticPath, path)
		
		// Check if file exists
		if _, err := os.Stat(filePath); err == nil {
			http.ServeFile(w, req, filePath)
			return
		}
		
		// For SPA routing, serve index.html for unknown paths
		indexPath := filepath.Join(staticPath, "index.html")
		http.ServeFile(w, req, indexPath)
	})
}
