package api

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// =============================================================================
// PHASE 1B: OFFER LIFECYCLE CRUD HANDLERS
// =============================================================================

// ---------------------------------------------------------------------------
// Null-to-pointer helpers
// ---------------------------------------------------------------------------

func nullStringPtr(ns sql.NullString) *string {
	if ns.Valid {
		return &ns.String
	}
	return nil
}

func nullFloat64Ptr(nf sql.NullFloat64) *float64 {
	if nf.Valid {
		return &nf.Float64
	}
	return nil
}

func nullTimePtr(nt sql.NullTime) *time.Time {
	if nt.Valid {
		return &nt.Time
	}
	return nil
}

// ---------------------------------------------------------------------------
// Phase 1B Types
// ---------------------------------------------------------------------------

type OfferVertical struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	Description    *string   `json:"description"`
	SortOrder      int       `json:"sort_order"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type CreateVerticalRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SortOrder   int    `json:"sort_order"`
}

type UpdateVerticalRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	SortOrder   *int    `json:"sort_order,omitempty"`
}

type OfferRecord struct {
	ID                    string     `json:"id"`
	OrganizationID        string     `json:"organization_id"`
	VerticalID            *string    `json:"vertical_id"`
	BrandName             string     `json:"brand_name"`
	Name                  string     `json:"name"`
	Description           *string    `json:"description"`
	EverflowOfferID       *string    `json:"everflow_offer_id"`
	EverflowCreativeID    *string    `json:"everflow_creative_id"`
	TrackingLinkTemplate  *string    `json:"tracking_link_template"`
	OptizmoLink           *string    `json:"optizmo_link"`
	OfferOptoutLink       *string    `json:"offer_optout_link"`
	WebProperty           *string    `json:"web_property"`
	LandingPageSlug       *string    `json:"landing_page_slug"`
	LandingPageURL        *string    `json:"landing_page_url"`
	LandingPageHTML       *string    `json:"landing_page_html"`
	OriginalHTMLCreative  *string    `json:"original_html_creative"`
	Payout                *float64   `json:"payout"`
	PayoutType            *string    `json:"payout_type"`
	OptizmoStatus            *string    `json:"optizmo_status"`
	OptizmoLastScrubbedAt    *time.Time `json:"optizmo_last_scrubbed_at"`
	SuppressionSyncEnabled   bool       `json:"suppression_sync_enabled"`
	LastSuppressionSyncAt    *time.Time `json:"last_suppression_sync_at"`
	LastSuppressionSyncError *string    `json:"last_suppression_sync_error"`
	Status                   string     `json:"status"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

type CreateOfferRequest struct {
	VerticalID           string   `json:"vertical_id"`
	BrandName            string   `json:"brand_name"`
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	EverflowOfferID      string   `json:"everflow_offer_id"`
	EverflowCreativeID   string   `json:"everflow_creative_id"`
	TrackingLinkTemplate string   `json:"tracking_link_template"`
	OptizmoLink          string   `json:"optizmo_link"`
	OfferOptoutLink      string   `json:"offer_optout_link"`
	WebProperty          string   `json:"web_property"`
	LandingPageSlug      string   `json:"landing_page_slug"`
	LandingPageURL       string   `json:"landing_page_url"`
	LandingPageHTML      string   `json:"landing_page_html"`
	OriginalHTMLCreative string   `json:"original_html_creative"`
	Payout               *float64 `json:"payout"`
	PayoutType           string   `json:"payout_type"`
	Status               string   `json:"status"`
}

type UpdateOfferRequest struct {
	VerticalID           *string  `json:"vertical_id,omitempty"`
	BrandName            *string  `json:"brand_name,omitempty"`
	Name                 *string  `json:"name,omitempty"`
	Description          *string  `json:"description,omitempty"`
	EverflowOfferID      *string  `json:"everflow_offer_id,omitempty"`
	EverflowCreativeID   *string  `json:"everflow_creative_id,omitempty"`
	TrackingLinkTemplate *string  `json:"tracking_link_template,omitempty"`
	OptizmoLink          *string  `json:"optizmo_link,omitempty"`
	OfferOptoutLink      *string  `json:"offer_optout_link,omitempty"`
	WebProperty          *string  `json:"web_property,omitempty"`
	LandingPageSlug      *string  `json:"landing_page_slug,omitempty"`
	LandingPageURL       *string  `json:"landing_page_url,omitempty"`
	LandingPageHTML      *string  `json:"landing_page_html,omitempty"`
	OriginalHTMLCreative *string  `json:"original_html_creative,omitempty"`
	Payout               *float64 `json:"payout,omitempty"`
	PayoutType           *string  `json:"payout_type,omitempty"`
	Status               *string  `json:"status,omitempty"`
}

