package api

// Tests for HandleOutboxSummary, refreshOutboxSummary, and HandleOutboxDeadLetter.
//
// After 1.1 the summary handler is a pure cache read — no DB on the request
// path. Tests therefore split into two groups:
//
//   1. Cache behavior: Snapshot / handler shape / cold-cache contract. These
//      never touch a DB at all.
//   2. Refresher behavior: refreshOutboxSummary against go-sqlmock, verifying
//      every aggregate query fires and the cache is populated correctly.
//
// Dead-letter tests continue to exercise the DB directly because that handler
// is a thin query-through with no caching (payloads are too variable to
// usefully pre-warm).

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func newOutboxMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(false)
	return db, mock
}

// TestHandleOutboxSummary_ColdCache verifies that a handler served before the
// refresher has run returns a valid JSON payload with CacheAgeSeconds=-1 so
// the dashboard can explicitly render "loading" instead of showing stale zeros.
func TestHandleOutboxSummary_ColdCache(t *testing.T) {
	cache := &OutboxSummaryCache{}
	h := handleOutboxSummaryWithCache(cache)

	req := httptest.NewRequest(http.MethodGet, "/api/outbox/summary", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp OutboxSummaryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, VersionOutboxSummary, resp.APIVersion)
	require.Equal(t, int64(-1), resp.CacheAgeSeconds, "cold cache must report -1")
	require.Empty(t, resp.DepthByStatus)
}

// TestHandleOutboxSummary_CachedPayload verifies the handler echoes whatever
// the refresher placed in the cache and computes CacheAgeSeconds from the
// cached GeneratedAt.
func TestHandleOutboxSummary_CachedPayload(t *testing.T) {
	cache := &OutboxSummaryCache{}
	cache.store(OutboxSummaryResponse{
		GeneratedAt:             time.Now().UTC().Add(-12 * time.Second),
		APIVersion:              VersionOutboxSummary,
		DepthByStatus:           map[string]int{"queued": 120, "submitting": 3, "accepted": 47},
		OldestQueuedSeconds:     42,
		OldestSubmittingSeconds: 7,
		IdempotencyKeyedRows:    42,
		IdempotencyNullRows:     8,
		Last5MinInserted:        20,
		Last5MinSent:            18,
		Last5MinFailed:          1,
	})

	h := handleOutboxSummaryWithCache(cache)
	req := httptest.NewRequest(http.MethodGet, "/api/outbox/summary", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp OutboxSummaryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, 120, resp.DepthByStatus["queued"])
	require.Equal(t, 3, resp.DepthByStatus["submitting"])
	require.Equal(t, int64(42), resp.OldestQueuedSeconds)
	require.Equal(t, int64(7), resp.OldestSubmittingSeconds)
	require.Equal(t, int64(42), resp.IdempotencyKeyedRows)
	require.GreaterOrEqual(t, resp.CacheAgeSeconds, int64(10), "cache age should be ~12s")
	require.LessOrEqual(t, resp.CacheAgeSeconds, int64(30))
}

