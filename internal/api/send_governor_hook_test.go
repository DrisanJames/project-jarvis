package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// T2 — the REAL 2026-09-23 D14-ENG payload (pmta_config->'campaign_input' of
// "09232026 - DB - NL-YF-NEWSLETTER-D14-ENG", html replaced by a stub) replayed
// against the deploy handler: scheduled_at 07:01Z, spans 07:01Z→11:07Z. On
// 09-23 it was deployed at 11:10Z, accepted, and every wave was cancelled by
// the janitor. Now: HTTP 400 "send window closed", and ZERO database
// statements (no sqlmock expectation is registered, so any query fails).
func TestHandleDeployCampaign_ClosedWindowRefusedBeforeAnyWrite(t *testing.T) {
	raw, err := os.ReadFile("testdata/sep23_d14_eng_input.json")
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		ScheduledAt string `json:"scheduled_at"`
		ISPPlans    []struct {
			TimeSpans []struct {
				EndAt string `json:"end_at"`
			} `json:"time_spans"`
		} `json:"isp_plans"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.ScheduledAt != "2026-09-23T07:01:00Z" || probe.ISPPlans[0].TimeSpans[0].EndAt != "2026-09-23T11:07:00Z" {
		t.Fatalf("fixture drifted: %+v", probe)
	}

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := newTestPMTAService(db, defaultOrgID)

	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(raw))
	req.Header.Set("X-Organization-ID", defaultOrgID)
	req.Header.Set("X-Internal-Caller", "family_sidecar")
	rr := httptest.NewRecorder()
	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "send window closed") {
		t.Fatalf("body = %s, want the closed-window refusal", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a refused deploy must touch nothing: %v", err)
	}
}

// The same refusal from the normalizer directly, for every send_mode, and the
// negative control: a window that is still open normalizes.
func TestNormalizeTimeSpans_ClosedWindowRefusedInEveryMode(t *testing.T) {
	now := time.Date(2026, 9, 23, 11, 10, 31, 0, time.UTC)
	start := time.Date(2026, 9, 23, 7, 1, 0, 0, time.UTC)
	end := time.Date(2026, 9, 23, 11, 7, 0, 0, time.UTC)
	spans := []engine.PMTATimeSpanInput{{Type: "absolute", StartAt: &start, EndAt: &end, Source: "duration-calc", Timezone: "America/Denver"}}
	for _, mode := range []string{"scheduled", "immediate", ""} {
		if _, err := normalizeTimeSpans(spans, mode, "America/Denver", &start, now); err == nil || !strings.Contains(err.Error(), "send window closed") {
			t.Fatalf("mode %q: want closed-window refusal, got %v", mode, err)
		}
	}
	// Closes 10 min from now → still refused (15 min lead).
	soon := now.Add(10 * time.Minute)
	if _, err := normalizeTimeSpans([]engine.PMTATimeSpanInput{{Type: "absolute", StartAt: &start, EndAt: &soon}}, "immediate", "UTC", nil, now); err == nil {
		t.Fatal("a window closing inside the lead must be refused")
	}
	// Open window (ends in 4h), immediate mode with a past start: accepted.
	openEnd := now.Add(4 * time.Hour)
	out, err := normalizeTimeSpans([]engine.PMTATimeSpanInput{{Type: "absolute", StartAt: &start, EndAt: &openEnd, Source: "duration-calc"}}, "immediate", "UTC", nil, now)
	if err != nil || len(out) != 1 || !out[0].EndAt.Equal(openEnd) {
		t.Fatalf("open window must pass: %v %+v", err, out)
	}
	// A start-only span (no end_at) is not subject to the closed-window rule.
	if _, err := normalizeTimeSpans([]engine.PMTATimeSpanInput{{Type: "absolute", StartAt: &start}}, "immediate", "UTC", nil, now); err != nil {
		t.Fatalf("start-only span: %v", err)
	}
}

// T3 — the planner clamp. A stub governor answers Headroom; the planner is
// driven over sqlmock exactly like TestPlanPMTAAudience_ReservePool_OverSelect.
type stubGovernor struct {
	enabled  bool
	headroom map[string]int // isp → headroom; absent = ungoverned
	calls    []string
}

func (s *stubGovernor) Enabled() bool { return s.enabled }
func (s *stubGovernor) Headroom(_ context.Context, _ worker.FamilyGovernorQueryer, lane, domain, isp string, _ time.Time) (int, bool, error) {
	s.calls = append(s.calls, lane+"|"+domain+"|"+isp)
	n, ok := s.headroom[isp]
	return n, ok, nil
}

func planWithStubGovernor(t *testing.T, g SendGovernorHeadroom, input engine.PMTACampaignInput, normalized pmtaNormalizedCampaign, members int) pmtaAudiencePlan {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := sqlmock.NewRows([]string{"id", "email"})
	for i := 0; i < members; i++ {
		rows.AddRow(fmt.Sprintf("11111111-0000-0000-0000-%012d", i), fmt.Sprintf("user%d@hotmail.com", i))
	}
	mock.ExpectQuery("SELECT s.id::text, s.email").WithArgs(input.InclusionLists[0], sqlmock.AnyArg()).WillReturnRows(rows)
	SetSendGovernor(g)
	t.Cleanup(func() { SetSendGovernor(nil) })
	result, err := planPMTAAudience(context.Background(), db, defaultOrgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	return result
}

func TestPlanPMTAAudience_GovernorClampsAudienceBoundCell(t *testing.T) {
	listID := "aaaaaaaa-0000-0000-0000-000000000002"
	input := engine.PMTACampaignInput{
		Name: "09252026 - DB - NL-MS-NEWSLETTER-D12-COLD", Lane: "cold", SendingDomain: "m.discountblog.com",
		InclusionLists: []string{listID},
		ISPPlans:       []engine.PMTAISPScheduleInput{{ISP: "microsoft", Quota: 0}},
	}
	normalized := pmtaNormalizedCampaign{Plans: []pmtaNormalizedPlan{{ISP: "microsoft", Quota: 0}}}

	// Contract headroom 100 on a 500-member audience-bound cell → 100 selected
	// (+ the reserve pool the quota now permits, 1.5x → 50 reserves).
	g := &stubGovernor{enabled: true, headroom: map[string]int{"microsoft": 100}}
	r := planWithStubGovernor(t, g, input, normalized, 500)
	if r.CountsByISP["microsoft"] != 100 || r.SelectedTotal != 100 {
		t.Fatalf("clamp: counts=%v total=%d, want 100", r.CountsByISP, r.SelectedTotal)
	}
	if r.ReserveCountsByISP["microsoft"] != 50 {
		t.Fatalf("reserve = %d, want 50", r.ReserveCountsByISP["microsoft"])
	}
	if len(g.calls) != 1 || g.calls[0] != "cold|m.discountblog.com|microsoft" {
		t.Fatalf("governor asked with %v", g.calls)
	}

	// Headroom 0 → nothing selected (not "unlimited").
	g0 := &stubGovernor{enabled: true, headroom: map[string]int{"microsoft": 0}}
	if r := planWithStubGovernor(t, g0, input, normalized, 500); r.CountsByISP["microsoft"] != 0 || r.SelectedTotal != 0 || len(r.RecipientsByISP["microsoft"]) != 0 {
		t.Fatalf("headroom 0 must select nothing: %v", r.CountsByISP)
	}

	// A capped cell above headroom is lowered to it; below it is untouched.
	capped := normalized
	capped.Plans = []pmtaNormalizedPlan{{ISP: "microsoft", Quota: 300}}
	if r := planWithStubGovernor(t, g, input, capped, 500); r.CountsByISP["microsoft"] != 100 {
		t.Fatalf("capped 300 over headroom 100: %v", r.CountsByISP)
	}
	capped.Plans = []pmtaNormalizedPlan{{ISP: "microsoft", Quota: 40}}
	if r := planWithStubGovernor(t, g, input, capped, 500); r.CountsByISP["microsoft"] != 40 {
		t.Fatalf("capped 40 under headroom 100 must stay 40: %v", r.CountsByISP)
	}

	// Ungoverned (no contract / lane not governed / governor off / nil): untouched.
	if r := planWithStubGovernor(t, &stubGovernor{enabled: true}, input, normalized, 500); r.CountsByISP["microsoft"] != 500 {
		t.Fatalf("ungoverned must stream all: %v", r.CountsByISP)
	}
	if r := planWithStubGovernor(t, &stubGovernor{enabled: false, headroom: map[string]int{"microsoft": 1}}, input, normalized, 500); r.CountsByISP["microsoft"] != 500 {
		t.Fatalf("disabled governor must not clamp: %v", r.CountsByISP)
	}
	if r := planWithStubGovernor(t, nil, input, normalized, 500); r.CountsByISP["microsoft"] != 500 {
		t.Fatalf("nil governor must not clamp: %v", r.CountsByISP)
	}
}
