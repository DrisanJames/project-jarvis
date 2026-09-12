package api

// Site contract (2026-09-12) — ONE server-to-server contract every brand site
// uses to push subscriber events to the mailing platform. It replaces the
// per-site integrations (the signup Lambda for 21 static sites, three Next.js
// apps, QuizFiesta), each of which held the full platform admin key and
// hardcoded its own list/profile/journey ids. Operator 2026-09-12: "ensure the
// sites can push a uniform contract to our mailing platform … that way we can
// just connect to the platform and honor frequency".
//
//	GET  /api/sites/v1/schema               public: the contract, machine-readable
//	POST /api/sites/v1/events               X-Site-Key: sk_<32> (one key per site)
//	GET  /api/mailing/site-keys             admin: keys (prefix only)
//	POST /api/mailing/site-keys             admin: mint a key for a site (raw key returned once)
//	POST /api/mailing/site-keys/{id}/revoke admin: revoke a key
//
// Event types:
//
//	subscribe.confirmed  a signup the site has confirmed (double opt-in): add the
//	                     address to the site's list. A suppressed address is never
//	                     re-added.
//	preferences.updated  the FULL preference state for the site's brand — frequency
//	                     (normal|reduced|weekly), paused_until, topics. Written to
//	                     mailing_subscriber_preferences (the store the send path
//	                     reads); fields left out reset to their defaults.
//	unsubscribe          scope "brand" (default) = brand-scoped suppression;
//	                     "all" = global suppression + subscriber status.
//
// The key decides the site and the list; a payload naming another site is
// refused. Idempotent on (site, event_id): a repeat returns duplicate=true and
// changes nothing; a failed apply forgets the event so the site can retry.
// dry_run=true validates, resolves and returns the plan without writing.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/preferences"
)

const (
	siteKeyHeader       = "X-Site-Key"
	siteKeyPrefix       = "sk_"
	SiteContractVersion = "2026-09-12.v1"
	siteEventMaxBody    = 16 << 10
	siteMaxTopics       = 50
)

// Site event types.
const (
	SiteEventSubscribeConfirmed = "subscribe.confirmed"
	SiteEventPreferencesUpdated = "preferences.updated"
	SiteEventUnsubscribe        = "unsubscribe"
)

var (
	siteEventTypes = map[string]bool{SiteEventSubscribeConfirmed: true, SiteEventPreferencesUpdated: true, SiteEventUnsubscribe: true}
	siteEventIDRE  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)
	siteTopicRE    = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

	errSiteKeyNotFound = errors.New("site key not found or revoked")
)

// SiteEvent is the one envelope every site sends.
type SiteEvent struct {
	EventID     string           `json:"event_id"`
	Type        string           `json:"type"`
	Site        string           `json:"site"`
	OccurredAt  *time.Time       `json:"occurred_at,omitempty"`
	DryRun      bool             `json:"dry_run,omitempty"`
	Subscriber  SiteSubscriber   `json:"subscriber"`
	Preferences *SitePreferences `json:"preferences,omitempty"`
	Unsubscribe *SiteUnsubscribe `json:"unsubscribe,omitempty"`
}

