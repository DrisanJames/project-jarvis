package api

// Account provider keys — swap the six third-party keys below from the portal.
//
// A saved value is encrypted (AES-256-GCM, key from AuthManager.DerivedKey) in
// account_provider_keys and applied with os.Setenv, because every consumer
// reads the environment or a config built from it:
//
//   - at boot, before config load, so constructors and config.LoadFromEnv see
//     it (ApplySavedProviderKeysAtBoot, called from cmd/server/main.go);
//   - on save, on the instance that took the request;
//   - every 60s on every instance, so both ECS tasks converge.
//
// The environment belongs to the process, so only the platform organization can
// manage keys. Any failure leaves the deployment value in place. The encryption
// key derives from the Google OAuth client secret: rotating that secret makes
// saved values unreadable, and GET /keys then says so and shows what is in use.
//
// The whitelist is closed. SESSION_SECRET, TRACKING_SECRET, ADMIN_API_KEY,
// database URLs and SES, PMTA and Google secrets are never exposed or accepted.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	providerKeyPurpose = "account-provider-keys"

	// ProviderKeyPurpose is the DerivedKey purpose string for the boot loader.
	ProviderKeyPurpose = providerKeyPurpose

	providerKeyRefreshEvery = 60 * time.Second

	providerKeyMinLen = 16
	providerKeyMaxLen = 512
	providerTailMin   = 12 // shorter values show no tail: 4 chars would be too much of them

	providerKeyRightAway   = "right away"
	providerKeyAfterDeploy = "after the next deploy"

	providerKeySourceDeployment = "deployment setting"
	providerKeySourceSaved      = "saved here"
)

// providerKeySpec is one whitelisted key. Applies is the honest worst case, from
// reading every consumer: "right away" only if ALL of them read the environment
// at call time. Note covers the mixed cases.
type providerKeySpec struct {
	Name    string
	Label   string
	InUseBy string
	Applies string
	Note    string
}

var providerKeyCatalog = []providerKeySpec{
	{
		// Call time: ai_subject_preheader_claude.go:251, creative_generation.go:85,
		// handlers_wave_content.go:280,556, handlers_pmta_campaign.go:3365,
		// landing_page_handlers.go:257, marketing_agent_calendar.go:1546,
		// worker/content_refresh.go:111.
		// At boot: ai_content_handlers.go:32, marketing_agent.go:111,
		// copilot_handler.go:49, contentdesk/llm.go:256 (SDK reads it in NewClient).
		Name: "ANTHROPIC_API_KEY", Label: "Anthropic",
		InUseBy: "Creative tools, subject lines, EDITH, Copilot, landing pages",
		Applies: providerKeyAfterDeploy,
		Note:    "Some tools pick it up right away. EDITH, Copilot and AI content need the deploy.",
	},
	{
		// At boot only: contentdesk/llm.go:243 via NewAnthropicLLM (llm.go:250),
		// built once by worker/content_desk_worker.go:102.
		Name: "CONTENT_DESK_ANTHROPIC_API_KEY", Label: "Anthropic (Content Desk)",
		InUseBy: "Content Desk article writing",
		Applies: providerKeyAfterDeploy,
	},
	{
		// Call time: handlers_wave_content.go:281,557, marketing_agent_calendar.go:1547,
		// worker/content_refresh.go:112.
		// At boot: config/config.go:561 (s.openAIConfig for the agents),
		// ai_content_handlers.go:33.
		Name: "OPENAI_API_KEY", Label: "OpenAI",
		InUseBy: "AI fallback when Anthropic isn't available",
		Applies: providerKeyAfterDeploy,
		Note:    "Some tools pick it up right away. The agents and AI content need the deploy.",
	},
	{
		// Call time: everflow_conversions.go:158.
		// At boot: config/config.go:553, server_routes_mailing.go:1081,
		// jarvis_orchestrator.go:32.
		Name: "EVERFLOW_API_KEY", Label: "Everflow",
		InUseBy: "Offer data, creatives and conversion sync",
		Applies: providerKeyAfterDeploy,
		Note:    "Conversion sync picks it up right away. Offer data and creatives need the deploy.",
	},
	{
		// At boot only: notify.go:54,78,101,122, all called while wiring
		// (cmd/server/main.go, server_routes_mailing.go:364,367).
		Name: "SLACK_BOT_TOKEN", Label: "Slack",
		InUseBy: "Alerts and conversion messages from the server",
		Applies: providerKeyAfterDeploy,
	},
	{
		// config/config.go:630 loads it; no code reads cfg.Alerting.Twilio and
		// internal/pkg/twilio is not imported, so nothing uses it today.
		Name: "TWILIO_AUTH_TOKEN", Label: "Twilio",
		InUseBy: "Nothing on the server uses this yet",
		Applies: providerKeyAfterDeploy,
		Note:    "Saved for later. No feature reads it today.",
	},
}

