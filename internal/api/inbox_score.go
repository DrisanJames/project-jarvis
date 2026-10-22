package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/lib/pq"
)

// ── Inbox Score ──────────────────────────────────────────────────────────────
//
// Per-ISP spam-filter profiles (SpamAssassin, trained on this estate's own
// campaign×ISP outcomes) score the bytes a campaign will send, and real seed
// mailboxes report where the live send landed. This service is the platform
// half: it renders the exact message through the worker's own compose path,
// stores scores / profiles / seed placements, and serves the portal.
//
// Routes (session, /api/mailing/inbox-score/*): summary, campaign detail,
// profiles, render. Admin (/api/admin/inbox-score/*, X-Admin-Key): render,
// scores, profiles, seed-placements — used by the off-box scanner, trainer
// and seed reader (agents/inboxscore).
//
// Every number carries its source. P(inbox) is a model output with the
// profile's published backtest accuracy beside it; a seed folder is an
// observation; neither is ever blended into the other.

const inboxScoreAdminPrefix = "/api/admin/inbox-score"

// InboxScoreISPs is the fixed profile set. The scanner may only post scores
// for these; anything else is rejected so the portal columns stay stable.
var InboxScoreISPs = []string{"gmail", "yahoo_family", "microsoft", "apple", "cables"}

type InboxScoreService struct {
	db   *sql.DB
	pool *worker.SendWorkerPool
	now  func() time.Time
}

// NewInboxScoreService wires the render pool with the same tracking config
// the live send pool uses (TRACKING_URL / TRACKING_SECRET env, single-tenant
// org), so rendered bytes match sent bytes.
func NewInboxScoreService(db *sql.DB) *InboxScoreService {
	trackURL := os.Getenv("TRACKING_URL")
	if trackURL == "" {
		trackURL = "https://t.projectjarvis.io"
	}
	return &InboxScoreService{
		db:   db,
		pool: worker.NewRenderPool(db, trackURL, os.Getenv("TRACKING_SECRET"), SingleTenantFallbackOrgID),
		now:  time.Now,
	}
}

// SetRenderPool replaces the compose pool (tests).
func (s *InboxScoreService) SetRenderPool(p *worker.SendWorkerPool) { s.pool = p }

// RegisterRoutes mounts the session routes under /api/mailing.
func (s *InboxScoreService) RegisterRoutes(r chi.Router) {
	r.Route("/inbox-score", func(cr chi.Router) {
		cr.Get("/summary", s.HandleSummary)
		cr.Get("/campaign/{id}", s.HandleCampaign)
		cr.Get("/profiles", s.HandleProfiles)
		cr.Get("/catalog", s.HandleCatalog)
		cr.Post("/render", s.HandleRender)
	})
}

// RegisterAdminRoutes mounts the X-Admin-Key routes on the root router.
func (s *InboxScoreService) RegisterAdminRoutes(root chi.Router) {
	root.Post(inboxScoreAdminPrefix+"/render", inboxScoreAdminOnly(s.HandleRender))
	root.Post(inboxScoreAdminPrefix+"/scores", inboxScoreAdminOnly(s.HandlePostScores))
	root.Post(inboxScoreAdminPrefix+"/profiles", inboxScoreAdminOnly(s.HandlePostProfile))
	root.Post(inboxScoreAdminPrefix+"/seed-placements", inboxScoreAdminOnly(s.HandlePostSeedPlacements))
	root.Get(inboxScoreAdminPrefix+"/summary", inboxScoreAdminOnly(s.HandleSummary))
	root.Get(inboxScoreAdminPrefix+"/seeds", inboxScoreAdminOnly(s.HandleGetSeeds))
	root.Post(inboxScoreAdminPrefix+"/seeds", inboxScoreAdminOnly(s.HandlePostSeeds))
	root.Post(inboxScoreAdminPrefix+"/catalog", inboxScoreAdminOnly(s.HandlePostCatalog))
	root.Get(inboxScoreAdminPrefix+"/catalog", inboxScoreAdminOnly(s.HandleCatalog))
}

// inboxScoreAdminOnly: X-Admin-Key must equal ADMIN_API_KEY; an unset key
// refuses everything (closed by default — same gate as Content Desk / Brain).
func inboxScoreAdminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		adminKey := os.Getenv("ADMIN_API_KEY")
		if adminKey == "" || req.Header.Get("X-Admin-Key") != adminKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		h(w, req)
	}
}

