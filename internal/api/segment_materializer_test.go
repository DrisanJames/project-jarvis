package api

import (
	"strings"
	"testing"
	"time"
)

func TestParseMaterializeConcurrency(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"empty falls back to default", "", defaultMaterializeConcurrency},
		{"whitespace falls back to default", "   ", defaultMaterializeConcurrency},
		{"valid value", "4", 4},
		{"trimmed valid value", "  3  ", 3},
		{"lower clamp boundary", "1", 1},
		{"upper clamp boundary", "16", 16},
		{"zero is invalid -> default", "0", defaultMaterializeConcurrency},
		{"negative is invalid -> default", "-2", defaultMaterializeConcurrency},
		{"above max is invalid -> default", "17", defaultMaterializeConcurrency},
		{"non-numeric -> default", "lots", defaultMaterializeConcurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseMaterializeConcurrency(tt.raw); got != tt.want {
				t.Fatalf("parseMaterializeConcurrency(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestDurationUntilNext(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		hour     int
		min      int
		wantWait time.Duration
	}{
		{
			name:     "10 minutes before target",
			now:      time.Date(2026, 4, 15, 3, 50, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 10 * time.Minute,
		},
		{
			name:     "5 minutes after target wraps to next day",
			now:      time.Date(2026, 4, 15, 4, 5, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 23*time.Hour + 55*time.Minute,
		},
		{
			name:     "exactly at target wraps to next day",
			now:      time.Date(2026, 4, 15, 4, 0, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 24 * time.Hour,
		},
		{
			name:     "midnight targeting 04:00",
			now:      time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 4 * time.Hour,
		},
		{
			name:     "23:59 targeting 04:00 next day",
			now:      time.Date(2026, 4, 15, 23, 59, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 4*time.Hour + 1*time.Minute,
		},
		{
			name:     "with minutes in target",
			now:      time.Date(2026, 4, 15, 4, 15, 0, 0, time.UTC),
			hour:     4, min: 30,
			wantWait: 15 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DurationUntilNext(tt.now, tt.hour, tt.min)
			if got != tt.wantWait {
				t.Errorf("DurationUntilNext(%v, %d:%02d) = %v, want %v",
					tt.now.Format("15:04"), tt.hour, tt.min, got, tt.wantWait)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Canonical master-list segment tests
//
// These pin the JSON conditions that ship in the phase21 seed migrations to
// the concrete SQL that MaterializeSegment will execute, so a regression in
// either the seed JSON or in buildSegmentQuery's V2 handling fails the
// build. The seeded JSON strings below MUST stay byte-identical to the
// values INSERTed in cmd/server/main.go's phase21 seed migrations
// (seed_master_list_segment_row / seed_engaged_openers_segment_row /
// seed_engaged_clickers_segment_row) — if you change one, update both.
// ---------------------------------------------------------------------------

const (
	seededMasterListConditions     = `{"logic_operator":"AND","conditions":[]}`
	seededEngagedOpenersConditions = `{"logic_operator":"AND","conditions":[{"condition_type":"profile","field":"last_open_at","field_type":"datetime","operator":"in_last_days","value":"30"}]}`
	seededEngagedClickersConditions = `{"logic_operator":"AND","conditions":[{"condition_type":"profile","field":"last_click_at","field_type":"datetime","operator":"in_last_days","value":"30"}]}`
)

// TestCanonicalSegmentNamesAreStable guards the contract between the seed
// migrations (which insert by name) and the boot-time materializer (which
// looks up by the same names). Renaming a segment without updating this
// slice would cause MaterializeCanonicalSegments to silently skip it.
func TestCanonicalSegmentNamesAreStable(t *testing.T) {
	want := map[string]bool{
		"Master List":             true,
		"Engaged Openers (30d)":   true,
		"Engaged Clickers (30d)":  true,
	}
	if len(CanonicalMasterListSegments) != len(want) {
		t.Fatalf("CanonicalMasterListSegments has %d entries, want %d — this list MUST match the phase21 seed migrations",
			len(CanonicalMasterListSegments), len(want))
	}
	for _, name := range CanonicalMasterListSegments {
		if !want[name] {
			t.Errorf("unexpected canonical segment %q — seed migrations don't create this name", name)
		}
	}
}

// TestBuildSegmentQuery_MasterListAllEligible validates the Master List
// segment's empty-conditions V2 JSON expands to a query that returns every
// eligible subscriber and nothing else. Specifically:
//   - status = 'confirmed' (default baseline)
//   - hard_bounced_at IS NULL (P7 global suppression)
//   - complained_at IS NULL (P7 global suppression)
//   - suppression list NOT EXISTS anti-join
//
// No custom WHERE terms, no list_id filter — that's what "master" means.
func TestBuildSegmentQuery_MasterListAllEligible(t *testing.T) {
	query, args := buildSegmentQuery(seededMasterListConditions, nil)
	if len(args) != 0 {
		t.Fatalf("Master List should bind zero args, got %d: %v", len(args), args)
	}
	for _, want := range []string{
		"FROM mailing_subscribers s",
		"s.status = 'confirmed'",
		"s.hard_bounced_at IS NULL",
		"s.complained_at IS NULL",
		"mailing_suppression_list sup",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("Master List query missing %q\nfull query: %s", want, query)
		}
	}
	// There must be NO list_id predicate — Master List deliberately spans
	// all lists, and a stray s.list_id = ... would break that.
	if strings.Contains(query, "s.list_id = $") {
		t.Errorf("Master List query unexpectedly filters by list_id — master segment must span all lists\nfull query: %s", query)
	}
}

// TestBuildSegmentQuery_EngagedOpenersInLast30Days validates the Openers
// segment expands to last_open_at >= NOW() - INTERVAL '30 days'. This is
// the contract the planner relies on when 'Engaged Openers (30d)' is
// chosen as the audience source.
//
// Note: the wrapping SELECT column list from QueryBuilder always includes
// both last_open_at and last_click_at (they're returned as output columns),
// so cross-wiring is detected by checking which field appears inside the
// WHERE-clause condition expression specifically, not anywhere in the
// query string.
func TestBuildSegmentQuery_EngagedOpenersInLast30Days(t *testing.T) {
	query, _ := buildSegmentQuery(seededEngagedOpenersConditions, nil)
	openerPred := "s.last_open_at >= NOW() - INTERVAL '30 days'"
	clickerPred := "s.last_click_at >= NOW() - INTERVAL '30 days'"
	if !strings.Contains(query, openerPred) {
		t.Errorf("Engaged Openers query missing predicate %q\nfull query: %s", openerPred, query)
	}
	if strings.Contains(query, clickerPred) {
		t.Errorf("Engaged Openers query has clicker predicate %q — condition is cross-wired\nfull query: %s", clickerPred, query)
	}
	for _, baseline := range []string{
		"s.status = 'confirmed'",
		"s.hard_bounced_at IS NULL",
		"s.complained_at IS NULL",
	} {
		if !strings.Contains(query, baseline) {
			t.Errorf("Engaged Openers query missing baseline %q — suppressions must still apply\nfull query: %s", baseline, query)
		}
	}
}

// TestBuildSegmentQuery_EngagedClickersInLast30Days mirrors the Openers
// test for the click cohort. Clickers is the highest-signal cohort we use
// for warmup and reputation recovery, so this predicate must be tight.
func TestBuildSegmentQuery_EngagedClickersInLast30Days(t *testing.T) {
	query, _ := buildSegmentQuery(seededEngagedClickersConditions, nil)
	clickerPred := "s.last_click_at >= NOW() - INTERVAL '30 days'"
	openerPred := "s.last_open_at >= NOW() - INTERVAL '30 days'"
	if !strings.Contains(query, clickerPred) {
		t.Errorf("Engaged Clickers query missing predicate %q\nfull query: %s", clickerPred, query)
	}
	if strings.Contains(query, openerPred) {
		t.Errorf("Engaged Clickers query has opener predicate %q — condition is cross-wired\nfull query: %s", openerPred, query)
	}
	for _, baseline := range []string{
		"s.status = 'confirmed'",
		"s.hard_bounced_at IS NULL",
		"s.complained_at IS NULL",
	} {
		if !strings.Contains(query, baseline) {
			t.Errorf("Engaged Clickers query missing baseline %q\nfull query: %s", baseline, query)
		}
	}
}

// TestBuildSegmentQuery_EmptyFlatArrayStillReturnsEmpty documents a
// deliberate safety: a segment with no conditions AND no list_id MUST NOT
// accidentally select every subscriber in the system via the legacy
// flat-array path. The empty-array path returns WHERE FALSE; only the V2
// object path with an explicit logic_operator opens up the master pool.
// This test fails if someone "helpfully" loosens buildSegmentQuery and
// creates a foot-gun for empty exclusion segments.
func TestBuildSegmentQuery_EmptyFlatArrayStillReturnsEmpty(t *testing.T) {
	query, _ := buildSegmentQuery("[]", nil)
	if !strings.Contains(query, "WHERE FALSE") {
		t.Errorf("empty flat-array segment with no list_id should return empty set; got: %s", query)
	}
}
