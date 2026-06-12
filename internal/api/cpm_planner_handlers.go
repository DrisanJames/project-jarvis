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
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
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
	Sent        int64 `json:"sent"`
	Delivered   int64 `json:"delivered"`
	Opened      int64 `json:"opened"`
	Clicked     int64 `json:"clicked"`
	HardBounces int64 `json:"hard_bounces"`
	SoftBounces int64 `json:"soft_bounces"`
	// Conversions is the TOTAL (tracked + manual) — the field name predates
	// manual uploads, kept as the total for backward compat. The split is
	// exposed alongside it.
	Conversions        int64   `json:"conversions"`
	ConversionsTracked int64   `json:"conversions_tracked"` // everflow postbacks (countOfferConversions)
	ConversionsManual  int64   `json:"conversions_manual"`  // operator CSV uploads / quick-adds
	ManualRevenue      float64 `json:"manual_revenue"`      // raw revenue reported on manual rows
	Payout             float64 `json:"payout"`
	PctDelivered       float64 `json:"pct_volume_delivered"` // sent / planned_volume (0..1+)
	Revenue            float64 `json:"revenue_earned"`
	ActualEcpm         float64 `json:"actual_ecpm"`
	ActualEcpa         float64 `json:"actual_ecpa"` // proportional budget spent / conversions; 0 when no conversions
	DaysElapsed        int64   `json:"days_elapsed"`
	RequiredDaily      float64 `json:"required_daily"`
	ActualDaily        float64 `json:"actual_daily"`
	OnPace             bool    `json:"on_pace"`
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
		// countOfferConversions (offer_center_handlers.go) is THE shared
		// attribution query — same implementation the Offers tab uses, so
		// the two surfaces can never disagree.
		if n, err := countOfferConversions(context.Background(), h.db, orgID, d.OfferID, d.startDate); err != nil {
			log.Printf("[CpmPlanner] progress conversions for deal %s: %v", d.ID, err)
		} else {
			p.ConversionsTracked = n
		}
	}

	// Revenue basis per conversion: offer payout, else eCPA goal as estimate
	// (same precedence the pre-manual revenue logic used).
	basis := payout
	if basis == 0 && d.EcpaGoal > 0 {
		basis = d.EcpaGoal
	}

	// Manual conversions (operator CSV uploads / quick-adds) — counted for
	// EVERY deal, mapped or not: offers without postback wiring are exactly
	// the manual-upload case. Window matches tracked: since deal start.
	// manualRevEffective values each manual row at its reported revenue when
	// present, else at the basis estimate — so a CSV with real revenue is
	// authoritative and a count-only quick-add still earns the payout.
	var manualRevEffective float64
	if err := h.db.QueryRow(`
		SELECT COALESCE(SUM(count), 0),
		       COALESCE(SUM(revenue), 0),
		       COALESCE(SUM(CASE WHEN revenue > 0 THEN revenue ELSE count * $4 END), 0)
		FROM mailing_cpm_manual_conversions
		WHERE organization_id = $1 AND deal_id = $2 AND converted_at >= $3`,
		orgID, d.ID, d.startDate, basis).Scan(&p.ConversionsManual, &p.ManualRevenue, &manualRevEffective); err != nil {
		log.Printf("[CpmPlanner] progress manual conversions for deal %s: %v", d.ID, err)
	}
	p.Conversions = p.ConversionsTracked + p.ConversionsManual

	if d.PlannedVolume > 0 {
		p.PctDelivered = float64(p.Sent) / float64(d.PlannedVolume)
	}
	p.Revenue = float64(p.ConversionsTracked)*basis + manualRevEffective
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

