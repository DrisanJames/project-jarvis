package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// GlobalSuppressionAPI exposes the GlobalSuppressionHub via REST endpoints.
// This is the SINGLE system of record — all negative signals converge here,
// and all pre-send checks query here.
type GlobalSuppressionAPI struct {
	hub   *engine.GlobalSuppressionHub
	orgID string
}

func NewGlobalSuppressionAPI(hub *engine.GlobalSuppressionHub, orgID string) *GlobalSuppressionAPI {
	return &GlobalSuppressionAPI{hub: hub, orgID: orgID}
}

func (api *GlobalSuppressionAPI) RegisterRoutes(r chi.Router) {
	r.Route("/global-suppression", func(r chi.Router) {
		r.Get("/stats", api.HandleStats)
		r.Get("/count", api.HandleCount)
		r.Get("/search", api.HandleSearch)
		r.Get("/check/{email}", api.HandleCheckEmail)
		r.Get("/check-hash/{hash}", api.HandleCheckHash)
		r.Post("/check-batch", api.HandleCheckBatch)
		r.Post("/check-batch-md5", api.HandleCheckBatchMD5)
		r.Post("/suppress", api.HandleSuppress)
		r.Post("/suppress-bulk", api.HandleSuppressBulk)
		r.Delete("/remove/{email}", api.HandleRemove)
		r.Get("/export-md5", api.HandleExportMD5)
		r.Get("/stream", api.HandleStream)

		r.Post("/scrub-list", api.HandleScrubList)
	})
}

// HandleStats returns aggregated suppression statistics.
func (api *GlobalSuppressionAPI) HandleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := api.hub.GetStats(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// HandleCount returns the total suppressed count.
func (api *GlobalSuppressionAPI) HandleCount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"count": api.hub.Count()})
}

// HandleSearch searches suppressions by email or MD5 hash.
func (api *GlobalSuppressionAPI) HandleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	entries, total, err := api.hub.Search(r.Context(), query, limit, offset)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entries": entries,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// HandleCheckEmail checks if a single email is globally suppressed.
func (api *GlobalSuppressionAPI) HandleCheckEmail(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	suppressed := api.hub.IsSuppressed(email)
	md5 := engine.MD5Hash(email)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"email":      email,
		"md5_hash":   md5,
		"suppressed": suppressed,
	})
}

// HandleCheckHash checks if an MD5 hash is globally suppressed.
func (api *GlobalSuppressionAPI) HandleCheckHash(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	suppressed := api.hub.IsSuppressedByHash(hash)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"md5_hash":   hash,
		"suppressed": suppressed,
	})
}

// HandleCheckBatch checks a batch of emails against global suppression.
func (api *GlobalSuppressionAPI) HandleCheckBatch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Emails []string `json:"emails"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	start := time.Now()
	result := api.hub.CheckBatch(input.Emails)

	suppressed := 0
	deliverable := 0
	suppressedEmails := []string{}
	deliverableEmails := []string{}
	for email, isSuppressed := range result {
		if isSuppressed {
			suppressed++
			suppressedEmails = append(suppressedEmails, email)
		} else {
			deliverable++
			deliverableEmails = append(deliverableEmails, email)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":              len(input.Emails),
		"suppressed_count":   suppressed,
		"deliverable_count":  deliverable,
		"suppressed_emails":  suppressedEmails,
		"deliverable_emails": deliverableEmails,
		"processing_ms":      time.Since(start).Milliseconds(),
	})
}

// HandleCheckBatchMD5 checks a batch of MD5 hashes against global suppression.
func (api *GlobalSuppressionAPI) HandleCheckBatchMD5(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	start := time.Now()
	result := api.hub.CheckBatchMD5(input.Hashes)

	suppressed := 0
	deliverable := 0
	for _, isSuppressed := range result {
		if isSuppressed {
			suppressed++
		} else {
			deliverable++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":             len(input.Hashes),
		"suppressed_count":  suppressed,
		"deliverable_count": deliverable,
		"results":           result,
		"processing_ms":     time.Since(start).Milliseconds(),
	})
}

// HandleSuppress manually adds an email to global suppression.
func (api *GlobalSuppressionAPI) HandleSuppress(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Reason   string `json:"reason"`
		Source   string `json:"source"`
		ISP      string `json:"isp"`
		Campaign string `json:"campaign_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	if input.Email == "" {
		http.Error(w, `{"error":"email required"}`, http.StatusBadRequest)
		return
	}
	if input.Reason == "" {
		input.Reason = "manual"
	}
	if input.Source == "" {
		input.Source = "manual"
	}

	isNew, err := api.hub.Suppress(r.Context(), input.Email, input.Reason, input.Source, input.ISP, "", "", "", input.Campaign)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"is_new":   isNew,
		"email":    input.Email,
		"md5_hash": engine.MD5Hash(input.Email),
	})
}

