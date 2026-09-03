package api

// Tests for REQ-118 WP9, the Supply API (drip_supply_handlers.go).
//
// What each block pins, and why it can FAIL:
//
//   - Route registration — a handler that compiles but is never mounted is dead
//     code, the most-shipped bug in this repo. chi.Walk is the proof, and the
//     test asserts the exact URL space, so a renamed or dropped route goes red.
//   - Fail-closed on CONTRACT_TOKEN_KEY — §1.5's "unset = the supply chain
//     stops". Negative control: the LEDGER endpoints must KEEP WORKING with the
//     key unset, so a test that just asserted "503 everywhere" would fail.
//   - Org resolution — the one org-scoped read (mailing_campaigns) must carry
//     the caller's org, and every handler must resolve one.
//   - labels + as_of on every response — a number without its label is a number
//     the screen can misrepresent.
//   - unknown is null, never 0 — the estate strip on an unplanned day.
//   - Validation → 400 with the field list.
//   - The health-colour rule as a pure function, including the negative
//     controls (recovered lane is not red; paused beats red).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// testOrgID is the org header every request in this file carries.
const dripTestOrgID = "11111111-2222-3333-4444-555555555555"

// dripTestKey is a >= 32 byte HMAC key for the tests that need one.
const dripTestKey = "test-contract-token-key-0123456789abcdef"

// dripTestDay is the Denver day every record-flow test keys its cache on.
var dripTestDay = time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

func dripNewMock(t *testing.T) (*DripSupplyService, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewDripSupplyService(db), mock, func() { _ = db.Close() }
}

func dripRequest(t *testing.T, method, target string, body string) *http.Request {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	req.Header.Set("X-Organization-ID", dripTestOrgID)
	return req
}

// dripServe mounts the service on a real chi router so URL params resolve the
// way they do in production — a handler tested by calling it directly never
// proves its {lane} / {kind} / {subject} wiring.
func dripServe(svc *DripSupplyService, req *http.Request) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ─────────────────────────────────────────────────────────────────────────────
// route registration
// ─────────────────────────────────────────────────────────────────────────────

func TestDripSupply_RoutesRegistered(t *testing.T) {
	svc, _, done := dripNewMock(t)
	defer done()

	r := chi.NewRouter()
	svc.RegisterRoutes(r)

	got := map[string]bool{}
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}

	want := []string{
		"GET /supply/health",
		"GET /supply/ecosystem",
		"GET /supply/lanes/{lane}",
		"GET /supply/domains",
		"GET /supply/domains/{domain}",
		"GET /supply/ledger/capacity",
		"GET /supply/ledger/supply",
		"GET /supply/plan",
		"GET /supply/contracts/{kind}/{subject}",
		"POST /supply/contracts/{kind}/{subject}",
		"POST /supply/contracts/{kind}/{subject}/{version}/approve",
		"POST /supply/contracts/{kind}/{subject}/{version}/reject",
		"POST /supply/manual-revenue",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("route %q is NOT registered; registered = %v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("route count drift: registered %d, documented %d (%v)", len(got), len(want), got)
	}
	// This surface projects and configures. It never triggers a send.
	for route := range got {
		low := strings.ToLower(route)
		for _, banned := range []string{"deploy", "send", "enqueue", "promote", "claim"} {
			if strings.Contains(low, banned) {
				t.Errorf("route %q looks like a send trigger — WP9 is a projection + contract surface only", route)
			}
		}
	}
}

// The registration must be reachable at /api/mailing/supply/* — the prefix the
// portal's apiFetch calls. Mounting under a differently named group would pass
// the walk above and still 404 in production.
func TestDripSupply_RegisteredUnderMailingGroup(t *testing.T) {
	svc, _, done := dripNewMock(t)
	defer done()

	root := chi.NewRouter()
	root.Route("/api", func(ar chi.Router) {
		ar.Route("/mailing", func(mr chi.Router) { svc.RegisterRoutes(mr) })
	})
	found := false
	_ = chi.Walk(root, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == "GET" && route == "/api/mailing/supply/health" {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("GET /api/mailing/supply/health is not reachable under the /api/mailing group")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// fail-closed on the contract key (§1.5) — with its negative control
// ─────────────────────────────────────────────────────────────────────────────

func TestDripSupply_ContractProjectingEndpointsFailClosedWithoutKey(t *testing.T) {
	t.Setenv(contractmeta.KeyEnvVar, "")

	for _, tc := range []struct{ name, method, target, body string }{
		{"ecosystem", http.MethodGet, "/supply/ecosystem", ""},
		{"lane", http.MethodGet, "/supply/lanes/wcl_remail", ""},
		{"approve", http.MethodPost, "/supply/contracts/dispatch/wcl_remail/3/approve", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, mock, done := dripNewMock(t)
			defer done()

			w := dripServe(svc, dripRequest(t, tc.method, tc.target, tc.body))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("want 503 with %s unset, got %d body=%s", contractmeta.KeyEnvVar, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), contractmeta.KeyEnvVar) {
				t.Errorf("the 503 must name %s so the operator knows the one-move fix; got %s", contractmeta.KeyEnvVar, w.Body.String())
			}
			// Fail closed means fail BEFORE touching the database. sqlmock has
			// no expectations set, so any query would already have errored;
			// this asserts none was even attempted.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a fail-closed refusal must execute ZERO SQL: %v", err)
			}
		})
	}
}

// NEGATIVE CONTROL for the test above: the ledger endpoints must keep serving
// with the key unset. An operator whose key is missing still has to be able to
// see the estate — that is exactly when they need to.
func TestDripSupply_LedgerEndpointsWorkWithoutKey(t *testing.T) {
	t.Setenv(contractmeta.KeyEnvVar, "")
	svc, mock, done := dripNewMock(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM drip_daily_plan").
		WillReturnRows(sqlmock.NewRows([]string{
			"lane", "isp", "sending_domain", "award_firm", "award_provisional", "followups_reserved",
			"plan_share", "rank", "rank_reason", "unserved", "unserved_reason", "supply_released", "frozen_at",
		}))
	mock.ExpectRollback()

	w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/plan?day=2026-09-03", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for /supply/plan with no contract key, got %d body=%s", w.Code, w.Body.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// /supply/health — sqlmock, org resolution, unknown-is-null
// ─────────────────────────────────────────────────────────────────────────────

// dripExpectHealth queues the five health reads. balanceRows/laneRows drive the
// "no rows for the day" case.
func dripExpectHealth(mock sqlmock.Sqlmock, balanceCells, laneCells int64) {
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM drip_capacity_balance").
		WillReturnRows(sqlmock.NewRows([]string{"count", "contracted", "effective", "reserved", "committed", "max"}).
			AddRow(balanceCells, 120000, 90000, 40000, 35000, time.Now()))
	mock.ExpectQuery("FROM drip_lane_balance").
		WillReturnRows(sqlmock.NewRows([]string{"count", "desired", "unfilled"}).
			AddRow(laneCells, 150000, 20000))
	mock.ExpectQuery("FROM drip_supply_ledger").
		WillReturnRows(sqlmock.NewRows([]string{"count", "spend"}).AddRow(int64(3), 12.5))
	mock.ExpectQuery("FROM drip_capacity_ledger").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(time.Now()))
	mock.ExpectQuery("FROM partner_clean_queue").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(7)))
	mock.ExpectRollback()
}

func TestDripSupply_Health(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()
	dripExpectHealth(mock, 26, 40)

	w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/health?day=2026-09-03", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var got dripHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if got.Day != "2026-09-03" {
		t.Errorf("day = %q, want 2026-09-03", got.Day)
	}
	if got.AsOf.IsZero() {
		t.Error("as_of must be set on every response (§3)")
	}
	if len(got.Labels) == 0 {
		t.Error("labels must be set on every response (§3)")
	}
	if got.Estate.Contracted == nil || *got.Estate.Contracted != 120000 {
		t.Errorf("contracted = %v, want 120000", got.Estate.Contracted)
	}
	if got.Estate.StrandedClaims == nil || *got.Estate.StrandedClaims != 7 {
		t.Errorf("stranded_claims = %v, want 7", got.Estate.StrandedClaims)
	}
	if got.Freshness.Max == nil {
		t.Error("freshness.max must be the newer of the two ledgers")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The rule that keeps a screen honest: a day with no rows is UNKNOWN, not zero.
func TestDripSupply_HealthUnknownIsNullNotZero(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()
	dripExpectHealth(mock, 0, 0)

	w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/health?day=2026-09-03", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	// Assert on the RAW JSON: a Go zero value and a JSON null are the same
	// after decoding into an int, which is exactly the bug this guards.
	body := w.Body.String()
	for _, field := range []string{`"contracted":null`, `"effective":null`, `"desired":null`, `"unfilled":null`} {
		if !strings.Contains(body, field) {
			t.Errorf("with no balance rows the estate strip must carry %s; got %s", field, body)
		}
	}
	var got dripHealthResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Degraded) == 0 {
		t.Error("a null must be accompanied by a degraded note so the UI can say WHY it is unknown")
	}
}

