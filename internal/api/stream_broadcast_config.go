package api

// =============================================================================
// FRESH BROADCAST CONFIG — /api/mailing/stream-broadcast/config
// =============================================================================
// Single source of truth for the fresh-introduction broadcast program. The
// operator edits per-stream knobs (enabled, daily_cap, isp_caps, offer,
// throttle_hours) on the portal screen; the Python build pipeline reads the
// SAME table (mailing_stream_broadcast_config — table/column names are a
// cross-team CONTRACT, seeded in cmd/server/main.go
// create_stream_broadcast_config / seed_stream_broadcast_config).
//
// Read-only per stream (mirrored from agents/scheduling/data/
// stream_routing.json at seed time): label, seg_prefix, vertical_tag,
// dataset_ids, primary_sites, secondary_sites, eo_mailable.
//
// PUT validation:
//   - stream must exist (404)
//   - daily_cap >= 0; every isp_caps value >= 0; throttle_hours in [1,24]
//   - offer must resolve to a mailing_offers row (400 otherwise). Resolution
//     is transparent (matched_by in the bench payload): exact
//     landing_page_slug, else exact slugified name, else slugified-name
//     prefix. status != 'active' is a WARNING in the response, not an error
//     (fidelity is currently 'draft' — the operator must see that, not be
//     blocked by it).
//   - optimistic lock: the caller echoes the row's updated_at; a mismatch is
//     HTTP 409 (someone else saved first), never a silent overwrite.
//
// OFFER BENCH: the operator-approved intro bench (streamOfferBench below).
// GET returns a READINESS light per bench offer from live data: offer row
// present, status, active proofs (mailing_offer_proofs.offer_key), smartlink
// seeded (mailing_smart_links.slug). An offer with no row (liz-buys-homes
// today) renders NOT ONBOARDED and the API refuses it on PUT.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// VersionStreamBroadcastAPI is bumped on every response-shape change.
const VersionStreamBroadcastAPI = "1.0"

// streamOfferBench is the operator's approved intro bench (coordinator brief
// 2026-07-26). Order is display order.
var streamOfferBench = []string{
	"fidelity", "liberty-mutual", "tahiti-village", "3-day-blinds",
	"mutual-of-omaha", "liz-buys-homes", "west-capital-heloc",
}

const (
	streamThrottleMin = 1
	streamThrottleMax = 24
)

// Bench readiness levels (JSON contract with the screen).
const (
	benchReady        = "ready"
	benchAttention    = "attention"
	benchNotOnboarded = "not_onboarded"
)

// ── Pure derivations (unit-tested) ──────────────────────────────────────────

var streamSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugifyOfferName turns a mailing_offers.name into a slug comparable with an
// offer key ("3 Day Blinds" → "3-day-blinds").
func slugifyOfferName(name string) string {
	s := streamSlugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	return strings.Trim(s, "-")
}

// benchReadinessLevel derives the light: no row = NOT ONBOARDED (red); row
// present but not-active status, no active proofs, or no smartlink = needs
// ATTENTION (amber); everything present = READY (green).
func benchReadinessLevel(exists bool, status string, proofs, smartlinks int) string {
	if !exists {
		return benchNotOnboarded
	}
	if status != "active" || proofs <= 0 || smartlinks <= 0 {
		return benchAttention
	}
	return benchReady
}

// validateStreamConfigWrite checks the editable fields. Returns "" when valid.
func validateStreamConfigWrite(dailyCap, throttleHours int, ispCaps map[string]int, offer string) string {
	if strings.TrimSpace(offer) == "" {
		return "offer is required"
	}
	if dailyCap < 0 {
		return "daily_cap must be >= 0"
	}
	if throttleHours < streamThrottleMin || throttleHours > streamThrottleMax {
		return fmt.Sprintf("throttle_hours must be between %d and %d", streamThrottleMin, streamThrottleMax)
	}
	for isp, cap := range ispCaps {
		if cap < 0 {
			return fmt.Sprintf("isp_caps[%s] must be >= 0", isp)
		}
	}
	return ""
}

// ── JSON shapes ─────────────────────────────────────────────────────────────

