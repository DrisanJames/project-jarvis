package api

// Lane Stats fixtures — the Go port of agents/reporting/lane_performance_ledger
// + drip_lane_isp_report.all_lanes_isp_day. Permanent fixtures:
//   - REGRESSION GUARD: the day SQL uses PAST-TENSE event types ('opened' /
//     'clicked' — 'open'/'click' are a silent zero) and always carries an
//     event_at range bound (mailing_tracking_events is RANGE-partitioned), and
//     never tz-casts a column in a predicate (non-sargable).
//   - rate math: a zero denominator is 0.0, never NaN/+Inf and never a panic.
//   - days clamping: 0 -> 1, 999 -> 30, absent/garbage -> 7.
//   - negative: missing/invalid vertical -> 400 before any DB access.
//   - happy path: cells map through to rows + totals, dense grid across days,
//     and a day whose query fails is a GAP in missing_days, never zeros.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// laneStatsResetCache drops every cached day — the cache is package-level, so
// tests must not inherit each other's slots.
func laneStatsResetCache() {
	laneStatsCache.Range(func(k, _ interface{}) bool {
		laneStatsCache.Delete(k)
		return true
	})
	laneStatsSweepMu.Lock()
	laneStatsSweptAt = time.Time{}
	laneStatsSweepMu.Unlock()
}

func getLaneStats(t *testing.T, s *PMTACampaignService, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/property-ledger/lane-stats?"+query, nil)
	rec := httptest.NewRecorder()
	s.HandleLaneStats(rec, req)
	return rec
}

func laneStatsDayCols() []string {
	return []string{"isp", "sent", "delivered_pg", "openers", "clickers",
		"human_clickers", "open_events", "click_events"}
}

// ── 5. REGRESSION GUARD — the two silent-zero footguns ──────────────────────

func TestLaneStatsSQLPinsPastTenseAndEventAtBound(t *testing.T) {
	// PAST TENSE. mailing_tracking_events stores 'opened'/'clicked';
	// 'open'/'click' match nothing and return a silent zero.
	for _, frag := range []string{"'opened'", "'clicked'", "'sent'", "'delivered'"} {
		if !strings.Contains(laneStatsDaySQL, frag) {
			t.Fatalf("day SQL must use the past-tense event type %s", frag)
		}
	}
	for _, bad := range []string{"event_type = 'open'", "event_type='open'",
		"event_type = 'click'", "event_type='click'"} {
		if strings.Contains(laneStatsDaySQL, bad) {
			t.Fatalf("day SQL uses present-tense %q — that is a SILENT ZERO", bad)
		}
	}
	// event_at bound: mailing_tracking_events is RANGE-partitioned on event_at,
	// so a query without a bound scans every partition.
	if !strings.Contains(laneStatsDaySQL, "m.event_at >= $2 AND m.event_at < $3") {
		t.Fatal("day SQL must carry a param-injected event_at range bound ($2/$3)")
	}
	// Non-sargable predicates: never tz-cast a column in a WHERE clause. The
	// Denver bounds are Go-computed and param-injected instead.
	for name, q := range map[string]string{"day": laneStatsDaySQL, "campaign": laneStatsCampaignSQL} {
		if strings.Contains(q, "AT TIME ZONE") {
			t.Fatalf("%s SQL must not tz-cast (non-sargable; Denver bounds are Go-computed)", name)
		}
	}
	// created_at LEADS the campaign resolve; the name regex is residual only.
	if !strings.Contains(laneStatsCampaignSQL, "created_at >= $2") ||
		!strings.Contains(laneStatsCampaignSQL, "organization_id = $1::uuid") {
		t.Fatal("campaign SQL must be org-scoped and lead with a created_at bound")
	}
	// The unnest JOIN (not `= ANY`) is what keeps the planner on per-campaign
	// index lookups against idx_tracking_campaign.
	if !strings.Contains(laneStatsDaySQL, "unnest($1::uuid[]) AS cid") ||
		!strings.Contains(laneStatsDaySQL, "JOIN mailing_tracking_events m ON m.campaign_id = c.cid") {
		t.Fatal("day SQL must keep the unnest JOIN shape (all_lanes_isp_day)")
	}
	// Backfilled placeholder bounce rows stay excluded (partner_lane_report._artifact_pred).
	if !strings.Contains(laneStatsDaySQL, "(undefined status)") {
		t.Fatal("day SQL must keep the backfill-artifact exclusion")
	}
}

