package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// VersionSmartLink tracks handler semantics. Bumped on any externally
// observable change (response shape, status code map, branch routing).
//
// 1.0 — initial Phase 1 surface
// 1.1 — pilot test (2026-05-27): rule_fired / matched_scanner / matched_cidr
//
//	/ request_id / host / referer fields on hit telemetry; structured
//	JSON logging on every operation; diagnostics endpoint.
//
// 1.2 — risk_profile (2026-07-22): risk_profile ('low'|'high') on smart-link
//
//	rows drives the brand site's 302-vs-bridge decision; hit telemetry
//	stamps the experience served; branch whitelist widened with
//	human_bridge / human_bridge_continue.
const VersionSmartLink = "1.2"

// allowedClassifierRules is the canonical whitelist for the rule_fired
// column. Mirrors the ClassifierRule type in
// coupon-site/src/lib/bot-classifier/classify.ts — any change on one side
// MUST be mirrored on the other so the gateway and DB stay in sync.
var allowedClassifierRules = map[string]bool{
	"rule1_scanner_ua":      true,
	"rule2_bare_cloud_ip":   true,
	"rule3_bare_fast_click": true,
	"none":                  true,
	"":                      true, // legacy hits before pilot upgrade
}

// logJSON emits a single-line JSON log record. We use plain fmt.Fprintf
// against log.Default()'s writer rather than pulling in slog because
// the rest of upside-down still uses log.Printf — mixing a structured
// logger here would create grep-hostile splits. JSON-as-a-message
// keeps log scrapers happy without changing the global log surface.
func logJSON(event string, fields map[string]interface{}) {
	if fields == nil {
		fields = map[string]interface{}{}
	}
	fields["event"] = event
	fields["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(fields)
	if err != nil {
		log.Printf("[smartlink] log marshal error: %v event=%s", err, event)
		return
	}
	log.Printf("[smartlink] %s", string(b))
}

// uaPrefix returns the first N runes of a UA string for compact logging.
// Real UAs cap around 200 chars; we log the prefix so log lines stay
// scannable while preserving enough signal to identify the scanner.
func uaPrefix(ua string, n int) string {
	if len(ua) <= n {
		return ua
	}
	return ua[:n] + "…"
}

// SmartLink is the canonical row shape returned by both admin and public
// endpoints. The two sets of endpoints intentionally share the struct so
// the brand-site fetcher and the operator UI render against an identical
// schema — a divergence here would silently mask bugs the next time we
// add a column.
type SmartLink struct {
	ID               string `json:"id"`
	BrandRoot        string `json:"brand_root"`
	Slug             string `json:"slug"`
	ReviewSlug       string `json:"review_slug"`
	OfferURLTemplate string `json:"offer_url_template"`
	Status           string `json:"status"`
	RiskProfile      string `json:"risk_profile"`
	Notes            string `json:"notes,omitempty"`
	// Hash is the short, URL-safe dictionary key for the tracking-layer offer
	// link (/o/<subscriber>/<hash>/<campaign>). Minted server-side on create;
	// COALESCE(hash,'') in every SELECT so legacy rows scan as "" rather than
	// NULL-panicking the Scan.
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// slugPattern restricts smart-link slugs to lowercase ASCII, digits, and
// hyphens. This is the same shape as the existing review-slug convention
// in [coupon-site/src/app/reviews/[slug]/page.tsx] and prevents path
// traversal / injection on the brand-site catch-all route.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// validateSmartLinkPayload enforces the integrity rules that downstream
// code depends on. Returns the first violation as a user-facing error.
func validateSmartLinkPayload(brandRoot, slug, reviewSlug, offerTemplate, status, riskProfile string) error {
	if brandRoot == "" {
		return errors.New("brand_root required")
	}
	if brand.Root(brandRoot) != brandRoot {
		return fmt.Errorf("brand_root %q is not a registered brand apex; check internal/pkg/brand/brand.go OwnedDomains", brandRoot)
	}
	if !slugPattern.MatchString(slug) {
		return errors.New("slug must match ^[a-z0-9][a-z0-9-]{0,127}$ (no slashes, no uppercase, no path traversal)")
	}
	if !slugPattern.MatchString(reviewSlug) {
		return errors.New("review_slug must match the same pattern as slug")
	}
	if offerTemplate == "" {
		return errors.New("offer_url_template required")
	}
	parsed, perr := url.Parse(offerTemplate)
	if perr != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("offer_url_template must be a full https URL")
	}
	if parsed.Scheme != "https" {
		return errors.New("offer_url_template must use https")
	}
	switch status {
	case "", "active", "paused", "deleted":
	default:
		return fmt.Errorf("status must be active, paused, or deleted (got %q)", status)
	}
	// risk_profile drives the brand site's 302-vs-bridge decision. Empty is
	// allowed on input (the insert path defaults it to 'low'); anything else
	// must be an explicit member of the whitelist.
	switch riskProfile {
	case "", "low", "high":
		return nil
	}
	return fmt.Errorf("risk_profile must be low or high (got %q)", riskProfile)
}

// SmartLinkService owns persistence + the public read path. The brand-site
// gateway route holds NO secret material; the public endpoint is safe to
// expose without auth because the smart-link content (brand, slug, review
// slug, offer URL) is rendered into HTML the brand site already serves.
type SmartLinkService struct {
	db *sql.DB
}

// NewSmartLinkService wires the service against the mailing DB.
func NewSmartLinkService(db *sql.DB) *SmartLinkService {
	return &SmartLinkService{db: db}
}

// resolveActiveSmartLink is the single source of truth for the active-row
// lookup shared by the public brand-site resolve path and the proof-send
// gateway preflight. It runs the exact SELECT (status='active',
// COALESCE(risk_profile,'low'), COALESCE(notes,”)) and Scans into a
// SmartLink, returning sql.ErrNoRows when no active row exists so callers can
// distinguish "no gateway here" (404/422) from a real DB error (500).
func (s *SmartLinkService) resolveActiveSmartLink(ctx context.Context, brandRoot, slug string) (SmartLink, error) {
	var sl SmartLink
	err := s.db.QueryRowContext(ctx, `
		SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE(risk_profile,'low'), COALESCE(notes,''), COALESCE(hash,''), created_at, updated_at
		FROM mailing_smart_links
		WHERE brand_root = $1 AND slug = $2 AND status = 'active'
		LIMIT 1
	`, brandRoot, slug).Scan(&sl.ID, &sl.BrandRoot, &sl.Slug, &sl.ReviewSlug, &sl.OfferURLTemplate, &sl.Status, &sl.RiskProfile, &sl.Notes, &sl.Hash, &sl.CreatedAt, &sl.UpdatedAt)
	if err != nil {
		return SmartLink{}, err
	}
	return sl, nil
}

// resolveByHash resolves a single active smart-link by its dictionary hash —
// the reverse of the (brand_root, slug) lookup, keyed on the tracking-layer
// /o/<subscriber>/<hash>/<campaign> URL's hash segment. Returns sql.ErrNoRows
// when no active row carries the hash so callers can distinguish "unknown
// offer link" from a real DB error. The tracking service (internal/tracking)
// owns its own copy for the live redirect; this one backs the proof path and
// any admin/debug lookups inside the API binary.
func (s *SmartLinkService) resolveByHash(ctx context.Context, hash string) (SmartLink, error) {
	var sl SmartLink
	err := s.db.QueryRowContext(ctx, `
		SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE(risk_profile,'low'), COALESCE(notes,''), COALESCE(hash,''), created_at, updated_at
		FROM mailing_smart_links
		WHERE hash = $1 AND status = 'active'
		LIMIT 1
	`, hash).Scan(&sl.ID, &sl.BrandRoot, &sl.Slug, &sl.ReviewSlug, &sl.OfferURLTemplate, &sl.Status, &sl.RiskProfile, &sl.Notes, &sl.Hash, &sl.CreatedAt, &sl.UpdatedAt)
	if err != nil {
		return SmartLink{}, err
	}
	return sl, nil
}

// SmartLinkTrackingURL builds the tracking-layer offer-link URL for the new
// contract: https://<trackingDomain>/o/<subscriberID>/<hash>/<campaignID>.
// trackingDomain may arrive as a bare host (t.em.discountblog.com), a host
// with a trailing slash, or a full https URL — it is normalized to an
// https-scheme, no-trailing-slash origin before the /o/ path is appended.
func SmartLinkTrackingURL(trackingDomain, subscriberID, hash, campaignID string) string {
	origin := ensureHTTPS(trackingDomain) // "" stays "" (caller guards)
	return origin + "/o/" + subscriberID + "/" + hash + "/" + campaignID
}

// mintSmartLinkHash generates a short, URL-safe token ([a-z0-9], 10 chars)
// for a new smart-link row's dictionary key. 10 chars of 36-symbol alphabet
// ≈ 51.7 bits of entropy — collision across the ~21-row table is negligible,
// and HandleAdminCreate still retries on the unique-index conflict as a
// belt-and-suspenders. Uses crypto/rand so tokens are unguessable (the hash
// is part of a public offer-link URL).
func mintSmartLinkHash() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	const n = 10
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}

