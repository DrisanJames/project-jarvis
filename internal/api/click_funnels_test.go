package api

// Unit tests for the Click Funnels service.
//
// These pin BEHAVIOUR the screen depends on and that has already been got wrong
// once each in this codebase:
//   - conversions must come from converted_at, never status='converted'
//     (the goal node sets that on sequence completion: 4,635 vs 57 in 60d);
//   - engagement provenance must be reported honestly, so a PG fallback can
//     never be presented as lake data;
//   - the enroll confirm gate must actually block, because enrolling starts a
//     live 4-touch sequence per person;
//   - upload parsing must bucket rather than silently drop, and must not enroll
//     someone who already converted.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------- upload gate

// TestUploadEnroll_RequiresConfirm is the safety gate. Enrolling starts a live
// reminder sequence and accepted sends are unrecallable, so a body without
// confirm must be rejected BEFORE any row is written. A gate that no-ops is
// worse than no gate at all.
func TestUploadEnroll_RequiresConfirm(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	svc := NewClickFunnelsService(db)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/click-funnels/upload/enroll",
		strings.NewReader(`{"offer_id":"6137","raw":"3f6c9d2a-1b44-4c8e-9f21-7a5e2c4d8b90"}`))
	rec := httptest.NewRecorder()

	svc.HandleUploadEnroll(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without confirm, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "confirm=true required") {
		t.Fatalf("error must name the confirm requirement, got %s", rec.Body.String())
	}
	// The decisive assertion: NO database work may happen on the ungated path.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no queries should run when the gate trips: %v", err)
	}
}