// ── 2. rate math ────────────────────────────────────────────────────────────

func TestLaneStatsRateZeroDenominator(t *testing.T) {
	for _, c := range []struct {
		num, den int64
		want     float64
	}{
		{0, 0, 0}, {5, 0, 0}, {-1, 0, 0}, {7, -3, 0},
		{1, 4, 0.25}, {210, 1100, 210.0 / 1100.0},
	} {
		got := laneStatsRate(c.num, c.den)
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("laneStatsRate(%d,%d) = %v — must never be NaN/Inf", c.num, c.den, got)
		}
		if got != c.want {
			t.Fatalf("laneStatsRate(%d,%d) = %v, want %v", c.num, c.den, got, c.want)
		}
	}
}

func TestLaneStatsZeroDeliveredRowsSerializeAsZeroRates(t *testing.T) {
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)
	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", time.Now().Add(-72*time.Hour)))
	// A lane that sent but has zero delivery confirmations yet: rates must be
	// 0.0 in the JSON, never NaN (which does not even encode to valid JSON).
	mock.ExpectQuery(`per_sub`).WillReturnRows(
		sqlmock.NewRows(laneStatsDayCols()).AddRow("microsoft", 900, 0, 0, 0, 0, 0, 0))

	rec := getLaneStats(t, s, "vertical=zero_denominator_lane&days=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "NaN") || strings.Contains(rec.Body.String(), "Inf") {
		t.Fatalf("payload carries NaN/Inf: %s", rec.Body.String())
	}
	var resp laneStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v — body %s", err, rec.Body.String())
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(resp.Rows))
	}
	if resp.Rows[0].OpenRate != 0 || resp.Rows[0].ClickRate != 0 {
		t.Fatalf("zero delivered must give 0.0 rates, got %+v", resp.Rows[0])
	}
	if resp.Totals.OpenRate != 0 || resp.Totals.ClickRate != 0 {
		t.Fatalf("totals rates must be 0.0, got %+v", resp.Totals)
	}
}

// ── 3. NEGATIVE — missing vertical ──────────────────────────────────────────

func TestLaneStatsMissingVertical400(t *testing.T) {
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)

	// No vertical at all, and a blank one: both rejected BEFORE any DB access,
	// which ExpectationsWereMet proves (no expectations were registered).
	for _, q := range []string{"", "days=7", "vertical=", "vertical=%20", "brand=db&days=3"} {
		if rec := getLaneStats(t, s, q); rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: got %d, want 400 (%s)", q, rec.Code, rec.Body.String())
		}
	}
	// Regex-injection / bad-shape verticals are rejected too, never escaped
	// into the Postgres regex.
	for _, q := range []string{"vertical=.%2A", "vertical=a%20b", "vertical=DROP%3B--", "vertical=_x"} {
		if rec := getLaneStats(t, s, q); rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: got %d, want 400", q, rec.Code)
		}
	}
	// A valid vertical with a bad brand is a 400 as well.
	if rec := getLaneStats(t, s, "vertical=internal_auto_insurance&brand=.%2A"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad brand: got %d, want 400", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("validation must reject before any DB access: %v", err)
	}
}

// ── 4. days clamping ────────────────────────────────────────────────────────

func TestLaneStatsDaysClamping(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int
	}{
		{"", 7}, {"nonsense", 7}, {"0", 1}, {"-5", 1}, {"1", 1},
		{"7", 7}, {"30", 30}, {"31", 30}, {"999", 30},
	} {
		if got := laneStatsClampDays(c.raw); got != c.want {
			t.Fatalf("laneStatsClampDays(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
	// And the clamp is what the payload reports, end to end.
	for _, c := range []struct {
		lane string
		q    string
		want int
	}{{"clamp_low", "days=0", 1}, {"clamp_high", "days=999", 30}} {
		laneStatsResetCache()
		s, mock := newLedgerServiceWithMock(t)
		mock.MatchExpectationsInOrder(false)
		mock.ExpectQuery(`FROM mailing_campaigns`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}))
		rec := getLaneStats(t, s, "vertical="+c.lane+"&"+c.q)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d (%s)", c.q, rec.Code, rec.Body.String())
		}
		var resp laneStatsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Days != c.want || len(resp.DayList) != c.want {
			t.Fatalf("%s: days=%d day_list=%d, want %d", c.q, resp.Days, len(resp.DayList), c.want)
		}
	}
}

