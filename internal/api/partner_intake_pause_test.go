package api

// Intake/sending split (operator ruling, brain #3823): a SENDING pause
// (partner_datasets.paused_emergency) must never close partner intake. The
// intake door is gated on partner_datasets.intake_paused ONLY, at the three
// intake gates in this package: the partner-key middleware, the CSV ingest
// resolver, and (in internal/worker) the slicer's batch claim.
//
// sqlmock returns canned rows without evaluating SQL, so the behavioral proof
// is two-sided: the row carries the intake flag and the response follows it,
// AND a query matcher fails the test if the gate's SQL text mentions
// paused_emergency at all (or stops mentioning intake_paused).

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// intakeGateMatcher accepts only SQL that reads intake_paused and never reads
// paused_emergency. expectedSQL is unused — the contract is on the text.
func intakeGateMatcher(t *testing.T) sqlmock.QueryMatcher {
	return sqlmock.QueryMatcherFunc(func(_ string, actual string) error {
		if strings.Contains(actual, "paused_emergency") {
			t.Errorf("intake gate must not read paused_emergency (the SENDING pause):\n%s", actual)
		}
		if !strings.Contains(actual, "intake_paused") {
			t.Errorf("intake gate must read intake_paused:\n%s", actual)
		}
		return nil
	})
}

func newIntakeGateMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(intakeGateMatcher(t)),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

var partnerKeyResolveCols = []string{
	"id", "partner_id", "dataset_id", "key_prefix",
	"p_slug", "p_name",
	"d_slug", "d_name", "d_vertical",
	"d_intake_paused", "d_status", "p_status", "k_status",
}

func partnerKeyResolveRow(intakePaused bool) *sqlmock.Rows {
	return sqlmock.NewRows(partnerKeyResolveCols).AddRow(
		"00000000-0000-0000-0000-0000000key01",
		"00000000-0000-0000-0000-000000000abc",
		"00000000-0000-0000-0000-000000000d01",
		"dpk_test",
		"attribits", "Attribits",
		"attribits-heloc", "Attribits-HELOC", "refi_heloc",
		intakePaused, "active", "active", "active",
	)
}

func servePartnerKey(t *testing.T, db *sql.DB) *httptest.ResponseRecorder {
	t.Helper()
	reached := false
	h := PartnerKeyAuth(db)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/partner-ingest/v1/records", nil)
	req.Header.Set("X-Partner-Key", "dpk_intake1234567890abcdefghijklmn")
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		require.True(t, reached, "200 without reaching the wrapped handler")
	}
	return rec
}