// SiteSubscriber identifies the person.
type SiteSubscriber struct {
	Email     string `json:"email"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// SitePreferences is the full preference state for the site's brand.
type SitePreferences struct {
	Frequency   string          `json:"frequency,omitempty"`
	PausedUntil *time.Time      `json:"paused_until,omitempty"`
	Topics      map[string]bool `json:"topics,omitempty"`
}

// SiteUnsubscribe scopes an unsubscribe.
type SiteUnsubscribe struct {
	Scope  string `json:"scope,omitempty"` // brand (default) | all
	Reason string `json:"reason,omitempty"`
}

// siteKey is a resolved X-Site-Key.
type siteKey struct {
	ID, OrgID, Site, ListID string
}

type siteContractStore interface {
	ResolveKey(ctx context.Context, keyHash string) (siteKey, error)
	RecordEvent(ctx context.Context, k siteKey, ev SiteEvent, emailHash string) (fresh bool, err error)
	ForgetEvent(ctx context.Context, k siteKey, eventID string)
	SaveResult(ctx context.Context, k siteKey, eventID string, result map[string]any)
	AddSubscriber(ctx context.Context, k siteKey, email, first, last string) (inserted bool, err error)
	MarkUnsubscribed(ctx context.Context, orgID, email string) error
	UpsertPreference(ctx context.Context, p preferences.Preference) error
}

// SiteContractHandler serves POST /api/sites/v1/events.
type SiteContractHandler struct {
	store siteContractStore
	hub   preferenceSuppressor
	now   func() time.Time
}

// siteContractHandler is published once the suppression hub exists (an
// unsubscribe cannot be honoured without it); until then the route answers 503.
var siteContractHandler atomic.Pointer[SiteContractHandler]

// NewSiteContractHandler wires the Postgres store and the live suppression hub.
func NewSiteContractHandler(db *sql.DB, hub preferenceSuppressor) *SiteContractHandler {
	return &SiteContractHandler{store: &pgSiteContractStore{db: db}, hub: hub, now: time.Now}
}

func siteErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}, "contract": SiteContractVersion})
}

func normalizeSite(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "www.")
}

// validateSiteEvent returns ("", "") when ev is acceptable for key k.
func validateSiteEvent(ev *SiteEvent, k siteKey, now time.Time) (code, msg string) {
	if !siteEventIDRE.MatchString(ev.EventID) {
		return "invalid_event_id", "event_id is required: 1-100 characters of A-Z a-z 0-9 . _ : -"
	}
	if !siteEventTypes[ev.Type] {
		return "invalid_type", "type must be subscribe.confirmed, preferences.updated or unsubscribe"
	}
	if normalizeSite(ev.Site) != k.Site {
		return "site_mismatch", fmt.Sprintf("this key belongs to %s; site was %q", k.Site, ev.Site)
	}
	email := strings.ToLower(strings.TrimSpace(ev.Subscriber.Email))
	if at := strings.LastIndex(email, "@"); at < 1 || at == len(email)-1 || len(email) > 254 || strings.ContainsAny(email, " \t\r\n<>") {
		return "invalid_email", "subscriber.email must be a single email address"
	}
	switch ev.Type {
	case SiteEventPreferencesUpdated:
		p := ev.Preferences
		if p == nil {
			return "missing_preferences", "preferences.updated needs a preferences object (the full state for this brand)"
		}
		if p.Frequency != "" && !preferences.ValidFrequency(p.Frequency) {
			return "invalid_frequency", "preferences.frequency must be normal, reduced or weekly"
		}
		if p.PausedUntil != nil && (!p.PausedUntil.After(now) || p.PausedUntil.After(now.AddDate(1, 0, 1))) {
			return "invalid_paused_until", "preferences.paused_until must be in the future and within a year"
		}
		if len(p.Topics) > siteMaxTopics {
			return "invalid_topics", fmt.Sprintf("at most %d topics", siteMaxTopics)
		}
		for t := range p.Topics {
			if !siteTopicRE.MatchString(t) {
				return "invalid_topics", fmt.Sprintf("topic %q: lowercase a-z 0-9 _ -, 1-64 characters", t)
			}
		}
	case SiteEventUnsubscribe:
		if u := ev.Unsubscribe; u != nil && u.Scope != "" && u.Scope != "brand" && u.Scope != "all" {
			return "invalid_scope", "unsubscribe.scope must be brand or all"
		}
	}
	return "", ""
}

func sitePlan(ev SiteEvent, brandRoot string) string {
	switch ev.Type {
	case SiteEventSubscribeConfirmed:
		return "add to the site list unless the address is suppressed"
	case SiteEventPreferencesUpdated:
		return "save frequency / pause / topics for " + brandRoot
	default:
		if ev.Unsubscribe != nil && ev.Unsubscribe.Scope == "all" {
			return "suppress globally and mark the subscriber unsubscribed"
		}
		return "suppress for " + brandRoot
	}
}

// HandleEvent is POST /api/sites/v1/events.
func (h *SiteContractHandler) HandleEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	raw := strings.TrimSpace(r.Header.Get(siteKeyHeader))
	if !strings.HasPrefix(raw, siteKeyPrefix) || len(raw) < len(siteKeyPrefix)+20 {
		siteErr(w, http.StatusUnauthorized, "invalid_site_key", "send the site's key in the X-Site-Key header (sk_…)")
		return
	}
	k, err := h.store.ResolveKey(ctx, HashPartnerKey(raw))
	if err != nil {
		if errors.Is(err, errSiteKeyNotFound) {
			siteErr(w, http.StatusUnauthorized, "invalid_site_key", "the site key is not recognised or has been revoked")
			return
		}
		log.Printf("[site-contract] key lookup failed: %v", err)
		siteErr(w, http.StatusServiceUnavailable, "unavailable", "the platform could not check the key; retry")
		return
	}
	var ev SiteEvent
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, siteEventMaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		siteErr(w, http.StatusBadRequest, "invalid_json", "body must be one JSON event object: "+clipString(err.Error(), 200))
		return
	}
	if code, msg := validateSiteEvent(&ev, k, h.now()); code != "" {
		status := http.StatusBadRequest
		if code == "site_mismatch" {
			status = http.StatusForbidden
		}
		siteErr(w, status, code, msg)
		return
	}
	email := strings.ToLower(strings.TrimSpace(ev.Subscriber.Email))
	brandRoot := brand.Root(k.Site)
	if ev.DryRun {
		respondJSON(w, http.StatusOK, map[string]any{"accepted": true, "dry_run": true, "event_id": ev.EventID, "type": ev.Type,
			"site": k.Site, "brand_root": brandRoot, "plan": sitePlan(ev, brandRoot), "contract": SiteContractVersion})
		return
	}
	fresh, err := h.store.RecordEvent(ctx, k, ev, preferences.EmailHash(email))
	if err != nil {
		log.Printf("[site-contract] record failed site=%s event=%s: %v", k.Site, ev.EventID, err)
		siteErr(w, http.StatusServiceUnavailable, "unavailable", "the platform could not record the event; retry with the same event_id")
		return
	}
	if !fresh {
		respondJSON(w, http.StatusOK, map[string]any{"accepted": true, "duplicate": true, "event_id": ev.EventID, "contract": SiteContractVersion})
		return
	}
	applied, detail, err := h.apply(ctx, r, k, ev, email, brandRoot)
	if err != nil {
		h.store.ForgetEvent(ctx, k, ev.EventID) // let the site retry with the same event_id
		log.Printf("[site-contract] apply failed site=%s event=%s type=%s: %v", k.Site, ev.EventID, ev.Type, err)
		siteErr(w, http.StatusInternalServerError, "apply_failed", "the event was not applied; retry with the same event_id")
		return
	}
	result := map[string]any{"accepted": true, "event_id": ev.EventID, "type": ev.Type, "applied": applied, "detail": detail,
		"brand_root": brandRoot, "contract": SiteContractVersion}
	h.store.SaveResult(ctx, k, ev.EventID, result)
	log.Printf("[site-contract] site=%s event=%s type=%s applied=%v: %s", k.Site, ev.EventID, ev.Type, applied, detail)
	respondJSON(w, http.StatusAccepted, result)
}

func (h *SiteContractHandler) apply(ctx context.Context, r *http.Request, k siteKey, ev SiteEvent, email, brandRoot string) (bool, string, error) {
	switch ev.Type {
	case SiteEventSubscribeConfirmed:
		if d := mailing.ClassifyEmailForIngest(email); !d.Accept {
			return false, "rejected by the ingest guard: " + d.Reason, nil
		}
		if h.hub != nil && (h.hub.IsSuppressed(email) || h.hub.IsSuppressedForBrand(email, brandRoot)) {
			return false, "address is suppressed — not re-added", nil
		}
		inserted, err := h.store.AddSubscriber(ctx, k, email, ev.Subscriber.FirstName, ev.Subscriber.LastName)
		if err != nil {
			return false, "", err
		}
		if inserted {
			return true, "added to the site list", nil
		}
		return true, "already on the site list — refreshed", nil
	case SiteEventPreferencesUpdated:
		p := preferences.Preference{OrgID: k.OrgID, EmailHash: preferences.EmailHash(email), BrandRoot: brandRoot,
			PausedUntil: ev.Preferences.PausedUntil, Frequency: ev.Preferences.Frequency, Topics: ev.Preferences.Topics,
			Source: "site:" + k.Site}
		if p.Frequency == "" {
			p.Frequency = preferences.FrequencyNormal
		}
		if err := h.store.UpsertPreference(ctx, p); err != nil {
			return false, "", err
		}
		return true, fmt.Sprintf("preferences saved for %s (frequency %s)", brandRoot, p.Frequency), nil
	default: // unsubscribe
		if h.hub == nil {
			return false, "", errPrefsHubNotWired
		}
		isp, ip := extractISP(email), clientIP(r)
		if ev.Unsubscribe == nil || ev.Unsubscribe.Scope != "all" {
			if err := h.hub.SuppressScoped(ctx, email, brandRoot, "user_unsubscribe", "site:"+k.Site, isp, ip, ""); err != nil {
				return false, "", err
			}
			return true, "unsubscribed from " + brandRoot, nil
		}
		if _, err := h.hub.Suppress(ctx, email, "unsubscribe", "site:"+k.Site, isp, "", "", ip, ""); err != nil {
			return false, "", err
		}
		if err := h.store.MarkUnsubscribed(ctx, k.OrgID, email); err != nil {
			log.Printf("[site-contract] subscriber status update failed (suppression already written) site=%s: %v", k.Site, err)
		}
		return true, "unsubscribed from all brands", nil
	}
}

func clipString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── Postgres store ──────────────────────────────────────────────────────────

type pgSiteContractStore struct{ db *sql.DB }

func (s *pgSiteContractStore) ResolveKey(ctx context.Context, keyHash string) (siteKey, error) {
	var k siteKey
	err := s.db.QueryRowContext(ctx, `SELECT id::text, organization_id::text, site_domain, list_id::text
		FROM mailing_site_contract_keys WHERE key_hash = $1 AND revoked_at IS NULL`, keyHash).Scan(&k.ID, &k.OrgID, &k.Site, &k.ListID)
	if errors.Is(err, sql.ErrNoRows) {
		return k, errSiteKeyNotFound
	}
	if err == nil {
		go func(id string) {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = s.db.ExecContext(c, `UPDATE mailing_site_contract_keys SET last_used_at = NOW() WHERE id = $1`, id)
		}(k.ID)
	}
	return k, err
}

func (s *pgSiteContractStore) RecordEvent(ctx context.Context, k siteKey, ev SiteEvent, emailHash string) (bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `INSERT INTO mailing_site_contract_events (organization_id, site_domain, event_id, event_type, email_hash, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (site_domain, event_id) DO NOTHING RETURNING id::text`,
		k.OrgID, k.Site, ev.EventID, ev.Type, emailHash, ev.OccurredAt).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *pgSiteContractStore) ForgetEvent(ctx context.Context, k siteKey, eventID string) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(c, `DELETE FROM mailing_site_contract_events WHERE site_domain = $1 AND event_id = $2`, k.Site, eventID); err != nil {
		log.Printf("[site-contract] forget failed site=%s event=%s: %v", k.Site, eventID, err)
	}
}

func (s *pgSiteContractStore) SaveResult(ctx context.Context, k siteKey, eventID string, result map[string]any) {
	b, _ := json.Marshal(result)
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(c, `UPDATE mailing_site_contract_events SET result = $3::jsonb WHERE site_domain = $1 AND event_id = $2`,
		k.Site, eventID, string(b)); err != nil {
		log.Printf("[site-contract] result save failed site=%s event=%s: %v", k.Site, eventID, err)
	}
}

// AddSubscriber mirrors HandleAddSubscriber's writes for the key's list; the
// list count moves only on a real insert.
func (s *pgSiteContractStore) AddSubscriber(ctx context.Context, k siteKey, email, first, last string) (bool, error) {
	emailHash := preferences.EmailHash(email)
	var inserted bool
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO mailing_subscribers (id, organization_id, list_id, email, email_hash, first_name, last_name, status, engagement_score, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 'confirmed', 50.0, NOW(), NOW())
		ON CONFLICT (list_id, email) DO UPDATE SET
			first_name = COALESCE(NULLIF(EXCLUDED.first_name, ''), mailing_subscribers.first_name),
			last_name  = COALESCE(NULLIF(EXCLUDED.last_name, ''), mailing_subscribers.last_name),
			status = 'confirmed', updated_at = NOW()
		RETURNING (xmax = 0)`, k.OrgID, k.ListID, email, emailHash, first, last).Scan(&inserted); err != nil {
		return false, err
	}
	if inserted {
		if _, err := s.db.ExecContext(ctx, `UPDATE mailing_lists SET subscriber_count = subscriber_count + 1 WHERE id = $1`, k.ListID); err != nil {
			log.Printf("[site-contract] list count update failed list=%s: %v", k.ListID, err)
		}
	}
	domain := email[strings.LastIndex(email, "@")+1:]
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, engagement_score, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 0.50, NOW(), NOW())
		ON CONFLICT (email_hash) DO UPDATE SET email = EXCLUDED.email`, emailHash, email, domain); err != nil {
		log.Printf("[site-contract] inbox profile upsert failed: %v", err)
	}
	return inserted, nil
}

func (s *pgSiteContractStore) MarkUnsubscribed(ctx context.Context, orgID, email string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE mailing_subscribers SET status = 'unsubscribed', updated_at = NOW()
		WHERE organization_id = $1 AND email = $2`, orgID, email)
	return err
}

