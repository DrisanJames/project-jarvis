package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

// ─────────────────────────────────────────────────────────────────────────────
// THE LAW TEST. cold-data-drip-pipeline-only-LAW (operator 2026-08-17): cold
// recipients reach production ONLY through dataset → partner ingest (EO) →
// partner_clean_queue → drip orchestrator. A prior agent built a campaign-
// sidecar bypass and burned 88,459 cold sends at 18.1% bounces.
//
// This file's job is to make that law FAIL-CLOSED SOFTWARE rather than a
// comment: every SQL constant in drip_lane_onboarding.go is screened for a
// write to the send path. If someone adds an INSERT into mailing_campaigns or
// partner_clean_queue here, the test run goes red.
// ─────────────────────────────────────────────────────────────────────────────

func TestDripLaneOnboarding_NoSidecarSendPath(t *testing.T) {
	// Tables this service must never WRITE. partner_clean_queue may not even be
	// written read-modify-write: supplier ingest owns it.
	forbiddenWrite := []string{
		"partner_clean_queue",
		"mailing_campaigns",
		"mailing_campaign_queue",
		"mailing_campaign_waves",
		"mailing_segments",
		"mailing_segment_members",
		"mailing_lists",
		"mailing_list_subscribers",
	}
	writeVerb := regexp.MustCompile(`(?is)\b(insert\s+into|update|delete\s+from)\b`)

	consts := dripLaneSQLConstants()
	if len(consts) == 0 {
		t.Fatal("dripLaneSQLConstants() is empty — the screen would pass vacuously")
	}
	for name, sqlText := range consts {
		lower := strings.ToLower(sqlText)
		isWrite := writeVerb.MatchString(lower)
		for _, tbl := range forbiddenWrite {
			if !strings.Contains(lower, tbl) {
				continue
			}
			if isWrite {
				t.Errorf("LAW VIOLATION: %s writes %q. Cold data enters production ONLY via "+
					"partner ingest → partner_clean_queue → the drip orchestrator. "+
					"This service configures orchestrator tables only.", name, tbl)
			}
		}
	}
}

