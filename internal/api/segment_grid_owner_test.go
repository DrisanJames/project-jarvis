package api

// Grid sole ownership (operator 2026-11-11): the Postgres builders leave
// grid-owned segments to the lake grid worker — only those, and only when the
// opt-in is on.

import (
	"context"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

const (
	ownerTestGridConds = `[{"field":"email_opened","operator":"in_last_days","value":"30"},{"field":"sending_domain","operator":"equals","value":"em.discountblog.com"}]`
	// A grid NAME whose conditions the lake build cannot express — the grid
	// worker refuses it, so Postgres must keep building it.
	ownerTestInexpressible = `[{"field":"email_opened","operator":"in_last_days","value":"30"},{"field":"sending_domain","operator":"equals","value":"em.discountblog.com"},{"field":"engagement_score","operator":"gt","value":"50"}]`
	ownerTestSegID         = "11111111-2222-3333-4444-555555555555"
)

// ---------- materializer ----------

func TestMaterializerLeavesToGrid(t *testing.T) {
	if !materializerLeavesToGrid("DB 30D Openers", ownerTestGridConds, true) {
		t.Fatal("sole owner on: a grid-owned segment must be skipped")
	}
	// Negative controls.
	if materializerLeavesToGrid("DB 30D Openers", ownerTestGridConds, false) {
		t.Fatal("sole owner off (default): nothing is skipped — old behaviour")
	}
	if materializerLeavesToGrid("Engaged Openers (30d)", ownerTestGridConds, true) {
		t.Fatal("a non-grid segment must still be materialized")
	}
	if materializerLeavesToGrid("DB 30D Openers", ownerTestInexpressible, true) {
		t.Fatal("a grid-named segment the grid cannot express must still be materialized")
	}
}

func TestSegmentMaterializerKillSwitch(t *testing.T) {
	// ON: the nightly pass returns before it touches the database.
	// (nil db: any database use would panic.)
	t.Setenv(segmentMaterializerDisabledEnv, "true")
	(&SegmentMaterializer{}).materializeAll(context.Background())

	// OFF (default): the pass proceeds exactly as before — takes the
	// single-writer lock and reads the segment queue.
	t.Setenv(segmentMaterializerDisabledEnv, "")
	db2, mock2, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db2.Close()
	mock2.ExpectQuery(`pg_try_advisory_lock`).WillReturnRows(sqlmock.NewRows([]string{"ok"}).AddRow(true))
	mock2.ExpectQuery(`(?s)FROM mailing_segments s.*ORDER BY s\.updated_at ASC`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "list_id", "name", "conditions"}))
	mock2.ExpectExec(`pg_advisory_unlock`).WillReturnResult(sqlmock.NewResult(0, 1))
	(&SegmentMaterializer{db: db2}).materializeAll(context.Background())
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatalf("kill switch off must run the nightly pass: %v", err)
	}
}

func TestSegmentMaterializerDisabledFlag(t *testing.T) {
	for v, want := range map[string]bool{"": false, "false": false, "0": false, "true": true, "TRUE": true, " true ": true} {
		t.Setenv(segmentMaterializerDisabledEnv, v)
		if got := segmentMaterializerDisabled(); got != want {
			t.Errorf("%s=%q → %v, want %v", segmentMaterializerDisabledEnv, v, got, want)
		}
	}
}

