package api

// Reservoir handler fixtures. What these PIN:
//   - the scan runs inside a transaction that RAISES statement_timeout: the
//     primary pool's DSN pins 30s (cmd/server/main.go:226) and this scan costs
//     8.4-9.2s against prod, so a busy moment answered 500 on 2026-09-16
//     instead of returning totals;
//   - the single grouped scan is shaped into buckets correctly, and an UNKNOWN
//     status is counted in the total and in by_status but is never folded into
//     mailable/reserve/exhausted (silently bucketing an unrecognised status
//     would overstate what can send);
//   - the 60s cache serves the next call WITHOUT a second scan (per process —
//     prod runs 2 tasks, so a caller can still land on a cold one);
//   - ?refresh=1 bypasses it and scans again;
//   - a query failure surfaces as 500, never as an empty reservoir (a
//     zero-looking reservoir reads as "we hold nothing").

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

var errReservoirTest = errors.New("scan failed")

func reservoirRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"vertical", "status", "isp_family", "n"}).
		AddRow("refi_heloc", "ready", "gmail", 374377).
		AddRow("refi_heloc", "ready", "microsoft", 110348).
		AddRow("refi_heloc", "held", "yahoo", 1271065).
		AddRow("refi_heloc", "mailed", "microsoft", 439792).
		AddRow("term_life", "pending_eo", "apple", 500).
		AddRow("term_life", "quarantined_by_a_future_release", "apple", 7)
}

// expectReservoirScan queues one full scan: tx → raised timeout → grouped
// query → rollback. The SET LOCAL is matched explicitly because it is the
// whole defence against the pool's 30s cap.
func expectReservoirScan(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '90s'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).WillReturnRows(rows)
	mock.ExpectRollback()
}

func expectReservoirScanFails(mock sqlmock.Sqlmock, err error) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '90s'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).WillReturnError(err)
	mock.ExpectRollback()
}

func decodeReservoir(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out
}

