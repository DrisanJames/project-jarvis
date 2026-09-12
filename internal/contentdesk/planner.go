package contentdesk

// Planner (2026-09-12) keeps the network writing on its own: it proposes one
// brief for the least-recently-served site that has nothing in flight, files
// it, and queues the article. The pipeline then researches, drafts, reviews
// and (with CONTENT_DESK_AGENT_REVIEW=1) approves it; the publishers release
// approved revisions. Bounded three ways: CONTENT_DESK_PLANNER_PER_DAY briefs
// per Denver day (0 = off), no new brief once half the daily budget is spent,
// and no new brief while maxInFlight articles are already queued.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	EnvPlannerPerDay       = "CONTENT_DESK_PLANNER_PER_DAY"
	EnvPlannerSiteGapHours = "CONTENT_DESK_PLANNER_SITE_GAP_HOURS"
	EnvPlannerSpendShare   = "CONTENT_DESK_PLANNER_SPEND_SHARE"
	// PlannerCreatedBy marks the briefs the planner files.
	PlannerCreatedBy = "content-desk-planner"
	// Operator 2026-09-12: "I don't want the content generated daily. I
	// wanna generate it every other day." — a site gets at most one new
	// brief per 48h by default.
	defaultPlannerSiteGap    = 48 * time.Hour
	defaultPlannerSpendShare = 0.5
	// plannerMaxExisting bounds the existing questions shown to the planner.
	plannerMaxExisting = 60
)

// PlannerPerDay is the daily brief budget (default 0 = planner off).
func PlannerPerDay() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvPlannerPerDay))); err == nil && v > 0 {
		return v
	}
	return 0
}

// PlannerSiteGap is the minimum time between two briefs for one site
// (default 48h; 12h..14d).
func PlannerSiteGap() time.Duration {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(EnvPlannerSiteGapHours)), 64); err == nil && v >= 12 && v <= 336 {
		return time.Duration(v * float64(time.Hour))
	}
	return defaultPlannerSiteGap
}

// PlannerSpendShare: the planner stops filing once this share of the daily
// budget is spent, so queued articles can still finish (default 0.5;
// 0.1..0.95).
func PlannerSpendShare() float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(EnvPlannerSpendShare)), 64); err == nil && v >= 0.1 && v <= 0.95 {
		return v
	}
	return defaultPlannerSpendShare
}

// SiteDefaultCategory is each property's desk category (the research
// allow-list it is written from). A site's own categories column, when it
// lists a valid category, wins.
var SiteDefaultCategory = map[string]string{
	"aadwd.com":                  CatDIY,
	"bestcreditcare.com":         CatFinance,
	"businessweeklypro.com":      CatFinance,
	"casainsure.com":             CatInsurance,
	"consumerpro.net":            CatFinance,
	"discountblog.com":           CatFinance,
	"financialcalculate.com":     CatFinance,
	"firsttimebuyerhomeloan.com": CatFinance,
	"hfcl.net":                   CatFinance,
	"historythinking.com":        CatHistory,
	"homeloansbyjaime.com":       CatFinance,
	"hometracmortgage.com":       CatFinance,
	"homewarrantyservices.org":   CatDIY,
	"learnpersonalloans.com":     CatFinance,
	"myownhealth.net":            CatHealth,
	"mypersonalfinancial.com":    CatFinance,
	"myrepairdiy.com":            CatDIY,
	"paymydebit.com":             CatFinance,
	"ratesbazar.com":             CatFinance,
	"refinanceratesusa.com":      CatFinance,
	"theretirementblog.com":      CatBenefits,
	"thingoftheday.org":          CatHistory,
	"us-finance.com":             CatFinance,
	"warrantyforyou.com":         CatDIY,
	"yourfinancialblog.com":      CatFinance,
	"yourinsurancehub.com":       CatInsurance,
}

// PlannerFormats are the brief formats the planner may choose.
var PlannerFormats = []string{"explainer", "document_anatomy", "how_to", "comparison"}

// PlannerSite is one enabled site with what the planner needs to rank it.
type PlannerSite struct {
	Site
	OrgID     string
	LastBrief time.Time // zero = never briefed
	InFlight  int       // articles in drafting
	Questions []string  // existing reader questions, newest first
}