// smartLinkHashPattern restricts an operator-supplied hash to the same
// URL-safe shape mintSmartLinkHash emits, so a hand-set value can't smuggle a
// slash or other path-breaking byte into the /o/ URL.
var smartLinkHashPattern = regexp.MustCompile(`^[a-z0-9]{8,16}$`)

// ============================================================================
// PUBLIC READ ENDPOINT — called by the brand site at request time. No auth.
// ============================================================================

// HandlePublicResolve resolves a single (brand_root, slug) to its smart-link
// row, returning 404 when no active row exists. The 60s in-memory cache on
// the brand-site fetcher absorbs the call volume; this endpoint only runs
// for cache misses (~one per brand+slug per minute per ECS task).
//
// URL: GET /api/smartlinks/public/:brand_root/:slug
func (s *SmartLinkService) HandlePublicResolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionSmartLink)

	brandRoot := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "brand_root")))
	slug := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "slug")))

	// Defensive: reject malformed input BEFORE the DB query. A bad slug
	// would never match a real row, but logging and returning 400 makes
	// misconfigurations on the brand side visible instead of silently
	// 404ing.
	if brandRoot == "" || slug == "" {
		writeJSONError(w, "brand_root and slug required", http.StatusBadRequest)
		return
	}
	if !slugPattern.MatchString(slug) {
		writeJSONError(w, "slug pattern invalid", http.StatusBadRequest)
		return
	}

	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))

	sl, err := s.resolveActiveSmartLink(r.Context(), brandRoot, slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 404 is the canonical "no gateway here, fall through to
			// notFound() on the brand site" signal. The brand site
			// must NOT treat any non-404 error as 404 — that would
			// turn DB outages into silent gateway loss.
			logJSON("public_resolve_not_found", map[string]interface{}{
				"brand_root": brandRoot,
				"slug":       slug,
				"request_id": requestID,
			})
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
			return
		}
		logJSON("public_resolve_db_error", map[string]interface{}{
			"brand_root": brandRoot,
			"slug":       slug,
			"err":        err.Error(),
			"request_id": requestID,
		})
		writeJSONError(w, "internal", http.StatusInternalServerError)
		return
	}

	logJSON("public_resolve_ok", map[string]interface{}{
		"brand_root":   brandRoot,
		"slug":         slug,
		"review_slug":  sl.ReviewSlug,
		"smart_link":   sl.ID,
		"risk_profile": sl.RiskProfile,
		"request_id":   requestID,
	})

	// Public-facing cache hint. The brand site holds its own 60s in-memory
	// cache so this header mainly affects intermediate CDNs / proxies.
	w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
	respondJSON(w, http.StatusOK, sl)
}

