package api

// Lane VDM stats + lane content panel fixtures (operator enhancement
// 2026-08-17). Permanent fixtures:
//   - the VDM lane SQL reads ses_vdm_daily complete=true rows only, both
//     windows in one aggregate pass (FILTER on yesterday).
//   - rate math: zero denominator → JSON null, never NaN/div-by-zero.
//   - laneServingCreativeSQL mirrors resolveOfferCreative's selection
//     VERBATIM (approved-over-generated CASE + updated_at DESC + LIMIT 1)
//     and never ships html_content in the list payload.
//   - writes: validation rejects before any DB access; archive of the last
//     serving subject → 422; offer swap requires exact confirm_name, an
//     active offer, and non-empty serving creative/subject/from-name pools
//     (422 naming the empty pool); every successful mutation writes the
//     audit log with the stamped actor.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ── VDM lane stats ──────────────────────────────────────────────────────────

func TestPropertyLedgerVDMSQLShape(t *testing.T) {
	if !strings.Contains(propertyLedgerVDMSQL, "FROM ses_vdm_daily") {
		t.Fatal("VDM lane SQL must read ses_vdm_daily")
	}
	if !strings.Contains(propertyLedgerVDMSQL, "WHERE complete") {
		t.Fatal("VDM lane SQL must serve complete=true rows ONLY")
	}
	if !regexp.MustCompile(`FILTER \(WHERE day = \$2::date\)`).MatchString(propertyLedgerVDMSQL) {
		t.Fatal("VDM lane SQL must compute the yesterday window via FILTER on $2")
	}
	if !strings.Contains(propertyLedgerVDMSQL, "GROUP BY identity, isp") {
		t.Fatal("VDM lane SQL must aggregate per (identity, isp) — regions summed")
	}
}

func TestVDMRateNullNotNaN(t *testing.T) {
	if got := vdmRate(5, 0); got != nil {
		t.Fatalf("zero denominator must return nil (JSON null), got %v", *got)
	}
	if got := vdmRate(0, 0); got != nil {
		t.Fatalf("0/0 must return nil, got %v", *got)
	}
	if got := vdmRate(45, 90); got == nil || *got != 0.5 {
		t.Fatalf("45/90 must be 0.5, got %v", got)
	}
}

// ledgerListRow builds one propertyLedgerListSQL result row (24 columns).
func ledgerListRow(rows *sqlmock.Rows, brand, isp string) *sqlmock.Rows {
	return rows.AddRow(brand, isp, 100, false, "", "", time.Now(), "", nil,
		nil, nil, int64(1), nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil)
}

func ledgerListCols() []string {
	return []string{"brand", "isp", "daily_budget", "hold", "notes", "updated_by", "updated_at",
		"approved_by", "approved_at", "min_budget", "max_budget", "lock_version",
		"pending_budget", "pending_effective_day", "reason", "held_from",
		"id", "proposed_budget", "base_budget", "basis", "created_at", "expires_at",
		"introduced", "c_updated_at"}
}