func providerKeyByName(name string) (providerKeySpec, bool) {
	for _, spec := range providerKeyCatalog {
		if spec.Name == name {
			return spec, true
		}
	}
	return providerKeySpec{}, false
}

// platformOrgID is the organization whose keys reach the process environment.
// It runs at boot, before SetProcessDefaultOrgID.
func platformOrgID() uuid.UUID {
	if v := os.Getenv("DEFAULT_ORG_ID"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			return id
		}
	}
	if id := GetProcessDefaultOrgID(); id != uuid.Nil {
		return id
	}
	return uuid.MustParse(SingleTenantFallbackOrgID)
}

// deploymentKeys is the environment as deployed, before any saved value. It
// lets GET /keys report a removed key still running until the next boot.
var deploymentKeys atomic.Pointer[map[string]string]

func captureDeploymentKeys() {
	snapshot := make(map[string]string, len(providerKeyCatalog))
	for _, spec := range providerKeyCatalog {
		snapshot[spec.Name] = os.Getenv(spec.Name)
	}
	deploymentKeys.CompareAndSwap(nil, &snapshot)
}

func providerKeyAAD(orgID uuid.UUID, name string) []byte {
	return []byte(orgID.String() + "|" + name)
}

// encryptProviderKey seals value to its row: the organization and key name are
// authenticated, so a ciphertext cannot be moved onto another key.
func encryptProviderKey(encKey []byte, orgID uuid.UUID, name, value string) ([]byte, error) {
	gcm, err := providerKeyAEAD(encKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(value), providerKeyAAD(orgID, name)), nil
}

