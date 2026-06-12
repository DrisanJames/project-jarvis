package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ----------------------------------------------------------------------------
// Shared DB handle for the consciousness API layer.
//
// The ConsciousnessService is constructed from engine objects only (none of
// which expose their *sql.DB), so the mailing DB is wired in separately via
// SetConsciousnessDB at startup. All DB-backed features below degrade
// gracefully when the handle is absent: UUID->name enrichment becomes a
// pass-through and the hourly assessment returns 503.
//
// IMPORTANT: this keeps every DB lookup at the API layer — the engine hot
// path (internal/engine) must never gain DB lookups for display concerns.
// ----------------------------------------------------------------------------

var (
	consciousnessDBMu sync.RWMutex
	consciousnessDB   *sql.DB
)

// SetConsciousnessDB wires the mailing DB into the consciousness API layer.
// Call once at startup, right after NewConsciousnessService.
func SetConsciousnessDB(db *sql.DB) {
	consciousnessDBMu.Lock()
	consciousnessDB = db
	consciousnessDBMu.Unlock()
}

func getConsciousnessDB() *sql.DB {
	consciousnessDBMu.RLock()
	defer consciousnessDBMu.RUnlock()
	return consciousnessDB
}

// ----------------------------------------------------------------------------
// Campaign UUID -> name enrichment.
//
// Thought/signal text embeds raw campaign UUIDs ("during campaign e9579398-…").
// At the API boundary we regex-extract UUIDs from thought content + related_ids,
// batch-resolve names from mailing_campaigns, and rewrite the display text to
// `"<campaign name>" (e957…)`. Lookup failures (or non-campaign UUIDs) leave
// the original UUID untouched — enrichment must never break a payload.
// ----------------------------------------------------------------------------

var campaignUUIDRe = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

const campaignNameCacheCap = 5000

var (
	campaignNameMu sync.Mutex
	// campaignNameCache maps lowercase UUID -> campaign name. An empty string
	// is a negative-cache entry (valid UUID that is not a campaign), so we
	// don't re-query the same subscriber/wave IDs on every request.
	campaignNameCache = make(map[string]string, 256)
)

// resolveCampaignNames returns lowercase-UUID -> name for every id that maps
// to a row in mailing_campaigns. Cache-first; misses are resolved with one
// batched query. Invalid UUIDs are ignored (they would poison the ::uuid[]
// cast). On any DB error the partial result is returned and the original
// UUIDs stay in the text.
func resolveCampaignNames(ctx context.Context, ids []string) map[string]string {
	out := make(map[string]string)
	if len(ids) == 0 {
		return out
	}

	missSet := make(map[string]struct{})
	campaignNameMu.Lock()
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		key := strings.ToLower(id)
		if name, ok := campaignNameCache[key]; ok {
			if name != "" {
				out[key] = name
			}
			continue
		}
		missSet[key] = struct{}{}
	}
	campaignNameMu.Unlock()

	if len(missSet) == 0 {
		return out
	}
	db := getConsciousnessDB()
	if db == nil {
		return out
	}

	misses := make([]string, 0, len(missSet))
	for k := range missSet {
		misses = append(misses, k)
	}

	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := db.QueryContext(qctx,
		`SELECT id::text, name FROM mailing_campaigns WHERE id = ANY($1::uuid[])`,
		pq.Array(misses))
	if err != nil {
		return out // leave UUIDs as-is
	}
	defer rows.Close()

	found := make(map[string]string, len(misses))
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil && name != "" {
			found[strings.ToLower(id)] = name
		}
	}

	campaignNameMu.Lock()
	for _, m := range misses {
		if len(campaignNameCache) >= campaignNameCacheCap {
			// Crude bounded eviction: drop arbitrary entries. The cache is a
			// display nicety; correctness never depends on what's in it.
			for k := range campaignNameCache {
				delete(campaignNameCache, k)
				if len(campaignNameCache) < campaignNameCacheCap {
					break
				}
			}
		}
		name := found[m] // "" => negative cache
		campaignNameCache[m] = name
		if name != "" {
			out[m] = name
		}
	}
	campaignNameMu.Unlock()
	return out
}

// enrichTextWithCampaignNames rewrites every resolved campaign UUID in s to
// the short display form `"<name>" (e957…)`. Unresolved UUIDs pass through.
func enrichTextWithCampaignNames(s string, names map[string]string) string {
	if s == "" || len(names) == 0 {
		return s
	}
	return campaignUUIDRe.ReplaceAllStringFunc(s, func(m string) string {
		if name := names[strings.ToLower(m)]; name != "" {
			return `"` + name + `" (` + m[:4] + `…)`
		}
		return m
	})
}