// SiteCategory resolves a site's desk category; "" = the planner skips it.
func SiteCategory(st Site) string {
	var cats []string
	if json.Unmarshal(st.Categories, &cats) == nil {
		for _, c := range cats {
			if ValidCategory(c) {
				return c
			}
		}
	}
	return SiteDefaultCategory[st.Domain]
}

type plannerStore interface {
	PlannerSites(ctx context.Context) ([]PlannerSite, error)
	PlannedToday(ctx context.Context, org string) (int, error)
	QueuedArticles(ctx context.Context, org string) (int, error)
	SpentToday(ctx context.Context, org string) (float64, error)
	CreateBrief(ctx context.Context, org string, in BriefInput) (Brief, error)
	EnqueueRun(ctx context.Context, org, id string) error
}

// Planner files briefs.
type Planner struct {
	Store plannerStore
	LLM   LLM
	Now   func() time.Time
}

// NewPlanner wires the planner to the desk's store and budget-gated model.
func NewPlanner(store *Store, llm LLM) *Planner {
	return &Planner{Store: store, LLM: llm, Now: time.Now}
}

// Plan files at most one brief per org and queues its article. Returns the
// number filed.
func (pl *Planner) Plan(ctx context.Context, maxInFlight int) (int, error) {
	per := PlannerPerDay()
	if per <= 0 {
		return 0, nil
	}
	sites, err := pl.Store.PlannerSites(ctx)
	if err != nil {
		return 0, err
	}
	var orgs []string
	byOrg := map[string][]PlannerSite{}
	for _, s := range sites {
		if _, ok := byOrg[s.OrgID]; !ok {
			orgs = append(orgs, s.OrgID)
		}
		byOrg[s.OrgID] = append(byOrg[s.OrgID], s)
	}
	filed := 0
	for _, org := range orgs {
		if planned, err := pl.Store.PlannedToday(ctx, org); err != nil || planned >= per {
			continue
		}
		if spent, err := pl.Store.SpentToday(ctx, org); err != nil || spent >= PlannerSpendShare()*DailyBudgetUSD() {
			continue
		}
		if queued, err := pl.Store.QueuedArticles(ctx, org); err != nil || queued >= maxInFlight {
			continue
		}
		for _, st := range byOrg[org] { // least recently briefed first
			cat := SiteCategory(st.Site)
			if cat == "" || st.InFlight > 0 || pl.Now().Sub(st.LastBrief) < PlannerSiteGap() {
				continue
			}
			b, err := pl.planSite(ctx, org, st, cat)
			if err != nil {
				log.Printf("[ContentDesk] planner site=%s: %v", st.Domain, err)
				if errors.Is(err, ErrBudgetExceeded) || errors.Is(err, ErrBudgetUnavailable) || errors.Is(err, ErrDisabled) {
					return filed, nil
				}
				continue
			}
			log.Printf("[ContentDesk] planner site=%s: filed brief %s (%s, %s) %q — article %s queued", st.Domain, b.ID, b.Format, b.Category, b.ReaderQuestion, b.ArticleID)
			filed++
			break
		}
	}
	return filed, nil
}

// PlannerSiteStatus is one site's row in the planner status report.
type PlannerSiteStatus struct {
	Site         string     `json:"site"`
	BrandCode    string     `json:"brand_code"`
	Category     string     `json:"category"`
	Briefs       int        `json:"briefs"`
	InFlight     int        `json:"in_flight"`
	LastBrief    *time.Time `json:"last_brief,omitempty"`
	NextEligible *time.Time `json:"next_eligible,omitempty"`
	EligibleNow  bool       `json:"eligible_now"`
	Reason       string     `json:"reason"`
}

// PlannerStatusReport is the live proof of what the planner will do: its
// settings, today's budget position, and every enabled site's standing.
type PlannerStatusReport struct {
	Enabled           bool                `json:"enabled"`
	PerDay            int                 `json:"per_day"`
	SiteGapHours      float64             `json:"site_gap_hours"`
	SpendShare        float64             `json:"spend_share"`
	DailyBudgetUSD    float64             `json:"daily_budget_usd"`
	SpentTodayUSD     float64             `json:"spent_today_usd"`
	FilingStopsAtUSD  float64             `json:"filing_stops_at_usd"`
	PlannedToday      int                 `json:"planned_today"`
	Queued            int                 `json:"queued"`
	SitesTotal        int                 `json:"sites_total"`
	SitesWithCategory int                 `json:"sites_with_category"`
	EligibleNow       int                 `json:"eligible_now"`
	Sites             []PlannerSiteStatus `json:"sites"`
}

