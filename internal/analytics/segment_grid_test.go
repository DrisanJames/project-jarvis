package analytics

import (
	"strings"
	"testing"
)

func TestBuildGridBucketSQL_Shape(t *testing.T) {
	sql, err := buildGridBucketSQL(SegmentEventOpen, "2026-07-22", "2026-08-21", 1753164000000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "SELECT subscriber_id, brand FROM email_events" +
		" WHERE event_type = 'open'" +
		" AND source IN ('app','ses')" +
		" AND dt BETWEEN '2026-07-22' AND '2026-08-21'" +
		" AND event_epoch_ms >= 1753164000000" +
		" AND subscriber_id <> ''" +
		" GROUP BY subscriber_id, brand" +
		" LIMIT 2000001"
	if sql != want {
		t.Fatalf("SQL mismatch:\n got: %s\nwant: %s", sql, want)
	}
}

func TestBuildGridBucketSQL_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		from    string
		to      string
		epochMs int64
	}{
		{"bad event", "delivered", "2026-07-22", "2026-08-21", 1},
		{"injection event", "open' OR '1'='1", "2026-07-22", "2026-08-21", 1},
		{"bad from", SegmentEventClick, "22-07-2026", "2026-08-21", 1},
		{"bad to", SegmentEventClick, "2026-07-22", "not-a-date", 1},
		{"inverted range", SegmentEventClick, "2026-08-22", "2026-08-21", 1},
		{"zero epoch", SegmentEventClick, "2026-07-22", "2026-08-21", 0},
	}
	for _, c := range cases {
		if _, err := buildGridBucketSQL(c.event, c.from, c.to, c.epochMs); err == nil {
			t.Errorf("%s: expected error, got none", c.name)
		}
	}
}

func TestBuildGridBucketSQL_ClickEvent(t *testing.T) {
	sql, err := buildGridBucketSQL(SegmentEventClick, "2026-08-14", "2026-08-21", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(sql, "event_type = 'click'") {
		t.Fatalf("click SQL missing event filter: %s", sql)
	}
}
