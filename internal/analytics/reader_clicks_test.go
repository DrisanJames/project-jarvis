package analytics

import (
	"strings"
	"testing"
)

const (
	cid1 = "11111111-1111-1111-1111-111111111111"
	cid2 = "22222222-2222-2222-2222-222222222222"
)

func TestBuildActionClickSQL_Shape(t *testing.T) {
	sql, err := buildActionClickSQL(ActionClickFilter{
		From: "2026-07-02", To: "2026-08-01", CampaignIDs: []string{cid1, cid2},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// The person key is subscriber_id, NEVER email: every source='app' click
	// row in the lake carries an EMPTY email (388,692 of 435,861 measured
	// 2026-08-01), so COUNT(DISTINCT email) would collapse the whole app click
	// stream to one "person". This assertion is the regression guard.
	if !strings.Contains(sql, "COUNT(DISTINCT subscriber_id) clickers") {
		t.Errorf("clickers must be COUNT(DISTINCT subscriber_id); got:\n%s", sql)
	}
	if strings.Contains(sql, "COUNT(DISTINCT email)") {
		t.Errorf("must never count distinct email — app click rows have no email:\n%s", sql)
	}

	// Denver day range widens the UTC dt partition bound by ±1 and applies the
	// exact local-day predicate (same rule Breakdown uses).
	if !strings.Contains(sql, "dt BETWEEN '2026-07-01' AND '2026-08-02'") {
		t.Errorf("dt bound must widen by +/-1 day; got:\n%s", sql)
	}
	if !strings.Contains(sql, localDtExpr+" BETWEEN '2026-07-02' AND '2026-08-01'") {
		t.Errorf("missing exact Denver local-day predicate; got:\n%s", sql)
	}

	if !strings.Contains(sql, "event_type = 'click'") {
		t.Errorf("must scope to click events; got:\n%s", sql)
	}
	if !strings.Contains(sql, "campaign_id IN ('"+cid1+"', '"+cid2+"')") {
		t.Errorf("campaign scoping missing; got:\n%s", sql)
	}
}

// The ACTION predicate is what separates engagement from vendor telemetry:
// asset fetches and compliance links are the majority of raw click rows.
func TestBuildActionClickSQL_ActionPredicate(t *testing.T) {
	sql, err := buildActionClickSQL(ActionClickFilter{
		From: "2026-07-02", To: "2026-08-01", CampaignIDs: []string{cid1},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, want := range []string{
		`css|js|woff2?`,  // asset extensions
		`fonts\.g|cdn\.`, // asset hosts
		`unsub|optout`,   // compliance links
		`^everflow-import:`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("action predicate missing %q; got:\n%s", want, sql)
		}
	}

	// t.em must be excluded ONLY for the /track/ family. The /o/ family is the
	// smart-link OFFER REDIRECT — 60,677 events / 1,797 people measured
	// 2026-08-01 — i.e. the money click. Excluding t.em wholesale (as the PG
	// doctrine does) would discard the majority of real offer engagement.
	if !strings.Contains(sql, `^https?://t\.em\.[^/]+/track/`) {
		t.Errorf("must exclude the t.em /track/ family; got:\n%s", sql)
	}
	if strings.Contains(sql, `NOT regexp_like(link_url, '(?i)^https?://t\.em\.')`) {
		t.Errorf("must NOT exclude t.em wholesale — that drops /o/ offer redirects:\n%s", sql)
	}
}

func TestBuildActionClickSQL_Rejects(t *testing.T) {
	cases := []struct {
		name string
		f    ActionClickFilter
	}{
		{"no campaign ids", ActionClickFilter{From: "2026-07-02", To: "2026-08-01"}},
		{"bad from", ActionClickFilter{From: "july", To: "2026-08-01", CampaignIDs: []string{cid1}}},
		{"bad to", ActionClickFilter{From: "2026-07-02", To: "nope", CampaignIDs: []string{cid1}}},
		{"non-uuid campaign", ActionClickFilter{From: "2026-07-02", To: "2026-08-01",
			CampaignIDs: []string{"'; DROP TABLE email_events; --"}}},
	}
	for _, c := range cases {
		if _, err := buildActionClickSQL(c.f); err == nil {
			t.Errorf("%s: expected an error, got none", c.name)
		}
	}
}

// An unscoped or over-large lake click scan is the shape that produced the
// 2026-07 Athena S3 GET storm; the cap must be enforced at build time.
func TestBuildActionClickSQL_CampaignCap(t *testing.T) {
	ids := make([]string, maxBreakdownCampaignIDs+1)
	for i := range ids {
		ids[i] = cid1
	}
	if _, err := buildActionClickSQL(ActionClickFilter{
		From: "2026-07-02", To: "2026-08-01", CampaignIDs: ids,
	}); err == nil {
		t.Fatalf("expected the %d-id cap to be enforced", maxBreakdownCampaignIDs)
	}
}

func TestBuildActionClickSQL_DedupesCampaignIDs(t *testing.T) {
	sql, err := buildActionClickSQL(ActionClickFilter{
		From: "2026-07-02", To: "2026-08-01", CampaignIDs: []string{cid1, cid1, cid2, cid1},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := strings.Count(sql, cid1); got != 1 {
		t.Errorf("campaign id should appear once after dedupe, got %d", got)
	}
}
