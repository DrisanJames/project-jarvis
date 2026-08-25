package analytics

import (
	"strings"
	"testing"
)

// TestBuildClickFunnelSQL_FiltersOnPartitionColumn — dt is the partition
// column. A read that drops it scans the whole table.
func TestBuildClickFunnelSQL_FiltersOnPartitionColumn(t *testing.T) {
	sql := BuildClickFunnelSQL("2026-08-23", "2026-08-25",
		[]string{"41813624-0866-5614-adef-1bea80b77116"})
	if !strings.Contains(sql, "WHERE dt BETWEEN '2026-08-23' AND '2026-08-25'") {
		t.Fatalf("partition filter missing:\n%s", sql)
	}
	if !strings.Contains(sql, "GROUP BY dt, campaign_id, source, event_type") {
		t.Fatalf("day grain missing — a window change would need a new query:\n%s", sql)
	}
}

// TestClickFunnelEventTypes_ArePresentTense is the silent-zero guard. The lake
// spells engagement 'open'/'click'; Postgres uses 'opened'/'clicked'. The wrong
// tense returns no rows, no error, and an empty engagement column.
func TestClickFunnelEventTypes_ArePresentTense(t *testing.T) {
	joined := strings.Join(ClickFunnelEventTypes, ",")
	for _, past := range []string{"opened", "clicked", "bounced", "unsubscribed"} {
		if strings.Contains(joined, past) {
			t.Fatalf("PAST-tense %q in the lake event set — this returns a SILENT ZERO", past)
		}
	}
	for _, want := range []string{"open", "click", "delivered", "relayed_to_ses", "delivery_delay"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("event type %q missing", want)
		}
	}
}

// TestBuildClickFunnelSQL_CarriesAllThreeCountDomains — events, recipients and
// mailboxes are different metrics (redelivered events, unique openers, deferred
// mailboxes) and re-deriving any of them costs another scan.
func TestBuildClickFunnelSQL_CarriesAllThreeCountDomains(t *testing.T) {
	sql := BuildClickFunnelSQL("2026-08-25", "2026-08-25", []string{"41813624-0866-5614-adef-1bea80b77116"})
	for _, frag := range []string{
		"COUNT(DISTINCT event_uid) events",
		"COUNT(DISTINCT subscriber_id) recipients",
		"COUNT(DISTINCT email) mailboxes",
		"is_machine_click IS NOT NULL",
		"is_machine_click = true",
	} {
		if !strings.Contains(sql, frag) {
			t.Fatalf("missing %q:\n%s", frag, sql)
		}
	}
}

// TestClickFunnelDaily_RejectsNonUUIDCampaigns — caller text must never reach a
// column position.
func TestClickFunnelDaily_RejectsNonUUIDCampaigns(t *testing.T) {
	r := &Reader{}
	_, err := r.ClickFunnelDaily(nil, "2026-08-25", "2026-08-25", []string{"'; DROP TABLE x --"})
	if err == nil || !strings.Contains(err.Error(), "not a campaign uuid") {
		t.Fatalf("a non-UUID campaign id must be rejected, got %v", err)
	}
}
