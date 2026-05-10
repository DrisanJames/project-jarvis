package api

// Tests for HandleWaveProcessorStatus (SA-7, per-domain engagement engine).
//
// Coverage:
//   1. Success — sqlmock returns 3 domains, handler returns 200 with the
//      expected JSON shape and merges in-memory throughput.
//   2. DBError — sqlmock returns an error, handler returns 500 with stable
//      JSON-shaped error body.
//   3. EmptyResult — no rows, handler returns 200 with empty maps so the
//      JSON shape stays consistent for downstream consumers.
//
// The DB is a sqlmock-backed *sql.DB; the throughput provider is a tiny
// in-test stub that lets us assert the merge logic without standing up a
// real SendWorkerPool.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// stubThroughput implements ThroughputProvider for unit tests.
type stubThroughput struct {
	data map[string]int
}

func (s stubThroughput) Throughput() map[string]int { return s.data }

func TestHandleWaveProcessorStatus_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{
		"sending_domain", "waves_due", "waves_overdue_5m",
		"max_due_age_seconds", "last_completed_at",
	}).
		AddRow("em.discountblog.com", 12, 3, 420.5, now.Add(-30*time.Second)).
		AddRow("em.historythinking.com", 7, 0, 12.0, now.Add(-2*time.Minute)).
		AddRow("em.myownhealth.net", 0, 0, 0.0, nil)

	mock.ExpectQuery("FROM mailing_campaign_waves").WillReturnRows(rows)

	provider := stubThroughput{data: map[string]int{
		"em.discountblog.com":    400,
		"em.historythinking.com": 250,
		"em.myownhealth.net":     0,
	}}

	h := &WaveProcessorStatusHandler{DB: db, ThroughputProvider: provider}

	req := httptest.NewRequest(http.MethodGet, "/api/wave-processor/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got WaveProcessorStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}

	if got.WaveQueueDepthByDomain["em.discountblog.com"] != 12 {
		t.Errorf("queue depth discountblog: got %d, want 12", got.WaveQueueDepthByDomain["em.discountblog.com"])
	}
	if got.WaveOverdue5mByDomain["em.discountblog.com"] != 3 {
		t.Errorf("overdue discountblog: got %d, want 3", got.WaveOverdue5mByDomain["em.discountblog.com"])
	}
	if got.MaxWaveDelaySecondsByDomain["em.discountblog.com"] != 420.5 {
		t.Errorf("max delay discountblog: got %v, want 420.5", got.MaxWaveDelaySecondsByDomain["em.discountblog.com"])
	}
	if got.SendsLast60sByDomain["em.discountblog.com"] != 400 {
		t.Errorf("sends discountblog: got %d, want 400", got.SendsLast60sByDomain["em.discountblog.com"])
	}
	// LastSendAtByDomain only set when last_completed_at is non-NULL.
	if _, ok := got.LastSendAtByDomain["em.discountblog.com"]; !ok {
		t.Errorf("last_send_at_by_domain missing entry for discountblog (non-NULL row)")
	}
	if _, ok := got.LastSendAtByDomain["em.myownhealth.net"]; ok {
		t.Errorf("last_send_at_by_domain should NOT have entry for myownhealth (NULL row)")
	}

	if got.Generated.IsZero() {
		t.Errorf("Generated timestamp should be set")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestHandleWaveProcessorStatus_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM mailing_campaign_waves").
		WillReturnError(errors.New("connection refused"))

	h := &WaveProcessorStatusHandler{DB: db, ThroughputProvider: nil}

	req := httptest.NewRequest(http.MethodGet, "/api/wave-processor/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Errorf("expected non-empty error body")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestHandleWaveProcessorStatus_EmptyResult(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	emptyRows := sqlmock.NewRows([]string{
		"sending_domain", "waves_due", "waves_overdue_5m",
		"max_due_age_seconds", "last_completed_at",
	})
	mock.ExpectQuery("FROM mailing_campaign_waves").WillReturnRows(emptyRows)

	provider := stubThroughput{data: map[string]int{}}
	h := &WaveProcessorStatusHandler{DB: db, ThroughputProvider: provider}

	req := httptest.NewRequest(http.MethodGet, "/api/wave-processor/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got WaveProcessorStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Maps must be present (non-nil) and empty so the JSON shape stays
	// stable. A nil map would marshal to `null`, breaking consumers.
	checkEmptyMap := func(name string, m map[string]int) {
		t.Helper()
		if m == nil {
			t.Errorf("%s: map is nil; expected empty map for stable JSON shape", name)
		}
		if len(m) != 0 {
			t.Errorf("%s: got %d entries, want 0", name, len(m))
		}
	}
	checkEmptyMap("WaveQueueDepthByDomain", got.WaveQueueDepthByDomain)
	checkEmptyMap("WaveOverdue5mByDomain", got.WaveOverdue5mByDomain)
	checkEmptyMap("SendsLast60sByDomain", got.SendsLast60sByDomain)

	if got.MaxWaveDelaySecondsByDomain == nil {
		t.Errorf("MaxWaveDelaySecondsByDomain: nil map breaks downstream JSON shape")
	}
	if len(got.MaxWaveDelaySecondsByDomain) != 0 {
		t.Errorf("MaxWaveDelaySecondsByDomain: got %d entries, want 0", len(got.MaxWaveDelaySecondsByDomain))
	}
	if got.LastSendAtByDomain == nil {
		t.Errorf("LastSendAtByDomain: nil map breaks downstream JSON shape")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestHandleWaveProcessorStatus_MethodNotAllowed asserts only GET is
// accepted. POST/DELETE/etc. are rejected with 405 and an Allow header.
func TestHandleWaveProcessorStatus_MethodNotAllowed(t *testing.T) {
	h := &WaveProcessorStatusHandler{DB: nil, ThroughputProvider: nil}
	req := httptest.NewRequest(http.MethodPost, "/api/wave-processor/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow header: got %q, want GET", got)
	}
}

// TestHandleWaveProcessorStatus_NilDB exercises the nil-DB graceful path.
// When the handler is constructed before the mailing DB is wired (early in
// boot) it must still respond with empty maps rather than crashing.
func TestHandleWaveProcessorStatus_NilDB(t *testing.T) {
	provider := stubThroughput{data: map[string]int{"em.foo.com": 7}}
	h := &WaveProcessorStatusHandler{DB: nil, ThroughputProvider: provider}

	req := httptest.NewRequest(http.MethodGet, "/api/wave-processor/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var got WaveProcessorStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SendsLast60sByDomain["em.foo.com"] != 7 {
		t.Errorf("throughput merge: got %d, want 7", got.SendsLast60sByDomain["em.foo.com"])
	}
}

// Compile-time interface checks — guards against silent breakage if the
// concrete *sql.DB ever stops satisfying sqlExec or the SendWorkerPool
// stops returning map[string]int from Throughput().
var (
	_ sqlExec            = (*sql.DB)(nil)
	_ ThroughputProvider = stubThroughput{}
)
