package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestWarmupAgentOverview_BadInput — the handler refuses malformed instance /
// date before touching S3 (no client needed).
func TestWarmupAgentOverview_BadInput(t *testing.T) {
	s := &WarmupAgentService{bucket: "x"}
	for _, q := range []string{"instance=../etc", "date=2026-1-1", "instance=COLO2"} {
		rr := httptest.NewRecorder()
		s.handleOverview(rr, httptest.NewRequest(http.MethodGet, "/api/mailing/warmup-agent/overview?"+q, nil))
		if rr.Code == http.StatusOK {
			t.Fatalf("%s: expected a refusal, got 200", q)
		}
	}
}

// TestWarmupAgentOverview_Live — reads the real bucket when WARMUP_LIVE_TEST=1
// (needs AWS credentials); otherwise skipped. Proves the bundle shape the
// portal tab relies on: journal + ticks arrays, rates object, no errors map.
func TestWarmupAgentOverview_Live(t *testing.T) {
	if os.Getenv("WARMUP_LIVE_TEST") != "1" {
		t.Skip("WARMUP_LIVE_TEST=1 to read the live bucket")
	}
	s := NewWarmupAgentService()
	if s.err != nil {
		t.Fatalf("client: %v", s.err)
	}
	rr := httptest.NewRecorder()
	s.handleOverview(rr, httptest.NewRequest(http.MethodGet, "/api/mailing/warmup-agent/overview?instance=colo2", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var out warmupOverview
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Errors != nil {
		t.Fatalf("errors: %v", out.Errors)
	}
	if len(out.Rates) == 0 || len(out.Journal) == 0 || len(out.Ticks) == 0 || out.FieldManual == nil {
		t.Fatalf("thin bundle: rates=%d journal=%d ticks=%d manual=%v", len(out.Rates), len(out.Journal), len(out.Ticks), out.FieldManual != nil)
	}
	t.Logf("ok: %s %s journal=%d ticks=%d report=%v bytes=%d", out.Instance, out.Date, len(out.Journal), len(out.Ticks), out.Report != nil, rr.Body.Len())
}

// TestAgentsOverview_Live — the scheduling/data agents + ledger bundle from the
// jarvis-agents state mirror (WARMUP_LIVE_TEST=1).
func TestAgentsOverview_Live(t *testing.T) {
	if os.Getenv("WARMUP_LIVE_TEST") != "1" {
		t.Skip("WARMUP_LIVE_TEST=1 to read the live bucket")
	}
	s := NewWarmupAgentService()
	rr := httptest.NewRecorder()
	s.handleAgentsOverview(rr, httptest.NewRequest(http.MethodGet, "/api/mailing/agents/overview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var out agentsOverview
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Errors != nil {
		t.Fatalf("errors: %v", out.Errors)
	}
	if len(out.Data.State) == 0 || len(out.Scheduling.Supervisor) == 0 {
		t.Fatalf("thin bundle: data.state=%d supervisor=%d", len(out.Data.State), len(out.Scheduling.Supervisor))
	}
	t.Logf("ok: %s plan_for_date=%d plan_tonight=%d data.journal=%d ledger=%d bytes=%d", out.Date, len(out.Scheduling.PlanForDate), len(out.Scheduling.PlanTonight), len(out.Data.Journal), len(out.Ledger), rr.Body.Len())
}

// TestWarmupAgentOverview_LightLive — light=1 drops the note text, field manual,
// report and box snapshot; everything the 7-day strip derives from stays.
func TestWarmupAgentOverview_LightLive(t *testing.T) {
	if os.Getenv("WARMUP_LIVE_TEST") != "1" {
		t.Skip("WARMUP_LIVE_TEST=1 to read the live bucket")
	}
	s := NewWarmupAgentService()
	full, light := httptest.NewRecorder(), httptest.NewRecorder()
	s.handleOverview(full, httptest.NewRequest(http.MethodGet, "/api/mailing/warmup-agent/overview?instance=colo2", nil))
	s.handleOverview(light, httptest.NewRequest(http.MethodGet, "/api/mailing/warmup-agent/overview?instance=colo2&light=1", nil))
	var out warmupOverview
	if err := json.Unmarshal(light.Body.Bytes(), &out); err != nil || light.Code != http.StatusOK {
		t.Fatalf("light: HTTP %d %v", light.Code, err)
	}
	if out.FieldManual != nil || out.Report != nil || len(out.Journal) == 0 || len(out.Ticks) == 0 || len(out.Jit) == 0 {
		t.Fatalf("light bundle wrong shape: manual=%v report=%v journal=%d ticks=%d jit=%d", out.FieldManual != nil, out.Report != nil, len(out.Journal), len(out.Ticks), len(out.Jit))
	}
	if light.Body.Len() >= full.Body.Len()*6/10 {
		t.Fatalf("light not light: %d vs full %d", light.Body.Len(), full.Body.Len())
	}
	t.Logf("full=%d light=%d bytes", full.Body.Len(), light.Body.Len())
}
