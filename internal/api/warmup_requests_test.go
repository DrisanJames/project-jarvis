package api

// Kumo warm-up request fixtures (warmup_requests.go).
//
// What these pin:
//   - the upsert binds the caller's org on BOTH the kumo-profile gate and the
//     insert (multi-tenancy is not optional);
//   - the three NEGATIVE gates actually fire, and the two that need no DB fire
//     BEFORE any DB access (zero sqlmock expectations consumed);
//   - the builder-owned states (building/built/failed) are refused from the
//     API — this is the whole point of the request/build split, so it gets a
//     dedicated negative fixture rather than being assumed;
//   - the creative endpoint labels updated_at and generated_at DISTINCTLY and
//     names updated_at as the freshness field (generated_at is frozen at first
//     insert and reads days stale on live rows);
//   - the DDL consts stay inside the 5s startup-migration budget: small,
//     idempotent, no backfill, no ALTER-with-DEFAULT rewrite, one statement per
//     const (no semicolons) so migrationSkipProbe classifies each correctly.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const warmupTestOrg = "11111111-2222-3333-4444-555555555555"

func postWarmup(t *testing.T, s *PMTACampaignService, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/mailing/pmta-campaign/warmup/requests", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", warmupTestOrg)
	rec := httptest.NewRecorder()
	s.HandleWarmupRequestUpsert(rec, req)
	return rec
}