func (s *pgSiteContractStore) UpsertPreference(ctx context.Context, p preferences.Preference) error {
	return preferences.Upsert(ctx, s.db, preferences.Current(), p)
}

// ── admin: site keys ────────────────────────────────────────────────────────

// SiteKeyAdmin mints, lists and revokes site keys (session / admin-key auth).
type SiteKeyAdmin struct{ db *sql.DB }

// NewSiteKeyAdmin builds the admin handlers.
func NewSiteKeyAdmin(db *sql.DB) *SiteKeyAdmin { return &SiteKeyAdmin{db: db} }

// generateSiteKey returns "sk_<32 url-safe chars>", its display prefix and hash.
func generateSiteKey() (raw, prefix, hash string, err error) {
	body := make([]byte, 24)
	if _, err := rand.Read(body); err != nil {
		return "", "", "", fmt.Errorf("rand: %w", err)
	}
	raw = siteKeyPrefix + strings.TrimRight(base64URLEncode(body), "=")
	return raw, raw[:8], HashPartnerKey(raw), nil
}

// HandleMint is POST /api/mailing/site-keys {site_domain, list_id}. The raw
// key is in the response once and never stored.
func (a *SiteKeyAdmin) HandleMint(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		siteErr(w, http.StatusUnauthorized, "no_org", "organization context required")
		return
	}
	var in struct {
		SiteDomain string `json:"site_domain"`
		ListID     string `json:"list_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		siteErr(w, http.StatusBadRequest, "invalid_json", clipString(err.Error(), 200))
		return
	}
	site := normalizeSite(in.SiteDomain)
	if site == "" || !strings.Contains(site, ".") {
		siteErr(w, http.StatusBadRequest, "invalid_site", "site_domain is required (e.g. bestcreditcare.com)")
		return
	}
	var listOK bool
	if err := a.db.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM mailing_lists WHERE id::text = $1 AND organization_id::text = $2)`,
		in.ListID, orgID).Scan(&listOK); err != nil || !listOK {
		siteErr(w, http.StatusBadRequest, "invalid_list", "list_id must be a list in this organization")
		return
	}
	raw, prefix, hash, err := generateSiteKey()
	if err != nil {
		siteErr(w, http.StatusInternalServerError, "keygen_failed", err.Error())
		return
	}
	var id string
	if err := a.db.QueryRowContext(r.Context(), `INSERT INTO mailing_site_contract_keys (organization_id, site_domain, list_id, key_hash, key_prefix)
		VALUES ($1, $2, $3, $4, $5) RETURNING id::text`, orgID, site, in.ListID, hash, prefix).Scan(&id); err != nil {
		siteErr(w, http.StatusInternalServerError, "store_failed", err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"id": id, "site_domain": site, "list_id": in.ListID, "key": raw, "key_prefix": prefix,
		"note": "store this key in the site's server-side secrets now; it is not shown again"})
}

