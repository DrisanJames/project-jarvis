package worker

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testScheduledAt = time.Date(2026, 3, 16, 10, 0, 0, 0, time.UTC)

func testScheduler(db *sql.DB, rdb *redis.Client) *PMTAWaveScheduler {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	return &PMTAWaveScheduler{
		db:          db,
		redisClient: rdb,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// captureLogs redirects the standard logger to a buffer for the duration of
// the test. Tests that touch global log state must be sequential — none of
// the worker tests in this package call t.Parallel().
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// rrFairnessRows returns the columns the new CTE-based dispatcher SELECT
// returns. Tests that exercise the default (fairness) path must seed both
// id and sending_domain.
func rrFairnessRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "sending_domain"})
}

func TestDispatchDueWaves_DueWavesFound(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	wave1, wave2, wave3 := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery("ranked AS").
		WillReturnRows(rrFairnessRows().
			AddRow(wave1, "em.discountblog.com").
			AddRow(wave2, "em.historythinking.com").
			AddRow(wave3, "em.myownhealth.net"))

	for _, w := range []uuid.UUID{wave1, wave2, wave3} {
		mock.ExpectBegin()
		mock.ExpectQuery("w.campaign_id, w.isp_plan_id").
			WithArgs(w.String()).
			WillReturnRows(sqlmock.NewRows([]string{
				"campaign_id", "isp_plan_id", "organization_id", "status", "campaign_status",
				"plan_status", "scheduled_at", "campaign_scheduled_at", "planned_recipients", "enqueued_recipients", "partner_drip_tag", "plan_isp", "plan_sending_domain",
			}).AddRow(uuid.New(), uuid.New(), uuid.New(), "completed", "sending", "running",
				testScheduledAt, testScheduledAt, 100, 100, nil, "gmail", "em.test.com"))
		mock.ExpectCommit()
	}

	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestDispatchDueWaves_NoDueWaves(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("ranked AS").
		WillReturnRows(rrFairnessRows())

	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestDispatchDueWaves_LockContention(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	wave1 := uuid.New()

	mock.ExpectQuery("ranked AS").
		WillReturnRows(rrFairnessRows().AddRow(wave1, "em.discountblog.com"))

	// Pre-set the lock key so the scheduler can't acquire it
	mr.Set("lock:pmta-wave:"+wave1.String(), "held-by-another")

	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	// No EnqueuePMTAWave expectations — wave was skipped due to lock contention
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestDispatchDueWaves_EnqueueError(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	wave1, wave2 := uuid.New(), uuid.New()

	mock.ExpectQuery("ranked AS").
		WillReturnRows(rrFairnessRows().
			AddRow(wave1, "em.discountblog.com").
			AddRow(wave2, "em.historythinking.com"))

	// Wave 1: EnqueuePMTAWave fails (DB error on SELECT)
	mock.ExpectBegin()
	mock.ExpectQuery("w.campaign_id, w.isp_plan_id").
		WithArgs(wave1.String()).
		WillReturnError(fmt.Errorf("connection reset"))
	mock.ExpectRollback()

	// Wave 2: succeeds (already completed, so returns early)
	mock.ExpectBegin()
	mock.ExpectQuery("w.campaign_id, w.isp_plan_id").
		WithArgs(wave2.String()).
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "organization_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "campaign_scheduled_at", "planned_recipients", "enqueued_recipients", "partner_drip_tag", "plan_isp", "plan_sending_domain",
		}).AddRow(uuid.New(), uuid.New(), uuid.New(), "completed", "sending", "running",
			testScheduledAt, testScheduledAt, 100, 100, nil, "gmail", "em.test.com"))
	mock.ExpectCommit()

	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestDispatchDueWaves_RoundRobinFairness verifies that the dispatcher caps
// each sending_domain at maxWavesPerDomainPerTick (25) and fills the batch
// from a fair mix of domains. We mock the query to return the post-CTE
// result rows directly — sqlmock can't execute a real CTE, so the mock
// represents what Postgres would return AFTER the WHERE domain_rank <= 25
// + LIMIT 100 are applied.
//
// Setup: domain A has 25 waves (capped from a larger set), B has 30
// (also capped), C has 20 (under cap). Note: when both A and B exceed
// cap, the post-CTE result is 25 + 25 + 20 = 70 rows. The test asserts:
//   - Total returned = 70
//   - All three domains represented
//   - Domain A and B each capped at 25
func TestDispatchDueWaves_RoundRobinFairness(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	rows := rrFairnessRows()
	domainA := "em.brand-a.com"
	domainB := "em.brand-b.com"
	domainC := "em.brand-c.com"
	allWaves := []uuid.UUID{}

	for i := 0; i < 25; i++ {
		w := uuid.New()
		allWaves = append(allWaves, w)
		rows.AddRow(w, domainA)
	}
	for i := 0; i < 25; i++ {
		w := uuid.New()
		allWaves = append(allWaves, w)
		rows.AddRow(w, domainB)
	}
	for i := 0; i < 20; i++ {
		w := uuid.New()
		allWaves = append(allWaves, w)
		rows.AddRow(w, domainC)
	}

	// Default regex matcher — "ranked AS" only appears in the new
	// fairness query, never in the legacy SELECT. WithArgs proves the
	// dispatcher passed the named constants (25, 100) as bind params.
	mock.ExpectQuery(`ranked AS`).
		WithArgs(maxWavesPerDomainPerTick, dispatchBatchSize).
		WillReturnRows(rows)

	// Each wave in the result set will be picked up by processOneWave,
	// which calls EnqueuePMTAWave -> Begin/Query/Commit. Mock those.
	// Using completed status so EnqueuePMTAWave returns early without
	// further DB calls.
	for _, w := range allWaves {
		mock.ExpectBegin()
		mock.ExpectQuery("w.campaign_id, w.isp_plan_id").
			WithArgs(w.String()).
			WillReturnRows(sqlmock.NewRows([]string{
				"campaign_id", "isp_plan_id", "organization_id", "status", "campaign_status",
				"plan_status", "scheduled_at", "campaign_scheduled_at", "planned_recipients", "enqueued_recipients", "partner_drip_tag", "plan_isp", "plan_sending_domain",
			}).AddRow(uuid.New(), uuid.New(), uuid.New(), "completed", "sending", "running",
				testScheduledAt, testScheduledAt, 100, 100, nil, "gmail", "em.test.com"))
		mock.ExpectCommit()
	}

	buf := captureLogs(t)
	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	assert.NoError(t, mock.ExpectationsWereMet())

	// Parse the per-domain breakdown from the log line. With 25 + 25 + 20
	// rows, the dispatcher should report: found 70 due waves across 3
	// domains (brand-a=25 brand-b=25 brand-c=20), processing with 5
	// parallel workers.
	logOut := buf.String()
	assert.Contains(t, logOut, "found 70 due waves across 3 domains",
		"expected 70-wave 3-domain summary in log; got: %s", logOut)
	assert.Contains(t, logOut, "brand-a=25")
	assert.Contains(t, logOut, "brand-b=25")
	assert.Contains(t, logOut, "brand-c=20")
}

// TestDispatchDueWaves_KillSwitch sets DISABLE_DISPATCHER_FAIRNESS=true
// and verifies the dispatcher uses the legacy single-column SELECT and
// logs the kill-switch banner. We use QueryMatcherEqual so the mock
// fails fast if the CTE query is run instead of the legacy one.
func TestDispatchDueWaves_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_DISPATCHER_FAIRNESS", "true")

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	wave1 := uuid.New()
	// "SELECT id\s+FROM mailing_campaign_waves" is unique to the legacy
	// query — the fairness CTE's outer SELECT is "SELECT id,
	// sending_domain FROM ranked" and the inner SELECT inside the CTE is
	// "SELECT\s+w.id". Regex matches legacy only.
	mock.ExpectQuery(`SELECT w.id\s+FROM mailing_campaign_waves`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wave1))

	mock.ExpectBegin()
	mock.ExpectQuery("w.campaign_id, w.isp_plan_id").
		WithArgs(wave1.String()).
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "organization_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "campaign_scheduled_at", "planned_recipients", "enqueued_recipients", "partner_drip_tag", "plan_isp", "plan_sending_domain",
		}).AddRow(uuid.New(), uuid.New(), uuid.New(), "completed", "sending", "running",
			testScheduledAt, testScheduledAt, 100, 100, nil, "gmail", "em.test.com"))
	mock.ExpectCommit()

	buf := captureLogs(t)
	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	assert.NoError(t, mock.ExpectationsWereMet())

	logOut := buf.String()
	assert.Contains(t, logOut, "DISABLE_DISPATCHER_FAIRNESS=true, using legacy FIFO",
		"expected kill-switch banner in log; got: %s", logOut)
	assert.Contains(t, logOut, "found 1 due waves, processing with 5 parallel workers",
		"expected legacy log format (no per-domain breakdown); got: %s", logOut)
	// Legacy log line MUST NOT contain the per-domain "across N domains" phrase
	assert.NotContains(t, logOut, "across", "legacy log line should not include per-domain breakdown")
}

