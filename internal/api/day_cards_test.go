package api

// Day Cards + rebuild tests. The rebuild negative paths are the point: a
// missing blob must 400 (never a degraded DB-fallback rebuild), an
// unconfirmed request must write NOTHING, and the confirmed path must cancel
// BEFORE it deploys (proven via the ordered mock + the deploy seam).
// Run: go test ./internal/api/ -run 'DayCards|RebaseTimeSpans' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

const dcOrg = "00000000-0000-0000-0000-000000000001"
const dcCampID = "dddddddd-0000-0000-0000-000000000001"

func dcGet(t *testing.T, svc *PMTACampaignService, url string, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Organization-ID", dcOrg)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func dcPostRebuild(t *testing.T, svc *PMTACampaignService, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/pmta-campaign/day-cards/rebuild", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", dcOrg)
	rec := httptest.NewRecorder()
	svc.HandleDayCardsRebuild(rec, req)
	return rec
}

// dcConfigBlob is a minimal but faithful pmta_config::text with the
// campaign_input the rebuild path requires.
func dcConfigBlob(t *testing.T, name string) string {
	t.Helper()
	sched := time.Date(2026, 8, 21, 7, 1, 0, 0, time.UTC)
	end := sched.Add(8 * time.Hour)
	input := map[string]interface{}{
		"name":           name,
		"sending_domain": "em.discountblog.com",
		"send_mode":      "scheduled",
		"scheduled_at":   sched.Format(time.RFC3339),
		"variants": []map[string]interface{}{
			{"variant_name": "A", "subject": "s1", "from_name": "Deal Desk", "html_content": "<b>x</b>"},
		},
		"isp_plans": []map[string]interface{}{
			{"isp": "gmail", "cadence": map[string]interface{}{"mode": "single"},
				"time_spans": []map[string]interface{}{
					{"type": "absolute", "start_at": sched.Format(time.RFC3339),
						"end_at": end.Format(time.RFC3339), "source": "duration-calc"},
				}},
		},
		"inclusion_segments": []string{"aaaaaaaa-0000-0000-0000-000000000001"},
	}
	b, err := json.Marshal(map[string]interface{}{"campaign_input": input})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	return string(b)
}

func dcRebuildSelectRows(status string, sent int, blob string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"name", "status", "sent_count", "pmta_config"}).
		AddRow("aug21-DB-gmail-CLK", status, sent, blob)
}

// ---------------------------------------------------------------------------
// 1. Domains listing — kumo transport label must survive.

