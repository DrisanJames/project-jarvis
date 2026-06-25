package api

// Offer Proofs — the Creative Studio "Offers" sub-view backend.
//
// The operator uploads a network-approved HTML creative (usually image-heavy).
// On create we REHOST its images to our CDN (reusing ImageCDNHandlers.RehostHTML)
// and append our brand footer/unsubscribe block (appendUnsubDisclaimer), then store the
// proof in a manual approval lifecycle. The operator emails the proof to selected
// account managers through a chosen sending domain (reusing the proof-send
// machinery), then manually approves it — recording the approved sending domains,
// approved ISPs, and the subject+preheader / from-name variants it may mail with.
//
// v1 is a REGISTRY: it records the approval matrix and the send-day can read it.
// Nothing here gates HandleDeployCampaign — enforcement is a later, separately
// gated phase.
//
// Routes (all org-scoped via GetOrgIDFromRequest):
//   GET    /api/mailing/offer-proofs            list (no html_content)
//   POST   /api/mailing/offer-proofs            create (rehost + footer-inject)
//   GET    /api/mailing/offer-proofs/isps       canonical ISP picklist
//   GET    /api/mailing/offer-proofs/{id}       get (with html_content)
//   GET    /api/mailing/offer-proofs/{id}/preview   rendered html
//   PATCH  /api/mailing/offer-proofs/{id}       partial update
//   POST   /api/mailing/offer-proofs/{id}/approve   approve + set domains/ISPs/variants
//   POST   /api/mailing/offer-proofs/{id}/reject    reject
//   POST   /api/mailing/offer-proofs/{id}/send       email proof to recipients
//   POST   /api/mailing/offer-proofs/bulk        bulk delete/activate/deactivate/approve/reject
//   GET/POST/PATCH/DELETE /api/mailing/proof-recipients[/{id}]  account-manager CRUD

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/lib/pq"
)

// proofSendMaxRecipients caps a single proof send. Recipients only ever come
// from the curated mailing_proof_recipients (account managers), never from a
// subscriber list — this cap is a belt-and-suspenders guard against fan-out.
const proofSendMaxRecipients = 50

type OfferProofsService struct {
	db          *sql.DB
	proofSender *ProofSendHandler
	imageCDN    *ImageCDNHandlers // nil-safe: rehost is skipped when unset / S3 unconfigured
}

func NewOfferProofsService(db *sql.DB, imageCDN *ImageCDNHandlers) *OfferProofsService {
	return &OfferProofsService{
		db:          db,
		proofSender: NewProofSendHandler(db),
		imageCDN:    imageCDN,
	}
}

type proofVariant struct {
	Subject   string `json:"subject"`
	Preheader string `json:"preheader"`
}