type streamConfigRow struct {
	StreamKey      string          `json:"stream_key"`
	Enabled        bool            `json:"enabled"`
	DailyCap       int             `json:"daily_cap"`
	ISPCaps        map[string]int  `json:"isp_caps"`
	Offer          string          `json:"offer"`
	ThrottleHours  int             `json:"throttle_hours"`
	Label          string          `json:"label"`
	SegPrefix      string          `json:"seg_prefix"`
	VerticalTag    *string         `json:"vertical_tag"`
	DatasetIDs     json.RawMessage `json:"dataset_ids"`
	PrimarySites   json.RawMessage `json:"primary_sites"`
	SecondarySites json.RawMessage `json:"secondary_sites"`
	EOMailable     json.RawMessage `json:"eo_mailable"`
	// SendingDomain / SendingProfileID pin a stream to its OWN sending lane
	// (explicit-domain streams, e.g. wcm → m.wcl-heloc.com). Nullable;
	// read-only through this API for now (PUT does not write them).
	SendingDomain    *string   `json:"sending_domain"`
	SendingProfileID *string   `json:"sending_profile_id"`
	UpdatedAt        time.Time `json:"updated_at"`
	UpdatedBy        string    `json:"updated_by"`
}

type benchOfferStatus struct {
	Key        string `json:"key"`
	Exists     bool   `json:"exists"`
	OfferID    string `json:"offer_id"`   // "" when not onboarded
	OfferName  string `json:"offer_name"` // "" when not onboarded
	Status     string `json:"status"`     // mailing_offers.status; "" when absent
	MatchedBy  string `json:"matched_by"` // slug | name | name_prefix | ""
	Proofs     int    `json:"proofs"`     // active mailing_offer_proofs rows
	SmartLinks int    `json:"smart_links"`
	Readiness  string `json:"readiness"` // ready | attention | not_onboarded
}

type streamConfigResponse struct {
	APIVersion  string             `json:"api_version"`
	GeneratedAt string             `json:"generated_at"`
	Streams     []streamConfigRow  `json:"streams"`
	Bench       []benchOfferStatus `json:"bench"`
	// BenchAvailable=false: a readiness source failed — lights are absent,
	// not silently green.
	BenchAvailable bool `json:"bench_available"`
}

type streamConfigPutRequest struct {
	StreamKey     string         `json:"stream_key"`
	Enabled       bool           `json:"enabled"`
	DailyCap      int            `json:"daily_cap"`
	ISPCaps       map[string]int `json:"isp_caps"`
	Offer         string         `json:"offer"`
	ThrottleHours int            `json:"throttle_hours"`
	// ExpectedUpdatedAt echoes the row's updated_at from GET (optimistic
	// lock; RFC3339Nano). Mismatch = 409.
	ExpectedUpdatedAt time.Time `json:"expected_updated_at"`
	UpdatedBy         string    `json:"updated_by"`
}

type streamConfigPutResponse struct {
	Stream   streamConfigRow `json:"stream"`
	Warnings []string        `json:"warnings"`
}

// ── Store ───────────────────────────────────────────────────────────────────

// StreamBroadcastStore is the data access layer (no raw SQL in handlers).
type StreamBroadcastStore struct {
	db *sql.DB
}

func NewStreamBroadcastStore(db *sql.DB) *StreamBroadcastStore {
	return &StreamBroadcastStore{db: db}
}

const streamConfigCols = `stream_key, enabled, daily_cap, isp_caps::text, offer, throttle_hours,
	label, seg_prefix, vertical_tag, dataset_ids::text, primary_sites::text,
	secondary_sites::text, eo_mailable::text, sending_domain, sending_profile_id,
	updated_at, updated_by`

func scanStreamConfigRow(sc interface{ Scan(...any) error }) (streamConfigRow, error) {
	var row streamConfigRow
	var ispCaps, datasets, primary, secondary, eo string
	var vertical, sendDomain, sendProfile sql.NullString
	if err := sc.Scan(&row.StreamKey, &row.Enabled, &row.DailyCap, &ispCaps, &row.Offer,
		&row.ThrottleHours, &row.Label, &row.SegPrefix, &vertical, &datasets,
		&primary, &secondary, &eo, &sendDomain, &sendProfile,
		&row.UpdatedAt, &row.UpdatedBy); err != nil {
		return row, err
	}
	if vertical.Valid {
		v := vertical.String
		row.VerticalTag = &v
	}
	if sendDomain.Valid {
		v := sendDomain.String
		row.SendingDomain = &v
	}
	if sendProfile.Valid {
		v := sendProfile.String
		row.SendingProfileID = &v
	}
	row.ISPCaps = map[string]int{}
	if err := json.Unmarshal([]byte(ispCaps), &row.ISPCaps); err != nil {
		// A malformed blob must not blank the screen — surface empty caps.
		row.ISPCaps = map[string]int{}
	}
	row.DatasetIDs = json.RawMessage(datasets)
	row.PrimarySites = json.RawMessage(primary)
	row.SecondarySites = json.RawMessage(secondary)
	row.EOMailable = json.RawMessage(eo)
	return row, nil
}

