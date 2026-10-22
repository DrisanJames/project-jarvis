package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestInboxScoreOfferFromProof(t *testing.T) {
	cases := []struct{ name, key, want string }{
		{"Liberty Mutual Savings Party", "lmsp", "Liberty Mutual"},
		{"Mad - Liberty Mutual General Bring Home Savings", "mlmgbhs", "Liberty Mutual"},
		{"New York Life Insurance CPL v1", "NYLCPLv1", "New York Life"},
		{"AEP - eHealth CPC - EXCLUSIVE - Advisor v1", "aepehealthcpcadvisorv1", "AEP eHealth"},
		{"eHealth CPC - EXCLUSIVE - Switchers v3", "ehealthcpcswitchersv3", "eHealth"},
		{"Insurify Home v1", "Insurhomev1", "Insurify Home"},
		{"Insurify v2", "Insurv2", "Insurify Auto"},
		{"Internal - West Capital HELOC - v6b (one lender's no)", "iwchelocv6b", "WCL HELOC"},
		{"Healthcare.com O65 v2", "HealthcareO65v2", "Healthcare.com"},
		{"Metal Roofing v11", "mrv11", "Metal Roofing"},
		{"Sams Club v1 oct", "samsclubv1oct", "Sam's Club"},
		{"Optima Tax CPA** (1054) - v3", "otcpav3", "Optima Tax"},
		{"Brand New Thing - v2", "bntv2", "Brand New Thing"},
		{"Brand New Thing v3 (personalized)", "bnt3", "Brand New Thing"},
		{"Another Offer CPL", "ao1", "Another Offer CPL"},
	}
	for _, c := range cases {
		if got := inboxScoreOfferFromProof(c.name, c.key); got != c.want {
			t.Errorf("offer(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	if inboxScoreCatalogKind("Newsletter - QF - Placement Test", "nlqfv1") != "newsletter" ||
		inboxScoreCatalogKind("nlv1", "Newsletter Placement Test") != "newsletter" ||
		inboxScoreCatalogKind("Testing Placement", "tpv1") != "newsletter" ||
		inboxScoreCatalogKind("Liberty Mutual Savings Party", "lmsp") != "offer" {
		t.Error("catalog kind classification wrong")
	}
}

func TestInboxScoreVariantVerdict(t *testing.T) {
	isps := []string{"a", "b"}
	pass := &inboxScoreCatalogCell{Verdict: "PASS"}
	fail := &inboxScoreCatalogCell{Verdict: "FAIL"}
	stale := &inboxScoreCatalogCell{Verdict: "PASS", Stale: true}
	if v := inboxScoreVariantVerdict(map[string]*inboxScoreCatalogCell{}, isps); v != "UNSCORED" {
		t.Errorf("empty = %s", v)
	}
	if v := inboxScoreVariantVerdict(map[string]*inboxScoreCatalogCell{"a": pass}, isps); v != "PARTIAL" {
		t.Errorf("one lane = %s", v)
	}
	if v := inboxScoreVariantVerdict(map[string]*inboxScoreCatalogCell{"a": pass, "b": pass}, isps); v != "PASS" {
		t.Errorf("all pass = %s", v)
	}
	if v := inboxScoreVariantVerdict(map[string]*inboxScoreCatalogCell{"a": pass, "b": fail}, isps); v != "FAIL" {
		t.Errorf("one fail = %s", v)
	}
	if v := inboxScoreVariantVerdict(map[string]*inboxScoreCatalogCell{"a": stale, "b": stale}, isps); v != "UNSCORED" {
		t.Errorf("stale only = %s", v)
	}
}

// The catalog groups proofs by offer name, keeps every variant (scored or not),
// marks a changed subject stale, and never keys on the sending domain.
func TestInboxScoreCatalog_Tree(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := &InboxScoreService{db: db, now: func() time.Time { return time.Date(2026, 10, 22, 22, 0, 0, 0, time.UTC) }}
	p1, p2 := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_offer_proofs")).WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "offer_key", "approval_status", "is_active", "variants", "from_names", "updated_at"}).
			AddRow(p1, "Liberty Mutual Savings Party", "lmsp", "approved", true,
				`[{"subject":"$700 in savings","preheader":"p1"},{"subject":"Second line","preheader":"p2"}]`, `["Liberty Mutual"]`, time.Now()).
			AddRow(p2, "Liberty Mutual Race Track", "lmrt", "approved", true, `[]`, `[]`, time.Now()).
			AddRow("33333333-3333-3333-3333-333333333333", "Newsletter - QF - Placement Test", "nlqfv1", "pending", true, `[]`, `[]`, time.Now()))
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_inbox_score_catalog")).WillReturnRows(
		sqlmock.NewRows([]string{"proof_id", "variant_index", "subject", "preheader", "isp", "source", "sa_score", "sa_rules", "defender_score",
			"defender_symbols", "folder", "auth", "reasons", "verdict", "sending_domain_used", "scored_at"}).
			AddRow(p1, 0, "$700 in savings", "p1", "gmail", "gate", 0.0, "[]", 0.51, "[]", "inbox", `{"dmarc":"pass"}`, "[]", "PASS", "em.businessweeklypro.com", time.Now()).
			AddRow(p1, 0, "$700 in savings", "p1", "apple", "gate", 2.1, `[{"rule":"SUBJ_DOLLARS","score":0.1}]`, 3.0, "[]", "spam", `{}`, "[]", "FAIL", "em.businessweeklypro.com", time.Now()).
			AddRow(p1, 1, "OLD second line", "p2", "gmail", "gate", 0.0, "[]", 0.2, "[]", "inbox", `{}`, "[]", "PASS", "em.casainsure.com", time.Now()))
	req := httptest.NewRequest(http.MethodGet, "/api/mailing/inbox-score/catalog", nil)
	req.Header.Set("X-Organization-ID", SingleTenantFallbackOrgID)
	rr := httptest.NewRecorder()
	svc.HandleCatalog(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Offers []inboxScoreCatalogOffer `json:"offers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Offers) != 1 || out.Offers[0].Offer != "Liberty Mutual" {
		t.Fatalf("offers = %+v (newsletter proof must be hidden, both Liberty proofs under one card)", out.Offers)
	}
	o := out.Offers[0]
	if len(o.Creatives) != 2 || o.Variants != 3 || o.Scored != 1 || o.Failed != 1 || o.Passed != 0 {
		t.Fatalf("card counts: creatives=%d variants=%d scored=%d passed=%d failed=%d", len(o.Creatives), o.Variants, o.Scored, o.Passed, o.Failed)
	}
	var sp inboxScoreCatalogCreative
	for _, c := range o.Creatives {
		if c.ProofID == p1 {
			sp = c
		}
	}
	if sp.Variants[0].Verdict != "FAIL" || sp.Variants[0].Cells["apple"].Folder != "spam" {
		t.Errorf("variant 0 = %s %+v", sp.Variants[0].Verdict, sp.Variants[0].Cells["apple"])
	}
	if !sp.Variants[1].Cells["gmail"].Stale || sp.Variants[1].Verdict != "UNSCORED" {
		t.Errorf("changed subject must be stale + unscored: %+v %s", sp.Variants[1].Cells["gmail"], sp.Variants[1].Verdict)
	}
	for _, c := range o.Creatives {
		if c.ProofID == p2 && (len(c.Variants) != 1 || c.Variants[0].Index != -1) {
			t.Errorf("proof without variants must carry one index -1 row: %+v", c.Variants)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
