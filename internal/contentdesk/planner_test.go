package contentdesk

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type fakePlannerStore struct {
	sites    []PlannerSite
	planned  int
	queued   int
	spent    float64
	briefs   []BriefInput
	enqueued []string
}

func (f *fakePlannerStore) PlannerSites(context.Context) ([]PlannerSite, error) { return f.sites, nil }
func (f *fakePlannerStore) PlannedToday(context.Context, string) (int, error)   { return f.planned, nil }
func (f *fakePlannerStore) QueuedArticles(context.Context, string) (int, error) {
	return f.queued, nil
}
func (f *fakePlannerStore) SpentToday(context.Context, string) (float64, error) { return f.spent, nil }
func (f *fakePlannerStore) CreateBrief(_ context.Context, _ string, in BriefInput) (Brief, error) {
	f.briefs = append(f.briefs, in)
	return Brief{ID: "b1", ArticleID: "art1", Format: in.Format, Category: in.Category, ReaderQuestion: in.ReaderQuestion}, nil
}
func (f *fakePlannerStore) EnqueueRun(_ context.Context, _, id string) error {
	f.enqueued = append(f.enqueued, id)
	return nil
}

var plannerNow = time.Date(2026, 9, 12, 15, 0, 0, 0, time.UTC)

func plannerSites() []PlannerSite {
	return []PlannerSite{
		{OrgID: "o", Site: Site{ID: "s-busy", Domain: "financialcalculate.com"}, InFlight: 1},                       // in flight
		{OrgID: "o", Site: Site{ID: "s-recent", Domain: "discountblog.com"}, LastBrief: plannerNow.Add(-time.Hour)}, // briefed an hour ago
		{OrgID: "o", Site: Site{ID: "s-unknown", Domain: "example.org"}},                                            // no category
		{OrgID: "o", Site: Site{ID: "s-health", Domain: "myownhealth.net"}, Questions: []string{"What does the % Daily Value on a Nutrition Facts label actually mean?"}},
		{OrgID: "o", Site: Site{ID: "s-diy", Domain: "myrepairdiy.com"}},
	}
}

const plannerOut = `{"angle":"Walk through the FDA's added-sugars line with a worked label.","format":"document_anatomy","reader_question":"How do you read the added sugars line on a Nutrition Facts label?","slug":"added-sugars-line-nutrition-label"}`

func TestPlanner_FilesOneBriefForTheFirstEligibleSiteAndQueuesIt(t *testing.T) {
	t.Setenv(EnvPlannerPerDay, "4")
	t.Setenv(EnvDailyUSD, "75")
	st := &fakePlannerStore{sites: plannerSites(), spent: 10}
	llm := &seqLLM{outs: []string{plannerOut}}
	n, err := (&Planner{Store: st, LLM: llm, Now: func() time.Time { return plannerNow }}).Plan(context.Background(), 2)
	if err != nil || n != 1 || len(st.briefs) != 1 || len(llm.reqs) != 1 {
		t.Fatalf("want one brief: n=%d err=%v briefs=%d calls=%d", n, err, len(st.briefs), len(llm.reqs))
	}
	b := st.briefs[0]
	if b.SiteID != "s-health" || b.Category != CatHealth || b.CreatedBy != PlannerCreatedBy || b.Slug != "added-sugars-line-nutrition-label" {
		t.Fatalf("busy, recent and uncategorised sites are skipped; health site gets a health brief: %+v", b)
	}
	if len(st.enqueued) != 1 || st.enqueued[0] != "art1" {
		t.Fatalf("the article must be queued: %v", st.enqueued)
	}
}

func TestPlanner_Gates(t *testing.T) {
	cases := []struct {
		name   string
		per    string
		spent  float64
		queued int
		plan   int
	}{
		{"off by default", "", 0, 0, 0},
		{"daily count reached", "4", 0, 0, 4},
		{"half the budget spent", "4", 37.5, 0, 0},
		{"queue full", "4", 0, 2, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EnvPlannerPerDay, c.per)
			t.Setenv(EnvDailyUSD, "75")
			st := &fakePlannerStore{sites: plannerSites(), spent: c.spent, queued: c.queued, planned: c.plan}
			llm := &seqLLM{outs: []string{plannerOut}}
			n, _ := (&Planner{Store: st, LLM: llm, Now: func() time.Time { return plannerNow }}).Plan(context.Background(), 2)
			if n != 0 || len(llm.reqs) != 0 || len(st.briefs) != 0 {
				t.Fatalf("gate must hold: n=%d calls=%d briefs=%d", n, len(llm.reqs), len(st.briefs))
			}
		})
	}
}

func TestPlanner_RefusesARepeatedQuestion(t *testing.T) {
	t.Setenv(EnvPlannerPerDay, "4")
	st := &fakePlannerStore{sites: plannerSites()[3:4]}
	dup := `{"angle":"a","format":"explainer","reader_question":"What does the % Daily Value on a Nutrition Facts label actually mean?","slug":"x"}`
	n, _ := (&Planner{Store: st, LLM: &seqLLM{outs: []string{dup}}, Now: func() time.Time { return plannerNow }}).Plan(context.Background(), 2)
	if n != 0 || len(st.briefs) != 0 {
		t.Fatalf("a repeated question must not be filed: %+v", st.briefs)
	}
}

func TestSiteCategory_SiteColumnWinsOverDefault(t *testing.T) {
	if got := SiteCategory(Site{Domain: "discountblog.com", Categories: json.RawMessage(`["nope","tax"]`)}); got != CatTax {
		t.Fatalf("site categories column must win: %q", got)
	}
	if got := SiteCategory(Site{Domain: "discountblog.com", Categories: json.RawMessage(`[]`)}); got != CatFinance {
		t.Fatalf("default map: %q", got)
	}
	for d, c := range SiteDefaultCategory {
		if !ValidCategory(c) {
			t.Fatalf("%s maps to %q, which has no research allow-list", d, c)
		}
	}
}
