package api

// Lane Stats READ-THROUGH fixtures — the daily rollup
// (mailing_lane_stats_daily, written by worker.LaneStatsRollupWorker) is a pure
// ACCELERATOR in front of the live day scan. Permanent fixtures:
//
//   - HIT: a usable rollup day is served WITHOUT the live day query running at
//     all. Proven by declaring no `per_sub` expectation — if the live path ran,
//     sqlmock would answer the unexpected call with an error and the day would
//     land in missing_days, which the test rejects.
//   - MISS: a day the table does not cover falls back to the live path and
//     produces the same shape, in the same payload as rollup-served days.
//   - DEGRADATION: an EMPTY rollup table (or a read that fails outright, e.g.
//     the table does not exist yet) must behave exactly like the endpoint did
//     before the rollup existed — every day live, same numbers, never zeros.
//   - FRESHNESS IS ENFORCED ON READ: a closed day whose row was written BEFORE
//     the day closed is PARTIAL and must be refused; a today row older than the
//     endpoint's own today TTL must be refused. A wedged writer degrades to the
//     live path, it never changes the numbers.
//   - the '__none__' sentinel means "computed, no cells" — a resolved day with
//     zero rows, never a live re-scan and never a missing day. It must never
//     appear as an ISP in the payload.

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func laneStatsRollupCols() []string {
	return []string{"day", "isp", "sent", "delivered_pg", "openers", "clickers",
		"human_clickers", "open_events", "click_events", "computed_at"}
}

// laneStatsTestDays returns the Denver day strings the handler will ask for.
func laneStatsTestDays(n int) []string { return laneStatsDayList(time.Now(), n) }

// errRelationDoesNotExist is what a binary that shipped ahead of its DDL sees.
var errRelationDoesNotExist = errors.New(
	`pq: relation "mailing_lane_stats_daily" does not exist`)

// ── 1. ROLLUP HIT — the live day query never runs ───────────────────────────

