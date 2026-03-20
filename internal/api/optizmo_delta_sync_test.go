package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestDeltaSyncWorker(t *testing.T) (*OptizmoDeltaSyncWorker, sqlmock.Sqlmock) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	w := NewOptizmoDeltaSyncWorker(db)
	return w, mock
}

// --- extractScrubMAK ---

func TestExtractScrubMAK_QueryParam(t *testing.T) {
	mak := extractScrubMAK("https://app.optizmo.com/access/campaigns?mak=m-abc-123-xyz")
	assert.Equal(t, "m-abc-123-xyz", mak)
}

func TestExtractScrubMAK_PathSegment(t *testing.T) {
	mak := extractScrubMAK("https://www.affiliateaccesskey.com/m-abc-123-xyz")
	assert.Equal(t, "m-abc-123-xyz", mak)
}

func TestExtractScrubMAK_Empty(t *testing.T) {
	mak := extractScrubMAK("https://example.com/nothing")
	assert.Empty(t, mak)
}

// --- isHexMD5 ---

func TestIsHexMD5_Valid(t *testing.T) {
	assert.True(t, isHexMD5("d41d8cd98f00b204e9800998ecf8427e"))
}

func TestIsHexMD5_TooShort(t *testing.T) {
	assert.False(t, isHexMD5("d41d8cd9"))
}

func TestIsHexMD5_UpperCase(t *testing.T) {
	assert.False(t, isHexMD5("D41D8CD98F00B204E9800998ECF8427E"))
}

// --- extractEmail ---

func TestExtractEmail_Plain(t *testing.T) {
	assert.Equal(t, "user@example.com", extractEmail("user@example.com"))
}

func TestExtractEmail_CSV(t *testing.T) {
	assert.Equal(t, "user@example.com", extractEmail("user@example.com,other,data"))
}

func TestExtractEmail_Quoted(t *testing.T) {
	assert.Equal(t, "user@example.com", extractEmail("\"user@example.com\""))
}

// --- matchAndSuppressSubscribers ---

