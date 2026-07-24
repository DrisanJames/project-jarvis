package api

// REQ-C18 — promote post-conditions verify endpoint.
// Negative paths are the point: each silent-failure class must produce a
// deterministic FAIL (or PENDING for wait-states), never a 200-and-shrug.
// Run: go test ./internal/api/ -run 'VerifyCampaigns' -v

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const vfyOrg = "00000000-0000-0000-0000-000000000001"

func vfyCampaignCols() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "status", "total_recipients", "offer_id", "sending_profile_id"})
}

func vfyPost(t *testing.T, db *sql.DB, body string) pmtaVerifyResponse {
	t.Helper()
	svc := &PMTACampaignService{db: db}
	req := httptest.NewRequest(http.MethodPost, "/pmta-campaign/verify", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", vfyOrg)
	rec := httptest.NewRecorder()
	svc.HandleVerifyCampaigns(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out pmtaVerifyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func checkStatus(t *testing.T, res pmtaVerifyCampaignResult, check, want string) {
	t.Helper()
	for _, c := range res.Checks {
		if c.Check == check {
			if c.Status != want {
				t.Errorf("check %s = %s (%s), want %s", check, c.Status, c.Detail, want)
			}
			return
		}
	}
	t.Errorf("check %s missing from result: %+v", check, res.Checks)
}

// Happy path: everything the 6-item list demands holds → overall PASS.
func TestVerifyCampaigns_AllPostConditionsPass(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	campID := "dddddddd-0000-0000-0000-000000000001"
	segID := "cccccccc-0000-0000-0000-000000000017"
	first := time.Now().Add(1 * time.Hour)
	last := time.Now().Add(7 * time.Hour)

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(vfyCampaignCols().AddRow(campID, "scheduled", 61234, "eeeeeeee-0000-0000-0000-000000000001", "ffffffff-0000-0000-0000-000000000001"))
	mock.ExpectQuery(`FROM mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "planned", "first_at", "last_at"}).
			AddRow(12, 61000, first, last))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).
		WillReturnRows(sqlmock.NewRows([]string{"audience_source_id", "count"}).AddRow(segID, 5000))
	mock.ExpectQuery(`FROM mailing_campaign_isp_plans`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "quota"}).
			AddRow("gmail", 20000).AddRow("microsoft", 41000))

	out := vfyPost(t, db, `{"campaigns":[{"name":"jul25-w1","expect_offer":true,"explicit_quotas":true,"segment_reserves":{"`+segID+`":5000}}]}`)
	if out.Overall != "PASS" {
		t.Fatalf("overall = %s, want PASS: %+v", out.Overall, out.Campaigns[0].Checks)
	}
	res := out.Campaigns[0]
	for _, c := range []string{"by_name", "finalized", "offer_id", "waves", "reserve_fill", "quota_sanity"} {
		checkStatus(t, res, c, "PASS")
	}
	if res.ReserveFill[segID] != 5000 {
		t.Errorf("reserve_fill[%s] = %d, want 5000", segID, res.ReserveFill[segID])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// The silent-failure class: scheduled with 0 recipients → FAIL.
func TestVerifyCampaigns_ZeroRecipientsFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(vfyCampaignCols().AddRow("dddddddd-0000-0000-0000-000000000002", "scheduled", 0, nil, nil))
	mock.ExpectQuery(`FROM mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "planned", "first_at", "last_at"}).AddRow(0, 0, nil, nil))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).
		WillReturnRows(sqlmock.NewRows([]string{"audience_source_id", "count"}))

	out := vfyPost(t, db, `{"campaigns":[{"name":"jul25-w2"}]}`)
	if out.Overall != "FAIL" {
		t.Fatalf("overall = %s, want FAIL", out.Overall)
	}
	checkStatus(t, out.Campaigns[0], "finalized", "FAIL")
	// scheduled + 0 waves is also a materialization failure
	checkStatus(t, out.Campaigns[0], "waves", "FAIL")
}

// failed status (the 0-recipient failed class) → FAIL, named.
func TestVerifyCampaigns_FailedStatusFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(vfyCampaignCols().AddRow("dddddddd-0000-0000-0000-000000000003", "failed", 0, nil, nil))
	mock.ExpectQuery(`FROM mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "planned", "first_at", "last_at"}).AddRow(0, 0, nil, nil))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).
		WillReturnRows(sqlmock.NewRows([]string{"audience_source_id", "count"}))

	out := vfyPost(t, db, `{"campaigns":[{"name":"jul25-w3"}]}`)
	checkStatus(t, out.Campaigns[0], "finalized", "FAIL")
}

// finalizing_audience → PENDING, never FAIL and never a re-deploy signal
// (jul13 lesson: promote WAITS, never re-POSTs).
func TestVerifyCampaigns_FinalizingIsPending(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(vfyCampaignCols().AddRow("dddddddd-0000-0000-0000-000000000004", "finalizing_audience", 0, nil, nil))
	mock.ExpectQuery(`FROM mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "planned", "first_at", "last_at"}).AddRow(0, 0, nil, nil))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).
		WillReturnRows(sqlmock.NewRows([]string{"audience_source_id", "count"}))

	out := vfyPost(t, db, `{"campaigns":[{"name":"jul25-w4"}]}`)
	if out.Overall != "PENDING" {
		t.Fatalf("overall = %s, want PENDING: %+v", out.Overall, out.Campaigns[0].Checks)
	}
	checkStatus(t, out.Campaigns[0], "finalized", "PENDING")
	checkStatus(t, out.Campaigns[0], "waves", "PENDING")
}

