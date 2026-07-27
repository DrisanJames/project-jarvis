package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

const sendDayTestOrg = "00000000-0000-0000-0000-000000000001"

// fakeScrubHub records LoadFromDB calls (the send-path cache reload proof).
type fakeScrubHub struct {
	loads int
	fail  bool
	count int
}

func (f *fakeScrubHub) LoadFromDB(ctx context.Context) error {
	f.loads++
	if f.fail {
		return context.DeadlineExceeded
	}
	return nil
}
func (f *fakeScrubHub) Count() int { return f.count }

func newSendDayScrubTestRouter(t *testing.T, hub sendDayScrubHubReloader) (*chi.Mux, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewSendDayScrubService(db)
	if hub != nil {
		svc.SetGlobalSuppressionHub(hub)
	}
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	return r, mock, func() { db.Close() }
}

func sendDayScrubRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Organization-ID", sendDayTestOrg)
	return req.WithContext(context.Background())
}

// ── Pure helpers ────────────────────────────────────────────────────────────

func TestSendDayDateToken(t *testing.T) {
	cases := map[string]string{
		"2026-07-28": "jul28",
		"2026-07-05": "jul05", // zero-padded day — matches registry.date_token %b%d
		"2026-12-01": "dec01",
	}
	for in, want := range cases {
		got, err := sendDayDateToken(in)
		if err != nil || got != want {
			t.Errorf("sendDayDateToken(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"jul28", "2026-7-28", "28-07-2026", ""} {
		if _, err := sendDayDateToken(bad); err == nil {
			t.Errorf("sendDayDateToken(%q) should fail", bad)
		}
	}
}

// TestNormalizeScrubMD5s: lowercase/trim/dedupe; anything that is not a
// 32-hex MD5 must land in bad — the import fail-closes on it.
func TestNormalizeScrubMD5s(t *testing.T) {
	raw := []string{
		"D41D8CD98F00B204E9800998ECF8427E", // uppercase → lowered
		" d41d8cd98f00b204e9800998ecf8427e ", // dupe of above after trim/lower
		"aabbccddeeff00112233445566778899",
		"", "  ", // blanks are noise, not errors
		"not-an-md5",
		"user@example.com",                    // an email in an md5 file is a loud failure
		"d41d8cd98f00b204e9800998ecf8427",     // 31 chars
		"d41d8cd98f00b204e9800998ecf8427e0",   // 33 chars
		"g41d8cd98f00b204e9800998ecf8427e",    // non-hex char
	}
	clean, bad := normalizeScrubMD5s(raw)
	if len(clean) != 2 {
		t.Fatalf("clean = %v; want 2 entries", clean)
	}
	if clean[0] != "d41d8cd98f00b204e9800998ecf8427e" || clean[1] != "aabbccddeeff00112233445566778899" {
		t.Errorf("clean order/content wrong: %v", clean)
	}
	if len(bad) != 5 {
		t.Errorf("bad = %v; want 5 rejected entries", bad)
	}
	for _, b := range bad {
		if !strings.Contains(b, "entry ") {
			t.Errorf("bad entry %q must carry its position", b)
		}
	}
}

// TestUnionInclusionSegments: union + dedup across fake campaign configs;
// non-uuid and unparseable refs are reported, never silently dropped.
func TestUnionInclusionSegments(t *testing.T) {
	segA := "11111111-1111-1111-1111-111111111111"
	segB := "22222222-2222-2222-2222-222222222222"
	segC := "33333333-3333-3333-3333-333333333333"
	configs := []string{
		`["` + segA + `","` + segB + `"]`,
		`["` + segB + `","` + segC + `"]`, // segB shared → deduped
		`[]`,
		``,                       // campaign without campaign_input
		`["not-a-uuid"]`,         // stale/foreign ref → skipped, reported
		`{"oops":"not-array"}`,   // unparseable → skipped, reported
		`["` + strings.ToUpper(segA) + `"]`, // case-folded dupe
	}
	ids, skipped := unionInclusionSegments(configs)
	if len(ids) != 3 || ids[0] != segA || ids[1] != segB || ids[2] != segC {
		t.Fatalf("ids = %v; want [%s %s %s]", ids, segA, segB, segC)
	}
	if len(skipped) != 2 {
		t.Errorf("skipped = %v; want 2 (non-uuid + unparseable)", skipped)
	}
}

// ── Export: streaming writer shape ──────────────────────────────────────────

func TestExportAudienceMD5StreamShape(t *testing.T) {
	r, mock, closeFn := newSendDayScrubTestRouter(t, nil)
	defer closeFn()

	segA := "11111111-1111-1111-1111-111111111111"
	segB := "22222222-2222-2222-2222-222222222222"

	// 1. Campaign configs for "jul28 - %" — org-scoped, staged/scheduled.
	mock.ExpectQuery(`jsonb_typeof[\s\S]*inclusion_segments[\s\S]*FROM mailing_campaigns`).
		WithArgs(sendDayTestOrg, "jul28 - %").
		WillReturnRows(sqlmock.NewRows([]string{"cfg"}).
			AddRow(`["` + segA + `","` + segB + `"]`).
			AddRow(`["` + segB + `"]`)) // union dedups segB
	// 2. Org-scoped segment verification.
	mock.ExpectQuery(`SELECT id::text FROM mailing_segments`).
		WithArgs(sendDayTestOrg, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(segA).AddRow(segB))
	// 3. Streamed DISTINCT hash query inside a raised-budget tx.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT DISTINCT LOWER\(s\.email_hash\)[\s\S]*mailing_segment_members[\s\S]*mailing_subscribers`).
		WithArgs(sqlmock.AnyArg(), sendDayTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"md5"}).
			AddRow("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").
			AddRow("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb").
			AddRow("cccccccccccccccccccccccccccccccc"))
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, sendDayScrubRequest("GET", "/send-day/2026-07-28/audience-md5", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="2026-07-28-audience-md5.csv"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 4 || lines[0] != "md5" || lines[1] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("csv body wrong: %v", lines)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestExportNoCampaigns404: an empty board must refuse loudly, never stream
// an empty file into Optizmo.
func TestExportNoCampaigns404(t *testing.T) {
	r, mock, closeFn := newSendDayScrubTestRouter(t, nil)
	defer closeFn()

	mock.ExpectQuery(`jsonb_typeof[\s\S]*FROM mailing_campaigns`).
		WithArgs(sendDayTestOrg, "jul28 - %").
		WillReturnRows(sqlmock.NewRows([]string{"cfg"}))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, sendDayScrubRequest("GET", "/send-day/2026-07-28/audience-md5", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s; want 404", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "jul28 - ") {
		t.Errorf("error should name the expected prefix: %s", w.Body.String())
	}
}

func TestExportBadDate400(t *testing.T) {
	r, _, closeFn := newSendDayScrubTestRouter(t, nil)
	defer closeFn()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, sendDayScrubRequest("GET", "/send-day/jul28/audience-md5", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
}

// ── Summary ─────────────────────────────────────────────────────────────────

func TestAudienceSummary(t *testing.T) {
	r, mock, closeFn := newSendDayScrubTestRouter(t, nil)
	defer closeFn()

	segA := "11111111-1111-1111-1111-111111111111"
	mock.ExpectQuery(`jsonb_typeof[\s\S]*FROM mailing_campaigns`).
		WithArgs(sendDayTestOrg, "jul28 - %").
		WillReturnRows(sqlmock.NewRows([]string{"cfg"}).AddRow(`["` + segA + `"]`))
	mock.ExpectQuery(`SELECT id::text FROM mailing_segments`).
		WithArgs(sendDayTestOrg, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(segA))
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM \([\s\S]*DISTINCT LOWER\(s\.email_hash\)`).
		WithArgs(sqlmock.AnyArg(), sendDayTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1520000))
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, sendDayScrubRequest("GET", "/send-day/2026-07-28/audience-md5/summary", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["campaigns"].(float64) != 1 || resp["segments"].(float64) != 1 ||
		resp["unique_md5s"].(float64) != 1520000 || resp["campaign_name_prefix"] != "jul28 - " {
		t.Errorf("summary wrong: %v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// ── Import: suppression write path golden ───────────────────────────────────

// TestImportScrubGolden pins the whole write path: temp-table load, org-scoped
// match count, the INSERT…SELECT into mailing_global_suppressions with
// source='optizmo-scrub/<date>' + ON CONFLICT (organization_id, md5_hash),
// commit, then the hub cache reload (the send-path visibility step).
func TestImportScrubGolden(t *testing.T) {
	hub := &fakeScrubHub{count: 42}
	r, mock, closeFn := newSendDayScrubTestRouter(t, hub)
	defer closeFn()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TEMP TABLE _sendday_scrub_hashes`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO _sendday_scrub_hashes[\s\S]*unnest`).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`CREATE INDEX ON _sendday_scrub_hashes`).WillReturnResult(sqlmock.NewResult(0, 0))
	// Org-scoped match count: 2 of 3 hashes exist in our subscriber base.
	mock.ExpectQuery(`SELECT COUNT\(DISTINCT h\.hash\)[\s\S]*mailing_subscribers`).
		WithArgs(sendDayTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(2))
	// The golden write: global store, resolved email, dated source tag.
	mock.ExpectExec(`INSERT INTO mailing_global_suppressions[\s\S]*LOWER\(TRIM\(s\.email\)\)[\s\S]*ON CONFLICT \(organization_id, md5_hash\) DO NOTHING`).
		WithArgs(sendDayTestOrg, "optizmo-scrub/2026-07-28").
		WillReturnResult(sqlmock.NewResult(0, 1)) // 1 new; 1 was already suppressed
	mock.ExpectCommit()

	body := map[string]interface{}{"md5s": []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", // normalized to lowercase
		"cccccccccccccccccccccccccccccccc",
	}}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, sendDayScrubRequest("POST", "/send-day/2026-07-28/scrub-suppressions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["unique_md5s"].(float64) != 3 || resp["matched_md5s"].(float64) != 2 ||
		resp["suppressed_new"].(float64) != 1 || resp["already_suppressed"].(float64) != 1 ||
		resp["unmatched_md5s"].(float64) != 1 {
		t.Errorf("counts wrong: %v", resp)
	}
	if resp["source"] != "optizmo-scrub/2026-07-28" {
		t.Errorf("source = %v", resp["source"])
	}
	if resp["hub_reloaded"] != true {
		t.Errorf("hub_reloaded = %v", resp["hub_reloaded"])
	}
	if hub.loads != 1 {
		t.Errorf("hub.LoadFromDB calls = %d; want 1 (send-path cache must refresh)", hub.loads)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestImportRejectsBadMD5sLoudly: one malformed row refuses the WHOLE request
// — no DB statement may run (a silently skipped opt-out is a compliance miss).
func TestImportRejectsBadMD5sLoudly(t *testing.T) {
	hub := &fakeScrubHub{}
	r, mock, closeFn := newSendDayScrubTestRouter(t, hub)
	defer closeFn()

	body := map[string]interface{}{"md5s": []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"user@example.com",
	}}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, sendDayScrubRequest("POST", "/send-day/2026-07-28/scrub-suppressions", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s; want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "entry 2") {
		t.Errorf("rejection must name the offending entry: %s", w.Body.String())
	}
	if hub.loads != 0 {
		t.Error("hub must not reload on a refused import")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no DB calls expected on refusal: %v", err)
	}
}

// TestImportWithoutHub503: an import whose cache reload cannot run would be
// send-path-inert until reboot — refuse it.
func TestImportWithoutHub503(t *testing.T) {
	r, mock, closeFn := newSendDayScrubTestRouter(t, nil)
	defer closeFn()

	body := map[string]interface{}{"md5s": []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, sendDayScrubRequest("POST", "/send-day/2026-07-28/scrub-suppressions", body))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no DB calls expected without hub: %v", err)
	}
}

// TestImportHubReloadFailureSurfaced: the DB write is durable; a failed cache
// reload must be SURFACED (hub_reloaded=false + error), never silent.
func TestImportHubReloadFailureSurfaced(t *testing.T) {
	hub := &fakeScrubHub{fail: true}
	r, mock, closeFn := newSendDayScrubTestRouter(t, hub)
	defer closeFn()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TEMP TABLE _sendday_scrub_hashes`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO _sendday_scrub_hashes[\s\S]*unnest`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`CREATE INDEX ON _sendday_scrub_hashes`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT COUNT\(DISTINCT h\.hash\)`).
		WithArgs(sendDayTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectExec(`INSERT INTO mailing_global_suppressions`).
		WithArgs(sendDayTestOrg, "optizmo-scrub/2026-07-28").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	body := map[string]interface{}{"md5s": []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, sendDayScrubRequest("POST", "/send-day/2026-07-28/scrub-suppressions", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["hub_reloaded"] != false {
		t.Errorf("hub_reloaded = %v; want false", resp["hub_reloaded"])
	}
	if _, ok := resp["hub_reload_error"]; !ok {
		t.Error("hub_reload_error must be surfaced")
	}
}
