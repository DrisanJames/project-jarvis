//go:build integration

// Content Desk CONTRACT goldens (2026-09-11). The backend is the single source
// of truth for the JSON the portal tab and the static-site publisher read.
// This test builds realistic responses through the REAL handlers on REAL
// Postgres (local apex-postgres ONLY — never prod), normalizes the volatile
// values (uuids, hashes, timestamps) and compares them with the goldens in
// internal/contentdesk/testdata/contract/. The same files are mirrored into
// the portal and publisher test fixtures (scripts/check-content-desk-contract.sh).
//
// RUN:     go test -tags integration -run ContentDeskContract ./cmd/server/ -v
// UPDATE:  CONTENT_DESK_CONTRACT_UPDATE=1 go test -tags integration -run ContentDeskContract ./cmd/server/
//          then scripts/check-content-desk-contract.sh --sync
//
// Creates scratch database content_desk_contract_test and drops it at the end.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/api"
	"github.com/ignite/sparkpost-monitor/internal/contentdesk"
	"github.com/ignite/sparkpost-monitor/internal/tracking"
	_ "github.com/lib/pq"
)

const (
	contractScratchDSN = "postgres://apex_user:apex_password@localhost:5432/content_desk_contract_test?sslmode=disable"
	contractGoldenDir  = "../../internal/contentdesk/testdata/contract"
	contractAdminKey   = "contract-admin-key"
)

// cdContractLLM drafts one block of EVERY allowed type and judges one claim
// sentence overstated (with a lost qualifier) plus one usefulness flag, so the
// goldens exercise every renderer and every judgment shape.
type cdContractLLM struct{}

