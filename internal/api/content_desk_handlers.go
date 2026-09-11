package api

// Content Desk HTTP surface (2026-09-11). Thin: every handler resolves the
// org, decodes, calls internal/contentdesk.Store, and maps sentinel errors to
// statuses. Session routes mount under /api/mailing/content-desk via
// RegisterRoutes (SetMailingDB); the release endpoints and the Mac-side
// ops-report ingest are admin routes on the ROOT router behind X-Admin-Key,
// closed by default (the pool-isolation pattern in server_routes_mailing.go).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/contentdesk"
	"github.com/ignite/sparkpost-monitor/internal/tracking"
)

const contentDeskAdminPrefix = "/api/mailing/content-desk"

// ContentDeskService serves the Content Desk API.
type ContentDeskService struct {
	store         *contentdesk.Store
	now           func() time.Time
	serverSignals func() contentdesk.ServerSignals
}

// NewContentDeskService wires the store.
func NewContentDeskService(db *sql.DB) *ContentDeskService {
	return &ContentDeskService{store: contentdesk.NewStore(db), now: time.Now, serverSignals: contentDeskServerSignals}
}

// contentDeskServerSignals reads this process's own /health track_sig block
// (same calls as health_handler.go) and PREFERENCES_MODE.
func contentDeskServerSignals() contentdesk.ServerSignals {
	mode, _ := tracking.SigStatus(0)["mode"].(string)
	pm, set := os.LookupEnv("PREFERENCES_MODE")
	return contentdesk.ServerSignals{
		TrackSig: &contentdesk.TrackSigState{
			Mode:     mode,
			Keys:     len(tracking.LoadSigKeysFromEnv()),
			Counters: tracking.SigCountersSnapshot(),
		},
		PreferencesModeSet: set && strings.TrimSpace(pm) != "",
		PreferencesMode:    strings.TrimSpace(pm),
	}
}

// RegisterRoutes mounts the session routes under the /api/mailing group.
func (s *ContentDeskService) RegisterRoutes(r chi.Router) {
	r.Route("/content-desk", func(cr chi.Router) {
		cr.Get("/sites", s.HandleListSites)
		cr.Patch("/sites/{id}", s.HandlePatchSite)
		cr.Post("/briefs", s.HandleCreateBrief)
		cr.Get("/briefs", s.HandleListBriefs)
		cr.Get("/articles", s.HandleListArticles)
		cr.Get("/articles/{id}", s.HandleGetArticle)
		cr.Post("/articles/{id}/run", s.HandleRunArticle)
		cr.Post("/articles/{id}/reviews", s.HandleReview)
		cr.Post("/articles/{id}/withdraw", s.HandleWithdraw)
		cr.Post("/claims/{claim_id}/versions", s.HandleClaimVersion)
		cr.Get("/ops-status", s.HandleOpsStatus)
	})
}

// RegisterAdminRoutes mounts the admin routes on the ROOT router.
func (s *ContentDeskService) RegisterAdminRoutes(root chi.Router) {
	root.Get(contentDeskAdminPrefix+"/releases/manifest", contentDeskAdminOnly(s.HandleManifest))
	root.Post(contentDeskAdminPrefix+"/releases", contentDeskAdminOnly(s.HandleCreateRelease))
	root.Post(contentDeskAdminPrefix+"/releases/{id}/status", contentDeskAdminOnly(s.HandleReleaseStatus))
	root.Post(contentDeskAdminPrefix+"/ops-report", contentDeskAdminOnly(s.HandleOpsReport))
}

// contentDeskAdminOnly: X-Admin-Key must equal ADMIN_API_KEY; an unset key
// refuses everything (closed by default).
func contentDeskAdminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		adminKey := os.Getenv("ADMIN_API_KEY")
		if adminKey == "" || req.Header.Get("X-Admin-Key") != adminKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		h(w, req)
	}
}

func contentDeskOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization not resolved: "+err.Error())
		return "", false
	}
	return id.String(), true
}

func contentDeskDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func contentDeskError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, contentdesk.ErrNotFound):
		respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, contentdesk.ErrHashMismatch), errors.Is(err, contentdesk.ErrConflict), errors.Is(err, contentdesk.ErrManifestDrop):
		respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, contentdesk.ErrInvalid):
		respondError(w, http.StatusBadRequest, err.Error())
	default:
		log.Printf("[ContentDesk] ERROR op=%s: %v", op, err)
		respondError(w, http.StatusInternalServerError, op+" failed: "+err.Error())
	}
}

func contentDeskCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 20*time.Second)
}

