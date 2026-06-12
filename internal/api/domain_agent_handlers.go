package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/domainagent"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/lib/pq"
)

// DomainAgentAPI exposes the Domain Agent surface: per-domain scorecard,
// deterministic briefing generation, and the plan lifecycle
// (draft → compiled → approved → deployed/failed). Plan approval deploys
// each compiled payload in-process via PMTACampaignService.DeployFromInput.
type DomainAgentAPI struct {
	db    *sql.DB
	pmta  *PMTACampaignService
	plans *domainagent.PlanStore
}

func NewDomainAgentAPI(db *sql.DB, pmta *PMTACampaignService) *DomainAgentAPI {
	return &DomainAgentAPI{db: db, pmta: pmta, plans: domainagent.NewPlanStore(db)}
}

// RegisterRoutes mounts the Domain Agent endpoints under /domain-agent.
func (a *DomainAgentAPI) RegisterRoutes(r chi.Router) {
	r.Route("/domain-agent", func(dr chi.Router) {
		dr.Get("/domains", a.HandleDomains)
		dr.Get("/scorecard", a.HandleScorecard)
		dr.Get("/upcoming", a.HandleUpcoming)
		dr.Post("/scorecard/refresh", a.HandleScorecardRefresh)
		dr.Post("/plans/generate", a.HandleGeneratePlan)
		dr.Get("/plans", a.HandlePlanLookup)
		dr.Get("/plans/{planID}", a.HandleGetPlan)
		dr.Patch("/plans/{planID}", a.HandlePatchPlan)
		dr.Post("/plans/{planID}/payloads", a.HandleAttachPayloads)
		dr.Post("/plans/{planID}/approve", a.HandleApprovePlan)
		dr.Get("/plans/{planID}/status", a.HandlePlanStatus)
	})
}

// planResponse is the canonical plan JSON shape returned by every plan endpoint.
func planResponse(p *domainagent.Plan) map[string]interface{} {
	rawOr := func(raw json.RawMessage, fallback string) json.RawMessage {
		if len(raw) == 0 {
			return json.RawMessage(fallback)
		}
		return raw
	}
	var approvedBy interface{}
	if p.ApprovedBy != "" {
		approvedBy = p.ApprovedBy
	}
	var approvedAt interface{}
	if p.ApprovedAt != nil {
		approvedAt = p.ApprovedAt
	}
	return map[string]interface{}{
		"id":             p.ID,
		"sending_domain": p.SendingDomain,
		"plan_date":      p.PlanDate.Format("2006-01-02"),
		"status":         p.Status,
		"briefing":       rawOr(p.Briefing, "{}"),
		"slots":          rawOr(p.Slots, "[]"),
		"payloads":       rawOr(p.Payloads, "[]"),
		"deploy_results": rawOr(p.DeployResults, "[]"),
		"approved_by":    approvedBy,
		"approved_at":    approvedAt,
	}
}

// respondPlanErr maps PlanStore sentinel errors to 404/409, else 500.
func respondPlanErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domainagent.ErrPlanNotFound):
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "plan not found"})
	case errors.Is(err, domainagent.ErrPlanNotEditable):
		respondJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// HandleDomains lists active sending domains with 7-day scorecard rollups,
