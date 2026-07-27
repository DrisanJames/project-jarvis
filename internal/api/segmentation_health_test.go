package api

// Segmentation Command endpoint tests — verdict derivation (SLA math,
// unregistered discovery, static-declared), worker lights (missing heartbeat
// = UNKNOWN never OK), divergence flag, and the degraded paths (empty
// registry, churn/refs source failure) over sqlmock. No DB needed.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

const segHealthTestOrg = "11111111-2222-3333-4444-555555555555"

// ── Pure derivations ────────────────────────────────────────────────────────

func TestFamilyVerdict(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	within := now.Add(-47 * time.Hour)
	past := now.Add(-49 * time.Hour)
	exactly := now.Add(-48 * time.Hour)

	cases := []struct {
		name       string
		registered bool
		slaHours   int
		lastBuild  *time.Time
		want       string
	}{
		{"unregistered wins over everything", false, 48, &within, segVerdictUnregistered},
		{"registered, built within SLA", true, 48, &within, segVerdictLive},
		{"registered, built past SLA", true, 48, &past, segVerdictStale},
		{"exactly at SLA boundary is still LIVE", true, 48, &exactly, segVerdictLive},
		{"registered, never built, SLA declared = STALE (staleness can't hide)", true, 48, nil, segVerdictStale},
		{"registered, sla_hours=0 = STATIC-DECLARED", true, 0, nil, segVerdictStatic},
		{"registered, negative sla = STATIC-DECLARED", true, -1, &past, segVerdictStatic},
	}
	for _, c := range cases {
		if got := familyVerdict(c.registered, c.slaHours, c.lastBuild, now); got != c.want {
			t.Errorf("%s: familyVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDeriveFamilyKey(t *testing.T) {
	cases := []struct {
		in, wantKey, wantPattern string
	}{
		{"SLOT-2026-07-26-gmail-CLK", "SLOT", "SLOT-%"},
		{"FRESH-J26-PARTNER-x", "FRESH", "FRESH-%"},
		{"DiscountBlog 7D Openers", "DiscountBlog", "DiscountBlog %"},
		{"orphan", "orphan", "orphan"},
		{"  ", "(unnamed)", "(unnamed)"},
		{"a-b", "a-b", "a-b"}, // 1-char prefix is degenerate — own family
	}
	for _, c := range cases {
		key, pattern := deriveFamilyKey(c.in)
		if key != c.wantKey || pattern != c.wantPattern {
			t.Errorf("deriveFamilyKey(%q) = (%q, %q), want (%q, %q)",
				c.in, key, pattern, c.wantKey, c.wantPattern)
		}
	}
}

func TestSegmentWorkerLight(t *testing.T) {
	cases := []struct {
		name     string
		hasBeat  bool
		stalled  bool
		status   string
		secs     int64
		interval int
		want     string
	}{
		{"no heartbeat row = UNKNOWN, never OK", false, false, "", 0, 0, workerLightUnknown},
		{"fresh ok beat", true, false, "ok", 100, 3600, workerLightOK},
		{"monitor-flagged stalled", true, true, "ok", 100, 3600, workerLightStalled},
		{"last cycle errored", true, false, "error", 100, 3600, workerLightError},
		{"beat older than 2x cadence = stale", true, false, "ok", 7201, 3600, workerLightStale},
		{"short cadence gets the 5m floor", true, false, "ok", 250, 10, workerLightOK},
		{"past the 5m floor", true, false, "ok", 301, 10, workerLightStale},
	}
	for _, c := range cases {
		if got := segmentWorkerLight(c.hasBeat, c.stalled, c.status, c.secs, c.interval); got != c.want {
			t.Errorf("%s: light = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSegmentCountsDiverged(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	countFresh := now
	buildOld := now.Add(-72 * time.Hour)
	buildFresh := now.Add(-1 * time.Hour)

	if !segmentCountsDiverged(&countFresh, &buildOld) {
		t.Error("counts refreshed 72h after membership build must flag divergence")
	}
	if !segmentCountsDiverged(&countFresh, nil) {
		t.Error("counts present with membership never built must flag divergence")
	}
	if segmentCountsDiverged(&countFresh, &buildFresh) {
		t.Error("counts and membership both fresh is not divergent")
	}
	if segmentCountsDiverged(nil, &buildOld) {
		t.Error("no count clock = nothing to diverge")
	}
}

// ── Assembly (pure, no DB) ──────────────────────────────────────────────────

func tp(t time.Time) *time.Time { return &t }

func TestBuildSegmentationFamilies_RegistryEmptyAllUnregistered(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	segments := []segHealthSegmentRow{
		{ID: "s1", Name: "SLOT-2026-gmail", SegmentType: "static", SubscriberCount: 100},
		{ID: "s2", Name: "SLOT-2026-yahoo", SegmentType: "static", SubscriberCount: 50},
		{ID: "s3", Name: "Verified Humans DB", SegmentType: "dynamic", SubscriberCount: 10},
	}
	fams := buildSegmentationFamilies(nil, segments, nil, false, nil, nil, false, nil, false, now)
	if len(fams) != 2 {
		t.Fatalf("expected 2 discovered families, got %d: %+v", len(fams), fams)
	}
	for _, f := range fams {
		if f.Verdict != segVerdictUnregistered {
			t.Errorf("family %s verdict = %s, want UNREGISTERED", f.FamilyKey, f.Verdict)
		}
		if f.Registered {
			t.Errorf("family %s marked registered with an empty registry", f.FamilyKey)
		}
	}
	// Sorted by blast radius within the same verdict: SLOT (150) first.
	if fams[0].FamilyKey != "SLOT" || fams[0].SubscriberTotal != 150 {
		t.Errorf("expected SLOT family (150 subs) first, got %+v", fams[0])
	}
	if fams[0].StaticCount != 2 || fams[0].DynamicCount != 0 {
		t.Errorf("SLOT static/dynamic split wrong: %+v", fams[0])
	}
}

func TestBuildSegmentationFamilies_VerdictsAndAggregates(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	registry := []segHealthRegistryRow{
		{FamilyKey: "slot", FamilyPattern: "SLOT-%", Owner: "board", SLAHours: 48, Active: true},
		{FamilyKey: "welcome_saturated", FamilyPattern: "Welcome-Saturated%", Owner: "cron", SLAHours: 48, Active: true},
		{FamilyKey: "conduit", FamilyPattern: "CONDUIT-%", Owner: "pools", SLAHours: 0, Active: true},
	}
	built25hAgo := now.Add(-25 * time.Hour)
	built1hAgo := now.Add(-1 * time.Hour)
	built80hAgo := now.Add(-80 * time.Hour)
	segments := []segHealthSegmentRow{
		// slot: two segments, newest build 1h ago → LIVE; one built in 24h.
		{ID: "a", Name: "SLOT-x", SegmentType: "static", SubscriberCount: 10, MatchedPattern: "SLOT-%",
			LastBuiltAt: tp(built1hAgo), LastBuildStatus: "ok", LedgerUpdatedAt: tp(built1hAgo)},
		{ID: "b", Name: "SLOT-y", SegmentType: "static", SubscriberCount: 20, MatchedPattern: "SLOT-%",
			LastBuiltAt: tp(built25hAgo), LastBuildStatus: "failed", LedgerUpdatedAt: tp(built25hAgo)},
		// welcome: counts fresh, membership 80h old → STALE + divergent.
		{ID: "c", Name: "Welcome-Saturated-x", SegmentType: "dynamic", SubscriberCount: 5,
			MatchedPattern: "Welcome-Saturated%",
			LastCountAt: tp(now.Add(-1 * time.Hour)), LastBuiltAt: tp(built80hAgo),
			LastBuildStatus: "ok", LedgerUpdatedAt: tp(built80hAgo)},
		// conduit: family declares no SLA → STATIC-DECLARED.
		{ID: "d", Name: "CONDUIT-yahoo-DB", SegmentType: "dynamic", SubscriberCount: 7,
			MatchedPattern: "CONDUIT-%",
			LastBuiltAt: tp(built80hAgo), LastBuildStatus: "ok", LedgerUpdatedAt: tp(built80hAgo)},
	}
	churn := map[string]segHealthChurnRow{
		"a": {Inserts24h: 100, Inserts7d: 700},
		"b": {Inserts24h: 50, Inserts7d: 350},
	}
	refs := map[string]int{"a": 3, "c": 1}

	fams := buildSegmentationFamilies(registry, segments, churn, true, refs, nil, false, nil, false, now)
	if len(fams) != 3 {
		t.Fatalf("expected 3 families, got %d", len(fams))
	}
	byKey := map[string]segHealthFamily{}
	for _, f := range fams {
		byKey[f.FamilyKey] = f
	}

	slot := byKey["slot"]
	if slot.Verdict != segVerdictLive {
		t.Errorf("slot verdict = %s, want LIVE", slot.Verdict)
	}
	if slot.Builds24h != 1 {
		t.Errorf("slot builds_last_24h = %d, want 1 (only the 1h-ago build)", slot.Builds24h)
	}
	if slot.FailedNow != 1 {
		t.Errorf("slot failed_now = %d, want 1", slot.FailedNow)
	}
	if slot.MemberInserts24 != 150 || slot.MemberInserts7d != 1050 {
		t.Errorf("slot churn = (%d, %d), want (150, 1050)", slot.MemberInserts24, slot.MemberInserts7d)
	}
	if slot.CampaignRefs3d != 3 {
		t.Errorf("slot campaign_refs_3d = %d, want 3", slot.CampaignRefs3d)
	}
	if slot.LastBuildMax == nil || !slot.LastBuildMax.Equal(built1hAgo) ||
		slot.LastBuildMin == nil || !slot.LastBuildMin.Equal(built25hAgo) {
		t.Errorf("slot build min/max wrong: min=%v max=%v", slot.LastBuildMin, slot.LastBuildMax)
	}
	// Drilldown ordered newest build first.
	if len(slot.Segments) != 2 || slot.Segments[0].ID != "a" {
		t.Errorf("slot drilldown order wrong: %+v", slot.Segments)
	}

	welcome := byKey["welcome_saturated"]
	if welcome.Verdict != segVerdictStale {
		t.Errorf("welcome verdict = %s, want STALE (80h > 48h SLA)", welcome.Verdict)
	}
	if welcome.DivergedCount != 1 {
		t.Errorf("welcome diverged_count = %d, want 1 (counts fresh, membership stale)", welcome.DivergedCount)
	}

	conduit := byKey["conduit"]
	if conduit.Verdict != segVerdictStatic {
		t.Errorf("conduit verdict = %s, want STATIC-DECLARED", conduit.Verdict)
	}

	// Worst-first ordering: STALE before LIVE before STATIC-DECLARED.
	if fams[0].FamilyKey != "welcome_saturated" {
		t.Errorf("expected STALE family first, got %s", fams[0].FamilyKey)
	}
}

func TestBuildSegmentationFamilies_RegisteredButPurgedFamilyIsStale(t *testing.T) {
	// A registered family with a live SLA and ZERO segments (purged / never
	// built) must surface STALE — vanishing was the alarm blindness.
	now := time.Now()
	registry := []segHealthRegistryRow{
		{FamilyKey: "rings", FamilyPattern: "%-MPP-LIVE-%", Owner: "x", SLAHours: 24, Active: true},
	}
	fams := buildSegmentationFamilies(registry, nil, nil, false, nil, nil, false, nil, false, now)
	if len(fams) != 1 || fams[0].Verdict != segVerdictStale || fams[0].SegmentsCount != 0 {
		t.Fatalf("purged registered family: %+v", fams)
	}
}

func TestBuildSegmentationAlerts_Ranking(t *testing.T) {
	now := time.Now()
	fams := []segHealthFamily{
		{FamilyKey: "small-stale", Verdict: segVerdictStale, SubscriberTotal: 10, SLAHours: 24, LastBuildMax: tp(now.Add(-48 * time.Hour))},
		{FamilyKey: "big-unreg", Verdict: segVerdictUnregistered, SubscriberTotal: 9999},
		{FamilyKey: "big-stale", Verdict: segVerdictStale, SubscriberTotal: 5000, SLAHours: 24, LastBuildMax: tp(now.Add(-30 * time.Hour))},
		{FamilyKey: "fine", Verdict: segVerdictLive, SubscriberTotal: 88888},
	}
	alerts := buildSegmentationAlerts(fams, now)
	if len(alerts) != 3 {
		t.Fatalf("expected 3 alerts, got %d", len(alerts))
	}
	// Reds first (by blast radius), amber last even though it is biggest.
	if alerts[0].FamilyKey != "big-stale" || alerts[0].Severity != "red" {
		t.Errorf("alert[0] = %+v, want big-stale red", alerts[0])
	}
	if alerts[1].FamilyKey != "small-stale" {
		t.Errorf("alert[1] = %+v, want small-stale", alerts[1])
	}
	if alerts[2].FamilyKey != "big-unreg" || alerts[2].Severity != "amber" {
		t.Errorf("alert[2] = %+v, want big-unreg amber", alerts[2])
	}
	if alerts[0].HoursOverdue < 5.9 || alerts[0].HoursOverdue > 6.1 {
		t.Errorf("big-stale hours_overdue = %f, want ~6", alerts[0].HoursOverdue)
	}
}

func TestBuildSegmentationWorkers_MissingHeartbeatIsUnknown(t *testing.T) {
	beats := map[string]segHealthBeatRow{
		"segment_refresh": {LastBeatAt: time.Now(), SecondsSinceBeat: 30,
			LastStatus: "ok", CycleCount: 5, ExpectedIntervalSeconds: 3600},
	}
	workers := buildSegmentationWorkers(beats, nil)
	byName := map[string]segHealthWorker{}
	for _, w := range workers {
		byName[w.Name] = w
	}
	for _, expected := range segmentationExpectedWorkers {
		if _, ok := byName[expected]; !ok {
			t.Fatalf("expected worker %s missing from response", expected)
		}
	}
	if byName["segment_refresh"].Light != workerLightOK {
		t.Errorf("segment_refresh light = %s, want ok", byName["segment_refresh"].Light)
	}
	for _, name := range []string{"segment_cleanup", "verified_humans_ledger", "partner_human_rollup"} {
		if got := byName[name].Light; got != workerLightUnknown {
			t.Errorf("%s light = %s, want unknown (no heartbeat row must NEVER read as ok)", name, got)
		}
		if byName[name].LastBeatAt != nil {
			t.Errorf("%s has a last_beat_at with no heartbeat row", name)
		}
	}
}

// ── Handler over sqlmock (degraded paths) ───────────────────────────────────

func segHealthRouter(svc *SegmentationHealthService) *chi.Mux {
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	return r
}

func segHealthSegmentCols() []string {
	return []string{"id", "name", "segment_type", "subscriber_count", "last_count_at",
		"last_built_at", "last_build_status", "ledger_count", "build_source",
		"ledger_updated_at", "family_pattern"}
}

// TestSegmentationHealth_DegradedSourcesStillRender: registry query errors
// (→ all families UNREGISTERED + registry_available=false), churn and refs
// queries error (→ churn_method/refs_method 'unavailable'), heartbeats and
// runs error (→ expected workers UNKNOWN). The endpoint still returns 200
// with truthful families.
func TestSegmentationHealth_DegradedSourcesStillRender(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM mailing_segment_registry`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(errFake("registry down"))
	mock.ExpectQuery(`FROM mailing_segments s`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(segHealthSegmentCols()).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", "SLOT-x", "static", 100,
				now, now.Add(-2*time.Hour), "ok", 100, "materializer", now.Add(-2*time.Hour), "").
			AddRow("aaaaaaaa-0000-0000-0000-000000000002", "SLOT-y", "static", 50,
				nil, nil, "none", 0, "", nil, ""))
	mock.ExpectQuery(`FROM mailing_segment_members`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(errFake("churn timeout"))
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(errFake("no pmta_config column"))
	// Perf rollup probe fails (table absent on this DB) → perf 'unavailable';
	// the window/timeline queries must NOT run.
	mock.ExpectQuery(`FROM mailing_segment_perf_daily`).
		WillReturnError(errFake("relation does not exist"))
	mock.ExpectQuery(`FROM mailing_worker_heartbeats`).
		WillReturnError(errFake("heartbeats missing"))
	mock.ExpectQuery(`FROM mailing_worker_runs`).
		WillReturnError(errFake("runs missing"))

	svc := NewSegmentationHealthService(db)
	req := httptest.NewRequest(http.MethodGet, "/segmentation/health", nil)
	req.Header.Set("X-Organization-ID", segHealthTestOrg)
	rec := httptest.NewRecorder()
	segHealthRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out segmentationHealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RegistryAvailable {
		t.Error("registry_available must be false when the registry read failed")
	}
	if out.ChurnMethod != "unavailable" || out.RefsMethod != "unavailable" {
		t.Errorf("degraded methods: churn=%s refs=%s, want unavailable/unavailable", out.ChurnMethod, out.RefsMethod)
	}
	if out.PerfMethod != "unavailable" || out.ChurnTimelineMethod != "unavailable" {
		t.Errorf("degraded perf methods: perf=%s timeline=%s, want unavailable", out.PerfMethod, out.ChurnTimelineMethod)
	}
	if out.Families[0].Perf != nil {
		t.Error("family perf must be null (not zeros) when the rollup is unavailable")
	}
	if len(out.Families) != 1 || out.Families[0].Verdict != segVerdictUnregistered {
		t.Fatalf("expected one UNREGISTERED SLOT family, got %+v", out.Families)
	}
	if out.Families[0].Segments[0].MemberInserts24 != nil {
		t.Error("member_inserts_24h must be null (not 0) when churn is unavailable")
	}
	if out.Summary.FamiliesUnregistered != 1 || out.Summary.SegmentsTotal != 2 {
		t.Errorf("summary wrong: %+v", out.Summary)
	}
	// Workers all UNKNOWN — no heartbeat source must never render OK.
	if len(out.Workers) < len(segmentationExpectedWorkers) {
		t.Fatalf("expected >= %d workers, got %d", len(segmentationExpectedWorkers), len(out.Workers))
	}
	for _, w := range out.Workers {
		if w.Light != workerLightUnknown {
			t.Errorf("worker %s light = %s, want unknown", w.Name, w.Light)
		}
	}
	// One amber alert for the unregistered family.
	if len(out.Alerts) != 1 || out.Alerts[0].Severity != "amber" {
		t.Errorf("alerts = %+v, want one amber", out.Alerts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSegmentationHealth_HappyPath: registered family within SLA renders
// LIVE, churn + refs land, workers classify from real heartbeat rows.
func TestSegmentationHealth_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM mailing_segment_registry`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"family_key", "family_pattern", "owner",
			"cadence", "sla_hours", "keep_policy", "active"}).
			AddRow("slot", "SLOT-%", "board program", "daily", 48, "protect", true))
	mock.ExpectQuery(`FROM mailing_segments s`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(segHealthSegmentCols()).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", "SLOT-x", "static", 100,
				now, now.Add(-2*time.Hour), "ok", 100, "materializer", now.Add(-2*time.Hour), "SLOT-%"))
	mock.ExpectQuery(`FROM mailing_segment_members`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "i24", "i7"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", 42, 300))
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"val", "n"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", 2))
	// Perf rollup: probe true → window sums + churn timeline.
	mock.ExpectQuery(`FROM mailing_segment_perf_daily`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`FROM mailing_segment_perf_daily`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "delivered", "opens",
			"clicks", "complaints", "unsubs", "hard", "soft"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", 1000, 300, 25, 1, 2, 5, 9))
	mock.ExpectQuery(`FROM mailing_segment_perf_daily`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "day", "members", "added"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", "2026-07-24", 1000, 0).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", "2026-07-25", 990, 40))
	mock.ExpectQuery(`FROM mailing_worker_heartbeats`).
		WillReturnRows(sqlmock.NewRows([]string{"worker_name", "last_beat_at", "secs",
			"last_status", "last_error", "cycle_count", "expected_interval_seconds", "stalled"}).
			AddRow("segment_refresh", now, 60, "ok", "", 12, 3600, false).
			AddRow("segment_cleanup", now.Add(-10*time.Hour), 36000, "ok", "", 3, 3600, false))
	mock.ExpectQuery(`FROM mailing_worker_runs`).
		WillReturnRows(sqlmock.NewRows([]string{"worker_name", "status", "started_at"}).
			AddRow("segment_refresh", "ok", now.Add(-30*time.Minute)))

	svc := NewSegmentationHealthService(db)
	req := httptest.NewRequest(http.MethodGet, "/segmentation/health", nil)
	req.Header.Set("X-Organization-ID", segHealthTestOrg)
	rec := httptest.NewRecorder()
	segHealthRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out segmentationHealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.RegistryAvailable {
		t.Error("registry_available must be true")
	}
	if out.ChurnMethod != "materialized_window" || out.RefsMethod != "inclusion_segments_3d" {
		t.Errorf("methods: churn=%s refs=%s", out.ChurnMethod, out.RefsMethod)
	}
	if len(out.Families) != 1 {
		t.Fatalf("families = %+v", out.Families)
	}
	f := out.Families[0]
	if f.Verdict != segVerdictLive || f.MemberInserts24 != 42 || f.MemberInserts7d != 300 || f.CampaignRefs3d != 2 {
		t.Errorf("family wrong: %+v", f)
	}
	if out.PerfMethod != "member_scoped_rollup" || out.ChurnTimelineMethod != "rollup_daily" || out.PerfWindowDays != 7 {
		t.Errorf("perf methods wrong: %s / %s / %d", out.PerfMethod, out.ChurnTimelineMethod, out.PerfWindowDays)
	}
	if f.Perf == nil || f.Perf.Delivered != 1000 || f.Perf.ClicksAction != 25 ||
		f.Perf.HardBounces != 5 || f.Perf.SoftBounces != 9 {
		t.Errorf("family perf wrong: %+v", f.Perf)
	}
	if f.Perf.EngagementRate < 0.024 || f.Perf.EngagementRate > 0.026 {
		t.Errorf("engagement rate = %f, want 25/1000", f.Perf.EngagementRate)
	}
	// Timeline: day 2 removals derived: 1000 + 40 - 990 = 50.
	if len(f.ChurnTimeline) != 2 || f.ChurnTimeline[1].Removed != 50 || f.ChurnTimeline[1].Added != 40 {
		t.Errorf("churn timeline wrong: %+v", f.ChurnTimeline)
	}
	if seg := f.Segments[0]; seg.Perf == nil || seg.Perf.Opens != 300 {
		t.Errorf("segment perf wrong: %+v", seg.Perf)
	}
	if out.Summary.FamiliesLive != 1 || out.Summary.FamiliesStale != 0 {
		t.Errorf("summary wrong: %+v", out.Summary)
	}
	byName := map[string]segHealthWorker{}
	for _, w := range out.Workers {
		byName[w.Name] = w
	}
	if byName["segment_refresh"].Light != workerLightOK {
		t.Errorf("segment_refresh light = %s", byName["segment_refresh"].Light)
	}
	if byName["segment_cleanup"].Light != workerLightStale {
		t.Errorf("segment_cleanup light = %s, want stale (10h since beat on 1h cadence)", byName["segment_cleanup"].Light)
	}
	if byName["segment_refresh"].LastRunStatus != "ok" {
		t.Errorf("segment_refresh last_run_status = %s", byName["segment_refresh"].LastRunStatus)
	}
	if byName["verified_humans_ledger"].Light != workerLightUnknown {
		t.Errorf("verified_humans_ledger light = %s, want unknown", byName["verified_humans_ledger"].Light)
	}
	if len(out.Alerts) != 0 {
		t.Errorf("no alerts expected on a green estate, got %+v", out.Alerts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ── Perf-window derivations ─────────────────────────────────────────────────

func TestParsePerfWindow(t *testing.T) {
	if parsePerfWindow("") != 7 || parsePerfWindow("7d") != 7 || parsePerfWindow("bogus") != 7 {
		t.Error("default window must be 7")
	}
	if parsePerfWindow("30d") != 30 || parsePerfWindow("30") != 30 {
		t.Error("30d window must parse to 30")
	}
}

func TestDeriveChurnRemoved(t *testing.T) {
	// prev 1000, added 40, now 990 → 50 removed.
	if got := deriveChurnRemoved(1000, 40, 990); got != 50 {
		t.Errorf("removed = %d, want 50", got)
	}
	// Growth with no removals derivable → 0, never negative.
	if got := deriveChurnRemoved(1000, 0, 1200); got != 0 {
		t.Errorf("removed = %d, want 0 (floored)", got)
	}
	if got := deriveChurnRemoved(0, 0, 0); got != 0 {
		t.Errorf("removed = %d, want 0", got)
	}
}

func TestEngagementRate(t *testing.T) {
	if got := engagementRate(25, 1000); got != 0.025 {
		t.Errorf("rate = %f, want 0.025", got)
	}
	if got := engagementRate(5, 0); got != 0 {
		t.Errorf("rate on zero delivered = %f, want 0 (no divide-by-zero)", got)
	}
}

func TestBuildSegmentationFamilies_PerfAggregationAndTimeline(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	built := now.Add(-1 * time.Hour)
	registry := []segHealthRegistryRow{
		{FamilyKey: "slot", FamilyPattern: "SLOT-%", Owner: "board", SLAHours: 48, Active: true},
	}
	segments := []segHealthSegmentRow{
		{ID: "a", Name: "SLOT-x", SegmentType: "static", SubscriberCount: 10, MatchedPattern: "SLOT-%",
			LastBuiltAt: tp(built), LastBuildStatus: "ok", LedgerUpdatedAt: tp(built)},
		{ID: "b", Name: "SLOT-y", SegmentType: "static", SubscriberCount: 20, MatchedPattern: "SLOT-%",
			LastBuiltAt: tp(built), LastBuildStatus: "ok", LedgerUpdatedAt: tp(built)},
	}
	perf := map[string]segHealthPerf{
		"a": {Delivered: 600, Opens: 100, ClicksAction: 12, Complaints: 1, Unsubs: 2, HardBounces: 3, SoftBounces: 4},
		"b": {Delivered: 400, Opens: 50, ClicksAction: 8, HardBounces: 1},
	}
	daily := map[string][]segHealthChurnDailyRow{
		"a": {{Day: "2026-07-24", Members: 500, Added: 0}, {Day: "2026-07-25", Members: 480, Added: 30}},
		"b": {{Day: "2026-07-24", Members: 200, Added: 0}, {Day: "2026-07-25", Members: 220, Added: 20}},
	}
	fams := buildSegmentationFamilies(registry, segments, nil, false, nil, perf, true, daily, true, now)
	if len(fams) != 1 {
		t.Fatalf("families = %d", len(fams))
	}
	f := fams[0]
	// Family sums: hard and soft stay SEPARATE (bounce doctrine).
	if f.Perf == nil || f.Perf.Delivered != 1000 || f.Perf.Opens != 150 || f.Perf.ClicksAction != 20 ||
		f.Perf.HardBounces != 4 || f.Perf.SoftBounces != 4 || f.Perf.Complaints != 1 || f.Perf.Unsubs != 2 {
		t.Fatalf("family perf sums wrong: %+v", f.Perf)
	}
	if f.Perf.EngagementRate != 0.02 {
		t.Errorf("family engagement rate = %f, want 20/1000", f.Perf.EngagementRate)
	}
	// Timeline aggregated across both segments:
	// 07-24: members 700, added 0; 07-25: members 700, added 50 → removed 50.
	if len(f.ChurnTimeline) != 2 {
		t.Fatalf("timeline: %+v", f.ChurnTimeline)
	}
	if d := f.ChurnTimeline[1]; d.Members != 700 || d.Added != 50 || d.Removed != 50 {
		t.Errorf("timeline day2 = %+v, want members 700 added 50 removed 50", d)
	}
	// Per-segment perf carried with per-segment rate.
	if f.Segments[0].Perf == nil || f.Segments[1].Perf == nil {
		t.Fatal("segment perf missing")
	}
}

// errFake is a trivial error type for sqlmock failure injection.
type errFake string

func (e errFake) Error() string { return string(e) }
