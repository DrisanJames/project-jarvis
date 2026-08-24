package api

// Board-grid stage tests. The negative paths are the point: a dry run must
// write NOTHING (strict ordered sqlmock — any write surfaces as both a driver
// error and an unmet-expectations failure), a confirmed run deploys through
// the seam in CELL ORDER, an unapproved proof fails ONLY its cell (422) while
// the batch continues, a past target is refused per cell, already_existed is
// reported not fatal, and a source campaign without a blob fails its cell
// without sinking the batch. Staging never cancels anything — there is no
// UPDATE in this path at all.
// Run: go test ./internal/api/ -run 'BoardGridStage|BoardGrid_Clone' -v

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

const (
	bgsOrg    = "00000000-0000-0000-0000-000000000001"
	bgsSrc1   = "aaaaaaaa-1111-0000-0000-000000000001"
	bgsSrc2   = "aaaaaaaa-2222-0000-0000-000000000002"
	bgsOffer  = "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d"
	bgsProof  = "bbbbbbbb-0000-0000-0000-000000000001"
	bgsSeg    = "cccccccc-0000-0000-0000-000000000009"
	bgsSrcSeg = "aaaaaaaa-0000-0000-0000-000000000001"
)

// bgsDate is a stage date far enough out that every slot clears the
// now+10min floor regardless of when the test runs.
func bgsDate(t *testing.T) string {
	t.Helper()
	return denverToday(time.Now()).AddDate(0, 0, 2).Format("2006-01-02")
}

func bgsPost(t *testing.T, svc *BoardGridService, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/board-grid/stage", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", bgsOrg)
	rec := httptest.NewRecorder()
	svc.HandleStageGrid(rec, req)
	return rec
}

// bgsBlob is a faithful pmta_config::text with a campaign_input carrying an
// audience program (segments + priority + reserves) so audience-override
// tests can prove what survives.
func bgsBlob(t *testing.T, name string) string {
	t.Helper()
	sched := time.Date(2026, 8, 21, 7, 1, 0, 0, time.UTC)
	end := sched.Add(8 * time.Hour)
	input := map[string]interface{}{
		"name":           name,
		"offer_id":       bgsOffer,
		"sending_domain": "em.discountblog.com",
		"send_mode":      "scheduled",
		"scheduled_at":   sched.Format(time.RFC3339),
		"variants": []map[string]interface{}{
			{"variant_name": "A", "subject": "s1", "from_name": "Deal Desk", "html_content": "<b>x</b>"},
		},
		"isp_plans": []map[string]interface{}{
			{"isp": "gmail", "throttle_strategy": "auto",
				"cadence": map[string]interface{}{"mode": "interval", "every_minutes": 15},
				"time_spans": []map[string]interface{}{
					{"type": "absolute", "start_at": sched.Format(time.RFC3339),
						"end_at": end.Format(time.RFC3339), "source": "duration-calc"},
				}},
		},
		"inclusion_segments": []string{bgsSrcSeg},
		"send_priority":      []map[string]interface{}{{"kind": "segment", "id": bgsSrcSeg}},
		"segment_reserves":   []map[string]interface{}{{"segment_id": bgsSrcSeg, "reserve": 100}},
	}
	b, err := json.Marshal(map[string]interface{}{"campaign_input": input})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	return string(b)
}

func bgsSourceRows(name, blob string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"name", "pmta_config"}).AddRow(name, blob)
}

// expectSourceLoad registers the per-cell source-blob SELECT.
func bgsExpectSource(mock sqlmock.Sqlmock, srcID, name, blob string) {
	mock.ExpectQuery(`SELECT COALESCE\(name,''\), COALESCE\(pmta_config::text,''\)`).
		WithArgs(srcID, bgsOrg).
		WillReturnRows(bgsSourceRows(name, blob))
}