func TestMatchAndSuppressSubscribers_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	emailHash := md5Hash("test@example.com")
	hashes := map[string]bool{emailHash: true}

	mock.ExpectQuery(`SELECT id, LOWER`).
		WithArgs(defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("sub-1", "test@example.com").
			AddRow("sub-2", "other@example.com"))

	mock.ExpectExec(`INSERT INTO mailing_offer_suppressions`).
		WithArgs(defaultOrgID, "offer-1", "sub-1", emailHash).
		WillReturnResult(sqlmock.NewResult(0, 1))

	audience, suppressed, matches, matchErr := matchAndSuppressSubscribers(context.Background(), db, "offer-1", hashes)
	require.NoError(t, matchErr)
	assert.Equal(t, 2, audience)
	assert.Equal(t, 1, suppressed)
	assert.Len(t, matches, 1)
	assert.Equal(t, "sub-1", matches[0].ID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestMatchAndSuppressSubscribers_NoMatches(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	hashes := map[string]bool{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1": true}

	mock.ExpectQuery(`SELECT id, LOWER`).
		WithArgs(defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("sub-1", "test@example.com"))

	audience, suppressed, matches, matchErr := matchAndSuppressSubscribers(context.Background(), db, "offer-1", hashes)
	require.NoError(t, matchErr)
	assert.Equal(t, 1, audience)
	assert.Equal(t, 0, suppressed)
	assert.Len(t, matches, 0)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// --- HandleToggleSync ---

func TestHandleToggleSync_Enable(t *testing.T) {
	w, mock := newTestDeltaSyncWorker(t)

	mock.ExpectQuery(`SELECT optizmo_link FROM mailing_offers`).
		WithArgs("offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"optizmo_link"}).
			AddRow("https://app.optizmo.com/access/campaigns?mak=m-test"))

	mock.ExpectExec(`UPDATE mailing_offers SET suppression_sync_enabled`).
		WithArgs(true, "offer-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := bytes.NewBufferString(`{"enabled":true}`)
	r := httptest.NewRequest(http.MethodPost, "/offer-center/offers/offer-1/optizmo/toggle-sync", body)
	r.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "offer-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	rr := httptest.NewRecorder()
	w.HandleToggleSync(rr, r)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["suppression_sync_enabled"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleToggleSync_Enable_NoOptizmoLink(t *testing.T) {
	w, mock := newTestDeltaSyncWorker(t)

	mock.ExpectQuery(`SELECT optizmo_link FROM mailing_offers`).
		WithArgs("offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"optizmo_link"}).
			AddRow(""))

	body := bytes.NewBufferString(`{"enabled":true}`)
	r := httptest.NewRequest(http.MethodPost, "/offer-center/offers/offer-1/optizmo/toggle-sync", body)
	r.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "offer-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	rr := httptest.NewRecorder()
	w.HandleToggleSync(rr, r)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "no Optizmo link")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleToggleSync_Disable(t *testing.T) {
	w, mock := newTestDeltaSyncWorker(t)

	mock.ExpectExec(`UPDATE mailing_offers SET suppression_sync_enabled`).
		WithArgs(false, "offer-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := bytes.NewBufferString(`{"enabled":false}`)
	r := httptest.NewRequest(http.MethodPost, "/offer-center/offers/offer-1/optizmo/toggle-sync", body)
	r.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "offer-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	rr := httptest.NewRecorder()
	w.HandleToggleSync(rr, r)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// --- HandleManualSync: dedup guard ---

func TestHandleManualSync_RejectsConcurrentSync(t *testing.T) {
	w, mock := newTestDeltaSyncWorker(t)

	// Preload an active sync for offer-1
	w.activeSyncs.Store("offer-1", struct{}{})

	mock.ExpectQuery(`SELECT COALESCE\(name`).
		WithArgs("offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"name", "optizmo_link"}).
			AddRow("Test Offer", "https://app.optizmo.com?mak=m-test"))

	r := httptest.NewRequest(http.MethodPost, "/offer-center/offers/offer-1/optizmo/trigger-sync", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "offer-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	rr := httptest.NewRecorder()
	w.HandleManualSync(rr, r)

	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Contains(t, rr.Body.String(), "already in progress")

	// Clean up
	w.activeSyncs.Delete("offer-1")
}

// --- HandleSyncStatus ---

func TestHandleSyncStatus_ReturnsFields(t *testing.T) {
	w, mock := newTestDeltaSyncWorker(t)

	mock.ExpectQuery(`SELECT COALESCE\(suppression_sync_enabled`).
		WithArgs("offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"sync_enabled", "last_sync_at", "last_sync_error"}).
			AddRow(true, nil, ""))

	mock.ExpectQuery(`SELECT id, status`).
		WithArgs("offer-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "status", "file_count", "audience_count", "suppressed_count",
			"error_message", "requested_at", "completed_at",
		}))

	r := httptest.NewRequest(http.MethodGet, "/offer-center/offers/offer-1/optizmo/sync-status", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "offer-1")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	rr := httptest.NewRecorder()
	w.HandleSyncStatus(rr, r)

	assert.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["sync_enabled"])
	assert.Contains(t, resp, "next_sync_window")
	assert.Contains(t, resp, "recent_sync_jobs")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// --- Worker lifecycle ---

func TestWorkerStartStop(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	w := NewOptizmoDeltaSyncWorker(db)
	w.Start()
	assert.True(t, w.running)

	w.Stop()
	assert.False(t, w.running)

	// Double-stop should not panic
	w.Stop()
}

func TestWorkerStartIdempotent(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	w := NewOptizmoDeltaSyncWorker(db)
	w.Start()
	w.Start() // should be a no-op
	assert.True(t, w.running)
	w.Stop()
}