func inboxScoreOrg(w http.ResponseWriter, r *http.Request) (string, bool) {
	org := strings.TrimSpace(getOrgID(r))
	if _, err := uuid.Parse(org); err != nil {
		respondError(w, http.StatusBadRequest, "organization could not be resolved")
		return "", false
	}
	return org, true
}

func inboxScoreValidISP(isp string) bool {
	for _, k := range InboxScoreISPs {
		if k == isp {
			return true
		}
	}
	return false
}

// ── render ───────────────────────────────────────────────────────────────────

type inboxScoreRenderRequest struct {
	CampaignID string `json:"campaign_id"`
	Email      string `json:"email"`
}

type inboxScoreRenderResponse struct {
	CampaignID    string            `json:"campaign_id"`
	Email         string            `json:"email"`
	RecipientISP  string            `json:"recipient_isp"`
	ContentHash   string            `json:"content_hash"`
	ContentLocked bool              `json:"content_locked"`
	Headers       map[string]string `json:"headers"`
	RFC822        string            `json:"rfc822"`
	RFC822SHA256  string            `json:"rfc822_sha256"`
	Deltas        []string          `json:"deltas"`
	Basis         string            `json:"basis"`
}

// HandleRender returns the message the worker would send for (campaign,
// email), byte-for-byte through composeMessage + BuildRFC822. No send, no
// queue write.
func (s *InboxScoreService) HandleRender(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	var req inboxScoreRenderRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	ri, err := worker.LoadRenderItem(ctx, s.db, org, req.CampaignID, req.Email)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		respondError(w, code, err.Error())
		return
	}
	msg, err := s.pool.ComposeForRender(ctx, ri.Item)
	if err != nil {
		respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	raw := worker.RenderRFC822(msg)
	sum := sha256.Sum256([]byte(raw))
	hdrs := make(map[string]string, len(msg.Headers))
	for k, v := range msg.Headers {
		hdrs[k] = v
	}
	respondJSON(w, http.StatusOK, inboxScoreRenderResponse{
		CampaignID:    req.CampaignID,
		Email:         strings.TrimSpace(req.Email),
		RecipientISP:  msg.RecipientISP,
		ContentHash:   ri.ContentHash,
		ContentLocked: ri.Locked,
		Headers:       hdrs,
		RFC822:        raw,
		RFC822SHA256:  hex.EncodeToString(sum[:]),
		Deltas:        ri.Deltas,
		Basis:         "worker.composeMessage + worker.BuildRFC822 (the live send path); see deltas for the per-send values a pre-dispatch render cannot know",
	})
}

// ── scores (admin write, session read) ──────────────────────────────────────

type inboxScoreRow struct {
	CampaignID     string          `json:"campaign_id"`
	ContentHash    string          `json:"content_hash"`
	ISP            string          `json:"isp"`
	ProfileVersion int             `json:"profile_version"`
	SAScore        *float64        `json:"sa_score"`
	PInbox         *float64        `json:"p_inbox"`
	Rules          json.RawMessage `json:"rules"`
	Reasons        json.RawMessage `json:"reasons"`
	Source         string          `json:"source"`
	ScoredAt       *string         `json:"scored_at,omitempty"`
}

type inboxScorePostScores struct {
	Scores []inboxScoreRow `json:"scores"`
}

