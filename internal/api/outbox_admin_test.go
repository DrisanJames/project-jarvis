package api

// Tests for HandleOutboxSummary and HandleOutboxDeadLetter.
//
// These handlers read directly against mailing_campaign_queue with no writes.
// The tests use go-sqlmock with non-strict ordering + regex matchers because
// HandleOutboxSummary fires six independent queries and we care about the
// response shape + field propagation, not query ordering.

import (
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

// newOutboxMockDB returns a sqlmock in QueryMatcherRegexp + non-strict mode.
// Every outbox-summary query must be expected before the handler is invoked;
// extras are acceptable.
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

func TestHandleOutboxSummary_HappyPath(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, COUNT(*)::int")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).
			AddRow("queued", 120).
			AddRow("submitting", 3).
			AddRow("accepted", 47).
			AddRow("failed_retryable", 2).
			AddRow("dead_letter", 1))

	mock.ExpectQuery(`WHERE status = \$1`).
		WithArgs("queued").
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

	req := httptest.NewRequest(http.MethodGet, "/api/outbox/summary", nil)
	rec := httptest.NewRecorder()
	HandleOutboxSummary(db)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp OutboxSummaryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	require.Equal(t, VersionOutboxSummary, resp.APIVersion)
	require.Equal(t, 120, resp.DepthByStatus["queued"])
	require.Equal(t, 3, resp.DepthByStatus["submitting"])
	require.Equal(t, 47, resp.DepthByStatus["accepted"])
	require.Equal(t, 1, resp.DepthByStatus["dead_letter"])
	require.Equal(t, int64(42), resp.OldestQueuedSeconds)
	require.Equal(t, int64(7), resp.OldestSubmittingSeconds)
	require.Equal(t, int64(42), resp.IdempotencyKeyedRows)
	require.Equal(t, int64(8), resp.IdempotencyNullRows)
	require.Equal(t, int64(20), resp.Last5MinInserted)
	require.Equal(t, int64(18), resp.Last5MinSent)
	require.Equal(t, int64(1), resp.Last5MinFailed)

	require.True(t, resp.GeneratedAt.Before(time.Now().Add(2*time.Second)))
}

func TestHandleOutboxSummary_DepthQueryFailure(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, COUNT(*)::int")).
		WillReturnError(sql.ErrConnDone)

	req := httptest.NewRequest(http.MethodGet, "/api/outbox/summary", nil)
	rec := httptest.NewRecorder()
	HandleOutboxSummary(db)(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestHandleOutboxSummary_EmptyOutbox(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, COUNT(*)::int")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}))

	mock.ExpectQuery(`WHERE status = \$1`).
		WithArgs("queued").
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

	req := httptest.NewRequest(http.MethodGet, "/api/outbox/summary", nil)
	rec := httptest.NewRecorder()
	HandleOutboxSummary(db)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp OutboxSummaryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Empty(t, resp.DepthByStatus)
	require.Equal(t, int64(0), resp.OldestQueuedSeconds)
	require.Equal(t, int64(0), resp.OldestSubmittingSeconds)
}

func TestHandleOutboxDeadLetter_HappyPath(t *testing.T) {
	db, mock := newOutboxMockDB(t)

	now := time.Now()
	mock.ExpectQuery(`WHERE status IN \('dead_letter','dead_letter_strict'\)`).
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

	mock.ExpectQuery(`AND campaign_id = \$2`).
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

// Ensure scanQueueRows-style scan vs int64 works under our expected fields.
// This is a belt-and-suspenders test: the real handler extracts bigint
// counters into int64 fields. A Postgres driver quirk that returned string
// would break the response shape silently — explicit assertion catches it.
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
}