type SubjectLineRecord struct {
	ID               string    `json:"id"`
	OfferID          string    `json:"offer_id"`
	SubjectLine      string    `json:"subject_line"`
	Status           string    `json:"status"`
	PerformanceScore *float64  `json:"performance_score"`
	TotalSends       int64     `json:"total_sends"`
	TotalOpens       int64     `json:"total_opens"`
	OpenRate         float64   `json:"open_rate"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type CreateSubjectLineRequest struct {
	SubjectLine string `json:"subject_line"`
	Status      string `json:"status"`
}

type UpdateSubjectLineRequest struct {
	SubjectLine *string `json:"subject_line,omitempty"`
	Status      *string `json:"status,omitempty"`
}

type FromNameRecord struct {
	ID               string    `json:"id"`
	OfferID          string    `json:"offer_id"`
	FromName         string    `json:"from_name"`
	Status           string    `json:"status"`
	PerformanceScore *float64  `json:"performance_score"`
	TotalSends       int64     `json:"total_sends"`
	TotalOpens       int64     `json:"total_opens"`
	ComplaintRate    float64   `json:"complaint_rate"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type CreateFromNameRequest struct {
	FromName string `json:"from_name"`
	Status   string `json:"status"`
}

type UpdateFromNameRequest struct {
	FromName *string `json:"from_name,omitempty"`
	Status   *string `json:"status,omitempty"`
}

type OfferCreativeRecord struct {
	ID               string    `json:"id"`
	OfferID          string    `json:"offer_id"`
	Version          int       `json:"version"`
	HTMLContent      *string   `json:"html_content"`
	SubjectLineID    *string   `json:"subject_line_id"`
	FromNameID       *string   `json:"from_name_id"`
	Status           string    `json:"status"`
	ApprovalNotes    *string   `json:"approval_notes"`
	TotalSends       int64     `json:"total_sends"`
	TotalClicks      int64     `json:"total_clicks"`
	TotalOpens       int64     `json:"total_opens"`
	TotalConversions int64     `json:"total_conversions"`
	ClickRate        float64   `json:"click_rate"`
	OpenRate         float64   `json:"open_rate"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type CreateOfferCreativeRequest struct {
	HTMLContent   string `json:"html_content"`
	SubjectLineID string `json:"subject_line_id"`
	FromNameID    string `json:"from_name_id"`
	Status        string `json:"status"`
}

type UpdateOfferCreativeRequest struct {
	HTMLContent   *string `json:"html_content,omitempty"`
	SubjectLineID *string `json:"subject_line_id,omitempty"`
	FromNameID    *string `json:"from_name_id,omitempty"`
	Status        *string `json:"status,omitempty"`
	ApprovalNotes *string `json:"approval_notes,omitempty"`
}

type DeploymentRecord struct {
	ID               string     `json:"id"`
	OfferID          string     `json:"offer_id"`
	CampaignID       *string    `json:"campaign_id"`
	CreativeID       *string    `json:"creative_id"`
	SubjectLineID    *string    `json:"subject_line_id"`
	FromNameID       *string    `json:"from_name_id"`
	AudienceListIDs  []string   `json:"audience_list_ids"`
	DeployedAt       *time.Time `json:"deployed_at"`
	TotalSent        int64      `json:"total_sent"`
	TotalConversions int64      `json:"total_conversions"`
	Revenue          float64    `json:"revenue"`
}

// Tree types

type OfferTreeResponse struct {
	Verticals []TreeVertical `json:"verticals"`
}

type TreeVertical struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	SortOrder int         `json:"sort_order"`
	Brands    []TreeBrand `json:"brands"`
}

type TreeBrand struct {
	BrandName string      `json:"brand_name"`
	Offers    []TreeOffer `json:"offers"`
}

type TreeOffer struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Status           string     `json:"status"`
	LastSentAt       *time.Time `json:"last_sent_at"`
	TotalConversions int64      `json:"total_conversions"`
}

// Performance types

type OfferDetailPerformanceResponse struct {
	OfferID          string                `json:"offer_id"`
	TotalSent        int64                 `json:"total_sent"`
	TotalOpens       int64                 `json:"total_opens"`
	TotalClicks      int64                 `json:"total_clicks"`
	TotalConversions int64                 `json:"total_conversions"`
	Revenue          float64               `json:"revenue"`
	EPC              float64               `json:"epc"`
	OpenRate         float64               `json:"open_rate"`
	ClickRate        float64               `json:"click_rate"`
	ConversionRate   float64               `json:"conversion_rate"`
	ByCreative       []CreativePerformance `json:"by_creative"`
	BySubject        []SubjectPerformance  `json:"by_subject"`
	ByFromName       []FromNamePerformance `json:"by_from_name"`
}

type CreativePerformance struct {
	ID               string  `json:"id"`
	Version          int     `json:"version"`
	Status           string  `json:"status"`
	TotalSends       int64   `json:"total_sends"`
	TotalClicks      int64   `json:"total_clicks"`
	TotalOpens       int64   `json:"total_opens"`
	TotalConversions int64   `json:"total_conversions"`
	ClickRate        float64 `json:"click_rate"`
	OpenRate         float64 `json:"open_rate"`
}

type SubjectPerformance struct {
	ID          string  `json:"id"`
	SubjectLine string  `json:"subject_line"`
	Status      string  `json:"status"`
	TotalSends  int64   `json:"total_sends"`
	TotalOpens  int64   `json:"total_opens"`
	OpenRate    float64 `json:"open_rate"`
}

type FromNamePerformance struct {
	ID            string  `json:"id"`
	FromName      string  `json:"from_name"`
	Status        string  `json:"status"`
	TotalSends    int64   `json:"total_sends"`
	TotalOpens    int64   `json:"total_opens"`
	ComplaintRate float64 `json:"complaint_rate"`
}

// ---------------------------------------------------------------------------
// Offer scan helper
// ---------------------------------------------------------------------------

const offerSelectCols = `id, organization_id, vertical_id, brand_name, name, description, everflow_offer_id, everflow_creative_id, tracking_link_template, optizmo_link, COALESCE(offer_optout_link,''), web_property, landing_page_slug, landing_page_url, landing_page_html, original_html_creative, payout, payout_type, optizmo_status, optizmo_last_scrubbed_at, COALESCE(suppression_sync_enabled, FALSE), last_suppression_sync_at, COALESCE(last_suppression_sync_error, ''), status, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanOfferRow(s rowScanner) (OfferRecord, error) {
	var o OfferRecord
	var verticalID, description, efOfferID, efCreativeID, trackingLink sql.NullString
	var optizmoLink, webProperty, lpSlug, lpURL, lpHTML, origHTML sql.NullString
	var offerOptoutLink string
	var payoutType, optizmoStatus sql.NullString
	var payout sql.NullFloat64
	var optizmoScrubbedAt sql.NullTime
	var lastSyncAt sql.NullTime
	var lastSyncError string

	err := s.Scan(
		&o.ID, &o.OrganizationID, &verticalID, &o.BrandName, &o.Name,
		&description, &efOfferID, &efCreativeID, &trackingLink,
		&optizmoLink, &offerOptoutLink, &webProperty, &lpSlug, &lpURL, &lpHTML, &origHTML,
		&payout, &payoutType, &optizmoStatus, &optizmoScrubbedAt,
		&o.SuppressionSyncEnabled, &lastSyncAt, &lastSyncError,
		&o.Status, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		return o, err
	}

	o.VerticalID = nullStringPtr(verticalID)
	o.Description = nullStringPtr(description)
	o.EverflowOfferID = nullStringPtr(efOfferID)
	o.EverflowCreativeID = nullStringPtr(efCreativeID)
	o.TrackingLinkTemplate = nullStringPtr(trackingLink)
	o.OptizmoLink = nullStringPtr(optizmoLink)
	if offerOptoutLink != "" {
		o.OfferOptoutLink = &offerOptoutLink
	}
	o.WebProperty = nullStringPtr(webProperty)
	o.LandingPageSlug = nullStringPtr(lpSlug)
	o.LandingPageURL = nullStringPtr(lpURL)
	o.LandingPageHTML = nullStringPtr(lpHTML)
	o.OriginalHTMLCreative = nullStringPtr(origHTML)
	o.Payout = nullFloat64Ptr(payout)
	o.PayoutType = nullStringPtr(payoutType)
	o.OptizmoStatus = nullStringPtr(optizmoStatus)
	o.OptizmoLastScrubbedAt = nullTimePtr(optizmoScrubbedAt)
	o.LastSuppressionSyncAt = nullTimePtr(lastSyncAt)
	if lastSyncError != "" {
		o.LastSuppressionSyncError = &lastSyncError
	}

	return o, nil
}

// ---------------------------------------------------------------------------
// VERTICALS CRUD
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleListVerticals(w http.ResponseWriter, r *http.Request) {
	rows, err := och.db.QueryContext(r.Context(),
		`SELECT id, organization_id, name, description, sort_order, created_at, updated_at
		 FROM mailing_offer_verticals WHERE organization_id = $1
		 ORDER BY sort_order, name`, defaultOrgID)
	if err != nil {
		log.Printf("ERROR: list verticals: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to list verticals")
		return
	}
	defer rows.Close()

	verticals := []OfferVertical{}
	for rows.Next() {
		var v OfferVertical
		var desc sql.NullString
		if err := rows.Scan(&v.ID, &v.OrganizationID, &v.Name, &desc, &v.SortOrder, &v.CreatedAt, &v.UpdatedAt); err != nil {
			log.Printf("ERROR: scan vertical: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to list verticals")
			return
		}
		v.Description = nullStringPtr(desc)
		verticals = append(verticals, v)
	}

	respondJSON(w, http.StatusOK, verticals)
}

func (och *OfferCenterHandlers) HandleCreateVertical(w http.ResponseWriter, r *http.Request) {
	var req CreateVerticalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}

	id := uuid.New().String()
	now := time.Now()

	_, err := och.db.ExecContext(r.Context(),
		`INSERT INTO mailing_offer_verticals (id, organization_id, name, description, sort_order, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, defaultOrgID, req.Name, ocNilIfEmpty(req.Description), req.SortOrder, now, now)
	if err != nil {
		if pgErr, ok := err.(*pq.Error); ok && pgErr.Code == "23505" {
			respondError(w, http.StatusConflict, fmt.Sprintf("Vertical '%s' already exists", req.Name))
			return
		}
		log.Printf("ERROR: create vertical: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to create vertical")
		return
	}

	respondJSON(w, http.StatusCreated, OfferVertical{
		ID:             id,
		OrganizationID: defaultOrgID,
		Name:           req.Name,
		Description:    nilStrPtr(req.Description),
		SortOrder:      req.SortOrder,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
}

func (och *OfferCenterHandlers) HandleUpdateVertical(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		respondError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req UpdateVerticalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	var sets []string
	var args []interface{}

	if req.Name != nil {
		args = append(args, *req.Name)
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)))
	}
	if req.Description != nil {
		args = append(args, *req.Description)
		sets = append(sets, fmt.Sprintf("description = $%d", len(args)))
	}
	if req.SortOrder != nil {
		args = append(args, *req.SortOrder)
		sets = append(sets, fmt.Sprintf("sort_order = $%d", len(args)))
	}
	if len(sets) == 0 {
		respondError(w, http.StatusBadRequest, "No fields to update")
		return
	}

	args = append(args, time.Now())
	sets = append(sets, fmt.Sprintf("updated_at = $%d", len(args)))

	args = append(args, id)
	query := fmt.Sprintf(
		"UPDATE mailing_offer_verticals SET %s WHERE id = $%d RETURNING id, organization_id, name, description, sort_order, created_at, updated_at",
		strings.Join(sets, ", "), len(args))

	var v OfferVertical
	var desc sql.NullString
	err := och.db.QueryRowContext(r.Context(), query, args...).Scan(
		&v.ID, &v.OrganizationID, &v.Name, &desc, &v.SortOrder, &v.CreatedAt, &v.UpdatedAt)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "Vertical not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: update vertical %s: %v", id, err)
		respondError(w, http.StatusInternalServerError, "Failed to update vertical")
		return
	}
	v.Description = nullStringPtr(desc)

	respondJSON(w, http.StatusOK, v)
}

func (och *OfferCenterHandlers) HandleDeleteVertical(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		respondError(w, http.StatusBadRequest, "id is required")
		return
	}

	result, err := och.db.ExecContext(r.Context(),
		`DELETE FROM mailing_offer_verticals WHERE id = $1`, id)
	if err != nil {
		log.Printf("ERROR: delete vertical %s: %v", id, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete vertical")
		return
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		respondError(w, http.StatusNotFound, "Vertical not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"deleted": true, "id": id})
}

// ---------------------------------------------------------------------------
// OFFER TREE
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleGetOfferTree(w http.ResponseWriter, r *http.Request) {
	vRows, err := och.db.QueryContext(r.Context(),
		`SELECT id, name, sort_order FROM mailing_offer_verticals
		 WHERE organization_id = $1 ORDER BY sort_order, name`, defaultOrgID)
	if err != nil {
		log.Printf("ERROR: tree verticals query: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to load offer tree")
		return
	}
	defer vRows.Close()

	type vertInfo struct {
		id        string
		name      string
		sortOrder int
	}
	var verts []vertInfo
	for vRows.Next() {
		var vi vertInfo
		if err := vRows.Scan(&vi.id, &vi.name, &vi.sortOrder); err != nil {
			log.Printf("ERROR: scan tree vertical: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to load offer tree")
			return
		}
		verts = append(verts, vi)
	}

	oRows, err := och.db.QueryContext(r.Context(),
		`SELECT o.id, o.name, o.status, o.brand_name, o.vertical_id,
		        MAX(d.deployed_at) AS last_sent_at,
		        COALESCE(SUM(d.total_conversions), 0) AS total_conversions
		 FROM mailing_offers o
		 LEFT JOIN mailing_offer_deployments d ON d.offer_id = o.id
		 WHERE o.organization_id = $1
		 GROUP BY o.id, o.name, o.status, o.brand_name, o.vertical_id
		 ORDER BY o.brand_name, o.name`, defaultOrgID)
	if err != nil {
		log.Printf("ERROR: tree offers query: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to load offer tree")
		return
	}
	defer oRows.Close()

	// vertical_id -> brand_name -> []TreeOffer
	vertBrandOffers := map[string]map[string][]TreeOffer{}
	for oRows.Next() {
		var offerID, offerName, status, brandName string
		var verticalID sql.NullString
		var lastSentAt sql.NullTime
		var totalConversions int64

		if err := oRows.Scan(&offerID, &offerName, &status, &brandName, &verticalID, &lastSentAt, &totalConversions); err != nil {
			log.Printf("ERROR: scan tree offer: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to load offer tree")
			return
		}

		vid := ""
		if verticalID.Valid {
			vid = verticalID.String
		}
		if vertBrandOffers[vid] == nil {
			vertBrandOffers[vid] = map[string][]TreeOffer{}
		}

		to := TreeOffer{
			ID:               offerID,
			Name:             offerName,
			Status:           status,
			TotalConversions: totalConversions,
		}
		if lastSentAt.Valid {
			to.LastSentAt = &lastSentAt.Time
		}
		vertBrandOffers[vid][brandName] = append(vertBrandOffers[vid][brandName], to)
	}

	var tree []TreeVertical
	for _, vi := range verts {
		tv := TreeVertical{
			ID:        vi.id,
			Name:      vi.name,
			SortOrder: vi.sortOrder,
			Brands:    []TreeBrand{},
		}
		if brandMap, ok := vertBrandOffers[vi.id]; ok {
			var brandNames []string
			for bn := range brandMap {
				brandNames = append(brandNames, bn)
			}
			sort.Strings(brandNames)
			for _, bn := range brandNames {
				tv.Brands = append(tv.Brands, TreeBrand{BrandName: bn, Offers: brandMap[bn]})
			}
			delete(vertBrandOffers, vi.id)
		}
		tree = append(tree, tv)
	}

	// Offers with no vertical go into "Uncategorized"
	if brandMap, ok := vertBrandOffers[""]; ok && len(brandMap) > 0 {
		tv := TreeVertical{
			ID:        "",
			Name:      "Uncategorized",
			SortOrder: 9999,
			Brands:    []TreeBrand{},
		}
		var brandNames []string
		for bn := range brandMap {
			brandNames = append(brandNames, bn)
		}
		sort.Strings(brandNames)
		for _, bn := range brandNames {
			tv.Brands = append(tv.Brands, TreeBrand{BrandName: bn, Offers: brandMap[bn]})
		}
		tree = append(tree, tv)
	}

	if tree == nil {
		tree = []TreeVertical{}
	}

	respondJSON(w, http.StatusOK, OfferTreeResponse{Verticals: tree})
}

// ---------------------------------------------------------------------------
// OFFERS CRUD
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleListOffers(w http.ResponseWriter, r *http.Request) {
	verticalID := r.URL.Query().Get("vertical_id")
	status := r.URL.Query().Get("status")
	search := r.URL.Query().Get("search")

	query := fmt.Sprintf("SELECT %s FROM mailing_offers WHERE organization_id = $1", offerSelectCols)
	args := []interface{}{defaultOrgID}

	if verticalID != "" {
		args = append(args, verticalID)
		query += fmt.Sprintf(" AND vertical_id = $%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	if search != "" {
		args = append(args, "%"+search+"%")
		query += fmt.Sprintf(" AND (name ILIKE $%d OR brand_name ILIKE $%d)", len(args), len(args))
	}

	query += " ORDER BY brand_name, name"

	rows, err := och.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		log.Printf("ERROR: list offers: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to list offers")
		return
	}
	defer rows.Close()

	offers := []OfferRecord{}
	for rows.Next() {
		o, err := scanOfferRow(rows)
		if err != nil {
			log.Printf("ERROR: scan offer row: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to list offers")
			return
		}
		offers = append(offers, o)
	}

	respondJSON(w, http.StatusOK, offers)
}

func (och *OfferCenterHandlers) HandleCreateOffer(w http.ResponseWriter, r *http.Request) {
	var req CreateOfferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.BrandName == "" {
		respondError(w, http.StatusBadRequest, "brand_name is required")
		return
	}
	if req.Status == "" {
		req.Status = "active"
	}

	id := uuid.New().String()
	now := time.Now()

	_, err := och.db.ExecContext(r.Context(),
		`INSERT INTO mailing_offers
			(id, organization_id, vertical_id, brand_name, name, description,
			 everflow_offer_id, everflow_creative_id, tracking_link_template, optizmo_link, offer_optout_link,
			 web_property, landing_page_slug, landing_page_url, landing_page_html,
			 original_html_creative, payout, payout_type, status, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		id, defaultOrgID,
		ocNilIfEmpty(req.VerticalID), req.BrandName, req.Name, ocNilIfEmpty(req.Description),
		ocNilIfEmpty(req.EverflowOfferID), ocNilIfEmpty(req.EverflowCreativeID),
		ocNilIfEmpty(req.TrackingLinkTemplate), ocNilIfEmpty(req.OptizmoLink), ocNilIfEmpty(req.OfferOptoutLink),
		ocNilIfEmpty(req.WebProperty), ocNilIfEmpty(req.LandingPageSlug),
		ocNilIfEmpty(req.LandingPageURL), ocNilIfEmpty(req.LandingPageHTML),
		ocNilIfEmpty(req.OriginalHTMLCreative), req.Payout, ocNilIfEmpty(req.PayoutType),
		req.Status, now, now)
	if err != nil {
		if pgErr, ok := err.(*pq.Error); ok && pgErr.Code == "23505" {
			respondError(w, http.StatusConflict, "An offer with these details already exists")
			return
		}
		log.Printf("ERROR: create offer: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to create offer")
		return
	}

	row := och.db.QueryRowContext(r.Context(),
		fmt.Sprintf("SELECT %s FROM mailing_offers WHERE id = $1", offerSelectCols), id)
	o, err := scanOfferRow(row)
	if err != nil {
		log.Printf("ERROR: fetch created offer %s: %v", id, err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve created offer")
		return
	}

	respondJSON(w, http.StatusCreated, o)
}

func (och *OfferCenterHandlers) HandleGetOffer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		respondError(w, http.StatusBadRequest, "id is required")
		return
	}

	row := och.db.QueryRowContext(r.Context(),
		fmt.Sprintf("SELECT %s FROM mailing_offers WHERE id = $1", offerSelectCols), id)
	o, err := scanOfferRow(row)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "Offer not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: get offer %s: %v", id, err)
		respondError(w, http.StatusInternalServerError, "Failed to get offer")
		return
	}

	respondJSON(w, http.StatusOK, o)
}

