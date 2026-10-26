package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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
		sqlmock.NewRows([]string{"id", "name", "offer_key", "approval_status", "is_active", "variants", "from_names", "updated_at", "md5"}).
			AddRow(p1, "Liberty Mutual Savings Party", "lmsp", "approved", true,
				`[{"subject":"$700 in savings","preheader":"p1"},{"subject":"Second line","preheader":"p2"}]`, `["Liberty Mutual"]`, time.Now(), "hash-sp").
			AddRow(p2, "Liberty Mutual Race Track", "lmrt", "approved", true, `[]`, `[]`, time.Now(), "hash-rt").
			AddRow("33333333-3333-3333-3333-333333333333", "Newsletter - QF - Placement Test", "nlqfv1", "pending", true, `[]`, `[]`, time.Now(), "hash-nl"))
	syms := `[{"symbol":"SUBJECT_HAS_CURRENCY","score":1.0},{"symbol":"ZERO_FONT","score":0.2},{"symbol":"FORGED_SENDER","score":0.3}]`
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_inbox_score_catalog")).WillReturnRows(
		sqlmock.NewRows([]string{"proof_id", "variant_index", "subject", "preheader", "isp", "source", "sa_score", "sa_rules", "defender_score",
			"defender_symbols", "folder", "auth", "reasons", "verdict", "sending_domain_used", "scored_at", "links", "link_check", "prev_defender_score", "prev_sa_score", "prev_scored_at"}).
			AddRow(p1, 0, "$700 in savings", "p1", "gmail", "gate", 0.0, "[]", 0.51, syms, "inbox", `{"dmarc":"pass"}`, "[]", "PASS", "em.businessweeklypro.com", time.Now(), `{"listed_hosts":[]}`, "degraded", 1.9, 0.3, time.Now().Add(-time.Hour)).
			AddRow(p1, 0, "$700 in savings", "p1", "apple", "gate", 2.1, `[{"rule":"SUBJ_DOLLARS","score":0.1}]`, 3.0, syms, "spam", `{}`, "[]", "FAIL", "em.businessweeklypro.com", time.Now(), `{}`, "ok", nil, nil, nil).
			AddRow(p1, 1, "OLD second line", "p2", "gmail", "gate", 0.0, "[]", 0.2, "[]", "inbox", `{}`, "[]", "PASS", "em.casainsure.com", time.Now(), `{}`, "ok", nil, nil, nil).
			AddRow(p2, -1, "", "", "gmail", "gate", -0.1, "[]", -0.49, `[{"symbol":"ZERO_FONT","score":0.2}]`, "inbox", `{}`, "[]", "PASS", "em.businessweeklypro.com", time.Now(), `{}`, "ok", nil, nil, nil))
	// The board: one cell carrying the Savings Party bytes, one Liberty cell with bytes no proof has
	// (drifted), one newsletter cell (ignored), one cell of an offer the catalogue does not know (ignored).
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_campaigns")).WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "offer_key", "status", "scheduled_at", "total_recipients", "subject", "md5"}).
			AddRow("c1", "10262026 - BWP - Liberty", "liberty-mutual", "scheduled", time.Date(2026, 10, 26, 10, 1, 0, 0, time.UTC), 1000, "$700 in savings", "hash-sp").
			AddRow("c2", "10262026 - DB - Liberty", "liberty-mutual", "scheduled", time.Date(2026, 10, 26, 10, 1, 0, 0, time.UTC), 2000, "A subject nobody catalogued", "hash-OLD").
			AddRow("c3", "10262026 - DB - NL-GM-NEWSLETTER", "", "scheduled", time.Date(2026, 10, 26, 10, 1, 0, 0, time.UTC), 50, "News", "hash-x").
			AddRow("c4", "10262026 - DB - SomethingElse", "", "scheduled", time.Date(2026, 10, 26, 10, 1, 0, 0, time.UTC), 50, "x", "hash-y").
			AddRow("c5", "10262026 - NX-aa.discountblog.com - OFFER-ENG-Liberty", "liberty-mutual", "scheduled", time.Date(2026, 10, 26, 10, 1, 0, 0, time.UTC), 300, "$700 in savings", "hash-nx-1"))
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
	if len(o.Creatives) != 2 || o.Variants != 3 || o.Scored != 2 || o.Failed != 2 || o.Passed != 0 { // FAIL + PARTIAL (Race Track scored on one lane)
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
	// Insight: link health, previous score, drivers with scope, extremes, board parity.
	g := sp.Variants[0].Cells["gmail"]
	if g.LinkCheck != "degraded" || g.PrevDefenderScore == nil || *g.PrevDefenderScore != 1.9 || g.PrevScoredAt == "" {
		t.Errorf("gmail cell link/prev: %+v", g)
	}
	if o.LinkChecks["degraded"] != 1 || o.LinkChecks["ok"] != 2 { // the stale cell does not count
		t.Errorf("link checks = %v", o.LinkChecks)
	}
	drv := map[string]inboxScoreDriver{}
	for _, d := range o.Drivers {
		drv[d.Kind+":"+d.Name] = d
	}
	// Two scored lines in the offer (Savings Party v0; v1 is stale; Race Track -1): SUBJECT_HAS_CURRENCY
	// fired on 1 of 2 → copy; ZERO_FONT on 2 of 2 → body; FORGED_SENDER is transport whatever its reach.
	if d := drv["defender:SUBJECT_HAS_CURRENCY"]; d.Scope != "copy" || d.Variants != 1 || d.Score != 1.0 {
		t.Errorf("currency driver = %+v", d)
	}
	if d := drv["defender:ZERO_FONT"]; d.Scope != "body" || d.Variants != 2 || d.Of != 2 {
		t.Errorf("zero_font driver = %+v", d)
	}
	if d := drv["defender:FORGED_SENDER"]; d.Scope != "transport" {
		t.Errorf("forged_sender driver = %+v", d)
	}
	if d := drv["presend:SUBJ_DOLLARS"]; d.Kind != "presend" || d.Score != 0.1 {
		t.Errorf("presend driver = %+v", d)
	}
	if o.Worst == nil || o.Worst.Defender != 3.0 || o.Worst.Subject != "$700 in savings" || o.Best == nil || o.Best.Defender != -0.49 || o.Best.Index != -1 {
		t.Errorf("extremes worst=%+v best=%+v", o.Worst, o.Best)
	}
	if o.Live.Mutated != 1 || o.Live.MutatedRecipients != 300 {
		t.Errorf("nx cell must count as mutated, not drifted: %+v", o.Live)
	}
	if o.Live.Cells != 3 || o.Live.Matching != 1 || o.Live.Drifted != 1 || o.Live.Recipients != 3300 || len(o.Live.Drift) != 1 ||
		o.Live.Drift[0].Name != "10262026 - DB - Liberty" || o.Live.Drift[0].Day != "2026-10-26" || o.Live.Drift[0].SubjectCatalogued {
		t.Errorf("board parity = %+v", o.Live)
	}
	for _, c := range o.Creatives {
		if c.ProofID == p1 && (c.LiveCells != 1 || c.LiveRecipients != 1000) {
			t.Errorf("savings party live cells = %d/%d", c.LiveCells, c.LiveRecipients)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The writer refuses a row for a proof the org does not have (it would never show on a card).
func TestInboxScorePostCatalog_UnknownProof(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := &InboxScoreService{db: db, now: time.Now}
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_offer_proofs")).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	body := `{"scores":[{"proof_id":"00000000-0000-0000-0000-000000000000","variant_index":0,"isp":"gmail","verdict":"PASS","folder":"inbox"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/inbox-score/catalog", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", SingleTenantFallbackOrgID)
	rr := httptest.NewRecorder()
	svc.HandlePostCatalog(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown proof_id") {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err) // no BEGIN, no INSERT
	}
}

func TestInboxScoreOfferByLabel_AmbiguousIsNone(t *testing.T) {
	by := map[string]*inboxScoreCatalogOffer{
		"Insurify Home":    {Offer: "Insurify Home"},
		"Insurify Auto":    {Offer: "Insurify Auto"},
		"Insurify Renters": {Offer: "Insurify Renters"},
		"Liberty Mutual":   {Offer: "Liberty Mutual"},
	}
	if o := inboxScoreOfferByLabel("Insurify", by); o != nil {
		t.Errorf("bare Insurify matched %q; two candidates must resolve to none", o.Offer)
	}
	if o := inboxScoreOfferByLabel("Insurify Auto", by); o == nil || o.Offer != "Insurify Auto" {
		t.Errorf("exact label must resolve")
	}
	if o := inboxScoreOfferByLabel("Liberty", by); o == nil || o.Offer != "Liberty Mutual" {
		t.Errorf("unique substring must resolve")
	}
	if !reMutatingBuilder.MatchString("10252026 - NX-aa.discountblog.com - OFFER-ENG-Insurify") || reMutatingBuilder.MatchString("10262026 - DB - Liberty-R") {
		t.Errorf("mutating-builder regex")
	}
}