// NULL offer_id on an offer wave → FAIL (suppression-gap class).
func TestVerifyCampaigns_NullOfferIDFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	first := time.Now()
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(vfyCampaignCols().AddRow("dddddddd-0000-0000-0000-000000000005", "scheduled", 500, nil, "ffffffff-0000-0000-0000-000000000001"))
	mock.ExpectQuery(`FROM mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "planned", "first_at", "last_at"}).AddRow(1, 500, first, first))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).
		WillReturnRows(sqlmock.NewRows([]string{"audience_source_id", "count"}))

	out := vfyPost(t, db, `{"campaigns":[{"name":"jul25-ofr","expect_offer":true}]}`)
	checkStatus(t, out.Campaigns[0], "offer_id", "FAIL")
	if out.Overall != "FAIL" {
		t.Fatalf("overall = %s, want FAIL", out.Overall)
	}
}

// By-name discipline: missing and colliding names both FAIL loudly.
func TestVerifyCampaigns_ByNameMissingAndColliding(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// missing
	mock.ExpectQuery(`FROM mailing_campaigns`).WillReturnRows(vfyCampaignCols())
	// colliding (two live rows)
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(vfyCampaignCols().
			AddRow("dddddddd-0000-0000-0000-000000000006", "scheduled", 10, nil, nil).
			AddRow("dddddddd-0000-0000-0000-000000000007", "scheduled", 12, nil, nil))

	out := vfyPost(t, db, `{"campaigns":[{"name":"ghost"},{"name":"twins"}]}`)
	if out.Overall != "FAIL" {
		t.Fatalf("overall = %s, want FAIL", out.Overall)
	}
	checkStatus(t, out.Campaigns[0], "by_name", "FAIL")
	checkStatus(t, out.Campaigns[1], "by_name", "FAIL")
	if !strings.Contains(out.Campaigns[1].Checks[0].Detail, "colliding") {
		t.Errorf("collision detail missing: %s", out.Campaigns[1].Checks[0].Detail)
	}
}

// volume:0 in explicit mode is BANNED → FAIL naming the ISP.
func TestVerifyCampaigns_ExplicitQuotaZeroFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	first := time.Now()
	last := first.Add(6 * time.Hour)
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(vfyCampaignCols().AddRow("dddddddd-0000-0000-0000-000000000008", "scheduled", 900, nil, nil))
	mock.ExpectQuery(`FROM mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "planned", "first_at", "last_at"}).AddRow(4, 900, first, last))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).
		WillReturnRows(sqlmock.NewRows([]string{"audience_source_id", "count"}))
	mock.ExpectQuery(`FROM mailing_campaign_isp_plans`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "quota"}).
			AddRow("gmail", 900).AddRow("yahoo", 0))

	out := vfyPost(t, db, `{"campaigns":[{"name":"jul25-w5","explicit_quotas":true}]}`)
	res := out.Campaigns[0]
	checkStatus(t, res, "quota_sanity", "FAIL")
	found := false
	for _, c := range res.Checks {
		if c.Check == "quota_sanity" && strings.Contains(c.Detail, "yahoo") {
			found = true
		}
	}
	if !found {
		t.Errorf("quota_sanity FAIL must name the zero-quota ISP: %+v", res.Checks)
	}
}

// Reserve floor unmet (the fresh-drew-zero class) → FAIL naming the segment.
func TestVerifyCampaigns_ReserveFloorUnmetFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	segID := "cccccccc-0000-0000-0000-000000000017"
	first := time.Now()
	last := first.Add(3 * time.Hour)
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(vfyCampaignCols().AddRow("dddddddd-0000-0000-0000-000000000009", "scheduled", 8000, nil, nil))
	mock.ExpectQuery(`FROM mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "planned", "first_at", "last_at"}).AddRow(3, 8000, first, last))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).
		WillReturnRows(sqlmock.NewRows([]string{"audience_source_id", "count"}).AddRow(segID, 120))

	out := vfyPost(t, db, `{"campaigns":[{"name":"jul25-w6","segment_reserves":{"`+segID+`":5000}}]}`)
	res := out.Campaigns[0]
	checkStatus(t, res, "reserve_fill", "FAIL")
	failDetail := ""
	for _, c := range res.Checks {
		if c.Check == "reserve_fill" {
			failDetail = c.Detail
		}
	}
	if !strings.Contains(failDetail, segID) || !strings.Contains(failDetail, "120/5000") {
		t.Errorf("reserve_fill FAIL must name segment and counts: %s", failDetail)
	}
}

// Waves all at the same instant when >1 → window not spread → FAIL.
func TestVerifyCampaigns_UnspreadWavesFail(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	at := time.Now()
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(vfyCampaignCols().AddRow("dddddddd-0000-0000-0000-000000000010", "scheduled", 4000, nil, nil))
	mock.ExpectQuery(`FROM mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "planned", "first_at", "last_at"}).AddRow(8, 4000, at, at))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).
		WillReturnRows(sqlmock.NewRows([]string{"audience_source_id", "count"}))

	out := vfyPost(t, db, `{"campaigns":[{"name":"jul25-w7"}]}`)
	checkStatus(t, out.Campaigns[0], "waves", "FAIL")
}