func TestPropertyLedgerListAttachesVDMWithNullRates(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)

	listRows := sqlmock.NewRows(ledgerListCols())
	ledgerListRow(listRows, "db", "gmail")   // has VDM volume
	ledgerListRow(listRows, "db", "yahoo")   // zero-delivery VDM lane
	ledgerListRow(listRows, "db", "verizon") // no VDM coverage
	mock.ExpectQuery(`FROM partner_drip_brand_budgets b`).WillReturnRows(listRows)

	vdmRows := sqlmock.NewRows([]string{"identity", "isp",
		"sent", "delivered", "opens", "clicks",
		"sent_7d", "delivered_7d", "opens_7d", "clicks_7d"}).
		AddRow("em.discountblog.com", "gmail", int64(200), int64(180), int64(45), int64(9),
			int64(1400), int64(1260), int64(300), int64(60)).
		AddRow("em.discountblog.com", "yahoo", int64(0), int64(0), int64(0), int64(0),
			int64(0), int64(0), int64(0), int64(0))
	mock.ExpectQuery(`FROM ses_vdm_daily`).WillReturnRows(vdmRows)

	req := httptest.NewRequest("GET", "/api/mailing/pmta-campaign/property-ledger", nil)
	rec := httptest.NewRecorder()
	s.HandleListPropertyLedger(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Rates computed server-side: 180/200, 45/180, 9/180.
	if !strings.Contains(body, `"delivered_pct":0.9`) {
		t.Errorf("expected delivered_pct 0.9 in body: %s", body)
	}
	if !strings.Contains(body, `"open_rate_vdm":0.25`) {
		t.Errorf("expected open_rate_vdm 0.25 in body: %s", body)
	}
	// Zero-delivery lane: null rates, never NaN.
	if !strings.Contains(body, `"open_rate_vdm":null`) {
		t.Errorf("zero-delivery lane must serve null open_rate_vdm: %s", body)
	}
	if strings.Contains(body, "NaN") {
		t.Errorf("body must never contain NaN: %s", body)
	}
	// Uncovered lane carries no vdm block (omitempty).
	if strings.Count(body, `"vdm":{"sent"`) != 2 {
		t.Errorf("expected exactly 2 lane vdm blocks (verizon uncovered): %s", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ── Lane content: read resolution pins ──────────────────────────────────────

func TestLaneServingCreativeSQLMirrorsOrchestrator(t *testing.T) {
	// The ORDER BY must be the orchestrator's resolveOfferCreative selection
	// VERBATIM (partner_drip_orchestrator.go) — approved over generated,
	// newest updated_at, one row.
	for _, frag := range []string{
		`AND COALESCE(status, '') NOT IN ('archived','rejected')`,
		`AND COALESCE(html_content, '') <> ''`,
		`WHEN 'approved' THEN 0`,
		`WHEN 'generated' THEN 1`,
		`ELSE 2`,
		`updated_at DESC`,
		`LIMIT 1`,
	} {
		if !strings.Contains(laneServingCreativeSQL, frag) {
			t.Errorf("laneServingCreativeSQL must mirror resolveOfferCreative — missing %q", frag)
		}
	}
	if !strings.Contains(laneServingCreativeSQL, "length(html_content)") {
		t.Error("list payload must carry length(html_content), never the HTML itself")
	}
	if regexp.MustCompile(`SELECT[^(]*html_content`).MatchString(laneServingCreativeSQL) {
		t.Error("laneServingCreativeSQL must not select raw html_content in the list payload")
	}
}

func TestLaneContentUnknownBrand400(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	req := httptest.NewRequest("GET", "/api/mailing/pmta-campaign/property-ledger/lane-content?brand=zz", nil)
	rec := httptest.NewRecorder()
	s.HandleLaneContent(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown brand: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("validation must reject before any DB access: %v", err)
	}
}

// ── Subject edits ───────────────────────────────────────────────────────────

const testOfferID = "11111111-2222-3333-4444-555555555555"
const testRowID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func TestLaneSubjectEditValidation(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	cases := []struct {
		name string
		body string
	}{
		{"missing offer_id", `{"action":"add","subject_line":"x"}`},
		{"bad offer uuid", `{"offer_id":"nope","action":"add","subject_line":"x"}`},
		{"add without subject_line", `{"offer_id":"` + testOfferID + `","action":"add"}`},
		{"archive without subject_id", `{"offer_id":"` + testOfferID + `","action":"archive"}`},
		{"archive bad subject uuid", `{"offer_id":"` + testOfferID + `","action":"archive","subject_id":"nope"}`},
		{"bad action", `{"offer_id":"` + testOfferID + `","action":"delete"}`},
	}
	for _, c := range cases {
		rec := postJSON(t, s.HandleLaneSubjectEdit, "/x", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", c.name, rec.Code, rec.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("validation must reject before any DB access: %v", err)
	}
}

func TestLaneSubjectArchiveLastServing422(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_offer_subject_lines`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	rec := postJSON(t, s.HandleLaneSubjectEdit, "/x",
		`{"offer_id":"`+testOfferID+`","action":"archive","subject_id":"`+testRowID+`"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("archiving the last serving subject: got %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLaneSubjectAddWritesAuditLog(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectExec(`INSERT INTO mailing_offer_subject_lines`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := postJSON(t, s.HandleLaneSubjectEdit, "/x",
		`{"offer_id":"`+testOfferID+`","action":"add","subject_line":"Fresh subject"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: got %d (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("add must INSERT the row AND the audit-log entry: %v", err)
	}
}

// ── Touch copy edits ────────────────────────────────────────────────────────

func TestLaneTouchCopyValidation(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	cases := []struct {
		name string
		body string
	}{
		{"unknown brand", `{"brand":"zz","vertical":"remodel","touch":1,"subject_line":"x"}`},
		{"touch zero", `{"brand":"db","vertical":"remodel","touch":0,"subject_line":"x"}`},
		{"nothing to update", `{"brand":"db","vertical":"remodel","touch":1}`},
		{"blank subject", `{"brand":"db","vertical":"remodel","touch":1,"subject_line":"  "}`},
		{"blank from_name", `{"brand":"db","vertical":"remodel","touch":1,"from_name":""}`},
		{"t1 without vertical", `{"brand":"db","touch":1,"subject_line":"x"}`},
	}
	for _, c := range cases {
		rec := postJSON(t, s.HandleLaneTouchCopyEdit, "/x", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", c.name, rec.Code, rec.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("validation must reject before any DB access: %v", err)
	}
}

func TestLaneTouchCopyT1NotFound404(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM partner_drip_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"subject_line", "preheader", "from_name"}))

	rec := postJSON(t, s.HandleLaneTouchCopyEdit, "/x",
		`{"brand":"db","vertical":"remodel","touch":1,"subject_line":"New subject"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing t1 row: got %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLaneTouchCopyT1UpdateWritesAuditLog(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM partner_drip_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"subject_line", "preheader", "from_name"}).
			AddRow("Old subject", "Old pre", "Old name"))
	mock.ExpectExec(`UPDATE partner_drip_creatives SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := postJSON(t, s.HandleLaneTouchCopyEdit, "/x",
		`{"brand":"db","vertical":"remodel","touch":1,"subject_line":"New subject","preheader":"New pre"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("t1 update: got %d (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("t1 update must UPDATE the row AND write the audit log: %v", err)
	}
}

// ── Offer swap ──────────────────────────────────────────────────────────────

const testDatasetID = "99999999-8888-7777-6666-555555555555"

func TestLaneOfferSwapValidation(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	cases := []struct {
		name string
		body string
	}{
		{"missing ids", `{"confirm_name":"x"}`},
		{"bad dataset uuid", `{"dataset_id":"nope","offer_id":"` + testOfferID + `","confirm_name":"x"}`},
		{"bad offer uuid", `{"dataset_id":"` + testDatasetID + `","offer_id":"nope","confirm_name":"x"}`},
	}
	for _, c := range cases {
		rec := postJSON(t, s.HandleLaneOfferSwap, "/x", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", c.name, rec.Code, rec.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("validation must reject before any DB access: %v", err)
	}
}

func TestLaneOfferSwapConfirmNameMismatch400(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM partner_datasets WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "vertical", "offer_id"}).
			AddRow("GlobUSA Remodel Feed", "remodel", ""))

	rec := postJSON(t, s.HandleLaneOfferSwap, "/x",
		`{"dataset_id":"`+testDatasetID+`","offer_id":"`+testOfferID+`","confirm_name":"wrong name"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("confirm_name mismatch: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLaneOfferSwapInactiveOffer422(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM partner_datasets WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "vertical", "offer_id"}).
			AddRow("GlobUSA Remodel Feed", "remodel", ""))
	mock.ExpectQuery(`FROM mailing_offers WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status"}).AddRow("Old Offer", "sunset"))

	rec := postJSON(t, s.HandleLaneOfferSwap, "/x",
		`{"dataset_id":"`+testDatasetID+`","offer_id":"`+testOfferID+`","confirm_name":"GlobUSA Remodel Feed"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("inactive offer: got %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLaneOfferSwapEmptySubjectPool422(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM partner_datasets WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "vertical", "offer_id"}).
			AddRow("GlobUSA Remodel Feed", "remodel", ""))
	mock.ExpectQuery(`FROM mailing_offers WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status"}).AddRow("New Offer", "active"))
	mock.ExpectQuery(`EXISTS \(SELECT 1 FROM mailing_offer_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"c", "s", "f"}).AddRow(true, false, true))

	rec := postJSON(t, s.HandleLaneOfferSwap, "/x",
		`{"dataset_id":"`+testDatasetID+`","offer_id":"`+testOfferID+`","confirm_name":"GlobUSA Remodel Feed"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty subject pool: got %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "subject") {
		t.Fatalf("422 must NAME the empty pool: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLaneOfferSwapSuccessWritesAuditLog(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM partner_datasets WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "vertical", "offer_id"}).
			AddRow("GlobUSA Remodel Feed", "remodel", "old-offer-id"))
	mock.ExpectQuery(`FROM mailing_offers WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status"}).AddRow("New Offer", "active"))
	mock.ExpectQuery(`EXISTS \(SELECT 1 FROM mailing_offer_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"c", "s", "f"}).AddRow(true, true, true))
	mock.ExpectExec(`UPDATE partner_datasets SET offer_id`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := postJSON(t, s.HandleLaneOfferSwap, "/x",
		`{"dataset_id":"`+testDatasetID+`","offer_id":"`+testOfferID+`","confirm_name":"GlobUSA Remodel Feed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("swap: got %d (%s)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("swap must UPDATE the dataset AND write the audit log: %v", err)
	}
}