// Status reports the planner's settings and every enabled site in org, in
// the order the planner would serve them.
func (pl *Planner) Status(ctx context.Context, org string) (PlannerStatusReport, error) {
	now := pl.Now()
	rep := PlannerStatusReport{PerDay: PlannerPerDay(), SiteGapHours: PlannerSiteGap().Hours(), SpendShare: PlannerSpendShare(),
		DailyBudgetUSD: DailyBudgetUSD(), Sites: []PlannerSiteStatus{}}
	rep.Enabled = rep.PerDay > 0
	rep.FilingStopsAtUSD = rep.SpendShare * rep.DailyBudgetUSD
	sites, err := pl.Store.PlannerSites(ctx)
	if err != nil {
		return rep, err
	}
	if rep.SpentTodayUSD, err = pl.Store.SpentToday(ctx, org); err != nil {
		return rep, err
	}
	if rep.PlannedToday, err = pl.Store.PlannedToday(ctx, org); err != nil {
		return rep, err
	}
	if rep.Queued, err = pl.Store.QueuedArticles(ctx, org); err != nil {
		return rep, err
	}
	gap := PlannerSiteGap()
	for _, st := range sites {
		if st.OrgID != org {
			continue
		}
		row := PlannerSiteStatus{Site: st.Domain, BrandCode: st.BrandCode, Category: SiteCategory(st.Site), Briefs: len(st.Questions), InFlight: st.InFlight}
		if !st.LastBrief.IsZero() && st.LastBrief.Year() > 1970 {
			lb, next := st.LastBrief, st.LastBrief.Add(gap)
			row.LastBrief, row.NextEligible = &lb, &next
		}
		switch {
		case row.Category == "":
			row.Reason = "no category — the planner skips this site"
		case st.InFlight > 0:
			row.Reason = "an article is being written"
		case row.NextEligible != nil && now.Before(*row.NextEligible):
			row.Reason = "next brief due " + row.NextEligible.UTC().Format("2006-01-02 15:04Z")
		default:
			row.EligibleNow = true
			row.Reason = "eligible now"
		}
		rep.SitesTotal++
		if row.Category != "" {
			rep.SitesWithCategory++
		}
		if row.EligibleNow {
			rep.EligibleNow++
		}
		rep.Sites = append(rep.Sites, row)
	}
	return rep, nil
}

const plannerSystem = `You are the editorial planner for one brand website. Propose ONE article brief.

Rules:
- The reader question is one a real reader of this site asks, answered by explaining how something works: a rule, a form or document, a calculation, a process, a historical event.
- It must be answerable completely from the allowed sources listed (government and reference sites). If the answer depends on today's rates or prices, a specific company's product, or a promotion, choose a different question.
- Evergreen and specific: one question, at most 140 characters, ending with "?".
- Not the same question as, or a rewording of, any existing question listed.
- No offers, brands, products or calls to action. No advice for an individual's situation — explain how the rule or process works.
- angle: one sentence naming the official source document or explanation the article walks through, and the worked example or structure it uses.
- slug: lowercase words joined by hyphens, at most 60 characters.`

func plannerSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"angle", "format", "reader_question", "slug"},
		"properties": map[string]any{
			"angle":           map[string]any{"type": "string"},
			"format":          map[string]any{"type": "string", "enum": PlannerFormats},
			"reader_question": map[string]any{"type": "string"},
			"slug":            map[string]any{"type": "string"},
		},
	}
}

