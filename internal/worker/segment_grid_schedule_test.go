package worker

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

const gridTestConds = `[{"field":"email_opened","operator":"in_last_days","value":"30"},{"field":"sending_domain","operator":"equals","value":"em.discountblog.com"}]`

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// ---------------------------------------------------------------------------
// Ownership
// ---------------------------------------------------------------------------

func TestGridOwnsSegment(t *testing.T) {
	if !GridOwnsSegment("DB 30D Openers", gridTestConds) {
		t.Fatal("a grid-named segment with expressible conditions is grid-owned")
	}
	// Negative controls: everything else stays with the Postgres builders.
	notOwned := []struct{ why, name, conds string }{
		{"not a grid name", "Engaged Openers (30d)", gridTestConds},
		{"grid-like name outside the windows", "DB 90D Openers", gridTestConds},
		{"grid name, condition the lake cannot express", "DB 30D Openers",
			`[{"field":"email_opened","operator":"in_last_days","value":"30"},{"field":"sending_domain","operator":"equals","value":"em.discountblog.com"},{"field":"engagement_score","operator":"gt","value":"50"}]`},
		{"grid name, no conditions", "DB 30D Openers", `[]`},
	}
	for _, c := range notOwned {
		if GridOwnsSegment(c.name, c.conds) {
			t.Errorf("%s: must NOT be grid-owned", c.why)
		}
	}
}

func TestGridSoleOwnerEnabled(t *testing.T) {
	up := func() bool { return true }
	down := func() bool { return false }
	if gridSoleOwnerEnabled(envMap(nil), up) {
		t.Fatal("default (env unset) must be OFF — the Postgres builders keep building")
	}
	if gridSoleOwnerEnabled(envMap(map[string]string{segmentGridSoleOwnerEnv: "false"}), up) {
		t.Fatal("explicit false must be OFF")
	}
	on := map[string]string{segmentGridSoleOwnerEnv: "true"}
	if !gridSoleOwnerEnabled(envMap(on), up) {
		t.Fatal("SEGMENT_GRID_SOLE_OWNER=true with a working grid must be ON")
	}
	if gridSoleOwnerEnabled(envMap(on), down) {
		t.Fatal("lake reader down: nobody would build the grid — must be OFF")
	}
	killed := map[string]string{segmentGridSoleOwnerEnv: "true", "DISABLE_SEGMENT_GRID_WORKER": "true"}
	if gridSoleOwnerEnabled(envMap(killed), up) {
		t.Fatal("grid worker kill-switched: must be OFF")
	}
}

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

func TestLoadGridSchedule_DefaultIsLegacy(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset":               nil,
		"blank":               {segmentGridDailyAtEnv: "  "},
		"garbage":             {segmentGridDailyAtEnv: "7pm"},
		"out of range":        {segmentGridDailyAtEnv: "25:00"},
		"second without main": {segmentGridSecondAtEnv: "12:00"},
	} {
		if s := loadGridSchedule(envMap(env)); s.loc != nil {
			t.Errorf("%s: must keep the legacy 05:30Z schedule, got %s", name, s.describe())
		}
	}
	// The default target itself is pinned.
	if segmentGridTargetHourUTC != 5 || segmentGridTargetMinUTC != 30 {
		t.Fatalf("default daily target moved: %02d:%02dZ", segmentGridTargetHourUTC, segmentGridTargetMinUTC)
	}
}

func TestNewSegmentGridWorker_DefaultScheduleUnchanged(t *testing.T) {
	t.Setenv(segmentGridDailyAtEnv, "")
	t.Setenv(segmentGridSecondAtEnv, "")
	w := NewSegmentGridWorker(nil, nil)
	if w.sched.loc != nil {
		t.Fatal("env unset: the worker must run the legacy schedule")
	}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 12, h, m, 0, 0, time.UTC) }
	w.nowFn = func() time.Time { return at(5, 29) }
	if w.dailyDue() {
		t.Fatal("legacy: not due before 05:30Z")
	}
	w.nowFn = func() time.Time { return at(5, 30) }
	if !w.dailyDue() {
		t.Fatal("legacy: due at 05:30Z")
	}
}