// List returns every stream row for the org, stable order.
func (s *StreamBroadcastStore) List(ctx context.Context, orgID uuid.UUID) ([]streamConfigRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+streamConfigCols+`
		FROM mailing_stream_broadcast_config
		WHERE organization_id = $1
		ORDER BY stream_key
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]streamConfigRow, 0, 8)
	for rows.Next() {
		row, err := scanStreamConfigRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// resolvedOffer is one mailing_offers match for an offer key.
type resolvedOffer struct {
	ID        string
	Name      string
	Status    string
	MatchedBy string // slug | name | name_prefix
}

// ResolveOffer maps an offer key to a mailing_offers row, transparently:
// 1. landing_page_slug = key            (matched_by 'slug')
// 2. slugified(name) = key              (matched_by 'name')
// 3. slugified(name) starts with key-   (matched_by 'name_prefix' — e.g.
//    'liberty-mutual' → "Liberty Mutual Insurance")
// Returns (nil, nil) when no row matches — the NOT ONBOARDED state.
func (s *StreamBroadcastStore) ResolveOffer(ctx context.Context, orgID uuid.UUID, key string) (*resolvedOffer, error) {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, COALESCE(landing_page_slug, ''), COALESCE(status, 'draft')
		FROM mailing_offers
		WHERE organization_id = $1
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var best *resolvedOffer
	rank := func(matchedBy string) int {
		switch matchedBy {
		case "slug":
			return 0
		case "name":
			return 1
		default:
			return 2
		}
	}
	for rows.Next() {
		var id, name, slug, status string
		if err := rows.Scan(&id, &name, &slug, &status); err != nil {
			return nil, err
		}
		var matchedBy string
		nameSlug := slugifyOfferName(name)
		switch {
		case strings.ToLower(strings.TrimSpace(slug)) == k:
			matchedBy = "slug"
		case nameSlug == k:
			matchedBy = "name"
		case strings.HasPrefix(nameSlug, k+"-"):
			matchedBy = "name_prefix"
		default:
			continue
		}
		cand := &resolvedOffer{ID: id, Name: name, Status: status, MatchedBy: matchedBy}
		if best == nil || rank(cand.MatchedBy) < rank(best.MatchedBy) {
			best = cand
		}
	}
	return best, rows.Err()
}

// BenchCounts returns (active proofs, smartlink rows) for an offer key.
// mailing_offer_proofs is org-scoped by column; mailing_smart_links is
// platform-global (no org column — same as the smart-links screen).
func (s *StreamBroadcastStore) BenchCounts(ctx context.Context, orgID uuid.UUID, key string) (proofs, smartlinks int, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM mailing_offer_proofs
			  WHERE organization_id = $1 AND offer_key = $2 AND is_active),
			(SELECT COUNT(*) FROM mailing_smart_links WHERE slug = $2)
	`, orgID, key).Scan(&proofs, &smartlinks)
	return proofs, smartlinks, err
}

// Update writes the editable fields under the optimistic lock. Returns
// sql.ErrNoRows when nothing matched — the caller distinguishes 404 vs 409
// via Exists.
func (s *StreamBroadcastStore) Update(ctx context.Context, orgID uuid.UUID, in streamConfigPutRequest, ispCapsJSON []byte) (streamConfigRow, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE mailing_stream_broadcast_config SET
			enabled = $4, daily_cap = $5, isp_caps = $6::jsonb, offer = $7,
			throttle_hours = $8, updated_at = NOW(), updated_by = $9
		WHERE organization_id = $1 AND stream_key = $2 AND updated_at = $3
		RETURNING `+streamConfigCols+`
	`, orgID, in.StreamKey, in.ExpectedUpdatedAt, in.Enabled, in.DailyCap,
		string(ispCapsJSON), strings.TrimSpace(in.Offer), in.ThrottleHours, in.UpdatedBy)
	return scanStreamConfigRow(row)
}

// Exists reports whether the stream row exists at all (409-vs-404 probe).
func (s *StreamBroadcastStore) Exists(ctx context.Context, orgID uuid.UUID, streamKey string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM mailing_stream_broadcast_config
		WHERE organization_id = $1 AND stream_key = $2
	`, orgID, streamKey).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// ── Service / handlers ──────────────────────────────────────────────────────

type StreamBroadcastService struct {
	store *StreamBroadcastStore
}

func NewStreamBroadcastService(db *sql.DB) *StreamBroadcastService {
	return &StreamBroadcastService{store: NewStreamBroadcastStore(db)}
}

// RegisterRoutes mounts the service under the /api/mailing group.
func (s *StreamBroadcastService) RegisterRoutes(r chi.Router) {
	r.Route("/stream-broadcast", func(r chi.Router) {
		r.Get("/config", s.HandleGetConfig)
		r.Put("/config", s.HandlePutConfig)
	})
}

