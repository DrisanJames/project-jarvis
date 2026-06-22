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

func passingPreflight(_ context.Context, _ *sql.DB, _, _, _ string) preflightResult {
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

	// Reservation phase: resolve identity (no draft) -> INSERT finalizing_audience -> COMMIT
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id\\s+FROM mailing_campaigns").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Post-commit durability verification (false-success guard)
	mock.ExpectQuery("SELECT id::text FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ok"))

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
	if payload["status"] != "finalizing_audience" {
		t.Fatalf("expected status=finalizing_audience, got %#v", payload["status"])
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

// TestHandleDeployCampaign_ThreadsSendingProfileIDToPreflight verifies that
// when a deploy payload pins SendingProfileID, the value is forwarded to
// preflightDeployCheck so the preflight validates the pinned profile (not the
// by-domain default). This is the contract that lets an operator route a
// single campaign through a non-default profile (e.g. AWS SES relay) while
// daily ops continue on the OVH warm-pool default for the same sending_domain.
func TestHandleDeployCampaign_ThreadsSendingProfileIDToPreflight(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	pinnedID := uuid.New().String()
	var capturedOverride string
	captured := false
	svc := &PMTACampaignService{
		db:          db,
		orgID:       defaultOrgID,
		suppMatcher: NewSuppressionMatcher(),
		colCache:    &campaignColumnCache{cols: map[string]bool{"pmta_config": true, "execution_mode": true}},
		preflightFn: func(_ context.Context, _ *sql.DB, _, _, override string) preflightResult {
			capturedOverride = override
			captured = true
			return preflightResult{OK: false, Errors: []preflightError{{Check: "test", Message: "halt after capture"}}}
		},
		skipBackgroundDeploy: true,
	}

	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignInput{
		Name:             "Pinned Profile Deploy",
		SendingProfileID: pinnedID,
		TargetISPs:       []engine.ISP{engine.ISPGmail},
		SendingDomain:    "mail.example.com",
		Variants: []engine.ContentVariant{{
			VariantName: "A",
			Subject:     "Subject",
			HTMLContent: "<html></html>",
		}},
		SendMode:    "scheduled",
		ScheduledAt: &scheduledAt,
		Timezone:    "UTC",
	}

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	svc.HandleDeployCampaign(rr, req)

	if !captured {
		t.Fatalf("preflightFn was never called; rr.Code=%d body=%s", rr.Code, rr.Body.String())
	}
	if capturedOverride != pinnedID {
		t.Fatalf("preflight received override %q, want %q", capturedOverride, pinnedID)
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 because preflight returned NOT OK, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandleDeployCampaign_NoOverrideThreadsEmpty verifies the back-compat
// path: when a deploy payload omits SendingProfileID, preflight receives ""
// and continues using the legacy by-domain auto-lookup.
func TestHandleDeployCampaign_NoOverrideThreadsEmpty(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	var capturedOverride string
	captured := false
	svc := &PMTACampaignService{
		db:          db,
		orgID:       defaultOrgID,
		suppMatcher: NewSuppressionMatcher(),
		colCache:    &campaignColumnCache{cols: map[string]bool{"pmta_config": true, "execution_mode": true}},
		preflightFn: func(_ context.Context, _ *sql.DB, _, _, override string) preflightResult {
			capturedOverride = override
			captured = true
			return preflightResult{OK: false, Errors: []preflightError{{Check: "test", Message: "halt after capture"}}}
		},
		skipBackgroundDeploy: true,
	}

	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignInput{
		Name:          "No Pin Deploy",
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

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	svc.HandleDeployCampaign(rr, req)

	if !captured {
		t.Fatalf("preflightFn was never called; rr.Code=%d body=%s", rr.Code, rr.Body.String())
	}
	if capturedOverride != "" {
		t.Fatalf("preflight received override %q, want empty string", capturedOverride)
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

	// Reservation phase: resolve identity (found draft) -> UPDATE finalizing_audience -> COMMIT
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(draftID))
	mock.ExpectExec("UPDATE mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Post-commit durability verification (false-success guard)
	mock.ExpectQuery("SELECT id::text FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(draftID))

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
	if payload["status"] != "finalizing_audience" {
		t.Fatalf("expected status=finalizing_audience, got %#v", payload["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// TestHandleDeployCampaign_ReusesScheduledCampaignID verifies that editing a
// previously-scheduled campaign and re-deploying it succeeds. The identity
// resolver now accepts status IN ('draft','scheduled','failed').
func TestHandleDeployCampaign_ReusesScheduledCampaignID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	scheduledAt := time.Now().UTC().Add(45 * time.Minute).Round(time.Minute)
	existingID := uuid.New().String()
	input := engine.PMTACampaignInput{
		CampaignID:    existingID,
		Name:          "Edited Scheduled Campaign",
		TargetISPs:    []engine.ISP{engine.ISPGmail, engine.ISPYahoo},
		SendingDomain: "mail.example.com",
		Variants: []engine.ContentVariant{{
			VariantName: "A",
			Subject:     "Updated Subject",
			HTMLContent: "<html><body>Updated</body></html>",
		}},
		SendMode:    "scheduled",
		ScheduledAt: &scheduledAt,
		Timezone:    "UTC",
	}

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(existingID))
	mock.ExpectExec("UPDATE mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Post-commit durability verification (false-success guard)
	mock.ExpectQuery("SELECT id::text FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(existingID))

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
	if payload["campaign_id"] != existingID {
		t.Fatalf("expected campaign_id %s, got %#v", existingID, payload["campaign_id"])
	}
	if payload["status"] != "finalizing_audience" {
		t.Fatalf("expected status=finalizing_audience, got %#v", payload["status"])
	}
	if targets, ok := payload["target_isps"].([]any); !ok || len(targets) != 2 {
		t.Fatalf("expected 2 target_isps, got %#v", payload["target_isps"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// TestHandleDeployCampaign_RejectsNonEditableCampaign verifies that a campaign
// in a non-editable status (sending, completed, cancelled) is rejected.
func TestHandleDeployCampaign_RejectsNonEditableCampaign(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	scheduledAt := time.Now().UTC().Add(45 * time.Minute).Round(time.Minute)
	completedID := uuid.New().String()
	input := engine.PMTACampaignInput{
		CampaignID:    completedID,
		Name:          "Cannot Redeploy Completed",
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

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id\\s+FROM mailing_campaigns").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for non-editable campaign, got %d, body = %s", rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	errMsg, _ := payload["error"].(string)
	if errMsg == "" {
		t.Fatalf("expected error message in response, got %#v", payload)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}