type offerProof struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	OfferKey        string         `json:"offer_key"`
	HTMLContent     string         `json:"html_content,omitempty"`
	ImagesRehosted  int            `json:"images_rehosted"`
	ApprovalStatus  string         `json:"approval_status"`
	IsActive        bool           `json:"is_active"`
	ApprovedBy      string         `json:"approved_by"`
	ApprovedAt      *time.Time     `json:"approved_at,omitempty"`
	Variants        []proofVariant `json:"variants"`
	FromNames       []string       `json:"from_names"`
	ApprovedDomains []string       `json:"approved_domains"`
	ApprovedISPs    []string       `json:"approved_isps"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// ── helpers ────────────────────────────────────────────────────────────────

func marshalJSONArray(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 || string(b) == "null" {
		return "[]"
	}
	return string(b)
}

func unmarshalVariants(b []byte) []proofVariant {
	out := []proofVariant{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	if out == nil {
		out = []proofVariant{}
	}
	return out
}

func unmarshalStrArray(b []byte) []string {
	out := []string{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// cleanStrings trims, drops empties, and de-dupes (case-preserving, first wins).
func cleanStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}

// validISPSet is the canonical lowercase ISP set from the engine.
func validISPSet() map[string]bool {
	m := map[string]bool{}
	for _, isp := range engine.AllISPs() {
		m[strings.ToLower(string(isp))] = true
	}
	return m
}

// scanProof reads a full proof row (with html). cols order is fixed.
func scanProof(rows interface {
	Scan(dest ...interface{}) error
}, includeHTML bool) (offerProof, error) {
	var p offerProof
	var html, approvedBy, offerKey string
	var approvedAt sql.NullTime
	var variantsB, fromNamesB, domainsB, ispsB []byte
	err := rows.Scan(&p.ID, &p.Name, &offerKey, &html, &p.ImagesRehosted,
		&p.ApprovalStatus, &p.IsActive, &approvedBy, &approvedAt,
		&variantsB, &fromNamesB, &domainsB, &ispsB, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.OfferKey = offerKey
	p.ApprovedBy = approvedBy
	if approvedAt.Valid {
		t := approvedAt.Time
		p.ApprovedAt = &t
	}
	p.Variants = unmarshalVariants(variantsB)
	p.FromNames = unmarshalStrArray(fromNamesB)
	p.ApprovedDomains = unmarshalStrArray(domainsB)
	p.ApprovedISPs = unmarshalStrArray(ispsB)
	if includeHTML {
		p.HTMLContent = html
	}
	return p, nil
}

const proofSelectCols = `id::text, name, offer_key, html_content, images_rehosted,
	approval_status, is_active, approved_by, approved_at,
	variants, from_names, approved_domains, approved_isps, created_at, updated_at`

// ── proofs ─────────────────────────────────────────────────────────────────

// HandleISPs returns the canonical ISP picklist (engine.AllISPs) so the frontend
// never drifts from the backend's notion of valid ISPs.
func (s *OfferProofsService) HandleISPs(w http.ResponseWriter, r *http.Request) {
	out := []string{}
	for _, isp := range engine.AllISPs() {
		out = append(out, string(isp))
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"isps": out})
}

func (s *OfferProofsService) HandleList(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	q := r.URL.Query()
	clauses := []string{"organization_id = $1"}
	args := []interface{}{orgID.String()}
	if st := strings.TrimSpace(q.Get("status")); st != "" {
		args = append(args, st)
		clauses = append(clauses, "approval_status = $2")
	}
	if act := strings.TrimSpace(q.Get("active")); act == "true" || act == "false" {
		clauses = append(clauses, "is_active = "+act)
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT `+proofSelectCols+` FROM mailing_offer_proofs WHERE `+strings.Join(clauses, " AND ")+
			` ORDER BY created_at DESC LIMIT 500`, args...)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	proofs := []offerProof{}
	for rows.Next() {
		p, err := scanProof(rows, false) // list: no html_content
		if err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		proofs = append(proofs, p)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"proofs": proofs})
}

func (s *OfferProofsService) loadProof(ctx context.Context, orgID, id string, includeHTML bool) (offerProof, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+proofSelectCols+` FROM mailing_offer_proofs WHERE id = $1 AND organization_id = $2`,
		id, orgID)
	return scanProof(row, includeHTML)
}

func (s *OfferProofsService) HandleGet(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	p, err := s.loadProof(r.Context(), orgID.String(), chi.URLParam(r, "id"), true)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "proof not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, p)
}

func (s *OfferProofsService) HandlePreview(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, "organization not resolved", http.StatusBadRequest)
		return
	}
	p, err := s.loadProof(r.Context(), orgID.String(), chi.URLParam(r, "id"), true)
	if err == sql.ErrNoRows {
		http.Error(w, "proof not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Show the footer as it will render at send: bottom of the email, branded
	// with the first approved domain (or neutral if none approved yet). The
	// unsubscribe merge tag has no send context here, so point it at "#".
	brand := ""
	if len(p.ApprovedDomains) > 0 {
		brand = brandFromSendingDomain(p.ApprovedDomains[0])
	}
	preview := appendUnsubDisclaimer(p.HTMLContent, brand, "")
	preview = strings.ReplaceAll(preview, "{{ system.unsubscribe_url }}", "#")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(preview))
}

type createProofRequest struct {
	Name      string         `json:"name"`
	OfferKey  string         `json:"offer_key"`
	HTML      string         `json:"html"`
	Variants  []proofVariant `json:"variants"`
	FromNames []string       `json:"from_names"`
	Rehost    *bool          `json:"rehost"` // default true
}

