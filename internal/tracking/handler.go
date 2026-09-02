package tracking

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"regexp"
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

// eventPublisher is the async telemetry sink. *Publisher satisfies it in prod;
// tests inject a capturing fake so the machine/human LABEL can be asserted
// deterministically.
type eventPublisher interface {
	Publish(ctx context.Context, evt TrackingEvent)
}

type Handler struct {
	pub  eventPublisher
	dict *SmartLinkDictionary
}

// NewHandler wires the tracking handler. dict may be nil (or a nil-db
// dictionary) — the /o/ offer redirect then always falls back to brand root,
// and all /track/* behavior is unchanged. This keeps the service bootable with
// no DATABASE_URL configured (the graceful default).
func NewHandler(pub eventPublisher, dict *SmartLinkDictionary) *Handler {
	return &Handler{pub: pub, dict: dict}
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
	// Path-based, scanner-safe offer redirect.
	//   Brand-in-path (2026-07-22, the mint default):
	//     https://t.em.<brand>/o/<brand>/<subscriber_id>/<hash>/<campaign_id>
	//   Legacy 4-segment (still emitted whenever the mint can't derive a brand,
	//   and by every link already in an inbox):
	//     https://t.em.<brand>/o/<subscriber_id>/<hash>/<campaign_id>
	// chi routes the two by SEGMENT COUNT (5 vs 4), both to HandleOfferRedirect;
	// the handler reads {brand} as OPTIONAL (empty on the legacy route). Distinct
	// prefix from /track/* and /health — no route collision.
	r.Get("/o/{brand}/{subscriber}/{hash}/{campaign}", h.HandleOfferRedirect)
	r.Get("/o/{subscriber}/{hash}/{campaign}", h.HandleOfferRedirect)
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

// hashPattern bounds the smart-link hash to the alphabet the minter uses.
// Anything else is treated as a bad link and falls back to brand root — never
// a 500.
var hashPattern = regexp.MustCompile(`^[A-Za-z0-9]{6,20}$`)

// offerTokenMaxLen bounds the ?t= passthrough. 256 is far above any real
// partner id and keeps a hostile 10KB query from reaching the regexp or the
// outbound URL at all.
const offerTokenMaxLen = 256

// offerTokenPattern is the RFC 3986 UNRESERVED set (ALPHA / DIGIT / "-" / "."
// / "_" / "~") and nothing else. Deliberately NOT a percent-encode-and-hope
// filter: every character that could alter the STRUCTURE of the outbound URL
// — "&" "=" "?" "#" "/" ":" "%" "+" whitespace, control chars, anything
// non-ASCII — is absent from this set, so a value that passes is inert
// wherever it lands in the destination (query value, path segment, fragment).
var offerTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// sanitizeOfferToken is REJECT-not-escape: a token that is not wholly within
// the unreserved set (or is over-long) becomes the empty string, which renders
// {{token}} empty and still yields a valid destination. There is no partial
// acceptance and no stripping — a hostile value never reaches the outbound URL
// in any form.
func sanitizeOfferToken(s string) string {
	if s == "" || len(s) > offerTokenMaxLen {
		return ""
	}
	if !offerTokenPattern.MatchString(s) {
		return ""
	}
	return s
}

// HandleOfferRedirect resolves the scanner-safe path contract
//
//	/o/<subscriber_id>/<hash>/<campaign_id>
//
// against the in-memory smart-link dictionary and hands the visitor off to the
// offer destination. NO CLOAKING: the bot classifier only LABELS the telemetry
// event; the bytes served for a given hash are identical for every visitor
// (scanner or human). Nothing in this path can 500 or block on telemetry:
//   - a panic is recovered into a brand-root fallback redirect;
//   - a bad/missing hash or a dictionary miss falls back to https://<brand>/;
//   - telemetry is published on the Publisher's async goroutine.
func (h *Handler) HandleOfferRedirect(w http.ResponseWriter, r *http.Request) {
	// pathBrand is the OPTIONAL first /o/ segment on the brand-in-path route; it
	// is "" on the legacy 4-segment route. It is the sending brand minted from the
	// tracking domain at send time — the ONLY reliable brand signal, because our
	// CloudFront distribution strips the viewer Host (origin-request-policy
	// allExcept:[host]) before this service sees the request, so r.Host is often
	// the projectjarvis.io sentinel on real sends.
	pathBrand := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "brand")))
	hostBrand := brandRootFromHost(r.Host)

	// bestBrand is the best available SENDING brand for both the safe fallback
	// destination and the default sub2 attribution: the path brand wins when it
	// looks like a domain apex; else the Host-derived brand.
	bestBrand := hostBrand
	if pathBrand != "" && looksLikeDomainApex(pathBrand) {
		bestBrand = pathBrand
	}
	fallback := "https://" + bestBrand + "/"

	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("PANIC in HandleOfferRedirect (recovered): %v", rec)
			http.Redirect(w, r, fallback, http.StatusFound)
		}
	}()

	subscriber := chi.URLParam(r, "subscriber")
	hash := chi.URLParam(r, "hash")
	campaign := chi.URLParam(r, "campaign")

	// Opaque per-recipient passthrough for {{token}} — read from the QUERY
	// string, identically on both route shapes (4- and 5-segment), and
	// sanitized to the unreserved set. NO DB LOOKUP: the token rides the URL
	// precisely so this hot path stays lookup-free (same trade-off documented
	// for deltaSinceSend below). Absent/hostile -> "" -> {{token}} renders
	// empty and the offer is still reachable.
	token := sanitizeOfferToken(r.URL.Query().Get("t"))

	// Hash gates the lookup; a malformed hash is a dead link -> brand root.
	if !hashPattern.MatchString(hash) {
		log.Printf("OFFER bad-hash hash=%q host=%s -> fallback", hash, r.Host)
		http.Redirect(w, r, fallback, http.StatusFound)
		return
	}

	// subscriber/campaign are best-effort UUIDs — used for attribution and
	// telemetry only, so a malformed value logs but NEVER gates the redirect.
	if subscriber != "" {
		if _, err := uuid.Parse(subscriber); err != nil {
			log.Printf("OFFER malformed subscriber=%q hash=%s (proceeding)", subscriber, hash)
		}
	}
	if campaign != "" {
		if _, err := uuid.Parse(campaign); err != nil {
			log.Printf("OFFER malformed campaign=%q hash=%s (proceeding)", campaign, hash)
		}
	}

	entry, ok := h.dict.Lookup(hash)
	if !ok {
		// Miss (or empty/no-op dictionary): no dead-end, send them to the brand
		// home page and record the miss for observability.
		log.Printf("OFFER miss hash=%s host=%s -> fallback", hash, r.Host)
		http.Redirect(w, r, fallback, http.StatusFound)
		return
	}

	// Attribution brand (sub2) is the ACTUAL sending brand, resolved by precedence:
	//   (1) the {brand} PATH segment minted from the tracking domain at send time —
	//       the CloudFront-safe ground truth (the viewer Host is stripped before we
	//       see it, so it is the only reliable signal on real sends);
	//   (2) brandRootFromHost(r.Host) when the Host survived and yields a real
	//       sending brand (not the projectjarvis.io sentinel) — the pre-brand-in-path
	//       behavior, still correct for direct/uncached hits;
	//   (3) the dictionary row's brand_root as the last resort.
	// This is correct even when the dictionary row's brand_root is a DIFFERENT
	// brand: offer destinations dedup ACROSS brands (one hash is reachable from
	// several sending brands' tracking hosts), so the row that won the dedup can
	// carry the wrong sending brand — the path brand (or Host) is ground truth.
	attrBrand := bestBrand
	if attrBrand == "" || attrBrand == "projectjarvis.io" {
		if entry.BrandRoot != "" {
			attrBrand = entry.BrandRoot
		}
	}
	dest := renderOfferDestination(entry.Destination, subscriber, attrBrand, campaign, token)

	// TELEMETRY — async, LABEL ONLY. ClassifyClickAsMachine decides the label
	// but has ZERO influence on what is served below. deltaSinceSend is 0 here
	// (no send-time lookup on the redirect hot path — documented trade-off in
	// ClassifyClickAsMachine: rules 1 and 2 still apply).
	label := "human"
	if ClassifyClickAsMachine(r.UserAgent(), realIP(r), 0) {
		label = "machine"
	}
	h.pub.Publish(r.Context(), TrackingEvent{
		EventType: EventClick,
		// REQ-085b: the /o/ path contract carries brand/subscriber/hash/campaign
		// and NO org — every link already in an inbox has that shape, so the
		// contract cannot be widened retroactively. This used to publish
		// OrgID: "", which the SQS consumer parsed into uuid.Nil and wrote as
		// organization_id = '00000000-0000-0000-0000-000000000000'. Those rows
		// are invisible to every org-scoped reader, including the nightly lake
		// loader: 7,909 of 57,152 clicked rows on 2026-08-31 (13.8%), all of
		// them /o/ redirects (verified: 100% carry the renderOfferDestination
		// source_id=email signature). Started 2026-07-23, the day after this
		// handler shipped (05d5a17).
		// The org is not ambiguous — this is a single-tenant deployment and
		// every one of those rows' campaigns belongs to …0001 — so stamp the
		// resolved default here, at the producer, and let the consumer's
		// resolveOrgID stay as defense in depth.
		OrgID:        defaultOrgID.String(),
		CampaignID:   campaign,
		SubscriberID: subscriber,
		LinkURL:      dest,
		IPAddress:    realIP(r),
		UserAgent:    r.UserAgent(),
		Actor:        label,
		Timestamp:    time.Now().UTC(),
	})
	log.Printf("OFFER hit hash=%s campaign=%s subscriber=%s actor=%s risk=%s", hash, campaign, subscriber, label, entry.RiskProfile)

	// HANDOFF — one 302 for EVERYONE, every hash, no exceptions.
	//
	// This used to branch on RiskProfile: a "high" link served a bridge page
	// with a Continue button. Removed 2026-08-20 on operator ruling ("I do not
	// want this hop. I do not care about bots. This is a bad experience for the
	// human."). On a CPC offer the interstitial taxes the only event that pays —
	// the reader already clicked, and the bridge asked them to click again.
	//
	// No cloaking risk is introduced by removing it: the guarantee was never the
	// bridge itself, it was that the handoff does not vary by VISITOR. A single
	// unconditional redirect satisfies that more simply than a per-hash branch —
	// scanner and human get byte-identical treatment. ClassifyClickAsMachine
	// stays exactly where it was, labelling telemetry and changing nothing served.
	http.Redirect(w, r, dest, http.StatusFound)
}

// domainApexRe is a cheap "does this look like a bare domain apex?" gate for the
// {brand} path segment. It is intentionally NOT a registry check — a newly
// onboarded sending domain must attribute correctly without redeploying the
// tracking service — it only rejects obvious junk (no dot, path chars, spaces)
// that could poison sub2.
var domainApexRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// looksLikeDomainApex reports whether s is a plausible bare domain apex
// (lowercased, has at least one dot, only domain-safe characters, ≤253 chars).
func looksLikeDomainApex(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 253 {
		return false
	}
	return domainApexRe.MatchString(s)
}

// brandRootFromHost derives the bare brand root from the request Host by
// stripping the tracking subdomain prefixes and any port, lowercased. Used only
// for the safe fallback destination and default attribution brand.
func brandRootFromHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i] // strip port
	}
	for _, p := range []string{"t.em.", "trk.em.", "www."} {
		if strings.HasPrefix(h, p) {
			h = strings.TrimPrefix(h, p)
			break
		}
	}
	if h == "" {
		// Totally malformed host — avoid emitting "https:///". projectjarvis.io
		// is a live page, so this is still a no-dead-end fallback.
		return "projectjarvis.io"
	}
	return h
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