func (pl *Planner) planSite(ctx context.Context, org string, st PlannerSite, cat string) (Brief, error) {
	existing := st.Questions
	if len(existing) > plannerMaxExisting {
		existing = existing[:plannerMaxExisting]
	}
	var p strings.Builder
	fmt.Fprintf(&p, "Site: %s (%s)\nCategory: %s\nAllowed sources: %s\n", st.Domain, st.BrandCode, cat, strings.Join(CategoryAllowlist[cat], ", "))
	if v := strings.TrimSpace(string(st.Voice)); v != "" && v != "{}" && v != "null" {
		fmt.Fprintf(&p, "Site voice: %s\n", v)
	}
	p.WriteString("\nExisting questions on this site:\n")
	if len(existing) == 0 {
		p.WriteString("(none)\n")
	}
	for _, q := range existing {
		fmt.Fprintf(&p, "- %s\n", q)
	}
	gen, err := pl.LLM.Generate(ctx, GenerateRequest{OrgID: org, Tier: TierWrite, System: plannerSystem,
		Prompt: p.String(), Schema: plannerSchema(), MaxTokens: 4000})
	if err != nil {
		return Brief{}, err
	}
	var out struct {
		Angle          string `json:"angle"`
		Format         string `json:"format"`
		ReaderQuestion string `json:"reader_question"`
		Slug           string `json:"slug"`
	}
	if err := json.Unmarshal(gen.JSON, &out); err != nil {
		return Brief{}, fmt.Errorf("%w: %v", ErrNoStructuredOutput, err)
	}
	q := strings.TrimSpace(out.ReaderQuestion)
	if len(q) < 20 || len(q) > 200 || !strings.HasSuffix(q, "?") {
		return Brief{}, fmt.Errorf("%w: planner question %q", ErrInvalid, q)
	}
	valid := false
	for _, f := range PlannerFormats {
		valid = valid || out.Format == f
	}
	if !valid {
		return Brief{}, fmt.Errorf("%w: planner format %q", ErrInvalid, out.Format)
	}
	for _, e := range st.Questions {
		if Slugify(e) == Slugify(q) {
			return Brief{}, fmt.Errorf("%w: planner repeated an existing question %q", ErrConflict, q)
		}
	}
	slug := strings.TrimSpace(out.Slug)
	if !slugRE.MatchString(slug) || len(slug) > MaxSlugLen {
		slug = Slugify(q)
	}
	b, err := pl.Store.CreateBrief(ctx, org, BriefInput{SiteID: st.ID, Format: out.Format, ReaderQuestion: q,
		Angle: strings.TrimSpace(out.Angle), Category: cat, CreatedBy: PlannerCreatedBy, Slug: slug})
	if err != nil {
		return b, err
	}
	return b, pl.Store.EnqueueRun(ctx, org, b.ArticleID)
}

// PlannerSites lists enabled sites, least recently briefed first.
func (s *Store) PlannerSites(ctx context.Context) ([]PlannerSite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.org_id, `+prefixCols("s.", siteCols)+`,
			COALESCE(max(b.created_at), 'epoch'::timestamptz),
			count(a.id) FILTER (WHERE a.status = 'drafting'),
			COALESCE(array_agg(b.reader_question ORDER BY b.created_at DESC) FILTER (WHERE b.id IS NOT NULL), '{}')
		FROM content_sites s
		LEFT JOIN content_briefs b ON b.site_id = s.id AND b.org_id = s.org_id
		LEFT JOIN content_articles a ON a.brief_id = b.id
		WHERE s.enabled
		GROUP BY s.org_id, s.id
		ORDER BY 10, s.domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlannerSite
	for rows.Next() {
		var ps PlannerSite
		var voice, cats, adapter []byte
		if err := rows.Scan(&ps.OrgID, &ps.ID, &ps.BrandCode, &ps.Domain, &ps.Surface, &ps.Enabled, &voice, &cats, &adapter,
			&ps.LastBrief, &ps.InFlight, pq.Array(&ps.Questions)); err != nil {
			return nil, err
		}
		ps.Voice, ps.Categories, ps.Adapter = voice, cats, adapter
		out = append(out, ps)
	}
	return out, rows.Err()
}

func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = prefix + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// PlannedToday counts the planner's briefs filed today (Denver).
func (s *Store) PlannedToday(ctx context.Context, org string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM content_briefs
		WHERE org_id = $1 AND created_by = $2 AND (created_at AT TIME ZONE 'America/Denver')::date = `+denverToday,
		org, PlannerCreatedBy).Scan(&n)
	return n, err
}

// QueuedArticles counts articles queued to run.
func (s *Store) QueuedArticles(ctx context.Context, org string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM content_articles
		WHERE org_id = $1 AND status = 'drafting' AND run_requested_at IS NOT NULL`, org).Scan(&n)
	return n, err
}