// HandleDealOfferPerformance GET /cpm-planner/deals/{id}/offer-performance?days=30
//
// Embeds the offer performance panel in the deal detail (doc §6.1/§6.3):
// resolves the deal's effective offer (offer_id, or everflow_offer_id
// mapping) and returns the SAME aggregation the Offers tab Performance view
// uses — loadOfferStats in offer_center_handlers.go (single implementation,
// two endpoints) — including human opens/clicks, hard/soft bounce split,
// conversions (shared attribution query), suppression total + DNM list size,
// daily sent/conversion series, recent campaigns, and the 8-week
// suppressed-count trend with audience size for ceiling awareness.
func (h *CpmPlannerHandlers) HandleDealOfferPerformance(w http.ResponseWriter, r *http.Request) {
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

	if deal.OfferID == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"deal_id":     deal.ID,
			"offer_id":    "",
			"offer_name":  "",
			"performance": nil,
			"note":        "deal is not mapped to a platform offer — set offer_id or everflow_offer_id to see live offer performance",
		})
		return
	}

	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}

	perf, err := loadOfferStats(r.Context(), h.db, orgID, deal.OfferID, days)
	if err != nil {
		log.Printf("[CpmPlanner] offer performance for deal %s (offer %s): %v", deal.ID, deal.OfferID, err)
		respondError(w, http.StatusInternalServerError, "Failed to load offer performance")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deal_id":     deal.ID,
		"offer_id":    deal.OfferID,
		"offer_name":  deal.OfferName,
		"performance": perf,
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

// ─── Manual conversions ─────────────────────────────────────────────────────
//
// Most CPM offers have NO everflow postback wiring, so tracked conversions
// (mailing_offer_suppressions reason='converted') under-count reality. The
// operator gets conversion truth two ways: an Everflow conversion-report CSV
// export, or just "N conversions happened on date D". Both land in
// mailing_cpm_manual_conversions and blend into deal pacing as
// conversions_manual (see loadProgress).

type cpmManualConvEntry struct {
	ID           string    `json:"id"`
	ConvertedAt  time.Time `json:"converted_at"`
	Count        int       `json:"count"`
	Revenue      float64   `json:"revenue"`
	Sub1         string    `json:"sub1"`
	Sub2         string    `json:"sub2"`
	ConversionID string    `json:"conversion_id"`
	Source       string    `json:"source"` // 'csv' | 'manual'
	Note         string    `json:"note"`
	CreatedAt    time.Time `json:"created_at"`
}

type cpmManualConvInput struct {
	Entries []struct {
		ConvertedAt string  `json:"converted_at"` // YYYY-MM-DD or RFC3339
		Count       int     `json:"count"`        // default 1
		Revenue     float64 `json:"revenue"`
		Note        string  `json:"note"`
	} `json:"entries"`
	CSV string `json:"csv"` // raw Everflow conversion-export text
}

// cpmManualConvRow is one row ready for insert (from either input shape).
type cpmManualConvRow struct {
	convertedAt  time.Time
	count        int
	revenue      float64
	sub1         string
	sub2         string
	conversionID string
	source       string
	note         string
}

// cpmConvDateLayouts: Everflow exports vary by report/timezone setting;
// tolerate the common shapes plus plain dates for quick-adds.
var cpmConvDateLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"01/02/2006 15:04:05",
	"01/02/2006 15:04",
	"01/02/2006",
	"1/2/2006 15:04",
	"1/2/2006",
}

func parseCpmConvDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range cpmConvDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	// Raw unix-seconds timestamps (some Everflow API-driven exports).
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 1_000_000_000 && n < 10_000_000_000 {
		return time.Unix(n, 0).UTC(), true
	}
	return time.Time{}, false
}