// HandlePostScores upserts scanner output. One row per (campaign, isp,
// source); a re-score replaces the previous row for that key.
func (s *InboxScoreService) HandlePostScores(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	var body inboxScorePostScores
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Scores) == 0 || len(body.Scores) > 5000 {
		respondError(w, http.StatusBadRequest, "scores must hold 1..5000 rows")
		return
	}
	for i, sc := range body.Scores {
		if _, err := uuid.Parse(sc.CampaignID); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("scores[%d].campaign_id is not a UUID", i))
			return
		}
		if !inboxScoreValidISP(sc.ISP) {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("scores[%d].isp %q is not one of %v", i, sc.ISP, InboxScoreISPs))
			return
		}
		if sc.PInbox != nil && (*sc.PInbox < 0 || *sc.PInbox > 1) {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("scores[%d].p_inbox must be within 0..1", i))
			return
		}
		if sc.Source == "" {
			body.Scores[i].Source = "render"
		}
		if len(sc.Rules) == 0 {
			body.Scores[i].Rules = json.RawMessage("[]")
		}
		if len(sc.Reasons) == 0 {
			body.Scores[i].Reasons = json.RawMessage("[]")
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db unavailable")
		return
	}
	defer tx.Rollback()
	n := 0
	for _, sc := range body.Scores {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_inbox_scores
			  (organization_id, campaign_id, content_hash, isp, profile_version, sa_score, p_inbox, rules, reasons, source, scored_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10,NOW())
			ON CONFLICT (organization_id, campaign_id, isp, source) DO UPDATE SET
			  content_hash = EXCLUDED.content_hash, profile_version = EXCLUDED.profile_version,
			  sa_score = EXCLUDED.sa_score, p_inbox = EXCLUDED.p_inbox,
			  rules = EXCLUDED.rules, reasons = EXCLUDED.reasons, scored_at = NOW()`,
			org, sc.CampaignID, sc.ContentHash, sc.ISP, sc.ProfileVersion, sc.SAScore, sc.PInbox,
			string(sc.Rules), string(sc.Reasons), sc.Source,
		); err != nil {
			respondError(w, http.StatusInternalServerError, "write failed: "+err.Error())
			return
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "upserted": n})
}

// ── profiles ─────────────────────────────────────────────────────────────────

type inboxScoreProfile struct {
	ISP         string          `json:"isp"`
	Version     int             `json:"version"`
	Status      string          `json:"status"`
	TrainedAt   *string         `json:"trained_at"`
	NHam        int             `json:"n_ham"`
	NSpam       int             `json:"n_spam"`
	AUC         *float64        `json:"auc"`
	Calibration json.RawMessage `json:"calibration"`
	Notes       string          `json:"notes"`
	Finding     string          `json:"finding"` // one plain sentence from the trainer: what the ISP's own rejection codes said
}

// inboxScoreProfileBrief is the latest profile per ISP, attached to the summary
// so the portal can say why a provider has no guess.
type inboxScoreProfileBrief struct {
	Version int      `json:"version"`
	Status  string   `json:"status"`
	AUC     *float64 `json:"auc"`
	Finding string   `json:"finding"`
}

func (s *InboxScoreService) HandlePostProfile(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	var p inboxScoreProfile
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !inboxScoreValidISP(p.ISP) || p.Version <= 0 {
		respondError(w, http.StatusBadRequest, "isp must be a known profile and version > 0")
		return
	}
	switch p.Status {
	case "", "candidate":
		p.Status = "candidate"
	case "active", "retired":
	default:
		respondError(w, http.StatusBadRequest, "status must be candidate|active|retired")
		return
	}
	if len(p.Calibration) == 0 {
		p.Calibration = json.RawMessage("{}")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db unavailable")
		return
	}
	defer tx.Rollback()
	if p.Status == "active" {
		// One active profile per ISP: demote the others in the same write.
		if _, err := tx.ExecContext(ctx, `UPDATE mailing_inbox_score_profiles SET status='retired'
			WHERE organization_id=$1 AND isp=$2 AND status='active' AND version<>$3`, org, p.ISP, p.Version); err != nil {
			respondError(w, http.StatusInternalServerError, "write failed: "+err.Error())
			return
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mailing_inbox_score_profiles (organization_id, isp, version, status, trained_at, n_ham, n_spam, auc, calibration, notes, finding)
		VALUES ($1,$2,$3,$4,NOW(),$5,$6,$7,$8::jsonb,$9,$10)
		ON CONFLICT (organization_id, isp, version) DO UPDATE SET
		  status=EXCLUDED.status, trained_at=NOW(), n_ham=EXCLUDED.n_ham, n_spam=EXCLUDED.n_spam,
		  auc=EXCLUDED.auc, calibration=EXCLUDED.calibration, notes=EXCLUDED.notes, finding=EXCLUDED.finding`,
		org, p.ISP, p.Version, p.Status, p.NHam, p.NSpam, p.AUC, string(p.Calibration), p.Notes, p.Finding,
	); err != nil {
		respondError(w, http.StatusInternalServerError, "write failed: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "isp": p.ISP, "version": p.Version, "status": p.Status})
}

func (s *InboxScoreService) HandleProfiles(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT isp, version, status, trained_at, n_ham, n_spam, auc, calibration::text, notes, finding
		FROM mailing_inbox_score_profiles WHERE organization_id=$1
		ORDER BY isp, version DESC`, org)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
		return
	}
	defer rows.Close()
	out := []inboxScoreProfile{}
	for rows.Next() {
		var p inboxScoreProfile
		var trained sql.NullTime
		var auc sql.NullFloat64
		var calib string
		if err := rows.Scan(&p.ISP, &p.Version, &p.Status, &trained, &p.NHam, &p.NSpam, &auc, &calib, &p.Notes, &p.Finding); err != nil {
			respondError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		if trained.Valid {
			t := trained.Time.UTC().Format(time.RFC3339)
			p.TrainedAt = &t
		}
		if auc.Valid {
			v := auc.Float64
			p.AUC = &v
		}
		p.Calibration = json.RawMessage(calib)
		out = append(out, p)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"profiles": out, "isps": InboxScoreISPs})
}

// ── seed placements ──────────────────────────────────────────────────────────

type inboxScoreSeedPlacement struct {
	CampaignID  string          `json:"campaign_id"`
	SeedEmail   string          `json:"seed_email"`
	ISP         string          `json:"isp"`
	Folder      string          `json:"folder"`
	MessageID   string          `json:"message_id"`
	XMessageID  string          `json:"x_message_id"`
	AuthResults json.RawMessage `json:"auth_results"`
	Headers     json.RawMessage `json:"headers"`
	ObservedAt  *string         `json:"observed_at,omitempty"`
	// The defender's verdict on the delivered bytes (mailcow rspamd X-Spamd-Result):
	// score on its 0..15 scale, the action it took (no action / add header /
	// greylist / reject) and the symbols that fired. Optional: the operator's
	// own provider mailboxes carry none of this.
	DefenderScore   *float64        `json:"defender_score,omitempty"`
	DefenderAction  string          `json:"defender_action,omitempty"`
	DefenderSymbols json.RawMessage `json:"defender_symbols,omitempty"`
}

type inboxScorePostSeeds struct {
	Placements []inboxScoreSeedPlacement `json:"placements"`
}

var inboxScoreFolders = map[string]bool{"inbox": true, "promotions": true, "spam": true, "missing": true, "other": true}

func (s *InboxScoreService) HandlePostSeedPlacements(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	var body inboxScorePostSeeds
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Placements) == 0 || len(body.Placements) > 5000 {
		respondError(w, http.StatusBadRequest, "placements must hold 1..5000 rows")
		return
	}
	for i, p := range body.Placements {
		if !inboxScoreFolders[p.Folder] {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("placements[%d].folder must be inbox|promotions|spam|missing|other", i))
			return
		}
		if !inboxScoreValidISP(p.ISP) {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("placements[%d].isp %q unknown", i, p.ISP))
			return
		}
		if strings.TrimSpace(p.SeedEmail) == "" || strings.TrimSpace(p.MessageID) == "" {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("placements[%d] needs seed_email and message_id", i))
			return
		}
		if p.CampaignID != "" {
			if _, err := uuid.Parse(p.CampaignID); err != nil {
				respondError(w, http.StatusBadRequest, fmt.Sprintf("placements[%d].campaign_id is not a UUID", i))
				return
			}
		}
		if len(p.AuthResults) == 0 {
			body.Placements[i].AuthResults = json.RawMessage("{}")
		}
		if len(p.Headers) == 0 {
			body.Placements[i].Headers = json.RawMessage("{}")
		}
		if len(p.DefenderSymbols) == 0 {
			body.Placements[i].DefenderSymbols = json.RawMessage("[]")
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "db unavailable")
		return
	}
	defer tx.Rollback()
	n := 0
	for _, p := range body.Placements {
		var camp interface{}
		if p.CampaignID != "" {
			camp = p.CampaignID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_seed_placements
			  (organization_id, campaign_id, seed_email, isp, folder, message_id, x_message_id, auth_results, headers,
			   defender_score, defender_action, defender_symbols, observed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10,$11,$12::jsonb,NOW())
			ON CONFLICT (organization_id, seed_email, message_id) DO UPDATE SET
			  campaign_id = COALESCE(EXCLUDED.campaign_id, mailing_seed_placements.campaign_id),
			  folder = EXCLUDED.folder, x_message_id = EXCLUDED.x_message_id,
			  auth_results = EXCLUDED.auth_results, headers = EXCLUDED.headers,
			  defender_score = EXCLUDED.defender_score, defender_action = EXCLUDED.defender_action,
			  defender_symbols = EXCLUDED.defender_symbols, observed_at = NOW()`,
			org, camp, strings.ToLower(strings.TrimSpace(p.SeedEmail)), p.ISP, p.Folder, p.MessageID, p.XMessageID,
			string(p.AuthResults), string(p.Headers), p.DefenderScore, p.DefenderAction, string(p.DefenderSymbols),
		); err != nil {
			respondError(w, http.StatusInternalServerError, "write failed: "+err.Error())
			return
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "upserted": n})
}

