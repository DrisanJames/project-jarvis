package tracking

import (
	"encoding/base64"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// 1x1 transparent GIF
var pixelGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00,
	0x80, 0x00, 0x00, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x2c,
	0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02,
	0x02, 0x44, 0x01, 0x00, 0x3b,
}

type Handler struct {
	pub *Publisher
}

func NewHandler(pub *Publisher) *Handler {
	return &Handler{pub: pub}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/track/open/{data}/{sig}", h.HandleOpen)
	r.Get("/track/click/{data}/{sig}", h.HandleClick)
	r.Get("/track/unsubscribe/{data}/{sig}", h.HandleUnsubscribe)
	r.Get("/health", h.HandleHealth)
	return r
}

func (h *Handler) HandleOpen(w http.ResponseWriter, r *http.Request) {
	encoded := chi.URLParam(r, "data")

	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		h.servePixel(w)
		return
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) < 4 {
		h.servePixel(w)
		return
	}

	evt := TrackingEvent{
		EventType:    EventOpen,
		OrgID:        parts[0],
		CampaignID:   parts[1],
		SubscriberID: parts[2],
		EmailID:      parts[3],
		IPAddress:    realIP(r),
		UserAgent:    r.UserAgent(),
		Timestamp:    time.Now().UTC(),
	}
	h.pub.Publish(r.Context(), evt)

	log.Printf("OPEN campaign=%s subscriber=%s", evt.CampaignID, evt.SubscriberID)
	h.servePixel(w)
}

func (h *Handler) HandleClick(w http.ResponseWriter, r *http.Request) {
	encoded := chi.URLParam(r, "data")

	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		http.Error(w, "bad link", http.StatusBadRequest)
		return
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) < 5 {
		http.Error(w, "bad link", http.StatusBadRequest)
		return
	}

	originalURL := parts[4]

	evt := TrackingEvent{
		EventType:    EventClick,
		OrgID:        parts[0],
		CampaignID:   parts[1],
		SubscriberID: parts[2],
		EmailID:      parts[3],
		LinkURL:      originalURL,
		IPAddress:    realIP(r),
		UserAgent:    r.UserAgent(),
		Timestamp:    time.Now().UTC(),
	}
	h.pub.Publish(r.Context(), evt)

	log.Printf("CLICK campaign=%s subscriber=%s url=%s", evt.CampaignID, evt.SubscriberID, originalURL)
	http.Redirect(w, r, originalURL, http.StatusTemporaryRedirect)
}

func (h *Handler) HandleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	encoded := chi.URLParam(r, "data")

	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		http.Error(w, "bad link", http.StatusBadRequest)
		return
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) < 3 {
		http.Error(w, "bad link", http.StatusBadRequest)
		return
	}

	evt := TrackingEvent{
		EventType:    EventUnsubscribe,
		OrgID:        parts[0],
		CampaignID:   parts[1],
		SubscriberID: parts[2],
		IPAddress:    realIP(r),
		UserAgent:    r.UserAgent(),
		Timestamp:    time.Now().UTC(),
	}
	if len(parts) > 3 {
		evt.EmailID = parts[3]
	}
	h.pub.Publish(r.Context(), evt)

	log.Printf("UNSUB campaign=%s subscriber=%s", evt.CampaignID, evt.SubscriberID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html><html><body style="font-family:Arial,sans-serif;text-align:center;padding:50px;">
		<h1>You have been unsubscribed</h1>
		<p>You will no longer receive emails from us.</p>
	</body></html>`))
}

func (h *Handler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (h *Handler) servePixel(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Write(pixelGIF)
}

func realIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.Index(xff, ","); idx > 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return xff
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return xri
	}
	return r.RemoteAddr
}

