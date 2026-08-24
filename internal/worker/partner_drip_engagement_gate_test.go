package worker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The gate must be OFF for lanes outside the operator's internal-feed scope,
// ON for the whole internal_auto family (including lanes that do not exist
// yet), and OFF entirely when the kill switch is set.
func TestVerticalEngagementGated_Scope(t *testing.T) {
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")

	gated := []string{
		"internal_auto_insurance",
		"internal_auto_insurance_v3",
		"internal_auto_insurance_v7",
		"internal_auto_insurance_gmail_v1",
		"internal_auto_insurance_gmail_v2",
		"INTERNAL_AUTO_INSURANCE_V9", // future lane, case-insensitive
		"  internal_auto_insurance_v5  ",
	}
	for _, v := range gated {
		if !VerticalEngagementGated(v) {
			t.Errorf("expected %q to be engagement-gated", v)
		}
	}

	// Partner lanes carry the 250k/day target and are explicitly NOT gated.
	ungated := []string{
		"refi_heloc", "consumer", "remodel", "flooring", "term_life",
		"auto_insurance", // must NOT be caught by the internal_auto_* prefix
		"metal_roofing_signal", "samsclub_internal", "jarvis_att", "",
	}
	for _, v := range ungated {
		if VerticalEngagementGated(v) {
			t.Errorf("expected %q NOT to be engagement-gated", v)
		}
	}
}

func TestVerticalEngagementGated_KillSwitch(t *testing.T) {
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")
	for _, val := range []string{"1", "true"} {
		t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED", val)
		if VerticalEngagementGated("internal_auto_insurance_v7") {
			t.Fatalf("kill switch %q did not disable the gate", val)
		}
		if got := engagementGateSQL("internal_auto_insurance_v7", ""); got != "" {
			t.Fatalf("kill switch %q left SQL behind: %q", val, got)
		}
		if got := engagementGateAnyVerticalSQL("q"); got != "" {
			t.Fatalf("kill switch %q left cross-vertical SQL behind: %q", val, got)
		}
	}
}

func TestGatedVerticalPrefixes_Override(t *testing.T) {
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED", "")

	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "consumer, Term_Life ")
	if !VerticalEngagementGated("consumer") || !VerticalEngagementGated("term_life") {
		t.Fatal("override did not gate the listed verticals")
	}
	// Override REPLACES the default set — the internal family is no longer gated.
	if VerticalEngagementGated("internal_auto_insurance_v7") {
		t.Fatal("override should replace, not extend, the default scope")
	}

	// A blank/garbage override falls back to the DEFAULT scope. The gate is a
	// standing rule: only the kill switch may turn it off, so an accidentally
	// blanked deploy variable must not silently un-gate the internal feeds.
	for _, blank := range []string{"", "  ", "  , ", ",,,"} {
		t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", blank)
		if !VerticalEngagementGated("internal_auto_insurance_v7") {
			t.Fatalf("blank override %q un-gated the internal feeds", blank)
		}
		if VerticalEngagementGated("consumer") {
			t.Fatalf("blank override %q leaked the previous override", blank)
		}
	}
}

// An ungated vertical must produce EMPTY SQL so its claim query stays
// byte-identical to the pre-gate behavior — the property that makes this change
// safe for the partner lanes.
func TestEngagementGateSQL_UngatedIsEmpty(t *testing.T) {
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")
	if got := engagementGateSQL("refi_heloc", ""); got != "" {
		t.Fatalf("ungated vertical emitted SQL: %q", got)
	}
}

// The window a record is judged on MUST be the touch gap: a touch is sent at
// next_touch_at - followupTouchGapHours, so anything else silently judges the
// wrong touch.
func TestEngagementGateSQL_WindowIsTouchGap(t *testing.T) {
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")

	sql := engagementGateSQL("internal_auto_insurance_v7", "")
	if sql == "" {
		t.Fatal("gated vertical emitted no SQL")
	}
	for _, want := range []string{
		"last_click_at",
		"last_open_at",
		"INTERVAL '24 hours'",
		"next_touch_at - INTERVAL '24 hours'",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("gate SQL missing %q\n%s", want, sql)
		}
	}
	// Opens AND clicks both advance (operator: "openers and clickers").
	if strings.Count(sql, ">=") != 2 {
		t.Errorf("expected both open and click comparisons, got:\n%s", sql)
	}
	// Must AND onto an existing WHERE chain, never start its own clause.
	if !strings.HasPrefix(strings.TrimSpace(sql), "AND (") {
		t.Errorf("gate SQL must be an AND fragment, got:\n%s", sql)
	}
	// engaged_at means EXIT and must not be conflated with progression.
	if strings.Contains(sql, "engaged_at") {
		t.Errorf("gate must not read engaged_at:\n%s", sql)
	}
}