// The scan must raise its own statement_timeout. Without the SET LOCAL the
// expectation below goes unmet and this test fails — which is the point: the
// 30s pool default is what broke the endpoint in prod.
func TestReservoir_RaisesStatementTimeoutInsideTx(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	expectReservoirScan(mock, reservoirRows())

	h := NewPartnerReservoirHandler(db)
	rec := httptest.NewRecorder()
	h.HandleGetReservoir(rec, httptest.NewRequest(http.MethodGet, "/reservoir", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReservoir_BucketsAndUnknownStatus(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	expectReservoirScan(mock, reservoirRows())

	h := NewPartnerReservoirHandler(db)
	rec := httptest.NewRecorder()
	h.HandleGetReservoir(rec, httptest.NewRequest(http.MethodGet, "/reservoir", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	out := decodeReservoir(t, rec)
	// 374377+110348+1271065+439792+500+7
	require.EqualValues(t, 2196089, out["total"])
	// ready only — the unknown status and pending_eo must NOT count as mailable.
	require.EqualValues(t, 484725, out["mailable_total"])

	byVertical, ok := out["by_vertical"].([]interface{})
	require.True(t, ok)
	require.Len(t, byVertical, 2)
	// Mailable-first ordering: refi_heloc leads.
	first := byVertical[0].(map[string]interface{})
	require.Equal(t, "refi_heloc", first["vertical"])
	require.EqualValues(t, 484725, first["mailable"])
	require.EqualValues(t, 1271065, first["reserve"])
	require.EqualValues(t, 439792, first["exhausted"])

	// The unknown status is visible and counted, but bucketed nowhere.
	second := byVertical[1].(map[string]interface{})
	require.Equal(t, "term_life", second["vertical"])
	require.EqualValues(t, 507, second["total"])
	require.EqualValues(t, 500, second["reserve"])
	require.EqualValues(t, 0, second["mailable"])
	require.EqualValues(t, 0, second["exhausted"])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReservoir_SecondCallServedFromCache(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	// Exactly ONE scan is queued. A second scan fails at the call site AND at
	// ExpectationsWereMet.
	expectReservoirScan(mock, reservoirRows())

	h := NewPartnerReservoirHandler(db)
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.HandleGetReservoir(rec, httptest.NewRequest(http.MethodGet, "/reservoir", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.EqualValues(t, 2196089, decodeReservoir(t, rec)["total"])
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReservoir_RefreshBypassesCache(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	expectReservoirScan(mock, reservoirRows())
	expectReservoirScan(mock, sqlmock.NewRows([]string{"vertical", "status", "isp_family", "n"}).
		AddRow("refi_heloc", "ready", "gmail", 1))

	h := NewPartnerReservoirHandler(db)
	rec := httptest.NewRecorder()
	h.HandleGetReservoir(rec, httptest.NewRequest(http.MethodGet, "/reservoir", nil))
	require.EqualValues(t, 2196089, decodeReservoir(t, rec)["total"])

	rec2 := httptest.NewRecorder()
	h.HandleGetReservoir(rec2, httptest.NewRequest(http.MethodGet, "/reservoir?refresh=1", nil))
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.EqualValues(t, 1, decodeReservoir(t, rec2)["total"])

	require.NoError(t, mock.ExpectationsWereMet())
}

// Single-flight: N callers arriving during one in-progress scan share it.
// ONE scan is queued for 8 concurrent callers — a second would fail both at
// the call site and at ExpectationsWereMet. Two overlapping 9-16s scans of a
// 15M-row table is exactly the load this endpoint must never generate, and
// nothing exercised this path until now.
func TestReservoir_ConcurrentCallersShareOneScan(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '90s'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WillDelayFor(150 * time.Millisecond).
		WillReturnRows(reservoirRows())
	mock.ExpectRollback()

	h := NewPartnerReservoirHandler(db)
	const callers = 8
	var wg sync.WaitGroup
	codes := make([]int, callers)
	totals := make([]interface{}, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.HandleGetReservoir(rec, httptest.NewRequest(http.MethodGet, "/reservoir", nil))
			codes[i] = rec.Code
			var out map[string]interface{}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			totals[i] = out["total"]
		}(i)
	}
	wg.Wait()

	for i := 0; i < callers; i++ {
		require.Equal(t, http.StatusOK, codes[i], "caller %d", i)
		require.EqualValues(t, 2196089, totals[i], "caller %d got a different answer", i)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

// Only refresh=1 forces a rescan. Anything else — a typo, refresh=0, an
// unrelated query param — must be served from cache, or a crawler with a
// stray query string could drive a 9-16s scan per request.
func TestReservoir_OnlyRefreshEquals1ForcesAScan(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	expectReservoirScan(mock, reservoirRows())

	h := NewPartnerReservoirHandler(db)
	rec := httptest.NewRecorder()
	h.HandleGetReservoir(rec, httptest.NewRequest(http.MethodGet, "/reservoir", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, q := range []string{"?refresh=banana", "?refresh=0", "?refresh=", "?refresh=true", "?vertical=refi_heloc"} {
		rec2 := httptest.NewRecorder()
		h.HandleGetReservoir(rec2, httptest.NewRequest(http.MethodGet, "/reservoir"+q, nil))
		require.Equal(t, http.StatusOK, rec2.Code, q)
		require.EqualValues(t, 2196089, decodeReservoir(t, rec2)["total"], q)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReservoir_QueryFailureIs500NotEmpty(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	expectReservoirScanFails(mock, errReservoirTest)

	h := NewPartnerReservoirHandler(db)
	rec := httptest.NewRecorder()
	h.HandleGetReservoir(rec, httptest.NewRequest(http.MethodGet, "/reservoir", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "reservoir_query_failed")
	require.NotContains(t, rec.Body.String(), `"total":0`)
	require.NoError(t, mock.ExpectationsWereMet())
}