func (cdContractLLM) Generate(_ context.Context, req contentdesk.GenerateRequest) (*contentdesk.GenerateResult, error) {
	var out string
	switch {
	case req.ToolName == "submit_research":
		out = `{"claims":[
		 {"text":"The 2026 IRA contribution limit is $7,000.","type":"sourced_fact",
		  "source_url":"https://www.irs.gov/retirement-plans/ira-contribution-limits","passage":"The annual contribution limit for 2026 is $7,000 for savers under age 50.",
		  "context":"IRS retirement plans page, contribution limits table","published_at":"2025-11-13","effective_at":"2026-01-01","jurisdiction":"US federal","population":"savers under 50",
		  "conditions":"earned income at least equal to the contribution","status":"supported","key":true,"claim_key":"ira_limit_2026",
		  "question":"What is the 2026 IRA contribution limit for savers under 50?","answer":"7000","calc":null},
		 {"text":"Some states add their own rules.","type":"interpretation","source_url":"","passage":"","context":"","published_at":"",
		  "effective_at":"","jurisdiction":"","population":"","conditions":"","status":"insufficient_evidence","key":false,"claim_key":"",
		  "question":"","answer":"","calc":null}]}`
	case req.ToolName == "submit_rederivation":
		out = `{"answers":[{"question_index":0,"text":"The limit is $7,000.","answer":"$7,000",
		  "source_url":"https://www.irs.gov/newsroom/ira-limit","passage":"$7,000 for 2026","context":"","published_at":"",
		  "effective_at":"","jurisdiction":"","population":"","conditions":"","status":"supported"}]}`
	case req.Tier == contentdesk.TierWrite:
		m := cdClaimRE.FindStringSubmatch(req.Prompt)
		if m == nil {
			return nil, fmt.Errorf("draft prompt carried no supported claim")
		}
		ref := func(block string, idx int) string {
			return fmt.Sprintf(`{"block_id":%q,"sentence_idx":%d,"claim_id":%q,"version":%s}`, block, idx, m[1], m[2])
		}
		out = `{"blocks":[
		 {"id":"b1","type":"lede","heading":"","text":"Most savers can put $7,000 into an IRA for 2026. The limit is shared across all of your IRAs.","items":[],"rows":[],"calc":null,"caption":""},
		 {"id":"b2","type":"section","heading":"Who the limit applies to","text":"The limit applies to people with earned income. You cannot contribute more than you earned.","items":[],"rows":[],"calc":null,"caption":""},
		 {"id":"b3","type":"key_takeaways","heading":"","text":"","items":["Check your earned income first.","The limit is shared across traditional and Roth IRAs."],"rows":[],"calc":null,"caption":""},
		 {"id":"b4","type":"worked_example","heading":"Splitting the limit across two IRAs","text":"Suppose you fund a traditional IRA and a Roth IRA in the same year.","items":[],"rows":[],
		  "calc":{"inputs":[{"name":"traditional","value":4000},{"name":"roth","value":3000}],"formula":"traditional + roth","result":7000},"caption":"Together the two contributions stay within the limit."},
		 {"id":"b5","type":"document_anatomy","heading":"Where contributions are reported","text":"Your IRA custodian reports contributions on Form 5498.","items":["Box 1 shows traditional IRA contributions.","Box 10 shows Roth IRA contributions."],"rows":[],"calc":null,"caption":""},
		 {"id":"b6","type":"stat","heading":"$7,000","text":"The 2026 IRA contribution limit.","items":[],"rows":[],"calc":null,"caption":"Savers aged 50 and over can add a catch-up contribution."},
		 {"id":"b7","type":"comparison_table","heading":"Traditional and Roth IRAs","text":"","items":[],
		  "rows":[["Feature","Traditional","Roth"],["Contributions","Often deductible","Not deductible"],["Qualified withdrawals","Taxed as income","Tax-free"]],"calc":null,"caption":"Both accounts count toward the same limit."},
		 {"id":"b8","type":"steps","heading":"How to stay under the limit","text":"","items":["Add up every IRA contribution for the year.","Compare the total with the limit.","Withdraw any excess before your tax deadline."],"rows":[],"calc":null,"caption":""},
		 {"id":"b9","type":"faq","heading":"Common questions","text":"","items":["Can I contribute to a traditional and a Roth IRA in the same year?","Yes, but the combined total cannot exceed the limit.","Does a workplace plan count toward the IRA limit?","No, workplace plans have their own separate limit."],"rows":[],"calc":null,"caption":""},
		 {"id":"b10","type":"pull_quote","heading":"","text":"The limit applies per person, not per account.","items":[],"rows":[],"calc":null,"caption":"IRS contribution guidance"},
		 {"id":"b11","type":"callout","heading":"Check before you contribute","text":"Excess contributions are taxed every year until you remove them.","items":[],"rows":[],"calc":null,"caption":""}],
		 "claim_refs":[` + ref("b1", 0) + `,` + ref("b6", 1) + `,` + ref("b10", 0) + `]}`
	case req.Tier == contentdesk.TierLight:
		m := cdClaimRE.FindStringSubmatch(req.Prompt)
		if m == nil {
			return nil, fmt.Errorf("package prompt carried no supported claim")
		}
		out = `{"title":"How the 2026 IRA contribution limit works","excerpt":"Most savers can contribute $7,000.",
		 "meta_title":"IRA contribution limit 2026","meta_description":"What the 2026 IRA contribution limit is and who it applies to.",
		 "subjects":["Your 2026 IRA limit, explained"],"preheaders":["Who it applies to and how to stay under it"],
		 "claim_refs":[{"block_id":"excerpt","sentence_idx":0,"claim_id":"` + m[1] + `","version":` + m[2] + `}]}`
	case req.Tier == contentdesk.TierJudge:
		n := len(cdRefRE.FindAllString(req.Prompt, -1))
		items := make([]string, n)
		for i := range items {
			if i == 1 {
				items[i] = `{"ref_index":1,"verdict":"overstated","lost_qualifier":"for savers under 50","note":"the sentence drops the age scope of the passage"}`
				continue
			}
			items[i] = fmt.Sprintf(`{"ref_index":%d,"verdict":"supported","lost_qualifier":"","note":""}`, i)
		}
		out = `{"items":[` + strings.Join(items, ",") + `],"flags":[{"kind":"low_usefulness","block_id":"b5","sentence_idx":1,
		 "note":"names the box without telling the reader what to check"}]}`
	default:
		return nil, fmt.Errorf("unexpected model request tier=%v tool=%q", req.Tier, req.ToolName)
	}
	return &contentdesk.GenerateResult{JSON: json.RawMessage(out), Usage: contentdesk.Usage{Model: "fake-model", InputTokens: 100, OutputTokens: 50, USD: 0.01}}, nil
}