// HandleSuppressBulk adds multiple emails to global suppression.
func (api *GlobalSuppressionAPI) HandleSuppressBulk(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Emails   []string `json:"emails"`
		Reason   string   `json:"reason"`
		Source   string   `json:"source"`
		ISP      string   `json:"isp"`
		Campaign string   `json:"campaign_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	if input.Reason == "" {
		input.Reason = "manual"
	}
	if input.Source == "" {
		input.Source = "bulk_import"
	}

	added := 0
	for _, email := range input.Emails {
		isNew, err := api.hub.Suppress(r.Context(), email, input.Reason, input.Source, input.ISP, "", "", "", input.Campaign)
		if err == nil && isNew {
			added++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"added":   added,
		"total":   len(input.Emails),
	})
}

// HandleRemove removes an email from global suppression (admin override).
func (api *GlobalSuppressionAPI) HandleRemove(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	if err := api.hub.Remove(r.Context(), email); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "email": email})
}

// HandleExportMD5 exports all suppressed MD5 hashes for external comparison.
func (api *GlobalSuppressionAPI) HandleExportMD5(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	hashes := api.hub.ExportMD5List()

	if format == "text" {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Disposition", "attachment; filename=global_suppression_md5.txt")
		for _, h := range hashes {
			fmt.Fprintln(w, h)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":  len(hashes),
		"hashes": hashes,
	})
}

// HandleScrubList takes an active mailing list and returns only deliverable
// emails after filtering against the global suppression hub. This is the
// core pre-send check — the single comparison point before any campaign send.
func (api *GlobalSuppressionAPI) HandleScrubList(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Emails []string `json:"emails"`
		MD5s   []string `json:"md5_hashes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	start := time.Now()

	deliverable := []string{}
	suppressed := []string{}

	if len(input.Emails) > 0 {
		result := api.hub.CheckBatch(input.Emails)
		for email, isSuppressed := range result {
			if isSuppressed {
				suppressed = append(suppressed, email)
			} else {
				deliverable = append(deliverable, email)
			}
		}
	}

	if len(input.MD5s) > 0 {
		result := api.hub.CheckBatchMD5(input.MD5s)
		for hash, isSuppressed := range result {
			if isSuppressed {
				suppressed = append(suppressed, hash)
			} else {
				deliverable = append(deliverable, hash)
			}
		}
	}

	totalInput := len(input.Emails) + len(input.MD5s)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_input":        totalInput,
		"deliverable_count":  len(deliverable),
		"suppressed_count":   len(suppressed),
		"suppression_rate":   suppressionRate(len(suppressed), totalInput),
		"deliverable":        deliverable,
		"suppressed":         suppressed,
		"processing_ms":      time.Since(start).Milliseconds(),
	})
}

// HandleStream provides an SSE stream of real-time suppression events.
func (api *GlobalSuppressionAPI) HandleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	subID := fmt.Sprintf("sse-%d", time.Now().UnixNano())
	ch := api.hub.Subscribe(subID)
	defer api.hub.Unsubscribe(subID)

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
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func suppressionRate(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den) * 100.0
}

// ScrubListForCampaign is a programmatic helper (not HTTP) that the campaign
// send pipeline calls to filter a recipient list before injection. It accepts
// emails and returns only those NOT on the global suppression list.
func ScrubListForCampaign(hub *engine.GlobalSuppressionHub, emails []string) (deliverable, suppressed []string) {
	if hub == nil {
		return emails, nil
	}

	result := hub.CheckBatch(emails)
	for _, email := range emails {
		lower := strings.ToLower(strings.TrimSpace(email))
		if result[lower] {
			suppressed = append(suppressed, email)
		} else {
			deliverable = append(deliverable, email)
		}
	}
	return deliverable, suppressed
}
