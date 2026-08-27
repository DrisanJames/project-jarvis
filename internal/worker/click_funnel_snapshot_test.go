package worker

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

// ── the rounding defect the audit found ─────────────────────────────────────

// TestRoundPct_RoundsInsteadOfTruncating pins METRIC_CONTRACT §10.10. round2
// (mailing_profiles.go) computes float64(int(f*100))/100 — TRUNCATION — which
// rendered a 0.0186% conversion rate as 0.01% and a 3.3565% step-through as
// 3.35%. Every rate on the funnel screen was biased downward.
func TestRoundPct_RoundsInsteadOfTruncating(t *testing.T) {
	cases := []struct {
		in        float64
		want      float64
		truncated float64
	}{
		{0.018648, 0.02, 0.01}, // offer 420 touch 1 conversion rate
		{0.086377, 0.09, 0.08}, // touch 2
		{0.096117, 0.10, 0.09}, // touch 3
		{3.356563, 3.36, 3.35}, // touch 4 step-through
		{31.923601, 31.92, 31.92},
	}
	for _, c := range cases {
		got := roundPct(c.in)
		if got != c.want {
			t.Errorf("roundPct(%v) = %v, want %v", c.in, got, c.want)
		}
		if c.want != c.truncated && got == c.truncated {
			t.Errorf("roundPct(%v) returned the TRUNCATED value %v — the defect is back", c.in, got)
		}
	}
}

func TestCfRate_GuardsDenominator(t *testing.T) {
	if got := cfRate(5, 0); got != 0 {
		t.Fatalf("cfRate with zero denominator must be 0, got %v", got)
	}
	if got := cfRate(671, 2932); got != 22.89 {
		// 671/2932 = 22.8853% -> rounds to 22.89 (the old truncating path gave 22.88)
		t.Fatalf("cfRate(671,2932) = %v, want 22.89", got)
	}
}

// ── exit classification (METRIC_CONTRACT §10.3) ─────────────────────────────

// TestExitClassSQL_NamesTheRealProductionReasons pins the classifier against
// the exact exit_reason strings read off prod on 2026-08-25. 130,070 of offer
// 420's 130,993 exits were two operator purges; filing them as attrition made a
// healthy lane read as dead.
func TestExitClassSQL_NamesTheRealProductionReasons(t *testing.T) {
	for _, marker := range []string{"operator", "retired", "lane_separation", "cleanup", "qa_probe"} {
		if !strings.Contains(clickFunnelExitClassSQL, marker) {
			t.Errorf("administrative marker %q missing — the June purges would be counted as lane attrition", marker)
		}
	}
	if !strings.Contains(clickFunnelExitClassSQL, "'^converted'") {
		t.Error("converted exits must be classified separately: they are a SUBSET of exits, not attrition")
	}
	if !strings.Contains(clickFunnelExitClassSQL, "ELSE 'behavioral'") {
		t.Error("the default must be behavioral — understating health beats inventing it")
	}
}

// ── ladder / maturity (METRIC_CONTRACT §10.2) ───────────────────────────────

func TestLadderHours_SumsOnlyDelayNodes(t *testing.T) {
	graph := mustGraph(t, `[
	  {"id":"trig","type":"trigger"},
	  {"id":"delay-1h","type":"delay","config":{"delayUnit":"hours","delayValue":1}},
	  {"id":"email-0","type":"email","config":{"reminder_sequence_index":0}},
	  {"id":"delay-5h","type":"delay","config":{"delayUnit":"hours","delayValue":5}},
	  {"id":"email-1","type":"email","config":{"reminder_sequence_index":1}},
	  {"id":"delay-18h","type":"delay","config":{"delayUnit":"hours","delayValue":18}},
	  {"id":"email-2","type":"email","config":{"reminder_sequence_index":2}},
	  {"id":"delay-48h","type":"delay","config":{"delayUnit":"hours","delayValue":48}},
	  {"id":"email-3","type":"email","config":{"reminder_sequence_index":3}},
	  {"id":"goal","type":"goal"}]`)
	// The production estate journey click-drip-4touch-72h.
	if got := ladderHours(graph); got != 72 {
		t.Fatalf("ladderHours = %v, want 72 (1+5+18+48)", got)
	}
}

