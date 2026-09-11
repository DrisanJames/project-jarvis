//go:build integration

// Content Desk end-to-end on REAL Postgres (the SQL sqlmock cannot run).
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -run ContentDesk ./cmd/server/ -v
//
// Creates a scratch database content_desk_test, applies contentDeskMigrations
// TWICE (boot idempotency), then drives one article through the whole
// lifecycle with a fake model: brief → pipeline (all six stages) → in_review
// → hash-bound review → release → live_confirmed/harvestable → claim version
// bump (changes_requested, not harvestable) → manifest-drop refusal →
// withdraw → takedown release. Drops the scratch database at the end.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/contentdesk"
	_ "github.com/lib/pq"
)

const (
	contentDeskAdminDSN   = "postgres://apex_user:apex_password@localhost:5432/postgres?sslmode=disable"
	contentDeskScratchDSN = "postgres://apex_user:apex_password@localhost:5432/content_desk_test?sslmode=disable"
	cdTestOrg             = "00000000-0000-0000-0000-000000000001"
)

type cdFakeLLM struct {
	mu    sync.Mutex
	calls map[string]int
}

var (
	cdClaimRE = regexp.MustCompile(`claim_id=([0-9a-f-]{36}) version=(\d+) status=supported`)
	cdRefRE   = regexp.MustCompile(`#\d+  unit=`)
)

func (f *cdFakeLLM) Generate(_ context.Context, req contentdesk.GenerateRequest) (*contentdesk.GenerateResult, error) {
	var stage, out string
	switch {
	case req.ToolName == "submit_research":
		stage = "research"
		out = `{"claims":[
		 {"text":"The 2026 IRA contribution limit is $7,000.","type":"sourced_fact",
		  "source_url":"https://www.irs.gov/retirement-plans/ira-contribution-limits","passage":"The annual contribution limit for 2026 is $7,000.",
		  "context":"","published_at":"2025-11-13","effective_at":"2026-01-01","jurisdiction":"US federal","population":"savers under 50",
		  "conditions":"earned income at least equal to the contribution","status":"supported","key":true,"claim_key":"ira_limit_2026",
		  "question":"What is the 2026 IRA contribution limit for savers under 50?","answer":"7000","calc":null},
		 {"text":"Some states add their own rules.","type":"interpretation","source_url":"","passage":"","context":"","published_at":"",
		  "effective_at":"","jurisdiction":"","population":"","conditions":"","status":"insufficient_evidence","key":false,"claim_key":"",
		  "question":"","answer":"","calc":null}]}`
	case req.ToolName == "submit_rederivation":
		stage = "rederive"
		if strings.Contains(req.Prompt, "7,000") || strings.Contains(req.Prompt, "7000") {
			return nil, errors.New("rederive prompt leaked the primary answer")
		}
		out = `{"answers":[{"question_index":0,"text":"The limit is $7,000.","answer":"$7,000",
		  "source_url":"https://www.irs.gov/newsroom/ira-limit","passage":"$7,000 for 2026","context":"","published_at":"",
		  "effective_at":"","jurisdiction":"","population":"","conditions":"","status":"supported"}]}`
	case req.Tier == contentdesk.TierWrite:
		stage = "draft"
		m := cdClaimRE.FindStringSubmatch(req.Prompt)
		if m == nil {
			return nil, errors.New("draft prompt carried no supported claim")
		}
		out = `{"blocks":[
		 {"id":"b1","type":"lede","heading":"","text":"Most savers can put $7,000 into an IRA for 2026.","items":[],"rows":[],"calc":null,"caption":""},
		 {"id":"b2","type":"section","heading":"Who it applies to","text":"The limit applies to people with earned income.","items":[],"rows":[],"calc":null,"caption":""},
		 {"id":"b3","type":"key_takeaways","heading":"","text":"","items":["Check your earned income first."],"rows":[],"calc":null,"caption":""}],
		 "claim_refs":[{"block_id":"b1","sentence_idx":0,"claim_id":"` + m[1] + `","version":` + m[2] + `}]}`
	case req.Tier == contentdesk.TierLight:
		stage = "package"
		m := cdClaimRE.FindStringSubmatch(req.Prompt)
		out = `{"title":"How the IRA contribution limit works","excerpt":"Most savers can contribute $7,000.",
		 "meta_title":"IRA contribution limit","meta_description":"What the IRA contribution limit is and who it applies to.",
		 "subjects":["Your IRA limit, explained"],"preheaders":["Who it applies to and when"],
		 "claim_refs":[{"block_id":"excerpt","sentence_idx":0,"claim_id":"` + m[1] + `","version":` + m[2] + `}]}`
	case req.Tier == contentdesk.TierJudge:
		stage = "judgment"
		n := len(cdRefRE.FindAllString(req.Prompt, -1))
		items := make([]string, n)
		for i := range items {
			items[i] = `{"ref_index":` + itoaCD(i) + `,"verdict":"supported","lost_qualifier":"","note":""}`
		}
		out = `{"items":[` + strings.Join(items, ",") + `],"flags":[]}`
	}
	f.mu.Lock()
	f.calls[stage]++
	f.mu.Unlock()
	return &contentdesk.GenerateResult{JSON: json.RawMessage(out), Usage: contentdesk.Usage{Model: "fake", USD: 0.01}}, nil
}

