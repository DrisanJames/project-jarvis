package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// =============================================================================
// EVERFLOW CREATIVES HANDLER
// Integrates Everflow creative assets into the campaign creation workflow.
// Supports creative search, CPM/CPS categorization, tracking link building,
// and manual link offer detection.
// =============================================================================

type EverflowCreativesHandler struct {
	db     *sql.DB
	apiKey string
}

func NewEverflowCreativesHandler(db *sql.DB, apiKey string) *EverflowCreativesHandler {
	return &EverflowCreativesHandler{db: db, apiKey: apiKey}
}

// RegisterEverflowCreativeRoutes registers all Everflow creative routes.
func RegisterEverflowCreativeRoutes(r chi.Router, db *sql.DB, apiKey string) {
	h := NewEverflowCreativesHandler(db, apiKey)
	r.Route("/everflow-creatives", func(r chi.Router) {
		r.Get("/", h.HandleSearchCreatives)
		r.Get("/categories", h.HandleGetCategories)
		r.Post("/build-tracking-link", h.HandleBuildTrackingLink)
		r.Get("/manual-link-offers", h.HandleGetManualLinkOffers)
		r.Get("/affiliate-encodings", h.HandleGetAffiliateEncodings)
		r.Get("/offer-encodings", h.HandleGetOfferEncodings)
	})
}

// --- Types ---

type everflowCreative struct {
	NetworkOfferCreativeID int    `json:"network_offer_creative_id"`
	NetworkID              int    `json:"network_id"`
	Name                   string `json:"name"`
	NetworkOfferID         int    `json:"network_offer_id"`
	NetworkOfferName       string `json:"network_offer_name"`
	CreativeType           string `json:"creative_type"`
	CreativeStatus         string `json:"creative_status"`
	HTMLCode               string `json:"html_code"`
	EmailFrom              string `json:"email_from"`
	EmailSubject           string `json:"email_subject"`
	EmailSubjectLines      string `json:"email_subject_lines"`
	EmailFromLines         string `json:"email_from_lines"`
}

type everflowResponse struct {
	Creatives []everflowCreative `json:"creatives"`
	Paging    struct {
		Page       int `json:"page"`
		PageSize   int `json:"page_size"`
		TotalCount int `json:"total_count"`
	} `json:"paging"`
}

type creativeCategory struct {
	PaymentModel       string                  `json:"payment_model"`
	OfferName          string                  `json:"offer_name"`
	OfferID            int                     `json:"offer_id"`
	InternalID         string                  `json:"internal_id"`
	RequiresManualLink bool                    `json:"requires_manual_link"`
	Creatives          []creativeCategoryEntry `json:"creatives"`
}

type creativeCategoryEntry struct {
	CreativeID      int    `json:"creative_id"`
	Name            string `json:"name"`
	HasTrackingLink bool   `json:"has_tracking_link"`
	HTMLPreview     string `json:"html_preview,omitempty"`
}

type buildTrackingLinkRequest struct {
	AffiliateID string `json:"affiliate_id"`
	OfferID     string `json:"offer_id"`
	CreativeID  int    `json:"creative_id"`
	DataSet     string `json:"data_set"`
}

type buildTrackingLinkResponse struct {
	TrackingLink      string   `json:"tracking_link"`
	MergeTagsUsed     []string `json:"merge_tags_used"`
	BrandedDomain     string   `json:"branded_domain,omitempty"`
	BrandedDomainUsed bool     `json:"branded_domain_used"`
	OriginalDomain    string   `json:"original_domain,omitempty"`
}

// --- Everflow API Client ---

