package api

// API-key lifecycle fixtures. What each test PINS (behavior):
//   - list never exposes the key hash — only id/prefix/status/timestamps;
//   - rotate is ATOMIC: the new active row is inserted and every other
//     active row revoked inside ONE transaction — a failed revoke rolls the
//     insert back (no commit), a success commits both;
//   - the raw key appears ONCE, at rotate, with the api_key_warning;
//   - revoke: 404 for an absent key, 409 for an already-revoked one (the
//     idempotency contract — a re-revoke is a no-op signalled, never a
//     rewrite of revoked_at);
//   - /verticals serves PartnerVerticals (the ONE Go source) and the public
//     schema endpoint advertises the SAME list — no more hardcoded 4.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

const keyTestDatasetID = "11111111-2222-3333-4444-555555555555"
const keyTestKeyID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func keyTestRequest(method, path, urlParamName, urlParamVal string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(urlParamName, urlParamVal)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestHandleListDatasetKeys_NeverExposesHash(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT id, COALESCE\(key_prefix, ''\), COALESCE\(status, 'active'\)`).
		WithArgs(keyTestDatasetID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "key_prefix", "status", "last_used_at", "created_at", "revoked_at"}).
			AddRow("k1", "dpk_abcd", "active", now, now, nil).
			AddRow("k2", "dpk_wxyz", "revoked", nil, now.Add(-time.Hour), now))

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	h.HandleListDatasetKeys(rec, keyTestRequest(http.MethodGet,
		"/api/mailing/data-partners/datasets/"+keyTestDatasetID+"/keys", "id", keyTestDatasetID))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, `"key_prefix":"dpk_abcd"`)
	require.Contains(t, body, `"status":"revoked"`)
	require.NotContains(t, body, "key_hash", "the hash must never leave the DB")
	require.NoError(t, mock.ExpectationsWereMet())
}

// Rotate atomicity, happy path: insert-new + revoke-others + COMMIT, raw key
// shown once with the warning.
func TestHandleRotateDatasetKey_Atomic(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectQuery(`SELECT partner_id FROM partner_datasets`).
		WithArgs(keyTestDatasetID).
		WillReturnRows(sqlmock.NewRows([]string{"partner_id"}).AddRow("p-1"))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO partner_api_keys`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE partner_api_keys\s+SET status = 'revoked'`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	h.HandleRotateDatasetKey(rec, keyTestRequest(http.MethodPost,
		"/api/mailing/data-partners/datasets/"+keyTestDatasetID+"/rotate-key", "id", keyTestDatasetID))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	rawKey, _ := resp["api_key"].(string)
	require.True(t, strings.HasPrefix(rawKey, "dpk_"), "raw key returned once at mint")
	require.EqualValues(t, 2, resp["revoked_previous"])
	require.Contains(t, resp["api_key_warning"], "ONCE")
	require.NoError(t, mock.ExpectationsWereMet())
}

// Rotate atomicity, failure path: the revoke UPDATE fails → ROLLBACK, no
// commit — the old key is revoked IFF the new key landed, and vice versa.
func TestHandleRotateDatasetKey_RevokeFailureRollsBackInsert(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectQuery(`SELECT partner_id FROM partner_datasets`).
		WithArgs(keyTestDatasetID).
		WillReturnRows(sqlmock.NewRows([]string{"partner_id"}).AddRow("p-1"))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO partner_api_keys`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE partner_api_keys\s+SET status = 'revoked'`).
		WillReturnError(fmt.Errorf("boom"))
	mock.ExpectRollback()

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	h.HandleRotateDatasetKey(rec, keyTestRequest(http.MethodPost,
		"/api/mailing/data-partners/datasets/"+keyTestDatasetID+"/rotate-key", "id", keyTestDatasetID))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), `"api_key":"dpk_`, "no raw key on a failed rotate")
	require.NoError(t, mock.ExpectationsWereMet(), "insert must be rolled back, never committed")
}

func TestHandleRotateDatasetKey_DatasetNotFound(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectQuery(`SELECT partner_id FROM partner_datasets`).
		WithArgs(keyTestDatasetID).
		WillReturnError(sql.ErrNoRows)

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	h.HandleRotateDatasetKey(rec, keyTestRequest(http.MethodPost,
		"/api/mailing/data-partners/datasets/"+keyTestDatasetID+"/rotate-key", "id", keyTestDatasetID))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleRevokeKey_HappyThenIdempotent409(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.MatchExpectationsInOrder(true)
	now := time.Now().UTC()

	// First revoke flips the row.
	mock.ExpectQuery(`UPDATE partner_api_keys\s+SET status = 'revoked'.*RETURNING`).
		WithArgs(keyTestKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"dataset_id", "key_prefix", "revoked_at"}).
			AddRow(keyTestDatasetID, "dpk_abcd", now))
	// (audit insert is best-effort; unordered/unexpected calls are swallowed
	// by writeAuditLog's discarded error — expect it explicitly for order.)
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Second revoke: no active row flips → status lookup says revoked → 409.
	mock.ExpectQuery(`UPDATE partner_api_keys\s+SET status = 'revoked'.*RETURNING`).
		WithArgs(keyTestKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"dataset_id", "key_prefix", "revoked_at"}))
	mock.ExpectQuery(`SELECT COALESCE\(status, 'active'\) FROM partner_api_keys`).
		WithArgs(keyTestKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("revoked"))

	h := NewPartnerAdminHandler(db)

	rec := httptest.NewRecorder()
	h.HandleRevokeKey(rec, keyTestRequest(http.MethodPost,
		"/api/mailing/data-partners/keys/"+keyTestKeyID+"/revoke", "keyId", keyTestKeyID))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"status":"revoked"`)

	rec2 := httptest.NewRecorder()
	h.HandleRevokeKey(rec2, keyTestRequest(http.MethodPost,
		"/api/mailing/data-partners/keys/"+keyTestKeyID+"/revoke", "keyId", keyTestKeyID))
	require.Equal(t, http.StatusConflict, rec2.Code, rec2.Body.String())
	require.Contains(t, rec2.Body.String(), "already revoked")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleRevokeKey_NotFound(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectQuery(`UPDATE partner_api_keys\s+SET status = 'revoked'.*RETURNING`).
		WithArgs(keyTestKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"dataset_id", "key_prefix", "revoked_at"}))
	mock.ExpectQuery(`SELECT COALESCE\(status, 'active'\) FROM partner_api_keys`).
		WithArgs(keyTestKeyID).
		WillReturnError(sql.ErrNoRows)

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	h.HandleRevokeKey(rec, keyTestRequest(http.MethodPost,
		"/api/mailing/data-partners/keys/"+keyTestKeyID+"/revoke", "keyId", keyTestKeyID))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// /verticals and the public schema endpoint must both serve PartnerVerticals.
func TestVerticalsSingleSource(t *testing.T) {
	require.Equal(t, len(validVerticals), len(PartnerVerticals), "map must be derived from the slice")
	for _, v := range PartnerVerticals {
		require.True(t, isValidVertical(v), v)
	}

	db, _ := newPartnerMockDB(t)
	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	h.HandleListVerticals(rec, httptest.NewRequest(http.MethodGet, "/api/mailing/data-partners/verticals", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Verticals []string `json:"verticals"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, PartnerVerticals, resp.Verticals)

	// The public schema advertises the same list (was hardcoded to 4).
	ih := NewPartnerIngestHandler(db, nil)
	rec2 := httptest.NewRecorder()
	ih.HandleGetSchema(rec2, httptest.NewRequest(http.MethodGet, "/api/partner-ingest/v1/schema", nil))
	require.Equal(t, http.StatusOK, rec2.Code)
	var schema struct {
		Verticals []string `json:"verticals_supported"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &schema))
	require.Equal(t, PartnerVerticals, schema.Verticals)
}