// ── 1. happy path ───────────────────────────────────────────────────────────

func TestLaneStatsHappyPathRowsAndTotals(t *testing.T) {
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)
	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", time.Now().Add(-96*time.Hour)).
			AddRow("22222222-2222-2222-2222-222222222222", time.Now().Add(-48*time.Hour)))
	mock.ExpectQuery(`per_sub`).WillReturnRows(
		sqlmock.NewRows(laneStatsDayCols()).
			AddRow("gmail", 1234, 1100, 210, 18, 11, 260, 25).
			AddRow("microsoft", 400, 300, 60, 3, 1, 90, 4))

	rec := getLaneStats(t, s, "vertical=happy_lane&brand=db&days=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp laneStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v — body %s", err, rec.Body.String())
	}
	if resp.Vertical != "happy_lane" || resp.Brand != "db" || resp.Days != 1 {
		t.Fatalf("scope echo wrong: %+v", resp)
	}
	if resp.Campaigns != 2 {
		t.Fatalf("campaigns = %d, want 2", resp.Campaigns)
	}
	if resp.Partial || len(resp.MissingDays) != 0 {
		t.Fatalf("no day failed, so partial must be false: %+v", resp.MissingDays)
	}
	if resp.Source != laneStatsSource || resp.DeliveredNote == "" {
		t.Fatal("payload must label the PG source so delivered_pg is never read as lake truth")
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("want 2 rows (2 ISPs x 1 day), got %d", len(resp.Rows))
	}
	// Busiest ISP first.
	if resp.Rows[0].ISP != "gmail" || resp.Rows[1].ISP != "microsoft" {
		t.Fatalf("rows must be ordered by window volume desc: %+v", resp.Rows)
	}
	g := resp.Rows[0]
	if g.Day != resp.DayList[0] {
		t.Fatalf("row day %q must be the requested Denver day %q", g.Day, resp.DayList[0])
	}
	if g.Sent != 1234 || g.DeliveredPG != 1100 || g.Openers != 210 || g.Clickers != 18 ||
		g.HumanClickers != 11 || g.OpenEvents != 260 || g.ClickEvents != 25 {
		t.Fatalf("gmail cell mismatch: %+v", g)
	}
	// Rates are UNIQUE subscribers / delivered_pg (lane_performance_ledger),
	// NOT raw events / delivered.
	if math.Abs(g.OpenRate-210.0/1100.0) > 1e-12 || math.Abs(g.ClickRate-18.0/1100.0) > 1e-12 {
		t.Fatalf("rates must be uniques/delivered_pg: %+v", g)
	}
	tot := resp.Totals
	if tot.Sent != 1634 || tot.DeliveredPG != 1400 || tot.Openers != 270 || tot.Clickers != 21 ||
		tot.HumanClickers != 12 || tot.OpenEvents != 350 || tot.ClickEvents != 29 {
		t.Fatalf("totals mismatch: %+v", tot)
	}
	if math.Abs(tot.OpenRate-270.0/1400.0) > 1e-12 || math.Abs(tot.ClickRate-21.0/1400.0) > 1e-12 {
		t.Fatalf("totals rates mismatch: %+v", tot)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLaneStatsDenseGridAcrossDays(t *testing.T) {
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)
	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", time.Now().Add(-240*time.Hour)))
	// Day A knows two ISPs, day B only one — the grid must still carry a cell
	// for every ISP on every resolved day so the UI can diff consecutive days.
	mock.ExpectQuery(`per_sub`).WillReturnRows(
		sqlmock.NewRows(laneStatsDayCols()).
			AddRow("gmail", 100, 90, 9, 1, 1, 10, 1).
			AddRow("yahoo", 50, 40, 4, 0, 0, 4, 0))
	mock.ExpectQuery(`per_sub`).WillReturnRows(
		sqlmock.NewRows(laneStatsDayCols()).
			AddRow("gmail", 200, 180, 18, 2, 2, 20, 2))

	rec := getLaneStats(t, s, "vertical=grid_lane&days=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Rows) != 4 { // 2 ISPs x 2 days
		t.Fatalf("dense grid must be 2 ISPs x 2 days = 4 rows, got %d: %+v", len(resp.Rows), resp.Rows)
	}
	seen := map[string]bool{}
	for _, r := range resp.Rows {
		seen[r.Day+"|"+r.ISP] = true
	}
	for _, isp := range []string{"gmail", "yahoo"} {
		for _, d := range resp.DayList {
			if !seen[d+"|"+isp] {
				t.Fatalf("missing grid cell %s|%s", d, isp)
			}
		}
	}
	if resp.Totals.Sent != 350 || resp.Totals.DeliveredPG != 310 {
		t.Fatalf("totals mismatch: %+v", resp.Totals)
	}
}