func TestLaneStatsServesFromRollupWithoutLiveQuery(t *testing.T) {
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)
	mock.MatchExpectationsInOrder(false)

	days := laneStatsTestDays(2)
	now := time.Now()

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", now.Add(-240*time.Hour)))

	// Both days precomputed and fresh: yesterday computed after it closed,
	// today computed just now.
	mock.ExpectQuery(`FROM mailing_lane_stats_daily`).
		WillReturnRows(sqlmock.NewRows(laneStatsRollupCols()).
			AddRow(days[0], "gmail", 100, 90, 9, 1, 1, 10, 1, now).
			AddRow(days[1], "gmail", 200, 180, 18, 2, 2, 20, 2, now))

	// NO ExpectQuery(`per_sub`): the live day scan must not run.
	rec := getLaneStats(t, s, "vertical=rollup_hit_lane&brand=db&days=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// If the live path had run, the unexpected query would have errored and the
	// day would be a GAP. An empty missing_days is the proof it did not run.
	if resp.Partial || len(resp.MissingDays) != 0 {
		t.Fatalf("rollup-served days must not be partial/missing: %+v", resp)
	}
	if len(resp.RollupDays) != 2 || len(resp.LiveDays) != 0 {
		t.Fatalf("both days must be labelled rollup-served: rollup=%v live=%v",
			resp.RollupDays, resp.LiveDays)
	}
	if resp.RollupOldestComputedAt == "" {
		t.Fatal("a rollup-served payload must publish how stale the fast path is")
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("want 2 rows (1 ISP x 2 days), got %d: %+v", len(resp.Rows), resp.Rows)
	}
	if resp.Totals.Sent != 300 || resp.Totals.DeliveredPG != 270 ||
		resp.Totals.Openers != 27 || resp.Totals.Clickers != 3 ||
		resp.Totals.HumanClickers != 3 || resp.Totals.OpenEvents != 30 || resp.Totals.ClickEvents != 3 {
		t.Fatalf("rollup cells must map through unchanged: %+v", resp.Totals)
	}
	// The counting-rule labels are unchanged: a rollup row is the SAME number,
	// computed by a copy of the same SQL.
	if resp.Source != laneStatsSource || resp.DeliveredNote == "" {
		t.Fatal("rollup-served payload must keep the PG-source labelling")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ── 2. ROLLUP MISS — falls back to the live path, same shape ────────────────

func TestLaneStatsRollupMissFallsBackToLivePath(t *testing.T) {
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)
	mock.MatchExpectationsInOrder(false)

	days := laneStatsTestDays(2)
	now := time.Now()

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", now.Add(-240*time.Hour)))
	// Only the CLOSED day is precomputed; today is not in the table at all.
	mock.ExpectQuery(`FROM mailing_lane_stats_daily`).
		WillReturnRows(sqlmock.NewRows(laneStatsRollupCols()).
			AddRow(days[0], "gmail", 100, 90, 9, 1, 1, 10, 1, now))
	// Exactly ONE live day scan — for today.
	mock.ExpectQuery(`per_sub`).WillReturnRows(
		sqlmock.NewRows(laneStatsDayCols()).AddRow("gmail", 200, 180, 18, 2, 2, 20, 2))

	rec := getLaneStats(t, s, "vertical=rollup_miss_lane&days=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Partial || len(resp.MissingDays) != 0 {
		t.Fatalf("a mixed payload must still be complete: %+v", resp)
	}
	if len(resp.RollupDays) != 1 || resp.RollupDays[0] != days[0] {
		t.Fatalf("rollup_days must name the precomputed day: %v", resp.RollupDays)
	}
	if len(resp.LiveDays) != 1 || resp.LiveDays[0] != days[1] {
		t.Fatalf("live_days must name the day the table lacked: %v", resp.LiveDays)
	}
	// Provenance partitions the window exactly.
	if len(resp.RollupDays)+len(resp.LiveDays)+len(resp.MissingDays) != len(resp.DayList) {
		t.Fatalf("rollup+live+missing must partition day_list: %+v", resp)
	}
	if len(resp.Rows) != 2 || resp.Totals.Sent != 300 || resp.Totals.DeliveredPG != 270 {
		t.Fatalf("mixed-provenance totals wrong: rows=%d totals=%+v", len(resp.Rows), resp.Totals)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ── 3. DEGRADATION — an empty rollup table is exactly today's behaviour ──────

func TestLaneStatsEmptyRollupIsIdenticalToLivePath(t *testing.T) {
	run := func(t *testing.T, rollup func(sqlmock.Sqlmock)) laneStatsResponse {
		t.Helper()
		laneStatsResetCache()
		s, mock := newLedgerServiceWithMock(t)
		mock.MatchExpectationsInOrder(false)
		mock.ExpectQuery(`FROM mailing_campaigns`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
				AddRow("11111111-1111-1111-1111-111111111111", time.Now().Add(-240*time.Hour)).
				AddRow("22222222-2222-2222-2222-222222222222", time.Now().Add(-48*time.Hour)))
		rollup(mock)
		mock.ExpectQuery(`per_sub`).WillReturnRows(
			sqlmock.NewRows(laneStatsDayCols()).
				AddRow("gmail", 1234, 1100, 210, 18, 11, 260, 25).
				AddRow("microsoft", 400, 300, 60, 3, 1, 90, 4))
		mock.ExpectQuery(`per_sub`).WillReturnRows(
			sqlmock.NewRows(laneStatsDayCols()).
				AddRow("gmail", 500, 450, 40, 4, 3, 44, 5))
		rec := getLaneStats(t, s, "vertical=degrade_lane&days=2")
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		var resp laneStatsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// (a) the table exists but is EMPTY for this lane.
	empty := run(t, func(m sqlmock.Sqlmock) {
		m.ExpectQuery(`FROM mailing_lane_stats_daily`).
			WillReturnRows(sqlmock.NewRows(laneStatsRollupCols()))
	})
	// (b) the table does not exist at all / the read blows up. The worker may
	// never have been wired; the binary must not care.
	broken := run(t, func(m sqlmock.Sqlmock) {
		m.ExpectQuery(`FROM mailing_lane_stats_daily`).
			WillReturnError(errRelationDoesNotExist)
	})

	for name, resp := range map[string]laneStatsResponse{"empty": empty, "read-failed": broken} {
		if len(resp.RollupDays) != 0 {
			t.Fatalf("%s: nothing may be claimed as rollup-served: %v", name, resp.RollupDays)
		}
		if len(resp.LiveDays) != len(resp.DayList) {
			t.Fatalf("%s: every day must be live-served: %v", name, resp.LiveDays)
		}
		if resp.Partial || len(resp.MissingDays) != 0 {
			t.Fatalf("%s: a missing rollup is not a missing day: %+v", name, resp)
		}
		// The numbers are the LIVE numbers, untouched.
		if resp.Totals.Sent != 2134 || resp.Totals.DeliveredPG != 1850 ||
			resp.Totals.Openers != 310 || resp.Totals.Clickers != 25 ||
			resp.Totals.HumanClickers != 15 || resp.Totals.OpenEvents != 394 ||
			resp.Totals.ClickEvents != 34 {
			t.Fatalf("%s: degraded path must produce the LIVE numbers: %+v", name, resp.Totals)
		}
		if resp.Campaigns != 2 || resp.Source != laneStatsSource {
			t.Fatalf("%s: payload must be otherwise unchanged: %+v", name, resp)
		}
		if len(resp.Rows) != 4 { // 2 ISPs x 2 days, dense grid
			t.Fatalf("%s: dense grid must be unchanged, got %d rows", name, len(resp.Rows))
		}
	}
}

// ── 4. FRESHNESS ENFORCED ON READ ───────────────────────────────────────────

func TestLaneStatsRollupRejectsPartialAndStaleRows(t *testing.T) {
	days := laneStatsTestDays(2)
	now := time.Now()
	_, yesterdayEnd := laneStatsDayBoundsUTC(
		time.Now().In(propertyLedgerLoc).AddDate(0, 0, -1))

	cases := []struct {
		name        string
		day         string
		computedAt  time.Time
		wantUsable  bool
		explanation string
	}{
		{"closed day computed after it closed", days[0], yesterdayEnd.Add(time.Minute), true,
			"a complete closed day never changes again"},
		{"closed day computed BEFORE it closed", days[0], yesterdayEnd.Add(-time.Minute), false,
			"that row is a PARTIAL day — serving it would under-report"},
		{"today computed just now", days[1], now, true, ""},
		{"today computed longer ago than the today TTL", days[1],
			now.Add(-2 * laneStatsTodayTTL), false,
			"a wedged writer must degrade to the live path, not freeze today"},
	}
	for _, c := range cases {
		isToday := c.day == days[1]
		dayEnd := yesterdayEnd
		if isToday {
			_, dayEnd = laneStatsDayBoundsUTC(time.Now())
		}
		got := laneStatsRollupUsable(
			laneStatsRollupDay{computedAt: c.computedAt}, isToday, dayEnd, now)
		if got != c.wantUsable {
			t.Fatalf("%s: usable=%v want %v (%s)", c.name, got, c.wantUsable, c.explanation)
		}
	}
	// A day with no row at all is never usable.
	if laneStatsRollupUsable(laneStatsRollupDay{}, false, yesterdayEnd, now) {
		t.Fatal("a zero-value rollup day must never be usable")
	}
}

// A stale closed-day row must put the day back on the live path — proven by
// declaring the live expectation and requiring it to be consumed.
func TestLaneStatsPartialClosedDayRowFallsBackLive(t *testing.T) {
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)
	mock.MatchExpectationsInOrder(false)

	days := laneStatsTestDays(2)
	_, yesterdayEnd := laneStatsDayBoundsUTC(time.Now().In(propertyLedgerLoc).AddDate(0, 0, -1))

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", time.Now().Add(-240*time.Hour)))
	mock.ExpectQuery(`FROM mailing_lane_stats_daily`).
		WillReturnRows(sqlmock.NewRows(laneStatsRollupCols()).
			// written an hour BEFORE the day closed → partial → refuse
			AddRow(days[0], "gmail", 5, 5, 1, 0, 0, 1, 0, yesterdayEnd.Add(-time.Hour)).
			AddRow(days[1], "gmail", 200, 180, 18, 2, 2, 20, 2, time.Now()))
	mock.ExpectQuery(`per_sub`).WillReturnRows(
		sqlmock.NewRows(laneStatsDayCols()).AddRow("gmail", 100, 90, 9, 1, 1, 10, 1))

	rec := getLaneStats(t, s, "vertical=partial_row_lane&days=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.LiveDays) != 1 || resp.LiveDays[0] != days[0] {
		t.Fatalf("the partial closed day must be recomputed live: %v", resp.LiveDays)
	}
	if resp.Totals.Sent != 300 {
		t.Fatalf("the LIVE value (100) must win over the partial row (5): %+v", resp.Totals)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the live scan for the partial day must have run: %v", err)
	}
}

// ── 5. the '__none__' sentinel is a resolved-empty day, not a cell ──────────

func TestLaneStatsRollupSentinelIsResolvedEmptyDay(t *testing.T) {
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)
	mock.MatchExpectationsInOrder(false)

	days := laneStatsTestDays(2)
	now := time.Now()

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", now.Add(-240*time.Hour)))
	mock.ExpectQuery(`FROM mailing_lane_stats_daily`).
		WillReturnRows(sqlmock.NewRows(laneStatsRollupCols()).
			AddRow(days[0], laneStatsRollupEmptyISP, 0, 0, 0, 0, 0, 0, 0, now).
			AddRow(days[1], "gmail", 200, 180, 18, 2, 2, 20, 2, now))

	// No live expectation: the sentinel day is RESOLVED, so nothing re-scans it.
	rec := getLaneStats(t, s, "vertical=sentinel_lane&days=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.RollupDays) != 2 || len(resp.LiveDays) != 0 || len(resp.MissingDays) != 0 {
		t.Fatalf("a sentinel day is resolved-empty, not missing and not live: %+v", resp)
	}
	for _, r := range resp.Rows {
		if r.ISP == laneStatsRollupEmptyISP {
			t.Fatalf("the sentinel must never surface as an ISP: %+v", r)
		}
	}
	// Dense grid: gmail exists on both days, zero-filled on the empty one.
	if len(resp.Rows) != 2 {
		t.Fatalf("want 1 ISP x 2 days = 2 rows, got %d: %+v", len(resp.Rows), resp.Rows)
	}
	if resp.Totals.Sent != 200 {
		t.Fatalf("the empty day contributes nothing: %+v", resp.Totals)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ── 6. read SQL shape ───────────────────────────────────────────────────────

func TestLaneStatsRollupReadIsOrgScopedAndSargable(t *testing.T) {
	if !contains(laneStatsRollupReadSQL, "organization_id = $1::uuid") {
		t.Fatal("the rollup read must be org-scoped — organization_id leads the PK")
	}
	if !contains(laneStatsRollupReadSQL, "day >= $4::date") ||
		!contains(laneStatsRollupReadSQL, "day <= $5::date") {
		t.Fatal("the rollup read must carry a param-injected day range")
	}
	if contains(laneStatsRollupReadSQL, "AT TIME ZONE") {
		t.Fatal("the rollup read must not tz-cast (non-sargable; Denver days are Go-computed)")
	}
	for _, bad := range []string{"DELETE", "UPDATE", "INSERT", "TRUNCATE", "DROP"} {
		if contains(laneStatsRollupReadSQL, bad) {
			t.Fatalf("the endpoint's rollup access is READ-ONLY; found %q", bad)
		}
	}
}
