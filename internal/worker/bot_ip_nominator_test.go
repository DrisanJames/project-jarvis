package worker

// Regression pins for BotIPNominator (bot_ip_nominator.go), the Go port of
// agents/jobs/bot_ip_nominate.py. Each test encodes a guarantee the operator
// relies on or a failure that was actually hit while the Python was validated:
//
//   1. a single-subscriber IP is NEVER nominated (home connection)
//   2. the 2-subscriber floor and the 80% rate
//   3. already-blocked-by-RANGE is skipped (containment, not exact cidr)
//   4. IPv6 nominee -> /128, never /32
//   5. the upsert never DOWNGRADES an operator-curated scanner row
//   6. BOT_NOMINATE_ENABLED unset -> no lock, no query, no write, one log line
//   7. distributed lock: two ticks in one window do not both write
//   8. the [BotNominate] logging contract, in order, with SUMMARY fields
//   9. errors carry the step name, are never swallowed, and a query error
//      never reaches the writer
//  10. the burst signature is LINK-AGNOSTIC: two DISTINCT link_urls from one
//      subscriber within 5s count; repeats of ONE link (duplicate logging —
//      a human) do not; 6s apart does not.
//
// Harness: sqlmock (regexp matcher, in-order) + miniredis, as the sibling
// worker tests. The nomination SQL itself is not executed by sqlmock, so the
// burst rule is pinned two ways: its text (always) and its behaviour against
// a real Postgres TEMP table (TestBotNominate_BurstSignature_RealPG, opt-in
// via BOT_NOMINATE_TEST_PG_DSN — it SKIPS loudly, it does not pass silently).
// The Athena side (analytics.Reader.runQuery) is only exercisable live; here
// the worker's handling of lake rows is driven through the injected reader.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

// Fixed clock: 2026-09-06 12:00 UTC = 06:00 America/Denver, outside the
// [03:10, 04:10) lake window, so a tick at this instant is the PG pass only.
var botTestNow = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

const (
	botTestTag      = "bot-nominate-20260906"
	botTestRedisKey = "lock:" + botNominateLockKey
)

// ── SQL shape pins (regexp matcher; sqlmock collapses whitespace) ───────────

var (
	// The nomination query: PAST-tense 'clicked', the 30-minute window, the
	// DISTINCT-link self-join, and the rule's WHERE — all in one statement.
	reBotNominateQuery = `(?s)FROM mailing_tracking_events.*event_type='clicked'.*INTERVAL '30 minutes'.*` +
		regexp.QuoteMeta("b.link_url <> a.link_url") + `.*` +
		regexp.QuoteMeta("WHERE subs >= 2 AND coclick::numeric/subs >= 0.8")
	// Containment: the lookup resolves through ignite_ip_class(), never an
	// equality on cidr.
	reBotLookup = regexp.QuoteMeta("ignite_ip_class(t.ip::inet)") + `.*` + regexp.QuoteMeta("unnest($1::text[])")
	// The write: scanner upsert whose DO UPDATE is guarded by class <> 'scanner'.
	reBotUpsert = `(?s)INSERT INTO ignite_ip_classification.*'scanner'.*ON CONFLICT \(cidr\) DO UPDATE.*` +
		regexp.QuoteMeta("WHERE ignite_ip_classification.class <> 'scanner'")
	reBotHeartbeat = `INSERT INTO mailing_worker_heartbeats`
	reBotSetLocal  = `SET LOCAL statement_timeout`
)

// ── harness ─────────────────────────────────────────────────────────────────

func newBotTestDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db, mock
}

func newBotTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return mr, rdb
}

func newBotTestNominator(t *testing.T, db *sql.DB, rdb *redis.Client) *BotIPNominator {
	t.Helper()
	w := NewBotIPNominator(db, rdb)
	w.SetNow(func() time.Time { return botTestNow })
	w.lakeAvailable = func() bool { return false } // lake is opt-in per test via SetLakeReader
	return w
}

func botNomRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"ip", "subs", "coclick", "clicks"})
}

func botLookupRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"ip", "ignite_ip_class"})
}

func botEvidence(ip string, subs, coclick, clicks int64, source string) string {
	return fmt.Sprintf(`{"ip":"%s","subs":%d,"coclick":%d,"clicks":%d,"rule":%q,"source":"%s"}`,
		ip, subs, coclick, clicks, botNominateRuleLabel, source)
}

func expectBotHeartbeat(mock sqlmock.Sqlmock) {
	mock.ExpectExec(reBotHeartbeat).WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectHealthyPGPass wires the canonical healthy pass:
//
//	A 203.0.113.7  subs=5 coclick=4 clicks=40 prior=none    -> written as /32
//	B 3.5.140.2    subs=2 coclick=2 clicks=6  prior=scanner -> skipped (inside an existing scanner range)
//	C 2001:db8::1  subs=2 coclick=2 clicks=3  prior=none    -> written as /128
//
// matched=3 new=2 written=2.
func expectHealthyPGPass(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows().
		AddRow("203.0.113.7", 5, 4, 40).
		AddRow("3.5.140.2", 2, 2, 6).
		AddRow("2001:db8::1", 2, 2, 3))
	mock.ExpectQuery(reBotLookup).
		WithArgs(pq.Array([]string{"203.0.113.7", "3.5.140.2", "2001:db8::1"})).
		WillReturnRows(botLookupRows().
			AddRow("203.0.113.7", nil).
			AddRow("3.5.140.2", "scanner").
			AddRow("2001:db8::1", nil))
	mock.ExpectBegin()
	mock.ExpectExec(reBotSetLocal).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(reBotUpsert).
		WithArgs("203.0.113.7/32", botTestTag, botEvidence("203.0.113.7", 5, 4, 40, "pg:30m")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(reBotUpsert).
		WithArgs("2001:db8::1/128", botTestTag, botEvidence("2001:db8::1", 2, 2, 3, "pg:30m")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectBotHeartbeat(mock)
}

// botLogLines returns the "[BotNominate]" lines captured by captureLogs.
func botLogLines(buf *bytes.Buffer) []string {
	var out []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "[BotNominate]") {
			out = append(out, l)
		}
	}
	return out
}

