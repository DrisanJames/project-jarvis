package api

// CPM Planner — the operator's living pricing/pacing screen for email CPM
// deals. A deal is "budget × eCPM goal × eCPA goal × avg campaign size";
// the planner derives the planned volume, conversions needed, and days to
// finish (the operator's spreadsheet math), persists the deal, and then maps
// LIVE platform delivery back onto it:
//
//	planned_volume     = budget / ecpm_goal * 1000
//	conversions_needed = ceil(budget / ecpa_goal)
//	days_to_finish     = ceil(planned_volume / avg_campaign_size)
//
// Ground truth sources (campaign counter columns are STALE — never used):
//   - delivery: mailing_tracking_events for campaigns whose offer_id matches
//     the deal's offer (hard/soft bounce split via HardBounceSQL)
//   - conversions: mailing_offer_suppressions reason='converted'
//     (everflow postbacks write these rows)
//   - payout: mailing_offers.payout
//
// Capacity risk compares the sum of active deals' required daily volume
// against the platform's 14-day average daily 'sent' trend.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type CpmPlannerHandlers struct {
	db *sql.DB
}

func NewCpmPlannerHandlers(db *sql.DB) *CpmPlannerHandlers {
	h := &CpmPlannerHandlers{db: db}
	h.ensureTables()
	return h
}