func warmupFutureRFC3339() string {
	return time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// 1. Upsert happy path — org id bound on the gate AND the insert.
// ---------------------------------------------------------------------------

func TestWarmupUpsertHappyPathBindsOrg(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	newID := "99999999-8888-7777-6666-555555555555"

	mock.ExpectBegin()
	// The kumo-profile gate must be scoped to the CALLER's org.
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "em.aadwd.com").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// The insert's $1 is the org; $2/$3 the domain/slug.
	mock.ExpectQuery(`INSERT INTO mailing_kumo_warmup_requests`).
		WithArgs(warmupTestOrg, "em.aadwd.com", "aad",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(newID))
	mock.ExpectCommit()

	body := fmt.Sprintf(`{
		"sending_domain":"em.aadwd.com",
		"brand_slug":"aad",
		"creative_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"audience_segment_ids":["12121212-3434-5656-7878-909090909090"],
		"cold_source":"S0-hot",
		"cold_quota":500,
		"isp_quotas":{"yahoo":100,"aol":327},
		"scheduled_at":%q,
		"status":"requested"
	}`, warmupFutureRFC3339())

	rec := postWarmup(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID            string `json:"id"`
		SendingDomain string `json:"sending_domain"`
		BrandSlug     string `json:"brand_slug"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != newID || resp.SendingDomain != "em.aadwd.com" ||
		resp.BrandSlug != "aad" || resp.Status != "requested" {
		t.Fatalf("response contract wrong: %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 2. NEGATIVE: unknown / non-kumo sending_domain → 400 (and no row written).
// ---------------------------------------------------------------------------

func TestWarmupUpsertRejectsNonKumoDomain(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "em.discountblog.com").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// Decisive: no INSERT expectation. sqlmock (ordered) fails the test if the
	// handler writes anyway; the tx must roll back.
	mock.ExpectRollback()

	body := fmt.Sprintf(`{"sending_domain":"em.discountblog.com","scheduled_at":%q}`,
		warmupFutureRFC3339())
	rec := postWarmup(t, s, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-kumo domain: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "routing_mode='kumo'") {
		t.Fatalf("400 should name the kumo requirement: %s", rec.Body.String())
	}

	// Empty domain is rejected before any DB access at all.
	s2, mock2 := newLedgerServiceWithMock(t)
	rec2 := postWarmup(t, s2, fmt.Sprintf(`{"sending_domain":"","scheduled_at":%q}`, warmupFutureRFC3339()))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("empty domain: got %d, want 400", rec2.Code)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty domain must not touch the DB: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3. NEGATIVE: the API may not set a BUILDER-owned status.
// ---------------------------------------------------------------------------

func TestWarmupUpsertRefusesBuilderOwnedStatuses(t *testing.T) {
	// building / built / failed belong to the Python builder. The API must
	// refuse all three, and must refuse them before opening a transaction —
	// otherwise a portal write could race the builder and clobber campaign_id.
	for _, status := range []string{"building", "built", "failed"} {
		s, mock := newLedgerServiceWithMock(t)
		body := fmt.Sprintf(`{"sending_domain":"em.aadwd.com","scheduled_at":%q,"status":%q}`,
			warmupFutureRFC3339(), status)
		rec := postWarmup(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%s: got %d, want 400 (%s)", status, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "owned by the build job") {
			t.Fatalf("status=%s: 400 should say the builder owns it: %s", status, rec.Body.String())
		}
		// Negative control: zero DB interaction.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("status=%s must be rejected before any DB access: %v", status, err)
		}
	}

	// And the allow-list is closed: an invented status is refused too.
	s, _ := newLedgerServiceWithMock(t)
	rec := postWarmup(t, s, fmt.Sprintf(
		`{"sending_domain":"em.aadwd.com","scheduled_at":%q,"status":"deployed"}`, warmupFutureRFC3339()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown status: got %d, want 400", rec.Code)
	}
}

// TestWarmupTransitionTable pins who owns which state transition.
func TestWarmupTransitionTable(t *testing.T) {
	cases := []struct {
		current, next string
		ok            bool
	}{
		{"draft", "draft", true},
		{"draft", "requested", true},
		{"draft", "cancelled", true},
		{"requested", "requested", true},
		{"requested", "cancelled", true},
		{"requested", "draft", false}, // no walk-back once handed to the builder
		{"building", "cancelled", false},
		{"building", "requested", false},
		{"built", "cancelled", false}, // campaign_id is stamped — frozen
		{"failed", "requested", true}, // operator retry
		{"cancelled", "cancelled", true},
		{"cancelled", "requested", false},
	}
	for _, c := range cases {
		msg, ok := warmupTransitionAllowed(c.current, c.next)
		if ok != c.ok {
			t.Fatalf("%s -> %s: got ok=%v (%s), want %v", c.current, c.next, ok, msg, c.ok)
		}
		if !ok && msg == "" {
			t.Fatalf("%s -> %s: refusal must explain itself", c.current, c.next)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. NEGATIVE: scheduled_at in the past → 400.
// ---------------------------------------------------------------------------

func TestWarmupUpsertRejectsPastSchedule(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	rec := postWarmup(t, s, fmt.Sprintf(
		`{"sending_domain":"em.aadwd.com","scheduled_at":%q,"status":"requested"}`, past))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("past scheduled_at: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "future") {
		t.Fatalf("400 should say the schedule must be in the future: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("past schedule must be rejected before any DB access: %v", err)
	}

	// Missing entirely is also a 400 (the builder has nothing to schedule).
	s2, _ := newLedgerServiceWithMock(t)
	if rec := postWarmup(t, s2, `{"sending_domain":"em.aadwd.com"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing scheduled_at: got %d, want 400", rec.Code)
	}
	// Non-RFC3339 is a 400, not a 500.
	s3, _ := newLedgerServiceWithMock(t)
	if rec := postWarmup(t, s3,
		`{"sending_domain":"em.aadwd.com","scheduled_at":"tomorrow morning"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unparseable scheduled_at: got %d, want 400", rec.Code)
	}
	// cold_quota < 0 is a 400.
	s4, _ := newLedgerServiceWithMock(t)
	if rec := postWarmup(t, s4, fmt.Sprintf(
		`{"sending_domain":"em.aadwd.com","scheduled_at":%q,"cold_quota":-1}`,
		warmupFutureRFC3339())); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative cold_quota: got %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// 5. Creative endpoint: updated_at AND generated_at, distinctly labelled.
// ---------------------------------------------------------------------------

func TestWarmupCreativeExposesBothTimestamps(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)

	// The live shape that burns an hour: generated_at frozen at first insert
	// (nine days stale), updated_at moved by this morning's refresh.
	generated := time.Date(2026, 8, 11, 18, 13, 44, 0, time.UTC)
	updated := time.Date(2026, 8, 20, 16, 33, 17, 0, time.UTC)

	mock.ExpectQuery(`FROM mailing_creatives`).
		WithArgs(warmupTestOrg, "aadwd.com").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "brand_code", "filename", "subject", "preheader",
			"updated_at", "generated_at", "length", "sha256", "approval_status"}).
			AddRow("cccccccc-dddd-eeee-ffff-000000000000", "aadwd.com",
				"nl-aad-kumo-digest.html", "Today's read", "Three minutes",
				updated, generated, int64(6744), "a103cb07607eb149", "approved"))

	req := httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/warmup/creative?sending_domain=em.aadwd.com", nil)
	req.Header.Set("X-Organization-ID", warmupTestOrg)
	rec := httptest.NewRecorder()
	s.HandleWarmupCreative(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"updated_at", "generated_at", "freshness_field", "sha256", "html_length", "brand_slug"} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("creative response must carry %q: %s", field, rec.Body.String())
		}
	}
	if raw["updated_at"] == raw["generated_at"] {
		t.Fatal("updated_at and generated_at must be DISTINCT fields carrying distinct values")
	}
	if raw["freshness_field"] != "updated_at" {
		t.Fatalf("freshness_field must name updated_at, got %v", raw["freshness_field"])
	}
	// brand_code is the APEX domain on this table (verified prod) — not a
	// 2-3 letter code, including for the aad/hfc pilots.
	if raw["brand_code"] != "aadwd.com" {
		t.Fatalf("brand_code must be the apex domain, got %v", raw["brand_code"])
	}
	if raw["brand_slug"] != "aad" {
		t.Fatalf("brand_slug must be read out of the filename, got %v", raw["brand_slug"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWarmupCreativeRequiresASelector(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	req := httptest.NewRequest("GET", "/api/mailing/pmta-campaign/warmup/creative", nil)
	rec := httptest.NewRecorder()
	s.HandleWarmupCreative(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("must not touch the DB without a selector: %v", err)
	}
}

// TestWarmupApexDerivation pins the sending-domain → apex mapping used to join
// mailing_creatives.brand_code (which carries the APEX, verified on prod).
func TestWarmupApexDerivation(t *testing.T) {
	for in, want := range map[string]string{
		"em.aadwd.com":                  "aadwd.com",
		"em.hfcl.net":                   "hfcl.net",
		"em.us-finance.com":             "us-finance.com",
		"em.firsttimebuyerhomeloan.com": "firsttimebuyerhomeloan.com",
		"aadwd.com":                     "aadwd.com", // already an apex
	} {
		if got := kumoApexFromSendingDomain(in); got != want {
			t.Fatalf("%s: got %q, want %q", in, got, want)
		}
	}
}

// TestWarmupSegmentsUsesBuildLedger — the count must come from the build
// ledger when one exists; mailing_segments.subscriber_count is zeroed on a
// refresh timeout and would show a healthy segment as empty.
func TestWarmupSegmentsUsesBuildLedger(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM mailing_segments`).
		WithArgs(warmupTestOrg, `(^|[ -])AAD([ -]|$)`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "subscriber_count", "ledger_count", "last_build_status", "last_built_at"}).
			// cached 0 but ledger 1534 — the documented silent-zero shape.
			AddRow("aaaa1111-2222-3333-4444-555555555555", "AAD 30D Clickers", int64(0), int64(1534), "ok", time.Now()).
			// no ledger row at all (true for FRESH-KUMO-*/RAMP-YHOO-* today).
			AddRow("bbbb1111-2222-3333-4444-555555555555", "RAMP-YHOO-A12-AAD", int64(4650), nil, nil, nil))

	req := httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/warmup/segments?brand_slug=aad", nil)
	req.Header.Set("X-Organization-ID", warmupTestOrg)
	rec := httptest.NewRecorder()
	s.HandleWarmupSegments(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Segments []warmupSegment `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Segments) != 2 {
		t.Fatalf("want 2 segments, got %d", len(resp.Segments))
	}
	a := resp.Segments[0]
	if a.SubscriberCount != 1534 || a.CountSource != "build_ledger" || !a.CounterMismatch || a.CounterCount != 0 {
		t.Fatalf("ledger-backed segment wrong: %+v", a)
	}
	b := resp.Segments[1]
	if b.SubscriberCount != 4650 || b.CountSource != "cached_counter" || b.BuildStatus != "unknown" {
		t.Fatalf("ledger-less segment wrong: %+v", b)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 6. DDL consts: small, idempotent, no rewrite, no backfill, one statement.
// ---------------------------------------------------------------------------

func TestWarmupDDLFitsMigrationBudget(t *testing.T) {
	consts := map[string]string{
		"table":     KumoWarmupRequestsDDL,
		"index":     KumoWarmupRequestsIndexDDL,
		"live_slot": KumoWarmupRequestsLiveSlotDDL,
	}
	for name, ddl := range consts {
		// One statement per migration entry: a semicolon means two, and the
		// runner executes the string as a single Exec.
		if strings.Contains(ddl, ";") {
			t.Fatalf("%s: DDL must be ONE statement with no semicolon", name)
		}
		// Idempotent — this slice re-runs on every boot.
		if !strings.Contains(strings.ToUpper(ddl), "IF NOT EXISTS") {
			t.Fatalf("%s: DDL must be idempotent (IF NOT EXISTS)", name)
		}
		// No table rewrite: an ALTER ... SET/ADD with a DEFAULT is the shape
		// that exceeds the 5s budget on a populated table.
		if strings.Contains(strings.ToUpper(ddl), "ALTER TABLE") {
			t.Fatalf("%s: no ALTER TABLE in this feature's DDL (rewrite/lock-wait risk)", name)
		}
		// No backfill: a timed-out DML entry is logged "skipped, will retry
		// next boot" and is then silently absent forever.
		for _, dml := range []string{"INSERT ", "UPDATE ", "DELETE ", "SELECT "} {
			if strings.Contains(strings.ToUpper(ddl), dml) {
				t.Fatalf("%s: DDL must carry no %s (backfills do not belong in the 5s slice)", name, strings.TrimSpace(dml))
			}
		}
		// Never CONCURRENTLY here — that needs the concurrentIndexSpecs
		// builder (statement_timeout=0), not the migration slice.
		if strings.Contains(strings.ToUpper(ddl), "CONCURRENTLY") {
			t.Fatalf("%s: CONCURRENTLY belongs in concurrentIndexSpecs, not runStartupMigrations", name)
		}
	}

	// migrationSkipProbe classifies an entry by its LEADING keywords: table and
	// index MUST be separate consts or the index never lands once the table
	// exists (cmd/server/migration_skip.go reMigCreateTable).
	if !strings.HasPrefix(strings.TrimSpace(KumoWarmupRequestsDDL), "CREATE TABLE IF NOT EXISTS") {
		t.Fatal("table DDL must LEAD with CREATE TABLE IF NOT EXISTS (skip-probe classification)")
	}
	if !strings.HasPrefix(strings.TrimSpace(KumoWarmupRequestsIndexDDL), "CREATE INDEX IF NOT EXISTS") {
		t.Fatal("index DDL must LEAD with CREATE INDEX IF NOT EXISTS (skip-probe classification)")
	}
	if !strings.HasPrefix(strings.TrimSpace(KumoWarmupRequestsLiveSlotDDL), "CREATE UNIQUE INDEX IF NOT EXISTS") {
		t.Fatal("live-slot DDL must LEAD with CREATE UNIQUE INDEX IF NOT EXISTS")
	}
	if strings.Contains(KumoWarmupRequestsDDL, "CREATE INDEX") {
		t.Fatal("the table const must NOT also carry an index — the probe would skip it forever")
	}

	// The partial-unique day expression must use the IMMUTABLE
	// timezone(text, timestamptz) form; a bare scheduled_at::date is STABLE
	// and Postgres rejects it in an index expression.
	if !strings.Contains(KumoWarmupRequestsLiveSlotDDL, "AT TIME ZONE 'America/Denver'") {
		t.Fatal("live-slot index must key on the Denver day via the immutable AT TIME ZONE form")
	}
	if strings.Contains(KumoWarmupRequestsLiveSlotDDL, "(scheduled_at::date)") {
		t.Fatal("scheduled_at::date is STABLE, not IMMUTABLE — not indexable")
	}
	// Cancelled/failed rows must NOT occupy the slot, or cancel-and-retry
	// for the same domain+day becomes impossible.
	if strings.Contains(KumoWarmupRequestsLiveSlotDDL, "'cancelled'") ||
		strings.Contains(KumoWarmupRequestsLiveSlotDDL, "'failed'") {
		t.Fatal("live-slot predicate must exclude cancelled/failed so retry is possible")
	}

	// The table itself must carry every column the builder stamps back.
	for _, col := range []string{"campaign_id", "build_note", "status", "requested_by",
		"audience_segment_ids", "isp_quotas", "cold_source", "cold_quota"} {
		if !strings.Contains(KumoWarmupRequestsDDL, col) {
			t.Fatalf("table DDL missing column %q", col)
		}
	}
}
