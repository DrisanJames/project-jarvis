package api

// Tests for the Smart Link Gateway handlers (smartlink_handlers.go).
//
// Pattern matches the rest of internal/api: go-sqlmock with the regex query
// matcher + httptest for request/response; chi URL params injected via
// chi.RouteCtxKey. sqlmock fails on unexpected queries, so "no DB call"
// assertions are implicit in the absence of an expectation plus the final
// ExpectationsWereMet call.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// smartLinkCols mirrors the SELECT projection used by HandlePublicResolve /
// HandleAdminList / the update fetch. Keep in lockstep with the SELECT lists
// in smartlink_handlers.go.
var smartLinkCols = []string{
	"id", "brand_root", "slug", "review_slug", "offer_url_template",
	"status", "risk_profile", "notes", "hash", "created_at", "updated_at",
}

func newSmartLinkService(t *testing.T) (*SmartLinkService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewSmartLinkService(db), mock
}

// ----- validateSmartLinkPayload --------------------------------------------

func TestValidateSmartLinkPayload_RiskProfiles(t *testing.T) {
	const (
		root  = "discountblog.com"
		tmpl  = "https://cratoolpro.example/offer?source_id=email"
		sslug = "auto-refi"
	)

	// Valid: empty (defaults to low at insert), low, high.
	for _, rp := range []string{"", "low", "high"} {
		assert.NoError(t,
			validateSmartLinkPayload(root, sslug, sslug, tmpl, "active", rp),
			"risk_profile %q should be accepted", rp)
	}

	// Rejected: anything outside the whitelist, with a clear error.
	err := validateSmartLinkPayload(root, sslug, sslug, tmpl, "active", "medium")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "risk_profile must be low or high")
	assert.Contains(t, err.Error(), "medium")
}

func TestValidateSmartLinkPayload_RejectsBadSlug(t *testing.T) {
	err := validateSmartLinkPayload(
		"discountblog.com", "Bad/Slug", "auto-refi",
		"https://x.example/t", "active", "low")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "slug must match")
}

func TestValidateSmartLinkPayload_RejectsBadStatus(t *testing.T) {
	err := validateSmartLinkPayload(
		"discountblog.com", "auto-refi", "auto-refi",
		"https://x.example/t", "archived", "low")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status must be active, paused, or deleted")
}

// ----- HandleHit ------------------------------------------------------------

func hitPOST(t *testing.T, svc *SmartLinkService, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/smartlinks/hit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	svc.HandleHit(rec, req)
	return rec
}

func TestHandleHit_RejectsBranchOutsideWhitelist(t *testing.T) {
	svc, mock := newSmartLinkService(t)

	// Negative path: the widened whitelist is {bot, human, human_bridge,
	// human_bridge_continue}; anything else must 400 BEFORE any DB write
	// (no ExpectExec registered — sqlmock would fail if the INSERT ran).
	rec := hitPOST(t, svc, `{"brand_root":"discountblog.com","slug":"auto-refi","branch":"scanner"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body["error"], "branch must be one of")

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleHit_RejectsBadRiskProfile(t *testing.T) {
	svc, mock := newSmartLinkService(t)

	rec := hitPOST(t, svc, `{"brand_root":"discountblog.com","slug":"auto-refi","branch":"human","risk_profile":"medium"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body["error"], "risk_profile")

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleHit_AcceptsHumanBridge(t *testing.T) {
	svc, mock := newSmartLinkService(t)

	mock.ExpectExec(`INSERT INTO mailing_smart_link_hits`).
		WithArgs(
			"discountblog.com", "auto-refi", "human_bridge",
			"", "",
			nil, nil,
			"none", "", "",
			"", "", "",
			"high",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := hitPOST(t, svc, `{"brand_root":"discountblog.com","slug":"auto-refi","branch":"human_bridge","rule_fired":"none","risk_profile":"high"}`)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.JSONEq(t, `{"ok":true}`, rec.Body.String())

	assert.NoError(t, mock.ExpectationsWereMet())
}

// ----- HandlePublicResolve --------------------------------------------------

func resolveGET(t *testing.T, svc *SmartLinkService, brandRoot, slug string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/smartlinks/public/"+brandRoot+"/"+slug, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("brand_root", brandRoot)
	rctx.URLParams.Add("slug", slug)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	svc.HandlePublicResolve(rec, req)
	return rec
}

func TestHandlePublicResolve_NotFoundShape(t *testing.T) {
	svc, mock := newSmartLinkService(t)

	mock.ExpectQuery(`SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE\(risk_profile,'low'\), COALESCE\(notes,''\)`).
		WithArgs("discountblog.com", "missing-slug").
		WillReturnRows(sqlmock.NewRows(smartLinkCols)) // zero rows -> sql.ErrNoRows

	rec := resolveGET(t, svc, "discountblog.com", "missing-slug")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":"not_found"}`, rec.Body.String())

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHandlePublicResolve_SuccessIncludesRiskProfile(t *testing.T) {
	svc, mock := newSmartLinkService(t)

	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE\(risk_profile,'low'\), COALESCE\(notes,''\)`).
		WithArgs("discountblog.com", "auto-refi").
		WillReturnRows(sqlmock.NewRows(smartLinkCols).AddRow(
			"11111111-2222-3333-4444-555555555555",
			"discountblog.com", "auto-refi", "best-auto-refi",
			"https://cratoolpro.example/offer?source_id=email",
			"active", "high", "", "abc1234567", now, now,
		))

	rec := resolveGET(t, svc, "discountblog.com", "auto-refi")
	assert.Equal(t, http.StatusOK, rec.Code)

	var sl SmartLink
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sl))
	assert.Equal(t, "high", sl.RiskProfile,
		"risk_profile must round-trip to the brand site — it drives the 302-vs-bridge decision")
	assert.Equal(t, "best-auto-refi", sl.ReviewSlug)

	// The raw body must literally carry the field (guards against a future
	// json:"-" / omitempty regression hiding it from the consumer).
	assert.Contains(t, rec.Body.String(), `"risk_profile":"high"`)

	assert.NoError(t, mock.ExpectationsWereMet())
}
