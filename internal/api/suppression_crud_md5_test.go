package api

// REQ-002 §3 — every suppression-entry writer derives md5_hash from the email
// when the caller didn't supply one. The planner's exclusion loader selects
// md5_hash ONLY (pmta_campaign_planner.go), so an md5-NULL row is never
// enforced — and with md5 NULL, ON CONFLICT (list_id, md5_hash) never fires,
// so duplicates accumulate. These tests pin that HandleAddSuppression and
// HandleBulkAddSuppressions can no longer create md5-NULL rows from
// email-only payloads (matching HandleAddSuppressionListEntry, which has
// always derived the hash).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/stretchr/testify/require"
)

func newSuppressionServiceForTest(t *testing.T) (*SuppressionService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &SuppressionService{db: db}, mock
}

func TestAddSuppression_EmailOnlyDerivesMD5(t *testing.T) {
	s, mock := newSuppressionServiceForTest(t)

	wantHash := engine.MD5Hash("user@example.com") // lower+trim canonical hash

	mock.ExpectExec(`INSERT INTO mailing_suppression_entries`).
		WithArgs(
			sqlmock.AnyArg(),   // id (sup-<nanos>)
			"list-1",           // list_id
			"user@example.com", // email (normalized)
			wantHash,           // md5_hash — MUST be derived, never NULL
			"manual test",      // reason
			"manual",           // source
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"email":"  User@Example.com ","reason":"manual test","source":"manual","list_id":"list-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/suppressions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.HandleAddSuppression(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAddSuppression_NoEmailNoHashRejected(t *testing.T) {
	s, mock := newSuppressionServiceForTest(t)

	body := `{"reason":"junk","source":"manual"}`
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/suppressions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.HandleAddSuppression(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an entry with neither email nor md5 is unenforceable and must be rejected, not stored as an md5-NULL row")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkAddSuppressions_EmailOnlyEntriesDeriveMD5(t *testing.T) {
	s, mock := newSuppressionServiceForTest(t)

	hashA := engine.MD5Hash("a@example.com")

	// Entry 1: email-only → md5 derived.
	mock.ExpectExec(`INSERT INTO mailing_suppression_entries`).
		WithArgs(sqlmock.AnyArg(), "list-1", "a@example.com", hashA, "unsub", "bulk").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Entry 2: md5-only → passed through lowercased, email NULL.
	mock.ExpectExec(`INSERT INTO mailing_suppression_entries`).
		WithArgs(sqlmock.AnyArg(), "list-1", nil, "abcdef0123456789abcdef0123456789", "unsub", "bulk").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Entry 3 (both empty) must be skipped — no third INSERT expected.

	body := `{"list_id":"list-1","source":"bulk","entries":[
		{"email":"A@Example.com","reason":"unsub"},
		{"md5_hash":"ABCDEF0123456789ABCDEF0123456789","reason":"unsub"},
		{"reason":"unsub"}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/suppressions/bulk", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.HandleBulkAddSuppressions(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"added":2`)
	require.NoError(t, mock.ExpectationsWereMet())
}