// TestDelayMillis_HonoursBothGraphSpellings — reading only the snake_case pair
// made every delay node render with no duration.
func TestDelayMillis_HonoursBothGraphSpellings(t *testing.T) {
	camel := mustGraph(t, `[{"id":"d","type":"delay","config":{"delayUnit":"days","delayValue":2}}]`)
	if got := camel[0].delayMillis(); got != 2*86400000 {
		t.Fatalf("delayUnit/delayValue: got %d", got)
	}
	snake := mustGraph(t, `[{"id":"d","type":"delay","config":{"delay_hours":3,"delay_minutes":30}}]`)
	if got := snake[0].delayMillis(); got != 3*3600000+30*60000 {
		t.Fatalf("delay_hours/delay_minutes: got %d", got)
	}
}

func mustGraph(t *testing.T, raw string) []cfGraphNode {
	t.Helper()
	var g []cfGraphNode
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("graph: %v", err)
	}
	return g
}

// ── the app-stream rule (METRIC_CONTRACT §1) ────────────────────────────────

// TestApplyLakeRow_SplitsDeliveryFromEngagementBySource is the double-count
// guard. source='app' MIRRORS Postgres and duplicates transport delivery rows;
// open/click exist ONLY on 'app'. Applying the transport filter to engagement
// returns zero rows, and not applying it to delivery double-counts.
func TestApplyLakeRow_SplitsDeliveryFromEngagementBySource(t *testing.T) {
	var d ClickFunnelNodeDay

	// A duplicate 'delivered' from the app mirror must be IGNORED.
	applyLakeRow(&d, analytics.ClickFunnelLakeRow{Source: "app", EventType: "delivered", Events: 41})
	if d.Delivered != 0 {
		t.Fatalf("app-source delivered must not count (it duplicates the transport row), got %d", d.Delivered)
	}
	applyLakeRow(&d, analytics.ClickFunnelLakeRow{Source: "pmta", EventType: "delivered", Events: 35})
	applyLakeRow(&d, analytics.ClickFunnelLakeRow{Source: "kumo", EventType: "delivered", Events: 3})
	if d.Delivered != 38 {
		t.Fatalf("transport delivered = %d, want 38", d.Delivered)
	}

	// Engagement is app-ONLY. A transport-source click cannot exist and must
	// never be counted if one appears.
	applyLakeRow(&d, analytics.ClickFunnelLakeRow{Source: "pmta", EventType: "click", Recipients: 999})
	if d.ClicksRaw != 0 {
		t.Fatalf("transport-source click must not count, got %d", d.ClicksRaw)
	}
	applyLakeRow(&d, analytics.ClickFunnelLakeRow{
		Source: "app", EventType: "click", Recipients: 671, Classified: 671, Machine: 0})
	if d.ClicksRaw != 671 || d.ClicksClassified != 671 || d.ClicksMachine != 0 {
		t.Fatalf("app click split wrong: raw=%d classified=%d machine=%d", d.ClicksRaw, d.ClicksClassified, d.ClicksMachine)
	}

	// Deferrals count DISTINCT MAILBOXES, not events — delay notices are per
	// retry (1,481 events = 539 mailboxes on offer 420 touch 1).
	applyLakeRow(&d, analytics.ClickFunnelLakeRow{
		Source: "pmta", EventType: "delivery_delay", Events: 1481, Mailboxes: 539})
	if d.Deferred != 539 {
		t.Fatalf("deferred = %d, want 539 (mailboxes, not the 1481 events)", d.Deferred)
	}

	// relayed_to_ses is the reason the base is ACCEPTED, not delivered.
	applyLakeRow(&d, analytics.ClickFunnelLakeRow{Source: "pmta", EventType: "relayed_to_ses", Events: 2894})
	if d.Accepted() != 2932 {
		t.Fatalf("accepted = %d, want 2932 (38 delivered + 2894 relayed)", d.Accepted())
	}
}

// TestClicksQualified_NeverNegative — classification counts arrive from two
// aggregates and must not produce a negative qualified figure.
func TestClicksQualified_NeverNegative(t *testing.T) {
	d := ClickFunnelNodeDay{ClicksClassified: 5, ClicksMachine: 9}
	if got := d.ClicksQualified(); got != 0 {
		t.Fatalf("qualified = %d, want 0", got)
	}
}