// enrichThoughts resolves all campaign UUIDs across the batch with a single
// lookup and rewrites each thought's display text. Thoughts are value copies
// from the engine (GetRecentThoughts/GetState copy the ring), so mutating
// Content/Reasoning here never touches engine state.
func enrichThoughts(ctx context.Context, thoughts []engine.Thought) []engine.Thought {
	if len(thoughts) == 0 {
		return thoughts
	}
	var candidates []string
	for i := range thoughts {
		candidates = append(candidates, campaignUUIDRe.FindAllString(thoughts[i].Content, -1)...)
		candidates = append(candidates, campaignUUIDRe.FindAllString(thoughts[i].Reasoning, -1)...)
		candidates = append(candidates, thoughts[i].RelatedIDs...)
	}
	names := resolveCampaignNames(ctx, candidates)
	if len(names) == 0 {
		return thoughts
	}
	for i := range thoughts {
		thoughts[i].Content = enrichTextWithCampaignNames(thoughts[i].Content, names)
		thoughts[i].Reasoning = enrichTextWithCampaignNames(thoughts[i].Reasoning, names)
	}
	return thoughts
}

// enrichThought is the single-thought variant used by the SSE stream.
func enrichThought(ctx context.Context, t engine.Thought) engine.Thought {
	candidates := campaignUUIDRe.FindAllString(t.Content, -1)
	candidates = append(candidates, campaignUUIDRe.FindAllString(t.Reasoning, -1)...)
	candidates = append(candidates, t.RelatedIDs...)
	names := resolveCampaignNames(ctx, candidates)
	if len(names) == 0 {
		return t
	}
	t.Content = enrichTextWithCampaignNames(t.Content, names)
	t.Reasoning = enrichTextWithCampaignNames(t.Reasoning, names)
	return t
}

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
	r.Route("/consciousness", func(r chi.Router) {
		r.Get("/state", s.HandleGetState)
		r.Get("/philosophies", s.HandleGetPhilosophies)
		r.Get("/philosophies/{isp}", s.HandleGetISPPhilosophies)
		r.Get("/thoughts", s.HandleGetThoughts)
		r.Get("/thoughts/stream", s.HandleThoughtStream)
		r.Get("/hourly-assessment", s.HandleHourlyAssessment)
	})

	r.Route("/campaign-events", func(r chi.Router) {
		r.Get("/campaigns", s.HandleGetCampaigns)
		r.Get("/campaigns/{campaignId}/metrics", s.HandleGetCampaignMetrics)
		r.Get("/campaigns/{campaignId}/report", s.HandleGetCampaignReport)
		r.Post("/campaigns/{campaignId}/flush", s.HandleFlushCampaignReport)
		r.Post("/ingest", s.HandleIngestEvent)
		r.Get("/stream", s.HandleCampaignEventStream)
	})
}

// --- Consciousness Endpoints ---

