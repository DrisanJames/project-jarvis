package api

import (
	"bytes"
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

func expectPMTAConfigColumnCheck(mock sqlmock.Sqlmock, exists bool) {
	mock.ExpectQuery("SELECT EXISTS \\(").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(exists))
}

func TestHandleDeployCampaign_LegacyPayloadReturnsNormalizedResponse(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := NewPMTACampaignService(db, nil, nil, nil, defaultOrgID)
	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignInput{
		Name:          "Legacy Deploy",
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

	mock.ExpectQuery("SELECT md5_hash FROM mailing_global_suppressions").
		WillReturnRows(sqlmock.NewRows([]string{"md5_hash"}))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id\\s+FROM mailing_campaigns").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT id, from_email, from_name, reply_email").
		WillReturnRows(sqlmock.NewRows([]string{"id", "from_email", "from_name", "reply_email"}))
	expectPMTAConfigColumnCheck(mock, true)
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_ab_tests").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_ab_variants").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_campaign_isp_plans").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_campaign_isp_time_spans").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_campaign_isp_plans").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_campaign_isp_time_spans").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if legacy, ok := payload["legacy_input"].(bool); !ok || !legacy {
		t.Fatalf("expected legacy_input=true, got %#v", payload["legacy_input"])
	}
	if plans, ok := payload["isp_plans"].([]any); !ok || len(plans) != 2 {
		t.Fatalf("expected 2 isp_plans, got %#v", payload["isp_plans"])
	}
	if _, ok := payload["initial_waves"].([]any); !ok {
		t.Fatalf("expected initial_waves array, got %#v", payload["initial_waves"])
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

	service := NewPMTACampaignService(db, nil, nil, nil, defaultOrgID)
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
	expectPMTAConfigColumnCheck(mock, true)
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

	service := NewPMTACampaignService(db, nil, nil, nil, defaultOrgID)
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

	mock.ExpectQuery("SELECT md5_hash FROM mailing_global_suppressions").
		WillReturnRows(sqlmock.NewRows([]string{"md5_hash"}))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(draftID))
	mock.ExpectQuery("SELECT id, from_email, from_name, reply_email").
		WillReturnRows(sqlmock.NewRows([]string{"id", "from_email", "from_name", "reply_email"}))
	expectPMTAConfigColumnCheck(mock, true)
	mock.ExpectExec("DELETE FROM mailing_ab_variants").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM mailing_ab_tests").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM mailing_campaign_isp_plans").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_ab_tests").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_ab_variants").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_campaign_isp_plans").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_campaign_isp_time_spans").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload["campaign_id"] != draftID {
		t.Fatalf("expected campaign_id %s, got %#v", draftID, payload["campaign_id"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}