// ── re-run safety ───────────────────────────────────────────────────────────

// TestLakeDayMerge_ReplacesRatherThanAccumulates is the idempotency guard. The
// incremental pass re-reads the last 3 days on every run and the reconcile pass
// re-reads 7; if the merge added instead of replacing, every tick would inflate
// the same day.
func TestLakeDayMerge_ReplacesRatherThanAccumulates(t *testing.T) {
	w := &ClickFunnelSnapshotWorker{
		days:          map[cfFlowKey]map[string]ClickFunnelNodeDay{},
		lakeAvailable: func() bool { return true },
		now:           func() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) },
	}
	key := cfFlowKey{Offer: "420", Node: "email-0"}
	campaigns := map[cfFlowKey]string{key: "41813624-0866-5614-adef-1bea80b77116"}

	w.lake = fakeCFLake{rows: []analytics.ClickFunnelLakeRow{
		{Dt: "2026-08-25", CampaignID: "41813624-0866-5614-adef-1bea80b77116",
			Source: "app", EventType: "open", Recipients: 936},
	}}

	var m1 ClickFunnelWatermarks
	w.refreshLake(context.Background(), campaigns, &m1)
	if got := w.days[key]["2026-08-25"].Opens; got != 936 {
		t.Fatalf("first pass opens = %d, want 936", got)
	}

	// Force the throttle open and run the SAME window again.
	w.lastLakeAt = time.Time{}
	w.lastReconcileAt = time.Time{}
	var m2 ClickFunnelWatermarks
	w.refreshLake(context.Background(), campaigns, &m2)
	if got := w.days[key]["2026-08-25"].Opens; got != 936 {
		t.Fatalf("re-run opens = %d, want 936 — the merge ACCUMULATED instead of replacing", got)
	}
}

// TestRefreshLake_FailureKeepsLastGoodAndSaysSo — a lake error must degrade to
// STALE, never to zero. Rendering an outage as "0 opens" is how an operator
// concludes a healthy lane is dead.
func TestRefreshLake_FailureKeepsLastGoodAndSaysSo(t *testing.T) {
	key := cfFlowKey{Offer: "420", Node: "email-0"}
	w := &ClickFunnelSnapshotWorker{
		days: map[cfFlowKey]map[string]ClickFunnelNodeDay{
			key: {"2026-08-24": {Dt: "2026-08-24", Opens: 500}},
		},
		lakeAvailable: func() bool { return true },
		now:           func() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) },
		lake:          fakeCFLake{err: errFake},
	}
	var m ClickFunnelWatermarks
	w.refreshLake(context.Background(), map[cfFlowKey]string{key: "41813624-0866-5614-adef-1bea80b77116"}, &m)

	if m.LakeError == "" {
		t.Fatal("a lake failure must be recorded in the watermarks")
	}
	if got := w.days[key]["2026-08-24"].Opens; got != 500 {
		t.Fatalf("last good day was discarded on failure: opens=%d", got)
	}
	if m.MetricsThrough != "2026-08-24" {
		t.Fatalf("watermark must describe what the accumulator actually covers, got %q", m.MetricsThrough)
	}
	if cfDataQuality(m) != "degraded" {
		t.Fatalf("data quality = %q, want degraded", cfDataQuality(m))
	}
}

// TestIncrementalWindow_IsThreeDaysNotTwo pins the measured freshness finding:
// 8.6% of app-source engagement lands in the 1-2d bucket, so a 2-day window has
// zero margin.
func TestIncrementalWindow_IsThreeDaysNotTwo(t *testing.T) {
	if clickFunnelIncrementalDays < 3 {
		t.Fatalf("incremental window is %d days — measured app-source ingest lag needs >= 3", clickFunnelIncrementalDays)
	}
	if clickFunnelReconcileDays < 7 {
		t.Fatalf("reconcile window is %d days — the ses tail reaches 2-7d", clickFunnelReconcileDays)
	}
}

// ── keys ────────────────────────────────────────────────────────────────────

