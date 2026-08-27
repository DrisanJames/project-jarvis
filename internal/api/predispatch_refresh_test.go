package api

import (
	"database/sql"
	"testing"
	"time"
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
		"a": {ID: "a", Name: "DB 7D Clickers", Status: "active", BuiltAt: nt(now.Add(-5 * time.Minute)), BuildState: "ok"},
		"b": {ID: "b", Name: "DB 7D Openers", Status: "active", BuiltAt: nt(now.Add(-5 * time.Minute)), BuildState: "ok"},
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