// ============================================================================
// HIT TELEMETRY — called by the brand site once per resolved request.
// ============================================================================

// SmartLinkHitRequest is the payload the brand site posts after it
// renders a gateway response. Branch is the verdict ("bot" or "human").
// All fields beyond brand_root/slug/branch are best-effort; the brand
// site won't always know subscriber/campaign IDs (e.g. for direct hits
// where the email body URL didn't carry them).
//
// 2026-05-27 pilot additions: rule_fired / matched_scanner / matched_cidr
// expose the classifier-internal "WHICH rule caught it" answer so the
// bot-only verification campaign can be diagnosed at SQL time. request_id
// lets us correlate the brand-site log line for one request with the
// upside-down log line for the same request. host + referer give us the
// browser-facing context (which brand domain, where the click came from).
type SmartLinkHitRequest struct {
	BrandRoot      string `json:"brand_root"`
	Slug           string `json:"slug"`
	Branch         string `json:"branch"`
	RuleFired      string `json:"rule_fired,omitempty"`
	MatchedScanner string `json:"matched_scanner,omitempty"`
	MatchedCidr    string `json:"matched_cidr,omitempty"`
	UA             string `json:"ua,omitempty"`
	IP             string `json:"ip,omitempty"`
	Host           string `json:"host,omitempty"`
	Referer        string `json:"referer,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	SubscriberID   string `json:"subscriber_id,omitempty"`
	CampaignID     string `json:"campaign_id,omitempty"`
	// RiskProfile is the point-in-time stamp of which experience the hit
	// actually got ('' for legacy posters, 'low', 'high') — stored as-given,
	// NOT re-derived from mailing_smart_links, so later edits to the link's
	// risk_profile don't rewrite history.
	RiskProfile string `json:"risk_profile,omitempty"`
}

// HandleHit appends a row to mailing_smart_link_hits. Fire-and-forget
// from the brand site's perspective; we never block a recipient's click
// on the success of this write. If the DB write fails we log and return
// 202 so the brand site doesn't retry-storm us.
//
// URL: POST /api/smartlinks/hit
func (s *SmartLinkService) HandleHit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionSmartLink)

	var req SmartLinkHitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		logJSON("hit_bad_payload", map[string]interface{}{
			"err":         err.Error(),
			"remote_addr": r.RemoteAddr,
		})
		writeJSONError(w, "invalid payload", http.StatusBadRequest)
		return
	}

	req.BrandRoot = strings.ToLower(strings.TrimSpace(req.BrandRoot))
	req.Slug = strings.ToLower(strings.TrimSpace(req.Slug))
	req.Branch = strings.ToLower(strings.TrimSpace(req.Branch))
	req.RuleFired = strings.TrimSpace(req.RuleFired)
	req.MatchedScanner = strings.TrimSpace(req.MatchedScanner)
	req.MatchedCidr = strings.TrimSpace(req.MatchedCidr)
	req.Host = strings.TrimSpace(req.Host)
	req.Referer = strings.TrimSpace(req.Referer)
	req.RequestID = strings.TrimSpace(req.RequestID)
	req.RiskProfile = strings.ToLower(strings.TrimSpace(req.RiskProfile))

	if req.BrandRoot == "" || req.Slug == "" {
		writeJSONError(w, "brand_root and slug required", http.StatusBadRequest)
		return
	}
	switch req.Branch {
	case "bot", "human", "human_bridge", "human_bridge_continue":
	default:
		writeJSONError(w, "branch must be one of 'bot', 'human', 'human_bridge', 'human_bridge_continue'", http.StatusBadRequest)
		return
	}
	switch req.RiskProfile {
	case "", "low", "high":
	default:
		writeJSONError(w, "risk_profile must be '', 'low', or 'high'", http.StatusBadRequest)
		return
	}
	if !allowedClassifierRules[req.RuleFired] {
		// Tolerate but log — schema drift between Go and TS is a
		// failure mode we surfaced explicitly in classify.ts. The
		// pilot diagnostics will show "unknown_rule" rows if this
		// fires in production.
		logJSON("hit_unknown_rule", map[string]interface{}{
			"rule_fired": req.RuleFired,
			"brand_root": req.BrandRoot,
			"slug":       req.Slug,
			"branch":     req.Branch,
			"request_id": req.RequestID,
		})
		req.RuleFired = "unknown_rule"
	}

	// Cap free-form text fields to keep an abusive payload from
	// blowing up the hits table. Real values are well under these caps.
	clip := func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n]
	}
	req.UA = clip(req.UA, 1024)
	req.IP = clip(req.IP, 64)
	req.Host = clip(req.Host, 255)
	req.Referer = clip(req.Referer, 1024)
	req.RequestID = clip(req.RequestID, 128)
	req.MatchedScanner = clip(req.MatchedScanner, 64)
	req.MatchedCidr = clip(req.MatchedCidr, 64)

	// Parse optional UUIDs defensively. uuid.Parse on "" returns an
	// error and uuid.Nil; we keep them as NULL in the DB so the partial
	// indexes only cover rows with real subscriber linkage.
	var subPtr, campPtr *uuid.UUID
	if req.SubscriberID != "" {
		if id, err := uuid.Parse(req.SubscriberID); err == nil && id != uuid.Nil {
			subPtr = &id
		}
	}
	if req.CampaignID != "" {
		if id, err := uuid.Parse(req.CampaignID); err == nil && id != uuid.Nil {
			campPtr = &id
		}
	}

	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO mailing_smart_link_hits (
			brand_root, slug, branch,
			ua, ip,
			subscriber_id, campaign_id,
			rule_fired, matched_scanner, matched_cidr,
			request_id, host, referer,
			risk_profile,
			requested_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW())
	`,
		req.BrandRoot, req.Slug, req.Branch,
		req.UA, req.IP,
		subPtr, campPtr,
		req.RuleFired, req.MatchedScanner, req.MatchedCidr,
		req.RequestID, req.Host, req.Referer,
		req.RiskProfile,
	); err != nil {
		logJSON("hit_db_error", map[string]interface{}{
			"err":        err.Error(),
			"brand_root": req.BrandRoot,
			"slug":       req.Slug,
			"branch":     req.Branch,
			"rule":       req.RuleFired,
			"request_id": req.RequestID,
		})
	} else {
		logJSON("hit_recorded", map[string]interface{}{
			"brand_root":      req.BrandRoot,
			"slug":            req.Slug,
			"branch":          req.Branch,
			"risk_profile":    req.RiskProfile,
			"rule":            req.RuleFired,
			"matched_scanner": req.MatchedScanner,
			"matched_cidr":    req.MatchedCidr,
			"ua_prefix":       uaPrefix(req.UA, 80),
			"ip":              req.IP,
			"host":            req.Host,
			"subscriber_id":   req.SubscriberID,
			"campaign_id":     req.CampaignID,
			"request_id":      req.RequestID,
		})
	}

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"ok":true}`))
}

// ============================================================================
// ADMIN CRUD — operator-only via /api/mailing/smartlinks under the auth gate.
// ============================================================================

// HandleAdminList returns every smart-link row for the supplied brand
// (or all brands if brand_root is empty). Includes deleted rows so the
// admin UI can show full history; the UI filters by default.
//
// URL: GET /api/mailing/smartlinks?brand_root=<optional>&include_deleted=0|1
func (s *SmartLinkService) HandleAdminList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionSmartLink)
	brandRoot := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand_root")))
	includeDeleted, _ := strconv.ParseBool(r.URL.Query().Get("include_deleted"))

	q := `SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE(risk_profile,'low'), COALESCE(notes,''), COALESCE(hash,''), created_at, updated_at
		FROM mailing_smart_links WHERE 1=1`
	args := []interface{}{}
	idx := 1
	if brandRoot != "" {
		q += fmt.Sprintf(" AND brand_root = $%d", idx)
		args = append(args, brandRoot)
		idx++
	}
	if !includeDeleted {
		q += " AND status <> 'deleted'"
	}
	q += " ORDER BY brand_root ASC, slug ASC"

	rows, err := s.db.QueryContext(r.Context(), q, args...)
	if err != nil {
		log.Printf("[smartlink] admin list error: %v", err)
		writeJSONError(w, "internal", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []SmartLink{}
	for rows.Next() {
		var sl SmartLink
		if err := rows.Scan(&sl.ID, &sl.BrandRoot, &sl.Slug, &sl.ReviewSlug, &sl.OfferURLTemplate, &sl.Status, &sl.RiskProfile, &sl.Notes, &sl.Hash, &sl.CreatedAt, &sl.UpdatedAt); err != nil {
			log.Printf("[smartlink] admin list scan error: %v", err)
			continue
		}
		out = append(out, sl)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version":  VersionSmartLink,
		"smart_links":  out,
		"total":        len(out),
		"brand_filter": brandRoot,
	})
}

// AdminUpsertRequest is the payload for both create and update. ID is
// optional on create; if supplied the value must be a valid UUID and an
// existing row with that ID will be updated in place.
type AdminUpsertRequest struct {
	ID               string `json:"id,omitempty"`
	BrandRoot        string `json:"brand_root"`
	Slug             string `json:"slug"`
	ReviewSlug       string `json:"review_slug"`
	OfferURLTemplate string `json:"offer_url_template"`
	Status           string `json:"status,omitempty"`
	RiskProfile      string `json:"risk_profile,omitempty"`
	Notes            string `json:"notes,omitempty"`
	// Hash is optional on create: when empty the server mints one; when
	// supplied it must match smartLinkHashPattern.
	Hash string `json:"hash,omitempty"`
}

// HandleAdminCreate creates a smart-link row. Status defaults to 'active'.
// URL: POST /api/mailing/smartlinks
func (s *SmartLinkService) HandleAdminCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionSmartLink)

	var req AdminUpsertRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeJSONError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	req.BrandRoot = strings.ToLower(strings.TrimSpace(req.BrandRoot))
	req.Slug = strings.ToLower(strings.TrimSpace(req.Slug))
	req.ReviewSlug = strings.ToLower(strings.TrimSpace(req.ReviewSlug))
	req.OfferURLTemplate = strings.TrimSpace(req.OfferURLTemplate)
	req.RiskProfile = strings.ToLower(strings.TrimSpace(req.RiskProfile))
	req.Hash = strings.ToLower(strings.TrimSpace(req.Hash))
	if req.Status == "" {
		req.Status = "active"
	}
	if err := validateSmartLinkPayload(req.BrandRoot, req.Slug, req.ReviewSlug, req.OfferURLTemplate, req.Status, req.RiskProfile); err != nil {
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.RiskProfile == "" {
		req.RiskProfile = "low"
	}
	// A caller-supplied hash must be URL-safe; an empty one is minted below.
	suppliedHash := req.Hash != ""
	if suppliedHash && !smartLinkHashPattern.MatchString(req.Hash) {
		writeJSONError(w, "hash must match ^[a-z0-9]{8,16}$", http.StatusBadRequest)
		return
	}

	// Insert, minting a fresh hash on each attempt when none was supplied. The
	// unique index idx_mailing_smart_links_hash can (astronomically rarely)
	// reject a minted token; we retry a handful of times before giving up. A
	// (brand_root, slug) collision is terminal — we surface it as 409 and do
	// NOT retry (retrying would loop forever on the same conflict).
	var sl SmartLink
	var err error
	const maxHashAttempts = 5
	for attempt := 0; attempt < maxHashAttempts; attempt++ {
		hash := req.Hash
		if !suppliedHash {
			hash, err = mintSmartLinkHash()
			if err != nil {
				logJSON("admin_create_hash_mint_error", map[string]interface{}{
					"brand_root": req.BrandRoot, "slug": req.Slug, "err": err.Error(),
				})
				writeJSONError(w, "internal", http.StatusInternalServerError)
				return
			}
		}
		err = s.db.QueryRowContext(r.Context(), `
			INSERT INTO mailing_smart_links (brand_root, slug, review_slug, offer_url_template, status, risk_profile, notes, hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id, brand_root, slug, review_slug, offer_url_template, status, COALESCE(risk_profile,'low'), COALESCE(notes,''), COALESCE(hash,''), created_at, updated_at
		`, req.BrandRoot, req.Slug, req.ReviewSlug, req.OfferURLTemplate, req.Status, req.RiskProfile, req.Notes, hash).Scan(
			&sl.ID, &sl.BrandRoot, &sl.Slug, &sl.ReviewSlug, &sl.OfferURLTemplate, &sl.Status, &sl.RiskProfile, &sl.Notes, &sl.Hash, &sl.CreatedAt, &sl.UpdatedAt,
		)
		if err == nil {
			break
		}
		// A hash-index collision on a MINTED token → retry with a new mint.
		if !suppliedHash && strings.Contains(err.Error(), "idx_mailing_smart_links_hash") {
			logJSON("admin_create_hash_retry", map[string]interface{}{
				"brand_root": req.BrandRoot, "slug": req.Slug, "attempt": attempt,
			})
			continue
		}
		break
	}
	if err != nil {
		if strings.Contains(err.Error(), "idx_mailing_smart_links_hash") {
			// Either a supplied-hash collision or exhausted mint retries.
			logJSON("admin_create_hash_conflict", map[string]interface{}{
				"brand_root": req.BrandRoot, "slug": req.Slug, "supplied": suppliedHash,
			})
			writeJSONError(w, "hash already in use — omit it to mint a fresh one", http.StatusConflict)
			return
		}
		if strings.Contains(err.Error(), "duplicate key") {
			logJSON("admin_create_conflict", map[string]interface{}{
				"brand_root": req.BrandRoot,
				"slug":       req.Slug,
			})
			writeJSONError(w, fmt.Sprintf("smart link (brand=%s, slug=%s) already exists", req.BrandRoot, req.Slug), http.StatusConflict)
			return
		}
		logJSON("admin_create_error", map[string]interface{}{
			"brand_root": req.BrandRoot,
			"slug":       req.Slug,
			"err":        err.Error(),
		})
		writeJSONError(w, "internal", http.StatusInternalServerError)
		return
	}
	logJSON("admin_create_ok", map[string]interface{}{
		"id":           sl.ID,
		"brand_root":   sl.BrandRoot,
		"slug":         sl.Slug,
		"review_slug":  sl.ReviewSlug,
		"status":       sl.Status,
		"risk_profile": sl.RiskProfile,
		"hash":         sl.Hash,
	})
	respondJSON(w, http.StatusCreated, sl)
}

// HandleAdminUpdate updates an existing smart-link row by ID. Status
// transitions (active <-> paused <-> deleted) are allowed; immutable
// fields (brand_root, slug) are not — operator must create a new row
// rather than rename. This is intentional because the slug is the
// public URL piece, and renames in place would break inbound campaigns.
//
// URL: PATCH /api/mailing/smartlinks/:id
func (s *SmartLinkService) HandleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionSmartLink)
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil || id == uuid.Nil {
		writeJSONError(w, "invalid id", http.StatusBadRequest)
		return
	}

	var req AdminUpsertRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeJSONError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	req.ReviewSlug = strings.ToLower(strings.TrimSpace(req.ReviewSlug))
	req.OfferURLTemplate = strings.TrimSpace(req.OfferURLTemplate)
	req.RiskProfile = strings.ToLower(strings.TrimSpace(req.RiskProfile))

	// Read current row so we can apply a partial update.
	var current SmartLink
	if err := s.db.QueryRowContext(r.Context(), `
		SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE(risk_profile,'low'), COALESCE(notes,''), COALESCE(hash,''), created_at, updated_at
		FROM mailing_smart_links WHERE id = $1
	`, id).Scan(&current.ID, &current.BrandRoot, &current.Slug, &current.ReviewSlug, &current.OfferURLTemplate, &current.Status, &current.RiskProfile, &current.Notes, &current.Hash, &current.CreatedAt, &current.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("[smartlink] admin update fetch error: %v", err)
		writeJSONError(w, "internal", http.StatusInternalServerError)
		return
	}

	if req.ReviewSlug != "" {
		current.ReviewSlug = req.ReviewSlug
	}
	if req.OfferURLTemplate != "" {
		current.OfferURLTemplate = req.OfferURLTemplate
	}
	if req.Status != "" {
		current.Status = req.Status
	}
	if req.RiskProfile != "" {
		current.RiskProfile = req.RiskProfile
	}
	if req.Notes != "" {
		current.Notes = req.Notes
	}

	if err := validateSmartLinkPayload(current.BrandRoot, current.Slug, current.ReviewSlug, current.OfferURLTemplate, current.Status, current.RiskProfile); err != nil {
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE mailing_smart_links
		SET review_slug = $2, offer_url_template = $3, status = $4, risk_profile = $5, notes = $6, updated_at = NOW()
		WHERE id = $1
	`, id, current.ReviewSlug, current.OfferURLTemplate, current.Status, current.RiskProfile, current.Notes); err != nil {
		logJSON("admin_update_error", map[string]interface{}{
			"id":  id.String(),
			"err": err.Error(),
		})
		writeJSONError(w, "internal", http.StatusInternalServerError)
		return
	}
	logJSON("admin_update_ok", map[string]interface{}{
		"id":           current.ID,
		"brand_root":   current.BrandRoot,
		"slug":         current.Slug,
		"review_slug":  current.ReviewSlug,
		"status":       current.Status,
		"risk_profile": current.RiskProfile,
	})

	respondJSON(w, http.StatusOK, current)
}

