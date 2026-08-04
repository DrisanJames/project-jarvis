package api

import (
	"database/sql"
	"testing"
	"time"
)

// TestRenderSegmentConditionsSummary pins the three definition-line branches:
// criteria-v2 rendered, lake_spec rendered, otherwise type+build_source.
func TestRenderSegmentConditionsSummary(t *testing.T) {
	cases := []struct {
		name, raw, segType, buildSource, want string
	}{
		{
			name:    "criteria v2",
			raw:     `{"v2":{"lists":["11111111-1111-1111-1111-111111111111"],"performance":{"opened_within_days":30}}}`,
			segType: "dynamic",
			want:    "1 list · opened ≤30d",
		},
		{
			name:    "invalid v2 is labeled, never mistaken for static",
			raw:     `{"v2":{}}`,
			segType: "dynamic",
			want:    "criteria-v2 (invalid)",
		},
		{
			name:        "static with build source",
			raw:         `[]`,
			segType:     "static",
			buildSource: "lake_recency",
			want:        "static — build_source: lake_recency",
		},
		{
			name:    "static without build source",
			raw:     "",
			segType: "static",
			want:    "static — build_source: unknown",
		},
	}
	for _, c := range cases {
		if got := renderSegmentConditionsSummary(c.raw, c.segType, c.buildSource); got != c.want {
			t.Errorf("%s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}

	// Lake spec branch: parseLakeSpec must win over the static fallback.
	lake := renderSegmentConditionsSummary(
		`{"lake_spec":{"event":"click","window_days":30,"scope":"brand","brand_apex":"discountblog.com"}}`,
		"static", "lake")
	if lake == "static — build_source: lake" {
		t.Errorf("lake_spec conditions fell through to the static branch: %s", lake)
	}
}

// TestDerivePruneAt pins the protection rules and the +30d clock.
func TestDerivePruneAt(t *testing.T) {
	created := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	used := sql.NullTime{Time: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	ref := sql.NullTime{Time: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC), Valid: true}
	none := sql.NullTime{}

	// Protected: non-active status / keep_active / registry protect / archived.
	if p := derivePruneAt("archived", false, "", none, none, none, created); p != nil {
		t.Errorf("non-active status must be protected, got %v", p)
	}
	if p := derivePruneAt("active", true, "", none, none, none, created); p != nil {
		t.Errorf("keep_active must be protected, got %v", p)
	}
	if p := derivePruneAt("active", false, "protect", none, none, none, created); p != nil {
		t.Errorf("registry protect match must be protected, got %v", p)
	}
	if p := derivePruneAt("active", false, "", sql.NullTime{Time: created, Valid: true}, none, none, created); p != nil {
		t.Errorf("already-archived must be protected, got %v", p)
	}

	// Prunable: clock = GREATEST(last ref, last used, created) + 30d.
	p := derivePruneAt("active", false, "purgeable", none, ref, used, created)
	if p == nil {
		t.Fatal("expected a prune countdown")
	}
	if want := ref.Time.Add(30 * 24 * time.Hour); !p.Equal(want) {
		t.Errorf("prune_at = %v, want last-ref+30d = %v", p, want)
	}

	// NULL-safe: no refs, no last_used → created_at anchors the clock.
	p = derivePruneAt("active", false, "", none, none, none, created)
	if p == nil || !p.Equal(created.Add(30*24*time.Hour)) {
		t.Errorf("prune_at = %v, want created+30d", p)
	}
}
