package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

// TestHandleGetJourneyNodeStats_ApiVersionAndShape exercises the
// happy path: the endpoint surfaces api_version and a node-keyed map
// with hard/soft bounce split. A separate awaiting query merges into
// the same map so a node can show non-zero awaiting even with zero
// shadow campaigns.
func TestHandleGetJourneyNodeStats_ApiVersionAndShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	hardSQL := HardBounceSQL("t")
	if !strings.Contains(hardSQL, "bounce_type") {
		t.Fatalf("HardBounceSQL must reference bounce_type")
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
			c.journey_node_id,`)).
		WithArgs("j1").
		WillReturnRows(sqlmock.NewRows([]string{
			"journey_node_id", "shadow_campaigns",
			"sent", "delivered", "opens", "clicks",
			"hard_bounce", "soft_bounce",
		}).
			AddRow("node_email_1", 2, 100, 95, 25, 4, 3, 2).
			AddRow("node_email_2", 1, 50, 48, 12, 1, 1, 1))

	// Sent override from the message_log send-truth ledger (v1.1): click-drip
	// touches emit no 'sent' tracking event, so the event-derived sent above
	// is replaced per node by the message_log count. node_email_2 has no
	// message_log row here and must keep its event-derived fallback.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_message_log ml`)).
		WithArgs("j1").
		WillReturnRows(sqlmock.NewRows([]string{"journey_node_id", "count"}).
			AddRow("node_email_1", 288))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT current_node_id, COUNT(*)
		FROM mailing_journey_enrollments`)).
		WithArgs("j1").
		WillReturnRows(sqlmock.NewRows([]string{"current_node_id", "count"}).
			AddRow("node_email_1", 7).
			AddRow("node_email_3", 12))

	jb := &JourneyBuilder{db: db}
	r := chi.NewRouter()
	r.Get("/journeys/{journeyId}/node-stats", jb.HandleGetJourneyNodeStats)

	req := httptest.NewRequest("GET", "/journeys/j1/node-stats", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp JourneyNodeStatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.APIVersion != VersionJourneyNodeStats {
		t.Fatalf("api_version: got %q want %q", resp.APIVersion, VersionJourneyNodeStats)
	}
	if resp.JourneyID != "j1" {
		t.Fatalf("journey_id: got %q want j1", resp.JourneyID)
	}

	n1, ok := resp.Nodes["node_email_1"]
	if !ok {
		t.Fatalf("missing node_email_1 in response")
	}
	// Sent must be the message_log override (288), NOT the event-derived 100 —
	// regressing to tracking-event 'sent' re-creates the aug04 "funnel not
	// mailing" zero for click-drip nodes.
	if n1.Sent != 288 || n1.HardBounce != 3 || n1.SoftBounce != 2 || n1.AudienceAwaiting != 7 {
		t.Fatalf("node_email_1 stats wrong: %+v", n1)
	}
	if n2 := resp.Nodes["node_email_2"]; n2.Sent != 50 {
		t.Fatalf("node_email_2 must keep event-derived sent fallback (50), got %d", n2.Sent)
	}

	n3, ok := resp.Nodes["node_email_3"]
	if !ok {
		t.Fatalf("missing node_email_3 in response (awaiting-only node)")
	}
	if n3.AudienceAwaiting != 12 {
		t.Fatalf("node_email_3 awaiting: got %d want 12", n3.AudienceAwaiting)
	}
	if n3.Sent != 0 {
		t.Fatalf("node_email_3 should have no sent activity; got %d", n3.Sent)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestHandleGetJourneyNodeStats_QueryError covers the failure-mode
// path: a DB error on the metrics query should return 500 with the
// api_version still present so the frontend's error envelope parser
// keeps working.
func TestHandleGetJourneyNodeStats_QueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
			c.journey_node_id,`)).
		WithArgs("j1").
		WillReturnError(context.DeadlineExceeded)

	jb := &JourneyBuilder{db: db}
	r := chi.NewRouter()
	r.Get("/journeys/{journeyId}/node-stats", jb.HandleGetJourneyNodeStats)

	req := httptest.NewRequest("GET", "/journeys/j1/node-stats", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rr.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if resp["api_version"] != VersionJourneyNodeStats {
		t.Fatalf("api_version on error: got %q want %q", resp["api_version"], VersionJourneyNodeStats)
	}
	if resp["error"] == "" {
		t.Fatalf("error envelope must include 'error' message")
	}
}