func TestGridSchedule_DenverWallClockAcrossDST(t *testing.T) {
	s := loadGridSchedule(envMap(map[string]string{segmentGridDailyAtEnv: "19:00"}))
	if s.loc == nil {
		t.Fatal("19:00 must parse")
	}
	cases := []struct {
		why  string
		now  time.Time
		want time.Time
	}{
		// MST (UTC-7): 19:00 Denver = 02:00Z next day.
		{"winter, after target", time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC), time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)},
		{"winter, before target → yesterday's", time.Date(2026, 11, 12, 1, 59, 0, 0, time.UTC), time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)},
		// MDT (UTC-6): 19:00 Denver = 01:00Z next day.
		{"summer, after target", time.Date(2026, 7, 15, 1, 30, 0, 0, time.UTC), time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)},
		// Spring forward 2026-03-08: the 03-07 target is MST, the 03-08 target MDT.
		{"spring-forward day", time.Date(2026, 3, 9, 1, 0, 0, 0, time.UTC), time.Date(2026, 3, 9, 1, 0, 0, 0, time.UTC)},
		{"day before spring-forward", time.Date(2026, 3, 8, 2, 0, 0, 0, time.UTC), time.Date(2026, 3, 8, 2, 0, 0, 0, time.UTC)},
		// Fall back 2026-11-01: the 11-01 target is MST.
		{"fall-back day", time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := s.lastTarget(c.now); !got.Equal(c.want) {
			t.Errorf("%s: lastTarget(%s) = %s, want %s", c.why, c.now.Format(time.RFC3339), got.UTC().Format(time.RFC3339), c.want.Format(time.RFC3339))
		}
		if got := s.lastTarget(c.now).In(s.loc); got.Hour() != 19 || got.Minute() != 0 {
			t.Errorf("%s: target is %s Denver, want 19:00", c.why, got.Format("15:04"))
		}
	}
}

func TestDailyDue_EnvSchedule(t *testing.T) {
	w := &SegmentGridWorker{sched: loadGridSchedule(envMap(map[string]string{segmentGridDailyAtEnv: "19:00"}))}
	utc := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.UTC) }
	target := utc(12, 2, 0) // 19:00 MST on 11-11

	w.nowFn = func() time.Time { return utc(12, 2, 1) }
	if !w.dailyDue() {
		t.Fatal("must be due once 19:00 Denver has passed")
	}
	w.lastPassTarget = target // completed
	if w.dailyDue() {
		t.Fatal("must not re-run a completed pass")
	}
	w.nowFn = func() time.Time { return utc(12, 5, 31) } // the OLD 05:30Z target
	if w.dailyDue() {
		t.Fatal("the legacy 05:30Z target must not fire under an env schedule")
	}
	w.nowFn = func() time.Time { return utc(13, 2, 0) }
	if !w.dailyDue() {
		t.Fatal("must be due again at the next 19:00 Denver")
	}
	w.lastDailyAttempt = utc(13, 1, 50)
	if w.dailyDue() {
		t.Fatal("retry inside the cooldown must be suppressed")
	}
}

func TestGridSchedule_SecondPass(t *testing.T) {
	s := loadGridSchedule(envMap(map[string]string{segmentGridDailyAtEnv: "19:00", segmentGridSecondAtEnv: "12:30"}))
	if len(s.clocks) != 2 {
		t.Fatalf("want two passes, got %s", s.describe())
	}
	// 13:00 MST on 11-12 = 20:00Z: the latest target is the 12:30 pass.
	if got, want := s.lastTarget(time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)), time.Date(2026, 11, 12, 19, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("lastTarget = %s, want %s", got.UTC(), want)
	}
	// Negative control: no second env → one pass.
	if one := loadGridSchedule(envMap(map[string]string{segmentGridDailyAtEnv: "19:00"})); len(one.clocks) != 1 {
		t.Fatalf("second pass must be off by default, got %s", one.describe())
	}
}

// ---------------------------------------------------------------------------
// Ordering
// ---------------------------------------------------------------------------

func TestGridOrderByPriority(t *testing.T) {
	row := func(id string) gridSegmentRow { return gridSegmentRow{spec: gridSpec{ID: id}} }
	need := []gridSegmentRow{row("a"), row("b"), row("c"), row("d")}
	ids := func(rs []gridSegmentRow) string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.spec.ID
		}
		return strings.Join(out, "")
	}
	if got := ids(gridOrderByPriority(need, map[string]bool{"c": true, "d": true})); got != "cdab" {
		t.Fatalf("campaign segments must go first in stable order, got %s", got)
	}
	if got := ids(gridOrderByPriority(need, nil)); got != "abcd" {
		t.Fatalf("no campaign segments: order must be unchanged, got %s", got)
	}
}

func TestLoadCampaignSegmentIDs_Query(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`(?s)FROM mailing_campaigns c\s+JOIN mailing_campaign_audiences a.*a\.role = 'include'.*c\.status IN \('draft','scheduled'\).*c\.scheduled_at <= NOW\(\) \+`).
		WithArgs(int((36 * time.Hour).Seconds())).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id"}).AddRow("s1").AddRow("s2"))
	w := &SegmentGridWorker{db: db}
	got, err := w.loadCampaignSegmentIDs(context.Background())
	if err != nil || !got["s1"] || !got["s2"] || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Env schedule: a build from BEFORE this pass's scheduled instant is rebuilt
// (the legacy 20h skip would have suppressed it — negative control:
// TestRunDailyPass_FreshSegmentsAreNotRebuilt).
// ---------------------------------------------------------------------------

