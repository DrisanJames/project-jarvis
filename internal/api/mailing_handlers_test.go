package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestService creates a MailingService with a sqlmock database.
func newTestService(t *testing.T) (*MailingService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	svc := &MailingService{
		db:        db,
		throttler: NewMailingThrottler(),
	}
	return svc, mock
}

// withOrgHeader adds the X-Organization-ID header for multi-tenant requests.
func withOrgHeader(r *http.Request, orgID uuid.UUID) *http.Request {
	r.Header.Set("X-Organization-ID", orgID.String())
	return r
}

// ---------------------------------------------------------------------------
// Throttler: TryAcquire concurrency test
// ---------------------------------------------------------------------------

func TestTryAcquire_Concurrent(t *testing.T) {
	throttler := NewMailingThrottler()
	throttler.SetLimits(100, 100000)

	var acquired int64
	var wg sync.WaitGroup

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if throttler.TryAcquire() {
				atomic.AddInt64(&acquired, 1)
			}
		}()
	}

	wg.Wait()
	assert.Equal(t, int64(100), acquired,
		"exactly minuteLimit slots should be acquired under concurrency")
}

func TestTryAcquire_WindowReset(t *testing.T) {
	throttler := &MailingThrottler{
		lastMinute:  time.Now().Add(-2 * time.Minute),
		lastHour:    time.Now().Add(-2 * time.Hour),
		minute:      999,
		hour:        999,
		minuteLimit: 10,
		hourLimit:   100,
	}

	ok := throttler.TryAcquire()
	assert.True(t, ok, "should reset expired windows and allow the send")
}

// ---------------------------------------------------------------------------
// HandleCreateCampaign validation tests
// ---------------------------------------------------------------------------