func parseCpmRevenue(s string) float64 {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "$", ""), ",", ""))
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// parseEverflowConversionsCSV parses a raw Everflow conversion-report export.
// Header-driven column lookup (extra/reordered columns tolerated): only
// 'date' is required; 'revenue', 'sub1', 'sub2', 'conversion_id' are used
// when present. One conversion per row (count=1). Ragged rows, lazy quotes
// and a UTF-8 BOM are tolerated; unparseable rows are counted as parse
// errors (first few reported back as samples) rather than failing the batch.
func parseEverflowConversionsCSV(raw string) (rows []cpmManualConvRow, parseErrors int, samples []string) {
	raw = strings.TrimPrefix(raw, "\ufeff")
	r := csv.NewReader(strings.NewReader(raw))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	header, err := r.Read()
	if err != nil {
		return nil, 1, []string{"unreadable CSV: " + err.Error()}
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(name))] = i
	}
	if _, ok := col["date"]; !ok {
		return nil, 1, []string{"CSV has no 'date' column — expected an Everflow conversions export"}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	line := 1
	for {
		rec, err := r.Read()
		line++
		if err == io.EOF {
			break
		}
		if err != nil {
			parseErrors++
			if len(samples) < 5 {
				samples = append(samples, fmt.Sprintf("line %d: %v", line, err))
			}
			continue
		}
		blank := true
		for _, f := range rec {
			if strings.TrimSpace(f) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		when, ok := parseCpmConvDate(get(rec, "date"))
		if !ok {
			parseErrors++
			if len(samples) < 5 {
				samples = append(samples, fmt.Sprintf("line %d: unparseable date %q", line, get(rec, "date")))
			}
			continue
		}
		rows = append(rows, cpmManualConvRow{
			convertedAt:  when,
			count:        1,
			revenue:      parseCpmRevenue(get(rec, "revenue")),
			sub1:         get(rec, "sub1"),
			sub2:         get(rec, "sub2"),
			conversionID: get(rec, "conversion_id"),
			source:       "csv",
		})
	}
	return rows, parseErrors, samples
}

// dealExists scopes the conversions sub-resource to an org-owned deal.
func (h *CpmPlannerHandlers) dealExists(orgID, dealID string) (bool, error) {
	var one int
	err := h.db.QueryRow(
		`SELECT 1 FROM mailing_cpm_deals WHERE id = $1 AND organization_id = $2`,
		dealID, orgID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// HandleAddDealConversions POST /cpm-planner/deals/{id}/conversions
//
// Body is EITHER {entries:[{converted_at,count,revenue?,note?}]} (quick-add)
// OR {csv:"<raw everflow export>"}. CSV rows dedupe on (deal_id,
// conversion_id) via the partial unique index — re-uploading the same export
// is safe and reports duplicates instead of double-counting.
func (h *CpmPlannerHandlers) HandleAddDealConversions(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	ok, err := h.dealExists(orgID, dealID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deal: %v", err))
		return
	}
	if !ok {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}

	var in cpmManualConvInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var rows []cpmManualConvRow
	parseErrors := 0
	var samples []string
	switch {
	case strings.TrimSpace(in.CSV) != "":
		rows, parseErrors, samples = parseEverflowConversionsCSV(in.CSV)
	case len(in.Entries) > 0:
		for i, e := range in.Entries {
			when, ok := parseCpmConvDate(e.ConvertedAt)
			if !ok {
				respondError(w, http.StatusBadRequest,
					fmt.Sprintf("entries[%d].converted_at: use YYYY-MM-DD or RFC3339", i))
				return
			}
			if e.Revenue < 0 {
				respondError(w, http.StatusBadRequest, fmt.Sprintf("entries[%d].revenue must be >= 0", i))
				return
			}
			count := e.Count
			if count <= 0 {
				count = 1
			}
			rows = append(rows, cpmManualConvRow{
				convertedAt: when,
				count:       count,
				revenue:     e.Revenue,
				source:      "manual",
				note:        strings.TrimSpace(e.Note),
			})
		}
	default:
		respondError(w, http.StatusBadRequest, "body must include entries[] or csv")
		return
	}
	if len(rows) > 50000 {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("too many rows (%d) — cap is 50,000 per upload", len(rows)))
		return
	}

	inserted, duplicates := 0, 0
	if len(rows) > 0 {
		tx, err := h.db.Begin()
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("begin: %v", err))
			return
		}
		defer tx.Rollback() //nolint:errcheck // no-op after commit
		stmt, err := tx.Prepare(`
			INSERT INTO mailing_cpm_manual_conversions
				(organization_id, deal_id, converted_at, count, revenue,
				 sub1, sub2, conversion_id, source, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (deal_id, conversion_id) WHERE conversion_id <> '' DO NOTHING`)
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("prepare: %v", err))
			return
		}
		defer stmt.Close()
		for _, row := range rows {
			res, err := stmt.Exec(orgID, dealID, row.convertedAt, row.count, row.revenue,
				row.sub1, row.sub2, row.conversionID, row.source, row.note)
			if err != nil {
				respondError(w, http.StatusInternalServerError, fmt.Sprintf("insert conversion: %v", err))
				return
			}
			if n, _ := res.RowsAffected(); n == 0 {
				duplicates++
			} else {
				inserted++
			}
		}
		if err := tx.Commit(); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("commit: %v", err))
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"inserted":     inserted,
		"duplicates":   duplicates,
		"parse_errors": parseErrors,
		"errors":       samples,
	})
}

// HandleListDealConversions GET /cpm-planner/deals/{id}/conversions?days=N
// Newest first, capped at 500 rows; totals are computed over the full
// filtered set (not the capped page).
func (h *CpmPlannerHandlers) HandleListDealConversions(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	ok, err := h.dealExists(orgID, dealID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deal: %v", err))
		return
	}
	if !ok {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}

	where := `organization_id = $1 AND deal_id = $2`
	args := []interface{}{orgID, dealID}
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 3650 {
		args = append(args, time.Now().AddDate(0, 0, -d))
		where += fmt.Sprintf(" AND converted_at >= $%d", len(args))
	}

	var manualTotal int64
	var manualRevenue float64
	if err := h.db.QueryRow(
		`SELECT COALESCE(SUM(count), 0), COALESCE(SUM(revenue), 0)
		 FROM mailing_cpm_manual_conversions WHERE `+where, args...).Scan(&manualTotal, &manualRevenue); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("conversion totals: %v", err))
		return
	}

	rows, err := h.db.Query(
		`SELECT id, converted_at, count, revenue, sub1, sub2, conversion_id, source, note, created_at
		 FROM mailing_cpm_manual_conversions WHERE `+where+`
		 ORDER BY converted_at DESC, created_at DESC LIMIT 500`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("list conversions: %v", err))
		return
	}
	defer rows.Close()
	entries := []cpmManualConvEntry{}
	for rows.Next() {
		var e cpmManualConvEntry
		if err := rows.Scan(&e.ID, &e.ConvertedAt, &e.Count, &e.Revenue,
			&e.Sub1, &e.Sub2, &e.ConversionID, &e.Source, &e.Note, &e.CreatedAt); err != nil {
			continue
		}
		entries = append(entries, e)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"totals": map[string]interface{}{
			"manual_total":   manualTotal,
			"manual_revenue": manualRevenue,
		},
	})
}