// ============================================================================
// DIAGNOSTICS — admin-only at-a-glance health for the bot-only pilot.
// ============================================================================

// DiagnosticsResponse is the structured payload returned to the admin
// UI / operator curl. Designed so a single GET answers "is the gateway
// working?" without joining tables by hand.
type DiagnosticsResponse struct {
	APIVersion  string             `json:"api_version"`
	GeneratedAt time.Time          `json:"generated_at"`
	WindowHours int                `json:"window_hours"`
	BrandRoot   string             `json:"brand_root,omitempty"`
	Slug        string             `json:"slug,omitempty"`
	TotalHits   int                `json:"total_hits"`
	ByBranch    map[string]int     `json:"by_branch"`
	ByRule      map[string]int     `json:"by_rule"`
	TopScanners []DiagnosticBucket `json:"top_scanners"`
	TopUAPrefix []DiagnosticBucket `json:"top_ua_prefix"`
	TopCidrs    []DiagnosticBucket `json:"top_cidrs"`
	RecentHits  []DiagnosticHit    `json:"recent_hits"`
}

type DiagnosticBucket struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

type DiagnosticHit struct {
	BrandRoot      string    `json:"brand_root"`
	Slug           string    `json:"slug"`
	Branch         string    `json:"branch"`
	RuleFired      string    `json:"rule_fired"`
	MatchedScanner string    `json:"matched_scanner"`
	MatchedCidr    string    `json:"matched_cidr"`
	UAPrefix       string    `json:"ua_prefix"`
	IP             string    `json:"ip"`
	RequestID      string    `json:"request_id"`
	RequestedAt    time.Time `json:"requested_at"`
}