func TestCreateCampaign_InvalidJSON(t *testing.T) {
	svc, _ := newTestService(t)

	body := bytes.NewBufferString(`{invalid json`)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
	req = withOrgHeader(req, uuid.New())
	rec := httptest.NewRecorder()

	svc.HandleCreateCampaign(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Equal(t, "invalid JSON body", resp["error"])
}

func TestCreateCampaign_MissingNameSubject(t *testing.T) {
	svc, _ := newTestService(t)

	body := bytes.NewBufferString(`{"from_email":"test@example.com","from_name":"Test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
	req = withOrgHeader(req, uuid.New())
	rec := httptest.NewRecorder()

	svc.HandleCreateCampaign(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Equal(t, "name and subject are required", resp["error"])
}

func TestCreateCampaign_MissingFromFields(t *testing.T) {
	svc, _ := newTestService(t)

	body := bytes.NewBufferString(`{"name":"Test","subject":"Hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
	req = withOrgHeader(req, uuid.New())
	rec := httptest.NewRecorder()

	svc.HandleCreateCampaign(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Equal(t, "from_name and from_email are required", resp["error"])
}

func TestCreateCampaign_InvalidEmailFormat(t *testing.T) {
	svc, _ := newTestService(t)

	for _, bad := range []string{"nope", "@domain.com", "user@", "user@@domain.com", ""} {
		body := bytes.NewBufferString(fmt.Sprintf(
			`{"name":"Test","subject":"Hi","from_name":"Sender","from_email":"%s"}`, bad))
		req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
		req = withOrgHeader(req, uuid.New())
		rec := httptest.NewRecorder()

		svc.HandleCreateCampaign(rec, req)
		if bad == "" {
			assert.Equal(t, http.StatusBadRequest, rec.Code, "empty email should fail on required check")
		} else {
			assert.Equal(t, http.StatusBadRequest, rec.Code, "bad email %q should be rejected", bad)
		}
	}
}

func TestCreateCampaign_WhitespaceOnlyFields(t *testing.T) {
	svc, _ := newTestService(t)

	body := bytes.NewBufferString(`{"name":"  ","subject":"  ","from_name":"Sender","from_email":"a@b.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
	req = withOrgHeader(req, uuid.New())
	rec := httptest.NewRecorder()

	svc.HandleCreateCampaign(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateCampaign_InvalidListID(t *testing.T) {
	svc, _ := newTestService(t)

	body := bytes.NewBufferString(`{"name":"Test","subject":"Hi","from_name":"Sender","from_email":"a@b.com","list_id":"not-a-uuid"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
	req = withOrgHeader(req, uuid.New())
	rec := httptest.NewRecorder()

	svc.HandleCreateCampaign(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Equal(t, "list_id is not a valid UUID", resp["error"])
}

func TestCreateCampaign_ListNotFound(t *testing.T) {
	svc, mock := newTestService(t)

	listID := uuid.New()

	mock.ExpectQuery("SELECT organization_id FROM mailing_lists").
		WithArgs(listID).
		WillReturnError(sql.ErrNoRows)

	body := bytes.NewBufferString(fmt.Sprintf(
		`{"name":"Test","subject":"Hi","from_name":"Sender","from_email":"a@b.com","list_id":"%s"}`, listID))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
	req = withOrgHeader(req, uuid.New())
	rec := httptest.NewRecorder()

	svc.HandleCreateCampaign(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Equal(t, "list_id does not exist", resp["error"])
}

func TestCreateCampaign_ListDBError_Returns500(t *testing.T) {
	svc, mock := newTestService(t)

	listID := uuid.New()

	mock.ExpectQuery("SELECT organization_id FROM mailing_lists").
		WithArgs(listID).
		WillReturnError(fmt.Errorf("connection refused"))

	body := bytes.NewBufferString(fmt.Sprintf(
		`{"name":"Test","subject":"Hi","from_name":"Sender","from_email":"a@b.com","list_id":"%s"}`, listID))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
	req = withOrgHeader(req, uuid.New())
	rec := httptest.NewRecorder()

	svc.HandleCreateCampaign(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Equal(t, "internal_error", resp["error"])
}

func TestCreateCampaign_CrossOrgList(t *testing.T) {
	svc, mock := newTestService(t)

	orgA := uuid.New()
	orgB := uuid.New()
	listID := uuid.New()

	// The list belongs to orgB, but the request comes from orgA
	mock.ExpectQuery("SELECT organization_id FROM mailing_lists").
		WithArgs(listID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow(orgB))

	body := bytes.NewBufferString(fmt.Sprintf(
		`{"name":"Test","subject":"Hi","from_name":"Sender","from_email":"a@b.com","list_id":"%s"}`, listID))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
	req = withOrgHeader(req, orgA)
	rec := httptest.NewRecorder()

	svc.HandleCreateCampaign(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	assert.Equal(t, "list_id does not belong to your organization", resp["error"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateCampaign_HappyPath(t *testing.T) {
	svc, mock := newTestService(t)

	orgID := uuid.New()
	listID := uuid.New()

	// List ownership check passes
	mock.ExpectQuery("SELECT organization_id FROM mailing_lists").
		WithArgs(listID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow(orgID))

	// INSERT succeeds
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(1, 1))

	body := bytes.NewBufferString(fmt.Sprintf(
		`{"name":"Welcome","subject":"Hello","from_name":"Ignite","from_email":"hi@example.com","list_id":"%s"}`, listID))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/campaigns", body)
	req = withOrgHeader(req, orgID)
	rec := httptest.NewRecorder()

	svc.HandleCreateCampaign(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
	var resp CampaignCreateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "Welcome", resp.Name)
	assert.Equal(t, "draft", resp.Status)
	assert.NotEmpty(t, resp.ID)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// HandleCreateList validation tests
// ---------------------------------------------------------------------------

func TestCreateList_InvalidJSON(t *testing.T) {
	svc, _ := newTestService(t)

	body := bytes.NewBufferString(`{broken`)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/lists", body)
	req = withOrgHeader(req, uuid.New())
	rec := httptest.NewRecorder()

	svc.HandleCreateList(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateList_MissingName(t *testing.T) {
	svc, _ := newTestService(t)

	body := bytes.NewBufferString(`{"description":"some desc"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/lists", body)
	req = withOrgHeader(req, uuid.New())
	rec := httptest.NewRecorder()

	svc.HandleCreateList(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestCreateList_NoOrgFallsBackToSingleTenant covers the case where no org
// header / context / query param is supplied. As of segments fix v2.1.1
// (commit ae47a12) GetOrgIDFromRequest has a hardcoded last-resort fallback
// to SingleTenantFallbackOrgID so the segments dashboard works under
// degraded boot conditions. That same fallback applies to every handler,
// including HandleCreateList — there is no longer a "no org" failure mode.
// The handler should therefore proceed to the INSERT against the
// hardcoded org id, and on success return 201 Created.
func TestCreateList_NoOrgFallsBackToSingleTenant(t *testing.T) {
	svc, mock := newTestService(t)

	fallbackOrg, err := uuid.Parse(SingleTenantFallbackOrgID)
	require.NoError(t, err)

	mock.ExpectExec(`INSERT INTO mailing_lists`).
		WithArgs(sqlmock.AnyArg(), fallbackOrg, "My List", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	body := bytes.NewBufferString(`{"name":"My List"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/lists", body)
	// No org header — handler must still resolve via the hardcoded fallback.
	rec := httptest.NewRecorder()

	svc.HandleCreateList(rec, req)
	assert.Equal(t, http.StatusCreated, rec.Code,
		"with the single-tenant hardcoded fallback in place, no-org requests resolve to the fallback org and create the list")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// GetStatus typed response
// ---------------------------------------------------------------------------

func TestThrottlerGetStatus_TypedResponse(t *testing.T) {
	throttler := NewMailingThrottler()
	status := throttler.GetStatus()
	assert.Equal(t, 1000, status.MinuteLimit)
	assert.Equal(t, 50000, status.HourLimit)
	assert.Equal(t, 0, status.MinuteUsed)
	assert.Equal(t, 0, status.HourUsed)
}

// ---------------------------------------------------------------------------
// DefaultDailyCapacity constant
// ---------------------------------------------------------------------------

func TestDefaultDailyCapacity(t *testing.T) {
	assert.Equal(t, int64(500000), DefaultDailyCapacity)
}