// HandleDeleteDealConversion DELETE /cpm-planner/deals/{id}/conversions/{convID}
func (h *CpmPlannerHandlers) HandleDeleteDealConversion(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	convID := chi.URLParam(r, "convID")
	res, err := h.db.Exec(
		`DELETE FROM mailing_cpm_manual_conversions
		 WHERE id = $1 AND deal_id = $2 AND organization_id = $3`,
		convID, dealID, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("delete conversion: %v", err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "conversion entry not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"id": convID, "status": "deleted"})
}

// ─── Insights helpers ───────────────────────────────────────────────────────

type cpmDailyPoint struct {
	Date        string `json:"date"`
	Sent        int64  `json:"sent"`
	Conversions int64  `json:"conversions"` // tracked (postback) + manual, by day
}

func (h *CpmPlannerHandlers) loadDailySeries(orgID string, d *cpmDeal) []cpmDailyPoint {
	byDay := map[string]*cpmDailyPoint{}
	point := func(day string) *cpmDailyPoint {
		if p, ok := byDay[day]; ok {
			return p
		}
		p := &cpmDailyPoint{Date: day}
		byDay[day] = p
		return p
	}

	if d.OfferID != "" {
		rows, err := h.db.Query(`
			SELECT to_char(date_trunc('day', event_at), 'YYYY-MM-DD') AS day, COUNT(*)
			FROM mailing_tracking_events
			WHERE organization_id = $1 AND event_type = 'sent' AND event_at >= $2
			  AND campaign_id IN (
				SELECT id FROM mailing_campaigns
				WHERE organization_id = $1 AND offer_id = $3 AND created_at >= $2
			  )
			GROUP BY 1`, orgID, d.startDate, d.OfferID)
		if err != nil {
			log.Printf("[CpmPlanner] daily series for deal %s: %v", d.ID, err)
		} else {
			func() {
				defer rows.Close()
				for rows.Next() {
					var day string
					var sent int64
					if err := rows.Scan(&day, &sent); err != nil {
						continue
					}
					point(day).Sent = sent
				}
			}()
		}

		// Tracked conversions by day (same source as countOfferConversions).
		convRows, err := h.db.Query(`
			SELECT to_char(date_trunc('day', suppressed_at), 'YYYY-MM-DD') AS day, COUNT(*)
			FROM mailing_offer_suppressions
			WHERE organization_id = $1 AND offer_id = $2
			  AND reason = 'converted' AND suppressed_at >= $3
			GROUP BY 1`, orgID, d.OfferID, d.startDate)
		if err != nil {
			log.Printf("[CpmPlanner] daily conversions for deal %s: %v", d.ID, err)
		} else {
			func() {
				defer convRows.Close()
				for convRows.Next() {
					var day string
					var n int64
					if err := convRows.Scan(&day, &n); err != nil {
						continue
					}
					point(day).Conversions += n
				}
			}()
		}
	}

	// Manual conversions by day — union'd into the same series (charts for
	// unmapped deals show manual conversions alone).
	manRows, err := h.db.Query(`
		SELECT to_char(date_trunc('day', converted_at), 'YYYY-MM-DD') AS day, SUM(count)
		FROM mailing_cpm_manual_conversions
		WHERE organization_id = $1 AND deal_id = $2 AND converted_at >= $3
		GROUP BY 1`, orgID, d.ID, d.startDate)
	if err != nil {
		log.Printf("[CpmPlanner] daily manual conversions for deal %s: %v", d.ID, err)
	} else {
		func() {
			defer manRows.Close()
			for manRows.Next() {
				var day string
				var n int64
				if err := manRows.Scan(&day, &n); err != nil {
					continue
				}
				point(day).Conversions += n
			}
		}()
	}

	series := make([]cpmDailyPoint, 0, len(byDay))
	for _, p := range byDay {
		series = append(series, *p)
	}
	sort.Slice(series, func(i, j int) bool { return series[i].Date < series[j].Date })
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
