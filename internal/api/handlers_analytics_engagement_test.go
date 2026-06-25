package api

import (
	"strings"
	"testing"
)

// The engagement-summary query MUST keep a raw event_at timestamptz bound so the
// monthly partitions of mailing_tracking_events prune. Filtering ONLY on the
// Denver-date expression wraps the partition key in a function and defeats
// pruning, which scans every partition and times out the 15s handler (the
// "engagement unavailable" P1 on 2026-06-25). This guards against that
// regression — see VersionEngagementSummary 1.2 + the package doc.
func TestEngagementSummaryQuery_HasPartitionPruneBound(t *testing.T) {
	q := engagementSummaryQuery

	// Prune-enabling raw bound on the partition key (both sides).
	if !strings.Contains(q, "event_at >= ($2::date - 1)::timestamptz") {
		t.Errorf("engagementSummaryQuery missing the lower event_at prune bound;\nquery:\n%s", q)
	}
	if !strings.Contains(q, "event_at <  ($3::date + 2)::timestamptz") {
		t.Errorf("engagementSummaryQuery missing the upper event_at prune bound;\nquery:\n%s", q)
	}
	// The precise Denver-day predicate must remain for exact day selection.
	if !strings.Contains(q, "AT TIME ZONE 'America/Denver')::date BETWEEN $2::date AND $3::date") {
		t.Errorf("engagementSummaryQuery missing the precise Denver-day predicate;\nquery:\n%s", q)
	}
	// Org scoping must be present (tenant isolation).
	if !strings.Contains(q, "organization_id = $1") {
		t.Errorf("engagementSummaryQuery missing org scoping;\nquery:\n%s", q)
	}
}
