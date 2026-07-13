package api

// REQ-003 — GlobalSuppressionHub boot load is no longer log-and-continue.
//
// Before this fix, a failed boot LoadFromDB left the hub with an EMPTY
// historical suppression set until the next deploy or a chance bulk import
// (suppression_import.go:595 was the only reload path). These tests pin:
//
//   1. LoadFromDBWithRetry — bounded retry that recovers from a transient
//      first failure, stops at the attempt cap, and honors ctx cancellation.
//   2. ReconcileNow — the periodic hub-vs-DB divergence alarm: a gap beyond
//      tolerance logs at error level, reloads the hub, and flags Diverged
//      so the dashboard chip degrades.
//   3. GET /api/mailing/suppressions/global exposes the reconcile verdict.
//
// Tests live in internal/api (the boot-wiring package) per the REQ-003 DoD
// but exercise the engine hub directly with sqlmock.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/stretchr/testify/require"
)

const hubTestOrgID = "00000000-0000-0000-0000-000000000001"

func newHubForTest(t *testing.T) (*engine.GlobalSuppressionHub, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return engine.NewGlobalSuppressionHub(db, hubTestOrgID, ""), mock
}

const hubGlobalLoadQuery = `SELECT email, md5_hash FROM mailing_global_suppressions WHERE organization_id = \$1`
const hubCountQuery = `SELECT COUNT\(\*\) FROM mailing_global_suppressions WHERE organization_id = \$1`

func TestHubLoadRetry_FirstFailThenSuccess(t *testing.T) {
	hub, mock := newHubForTest(t)

	// Attempt 1 fails (the "one slow boot" case)…
	mock.ExpectQuery(hubGlobalLoadQuery).
		WithArgs(hubTestOrgID).
		WillReturnError(errors.New("canceling statement due to statement timeout"))
	// …attempt 2 succeeds. (The brand-table read that follows is
	// deliberately unmocked — LoadFromDB tolerates its absence by design.)
	mock.ExpectQuery(hubGlobalLoadQuery).
		WithArgs(hubTestOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"email", "md5_hash"}).
			AddRow("a@example.com", engine.MD5Hash("a@example.com")).
			AddRow("b@example.com", engine.MD5Hash("b@example.com")))

	err := hub.LoadFromDBWithRetry(context.Background(), 3, time.Millisecond, time.Second)
	require.NoError(t, err, "a transient first failure must be recovered by the bounded retry")
	require.Equal(t, 2, hub.Count(), "hub must hold the entries loaded on the successful attempt")
	require.True(t, hub.IsSuppressed("a@example.com"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHubLoadRetry_ExhaustsBoundedAttempts(t *testing.T) {
	hub, mock := newHubForTest(t)

	// Exactly 2 attempts — never more (bounded).
	mock.ExpectQuery(hubGlobalLoadQuery).WithArgs(hubTestOrgID).WillReturnError(errors.New("down"))
	mock.ExpectQuery(hubGlobalLoadQuery).WithArgs(hubTestOrgID).WillReturnError(errors.New("still down"))

	err := hub.LoadFromDBWithRetry(context.Background(), 2, time.Millisecond, time.Second)
	require.Error(t, err, "exhausted retry must surface the failure, never silently succeed")
	require.NoError(t, mock.ExpectationsWereMet(), "retry must stop at the attempt cap")
}

func TestHubLoadRetry_HonorsContextCancellation(t *testing.T) {
	hub, _ := newHubForTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — no attempt may run

	err := hub.LoadFromDBWithRetry(ctx, 5, time.Hour, time.Second)
	require.ErrorIs(t, err, context.Canceled)
}

func TestHubReconcile_DivergenceReloadsAndFlags(t *testing.T) {
	hub, mock := newHubForTest(t) // in-memory count = 0 (the failed-boot state)

	// DB holds 5000 rows — gap 5000 > tolerance max(1000, 1%) → diverged.
	mock.ExpectQuery(hubCountQuery).
		WithArgs(hubTestOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5000))
	// The reconciler must immediately reload.
	mock.ExpectQuery(hubGlobalLoadQuery).
		WithArgs(hubTestOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"email", "md5_hash"}).
			AddRow("a@example.com", engine.MD5Hash("a@example.com")).
			AddRow("b@example.com", engine.MD5Hash("b@example.com")).
			AddRow("c@example.com", engine.MD5Hash("c@example.com")))

	status := hub.ReconcileNow(context.Background())
	require.True(t, status.Diverged, "a hub missing DB entries beyond tolerance must flag Diverged")
	require.Equal(t, int64(5000), status.DBCount)
	require.Equal(t, 3, status.MemoryCount, "status must reflect the post-reload memory count")
	require.Equal(t, 3, hub.Count(), "reconcile must have reloaded the hub")
	require.True(t, hub.ReconcileStatus().Diverged, "verdict must persist for the dashboard chip")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHubReconcile_WithinToleranceDoesNotReload(t *testing.T) {
	hub, mock := newHubForTest(t)

	// Gap of 500 is inside the 1000-row floor — no reload expected.
	mock.ExpectQuery(hubCountQuery).
		WithArgs(hubTestOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(500))

	status := hub.ReconcileNow(context.Background())
	require.False(t, status.Diverged)
	require.NoError(t, mock.ExpectationsWereMet(), "no reload query may run when within tolerance")
}

func TestGlobalSuppressionEndpoint_IncludesReconcileStatus(t *testing.T) {
	hub, mock := newHubForTest(t)

	// Seed a diverged verdict.
	mock.ExpectQuery(hubCountQuery).
		WithArgs(hubTestOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5000))
	mock.ExpectQuery(hubGlobalLoadQuery).
		WithArgs(hubTestOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"email", "md5_hash"}))
	hub.ReconcileNow(context.Background())

	svc := &SuppressionService{globalHub: hub}
	req := httptest.NewRequest(http.MethodGet, "/api/mailing/suppressions/global", nil)
	rec := httptest.NewRecorder()
	svc.HandleGetGlobalSuppression(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"reconcile"`)
	require.Contains(t, rec.Body.String(), `"diverged":true`,
		"the endpoint must expose the divergence verdict so the dashboard chip can degrade")
}