// HandleList is GET /api/mailing/site-keys.
func (a *SiteKeyAdmin) HandleList(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		siteErr(w, http.StatusUnauthorized, "no_org", "organization context required")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id::text, site_domain, list_id::text, key_prefix, created_at, last_used_at, revoked_at
		FROM mailing_site_contract_keys WHERE organization_id::text = $1 ORDER BY site_domain, created_at`, orgID)
	if err != nil {
		siteErr(w, http.StatusInternalServerError, "query_failed", err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, site, list, prefix string
		var created time.Time
		var used, revoked sql.NullTime
		if err := rows.Scan(&id, &site, &list, &prefix, &created, &used, &revoked); err != nil {
			siteErr(w, http.StatusInternalServerError, "scan_failed", err.Error())
			return
		}
		row := map[string]any{"id": id, "site_domain": site, "list_id": list, "key_prefix": prefix, "created_at": created, "active": !revoked.Valid}
		if used.Valid {
			row["last_used_at"] = used.Time
		}
		out = append(out, row)
	}
	respondJSON(w, http.StatusOK, map[string]any{"keys": out, "contract": SiteContractVersion})
}

// HandleRevoke is POST /api/mailing/site-keys/{id}/revoke.
func (a *SiteKeyAdmin) HandleRevoke(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		siteErr(w, http.StatusUnauthorized, "no_org", "organization context required")
		return
	}
	res, err := a.db.ExecContext(r.Context(), `UPDATE mailing_site_contract_keys SET revoked_at = NOW()
		WHERE id::text = $1 AND organization_id::text = $2 AND revoked_at IS NULL`, chi.URLParam(r, "id"), orgID)
	if err != nil {
		siteErr(w, http.StatusInternalServerError, "revoke_failed", err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		siteErr(w, http.StatusNotFound, "not_found", "no active key with that id")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// ── schema ──────────────────────────────────────────────────────────────────

// HandleSiteContractSchema is GET /api/sites/v1/schema (public).
func HandleSiteContractSchema(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]any{
		"contract": SiteContractVersion,
		"endpoint": "POST /api/sites/v1/events",
		"auth":     "X-Site-Key: sk_<32> — one key per site, minted by the platform; server-side only, never in the browser",
		"envelope": map[string]string{
			"event_id":    "required, unique per site, 1-100 of A-Z a-z 0-9 . _ : - (retry with the same id)",
			"type":        "subscribe.confirmed | preferences.updated | unsubscribe",
			"site":        "required, the site's domain; must match the key",
			"occurred_at": "optional RFC3339",
			"dry_run":     "optional; true = validate and return the plan, write nothing",
			"subscriber":  "{email (required), first_name, last_name}",
			"preferences": "preferences.updated only — FULL state for the site's brand: {frequency: normal|reduced|weekly, paused_until: RFC3339 future ≤1y, topics: {name: bool}}; omitted fields reset to defaults",
			"unsubscribe": "unsubscribe only — {scope: brand (default) | all, reason}",
		},
		"responses": map[string]string{
			"202": "applied — {accepted, event_id, type, applied, detail, brand_root}",
			"200": "duplicate event_id (nothing changed) or dry_run plan",
			"400": "invalid_json | invalid_event_id | invalid_type | invalid_email | missing_preferences | invalid_frequency | invalid_paused_until | invalid_topics | invalid_scope",
			"401": "invalid_site_key",
			"403": "site_mismatch",
			"500": "apply_failed — retry with the same event_id",
			"503": "unavailable — retry with the same event_id",
		},
	})
}