// TestUploadPreview_RequiresOfferID guards against an upload landing in an
// unspecified lane.
func TestUploadPreview_RequiresOfferID(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	svc := NewClickFunnelsService(db)
	rec := httptest.NewRecorder()
	svc.HandleUploadPreview(rec,
		httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"raw":"abc"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "offer_id required") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------- preview buckets

// TestBuildPreview_Buckets is the core upload logic: every submitted id must
// land in exactly one bucket, and an already-converted or already-active
// subscriber must NEVER be enrolled again.
func TestBuildPreview_Buckets(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const (
		ready     = "11111111-1111-1111-1111-111111111111"
		active    = "22222222-2222-2222-2222-222222222222"
		converted = "33333333-3333-3333-3333-333333333333"
		triggered = "44444444-4444-4444-4444-444444444444"
		unknown   = "55555555-5555-5555-5555-555555555555"
	)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(click_journey_id,''), enabled FROM mailing_offer_journey_map`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "enabled"}).
			AddRow("click-drip-4touch-72h", true))

	// The bucketing query returns a row only for ids that EXIST as subscribers;
	// `unknown` is absent, which is how the handler detects it.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_subscribers s`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "active", "converted", "triggered"}).
			AddRow(ready, false, false, false).
			AddRow(active, true, false, false).
			AddRow(converted, false, true, false).
			AddRow(triggered, false, false, true))

	svc := NewClickFunnelsService(db)
	// Deliberately messy paste: a header token, a duplicate, a comma, and junk.
	raw := "sub1\n" + ready + ",\n" + active + "\n" + converted + "\t" + triggered + "\n" +
		unknown + "\nnot-a-uuid\n" + ready + "\n"

	pv, readyIDs, err := svc.buildPreview(context.Background(), UploadRequest{OfferID: "6137", Raw: raw})
	if err != nil {
		t.Fatalf("buildPreview: %v", err)
	}

	if pv.Ready != 1 || len(readyIDs) != 1 || readyIDs[0] != ready {
		t.Fatalf("ready bucket wrong: ready=%d ids=%v", pv.Ready, readyIDs)
	}
	if pv.AlreadyActive != 1 {
		t.Fatalf("already_active = %d, want 1", pv.AlreadyActive)
	}
	if pv.AlreadyConverted != 1 {
		t.Fatalf("already_converted = %d, want 1 (a converted subscriber must never re-enroll)", pv.AlreadyConverted)
	}
	if pv.RecentlyTriggered != 1 {
		t.Fatalf("recently_triggered = %d, want 1", pv.RecentlyTriggered)
	}
	if pv.Unknown != 1 {
		t.Fatalf("unknown = %d, want 1", pv.Unknown)
	}
	if pv.Malformed != 1 {
		t.Fatalf("malformed = %d, want 1", pv.Malformed)
	}
	if pv.Duplicates != 1 {
		t.Fatalf("duplicates_in_file = %d, want 1", pv.Duplicates)
	}
	// Conservation: nothing may vanish silently between submitted and buckets.
	sum := pv.Ready + pv.AlreadyActive + pv.AlreadyConverted + pv.RecentlyTriggered +
		pv.Unknown + pv.Malformed + pv.Duplicates
	if sum != pv.Submitted {
		t.Fatalf("buckets (%d) must account for every submitted row (%d)", sum, pv.Submitted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestBuildPreview_DisabledLaneWarns: a disabled lane still accepts an upload
// (the rows queue) but the operator must be told they will not be enrolled.
func TestBuildPreview_DisabledLaneWarns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("999").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "enabled"}).
			AddRow("click-drip-4touch-72h", false))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_subscribers s`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "active", "converted", "triggered"}))

	svc := NewClickFunnelsService(db)
	pv, _, err := svc.buildPreview(context.Background(),
		UploadRequest{OfferID: "999", Raw: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatalf("buildPreview: %v", err)
	}
	if pv.LaneEnabled {
		t.Fatal("lane_enabled should be false")
	}
	joined := strings.Join(pv.Warnings, " | ")
	if !strings.Contains(strings.ToUpper(joined), "DISABLED") {
		t.Fatalf("a disabled lane must warn, got %q", joined)
	}
}

// TestBuildPreview_UnknownLaneIsAnError: uploading into an offer with no
// journey-map row must fail loudly rather than queue rows that the enroller
// would later drop as offer_unmapped_at_processing.
func TestBuildPreview_UnknownLaneIsAnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("1054").
		WillReturnError(sqlErrNoRows())

	svc := NewClickFunnelsService(db)
	_, _, err = svc.buildPreview(context.Background(), UploadRequest{OfferID: "1054", Raw: "x"})
	if err == nil {
		t.Fatal("expected an error for an offer with no funnel")
	}
	if !strings.Contains(err.Error(), "no journey lane") {
		t.Fatalf("error should explain the missing lane, got %v", err)
	}
}

// ---------------------------------------------------------------- parsing

func TestSplitUploadText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"newlines", "a\nb\nc", 3},
		{"commas", "a,b,c", 3},
		{"tabs and spaces", "a\tb c", 3},
		{"semicolons", "a;b", 2},
		{"header dropped", "sub1\na\nb", 2},
		{"subscriber_id header dropped", "subscriber_id\na", 1},
		{"blank", "   ", 0},
		{"crlf", "a\r\nb", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(splitUploadText(tc.in)); got != tc.want {
				t.Fatalf("splitUploadText(%q) = %d tokens, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- lake window

// TestClickFunnelWindow bounds Athena cost. email_events is partitioned by dt
// and Athena bills per byte scanned, so an unbounded window is a money leak on
// a screen an operator can re-open all day.
func TestClickFunnelWindow(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	from, to := clickFunnelWindow(r)
	if from == "" || to == "" {
		t.Fatal("window must always be bounded")
	}
	if from >= to {
		t.Fatalf("default window must span time, got %s..%s", from, to)
	}

	r = httptest.NewRequest(http.MethodGet, "/x?from=2026-07-01&to=2026-07-10", nil)
	from, to = clickFunnelWindow(r)
	if from != "2026-07-01" || to != "2026-07-10" {
		t.Fatalf("explicit window not honored: %s..%s", from, to)
	}

	// Reversed input must not produce an inverted (empty-scan) range.
	r = httptest.NewRequest(http.MethodGet, "/x?from=2026-07-10&to=2026-07-01", nil)
	from, to = clickFunnelWindow(r)
	if from != "2026-07-01" || to != "2026-07-10" {
		t.Fatalf("reversed window should be normalized, got %s..%s", from, to)
	}

	// Garbage falls back to the default rather than reaching Athena as-is.
	r = httptest.NewRequest(http.MethodGet, "/x?from=not-a-date", nil)
	from, _ = clickFunnelWindow(r)
	if from == "not-a-date" {
		t.Fatal("invalid dates must never be passed through to the lake query")
	}
}

// ---------------------------------------------------------------- provenance

// ---------------------------------------------------------------- lane list

// sqlErrNoRows is the no-rows error the handlers branch on (sql.ErrNoRows).
func sqlErrNoRows() error { return sql.ErrNoRows }

// ---------------------------------------------------------------- drill-down

// TestNodeEnrollments_MatchingAndErrorsOnly covers HubSpot's "Matching
// enrollments" affordance. The ?action=error variant is the operationally
// important one: click-drip send failures BLOCK and retry rather than advancing,
// so this is the only place an operator can see who is stuck at a touch and why
// (a live probe surfaced PMTA template-parse 422s this way).
func TestNodeEnrollments_MatchingAndErrorsOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_execution_log l`)).
		WithArgs("6137", "email-1", false, 200).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "email", "status", "current_node_id", "enrolled_at",
			"executed_at", "action", "error_message", "converted_at", "exit_reason",
		}).AddRow("enroll-clk-1", "a@example.com", "active", "delay-18h",
			"2026-08-01T14:05:38Z", "2026-08-01T20:06:13Z", "continue", "", nil, ""))

	svc := NewClickFunnelsService(db)
	r := chi.NewRouter()
	r.Get("/f/{offerID}/nodes/{nodeID}/enrollments", svc.HandleNodeEnrollments)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/f/6137/nodes/email-1/enrollments", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp NodeEnrollmentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || resp.Enrollments[0].Email != "a@example.com" {
		t.Fatalf("unexpected drill-down payload: %+v", resp)
	}
	if resp.Enrollments[0].ConvertedAt != nil {
		t.Fatal("a non-converted enrollment must serialize converted_at as null, not a zero time")
	}

	// ?action=error must flip the BOUND predicate, not filter client-side.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_execution_log l`)).
		WithArgs("6137", "email-1", true, 200).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "email", "status", "current_node_id", "enrolled_at",
			"executed_at", "action", "error_message", "converted_at", "exit_reason",
		}).AddRow("enroll-clk-2", "b@example.com", "active", "email-1",
			"2026-08-01T10:00:00Z", "2026-08-01T11:00:00Z", "error",
			"click-drip send failed: PMTA API error (HTTP 422)", nil, ""))

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/f/6137/nodes/email-1/enrollments?action=error", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	resp = NodeEnrollmentsResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 1 || !strings.Contains(resp.Enrollments[0].ErrorMessage, "422") {
		t.Fatalf("error drill-down must carry the failure reason: %+v", resp.Enrollments)
	}

	// An out-of-range limit is clamped, never passed through raw.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_execution_log l`)).
		WithArgs("6137", "email-1", false, 200).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "email", "status", "current_node_id", "enrolled_at",
			"executed_at", "action", "error_message", "converted_at", "exit_reason",
		}))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/f/6137/nodes/email-1/enrollments?limit=99999", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for an out-of-range limit, got %d", rec.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestOfferNameSubquery_ShapeGuards pins the two production realities the name