// TestLaneKey_CannotEscapeItsPrefix — object keys are built from data.
func TestLaneKey_CannotEscapeItsPrefix(t *testing.T) {
	k := ClickFunnelLaneKey("click-funnel-snapshots/", "../../etc/passwd")
	if strings.Contains(k, "..") || strings.Contains(k, "/etc/") {
		t.Fatalf("offer id escaped the prefix: %s", k)
	}
	if !strings.HasPrefix(k, "click-funnel-snapshots/lanes/") {
		t.Fatalf("unexpected key: %s", k)
	}
}

// ── retry policy ────────────────────────────────────────────────────────────

// TestClassifyJourneySendError_TerminalVsTransient. The three prod loops all
// carried "all IPs exhausted warmup limits" — a CAPACITY error that is
// legitimately transient, which is why attempt and elapsed caps (not
// classification alone) are what stop it.
func TestClassifyJourneySendError_TerminalVsTransient(t *testing.T) {
	terminal := []string{
		"click-drip send failed: PMTA API error (HTTP 422): Error: bad recipient",
		"click-drip send to x@y.com: no sending IPs configured for profile",
		"offer proof is not approved+active",
	}
	for _, m := range terminal {
		if got := classifyJourneySendError(errStr(m)); got != journeyRetryTerminal {
			t.Errorf("%q classified %s, want terminal", m, got)
		}
	}
	transient := []string{
		"click-drip send failed: all IPs exhausted warmup limits",
		"ensure shadow campaign: pq: canceling statement due to statement timeout",
		"PMTA API error (HTTP 502): gateway",
	}
	for _, m := range transient {
		if got := classifyJourneySendError(errStr(m)); got != journeyRetryTransient {
			t.Errorf("%q classified %s, want transient", m, got)
		}
	}
}

// TestJourneyRetryBackoff_GrowsAndCaps. The prod loop retried every ~2 minutes
// for 13 days; the first retry here is already 5 minutes and it caps at 6h.
func TestJourneyRetryBackoff_GrowsAndCaps(t *testing.T) {
	if got := journeyRetryBackoff(1); got != 5*time.Minute {
		t.Fatalf("attempt 1 backoff = %v, want 5m", got)
	}
	if got := journeyRetryBackoff(3); got != 20*time.Minute {
		t.Fatalf("attempt 3 backoff = %v, want 20m", got)
	}
	if got := journeyRetryBackoff(50); got != journeyRetryMaxDelay {
		t.Fatalf("backoff must cap at %v, got %v", journeyRetryMaxDelay, got)
	}
	// Monotonic up to the cap — a backoff that ever shrinks reopens the loop.
	prev := time.Duration(0)
	for i := 1; i <= 12; i++ {
		d := journeyRetryBackoff(i)
		if d < prev {
			t.Fatalf("backoff shrank at attempt %d: %v < %v", i, d, prev)
		}
		prev = d
	}
}

// TestJourneyRetry_BoundedTotalAttempts — the whole point. Under the OLD
// behaviour three enrollments produced 26,908 attempts; the caps must make an
// unbounded loop impossible.
func TestJourneyRetry_BoundedTotalAttempts(t *testing.T) {
	if journeyRetryMaxAttempts > 20 {
		t.Fatalf("max attempts %d is too loose", journeyRetryMaxAttempts)
	}
	total := time.Duration(0)
	for i := 1; i < journeyRetryMaxAttempts; i++ {
		total += journeyRetryBackoff(i)
	}
	if total < 2*time.Hour {
		t.Fatalf("the attempt ladder spans only %v — too eager to give up on a capacity blip", total)
	}
	if journeyRetryMaxElapsed > 48*time.Hour {
		t.Fatalf("max elapsed %v is too loose", journeyRetryMaxElapsed)
	}
}

// ── test doubles ────────────────────────────────────────────────────────────

type fakeCFLake struct {
	rows []analytics.ClickFunnelLakeRow
	err  error
}