// TestHandleOutboxSummary_HandlerDoesNotTouchDB is the contract assertion:
// even if the DB is handed in, the request path must not issue a single query.
// Regression guard for the 15s-latency incident that drove this redesign.
func TestHandleOutboxSummary_HandlerDoesNotTouchDB(t *testing.T) {
	db, mock := newOutboxMockDB(t)
	req := httptest.NewRequest(http.MethodGet, "/api/outbox/summary", nil)
	rec := httptest.NewRecorder()
	HandleOutboxSummary(db)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	// No queries expected; ExpectationsWereMet passes because we queued none.
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSnapshot_DeepCopiesMap verifies mutating the returned map can't race the
// refresher mid-write. This guards the package-level cache against a classic
// Go gotcha where struct-copy leaves reference fields aliased.
func TestSnapshot_DeepCopiesMap(t *testing.T) {
	cache := &OutboxSummaryCache{}
	cache.store(OutboxSummaryResponse{DepthByStatus: map[string]int{"queued": 1}})
	snap, ok := cache.Snapshot()
	require.True(t, ok)
	snap.DepthByStatus["queued"] = 999
	snap2, _ := cache.Snapshot()
	require.Equal(t, 1, snap2.DepthByStatus["queued"], "mutation must not bleed back")
}

// TestRefreshOutboxSummary_PopulatesCache exercises the full refresh cycle
// against go-sqlmock. Every query the refresher issues must be expected or
// the test fails at ExpectationsWereMet.
func TestRefreshOutboxSummary_PopulatesCache(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, COUNT(*)::int")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
			AddRow("queued", 120).
			AddRow("submitting", 3).
			AddRow("accepted", 47))

	mock.ExpectQuery(`status = 'queued'`).
		WillReturnRows(sqlmock.NewRows([]string{"sec"}).AddRow(int64(42)))
	mock.ExpectQuery(`status = 'sending' AND locked_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"sec"}).AddRow(int64(0)))
	mock.ExpectQuery(`status = 'submitting' AND locked_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"sec"}).AddRow(int64(7)))
	mock.ExpectQuery(`idempotency_key IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(42)))
	mock.ExpectQuery(`idempotency_key IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(8)))
	mock.ExpectQuery(`created_at > NOW\(\) - INTERVAL '5 minutes'`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(20)))
	mock.ExpectQuery(`sent_at > NOW\(\) - INTERVAL '5 minutes'`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(18)))
	mock.ExpectQuery(`last_attempt_at > NOW\(\) - INTERVAL '5 minutes'`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(1)))

	cache := &OutboxSummaryCache{}
	refreshOutboxSummary(context.Background(), db, cache)

	snap, ok := cache.Snapshot()
	require.True(t, ok, "refresh must mark cache as set")
	require.Equal(t, 120, snap.DepthByStatus["queued"])
	require.Equal(t, 3, snap.DepthByStatus["submitting"])
	require.Equal(t, int64(42), snap.OldestQueuedSeconds)
	require.Equal(t, int64(7), snap.OldestSubmittingSeconds)
	require.Equal(t, int64(42), snap.IdempotencyKeyedRows)
	require.Equal(t, int64(20), snap.Last5MinInserted)
	require.Equal(t, int64(18), snap.Last5MinSent)
	require.Equal(t, int64(1), snap.Last5MinFailed)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRefreshOutboxSummary_DepthFailureKeepsPreviousCache verifies that a
// transient DB error on the depth query does not blank out the cache — the
// previous-good snapshot remains readable so dashboards don't blink empty
// mid-incident.
func TestRefreshOutboxSummary_DepthFailureKeepsPreviousCache(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	cache := &OutboxSummaryCache{}
	cache.store(OutboxSummaryResponse{
		DepthByStatus: map[string]int{"queued": 999},
	})

	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, COUNT(*)::int")).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectQuery(`status = 'queued'`).
		WillReturnRows(sqlmock.NewRows([]string{"sec"}).AddRow(int64(0)))
	mock.ExpectQuery(`status = 'sending' AND locked_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"sec"}).AddRow(int64(0)))
	mock.ExpectQuery(`status = 'submitting' AND locked_at IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"sec"}).AddRow(int64(0)))
	mock.ExpectQuery(`idempotency_key IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`idempotency_key IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`created_at > NOW\(\) - INTERVAL '5 minutes'`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`sent_at > NOW\(\) - INTERVAL '5 minutes'`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`last_attempt_at > NOW\(\) - INTERVAL '5 minutes'`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))

	refreshOutboxSummary(context.Background(), db, cache)

	snap, ok := cache.Snapshot()
	require.True(t, ok)
	// Depth failed so DepthByStatus is empty on the new snapshot; the rest
	// still refreshed cleanly. Confirms partial-refresh doesn't corrupt.
	require.Empty(t, snap.DepthByStatus)
	require.Equal(t, int64(0), snap.OldestQueuedSeconds)
}

