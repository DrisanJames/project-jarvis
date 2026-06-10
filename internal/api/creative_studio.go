package api

// Creative Studio orchestrator — the portal-facing API over the ReviewForge
// engine sidecar. The sidecar (review-forge Next.js, same ECS task, shared
// network namespace) renders byte-identical newsletter/solo HTML with the
// operator's daily-honed template engine; this service persists the results
// where the operator manages them:
//
//   - Content Library: mailing_templates under "Creative Studio/<SITE>"
//     (edit / clone / move / delete via the existing library UX)
//   - mailing_creatives registry: the send-day archive (forge-pull brings
//     studio creatives down to the laptop for briefs)
//
// In local dev the "sidecar" is simply the operator's `npm run dev` on :3100.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

type CreativeStudioService struct {
	db        *sql.DB
	templates *mailing.EmailTemplateStore
	folders   *mailing.TemplateFolderService
	client    *http.Client

	brandsMu       sync.Mutex
	brandsCache    []studioBrand
	brandsCachedAt time.Time
}

func NewCreativeStudioService(db *sql.DB) *CreativeStudioService {
	return &CreativeStudioService{
		db:        db,
		templates: mailing.NewEmailTemplateStore(db),
		folders:   mailing.NewTemplateFolderService(db),
		client:    &http.Client{Timeout: 120 * time.Second},
	}
}