// HandleAdminDiagnostics returns a structured health snapshot for the
// supplied brand+slug (or all if omitted), defaulting to a 24-hour window.
// This is the single most useful query for verifying the bot-only test
// campaign hit the gateway as expected.
//
// URL: GET /api/mailing/smartlinks/diagnostics?brand_root=&slug=&hours=24
func (s *SmartLinkService) HandleAdminDiagnostics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionSmartLink)
	brandRoot := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand_root")))
	slug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("slug")))
	hoursStr := r.URL.Query().Get("hours")
	hours, _ := strconv.Atoi(hoursStr)
	if hours <= 0 || hours > 24*30 {
		hours = 24
	}

	args := []interface{}{hours}
	where := "requested_at > NOW() - ($1 || ' hours')::interval"
	if brandRoot != "" {
		args = append(args, brandRoot)
		where += fmt.Sprintf(" AND brand_root = $%d", len(args))
	}
	if slug != "" {
		args = append(args, slug)
		where += fmt.Sprintf(" AND slug = $%d", len(args))
	}

	resp := DiagnosticsResponse{
		APIVersion:  VersionSmartLink,
		GeneratedAt: time.Now().UTC(),
		WindowHours: hours,
		BrandRoot:   brandRoot,
		Slug:        slug,
		ByBranch:    map[string]int{},
		ByRule:      map[string]int{},
		TopScanners: []DiagnosticBucket{},
		TopUAPrefix: []DiagnosticBucket{},
		TopCidrs:    []DiagnosticBucket{},
		RecentHits:  []DiagnosticHit{},
	}

	// 1. by_branch
	if rows, err := s.db.QueryContext(r.Context(),
		`SELECT branch, COUNT(*) FROM mailing_smart_link_hits WHERE `+where+` GROUP BY branch`,
		args...); err == nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err == nil {
				resp.ByBranch[k] = n
				resp.TotalHits += n
			}
		}
	} else {
		logJSON("diagnostics_by_branch_err", map[string]interface{}{"err": err.Error()})
	}

	// 2. by_rule
	if rows, err := s.db.QueryContext(r.Context(),
		`SELECT COALESCE(rule_fired,''), COUNT(*) FROM mailing_smart_link_hits WHERE `+where+` GROUP BY rule_fired`,
		args...); err == nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err == nil {
				if k == "" {
					k = "(empty)"
				}
				resp.ByRule[k] = n
			}
		}
	}

	// 3. top_scanners
	if rows, err := s.db.QueryContext(r.Context(),
		`SELECT matched_scanner, COUNT(*) AS c FROM mailing_smart_link_hits
		 WHERE `+where+` AND matched_scanner <> '' GROUP BY matched_scanner ORDER BY c DESC LIMIT 20`,
		args...); err == nil {
		defer rows.Close()
		for rows.Next() {
			var b DiagnosticBucket
			if err := rows.Scan(&b.Key, &b.Count); err == nil {
				resp.TopScanners = append(resp.TopScanners, b)
			}
		}
	}

	// 4. top_ua_prefix (first 60 chars of UA)
	if rows, err := s.db.QueryContext(r.Context(),
		`SELECT LEFT(COALESCE(ua,''), 60) AS p, COUNT(*) AS c FROM mailing_smart_link_hits
		 WHERE `+where+` GROUP BY p ORDER BY c DESC LIMIT 20`,
		args...); err == nil {
		defer rows.Close()
		for rows.Next() {
			var b DiagnosticBucket
			if err := rows.Scan(&b.Key, &b.Count); err == nil {
				resp.TopUAPrefix = append(resp.TopUAPrefix, b)
			}
		}
	}

	// 5. top_cidrs
	if rows, err := s.db.QueryContext(r.Context(),
		`SELECT matched_cidr, COUNT(*) AS c FROM mailing_smart_link_hits
		 WHERE `+where+` AND matched_cidr <> '' GROUP BY matched_cidr ORDER BY c DESC LIMIT 20`,
		args...); err == nil {
		defer rows.Close()
		for rows.Next() {
			var b DiagnosticBucket
			if err := rows.Scan(&b.Key, &b.Count); err == nil {
				resp.TopCidrs = append(resp.TopCidrs, b)
			}
		}
	}

	// 6. recent_hits (newest 50)
	if rows, err := s.db.QueryContext(r.Context(),
		`SELECT brand_root, slug, branch,
				COALESCE(rule_fired,''), COALESCE(matched_scanner,''), COALESCE(matched_cidr,''),
				LEFT(COALESCE(ua,''), 80), COALESCE(ip,''),
				COALESCE(request_id,''), requested_at
		 FROM mailing_smart_link_hits
		 WHERE `+where+` ORDER BY requested_at DESC LIMIT 50`,
		args...); err == nil {
		defer rows.Close()
		for rows.Next() {
			var h DiagnosticHit
			if err := rows.Scan(&h.BrandRoot, &h.Slug, &h.Branch, &h.RuleFired,
				&h.MatchedScanner, &h.MatchedCidr, &h.UAPrefix, &h.IP, &h.RequestID, &h.RequestedAt); err == nil {
				resp.RecentHits = append(resp.RecentHits, h)
			}
		}
	}

	respondJSON(w, http.StatusOK, resp)
}

// HandleAdminDelete soft-deletes (sets status='deleted'). Hard delete is
// intentionally not exposed — historical hits in mailing_smart_link_hits
// reference brand/slug as text columns, and a hard delete would break
// the operator's ability to look back at a previously-active gateway.
//
// URL: DELETE /api/mailing/smartlinks/:id
func (s *SmartLinkService) HandleAdminDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Api-Version", VersionSmartLink)
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil || id == uuid.Nil {
		writeJSONError(w, "invalid id", http.StatusBadRequest)
		return
	}
	res, err := s.db.ExecContext(r.Context(), `
		UPDATE mailing_smart_links SET status = 'deleted', updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted'
	`, id)
	if err != nil {
		logJSON("admin_delete_error", map[string]interface{}{
			"id":  idStr,
			"err": err.Error(),
		})
		writeJSONError(w, "internal", http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	logJSON("admin_delete_ok", map[string]interface{}{
		"id":      idStr,
		"deleted": n > 0,
	})
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deleted": n > 0,
		"id":      idStr,
	})
}
