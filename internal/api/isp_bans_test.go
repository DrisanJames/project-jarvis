package api

// REQ-083 regression guards. The defect these pin: a board cell for a
// gmail-banned brand carries an ISP plan with no quota key, the planner reads
// quota<=0 as UNLIMITED, and gmail materializes + sends anyway (3,416 sends on
// 2026-09-01 across WFY/RB/RRU/TOT/CP/LPL/YIH/CI). The ban must be applied by
// normalizePMTACampaignInput itself, because that is the one function every
// deploy path passes through.

import (
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// installISPBanRows points the package ban registry at a mock DB returning
// the given (org, brand_code, isp) rows. The returned func restores the
// unwired (inert) state.
func installISPBanRows(t *testing.T, triples [][3]string, qerr error) func() {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	mock.MatchExpectationsInOrder(false)
	rows := sqlmock.NewRows([]string{"organization_id", "brand_code", "isp"})
	for _, tr := range triples {
		rows.AddRow(tr[0], tr[1], tr[2])
	}
	q := mock.ExpectQuery("FROM mailing_isp_bans")
	if qerr != nil {
		q.WillReturnError(qerr)
	} else {
		q.WillReturnRows(rows)
	}

	ispBans.mu.Lock()
	ispBans.db = db
	ispBans.cached = nil
	ispBans.fetched = time.Time{}
	ispBans.mu.Unlock()

	return func() {
		ispBans.mu.Lock()
		ispBans.db = nil
		ispBans.cached = nil
		ispBans.fetched = time.Time{}
		ispBans.mu.Unlock()
		db.Close()
	}
}

const ispBanTestOrg = "00000000-0000-0000-0000-000000000001"

// banTestInput mirrors a live board cell: gmail carries NO quota (the blob
// shape that reads as UNLIMITED), alongside two other ISPs.
func banTestInput(sendingDomain string) engine.PMTACampaignInput {
	startAt := time.Now().Add(2 * time.Hour).Truncate(time.Minute)
	return engine.PMTACampaignInput{
		Name:          "09022026 - WFY - Destiny",
		SendingDomain: sendingDomain,
		TargetISPs:    []engine.ISP{"gmail", "yahoo", "microsoft"},
		ISPQuotas: []engine.ISPQuota{
			{ISP: "gmail", Volume: 0},
			{ISP: "yahoo", Volume: 500},
			{ISP: "microsoft", Volume: 300},
		},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", TimeSpans: []engine.PMTATimeSpanInput{{StartAt: &startAt}}},
			{ISP: "yahoo", Quota: 500, TimeSpans: []engine.PMTATimeSpanInput{{StartAt: &startAt}}},
			{ISP: "microsoft", Quota: 300, TimeSpans: []engine.PMTATimeSpanInput{{StartAt: &startAt}}},
		},
		Variants: []engine.ContentVariant{{VariantName: "A", Subject: "s", HTMLContent: "<p>x</p>"}},
	}
}

func planISPs(n pmtaNormalizedCampaign) map[string]bool {
	out := map[string]bool{}
	for _, p := range n.Plans {
		out[p.ISP] = true
	}
	return out
}

func TestISPBanDropsPlan(t *testing.T) {
	// warrantyforyou.com = brandident code "wf" (the board token is WFY —
	// seeding the token instead of the code is the way this gate goes inert).
	restore := installISPBanRows(t, [][3]string{{ispBanTestOrg, "wf", "gmail"}}, nil)
	defer restore()

	// 1. Banned brand: the gmail plan, quota and target are all gone.
	got, err := normalizePMTACampaignInput(banTestInput("m.warrantyforyou.com"))
	if err != nil {
		t.Fatalf("normalize banned brand: %v", err)
	}
	isps := planISPs(got)
	if isps["gmail"] {
		t.Errorf("gmail plan survived for a banned brand: %v", isps)
	}
	for _, want := range []string{"yahoo", "microsoft"} {
		if !isps[want] {
			t.Errorf("ban removed an unrelated ISP %q: %v", want, isps)
		}
	}
	for _, ti := range got.TargetISPs {
		if string(ti) == "gmail" {
			t.Errorf("gmail survived in TargetISPs: %v", got.TargetISPs)
		}
	}

	// 2. Non-banned brand on the SAME table: gmail is untouched.
	got, err = normalizePMTACampaignInput(banTestInput("m.discountblog.com"))
	if err != nil {
		t.Fatalf("normalize non-banned brand: %v", err)
	}
	if !planISPs(got)["gmail"] {
		t.Errorf("gmail plan dropped for a NON-banned brand: %v", planISPs(got))
	}
}