func (och *OfferCenterHandlers) HandleUpdateOffer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		respondError(w, http.StatusBadRequest, "id is required")
		return
	}

	var req UpdateOfferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	var sets []string
	var args []interface{}

	addStr := func(col string, val *string, nullable bool) {
		if val == nil {
			return
		}
		if nullable {
			args = append(args, ocNilIfEmpty(*val))
		} else {
			args = append(args, *val)
		}
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}

	addStr("vertical_id", req.VerticalID, true)
	addStr("brand_name", req.BrandName, false)
	addStr("name", req.Name, false)
	addStr("description", req.Description, true)
	addStr("everflow_offer_id", req.EverflowOfferID, true)
	addStr("everflow_creative_id", req.EverflowCreativeID, true)
	addStr("tracking_link_template", req.TrackingLinkTemplate, true)
	addStr("optizmo_link", req.OptizmoLink, true)
	addStr("offer_optout_link", req.OfferOptoutLink, true)
	addStr("web_property", req.WebProperty, true)
	addStr("landing_page_slug", req.LandingPageSlug, true)
	addStr("landing_page_url", req.LandingPageURL, true)
	addStr("landing_page_html", req.LandingPageHTML, true)
	addStr("original_html_creative", req.OriginalHTMLCreative, true)
	addStr("payout_type", req.PayoutType, true)
	addStr("status", req.Status, false)

	if req.Payout != nil {
		args = append(args, *req.Payout)
		sets = append(sets, fmt.Sprintf("payout = $%d", len(args)))
	}

	if len(sets) == 0 {
		respondError(w, http.StatusBadRequest, "No fields to update")
		return
	}

	args = append(args, time.Now())
	sets = append(sets, fmt.Sprintf("updated_at = $%d", len(args)))

	args = append(args, id)
	query := fmt.Sprintf("UPDATE mailing_offers SET %s WHERE id = $%d RETURNING %s",
		strings.Join(sets, ", "), len(args), offerSelectCols)

	o, err := scanOfferRow(och.db.QueryRowContext(r.Context(), query, args...))
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "Offer not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: update offer %s: %v", id, err)
		respondError(w, http.StatusInternalServerError, "Failed to update offer")
		return
	}

	respondJSON(w, http.StatusOK, o)
}

