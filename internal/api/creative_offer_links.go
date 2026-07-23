package api

// Creative Studio offer-link mapping report.
//
// GET /api/mailing/creatives/{id}/offer-links
//
// Detects the money-links in a creative's stored HTML and reports, for each
// DISTINCT destination, whether it is already mapped to a tracking-layer /o/
// hash — so the operator can confirm the mapped ones and create maps for the
// rest, in-app, without leaving Creative Studio.
//
// CRITICAL INVARIANT: "mapped" is decided with moneylink.Normalize — the EXACT
// same normalizer the send worker (internal/worker/smartlink_emitter.go) uses
// to mint /o/ hashes, and the same money-host regex (moneylink.HrefRe) the API
// rewriter uses. So this report can never disagree with what actually happens
// on the send path: if it says "mapped: true, hash: X", the worker will swap
// that rendered href for hash X at send time.
//
// The CREATE action is the EXISTING POST /api/mailing/smartlinks
// (HandleAdminCreate mints the hash); the frontend calls that on confirm. This
// endpoint is read-only.

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/pkg/moneylink"
)

// offerLinkEntry is one detected money-link destination + its mapping status.
type offerLinkEntry struct {
	// URL is the money href exactly as found in the creative (query intact).
	URL string `json:"url"`
	// Host is the money-network host (www. stripped, lowercased).
	Host string `json:"host"`
	// Normalized is the moneylink.Normalize key both this report and the send
	// worker's /o/ minter agree on. Dedup is by this value.
	Normalized string `json:"normalized"`
	// Mapped is true when an active, hash-bearing smart-link row exists whose
	// offer_url_template normalizes to Normalized.
	Mapped bool `json:"mapped"`
	// Hash / Slug / RiskProfile are the mapped row's fields (present only when
	// Mapped). Hash is the /o/ dictionary key.
	Hash        string `json:"hash,omitempty"`
	Slug        string `json:"slug,omitempty"`
	RiskProfile string `json:"risk_profile,omitempty"`
	// SuggestedSlug is a slugPattern-valid seed the operator can accept or
	// override when creating a map (present only when NOT Mapped).
	SuggestedSlug string `json:"suggested_slug,omitempty"`
}

// mappedSmartLink is the subset of an active smart-link row this report needs.
type mappedSmartLink struct {
	Hash        string
	Slug        string
	RiskProfile string
}

// HandleCreativeOfferLinks reports the money-link mapping status of a creative.
// Org-scoped (same as HandleCreativeProof); read-only.
func (h *ProofSendHandler) HandleCreativeOfferLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved: " + err.Error()})
		return
	}
	creativeID := chi.URLParam(r, "id")
	if creativeID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "missing creative id"})
		return
	}

	// Org-scoped creative load — same query shape as HandleCreativeProof.
	var htmlContent string
	err = h.db.QueryRowContext(ctx,
		`SELECT COALESCE(html_content,'') FROM mailing_creatives WHERE id = $1 AND organization_id = $2`,
		creativeID, orgID.String(),
	).Scan(&htmlContent)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "creative not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Active, hash-bearing smart links keyed by the SAME normalization the send
	// worker mints with. This is the whole point of the endpoint.
	mappedByNorm, err := h.loadMappedOfferLinks(ctx)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	entries := detectOfferLinks(htmlContent, mappedByNorm)
	respondJSON(w, http.StatusOK, map[string]interface{}{"offer_links": entries})
}

// loadMappedOfferLinks reads the active, hash-bearing smart links into a map
// keyed by moneylink.Normalize(offer_url_template) — mirroring the send
// worker's queryDB (internal/worker/smartlink_emitter.go) so this report and
// the /o/ minter agree row-for-row. Two active rows normalizing to the same
// destination is a data condition (should be prevented upstream);
// last-writer-wins here, deterministic within a single load.
func (h *ProofSendHandler) loadMappedOfferLinks(ctx context.Context) (map[string]mappedSmartLink, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT hash, offer_url_template, slug, COALESCE(risk_profile,'low')
		 FROM mailing_smart_links
		 WHERE hash IS NOT NULL AND status='active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]mappedSmartLink{}
	for rows.Next() {
		var hash, slug, risk string
		var tmpl sql.NullString
		if err := rows.Scan(&hash, &tmpl, &slug, &risk); err != nil {
			return nil, err
		}
		hash = strings.TrimSpace(hash)
		key := moneylink.Normalize(tmpl.String)
		if hash == "" || key == "" {
			continue // unusable row — skip, don't fail the whole report
		}
		out[key] = mappedSmartLink{Hash: hash, Slug: slug, RiskProfile: risk}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// detectOfferLinks finds every money href in html, dedups by normalized
// destination, and joins each against the mapped map. Applies the same
// /integration/ exclusion the rewriter does (the postback path is never a
// clickable offer link). Always returns a non-nil slice so the JSON is
// "offer_links":[] rather than null when there are no money links.
func detectOfferLinks(html string, mapped map[string]mappedSmartLink) []offerLinkEntry {
	entries := []offerLinkEntry{}
	seen := map[string]bool{}
	re := moneylink.HrefRe()
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		if len(m) < 3 {
			continue
		}
		fullURL := m[1]
		path := m[2]
		// Same /integration/ exclusion as RewriteMoneyLinksToTracking: the
		// postback path is not a money link, and RE2 can't express the
		// negative lookahead so we reject it here.
		if strings.HasPrefix(strings.ToLower(path), "integration/") {
			continue
		}
		norm := moneylink.Normalize(fullURL)
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true

		host := moneyHostOf(fullURL)
		e := offerLinkEntry{URL: fullURL, Host: host, Normalized: norm}
		if sl, ok := mapped[norm]; ok {
			e.Mapped = true
			e.Hash = sl.Hash
			e.Slug = sl.Slug
			e.RiskProfile = sl.RiskProfile
		} else {
			e.SuggestedSlug = deriveSuggestedSlug(fullURL, host)
		}
		entries = append(entries, e)
	}
	return entries
}

// moneyHostOf extracts the lowercased host (www. stripped) from a money URL.
func moneyHostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Host), "www.")
}

// deriveSuggestedSlug proposes a slugPattern-valid slug for an unmapped
// destination: the last non-empty path segment (lowercased, non-alnum runs
// collapsed to hyphens), falling back to the money host's first label. The
// operator can override it in the create form. Returns "" if nothing valid can
// be derived (the frontend then requires an operator-typed slug).
func deriveSuggestedSlug(rawURL, host string) string {
	var seg string
	if u, err := url.Parse(rawURL); err == nil {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i := len(parts) - 1; i >= 0; i-- {
			if p := strings.TrimSpace(parts[i]); p != "" {
				seg = p
				break
			}
		}
	}
	if slug := slugifyCandidate(seg); slug != "" {
		return slug
	}
	// Fall back to the money host's first label (e.g. cratoolpro.com -> cratoolpro).
	h := strings.TrimPrefix(strings.ToLower(host), "www.")
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return slugifyCandidate(h)
}

// slugifyCandidate lowercases s, collapses each non-[a-z0-9] run to a single
// hyphen, trims leading/trailing hyphens, caps at slugPattern's 128-char limit,
// and returns "" if the result is not slugPattern-valid.
func slugifyCandidate(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		if b.Len() > 0 && !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 128 {
		out = strings.Trim(out[:128], "-")
	}
	if out == "" || !slugPattern.MatchString(out) {
		return ""
	}
	return out
}