// assertBotLogOrder requires each wanted substring to appear on a
// [BotNominate] line strictly after the previous one.
func assertBotLogOrder(t *testing.T, lines []string, wants ...string) {
	t.Helper()
	pos := -1
	for _, want := range wants {
		found := -1
		for i := pos + 1; i < len(lines); i++ {
			if strings.Contains(lines[i], want) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("log contract: %q not found after line %d\nlines:\n%s", want, pos, strings.Join(lines, "\n"))
		}
		pos = found
	}
}

type fakeBotLakeReader struct {
	rows  []BotNominee
	err   error
	calls int
	days  int
}

func (f *fakeBotLakeReader) Nominate(_ context.Context, days int) ([]BotNominee, error) {
	f.calls++
	f.days = days
	return f.rows, f.err
}

// ── 1 + 2: the rule ─────────────────────────────────────────────────────────

func TestBotNominate_Rule_SubscriberFloorAndRate(t *testing.T) {
	cases := []struct {
		subs, coclick int64
		want          bool
		why           string
	}{
		{1, 1, false, "single subscriber at 100% is a home connection — never"},
		{1, 0, false, "single subscriber"},
		{0, 0, false, "no subscribers"},
		{2, 2, true, "2 subs / 2 burst = 100%"},
		{2, 1, false, "2 subs / 1 burst = 50%"},
		{5, 4, true, "5 subs / 4 burst = 80% (floor inclusive)"},
		{5, 3, false, "5 subs / 3 burst = 60%"},
		{10, 8, true, "10/8 = 80%"},
		{10, 7, false, "10/7 = 70%"},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, botNominateRule(c.subs, c.coclick), "subs=%d coclick=%d: %s", c.subs, c.coclick, c.why)
	}
}

// Defence in depth for pin 1: the SQL WHERE cannot return a subs=1 row, but
// if the source ever does, the Go re-filter must drop it BEFORE the
// containment lookup. Removing the subs floor (bot_ip_nominator.go
// botNominateRule) makes the row pass at 100%, issues an unexpected lookup,
// and this test fails. So does lowering botNominateMinSubs to 1 — the
// nomination query's rendered WHERE no longer matches reBotNominateQuery.
func TestBotNominate_SingleSubscriberIPNeverNominated(t *testing.T) {
	t.Run("sql_floor", func(t *testing.T) {
		sql := BuildBotNominatePGSQL(30)
		assert.Contains(t, sql, "WHERE subs >= 2 AND coclick::numeric/subs >= 0.8")
	})
	t.Run("go_refilter", func(t *testing.T) {
		buf := captureLogs(t)
		db, mock := newBotTestDB(t)
		mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows().
			AddRow("198.51.100.1", 1, 1, 8)) // one person, 100% burst, 8 rows
		expectBotHeartbeat(mock) // matched=0 -> straight to SUMMARY

		w := newBotTestNominator(t, db, nil)
		res := w.RunOnce(context.Background(), "pg")

		assert.Equal(t, "ok", res.Status, "err=%v step=%s", res.Err, res.Step)
		assert.Equal(t, 1, res.Rows)
		assert.Equal(t, 0, res.Matched)
		assert.Equal(t, 1, res.Dropped)
		assert.Equal(t, 0, res.New)
		assert.Equal(t, 0, res.Written)
		assert.Empty(t, res.Nominees)
		assert.NoError(t, mock.ExpectationsWereMet(), "no containment lookup, no transaction")
		lines := botLogLines(buf)
		assertBotLogOrder(t, lines, "dropped source=pg ip=198.51.100.1 subs=1 coclick=1", "SUMMARY source=pg window=30m matched=0 new=0 written=0")
	})
}

