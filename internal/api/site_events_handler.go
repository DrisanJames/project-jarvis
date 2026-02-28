package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/datanorm"
)

// SiteEventsHandler accepts engagement events from owned websites (H9, H10).
type SiteEventsHandler struct {
	eventWriter *datanorm.EventWriter
	recentSeen  map[string]time.Time // dedup cache (H23)
}

func NewSiteEventsHandler(ew *datanorm.EventWriter) *SiteEventsHandler {
	return &SiteEventsHandler{
		eventWriter: ew,
		recentSeen:  make(map[string]time.Time),
	}
}

type siteEventPayload struct {
	EID       string                 `json:"eid"`
	CID       string                 `json:"cid"`
	SID       string                 `json:"sid"`
	EventType string                 `json:"event_type"`
	Metadata  map[string]interface{} `json:"metadata"`
}

// HandleSiteEvent handles POST /api/v1/site-events.
// Accepts both application/json and text/plain (for sendBeacon, H10).
func (h *SiteEventsHandler) HandleSiteEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// H9: Referrer check
	referer := r.Header.Get("Referer")
	if referer != "" && !isAllowedReferer(referer) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// H9: Bot detection
	ua := r.UserAgent()
	if isBot(ua) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var payload siteEventPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	if payload.EID == "" || payload.EventType == "" {
		http.Error(w, "missing required fields", http.StatusBadRequest)
		return
	}

	// H23: Dedup within 30-second window
	dedupKey := fmt.Sprintf("%s:%s:%v", payload.EID, payload.EventType, payload.Metadata["page_path"])
	if lastSeen, ok := h.recentSeen[dedupKey]; ok && time.Since(lastSeen) < 30*time.Second {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.recentSeen[dedupKey] = time.Now()
	// Evict old entries periodically
	if len(h.recentSeen) > 10000 {
		cutoff := time.Now().Add(-30 * time.Second)
		for k, v := range h.recentSeen {
			if v.Before(cutoff) {
				delete(h.recentSeen, k)
			}
		}
	}

	emailHash := siteEventHash(payload.SID)
	err := h.eventWriter.WriteEvent(r.Context(), emailHash, payload.EventType, nil, nil, "site", payload.Metadata, true)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleSiteEventBeacon handles GET /api/v1/site-events/beacon (img pixel fallback).
func (h *SiteEventsHandler) HandleSiteEventBeacon(w http.ResponseWriter, r *http.Request) {
	eid := r.URL.Query().Get("eid")
	eventType := r.URL.Query().Get("event_type")

	if eid != "" && eventType != "" {
		emailHash := siteEventHash(eid)
		metadata := map[string]interface{}{
			"page_path": r.URL.Query().Get("page_path"),
		}
		h.eventWriter.WriteEvent(r.Context(), emailHash, eventType, nil, nil, "site_beacon", metadata, false)
	}

	// Return 1x1 transparent GIF
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(transparentGIF)
}

var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00,
	0x80, 0x00, 0x00, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x21,
	0xf9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00,
	0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44,
	0x01, 0x00, 0x3b,
}

func siteEventHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

var allowedRefererDomains = []string{"getmecoupons.net", "discountblog.com", "quizfiesta.com", "localhost"}

func isAllowedReferer(referer string) bool {
	lower := strings.ToLower(referer)
	for _, d := range allowedRefererDomains {
		if strings.Contains(lower, d) {
			return true
		}
	}
	return false
}

var botKeywords = []string{"bot", "crawler", "spider", "headless", "phantom", "wget", "curl", "python-requests"}

func isBot(ua string) bool {
	lower := strings.ToLower(ua)
	for _, kw := range botKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