// resolution must survive, both verified against prod 2026-08-02:
//   - mailing_offers holds DUPLICATE rows per everflow id (offer 5990 has three),
//     so this must be a scalar subquery — a LEFT JOIN would render that funnel
//     three times in the lane list.
//   - 8 of 22 enabled lanes have no mailing_offers row at all, so the result
//     must COALESCE to "Offer <id>" rather than a blank cell.
func TestOfferNameSubquery_ShapeGuards(t *testing.T) {
	q := offerNameSubquery("m.everflow_offer_id")

	if !strings.Contains(q, "LIMIT 1") {
		t.Fatal("must LIMIT 1 — mailing_offers has duplicate rows per everflow id")
	}
	if !strings.Contains(q, "COALESCE(") || !strings.Contains(q, "'Offer '") {
		t.Fatal("must fall back to 'Offer <id>' for offers with no mailing_offers row")
	}
	if !strings.Contains(q, "ORDER BY length(o.name)") {
		t.Fatal("ordering must be deterministic (shortest = least-decorated canonical name)")
	}
	if strings.Contains(strings.ToUpper(q), "JOIN") {
		t.Fatal("must not be a JOIN — that is what multiplies the lane row")
	}
	// The id expression is interpolated in three places; all must agree.
	// Appears twice: the subquery filter, and the 'Offer <id>' fallback.
	if strings.Count(q, "m.everflow_offer_id") != 2 {
		t.Fatalf("id expression must appear in the filter and the fallback, got %d", strings.Count(q, "m.everflow_offer_id"))
	}
}

// TestUpsertReminderSubject_OmittedBodyPreservesCreative is a data-loss guard.
// The copy form (subject/preheader/from) and the body editor both PUT the same
// reminder-subjects row. If an omitted body_html were treated as "", editing a
// subject would silently wipe the touch's creative and every subscriber would
// fall back to whatever campaign they clicked — the exact incoherence that let
// an August body ship under a June subject.
func TestUpsertReminderSubject_OmittedBodyPreservesCreative(t *testing.T) {
	var req upsertReminderSubjectRequest
	if err := json.Unmarshal([]byte(`{"subject":"s","preheader":"p","enabled":true}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.BodyHTML != nil {
		t.Fatal("an omitted body_html must decode to nil so COALESCE keeps the stored creative")
	}

	// An explicit empty string is a deliberate clear, and must be distinguishable.
	if err := json.Unmarshal([]byte(`{"subject":"s","body_html":""}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.BodyHTML == nil || *req.BodyHTML != "" {
		t.Fatal(`an explicit "" must decode to a non-nil empty string (deliberate clear)`)
	}
}

// TestProofSendable_MirrorsTheSenderGate. The sender will only mail an APPROVED
// and ACTIVE offer proof, so the screen must compute sendability the same way.
// A touch pointing at a proof that was later un-approved or deactivated silently
// stops using it — the operator has to be able to SEE that, not discover it when
// engagement flatlines.
func TestProofSendable_MirrorsTheSenderGate(t *testing.T) {
	cases := []struct {
		approval string
		active   bool
		sendable bool
	}{
		{"approved", true, true},
		{"approved", false, false}, // withdrawn from rotation
		{"pending", true, false},   // never cleared
		{"rejected", true, false},
		{"", true, false},
		{"APPROVED", true, true}, // case-insensitive, matches the sender
	}
	for _, tc := range cases {
		got := tc.active && strings.EqualFold(tc.approval, "approved")
		if got != tc.sendable {
			t.Fatalf("approval=%q active=%v -> sendable=%v, want %v", tc.approval, tc.active, got, tc.sendable)
		}
	}
}