func (och *OfferCenterHandlers) HandleDeleteOffer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		respondError(w, http.StatusBadRequest, "id is required")
		return
	}

	result, err := och.db.ExecContext(r.Context(),
		`DELETE FROM mailing_offers WHERE id = $1`, id)
	if err != nil {
		log.Printf("ERROR: delete offer %s: %v", id, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete offer")
		return
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		respondError(w, http.StatusNotFound, "Offer not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"deleted": true, "id": id})
}

// ---------------------------------------------------------------------------
// SUBJECT LINES CRUD
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleListSubjectLines(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	rows, err := och.db.QueryContext(r.Context(),
		`SELECT id, offer_id, subject_line, status, performance_score,
		        total_sends, total_opens, open_rate, created_at, updated_at
		 FROM mailing_offer_subject_lines WHERE offer_id = $1
		 ORDER BY created_at DESC`, offerID)
	if err != nil {
		log.Printf("ERROR: list subject lines for offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to list subject lines")
		return
	}
	defer rows.Close()

	items := []SubjectLineRecord{}
	for rows.Next() {
		var s SubjectLineRecord
		var perfScore sql.NullFloat64
		if err := rows.Scan(&s.ID, &s.OfferID, &s.SubjectLine, &s.Status, &perfScore,
			&s.TotalSends, &s.TotalOpens, &s.OpenRate, &s.CreatedAt, &s.UpdatedAt); err != nil {
			log.Printf("ERROR: scan subject line: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to list subject lines")
			return
		}
		s.PerformanceScore = nullFloat64Ptr(perfScore)
		items = append(items, s)
	}

	respondJSON(w, http.StatusOK, items)
}

func (och *OfferCenterHandlers) HandleCreateSubjectLine(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	var req CreateSubjectLineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.SubjectLine == "" {
		respondError(w, http.StatusBadRequest, "subject_line is required")
		return
	}
	if req.Status == "" {
		req.Status = "active"
	}

	id := uuid.New().String()
	now := time.Now()

	_, err := och.db.ExecContext(r.Context(),
		`INSERT INTO mailing_offer_subject_lines
		 (id, offer_id, subject_line, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, offerID, req.SubjectLine, req.Status, now, now)
	if err != nil {
		log.Printf("ERROR: create subject line: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to create subject line")
		return
	}

	respondJSON(w, http.StatusCreated, SubjectLineRecord{
		ID:          id,
		OfferID:     offerID,
		SubjectLine: req.SubjectLine,
		Status:      req.Status,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
}

func (och *OfferCenterHandlers) HandleUpdateSubjectLine(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	sid := chi.URLParam(r, "sid")
	if offerID == "" || sid == "" {
		respondError(w, http.StatusBadRequest, "offer id and subject line id are required")
		return
	}

	var req UpdateSubjectLineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	var sets []string
	var args []interface{}

	if req.SubjectLine != nil {
		args = append(args, *req.SubjectLine)
		sets = append(sets, fmt.Sprintf("subject_line = $%d", len(args)))
	}
	if req.Status != nil {
		args = append(args, *req.Status)
		sets = append(sets, fmt.Sprintf("status = $%d", len(args)))
	}
	if len(sets) == 0 {
		respondError(w, http.StatusBadRequest, "No fields to update")
		return
	}

	args = append(args, time.Now())
	sets = append(sets, fmt.Sprintf("updated_at = $%d", len(args)))

	args = append(args, sid)
	args = append(args, offerID)
	query := fmt.Sprintf(
		`UPDATE mailing_offer_subject_lines SET %s WHERE id = $%d AND offer_id = $%d
		 RETURNING id, offer_id, subject_line, status, performance_score, total_sends, total_opens, open_rate, created_at, updated_at`,
		strings.Join(sets, ", "), len(args)-1, len(args))

	var s SubjectLineRecord
	var perfScore sql.NullFloat64
	err := och.db.QueryRowContext(r.Context(), query, args...).Scan(
		&s.ID, &s.OfferID, &s.SubjectLine, &s.Status, &perfScore,
		&s.TotalSends, &s.TotalOpens, &s.OpenRate, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "Subject line not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: update subject line %s: %v", sid, err)
		respondError(w, http.StatusInternalServerError, "Failed to update subject line")
		return
	}
	s.PerformanceScore = nullFloat64Ptr(perfScore)

	respondJSON(w, http.StatusOK, s)
}

func (och *OfferCenterHandlers) HandleDeleteSubjectLine(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	sid := chi.URLParam(r, "sid")
	if offerID == "" || sid == "" {
		respondError(w, http.StatusBadRequest, "offer id and subject line id are required")
		return
	}

	result, err := och.db.ExecContext(r.Context(),
		`DELETE FROM mailing_offer_subject_lines WHERE id = $1 AND offer_id = $2`, sid, offerID)
	if err != nil {
		log.Printf("ERROR: delete subject line %s: %v", sid, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete subject line")
		return
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		respondError(w, http.StatusNotFound, "Subject line not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"deleted": true, "id": sid})
}

// ---------------------------------------------------------------------------
// FROM NAMES CRUD
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleListFromNames(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	rows, err := och.db.QueryContext(r.Context(),
		`SELECT id, offer_id, from_name, status, performance_score,
		        total_sends, total_opens, complaint_rate, created_at, updated_at
		 FROM mailing_offer_from_names WHERE offer_id = $1
		 ORDER BY created_at DESC`, offerID)
	if err != nil {
		log.Printf("ERROR: list from names for offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to list from names")
		return
	}
	defer rows.Close()

	items := []FromNameRecord{}
	for rows.Next() {
		var f FromNameRecord
		var perfScore sql.NullFloat64
		if err := rows.Scan(&f.ID, &f.OfferID, &f.FromName, &f.Status, &perfScore,
			&f.TotalSends, &f.TotalOpens, &f.ComplaintRate, &f.CreatedAt, &f.UpdatedAt); err != nil {
			log.Printf("ERROR: scan from name: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to list from names")
			return
		}
		f.PerformanceScore = nullFloat64Ptr(perfScore)
		items = append(items, f)
	}

	respondJSON(w, http.StatusOK, items)
}

func (och *OfferCenterHandlers) HandleCreateFromName(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	var req CreateFromNameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.FromName == "" {
		respondError(w, http.StatusBadRequest, "from_name is required")
		return
	}
	if req.Status == "" {
		req.Status = "active"
	}

	id := uuid.New().String()
	now := time.Now()

	_, err := och.db.ExecContext(r.Context(),
		`INSERT INTO mailing_offer_from_names
		 (id, offer_id, from_name, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, offerID, req.FromName, req.Status, now, now)
	if err != nil {
		log.Printf("ERROR: create from name: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to create from name")
		return
	}

	respondJSON(w, http.StatusCreated, FromNameRecord{
		ID:        id,
		OfferID:   offerID,
		FromName:  req.FromName,
		Status:    req.Status,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

func (och *OfferCenterHandlers) HandleUpdateFromName(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	fid := chi.URLParam(r, "fid")
	if offerID == "" || fid == "" {
		respondError(w, http.StatusBadRequest, "offer id and from name id are required")
		return
	}

	var req UpdateFromNameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	var sets []string
	var args []interface{}

	if req.FromName != nil {
		args = append(args, *req.FromName)
		sets = append(sets, fmt.Sprintf("from_name = $%d", len(args)))
	}
	if req.Status != nil {
		args = append(args, *req.Status)
		sets = append(sets, fmt.Sprintf("status = $%d", len(args)))
	}
	if len(sets) == 0 {
		respondError(w, http.StatusBadRequest, "No fields to update")
		return
	}

	args = append(args, time.Now())
	sets = append(sets, fmt.Sprintf("updated_at = $%d", len(args)))

	args = append(args, fid)
	args = append(args, offerID)
	query := fmt.Sprintf(
		`UPDATE mailing_offer_from_names SET %s WHERE id = $%d AND offer_id = $%d
		 RETURNING id, offer_id, from_name, status, performance_score, total_sends, total_opens, complaint_rate, created_at, updated_at`,
		strings.Join(sets, ", "), len(args)-1, len(args))

	var f FromNameRecord
	var perfScore sql.NullFloat64
	err := och.db.QueryRowContext(r.Context(), query, args...).Scan(
		&f.ID, &f.OfferID, &f.FromName, &f.Status, &perfScore,
		&f.TotalSends, &f.TotalOpens, &f.ComplaintRate, &f.CreatedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "From name not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: update from name %s: %v", fid, err)
		respondError(w, http.StatusInternalServerError, "Failed to update from name")
		return
	}
	f.PerformanceScore = nullFloat64Ptr(perfScore)

	respondJSON(w, http.StatusOK, f)
}

func (och *OfferCenterHandlers) HandleDeleteFromName(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	fid := chi.URLParam(r, "fid")
	if offerID == "" || fid == "" {
		respondError(w, http.StatusBadRequest, "offer id and from name id are required")
		return
	}

	result, err := och.db.ExecContext(r.Context(),
		`DELETE FROM mailing_offer_from_names WHERE id = $1 AND offer_id = $2`, fid, offerID)
	if err != nil {
		log.Printf("ERROR: delete from name %s: %v", fid, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete from name")
		return
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		respondError(w, http.StatusNotFound, "From name not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"deleted": true, "id": fid})
}

// ---------------------------------------------------------------------------
// OFFER CREATIVES CRUD (mailing_offer_creatives table)
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleListOfferCreatives(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	rows, err := och.db.QueryContext(r.Context(),
		`SELECT id, offer_id, version, html_content, subject_line_id, from_name_id,
		        status, approval_notes, total_sends, total_clicks, total_opens, total_conversions,
		        click_rate, open_rate, created_at, updated_at
		 FROM mailing_offer_creatives WHERE offer_id = $1
		 ORDER BY version DESC`, offerID)
	if err != nil {
		log.Printf("ERROR: list offer creatives for %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to list offer creatives")
		return
	}
	defer rows.Close()

	items := []OfferCreativeRecord{}
	for rows.Next() {
		var c OfferCreativeRecord
		var htmlContent, subjectLineID, fromNameID, approvalNotes sql.NullString
		if err := rows.Scan(&c.ID, &c.OfferID, &c.Version, &htmlContent, &subjectLineID, &fromNameID,
			&c.Status, &approvalNotes, &c.TotalSends, &c.TotalClicks, &c.TotalOpens, &c.TotalConversions,
			&c.ClickRate, &c.OpenRate, &c.CreatedAt, &c.UpdatedAt); err != nil {
			log.Printf("ERROR: scan offer creative: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to list offer creatives")
			return
		}
		c.HTMLContent = nullStringPtr(htmlContent)
		c.SubjectLineID = nullStringPtr(subjectLineID)
		c.FromNameID = nullStringPtr(fromNameID)
		c.ApprovalNotes = nullStringPtr(approvalNotes)
		items = append(items, c)
	}

	respondJSON(w, http.StatusOK, items)
}

func (och *OfferCenterHandlers) HandleCreateOfferCreative(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	var req CreateOfferCreativeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.Status == "" {
		req.Status = "draft"
	}

	id := uuid.New().String()
	now := time.Now()

	htmlToStore := req.HTMLContent
	if htmlToStore != "" {
		// {{ brand.domain }} resolves at send to the actual sending brand (item.BrandRoot),
		// so the footer reads the brand it was mailed from, never a baked-in/wrong brand.
		htmlToStore = appendUnsubDisclaimer(htmlToStore, "{{ brand.domain }}", "")
	}

	var version int
	err := och.db.QueryRowContext(r.Context(),
		`INSERT INTO mailing_offer_creatives
		 (id, offer_id, version, html_content, subject_line_id, from_name_id, status, created_at, updated_at)
		 VALUES ($1, $2, COALESCE((SELECT MAX(version) FROM mailing_offer_creatives WHERE offer_id = $2), 0) + 1,
		         $3, $4, $5, $6, $7, $8)
		 RETURNING version`,
		id, offerID,
		ocNilIfEmpty(htmlToStore), ocNilIfEmpty(req.SubjectLineID), ocNilIfEmpty(req.FromNameID),
		req.Status, now, now).Scan(&version)
	if err != nil {
		log.Printf("ERROR: create offer creative: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to create offer creative")
		return
	}

	respondJSON(w, http.StatusCreated, OfferCreativeRecord{
		ID:            id,
		OfferID:       offerID,
		Version:       version,
		HTMLContent:   nilStrPtr(req.HTMLContent),
		SubjectLineID: nilStrPtr(req.SubjectLineID),
		FromNameID:    nilStrPtr(req.FromNameID),
		Status:        req.Status,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

func (och *OfferCenterHandlers) HandleUpdateOfferCreative(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	cid := chi.URLParam(r, "cid")
	if offerID == "" || cid == "" {
		respondError(w, http.StatusBadRequest, "offer id and creative id are required")
		return
	}

	var req UpdateOfferCreativeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	var sets []string
	var args []interface{}

	if req.HTMLContent != nil {
		html := *req.HTMLContent
		if html != "" {
			html = appendUnsubDisclaimer(html, "{{ brand.domain }}", "")
		}
		args = append(args, html)
		sets = append(sets, fmt.Sprintf("html_content = $%d", len(args)))
	}
	if req.SubjectLineID != nil {
		args = append(args, ocNilIfEmpty(*req.SubjectLineID))
		sets = append(sets, fmt.Sprintf("subject_line_id = $%d", len(args)))
	}
	if req.FromNameID != nil {
		args = append(args, ocNilIfEmpty(*req.FromNameID))
		sets = append(sets, fmt.Sprintf("from_name_id = $%d", len(args)))
	}
	if req.Status != nil {
		args = append(args, *req.Status)
		sets = append(sets, fmt.Sprintf("status = $%d", len(args)))
	}
	if req.ApprovalNotes != nil {
		args = append(args, *req.ApprovalNotes)
		sets = append(sets, fmt.Sprintf("approval_notes = $%d", len(args)))
	}
	if len(sets) == 0 {
		respondError(w, http.StatusBadRequest, "No fields to update")
		return
	}

	args = append(args, time.Now())
	sets = append(sets, fmt.Sprintf("updated_at = $%d", len(args)))

	args = append(args, cid)
	args = append(args, offerID)
	query := fmt.Sprintf(
		`UPDATE mailing_offer_creatives SET %s WHERE id = $%d AND offer_id = $%d
		 RETURNING id, offer_id, version, html_content, subject_line_id, from_name_id,
		           status, approval_notes, total_sends, total_clicks, total_opens, total_conversions,
		           click_rate, open_rate, created_at, updated_at`,
		strings.Join(sets, ", "), len(args)-1, len(args))

	var c OfferCreativeRecord
	var htmlContent, subjectLineID, fromNameID, approvalNotes sql.NullString
	err := och.db.QueryRowContext(r.Context(), query, args...).Scan(
		&c.ID, &c.OfferID, &c.Version, &htmlContent, &subjectLineID, &fromNameID,
		&c.Status, &approvalNotes, &c.TotalSends, &c.TotalClicks, &c.TotalOpens, &c.TotalConversions,
		&c.ClickRate, &c.OpenRate, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "Offer creative not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: update offer creative %s: %v", cid, err)
		respondError(w, http.StatusInternalServerError, "Failed to update offer creative")
		return
	}
	c.HTMLContent = nullStringPtr(htmlContent)
	c.SubjectLineID = nullStringPtr(subjectLineID)
	c.FromNameID = nullStringPtr(fromNameID)
	c.ApprovalNotes = nullStringPtr(approvalNotes)

	respondJSON(w, http.StatusOK, c)
}

func (och *OfferCenterHandlers) HandleDeleteOfferCreative(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	cid := chi.URLParam(r, "cid")
	if offerID == "" || cid == "" {
		respondError(w, http.StatusBadRequest, "offer id and creative id are required")
		return
	}

	result, err := och.db.ExecContext(r.Context(),
		`DELETE FROM mailing_offer_creatives WHERE id = $1 AND offer_id = $2`, cid, offerID)
	if err != nil {
		log.Printf("ERROR: delete offer creative %s: %v", cid, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete offer creative")
		return
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		respondError(w, http.StatusNotFound, "Offer creative not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"deleted": true, "id": cid})
}

func (och *OfferCenterHandlers) HandleDeleteAllOfferCreatives(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}
	result, err := och.db.ExecContext(r.Context(),
		`DELETE FROM mailing_offer_creatives WHERE offer_id = $1`, offerID)
	if err != nil {
		log.Printf("ERROR: delete all creatives for offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete creatives")
		return
	}
	n, _ := result.RowsAffected()
	respondJSON(w, http.StatusOK, map[string]interface{}{"deleted": n})
}

// HandleDownloadCreativesZip — GET /offer-center/offers/{id}/creatives/download-zip
// Serves a zip file containing each creative as a separate .html file with a human-readable name.
func (och *OfferCenterHandlers) HandleDownloadCreativesZip(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	var offerName string
	och.db.QueryRowContext(r.Context(), `SELECT name FROM mailing_offers WHERE id=$1`, offerID).Scan(&offerName)

	rows, err := och.db.QueryContext(r.Context(),
		`SELECT id, version, html_content, COALESCE(status,''), COALESCE(approval_notes,'')
		 FROM mailing_offer_creatives
		 WHERE offer_id = $1 AND html_content != ''
		 ORDER BY version ASC, created_at ASC`, offerID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to query creatives")
		return
	}
	defer rows.Close()

	slugRe := regexp.MustCompile(`[^a-zA-Z0-9]+`)
	safeName := slugRe.ReplaceAllString(offerName, "_")
	if safeName == "" || safeName == "_" {
		safeName = "creatives"
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	count := 0

	for rows.Next() {
		var id, html, status, notes string
		var version int
		if err := rows.Scan(&id, &version, &html, &status, &notes); err != nil {
			continue
		}
		count++

		label := slugRe.ReplaceAllString(notes, "_")
		if len(label) > 60 {
			label = label[:60]
		}
		if label == "" || label == "_" {
			label = fmt.Sprintf("creative_%d", count)
		}

		filename := fmt.Sprintf("%s_v%d_%s_%s.html", safeName, version, status, label)
		fw, err := zw.Create(filename)
		if err != nil {
			continue
		}
		fw.Write([]byte(html))
	}

	zw.Close()

	if count == 0 {
		respondError(w, http.StatusNotFound, "No creatives to download")
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s_creatives.zip"`, safeName))
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// ---------------------------------------------------------------------------
// DEPLOYMENTS (read-only)
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleListOfferDeployments(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	rows, err := och.db.QueryContext(r.Context(),
		`SELECT id, offer_id, campaign_id, creative_id, subject_line_id, from_name_id,
		        audience_list_ids, deployed_at, total_sent, total_conversions, revenue
		 FROM mailing_offer_deployments WHERE offer_id = $1
		 ORDER BY deployed_at DESC`, offerID)
	if err != nil {
		log.Printf("ERROR: list deployments for offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to list deployments")
		return
	}
	defer rows.Close()

	items := []DeploymentRecord{}
	for rows.Next() {
		var d DeploymentRecord
		var campaignID, creativeID, subjectLineID, fromNameID sql.NullString
		var deployedAt sql.NullTime
		var audienceIDs []byte

		if err := rows.Scan(&d.ID, &d.OfferID, &campaignID, &creativeID, &subjectLineID, &fromNameID,
			&audienceIDs, &deployedAt, &d.TotalSent, &d.TotalConversions, &d.Revenue); err != nil {
			log.Printf("ERROR: scan deployment: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to list deployments")
			return
		}
		d.CampaignID = nullStringPtr(campaignID)
		d.CreativeID = nullStringPtr(creativeID)
		d.SubjectLineID = nullStringPtr(subjectLineID)
		d.FromNameID = nullStringPtr(fromNameID)
		d.DeployedAt = nullTimePtr(deployedAt)
		if len(audienceIDs) > 0 {
			d.AudienceListIDs = parsePgTextArray(string(audienceIDs))
		} else {
			d.AudienceListIDs = []string{}
		}
		items = append(items, d)
	}

	respondJSON(w, http.StatusOK, items)
}

// ---------------------------------------------------------------------------
// PERFORMANCE ENDPOINTS
// ---------------------------------------------------------------------------

func (och *OfferCenterHandlers) HandleGetOfferDetailPerformance(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id is required")
		return
	}

	ctx := r.Context()
	resp := OfferDetailPerformanceResponse{
		OfferID:    offerID,
		ByCreative: []CreativePerformance{},
		BySubject:  []SubjectPerformance{},
		ByFromName: []FromNamePerformance{},
	}

	// Aggregate from deployments
	och.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_sent), 0), COALESCE(SUM(total_conversions), 0), COALESCE(SUM(revenue), 0)
		 FROM mailing_offer_deployments WHERE offer_id = $1`, offerID).
		Scan(&resp.TotalSent, &resp.TotalConversions, &resp.Revenue)

	// Aggregate opens/clicks from creatives
	cRows, err := och.db.QueryContext(ctx,
		`SELECT id, version, status, total_sends, total_clicks, total_opens, total_conversions, click_rate, open_rate
		 FROM mailing_offer_creatives WHERE offer_id = $1 ORDER BY version`, offerID)
	if err == nil {
		defer cRows.Close()
		for cRows.Next() {
			var cp CreativePerformance
			if err := cRows.Scan(&cp.ID, &cp.Version, &cp.Status, &cp.TotalSends, &cp.TotalClicks,
				&cp.TotalOpens, &cp.TotalConversions, &cp.ClickRate, &cp.OpenRate); err == nil {
				resp.TotalOpens += cp.TotalOpens
				resp.TotalClicks += cp.TotalClicks
				resp.ByCreative = append(resp.ByCreative, cp)
			}
		}
	}

	// Subject line breakdown
	sRows, err := och.db.QueryContext(ctx,
		`SELECT id, subject_line, status, total_sends, total_opens, open_rate
		 FROM mailing_offer_subject_lines WHERE offer_id = $1 ORDER BY total_sends DESC`, offerID)
	if err == nil {
		defer sRows.Close()
		for sRows.Next() {
			var sp SubjectPerformance
			if err := sRows.Scan(&sp.ID, &sp.SubjectLine, &sp.Status, &sp.TotalSends, &sp.TotalOpens, &sp.OpenRate); err == nil {
				resp.BySubject = append(resp.BySubject, sp)
			}
		}
	}

	// From name breakdown
	fRows, err := och.db.QueryContext(ctx,
		`SELECT id, from_name, status, total_sends, total_opens, complaint_rate
		 FROM mailing_offer_from_names WHERE offer_id = $1 ORDER BY total_sends DESC`, offerID)
	if err == nil {
		defer fRows.Close()
		for fRows.Next() {
			var fp FromNamePerformance
			if err := fRows.Scan(&fp.ID, &fp.FromName, &fp.Status, &fp.TotalSends, &fp.TotalOpens, &fp.ComplaintRate); err == nil {
				resp.ByFromName = append(resp.ByFromName, fp)
			}
		}
	}

	// Compute rates
	if resp.TotalClicks > 0 {
		resp.EPC = roundTo(resp.Revenue/float64(resp.TotalClicks), 4)
		resp.ConversionRate = roundTo(float64(resp.TotalConversions)/float64(resp.TotalClicks), 4)
	}
	if resp.TotalSent > 0 {
		resp.OpenRate = roundTo(float64(resp.TotalOpens)/float64(resp.TotalSent), 4)
		resp.ClickRate = roundTo(float64(resp.TotalClicks)/float64(resp.TotalSent), 4)
	}

	respondJSON(w, http.StatusOK, resp)
}

func (och *OfferCenterHandlers) HandleGetCreativePerformance(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	cid := chi.URLParam(r, "cid")
	if offerID == "" || cid == "" {
		respondError(w, http.StatusBadRequest, "offer id and creative id are required")
		return
	}

	var cp CreativePerformance
	err := och.db.QueryRowContext(r.Context(),
		`SELECT id, version, status, total_sends, total_clicks, total_opens, total_conversions, click_rate, open_rate
		 FROM mailing_offer_creatives WHERE id = $1 AND offer_id = $2`, cid, offerID).
		Scan(&cp.ID, &cp.Version, &cp.Status, &cp.TotalSends, &cp.TotalClicks,
			&cp.TotalOpens, &cp.TotalConversions, &cp.ClickRate, &cp.OpenRate)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "Creative not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: get creative performance %s: %v", cid, err)
		respondError(w, http.StatusInternalServerError, "Failed to get creative performance")
		return
	}

	respondJSON(w, http.StatusOK, cp)
}

func (och *OfferCenterHandlers) HandleGetSubjectPerformance(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	sid := chi.URLParam(r, "sid")
	if offerID == "" || sid == "" {
		respondError(w, http.StatusBadRequest, "offer id and subject line id are required")
		return
	}

	var sp SubjectPerformance
	err := och.db.QueryRowContext(r.Context(),
		`SELECT id, subject_line, status, total_sends, total_opens, open_rate
		 FROM mailing_offer_subject_lines WHERE id = $1 AND offer_id = $2`, sid, offerID).
		Scan(&sp.ID, &sp.SubjectLine, &sp.Status, &sp.TotalSends, &sp.TotalOpens, &sp.OpenRate)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "Subject line not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: get subject performance %s: %v", sid, err)
		respondError(w, http.StatusInternalServerError, "Failed to get subject line performance")
		return
	}

	respondJSON(w, http.StatusOK, sp)
}

func (och *OfferCenterHandlers) HandleGetFromNamePerformance(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	fid := chi.URLParam(r, "fid")
	if offerID == "" || fid == "" {
		respondError(w, http.StatusBadRequest, "offer id and from name id are required")
		return
	}

	var fp FromNamePerformance
	err := och.db.QueryRowContext(r.Context(),
		`SELECT id, from_name, status, total_sends, total_opens, complaint_rate
		 FROM mailing_offer_from_names WHERE id = $1 AND offer_id = $2`, fid, offerID).
		Scan(&fp.ID, &fp.FromName, &fp.Status, &fp.TotalSends, &fp.TotalOpens, &fp.ComplaintRate)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "From name not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: get from name performance %s: %v", fid, err)
		respondError(w, http.StatusInternalServerError, "Failed to get from name performance")
		return
	}

	respondJSON(w, http.StatusOK, fp)
}