// buildBench derives the readiness light for every bench offer. Each offer
// degrades independently; a failed source sets benchAvailable=false rather
// than rendering a silently-green light.
func (s *StreamBroadcastService) buildBench(ctx context.Context, orgID uuid.UUID) ([]benchOfferStatus, bool) {
	out := make([]benchOfferStatus, 0, len(streamOfferBench))
	available := true
	for _, key := range streamOfferBench {
		b := benchOfferStatus{Key: key}
		res, err := s.store.ResolveOffer(ctx, orgID, key)
		if err != nil {
			log.Printf("[stream-broadcast] bench resolve %s: %v", key, err)
			available = false
			break
		}
		if res != nil {
			b.Exists = true
			b.OfferID = res.ID
			b.OfferName = res.Name
			b.Status = res.Status
			b.MatchedBy = res.MatchedBy
			proofs, smartlinks, err := s.store.BenchCounts(ctx, orgID, key)
			if err != nil {
				log.Printf("[stream-broadcast] bench counts %s: %v", key, err)
				available = false
				break
			}
			b.Proofs = proofs
			b.SmartLinks = smartlinks
		}
		b.Readiness = benchReadinessLevel(b.Exists, b.Status, b.Proofs, b.SmartLinks)
		out = append(out, b)
	}
	if !available {
		return nil, false
	}
	return out, true
}

// HandleGetConfig handles GET /api/mailing/stream-broadcast/config.
func (s *StreamBroadcastService) HandleGetConfig(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	streams, err := s.store.List(ctx, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "stream broadcast config: "+err.Error())
		return
	}
	bench, benchOK := s.buildBench(ctx, orgID)
	if bench == nil {
		bench = []benchOfferStatus{}
	}
	respondJSON(w, http.StatusOK, streamConfigResponse{
		APIVersion:     VersionStreamBroadcastAPI,
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Streams:        streams,
		Bench:          bench,
		BenchAvailable: benchOK,
	})
}

// HandlePutConfig handles PUT /api/mailing/stream-broadcast/config.
func (s *StreamBroadcastService) HandlePutConfig(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	var in streamConfigPutRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	in.StreamKey = strings.TrimSpace(in.StreamKey)
	if in.StreamKey == "" {
		respondError(w, http.StatusBadRequest, "stream_key is required")
		return
	}
	if in.ISPCaps == nil {
		in.ISPCaps = map[string]int{}
	}
	if msg := validateStreamConfigWrite(in.DailyCap, in.ThrottleHours, in.ISPCaps, in.Offer); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	if in.ExpectedUpdatedAt.IsZero() {
		respondError(w, http.StatusBadRequest, "expected_updated_at is required (echo the row's updated_at from GET)")
		return
	}
	if in.UpdatedBy = strings.TrimSpace(in.UpdatedBy); in.UpdatedBy == "" {
		in.UpdatedBy = strings.TrimSpace(r.Header.Get("X-User-Email"))
	}
	if in.UpdatedBy == "" {
		in.UpdatedBy = "portal"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Offer must be onboarded; non-active status is a warning, not a block.
	offer, err := s.store.ResolveOffer(ctx, orgID, in.Offer)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "resolve offer: "+err.Error())
		return
	}
	if offer == nil {
		respondError(w, http.StatusBadRequest,
			fmt.Sprintf("offer '%s' is not onboarded in mailing_offers — onboard it before assigning it to a stream", in.Offer))
		return
	}
	warnings := []string{}
	if offer.Status != "active" {
		warnings = append(warnings, fmt.Sprintf(
			"offer '%s' resolved to %q but its status is '%s' (not 'active') — the build pipeline may refuse it until it is activated",
			in.Offer, offer.Name, offer.Status))
	}

	ispCapsJSON, err := json.Marshal(in.ISPCaps)
	if err != nil {
		respondError(w, http.StatusBadRequest, "isp_caps not serializable: "+err.Error())
		return
	}

	row, err := s.store.Update(ctx, orgID, in, ispCapsJSON)
	if err == sql.ErrNoRows {
		exists, exErr := s.store.Exists(ctx, orgID, in.StreamKey)
		if exErr != nil {
			respondError(w, http.StatusInternalServerError, "stream lookup: "+exErr.Error())
			return
		}
		if !exists {
			respondError(w, http.StatusNotFound, "stream not found: "+in.StreamKey)
			return
		}
		respondError(w, http.StatusConflict,
			"stream config changed since you loaded it (optimistic-lock mismatch) — reload and re-apply your edit")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "update stream config: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, streamConfigPutResponse{Stream: row, Warnings: warnings})
}