func decryptProviderKey(encKey []byte, orgID uuid.UUID, name string, sealed []byte) (string, error) {
	gcm, err := providerKeyAEAD(encKey)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, body := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, providerKeyAAD(orgID, name))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func providerKeyAEAD(encKey []byte) (cipher.AEAD, error) {
	if len(encKey) != 32 {
		return nil, errors.New("encryption key not available")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// providerKeyTail is the only part of a key that ever leaves the server.
func providerKeyTail(value string) string {
	if len(value) < providerTailMin {
		return ""
	}
	return value[len(value)-4:]
}

// providerKeyProblem validates a new value. Its messages never quote the value.
func providerKeyProblem(value string) string {
	if len(value) < providerKeyMinLen || len(value) > providerKeyMaxLen {
		return fmt.Sprintf("Keys must be between %d and %d characters.", providerKeyMinLen, providerKeyMaxLen)
	}
	for _, r := range value {
		if r <= 0x20 || r >= 0x7f {
			return "Keys can't contain spaces or line breaks. Check for extra characters from copying."
		}
	}
	return ""
}

// providerKeyApplier copies saved values into the environment.
type providerKeyApplier struct {
	db     *sql.DB
	encKey []byte
	orgID  uuid.UUID
	warned map[string]bool
}

func newProviderKeyApplier(db *sql.DB, encKey []byte, orgID uuid.UUID) *providerKeyApplier {
	return &providerKeyApplier{db: db, encKey: encKey, orgID: orgID, warned: map[string]bool{}}
}

// apply sets every saved value that differs from the environment. A row that
// can't be used is logged once and skipped; the rest still apply.
func (a *providerKeyApplier) apply(ctx context.Context) (int, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT key_name, ciphertext FROM account_provider_keys WHERE organization_id = $1`, a.orgID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	applied := 0
	for rows.Next() {
		var (
			name   string
			sealed []byte
		)
		if err := rows.Scan(&name, &sealed); err != nil {
			return applied, err
		}
		if _, ok := providerKeyByName(name); !ok {
			a.warnOnce(name, "is not on the whitelist")
			continue
		}
		value, err := decryptProviderKey(a.encKey, a.orgID, name, sealed)
		if err != nil {
			a.warnOnce(name, "can't be decrypted, keeping the current value")
			continue
		}
		delete(a.warned, name)
		if os.Getenv(name) == value {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			a.warnOnce(name, "couldn't be set")
			continue
		}
		log.Printf("[Account] provider key %s applied from saved value", name)
		applied++
	}
	return applied, rows.Err()
}

func (a *providerKeyApplier) warnOnce(name, problem string) {
	if a.warned[name] {
		return
	}
	a.warned[name] = true
	log.Printf("[Account] saved provider key %s %s", name, problem)
}

// ApplySavedProviderKeysAtBoot applies saved keys before config load, so config
// and constructors see them. Any failure logs and boot continues on env values.
func ApplySavedProviderKeysAtBoot(databaseURL string, encKey []byte) {
	captureDeploymentKeys()
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	loadSavedProviderKeys(ctx, encKey, platformOrgID(), func() (*sql.DB, error) {
		return sql.Open("postgres", databaseURL)
	})
}

func loadSavedProviderKeys(ctx context.Context, encKey []byte, orgID uuid.UUID, open func() (*sql.DB, error)) int {
	if len(encKey) == 0 {
		log.Println("[Account] no encryption key, running on deployment settings")
		return 0
	}
	db, err := open()
	if err != nil {
		logProviderKeyLoadFailure(err)
		return 0
	}
	defer db.Close()
	applied, err := newProviderKeyApplier(db, encKey, orgID).apply(ctx)
	if err != nil {
		logProviderKeyLoadFailure(err)
	}
	if applied > 0 {
		log.Printf("[Account] applied %d saved provider keys at boot", applied)
	}
	return applied
}

// logProviderKeyLoadFailure hides a URL parse error, which quotes the database
// URL and its password.
func logProviderKeyLoadFailure(err error) {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = errors.New("database URL could not be parsed")
	}
	log.Printf("[Account] couldn't load saved provider keys, running on deployment settings: %v", err)
}

// StartKeyRefresher keeps this instance on the newest saved values.
func (s *AccountService) StartKeyRefresher(ctx context.Context) {
	if len(s.keyEnc) == 0 {
		log.Println("[Account] no encryption key, provider key refresh off")
		return
	}
	go func() {
		ticker := time.NewTicker(providerKeyRefreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshProviderKeys(ctx)
			}
		}
	}()
}

func (s *AccountService) refreshProviderKeys(ctx context.Context) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, accountTimeout)
	defer cancel()
	if _, err := s.applier.apply(ctx); err != nil {
		log.Printf("[Account] provider key refresh failed: %v", err)
	}
}

// providerKeyRow is what GET /keys returns. There is no field a value could
// travel in except the four-character tail.
type providerKeyRow struct {
	Name             string     `json:"name"`
	Label            string     `json:"label"`
	InUseBy          string     `json:"in_use_by"`
	Source           string     `json:"source"`
	Last4            string     `json:"last4"`
	Configured       bool       `json:"configured"`
	UpdatedBy        string     `json:"updated_by"`
	UpdatedAt        *time.Time `json:"updated_at"`
	Applies          string     `json:"applies"`
	AppliesNote      string     `json:"applies_note"`
	RevertsOnRestart bool       `json:"reverts_on_restart"`
	Warning          string     `json:"warning"`
}

type savedProviderKey struct {
	sealed    []byte
	last4     string
	updatedBy string
	updatedAt time.Time
}

func (s *AccountService) loadSavedKeys(ctx context.Context, orgID uuid.UUID) (map[string]savedProviderKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_name, ciphertext, last4, updated_by, updated_at
		  FROM account_provider_keys WHERE organization_id = $1`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	saved := map[string]savedProviderKey{}
	for rows.Next() {
		var (
			name string
			k    savedProviderKey
		)
		if err := rows.Scan(&name, &k.sealed, &k.last4, &k.updatedBy, &k.updatedAt); err != nil {
			return nil, err
		}
		saved[name] = k
	}
	return saved, rows.Err()
}

func (s *AccountService) keyRow(spec providerKeySpec, orgID uuid.UUID, saved map[string]savedProviderKey) providerKeyRow {
	current := os.Getenv(spec.Name)
	row := providerKeyRow{
		Name: spec.Name, Label: spec.Label, InUseBy: spec.InUseBy,
		Source: providerKeySourceDeployment, Last4: providerKeyTail(current),
		Configured: current != "", Applies: spec.Applies, AppliesNote: spec.Note,
	}
	k, ok := saved[spec.Name]
	if !ok {
		if snapshot := deploymentKeys.Load(); snapshot != nil {
			row.RevertsOnRestart = current != (*snapshot)[spec.Name]
		}
		return row
	}
	if _, err := decryptProviderKey(s.keyEnc, orgID, spec.Name, k.sealed); err != nil {
		row.Warning = "The saved key can't be read, so the deployment setting is in use. Save it again."
		return row
	}
	row.Source, row.Last4, row.Configured = providerKeySourceSaved, k.last4, true
	row.UpdatedBy = k.updatedBy
	updatedAt := k.updatedAt
	row.UpdatedAt = &updatedAt
	return row
}

// HandleListKeys lists the whitelisted keys, tail only.
func (s *AccountService) HandleListKeys(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	saved, err := s.loadSavedKeys(ctx, c.OrgID)
	if err != nil {
		log.Printf("[Account] provider key read failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't load the keys.")
		return
	}
	keys := make([]providerKeyRow, 0, len(providerKeyCatalog))
	for _, spec := range providerKeyCatalog {
		keys = append(keys, s.keyRow(spec, c.OrgID, saved))
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"keys": keys, "can_save": len(s.keyEnc) > 0})
}

// keyWriteTarget applies the refusals shared by save and delete.
func (s *AccountService) keyWriteTarget(w http.ResponseWriter, r *http.Request) (*accountCaller, providerKeySpec, bool) {
	c, ok := s.requireManager(w, r)
	if !ok {
		return nil, providerKeySpec{}, false
	}
	spec, ok := providerKeyByName(chi.URLParam(r, "name"))
	if !ok {
		respondError(w, http.StatusBadRequest, "That key can't be changed here.")
		return nil, providerKeySpec{}, false
	}
	if c.OrgID != platformOrgID() {
		respondError(w, http.StatusForbidden, "Keys are managed by the platform organization.")
		return nil, providerKeySpec{}, false
	}
	return c, spec, true
}

// HandleSaveKey replaces a provider key. The value is written once, encrypted,
// and never returned or logged.
func (s *AccountService) HandleSaveKey(w http.ResponseWriter, r *http.Request) {
	c, spec, ok := s.keyWriteTarget(w, r)
	if !ok {
		return
	}
	if len(s.keyEnc) == 0 {
		respondError(w, http.StatusServiceUnavailable, "Keys can't be saved until sign-in is configured on this server.")
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if !decodeAccountBody(w, r, &body) {
		return
	}
	if problem := providerKeyProblem(body.Value); problem != "" {
		respondError(w, http.StatusBadRequest, problem)
		return
	}
	sealed, err := encryptProviderKey(s.keyEnc, c.OrgID, spec.Name, body.Value)
	if err != nil {
		log.Printf("[Account] provider key %s encrypt failed", spec.Name)
		respondError(w, http.StatusInternalServerError, "Couldn't save the key.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO account_provider_keys (organization_id, key_name, ciphertext, last4, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (organization_id, key_name) DO UPDATE SET
			ciphertext = EXCLUDED.ciphertext, last4 = EXCLUDED.last4,
			updated_by = EXCLUDED.updated_by, updated_at = NOW()`,
		c.OrgID, spec.Name, sealed, providerKeyTail(body.Value), c.Email)
	if err != nil {
		log.Printf("[Account] provider key %s save failed by %s: %v", spec.Name, c.Email, err)
		respondError(w, http.StatusInternalServerError, "Couldn't save the key.")
		return
	}
	if err := os.Setenv(spec.Name, body.Value); err != nil {
		log.Printf("[Account] provider key %s saved but not applied here", spec.Name)
	}
	log.Printf("[Account] provider key %s saved by %s", spec.Name, c.Email)
	s.respondKeyRow(ctx, w, spec, c.OrgID)
}

// HandleDeleteKey removes a saved key. The deployment setting returns at the
// next boot; until then the running instances keep the value they have.
func (s *AccountService) HandleDeleteKey(w http.ResponseWriter, r *http.Request) {
	c, spec, ok := s.keyWriteTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM account_provider_keys WHERE organization_id = $1 AND key_name = $2`, c.OrgID, spec.Name)
	if err != nil {
		log.Printf("[Account] provider key %s delete failed by %s: %v", spec.Name, c.Email, err)
		respondError(w, http.StatusInternalServerError, "Couldn't remove the key.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "There's no saved key to remove.")
		return
	}
	log.Printf("[Account] provider key %s removed by %s", spec.Name, c.Email)
	s.respondKeyRow(ctx, w, spec, c.OrgID)
}

func (s *AccountService) respondKeyRow(ctx context.Context, w http.ResponseWriter, spec providerKeySpec, orgID uuid.UUID) {
	saved, err := s.loadSavedKeys(ctx, orgID)
	if err != nil {
		log.Printf("[Account] provider key reread failed: %v", err)
		saved = map[string]savedProviderKey{}
	}
	respondJSON(w, http.StatusOK, s.keyRow(spec, orgID, saved))
}