// HandleListSites — GET /content-desk/sites
func (s *ContentDeskService) HandleListSites(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	sites, err := s.store.ListSites(ctx, org)
	if err != nil {
		contentDeskError(w, "list sites", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

// HandlePatchSite — PATCH /content-desk/sites/{id}
func (s *ContentDeskService) HandlePatchSite(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	var p contentdesk.SitePatch
	if !contentDeskDecode(w, r, &p) {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	site, err := s.store.PatchSite(ctx, org, chi.URLParam(r, "id"), p)
	if err != nil {
		contentDeskError(w, "patch site", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"site": site})
}

// HandleCreateBrief — POST /content-desk/briefs (creates the drafting article too)
func (s *ContentDeskService) HandleCreateBrief(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	var in contentdesk.BriefInput
	if !contentDeskDecode(w, r, &in) {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	b, err := s.store.CreateBrief(ctx, org, in)
	if err != nil {
		contentDeskError(w, "create brief", err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"brief": b})
}

// HandleListBriefs — GET /content-desk/briefs?site=
func (s *ContentDeskService) HandleListBriefs(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	briefs, err := s.store.ListBriefs(ctx, org, r.URL.Query().Get("site"))
	if err != nil {
		contentDeskError(w, "list briefs", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"briefs": briefs})
}

// HandleListArticles — GET /content-desk/articles?status=&site=
func (s *ContentDeskService) HandleListArticles(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	q := r.URL.Query()
	arts, err := s.store.ListArticles(ctx, org, q.Get("status"), q.Get("site"))
	if err != nil {
		contentDeskError(w, "list articles", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"articles": arts})
}

// HandleGetArticle — GET /content-desk/articles/{id}: revision, package,
// claims with passages, checks, reviews, sentence↔passage pairs.
func (s *ContentDeskService) HandleGetArticle(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	d, err := s.store.GetArticleDetail(ctx, org, chi.URLParam(r, "id"))
	if err != nil {
		contentDeskError(w, "get article", err)
		return
	}
	respondJSON(w, http.StatusOK, d)
}

// HandleRunArticle — POST /content-desk/articles/{id}/run (enqueue)
func (s *ContentDeskService) HandleRunArticle(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	if err := s.store.EnqueueRun(ctx, org, chi.URLParam(r, "id")); err != nil {
		contentDeskError(w, "enqueue run", err)
		return
	}
	// Queued even with the kill switch off — say so, so a queued-but-idle
	// article is never mistaken for a stuck worker.
	respondJSON(w, http.StatusAccepted, map[string]any{"queued": true, "pipeline_enabled": contentdesk.Enabled()})
}

// HandleReview — POST /content-desk/articles/{id}/reviews. 409 when
// revision_hash is not the current revision's; 422 with blockers when an
// approve is blocked (nothing is recorded).
func (s *ContentDeskService) HandleReview(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	var in contentdesk.ReviewInput
	if !contentDeskDecode(w, r, &in) {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	rv, outcome, err := s.store.SubmitReview(ctx, org, chi.URLParam(r, "id"), in)
	if errors.Is(err, contentdesk.ErrApprovalBlocked) {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "blockers": outcome.Blockers})
		return
	}
	if err != nil {
		contentDeskError(w, "submit review", err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"review": rv, "outcome": outcome})
}

// HandleWithdraw — POST /content-desk/articles/{id}/withdraw
func (s *ContentDeskService) HandleWithdraw(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	siteID, err := s.store.Withdraw(ctx, org, chi.URLParam(r, "id"))
	if err != nil {
		contentDeskError(w, "withdraw", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": contentdesk.StatusWithdrawn, "harvestable": false,
		"release_required_for_site": siteID})
}

// HandleClaimVersion — POST /content-desk/claims/{claim_id}/versions
func (s *ContentDeskService) HandleClaimVersion(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	var in contentdesk.Claim
	if !contentDeskDecode(w, r, &in) {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	c, flagged, err := s.store.NewClaimVersion(ctx, org, chi.URLParam(r, "claim_id"), in)
	if err != nil {
		contentDeskError(w, "new claim version", err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"claim": c, "flagged_articles": flagged})
}

// HandleOpsStatus — GET /content-desk/ops-status: one JSON for the dashboard.
func (s *ContentDeskService) HandleOpsStatus(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	o, err := s.store.OpsStatus(ctx, org)
	if err != nil {
		contentDeskError(w, "ops status", err)
		return
	}
	reports, err := s.store.ListOpsReports(ctx, org)
	if err != nil {
		contentDeskError(w, "ops reports", err)
		return
	}
	contentdesk.ApplyOpsChecks(o, reports, s.serverSignals(), s.now())
	respondJSON(w, http.StatusOK, o)
}

// HandleManifest — admin GET /api/mailing/content-desk/releases/manifest?site=
func (s *ContentDeskService) HandleManifest(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	site := r.URL.Query().Get("site")
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	m, hash, err := s.store.Manifest(ctx, org, site)
	if err != nil {
		contentDeskError(w, "manifest", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"site_id": site, "manifest": m, "manifest_hash": hash})
}

// HandleCreateRelease — admin POST /api/mailing/content-desk/releases {site_id}
func (s *ContentDeskService) HandleCreateRelease(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	var body struct {
		SiteID string `json:"site_id"`
	}
	if !contentDeskDecode(w, r, &body) {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	rel, err := s.store.CreateRelease(ctx, org, body.SiteID)
	if err != nil {
		contentDeskError(w, "create release", err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"release": rel})
}

// HandleReleaseStatus — admin POST /api/mailing/content-desk/releases/{id}/status
func (s *ContentDeskService) HandleReleaseStatus(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if !contentDeskDecode(w, r, &body) {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	rel, err := s.store.SetReleaseStatus(ctx, org, chi.URLParam(r, "id"), body.Status, body.Error)
	if err != nil {
		contentDeskError(w, "release status", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"release": rel})
}

// HandleOpsReport — admin POST /api/mailing/content-desk/ops-report: the
// latest Mac-side report per (org, source).
func (s *ContentDeskService) HandleOpsReport(w http.ResponseWriter, r *http.Request) {
	org, ok := contentDeskOrg(w, r)
	if !ok {
		return
	}
	var rep contentdesk.OpsReport
	if !contentDeskDecode(w, r, &rep) {
		return
	}
	ctx, cancel := contentDeskCtx(r)
	defer cancel()
	stored, err := s.store.UpsertOpsReport(ctx, org, rep)
	if err != nil {
		contentDeskError(w, "ops report", err)
		return
	}
	respondJSON(w, http.StatusAccepted, map[string]any{"source": stored.Source, "generated_at": stored.GeneratedAt,
		"received_at": stored.ReceivedAt, "checks": len(stored.Checks)})
}
