package api

// Tests for the tracking handlers' nil-UUID rejection path.
//
// uuid.Parse returns uuid.Nil (00000000-...) when it fails to parse a
// component. Without a guard, a malformed or tampered tracking URL would
// insert rows into mailing_tracking_events keyed on zero UUIDs — collapsing
// unrelated events onto a single synthetic row and poisoning analytics.
//
// These tests lock in the guard so a regression that removes the nil-UUID
// check is caught by CI. We deliberately do NOT set up any sqlmock
// expectations: the guard must return BEFORE any DB call, and any
// accidental query would fail the test because no expectation was set.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// newTrackingTestService builds a MailingService with a sqlmock DB.
// The mock is left strict — any DB hit without a matching expectation
// will fail the test, which is exactly what we want for guard-clause
// tests (they should short-circuit before touching the DB).
func newTrackingTestService(t *testing.T) (*MailingService, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := &MailingService{
		db:        db,
		throttler: NewMailingThrottler(),
	}
	return svc, mock, db
}

// withChiURLParam returns a shallow copy of the request with a chi route
// context populated so chi.URLParam(r, key) returns the given value.
// The handlers read `data` (and optionally `sig`) via chi.URLParam.
func withChiURLParam(r *http.Request, pairs map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range pairs {
		rctx.URLParams.Add(k, v)
	}
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// encodeTrackingPayload base64-URL-encodes a pipe-separated string in the
// same format the tracking URL builder produces.
func encodeTrackingPayload(parts ...string) string {
	joined := parts[0]
	for _, p := range parts[1:] {
		joined += "|" + p
	}
	return base64.URLEncoding.EncodeToString([]byte(joined))
}

const nilUUID = "00000000-0000-0000-0000-000000000000"

func TestHandleTrackOpen_RejectsNilUUIDs_ReturnsPixel(t *testing.T) {
	svc, mock, _ := newTrackingTestService(t)

	// Four nil-UUID components — the guard must fire before any DB work.
	data := encodeTrackingPayload(nilUUID, nilUUID, nilUUID, nilUUID)

	req := httptest.NewRequest(http.MethodGet, "/track/open/"+data, nil)
	req = withChiURLParam(req, map[string]string{"data": data})
	rec := httptest.NewRecorder()

	svc.HandleTrackOpen(rec, req)

	// The serveTrackingPixel response is a 1x1 GIF with Content-Type
	// image/gif. Any non-200 or non-gif response signals the guard
	// didn't trigger the fallback path.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (pixel returned on nil-UUID rejection)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/gif" {
		t.Errorf("content-type = %q, want image/gif", ct)
	}

	// No DB query should have been issued — strict sqlmock will complain
	// if any was.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v (guard should short-circuit before DB)", err)
	}
}

func TestHandleTrackOpen_RejectsMixedNilUUIDs(t *testing.T) {
	// Only one nil component — still rejected. Also verifies a valid orgID
	// does not accidentally rescue a request with a nil campaign or
	// subscriber. Org nil is actually ALLOWED in the current guard (only
	// campaign/subscriber/emailID are checked) so we pair it with a nil
	// campaign here to exercise the reject branch.
	svc, mock, _ := newTrackingTestService(t)
	data := encodeTrackingPayload(
		"11111111-1111-1111-1111-111111111111", // orgID (valid)
		nilUUID,                                // campaignID (nil — rejected)
		"22222222-2222-2222-2222-222222222222", // subscriberID
		"33333333-3333-3333-3333-333333333333", // emailID
	)

	req := httptest.NewRequest(http.MethodGet, "/track/open/"+data, nil)
	req = withChiURLParam(req, map[string]string{"data": data})
	rec := httptest.NewRecorder()

	svc.HandleTrackOpen(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (pixel returned on nil-UUID rejection)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/gif" {
		t.Errorf("content-type = %q, want image/gif", ct)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v (guard should short-circuit before DB)", err)
	}
}

func TestHandleTrackOpen_RejectsMalformedBase64(t *testing.T) {
	// Fully-broken base64 — decoder returns an error and handler serves
	// the pixel without any DB call.
	svc, mock, _ := newTrackingTestService(t)
	req := httptest.NewRequest(http.MethodGet, "/track/open/not-valid-base64!!!", nil)
	req = withChiURLParam(req, map[string]string{"data": "not-valid-base64!!!"})
	rec := httptest.NewRecorder()

	svc.HandleTrackOpen(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestHandleTrackOpen_RejectsTooFewParts(t *testing.T) {
	// Properly base64-encoded but only 2 pipe-separated parts. The handler
	// requires 4 (org | campaign | subscriber | email) and must bail
	// before uuid.Parse — another guard that must never hit the DB.
	svc, mock, _ := newTrackingTestService(t)
	data := encodeTrackingPayload(nilUUID, nilUUID)
	req := httptest.NewRequest(http.MethodGet, "/track/open/"+data, nil)
	req = withChiURLParam(req, map[string]string{"data": data})
	rec := httptest.NewRecorder()

	svc.HandleTrackOpen(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestHandleTrackClick_RejectsNilUUIDs_WithRedirect(t *testing.T) {
	// Click tracking still redirects the user to the destination URL
	// even when the tracking payload is malformed — UX should not break
	// because someone tampered with the link. The DB write is the only
	// thing that must NOT happen.
	svc, mock, _ := newTrackingTestService(t)
	dest := "https://example.com/offer/abc"
	data := encodeTrackingPayload(nilUUID, nilUUID, nilUUID, nilUUID, dest)

	req := httptest.NewRequest(http.MethodGet, "/track/click/"+data, nil)
	req = withChiURLParam(req, map[string]string{"data": data})
	rec := httptest.NewRecorder()

	svc.HandleTrackClick(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want 302 (redirect on nil-UUID rejection with URL)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != dest {
		t.Errorf("Location = %q, want %q", loc, dest)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v (guard should short-circuit before DB)", err)
	}
}

func TestHandleTrackClick_RejectsNilUUIDs_NoURL_Returns400(t *testing.T) {
	// When no destination URL is present, there is nothing to redirect to;
	// the handler must reject with 400 instead of silently pretending
	// success.
	svc, mock, _ := newTrackingTestService(t)
	data := encodeTrackingPayload(nilUUID, nilUUID, nilUUID, nilUUID, "")

	req := httptest.NewRequest(http.MethodGet, "/track/click/"+data, nil)
	req = withChiURLParam(req, map[string]string{"data": data})
	rec := httptest.NewRecorder()

	svc.HandleTrackClick(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (nil UUID, no redirect URL)", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

func TestHandleTrackClick_RejectsMalformedBase64(t *testing.T) {
	svc, mock, _ := newTrackingTestService(t)
	req := httptest.NewRequest(http.MethodGet, "/track/click/not!valid!base64", nil)
	req = withChiURLParam(req, map[string]string{"data": "not!valid!base64"})
	rec := httptest.NewRecorder()

	svc.HandleTrackClick(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (malformed base64)", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}