// The service must register exactly the documented URL space. An unregistered
// handler compiles fine and is dead code — the #1 shipped bug in this repo.
func TestDripLaneOnboarding_RoutesRegistered(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	r := chi.NewRouter()
	NewDripLaneOnboardingService(db).RegisterRoutes(r)

	got := map[string]bool{}
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}

	for _, want := range []string{
		"GET /drip-lane/options",
		"GET /drip-lane/verify",
		"POST /drip-lane/onboard",
	} {
		if !got[want] {
			t.Errorf("route %q is NOT registered; registered=%v", want, got)
		}
	}
	// Nothing on this surface may look like a send trigger.
	for route := range got {
		low := strings.ToLower(route)
		for _, banned := range []string{"deploy", "send", "enqueue", "promote"} {
			if strings.Contains(low, banned) {
				t.Errorf("route %q looks like a send path — this service configures orchestrator tables only", route)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GATE-FIRES TESTS. A documented gate that no-ops is worse than none.
// Both write gates must refuse BEFORE any SQL runs — proved by asserting the
// sqlmock had zero expectations and none were consumed.
// ─────────────────────────────────────────────────────────────────────────────

func TestDripLaneOnboard_RefusesWhenFlagUnset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	t.Setenv(dripLaneOnboardFlagEnv, "")
	t.Setenv(laneRosterWriteFlagEnv, "1")

	svc := NewDripLaneOnboardingService(db)
	req := httptest.NewRequest(http.MethodPost, "/drip-lane/onboard",
		strings.NewReader(`{"vertical":"refi_heloc","brand":"rru","offer_id":"11111111-1111-1111-1111-111111111111","touches":4,"confirm":true}`))
	w := httptest.NewRecorder()
	svc.HandleOnboard(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 with %s unset, got %d body=%s", dripLaneOnboardFlagEnv, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), dripLaneOnboardFlagEnv) {
		t.Errorf("403 body must name the env var so the operator knows the one-move enable; got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("gated request must execute ZERO SQL: %v", err)
	}
}

// The roster row changes LIVE sending on the orchestrator's next tick, and the
// Property Ledger already gates that table. This endpoint must not become a
// bypass around that gate.
func TestDripLaneOnboard_RefusesWhenRosterGateUnset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	t.Setenv(dripLaneOnboardFlagEnv, "1")
	t.Setenv(laneRosterWriteFlagEnv, "")

	svc := NewDripLaneOnboardingService(db)
	req := httptest.NewRequest(http.MethodPost, "/drip-lane/onboard",
		strings.NewReader(`{"vertical":"refi_heloc","brand":"rru","offer_id":"11111111-1111-1111-1111-111111111111","touches":4,"confirm":true}`))
	w := httptest.NewRecorder()
	svc.HandleOnboard(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 when %s is unset, got %d body=%s", laneRosterWriteFlagEnv, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), laneRosterWriteFlagEnv) {
		t.Errorf("403 must name the roster gate it refuses to bypass; got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("gated request must execute ZERO SQL: %v", err)
	}
}

// confirm:true is required — this writes live orchestrator configuration.
func TestDripLaneOnboard_RequiresConfirm(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	t.Setenv(dripLaneOnboardFlagEnv, "1")
	t.Setenv(laneRosterWriteFlagEnv, "1")

	svc := NewDripLaneOnboardingService(db)
	req := httptest.NewRequest(http.MethodPost, "/drip-lane/onboard",
		strings.NewReader(`{"vertical":"refi_heloc","brand":"rru","offer_id":"11111111-1111-1111-1111-111111111111","touches":4}`))
	w := httptest.NewRecorder()
	svc.HandleOnboard(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 without confirm, got %d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unconfirmed request must execute ZERO SQL: %v", err)
	}
}

// A follow-up row written at a touch number outside 2..MaxTouchCount is dead
// configuration: the follow-up pass computes MAX(touch_count)+1 and rejects
// anything outside that band (partner_drip_orchestrator.go:4666-4669).
func TestDripLaneOnboard_RejectsLadderBeyondMaxTouch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	t.Setenv(dripLaneOnboardFlagEnv, "1")
	t.Setenv(laneRosterWriteFlagEnv, "1")

	svc := NewDripLaneOnboardingService(db)
	for _, touches := range []int{0, -1, 99} {
		req := httptest.NewRequest(http.MethodPost, "/drip-lane/onboard",
			strings.NewReader(`{"vertical":"refi_heloc","brand":"rru","offer_id":"11111111-1111-1111-1111-111111111111","touches":`+itoaTest(touches)+`,"confirm":true}`))
		w := httptest.NewRecorder()
		svc.HandleOnboard(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("touches=%d: want 400, got %d body=%s", touches, w.Code, w.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("rejected requests must execute ZERO SQL: %v", err)
	}
}

// verify requires a vertical — an unfiltered gate sweep is not a screen this
// surface serves.
func TestDripLaneVerify_RequiresVertical(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewDripLaneOnboardingService(db)
	w := httptest.NewRecorder()
	svc.HandleVerify(w, httptest.NewRequest(http.MethodGet, "/drip-lane/verify", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 without vertical, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("rejected request must execute ZERO SQL: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GATE SEMANTICS. These pin the two readings that would otherwise silently
// invert: an empty budget ledger is UNCONSTRAINED (not "capped at zero"), and
// a partner_clean_queue count that times out is UNKNOWN (not zero).
// ─────────────────────────────────────────────────────────────────────────────

func TestDripLaneVerify_EmptyBudgetLedgerIsWarnNotPass(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM partner_clean_queue WHERE vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1200))
	mock.ExpectQuery(`FROM partner_clean_queue WHERE vertical .* AND status`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(400))
	mock.ExpectQuery(`FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"brand", "active"}).AddRow("rru", true))
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "via_ses", "routing_mode", "tracking_domain"}).
			AddRow("p-1", false, "", "t.em.refinanceratesusa.com"))
	mock.ExpectQuery(`FROM partner_drip_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"creative_filename", "offer_id", "active"}).
			AddRow("welcome.html", "", true))
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "creative_filename", "offer_id", "active", "scoped"}).
			AddRow(2, "t2.html", "", true, true))
	// No budget rows at all.
	mock.ExpectQuery(`FROM partner_drip_brand_budgets`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "daily_budget", "hold", "pending_budget", "pending_effective_day", "lock_version"}))

	svc := NewDripLaneOnboardingService(db)
	w := httptest.NewRecorder()
	svc.HandleVerify(w, httptest.NewRequest(http.MethodGet, "/drip-lane/verify?vertical=refi_heloc&brand=rru", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var res dripLaneVerifyResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	caps := gateByName(t, res, "CAPS")
	if caps.Status != dripGateWarn {
		t.Errorf("an empty ledger is UNCONSTRAINED, not zero — CAPS must WARN, got %q (%s)", caps.Status, caps.Detail)
	}
	if !strings.Contains(strings.ToUpper(caps.Detail), "UNCONSTRAINED") {
		t.Errorf("CAPS detail must say the lane is unconstrained; got %q", caps.Detail)
	}
	if res.Verdict != "WARN" {
		t.Errorf("verdict should be WARN, got %q", res.Verdict)
	}
}

func TestDripLaneVerify_ZeroBudgetCellsFailNotPass(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM partner_clean_queue WHERE vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	mock.ExpectQuery(`FROM partner_clean_queue WHERE vertical .* AND status`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	mock.ExpectQuery(`FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"brand", "active"}).AddRow("rru", true))
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "via_ses", "routing_mode", "tracking_domain"}).
			AddRow("p-1", true, "", "t.em.refinanceratesusa.com"))
	mock.ExpectQuery(`FROM partner_drip_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"creative_filename", "offer_id", "active"}).
			AddRow("welcome.html", "", true))
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "creative_filename", "offer_id", "active", "scoped"}).
			AddRow(2, "t2.html", "", true, true))
	mock.ExpectQuery(`FROM partner_drip_brand_budgets`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "daily_budget", "hold", "pending_budget", "pending_effective_day", "lock_version"}).
			AddRow("gmail", 0, false, nil, nil, int64(0)).
			AddRow("yahoo", 500, true, nil, nil, int64(3)))

	svc := NewDripLaneOnboardingService(db)
	w := httptest.NewRecorder()
	svc.HandleVerify(w, httptest.NewRequest(http.MethodGet, "/drip-lane/verify?vertical=refi_heloc&brand=rru", nil))

	var res dripLaneVerifyResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	caps := gateByName(t, res, "CAPS")
	if caps.Status != dripGateFail {
		t.Errorf("every cell 0-or-held means the lane sends NOTHING — want fail, got %q (%s)", caps.Status, caps.Detail)
	}
	if res.Verdict != "FAIL" {
		t.Errorf("want FAIL verdict, got %q", res.Verdict)
	}
}