func (h *EverflowCreativesHandler) fetchEverflowCreatives(search string) ([]byte, error) {
	searchTerms := []map[string]string{
		{"search_type": "creative_status", "value": "active"},
	}
	if search != "" {
		searchTerms = append(searchTerms, map[string]string{
			"search_type": "name",
			"value":       search,
		})
	}

	body := map[string]interface{}{
		"search_terms": searchTerms,
	}
	bodyJSON, _ := json.Marshal(body)

	url := "https://api.eflow.team/v1/networks/creativestable?page=1&page_size=100&order_field=&order_direction=desc"
	req, err := http.NewRequest("POST", url, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-eflow-api-key", h.apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("everflow API request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("everflow API returned %d: %s", resp.StatusCode, string(data[:minInt(len(data), 200)]))
	}

	return data, nil
}

// --- Handlers ---

// HandleSearchCreatives proxies creative search to Everflow API.
func (h *EverflowCreativesHandler) HandleSearchCreatives(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("search")

	data, err := h.fetchEverflowCreatives(search)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "%s"}`, err.Error()), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// HandleGetCategories groups creatives by payment model (CPM/CPS) and offer.
func (h *EverflowCreativesHandler) HandleGetCategories(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error": "organization context required"}`, http.StatusBadRequest)
		return
	}

	search := r.URL.Query().Get("search")

	data, err := h.fetchEverflowCreatives(search)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "%s"}`, err.Error()), http.StatusBadGateway)
		return
	}

	var efResp everflowResponse
	if err := json.Unmarshal(data, &efResp); err != nil {
		http.Error(w, `{"error": "failed to parse everflow response"}`, http.StatusInternalServerError)
		return
	}

	// Group by payment_model + offer_name + internal_id
	type catKey struct {
		PaymentModel string
		OfferName    string
		InternalID   string
		OfferID      int
	}

	categoryMap := make(map[catKey]*creativeCategory)
	var categoryOrder []catKey
	totalCreatives := 0

	for _, c := range efResp.Creatives {
		totalCreatives++

		offerName := c.NetworkOfferName
		paymentModel := ""
		internalID := ""
		offerID := c.NetworkOfferID

		// Extract internal ID from parentheses: "Sam's Club CPS - Club Membership (3250)"
		if idx := strings.LastIndex(offerName, "("); idx != -1 {
			if endIdx := strings.LastIndex(offerName, ")"); endIdx > idx {
				internalID = strings.TrimSpace(offerName[idx+1 : endIdx])
				offerName = strings.TrimSpace(offerName[:idx])
			}
		}

		// Extract payment model (CPM or CPS)
		upperName := strings.ToUpper(offerName)
		if strings.Contains(upperName, "CPM") {
			paymentModel = "CPM"
		} else if strings.Contains(upperName, "CPS") {
			paymentModel = "CPS"
		} else if strings.Contains(upperName, "CPL") {
			paymentModel = "CPL"
		} else if strings.Contains(upperName, "CPA") {
			paymentModel = "CPA"
		}

		// Clean offer name: extract the brand name (everything before CPM/CPS keyword)
		cleanName := offerName
		for _, keyword := range []string{" CPM ", " CPS ", " CPL ", " CPA "} {
			if idx := strings.Index(strings.ToUpper(cleanName), keyword); idx != -1 {
				cleanName = strings.TrimSpace(cleanName[:idx])
				break
			}
		}
		// Also strip trailing " -" or " - "
		cleanName = strings.TrimRight(cleanName, " -")

		key := catKey{
			PaymentModel: paymentModel,
			OfferName:    cleanName,
			InternalID:   internalID,
			OfferID:      offerID,
		}

		cat, exists := categoryMap[key]
		if !exists {
			cat = &creativeCategory{
				PaymentModel: paymentModel,
				OfferName:    cleanName,
				OfferID:      offerID,
				InternalID:   internalID,
				Creatives:    []creativeCategoryEntry{},
			}
			categoryMap[key] = cat
			categoryOrder = append(categoryOrder, key)
		}

		// Check if HTML contains {tracking_link}
		hasTrackingLink := strings.Contains(c.HTMLCode, "{tracking_link}")

		cat.Creatives = append(cat.Creatives, creativeCategoryEntry{
			CreativeID:      c.NetworkOfferCreativeID,
			Name:            c.Name,
			HasTrackingLink: hasTrackingLink,
		})
	}

	// Check requires_manual_link for each category by matching offer name patterns
	ctx := r.Context()
	for _, key := range categoryOrder {
		cat := categoryMap[key]

		rows, err := h.db.QueryContext(ctx,
			"SELECT offer_name_pattern FROM mailing_manual_link_offers WHERE organization_id = $1",
			orgID,
		)
		if err != nil {
			continue
		}
		for rows.Next() {
			var pattern string
			if err := rows.Scan(&pattern); err != nil {
				continue
			}
			if strings.Contains(strings.ToLower(cat.OfferName), strings.ToLower(pattern)) {
				cat.RequiresManualLink = true
				break
			}
		}
		rows.Close()
	}

	// Build ordered response
	categories := make([]creativeCategory, 0, len(categoryOrder))
	for _, key := range categoryOrder {
		categories = append(categories, *categoryMap[key])
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"categories":      categories,
		"total_creatives": totalCreatives,
	})
}

// HandleBuildTrackingLink builds an encoded tracking link from a creative selection.
func (h *EverflowCreativesHandler) HandleBuildTrackingLink(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error": "organization context required"}`, http.StatusBadRequest)
		return
	}

	var req buildTrackingLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error": "invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.AffiliateID == "" || req.OfferID == "" || req.CreativeID == 0 {
		http.Error(w, `{"error": "affiliate_id, offer_id, and creative_id are required"}`, http.StatusBadRequest)
		return
	}
	if req.DataSet == "" {
		req.DataSet = "IGN"
	}

	ctx := r.Context()

	// Lookup affiliate encoding
	var affiliateEncoded string
	err = h.db.QueryRowContext(ctx,
		"SELECT encoded_value FROM mailing_affiliate_encodings WHERE organization_id = $1 AND affiliate_id = $2",
		orgID, req.AffiliateID,
	).Scan(&affiliateEncoded)
	if err == sql.ErrNoRows {
		http.Error(w, `{"error": "affiliate encoding not found — add it via Affiliate Encodings settings"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "query affiliate: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Lookup offer encoding
	var offerEncoded, trackingDomain string
	err = h.db.QueryRowContext(ctx,
		"SELECT encoded_value, tracking_domain FROM mailing_offer_encodings WHERE organization_id = $1 AND offer_id = $2",
		orgID, req.OfferID,
	).Scan(&offerEncoded, &trackingDomain)
	if err == sql.ErrNoRows {
		http.Error(w, `{"error": "offer encoding not found — add it via Offer Encodings settings"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "query offer: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Check if the org has an active branded tracking domain - if so, use it
	// instead of the default Everflow domain (si3p4trk.com)
	var brandedDomain string
	_ = h.db.QueryRowContext(ctx, `
		SELECT domain FROM mailing_tracking_domains
		WHERE org_id = $1 AND verified = true AND (ssl_status = 'active' OR ssl_provisioned = true)
		ORDER BY created_at ASC LIMIT 1
	`, orgID).Scan(&brandedDomain)

	// If branded domain is available, replace the tracking domain
	effectiveTrackingDomain := trackingDomain
	brandedUsed := false
	if brandedDomain != "" {
		effectiveTrackingDomain = "https://" + brandedDomain
		brandedUsed = true
	}

	// Build the tracking link with merge tag placeholders
	// These get replaced by the sending system at send time:
	//   {{DATE_MMDDYYYY}} -> current date in mmddYYYY format
	//   {{MAILING_ID}}    -> the campaign/mailing ID
	const (
		datePlaceholder      = "{{DATE_MMDDYYYY}}"
		mailingIDPlaceholder = "{{MAILING_ID}}"
	)

	trackingLink := fmt.Sprintf("%s/%s/%s/?creative_id=%d&sub1=%s_%d_%s_%s&sub2=%s_",
		effectiveTrackingDomain,
		affiliateEncoded,
		offerEncoded,
		req.CreativeID,
		req.OfferID,
		req.CreativeID,
		datePlaceholder,
		mailingIDPlaceholder,
		req.DataSet,
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buildTrackingLinkResponse{
		TrackingLink:       trackingLink,
		MergeTagsUsed:      []string{datePlaceholder, mailingIDPlaceholder},
		BrandedDomain:      brandedDomain,
		BrandedDomainUsed:  brandedUsed,
		OriginalDomain:     trackingDomain,
	})
}

// HandleGetManualLinkOffers returns offers that require manual tracking links.
func (h *EverflowCreativesHandler) HandleGetManualLinkOffers(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error": "organization context required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	rows, err := h.db.QueryContext(ctx,
		"SELECT id, offer_name_pattern, description, created_at FROM mailing_manual_link_offers WHERE organization_id = $1 ORDER BY offer_name_pattern",
		orgID,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "query: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type manualOffer struct {
		ID          string `json:"id"`
		OfferName   string `json:"offer_name_pattern"`
		Description string `json:"description"`
		CreatedAt   string `json:"created_at"`
	}

	offers := []manualOffer{}
	for rows.Next() {
		var o manualOffer
		var createdAt time.Time
		if err := rows.Scan(&o.ID, &o.OfferName, &o.Description, &createdAt); err != nil {
			continue
		}
		o.CreatedAt = createdAt.Format(time.RFC3339)
		offers = append(offers, o)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(offers)
}

// HandleGetAffiliateEncodings returns all affiliate encoding mappings.
func (h *EverflowCreativesHandler) HandleGetAffiliateEncodings(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error": "organization context required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	rows, err := h.db.QueryContext(ctx,
		"SELECT id, affiliate_id, encoded_value, COALESCE(affiliate_name, ''), is_default, created_at FROM mailing_affiliate_encodings WHERE organization_id = $1 ORDER BY affiliate_id",
		orgID,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "query: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type encoding struct {
		ID            string `json:"id"`
		AffiliateID   string `json:"affiliate_id"`
		EncodedValue  string `json:"encoded_value"`
		AffiliateName string `json:"affiliate_name"`
		IsDefault     bool   `json:"is_default"`
		CreatedAt     string `json:"created_at"`
	}

	encodings := []encoding{}
	for rows.Next() {
		var e encoding
		var createdAt time.Time
		if err := rows.Scan(&e.ID, &e.AffiliateID, &e.EncodedValue, &e.AffiliateName, &e.IsDefault, &createdAt); err != nil {
			continue
		}
		e.CreatedAt = createdAt.Format(time.RFC3339)
		encodings = append(encodings, e)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(encodings)
}

// HandleGetOfferEncodings returns all offer encoding mappings.
func (h *EverflowCreativesHandler) HandleGetOfferEncodings(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error": "organization context required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	rows, err := h.db.QueryContext(ctx,
		"SELECT id, offer_id, encoded_value, COALESCE(offer_name, ''), tracking_domain, requires_manual_link, created_at FROM mailing_offer_encodings WHERE organization_id = $1 ORDER BY offer_id",
		orgID,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "query: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type encoding struct {
		ID                 string `json:"id"`
		OfferID            string `json:"offer_id"`
		EncodedValue       string `json:"encoded_value"`
		OfferName          string `json:"offer_name"`
		TrackingDomain     string `json:"tracking_domain"`
		RequiresManualLink bool   `json:"requires_manual_link"`
		CreatedAt          string `json:"created_at"`
	}

	encodings := []encoding{}
	for rows.Next() {
		var e encoding
		var createdAt time.Time
		if err := rows.Scan(&e.ID, &e.OfferID, &e.EncodedValue, &e.OfferName, &e.TrackingDomain, &e.RequiresManualLink, &createdAt); err != nil {
			continue
		}
		e.CreatedAt = createdAt.Format(time.RFC3339)
		encodings = append(encodings, e)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(encodings)
}

// minInt returns the smaller of two ints (avoiding conflict with Go 1.21+ builtin).
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
