package worker

import (
	"strings"
	"testing"
	"time"
)

func denverLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skipf("America/Denver tz unavailable: %v", err)
	}
	return loc
}

func newTestGrowthWorker(t *testing.T) *GrowthRollupWorker {
	t.Helper()
	return &GrowthRollupWorker{
		tick:         growthTickInterval,
		trailingDays: growthTrailingDays,
		chunkDays:    growthChunkDays,
		maxBackfill:  growthBackfillChunks,
		epoch:        growthLakeEpoch,
		loc:          denverLoc(t),
	}
}

// The trailing window must ALWAYS be recomputed — that is what keeps the
// current day (and late-arriving lake rows) fresh on screen. A gap-fill-only
// pass would freeze today's row at whatever it was when first written.
func TestChunksToCompute_TrailingWindowAlwaysFirst(t *testing.T) {
	w := newTestGrowthWorker(t)
	// A fully covered history means "nothing to gap-fill" — the trailing
	// window must still be returned.
	w.coveredOverride = func() map[string]bool {
		out := map[string]bool{}
		d, _ := time.ParseInLocation("2006-01-02", growthLakeEpoch, w.loc)
		for ; d.Year() < 2027; d = d.AddDate(0, 0, 1) {
			out[d.Format("2006-01-02")] = true
		}
		return out
	}
	now := time.Date(2026, 7, 29, 15, 0, 0, 0, w.loc)
	chunks, err := w.chunksToCompute(nil, now)
	if err != nil {
		t.Fatalf("chunksToCompute: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("fully-covered history should yield ONLY the trailing chunk, got %d: %+v", len(chunks), chunks)
	}
	if got := chunks[0].To.Format("2006-01-02"); got != "2026-07-29" {
		t.Errorf("trailing chunk must end TODAY (partial day stays fresh), got %s", got)
	}
	if got := chunks[0].From.Format("2006-01-02"); got != "2026-07-27" {
		t.Errorf("trailing chunk should span %d days back, got from=%s", growthTrailingDays, got)
	}
}

// An empty table (first boot) must not try to compute the whole lake history
// in one pass — it is capped so the shared primary is never monopolised.
func TestChunksToCompute_BackfillIsCapped(t *testing.T) {
	w := newTestGrowthWorker(t)
	w.coveredOverride = func() map[string]bool { return map[string]bool{} }
	now := time.Date(2026, 7, 29, 15, 0, 0, 0, w.loc)
	chunks, err := w.chunksToCompute(nil, now)
	if err != nil {
		t.Fatalf("chunksToCompute: %v", err)
	}
	if want := 1 + growthBackfillChunks; len(chunks) != want {
		t.Fatalf("expected trailing + %d backfill chunk(s) = %d, got %d", growthBackfillChunks, want, len(chunks))
	}
	// Backfill walks BACKWARDS from the day before the trailing window, and
	// chunks must not overlap it.
	if !chunks[1].To.Before(chunks[0].From) {
		t.Errorf("backfill chunk %s..%s overlaps the trailing window starting %s",
			chunks[1].From.Format("2006-01-02"), chunks[1].To.Format("2006-01-02"),
			chunks[0].From.Format("2006-01-02"))
	}
	for i, c := range chunks {
		if c.From.After(c.To) {
			t.Errorf("chunk %d inverted: %s..%s", i, c.From, c.To)
		}
	}
}

// Nothing before the lake epoch is computable, so the backfill must stop there
// instead of walking backwards forever writing empty days.
func TestChunksToCompute_StopsAtLakeEpoch(t *testing.T) {
	w := newTestGrowthWorker(t)
	w.coveredOverride = func() map[string]bool { return map[string]bool{} }
	// Only a few days after the epoch — the backfill has almost nothing to do.
	epoch, _ := time.ParseInLocation("2006-01-02", growthLakeEpoch, w.loc)
	now := epoch.AddDate(0, 0, 4).Add(15 * time.Hour)
	chunks, err := w.chunksToCompute(nil, now)
	if err != nil {
		t.Fatalf("chunksToCompute: %v", err)
	}
	for _, c := range chunks {
		if c.From.Before(epoch) {
			t.Errorf("chunk starts %s, before the lake epoch %s", c.From.Format("2006-01-02"), growthLakeEpoch)
		}
	}
}

// The two PG statements are the whole engagement half; these pin the
// properties that would silently corrupt every rate.
func TestGrowthPGSQL_Invariants(t *testing.T) {
	// Upserts, so a double-fire recomputes identical rows rather than
	// duplicating or erroring (restart/re-run safety).
	for name, sql := range map[string]string{"engagement": growthEngagementSQL, "unsubs": growthUnsubSQL} {
		if !strings.Contains(sql, "ON CONFLICT (day, sending_domain, isp) DO UPDATE") {
			t.Errorf("%s: must upsert on the composite key", name)
		}
	}

	// Each half owns ONLY its own columns — otherwise re-running one half
	// would zero the other's counters.
	if strings.Contains(growthEngagementSQL, "unsubs") {
		t.Error("engagement statement must not touch unsubs (owned by the unsub statement)")
	}
	for _, col := range []string{"delivered", "hard_bounce", "soft_bounce", "complaints"} {
		if strings.Contains(growthEngagementSQL, col) || strings.Contains(growthUnsubSQL, col) {
			t.Errorf("PG statements must not touch the lake-owned column %q", col)
		}
	}

	// Raw event_at bounds so the planner prunes the monthly partitions — the
	// Denver day is derived in the projection only.
	if !strings.Contains(growthEngagementSQL, "event_at >= $1 AND event_at < $2") {
		t.Error("engagement: event_at bound must stay RAW for partition pruning")
	}

	// Unsubscribed events carry NO recipient_domain, so their ISP must come
	// from the subscriber's own address via the join.
	if !strings.Contains(growthUnsubSQL, "JOIN mailing_subscribers s ON s.id = e.subscriber_id") {
		t.Error("unsubs: ISP must be classified from the subscriber's own email (no recipient_domain on unsub rows)")
	}
	if !strings.Contains(growthUnsubSQL, "split_part(s.email, '@', 2)") {
		t.Error("unsubs: ISP must classify off s.email")
	}

	// Both halves must key on the SAME apex as the lake's brand column.
	for name, sql := range map[string]string{"engagement": growthEngagementSQL, "unsubs": growthUnsubSQL} {
		if !strings.Contains(sql, `'^(em|m)\.'`) {
			t.Errorf("%s: sending_domain must be normalised to the brand apex", name)
		}
	}

	// The action-click cohort must stay identical to the platform's canonical
	// predicate — asset fetches are ~93% of raw clicked events.
	if !strings.Contains(growthEngagementSQL, "cloudfront") || !strings.Contains(growthEngagementSQL, "unsub|optout") {
		t.Error("engagement: action-click predicate drifted from C_CLICK_ACTION")
	}
}

// The delivery half must fully replace a chunk (zero then rewrite) so volume
// that moved between sending domains cannot leave a stale bucket behind.
func TestGrowthDeliverySQL_ZeroesBeforeRewrite(t *testing.T) {
	if !strings.Contains(growthZeroDeliverySQL, "delivered = 0") {
		t.Error("delivery rewrite must zero the chunk first")
	}
	for _, col := range []string{"open_events", "click_events", "unsubs"} {
		if strings.Contains(growthZeroDeliverySQL, col) {
			t.Errorf("the delivery zeroing must NOT clear the engagement column %q", col)
		}
	}
	if !strings.Contains(growthDeliverySQL, "ON CONFLICT (day, sending_domain, isp) DO UPDATE") {
		t.Error("delivery insert must upsert")
	}
}

func TestGrowthISPCase_MatchesCanonicalBuckets(t *testing.T) {
	sql := growthISPCase("recipient_domain")
	for _, want := range []string{
		"'microsoft'", "'gmail'", "'yahoo'", "'apple'", "'comcast'", "'aol'",
		"'att'", "'sbcglobal'", "'cox'", "'charter'", "'verizon'", "ELSE 'other'",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("ISP case missing bucket %s", want)
		}
	}
	if !strings.Contains(sql, "lower(recipient_domain)") {
		t.Error("ISP case must lower-case the domain expression")
	}
}