func TestDayCardsDomains_IncludesKumoTransport(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := &PMTACampaignService{db: db}

	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(dcOrg).
		WillReturnRows(sqlmock.NewRows([]string{"sending_domain", "transport", "profile_ids"}).
			AddRow("em.aadwd.com", "kumo", 1).
			AddRow("em.discountblog.com", "pmta", 4).
			AddRow("m.discountblog.com", "ses", 2))

	rec := dcGet(t, svc, "/pmta-campaign/day-cards/domains", svc.HandleDayCardsDomains)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Domains []dayCardDomain `json:"domains"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Domains) != 3 {
		t.Fatalf("domains = %d, want 3", len(out.Domains))
	}
	if out.Domains[0].SendingDomain != "em.aadwd.com" || out.Domains[0].Transport != "kumo" {
		t.Errorf("kumo domain lost its label: %+v", out.Domains[0])
	}
	if out.Domains[1].ProfileIDs != 4 {
		t.Errorf("profile_ids = %d, want 4", out.Domains[1].ProfileIDs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2. Day-cards Denver window — the query must carry the [start, end) UTC
// instants of the requested Denver day, and the prev-day query the day before
// restricted to completed statuses.

func dayCardCols() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name", "status", "scheduled_at", "slot",
		"offer_id", "offer_key", "offer_name", "subject", "from_name",
		"total_recipients", "queued_count", "sent_count", "delivered_count",
		"open_count", "click_count", "hard_bounce_count", "soft_bounce_count",
		"unsubscribe_count", "has_config", "rebuilt_from", "transport"})
}

func TestDayCards_DenverWindowParams(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := &PMTACampaignService{db: db}

	dayStart, dayEnd, err := denverDayBounds("2026-08-21")
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	prevStart, prevEnd, err := denverDayBounds("2026-08-20")
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}

	sched := time.Date(2026, 8, 21, 7, 1, 0, 0, time.UTC)
	mock.ExpectQuery(`status <> 'deleted'`).
		WithArgs(dcOrg, dayStart, dayEnd, "em.discountblog.com").
		WillReturnRows(dayCardCols().AddRow(
			dcCampID, "aug21-DB-gmail-CLK", "scheduled", sched, "01:01",
			"", "personal-loans", "Personal Loans", "s1", "Deal Desk",
			1000, 900, 100, 90, 10, 2, 3, 4, 1, true, "", "ses"))
	mock.ExpectQuery(`status IN \('sent','completed','completed_with_errors','sending'\)`).
		WithArgs(dcOrg, prevStart, prevEnd, "em.discountblog.com").
		WillReturnRows(dayCardCols().AddRow(
			"eeeeeeee-0000-0000-0000-000000000001", "aug20-DB-gmail-CLK", "sent", sched.AddDate(0, 0, -1), "01:01",
			"", "personal-loans", "Personal Loans", "s0", "Deal Desk",
			2000, 0, 2000, 1900, 200, 30, 5, 15, 2, true, "", "ses"))

	rec := dcGet(t, svc, "/pmta-campaign/day-cards?domain=em.discountblog.com&date=2026-08-21", svc.HandleDayCards)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Date    string    `json:"date"`
		Domain  string    `json:"domain"`
		Cards   []dayCard `json:"cards"`
		PrevDay struct {
			Date      string         `json:"date"`
			Totals    dayCardsTotals `json:"totals"`
			Campaigns []dayCard      `json:"campaigns"`
		} `json:"prev_day"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.PrevDay.Date != "2026-08-20" {
		t.Errorf("prev_day.date = %s, want 2026-08-20", out.PrevDay.Date)
	}
	if len(out.Cards) != 1 || out.Cards[0].BounceCount != 7 {
		t.Errorf("cards = %+v (want 1 card, bounce_count 7 = hard 3 + soft 4)", out.Cards)
	}
	tt := out.PrevDay.Totals
	if tt.Campaigns != 1 || tt.Sent != 2000 || tt.Delivered != 1900 || tt.Bounced != 20 || tt.Unsubscribed != 2 {
		t.Errorf("prev totals = %+v", tt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3. Rebuild: missing blob → 400, never a degraded rebuild.

func TestDayCardsRebuild_NoBlob400(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := &PMTACampaignService{db: db}

	mock.ExpectQuery(`SELECT COALESCE\(name,''\), status, COALESCE\(sent_count,0\)`).
		WithArgs(dcCampID, dcOrg).
		WillReturnRows(dcRebuildSelectRows("scheduled", 0, "{}"))

	rec := dcPostRebuild(t, svc, `{"campaign_id":"`+dcCampID+`","confirmed":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "campaign_input") {
		t.Errorf("error should name the missing blob: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// 4. Rebuild: cancelled campaign → 409.

func TestDayCardsRebuild_Cancelled409(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := &PMTACampaignService{db: db}

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(dcCampID, dcOrg).
		WillReturnRows(dcRebuildSelectRows("cancelled", 0, dcConfigBlob(t, "aug21-DB-gmail-CLK")))

	rec := dcPostRebuild(t, svc, `{"campaign_id":"`+dcCampID+`","confirmed":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// 5. Rebuild unconfirmed → dry preview and ZERO writes. The mock is strict
// and ordered: ONLY the SELECT is expected, so any UPDATE/INSERT the handler
// attempted would come back as a driver error and fail the 200 assertion, and
// ExpectationsWereMet proves nothing else ran.

func TestDayCardsRebuild_UnconfirmedPreviewNoWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	var deployCalled bool
	svc := &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployCalled = true
			return "", "", false, fmt.Errorf("must not deploy on dry run")
		}}

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(dcCampID, dcOrg).
		WillReturnRows(dcRebuildSelectRows("sending", 250, dcConfigBlob(t, "aug21-DB-gmail-CLK")))

	rec := dcPostRebuild(t, svc, `{"campaign_id":"`+dcCampID+`","confirmed":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["dry_run"] != true {
		t.Errorf("dry_run missing: %v", out)
	}
	if _, ok := out["partial_send_warning"]; !ok {
		t.Errorf("sent_count=250 must produce partial_send_warning: %v", out)
	}
	if deployCalled {
		t.Errorf("deploy seam called on unconfirmed request")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("writes happened on a dry run: %v", err)
	}
}

// 6. Confirmed happy path — cancel UPDATEs run BEFORE the deploy call.
// Proven from inside the deploy seam: at deploy time every expectation
// registered so far (SELECT + both cancel UPDATEs) must already be met; the
// rebuilt_from stamp + audit insert are registered from inside the seam, so
// they can only be satisfied after it.

func TestDayCardsRebuild_ConfirmedCancelBeforeDeploy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	newID := "ffffffff-0000-0000-0000-000000000009"
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(dcCampID, dcOrg).
		WillReturnRows(dcRebuildSelectRows("scheduled", 0, dcConfigBlob(t, "aug21-DB-gmail-CLK")))
	mock.ExpectExec(`SET status = 'cancelled'`).
		WithArgs(dcCampID, dcOrg).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_queue`).
		WithArgs(dcCampID).
		WillReturnResult(sqlmock.NewResult(0, 42))

	var deployedInput engine.PMTACampaignInput
	svc := &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			// THE ORDER PROOF: the cancel writes must be complete already.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("deploy ran before cancel completed: %v", err)
			}
			if orgID != dcOrg {
				t.Errorf("deploy orgID = %s", orgID)
			}
			deployedInput = input
			// Post-deploy writes become expectable only now.
			mock.ExpectExec(`jsonb_set`).
				WithArgs(newID, dcCampID, dcOrg).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
				WillReturnResult(sqlmock.NewResult(1, 1))
			return newID, "finalizing_audience", false, nil
		}}

	rec := dcPostRebuild(t, svc, `{"campaign_id":"`+dcCampID+`","confirmed":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["cancelled_campaign_id"] != dcCampID || out["new_campaign_id"] != newID {
		t.Errorf("lineage wrong: %v", out)
	}
	if out["sent_before_cancel"] != float64(0) {
		t.Errorf("sent_before_cancel = %v", out["sent_before_cancel"])
	}
	if deployedInput.CampaignID != "" {
		t.Errorf("sibling must deploy id-less, got campaign_id %q", deployedInput.CampaignID)
	}
	if deployedInput.Name != "aug21-DB-gmail-CLK" {
		t.Errorf("sibling must keep the name (cancel-first frees it), got %q", deployedInput.Name)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// 7. Proof override that is not approved+active → 422, before any write.

func TestDayCardsRebuild_ProofNotApproved422(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := &PMTACampaignService{db: db}

	proofID := "bbbbbbbb-0000-0000-0000-000000000001"
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(dcCampID, dcOrg).
		WillReturnRows(dcRebuildSelectRows("scheduled", 0, dcConfigBlob(t, "aug21-DB-gmail-CLK")))
	mock.ExpectQuery(`FROM mailing_offer_proofs`).
		WithArgs(proofID, dcOrg).
		WillReturnRows(sqlmock.NewRows([]string{"html_content", "approval_status", "is_active", "variants", "from_names"}).
			AddRow("<b>y</b>", "pending", true, []byte(`[{"subject":"px"}]`), []byte(`["P"]`)))

	rec := dcPostRebuild(t, svc,
		`{"campaign_id":"`+dcCampID+`","confirmed":true,"overrides":{"proof_id":"`+proofID+`"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s, want 422", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// 8. scheduled_at inside the now+5min auto-immediate trap → 422 naming it.

func TestDayCardsRebuild_ScheduledAtTooSoon422(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := &PMTACampaignService{db: db}

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(dcCampID, dcOrg).
		WillReturnRows(dcRebuildSelectRows("scheduled", 0, dcConfigBlob(t, "aug21-DB-gmail-CLK")))

	soon := time.Now().Add(3 * time.Minute).UTC().Format(time.RFC3339)
	rec := dcPostRebuild(t, svc,
		`{"campaign_id":"`+dcCampID+`","confirmed":true,"overrides":{"scheduled_at":"`+soon+`"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s, want 422", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "IMMEDIATE") {
		t.Errorf("error must name the auto-immediate trap: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 9. rebaseTimeSpans — pure-function proof that every span keeps its own
// duration and its source VERBATIM, and only shifts by the anchor delta.

func TestRebaseTimeSpans_PreservesDurationAndSource(t *testing.T) {
	t0 := time.Date(2026, 8, 21, 7, 1, 0, 0, time.UTC)
	s1s, s1e := t0, t0.Add(2*time.Hour)
	s2s, s2e := t0.Add(1*time.Hour), t0.Add(9*time.Hour)
	weeklyStart := 9
	input := engine.PMTACampaignInput{
		SendMode:    "scheduled",
		ScheduledAt: &t0,
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", TimeSpans: []engine.PMTATimeSpanInput{
				{Type: "absolute", StartAt: &s1s, EndAt: &s1e, Source: "duration-calc"},
				{Type: "absolute", StartAt: &s2s, EndAt: &s2e, Source: "manual"},
			}},
			{ISP: "aol", TimeSpans: []engine.PMTATimeSpanInput{
				{Type: "weekly", DayOfWeek: "Monday", StartHour: &weeklyStart},
			}},
		},
	}

	newAt := t0.Add(26 * time.Hour)
	rebaseTimeSpans(&input, newAt)

	if input.SendMode != "scheduled" || input.ScheduledAt == nil || !input.ScheduledAt.Equal(newAt) {
		t.Fatalf("anchor not moved: mode=%s at=%v", input.SendMode, input.ScheduledAt)
	}
	sp := input.ISPPlans[0].TimeSpans
	if !sp[0].StartAt.Equal(t0.Add(26 * time.Hour)) {
		t.Errorf("span1 start = %v", sp[0].StartAt)
	}
	if got := sp[0].EndAt.Sub(*sp[0].StartAt); got != 2*time.Hour {
		t.Errorf("span1 duration = %v, want 2h (must be preserved)", got)
	}
	if !sp[1].StartAt.Equal(t0.Add(27 * time.Hour)) {
		t.Errorf("span2 start = %v (offset from anchor must be preserved)", sp[1].StartAt)
	}
	if got := sp[1].EndAt.Sub(*sp[1].StartAt); got != 8*time.Hour {
		t.Errorf("span2 duration = %v, want 8h", got)
	}
	if sp[0].Source != "duration-calc" || sp[1].Source != "manual" {
		t.Errorf("sources rewritten: %q %q — that re-arms/disarms the wave sanity check", sp[0].Source, sp[1].Source)
	}
	wk := input.ISPPlans[1].TimeSpans[0]
	if wk.StartAt != nil || wk.EndAt != nil || *wk.StartHour != 9 {
		t.Errorf("weekly span must be untouched: %+v", wk)
	}
}
