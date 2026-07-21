package tracking

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/buildinfo"
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
	// RFC 8058 one-click: ISPs (Google/Yahoo) POST to the List-Unsubscribe
	// https URL with body "List-Unsubscribe=One-Click". This service is what
	// the public trk./t.em tracking hosts route to, and chi answered POST with
	// 405 Method Not Allowed (live-verified 2026-07-21 on trk.em.discountblog
	// .com et al.) — failing Google's compliance check on EVERY sending
	// domain. Register POST on both token shapes (parity with the API
	// server's registration in server_routes_mailing.go).
	r.Post("/track/unsubscribe/{data}", h.HandleUnsubscribe)
	r.Post("/track/unsubscribe/{data}/{sig}", h.HandleUnsubscribe)
	r.Get("/health", h.HandleHealth)
	r.Get("/version", h.HandleVersion)
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
	http.Redirect(w, r, applyDeadLinkRemap(originalURL), http.StatusTemporaryRedirect)
}

// deadLinkRemap repoints money links whose offer destination went dead AFTER
// the email was sent, so clicks on already-mailed messages are salvaged without
// re-mailing. The click event above still logs the true original URL; only the
// redirect target changes. Remove an entry once its offer is retired.
//
// jun21 Metal Roofing dead-link incident (2026-06-21): cratoolpro offer J78S2MD
// went dead; live destination is the eos57ytf smartlink (corrected 2026-06-21 —
// the interim k8k0hfdt 3QJ6DW/3LKS16 target was also wrong). Mirrors the same
// map in internal/api/mailing_tracking.go — the public t.em/track/click path is
// served by THIS tracking service, so the fix must live here too.
var deadLinkRemap = []struct{ match, to string }{
	{"cratoolpro.com/BJB4Q5BF/J78S2MD", "https://www.eos57ytf.com/K4C5ZLC/S6WFF5/"}, // metal roofing
	{"k8k0hfdt.com/3QJ6DW/3MZNPR", "https://www.xnonu.com/TQ5MX18J/XF1SR2CS/"},      // empire flooring (2026-06-21)
}

func applyDeadLinkRemap(rawURL string) string {
	for _, m := range deadLinkRemap {
		if strings.Contains(rawURL, m.match) {
			return carryAttribution(m.to, rawURL)
		}
	}
	return rawURL
}

// carryAttribution copies per-subscriber attribution params (source_id, sub1,
// sub2, sub3) from the original rendered money link onto the remap target so
// conversions on the new network still tie back to subscriber/brand/campaign.
func carryAttribution(target, originalURL string) string {
	u, err := url.Parse(originalURL)
	if err != nil {
		return target
	}
	src := u.Query()
	keep := url.Values{}
	for _, k := range []string{"source_id", "sub1", "sub2", "sub3"} {
		if v := src.Get(k); v != "" {
			keep.Set(k, v)
		}
	}
	if len(keep) == 0 {
		return target
	}
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + keep.Encode()
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
		// Two 4-part token shapes reach this endpoint:
		//   org|campaign|subscriber|emailID    (UUID — legacy pixel/link token)
		//   org|campaign|subscriber|brandRoot  (e.g. "discountblog.com" — the
		//       brand-scoped token in the List-Unsubscribe https leg, built by
		//       worker.GenerateBrandUnsubscribeURL)
		// Only a UUID is an email id; stuffing a brand root into EmailID would
		// pollute the event stream. Brand scoping itself is resolved
		// campaign-side by the SQS consumer, so the brand token needs no
		// further handling here.
		if _, err := uuid.Parse(parts[3]); err == nil {
			evt.EmailID = parts[3]
		}
	}
	h.pub.Publish(r.Context(), evt)

	log.Printf("UNSUB campaign=%s subscriber=%s method=%s", evt.CampaignID, evt.SubscriberID, r.Method)

	// RFC 8058: ISP one-click POST expects a minimal 200 response, not HTML.
	// The unsubscribe above is already recorded unconditionally — no
	// confirmation step on either method.
	if r.Method == http.MethodPost {
		w.WriteHeader(http.StatusOK)
		return
	}

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

func (h *Handler) HandleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buildinfo.Current())
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
