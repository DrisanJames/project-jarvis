package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

func passingPreflight(_ context.Context, _ *sql.DB, _, _ string) preflightResult {
	return preflightResult{OK: true}
}

func newTestPMTAService(db *sql.DB, orgID string) *PMTACampaignService {
	svc := &PMTACampaignService{
		db:                   db,
		orgID:                orgID,
		suppMatcher:          NewSuppressionMatcher(),
		colCache:             &campaignColumnCache{cols: map[string]bool{"pmta_config": true, "execution_mode": true}},
		preflightFn:          passingPreflight,
		skipBackgroundDeploy: true,
	}
	return svc
}

func TestHandleDeployCampaign_ReservesAndReturns202(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignInput{
		Name:          "Async Deploy",
		TargetISPs:    []engine.ISP{engine.ISPGmail, engine.ISPApple},
		SendingDomain: "mail.example.com",
		Variants: []engine.ContentVariant{{
			VariantName: "A",
			Subject:     "Subject",
			HTMLContent: "<html></html>",
		}},
		SendMode:    "scheduled",
		ScheduledAt: &scheduledAt,
		Timezone:    "UTC",
	}

	// Reservation phase: resolve identity (no draft) -> INSERT preparing -> COMMIT
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id\\s+FROM mailing_campaigns").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload["status"] != "preparing" {
		t.Fatalf("expected status=preparing, got %#v", payload["status"])
	}
	if payload["name"] != "Async Deploy" {
		t.Fatalf("expected name='Async Deploy', got %#v", payload["name"])
	}
	if payload["campaign_id"] == "" {
		t.Fatalf("expected campaign_id in response")
	}
	if targets, ok := payload["target_isps"].([]any); !ok || len(targets) != 2 {
		t.Fatalf("expected 2 target_isps, got %#v", payload["target_isps"])
	}
	if vc, ok := payload["variant_count"].(float64); !ok || vc != 1 {
		t.Fatalf("expected variant_count=1, got %#v", payload["variant_count"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestHandleSaveDraftCampaign_CreatesDraft(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	scheduledAt := time.Now().UTC().Add(30 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignDraftInput{
		ScheduleMode: "quick",
		CampaignInput: engine.PMTACampaignInput{
			Name:          "Draft Campaign",
			TargetISPs:    []engine.ISP{engine.ISPGmail},
			SendingDomain: "mail.example.com",
			Variants: []engine.ContentVariant{{
				VariantName: "A",
				Subject:     "Subject",
				HTMLContent: "<html></html>",
			}},
			SendMode:    "scheduled",
			ScheduledAt: &scheduledAt,
			Timezone:    "UTC",
		},
	}

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id\\s+FROM mailing_campaigns").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT id, from_email, from_name, reply_email").
		WillReturnRows(sqlmock.NewRows([]string{"id", "from_email", "from_name", "reply_email"}))
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/draft", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleSaveDraftCampaign(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload["status"] != "draft" {
		t.Fatalf("expected draft status, got %#v", payload["status"])
	}
	if payload["campaign_id"] == "" {
		t.Fatalf("expected campaign_id in response")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestHandleDeployCampaign_ReusesDraftCampaignID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	scheduledAt := time.Now().UTC().Add(45 * time.Minute).Round(time.Minute)
	draftID := uuid.New().String()
	input := engine.PMTACampaignInput{
		CampaignID:    draftID,
		Name:          "Draft Deploy",
		TargetISPs:    []engine.ISP{engine.ISPGmail},
		SendingDomain: "mail.example.com",
		Variants: []engine.ContentVariant{{
			VariantName: "A",
			Subject:     "Subject",
			HTMLContent: "<html></html>",
		}},
		SendMode:    "scheduled",
		ScheduledAt: &scheduledAt,
		Timezone:    "UTC",
	}

	// Reservation phase: resolve identity (found draft) -> UPDATE preparing -> COMMIT
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(draftID))
	mock.ExpectExec("UPDATE mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload["campaign_id"] != draftID {
		t.Fatalf("expected campaign_id %s, got %#v", draftID, payload["campaign_id"])
	}
	if payload["status"] != "preparing" {
		t.Fatalf("expected status=preparing, got %#v", payload["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}
