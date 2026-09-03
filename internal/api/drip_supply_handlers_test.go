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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// testOrgID is the org header every request in this file carries.
const dripTestOrgID = "11111111-2222-3333-4444-555555555555"

// dripTestKey is a >= 32 byte HMAC key for the tests that need one.
const dripTestKey = "test-contract-token-key-0123456789abcdef"

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
			"sending_domain", "cells", "contracted", "effective", "reserved", "committed", "released", "reason", "refill",
		}).AddRow("em.historythinking.com", int64(9), int64(17000), int64(12000), int64(8000), int64(4000), int64(0), "throttle", time.Now()))
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