// Every handler resolves an org, and the org-scoped read carries it. This is
// the org-isolation proof for the one query in the file that is org-scoped.
func TestDripSupply_OrgReachesTheOrgScopedQuery(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	orgID := uuid.MustParse(dripTestOrgID)
	lanes := []string{"wcl_remail"}
	mock.ExpectQuery("FROM mailing_campaigns").
		WithArgs(orgID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"lane", "sent"}).AddRow("wcl_remail", int64(4200)))

	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	got, err := dripSentTodayByLane(context.Background(), svc.db, day, orgID, lanes)
	if err != nil {
		t.Fatalf("dripSentTodayByLane: %v", err)
	}
	if got["wcl_remail"] != 4200 {
		t.Errorf("sent today = %v, want 4200", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the org must be bound as a parameter, not omitted: %v", err)
	}
}

// A lane the caller did not ask about must not leak into the answer, even
// though the SQL groups every drip campaign in the window.
func TestDripSupply_SentTodayIgnoresUnrequestedLanes(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	mock.ExpectQuery("FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"lane", "sent"}).
			AddRow("wcl_remail", int64(10)).
			AddRow("some_other_lane", int64(999)))

	got, err := dripSentTodayByLane(context.Background(), svc.db,
		time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), uuid.MustParse(dripTestOrgID), []string{"wcl_remail"})
	if err != nil {
		t.Fatalf("dripSentTodayByLane: %v", err)
	}
	if _, leaked := got["some_other_lane"]; leaked {
		t.Errorf("a lane outside the requested set leaked into the result: %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// /supply/domains and /supply/ledger/capacity — one handler per remaining group
// ─────────────────────────────────────────────────────────────────────────────

func TestDripSupply_Domains(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM drip_capacity_balance").
		WillReturnRows(sqlmock.NewRows([]string{
			"sending_domain", "cells", "contracted", "effective", "reserved", "committed",
			"released", "reason", "refill", "ct_version", "health_band", "ramp_stage",
		}).AddRow("em.historythinking.com", int64(9), int64(17000), int64(12000), int64(8000), int64(4000), int64(0),
			"throttle", time.Now(), int64(12), "green", "mature"))
	mock.ExpectRollback()

	w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/domains?day=2026-09-03", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var got dripDomainsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Domains) != 1 {
		t.Fatalf("want 1 domain row, got %d", len(got.Domains))
	}
	d := got.Domains[0]
	if d.Remaining == nil || *d.Remaining != 0 {
		t.Errorf("remaining = %v, want 0 (effective 12000 - reserved 8000 - committed 4000)", d.Remaining)
	}
	if d.Status != "met" {
		t.Errorf("status = %q, want met when nothing remains", d.Status)
	}
	if d.EffectiveReason != "throttle" {
		t.Errorf("effective_reason must be projected so the operator sees WHICH governor bound it, got %q", d.EffectiveReason)
	}
	if len(got.Labels) == 0 || got.AsOf.IsZero() {
		t.Error("labels + as_of required on every response")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDripSupply_CapacityLedgerFiltersAreBoundParameters(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	// The filters must arrive as PARAMETERS. A handler that interpolated them
	// would not match these WithArgs.
	mock.ExpectQuery("FROM drip_capacity_ledger").
		WithArgs(sqlmock.AnyArg(), "em.historythinking.com", "aol", "wcl_remail", dripSupplyDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"allocation_id", "idempotency_key", "tick", "sending_domain", "isp", "lane", "touch_class",
			"domain_contract_version", "dispatch_contract_version", "requested", "reserved", "committed",
			"released", "status", "campaign_id", "binding_reason", "release_reason",
			"domain_balance_after", "lane_unfilled_after", "created_at", "updated_at",
		}))
	mock.ExpectRollback()

	w := dripServe(svc, dripRequest(t, http.MethodGet,
		"/supply/ledger/capacity?day=2026-09-03&domain=EM.HistoryThinking.com&isp=AOL&lane=WCL_Remail", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("filters must be lowercased and bound as parameters: %v", err)
	}
}

// An unfilterable day is a real 400, not a silent answer for today.
func TestDripSupply_BadDayIsRejected(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/health?day=yesterday", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unparseable day, got %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a rejected day must execute zero SQL: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// /supply/contracts — listing, token redaction, validation
// ─────────────────────────────────────────────────────────────────────────────

func TestDripSupply_ContractVersionsRedactTheToken(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	blk := contractmeta.Block{
		ContractID: uuid.NewString(), Kind: "dispatch", Version: 4,
		Token: contractmeta.Token{
			Alg: contractmeta.AlgHMACSHA256, IssuedAt: time.Now().UTC(),
			IssuedBy: contractmeta.IssuerSystem, Value: "deadbeefcafef00d",
		},
	}
	raw, _ := json.Marshal(blk)

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM drip_dispatch_contracts").
		WithArgs("wcl_remail").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "version", "status", "effective_at", "superseded_at", "created_by", "created_at",
			"approved_by", "approved_at", "change_ledger_id", "notes", "metadata", "token",
		}).AddRow(uuid.NewString(), 4, "active", time.Now(), nil, "ops@x", time.Now(),
			"ops@x", time.Now(), "portal:abc", "", raw, "deadbeefcafef00d"))
	// dripHydrateBodies re-reads the version for its policy body.
	mock.ExpectQuery("FROM drip_dispatch_contracts").WillReturnRows(sqlmock.NewRows([]string{
		"id", "version", "status", "effective_at", "superseded_at", "created_by", "created_at",
		"approved_by", "approved_at", "change_ledger_id", "notes", "metadata", "token",
		"lane", "operator_priority_tier", "desired_daily_intros", "demand_mode", "daily_ceiling",
		"allowed_domains", "isp_exclusions", "ladder_touches", "ladder_gap_hours",
		"followups_committed", "max_intro_share", "exploration_share",
	}).AddRow(uuid.NewString(), 4, "active", time.Now(), nil, "ops@x", time.Now(),
		"ops@x", time.Now(), "portal:abc", "", raw, "deadbeefcafef00d",
		"wcl_remail", 1, []byte(`{"aol":5000}`), "target", nil, "{ht}", "{}", 5, 24, true, 0.40, 0.0))
	mock.ExpectRollback()

	w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/contracts/dispatch/wcl_remail", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "deadbeefcafef00d") {
		t.Errorf("the token VALUE must never be projected — presence only (§WP9); got %s", w.Body.String())
	}
	var got dripContractsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Active == nil || *got.Active != 4 {
		t.Errorf("active_version = %v, want 4", got.Active)
	}
	if len(got.Versions) != 1 || !got.Versions[0].TokenPresent {
		t.Errorf("token_present must be true for an issued token: %+v", got.Versions)
	}
}

func TestDripSupply_UnknownContractKindIsRejected(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/contracts/nonsense/wcl_remail", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown kind, got %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an unknown kind must never reach the database (it selects the TABLE NAME): %v", err)
	}
}