// A dataset paused for SENDING (paused_emergency=true in prod today for all
// 62) with intake open resolves: the middleware never consults the sending
// flag, so the key reaches the handler (200 path).
func TestPartnerKeyAuth_SendingPauseDoesNotCloseIntake(t *testing.T) {
	db, mock := newIntakeGateMockDB(t)
	mock.ExpectQuery("resolvePartnerKey").WillReturnRows(partnerKeyResolveRow(false))
	mock.ExpectExec("bumpLastUsed").WillReturnResult(sqlmock.NewResult(0, 1))

	rec := servePartnerKey(t, db)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// intake_paused=true is the ONE dataset-flag refusal: 503 + code dataset_paused
// (client compatibility) with the intake-specific message.
func TestPartnerKeyAuth_IntakePausedRefuses503(t *testing.T) {
	db, mock := newIntakeGateMockDB(t)
	mock.ExpectQuery("resolvePartnerKey").WillReturnRows(partnerKeyResolveRow(true))

	rec := servePartnerKey(t, db)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"dataset_paused"`)
	require.Contains(t, rec.Body.String(), "Dataset intake is paused. Contact your account manager.")
	require.NoError(t, mock.ExpectationsWereMet())
}

// CSV ingest resolver: same split. Sending-paused + intake open => the dataset
// resolves; intake paused => 409 with the intake message.
func TestCSVResolveDataset_IntakeGate(t *testing.T) {
	csvCols := []string{"partner_id", "slug", "name", "vertical", "status", "intake_paused", "pslug", "pstatus"}
	for _, tc := range []struct {
		name         string
		intakePaused bool
		wantOK       bool
		wantStatus   int
	}{
		{"sending paused, intake open", false, true, http.StatusOK},
		{"intake paused", true, false, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newIntakeGateMockDB(t)
			mock.ExpectQuery("resolveDataset").WithArgs(csvTestDatasetID).
				WillReturnRows(sqlmock.NewRows(csvCols).
					AddRow("p-1", "test-feed", "Test Feed", "consumer", "active", tc.intakePaused, "testpartner", "active"))
			s := &PartnerCSVIngestService{db: db}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/mailing/partner-csv/preview", nil)
			_, ok := s.resolveDataset(rec, req, csvTestDatasetID)
			require.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				require.Equal(t, tc.wantStatus, rec.Code)
				require.Contains(t, rec.Body.String(), "dataset intake is paused")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// The fresh-broadcast draw is a SENDING claim: it stays on paused_emergency.
func TestFreshQueueDrawSQL_StaysOnSendingPause(t *testing.T) {
	q := freshQueueDrawSQL(false, false)
	require.Contains(t, q, "d.paused_emergency")
	require.NotContains(t, q, "intake_paused")
}

// ---------------- intake-pause / intake-resume admin endpoints ----------------

func intakeAdminRouter(db *sql.DB) http.Handler {
	h := NewPartnerAdminHandler(db)
	r := chi.NewRouter()
	r.Post("/datasets/{id}/intake-pause", h.HandleIntakePauseDataset)
	r.Post("/datasets/{id}/intake-resume", h.HandleIntakeResumeDataset)
	return r
}

func TestHandleIntakePauseDataset(t *testing.T) {
	const id = "00000000-0000-0000-0000-000000000d01"
	db, mock := newPartnerMockDB(t)
	// The UPDATE touches intake_paused(+_reason) ONLY — never the sending flag.
	mock.ExpectExec(`UPDATE partner_datasets\s+SET intake_paused = true,\s+intake_paused_reason = \$2,\s+updated_at = NOW\(\)\s+WHERE id = \$1`).
		WithArgs(id, "partner migrating feeds").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WithArgs(sqlmock.AnyArg(), "intake_pause_dataset", "partner_dataset", id, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/datasets/"+id+"/intake-pause",
		bytes.NewBufferString(`{"reason":"partner migrating feeds"}`))
	intakeAdminRouter(db).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"intake_paused":true`)
	require.NotContains(t, rec.Body.String(), "paused_emergency")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleIntakePauseDataset_NotFound(t *testing.T) {
	const id = "00000000-0000-0000-0000-000000000d04"
	db, mock := newPartnerMockDB(t)
	mock.ExpectExec(`UPDATE partner_datasets\s+SET intake_paused = true`).
		WithArgs(id, "operator intake pause").
		WillReturnResult(sqlmock.NewResult(0, 0))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/datasets/"+id+"/intake-pause", nil)
	intakeAdminRouter(db).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleIntakeResumeDataset(t *testing.T) {
	const id = "00000000-0000-0000-0000-000000000d01"
	db, mock := newPartnerMockDB(t)
	mock.ExpectExec(`UPDATE partner_datasets\s+SET intake_paused = false,\s+intake_paused_reason = NULL,\s+updated_at = NOW\(\)\s+WHERE id = \$1`).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WithArgs(sqlmock.AnyArg(), "intake_resume_dataset", "partner_dataset", id, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/datasets/"+id+"/intake-resume", nil)
	intakeAdminRouter(db).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"intake_paused":false`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleIntakePauseDataset_InvalidID(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	for _, p := range []string{"/datasets/not-a-uuid/intake-pause", "/datasets/not-a-uuid/intake-resume"} {
		rec := httptest.NewRecorder()
		intakeAdminRouter(db).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, p, nil))
		require.Equal(t, http.StatusBadRequest, rec.Code, p)
	}
}

// The roster carries both flags so the portal can render SENDING PAUSED and
// INTAKE PAUSED independently.
func TestHandleListDatasets_CarriesIntakeFlags(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	rows := sqlmock.NewRows([]string{
		"id", "partner_id", "name", "slug", "vertical", "flush_window_hours",
		"paused_emergency", "paused_reason", "status", "created_at",
		"intake_paused", "intake_paused_reason",
		"partner_name", "partner_slug", "batch_count", "ready_count", "mailed_count",
	}).AddRow(
		"00000000-0000-0000-0000-0000000abc01",
		"00000000-0000-0000-0000-000000000abc",
		"Attribits-HELOC", "attribits-heloc", "refi_heloc", 24,
		true, "sending stopped", "active", mustParseTime(t, "2026-05-12T00:00:00Z"),
		true, "partner migrating feeds",
		"Attribits", "attribits", 3, 1500, 4500,
	)
	mock.ExpectQuery(`SELECT d\.id, d\.partner_id.*COALESCE\(d\.intake_paused, false\), COALESCE\(d\.intake_paused_reason, ''\).*FROM partner_datasets d`).
		WillReturnRows(rows)
	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	h.HandleListDatasets(rec, httptest.NewRequest(http.MethodGet, "/datasets", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, `"paused_emergency":true`)
	require.Contains(t, body, `"intake_paused":true`)
	require.Contains(t, body, `"intake_paused_reason":"partner migrating feeds"`)
	require.NoError(t, mock.ExpectationsWereMet())
}