// processHTML rehosts external images (when S3 is wired). The footer/unsub block
// is NOT injected here — it is appended at SEND time (and preview), at the bottom
// of the email, carrying the actual sending brand (the domain isn't known at
// create/rehost time, and a fragment creative with no <body> would otherwise get
// the footer prepended into the header). Returns the processed html + the full
// rehost outcome so the caller can surface failed/skipped images.
func (s *OfferProofsService) processHTML(ctx context.Context, orgID, html string, rehost bool) (string, RehostCounts) {
	var counts RehostCounts
	if rehost && s.imageCDN != nil {
		var processed string
		processed, counts = s.imageCDN.RehostHTML(ctx, orgID, html)
		html = processed
	}
	return html, counts
}

// brandFromSendingDomain turns a sending domain into the recipient-facing brand
// shown in the footer: em.discountblog.com -> discountblog.com.
func brandFromSendingDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(d, "em.")
	return d
}

// brandImageHost returns the provisioned per-brand image domain (img.<brand>) for
// a sending brand, or "" if it isn't set up + verified — in which case images
// stay on the neutral platform CDN (never a wrong brand). e.g. "discountblog.com"
// -> "img.discountblog.com" when that domain exists in mailing_image_domains.
func (s *OfferProofsService) brandImageHost(ctx context.Context, orgID, brand string) string {
	if strings.TrimSpace(brand) == "" {
		return ""
	}
	candidate := "img." + brand
	var got string
	_ = s.db.QueryRowContext(ctx,
		`SELECT domain FROM mailing_image_domains
		 WHERE org_id = $1 AND lower(domain) = lower($2) AND verified = true AND ssl_status = 'active'
		 LIMIT 1`,
		orgID, candidate).Scan(&got)
	return got
}

// brandMatchImageHosts rewrites neutral-CDN image hosts to the sending brand's
// img.<brand> host when that domain is provisioned; otherwise returns html
// unchanged (neutral). The S3 object/path is identical across brands — only the
// hostname differs — so this is a pure host find-and-replace.
func (s *OfferProofsService) brandMatchImageHosts(ctx context.Context, orgID, brand, html string) string {
	if s.imageCDN == nil {
		return html
	}
	neutral := s.imageCDN.CDNDomain()
	bh := s.brandImageHost(ctx, orgID, brand)
	if neutral == "" || bh == "" {
		return html
	}
	return strings.ReplaceAll(html, "https://"+neutral, "https://"+bh)
}

// rehostSummary is the per-create/per-rehost outcome surfaced to the operator.
func rehostSummary(c RehostCounts) map[string]interface{} {
	return map[string]interface{}{
		"rehosted": c.Rehosted, "cached": c.Cached, "skipped": c.Skipped,
		"failed": c.Failed, "details": c.Details,
	}
}

func (s *OfferProofsService) HandleCreate(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	var req createProofRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	if strings.TrimSpace(req.HTML) == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "html required"})
		return
	}
	rehost := req.Rehost == nil || *req.Rehost
	processed, counts := s.processHTML(r.Context(), orgID.String(), req.HTML, rehost)

	var id string
	err = s.db.QueryRowContext(r.Context(),
		`INSERT INTO mailing_offer_proofs
			(organization_id, name, offer_key, html_content, html_raw, images_rehosted,
			 variants, from_names, approval_status, is_active)
		 VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,'pending',TRUE)
		 RETURNING id::text`,
		orgID.String(), req.Name, strings.TrimSpace(req.OfferKey), processed, req.HTML, counts.Rehosted,
		marshalJSONArray(req.Variants), marshalJSONArray(cleanStrings(req.FromNames)),
	).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			respondJSON(w, http.StatusConflict, map[string]string{"error": "a proof named '" + req.Name + "' already exists"})
			return
		}
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p, err := s.loadProof(r.Context(), orgID.String(), id, true)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"proof": p, "rehost": rehostSummary(counts)})
}

// HandleRehostProof re-runs image rehosting + footer injection on an existing
// proof's STORED raw html (html_raw) and updates html_content + images_rehosted.
// Lets the operator fix a proof whose images didn't rehost on first upload
// without re-pasting the creative. Returns the refreshed proof + rehost summary.
func (s *OfferProofsService) HandleRehostProof(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	id := chi.URLParam(r, "id")
	var raw string
	err = s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(NULLIF(html_raw,''), html_content) FROM mailing_offer_proofs
		 WHERE id = $1 AND organization_id = $2`, id, orgID.String()).Scan(&raw)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "proof not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	processed, counts := s.processHTML(r.Context(), orgID.String(), raw, true)
	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE mailing_offer_proofs SET html_content=$1, images_rehosted=$2, updated_at=NOW()
		 WHERE id=$3 AND organization_id=$4`,
		processed, counts.Rehosted, id, orgID.String()); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p, err := s.loadProof(r.Context(), orgID.String(), id, true)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"proof": p, "rehost": rehostSummary(counts)})
}