// ── summary / campaign (portal reads) ───────────────────────────────────────

type inboxScoreCell struct {
	ISP            string          `json:"isp"`
	PInbox         *float64        `json:"p_inbox"`
	SAScore        *float64        `json:"sa_score"`
	ProfileVersion int             `json:"profile_version"`
	ProfileAUC     *float64        `json:"profile_auc"`
	Reasons        json.RawMessage `json:"reasons"`
	ScoredAt       *string         `json:"scored_at"`
	SeedFolder     string          `json:"seed_folder"` // "" = no seed observation yet
	SeedEmail      string          `json:"seed_email,omitempty"`
	SeedObservedAt *string         `json:"seed_observed_at,omitempty"`
	SeedScore      *float64        `json:"seed_score,omitempty"`   // defender score (rspamd, 0..15) when the seed is on mailcow
	SeedAction     string          `json:"seed_action,omitempty"`  // defender action: no action | add header | greylist | reject
	SeedSymbols    json.RawMessage `json:"seed_symbols,omitempty"` // defender symbols [{symbol, score, detail}]
}

type inboxScoreCampaignSummary struct {
	CampaignID    string           `json:"campaign_id"`
	Name          string           `json:"name"`
	SendingDomain string           `json:"sending_domain"`
	Status        string           `json:"status"`
	ScheduledAt   *string          `json:"scheduled_at"`
	Kind          string           `json:"kind"`    // offer | newsletter (inbox_score_offer.go)
	Offer         string           `json:"offer"`   // the offer the creative sells, from offer_key or the campaign name
	Subject       string           `json:"subject"` // the creative's subject line
	Cells         []inboxScoreCell `json:"cells"`
}