// HandleGetState returns the full consciousness state, with campaign UUIDs in
// thought/summary text rewritten to human-friendly campaign names.
func (s *ConsciousnessService) HandleGetState(w http.ResponseWriter, r *http.Request) {
	state := s.consciousness.GetState()
	state.Thoughts = enrichThoughts(r.Context(), state.Thoughts)
	if names := resolveCampaignNames(r.Context(), campaignUUIDRe.FindAllString(state.Summary, -1)); len(names) > 0 {
		state.Summary = enrichTextWithCampaignNames(state.Summary, names)
	}
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
	thoughts := enrichThoughts(r.Context(), s.consciousness.GetRecentThoughts(limit))
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
			// Enrich per-event: usually a cache hit; misses cost one short
			// bounded DB lookup which then warms the cache for the stream.
			data, _ := json.Marshal(enrichThought(ctx, thought))
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

// ─── Hourly Assessment ───────────────────────────────────────────────────────
//
// The first step toward the consciousness engine ADVISING the Domain Agents
// before send days: an hourly aggregated view of every (ISP, VMTA) pipe from
// PMTA accounting, with routing recommendations ("this pipe is deferring —
// shift the family to the pipe with better reputation"). Cached 10 minutes —
// this is an assessment, not a live feed.

type assessmentPipe struct {
	ISP          string  `json:"isp"`
	VMTA         string  `json:"vmta"`
	Attempted    int64   `json:"attempted"`
	Delivered    int64   `json:"delivered"`
	Deferred     int64   `json:"deferred"`
	Bounced      int64   `json:"bounced"`
	DeliveredPct float64 `json:"delivered_pct"`
	DeferralPct  float64 `json:"deferral_pct"`
	BouncePct    float64 `json:"bounce_pct"`
}

type assessmentRecommendation struct {
	ISP      string `json:"isp"`
	Kind     string `json:"kind"` // reroute | pace_down
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type hourlyAssessmentResponse struct {
	GeneratedAt     time.Time                  `json:"generated_at"`
	WindowMinutes   int                        `json:"window_minutes"`
	Pipes           []assessmentPipe           `json:"pipes"`
	Recommendations []assessmentRecommendation `json:"recommendations"`
}

var hourlyAssessmentCache = struct {
	sync.Mutex
	resp    *hourlyAssessmentResponse
	expires time.Time
}{}

// HandleHourlyAssessment — GET /api/mailing/consciousness/hourly-assessment.
func (s *ConsciousnessService) HandleHourlyAssessment(w http.ResponseWriter, r *http.Request) {
	db := getConsciousnessDB()
	if db == nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "assessment not wired: consciousness DB not set"})
		return
	}

	hourlyAssessmentCache.Lock()
	if hourlyAssessmentCache.resp != nil && time.Now().Before(hourlyAssessmentCache.expires) {
		resp := *hourlyAssessmentCache.resp
		hourlyAssessmentCache.Unlock()
		respondJSON(w, http.StatusOK, resp)
		return
	}
	hourlyAssessmentCache.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Classification mirrors handlers_deliverability.go exactly (shared consts).
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(recipient_isp, ''), 'other'), COALESCE(NULLIF(vmta, ''), '(none)'),
		       COUNT(*) FILTER (WHERE `+attemptedFilterSQL+`),
		       COUNT(*) FILTER (WHERE `+deliveredFilterSQL+`),
		       COUNT(*) FILTER (WHERE `+deferredFilterSQL+`),
		       COUNT(*) FILTER (WHERE record_type = 'b')
		FROM pmta_acct_raw
		WHERE received_at > NOW() - INTERVAL '60 minutes'
		GROUP BY 1, 2
		ORDER BY 1, 3 DESC
	`)
	if err != nil {
		// Serve a stale snapshot over a 500 if we have one.
		hourlyAssessmentCache.Lock()
		stale := hourlyAssessmentCache.resp
		hourlyAssessmentCache.Unlock()
		if stale != nil {
			respondJSON(w, http.StatusOK, *stale)
			return
		}
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	resp := hourlyAssessmentResponse{
		GeneratedAt:   time.Now().UTC(),
		WindowMinutes: 60,
		Pipes:         []assessmentPipe{},
	}
	byISP := map[string][]assessmentPipe{}
	for rows.Next() {
		var p assessmentPipe
		if err := rows.Scan(&p.ISP, &p.VMTA, &p.Attempted, &p.Delivered, &p.Deferred, &p.Bounced); err != nil {
			continue
		}
		if p.Attempted > 0 {
			p.DeliveredPct = 100 * float64(p.Delivered) / float64(p.Attempted)
			p.DeferralPct = 100 * float64(p.Deferred) / float64(p.Attempted)
			p.BouncePct = 100 * float64(p.Bounced) / float64(p.Attempted)
		}
		resp.Pipes = append(resp.Pipes, p)
		byISP[p.ISP] = append(byISP[p.ISP], p)
	}

	const minAttempts = 50
	resp.Recommendations = []assessmentRecommendation{}
	for isp, pipes := range byISP {
		qualified := []assessmentPipe{}
		for _, p := range pipes {
			if p.Attempted >= minAttempts {
				qualified = append(qualified, p)
			}
		}
		if len(qualified) == 0 {
			continue
		}
		sort.Slice(qualified, func(i, j int) bool { return qualified[i].DeferralPct < qualified[j].DeferralPct })
		best := qualified[0]

		allStorming := true
		for _, p := range qualified {
			if p.DeferralPct <= 50 {
				allStorming = false
				break
			}
		}
		if allStorming {
			resp.Recommendations = append(resp.Recommendations, assessmentRecommendation{
				ISP: isp, Kind: "pace_down", Severity: "critical",
				Message: fmt.Sprintf("%s: every pipe deferring >50%% in the last hour — pace down; this is reputation, not routing.", isp),
			})
			continue
		}
		for _, p := range qualified[1:] {
			if p.DeferralPct > 2*best.DeferralPct && p.DeferralPct-best.DeferralPct > 5 {
				sev := "warn"
				if p.DeferralPct > 50 {
					sev = "critical"
				}
				resp.Recommendations = append(resp.Recommendations, assessmentRecommendation{
					ISP: isp, Kind: "reroute", Severity: sev,
					Message: fmt.Sprintf("%s deferring %.1f%% to %s — %s at %.1f%%; shift %s-family volume there.",
						p.VMTA, p.DeferralPct, isp, best.VMTA, best.DeferralPct, isp),
				})
			}
		}
	}

	hourlyAssessmentCache.Lock()
	hourlyAssessmentCache.resp = &resp
	hourlyAssessmentCache.expires = time.Now().Add(10 * time.Minute)
	hourlyAssessmentCache.Unlock()

	respondJSON(w, http.StatusOK, resp)
}
