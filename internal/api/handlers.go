package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/agent"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/datainjections"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
	"github.com/ignite/sparkpost-monitor/internal/financial"
	"github.com/ignite/sparkpost-monitor/internal/intelligence"
	"github.com/ignite/sparkpost-monitor/internal/kanban"
	"github.com/ignite/sparkpost-monitor/internal/mailgun"
	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
	"github.com/ignite/sparkpost-monitor/internal/storage"
)

// Handlers contains all HTTP handlers
type Handlers struct {
	collector             *sparkpost.Collector
	mailgunCollector      *mailgun.Collector
	sesCollector          *ses.Collector
	ongageCollector       *ongage.Collector
	everflowCollector     *everflow.Collector
	networkIntelCollector *everflow.NetworkIntelligenceCollector
	enrichmentService     *everflow.EnrichmentService
	dataInjectionsService *datainjections.Service
	kanbanService         *kanban.Service
	kanbanAIAnalyzer      *kanban.AIAnalyzer
	kanbanArchival        *kanban.ArchivalService
	agent                 *agent.Agent
	openaiAgent           *agent.OpenAIAgent
	agenticLoop           *agent.AgenticLoop
	storage               *storage.Storage
	revenueModelService   *financial.RevenueModelService
	intelligenceService   *intelligence.Service
	config                *config.Config
}

// SetConfig sets the application config
func (h *Handlers) SetConfig(cfg *config.Config) {
	h.config = cfg
}

// NewHandlers creates a new Handlers instance
func NewHandlers(collector *sparkpost.Collector, agent *agent.Agent, storage *storage.Storage) *Handlers {
	return &Handlers{
		collector: collector,
		agent:     agent,
		storage:   storage,
	}
}

// SetMailgunCollector sets the Mailgun collector
func (h *Handlers) SetMailgunCollector(collector *mailgun.Collector) {
	h.mailgunCollector = collector
}

// SetSESCollector sets the SES collector
func (h *Handlers) SetSESCollector(collector *ses.Collector) {
	h.sesCollector = collector
}

// SetOngageCollector sets the Ongage collector
func (h *Handlers) SetOngageCollector(collector *ongage.Collector) {
	h.ongageCollector = collector
}

// GetOngageCollector returns the Ongage collector
func (h *Handlers) GetOngageCollector() *ongage.Collector {
	return h.ongageCollector
}

// SetEverflowCollector sets the Everflow collector
func (h *Handlers) SetEverflowCollector(collector *everflow.Collector) {
	h.everflowCollector = collector
}

// SetNetworkIntelligenceCollector sets the network-wide intelligence collector
func (h *Handlers) SetNetworkIntelligenceCollector(collector *everflow.NetworkIntelligenceCollector) {
	h.networkIntelCollector = collector
}

// SetEnrichmentService sets the Everflow enrichment service
func (h *Handlers) SetEnrichmentService(service *everflow.EnrichmentService) {
	h.enrichmentService = service
}

// SetOpenAIAgent sets the OpenAI-powered conversational agent
func (h *Handlers) SetOpenAIAgent(openaiAgent *agent.OpenAIAgent) {
	h.openaiAgent = openaiAgent
}

// SetAgenticLoop sets the self-learning agentic loop
func (h *Handlers) SetAgenticLoop(loop *agent.AgenticLoop) {
	h.agenticLoop = loop
}

// HandleAgenticStatus returns the status of the agentic self-learning system
func (h *Handlers) HandleAgenticStatus(w http.ResponseWriter, r *http.Request) {
	if h.agenticLoop == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"running": false,
			"message": "Agentic loop not initialized",
		})
		return
	}
	
	status := h.agenticLoop.GetStatus()
	respondJSON(w, http.StatusOK, status)
}

// HandleAgenticActions returns recent agentic actions
func (h *Handlers) HandleAgenticActions(w http.ResponseWriter, r *http.Request) {
	if h.agenticLoop == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"actions": []interface{}{},
		})
		return
	}
	
	actions := h.agenticLoop.GetRecentActions(20)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"actions": actions,
		"total":   len(actions),
	})
}

// SetDataInjectionsService sets the data injections service
func (h *Handlers) SetDataInjectionsService(service *datainjections.Service) {
	h.dataInjectionsService = service
}

// SetKanbanService sets the Kanban service
func (h *Handlers) SetKanbanService(service *kanban.Service) {
	h.kanbanService = service
}

// SetKanbanAIAnalyzer sets the Kanban AI analyzer
func (h *Handlers) SetKanbanAIAnalyzer(analyzer *kanban.AIAnalyzer) {
	h.kanbanAIAnalyzer = analyzer
}

// SetKanbanArchival sets the Kanban archival service
func (h *Handlers) SetKanbanArchival(archival *kanban.ArchivalService) {
	h.kanbanArchival = archival
}

// SetRevenueModelService sets the revenue model service for financial dashboard
func (h *Handlers) SetRevenueModelService(service *financial.RevenueModelService) {
	h.revenueModelService = service
}

// Response helpers

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{"error": message})
}

// DateRange represents a date range for filtering data
type DateRange struct {
	StartDate time.Time
	EndDate   time.Time
	Type      string // "mtd", "last30", "lastMonth"
}

// parseDateRange extracts date range from query parameters
// If no params provided, defaults to Month to Date (MTD)
func parseDateRange(r *http.Request) DateRange {
	now := time.Now()
	rangeType := r.URL.Query().Get("range_type")
	startDateStr := r.URL.Query().Get("start_date")
	endDateStr := r.URL.Query().Get("end_date")

	// If explicit dates provided, parse them
	if startDateStr != "" && endDateStr != "" {
		startDate, err1 := time.Parse("2006-01-02", startDateStr)
		endDate, err2 := time.Parse("2006-01-02", endDateStr)
		if err1 == nil && err2 == nil {
			// Set end date to end of day
			endDate = endDate.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
			return DateRange{
				StartDate: startDate,
				EndDate:   endDate,
				Type:      rangeType,
			}
		}
	}

	// Calculate based on range type (default to MTD)
	switch rangeType {
	case "last30":
		// Last 30 days rolling window
		return DateRange{
			StartDate: now.AddDate(0, 0, -30),
			EndDate:   now,
			Type:      "last30",
		}
	case "lastMonth":
		// Previous complete month
		firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		lastDayOfPrevMonth := firstOfThisMonth.Add(-time.Second)
		firstOfPrevMonth := time.Date(lastDayOfPrevMonth.Year(), lastDayOfPrevMonth.Month(), 1, 0, 0, 0, 0, now.Location())
		return DateRange{
			StartDate: firstOfPrevMonth,
			EndDate:   lastDayOfPrevMonth,
			Type:      "lastMonth",
		}
	default:
		// Month to Date (default)
		firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return DateRange{
			StartDate: firstOfMonth,
			EndDate:   now,
			Type:      "mtd",
		}
	}
}

// Health check

// HealthCheck returns the health status of the API
func (h *Handlers) HealthCheck(w http.ResponseWriter, r *http.Request) {
	status := "healthy"
	lastFetch := h.collector.GetLastFetchTime()
	
	// Consider unhealthy if no data in last 5 minutes
	if time.Since(lastFetch) > 5*time.Minute && !lastFetch.IsZero() {
		status = "degraded"
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":     status,
		"timestamp":  time.Now(),
		"last_fetch": lastFetch,
		"is_running": h.collector.IsRunning(),
	})
}
