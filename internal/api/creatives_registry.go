package api

// Creative registry — browse/preview/audit surface for the send-day creative
// archive (ReviewForge newsletters + converter emits). The operator-side
// tooling (mailing-saas agents/scheduling/forge.py `sync`) POSTs built
// artifacts here AFTER running them through the scheduler's real pipeline,
// so html_content is byte-identical to what a campaign deploy would carry
// (footer, sub1/sub2, preheader already injected for the given brand).
//
// Phase 2 of the ReviewForge integration (mailing-saas
// docs/REVIEW_FORGE_PHASE2_PORTAL.md). Deploys keep reading HTML from the
// brief/compiler path — this registry is read-only for the portal; the
// sha256 column exists so a later phase can prove registry/compiler parity.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

type CreativesRegistry struct {
	db *sql.DB
}

func NewCreativesRegistry(db *sql.DB) *CreativesRegistry {
	return &CreativesRegistry{db: db}
}

type creativeSyncItem struct {
	OfferKey      string `json:"offer_key"`
	BrandCode     string `json:"brand_code"`
	Filename      string `json:"filename"`
	Subject       string `json:"subject"`
	Preheader     string `json:"preheader"`
	HTMLContent   string `json:"html_content"`
	MoneyURLs     int    `json:"money_urls"`
	Tagged        bool   `json:"tagged"`
	Source        string `json:"source"` // forge | converter | manual
	ForgeBrandKey string `json:"forge_brand_key"`
	GeneratedAt   string `json:"generated_at"` // RFC3339; optional
}

// HandleSync upserts a batch of built creatives. Admin-key gated at the route
// (registered under /api/admin/), org from the standard request context.
func (cr *CreativesRegistry) HandleSync(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved: " + err.Error()})
		return
	}
	var body struct {
		Creatives []creativeSyncItem `json:"creatives"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if len(body.Creatives) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "creatives is empty"})
		return
	}

	upserted := 0
	for _, c := range body.Creatives {
		if c.Filename == "" || c.HTMLContent == "" || c.BrandCode == "" {
			respondJSON(w, http.StatusBadRequest, map[string]string{
				"error": "each creative requires filename, brand_code, html_content (offender: " + c.Filename + ")"})
			return
		}
		sum := sha256.Sum256([]byte(c.HTMLContent))
		genAt := time.Now().UTC()
		if c.GeneratedAt != "" {
			if t, perr := time.Parse(time.RFC3339, c.GeneratedAt); perr == nil {
				genAt = t
			}
		}
		source := c.Source
		if source == "" {
			source = "manual"
		}
		_, err := cr.db.ExecContext(r.Context(), `
			INSERT INTO mailing_creatives
				(organization_id, offer_key, brand_code, filename, subject, preheader,
				 html_content, money_urls, tagged, source, forge_brand_key, sha256, generated_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NOW())
			ON CONFLICT (organization_id, filename) DO UPDATE SET
				offer_key = EXCLUDED.offer_key,
				brand_code = EXCLUDED.brand_code,
				subject = EXCLUDED.subject,
				preheader = EXCLUDED.preheader,
				html_content = EXCLUDED.html_content,
				money_urls = EXCLUDED.money_urls,
				tagged = EXCLUDED.tagged,
				source = EXCLUDED.source,
				forge_brand_key = EXCLUDED.forge_brand_key,
				sha256 = EXCLUDED.sha256,
				generated_at = EXCLUDED.generated_at,
				updated_at = NOW()`,
			orgID, c.OfferKey, c.BrandCode, c.Filename, c.Subject, c.Preheader,
			c.HTMLContent, c.MoneyURLs, c.Tagged, source, c.ForgeBrandKey,
			hex.EncodeToString(sum[:]), genAt)
		if err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "upsert " + c.Filename + ": " + err.Error()})
			return
		}
		upserted++
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"upserted": upserted})
}

// HandleList returns registry metadata (no HTML bodies) newest-first,
// optionally filtered by offer/brand substring match.
func (cr *CreativesRegistry) HandleList(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, perr := strconv.Atoi(l); perr == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}
	rows, err := cr.db.QueryContext(r.Context(), `
		SELECT id, offer_key, brand_code, filename, subject, preheader,
		       money_urls, tagged, source, forge_brand_key, sha256,
		       generated_at, updated_at, length(html_content)
		FROM mailing_creatives
		WHERE organization_id = $1
		  AND ($2 = '' OR offer_key ILIKE '%' || $2 || '%')
		  AND ($3 = '' OR brand_code ILIKE $3)
		ORDER BY generated_at DESC
		LIMIT $4`,
		orgID, r.URL.Query().Get("offer"), r.URL.Query().Get("brand"), limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type creativeMeta struct {
		ID            string    `json:"id"`
		OfferKey      string    `json:"offer_key"`
		BrandCode     string    `json:"brand_code"`
		Filename      string    `json:"filename"`
		Subject       string    `json:"subject"`
		Preheader     string    `json:"preheader"`
		MoneyURLs     int       `json:"money_urls"`
		Tagged        bool      `json:"tagged"`
		Source        string    `json:"source"`
		ForgeBrandKey string    `json:"forge_brand_key"`
		SHA256        string    `json:"sha256"`
		GeneratedAt   time.Time `json:"generated_at"`
		UpdatedAt     time.Time `json:"updated_at"`
		HTMLBytes     int       `json:"html_bytes"`
	}
	out := []creativeMeta{}
	for rows.Next() {
		var m creativeMeta
		if err := rows.Scan(&m.ID, &m.OfferKey, &m.BrandCode, &m.Filename, &m.Subject,
			&m.Preheader, &m.MoneyURLs, &m.Tagged, &m.Source, &m.ForgeBrandKey,
			&m.SHA256, &m.GeneratedAt, &m.UpdatedAt, &m.HTMLBytes); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out = append(out, m)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"creatives": out, "count": len(out)})
}

// HandlePreview serves the stored HTML body for in-portal iframe preview.
func (cr *CreativesRegistry) HandlePreview(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	id := chi.URLParam(r, "id")
	var html string
	err = cr.db.QueryRowContext(r.Context(),
		`SELECT html_content FROM mailing_creatives WHERE id = $1 AND organization_id = $2`,
		id, orgID).Scan(&html)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "creative not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'unsafe-inline' https: data:")
	w.Write([]byte(html))
}