type updateProofRequest struct {
	Name      *string         `json:"name"`
	OfferKey  *string         `json:"offer_key"`
	HTML      *string         `json:"html"`
	Variants  *[]proofVariant `json:"variants"`
	FromNames *[]string       `json:"from_names"`
	IsActive  *bool           `json:"is_active"`
}

func (s *OfferProofsService) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	id := chi.URLParam(r, "id")
	var req updateProofRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	sets := []string{"updated_at = NOW()"}
	args := []interface{}{}
	n := 1
	add := func(expr string, val interface{}) {
		sets = append(sets, expr+" = $"+strconv.Itoa(n))
		args = append(args, val)
		n++
	}
	if req.Name != nil {
		nm := strings.TrimSpace(*req.Name)
		if nm == "" {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty"})
			return
		}
		add("name", nm)
	}
	if req.OfferKey != nil {
		add("offer_key", strings.TrimSpace(*req.OfferKey))
	}
	if req.HTML != nil {
		processed, counts := s.processHTML(r.Context(), orgID.String(), *req.HTML, true)
		add("html_content", processed)
		add("html_raw", *req.HTML)
		add("images_rehosted", counts.Rehosted)
	}
	if req.Variants != nil {
		add("variants", marshalJSONArray(*req.Variants))
		// cast handled below via ::jsonb in the expr; lib/pq sends as text
		sets[len(sets)-1] = "variants = $" + strconv.Itoa(n-1) + "::jsonb"
	}
	if req.FromNames != nil {
		add("from_names", marshalJSONArray(cleanStrings(*req.FromNames)))
		sets[len(sets)-1] = "from_names = $" + strconv.Itoa(n-1) + "::jsonb"
	}
	if req.IsActive != nil {
		add("is_active", *req.IsActive)
	}
	if len(args) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "no fields to update"})
		return
	}
	args = append(args, id, orgID.String())
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE mailing_offer_proofs SET `+strings.Join(sets, ", ")+
			` WHERE id = $`+strconv.Itoa(n)+` AND organization_id = $`+strconv.Itoa(n+1), args...)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			respondJSON(w, http.StatusConflict, map[string]string{"error": "name already in use"})
			return
		}
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if cnt, _ := res.RowsAffected(); cnt == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "proof not found"})
		return
	}
	p, err := s.loadProof(r.Context(), orgID.String(), id, true)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, p)
}

type approveProofRequest struct {
	ApprovedDomains []string       `json:"approved_domains"`
	ApprovedISPs    []string       `json:"approved_isps"`
	Variants        []proofVariant `json:"variants"`
	FromNames       []string       `json:"from_names"`
	ApprovedBy      string         `json:"approved_by"`
}

func (s *OfferProofsService) HandleApprove(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	id := chi.URLParam(r, "id")
	var req approveProofRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	domains := cleanStrings(req.ApprovedDomains)
	isps := cleanStrings(req.ApprovedISPs)
	// Reject any ISP not in the canonical engine set — keeps the registry clean.
	valid := validISPSet()
	for _, isp := range isps {
		if !valid[strings.ToLower(isp)] {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown ISP '" + isp + "'"})
			return
		}
	}
	approvedBy := strings.TrimSpace(req.ApprovedBy)
	if approvedBy == "" {
		approvedBy = "operator"
	}
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE mailing_offer_proofs
		 SET approval_status='approved', approved_by=$1, approved_at=NOW(), updated_at=NOW(),
		     approved_domains=$2::jsonb, approved_isps=$3::jsonb,
		     variants=$4::jsonb, from_names=$5::jsonb
		 WHERE id=$6 AND organization_id=$7`,
		approvedBy, marshalJSONArray(domains), marshalJSONArray(isps),
		marshalJSONArray(req.Variants), marshalJSONArray(cleanStrings(req.FromNames)),
		id, orgID.String())
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if cnt, _ := res.RowsAffected(); cnt == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "proof not found"})
		return
	}
	p, err := s.loadProof(r.Context(), orgID.String(), id, true)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, p)
}

