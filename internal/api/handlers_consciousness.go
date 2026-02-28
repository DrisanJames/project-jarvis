package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ignite/sparkpost-monitor/internal/engine"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ConsciousnessService exposes the consciousness layer and campaign event
// tracking as REST endpoints + SSE streams.
type ConsciousnessService struct {
	consciousness *engine.Consciousness
	tracker       *engine.CampaignEventTracker
	convictions   *engine.ConvictionStore
	processor     *engine.SignalProcessor
	orgID         string
}

// NewConsciousnessService creates the API service.
func NewConsciousnessService(
	consciousness *engine.Consciousness,
	tracker *engine.CampaignEventTracker,
	convictions *engine.ConvictionStore,
	processor *engine.SignalProcessor,
	orgID string,
) *ConsciousnessService {
	return &ConsciousnessService{
		consciousness: consciousness,
		tracker:       tracker,
		convictions:   convictions,
		processor:     processor,
		orgID:         orgID,
	}
}

// RegisterRoutes mounts all consciousness and campaign event routes.
func (s *ConsciousnessService) RegisterRoutes(r chi.Router) {
	r.Route("/api/mailing/consciousness", func(r chi.Router) {
		r.Get("/state", s.HandleGetState)
		r.Get("/philosophies", s.HandleGetPhilosophies)
		r.Get("/philosophies/{isp}", s.HandleGetISPPhilosophies)
		r.Get("/thoughts", s.HandleGetThoughts)
		r.Get("/thoughts/stream", s.HandleThoughtStream)
	})

	r.Route("/api/mailing/campaign-events", func(r chi.Router) {
		r.Get("/campaigns", s.HandleGetCampaigns)
		r.Get("/campaigns/{campaignId}/metrics", s.HandleGetCampaignMetrics)
		r.Get("/campaigns/{campaignId}/report", s.HandleGetCampaignReport)
		r.Post("/campaigns/{campaignId}/flush", s.HandleFlushCampaignReport)
		r.Post("/ingest", s.HandleIngestEvent)
		r.Get("/stream", s.HandleCampaignEventStream)
	})
}

// --- Consciousness Endpoints ---

// HandleGetState returns the full consciousness state.
func (s *ConsciousnessService) HandleGetState(w http.ResponseWriter, r *http.Request) {
	state := s.consciousness.GetState()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

// HandleGetPhilosophies returns all current philosophies.
func (s *ConsciousnessService) HandleGetPhilosophies(w http.ResponseWriter, r *http.Request) {
	philosophies := s.consciousness.GetPhilosophies()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(philosophies)
}

// HandleGetISPPhilosophies returns philosophies for a specific ISP.
func (s *ConsciousnessService) HandleGetISPPhilosophies(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	philosophies := s.consciousness.GetPhilosophiesByISP(isp)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(philosophies)
}

// HandleGetThoughts returns recent thoughts.
func (s *ConsciousnessService) HandleGetThoughts(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	thoughts := s.consciousness.GetRecentThoughts(limit)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(thoughts)
}

// HandleThoughtStream is an SSE endpoint for real-time thought streaming.
func (s *ConsciousnessService) HandleThoughtStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	subID := uuid.New().String()
	ch := s.consciousness.Subscribe(subID)
	defer s.consciousness.Unsubscribe(subID)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case thought, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(thought)
			fmt.Fprintf(w, "event: thought\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// --- Campaign Event Endpoints ---

// HandleGetCampaigns returns all active campaigns.
func (s *ConsciousnessService) HandleGetCampaigns(w http.ResponseWriter, r *http.Request) {
	campaigns := s.tracker.GetAllCampaigns()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(campaigns)
}

// HandleGetCampaignMetrics returns metrics for a specific campaign.
func (s *ConsciousnessService) HandleGetCampaignMetrics(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "campaignId")
	metrics := s.tracker.GetMetrics(id)
	if metrics == nil {
		http.Error(w, "campaign not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

// HandleGetCampaignReport returns the full report for a campaign.
func (s *ConsciousnessService) HandleGetCampaignReport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "campaignId")
	report := s.tracker.GetReport(id)
	if report == nil {
		http.Error(w, "campaign not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(report)
}

// HandleFlushCampaignReport flushes the campaign report to S3.
func (s *ConsciousnessService) HandleFlushCampaignReport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "campaignId")
	if err := s.tracker.FlushToS3(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "flushed", "campaign_id": id})
}

// HandleIngestEvent receives campaign events from the tracking system.
func (s *ConsciousnessService) HandleIngestEvent(w http.ResponseWriter, r *http.Request) {
	var events []engine.CampaignEvent
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		var single engine.CampaignEvent
		if err2 := json.NewDecoder(r.Body).Decode(&single); err2 != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		events = []engine.CampaignEvent{single}
	}

	for _, e := range events {
		s.tracker.RecordEvent(e)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"ingested": len(events)})
}

// HandleCampaignEventStream is an SSE endpoint for real-time campaign events.
func (s *ConsciousnessService) HandleCampaignEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	subID := uuid.New().String()
	ch := s.tracker.Subscribe(subID)
	defer s.tracker.Unsubscribe(subID)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: campaign_event\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}