// A draft that would not validate is refused BEFORE a transaction is opened,
// and the 400 carries the exact field list the portal form binds to.
func TestDripSupply_ContractDraftValidationErrorsListFields(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	// A domain contract missing most of the 12 ISP keys and with gmail > 0 and
	// no note — two distinct rules from §1.1.
	body := `{"brand_code":"ht","daily_max_by_isp":{"gmail":500},"active_window_start":"01:00","active_window_end":"20:00","interval_minutes":15,"max_burst_intervals":2}`
	w := dripServe(svc, dripRequest(t, http.MethodPost, "/supply/contracts/domain/em.historythinking.com", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Error  string   `json:"error"`
		Fields []string `json:"fields"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(got.Fields) == 0 {
		t.Fatalf("a validation 400 must name the offending fields; got %s", w.Body.String())
	}
	joined := strings.Join(got.Fields, ",")
	if !strings.Contains(joined, "daily_max_by_isp") {
		t.Errorf("fields %v must name daily_max_by_isp (missing ISP keys / gmail>0 without a note)", got.Fields)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an invalid draft must execute ZERO SQL — validation runs before the transaction: %v", err)
	}
}

// NEGATIVE CONTROL for the test above: a body that IS valid must get past
// validation and reach the database. Without this, a handler that returned 400
// unconditionally would pass the validation test.
func TestDripSupply_ValidContractDraftReachesTheDatabase(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	isps := map[string]int{}
	for _, isp := range dripsupply.ISPClasses() {
		isps[isp] = 1000
	}
	isps["gmail"] = 0
	raw, _ := json.Marshal(map[string]any{
		"brand_code": "ht", "daily_max_by_isp": isps,
		"active_window_start": "01:00", "active_window_end": "20:00",
		"interval_minutes": 15, "max_burst_intervals": 2,
	})

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	// The first thing after the tx opens is ref resolution (§1.5 rule 3).
	mock.ExpectQuery("FROM mailing_sending_profiles").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.NewString()))
	mock.ExpectQuery("mailing_owned_domains").
		WillReturnRows(sqlmock.NewRows([]string{"domain"}).AddRow("historythinking.com"))
	mock.ExpectQuery("COALESCE\\(MAX\\(version\\), 0\\)").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(7))
	mock.ExpectExec("INSERT INTO drip_domain_contracts").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	w := dripServe(svc, dripRequest(t, http.MethodPost, "/supply/contracts/domain/em.historythinking.com", string(raw)))
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201 for a valid draft, got %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Version        int    `json:"version"`
		Status         string `json:"status"`
		ChangeLedgerID string `json:"change_ledger_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != 7 {
		t.Errorf("version = %d, want the NextVersion the database returned (7)", got.Version)
	}
	if got.Status != "draft" {
		t.Errorf("a new contract is a DRAFT, never active: got %q", got.Status)
	}
	if !strings.HasPrefix(got.ChangeLedgerID, "portal:") {
		t.Errorf("change_ledger_id must be the portal audit id 'portal:<uuid>', got %q", got.ChangeLedgerID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// manual revenue
// ─────────────────────────────────────────────────────────────────────────────

func TestDripSupply_ManualRevenueValidationErrorsListFields(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	// Amount 0 and no lane: two rules from InsertManualRevenue.
	body := `{"lane":"","revenue_date":"2026-09-03","attribution_start":"2026-09-01","attribution_end":"2026-09-03","amount":0}`
	w := dripServe(svc, dripRequest(t, http.MethodPost, "/supply/manual-revenue", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Fields []string `json:"fields"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	joined := strings.Join(got.Fields, ",")
	if !strings.Contains(joined, "lane") || !strings.Contains(joined, "amount") {
		t.Errorf("fields must name both broken rules (lane, amount); got %v", got.Fields)
	}
}

func TestDripSupply_ManualRevenueMalformedDatesAreRejectedBeforeSQL(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	body := `{"lane":"wcl_remail","revenue_date":"09/03/2026","attribution_start":"2026-09-01","attribution_end":"2026-09-03","amount":100}`
	w := dripServe(svc, dripRequest(t, http.MethodPost, "/supply/manual-revenue", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "revenue_date") {
		t.Errorf("the 400 must name the malformed field; got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a malformed body must execute zero SQL: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// the ecosystem projection, through its tested seam
// ─────────────────────────────────────────────────────────────────────────────

// dripTestContracts builds an ActiveSet directly, so the projection can be
// tested without minting HMAC tokens (LoadActiveWithKey's job, covered by WP2).
func dripTestContracts(day time.Time) *dripsupply.ActiveSet {
	dc := &dripsupply.DispatchContract{
		Lane: "wcl_remail", OperatorPriorityTier: 1, DemandMode: "target",
		DesiredDailyIntros: map[string]int{"aol": 5000, "yahoo": 3000},
		AllowedDomains:     []string{"ht"},
	}
	dc.Meta.Version = 4
	inv := &dripsupply.InventoryContract{Lane: "wcl_remail", VerdictValidDays: 60, RemailEnabled: false}
	inv.Meta.Version = 2
	return &dripsupply.ActiveSet{
		Day:           day,
		Domains:       map[string]*dripsupply.DomainContract{},
		Dispatches:    map[string]*dripsupply.DispatchContract{"wcl_remail": dc},
		Inventories:   map[string]*dripsupply.InventoryContract{"wcl_remail": inv},
		SourcesBySlug: map[string]*dripsupply.SourceContract{},
	}
}

func TestDripSupply_EcosystemProjection(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery("FROM drip_daily_plan").
		WillReturnRows(sqlmock.NewRows([]string{
			"lane", "rank", "rank_reason", "firm", "prov", "fups", "unserved", "unserved_reason", "released", "frozen",
		}).AddRow("wcl_remail", int64(1), "tier 1 · supply available", int64(4000), int64(500), int64(1200), int64(500), "supply", int64(0), time.Now()))
	mock.ExpectQuery("FROM drip_lane_balance").
		WillReturnRows(sqlmock.NewRows([]string{"lane", "desired", "firm", "prov", "reserved", "committed", "unfilled"}).
			AddRow("wcl_remail", int64(8000), int64(4000), int64(500), int64(4500), int64(4000), int64(3500)))
	mock.ExpectQuery("FROM drip_capacity_ledger").
		WillReturnRows(sqlmock.NewRows([]string{"lane", "binding"}).AddRow("wcl_remail", "supply"))
	mock.ExpectQuery("FROM drip_tick_outcomes").
		WillReturnRows(sqlmock.NewRows([]string{"lane", "tick", "bad", "reason"}).
			AddRow("wcl_remail", time.Now(), false, ""))
	// fresh, remail (skipped: contract disables it), pending EO, follow-ups
	mock.ExpectQuery("status = 'ready'").
		WillReturnRows(sqlmock.NewRows([]string{"vertical", "n"}).AddRow("wcl_remail", int64(9000)))
	mock.ExpectQuery("pending_eo").
		WillReturnRows(sqlmock.NewRows([]string{"vertical", "n"}).AddRow("wcl_remail", int64(1500)))
	mock.ExpectQuery("next_touch_at").
		WillReturnRows(sqlmock.NewRows([]string{"vertical", "n"}).AddRow("wcl_remail", int64(1200)))
	mock.ExpectQuery("FROM drip_supply_ledger").
		WillReturnRows(sqlmock.NewRows([]string{"lane", "qty", "cost"}).AddRow("wcl_remail", int64(20000), 4.88))
	mock.ExpectQuery("FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"lane", "sent"}).AddRow("wcl_remail", int64(4000)))
	mock.ExpectQuery("partner_datasets").
		WillReturnRows(sqlmock.NewRows([]string{"lane", "dataset", "roster"}).AddRow("wcl_remail", true, true))
	mock.ExpectQuery("FROM drip_lane_economics").
		WillReturnRows(sqlmock.NewRows([]string{"lane", "messages", "conversions", "revenue", "send_cost"}).
			AddRow("wcl_remail", int64(50000), int64(30), 480.0, 5.0))

	got, err := svc.ecosystem(context.Background(), svc.db, day, uuid.MustParse(dripTestOrgID), dripTestContracts(day))
	if err != nil {
		t.Fatalf("ecosystem: %v", err)
	}
	if len(got.Lanes) != 1 {
		t.Fatalf("want 1 lane row, got %d", len(got.Lanes))
	}
	row := got.Lanes[0]
	if row.Rank == nil || *row.Rank != 1 {
		t.Errorf("rank = %v, want 1", row.Rank)
	}
	if row.Tier != 1 {
		t.Errorf("tier = %d, want the dispatch contract's 1", row.Tier)
	}
	if row.Demand.SupplyBacked == nil || *row.Demand.SupplyBacked != 4500 {
		t.Errorf("supply_backed = %v, want firm+provisional = 4500", row.Demand.SupplyBacked)
	}
	if row.Demand.UnservedReason != "supply" {
		t.Errorf("unserved_reason = %q, want supply", row.Demand.UnservedReason)
	}
	if row.BindingConstraint != "supply" {
		t.Errorf("binding_constraint = %q, want supply", row.BindingConstraint)
	}
	if row.RemailEligible != nil {
		t.Errorf("remail_eligible must be null for a lane whose inventory contract disables remail, got %v", *row.RemailEligible)
	}
	if row.FillRate == nil || *row.FillRate != 0.5 {
		t.Errorf("fill_rate = %v, want committed/desired = 4000/8000", row.FillRate)
	}
	if row.Health != dripHealthAmber {
		t.Errorf("health = %q, want amber at a 50%% fill rate", row.Health)
	}
	if row.DispatchValue.Maturity != "mature" {
		t.Errorf("maturity = %q, want mature at 50k messages", row.DispatchValue.Maturity)
	}
	if row.DispatchVersion != 4 || row.InventoryVersion != 2 {
		t.Errorf("contract versions must be projected: dispatch=%d inventory=%d", row.DispatchVersion, row.InventoryVersion)
	}
	if len(got.Labels) == 0 || got.AsOf.IsZero() {
		t.Error("labels + as_of required on every response")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// pure rules
// ─────────────────────────────────────────────────────────────────────────────

func TestDripSupply_HealthColourRule(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		paused  bool
		desired int
		fill    *float64
		tick    dripTickHealth
		want    string
	}{
		{"paused beats everything", true, 5000, f(0.1), dripTickHealth{ConsecutiveBad: 2}, dripHealthGrey},
		{"two bad ticks with demand is red", false, 5000, f(0.95), dripTickHealth{ConsecutiveBad: 2}, dripHealthRed},
		{"two bad ticks with NO demand is not red", false, 0, nil, dripTickHealth{ConsecutiveBad: 2}, dripHealthGreen},
		{"one bad tick is not red", false, 5000, f(0.95), dripTickHealth{ConsecutiveBad: 1}, dripHealthGreen},
		{"below 80% fill is amber", false, 5000, f(0.79), dripTickHealth{}, dripHealthAmber},
		{"exactly 80% fill is green", false, 5000, f(0.80), dripTickHealth{}, dripHealthGreen},
		{"demand with no fill number is amber", false, 5000, nil, dripTickHealth{}, dripHealthAmber},
		{"no demand and no fill is green", false, 0, nil, dripTickHealth{}, dripHealthGreen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := dripHealthColour(tc.paused, tc.desired, tc.fill, tc.tick)
			if got != tc.want {
				t.Errorf("health = %q, want %q (reason %q)", got, tc.want, reason)
			}
			if got != dripHealthGreen && reason == "" {
				t.Error("a non-green colour must carry a reason — a colour with no explanation is not actionable")
			}
		})
	}
}

func TestDripSupply_FillRateUndefinedWithoutDemand(t *testing.T) {
	if got := dripFillRate(0, 0); got != nil {
		t.Errorf("fill rate against zero demand must be null (undefined), got %v", *got)
	}
	if got := dripFillRate(50, 100); got == nil || *got != 0.5 {
		t.Errorf("fill rate = %v, want 0.5", got)
	}
}

// Every labels map this file builds must draw only from the §3 vocabulary — a
// label the UI does not understand still reads as authoritative.
func TestDripSupply_LabelsUseTheFixedVocabulary(t *testing.T) {
	maps := map[string]map[string]string{
		"health":    dripHealthLabels(),
		"ecosystem": dripEcosystemLabels(),
		"lane":      dripLaneLabels(),
		"domain":    dripDomainLabels(),
	}
	for name, m := range maps {
		if len(m) == 0 {
			t.Errorf("%s labels map is empty — the response would carry no labels", name)
		}
		for field, label := range m {
			if !dripLabelVocab[label] {
				t.Errorf("%s.%s has label %q, which is not in the §3 vocabulary %v", name, field, label, dripLabelVocab)
			}
		}
	}
}

// The stranded-claim count must stay byte-compatible with the reap predicate,
// or the screen and the reaper disagree about what an orphan is — and the query
// falls off idx_pcq_reap_orphans onto a 13.7M-row seq scan.
func TestDripSupply_StrandedClaimsMatchesTheReapPredicate(t *testing.T) {
	for _, frag := range []string{
		"status = 'claimed'",
		"last_touch_campaign_id IS NULL",
		"mailed_campaign_id IS NULL",
		"claimed_at < NOW() - make_interval",
	} {
		if !strings.Contains(dripStrandedClaimsSQL, frag) {
			t.Errorf("dripStrandedClaimsSQL lost %q — it no longer matches idx_pcq_reap_orphans' predicate", frag)
		}
	}
	if strings.Contains(dripStrandedClaimsSQL, "subscriber_id") {
		t.Error("the reap predicate keys on the two campaign columns, never subscriber_id (that is the OLD janitor's shape)")
	}
	if dripSupplyStrandedAge != dripsupply.DefaultReapAge {
		t.Errorf("stranded age %s must equal dripsupply.DefaultReapAge %s", dripSupplyStrandedAge, dripsupply.DefaultReapAge)
	}
}

// Every read must be time-bounded. A read path that forgot SET LOCAL would run
// under the pool's default and could hold a backend for 30s.
func TestDripSupply_ReadsAreTimeBounded(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout = '20s'").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	tx, err := svc.readTx(context.Background())
	if err != nil {
		t.Fatalf("readTx: %v", err)
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("every read transaction must SET LOCAL statement_timeout = '20s': %v", err)
	}
}

// A contract takes effect at the NEXT Denver midnight, never immediately (§0).
func TestDripSupply_DraftEffectiveAtIsNextDenverMidnight(t *testing.T) {
	loc := dripSupplyDenverLoc()
	now := time.Date(2026, 9, 3, 14, 30, 0, 0, loc)
	got := dripNextDenverMidnight(now)
	want := time.Date(2026, 9, 4, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("effective_at = %s, want %s", got, want)
	}
	if !got.After(now) {
		t.Error("a contract must never take effect in the past")
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// WP10 gap-closure tests
// ═════════════════════════════════════════════════════════════════════════════

// ── (1) policy body on every contract version ───────────────────────────────

// dripContractListRows is the meta listing sqlmock returns for one version.
func dripContractListRows(version int, status string) *sqlmock.Rows {
	blk := contractmeta.Block{ContractID: uuid.NewString(), Version: version,
		Token: contractmeta.Token{Alg: contractmeta.AlgHMACSHA256, IssuedAt: time.Now().UTC(),
			IssuedBy: contractmeta.IssuerSystem, Value: "abc123"}}
	raw, _ := json.Marshal(blk)
	return sqlmock.NewRows([]string{
		"id", "version", "status", "effective_at", "superseded_at", "created_by", "created_at",
		"approved_by", "approved_at", "change_ledger_id", "notes", "metadata", "token",
	}).AddRow(uuid.NewString(), version, status, time.Now(), nil, "ops@x", time.Now(),
		"ops@x", time.Now(), "portal:abc", "", raw, "abc123")
}

// The editor cannot prefill without the policy body, and it must arrive for
// EVERY kind — a kind whose LoadOne shape drifted would ship a blank form.
func TestDripSupply_ContractVersionsCarryPolicyBodyForEveryKind(t *testing.T) {
	metaCols := []string{"id", "version", "status", "effective_at", "superseded_at", "created_by",
		"created_at", "approved_by", "approved_at", "change_ledger_id", "notes", "metadata", "token"}
	blk := contractmeta.Block{ContractID: uuid.NewString(), Version: 3}
	rawMeta, _ := json.Marshal(blk)

	cases := []struct {
		kind      string
		table     string
		subject   string
		bodyCols  []string
		bodyVals  []driver.Value
		wantField string
	}{
		{"domain", "drip_domain_contracts", "em.historythinking.com",
			[]string{"sending_domain", "brand_code", "daily_max_by_isp", "active_window_start",
				"active_window_end", "interval_minutes", "max_burst_intervals", "ramp_source",
				"health_band", "ramp_stage"},
			[]driver.Value{"em.historythinking.com", "ht", []byte(`{"aol":4900}`), "01:00:00", "20:00:00", 15, 2, nil,
				"amber", "ramp day 17"},
			"daily_max_by_isp"},
		{"dispatch", "drip_dispatch_contracts", "wcl_remail",
			[]string{"lane", "operator_priority_tier", "desired_daily_intros", "demand_mode", "daily_ceiling",
				"allowed_domains", "isp_exclusions", "ladder_touches", "ladder_gap_hours",
				"followups_committed", "max_intro_share", "exploration_share"},
			[]driver.Value{"wcl_remail", 1, []byte(`{"aol":5000}`), "target", nil,
				"{ht}", "{}", 5, 24, true, 0.40, 0.0},
			"desired_daily_intros"},
		{"inventory", "drip_inventory_contracts", "wcl_remail",
			[]string{"lane", "accepted_sources", "verdict_valid_days", "eo_enabled", "max_daily_eo_spend_usd",
				"min_eo_order", "min_coverage_hours", "target_coverage_hours", "max_coverage_hours",
				"remail_enabled", "remail_after_days", "remail_mode", "max_remail_share"},
			[]driver.Value{"wcl_remail", "{wcl-remail}", 60, true, 50.0, 1000, 8, 16, 36, false, 7, "full_ladder", 0.25},
			"accepted_sources"},
		{"source", "drip_source_contracts", "wcl-remail",
			[]string{"source_slug", "record_class", "eligible_isps", "max_daily_intake", "arrival_cadence",
				"validated_on_arrival", "record_max_age_days", "unit_acquisition_cost"},
			[]driver.Value{"wcl-remail", "mortgage", "{aol,yahoo}", nil, "continuous", false, nil, 0.0},
			"record_class"},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			svc, mock, done := dripNewMock(t)
			defer done()

			mock.ExpectBegin()
			mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("FROM " + tc.table).
				WillReturnRows(sqlmock.NewRows(metaCols).AddRow(uuid.NewString(), 3, "active", time.Now(), nil,
					"ops@x", time.Now(), "ops@x", time.Now(), "portal:abc", "", rawMeta, "tok"))
			// dripHydrateBodies re-reads the version through dripsupply.LoadOne.
			mock.ExpectQuery("FROM " + tc.table).
				WillReturnRows(sqlmock.NewRows(append(append([]string{}, metaCols...), tc.bodyCols...)).
					AddRow(append([]driver.Value{uuid.NewString(), 3, "active", time.Now(), nil,
						"ops@x", time.Now(), "ops@x", time.Now(), "portal:abc", "", rawMeta, "tok"}, tc.bodyVals...)...))
			mock.ExpectRollback()

			w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/contracts/"+tc.kind+"/"+tc.subject, ""))
			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
			}
			var got struct {
				Versions []struct {
					Body map[string]any `json:"body"`
				} `json:"versions"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v body=%s", err, w.Body.String())
			}
			if len(got.Versions) != 1 {
				t.Fatalf("want 1 version, got %d", len(got.Versions))
			}
			if got.Versions[0].Body == nil {
				t.Fatalf("%s version carries NO policy body — the editor cannot prefill: %s", tc.kind, w.Body.String())
			}
			if _, ok := got.Versions[0].Body[tc.wantField]; !ok {
				t.Errorf("%s body is missing %q; got keys %v", tc.kind, tc.wantField, got.Versions[0].Body)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

// NEGATIVE CONTROL: the body must not smuggle the token value out. A handler
// that shipped the whole row as `body` would pass the test above and fail here.
func TestDripSupply_PolicyBodyStillRedactsTheToken(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	metaCols := []string{"id", "version", "status", "effective_at", "superseded_at", "created_by",
		"created_at", "approved_by", "approved_at", "change_ledger_id", "notes", "metadata", "token"}
	blk := contractmeta.Block{ContractID: uuid.NewString(), Version: 3,
		Token: contractmeta.Token{Alg: contractmeta.AlgHMACSHA256, IssuedAt: time.Now().UTC(),
			IssuedBy: contractmeta.IssuerSystem, Value: "SUPERSECRETMAC"}}
	rawMeta, _ := json.Marshal(blk)
	meta := []driver.Value{uuid.NewString(), 3, "active", time.Now(), nil, "ops@x", time.Now(),
		"ops@x", time.Now(), "portal:abc", "", rawMeta, "SUPERSECRETMAC"}

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM drip_inventory_contracts").WillReturnRows(sqlmock.NewRows(metaCols).AddRow(meta...))
	mock.ExpectQuery("FROM drip_inventory_contracts").WillReturnRows(
		sqlmock.NewRows(append(append([]string{}, metaCols...),
			"lane", "accepted_sources", "verdict_valid_days", "eo_enabled", "max_daily_eo_spend_usd",
			"min_eo_order", "min_coverage_hours", "target_coverage_hours", "max_coverage_hours",
			"remail_enabled", "remail_after_days", "remail_mode", "max_remail_share")).
			AddRow(append(append([]driver.Value{}, meta...),
				"wcl_remail", "{wcl-remail}", 60, true, 50.0, 1000, 8, 16, 36, false, 7, "full_ladder", 0.25)...))
	mock.ExpectRollback()

	w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/contracts/inventory/wcl_remail", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "SUPERSECRETMAC") {
		t.Errorf("adding `body` must not leak the token value; got %s", w.Body.String())
	}
}

// ── (2) reject ──────────────────────────────────────────────────────────────

func TestDripSupply_RejectMovesVersionToSuperseded(t *testing.T) {
	for _, from := range []string{"draft", "approved", "scheduled"} {
		t.Run("from_"+from, func(t *testing.T) {
			svc, mock, done := dripNewMock(t)
			defer done()

			mock.ExpectBegin()
			mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("SELECT status FROM drip_dispatch_contracts").
				WithArgs("wcl_remail", 3).
				WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(from))
			mock.ExpectQuery("UPDATE drip_dispatch_contracts").
				WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("superseded"))
			mock.ExpectCommit()
			// audit row, best-effort, on the pool after commit
			mock.ExpectExec("INSERT INTO partner_admin_audit_log").WillReturnResult(sqlmock.NewResult(1, 1))

			w := dripServe(svc, dripRequest(t, http.MethodPost, "/supply/contracts/dispatch/wcl_remail/3/reject", ""))
			if w.Code != http.StatusOK {
				t.Fatalf("want 200 rejecting a %s version, got %d body=%s", from, w.Code, w.Body.String())
			}
			var got struct {
				Status         string `json:"status"`
				PreviousStatus string `json:"previous_status"`
				Note           string `json:"note"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if got.Status != "superseded" {
				t.Errorf("status = %q, want superseded", got.Status)
			}
			if got.PreviousStatus != from {
				t.Errorf("previous_status = %q, want %q", got.PreviousStatus, from)
			}
			if !strings.HasPrefix(got.Note, "rejected by ") {
				t.Errorf("note must record who rejected it, got %q", got.Note)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("the reject must write an audit row: %v", err)
			}
		})
	}
}

// The forbidden states. `active` is the one that matters: rejecting it would
// leave the subject with no active contract mid-day, which is a hard stop.
func TestDripSupply_RejectForbiddenOnActiveAndSuperseded(t *testing.T) {
	for _, from := range []string{"active", "superseded"} {
		t.Run("from_"+from, func(t *testing.T) {
			svc, mock, done := dripNewMock(t)
			defer done()

			mock.ExpectBegin()
			mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery("SELECT status FROM drip_domain_contracts").
				WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(from))
			// The guarded UPDATE matches nothing — the DATABASE refuses, not the handler.
			mock.ExpectQuery("UPDATE drip_domain_contracts").WillReturnError(sql.ErrNoRows)
			mock.ExpectRollback()

			w := dripServe(svc, dripRequest(t, http.MethodPost,
				"/supply/contracts/domain/em.historythinking.com/9/reject", ""))
			if w.Code != http.StatusConflict {
				t.Fatalf("want 409 rejecting a %s version, got %d body=%s", from, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), from) {
				t.Errorf("the 409 must name the current status so the operator knows why; got %s", w.Body.String())
			}
		})
	}
}

func TestDripSupply_RejectMissingVersionIs404(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT status FROM drip_source_contracts").WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	w := dripServe(svc, dripRequest(t, http.MethodPost, "/supply/contracts/source/wcl-remail/99/reject", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", w.Code, w.Body.String())
	}
}

// The guard belongs in the SQL, not only in a Go branch a concurrent approve
// could race past.
func TestDripSupply_RejectSQLIsGuardedOnStatus(t *testing.T) {
	q, err := dripRejectSQL(dripsupply.KindDispatch)
	if err != nil {
		t.Fatalf("dripRejectSQL: %v", err)
	}
	for _, frag := range []string{"status = 'superseded'", "superseded_at = NOW()", "status = ANY($4)", "RETURNING status"} {
		if !strings.Contains(q, frag) {
			t.Errorf("reject SQL lost %q:\n%s", frag, q)
		}
	}
	if strings.Contains(q, "notes = $1") {
		t.Error("notes must be APPENDED, never overwritten — the record of why a version existed outlives its rejection")
	}
	for _, forbidden := range []string{"active", "superseded"} {
		for _, allowed := range dripRejectableStatuses {
			if allowed == forbidden {
				t.Errorf("%q must never be a rejectable status", forbidden)
			}
		}
	}
	if len(dripRejectableStatuses) != 3 {
		t.Errorf("dripRejectableStatuses = %v, want exactly draft/approved/scheduled", dripRejectableStatuses)
	}
}

// ── (3) record_flow ─────────────────────────────────────────────────────────

func dripFlowRows(pairs map[string]int64, total int64) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"bucket", "n", "median_age_secs"})
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.AddRow(k, pairs[k], 7200.0)
	}
	r.AddRow(nil, total, nil) // the GROUPING SETS grand-total row
	return r
}

func TestDripSupply_RecordFlowBucketsSumToTheLaneTotal(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	buckets := map[string]int64{
		"pending_eo": 1000, "ready_fresh": 5000, "ready_touched": 200,
		"claimed_active": 300, "claimed_orphan": 42,
		"mailed_t1": 4000, "mailed_t2": 3000, "mailed_t3": 1500,
		"cold": 800, "exited": 12, "completed": 2200,
		"suppressed_eo": 600, "dead_letter": 100, "held": 250,
	}
	var total int64
	for _, v := range buckets {
		total += v
	}

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM partner_datasets").WithArgs("wcl_remail").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.NewString()).AddRow(uuid.NewString()))
	mock.ExpectQuery("FROM partner_clean_queue").WillReturnRows(dripFlowRows(buckets, total))
	mock.ExpectRollback()

	flow, note, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay)
	if err != nil {
		t.Fatalf("recordFlow: %v", err)
	}
	if note != "" {
		t.Fatalf("unexpected degraded note: %s", note)
	}
	if flow == nil {
		t.Fatal("record_flow is nil")
	}
	sum := flow.Unclassified
	for _, b := range flow.Buckets {
		sum += b.Count
	}
	if sum != flow.Total || flow.Total != int(total) {
		t.Errorf("buckets sum to %d, flow.Total = %d, scan counted %d — they must agree", sum, flow.Total, total)
	}
	// The sum check above is NOT enough on its own: Unclassified absorbs any
	// bucket that falls out of dripFlowOrder, so the totals would still
	// reconcile while the diagram silently lost a node. Every bucket in this
	// fixture is a NAMED flow bucket, so unclassified must be exactly 0 — that
	// is what turns "a bucket was dropped from the flow" into a red test.
	if flow.Unclassified != 0 {
		t.Errorf("unclassified = %d, want 0 — every fixture bucket is a named flow node; a non-zero value means one fell out of dripFlowOrder", flow.Unclassified)
	}
	// And pin the flow itself. The order IS the diagram: an operator reads it
	// left to right, so a reordering or a missing node is a behaviour change,
	// not a cosmetic one.
	wantOrder := []string{
		"pending_eo", "eo_in_flight", "ready_fresh", "ready_touched",
		"claimed_active", "claimed_orphan",
		"mailed_t1", "mailed_t2", "mailed_t3", "mailed_t4", "mailed_t5",
		"cold", "exited", "completed", "suppressed_eo", "dead_letter", "held",
	}
	if len(flow.Buckets) != len(wantOrder) {
		t.Fatalf("flow has %d nodes, want %d (%v)", len(flow.Buckets), len(wantOrder), wantOrder)
	}
	for i, want := range wantOrder {
		if flow.Buckets[i].Bucket != want {
			t.Errorf("flow position %d = %q, want %q — the flow order is the diagram", i, flow.Buckets[i].Bucket, want)
		}
	}
	// Every dead end the doc names must be drawn as one.
	terminal := map[string]bool{}
	for _, b := range flow.Buckets {
		terminal[b.Bucket] = b.Terminal
	}
	for _, deadEnd := range []string{"claimed_orphan", "cold", "exited", "completed", "suppressed_eo", "dead_letter"} {
		if !terminal[deadEnd] {
			t.Errorf("%q must be drawn as a dead end — nothing leaves it on its own", deadEnd)
		}
	}
	for _, flowing := range []string{"pending_eo", "ready_fresh", "claimed_active", "mailed_t1", "held"} {
		if terminal[flowing] {
			t.Errorf("%q is not a dead end — records move on from it", flowing)
		}
	}
	// Flow order, dead ends and the orphan bucket are what make the diagram readable.
	if flow.Buckets[0].Bucket != "pending_eo" {
		t.Errorf("flow must start at pending_eo, got %q", flow.Buckets[0].Bucket)
	}
	var orphan *dripFlowBucket
	for i := range flow.Buckets {
		if flow.Buckets[i].Orphan {
			orphan = &flow.Buckets[i]
		}
	}
	if orphan == nil || orphan.Bucket != "claimed_orphan" || orphan.Count != 42 || !orphan.Terminal {
		t.Errorf("the orphan bucket must be present, counted and drawn as a dead end: %+v", orphan)
	}
	for _, b := range flow.Buckets {
		if b.Count == 0 && b.MedianAgeHours != nil {
			t.Errorf("bucket %q has no rows but reports a median age", b.Bucket)
		}
		if b.Count > 0 && b.MedianAgeHours == nil {
			t.Errorf("bucket %q has rows but no median age", b.Bucket)
		}
	}
	if flow.Buckets[0].MedianAgeHours == nil || *flow.Buckets[0].MedianAgeHours != 2.0 {
		t.Errorf("median age must be reported in HOURS (7200s = 2h), got %v", flow.Buckets[0].MedianAgeHours)
	}
}

// NEGATIVE CONTROL: a status outside the flow order must be counted as
// unclassified, not dropped — otherwise the sum check above passes vacuously
// while the diagram lies by omission.
func TestDripSupply_RecordFlowCountsUnknownStatusesRatherThanDroppingThem(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	buckets := map[string]int64{"ready_fresh": 100, "suppressed_global": 7, "engaged": 3}

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM partner_datasets").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.NewString()))
	mock.ExpectQuery("FROM partner_clean_queue").WillReturnRows(dripFlowRows(buckets, 110))
	mock.ExpectRollback()

	flow, note, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay)
	if err != nil {
		t.Fatalf("recordFlow: %v", err)
	}
	if note != "" {
		t.Errorf("the sum still reconciles, so there must be no drop warning: %s", note)
	}
	if flow.Unclassified != 10 {
		t.Errorf("unclassified = %d, want 10 (suppressed_global 7 + engaged 3)", flow.Unclassified)
	}
	sum := flow.Unclassified
	for _, b := range flow.Buckets {
		sum += b.Count
	}
	if sum != flow.Total {
		t.Errorf("buckets + unclassified = %d, total = %d", sum, flow.Total)
	}
}

// The degraded path returns NULL, not zeros — and never fails the lane pane.
func TestDripSupply_RecordFlowDegradesToNullNotZero(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		svc, mock, done := dripNewMock(t)
		defer done()

		mock.ExpectBegin()
		mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("FROM partner_datasets").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.NewString()))
		mock.ExpectQuery("FROM partner_clean_queue").
			WillReturnError(&pq.Error{Code: "57014", Message: "canceling statement due to statement timeout"})
		mock.ExpectRollback()

		out := &dripLaneResponse{}
		svc.attachRecordFlow(context.Background(), "wcl_remail", dripTestDay, out)
		if out.RecordFlow != nil {
			t.Errorf("a timed-out record_flow must be null, not a zeroed diagram: %+v", out.RecordFlow)
		}
		if len(out.Degraded) == 0 || !strings.Contains(strings.Join(out.Degraded, " "), "timed out") {
			t.Errorf("a null must carry a degraded note saying why; got %v", out.Degraded)
		}
	})

	t.Run("no active datasets", func(t *testing.T) {
		svc, mock, done := dripNewMock(t)
		defer done()

		mock.ExpectBegin()
		mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("FROM partner_datasets").WillReturnRows(sqlmock.NewRows([]string{"id"}))
		mock.ExpectRollback()

		out := &dripLaneResponse{}
		svc.attachRecordFlow(context.Background(), "wcl_remail", dripTestDay, out)
		if out.RecordFlow != nil {
			t.Errorf("with nothing scanned the flow is unknown, not all-zero: %+v", out.RecordFlow)
		}
		if len(out.Degraded) == 0 {
			t.Error("the null must be explained")
		}
	})

	t.Run("hard error never fails the pane", func(t *testing.T) {
		svc, mock, done := dripNewMock(t)
		defer done()

		mock.ExpectBegin()
		mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("FROM partner_datasets").WillReturnError(errors.New("boom"))
		mock.ExpectRollback()

		out := &dripLaneResponse{Lane: "wcl_remail"}
		svc.attachRecordFlow(context.Background(), "wcl_remail", dripTestDay, out)
		if out.RecordFlow != nil {
			t.Error("record_flow must be null on error")
		}
		if len(out.Degraded) == 0 {
			t.Error("the error must surface as a degraded note, not be swallowed")
		}
	})
}

// ── (4) ramp_stage / health_band ────────────────────────────────────────────

func TestDripSupply_DomainsProjectRampAndBandFromTheActiveContract(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM drip_capacity_balance").
		WillReturnRows(sqlmock.NewRows([]string{
			"sending_domain", "cells", "contracted", "effective", "reserved", "committed",
			"released", "reason", "refill", "ct_version", "health_band", "ramp_stage",
		}).
			AddRow("em.historythinking.com", int64(9), int64(17000), int64(8500), int64(0), int64(0), int64(0), "", time.Now(),
				int64(12), "amber", "ramp day 17").
			// A domain with NO active contract: both fields unknown, and the
			// mediator fails closed on it.
			AddRow("em.quizfiesta.com", int64(9), int64(5000), int64(5000), int64(0), int64(0), int64(0), "", time.Now(),
				nil, nil, nil).
			// An active contract with an empty band resolves to green, matching
			// DomainContract.Band() and the column default.
			AddRow("em.myownhealth.net", int64(9), int64(9000), int64(9000), int64(0), int64(0), int64(0), "", time.Now(),
				int64(3), "", ""))
	mock.ExpectRollback()

	w := dripServe(svc, dripRequest(t, http.MethodGet, "/supply/domains?day=2026-09-03", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var got dripDomainsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Domains) != 3 {
		t.Fatalf("want 3 rows, got %d", len(got.Domains))
	}

	ht := got.Domains[0]
	if ht.HealthBand == nil || *ht.HealthBand != "amber" {
		t.Errorf("health_band = %v, want amber from the active contract", ht.HealthBand)
	}
	if ht.RampStage == nil || *ht.RampStage != "ramp day 17" {
		t.Errorf("ramp_stage = %v, want the contract's free text", ht.RampStage)
	}
	if ht.DomainContractVersion == nil || *ht.DomainContractVersion != 12 {
		t.Errorf("the band must be attributable to a contract VERSION, got %v", ht.DomainContractVersion)
	}

	qf := got.Domains[1]
	if qf.HealthBand != nil || qf.RampStage != nil || qf.DomainContractVersion != nil {
		t.Errorf("with no active contract all three must be null, not defaulted: %+v", qf)
	}
	if !strings.Contains(w.Body.String(), `"health_band":null`) {
		t.Errorf("a domain with no contract must serialise health_band as null; got %s", w.Body.String())
	}

	mh := got.Domains[2]
	if mh.HealthBand == nil || *mh.HealthBand != dripsupply.HealthBandGreen {
		t.Errorf("an empty band on an ACTIVE contract resolves to green (Band()); got %v", mh.HealthBand)
	}
	if mh.RampStage != nil {
		t.Errorf("an empty ramp_stage is unknown, not the empty string: %v", mh.RampStage)
	}

	joined := strings.Join(got.Degraded, " ")
	if !strings.Contains(joined, "drip_domain_contracts") {
		t.Errorf("the response must name where the band came from; degraded = %v", got.Degraded)
	}
	if !strings.Contains(joined, "em.quizfiesta.com") {
		t.Errorf("a domain with no active contract must be named — the mediator skips it; degraded = %v", got.Degraded)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the band must ride the SAME query as the balances — no N+1: %v", err)
	}
}

// NEGATIVE CONTROL: this handler must never COMPUTE a band. If someone wires a
// derivation in, the source note stops being true and the two definitions of
// domain health drift.
func TestDripSupply_DomainsNeverComputeTheBand(t *testing.T) {
	src, err := os.ReadFile("drip_supply_handlers.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	text := string(src)
	i := strings.Index(text, "const dripDomainsSQL")
	j := strings.Index(text[i:], "// HandleDomain GET")
	if i < 0 || j < 0 {
		t.Fatal("could not locate the domains section")
	}
	section := text[i : i+j]
	if !strings.Contains(section, "FROM drip_domain_contracts") {
		t.Error("ramp_stage/health_band must be READ from the active domain contract")
	}
	for _, banned := range []string{"bounce_rate", "complaint_rate", "human_clicks", "mailing_domain_agent_scorecard"} {
		if strings.Contains(section, banned) {
			t.Errorf("the domains handler references %q — it must project the contract's band, never derive one", banned)
		}
	}
	if !strings.Contains(dripBandSourceNote, "drip_domain_contracts") {
		t.Errorf("the source note must name the table it reads: %s", dripBandSourceNote)
	}
}

// The note has to keep naming the wrong turn as well as the right one, so the
// next engineer does not re-investigate the scorecard.
func TestDripSupply_BandSourceNoteNamesBothSources(t *testing.T) {
	for _, frag := range []string{"drip_domain_contracts", "scorecard", "CONTRACT_TOKEN_KEY"} {
		if !strings.Contains(dripBandSourceNote, frag) {
			t.Errorf("dripBandSourceNote must mention %q: %s", frag, dripBandSourceNote)
		}
	}
}

// ── (5) /supply/plan is reconcile tooling ───────────────────────────────────

func TestDripSupply_PlanIsMarkedAsReconcileTooling(t *testing.T) {
	src, err := os.ReadFile("drip_supply_handlers.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	i := strings.Index(string(src), "func (s *DripSupplyService) HandlePlan(")
	if i < 0 {
		t.Fatal("HandlePlan not found")
	}
	doc := string(src)[max(0, i-1600):i]
	for _, frag := range []string{"RECONCILE TOOLING", "NOT A PORTAL SURFACE", "supply_reconcile.py"} {
		if !strings.Contains(doc, frag) {
			t.Errorf("HandlePlan's doc comment must say %q — WP11 owns this view, the portal must not build on it", frag)
		}
	}
}

// ── record_flow cache: TTL, negative caching, single flight ─────────────────

// dripExpectFlowScan queues ONE complete scan (datasets + classification).
// Queueing exactly N of these and then asserting ExpectationsWereMet is how
// these tests COUNT scans: an extra scan surfaces as "call was not expected"
// inside recordFlow, and a missing one leaves an unfulfilled expectation.
func dripExpectFlowScan(mock sqlmock.Sqlmock, buckets map[string]int64, total int64) {
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM partner_datasets").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.NewString()))
	mock.ExpectQuery("FROM partner_clean_queue").WillReturnRows(dripFlowRows(buckets, total))
	mock.ExpectRollback()
}

// Two requests inside the TTL must run ONE scan.
func TestDripSupply_RecordFlowCachedWithinTTL(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	clock := base
	svc.now = func() time.Time { return clock }

	dripExpectFlowScan(mock, map[string]int64{"ready_fresh": 100}, 100)

	first, note, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay)
	if err != nil || note != "" || first == nil {
		t.Fatalf("first call: flow=%v note=%q err=%v", first, note, err)
	}
	if first.CacheAgeSeconds != 0 {
		t.Errorf("a fresh scan must report cache_age_seconds = 0, got %d", first.CacheAgeSeconds)
	}
	if !first.AsOf.Equal(base) {
		t.Errorf("as_of = %s, want the scan time %s", first.AsOf, base)
	}

	// 9 minutes later — still inside the 10 minute TTL.
	clock = base.Add(9 * time.Minute)
	second, note, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay)
	if err != nil || note != "" || second == nil {
		t.Fatalf("second call: flow=%v note=%q err=%v", second, note, err)
	}
	if second.CacheAgeSeconds != 540 {
		t.Errorf("cache_age_seconds = %d, want 540 (9 minutes)", second.CacheAgeSeconds)
	}
	if !second.AsOf.Equal(base) {
		t.Errorf("as_of must stay the SCAN time across cache hits, got %s want %s", second.AsOf, base)
	}
	if second.Total != first.Total {
		t.Errorf("cached total drifted: %d vs %d", second.Total, first.Total)
	}
	// THE COUNT: exactly one scan was queued and exactly one was consumed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("two requests inside the TTL must run exactly ONE scan: %v", err)
	}
}

// NEGATIVE CONTROL: past the TTL a second scan must actually run. Without this
// the test above is satisfied by a cache that never expires.
func TestDripSupply_RecordFlowRescansAfterTTL(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	clock := base
	svc.now = func() time.Time { return clock }

	dripExpectFlowScan(mock, map[string]int64{"ready_fresh": 100}, 100)
	dripExpectFlowScan(mock, map[string]int64{"ready_fresh": 250}, 250)

	if _, _, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay); err != nil {
		t.Fatalf("first call: %v", err)
	}
	clock = base.Add(dripFlowCacheTTL + time.Second)
	second, _, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.Total != 250 {
		t.Errorf("after the TTL the caller must get the RE-SCANNED value (250), got %d", second.Total)
	}
	if second.CacheAgeSeconds != 0 {
		t.Errorf("a re-scan resets the age, got %d", second.CacheAgeSeconds)
	}
	if !second.AsOf.Equal(clock) {
		t.Errorf("as_of must advance to the new scan, got %s want %s", second.AsOf, clock)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("both scans must have run: %v", err)
	}
}

// The lane too big to scan must not be re-scanned on every click.
func TestDripSupply_RecordFlowDegradedResultIsCachedBriefly(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	clock := base
	svc.now = func() time.Time { return clock }

	// ONE timed-out scan queued. A second attempt inside the negative TTL would
	// consume an expectation that does not exist.
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM partner_datasets").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.NewString()))
	mock.ExpectQuery("FROM partner_clean_queue").
		WillReturnError(&pq.Error{Code: "57014", Message: "canceling statement due to statement timeout"})
	mock.ExpectRollback()

	flow, note, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay)
	if err != nil || flow != nil {
		t.Fatalf("a timeout must degrade to a null flow, got flow=%v err=%v", flow, err)
	}
	if !strings.Contains(note, "timed out") {
		t.Fatalf("note = %q, want it to say the scan timed out", note)
	}
	if strings.Contains(note, "cached") {
		t.Errorf("the first, live result must not claim to be cached: %q", note)
	}

	// 90 s later: inside the 2 minute negative TTL — no re-scan.
	clock = base.Add(90 * time.Second)
	flow, note, err = svc.recordFlow(context.Background(), "wcl_remail", dripTestDay)
	if err != nil || flow != nil {
		t.Fatalf("cached degraded result must still be a null flow, got flow=%v err=%v", flow, err)
	}
	if !strings.Contains(note, "cached 90s ago") {
		t.Errorf("a served-from-cache note must say how old it is; got %q", note)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a timed-out lane must NOT be re-scanned inside the negative TTL: %v", err)
	}
}

// NEGATIVE CONTROL for the negative cache: it must be SHORT. An operator has to
// be able to retry once the pressure passes.
func TestDripSupply_RecordFlowRetriesAfterNegativeTTL(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	clock := base
	svc.now = func() time.Time { return clock }

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM partner_datasets").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.NewString()))
	mock.ExpectQuery("FROM partner_clean_queue").
		WillReturnError(&pq.Error{Code: "57014", Message: "canceling statement due to statement timeout"})
	mock.ExpectRollback()
	dripExpectFlowScan(mock, map[string]int64{"ready_fresh": 7}, 7)

	if _, _, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay); err != nil {
		t.Fatalf("first call: %v", err)
	}
	clock = base.Add(dripFlowCacheNegativeTTL + time.Second)
	flow, note, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if flow == nil || flow.Total != 7 {
		t.Errorf("past the negative TTL the retry must actually scan; got flow=%v note=%q", flow, note)
	}
	if dripFlowCacheNegativeTTL >= dripFlowCacheTTL {
		t.Errorf("the negative TTL (%s) must be SHORTER than the success TTL (%s) — a failure is not as reusable as an answer",
			dripFlowCacheNegativeTTL, dripFlowCacheTTL)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("both attempts must have run: %v", err)
	}
}

// Concurrent requests for the same lane share ONE scan. Without single flight a
// page refresh during a slow scan starts a second one, and the fix for load
// becomes a source of it.
func TestDripSupply_RecordFlowSingleFlight(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()
	svc.now = time.Now

	// ONE scan queued, with the classification delayed so the other callers
	// pile up behind it.
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM partner_datasets").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.NewString()))
	mock.ExpectQuery("FROM partner_clean_queue").
		WillDelayFor(150 * time.Millisecond).
		WillReturnRows(dripFlowRows(map[string]int64{"ready_fresh": 42}, 42))
	mock.ExpectRollback()

	const callers = 8
	var wg sync.WaitGroup
	results := make([]*dripRecordFlow, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			f, _, err := svc.recordFlow(context.Background(), "wcl_remail", dripTestDay)
			results[i], errs[i] = f, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] == nil || results[i].Total != 42 {
			t.Errorf("caller %d got %v, want the shared scan's total 42", i, results[i])
		}
	}
	// THE COUNT: 8 concurrent callers, exactly one queued scan, all consumed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("%d concurrent callers must share ONE scan: %v", callers, err)
	}
}

// A waiter must not be stuck behind someone else's scan forever.
func TestDripSupply_RecordFlowWaiterRespectsContext(t *testing.T) {
	svc, _, done := dripNewMock(t)
	defer done()

	// Occupy the slot with an in-flight scan that never completes.
	key := dripFlowCacheKey("wcl_remail", dripTestDay)
	slot, leader := svc.flow.acquire(key)
	if !leader {
		t.Fatal("expected to be the leader on an empty cache")
	}
	_ = slot

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := svc.recordFlow(ctx, "wcl_remail", dripTestDay); err == nil {
		t.Error("a waiter on a cancelled context must return, not block on the leader's scan")
	}
}

// Different lanes and different days are different keys, or one lane's flow
// would be served for another's.
func TestDripSupply_RecordFlowCacheKeyIsLaneAndDay(t *testing.T) {
	d1 := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	if dripFlowCacheKey("a", d1) == dripFlowCacheKey("b", d1) {
		t.Error("two lanes must not share a cache key")
	}
	if dripFlowCacheKey("a", d1) == dripFlowCacheKey("a", d2) {
		t.Error("two days must not share a cache key")
	}

	svc, mock, done := dripNewMock(t)
	defer done()
	svc.now = func() time.Time { return d1 }
	dripExpectFlowScan(mock, map[string]int64{"ready_fresh": 1}, 1)
	dripExpectFlowScan(mock, map[string]int64{"ready_fresh": 2}, 2)

	if _, _, err := svc.recordFlow(context.Background(), "lane_a", d1); err != nil {
		t.Fatalf("lane_a: %v", err)
	}
	// Same instant, different lane: must scan rather than serve lane_a's flow.
	f, _, err := svc.recordFlow(context.Background(), "lane_b", d1)
	if err != nil {
		t.Fatalf("lane_b: %v", err)
	}
	if f.Total != 2 {
		t.Errorf("lane_b got lane_a's cached flow (total %d)", f.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("each key must scan once: %v", err)
	}
}

// The cache must not grow without bound: `lane` comes from a URL path.
func TestDripSupply_RecordFlowCacheIsBounded(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	c := newDripFlowCache(func() time.Time { return now })
	for i := 0; i < dripFlowCacheMaxEntries*2; i++ {
		key := fmt.Sprintf("lane-%d|2026-09-03", i)
		slot, leader := c.acquire(key)
		if !leader {
			t.Fatalf("key %s should be a fresh leader", key)
		}
		c.publish(key, slot, dripFlowResult{at: now, ttl: dripFlowCacheTTL, flow: &dripRecordFlow{}})
	}
	c.mu.Lock()
	n := len(c.slots)
	c.mu.Unlock()
	if n > dripFlowCacheMaxEntries {
		t.Errorf("cache grew to %d entries, cap is %d — `lane` is attacker-supplied", n, dripFlowCacheMaxEntries)
	}
}

// The lane pane must serve the cache age through the HTTP surface, not just
// internally — the operator has to see how old the diagram is.
func TestDripSupply_RecordFlowAgeIsSerialised(t *testing.T) {
	f := &dripRecordFlow{
		Total:   10,
		AsOf:    time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		Buckets: []dripFlowBucket{{Bucket: "ready_fresh", Count: 10}},
	}
	got := f.withAge(f.AsOf.Add(125 * time.Second))
	if got.CacheAgeSeconds != 125 {
		t.Errorf("cache_age_seconds = %d, want 125", got.CacheAgeSeconds)
	}
	if f.CacheAgeSeconds != 0 {
		t.Error("withAge must COPY — stamping the cached value hands the next caller a stale age")
	}
	got.Buckets[0].Count = 999
	if f.Buckets[0].Count != 10 {
		t.Error("withAge must copy the bucket slice — a caller must not be able to mutate the cached flow")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, frag := range []string{`"as_of"`, `"cache_age_seconds":125`} {
		if !strings.Contains(string(raw), frag) {
			t.Errorf("record_flow JSON must carry %s; got %s", frag, raw)
		}
	}
}