func (s *OfferProofsService) HandleReject(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	id := chi.URLParam(r, "id")
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE mailing_offer_proofs SET approval_status='rejected', updated_at=NOW()
		 WHERE id=$1 AND organization_id=$2`, id, orgID.String())
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if cnt, _ := res.RowsAffected(); cnt == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "proof not found"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

type sendProofReq struct {
	RecipientIDs  []string `json:"recipient_ids"`
	SendingDomain string   `json:"sending_domain"`
	Subject       string   `json:"subject"`
	FromName      string   `json:"from_name"`
	Preheader     string   `json:"preheader"`
}

type proofSendResult struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	MessageID string `json:"message_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *OfferProofsService) HandleSend(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	id := chi.URLParam(r, "id")
	var req sendProofReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	req.SendingDomain = strings.TrimSpace(req.SendingDomain)
	if req.SendingDomain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "sending_domain required"})
		return
	}
	ids := cleanStrings(req.RecipientIDs)
	if len(ids) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "select at least one recipient"})
		return
	}
	if len(ids) > proofSendMaxRecipients {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "too many recipients (max 50)"})
		return
	}

	// Load proof html (org-scoped).
	p, err := s.loadProof(r.Context(), orgID.String(), id, true)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "proof not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(p.HTMLContent) == "" {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "proof has no html"})
		return
	}

	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		if len(p.Variants) > 0 {
			subject = p.Variants[0].Subject
		}
		if subject == "" {
			subject = p.Name
		}
	}

	// Append the footer/unsub at the bottom, branded with the actual sending
	// domain (resolved only now, at send time). sendProofMessage renders the
	// {{ system.unsubscribe_url }} merge tag inside it.
	brand := brandFromSendingDomain(req.SendingDomain)
	htmlForSend := appendUnsubDisclaimer(p.HTMLContent, brand, "")
	// Brand-match image hosts to img.<brand> when provisioned; else stay neutral.
	htmlForSend = s.brandMatchImageHosts(r.Context(), orgID.String(), brand, htmlForSend)

	// Resolve recipients from the curated account-manager table only.
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT name, email FROM mailing_proof_recipients
		 WHERE organization_id = $1 AND id = ANY($2::uuid[]) AND is_active = TRUE`,
		orgID.String(), pq.Array(ids))
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type rcpt struct{ name, email string }
	var recipients []rcpt
	for rows.Next() {
		var rc rcpt
		if err := rows.Scan(&rc.name, &rc.email); err == nil {
			recipients = append(recipients, rc)
		}
	}
	rows.Close()
	if len(recipients) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "no active recipients matched"})
		return
	}

	results := make([]proofSendResult, 0, len(recipients))
	sent, failed := 0, 0
	for _, rc := range recipients {
		res := proofSendResult{Email: rc.email, Name: rc.name}
		if !looksLikeEmail(rc.email) {
			res.Status = "error"
			res.Error = "invalid email"
			failed++
			results = append(results, res)
			continue
		}
		msgID, sendErr := s.proofSender.sendProofMessage(r.Context(), orgID.String(), req.SendingDomain,
			subject, strings.TrimSpace(req.FromName), strings.TrimSpace(req.Preheader), htmlForSend, rc.email, id,
			map[string]string{"X-Proof-Send": "true", "X-Offer-Proof-ID": id})
		if sendErr != nil {
			res.Status = "error"
			res.Error = sendErr.Error()
			failed++
		} else {
			res.Status = "sent"
			res.MessageID = msgID
			sent++
		}
		results = append(results, res)
		if _, logErr := s.db.ExecContext(r.Context(),
			`INSERT INTO mailing_proof_sends
				(organization_id, proof_id, sending_domain, recipient_email, recipient_name, subject, from_name, status, message_id, error)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			orgID.String(), id, req.SendingDomain, rc.email, rc.name, subject, strings.TrimSpace(req.FromName),
			res.Status, res.MessageID, res.Error); logErr != nil {
			log.Printf("[offer-proof] audit insert failed proof=%s to=%s: %v", id, rc.email, logErr)
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"results": results, "sent": sent, "failed": failed,
	})
}

type bulkProofReq struct {
	IDs    []string `json:"ids"`
	Action string   `json:"action"` // delete|activate|deactivate|approve|reject
}