func TestISPBanUnknownISPNameRefuses(t *testing.T) {
	// A typo'd class is a policy violation that LOOKS applied — it must
	// refuse at load, not silently no-op (applyCellISPControls precedent).
	restore := installISPBanRows(t, [][3]string{{ispBanTestOrg, "wf", "gmial"}}, nil)
	defer restore()

	if _, err := normalizePMTACampaignInput(banTestInput("m.warrantyforyou.com")); err == nil {
		t.Fatal("expected an error for an unknown ISP class in mailing_isp_bans, got nil")
	}
}

func TestISPBanLoadErrorRefusesDeploy(t *testing.T) {
	// Fail CLOSED: an unreadable ban table must stop the deploy. Failing open
	// is exactly the leak this requirement closes.
	restore := installISPBanRows(t, nil, errors.New("relation \"mailing_isp_bans\" does not exist"))
	defer restore()

	if _, err := normalizePMTACampaignInput(banTestInput("m.warrantyforyou.com")); err == nil {
		t.Fatal("expected the deploy to be refused when the ban table cannot be read")
	}
}

func TestISPBanUnwiredRegistryIsInert(t *testing.T) {
	// No DB wired (tests, non-server binaries): behavior identical to before.
	ispBans.mu.Lock()
	ispBans.db = nil
	ispBans.cached = nil
	ispBans.fetched = time.Time{}
	ispBans.mu.Unlock()

	got, err := normalizePMTACampaignInput(banTestInput("m.warrantyforyou.com"))
	if err != nil {
		t.Fatalf("normalize with an unwired registry: %v", err)
	}
	if !planISPs(got)["gmail"] {
		t.Errorf("unwired registry must not drop anything: %v", planISPs(got))
	}
}

func TestISPBanBrandCodeResolution(t *testing.T) {
	// The mapping is brand.Root + brandident.CodeForApex — never a local
	// brand map. All 8 banned board tokens must resolve to their code, in the
	// sending-domain form the LIVE board actually uses: verified 2026-09-01
	// against prod mailing_campaign_isp_plans, every banned brand's plans
	// carry sending_domain = "m.<apex>" (CI m.casainsure.com, CP
	// m.consumerpro.net, LPL m.learnpersonalloans.com, RB m.ratesbazar.com,
	// RRU m.refinanceratesusa.com, TOT m.thingoftheday.org, WFY
	// m.warrantyforyou.com, YIH m.yourinsurancehub.com) — NOT "em.".
	cases := map[string]string{
		"m.warrantyforyou.com":      "wf",
		"m.ratesbazar.com":          "rb",
		"m.refinanceratesusa.com":   "rr",
		"m.thingoftheday.org":       "tt",
		"m.consumerpro.net":         "cp",
		"m.learnpersonalloans.com":  "lp",
		"m.yourinsurancehub.com":    "yi",
		"m.casainsure.com":          "ci",
		"em.warrantyforyou.com":     "wf",
		"em.ratesbazar.com":         "rb",
		"em.refinanceratesusa.com":  "rr",
		"em.thingoftheday.org":      "tt",
		"em.consumerpro.net":        "cp",
		"em.learnpersonalloans.com": "lp",
		"em.yourinsurancehub.com":   "yi",
		"em.casainsure.com":         "ci",
		"em.discountblog.com":       "db",
		"nosuchdomain.example":      "",
	}
	for domain, want := range cases {
		got, ok := ispBanBrandCode(domain)
		if want == "" {
			if ok {
				t.Errorf("%s: expected no brand code, got %q", domain, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%s: got (%q,%v), want %q", domain, got, ok, want)
		}
	}
}