func (f fakeCFLake) ClickFunnelDaily(ctx context.Context, from, to string, ids []string) ([]analytics.ClickFunnelLakeRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type errString string

func (e errString) Error() string { return string(e) }

func errStr(s string) error { return errString(s) }

var errFake = errStr("athena: query failed")

// ── the version-0 alias (METRIC_CONTRACT §10.12) ────────────────────────────

// TestResolveShadowCampaignID_Version0KeepsTheLegacyID is the migration guard
// for the single most dangerous change in this set.
//
// shadowCampaignID seeds a UUIDv5 with
// "click-drip-shadow-offer-<offer>-node-<node>[-v-<hash>]". ContentHash was
// never populated in production, so EVERY live campaign id was minted from the
// HASHLESS seed. Simply wiring the hash through would change the seed, change
// the id, and detach every historical lake event — all 22 lanes would have read
// zero engagement on their next send.
//
// The first version recorded for a (offer, node) must therefore inherit the
// legacy id.
func TestResolveShadowCampaignID_Version0KeepsTheLegacyID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := &JourneyClickDripSender{db: db}

	p := ClickDripSendParams{EverflowOfferID: "420", NodeID: "email-0", ContentHash: "deadbeef"}
	legacy := shadowCampaignID("420", "email-0", "")
	versioned := shadowCampaignID("420", "email-0", "deadbeef")
	if legacy == versioned {
		t.Fatal("precondition: the hash must change the id, otherwise this guard is meaningless")
	}
	// This is the id production actually carries today.
	if legacy != "41813624-0866-5614-adef-1bea80b77116" {
		t.Fatalf("legacy id drifted from the one in production: %s", legacy)
	}

	// Registry empty for this node => version 0.
	mock.ExpectQuery("FROM mailing_clickdrip_touch_versions").
		WithArgs("420", "email-0").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	got := s.resolveShadowCampaignID(context.Background(), p)
	if got != legacy {
		t.Fatalf("version 0 must inherit the legacy id.\n got %s\nwant %s\n(using %s would orphan every historical lake event)",
			got, legacy, versioned)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestResolveShadowCampaignID_NewVersionGetsItsOwnID — once a node HAS a
// recorded version, a different creative must mint a new id so the previous
// version's numbers freeze instead of blending.
func TestResolveShadowCampaignID_NewVersionGetsItsOwnID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := &JourneyClickDripSender{db: db}

	mock.ExpectQuery("FROM mailing_clickdrip_touch_versions").
		WithArgs("420", "email-0").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT shadow_campaign_id").
		WithArgs("420", "email-0", "newhash").
		WillReturnRows(sqlmock.NewRows([]string{"shadow_campaign_id"}))

	got := s.resolveShadowCampaignID(context.Background(),
		ClickDripSendParams{EverflowOfferID: "420", NodeID: "email-0", ContentHash: "newhash"})
	want := shadowCampaignID("420", "email-0", "newhash")
	if got != want {
		t.Fatalf("a genuinely new creative version must get its own id: got %s want %s", got, want)
	}
}

// TestResolveShadowCampaignID_RegistryErrorIsNotMemoized — a transient failure
// must not pin the touch to the legacy id for the process lifetime.
func TestResolveShadowCampaignID_RegistryErrorIsNotMemoized(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := &JourneyClickDripSender{db: db}
	p := ClickDripSendParams{EverflowOfferID: "420", NodeID: "email-0", ContentHash: "h1"}

	mock.ExpectQuery("FROM mailing_clickdrip_touch_versions").
		WithArgs("420", "email-0").WillReturnError(errStr("connection reset"))
	if got := s.resolveShadowCampaignID(context.Background(), p); got != shadowCampaignID("420", "email-0", "") {
		t.Fatalf("an unreachable registry must fall back to the legacy id, got %s", got)
	}

	// A second call must re-query, not serve a memoized error result.
	mock.ExpectQuery("FROM mailing_clickdrip_touch_versions").
		WithArgs("420", "email-0").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT shadow_campaign_id").
		WithArgs("420", "email-0", "h1").
		WillReturnRows(sqlmock.NewRows([]string{"shadow_campaign_id"}))
	_ = s.resolveShadowCampaignID(context.Background(), p)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the second call did not re-query — a transient error was memoized: %v", err)
	}
}