func studioEngineBase() string {
	if v := os.Getenv("REVIEW_FORGE_INTERNAL_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:3100"
}

type studioBrand struct {
	BrandKey string `json:"brandKey"`
	Name     string `json:"name"`
	Category string `json:"category,omitempty"`
}

// engineGET proxies a GET to the sidecar and returns the raw body.
func (s *CreativeStudioService) engineGET(path string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(studioEngineBase() + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("engine %s: %d %s", path, resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// HandleStatus reports sidecar reachability for the UI banner.
func (s *CreativeStudioService) HandleStatus(w http.ResponseWriter, r *http.Request) {
	_, err := s.engineGET("/api/brands", 3*time.Second)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"engine_url": studioEngineBase(),
		"engine_up":  err == nil,
	})
}

// HandleBrands proxies the merged brand list (hardcoded + custom).
func (s *CreativeStudioService) HandleBrands(w http.ResponseWriter, r *http.Request) {
	body, err := s.engineGET("/api/brands", 10*time.Second)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": "creative engine unreachable: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// HandleSubjectLines proxies the per-site subject/preheader pools.
func (s *CreativeStudioService) HandleSubjectLines(w http.ResponseWriter, r *http.Request) {
	body, err := s.engineGET("/api/subject-lines", 10*time.Second)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{"error": "creative engine unreachable: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (s *CreativeStudioService) brandName(brandKey string) string {
	s.brandsMu.Lock()
	defer s.brandsMu.Unlock()
	if time.Since(s.brandsCachedAt) > 5*time.Minute || s.brandsCache == nil {
		if body, err := s.engineGET("/api/brands", 10*time.Second); err == nil {
			var parsed struct {
				Brands []studioBrand `json:"brands"`
			}
			if json.Unmarshal(body, &parsed) == nil {
				s.brandsCache = parsed.Brands
				s.brandsCachedAt = time.Now()
			}
		}
	}
	for _, b := range s.brandsCache {
		if b.BrandKey == brandKey {
			return b.Name
		}
	}
	return brandKey
}

var studioSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func studioSlug(s string) string {
	out := studioSlugRe.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(out, "-")
}

var cratoolproRe = regexp.MustCompile(`https?://(?:www\.)?cratoolpro\.com/[A-Za-z0-9]+/[A-Za-z0-9]+/`)

type StudioGenerateRequest struct {
	SiteKey           string          `json:"site_key,omitempty"`
	SiteKeys          []string        `json:"site_keys,omitempty"` // multi-brand fan-out
	PrimaryBrandKey   string          `json:"primary_brand_key"`
	SecondaryBrandKey string          `json:"secondary_brand_key,omitempty"`
	SubjectLine       string          `json:"subject_line,omitempty"`
	Preheader         string          `json:"preheader,omitempty"`
	RefreshContent    bool            `json:"refresh_content"`
	Mode              string          `json:"mode,omitempty"` // newsletter (default) | solo
	Solo              json.RawMessage `json:"solo,omitempty"`
	// Engine BrandOverrides passthrough (title, subtitle, excerpt, rating,
	// highlights, ctaLabel, ctaUrl, bannerUrl, logoUrl, imageOrientation) —
	// the operator swaps imagery per send often.
	PrimaryOverrides   json.RawMessage `json:"primary_overrides,omitempty"`
	SecondaryOverrides json.RawMessage `json:"secondary_overrides,omitempty"`
	Save               bool            `json:"save"`
	Name               string          `json:"name,omitempty"` // template display name override
}

type StudioGenerateResult struct {
	SiteKey    string `json:"site_key"`
	HTML       string `json:"html"`
	Subject    string `json:"subject"`
	Preheader  string `json:"preheader"`
	Filename   string `json:"filename,omitempty"`
	TemplateID string `json:"template_id,omitempty"`
	CreativeID string `json:"creative_id,omitempty"`
	MoneyURLs  int    `json:"money_urls"`
	Saved      bool   `json:"saved"`
	Error      string `json:"error,omitempty"` // per-site failure in a fan-out
}

// Generate renders via the sidecar and (optionally) persists. Shared by the
// HTTP handler and the creative agent's generate tools.
func (s *CreativeStudioService) Generate(r *http.Request, req StudioGenerateRequest) (*StudioGenerateResult, error) {
	if req.SiteKey == "" || req.PrimaryBrandKey == "" {
		return nil, fmt.Errorf("site_key and primary_brand_key are required")
	}
	mode := req.Mode
	if mode == "" {
		mode = "newsletter"
	}

	enginePayload := map[string]interface{}{
		"action":          "preview",
		"siteKey":         strings.ToLower(req.SiteKey),
		"primaryBrandKey": req.PrimaryBrandKey,
		"refreshContent":  req.RefreshContent,
		"mode":            mode,
	}
	if req.SecondaryBrandKey != "" {
		enginePayload["secondaryBrandKey"] = req.SecondaryBrandKey
	}
	if req.SubjectLine != "" {
		enginePayload["subjectLine"] = req.SubjectLine
	}
	if req.Preheader != "" {
		enginePayload["preheader"] = req.Preheader
	}
	if len(req.Solo) > 0 {
		var solo interface{}
		if err := json.Unmarshal(req.Solo, &solo); err != nil {
			return nil, fmt.Errorf("invalid solo payload: %w", err)
		}
		enginePayload["solo"] = solo
	}
	if len(req.PrimaryOverrides) > 0 {
		var ov interface{}
		if err := json.Unmarshal(req.PrimaryOverrides, &ov); err != nil {
			return nil, fmt.Errorf("invalid primary_overrides: %w", err)
		}
		enginePayload["primaryOverrides"] = ov
	}
	if len(req.SecondaryOverrides) > 0 {
		var ov interface{}
		if err := json.Unmarshal(req.SecondaryOverrides, &ov); err != nil {
			return nil, fmt.Errorf("invalid secondary_overrides: %w", err)
		}
		enginePayload["secondaryOverrides"] = ov
	}

	body, _ := json.Marshal(enginePayload)
	resp, err := s.client.Post(studioEngineBase()+"/api/template-email", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creative engine unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("engine render failed: %d %s", resp.StatusCode, truncate(string(respBody), 300))
	}
	var rendered struct {
		HTML        string `json:"html"`
		SubjectLine string `json:"subjectLine"`
		Preheader   string `json:"preheader"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(respBody, &rendered); err != nil || rendered.HTML == "" {
		return nil, fmt.Errorf("engine returned no HTML: %s", truncate(string(respBody), 300))
	}

	result := &StudioGenerateResult{
		SiteKey:   strings.ToLower(req.SiteKey),
		HTML:      rendered.HTML,
		Subject:   rendered.SubjectLine,
		Preheader: rendered.Preheader,
		MoneyURLs: len(cratoolproRe.FindAllString(rendered.HTML, -1)),
	}
	if !req.Save {
		return result, nil
	}

	orgUUID, err := GetOrgIDFromRequest(r)
	if err != nil {
		return nil, fmt.Errorf("organization not resolved: %w", err)
	}
	orgID := orgUUID.String()

	site := strings.ToUpper(req.SiteKey)
	brandName := s.brandName(req.PrimaryBrandKey)
	date := time.Now().Format("01022006")
	suffix := "newsletter"
	if mode == "solo" {
		suffix = "solo"
	}
	filename := fmt.Sprintf("%s-%s-%s-%s.html", studioSlug(brandName), strings.ToLower(req.SiteKey), suffix, date)

	// 1) Content Library — the operator's management surface.
	folder, err := s.folders.GetOrCreateFolderPath(r.Context(), orgUUID, "Creative Studio/"+site)
	if err != nil {
		return nil, fmt.Errorf("library folder: %w", err)
	}
	displayName := req.Name
	if displayName == "" {
		displayName = fmt.Sprintf("%s — %s %s", brandName, site, time.Now().Format("Jan 02"))
	}
	tmpl, err := s.templates.CreateTemplate(r.Context(), orgUUID, &mailing.CreateTemplateRequest{
		FolderID:    &folder.ID,
		Name:        displayName,
		Description: fmt.Sprintf("Creative Studio %s · %s · %s", mode, req.PrimaryBrandKey, filename),
		Subject:     rendered.SubjectLine,
		HTMLContent: rendered.HTML,
		PreviewText: rendered.Preheader,
	})
	if err != nil {
		return nil, fmt.Errorf("library save: %w", err)
	}

	// 2) Registry — the send-day archive (same upsert shape as creatives-sync).
	sum := sha256Hex(rendered.HTML)
	var creativeID string
	err = s.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_creatives
			(organization_id, offer_key, brand_code, filename, subject, preheader,
			 html_content, money_urls, tagged, source, forge_brand_key, sha256,
			 template_id, generated_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'studio',$10,$11,$12,NOW(),NOW())
		ON CONFLICT (organization_id, filename) DO UPDATE SET
			subject = EXCLUDED.subject, preheader = EXCLUDED.preheader,
			html_content = EXCLUDED.html_content, money_urls = EXCLUDED.money_urls,
			tagged = EXCLUDED.tagged, sha256 = EXCLUDED.sha256,
			template_id = EXCLUDED.template_id, updated_at = NOW()
		RETURNING id`,
		orgID, studioSlug(brandName), site, filename, rendered.SubjectLine, rendered.Preheader,
		rendered.HTML, result.MoneyURLs,
		strings.Contains(rendered.HTML, "sub1={{subscriber.id}}"),
		req.PrimaryBrandKey, sum, tmpl.ID.String()).Scan(&creativeID)
	if err != nil {
		return nil, fmt.Errorf("registry save: %w", err)
	}

	result.Filename = filename
	result.TemplateID = tmpl.ID.String()
	result.CreativeID = creativeID
	result.Saved = true
	return result, nil
}

// HandleGenerate fans out over site_keys (falling back to the single
// site_key), isolating per-site failures so one bad feed doesn't kill the
// batch. Response: {results: [StudioGenerateResult]}.
func (s *CreativeStudioService) HandleGenerate(w http.ResponseWriter, r *http.Request) {
	var req StudioGenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	sites := req.SiteKeys
	if len(sites) == 0 && req.SiteKey != "" {
		sites = []string{req.SiteKey}
	}
	if len(sites) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "site_key or site_keys required"})
		return
	}
	results := make([]StudioGenerateResult, 0, len(sites))
	for _, site := range sites {
		perSite := req
		perSite.SiteKey = site
		perSite.SiteKeys = nil
		res, err := s.Generate(r, perSite)
		if err != nil {
			results = append(results, StudioGenerateResult{SiteKey: strings.ToLower(site), Error: err.Error()})
			continue
		}
		results = append(results, *res)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}