// worst-ISP posture, and whether a plan exists for today.
func (a *DomainAgentAPI) HandleDomains(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	domRows, err := a.db.QueryContext(ctx, `
		SELECT DISTINCT sending_domain
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND status = 'active' AND COALESCE(sending_domain, '') <> ''
		ORDER BY sending_domain
	`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var domains []string
	for domRows.Next() {
		var d string
		if err := domRows.Scan(&d); err != nil {
			domRows.Close()
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		domains = append(domains, strings.ToLower(d))
	}
	domRows.Close()

	type ispAgg struct{ sends, humanOpens, machineOpens, bounces int }
	perDomainISP := map[string]map[string]*ispAgg{}
	aggRows, err := a.db.QueryContext(ctx, `
		SELECT sending_domain, isp, SUM(sends), SUM(human_opens), SUM(machine_opens),
		       SUM(hard_bounces + soft_bounces + other_bounces)
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1 AND day >= CURRENT_DATE - 6
		GROUP BY 1, 2
	`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for aggRows.Next() {
		var domain, ispName string
		agg := &ispAgg{}
		if err := aggRows.Scan(&domain, &ispName, &agg.sends, &agg.humanOpens, &agg.machineOpens, &agg.bounces); err != nil {
			aggRows.Close()
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if perDomainISP[domain] == nil {
			perDomainISP[domain] = map[string]*ispAgg{}
		}
		perDomainISP[domain][ispName] = agg
	}
	aggRows.Close()

	hasPlanToday := map[string]bool{}
	planRows, err := a.db.QueryContext(ctx, `
		SELECT sending_domain FROM mailing_domain_agent_plans
		WHERE organization_id = $1 AND plan_date = CURRENT_DATE
	`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for planRows.Next() {
		var d string
		if err := planRows.Scan(&d); err == nil {
			hasPlanToday[strings.ToLower(d)] = true
		}
	}
	planRows.Close()

	out := make([]map[string]interface{}, 0, len(domains))
	for _, domain := range domains {
		sends, humanOpens := 0, 0
		posture := domainagent.PostureUnknown
		if isps := perDomainISP[domain]; len(isps) > 0 {
			posture = domainagent.PostureHealthy
			for _, agg := range isps {
				sends += agg.sends
				humanOpens += agg.humanOpens
				p, _ := domainagent.ClassifyPosture(agg.sends, agg.humanOpens, agg.machineOpens, agg.bounces)
				posture = domainagent.WorsePosture(posture, p)
			}
		}
		openPct := 0.0
		if sends > 0 {
			openPct = 100 * float64(humanOpens) / float64(sends)
		}
		out = append(out, map[string]interface{}{
			"domain":            domain,
			"sends_7d":          sends,
			"human_open_pct_7d": openPct,
			"posture_worst":     posture,
			"has_plan_today":    hasPlanToday[domain],
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// HandleScorecard returns the raw scorecard rows for one domain, day asc.
func (a *DomainAgentAPI) HandleScorecard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	domain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
	if domain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "domain query parameter is required"})
		return
	}
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > 45 {
		days = 45
	}

	rows, err := a.db.QueryContext(ctx, `
		SELECT day, sending_domain, isp, sends, delivered,
		       human_opens, machine_opens, human_clicks, machine_clicks,
		       hard_bounces, soft_bounces, other_bounces, complaints, unsubscribes
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1 AND sending_domain = $2 AND day >= CURRENT_DATE - ($3 - 1)::int
		ORDER BY day ASC, isp ASC
	`, orgID, domain, days)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	out := []map[string]interface{}{}
	for rows.Next() {
		var day time.Time
		var sendingDomain, ispName string
		var sends, delivered, humanOpens, machineOpens, humanClicks, machineClicks int
		var hardBounces, softBounces, otherBounces, complaints, unsubscribes int
		if err := rows.Scan(&day, &sendingDomain, &ispName, &sends, &delivered,
			&humanOpens, &machineOpens, &humanClicks, &machineClicks,
			&hardBounces, &softBounces, &otherBounces, &complaints, &unsubscribes); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out = append(out, map[string]interface{}{
			"day":            day.Format("2006-01-02"),
			"sending_domain": sendingDomain,
			"isp":            ispName,
			"sends":          sends,
			"delivered":      delivered,
			"human_opens":    humanOpens,
			"machine_opens":  machineOpens,
			"human_clicks":   humanClicks,
			"machine_clicks": machineClicks,
			"hard_bounces":   hardBounces,
			"soft_bounces":   softBounces,
			"other_bounces":  otherBounces,
			"complaints":     complaints,
			"unsubscribes":   unsubscribes,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// HandleScorecardRefresh re-rolls the scorecard for the trailing N days.
// Small windows (≤3 days) run synchronously; larger ones run in the background.
func (a *DomainAgentAPI) HandleScorecardRefresh(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	var body struct {
		Days int `json:"days"`
	}
	// Empty body is fine — defaults to a 3-day synchronous refresh.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	days := body.Days
	if days <= 0 {
		days = 3
	}
	if days > 45 {
		days = 45
	}
	today := time.Now().UTC()
	from := today.AddDate(0, 0, -(days - 1))

	if days <= 3 {
		if err := domainagent.RollupRange(r.Context(), a.db, orgID, from, today); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := domainagent.RollupRange(ctx, a.db, orgID, from, today); err != nil {
			log.Printf("[DomainAgent] background scorecard refresh (org=%s days=%d) completed with errors: %v", orgID, days, err)
		}
	}()
	respondJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// HandleGeneratePlan creates (or refreshes the briefing of) the draft plan
// for (domain, date). Plans past draft are immutable here → 409.
func (a *DomainAgentAPI) HandleGeneratePlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	var body struct {
		Domain string `json:"domain"`
		Date   string `json:"date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	domain := strings.ToLower(strings.TrimSpace(body.Domain))
	if domain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "domain is required"})
		return
	}
	planDate := time.Now().UTC()
	if body.Date != "" {
		parsed, err := time.Parse("2006-01-02", body.Date)
		if err != nil {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "date must be YYYY-MM-DD"})
			return
		}
		planDate = parsed
	}

	briefing, err := domainagent.GenerateBriefing(ctx, a.db, orgID, domain, 7)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	briefingJSON, err := json.Marshal(briefing)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	existing, err := a.plans.GetByDomainDate(ctx, orgID, domain, planDate)
	switch {
	case errors.Is(err, domainagent.ErrPlanNotFound):
		plan, err := a.plans.CreateDraft(ctx, orgID, domain, planDate, briefingJSON)
		if err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		respondJSON(w, http.StatusOK, planResponse(plan))
	case err != nil:
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	case existing.Status == "draft":
		// Regenerate the briefing in place; operator-edited slots survive.
		if err := a.plans.UpdateBriefing(ctx, orgID, existing.ID, briefingJSON); err != nil {
			respondPlanErr(w, err)
			return
		}
		plan, err := a.plans.Get(ctx, orgID, existing.ID)
		if err != nil {
			respondPlanErr(w, err)
			return
		}
		respondJSON(w, http.StatusOK, planResponse(plan))
	default:
		respondJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("plan for %s on %s already exists with status %s", domain, existing.PlanDate.Format("2006-01-02"), existing.Status),
		})
	}
}

// HandlePlanLookup fetches a plan by (domain, date). Date defaults to today.
func (a *DomainAgentAPI) HandlePlanLookup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	domain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
	if domain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "domain query parameter is required"})
		return
	}
	planDate := time.Now().UTC()
	if v := r.URL.Query().Get("date"); v != "" {
		parsed, err := time.Parse("2006-01-02", v)
		if err != nil {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "date must be YYYY-MM-DD"})
			return
		}
		planDate = parsed
	}
	plan, err := a.plans.GetByDomainDate(ctx, orgID, domain, planDate)
	if err != nil {
		respondPlanErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, planResponse(plan))
}

// HandleGetPlan fetches a plan by id.
func (a *DomainAgentAPI) HandleGetPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := a.plans.Get(r.Context(), getOrgID(r), chi.URLParam(r, "planID"))
	if err != nil {
		respondPlanErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, planResponse(plan))
}

// HandlePatchPlan updates slots and/or briefing on a draft/compiled plan.
func (a *DomainAgentAPI) HandlePatchPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	planID := chi.URLParam(r, "planID")
	var body struct {
		Slots    json.RawMessage `json:"slots"`
		Briefing json.RawMessage `json:"briefing"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if len(body.Slots) == 0 && len(body.Briefing) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "slots or briefing is required"})
		return
	}
	if len(body.Slots) > 0 {
		if err := a.plans.UpdateSlots(ctx, orgID, planID, body.Slots); err != nil {
			respondPlanErr(w, err)
			return
		}
	}
	if len(body.Briefing) > 0 {
		if err := a.plans.UpdateBriefing(ctx, orgID, planID, body.Briefing); err != nil {
			respondPlanErr(w, err)
			return
		}
	}
	plan, err := a.plans.Get(ctx, orgID, planID)
	if err != nil {
		respondPlanErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, planResponse(plan))
}

// HandleAttachPayloads validates and stores the compiled deploy payloads.
// Each payload must decode into engine.PMTACampaignInput; the raw JSON is
// stored as given so extra keys (e.g. "_audit") survive round-trip.
func (a *DomainAgentAPI) HandleAttachPayloads(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	planID := chi.URLParam(r, "planID")

	plan, err := a.plans.Get(ctx, orgID, planID)
	if err != nil {
		respondPlanErr(w, err)
		return
	}

	var body struct {
		Payloads []json.RawMessage `json:"payloads"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if len(body.Payloads) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one payload is required"})
		return
	}

	type payloadError struct {
		Index int    `json:"index"`
		Error string `json:"error"`
	}
	var payloadErrors []payloadError
	addErr := func(i int, msg string) {
		payloadErrors = append(payloadErrors, payloadError{Index: i, Error: msg})
	}
	for i, raw := range body.Payloads {
		var input engine.PMTACampaignInput
		if err := json.Unmarshal(raw, &input); err != nil {
			addErr(i, "invalid PMTACampaignInput JSON: "+err.Error())
			continue
		}
		if input.Name == "" {
			addErr(i, "campaign name is required")
			continue
		}
		if !strings.EqualFold(input.SendingDomain, plan.SendingDomain) {
			addErr(i, fmt.Sprintf("sending_domain %q does not match plan domain %q", input.SendingDomain, plan.SendingDomain))
			continue
		}
		if len(input.Variants) == 0 {
			addErr(i, "at least one content variant is required")
			continue
		}
		for _, v := range input.Variants {
			if strings.TrimSpace(v.HTMLContent) == "" {
				addErr(i, fmt.Sprintf("variant %s has empty HTML content", v.VariantName))
				break
			}
		}
	}
	if len(payloadErrors) > 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"errors": payloadErrors})
		return
	}

	payloadsJSON, err := json.Marshal(body.Payloads)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := a.plans.AttachPayloads(ctx, orgID, planID, payloadsJSON); err != nil {
		respondPlanErr(w, err)
		return
	}
	plan, err = a.plans.Get(ctx, orgID, planID)
	if err != nil {
		respondPlanErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, planResponse(plan))
}

// domainAgentDeployResult is one payload's deploy outcome on a plan.
type domainAgentDeployResult struct {
	Name       string `json:"name"`
	CampaignID string `json:"campaign_id,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// HandleApprovePlan approves a compiled plan and deploys each payload
// sequentially in-process via PMTACampaignService.DeployFromInput. Failures
// are recorded and do not stop later payloads.
func (a *DomainAgentAPI) HandleApprovePlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	planID := chi.URLParam(r, "planID")

	var body struct {
		ApprovedBy string `json:"approved_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if strings.TrimSpace(body.ApprovedBy) == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "approved_by is required"})
		return
	}

	plan, err := a.plans.Get(ctx, orgID, planID)
	if err != nil {
		respondPlanErr(w, err)
		return
	}
	if plan.Status != "compiled" {
		respondJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("plan must be compiled to approve (status=%s)", plan.Status),
		})
		return
	}
	var payloads []json.RawMessage
	if err := json.Unmarshal(plan.Payloads, &payloads); err != nil || len(payloads) == 0 {
		respondJSON(w, http.StatusConflict, map[string]string{"error": "plan has no compiled payloads"})
		return
	}

	if err := a.plans.Approve(ctx, orgID, planID, body.ApprovedBy); err != nil {
		respondPlanErr(w, err)
		return
	}

	results := make([]domainAgentDeployResult, 0, len(payloads))
	allOK := true
	for i, raw := range payloads {
		var input engine.PMTACampaignInput
		if err := json.Unmarshal(raw, &input); err != nil {
			results = append(results, domainAgentDeployResult{
				Name:   fmt.Sprintf("payload[%d]", i),
				Status: "failed",
				Error:  "invalid PMTACampaignInput JSON: " + err.Error(),
			})
			allOK = false
			continue
		}
		campaignID, status, err := a.pmta.DeployFromInput(ctx, orgID, input)
		if err != nil {
			log.Printf("[DomainAgent] plan %s payload %d (%s) deploy failed: %v", planID, i, input.Name, err)
			results = append(results, domainAgentDeployResult{Name: input.Name, Status: "failed", Error: err.Error()})
			allOK = false
			continue
		}
		log.Printf("[DomainAgent] plan %s payload %d (%s) deployed → campaign %s (%s)", planID, i, input.Name, campaignID, status)
		results = append(results, domainAgentDeployResult{Name: input.Name, CampaignID: campaignID, Status: status})
	}

	finalStatus := "deployed"
	if !allOK {
		finalStatus = "failed"
	}
	resultsJSON, _ := json.Marshal(results)
	if err := a.plans.SetDeployResults(ctx, orgID, planID, resultsJSON, finalStatus); err != nil {
		log.Printf("[DomainAgent] plan %s: failed to persist deploy results: %v", planID, err)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
		"status":  finalStatus,
	})
}

// HandlePlanStatus reports live campaign state for the campaigns a plan deployed.
func (a *DomainAgentAPI) HandlePlanStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	plan, err := a.plans.Get(ctx, orgID, chi.URLParam(r, "planID"))
	if err != nil {
		respondPlanErr(w, err)
		return
	}

	var recorded []domainAgentDeployResult
	if err := json.Unmarshal(plan.DeployResults, &recorded); err != nil {
		respondJSON(w, http.StatusOK, []map[string]interface{}{})
		return
	}
	var ids []string
	for _, res := range recorded {
		if res.CampaignID != "" {
			ids = append(ids, res.CampaignID)
		}
	}
	out := []map[string]interface{}{}
	if len(ids) == 0 {
		respondJSON(w, http.StatusOK, out)
		return
	}

	rows, err := a.db.QueryContext(ctx, `
		SELECT id, COALESCE(name, ''), status, COALESCE(total_recipients, 0), COALESCE(queued_count, 0)
		FROM mailing_campaigns
		WHERE id = ANY($1::uuid[])
	`, pq.Array(ids))
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, status string
		var totalRecipients, queuedCount int
		if err := rows.Scan(&id, &name, &status, &totalRecipients, &queuedCount); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out = append(out, map[string]interface{}{
			"campaign_id":      id,
			"name":             name,
			"status":           status,
			"total_recipients": totalRecipients,
			"queued_count":     queuedCount,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// HandleUpcoming returns the domain's SCHEDULED sending board — every campaign
// (regardless of which tool deployed it) whose planned waves fire from now
// through the next N days, grouped by calendar day. This is the forward-looking
// counterpart to the scorecard: the scorecard reads what happened, /upcoming
// reads what is committed to happen (mailing_campaigns + isp plans + waves).
func (a *DomainAgentAPI) HandleUpcoming(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	domain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
	if domain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "domain query parameter is required"})
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > 30 {
		days = 30
	}
	// A brand's PMTA route (em.<apex>) and SES tenant (m.<apex>) are the same
	// property to the operator — show both under either spelling.
	alt := domain
	switch {
	case strings.HasPrefix(domain, "em."):
		alt = "m." + strings.TrimPrefix(domain, "em.")
	case strings.HasPrefix(domain, "m."):
		alt = "em." + strings.TrimPrefix(domain, "m.")
	}

	rows, err := a.db.QueryContext(ctx, `
		SELECT c.id::text, c.name, c.status, COALESCE(c.subject, ''),
		       COALESCE(c.total_recipients, 0),
		       ARRAY_AGG(DISTINCT ip.isp) FILTER (WHERE ip.isp IS NOT NULL),
		       ARRAY_AGG(DISTINCT ip.sending_domain),
		       MIN(w.scheduled_at), MAX(w.window_end_at),
		       COUNT(DISTINCT w.id),
		       COUNT(DISTINCT w.id) FILTER (WHERE w.status = 'planned')
		FROM mailing_campaigns c
		JOIN mailing_campaign_isp_plans ip ON ip.campaign_id = c.id
		JOIN mailing_campaign_waves w ON w.campaign_id = c.id
		WHERE c.organization_id = $1
		  AND LOWER(ip.sending_domain) IN ($2, $3)
		  AND c.status IN ('scheduled', 'sending', 'preparing', 'finalizing_audience')
		  AND w.scheduled_at >= NOW() - INTERVAL '6 hours'
		  AND w.scheduled_at < CURRENT_DATE + ($4 + 1) * INTERVAL '1 day'
		GROUP BY c.id
		ORDER BY MIN(w.scheduled_at)
	`, orgID, domain, alt, days)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type upcomingCampaign struct {
		ID            string    `json:"id"`
		Name          string    `json:"name"`
		Status        string    `json:"status"`
		Subject       string    `json:"subject"`
		Recipients    int       `json:"recipients"`
		ISPs          []string  `json:"isps"`
		Domains       []string  `json:"sending_domains"`
		FirstWaveAt   time.Time `json:"first_wave_at"`
		WindowEndAt   time.Time `json:"window_end_at"`
		Waves         int       `json:"waves"`
		WavesPending  int       `json:"waves_pending"`
	}
	type upcomingDay struct {
		Date           string             `json:"date"`
		Campaigns      []upcomingCampaign `json:"campaigns"`
		Recipients     int                `json:"recipients"`
		DripCampaigns  int                `json:"drip_campaigns"`
		DripRecipients int                `json:"drip_recipients"`
	}
	dayIdx := map[string]int{}
	var out []upcomingDay
	for rows.Next() {
		var uc upcomingCampaign
		var isps, doms pq.StringArray
		if err := rows.Scan(&uc.ID, &uc.Name, &uc.Status, &uc.Subject, &uc.Recipients,
			&isps, &doms, &uc.FirstWaveAt, &uc.WindowEndAt, &uc.Waves, &uc.WavesPending); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		uc.ISPs = isps
		uc.Domains = doms
		day := uc.FirstWaveAt.UTC().Format("2006-01-02")
		i, ok := dayIdx[day]
		if !ok {
			i = len(out)
			dayIdx[day] = i
			out = append(out, upcomingDay{Date: day})
		}
		// Partner-drip micro-campaigns fire every ~15 min; rolled up so they
		// don't drown the scheduled lanes in the table.
		if strings.HasPrefix(uc.Name, "[partner-drip]") {
			out[i].DripCampaigns++
			out[i].DripRecipients += uc.Recipients
			continue
		}
		out[i].Campaigns = append(out[i].Campaigns, uc)
		out[i].Recipients += uc.Recipients
	}
	if err := rows.Err(); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = []upcomingDay{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"domain": domain,
		"days":   days,
		"dates":  out,
	})
}