// An offer-center touch whose offer is missing any of the three pools must FAIL
// gate 6. A naive COUNT(*) without the archived/rejected predicate passes here
// and then hard-fails at send time — that is the regression this pins.
func TestDripLaneVerify_IncompleteOfferFailsGateSix(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	const offerID = "22222222-2222-2222-2222-222222222222"

	mock.ExpectQuery(`FROM partner_clean_queue WHERE vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))
	mock.ExpectQuery(`FROM partner_clean_queue WHERE vertical .* AND status`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))
	mock.ExpectQuery(`FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"brand", "active"}).AddRow("rru", true))
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "via_ses", "routing_mode", "tracking_domain"}).
			AddRow("p-1", false, "kumo", ""))
	// creative_filename '' + offer_id set == the offer-center path.
	mock.ExpectQuery(`FROM partner_drip_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"creative_filename", "offer_id", "active"}).
			AddRow("", offerID, true))
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "creative_filename", "offer_id", "active", "scoped"}).
			AddRow(2, "", offerID, true, true))
	mock.ExpectQuery(`FROM mailing_offers o`).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "has_creative", "has_subjects", "has_from_names"}).
			AddRow("Sam's Club Summer", "active", true, true, false))
	mock.ExpectQuery(`FROM partner_drip_brand_budgets`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "daily_budget", "hold", "pending_budget", "pending_effective_day", "lock_version"}).
			AddRow("yahoo", 2000, false, nil, nil, int64(1)))

	svc := NewDripLaneOnboardingService(db)
	w := httptest.NewRecorder()
	svc.HandleVerify(w, httptest.NewRequest(http.MethodGet, "/drip-lane/verify?vertical=refi_heloc&brand=rru", nil))

	var res dripLaneVerifyResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	off := gateByName(t, res, "OFFER")
	if off.Status != dripGateFail {
		t.Fatalf("offer missing from_names must FAIL gate 6, got %q (%s)", off.Status, off.Detail)
	}
	if !strings.Contains(off.Detail, "from_names") {
		t.Errorf("gate 6 must name the missing pool; got %q", off.Detail)
	}
	if res.Verdict != "FAIL" {
		t.Errorf("want FAIL verdict, got %q", res.Verdict)
	}
	// The KUMO transport must be visible so a misroute is caught at selection.
	if len(res.Profiles) != 1 || res.Profiles[0].Transport != "KUMO" {
		t.Errorf("routing_mode='kumo' must render as KUMO transport; got %+v", res.Profiles)
	}
}

// A follow-up row at touch 1 is INERT (the follow-up pass only asks for
// 2..MaxTouchCount). verify must say so rather than counting it as a ladder.
func TestDripLaneVerify_TouchOneFollowupIsReportedInert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM partner_clean_queue WHERE vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))
	mock.ExpectQuery(`FROM partner_clean_queue WHERE vertical .* AND status`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))
	mock.ExpectQuery(`FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"brand", "active"}).AddRow("rru", true))
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "via_ses", "routing_mode", "tracking_domain"}).
			AddRow("p-1", false, "", "t.x"))
	mock.ExpectQuery(`FROM partner_drip_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"creative_filename", "offer_id", "active"}).
			AddRow("w.html", "", true))
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "creative_filename", "offer_id", "active", "scoped"}).
			AddRow(1, "t1.html", "", true, true).
			AddRow(2, "t2.html", "", true, true))
	mock.ExpectQuery(`FROM partner_drip_brand_budgets`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "daily_budget", "hold", "pending_budget", "pending_effective_day", "lock_version"}).
			AddRow("yahoo", 100, false, nil, nil, int64(1)))

	svc := NewDripLaneOnboardingService(db)
	w := httptest.NewRecorder()
	svc.HandleVerify(w, httptest.NewRequest(http.MethodGet, "/drip-lane/verify?vertical=refi_heloc&brand=rru", nil))

	var res dripLaneVerifyResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	fu := gateByName(t, res, "FOLLOWUP")
	if !strings.Contains(fu.Detail, "INERT") {
		t.Errorf("a touch-1 follow-up row is never resolved and must be flagged INERT; got %q", fu.Detail)
	}
}

func gateByName(t *testing.T, res dripLaneVerifyResult, name string) dripLaneGate {
	t.Helper()
	for _, g := range res.Gates {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("gate %q missing from verify result (gates=%+v)", name, res.Gates)
	return dripLaneGate{}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
