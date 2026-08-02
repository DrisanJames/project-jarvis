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
	"time"

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

// TestFunnelNodes_ReportsPGFallbackProvenance is the anti-dishonesty test. With
// no Athena reader configured the handler falls back to PG, whose engagement is
// PMTA-route-complete only and under-reports SES. The response MUST say so —
// silently labeling fallback numbers as lake data is how a wrong number becomes
// a decision.
func TestFunnelNodes_ReportsPGFallbackProvenance(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	nodes := `[{"id":"trig","type":"trigger","config":{}},` +
		`{"id":"delay-1h","type":"delay","config":{"delayValue":1,"delayUnit":"hours"}},` +
		`{"id":"email-0","type":"email","config":{"reminder_sequence_index":0}},` +
		`{"id":"goal","type":"goal","config":{}}]`

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(click_journey_id,''), COALESCE((SELECT o.name`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "offer_name"}).
			AddRow("click-drip-4touch-72h", "Tahiti Village Resort"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journeys WHERE id=`)).
		WithArgs("click-drip-4touch-72h").
		WillReturnRows(sqlmock.NewRows([]string{"nodes"}).AddRow([]byte(nodes)))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_execution_log l`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{"node_id", "reached", "errs"}).
			AddRow("email-0", 100, 3))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_enrollments
		WHERE enrollment_offer_id=`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{"current_node_id", "count"}).AddRow("delay-1h", 7))
	// loadNodeEngagement -> nodeCampaigns. That first probes for the attribution
	// columns (present here), then (reader disabled) takes the PG path.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM information_schema.columns`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text, journey_node_id`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{"id", "journey_node_id"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", "email-0"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_campaigns c
		LEFT JOIN mailing_tracking_events t`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{
			"journey_node_id", "sent", "delivered", "opens", "clicks", "unsubs", "hard", "soft",
		}).AddRow("email-0", 100, 95, 20, 5, 1, 2, 3))
	mock.ExpectQuery(regexp.QuoteMeta(`WITH conv AS`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{"node_id", "count"}).AddRow("email-0", 2))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_reminder_subjects`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{
			"sequence_index", "subject", "preheader", "from_name_override", "enabled",
			"body_html", "updated_at",
		}).AddRow(0, "Still interested?", "one more look", "", true, "", time.Now()))
	// Lane outcome split: enrolled / active / converted(postback) /
	// completed(sequence) / exited(early) / median hours-to-goal.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_clickdrip_touch_versions`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{
			"node_id", "content_hash", "subject", "preheader", "from_name_override",
			"body_html", "shadow_campaign_id", "first_seen_at", "last_seen_at", "superseded_at",
		}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*),`)).
		WithArgs("6137").
		WillReturnRows(sqlmock.NewRows([]string{
			"c", "active", "converted", "completed", "exited", "median_hours",
		}).AddRow(120, 7, 2, 40, 71, 0.4591))

	svc := NewClickFunnelsService(db)
	r := chi.NewRouter()
	r.Get("/api/mailing/click-funnels/{offerID}/nodes", svc.HandleFunnelNodes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mailing/click-funnels/6137/nodes", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp ClickFunnelNodesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.OfferName != "Tahiti Village Resort" {
		t.Fatalf("offer_name = %q, want the resolved name (the screen leads with it, not the id)", resp.OfferName)
	}
	if resp.EngagementSource != "pg-fallback" {
		t.Fatalf("engagement_source = %q, want pg-fallback when the lake reader is off", resp.EngagementSource)
	}
	if !strings.Contains(resp.AttributionNote, "LAKE UNAVAILABLE") {
		t.Fatalf("the note must disclose the fallback, got %q", resp.AttributionNote)
	}
	if resp.WindowFrom == "" || resp.WindowTo == "" {
		t.Fatal("response must state the window it measured")
	}

	// Conversions must be the postback count, and completions must stay a
	// SEPARATE number — conflating them is the ~81x overstatement.
	if resp.TotalConverted != 2 {
		t.Fatalf("total_converted = %d, want 2 (converted_at, not status='converted')", resp.TotalConverted)
	}
	if resp.TotalCompleted != 40 {
		t.Fatalf("total_completed = %d, want 40 (sequence completion, reported separately)", resp.TotalCompleted)
	}
	if resp.TotalCompleted == resp.TotalConverted {
		t.Fatal("completed and converted must never be the same field")
	}
	if resp.TotalExited != 71 {
		t.Fatalf("total_exited = %d, want 71 (exits before completion)", resp.TotalExited)
	}
	if resp.MedianHoursToConvert == nil || *resp.MedianHoursToConvert != round2(0.4591) {
		t.Fatalf("median_hours_to_convert = %v, want %v", resp.MedianHoursToConvert, round2(0.4591))
	}
	if resp.ConversionRate != laneRate(2, 120) || resp.CompletionRate != laneRate(40, 120) {
		t.Fatalf("lane rates wrong: conv=%v completion=%v", resp.ConversionRate, resp.CompletionRate)
	}

	var email *ClickFunnelNode
	for i := range resp.Nodes {
		if resp.Nodes[i].NodeID == "email-0" {
			email = &resp.Nodes[i]
		}
	}
	if email == nil {
		t.Fatal("email-0 missing from the node list")
	}
	if email.Subject != "Still interested?" {
		t.Fatalf("per-touch copy not surfaced: %q", email.Subject)
	}
	if email.Reached != 100 || email.Errors != 3 {
		t.Fatalf("flow wrong: reached=%d errors=%d", email.Reached, email.Errors)
	}
	// Rates are computed on delivered (95), not sent.
	if email.OpenRate != round2(20.0/95.0*100) {
		t.Fatalf("open_rate = %v, want opens/delivered", email.OpenRate)
	}
	if email.ClickRate != round2(5.0/95.0*100) {
		t.Fatalf("click_rate = %v, want clicks/delivered", email.ClickRate)
	}
	if email.Conversions != 2 {
		t.Fatalf("node conversions = %d, want 2 (last-touch attributed)", email.Conversions)
	}
	if !email.Attributed {
		t.Fatal("a node with engagement rows must be marked attributed")
	}

	// A delay node must carry its wait, and the goal node must not be an email.
	var delay *ClickFunnelNode
	for i := range resp.Nodes {
		if resp.Nodes[i].NodeID == "delay-1h" {
			delay = &resp.Nodes[i]
		}
	}
	if delay == nil || delay.DelayMs != 3600000 {
		t.Fatalf("delay node wrong: %+v", delay)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestFunnelNodes_UnknownOffer404s keeps a typo'd offer from rendering an empty
// funnel that looks like "this lane has no activity".
func TestFunnelNodes_UnknownOffer404s(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("nope").
		WillReturnError(sqlErrNoRows())

	svc := NewClickFunnelsService(db)
	r := chi.NewRouter()
	r.Get("/f/{offerID}/nodes", svc.HandleFunnelNodes)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/f/nope/nodes", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------- lane list

// TestListFunnels_SurfacesOrphanInlets: an enabled money-slug whose offer has no
// journey row drops every click it receives with no other trace. The list must
// surface it — that is the only place the operator can see it.
func TestListFunnels_SurfacesOrphanInlets(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map m`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"everflow_offer_id", "offer_name", "click_journey_id", "name", "enabled", "payout_type",
			"routing_state", "redirect_offer_id", "routing_recommendation",
			"slug_inlets", "active_enrollments", "enrolled_30d", "conversions_30d",
			"touches_30d", "configured_touches",
		}).AddRow("6137", "Tahiti Village Resort", "click-drip-4touch-72h", "Click-Drip 4-Touch", true, "CPM",
			"active", "", "active", 1, 200, 1011, 0, 3000, 4))

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_slug_map s`)).
		WillReturnRows(sqlmock.NewRows([]string{"everflow_offer_id"}).AddRow("1054"))

	svc := NewClickFunnelsService(db)
	rec := httptest.NewRecorder()
	svc.HandleListFunnels(rec, httptest.NewRequest(http.MethodGet, "/api/mailing/click-funnels", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp ClickFunnelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Lanes) != 1 || resp.Lanes[0].OfferID != "6137" {
		t.Fatalf("lanes wrong: %+v", resp.Lanes)
	}
	if resp.Lanes[0].OfferName != "Tahiti Village Resort" {
		t.Fatalf("offer_name = %q, want the resolved name", resp.Lanes[0].OfferName)
	}
	if len(resp.Orphans) != 1 || resp.Orphans[0] != "1054" {
		t.Fatalf("orphan inlets must be surfaced, got %v", resp.Orphans)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

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

// TestLaneRate guards the tile math: a zero denominator must yield 0, never NaN
// (NaN serializes as invalid JSON and blanks the whole response).
func TestLaneRate(t *testing.T) {
	if got := laneRate(0, 0); got != 0 {
		t.Fatalf("laneRate(0,0) = %v, want 0", got)
	}
	if got := laneRate(5, 0); got != 0 {
		t.Fatalf("laneRate(5,0) = %v, want 0", got)
	}
	if got := laneRate(1, 4); got != 25 {
		t.Fatalf("laneRate(1,4) = %v, want 25", got)
	}
}

// TestDelayMillis pins the graph-shape bug this suite caught: the persisted
// click-drip graph writes {"delayUnit":"hours","delayValue":1}, so reading only
// delay_hours/delay_minutes rendered every wait as blank.
func TestDelayMillis(t *testing.T) {
	mk := func(unit string, val, hours, mins float64) journeyGraphNode {
		var g journeyGraphNode
		g.Config.DelayUnit, g.Config.DelayValue = unit, val
		g.Config.DelayHours, g.Config.DelayMinutes = hours, mins
		return g
	}
	cases := []struct {
		name string
		node journeyGraphNode
		want int64
	}{
		{"hours (production shape)", mk("hours", 1, 0, 0), 3600000},
		{"48h", mk("hours", 48, 0, 0), 172800000},
		{"minutes", mk("minutes", 30, 0, 0), 1800000},
		{"days", mk("days", 2, 0, 0), 172800000},
		{"unit omitted defaults to hours", mk("", 5, 0, 0), 18000000},
		{"legacy snake_case fallback", mk("", 0, 1, 30), 5400000},
		{"no delay", mk("", 0, 0, 0), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.node.delayMillis(); got != tc.want {
				t.Fatalf("delayMillis() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestListFunnels_DegradesWithoutAttributionColumns is the other half of the
// 2026-08-02 incident. The send path was hardened first, but every READ here
// also referenced journey_offer_id — so with the DDL still waiting on its lock,
// sending was perfectly healthy while the screen itself 500'd with
// `column c.journey_offer_id does not exist`. A reporting screen must degrade to
// "no attribution yet", never to an error page.
func TestListFunnels_DegradesWithoutAttributionColumns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// Probe reports the columns ABSENT.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM information_schema.columns`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// The lane list must then run WITHOUT the journey_offer_id subquery.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map m`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"everflow_offer_id", "offer_name", "click_journey_id", "name", "enabled", "payout_type",
			"routing_state", "redirect_offer_id", "routing_recommendation",
			"slug_inlets", "active_enrollments", "enrolled_30d", "conversions_30d",
			"touches_30d", "configured_touches",
		}).AddRow("6137", "Tahiti Village Resort", "click-drip-4touch-72h", "Click-Drip 4-Touch", true, "CPM",
			"active", "", "active", 1, 200, 1011, 0, 0, 4))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_slug_map s`)).
		WillReturnRows(sqlmock.NewRows([]string{"everflow_offer_id"}))

	svc := NewClickFunnelsService(db)
	rec := httptest.NewRecorder()
	svc.HandleListFunnels(rec, httptest.NewRequest(http.MethodGet, "/api/mailing/click-funnels", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("must render without the attribution columns, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp ClickFunnelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Lanes) != 1 {
		t.Fatalf("lanes should still render: %+v", resp.Lanes)
	}
	if resp.Lanes[0].TouchesSent != 0 {
		t.Fatalf("touches_30d must degrade to 0, got %d", resp.Lanes[0].TouchesSent)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestNodeCampaigns_EmptyWithoutAttributionColumns: no attribution columns means
// nothing is node-scoped, which is an empty result — not an error.
func TestNodeCampaigns_EmptyWithoutAttributionColumns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`FROM information_schema.columns`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	svc := NewClickFunnelsService(db)
	got, err := svc.nodeCampaigns(context.Background(), "6137")
	if err != nil {
		t.Fatalf("must not error when the columns are absent: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected an empty map, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestHasAttributionCols_CachesAndRequiresBoth: the probe must require BOTH
// columns (one present is still broken) and must not re-query per request.
func TestHasAttributionCols_CachesAndRequiresBoth(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// Only ONE of the two columns exists → still degraded.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM information_schema.columns`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	svc := NewClickFunnelsService(db)
	if svc.hasAttributionCols(context.Background()) {
		t.Fatal("one column of two must count as ABSENT")
	}
	// Second call inside the TTL must be served from cache (no new query; a
	// stray query would trip ExpectationsWereMet).
	if svc.hasAttributionCols(context.Background()) {
		t.Fatal("cached answer changed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("probe should be cached, not re-run: %v", err)
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
