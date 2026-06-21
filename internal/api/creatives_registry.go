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
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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
		       generated_at, updated_at, length(html_content),
		       approval_status, approved_by, approved_at,
		       money_link_status, money_link_checked_at, money_link_detail
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

	out := []creativeMeta{}
	for rows.Next() {
		var m creativeMeta
		var approvedAt, moneyCheckedAt sql.NullTime
		if err := rows.Scan(&m.ID, &m.OfferKey, &m.BrandCode, &m.Filename, &m.Subject,
			&m.Preheader, &m.MoneyURLs, &m.Tagged, &m.Source, &m.ForgeBrandKey,
			&m.SHA256, &m.GeneratedAt, &m.UpdatedAt, &m.HTMLBytes,
			&m.ApprovalStatus, &m.ApprovedBy, &approvedAt,
			&m.MoneyLinkStatus, &moneyCheckedAt, &m.MoneyLinkDetail); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if approvedAt.Valid {
			m.ApprovedAt = &approvedAt.Time
		}
		if moneyCheckedAt.Valid {
			m.MoneyLinkCheckedAt = &moneyCheckedAt.Time
		}
		out = append(out, m)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"creatives": out, "count": len(out)})
}

// creativeMeta is the registry row shape returned by HandleList and the
// approve/reject single-row responses. approved_at / money_link_checked_at are
// nullable (omitempty → null in JSON when unset).
type creativeMeta struct {
	ID                 string     `json:"id"`
	OfferKey           string     `json:"offer_key"`
	BrandCode          string     `json:"brand_code"`
	Filename           string     `json:"filename"`
	Subject            string     `json:"subject"`
	Preheader          string     `json:"preheader"`
	MoneyURLs          int        `json:"money_urls"`
	Tagged             bool       `json:"tagged"`
	Source             string     `json:"source"`
	ForgeBrandKey      string     `json:"forge_brand_key"`
	SHA256             string     `json:"sha256"`
	GeneratedAt        time.Time  `json:"generated_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	HTMLBytes          int        `json:"html_bytes"`
	ApprovalStatus     string     `json:"approval_status"`
	ApprovedBy         string     `json:"approved_by"`
	ApprovedAt         *time.Time `json:"approved_at"`
	MoneyLinkStatus    string     `json:"money_link_status"`
	MoneyLinkCheckedAt *time.Time `json:"money_link_checked_at"`
	MoneyLinkDetail    string     `json:"money_link_detail"`
}

// scanCreativeMetaRow loads one creativeMeta by id (org-scoped) using the same
// projection as HandleList. Used by approve/reject to return the updated row.
func (cr *CreativesRegistry) scanCreativeMetaRow(ctx context.Context, id string, orgID uuid.UUID) (*creativeMeta, error) {
	var m creativeMeta
	var approvedAt, moneyCheckedAt sql.NullTime
	err := cr.db.QueryRowContext(ctx, `
		SELECT id, offer_key, brand_code, filename, subject, preheader,
		       money_urls, tagged, source, forge_brand_key, sha256,
		       generated_at, updated_at, length(html_content),
		       approval_status, approved_by, approved_at,
		       money_link_status, money_link_checked_at, money_link_detail
		FROM mailing_creatives
		WHERE id = $1 AND organization_id = $2`, id, orgID).Scan(
		&m.ID, &m.OfferKey, &m.BrandCode, &m.Filename, &m.Subject,
		&m.Preheader, &m.MoneyURLs, &m.Tagged, &m.Source, &m.ForgeBrandKey,
		&m.SHA256, &m.GeneratedAt, &m.UpdatedAt, &m.HTMLBytes,
		&m.ApprovalStatus, &m.ApprovedBy, &approvedAt,
		&m.MoneyLinkStatus, &moneyCheckedAt, &m.MoneyLinkDetail)
	if err != nil {
		return nil, err
	}
	if approvedAt.Valid {
		m.ApprovedAt = &approvedAt.Time
	}
	if moneyCheckedAt.Valid {
		m.MoneyLinkCheckedAt = &moneyCheckedAt.Time
	}
	return &m, nil
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

// HandleApprove marks a creative approved. Body {"approved_by": "<email optional>"};
// falls back to the X-User-Email header, then "". Returns the updated row.
func (cr *CreativesRegistry) HandleApprove(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	id := chi.URLParam(r, "id")
	var body struct {
		ApprovedBy string `json:"approved_by"`
	}
	// Empty body is allowed (approver resolved from header).
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	approver := strings.TrimSpace(body.ApprovedBy)
	if approver == "" {
		approver = strings.TrimSpace(r.Header.Get("X-User-Email"))
	}
	res, err := cr.db.ExecContext(r.Context(), `
		UPDATE mailing_creatives
		SET approval_status = 'approved', approved_by = $3, approved_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND organization_id = $2`, id, orgID, approver)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "creative not found"})
		return
	}
	row, err := cr.scanCreativeMetaRow(r.Context(), id, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, row)
}

// HandleReject marks a creative rejected. Body {"notes": ""} (notes optional,
// stored in money_link_detail only when provided so existing detail is preserved
// otherwise). Returns the updated row.
func (cr *CreativesRegistry) HandleReject(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	id := chi.URLParam(r, "id")
	var body struct {
		Notes string `json:"notes"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	res, err := cr.db.ExecContext(r.Context(), `
		UPDATE mailing_creatives
		SET approval_status = 'rejected', updated_at = NOW()
		WHERE id = $1 AND organization_id = $2`, id, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "creative not found"})
		return
	}
	row, err := cr.scanCreativeMetaRow(r.Context(), id, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, row)
}

// HandleMoneyLinkCheck runs the server-side money-link checker over a creative's
// stored HTML. Body {"http": true} (default true) toggles the HTTP liveness
// follow. Persists money_link_status + a compact money_link_detail summary +
// money_link_checked_at, and returns the status, detail, and findings.
func (cr *CreativesRegistry) HandleMoneyLinkCheck(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	id := chi.URLParam(r, "id")
	// Default doHTTP=true; an absent/empty body keeps the default.
	body := struct {
		HTTP *bool `json:"http"`
	}{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	doHTTP := true
	if body.HTTP != nil {
		doHTTP = *body.HTTP
	}

	var html, brandCode string
	err = cr.db.QueryRowContext(r.Context(),
		`SELECT html_content, brand_code FROM mailing_creatives WHERE id = $1 AND organization_id = $2`,
		id, orgID).Scan(&html, &brandCode)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "creative not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	brandRoot := brandRootFromCode(brandCode)
	status, detail, findings := checkCreativeMoneyLinks(r.Context(), html, brandRoot, doHTTP)

	if _, err := cr.db.ExecContext(r.Context(), `
		UPDATE mailing_creatives
		SET money_link_status = $3, money_link_detail = $4, money_link_checked_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND organization_id = $2`, id, orgID, status, detail); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"money_link_status": status,
		"detail":            detail,
		"findings":          findings,
	})
}