type contractClient struct {
	t *testing.T
	h http.Handler
}

func (c contractClient) do(method, path string, body any, admin bool, want int) []byte {
	c.t.Helper()
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("X-Organization-ID", cdTestOrg)
	req.Header.Set("Content-Type", "application/json")
	if admin {
		req.Header.Set("X-Admin-Key", contractAdminKey)
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	if rec.Code != want {
		c.t.Fatalf("%s %s: want %d, got %d %s", method, path, want, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func TestContentDeskContractGoldens(t *testing.T) {
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
	_, _ = admin.Exec(`DROP DATABASE IF EXISTS content_desk_contract_test WITH (FORCE)`)
	cdMust(t, func() error { _, e := admin.Exec(`CREATE DATABASE content_desk_contract_test`); return e }(), "create scratch db")
	defer admin.Exec(`DROP DATABASE IF EXISTS content_desk_contract_test WITH (FORCE)`)

	db, err := sql.Open("postgres", req118DSN("CONTENT_DESK_CONTRACT_DSN", contractScratchDSN))
	cdMust(t, err, "open scratch")
	defer db.Close()
	for _, m := range contentDeskMigrations {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
	}

	// Deterministic process state for ops-status.
	t.Setenv(contentdesk.EnvEnabled, "1")
	t.Setenv(contentdesk.EnvDailyUSD, "25")
	t.Setenv(contentdesk.EnvModelWrite, "model-write")
	t.Setenv(contentdesk.EnvModelJudge, "model-judge")
	t.Setenv(contentdesk.EnvModelLight, "model-light")
	t.Setenv("PREFERENCES_MODE", "shadow")
	t.Setenv(tracking.SigModeEnv, "off")
	t.Setenv(tracking.SigKeyEnv, "")
	t.Setenv("ADMIN_API_KEY", contractAdminKey)

	svc := api.NewContentDeskService(db)
	r := chi.NewRouter()
	r.Route("/api/mailing", func(mr chi.Router) { svc.RegisterRoutes(mr) })
	svc.RegisterAdminRoutes(r)
	c := contractClient{t: t, h: r}
	const base = "/api/mailing/content-desk"
	ctx := context.Background()
	store := contentdesk.NewStore(db)

	var sites struct {
		Sites []contentdesk.Site `json:"sites"`
	}
	cdMust(t, json.Unmarshal(c.do("GET", base+"/sites", nil, false, 200), &sites), "sites")
	siteID := map[string]string{}
	for _, s := range sites.Sites {
		siteID[s.Domain] = s.ID
	}

	brief := func(in contentdesk.BriefInput) string {
		var out struct {
			Brief contentdesk.Brief `json:"brief"`
		}
		cdMust(t, json.Unmarshal(c.do("POST", base+"/briefs", in, false, 201), &out), "brief")
		return out.Brief.ArticleID
	}
	artA := brief(contentdesk.BriefInput{SiteID: siteID["financialcalculate.com"], Format: "explainer", Category: contentdesk.CatFinance,
		ReaderQuestion: "What is the 2026 IRA contribution limit?", Slug: "ira-contribution-limit", CreatedBy: "contract"})
	artB := brief(contentdesk.BriefInput{SiteID: siteID["learnpersonalloans.com"], Format: "explainer", Category: contentdesk.CatFinance,
		ReaderQuestion: "Does the IRA contribution limit apply to me?", Angle: "who qualifies", Consequential: true,
		Slug: "ira-limit-who-qualifies", CreatedBy: "contract"})
	artC := brief(contentdesk.BriefInput{SiteID: siteID["financialcalculate.com"], Format: "explainer", Category: contentdesk.CatFinance,
		ReaderQuestion: "What happens to an unused HSA balance?", Slug: "unused-hsa-balance", CreatedBy: "contract"})

	p := contentdesk.NewPipeline(store, cdContractLLM{})
	for _, a := range []string{artA, artB} {
		c.do("POST", base+"/articles/"+a+"/run", nil, false, 202)
		cdMust(t, p.Run(ctx, cdTestOrg, a), "pipeline "+a)
	}

	// Approve with every non-supported judgment item and failed S2 code check
	// accepted (what the portal's accept list sends).
	review := func(article, reviewer, decision string, findings []contentdesk.Finding) contentdesk.ReviewOutcome {
		var d contentdesk.ArticleDetail
		cdMust(t, json.Unmarshal(c.do("GET", base+"/articles/"+article, nil, false, 200), &d), "detail")
		if d.Revision == nil {
			t.Fatalf("article %s has no revision after the pipeline (status %s)", article, d.Article.Status)
		}
		accepted := []string{}
		for _, j := range d.Revision.Checks.Judgment {
			if !(j.Kind == "claim" && j.Verdict == "supported") {
				accepted = append(accepted, j.ID)
			}
		}
		for _, ck := range d.Revision.Checks.Code {
			if !ck.Passed {
				if ck.Severity == "S1" {
					t.Fatalf("article %s: S1 code check %s failed: %v", article, ck.Name, ck.Details)
				}
				accepted = append(accepted, "code:"+ck.Name)
			}
		}
		var out struct {
			Outcome contentdesk.ReviewOutcome `json:"outcome"`
		}
		cdMust(t, json.Unmarshal(c.do("POST", base+"/articles/"+article+"/reviews", contentdesk.ReviewInput{
			RevisionHash: d.Revision.RevisionHash, Reviewer: reviewer, Role: "primary", Decision: decision,
			Findings: findings, Minutes: 14, AcceptedIDs: accepted}, false, 201), &out), "review")
		return out.Outcome
	}
	if o := review(artA, "ann", "approve", []contentdesk.Finding{{Severity: "S3", CaughtBy: "human", BlockID: "b2", Text: "Tighten the second sentence."}}); o.Status != contentdesk.StatusApproved {
		t.Fatalf("A must approve: %+v", o)
	}
	if o := review(artB, "ann", "approve", []contentdesk.Finding{
		{Severity: "S2", CaughtBy: "judgment", BlockID: "b6", Text: "The stat drops the under-50 qualifier."},
		{Severity: "S3", CaughtBy: "human", BlockID: "b1", Text: "Lede could name the tax year once."},
	}); o.Status != contentdesk.StatusInReview || o.Awaiting == "" {
		t.Fatalf("consequential B must await a second reviewer: %+v", o)
	}
	c.do("POST", base+"/articles/"+artC+"/withdraw", nil, false, 200)

	golden := map[string][]byte{}
	golden["manifest"] = c.do("GET", base+"/releases/manifest?site=financialcalculate.com", nil, true, 200)
	golden["release_create"] = c.do("POST", base+"/releases", map[string]string{"site_id": siteID["financialcalculate.com"]}, true, 201)
	var rel struct {
		Release contentdesk.Release `json:"release"`
	}
	cdMust(t, json.Unmarshal(golden["release_create"], &rel), "release")
	c.do("POST", base+"/releases/"+rel.Release.ID+"/status", map[string]string{"status": "building"}, true, 200)
	now := time.Now().UTC().Truncate(time.Second)
	c.do("POST", base+"/ops-report", map[string]any{"source": "job_watchdog", "generated_at": now,
		"checks": []map[string]string{{"name": "content_desk_publish", "status": "ok", "detail": "last run OK"}}}, true, 202)
	c.do("POST", base+"/ops-report", map[string]any{"source": "supply_runway", "generated_at": now,
		"checks": []map[string]string{{"name": "newsletter runway", "status": "warn", "detail": "1 consumer under 3 days"}},
		"payload": map[string]any{"consumers": []map[string]any{
			{"name": "yahoo_family", "brand": "FC", "eligible": 9, "runway_days": 2.5, "status": "warn"},
			{"name": "legacy_newsletter", "brand": "LP", "eligible": 40, "runway_days": 12, "status": "ok"}}}}, true, 202)

	golden["sites"] = c.do("GET", base+"/sites", nil, false, 200)
	golden["briefs"] = c.do("GET", base+"/briefs", nil, false, 200)
	golden["list"] = c.do("GET", base+"/articles", nil, false, 200)
	golden["detail"] = c.do("GET", base+"/articles/"+artB, nil, false, 200)
	golden["detail_withdrawn"] = c.do("GET", base+"/articles/"+artC, nil, false, 200)
	golden["detail_released"] = c.do("GET", base+"/articles/"+artA, nil, false, 200) // the manifest's article (publisher)
	golden["ops_status"] = c.do("GET", base+"/ops-status", nil, false, 200)

	// One normalizer across every file, in a fixed order, so an id maps to the
	// same placeholder everywhere (manifest article_id == list id == detail id).
	n := newContractNorm()
	order := []string{"sites", "briefs", "list", "detail", "detail_withdrawn", "detail_released", "manifest", "release_create", "ops_status"}
	update := os.Getenv("CONTENT_DESK_CONTRACT_UPDATE") == "1"
	if update {
		cdMust(t, os.MkdirAll(contractGoldenDir, 0o755), "mkdir goldens")
	}
	for _, name := range order {
		got, err := n.normalize(golden[name])
		cdMust(t, err, "normalize "+name)
		path := filepath.Join(contractGoldenDir, name+".json")
		if update {
			cdMust(t, os.WriteFile(path, got, 0o644), "write "+path)
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v (run with CONTENT_DESK_CONTRACT_UPDATE=1)", path, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s drifted from the backend's real response: %s\n(re-generate with CONTENT_DESK_CONTRACT_UPDATE=1, then scripts/check-content-desk-contract.sh --sync)",
				path, firstDiff(want, got))
		}
	}
}

// ── normalization ────────────────────────────────────────────────────────

var (
	contractUUIDRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	contractHexRE  = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
	contractTSRE   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	contractDayRE  = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
)

const contractTS = "2026-09-11T12:00:00Z"

type contractNorm struct {
	ids, hashes map[string]string
	today       map[string]bool
}

func newContractNorm() *contractNorm {
	den, _ := time.LoadLocation("America/Denver")
	now := time.Now()
	return &contractNorm{ids: map[string]string{}, hashes: map[string]string{},
		today: map[string]bool{now.UTC().Format("2006-01-02"): true, now.In(den).Format("2006-01-02"): true}}
}

func (n *contractNorm) normalize(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	v = n.walk("", v)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (n *contractNorm) walk(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			x[k] = n.walk(k, x[k])
		}
		return x
	case []any:
		if key == "claims" { // ClaimsByIDs orders by random claim uuid — order is not contract
			sort.SliceStable(x, func(i, j int) bool { return claimSortKey(x[i]) < claimSortKey(x[j]) })
		}
		for i := range x {
			x[i] = n.walk(key, x[i])
		}
		return x
	case json.Number:
		if key == "oldest_age_seconds" {
			return json.Number("5400")
		}
		return x
	case string:
		switch key {
		case "day":
			return "2026-09-11"
		case "detail":
			x = contractDayRE.ReplaceAllStringFunc(x, func(d string) string {
				if n.today[d] {
					return "2026-09-11"
				}
				return d
			})
		}
		x = contractTSRE.ReplaceAllString(x, contractTS)
		x = contractUUIDRE.ReplaceAllStringFunc(x, func(u string) string {
			if u == cdTestOrg {
				return u
			}
			if _, ok := n.ids[u]; !ok {
				n.ids[u] = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(n.ids)+1)
			}
			return n.ids[u]
		})
		return contractHexRE.ReplaceAllStringFunc(x, func(h string) string {
			if _, ok := n.hashes[h]; !ok {
				sum := sha256.Sum256([]byte(fmt.Sprintf("content-desk-contract-hash-%d", len(n.hashes)+1)))
				n.hashes[h] = hex.EncodeToString(sum[:])
			}
			return n.hashes[h]
		})
	}
	return v
}

func claimSortKey(v any) string {
	m, _ := v.(map[string]any)
	return fmt.Sprintf("%v|%v|%v", m["text"], m["derivation"], m["version"])
}

func firstDiff(want, got []byte) string {
	w, g := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return fmt.Sprintf("line %d:\n  golden:  %s\n  backend: %s", i+1, a, b)
		}
	}
	return "(no line diff)"
}