func TestRunDailyPass_EnvScheduleRebuildsBuildsOlderThanTheTarget(t *testing.T) {
	resetGridState()
	run := func(builtAgo time.Duration) (swapped bool, w *SegmentGridWorker) {
		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()
		mock.MatchExpectationsInOrder(false)
		now := time.Date(2026, 11, 12, 2, 5, 0, 0, time.UTC) // 19:05 MST, target 02:00Z
		segID := uuid.New().String()
		mock.ExpectQuery(`FROM mailing_segments`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "name", "conditions", "organization_id"}).
				AddRow(segID, "DB 30D Openers", gridTestConds, uuid.New().String()))
		mock.ExpectQuery(`FROM mailing_segment_build_ledger`).
			WillReturnRows(sqlmock.NewRows([]string{"segment_id", "count", "built", "status", "updated"}).
				AddRow(segID, 100, now.Add(-builtAgo), "ok", now.Add(-builtAgo)))
		for i := 0; i < 8; i++ { // ledger / heartbeat / run bookkeeping — best-effort writes
			mock.ExpectExec(`.*`).WillReturnResult(sqlmock.NewResult(0, 1))
		}

		w = NewSegmentGridWorker(db, nil)
		w.sched = loadGridSchedule(envMap(map[string]string{segmentGridDailyAtEnv: "19:00"}))
		w.nowFn = func() time.Time { return now }
		w.readerEnabledFn = func() bool { return true }
		w.deltaSupportedFn = func() bool { return false }
		w.priorityFn = nil
		w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
			return []analytics.GridPair{{SubscriberID: "11111111-1111-1111-1111-111111111111", BrandApex: "discountblog.com"}}, nil
		}
		w.resolveFn = func(ctx context.Context, ids []string, seeds []string) (map[string]gridSubscriber, error) {
			return map[string]gridSubscriber{ids[0]: {Email: "a@example.com"}}, nil
		}
		w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
			swapped = true
			return nil
		}
		w.runDailyPass(context.Background())
		return swapped, w
	}

	// Built 8h ago = by the previous pass (or the old 05:30Z one): rebuild.
	swapped, w := run(8 * time.Hour)
	if !swapped {
		t.Fatal("a build older than this pass's 19:00 target must be rebuilt")
	}
	if want := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC); !w.lastPassTarget.Equal(want) {
		t.Fatalf("completed pass must record its target, got %s", w.lastPassTarget)
	}
	// Built 3 minutes ago = already built in THIS pass (restart resume): skip.
	if swapped, _ := run(3 * time.Minute); swapped {
		t.Fatal("a segment built since the target must not be rebuilt (resume contract)")
	}
}

// ---------------------------------------------------------------------------
// Ledger truth: a non-ok attempt keeps the last OK build's source
// ---------------------------------------------------------------------------

func TestGridLedgerUpsert_FailedAttemptKeepsLastGoodSource(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	keepSource := `(?s)build_source\s+= CASE WHEN EXCLUDED\.last_build_status = 'ok'\s+OR mailing_segment_build_ledger\.last_built_at IS NULL\s+THEN EXCLUDED\.build_source\s+ELSE mailing_segment_build_ledger\.build_source END.*` +
		regexp.QuoteMeta(`'[' || EXCLUDED.build_source || '] ' || EXCLUDED.last_error`)
	mock.ExpectExec(keepSource).
		WithArgs("seg", int64(10), segmentGridLedgerSourceDelta, "error", 5, 0.0, "boom").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)'running'.*build_source\s+= CASE WHEN mailing_segment_build_ledger\.last_built_at IS NULL\s+THEN EXCLUDED\.build_source\s+ELSE mailing_segment_build_ledger\.build_source END`).
		WithArgs("seg", segmentGridLedgerSourceDelta).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := &SegmentGridWorker{db: db}
	w.ledgerUpsert(context.Background(), "seg", 10, segmentGridLedgerSourceDelta, "error", 5, 0, "boom")
	w.ledgerMarkRunning(context.Background(), "seg", segmentGridLedgerSourceDelta)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// The two hot readers
// ---------------------------------------------------------------------------

func TestHotReaderPredicatesAreIndexUsable(t *testing.T) {
	// idx_pcq_isp_family (dataset_id, isp_family, status) needs the bare column.
	if strings.Contains(pcqISPFamilyIsParam2SQL, "LOWER(") || strings.Contains(pcqISPFamilyIsParam2SQL, "COALESCE(") {
		t.Fatalf("isp_family must not be wrapped: %s", pcqISPFamilyIsParam2SQL)
	}
	if !strings.Contains(pcqISPFamilyIsParam2SQL, "isp_family = $2") ||
		!strings.Contains(pcqISPFamilyIsParam2SQL, "$2 = 'other' AND isp_family = ''") {
		t.Fatalf("must keep equality plus the empty-counts-as-other arm: %s", pcqISPFamilyIsParam2SQL)
	}
	// idx_subscribers_lower_email is on lower(TRIM(email)) — exact expression.
	if subscriberEmailMatchParam1SQL != `lower(TRIM(email)) = lower(TRIM($1))` {
		t.Fatalf("email lookup must use the indexed expression: %s", subscriberEmailMatchParam1SQL)
	}
}