// A failed build must go to the back of the oldest-updated_at-first queue —
// and must not claim a fresh count or calculation time.
func TestRequeueFailedSegmentBumpsOnlyUpdatedAt(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectExec(`
		UPDATE mailing_segments SET updated_at = NOW() WHERE id = $1::uuid
	`).WithArgs(ownerTestSegID).WillReturnResult(sqlmock.NewResult(0, 1))
	(&SegmentMaterializer{db: db}).requeueFailedSegment(context.Background(), ownerTestSegID)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------- pre-dispatch ----------

func TestPredispatchSplitGridOwned(t *testing.T) {
	now := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
	fresh := 26 * time.Hour
	toBuild := []predispatchSegment{
		{ID: "grid-fresh", Name: "DB 30D Openers", Conditions: ownerTestGridConds, BuiltAt: nt(now.Add(-5 * time.Hour)), BuildState: "ok"},
		{ID: "grid-stale", Name: "DB 7D Openers", Conditions: ownerTestGridConds, BuiltAt: nt(now.Add(-30 * time.Hour)), BuildState: "ok", AttemptAt: nt(now.Add(-30 * time.Hour))},
		{ID: "grid-never", Name: "DB 14D Openers", Conditions: ownerTestGridConds},
		{ID: "grid-stale-just-tried", Name: "DB 60D Openers", Conditions: ownerTestGridConds, BuiltAt: nt(now.Add(-30 * time.Hour)), BuildState: "skipped_delta", AttemptAt: nt(now.Add(-10 * time.Minute))},
		{ID: "pg-plain", Name: "Engaged Openers (30d)", Conditions: `{"v2":{}}`, BuiltAt: nt(now.Add(-3 * time.Hour))},
		{ID: "pg-grid-name", Name: "DB 30D Clickers", Conditions: ownerTestInexpressible, BuiltAt: nt(now.Add(-3 * time.Hour))},
	}
	pg, gridStale := predispatchSplitGridOwned(toBuild, now, fresh)
	ids := func(segs []predispatchSegment) map[string]bool {
		m := map[string]bool{}
		for _, s := range segs {
			m[s.ID] = true
		}
		return m
	}
	gotPG, gotStale := ids(pg), ids(gridStale)

	// Negative control: ONLY grid-owned segments leave the Postgres path.
	if len(gotPG) != 2 || !gotPG["pg-plain"] || !gotPG["pg-grid-name"] {
		t.Fatalf("Postgres must still build exactly the non-grid-owned segments, got %v", gotPG)
	}
	if len(gotStale) != 2 || !gotStale["grid-stale"] || !gotStale["grid-never"] {
		t.Fatalf("grid refresh must be requested for exactly the stale/never-built grid segments, got %v", gotStale)
	}
	if gotPG["grid-fresh"] || gotStale["grid-fresh"] {
		t.Fatal("a grid build inside the freshness window is used as it is")
	}
	if gotStale["grid-stale-just-tried"] {
		t.Fatal("a grid segment attempted in the last hour must not be re-requested every tick")
	}
	// The freshness window is the knob: at 4h the 5h-old build is stale.
	if _, st := predispatchSplitGridOwned(toBuild[:1], now, 4*time.Hour); len(st) != 1 {
		t.Fatal("a build older than the window must be handed to the grid")
	}
}

func TestPredispatchGridFreshEnv(t *testing.T) {
	t.Setenv("PREDISPATCH_GRID_FRESH_HOURS", "")
	if got := predispatchGridFresh(); got != 26*time.Hour {
		t.Fatalf("default must be 26h, got %s", got)
	}
	t.Setenv("PREDISPATCH_GRID_FRESH_HOURS", "30")
	if got := predispatchGridFresh(); got != 30*time.Hour {
		t.Fatalf("env override ignored, got %s", got)
	}
	t.Setenv("PREDISPATCH_GRID_FRESH_HOURS", "junk")
	if got := predispatchGridFresh(); got != 26*time.Hour {
		t.Fatalf("invalid value must fall back to 26h, got %s", got)
	}
}

// Sole ownership is opt-in: with SEGMENT_GRID_SOLE_OWNER unset the refresher's
// gate is off and every segment keeps going through MaterializeSegment.
func TestPredispatchGridSoleOwnerDefaultOff(t *testing.T) {
	t.Setenv("SEGMENT_GRID_SOLE_OWNER", "")
	if (&PMTACampaignService{}).predispatchGridSoleOwner() {
		t.Fatal("default must be off")
	}
	on := &PMTACampaignService{predispatchGridSoleOwnerFn: func() bool { return true }}
	if !on.predispatchGridSoleOwner() {
		t.Fatal("seam ignored")
	}
}

// The refresh request goes through the SAME queue row shape the Refresh
// button writes, tagged source='predispatch', one open request per segment.
func TestPredispatchRequestGridRefreshUsesTheRefreshQueue(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	insert := regexp.QuoteMeta(`INSERT INTO mailing_segment_refresh_requests`) + `(?s).*` +
		regexp.QuoteMeta(`ON CONFLICT (segment_id) WHERE status IN ('queued','running') DO NOTHING`)
	mock.ExpectExec(insert).
		WithArgs(sqlmock.AnyArg(), ownerTestSegID, "org-1", "predispatch").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No org on the segment row → NULL, never '' into a UUID column.
	mock.ExpectExec(insert).
		WithArgs(sqlmock.AnyArg(), "seg-2", nil, "predispatch").
		WillReturnResult(sqlmock.NewResult(0, 0)) // already pending

	s := &PMTACampaignService{db: db}
	s.predispatchRequestGridRefresh(context.Background(), []predispatchSegment{
		{ID: ownerTestSegID, OrgID: "org-1", Name: "DB 7D Openers"},
		{ID: "seg-2", Name: "DB 14D Openers"},
	})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------- ledger truth ----------

// A failed/blocked attempt must not relabel the last successful build: the
// stored build_source survives unless this write is 'ok' (or there is no
// successful build yet), and the attempt's source rides in last_error.
func TestUpsertSegmentLedgerFailedAttemptKeepsLastGoodSource(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	pattern := `(?s)build_source\s+= CASE WHEN EXCLUDED\.last_build_status = 'ok'\s+` +
		`OR mailing_segment_build_ledger\.last_built_at IS NULL\s+` +
		`THEN EXCLUDED\.build_source\s+` +
		`ELSE mailing_segment_build_ledger\.build_source END.*` +
		`last_error\s+= CASE WHEN EXCLUDED\.last_error IS NOT NULL\s+` +
		regexp.QuoteMeta(`THEN '[' || EXCLUDED.build_source || '] ' || EXCLUDED.last_error END`)
	mock.ExpectExec(pattern).
		WithArgs(testLedgerSegID, int64(46484), "predispatch", "failed", 600000, 0.0, "canceling statement due to statement timeout").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := UpsertSegmentLedger(context.Background(), db, testLedgerSegID, 46484, "predispatch", "failed", 600000, 0, "canceling statement due to statement timeout"); err != nil {
		t.Fatalf("UpsertSegmentLedger: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkSegmentBuildRunningKeepsLastGoodSource(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectExec(`(?s)build_source\s+= CASE WHEN mailing_segment_build_ledger\.last_built_at IS NULL\s+`+
		`THEN EXCLUDED\.build_source\s+ELSE mailing_segment_build_ledger\.build_source END`).
		WithArgs(testLedgerSegID, "predispatch").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := MarkSegmentBuildRunning(context.Background(), db, testLedgerSegID, "predispatch"); err != nil {
		t.Fatalf("MarkSegmentBuildRunning: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