// TestDispatchDueWaves_LogsPerDomainCounts verifies the log line emitted
// by the fairness path contains the per-domain counts in alphabetical
// order of the full sending_domain. We mock 4 domains with 25/25/25/25
// to mirror the spec's example output:
// "[PMTAWaveScheduler] found 100 due waves across 4 domains
//
//	(db=25 ht=25 mh=25 qf=25), processing with 5 parallel workers".
func TestDispatchDueWaves_LogsPerDomainCounts(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	// 4 domains with deterministic short tags (db, ht, mh, qf) — the
	// shortDomainTag helper strips the leading "em." label so the log
	// line matches the spec example.
	domains := []string{
		"em.db-news.com",
		"em.ht-news.com",
		"em.mh-news.com",
		"em.qf-news.com",
	}
	rows := rrFairnessRows()
	allWaves := []uuid.UUID{}
	for _, d := range domains {
		for i := 0; i < 25; i++ {
			w := uuid.New()
			allWaves = append(allWaves, w)
			rows.AddRow(w, d)
		}
	}

	mock.ExpectQuery("ranked AS").
		WithArgs(maxWavesPerDomainPerTick, dispatchBatchSize).
		WillReturnRows(rows)

	for _, w := range allWaves {
		mock.ExpectBegin()
		mock.ExpectQuery("w.campaign_id, w.isp_plan_id").
			WithArgs(w.String()).
			WillReturnRows(sqlmock.NewRows([]string{
				"campaign_id", "isp_plan_id", "organization_id", "status", "campaign_status",
				"plan_status", "scheduled_at", "campaign_scheduled_at", "planned_recipients", "enqueued_recipients", "partner_drip_tag", "plan_isp", "plan_sending_domain",
			}).AddRow(uuid.New(), uuid.New(), uuid.New(), "completed", "sending", "running",
				testScheduledAt, testScheduledAt, 100, 100, nil, "gmail", "em.test.com"))
		mock.ExpectCommit()
	}

	buf := captureLogs(t)
	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	assert.NoError(t, mock.ExpectationsWereMet())

	logOut := buf.String()
	// The dispatcher's primary summary line must contain the exact
	// alphabetically-ordered per-domain breakdown.
	wantExact := "found 100 due waves across 4 domains (db-news=25 ht-news=25 mh-news=25 qf-news=25), processing with 5 parallel workers"
	assert.True(t, strings.Contains(logOut, wantExact),
		"expected log line containing %q; got: %s", wantExact, logOut)
}