type inboxScoreSummaryResponse struct {
	AsOf      string                            `json:"as_of"`
	From      string                            `json:"from"`
	To        string                            `json:"to"`
	Timezone  string                            `json:"timezone"`
	ISPs      []string                          `json:"isps"`
	Campaigns []inboxScoreCampaignSummary       `json:"campaigns"`
	Profiles  map[string]inboxScoreProfileBrief `json:"profiles"` // latest profile per ISP (why a provider has or lacks a guess)
	HowToRead []string                          `json:"how_to_read"`
}

// loadProfileBriefs returns the newest profile row per ISP for the org.
func (s *InboxScoreService) loadProfileBriefs(ctx context.Context, org string) (map[string]inboxScoreProfileBrief, error) {
	out := map[string]inboxScoreProfileBrief{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (isp) isp, version, status, auc, finding
		FROM mailing_inbox_score_profiles WHERE organization_id=$1
		ORDER BY isp, version DESC`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var isp string
		var b inboxScoreProfileBrief
		var auc sql.NullFloat64
		if err := rows.Scan(&isp, &b.Version, &b.Status, &auc, &b.Finding); err != nil {
			return nil, err
		}
		if auc.Valid {
			v := auc.Float64
			b.AUC = &v
		}
		out[isp] = b
	}
	return out, nil
}

var inboxScoreHowToRead = []string{
	"Offers only, grouped by offer. Each cell is one provider's verdict on the exact email that campaign sends.",
	"Test inbox = the real verdict: our own mailbox at that provider's lane received the live send and our filter (the defender) scored it. Inbox or Junk, score out of 15: ready under 2.4, review under 4.6, fix before send under 8, junked at 8.",
	"Pre-send = the scan before sending (score on SpamAssassin's scale: ready under 1.5, review under 3, fix under 5, fail at 5). It is the same copy for every provider, so it only differs by provider once a provider's own model is trusted.",
	"Click a row for the reasons in plain words. Reasons shared by every provider are shown once.",
}

func inboxScoreDenverDay(s string, now time.Time) (time.Time, error) {
	loc, _ := time.LoadLocation("America/Denver")
	if s == "" {
		y, m, d := now.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc), nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("dates must be YYYY-MM-DD")
	}
	return t, nil
}

// HandleSummary lists campaigns scheduled in [from, to] (Denver days) with
// one cell per ISP: latest render score and latest seed observation.
func (s *InboxScoreService) HandleSummary(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	now := s.now()
	from, err := inboxScoreDenverDay(r.URL.Query().Get("from"), now)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	to, err := inboxScoreDenverDay(r.URL.Query().Get("to"), now)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if to.Before(from) {
		respondError(w, http.StatusBadRequest, "to must not be before from")
		return
	}
	if to.Sub(from) > 31*24*time.Hour {
		respondError(w, http.StatusBadRequest, "window must be 31 days or less")
		return
	}
	end := to.Add(24 * time.Hour)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, COALESCE(c.name,''), COALESCE(sp.sending_domain,''), COALESCE(c.status,''), c.scheduled_at,
		       COALESCE(c.offer_key,''), COALESCE(c.subject,'')
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		WHERE c.organization_id = $1 AND c.scheduled_at >= $2 AND c.scheduled_at < $3
		  AND c.status NOT IN ('cancelled','deleted')
		  AND (EXISTS (SELECT 1 FROM mailing_inbox_scores sc WHERE sc.organization_id=c.organization_id AND sc.campaign_id=c.id)
		       OR EXISTS (SELECT 1 FROM mailing_seed_placements pl WHERE pl.organization_id=c.organization_id AND pl.campaign_id=c.id))
		ORDER BY c.scheduled_at, c.name
		LIMIT 2000`, org, from, end)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
		return
	}
	defer rows.Close()
	campaigns := []inboxScoreCampaignSummary{}
	ids := []string{}
	for rows.Next() {
		var cs inboxScoreCampaignSummary
		var sched sql.NullTime
		var offerKey string
		if err := rows.Scan(&cs.CampaignID, &cs.Name, &cs.SendingDomain, &cs.Status, &sched, &offerKey, &cs.Subject); err != nil {
			respondError(w, http.StatusInternalServerError, "scan failed")
			return
		}
		cs.Kind = inboxScoreKind(cs.Name)
		cs.Offer = inboxScoreOffer(cs.Name, offerKey)
		if sched.Valid {
			loc, _ := time.LoadLocation("America/Denver")
			t := sched.Time.In(loc).Format(time.RFC3339)
			cs.ScheduledAt = &t
		}
		campaigns = append(campaigns, cs)
		ids = append(ids, cs.CampaignID)
	}
	cellsByCampaign, err := s.loadCells(ctx, org, ids)
	briefs, berr := s.loadProfileBriefs(ctx, org)
	if berr != nil {
		respondError(w, http.StatusInternalServerError, "read failed: "+berr.Error())
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
		return
	}
	for i := range campaigns {
		campaigns[i].Cells = cellsByCampaign[campaigns[i].CampaignID]
	}
	respondJSON(w, http.StatusOK, inboxScoreSummaryResponse{
		AsOf: now.UTC().Format(time.RFC3339), From: from.Format("2006-01-02"), To: to.Format("2006-01-02"),
		Timezone: "America/Denver", ISPs: InboxScoreISPs, Campaigns: campaigns, Profiles: briefs, HowToRead: inboxScoreHowToRead,
	})
}