func bgsExpectOffer(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM mailing_offers`).
		WithArgs(bgsOffer, bgsOrg).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Sams Club"))
}

func bgsExpectProof(mock sqlmock.Sqlmock, approval string, active bool) {
	// Name lookup, then applyDayCardsCreativeOverride's full row.
	mock.ExpectQuery(`SELECT COALESCE\(name,''\) FROM mailing_offer_proofs`).
		WithArgs(bgsProof, bgsOrg).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Sams Summer 50"))
	mock.ExpectQuery(`FROM mailing_offer_proofs`).
		WithArgs(bgsProof, bgsOrg).
		WillReturnRows(sqlmock.NewRows([]string{"html_content", "approval_status", "is_active", "variants", "from_names"}).
			AddRow("<b>proof</b>", approval, active, []byte(`[{"subject":"proof subj","preheader":"proof pre"}]`), []byte(`["Proof From"]`)))
}

func bgsCellJSON(prop, slot, name, srcID string, withOffer, withProof bool) string {
	m := map[string]interface{}{
		"property": prop, "slot": slot, "name": name, "source_campaign_id": srcID,
	}
	if withOffer {
		m["offer_id"] = bgsOffer
	}
	if withProof {
		m["proof_id"] = bgsProof
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func bgsDecode(t *testing.T, rec *httptest.ResponseRecorder) (results []boardGridStageResult, totals map[string]int) {
	t.Helper()
	var out struct {
		Results []boardGridStageResult `json:"results"`
		Totals  map[string]int         `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return out.Results, out.Totals
}

// ---------------------------------------------------------------------------
// 1. Dry run = ZERO writes. Only SELECTs are registered; the seam must not be
// called; ExpectationsWereMet proves nothing else ran (any write would also
// come back as a driver error and fail the 200).

