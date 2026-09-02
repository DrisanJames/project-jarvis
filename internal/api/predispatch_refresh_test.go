package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

func nt(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: true} }

func TestPredispatchSegmentsToBuild(t *testing.T) {
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	segs := map[string]predispatchSegment{
		"fresh":    {ID: "fresh", Status: "active", BuiltAt: nt(now.Add(-30 * time.Minute)), BuildState: "ok"},
		"stale":    {ID: "stale", Status: "active", BuiltAt: nt(now.Add(-3 * time.Hour)), BuildState: "ok"},
		"never":    {ID: "never", Status: "active"},
		"running":  {ID: "running", Status: "active", BuiltAt: nt(now.Add(-3 * time.Hour)), BuildState: "running"},
		"archived": {ID: "archived", Status: "archived", BuiltAt: nt(now.Add(-3 * time.Hour)), BuildState: "ok"},
	}
	got := map[string]bool{}
	for _, sg := range predispatchSegmentsToBuild(segs, now) {
		got[sg.ID] = true
	}
	for _, want := range []string{"stale", "never"} {
		if !got[want] {
			t.Errorf("expected %s to be rebuilt", want)
		}
	}
	for _, skip := range []string{"fresh", "running", "archived"} {
		if got[skip] {
			t.Errorf("expected %s to be skipped", skip)
		}
	}
}

func TestPredispatchCellReady(t *testing.T) {
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	minLead := 15 * time.Minute
	plan := now.Add(-3 * 24 * time.Hour) // promoted days ago
	segs := map[string]predispatchSegment{
		"a":   {ID: "a", Name: "DB 7D Clickers", Status: "active", BuiltAt: nt(now.Add(-5 * time.Minute)), BuildState: "ok"},
		"b":   {ID: "b", Name: "DB 7D Openers", Status: "active", BuiltAt: nt(now.Add(-5 * time.Minute)), BuildState: "ok"},
		"old": {ID: "old", Name: "DB 30D Openers", Status: "active", BuiltAt: nt(plan.Add(-time.Hour)), BuildState: "ok"},
	}
	base := predispatchCell{ID: "c1", Name: "08282026 - DB - Globe", ScheduledAt: now.Add(60 * time.Minute),
		PlanAt: nt(plan), Segments: []string{"a", "b"}}

	if ok, why := predispatchCellReady(base, segs, now, minLead); !ok {
		t.Fatalf("expected ready, got %q", why)
	}

	// Negative paths — every one must refuse, and the send-critical ones must say MISSED.
	cases := []struct {
		name   string
		mut    func(c *predispatchCell)
		missed bool
	}{
		{"queue rows already enqueued", func(c *predispatchCell) { c.Queued = 5 }, true},
		{"inside min lead", func(c *predispatchCell) { c.ScheduledAt = now.Add(10 * time.Minute) }, true},
		{"segment older than plan", func(c *predispatchCell) { c.Segments = []string{"a", "old"} }, false},
		{"unknown segment", func(c *predispatchCell) { c.Segments = []string{"a", "zzz"} }, false},
		{"no plan yet", func(c *predispatchCell) { c.PlanAt = sql.NullTime{} }, false},
		{"list-sourced (no segments)", func(c *predispatchCell) { c.Segments = nil }, false},
	}
	for _, tc := range cases {
		c := base
		tc.mut(&c)
		ok, why := predispatchCellReady(c, segs, now, minLead)
		if ok {
			t.Errorf("%s: expected refusal", tc.name)
			continue
		}
		if tc.missed && why[:6] != "MISSED" {
			t.Errorf("%s: expected MISSED reason, got %q", tc.name, why)
		}
		if !tc.missed && len(why) >= 6 && why[:6] == "MISSED" {
			t.Errorf("%s: should be a wait, not MISSED: %q", tc.name, why)
		}
	}
}

func TestPredispatchDisabledFlag(t *testing.T) {
	t.Setenv("DISABLE_PREDISPATCH_REFRESH", "true")
	if !predispatchDisabled() {
		t.Fatal("kill switch not honoured")
	}
	t.Setenv("DISABLE_PREDISPATCH_REFRESH", "")
	if predispatchDisabled() {
		t.Fatal("disabled without flag")
	}
	t.Setenv("PREDISPATCH_LOOKAHEAD_MIN", "90")
	if got := predispatchMinutes("PREDISPATCH_LOOKAHEAD_MIN", predispatchLookaheadDflt); got != 90*time.Minute {
		t.Fatalf("lookahead override: got %s", got)
	}
	t.Setenv("PREDISPATCH_LOOKAHEAD_MIN", "garbage")
	if got := predispatchMinutes("PREDISPATCH_LOOKAHEAD_MIN", predispatchLookaheadDflt); got != predispatchLookaheadDflt {
		t.Fatalf("bad override should fall back: got %s", got)
	}
}

// TestPredispatchRebindInheritsISPBan pins REQ-083 DoD item 2. The
// PreDispatch rebind re-deploys the cell's OWN stored blob — which still
// contains the gmail plan — through deployFromInput ~1h before each anchor,
// i.e. after the nightly Python wave-cancel had run. The ban therefore has to
// be inherited from normalizePMTACampaignInput inside that deploy, not
// re-implemented here: this test proves the blob still carries gmail AND that
// the deploy path drops it.
func TestPredispatchRebindInheritsISPBan(t *testing.T) {
	restore := installISPBanRows(t, [][3]string{{ispBanTestOrg, "wf", "gmail"}}, nil)
	defer restore()

	blob, err := json.Marshal(pmtaCampaignConfig{CampaignInput: banTestInput("m.warrantyforyou.com")})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}

	var deployed engine.PMTACampaignInput
	var normalized pmtaNormalizedCampaign
	var normErr error
	deps := predispatchDeps{
		now: time.Now,
		deploy: func(ctx context.Context, orgID string, in engine.PMTACampaignInput) (string, string, bool, error) {
			deployed = in
			// Exactly what deployFromInput does with the payload
			// (handlers_pmta_campaign.go). Returning an error stops the
			// rebind before it needs a database.
			normalized, normErr = normalizePMTACampaignInput(in)
			return "", "", false, errors.New("stop after deploy input capture")
		},
	}

	s := &PMTACampaignService{}
	cell := predispatchCell{
		ID:    "00000000-0000-0000-0000-0000000000aa",
		OrgID: ispBanTestOrg,
		Name:  "09022026 - WFY - Destiny",
		// The blob the rebind re-deploys.
		ConfigRaw: string(blob),
	}
	if err := s.predispatchRebind(context.Background(), deps, cell, nil); err == nil {
		t.Fatal("expected the injected deploy error to surface")
	}

	// The stored blob is unchanged — the hazard is real, not hypothetical.
	gmailInBlob := false
	for _, p := range deployed.ISPPlans {
		if p.ISP == "gmail" {
			gmailInBlob = true
		}
	}
	if !gmailInBlob {
		t.Fatal("test is not exercising the hazard: the rebind blob carried no gmail plan")
	}
	if deployed.Name != cell.Name+predispatchSiblingSfx {
		t.Errorf("sibling name: got %q", deployed.Name)
	}

	// ...and the deploy path drops it.
	if normErr != nil {
		t.Fatalf("normalize inside the rebind deploy: %v", normErr)
	}
	for _, p := range normalized.Plans {
		if p.ISP == "gmail" {
			t.Fatalf("PreDispatch rebind deployed a gmail plan for a banned brand: %+v", normalized.Plans)
		}
	}
	if len(normalized.Plans) == 0 {
		t.Fatal("rebind lost every plan — the ban must only remove gmail")
	}
}