func TestBotNominate_ThresholdsEndToEnd(t *testing.T) {
	db, mock := newBotTestDB(t)
	mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows().
		AddRow("10.0.0.1", 2, 2, 4).  // 2/2 -> nominate
		AddRow("10.0.0.2", 2, 1, 3).  // 2/1 = 50% -> no
		AddRow("10.0.0.3", 5, 4, 20). // 5/4 = 80% -> nominate
		AddRow("10.0.0.4", 1, 1, 1))  // single subscriber -> never
	mock.ExpectQuery(reBotLookup).
		WithArgs(pq.Array([]string{"10.0.0.1", "10.0.0.3"})).
		WillReturnRows(botLookupRows().AddRow("10.0.0.1", nil).AddRow("10.0.0.3", "hosting"))
	mock.ExpectBegin()
	mock.ExpectExec(reBotSetLocal).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(reBotUpsert).
		WithArgs("10.0.0.1/32", botTestTag, botEvidence("10.0.0.1", 2, 2, 4, "pg:30m")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(reBotUpsert).
		WithArgs("10.0.0.3/32", botTestTag, botEvidence("10.0.0.3", 5, 4, 20, "pg:30m")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectBotHeartbeat(mock)

	w := newBotTestNominator(t, db, nil)
	res := w.RunOnce(context.Background(), "pg")

	require.Equal(t, "ok", res.Status, "err=%v step=%s", res.Err, res.Step)
	assert.Equal(t, 4, res.Rows)
	assert.Equal(t, 2, res.Matched)
	assert.Equal(t, 2, res.Dropped)
	assert.Equal(t, 2, res.New)
	assert.Equal(t, 2, res.Written)
	assert.Equal(t, botTestTag, res.Tag)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// ── 3: containment, not exact cidr ──────────────────────────────────────────

// An IP inside an existing scanner /9 resolves to 'scanner' through
// ignite_ip_class() (narrowest match over is_active rows) and must not be
// re-added as a /32. The lookup must therefore be the containment function,
// not an equality on cidr: an exact-cidr lookup returns NULL for the /32 and
// re-adds thousands of already-blocked addresses. reBotLookup pins the
// function call; the behavioural half pins that a 'scanner' answer is skipped.
func TestBotNominate_ContainmentSkipsRangeBlockedIP(t *testing.T) {
	t.Run("lookup_is_containment_not_exact", func(t *testing.T) {
		assert.Contains(t, botNominateLookupSQL, "ignite_ip_class(")
		assert.NotContains(t, strings.ToLower(botNominateLookupSQL), "cidr =")
		assert.NotContains(t, strings.ToLower(botNominateLookupSQL), "= any(")
	})
	t.Run("scanner_by_range_is_skipped", func(t *testing.T) {
		buf := captureLogs(t)
		db, mock := newBotTestDB(t)
		expectHealthyPGPass(mock) // B 3.5.140.2 answers 'scanner' -> no upsert for it

		w := newBotTestNominator(t, db, nil)
		res := w.RunOnce(context.Background(), "pg")

		require.Equal(t, "ok", res.Status, "err=%v step=%s", res.Err, res.Step)
		assert.Equal(t, 3, res.Matched)
		assert.Equal(t, 1, res.SkippedScanner)
		assert.Equal(t, 2, res.New)
		assert.Equal(t, 2, res.Written)
		assert.NoError(t, mock.ExpectationsWereMet(), "exactly two upserts: A and C, never B")
		assertBotLogOrder(t, botLogLines(buf), "nominee source=pg ip=3.5.140.2 subs=2 coclick=2 clicks=6 prior=scanner → skip")
	})
}

// ── 4: IPv6 -> /128 ─────────────────────────────────────────────────────────

func TestBotNominate_HostCIDR_V6Is128(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"203.0.113.7", "203.0.113.7/32", false},
		{" 203.0.113.7 ", "203.0.113.7/32", false},
		{"2001:db8::1", "2001:db8::1/128", false},
		{"::ffff:203.0.113.7", "::ffff:203.0.113.7/128", false}, // v4-mapped literal is version 6, as ipaddress.ip_address
		{"2001:db8::1/32", "", true},
		{"not-an-ip", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := botNominateHostCIDR(c.in)
		if c.wantErr {
			assert.Errorf(t, err, "input %q", c.in)
			continue
		}
		require.NoErrorf(t, err, "input %q", c.in)
		assert.Equal(t, c.want, got, "input %q", c.in)
	}
}

// End-to-end: the v6 nominee reaches the upsert as /128 (expectHealthyPGPass
// pins the arg), and an unparseable IP is counted invalid and never written.
func TestBotNominate_InvalidIPSkippedNeverWritten(t *testing.T) {
	buf := captureLogs(t)
	db, mock := newBotTestDB(t)
	mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows().
		AddRow("garbage", 3, 3, 9).
		AddRow("2001:db8::9", 2, 2, 2))
	mock.ExpectQuery(reBotLookup).
		WithArgs(pq.Array([]string{"garbage", "2001:db8::9"})).
		WillReturnRows(botLookupRows().AddRow("garbage", nil).AddRow("2001:db8::9", nil))
	mock.ExpectBegin()
	mock.ExpectExec(reBotSetLocal).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(reBotUpsert).
		WithArgs("2001:db8::9/128", botTestTag, botEvidence("2001:db8::9", 2, 2, 2, "pg:30m")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectBotHeartbeat(mock)

	w := newBotTestNominator(t, db, nil)
	res := w.RunOnce(context.Background(), "pg")

	require.Equal(t, "ok", res.Status, "err=%v step=%s", res.Err, res.Step)
	assert.Equal(t, 2, res.New)
	assert.Equal(t, 1, res.Invalid)
	assert.Equal(t, 1, res.Written)
	assert.NoError(t, mock.ExpectationsWereMet())
	assertBotLogOrder(t, botLogLines(buf), `ERROR step=upsert ip="garbage"`, "SUMMARY source=pg window=30m matched=2 new=2 written=1")
}

// ── 5: never downgrade ──────────────────────────────────────────────────────

// The DO UPDATE is guarded by WHERE ignite_ip_classification.class <> 'scanner':
// an operator-curated scanner row keeps its evidence_source/evidence/note.
// Text half: the guard is present on the upsert statement. Behavioural half:
// a conflict the guard rejects reports 0 rows affected, and the pass counts
// only rows the database actually changed (written=1 of new=2).
func TestBotNominate_UpsertNeverDowngradesCuratedScanner(t *testing.T) {
	t.Run("guard_text", func(t *testing.T) {
		idx := strings.Index(botNominateUpsertSQL, "ON CONFLICT (cidr) DO UPDATE")
		require.GreaterOrEqual(t, idx, 0)
		tail := botNominateUpsertSQL[idx:]
		assert.Contains(t, tail, "WHERE ignite_ip_classification.class <> 'scanner'")
		assert.Contains(t, tail, "evidence_source=EXCLUDED.evidence_source")
		assert.NotContains(t, botNominateUpsertSQL, "DO NOTHING")
	})
	t.Run("rows_changed_only", func(t *testing.T) {
		buf := captureLogs(t)
		db, mock := newBotTestDB(t)
		mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows().
			AddRow("192.0.2.1", 4, 4, 16).
			AddRow("192.0.2.2", 3, 3, 9))
		mock.ExpectQuery(reBotLookup).
			WithArgs(pq.Array([]string{"192.0.2.1", "192.0.2.2"})).
			WillReturnRows(botLookupRows().AddRow("192.0.2.1", "unresolved").AddRow("192.0.2.2", nil))
		mock.ExpectBegin()
		mock.ExpectExec(reBotSetLocal).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(reBotUpsert).
			WithArgs("192.0.2.1/32", botTestTag, botEvidence("192.0.2.1", 4, 4, 16, "pg:30m")).
			WillReturnResult(sqlmock.NewResult(0, 1)) // unresolved -> scanner: changed
		mock.ExpectExec(reBotUpsert).
			WithArgs("192.0.2.2/32", botTestTag, botEvidence("192.0.2.2", 3, 3, 9, "pg:30m")).
			WillReturnResult(sqlmock.NewResult(0, 0)) // guard rejected the conflict: untouched
		mock.ExpectCommit()
		expectBotHeartbeat(mock)

		w := newBotTestNominator(t, db, nil)
		res := w.RunOnce(context.Background(), "pg")

		require.Equal(t, "ok", res.Status, "err=%v step=%s", res.Err, res.Step)
		assert.Equal(t, 2, res.New)
		assert.Equal(t, 1, res.Written)
		assert.NoError(t, mock.ExpectationsWereMet())
		assertBotLogOrder(t, botLogLines(buf), "upsert source=pg rows_written=1 invalid_ip=0 tag="+botTestTag, "SUMMARY source=pg window=30m matched=2 new=2 written=1 tag="+botTestTag)
	})
}