func TestLaneStatsFailedDayIsAGapNotZeros(t *testing.T) {
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)
	mock.MatchExpectationsInOrder(false)

	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", time.Now().Add(-240*time.Hour)))
	mock.ExpectQuery(`per_sub`).WillReturnRows(
		sqlmock.NewRows(laneStatsDayCols()).AddRow("gmail", 100, 90, 9, 1, 1, 10, 1))
	mock.ExpectQuery(`per_sub`).WillReturnError(fmt.Errorf("canceling statement due to statement timeout"))

	rec := getLaneStats(t, s, "vertical=gap_lane&days=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("a lost day must still serve the resolved one: got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp laneStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Partial || len(resp.MissingDays) != 1 {
		t.Fatalf("failed day must be named in missing_days + partial=true: %+v", resp)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("a lost day must be a GAP, never a zero row: got %d rows %+v", len(resp.Rows), resp.Rows)
	}
	if resp.Rows[0].Day == resp.MissingDays[0] {
		t.Fatal("the missing day must not appear in rows")
	}
	if resp.Totals.Sent != 100 {
		t.Fatalf("totals must cover only resolved days: %+v", resp.Totals)
	}
}

func TestLaneStatsCampaignResolutionErrorFails(t *testing.T) {
	// Negative control: a failed resolve must 500 — never a silent empty-200
	// that reads as "this lane sent nothing".
	laneStatsResetCache()
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM mailing_campaigns`).WillReturnError(fmt.Errorf("boom"))
	rec := getLaneStats(t, s, "vertical=err_lane&days=1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rec.Code)
	}
}

func TestLaneStatsDayBoundsDSTCorrect(t *testing.T) {
	for _, c := range []struct {
		name, at, start, end string
		hours                float64
	}{
		{"ordinary", "2026-08-17T18:00:00Z", "2026-08-17T06:00:00Z", "2026-08-18T06:00:00Z", 24},
		{"spring-forward", "2026-03-08T20:00:00Z", "2026-03-08T07:00:00Z", "2026-03-09T06:00:00Z", 23},
		{"fall-back", "2026-11-01T20:00:00Z", "2026-11-01T06:00:00Z", "2026-11-02T07:00:00Z", 25},
	} {
		at, _ := time.Parse(time.RFC3339, c.at)
		start, end := laneStatsDayBoundsUTC(at)
		if start.Format(time.RFC3339) != c.start || end.Format(time.RFC3339) != c.end {
			t.Fatalf("%s: got [%s,%s), want [%s,%s)", c.name,
				start.Format(time.RFC3339), end.Format(time.RFC3339), c.start, c.end)
		}
		if got := end.Sub(start).Hours(); got != c.hours {
			t.Fatalf("%s: %v h, want %v h", c.name, got, c.hours)
		}
	}
}

func TestLaneStatsNamePatternIsAnchoredAndExact(t *testing.T) {
	if got := laneStatsNamePattern("internal_auto_insurance", ""); got != `^\[partner-drip\] internal_auto_insurance ` {
		t.Fatalf("vertical pattern must be anchored with a trailing space, got %q", got)
	}
	if got := laneStatsNamePattern("internal_auto_insurance", "db"); got != `^\[partner-drip\] internal_auto_insurance db ` {
		t.Fatalf("brand pattern wrong, got %q", got)
	}
}