func (s *OfferProofsService) HandleBulk(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	var req bulkProofReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	ids := cleanStrings(req.IDs)
	if len(ids) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "no ids"})
		return
	}
	var q string
	switch req.Action {
	case "delete":
		q = `DELETE FROM mailing_offer_proofs WHERE organization_id=$1 AND id = ANY($2::uuid[])`
	case "activate":
		q = `UPDATE mailing_offer_proofs SET is_active=TRUE, updated_at=NOW() WHERE organization_id=$1 AND id = ANY($2::uuid[])`
	case "deactivate":
		q = `UPDATE mailing_offer_proofs SET is_active=FALSE, updated_at=NOW() WHERE organization_id=$1 AND id = ANY($2::uuid[])`
	case "approve":
		q = `UPDATE mailing_offer_proofs SET approval_status='approved', approved_at=NOW(), updated_at=NOW() WHERE organization_id=$1 AND id = ANY($2::uuid[])`
	case "reject":
		q = `UPDATE mailing_offer_proofs SET approval_status='rejected', updated_at=NOW() WHERE organization_id=$1 AND id = ANY($2::uuid[])`
	default:
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action '" + req.Action + "'"})
		return
	}
	res, err := s.db.ExecContext(r.Context(), q, orgID.String(), pq.Array(ids))
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cnt, _ := res.RowsAffected()
	respondJSON(w, http.StatusOK, map[string]interface{}{"action": req.Action, "affected": cnt})
}

// ── account managers (proof recipients) ──────────────────────────────────────

type proofRecipient struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *OfferProofsService) HandleListRecipients(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id::text, name, email, is_active, created_at FROM mailing_proof_recipients
		 WHERE organization_id=$1 ORDER BY name ASC`, orgID.String())
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []proofRecipient{}
	for rows.Next() {
		var rc proofRecipient
		if err := rows.Scan(&rc.ID, &rc.Name, &rc.Email, &rc.IsActive, &rc.CreatedAt); err == nil {
			out = append(out, rc)
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"recipients": out})
}

type recipientReq struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	IsActive *bool  `json:"is_active"`
}

func (s *OfferProofsService) HandleCreateRecipient(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	var req recipientReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Name == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	if !looksLikeEmail(req.Email) {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "valid email required"})
		return
	}
	var id string
	err = s.db.QueryRowContext(r.Context(),
		`INSERT INTO mailing_proof_recipients (organization_id, name, email)
		 VALUES ($1,$2,$3) RETURNING id::text`, orgID.String(), req.Name, req.Email).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			respondJSON(w, http.StatusConflict, map[string]string{"error": req.Email + " already exists"})
			return
		}
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"id": id})
}

func (s *OfferProofsService) HandleUpdateRecipient(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	id := chi.URLParam(r, "id")
	var req recipientReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	sets := []string{"updated_at = NOW()"}
	args := []interface{}{}
	n := 1
	if strings.TrimSpace(req.Name) != "" {
		sets = append(sets, "name = $"+strconv.Itoa(n))
		args = append(args, strings.TrimSpace(req.Name))
		n++
	}
	if e := strings.ToLower(strings.TrimSpace(req.Email)); e != "" {
		if !looksLikeEmail(e) {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "valid email required"})
			return
		}
		sets = append(sets, "email = $"+strconv.Itoa(n))
		args = append(args, e)
		n++
	}
	if req.IsActive != nil {
		sets = append(sets, "is_active = $"+strconv.Itoa(n))
		args = append(args, *req.IsActive)
		n++
	}
	if len(args) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "no fields to update"})
		return
	}
	args = append(args, id, orgID.String())
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE mailing_proof_recipients SET `+strings.Join(sets, ", ")+
			` WHERE id=$`+strconv.Itoa(n)+` AND organization_id=$`+strconv.Itoa(n+1), args...)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			respondJSON(w, http.StatusConflict, map[string]string{"error": "email already in use"})
			return
		}
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if cnt, _ := res.RowsAffected(); cnt == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "recipient not found"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *OfferProofsService) HandleDeleteRecipient(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "organization not resolved"})
		return
	}
	res, err := s.db.ExecContext(r.Context(),
		`DELETE FROM mailing_proof_recipients WHERE id=$1 AND organization_id=$2`,
		chi.URLParam(r, "id"), orgID.String())
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if cnt, _ := res.RowsAffected(); cnt == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "recipient not found"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