// ── 6: kill switch ──────────────────────────────────────────────────────────

func TestBotNominate_KillSwitch(t *testing.T) {
	t.Run("default_off", func(t *testing.T) {
		t.Setenv("BOT_NOMINATE_ENABLED", "")
		assert.False(t, BotNominateEnabled())
	})
	for _, v := range []string{"", "0", "false", "off", "no", "garbage"} {
		t.Run("disabled_"+v, func(t *testing.T) {
			t.Setenv("BOT_NOMINATE_ENABLED", v)
			buf := captureLogs(t)
			mr, rdb := newBotTestRedis(t)
			db, mock := newBotTestDB(t)

			w := newBotTestNominator(t, db, rdb)
			got := w.tick(context.Background())

			assert.Equal(t, "disabled", got)
			assert.Empty(t, mr.Keys(), "no lock may be taken")
			assert.NoError(t, mock.ExpectationsWereMet())
			lines := botLogLines(buf)
			require.Len(t, lines, 1, "exactly one line, the disabled notice:\n%s", buf.String())
			assert.Contains(t, lines[0], "enabled=false")
			assert.Contains(t, lines[0], "no query issued")
			assert.NotContains(t, buf.String(), "ERROR", "a disabled tick must not touch the DB (an unexpected sqlmock call would log ERROR)")
		})
	}
	for _, v := range []string{"1", "true", "on", "yes", " TRUE "} {
		t.Run("enabled_"+strings.TrimSpace(v), func(t *testing.T) {
			t.Setenv("BOT_NOMINATE_ENABLED", v)
			_, rdb := newBotTestRedis(t)
			db, mock := newBotTestDB(t)
			mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows())
			expectBotHeartbeat(mock)

			w := newBotTestNominator(t, db, rdb)
			assert.Equal(t, "ran", w.tick(context.Background()))
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// ── 7: distributed lock ─────────────────────────────────────────────────────

func TestBotNominate_DistLock(t *testing.T) {
	t.Run("redis_held_elsewhere_skips_whole_tick", func(t *testing.T) {
		t.Setenv("BOT_NOMINATE_ENABLED", "1")
		buf := captureLogs(t)
		mr, rdb := newBotTestRedis(t)
		db, mock := newBotTestDB(t)

		// Another instance's tick is in flight: it holds the lock.
		other := distlock.NewRedisLock(rdb, botNominateLockKey, time.Minute)
		held, err := other.Acquire(context.Background())
		require.NoError(t, err)
		require.True(t, held)
		require.True(t, mr.Exists(botTestRedisKey))

		w := newBotTestNominator(t, db, rdb)
		assert.Equal(t, "lock-held", w.tick(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet(), "no query, no write while another tick holds the lock")
		assert.True(t, mr.Exists(botTestRedisKey), "the other instance's lock must not be released by us")
		assertBotLogOrder(t, botLogLines(buf), "lock=held-elsewhere")

		// The other tick finishes: the next tick here runs, and releases.
		require.NoError(t, other.Release(context.Background()))
		mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows())
		expectBotHeartbeat(mock)
		assert.Equal(t, "ran", w.tick(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet())
		assert.False(t, mr.Exists(botTestRedisKey), "lock released after the tick")
	})

	t.Run("pg_advisory_fallback_not_acquired", func(t *testing.T) {
		t.Setenv("BOT_NOMINATE_ENABLED", "1")
		db, mock := newBotTestDB(t)
		mock.ExpectQuery(`pg_try_advisory_lock`).
			WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))

		w := newBotTestNominator(t, db, nil)
		assert.Equal(t, "lock-held", w.tick(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet(), "no nomination query, no unlock")
	})

	t.Run("pg_advisory_fallback_acquired_runs_and_unlocks", func(t *testing.T) {
		t.Setenv("BOT_NOMINATE_ENABLED", "1")
		db, mock := newBotTestDB(t)
		mock.ExpectQuery(`pg_try_advisory_lock`).
			WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
		mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows())
		expectBotHeartbeat(mock)
		mock.ExpectExec(`pg_advisory_unlock`).WillReturnResult(sqlmock.NewResult(0, 0))

		w := newBotTestNominator(t, db, nil)
		assert.Equal(t, "ran", w.tick(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("lock_error_logged_no_query", func(t *testing.T) {
		t.Setenv("BOT_NOMINATE_ENABLED", "1")
		buf := captureLogs(t)
		mr, rdb := newBotTestRedis(t)
		mr.Close() // redis down
		db, mock := newBotTestDB(t)

		w := newBotTestNominator(t, db, rdb)
		assert.Equal(t, "lock-error", w.tick(context.Background()))
		assert.NoError(t, mock.ExpectationsWereMet())
		assertBotLogOrder(t, botLogLines(buf), "ERROR step=lock")
	})

	// Re-run safety: the same window seen twice writes once. On the second
	// pass every nominee already resolves to scanner (the first pass wrote
	// it), so the writer is never reached.
	t.Run("second_pass_same_window_writes_nothing", func(t *testing.T) {
		db, mock := newBotTestDB(t)
		expectHealthyPGPass(mock)
		mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows().
			AddRow("203.0.113.7", 5, 4, 40).
			AddRow("3.5.140.2", 2, 2, 6).
			AddRow("2001:db8::1", 2, 2, 3))
		mock.ExpectQuery(reBotLookup).
			WithArgs(pq.Array([]string{"203.0.113.7", "3.5.140.2", "2001:db8::1"})).
			WillReturnRows(botLookupRows().
				AddRow("203.0.113.7", "scanner").
				AddRow("3.5.140.2", "scanner").
				AddRow("2001:db8::1", "scanner"))
		expectBotHeartbeat(mock)

		w := newBotTestNominator(t, db, nil)
		first := w.RunOnce(context.Background(), "pg")
		second := w.RunOnce(context.Background(), "pg")

		require.Equal(t, "ok", first.Status)
		require.Equal(t, "ok", second.Status, "err=%v step=%s", second.Err, second.Step)
		assert.Equal(t, 2, first.Written)
		assert.Equal(t, 3, second.SkippedScanner)
		assert.Equal(t, 0, second.New)
		assert.Equal(t, 0, second.Written)
		assert.NoError(t, mock.ExpectationsWereMet(), "no BEGIN on the second pass")
	})
}

// ── 8: logging contract ─────────────────────────────────────────────────────

// A healthy tick emits, in order: tick start, query, candidates, each
// nominee (write or skip), containment/skipped, upsert, SUMMARY — every line
// prefixed "[BotNominate]". The SUMMARY carries source, window, matched, new,
// written, tag. A tick that writes without each step logged is a FAIL.
func TestBotNominate_LoggingContract(t *testing.T) {
	t.Setenv("BOT_NOMINATE_ENABLED", "1")
	buf := captureLogs(t)
	_, rdb := newBotTestRedis(t)
	db, mock := newBotTestDB(t)
	expectHealthyPGPass(mock)

	w := newBotTestNominator(t, db, rdb)
	require.Equal(t, "ran", w.tick(context.Background()))
	require.NoError(t, mock.ExpectationsWereMet())

	lines := botLogLines(buf)
	for _, l := range lines {
		assert.True(t, strings.HasPrefix(l, "[BotNominate] "), "prefix: %q", l)
	}
	assertBotLogOrder(t, lines,
		"tick start source=pg window=30m enabled=true lock=acquired",
		"query source=pg window=30m duration=",
		"candidates source=pg window=30m matched=3 dropped=0",
		"nominee source=pg ip=203.0.113.7 subs=5 coclick=4 clicks=40 prior=none → write",
		"nominee source=pg ip=3.5.140.2 subs=2 coclick=2 clicks=6 prior=scanner → skip",
		"nominee source=pg ip=2001:db8::1 subs=2 coclick=2 clicks=3 prior=none → write",
		"containment source=pg already_scanner=1 skipped, new=2",
		"upsert source=pg rows_written=2 invalid_ip=0 tag="+botTestTag,
		"SUMMARY",
	)
	assert.Regexp(t, `(?m)^\[BotNominate\] query source=pg window=30m duration=\S+ rows_returned=3$`, buf.String(), "query line carries the row count")
	// The upsert line carries the one-statement revert.
	assertBotLogOrder(t, lines, `revert="DELETE FROM ignite_ip_classification WHERE evidence_source='`+botTestTag+`'"`)

	var summary string
	for _, l := range lines {
		if strings.Contains(l, "SUMMARY") {
			summary = l
		}
	}
	require.NotEmpty(t, summary, "SUMMARY line missing:\n%s", strings.Join(lines, "\n"))
	assert.Regexp(t,
		`^\[BotNominate\] SUMMARY source=pg window=30m matched=3 new=2 written=2 tag=`+regexp.QuoteMeta(botTestTag)+`\b`,
		summary)
	assert.Equal(t, 1, strings.Count(buf.String(), "SUMMARY"), "exactly one SUMMARY per pass")
	assert.NotContains(t, buf.String(), "ERROR")
}

// ── 9: errors carry the step, are never swallowed, never reach the writer ──

func TestBotNominate_ErrorsLoggedWithStep(t *testing.T) {
	t.Run("query_error_no_write", func(t *testing.T) {
		buf := captureLogs(t)
		db, mock := newBotTestDB(t)
		mock.ExpectQuery(reBotNominateQuery).WillReturnError(fmt.Errorf("canceling statement due to statement timeout"))
		mock.ExpectExec(reBotHeartbeat).WithArgs(botNominateWorkerName, "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		w := newBotTestNominator(t, db, nil)
		res := w.RunOnce(context.Background(), "pg")

		assert.Equal(t, "error", res.Status)
		assert.Equal(t, "query", res.Step)
		require.Error(t, res.Err)
		assert.Contains(t, res.Err.Error(), "statement timeout")
		assert.Equal(t, 0, res.Written)
		assert.NoError(t, mock.ExpectationsWereMet(), "no lookup, no BEGIN after a query error")
		assertBotLogOrder(t, botLogLines(buf), "ERROR step=query source=pg window=30m: canceling statement due to statement timeout")
		assert.NotContains(t, buf.String(), "SUMMARY", "a failed pass has no SUMMARY")
	})

	t.Run("containment_error_no_write", func(t *testing.T) {
		buf := captureLogs(t)
		db, mock := newBotTestDB(t)
		mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows().AddRow("203.0.113.7", 5, 4, 40))
		mock.ExpectQuery(reBotLookup).WillReturnError(fmt.Errorf("function ignite_ip_class(inet) does not exist"))
		mock.ExpectExec(reBotHeartbeat).WithArgs(botNominateWorkerName, "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		w := newBotTestNominator(t, db, nil)
		res := w.RunOnce(context.Background(), "pg")

		assert.Equal(t, "error", res.Status)
		assert.Equal(t, "containment", res.Step)
		assert.Equal(t, 0, res.Written)
		assert.NoError(t, mock.ExpectationsWereMet(), "no BEGIN after a containment error")
		assertBotLogOrder(t, botLogLines(buf), "ERROR step=containment source=pg window=30m: function ignite_ip_class(inet) does not exist")
	})

	t.Run("upsert_error_rolls_back", func(t *testing.T) {
		buf := captureLogs(t)
		db, mock := newBotTestDB(t)
		mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows().
			AddRow("203.0.113.7", 5, 4, 40).
			AddRow("203.0.113.8", 2, 2, 2))
		mock.ExpectQuery(reBotLookup).
			WillReturnRows(botLookupRows().AddRow("203.0.113.7", nil).AddRow("203.0.113.8", nil))
		mock.ExpectBegin()
		mock.ExpectExec(reBotSetLocal).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(reBotUpsert).WithArgs("203.0.113.7/32", botTestTag, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(reBotUpsert).WithArgs("203.0.113.8/32", botTestTag, sqlmock.AnyArg()).
			WillReturnError(fmt.Errorf("deadlock detected"))
		mock.ExpectRollback()
		mock.ExpectExec(reBotHeartbeat).WithArgs(botNominateWorkerName, "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		w := newBotTestNominator(t, db, nil)
		res := w.RunOnce(context.Background(), "pg")

		assert.Equal(t, "error", res.Status)
		assert.Equal(t, "upsert", res.Step)
		assert.Equal(t, 0, res.Written, "a rolled-back batch wrote nothing")
		assert.NoError(t, mock.ExpectationsWereMet(), "the batch must ROLLBACK, never COMMIT")
		assertBotLogOrder(t, botLogLines(buf), "ERROR step=upsert source=pg window=30m: upsert 203.0.113.8/32: deadlock detected")
	})

	t.Run("unknown_source", func(t *testing.T) {
		db, mock := newBotTestDB(t)
		w := newBotTestNominator(t, db, nil)
		res := w.RunOnce(context.Background(), "athena")
		assert.Equal(t, "error", res.Status)
		assert.Equal(t, "source", res.Step)
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

// ── lake pass (worker side; Athena execution itself is live-only) ───────────

func TestBotNominate_LakePass(t *testing.T) {
	t.Run("rows_flow_through_same_rule_and_writer", func(t *testing.T) {
		buf := captureLogs(t)
		db, mock := newBotTestDB(t)
		mock.ExpectQuery(reBotLookup).
			WithArgs(pq.Array([]string{"198.51.100.9"})).
			WillReturnRows(botLookupRows().AddRow("198.51.100.9", nil))
		mock.ExpectBegin()
		mock.ExpectExec(reBotSetLocal).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(reBotUpsert).
			WithArgs("198.51.100.9/32", botTestTag, botEvidence("198.51.100.9", 3, 3, 12, "lake:30d")).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		expectBotHeartbeat(mock)

		fake := &fakeBotLakeReader{rows: []BotNominee{
			{IP: "198.51.100.9", Subs: 3, CoClick: 3, Clicks: 12},
			{IP: "198.51.100.1", Subs: 1, CoClick: 1, Clicks: 8}, // home connection, dropped
		}}
		w := newBotTestNominator(t, db, nil).SetLakeReader(fake)
		res := w.RunOnce(context.Background(), "lake")

		require.Equal(t, "ok", res.Status, "err=%v step=%s", res.Err, res.Step)
		assert.Equal(t, 1, fake.calls)
		assert.Equal(t, 30, fake.days)
		assert.Equal(t, "lake", res.Source)
		assert.Equal(t, "30d", res.Window)
		assert.Equal(t, 1, res.Matched)
		assert.Equal(t, 1, res.Dropped)
		assert.Equal(t, 1, res.Written)
		assert.NoError(t, mock.ExpectationsWereMet())
		assertBotLogOrder(t, botLogLines(buf), "query source=lake window=30d", "SUMMARY source=lake window=30d matched=1 new=1 written=1 tag="+botTestTag)
	})

	t.Run("reader_error_no_write", func(t *testing.T) {
		buf := captureLogs(t)
		db, mock := newBotTestDB(t)
		mock.ExpectExec(reBotHeartbeat).WithArgs(botNominateWorkerName, "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		fake := &fakeBotLakeReader{err: fmt.Errorf("athena: query FAILED")}
		w := newBotTestNominator(t, db, nil).SetLakeReader(fake)
		res := w.RunOnce(context.Background(), "lake")
		assert.Equal(t, "error", res.Status)
		assert.Equal(t, "query", res.Step)
		assert.NoError(t, mock.ExpectationsWereMet())
		assertBotLogOrder(t, botLogLines(buf), "ERROR step=query source=lake window=30d: athena: query FAILED")
	})

	t.Run("reader_unavailable_skips_loudly", func(t *testing.T) {
		buf := captureLogs(t)
		db, mock := newBotTestDB(t)
		w := newBotTestNominator(t, db, nil) // lakeAvailable=false
		res := w.RunOnce(context.Background(), "lake")
		assert.Equal(t, "lake-disabled", res.Status)
		assert.NoError(t, mock.ExpectationsWereMet(), "no DB traffic at all")
		assertBotLogOrder(t, botLogLines(buf), "query source=lake window=30d skipped")
	})

	t.Run("due_window_once_per_denver_day", func(t *testing.T) {
		db, _ := newBotTestDB(t)
		w := newBotTestNominator(t, db, nil)
		at := func(h, m int) time.Time { return time.Date(2026, 9, 6, h, m, 0, 0, w.loc) }
		_, due := w.lakeDue(at(3, 9))
		assert.False(t, due, "03:09 is before the window")
		day, due := w.lakeDue(at(3, 10))
		assert.True(t, due, "03:10 opens the window")
		assert.Equal(t, "2026-09-06", day)
		_, due = w.lakeDue(at(4, 10))
		assert.False(t, due, "04:10 is past the window")
		w.lastLakeDay = day
		_, due = w.lakeDue(at(3, 30))
		assert.False(t, due, "already ran today")
		_, due = w.lakeDue(time.Date(2026, 9, 7, 3, 30, 0, 0, w.loc))
		assert.True(t, due, "next Denver day runs again")
	})

	t.Run("tick_in_window_runs_lake_once", func(t *testing.T) {
		t.Setenv("BOT_NOMINATE_ENABLED", "1")
		buf := captureLogs(t)
		_, rdb := newBotTestRedis(t)
		db, mock := newBotTestDB(t)
		// tick 1: PG pass (empty) then the daily lake pass
		mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows())
		expectBotHeartbeat(mock)
		mock.ExpectQuery(reBotLookup).WithArgs(pq.Array([]string{"198.51.100.9"})).
			WillReturnRows(botLookupRows().AddRow("198.51.100.9", nil))
		mock.ExpectBegin()
		mock.ExpectExec(reBotSetLocal).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(reBotUpsert).WithArgs("198.51.100.9/32", botTestTag, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		expectBotHeartbeat(mock)
		// tick 2, still inside the window: PG pass only
		mock.ExpectQuery(reBotNominateQuery).WillReturnRows(botNomRows())
		expectBotHeartbeat(mock)

		fake := &fakeBotLakeReader{rows: []BotNominee{{IP: "198.51.100.9", Subs: 3, CoClick: 3, Clicks: 12}}}
		w := newBotTestNominator(t, db, rdb).SetLakeReader(fake)
		// 03:30 America/Denver = 09:30 UTC -> tag stays bot-nominate-20260906
		w.SetNow(func() time.Time { return time.Date(2026, 9, 6, 3, 30, 0, 0, w.loc) })

		assert.Equal(t, "ran", w.tick(context.Background()))
		assert.Equal(t, "ran", w.tick(context.Background()))
		assert.Equal(t, 1, fake.calls, "lake pass runs once per Denver day")
		assert.NoError(t, mock.ExpectationsWereMet())
		assertBotLogOrder(t, botLogLines(buf),
			"tick start source=pg window=30m enabled=true lock=acquired",
			"SUMMARY source=pg",
			"tick start source=lake window=30d enabled=true lock=acquired (daily pass for 2026-09-06)",
			"SUMMARY source=lake window=30d matched=1 new=1 written=1 tag="+botTestTag,
			"tick start source=pg window=30m enabled=true lock=acquired",
			"SUMMARY source=pg",
		)
	})
}

// ── 10: the burst signature is link-agnostic ────────────────────────────────

// Text half, always run: the PG query joins a subscriber's clicks to its OWN
// clicks on a DIFFERENT link_url within 5 seconds, and carries no link-host
// allow-list. Dropping "b.link_url <> a.link_url" makes every row match
// itself (abs(0) <= 5), so every subscriber with one click "bursts" and every
// IP with two people nominates — the exact failure this pins.
func TestBotNominate_PGSQL_BurstSignatureIsLinkAgnostic(t *testing.T) {
	sql := BuildBotNominatePGSQL(30)
	assert.Contains(t, sql, "b.link_url <> a.link_url", "DISTINCT links: repeats of one link are duplicate logging, i.e. a human")
	assert.Contains(t, sql, "abs(EXTRACT(EPOCH FROM (b.event_at-a.event_at))) <= 5")
	assert.Contains(t, sql, "b.subscriber_id=a.subscriber_id", "the burst is within ONE subscriber")
	assert.Contains(t, sql, "event_type='clicked'", "PAST tense in Postgres; 'click' is a silent zero")
	assert.Contains(t, sql, "INTERVAL '30 minutes'")
	assert.Contains(t, sql, "host(mo.ip_address)")
	assert.Contains(t, sql, "WHERE subs >= 2 AND coclick::numeric/subs >= 0.8")
	for _, host := range []string{"~*", "cratoolpro", "wcl-heloc", "/unsub", "affiliateaccesskey"} {
		assert.NotContains(t, sql, host, "link-agnostic: no link-host filter may remain")
	}
	assert.Contains(t, BuildBotNominatePGSQL(0), "INTERVAL '30 minutes'", "non-positive window falls back to the default")
	assert.Contains(t, BuildBotNominatePGSQL(45), "INTERVAL '45 minutes'")

	// The lake half renders the SAME rule from the worker's constants: present
	// tense 'click', dt partition, epoch-ms with the 5s rendered as 5000.
	lake := analytics.BuildBotNominateLakeSQL(30, botNominateMinSubs, botNominateMinRate, botNominateCoClickS)
	assert.Contains(t, lake, "FROM ignite_analytics.email_events")
	assert.Contains(t, lake, "event_type='click'")
	assert.Contains(t, lake, "dt >= date_format(date_add('day',-30,current_date),'%Y-%m-%d')")
	assert.Contains(t, lake, "b.link_url <> a.link_url")
	assert.Contains(t, lake, "abs(b.event_epoch_ms-a.event_epoch_ms) <= 5000")
	assert.Contains(t, lake, "WHERE subs >= 2 AND coclick*1.0/subs >= 0.8")
	for _, host := range []string{"regexp_like", "cratoolpro", "/unsub"} {
		assert.NotContains(t, lake, host, "link-agnostic: no link-host filter may remain")
	}
}

// Behavioural half, opt-in: run the REAL nomination SQL against a real
// Postgres with a session TEMP table shadowing mailing_tracking_events. This
// is the only in-repo proof that the signature does what the operator means;
// sqlmock cannot execute it.
//
//	BOT_NOMINATE_TEST_PG_DSN='postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable' \
//	  go test ./internal/worker/ -run TestBotNominate_BurstSignature_RealPG -v
//
// Nothing persists: the temp table dies with the connection.
func TestBotNominate_BurstSignature_RealPG(t *testing.T) {
	dsn := os.Getenv("BOT_NOMINATE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SKIPPED, NOT PASSED: the burst signature lives in SQL and is only provable against a real Postgres. " +
			"Set BOT_NOMINATE_TEST_PG_DSN (local apex-postgres: postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable).")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx) // one session: the TEMP table must be visible to the query
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.ExecContext(ctx, `CREATE TEMP TABLE mailing_tracking_events (
		subscriber_id uuid, ip_address inet, link_url text, event_type text, event_at timestamptz)`)
	require.NoError(t, err)

	base := time.Now().Add(-2 * time.Minute) // inside the 30-minute window
	type ev struct {
		ip, link string
		sub      uuid.UUID
		offset   time.Duration
	}
	var rows []ev
	sub := func() uuid.UUID { return uuid.New() }
	// A: 2 subs, both burst on ARBITRARY links (no money/opt-out host anywhere) -> 2/2 nominates
	a1, a2 := sub(), sub()
	rows = append(rows,
		ev{"203.0.113.10", "https://example.org/a", a1, 0}, ev{"203.0.113.10", "https://example.org/b", a1, 2 * time.Second},
		ev{"203.0.113.10", "https://example.net/p", a2, 0}, ev{"203.0.113.10", "https://example.net/q", a2, 4 * time.Second})
	// B: sub b1 = EIGHT rows of ONE link inside 5s (duplicate logging = a human);
	//    sub b2 = burst. 2 subs / 1 burst = 50% -> not nominated. If repeats
	//    counted, B would be 2/2 and nominate.
	b1, b2 := sub(), sub()
	for i := 0; i < 8; i++ {
		rows = append(rows, ev{"203.0.113.20", "https://example.org/same", b1, time.Duration(i) * 500 * time.Millisecond})
	}
	rows = append(rows, ev{"203.0.113.20", "https://example.org/x", b2, 0}, ev{"203.0.113.20", "https://example.org/y", b2, time.Second})
	// C: sub c1 = two DIFFERENT links 6s apart (no burst); sub c2 = burst -> 50%, not nominated.
	//    If 6s counted, C would be 2/2.
	c1, c2 := sub(), sub()
	rows = append(rows,
		ev{"203.0.113.30", "https://example.org/1", c1, 0}, ev{"203.0.113.30", "https://example.org/2", c1, 6 * time.Second},
		ev{"203.0.113.30", "https://example.org/3", c2, 0}, ev{"203.0.113.30", "https://example.org/4", c2, 3 * time.Second})
	// D: ONE subscriber, perfect burst -> never (home connection)
	d1 := sub()
	rows = append(rows, ev{"203.0.113.40", "https://example.org/d1", d1, 0}, ev{"203.0.113.40", "https://example.org/d2", d1, time.Second})
	// E: 5 subs, 4 burst + 1 single-link human = 80% -> nominates
	for i := 0; i < 4; i++ {
		s := sub()
		rows = append(rows, ev{"203.0.113.50", fmt.Sprintf("https://example.org/e%d", i), s, 0},
			ev{"203.0.113.50", fmt.Sprintf("https://example.org/f%d", i), s, 5 * time.Second}) // exactly 5s: inclusive
	}
	rows = append(rows, ev{"203.0.113.50", "https://example.org/human", sub(), 0})
	// F: v6, 2 subs both burst -> nominates, host() renders the bare address
	f1, f2 := sub(), sub()
	rows = append(rows,
		ev{"2001:db8::5", "https://example.org/v1", f1, 0}, ev{"2001:db8::5", "https://example.org/v2", f1, time.Second},
		ev{"2001:db8::5", "https://example.org/v3", f2, 0}, ev{"2001:db8::5", "https://example.org/v4", f2, time.Second})
	// H: 2 subs, each EIGHT rows of ONE link -> 2 subs / 0 burst -> not nominated.
	//    The most important negative: with repeats counting, H is 2/2.
	for _, s := range []uuid.UUID{sub(), sub()} {
		for i := 0; i < 8; i++ {
			rows = append(rows, ev{"203.0.113.80", "https://example.org/only", s, time.Duration(i) * 300 * time.Millisecond})
		}
	}
	// G: out-of-window burst (2 hours ago) -> invisible to the 30-minute pass
	g1, g2 := sub(), sub()
	rows = append(rows,
		ev{"203.0.113.90", "https://example.org/g1", g1, -2 * time.Hour}, ev{"203.0.113.90", "https://example.org/g2", g1, -2*time.Hour + time.Second},
		ev{"203.0.113.90", "https://example.org/g3", g2, -2 * time.Hour}, ev{"203.0.113.90", "https://example.org/g4", g2, -2*time.Hour + time.Second})

	for _, r := range rows {
		_, err := conn.ExecContext(ctx, `INSERT INTO mailing_tracking_events VALUES ($1, $2::inet, $3, 'clicked', $4)`,
			r.sub, r.ip, r.link, base.Add(r.offset))
		require.NoError(t, err)
	}

	rs, err := conn.QueryContext(ctx, BuildBotNominatePGSQL(30))
	require.NoError(t, err)
	defer rs.Close()
	got := map[string][3]int64{}
	for rs.Next() {
		var ip string
		var subs, coclick, clicks int64
		require.NoError(t, rs.Scan(&ip, &subs, &coclick, &clicks))
		got[ip] = [3]int64{subs, coclick, clicks}
	}
	require.NoError(t, rs.Err())

	want := map[string][3]int64{
		"203.0.113.10": {2, 2, 4},
		"203.0.113.50": {5, 4, 9},
		"2001:db8::5":  {2, 2, 4},
	}
	assert.Equal(t, want, got, "nominated set (ip -> subs, burst, clicks)")
	for _, ip := range []string{"203.0.113.20", "203.0.113.30", "203.0.113.40", "203.0.113.80", "203.0.113.90"} {
		assert.NotContains(t, got, ip)
	}
}