func TestBoardGridStage_DryRunZeroWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	var deployCalled bool
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployCalled = true
			return "", "", false, fmt.Errorf("must not deploy on dry run")
		}}}

	bgsExpectSource(mock, bgsSrc1, "src one", bgsBlob(t, "src one"))
	bgsExpectOffer(mock)
	bgsExpectProof(mock, "approved", true)

	date := bgsDate(t)
	rec := bgsPost(t, svc, `{"date":"`+date+`","confirmed":false,"cells":[`+
		bgsCellJSON("DB", "01:01", "08252026 - DB - Sams", bgsSrc1, true, true)+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	results, totals := bgsDecode(t, rec)
	if len(results) != 1 || results[0].Status != "dry" {
		t.Fatalf("results = %+v", results)
	}
	if results[0].RecipientsEstimate != "planned at deploy" {
		t.Errorf("recipients_estimate = %q", results[0].RecipientsEstimate)
	}
	if results[0].Offer != "Sams Club" || results[0].ProofName != "Sams Summer 50" {
		t.Errorf("dry summary lost offer/proof: %+v", results[0])
	}
	if results[0].Audience != "source" {
		t.Errorf("audience = %q, want source", results[0].Audience)
	}
	if results[0].ScheduledAt == nil {
		t.Fatal("dry summary must carry the target scheduled_at")
	}
	if totals["dry"] != 1 {
		t.Errorf("totals = %v", totals)
	}
	if deployCalled {
		t.Error("deploy seam called on a dry run")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("writes happened on a dry run: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2. Confirmed: deploys through the seam in CELL ORDER, and the deploy input
// carries the cell's name, offer, the proof's html/copy, and a scheduled_at at
// the CELL's slot on the target Denver day — never the source campaign's time.

func TestBoardGridStage_ConfirmedDeploysInOrderAtCellSlot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	var deployed []engine.PMTACampaignInput
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			if orgID != bgsOrg {
				t.Errorf("deploy orgID = %s", orgID)
			}
			deployed = append(deployed, input)
			return fmt.Sprintf("ffffffff-0000-0000-0000-00000000000%d", len(deployed)), "finalizing_audience", false, nil
		}}}

	// Cell 1: offer+proof. Cell 2: offer only.
	bgsExpectSource(mock, bgsSrc1, "src one", bgsBlob(t, "src one"))
	bgsExpectOffer(mock)
	bgsExpectProof(mock, "approved", true)
	bgsExpectSource(mock, bgsSrc2, "src two", bgsBlob(t, "src two"))
	bgsExpectOffer(mock)
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	date := bgsDate(t)
	rec := bgsPost(t, svc, `{"date":"`+date+`","confirmed":true,"cells":[`+
		bgsCellJSON("DB", "01:01", "cell one", bgsSrc1, true, true)+`,`+
		bgsCellJSON("CI", "06:01", "cell two", bgsSrc2, true, false)+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	results, totals := bgsDecode(t, rec)
	if totals["deployed"] != 2 {
		t.Fatalf("totals = %v results=%+v", totals, results)
	}
	if len(deployed) != 2 || deployed[0].Name != "cell one" || deployed[1].Name != "cell two" {
		t.Fatalf("deploy order/names wrong: %d %q %q", len(deployed),
			deployed[0].Name, deployed[1].Name)
	}
	if results[0].CampaignID == "" || results[1].CampaignID == "" {
		t.Errorf("campaign ids missing: %+v", results)
	}
	// Sibling identity: id-less, offer applied, proof copy riding.
	if deployed[0].CampaignID != "" {
		t.Errorf("deploy must be id-less, got %q", deployed[0].CampaignID)
	}
	if deployed[0].OfferID != bgsOffer {
		t.Errorf("offer_id = %q", deployed[0].OfferID)
	}
	if deployed[0].Variants[0].HTMLContent != "<b>proof</b>" ||
		deployed[0].Variants[0].Subject != "proof subj" ||
		deployed[0].Variants[0].PreviewText != "proof pre" ||
		deployed[0].Variants[0].FromName != "Proof From" {
		t.Errorf("proof copy did not ride the deploy: %+v", deployed[0].Variants[0])
	}
	// The target time is the CELL's slot on the target Denver day.
	loc, _ := time.LoadLocation("America/Denver")
	for i, wantSlot := range []string{"01:01", "06:01"} {
		if deployed[i].ScheduledAt == nil {
			t.Fatalf("cell %d: no scheduled_at", i)
		}
		got := deployed[i].ScheduledAt.In(loc)
		if got.Format("2006-01-02") != date || got.Format("15:04") != wantSlot {
			t.Errorf("cell %d target = %s %s Denver, want %s %s",
				i, got.Format("2006-01-02"), got.Format("15:04"), date, wantSlot)
		}
		// Span rebase: duration + source preserved.
		sp := deployed[i].ISPPlans[0].TimeSpans[0]
		if sp.Source != "duration-calc" {
			t.Errorf("cell %d span source = %q", i, sp.Source)
		}
		if d := sp.EndAt.Sub(*sp.StartAt); d != 8*time.Hour {
			t.Errorf("cell %d span duration = %v, want 8h preserved", i, d)
		}
	}
	// The source audience program rides along untouched without an override.
	if len(deployed[0].InclusionSegments) != 1 || deployed[0].InclusionSegments[0] != bgsSrcSeg {
		t.Errorf("source segments lost: %+v", deployed[0].InclusionSegments)
	}
	if len(deployed[0].SendPriority) != 1 || len(deployed[0].SegmentReserves) != 1 {
		t.Errorf("source priority/reserves lost: %+v %+v",
			deployed[0].SendPriority, deployed[0].SegmentReserves)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// 2b. EMPTY-CELL flow regression: the stage lands at the CLICKED slot column,
// not the source campaign's own slot. The source blob is anchored 07:01Z
// (01:01 Denver); the cell says 11:01 — the deploy must fire 11:01 Denver.

func TestBoardGridStage_EmptyCellLandsAtClickedSlotNotSourceSlot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	var deployed []engine.PMTACampaignInput
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployed = append(deployed, input)
			return "ffffffff-0000-0000-0000-000000000009", "finalizing_audience", false, nil
		}}}

	bgsExpectSource(mock, bgsSrc1, "src one", bgsBlob(t, "src one")) // anchored 01:01 Denver
	bgsExpectOffer(mock)
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).WillReturnResult(sqlmock.NewResult(1, 1))

	date := bgsDate(t)
	rec := bgsPost(t, svc, `{"date":"`+date+`","confirmed":true,"cells":[`+
		bgsCellJSON("DB", "11:01", "new at 11", bgsSrc1, true, false)+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	loc, _ := time.LoadLocation("America/Denver")
	got := deployed[0].ScheduledAt.In(loc)
	if got.Format("15:04") != "11:01" {
		t.Fatalf("landed at %s Denver — the SOURCE's slot leaked; want the clicked 11:01", got.Format("15:04"))
	}
	if got.Format("2006-01-02") != date {
		t.Errorf("landed on %s, want %s", got.Format("2006-01-02"), date)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3. An unapproved proof fails ITS cell with 422 and the batch continues.

func TestBoardGridStage_ProofNotApproved422PerCellContinues(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	var deployedNames []string
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployedNames = append(deployedNames, input.Name)
			return "ffffffff-0000-0000-0000-000000000001", "finalizing_audience", false, nil
		}}}

	// Cell 1: pending proof → 422 per-cell, no deploy.
	bgsExpectSource(mock, bgsSrc1, "src one", bgsBlob(t, "src one"))
	bgsExpectOffer(mock)
	bgsExpectProof(mock, "pending", true)
	// Cell 2: clean.
	bgsExpectSource(mock, bgsSrc2, "src two", bgsBlob(t, "src two"))
	bgsExpectOffer(mock)
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).WillReturnResult(sqlmock.NewResult(1, 1))

	date := bgsDate(t)
	rec := bgsPost(t, svc, `{"date":"`+date+`","confirmed":true,"cells":[`+
		bgsCellJSON("DB", "01:01", "bad proof cell", bgsSrc1, true, true)+`,`+
		bgsCellJSON("CI", "06:01", "good cell", bgsSrc2, true, false)+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	results, totals := bgsDecode(t, rec)
	if results[0].Status != "failed" || results[0].Code != http.StatusUnprocessableEntity {
		t.Fatalf("cell 1 = %+v, want failed/422", results[0])
	}
	if !strings.Contains(results[0].Error, "not shippable") {
		t.Errorf("cell 1 error = %q", results[0].Error)
	}
	if results[1].Status != "deployed" {
		t.Fatalf("cell 2 = %+v — one bad cell must not sink the batch", results[1])
	}
	if totals["failed"] != 1 || totals["deployed"] != 1 {
		t.Errorf("totals = %v", totals)
	}
	if len(deployedNames) != 1 || deployedNames[0] != "good cell" {
		t.Errorf("deployed = %v", deployedNames)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 4. A target inside now+10min → 422 per cell, no deploy for that cell.

func TestBoardGridStage_PastTarget422(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	var deployCalled bool
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployCalled = true
			return "", "", false, nil
		}}}

	bgsExpectSource(mock, bgsSrc1, "src one", bgsBlob(t, "src one"))
	bgsExpectOffer(mock)
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).WillReturnResult(sqlmock.NewResult(1, 1))

	// Today's Denver date at 00:00: always <= now, so always inside the floor.
	today := denverToday(time.Now()).Format("2006-01-02")
	rec := bgsPost(t, svc, `{"date":"`+today+`","confirmed":true,"cells":[`+
		bgsCellJSON("DB", "00:00", "too soon", bgsSrc1, true, false)+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	results, _ := bgsDecode(t, rec)
	if results[0].Status != "failed" || results[0].Code != http.StatusUnprocessableEntity {
		t.Fatalf("result = %+v, want failed/422", results[0])
	}
	if !strings.Contains(results[0].Error, "IMMEDIATE") {
		t.Errorf("error must name the auto-immediate trap: %q", results[0].Error)
	}
	if deployCalled {
		t.Error("deploy seam called for a past target")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. already_existed (the by-(org,name) guard) is reported, never fatal —
// re-posting the same proposal converges.

func TestBoardGridStage_AlreadyExistedReportedNotFatal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			return "eeeeeeee-0000-0000-0000-000000000007", "scheduled", true, nil
		}}}

	bgsExpectSource(mock, bgsSrc1, "src one", bgsBlob(t, "src one"))
	bgsExpectOffer(mock)
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).WillReturnResult(sqlmock.NewResult(1, 1))

	date := bgsDate(t)
	rec := bgsPost(t, svc, `{"date":"`+date+`","confirmed":true,"cells":[`+
		bgsCellJSON("DB", "01:01", "repost", bgsSrc1, true, false)+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	results, totals := bgsDecode(t, rec)
	if results[0].Status != "already_existed" || results[0].Error != "" {
		t.Fatalf("result = %+v, want already_existed without error", results[0])
	}
	if results[0].CampaignID != "eeeeeeee-0000-0000-0000-000000000007" {
		t.Errorf("existing id not reported: %+v", results[0])
	}
	if totals["already_existed"] != 1 || totals["failed"] != 0 {
		t.Errorf("totals = %v", totals)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 6. A source campaign with no blob fails ITS cell (400) and the batch
// continues to the next cell.

func TestBoardGridStage_MissingBlobPerCellContinues(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	var deployedNames []string
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployedNames = append(deployedNames, input.Name)
			return "ffffffff-0000-0000-0000-000000000002", "finalizing_audience", false, nil
		}}}

	bgsExpectSource(mock, bgsSrc1, "no blob", "{}")
	bgsExpectSource(mock, bgsSrc2, "src two", bgsBlob(t, "src two"))
	bgsExpectOffer(mock)
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).WillReturnResult(sqlmock.NewResult(1, 1))

	date := bgsDate(t)
	rec := bgsPost(t, svc, `{"date":"`+date+`","confirmed":true,"cells":[`+
		bgsCellJSON("DB", "01:01", "blobless", bgsSrc1, true, false)+`,`+
		bgsCellJSON("CI", "06:01", "good", bgsSrc2, true, false)+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	results, totals := bgsDecode(t, rec)
	if results[0].Status != "failed" || results[0].Code != http.StatusBadRequest ||
		!strings.Contains(results[0].Error, "campaign_input") {
		t.Fatalf("cell 1 = %+v, want failed/400 naming the blob", results[0])
	}
	if results[1].Status != "deployed" || totals["deployed"] != 1 {
		t.Fatalf("cell 2 = %+v totals=%v", results[1], totals)
	}
	if len(deployedNames) != 1 || deployedNames[0] != "good" {
		t.Errorf("deployed = %v", deployedNames)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 7. Per-cell AUDIENCE override: inclusion_segments replaces the source's
// audience program (segments set, priority + reserves CLEARED); absent, the
// blob's program rides untouched (proven in test 2).

func TestBoardGridStage_AudienceOverrideReplacesProgram(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	var deployed []engine.PMTACampaignInput
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployed = append(deployed, input)
			return "ffffffff-0000-0000-0000-000000000003", "finalizing_audience", false, nil
		}}}

	bgsExpectSource(mock, bgsSrc1, "src one", bgsBlob(t, "src one"))
	bgsExpectOffer(mock)
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).WillReturnResult(sqlmock.NewResult(1, 1))

	date := bgsDate(t)
	rec := bgsPost(t, svc, `{"date":"`+date+`","confirmed":true,"cells":[{
		"property":"DB","slot":"01:01","name":"aud override",
		"source_campaign_id":"`+bgsSrc1+`","offer_id":"`+bgsOffer+`",
		"inclusion_segments":["`+bgsSeg+`"]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	results, _ := bgsDecode(t, rec)
	if results[0].Audience != "1 segments override" {
		t.Errorf("audience = %q", results[0].Audience)
	}
	in := deployed[0]
	if len(in.InclusionSegments) != 1 || in.InclusionSegments[0] != bgsSeg {
		t.Fatalf("override did not land: %+v", in.InclusionSegments)
	}
	if len(in.SendPriority) != 0 || len(in.SegmentReserves) != 0 {
		t.Errorf("priority/reserves must be CLEARED with an audience override: %+v %+v",
			in.SendPriority, in.SegmentReserves)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 8. BOARD-WIDE overrides: slot_times remaps a whole column; window_hours
// replaces spans with one [target, target+w] 'duration-calc' span;
// throttle_strategy lands on the input AND every plan.

func TestBoardGridStage_BoardWideTimingAndThrottle(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	var deployed []engine.PMTACampaignInput
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployed = append(deployed, input)
			return "ffffffff-0000-0000-0000-000000000004", "finalizing_audience", false, nil
		}}}

	bgsExpectSource(mock, bgsSrc1, "src one", bgsBlob(t, "src one"))
	bgsExpectOffer(mock)
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).WillReturnResult(sqlmock.NewResult(1, 1))

	date := bgsDate(t)
	rec := bgsPost(t, svc, `{"date":"`+date+`","confirmed":true,
		"slot_times":{"01:01":"02:30"},"throttle_strategy":"gentle","window_hours":4,
		"cells":[`+bgsCellJSON("DB", "01:01", "retimed", bgsSrc1, true, false)+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	in := deployed[0]
	loc, _ := time.LoadLocation("America/Denver")
	got := in.ScheduledAt.In(loc)
	if got.Format("15:04") != "02:30" {
		t.Fatalf("column remap did not land: fired %s Denver, want 02:30", got.Format("15:04"))
	}
	if in.ThrottleStrategy != "gentle" || in.ISPPlans[0].ThrottleStrategy != "gentle" {
		t.Errorf("throttle override must land on input AND plans: %q / %q",
			in.ThrottleStrategy, in.ISPPlans[0].ThrottleStrategy)
	}
	if len(in.ISPPlans[0].TimeSpans) != 1 {
		t.Fatalf("window must collapse to one span: %+v", in.ISPPlans[0].TimeSpans)
	}
	sp := in.ISPPlans[0].TimeSpans[0]
	if sp.Source != "duration-calc" {
		t.Errorf("changed-duration span must be source=duration-calc, got %q", sp.Source)
	}
	if d := sp.EndAt.Sub(*sp.StartAt); d != 4*time.Hour {
		t.Errorf("window = %v, want 4h", d)
	}
	if !sp.StartAt.Equal(in.ScheduledAt.UTC()) {
		t.Errorf("window must start at the target: %v vs %v", sp.StartAt, in.ScheduledAt)
	}
	// The cell result keeps its COLUMN identity while firing at the remap.
	results, _ := bgsDecode(t, rec)
	if results[0].Slot != "01:01" {
		t.Errorf("result slot = %q, want the column identity 01:01", results[0].Slot)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// Bad board-wide values are refused at the door — whole request, not per cell.
func TestBoardGridStage_BoardOverrideValidation(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := &BoardGridService{db: db, campaigns: &PMTACampaignService{db: db}}
	date := bgsDate(t)
	for _, body := range []string{
		`{"date":"` + date + `","cells":[{"property":"DB","slot":"01:01","name":"x","source_campaign_id":"` + bgsSrc1 + `"}],"slot_times":{"01:01":"25:99"}}`,
		`{"date":"` + date + `","cells":[{"property":"DB","slot":"01:01","name":"x","source_campaign_id":"` + bgsSrc1 + `"}],"throttle_strategy":"warp"}`,
		`{"date":"` + date + `","cells":[{"property":"DB","slot":"01:01","name":"x","source_campaign_id":"` + bgsSrc1 + `"}],"window_hours":40}`,
	} {
		if rec := bgsPost(t, svc, body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// 9. The stage handler without a wired campaign service answers 503, and the
// clone keeps source_campaign_id (the id /stage needs).

func TestBoardGridStage_UnwiredService503(t *testing.T) {
	svc := &BoardGridService{}
	rec := bgsPost(t, svc, `{"date":"2026-08-25","cells":[]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestBoardGrid_CloneKeepsSourceCampaignID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewBoardGridService(db)
	resetBoardGridCacheForTest()

	dayStart, dayEnd, err := denverDayBounds("2026-08-22")
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WithArgs(dayStart, dayEnd, bgsOrg).
		WillReturnRows(sqlmock.NewRows([]string{"brand_code", "brand_label", "sending_domain",
			"brand_root", "slot", "campaign_id", "name", "offer_id", "offer_name",
			"subject", "status", "recipients"}).
			AddRow("DB", "Discount Blog", "m.discountblog.com", "discountblog.com", "01:01",
				bgsSrc1, "08222026 - DB - Sams", bgsOffer, "Sams Club", "s", "sent", 1000))

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/board-grid/clone?from=2026-08-22&to=2026-08-23", nil)
	req.Header.Set("X-Organization-ID", bgsOrg)
	rec := httptest.NewRecorder()
	svc.HandleCloneGrid(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out BoardGrid
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Cells) != 1 {
		t.Fatalf("cells = %d", len(out.Cells))
	}
	c := out.Cells[0]
	if c.SourceCampaignID != bgsSrc1 {
		t.Fatalf("source_campaign_id = %q, want %q — the proposal is un-stageable without it", c.SourceCampaignID, bgsSrc1)
	}
	if c.CampaignID != "" || !c.Proposed {
		t.Errorf("proposal identity wrong: campaign_id=%q proposed=%t", c.CampaignID, c.Proposed)
	}
	if c.Name != "08232026 - DB - Sams" {
		t.Errorf("name = %q", c.Name)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}