// TestJourneyRetryEject_HasAnIndependentKillSwitch — the backoff (subtractive)
// and the ejection (mutates a live enrollment) must be separately disableable,
// so a too-eager classifier can be neutralized in production without giving the
// 13-day hot loop back.
func TestJourneyRetryEject_HasAnIndependentKillSwitch(t *testing.T) {
	t.Setenv("JOURNEY_RETRY_EJECT_DISABLED", "1")
	if !journeyRetryEjectDisabled() {
		t.Fatal("kill switch did not read")
	}
	t.Setenv("JOURNEY_RETRY_EJECT_DISABLED", "")
	if journeyRetryEjectDisabled() {
		t.Fatal("kill switch must default OFF")
	}
}

// TestOrphanRepair_NamespaceMatchesTheSender — the repair proves its mapping by
// recomputing the UUIDv5 in SQL, so its namespace literal MUST equal the one
// the sender mints ids with. A drift would silently repair nothing (or, worse,
// match the wrong row).
func TestOrphanRepair_NamespaceMatchesTheSender(t *testing.T) {
	if clickFunnelShadowNamespace != clickDripShadowNamespace.String() {
		t.Fatalf("namespace drift: repair uses %s, sender uses %s",
			clickFunnelShadowNamespace, clickDripShadowNamespace.String())
	}
	// And the digest it proves against is the one production actually carries.
	if got := shadowCampaignID("420", "email-1", ""); got != "052acdeb-6656-5aa6-9ac8-46c445739985" {
		t.Fatalf("offer 420 email-1 legacy id = %s, want the orphan id observed in prod", got)
	}
}

// TestOrphanRepair_IsNotAMigration — the repair is a BACKFILL. It timed out and
// was silently skipped when it shipped in the 5s startup-migration slice
// (verified in prod 2026-08-25), so it must stay in the worker.
func TestOrphanRepair_IsNotAMigration(t *testing.T) {
	main, err := os.ReadFile("../../cmd/server/main.go")
	if err != nil {
		t.Skipf("cannot read main.go: %v", err)
	}
	if strings.Contains(string(main), "clickdrip_orphan_stamp_repair") {
		t.Fatal("the orphan repair is registered as a startup migration again — the 5s budget drops backfills silently")
	}
}

// TestJourneyRetryCtx_SurvivesAnExpiredSendContext is the regression guard for
// the defect that made the retry policy inert in production on the day it
// shipped.
//
// The bookkeeping used the caller's context — which at the moment a send fails
// is frequently already expired, because an expired context is one of the
// things that MAKES a send fail. Both writes then failed with "context deadline
// exceeded", so the backoff never landed and the 13-day hot loop continued
// exactly as before. The negative path is the whole test: hand these functions
// a dead context and the UPDATE must still execute.
func TestJourneyRetryCtx_SurvivesAnExpiredSendContext(t *testing.T) {
	dead, cancel := context.WithCancel(context.Background())
	cancel() // the send's context, already gone
	if dead.Err() == nil {
		t.Fatal("precondition: the context must be dead")
	}

	// The detached context must be usable even though its parent is not.
	live, cancel2 := journeyRetryCtx(dead)
	defer cancel2()
	if live.Err() != nil {
		t.Fatalf("detached context inherited the parent's cancellation: %v", live.Err())
	}
	if dl, ok := live.Deadline(); !ok || time.Until(dl) <= 0 {
		t.Fatal("detached context must carry its own live deadline, not the parent's expired one")
	}

	// End to end: the defer must still write with a dead parent context.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectExec("UPDATE mailing_journey_enrollments").
		WithArgs("enroll-clk-7d40ce26", 300).
		WillReturnResult(sqlmock.NewResult(0, 1))

	deferJourneyEnrollment(dead, db, "enroll-clk-7d40ce26", 5*time.Minute)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the backoff write did not happen with an expired send context — the policy is inert again: %v", err)
	}
}

// TestJourneyRetryCtx_KeepsContextValues — WithoutCancel must drop the deadline
// and KEEP the values, or tracing and org scope are lost on every retry write.
func TestJourneyRetryCtx_KeepsContextValues(t *testing.T) {
	type ctxKey string
	const k ctxKey = "org"
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), k, "acme"))
	cancel()

	live, cancel2 := journeyRetryCtx(parent)
	defer cancel2()
	if got, _ := live.Value(k).(string); got != "acme" {
		t.Fatalf("context values were dropped: %q", got)
	}
}