// HandleCampaign returns the cells for one campaign (same shape as summary).
func (s *InboxScoreService) HandleCampaign(w http.ResponseWriter, r *http.Request) {
	org, ok := inboxScoreOrg(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		respondError(w, http.StatusBadRequest, "id is not a UUID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var cs inboxScoreCampaignSummary
	var sched sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT c.id, COALESCE(c.name,''), COALESCE(sp.sending_domain,''), COALESCE(c.status,''), c.scheduled_at
		FROM mailing_campaigns c LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		WHERE c.organization_id=$1 AND c.id=$2`, org, id).Scan(&cs.CampaignID, &cs.Name, &cs.SendingDomain, &cs.Status, &sched)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
		return
	}
	if sched.Valid {
		loc, _ := time.LoadLocation("America/Denver")
		t := sched.Time.In(loc).Format(time.RFC3339)
		cs.ScheduledAt = &t
	}
	cells, err := s.loadCells(ctx, org, []string{id})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
		return
	}
	cs.Cells = cells[id]
	respondJSON(w, http.StatusOK, map[string]interface{}{"campaign": cs, "isps": InboxScoreISPs, "how_to_read": inboxScoreHowToRead})
}

// loadCells assembles one cell per ISP per campaign from the latest render
// score and the latest seed observation. Campaigns with nothing get a cell
// per ISP with empty values so the portal grid stays rectangular.
func (s *InboxScoreService) loadCells(ctx context.Context, org string, ids []string) (map[string][]inboxScoreCell, error) {
	out := make(map[string][]inboxScoreCell, len(ids))
	type key struct{ camp, isp string }
	cells := map[key]*inboxScoreCell{}
	for _, id := range ids {
		for _, isp := range InboxScoreISPs {
			c := &inboxScoreCell{ISP: isp, Reasons: json.RawMessage("[]")}
			cells[key{id, isp}] = c
		}
	}
	if len(ids) > 0 {
		rows, err := s.db.QueryContext(ctx, `
			SELECT sc.campaign_id, sc.isp, sc.sa_score, sc.p_inbox, sc.profile_version, sc.reasons::text, sc.scored_at,
			       pr.auc
			FROM mailing_inbox_scores sc
			LEFT JOIN mailing_inbox_score_profiles pr
			  ON pr.organization_id = sc.organization_id AND pr.isp = sc.isp AND pr.version = sc.profile_version
			WHERE sc.organization_id = $1 AND sc.source = 'render' AND sc.campaign_id = ANY($2::uuid[])`,
			org, pq.Array(ids))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var camp, isp, reasons string
			var sa, p, auc sql.NullFloat64
			var ver int
			var at sql.NullTime
			if err := rows.Scan(&camp, &isp, &sa, &p, &ver, &reasons, &at, &auc); err != nil {
				rows.Close()
				return nil, err
			}
			c, ok := cells[key{camp, isp}]
			if !ok {
				continue
			}
			if sa.Valid {
				v := sa.Float64
				c.SAScore = &v
			}
			if p.Valid {
				v := p.Float64
				c.PInbox = &v
			}
			if auc.Valid {
				v := auc.Float64
				c.ProfileAUC = &v
			}
			c.ProfileVersion = ver
			c.Reasons = json.RawMessage(reasons)
			if at.Valid {
				t := at.Time.UTC().Format(time.RFC3339)
				c.ScoredAt = &t
			}
		}
		rows.Close()

		srows, err := s.db.QueryContext(ctx, `
			SELECT DISTINCT ON (campaign_id, isp) campaign_id, isp, folder, seed_email, observed_at, defender_score, defender_action, defender_symbols
			FROM mailing_seed_placements
			WHERE organization_id = $1 AND campaign_id = ANY($2::uuid[])
			ORDER BY campaign_id, isp, observed_at DESC`, org, pq.Array(ids))
		if err != nil {
			return nil, err
		}
		for srows.Next() {
			var camp, isp, folder, email string
			var at sql.NullTime
			var score sql.NullFloat64
			var action sql.NullString
			var symbols []byte
			if err := srows.Scan(&camp, &isp, &folder, &email, &at, &score, &action, &symbols); err != nil {
				srows.Close()
				return nil, err
			}
			if c, ok := cells[key{camp, isp}]; ok {
				c.SeedFolder = folder
				c.SeedEmail = email
				if at.Valid {
					t := at.Time.UTC().Format(time.RFC3339)
					c.SeedObservedAt = &t
				}
				if score.Valid {
					v := score.Float64
					c.SeedScore = &v
				}
				if action.Valid {
					c.SeedAction = action.String
				}
				if len(symbols) > 0 {
					c.SeedSymbols = json.RawMessage(symbols)
				}
			}
		}
		srows.Close()
	}
	for _, id := range ids {
		list := make([]inboxScoreCell, 0, len(InboxScoreISPs))
		for _, isp := range InboxScoreISPs {
			list = append(list, *cells[key{id, isp}])
		}
		sort.SliceStable(list, func(i, j int) bool { return inboxScoreISPOrder(list[i].ISP) < inboxScoreISPOrder(list[j].ISP) })
		out[id] = list
	}
	return out, nil
}

func inboxScoreISPOrder(isp string) int {
	for i, k := range InboxScoreISPs {
		if k == isp {
			return i
		}
	}
	return len(InboxScoreISPs)
}