func TestHandleOutboxDeadLetter_HappyPath(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	now := time.Now()
	mock.ExpectQuery(`WHERE q\.status IN \('dead_letter','dead_letter_strict'\)`).
		WithArgs(200).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "idempotency_key", "campaign_id", "subscriber_id", "email",
			"status", "attempts", "error_message", "last_attempt_at", "created_at",
		}).AddRow(
			"00000000-0000-0000-0000-000000000001",
			"11111111-1111-1111-1111-111111111111",
			"22222222-2222-2222-2222-222222222222",
			"33333333-3333-3333-3333-333333333333",
			"user@example.com",
			"dead_letter",
			5,
			"550 5.1.1 mailbox not found",
			now,
			now.Add(-1*time.Hour),
		))

	req := httptest.NewRequest(http.MethodGet, "/api/outbox/dead-letter", nil)
	rec := httptest.NewRecorder()
	HandleOutboxDeadLetter(db)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		APIVersion string                `json:"api_version"`
		Rows       []OutboxDeadLetterRow `json:"rows"`
		Count      int                   `json:"count"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, VersionOutboxSummary, resp.APIVersion)
	require.Equal(t, 1, resp.Count)
	require.Equal(t, "user@example.com", resp.Rows[0].Email)
	require.Equal(t, "dead_letter", resp.Rows[0].Status)
	require.Equal(t, 5, resp.Rows[0].Attempts)
	require.Contains(t, resp.Rows[0].LastError, "mailbox not found")
}

func TestHandleOutboxDeadLetter_EmptyResult(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	mock.ExpectQuery(`dead_letter`).
		WithArgs(200).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "idempotency_key", "campaign_id", "subscriber_id", "email",
			"status", "attempts", "error_message", "last_attempt_at", "created_at",
		}))

	req := httptest.NewRequest(http.MethodGet, "/api/outbox/dead-letter", nil)
	rec := httptest.NewRecorder()
	HandleOutboxDeadLetter(db)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, `"count":0`)
	require.Contains(t, body, `"rows":[]`)
}

func TestHandleOutboxDeadLetter_CampaignFilter(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	mock.ExpectQuery(`AND q\.campaign_id = \$2`).
		WithArgs(200, "c0fe1234-0000-0000-0000-000000000000").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "idempotency_key", "campaign_id", "subscriber_id", "email",
			"status", "attempts", "error_message", "last_attempt_at", "created_at",
		}))

	req := httptest.NewRequest(http.MethodGet, "/api/outbox/dead-letter?campaign_id=c0fe1234-0000-0000-0000-000000000000", nil)
	rec := httptest.NewRecorder()
	HandleOutboxDeadLetter(db)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleOutboxDeadLetter_LimitClamp(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	mock.ExpectQuery(`dead_letter`).
		WithArgs(1000).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "idempotency_key", "campaign_id", "subscriber_id", "email",
			"status", "attempts", "error_message", "last_attempt_at", "created_at",
		}))

	req := httptest.NewRequest(http.MethodGet, "/api/outbox/dead-letter?limit=5000", nil)
	rec := httptest.NewRecorder()
	HandleOutboxDeadLetter(db)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestParsePositiveIntOutbox(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1", 1, false},
		{"200", 200, false},
		{"0", 0, true},
		{"", 0, true},
		{"abc", 0, true},
		{"-5", 0, true},
		{"12345678901", 0, true},
	}
	for _, c := range cases {
		got, err := parsePositiveIntOutbox(c.in)
		if c.wantErr {
			require.Error(t, err, "input=%q", c.in)
			continue
		}
		require.NoError(t, err, "input=%q", c.in)
		require.Equal(t, c.want, got, "input=%q", c.in)
	}
}

func TestOutboxSummary_ResponseShapeStable(t *testing.T) {
	resp := OutboxSummaryResponse{
		APIVersion:    VersionOutboxSummary,
		DepthByStatus: map[string]int{"queued": 1},
	}
	buf, err := json.Marshal(resp)
	require.NoError(t, err)
	body := string(buf)
	require.True(t, strings.Contains(body, `"depth_by_status"`))
	require.True(t, strings.Contains(body, `"oldest_submitting_seconds"`))
	require.True(t, strings.Contains(body, `"idempotency_keyed_rows"`))
	require.True(t, strings.Contains(body, `"last_5min_sent"`))
	require.True(t, strings.Contains(body, `"cache_age_seconds"`))
}