func TestEngagementGateSQL_AliasQualifies(t *testing.T) {
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")

	sql := engagementGateSQL("internal_auto_insurance", "q")
	for _, want := range []string{"q.last_click_at", "q.last_open_at", "q.next_touch_at"} {
		if !strings.Contains(sql, want) {
			t.Errorf("alias not applied, missing %q\n%s", want, sql)
		}
	}
}

// The cross-vertical form is used by scans that cannot specialise per lane. It
// must let ungated lanes through untouched while still gating the internal ones.
func TestEngagementGateAnyVerticalSQL_Shape(t *testing.T) {
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")

	sql := engagementGateAnyVerticalSQL("q")
	if sql == "" {
		t.Fatal("expected cross-vertical gate SQL")
	}
	for _, want := range []string{
		"NOT (LOWER(q.vertical) LIKE 'internal_auto_insurance%')",
		"q.last_click_at",
		"q.last_open_at",
		"q.next_touch_at - INTERVAL '24 hours'",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("cross-vertical SQL missing %q\n%s", want, sql)
		}
	}
}

func TestQuoteSQLLiteral(t *testing.T) {
	cases := map[string]string{
		"internal_auto%": "'internal_auto%'",
		"o'brien%":       "'o''brien%'",
	}
	for in, want := range cases {
		if got := quoteSQLLiteral(in); got != want {
			t.Errorf("quoteSQLLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestColdGraceHours(t *testing.T) {
	t.Setenv("PARTNER_DRIP_COLD_GRACE_HOURS", "")
	if got := coldGraceHours(); got != defaultColdGraceHours {
		t.Errorf("default grace = %d, want %d", got, defaultColdGraceHours)
	}
	t.Setenv("PARTNER_DRIP_COLD_GRACE_HOURS", "12")
	if got := coldGraceHours(); got != 12 {
		t.Errorf("override grace = %d, want 12", got)
	}
	// A garbage or negative value falls back to the default rather than
	// bucketing records the instant they come due.
	for _, bad := range []string{"abc", "-3"} {
		t.Setenv("PARTNER_DRIP_COLD_GRACE_HOURS", bad)
		if got := coldGraceHours(); got != defaultColdGraceHours {
			t.Errorf("grace %q = %d, want default %d", bad, got, defaultColdGraceHours)
		}
	}
}

func TestVerticalPrefixPredicate(t *testing.T) {
	pred, args := verticalPrefixPredicate([]string{"internal_auto_insurance", "consumer"}, 1)
	if pred != "(LOWER(vertical) LIKE $1 OR LOWER(vertical) LIKE $2)" {
		t.Errorf("unexpected predicate: %s", pred)
	}
	if len(args) != 2 || args[0] != "internal_auto_insurance%" || args[1] != "consumer%" {
		t.Errorf("unexpected args: %v", args)
	}
	// Offset start so the fragment can follow other bound parameters.
	pred, _ = verticalPrefixPredicate([]string{"x"}, 4)
	if pred != "(LOWER(vertical) LIKE $4)" {
		t.Errorf("start offset ignored: %s", pred)
	}
}

// The cold bucket is a BUCKET, not a suppression — the reason string is the
// slicing key every retargeting/activation query will filter on, so pinning it
// guards against a silent rename.
func TestColdTerminalReasonIsStable(t *testing.T) {
	if ColdTerminalReason != "cold_no_engagement" {
		t.Fatalf("cold terminal reason changed to %q — every retargeting slice keys on the old value", ColdTerminalReason)
	}
}

// WIRING GUARD. This repo's most common defect is a gate that compiles, reads
// correctly, and is never consulted (Gate F, the is_machine_* columns, the
// still-empty cap_decisions trace). The gate is only real if every query that
// selects a record for its NEXT TOUCH applies it, so pin all four call sites in
// the orchestrator source.
func TestEngagementGateIsWiredIntoEveryClaimPath(t *testing.T) {
	src, err := os.ReadFile("partner_drip_orchestrator.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// Scope each check to the FUNCTION that selects follow-up records. The
	// welcome claim (claimRecordsByISPCaps) is deliberately NOT gated — touch 1
	// is the introduction, so there is no prior touch to have engaged with.
	fnBody := func(sig string) string {
		i := strings.Index(body, sig)
		if i < 0 {
			return ""
		}
		rest := body[i:]
		if j := strings.Index(rest, "\n}\n"); j > 0 {
			return rest[:j]
		}
		return rest
	}

	sites := []struct {
		name string
		sig  string
	}{
		{"follow-up claim (dominant pick + ranked + picked)",
			"func (po *PartnerDripOrchestrator) claimFollowupRecordsByISPCaps("},
		{"express follow-up brand selection",
			"func (po *PartnerDripOrchestrator) expressFollowupBrands("},
		{"follow-up verticals with due records",
			"func (po *PartnerDripOrchestrator) followupVerticalsWithDueRecords("},
	}
	for _, s := range sites {
		fn := fnBody(s.sig)
		if fn == "" {
			t.Errorf("%s: %q not found — update this guard alongside the refactor", s.name, s.sig)
			continue
		}
		if !strings.Contains(fn, "engGate") {
			t.Errorf("%s: engagement gate not applied — a record could earn a touch it did not engage with", s.name)
		}
	}

	// All three follow-up SQL blocks inside the claim must carry it, not just
	// one: the dominant-touch pick, the ranked CTE and the picked CTE.
	claim := fnBody("func (po *PartnerDripOrchestrator) claimFollowupRecordsByISPCaps(")
	if n := strings.Count(claim, "engGate"); n < 4 {
		t.Errorf("follow-up claim applies the gate %d time(s); want the declaration plus all 3 SQL blocks", n)
	}

	// The WELCOME claim must stay ungated — gating touch 1 would stop the lane
	// introducing anyone and silently kill the feed.
	if w := fnBody("func (po *PartnerDripOrchestrator) claimRecordsByISPCaps("); w == "" {
		t.Error("welcome claim not found")
	} else if strings.Contains(w, "engGate") {
		t.Error("welcome claim is gated — touch 1 has no prior touch to engage with; this would kill the feed")
	}

	// The two gate builders must be the ONLY source of the predicate: an
	// open-coded copy would drift from the cold sweep and start bucketing
	// records the claim would still have mailed.
	if n := strings.Count(body, "last_open_at"); n != 0 {
		t.Errorf("orchestrator open-codes last_open_at %d time(s); use engagementGateSQL/engagementGateAnyVerticalSQL", n)
	}
}

// The cold sweep must bucket EXACTLY the rows the gate refuses. If the two
// windows ever diverge, records are either bucketed while still mailable or
// left due forever. Both are derived from followupTouchGapHours — pin that.
func TestColdSweepWindowMatchesGateWindow(t *testing.T) {
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")

	src, err := os.ReadFile("partner_drip_engagement_gate.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	gate := engagementGateSQL("internal_auto_insurance", "")
	if !strings.Contains(gate, "next_touch_at - INTERVAL '24 hours'") {
		t.Fatalf("gate window is not the touch gap:\n%s", gate)
	}
	// The sweep's NOT(...) clause is built from the same constant.
	if !strings.Contains(body, "next_touch_at - INTERVAL '%[6]d hours'") {
		t.Error("cold sweep no longer derives its window from followupTouchGapHours")
	}
	if followupTouchGapHours != 24 {
		t.Errorf("touch gap changed to %d — re-measure the engagement lag curve before shipping", followupTouchGapHours)
	}
}

// BOOT-WINDOW GUARD. The migration adds last_open_at/last_click_at as NULL on
// every row. If the cold sweep ran before the marker's first pass, all ~90k
// live internal ladder rows — including the ~6.5k genuinely engaged — would be
// bucketed at once. The sweeper must fail CLOSED until signals exist.
func TestColdSweepHeldUntilProgressionSignalsStamped(t *testing.T) {
	prev := progressionSignalsStamped.Load()
	t.Cleanup(func() { progressionSignalsStamped.Store(prev) })

	progressionSignalsStamped.Store(false)
	if progressionSignalsStamped.Load() {
		t.Fatal("flag did not reset")
	}
	// A nil-db sweeper proves the ordering guard short-circuits before any SQL:
	// if bucketing were attempted here it would panic on the nil *sql.DB.
	s := &PartnerDripColdSweeper{db: nil, interval: time.Minute, batchSize: 10, maxBatch: 1}
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")
	t.Setenv("PARTNER_DRIP_COLD_SWEEP_DISABLED", "")
	t.Setenv("PARTNER_DRIP_COLD_REVIVE_DISABLED", "1") // isolate the cold half
	s.sweepOnce(context.Background())                  // must not panic

	MarkProgressionSignalsReady()
	if !progressionSignalsStamped.Load() {
		t.Fatal("MarkProgressionSignalsReady did not latch")
	}
}

// The marker must latch readiness, or the sweeper stays held forever and the
// cold bucket never fills.
func TestMarkerLatchesProgressionReadiness(t *testing.T) {
	src, err := os.ReadFile("partner_engagement_marker.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "MarkProgressionSignalsReady()") {
		t.Fatal("engagement marker never latches progression readiness — cold sweep would stay held forever")
	}
	if !strings.Contains(body, "markProgressionSignals") {
		t.Fatal("engagement marker no longer stamps progression signals — the gate would refuse every record")
	}
	// Opens must be part of the progression signal (operator: openers AND
	// clickers), even though engaged_at remains clicks-only.
	i := strings.Index(body, "func (m *PartnerEngagementMarker) markProgressionSignals(")
	if i < 0 {
		t.Fatal("markProgressionSignals not found")
	}
	fn := body[i:]
	if j := strings.Index(fn, "\n}\n"); j > 0 {
		fn = fn[:j]
	}
	for _, want := range []string{"'opened'", "'clicked'", "last_open_at", "last_click_at", "GREATEST"} {
		if !strings.Contains(fn, want) {
			t.Errorf("progression-signal SQL missing %q", want)
		}
	}
}