func itoaCD(i int) string { b, _ := json.Marshal(i); return string(b) }

func cdMust(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func TestContentDeskEndToEndOnRealPostgres(t *testing.T) {
	admin, err := sql.Open("postgres", req118DSN("CONTENT_DESK_ADMIN_DSN", contentDeskAdminDSN))
	if err != nil {
		t.Skipf("SKIP: cannot open local dev DB (%v)", err)
	}
	defer admin.Close()
	pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pcancel()
	if err := admin.PingContext(pctx); err != nil {
		t.Skipf("SKIP: cannot ping local dev DB (%v). Start apex-postgres.", err)
	}
	_, _ = admin.Exec(`DROP DATABASE IF EXISTS content_desk_test`)
	cdMust(t, func() error { _, e := admin.Exec(`CREATE DATABASE content_desk_test`); return e }(), "create scratch db")
	defer admin.Exec(`DROP DATABASE IF EXISTS content_desk_test WITH (FORCE)`)

	db, err := sql.Open("postgres", req118DSN("CONTENT_DESK_SCRATCH_DSN", contentDeskScratchDSN))
	cdMust(t, err, "open scratch")
	defer db.Close()
	for pass := 1; pass <= 2; pass++ {
		for _, m := range contentDeskMigrations {
			if _, err := db.Exec(m.sql); err != nil {
				t.Fatalf("pass %d %s: %v", pass, m.name, err)
			}
		}
	}
	ctx := context.Background()
	s := contentdesk.NewStore(db)

	sites, err := s.ListSites(ctx, cdTestOrg)
	cdMust(t, err, "list sites")
	if len(sites) != 27 {
		t.Fatalf("want 27 seeded sites after a double apply, got %d", len(sites))
	}
	var siteID string
	for _, st := range sites {
		if st.Domain == "financialcalculate.com" {
			siteID = st.ID
		}
	}

	b, err := s.CreateBrief(ctx, cdTestOrg, contentdesk.BriefInput{SiteID: siteID, Format: "explainer", Category: contentdesk.CatFinance,
		ReaderQuestion: "What is the IRA contribution limit?", Slug: "ira-contribution-limit", CreatedBy: "it"})
	cdMust(t, err, "create brief")
	if _, err := s.CreateBrief(ctx, cdTestOrg, contentdesk.BriefInput{SiteID: siteID, Format: "explainer", Category: contentdesk.CatFinance,
		ReaderQuestion: "dup", Slug: "ira-contribution-limit"}); !errors.Is(err, contentdesk.ErrConflict) {
		t.Fatalf("duplicate slug on a site must conflict, got %v", err)
	}
	art := b.ArticleID
	cdMust(t, s.EnqueueRun(ctx, cdTestOrg, art), "enqueue")
	if pend, _ := s.PendingArticles(ctx, 10); len(pend) != 1 || pend[0].ArticleID != art {
		t.Fatalf("pending = %+v", pend)
	}

	// Stage idempotency on real PG: same (article, stage, input_hash) twice.
	p := contentdesk.NewPipeline(s, &cdFakeLLM{calls: map[string]int{}})
	n := 0
	fn := func(context.Context) (any, contentdesk.Usage, error) { n++; return map[string]int{"n": n}, contentdesk.Usage{}, nil }
	_, _, err = p.RunStage(ctx, cdTestOrg, art, "probe", "h", fn)
	cdMust(t, err, "probe 1")
	out, _, err := p.RunStage(ctx, cdTestOrg, art, "probe", "h", fn)
	cdMust(t, err, "probe 2")
	var reused map[string]int // JSONB reformats stored output ({"n": 1}); compare values, not bytes
	if n != 1 || json.Unmarshal(out, &reused) != nil || reused["n"] != 1 {
		t.Fatalf("a finished stage input must be reused: n=%d out=%s", n, out)
	}

	fake := &cdFakeLLM{calls: map[string]int{}}
	p = contentdesk.NewPipeline(s, fake)
	cdMust(t, p.Run(ctx, cdTestOrg, art), "pipeline run")
	for _, st := range []string{"research", "rederive", "draft", "package", "judgment"} {
		if fake.calls[st] != 1 {
			t.Errorf("stage %s called %d times, want 1", st, fake.calls[st])
		}
	}

	d, err := s.GetArticleDetail(ctx, cdTestOrg, art)
	cdMust(t, err, "detail")
	if d.Article.Status != contentdesk.StatusInReview || d.Revision == nil || len(d.Revision.RevisionHash) != 64 {
		t.Fatalf("after the pipeline: %+v", d.Article)
	}
	for _, c := range d.Revision.Checks.Code {
		if !c.Passed {
			t.Errorf("code check %s failed: %v", c.Name, c.Details)
		}
	}
	if len(d.Pairs) != 2 || d.Pairs[0].Claim == nil || d.Pairs[0].Claim.Passage == "" || d.Pairs[0].Judgment == nil {
		t.Fatalf("sentence↔passage pairs: %+v", d.Pairs)
	}
	hash := d.Revision.RevisionHash
	claimID := d.Pairs[0].ClaimID

	if _, _, err := s.SubmitReview(ctx, cdTestOrg, art, contentdesk.ReviewInput{RevisionHash: strings.Repeat("0", 64),
		Reviewer: "ann", Role: "primary", Decision: "approve"}); !errors.Is(err, contentdesk.ErrHashMismatch) {
		t.Fatalf("wrong hash must be refused, got %v", err)
	}
	_, outc, err := s.SubmitReview(ctx, cdTestOrg, art, contentdesk.ReviewInput{RevisionHash: hash, Reviewer: "ann", Role: "primary", Decision: "approve", Minutes: 12})
	cdMust(t, err, "approve")
	if outc.Status != contentdesk.StatusApproved {
		t.Fatalf("outcome %+v", outc)
	}

	rel, err := s.CreateRelease(ctx, cdTestOrg, siteID)
	cdMust(t, err, "create release")
	if _, err := s.CreateRelease(ctx, cdTestOrg, siteID); !errors.Is(err, contentdesk.ErrConflict) {
		t.Fatalf("a second in-flight release must conflict, got %v", err)
	}
	for _, st := range []string{contentdesk.ReleaseBuilding, contentdesk.ReleaseDeployed, contentdesk.ReleaseLiveConfirmed} {
		_, err := s.SetReleaseStatus(ctx, cdTestOrg, rel.ID, st, "")
		cdMust(t, err, "release → "+st)
	}
	d, _ = s.GetArticleDetail(ctx, cdTestOrg, art)
	if d.Article.Status != contentdesk.StatusLiveConfirmed || !d.Article.Harvestable {
		t.Fatalf("live_confirmed must set harvestable: %+v", d.Article)
	}

	_, flagged, err := s.NewClaimVersion(ctx, cdTestOrg, claimID, contentdesk.Claim{Text: "The limit is $7,500.",
		Type: contentdesk.ClaimSourcedFact, Status: contentdesk.ClaimSupported})
	cdMust(t, err, "claim bump")
	d, _ = s.GetArticleDetail(ctx, cdTestOrg, art)
	if len(flagged) != 1 || d.Article.Status != contentdesk.StatusChangesRequested || d.Article.Harvestable || !d.Pairs[0].Stale {
		t.Fatalf("claim bump must invalidate: flagged=%v article=%+v", flagged, d.Article)
	}
	if _, err := db.Exec(`UPDATE content_claims SET text = 'rewritten' WHERE claim_id = $1`, claimID); err == nil {
		t.Fatal("content_claims must be append-only")
	}

	if _, err := s.CreateRelease(ctx, cdTestOrg, siteID); !errors.Is(err, contentdesk.ErrManifestDrop) {
		t.Fatalf("dropping the live article must be refused, got %v", err)
	}
	if _, err := s.Withdraw(ctx, cdTestOrg, art); err != nil {
		t.Fatal(err)
	}
	take, err := s.CreateRelease(ctx, cdTestOrg, siteID)
	cdMust(t, err, "takedown release after withdraw")
	if len(take.Manifest) != 0 {
		t.Fatalf("takedown manifest must be empty: %+v", take.Manifest)
	}

	cdMust(t, s.AddSpend(ctx, cdTestOrg, 1.25), "add spend")
	if v, _ := s.SpentToday(ctx, cdTestOrg); v < 1.25 {
		t.Fatalf("spent today = %v", v)
	}
	now := time.Now().UTC().Truncate(time.Second)
	_, err = s.UpsertOpsReport(ctx, cdTestOrg, contentdesk.OpsReport{Source: "site_probes", GeneratedAt: now,
		Checks: []contentdesk.OpsCheck{{Name: "db", Status: contentdesk.CheckOK}}, Payload: json.RawMessage(`{"x":1}`)})
	cdMust(t, err, "ops report")
	_, err = s.UpsertOpsReport(ctx, cdTestOrg, contentdesk.OpsReport{Source: "site_probes", GeneratedAt: now.Add(-time.Hour),
		Checks: []contentdesk.OpsCheck{{Name: "old", Status: contentdesk.CheckFail}}})
	cdMust(t, err, "older ops report")
	reps, err := s.ListOpsReports(ctx, cdTestOrg)
	cdMust(t, err, "list reports")
	if len(reps) != 1 || reps[0].Checks[0].Name != "db" {
		t.Fatalf("an older report must not overwrite: %+v", reps)
	}
	o, err := s.OpsStatus(ctx, cdTestOrg)
	cdMust(t, err, "ops status")
	if len(o.Releases) != 27 || o.Spend.TodayUSD < 1.25 || len(o.Pipeline) == 0 {
		t.Fatalf("ops status: releases=%d spend=%v pipeline=%v", len(o.Releases), o.Spend.TodayUSD, o.Pipeline)
	}
}