func (h *CpmPlannerHandlers) ensureTables() {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS mailing_cpm_deals (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			name TEXT NOT NULL,
			offer_id UUID,
			everflow_offer_id VARCHAR(64) DEFAULT '',
			budget NUMERIC(12,2) NOT NULL,
			ecpm_goal NUMERIC(8,4) NOT NULL,
			ecpa_goal NUMERIC(10,2),
			avg_campaign_size INTEGER NOT NULL DEFAULT 160000,
			start_date DATE DEFAULT CURRENT_DATE,
			status TEXT NOT NULL DEFAULT 'active',
			notes TEXT DEFAULT '',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cpm_deals_org ON mailing_cpm_deals(organization_id, status)`,
	}
	for _, s := range stmts {
		if _, err := h.db.Exec(s); err != nil {
			log.Printf("[CpmPlanner] ensure tables: %v", err)
		}
	}
}

// ─── Types ──────────────────────────────────────────────────────────────────

type cpmDealProgress struct {
	Sent         int64   `json:"sent"`
	Delivered    int64   `json:"delivered"`
	Opened       int64   `json:"opened"`
	Clicked      int64   `json:"clicked"`
	HardBounces  int64   `json:"hard_bounces"`
	SoftBounces  int64   `json:"soft_bounces"`
	Conversions  int64   `json:"conversions"`
	Payout       float64 `json:"payout"`
	PctDelivered float64 `json:"pct_volume_delivered"` // sent / planned_volume (0..1+)
	Revenue      float64 `json:"revenue_earned"`
	ActualEcpm   float64 `json:"actual_ecpm"`
	ActualEcpa   float64 `json:"actual_ecpa"` // proportional budget spent / conversions; 0 when no conversions
	DaysElapsed  int64   `json:"days_elapsed"`
	RequiredDaily float64 `json:"required_daily"`
	ActualDaily   float64 `json:"actual_daily"`
	OnPace        bool    `json:"on_pace"`
}

type cpmDeal struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	OfferID         string    `json:"offer_id"` // effective offer id ('' when unmapped)
	OfferName       string    `json:"offer_name"`
	EverflowOfferID string    `json:"everflow_offer_id"`
	Budget          float64   `json:"budget"`
	EcpmGoal        float64   `json:"ecpm_goal"`
	EcpaGoal        float64   `json:"ecpa_goal"`
	AvgCampaignSize int       `json:"avg_campaign_size"`
	StartDate       string    `json:"start_date"` // YYYY-MM-DD
	Status          string    `json:"status"`
	Notes           string    `json:"notes"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`

	PlannedVolume     int64 `json:"planned_volume"`
	ConversionsNeeded int64 `json:"conversions_needed"`
	DaysToFinish      int64 `json:"days_to_finish"`

	Progress cpmDealProgress `json:"progress"`

	startDate time.Time `json:"-"`
}

type cpmCapacity struct {
	PlatformDaily      float64 `json:"platform_daily"`       // 14-day avg daily 'sent'
	TotalRequiredDaily float64 `json:"total_required_daily"` // sum over active deals
	UtilizationPct     float64 `json:"utilization_pct"`      // required / platform (0..1+)
	Headroom           float64 `json:"headroom"`             // platform - required
	Risk               string  `json:"risk"`                 // HIGH | MODERATE | LOW
	ActiveDeals        int     `json:"active_deals"`
}

type cpmDealInput struct {
	Name            *string  `json:"name"`
	OfferID         *string  `json:"offer_id"`
	EverflowOfferID *string  `json:"everflow_offer_id"`
	Budget          *float64 `json:"budget"`
	EcpmGoal        *float64 `json:"ecpm_goal"`
	EcpaGoal        *float64 `json:"ecpa_goal"`
	AvgCampaignSize *int     `json:"avg_campaign_size"`
	StartDate       *string  `json:"start_date"`
	Status          *string  `json:"status"`
	Notes           *string  `json:"notes"`
}

// ─── Core math ──────────────────────────────────────────────────────────────

// cpmPlanNumbers derives the deal's plan from its pricing terms.
// Verified against the operator's reference: budget $2,000, eCPM $0.70,
// eCPA $38, avg campaign 160,000 → 2,857,143 planned / 53 conversions / 18 days.
func cpmPlanNumbers(budget, ecpmGoal, ecpaGoal float64, avgCampaignSize int) (planned, convNeeded, days int64) {
	if budget > 0 && ecpmGoal > 0 {
		planned = int64(math.Ceil(budget / ecpmGoal * 1000))
	}
	if budget > 0 && ecpaGoal > 0 {
		convNeeded = int64(math.Ceil(budget / ecpaGoal))
	}
	if planned > 0 && avgCampaignSize > 0 {
		days = int64(math.Ceil(float64(planned) / float64(avgCampaignSize)))
	}
	return
}

// ─── Deal loading + live progress ───────────────────────────────────────────

const cpmDealSelect = `
	SELECT d.id, d.name,
	       COALESCE(d.offer_id::text, o2.id::text, '') AS effective_offer_id,
	       COALESCE(o.name, o2.name, '') AS offer_name,
	       COALESCE(d.everflow_offer_id, ''),
	       d.budget, d.ecpm_goal, COALESCE(d.ecpa_goal, 0),
	       d.avg_campaign_size, d.start_date, d.status, COALESCE(d.notes, ''),
	       d.created_at, d.updated_at,
	       COALESCE(o.payout, o2.payout, 0) AS payout
	FROM mailing_cpm_deals d
	LEFT JOIN mailing_offers o  ON o.id = d.offer_id
	LEFT JOIN mailing_offers o2 ON d.offer_id IS NULL
	      AND COALESCE(d.everflow_offer_id,'') <> ''
	      AND o2.everflow_offer_id = d.everflow_offer_id
	      AND o2.organization_id = d.organization_id
	WHERE d.organization_id = $1`

func scanCpmDeal(rows interface{ Scan(...interface{}) error }) (cpmDeal, float64, error) {
	var d cpmDeal
	var payout float64
	var start time.Time
	err := rows.Scan(&d.ID, &d.Name, &d.OfferID, &d.OfferName, &d.EverflowOfferID,
		&d.Budget, &d.EcpmGoal, &d.EcpaGoal, &d.AvgCampaignSize, &start,
		&d.Status, &d.Notes, &d.CreatedAt, &d.UpdatedAt, &payout)
	if err != nil {
		return d, 0, err
	}
	d.startDate = start
	d.StartDate = start.Format("2006-01-02")
	d.PlannedVolume, d.ConversionsNeeded, d.DaysToFinish = cpmPlanNumbers(d.Budget, d.EcpmGoal, d.EcpaGoal, d.AvgCampaignSize)
	return d, payout, nil
}

func (h *CpmPlannerHandlers) loadDeals(orgID, dealID string) ([]cpmDeal, error) {
	q := cpmDealSelect
	args := []interface{}{orgID}
	if dealID != "" {
		q += " AND d.id = $2"
		args = append(args, dealID)
	}
	q += " ORDER BY d.created_at DESC"
	rows, err := h.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deals := []cpmDeal{}
	for rows.Next() {
		d, payout, err := scanCpmDeal(rows)
		if err != nil {
			return nil, err
		}
		d.Progress = h.loadProgress(orgID, &d, payout)
		deals = append(deals, d)
	}
	return deals, rows.Err()
}

// loadProgress maps live platform delivery + conversion ground truth onto a
// deal. Best-effort: a failed sub-query logs and leaves zeros rather than
// failing the whole list.
func (h *CpmPlannerHandlers) loadProgress(orgID string, d *cpmDeal, payout float64) cpmDealProgress {
	p := cpmDealProgress{Payout: payout}

	if d.OfferID != "" {
		// Delivery aggregates from tracking events (counter columns are stale).
		evQ := `
			SELECT
				COUNT(*) FILTER (WHERE event_type = 'sent'),
				COUNT(*) FILTER (WHERE event_type = 'delivered'),
				COUNT(*) FILTER (WHERE event_type = 'opened'),
				COUNT(*) FILTER (WHERE event_type = 'clicked'),
				COUNT(*) FILTER (WHERE event_type = 'bounced' AND ` + HardBounceSQL("") + `),
				COUNT(*) FILTER (WHERE event_type = 'bounced' AND NOT (` + HardBounceSQL("") + `))
			FROM mailing_tracking_events
			WHERE organization_id = $1
			  AND event_at >= $2
			  AND campaign_id IN (
				SELECT id FROM mailing_campaigns
				WHERE organization_id = $1 AND offer_id = $3 AND created_at >= $2
			  )`
		if err := h.db.QueryRow(evQ, orgID, d.startDate, d.OfferID).Scan(
			&p.Sent, &p.Delivered, &p.Opened, &p.Clicked, &p.HardBounces, &p.SoftBounces); err != nil {
			log.Printf("[CpmPlanner] progress events for deal %s: %v", d.ID, err)
		}

		// Conversion ground truth: everflow postbacks → offer suppressions.
		convQ := `
			SELECT COUNT(*) FROM mailing_offer_suppressions
			WHERE organization_id = $1 AND offer_id = $2
			  AND reason = 'converted' AND suppressed_at >= $3`
		if err := h.db.QueryRow(convQ, orgID, d.OfferID, d.startDate).Scan(&p.Conversions); err != nil {
			log.Printf("[CpmPlanner] progress conversions for deal %s: %v", d.ID, err)
		}
	}

	if d.PlannedVolume > 0 {
		p.PctDelivered = float64(p.Sent) / float64(d.PlannedVolume)
	}
	if payout > 0 {
		p.Revenue = float64(p.Conversions) * payout
	} else if d.EcpaGoal > 0 {
		p.Revenue = float64(p.Conversions) * d.EcpaGoal // eCPA-goal-based estimate
	}
	if p.Sent > 0 {
		p.ActualEcpm = p.Revenue / float64(p.Sent) * 1000
	}
	if p.Conversions > 0 {
		p.ActualEcpa = (d.Budget * p.PctDelivered) / float64(p.Conversions)
	}

	p.DaysElapsed = int64(time.Since(d.startDate).Hours() / 24)
	if p.DaysElapsed < 0 {
		p.DaysElapsed = 0
	}
	if d.DaysToFinish > 0 {
		p.RequiredDaily = float64(d.PlannedVolume) / float64(d.DaysToFinish)
	}
	elapsed := p.DaysElapsed
	if elapsed < 1 {
		elapsed = 1
	}
	p.ActualDaily = float64(p.Sent) / float64(elapsed)
	p.OnPace = p.ActualDaily >= p.RequiredDaily
	return p
}

// ─── Capacity ───────────────────────────────────────────────────────────────

func (h *CpmPlannerHandlers) loadCapacity(orgID string) cpmCapacity {
	c := cpmCapacity{Risk: "LOW"}

	// 14-day average daily sends — from the daily domain-agent scorecard
	// rollup, NOT raw tracking events (a 14-day event scan per poll was a
	// prod hazard — QA finding C4, 2026-06-12).
	trendQ := `
		SELECT COALESCE(AVG(cnt), 0) FROM (
			SELECT day, SUM(sends) AS cnt
			FROM mailing_domain_agent_scorecard
			WHERE organization_id = $1 AND day >= CURRENT_DATE - 14 AND day < CURRENT_DATE
			GROUP BY 1
		) t`
	if err := h.db.QueryRow(trendQ, orgID).Scan(&c.PlatformDaily); err != nil {
		log.Printf("[CpmPlanner] capacity trend: %v", err)
	}

	// Required daily across active deals.
	rows, err := h.db.Query(`
		SELECT budget, ecpm_goal, COALESCE(ecpa_goal, 0), avg_campaign_size
		FROM mailing_cpm_deals
		WHERE organization_id = $1 AND status = 'active'`, orgID)
	if err != nil {
		log.Printf("[CpmPlanner] capacity deals: %v", err)
		return c
	}
	defer rows.Close()
	for rows.Next() {
		var budget, ecpm, ecpa float64
		var avgSize int
		if err := rows.Scan(&budget, &ecpm, &ecpa, &avgSize); err != nil {
			continue
		}
		planned, _, days := cpmPlanNumbers(budget, ecpm, ecpa, avgSize)
		if days > 0 {
			c.TotalRequiredDaily += float64(planned) / float64(days)
		}
		c.ActiveDeals++
	}

	c.Headroom = c.PlatformDaily - c.TotalRequiredDaily
	if c.PlatformDaily > 0 {
		c.UtilizationPct = c.TotalRequiredDaily / c.PlatformDaily
	} else if c.TotalRequiredDaily > 0 {
		c.UtilizationPct = 1 // demand with zero observed trend = fully at risk
	}
	switch {
	case c.UtilizationPct > 0.60:
		c.Risk = "HIGH"
	case c.UtilizationPct > 0.35:
		c.Risk = "MODERATE"
	}
	return c
}

// ─── Handlers ───────────────────────────────────────────────────────────────

// HandleListDeals GET /cpm-planner/deals
func (h *CpmPlannerHandlers) HandleListDeals(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	deals, err := h.loadDeals(orgID, "")
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("list deals: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"deals": deals, "total": len(deals)})
}

// HandleCreateDeal POST /cpm-planner/deals
func (h *CpmPlannerHandlers) HandleCreateDeal(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	var in cpmDealInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}
	if in.Budget == nil || *in.Budget <= 0 {
		respondError(w, http.StatusBadRequest, "budget must be > 0")
		return
	}
	if in.EcpmGoal == nil || *in.EcpmGoal <= 0 {
		respondError(w, http.StatusBadRequest, "ecpm_goal must be > 0")
		return
	}
	var offerID interface{}
	if in.OfferID != nil && strings.TrimSpace(*in.OfferID) != "" {
		offerID = strings.TrimSpace(*in.OfferID)
	}
	var ecpaGoal interface{}
	if in.EcpaGoal != nil && *in.EcpaGoal > 0 {
		ecpaGoal = *in.EcpaGoal
	}
	everflowID := ""
	if in.EverflowOfferID != nil {
		everflowID = strings.TrimSpace(*in.EverflowOfferID)
	}
	avgSize := 160000
	if in.AvgCampaignSize != nil && *in.AvgCampaignSize > 0 {
		avgSize = *in.AvgCampaignSize
	}
	startDate := time.Now().Format("2006-01-02")
	if in.StartDate != nil && *in.StartDate != "" {
		if _, err := time.Parse("2006-01-02", *in.StartDate); err != nil {
			respondError(w, http.StatusBadRequest, "start_date must be YYYY-MM-DD")
			return
		}
		startDate = *in.StartDate
	}
	notes := ""
	if in.Notes != nil {
		notes = *in.Notes
	}

	var id string
	err := h.db.QueryRow(`
		INSERT INTO mailing_cpm_deals
			(organization_id, name, offer_id, everflow_offer_id, budget, ecpm_goal,
			 ecpa_goal, avg_campaign_size, start_date, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id`,
		orgID, strings.TrimSpace(*in.Name), offerID, everflowID, *in.Budget, *in.EcpmGoal,
		ecpaGoal, avgSize, startDate, notes).Scan(&id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("create deal: %v", err))
		return
	}
	deals, err := h.loadDeals(orgID, id)
	if err != nil || len(deals) == 0 {
		respondJSON(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	respondJSON(w, http.StatusCreated, deals[0])
}

// HandleUpdateDeal PUT /cpm-planner/deals/{id}
func (h *CpmPlannerHandlers) HandleUpdateDeal(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	id := chi.URLParam(r, "id")
	var in cpmDealInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	sets := []string{}
	args := []interface{}{}
	add := func(col string, val interface{}) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			respondError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		add("name", strings.TrimSpace(*in.Name))
	}
	if in.OfferID != nil {
		if strings.TrimSpace(*in.OfferID) == "" {
			add("offer_id", nil)
		} else {
			add("offer_id", strings.TrimSpace(*in.OfferID))
		}
	}
	if in.EverflowOfferID != nil {
		add("everflow_offer_id", strings.TrimSpace(*in.EverflowOfferID))
	}
	if in.Budget != nil {
		if *in.Budget <= 0 {
			respondError(w, http.StatusBadRequest, "budget must be > 0")
			return
		}
		add("budget", *in.Budget)
	}
	if in.EcpmGoal != nil {
		if *in.EcpmGoal <= 0 {
			respondError(w, http.StatusBadRequest, "ecpm_goal must be > 0")
			return
		}
		add("ecpm_goal", *in.EcpmGoal)
	}
	if in.EcpaGoal != nil {
		if *in.EcpaGoal > 0 {
			add("ecpa_goal", *in.EcpaGoal)
		} else {
			add("ecpa_goal", nil)
		}
	}
	if in.AvgCampaignSize != nil {
		if *in.AvgCampaignSize <= 0 {
			respondError(w, http.StatusBadRequest, "avg_campaign_size must be > 0")
			return
		}
		add("avg_campaign_size", *in.AvgCampaignSize)
	}
	if in.StartDate != nil {
		if _, err := time.Parse("2006-01-02", *in.StartDate); err != nil {
			respondError(w, http.StatusBadRequest, "start_date must be YYYY-MM-DD")
			return
		}
		add("start_date", *in.StartDate)
	}
	if in.Status != nil {
		s := strings.ToLower(strings.TrimSpace(*in.Status))
		if s != "active" && s != "paused" && s != "completed" {
			respondError(w, http.StatusBadRequest, "status must be active|paused|completed")
			return
		}
		add("status", s)
	}
	if in.Notes != nil {
		add("notes", *in.Notes)
	}
	if len(sets) == 0 {
		respondError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	args = append(args, id, orgID)
	q := fmt.Sprintf(`UPDATE mailing_cpm_deals SET %s, updated_at = NOW() WHERE id = $%d AND organization_id = $%d`,
		strings.Join(sets, ", "), len(args)-1, len(args))
	res, err := h.db.Exec(q, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("update deal: %v", err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	deals, err := h.loadDeals(orgID, id)
	if err != nil || len(deals) == 0 {
		respondJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
		return
	}
	respondJSON(w, http.StatusOK, deals[0])
}

// HandleDeleteDeal DELETE /cpm-planner/deals/{id}
func (h *CpmPlannerHandlers) HandleDeleteDeal(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	id := chi.URLParam(r, "id")
	res, err := h.db.Exec(`DELETE FROM mailing_cpm_deals WHERE id = $1 AND organization_id = $2`, id, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("delete deal: %v", err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
}

// HandleDealInsights GET /cpm-planner/deals/{id}/insights
func (h *CpmPlannerHandlers) HandleDealInsights(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	id := chi.URLParam(r, "id")
	deals, err := h.loadDeals(orgID, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deal: %v", err))
		return
	}
	if len(deals) == 0 {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	deal := deals[0]
	capacity := h.loadCapacity(orgID)
	daily := h.loadDailySeries(orgID, &deal)
	topDomains := h.loadTopConvertingDomains(orgID, &deal)
	recs := buildCpmRecommendations(&deal, capacity, topDomains)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deal":            deal,
		"capacity":        capacity,
		"daily_series":    daily,
		"top_domains":     topDomains,
		"recommendations": recs,
	})
}

// HandleCapacity GET /cpm-planner/capacity
func (h *CpmPlannerHandlers) HandleCapacity(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.loadCapacity(getOrgID(r)))
}

// HandleOffersLite GET /cpm-planner/offers-lite — minimal offers list for the deal form.
func (h *CpmPlannerHandlers) HandleOffersLite(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	rows, err := h.db.Query(`
		SELECT id, name, COALESCE(everflow_offer_id, ''), COALESCE(payout, 0)
		FROM mailing_offers
		WHERE organization_id = $1
		ORDER BY name`, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("list offers: %v", err))
		return
	}
	defer rows.Close()
	type offerLite struct {
		ID              string  `json:"id"`
		Name            string  `json:"name"`
		EverflowOfferID string  `json:"everflow_offer_id"`
		Payout          float64 `json:"payout"`
	}
	offers := []offerLite{}
	for rows.Next() {
		var o offerLite
		if err := rows.Scan(&o.ID, &o.Name, &o.EverflowOfferID, &o.Payout); err != nil {
			continue
		}
		offers = append(offers, o)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"offers": offers})
}

// ─── Insights helpers ───────────────────────────────────────────────────────

type cpmDailyPoint struct {
	Date string `json:"date"`
	Sent int64  `json:"sent"`
}

func (h *CpmPlannerHandlers) loadDailySeries(orgID string, d *cpmDeal) []cpmDailyPoint {
	series := []cpmDailyPoint{}
	if d.OfferID == "" {
		return series
	}
	rows, err := h.db.Query(`
		SELECT to_char(date_trunc('day', event_at), 'YYYY-MM-DD') AS day, COUNT(*)
		FROM mailing_tracking_events
		WHERE organization_id = $1 AND event_type = 'sent' AND event_at >= $2
		  AND campaign_id IN (
			SELECT id FROM mailing_campaigns
			WHERE organization_id = $1 AND offer_id = $3 AND created_at >= $2
		  )
		GROUP BY 1 ORDER BY 1`, orgID, d.startDate, d.OfferID)
	if err != nil {
		log.Printf("[CpmPlanner] daily series for deal %s: %v", d.ID, err)
		return series
	}
	defer rows.Close()
	for rows.Next() {
		var p cpmDailyPoint
		if err := rows.Scan(&p.Date, &p.Sent); err != nil {
			continue
		}
		series = append(series, p)
	}
	return series
}

type cpmDomainConversions struct {
	Domain      string `json:"domain"`
	Conversions int64  `json:"conversions"`
}

func (h *CpmPlannerHandlers) loadTopConvertingDomains(orgID string, d *cpmDeal) []cpmDomainConversions {
	out := []cpmDomainConversions{}
	if d.OfferID == "" {
		return out
	}
	rows, err := h.db.Query(`
		SELECT LOWER(SPLIT_PART(sub.email, '@', 2)) AS dom, COUNT(*)
		FROM mailing_offer_suppressions s
		JOIN mailing_subscribers sub ON sub.id = s.subscriber_id
		WHERE s.organization_id = $1 AND s.offer_id = $2
		  AND s.reason = 'converted' AND s.suppressed_at >= $3
		GROUP BY 1 ORDER BY 2 DESC LIMIT 5`, orgID, d.OfferID, d.startDate)
	if err != nil {
		log.Printf("[CpmPlanner] top domains for deal %s: %v", d.ID, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var dc cpmDomainConversions
		if err := rows.Scan(&dc.Domain, &dc.Conversions); err != nil {
			continue
		}
		out = append(out, dc)
	}
	return out
}

func buildCpmRecommendations(d *cpmDeal, cap cpmCapacity, topDomains []cpmDomainConversions) []string {
	recs := []string{}
	p := d.Progress

	if d.OfferID == "" {
		recs = append(recs, "Deal is not mapped to a platform offer — set offer_id or everflow_offer_id so live delivery and conversions can be tracked against the plan.")
	}

	// 1. Pace.
	if d.Status == "active" && p.RequiredDaily > 0 {
		if !p.OnPace {
			gap := p.RequiredDaily - p.ActualDaily
			extraSlots := int64(math.Ceil(gap / float64(d.AvgCampaignSize)))
			if extraSlots < 1 {
				extraSlots = 1
			}
			remaining := float64(d.PlannedVolume) - float64(p.Sent)
			if remaining < 0 {
				remaining = 0
			}
			extendedDays := d.DaysToFinish
			if p.ActualDaily > 0 {
				extendedDays = p.DaysElapsed + int64(math.Ceil(remaining/p.ActualDaily))
			}
			recs = append(recs, fmt.Sprintf(
				"Behind pace: sending %s/day vs %s/day needed — add %d more campaign slot(s) of %s per day, or extend the window to %d days at the current rate.",
				fmtCpmInt(int64(p.ActualDaily)), fmtCpmInt(int64(p.RequiredDaily)),
				extraSlots, fmtCpmInt(int64(d.AvgCampaignSize)), extendedDays))
		} else if p.Sent > 0 {
			recs = append(recs, fmt.Sprintf(
				"On pace: %s/day actual vs %s/day required (%.1f%% of planned volume delivered).",
				fmtCpmInt(int64(p.ActualDaily)), fmtCpmInt(int64(p.RequiredDaily)), p.PctDelivered*100))
		}
	}

	// 2. eCPM vs goal.
	if p.Sent > 0 && p.ActualEcpm < d.EcpmGoal {
		crPerMillion := float64(p.Conversions) / float64(p.Sent) * 1_000_000
		remainingVolume := d.PlannedVolume - p.Sent
		if remainingVolume < 0 {
			remainingVolume = 0
		}
		neededConv := d.ConversionsNeeded - p.Conversions
		if neededConv < 0 {
			neededConv = 0
		}
		requiredCr := 0.0
		if remainingVolume > 0 {
			requiredCr = float64(neededConv) / float64(remainingVolume) * 1_000_000
		}
		msg := fmt.Sprintf(
			"Actual eCPM $%.2f vs $%.2f goal — converting %.1f per million sent; you need %d more conversions over the remaining %s sends (%.1f per million required). Consider concentrating volume on the ISPs/brands where this offer converts.",
			p.ActualEcpm, d.EcpmGoal, crPerMillion, neededConv, fmtCpmInt(remainingVolume), requiredCr)
		if len(topDomains) > 0 {
			parts := make([]string, 0, len(topDomains))
			for _, td := range topDomains {
				parts = append(parts, fmt.Sprintf("%s (%d)", td.Domain, td.Conversions))
			}
			msg += " Top converting domains: " + strings.Join(parts, ", ") + "."
		}
		recs = append(recs, msg)
	} else if p.Sent > 0 && p.ActualEcpm >= d.EcpmGoal {
		recs = append(recs, fmt.Sprintf(
			"Earning above plan: actual eCPM $%.2f vs $%.2f goal — current mix is working, hold the volume allocation.",
			p.ActualEcpm, d.EcpmGoal))
	}

	// 3. Capacity risk.
	if cap.PlatformDaily > 0 {
		recs = append(recs, fmt.Sprintf(
			"Platform is sending %s emails/day (14-day avg). Active CPM deals require %s/day — %.0f%% of capacity. Risk: %s.",
			fmtCpmInt(int64(cap.PlatformDaily)), fmtCpmInt(int64(cap.TotalRequiredDaily)),
			cap.UtilizationPct*100, cap.Risk))
	}

	// 4. eCPA check.
	if d.EcpaGoal > 0 {
		if p.Conversions == 0 && p.PctDelivered > 0.20 {
			recs = append(recs, fmt.Sprintf(
				"Zero conversions after %.0f%% of planned volume (%s sent) — verify the everflow postback wiring and the creative's money links before sending more budgeted volume.",
				p.PctDelivered*100, fmtCpmInt(p.Sent)))
		} else if p.Conversions > 0 && p.ActualEcpa > d.EcpaGoal {
			recs = append(recs, fmt.Sprintf(
				"Actual eCPA $%.2f is above the $%.2f goal — conversions are costing more volume than planned; tighten the audience toward proven converters.",
				p.ActualEcpa, d.EcpaGoal))
		}
	}

	return recs
}

func fmtCpmInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
