package analytics

import (
	"strings"
	"testing"
)

// The growth aggregate is the ONLY delivery source for the Growth screen, so
// these pin the properties that would silently corrupt every number if they
// regressed: transport allowlist, Denver-day bucketing with the widened dt
// partition bound, and the read-time bounce taxonomy.

func TestBuildGrowthDeliverySQL_Shape(t *testing.T) {
	sql := buildGrowthDeliverySQL("2026-07-16", "2026-07-29")

	// Transport allowlist — 'app' is the PG mirror and double-counts deliveries;
	// including it would roughly double every delivered number on the screen.
	if !strings.Contains(sql, "source IN ('pmta','ses','kumo')") {
		t.Fatalf("missing real-transport allowlist:\n%s", sql)
	}
	if strings.Contains(sql, "'app'") {
		t.Fatalf("app-source rows must never be counted (double-count):\n%s", sql)
	}

	// dt partition bound widened ±1 day (a Denver day straddles two UTC
	// partitions) AND the precise local-day predicate present.
	if !strings.Contains(sql, "dt BETWEEN '2026-07-15' AND '2026-07-30'") {
		t.Fatalf("dt partition bound not widened ±1 day:\n%s", sql)
	}
	if !strings.Contains(sql, "BETWEEN '2026-07-16' AND '2026-07-29'") {
		t.Fatalf("missing precise Denver-day predicate:\n%s", sql)
	}

	// Denver bucketing, clean ISP (from the real recipient address, not the
	// polluted stored isp_group), and brand with the VMTA fallback.
	if !strings.Contains(sql, localDtExpr) {
		t.Error("day bucket must use the Denver localDtExpr")
	}
	if !strings.Contains(sql, ispDomainExpr) {
		t.Error("ISP must be derived from the real recipient address")
	}
	if strings.Contains(sql, "isp_group") {
		t.Error("stored isp_group is *.queue-polluted and must not be used")
	}
	if !strings.Contains(sql, brandCodeExpr) {
		t.Error("brand must keep the VMTA-code fallback")
	}

	// Dedup-safe counting.
	if !strings.Contains(sql, "COUNT(DISTINCT CASE WHEN") || !strings.Contains(sql, "THEN event_uid END)") {
		t.Errorf("counts must be COUNT(DISTINCT event_uid):\n%s", sql)
	}

	// Bounce taxonomy: hard and soft are separate buckets and reputation
	// blocks / administrative flushes are counted in NEITHER.
	for _, want := range []string{"= 'delivered'", "= 'hard_bounce'", "= 'soft_bounce'", "= 'reputation_block'", "= 'complaint'"} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing bucket %q", want)
		}
	}
	if !strings.Contains(sql, "deleted by administrator") {
		t.Error("must apply the read-time eventTypeExpr so administrative flushes are excluded from soft")
	}
}

func TestGrowthDelivery_RejectsBadDates(t *testing.T) {
	r := &Reader{}
	if _, err := r.GrowthDelivery(nil, "not-a-date", "2026-07-29"); err == nil { //nolint:staticcheck — nil ctx never reached
		t.Error("expected validation error for a malformed from date")
	}
	if _, err := r.GrowthDelivery(nil, "2026-07-16", "2026/07/29"); err == nil { //nolint:staticcheck
		t.Error("expected validation error for a malformed to date")
	}
}

func TestGrowthDelivery_DisabledReader(t *testing.T) {
	readerMu.Lock()
	prev := reader
	reader = nil
	readerMu.Unlock()
	defer func() {
		readerMu.Lock()
		reader = prev
		readerMu.Unlock()
	}()

	_, err := GrowthDelivery(nil, "2026-07-16", "2026-07-29") //nolint:staticcheck
	if !IsDisabledErr(err) {
		t.Fatalf("expected the disabled sentinel, got %v", err)
	}
}
