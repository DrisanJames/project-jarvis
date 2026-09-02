package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// REQ-090 DoD 4 — GET /api/mailing/pmta-campaign/trigger-send (registered at
// handlers_pmta_campaign.go, `cr.Get("/trigger-send", s.HandleTriggerSend)`) is
// the admin recovery path used when the wave scheduler is not dispatching.
//
// On 2026-09-01 it answered `409 kafka send-routing is ON` all day because the
// SK-5 guard keyed on KAFKA_SEND_QUEUE_ENABLED (a WIRING flag) rather than on
// whether anything was actually routed. Task-def 1077 ran ENABLED=1 with ALL=0:
// zero waves routed, five paths blocked. These tests pin both directions of the
// corrected gate.

func clearSendRouteEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")
}

// THE :1077 STATE: ENABLED=1, ALL=0 => nothing routes => trigger-send must NOT
// 409. It reaches its first query (oldest scheduled campaign) and answers 200.
func TestTriggerSend_NotBlockedWhenRoutingOff(t *testing.T) {
	clearSendRouteEnv(t)
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "1") // wired (consumer draining)
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "0")     // but routing OFF

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()

	// Past the guard the handler looks for the oldest ready campaign; return no
	// rows so it stops there with a 200. Reaching this query at all IS the proof
	// the guard did not fire.
	mock.ExpectQuery(`SELECT id::text FROM mailing_campaigns`).
		WithArgs(defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	svc := &PMTACampaignService{db: db, orgID: defaultOrgID}
	rec := httptest.NewRecorder()
	svc.HandleTriggerSend(rec, httptest.NewRequest(http.MethodGet, "/api/mailing/pmta-campaign/trigger-send", nil))

	if rec.Code == http.StatusConflict {
		t.Fatalf("trigger-send must NOT 409 when ALL=0 (nothing routes); body=%s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if body["message"] != "no scheduled campaigns ready" {
		t.Fatalf("handler did not reach its first query; body=%v", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the guard blocked before the query: %v", err)
	}
}

// NEGATIVE PATH: with routing actually ON (KAFKA_SEND_QUEUE_ALL=1) the guard
// MUST still fire — a direct INSERT would bypass the Kafka hard send path. The
// nil DB proves it blocks before touching the database.
func TestTriggerSend_BlockedWhenRoutingOn(t *testing.T) {
	clearSendRouteEnv(t)
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "1")

	svc := &PMTACampaignService{db: nil, orgID: defaultOrgID}
	rec := httptest.NewRecorder()
	svc.HandleTriggerSend(rec, httptest.NewRequest(http.MethodGet, "/api/mailing/pmta-campaign/trigger-send", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 when routing is ON, got %d: %s", rec.Code, rec.Body.String())
	}
}

// An allowlist is routing too: with a non-empty KAFKA_SEND_QUEUE_WAVES some wave
// routes, so the bypass must stay closed even though ALL is unset.
func TestTriggerSend_BlockedWhenAllowlistRoutes(t *testing.T) {
	clearSendRouteEnv(t)
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "some-wave-id")

	svc := &PMTACampaignService{db: nil, orgID: defaultOrgID}
	rec := httptest.NewRecorder()
	svc.HandleTriggerSend(rec, httptest.NewRequest(http.MethodGet, "/api/mailing/pmta-campaign/trigger-send", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 with a non-empty wave allowlist, got %d", rec.Code)
	}
}

// The dark default (no routing env at all) must never block.
func TestTriggerSend_RouteGate_DarkDefaultOpen(t *testing.T) {
	clearSendRouteEnv(t)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT id::text FROM mailing_campaigns`).
		WithArgs(defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	svc := &PMTACampaignService{db: db, orgID: defaultOrgID}
	rec := httptest.NewRecorder()
	svc.HandleTriggerSend(rec, httptest.NewRequest(http.MethodGet, "/api/mailing/pmta-campaign/trigger-send", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("dark default must be a no-op; got %d: %s", rec.Code, rec.Body.String())
	}
}