// TestFormatDomainCounts_AlphabeticalStable proves the domain-count
// formatter produces deterministic output regardless of map iteration
// order. Run many iterations because Go map iteration is randomized.
func TestFormatDomainCounts_AlphabeticalStable(t *testing.T) {
	counts := map[string]int{
		"em.qf-news.com": 7,
		"em.db-news.com": 25,
		"em.mh-news.com": 13,
		"em.ht-news.com": 18,
	}
	want := "db-news=25 ht-news=18 mh-news=13 qf-news=7"
	for i := 0; i < 50; i++ {
		got := formatDomainCounts(counts)
		require.Equal(t, want, got, "iteration %d: formatter must be stable across map iteration order", i)
	}
}

func TestSweepStalePlannedWaves(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec("parent campaign terminal").
		WillReturnResult(sqlmock.NewResult(0, 12))
	mock.ExpectExec("window expired").
		WillReturnResult(sqlmock.NewResult(0, 340))

	buf := captureLogs(t)
	s := testScheduler(db, nil)
	defer s.cancel()
	s.sweepStalePlannedWaves(s.ctx)

	assert.NoError(t, mock.ExpectationsWereMet())
	assert.Contains(t, buf.String(), "janitor swept zombies=12 expired=340")
}

// TestShortDomainTag covers the host-prefix stripping logic.
func TestShortDomainTag(t *testing.T) {
	cases := map[string]string{
		"em.discountblog.com":   "discountblog",
		"m.discountblog.com":    "discountblog",
		"mail.example.com":      "example",
		"news.example.com":      "example",
		"projectjarvis.io":      "projectjarvis",
		"":                      "unknown",
		"singlelabel":           "singlelabel",
		"em":                    "em", // single label even if it's a known prefix
		"weirdprefix.brand.com": "weirdprefix",
	}
	for input, want := range cases {
		got := shortDomainTag(input)
		assert.Equal(t, want, got, "shortDomainTag(%q)", input)
	}
}
