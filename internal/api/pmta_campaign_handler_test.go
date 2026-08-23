package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

func passingPreflight(_ context.Context, _ *sql.DB, _, _, _ string) preflightResult {
	return preflightResult{OK: true}
}

// allGreenGates stubs the REQ-007 server-side gate evaluation so tests that
// pin deploy mechanics (reservation, idempotency, preflight threading) don't
// have to mock the gate-source queries. Gate-enforcement behavior itself is
// pinned by the TestHandleDeployCampaign_*Gate* tests below.
func allGreenGates(_ context.Context, _ sendDayGateEvalInput) sendDayGateReport {
	return sendDayGateReport{Verdicts: []sendDayGateVerdict{
		{Gate: "A", Name: "PMTA host health attestation", State: "pass"},
		{Gate: "B", Name: "Wave-dispatcher cleanup", State: "pass"},
		{Gate: "C", Name: "Delivery build check", State: "pass"},
		{Gate: "D", Name: "Sending-profile preflight", State: "pass"},
		{Gate: "F", Name: "Volume reconciliation", State: "pass"},
	}}
}

func newTestPMTAService(db *sql.DB, orgID string) *PMTACampaignService {
	svc := &PMTACampaignService{
		db:                   db,
		orgID:                orgID,
		suppMatcher:          NewSuppressionMatcher(),
		colCache:             &campaignColumnCache{cols: map[string]bool{"pmta_config": true, "execution_mode": true}},
		preflightFn:          passingPreflight,
		gateEvalFn:           allGreenGates,
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
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
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

	// Reservation phase: id-less deploy takes the (org, name) advisory lock,
	// finds no live same-name campaign (by-name idempotency guard —
	// 2026-07-13 over-deploy fix), mints a fresh UUID (NO draft-reuse
	// lookup — 2026-07-11 draft-eater fix) -> INSERT finalizing_audience -> COMMIT
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id::text, status\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
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
		gateEvalFn:           allGreenGates,
		skipBackgroundDeploy: true,
	}

	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignInput{
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
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
		gateEvalFn:           allGreenGates,
		skipBackgroundDeploy: true,
	}

	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignInput{
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
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

// TestHandleDeployCampaign_RePostConvergesOnExistingName pins the by-(org,
// name) idempotency guard (2026-07-13 over-deploy incident): an id-less
// re-POST of a name that already has a live campaign returns the EXISTING
// campaign — 200 + already_existed:true, existing id/status — and inserts no
// second mailing_campaigns row. The matched row here is in
// finalizing_audience (the state whose NULL sending_profile_id fooled the
// jul13 (name, profile) reconciliation): the guard matches by name alone and
// never consults sending_profile_id, so "pending" is never read as "missing".
func TestHandleDeployCampaign_RePostConvergesOnExistingName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	existingID := uuid.New().String()
	input := engine.PMTACampaignInput{
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
		Name:          "Repeated Deploy",
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

	// Advisory lock -> by-name check finds the live campaign (only
	// cancelled/failed/deleted free a name for re-deploy) -> early return,
	// tx rolled back, NO INSERT (sqlmock is strict-ordered: an INSERT here
	// would fail the test).
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id::text, status\s+FROM mailing_campaigns\s+WHERE organization_id = \$1 AND name = \$2\s+AND status NOT IN \('cancelled', 'failed', 'deleted'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(existingID, "finalizing_audience"))
	mock.ExpectRollback()

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload["already_existed"] != true {
		t.Fatalf("expected already_existed=true, got %#v", payload["already_existed"])
	}
	if payload["campaign_id"] != existingID {
		t.Fatalf("expected existing campaign_id %s, got %#v", existingID, payload["campaign_id"])
	}
	if payload["status"] != "finalizing_audience" {
		t.Fatalf("expected existing status=finalizing_audience, got %#v", payload["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations (a second INSERT would appear here): %v", err)
	}
}

// TestHandleDeployCampaign_TerminalNameAllowsRedeploy verifies the guard's
// escape hatch: when every same-name campaign is terminal (cancelled/failed/
// deleted the SELECT filters them out), the deploy proceeds and reserves a
// fresh campaign as before (202 + already_existed:false).
func TestHandleDeployCampaign_TerminalNameAllowsRedeploy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignInput{
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
		Name:          "Redeploy After Cancel",
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
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Terminal rows are filtered by the SELECT itself → no live match.
	mock.ExpectQuery("SELECT id::text, status\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT id::text FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ok"))

	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload["already_existed"] != false {
		t.Fatalf("expected already_existed=false, got %#v", payload["already_existed"])
	}
	if payload["status"] != "finalizing_audience" {
		t.Fatalf("expected status=finalizing_audience, got %#v", payload["status"])
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
			OfferID: "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
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

	// Id-less save mints a fresh UUID (no draft-reuse lookup — 2026-07-11
	// draft-eater fix); subsequent saves upsert by the returned id.
	mock.ExpectBegin()
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
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
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
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
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
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
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

// ─── Server-side send-day gate enforcement (REQ-007) ─────────────────────────

// gateTestInput builds the minimal valid deploy payload the gate tests share.
// No ISPQuotas/ISPPlans → audience-bound (uncapped) per the standing doctrine.
func gateTestInput(name string) engine.PMTACampaignInput {
	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	return engine.PMTACampaignInput{
		OfferID:       "0d0d0d0d-0d0d-40d0-80d0-0d0d0d0d0d0d",
		Name:          name,
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
}

// redGates stubs a gate evaluation where Gate B and Gate F are red.
func redGates(_ context.Context, _ sendDayGateEvalInput) sendDayGateReport {
	return sendDayGateReport{Verdicts: []sendDayGateVerdict{
		{Gate: "A", Name: "PMTA host health attestation", State: "pass"},
		{Gate: "B", Name: "Wave-dispatcher cleanup", State: "fail", Detail: "zombies=500 expired=12 (threshold <50 each) — run the pre-deploy janitor"},
		{Gate: "C", Name: "Delivery build check", State: "pass"},
		{Gate: "D", Name: "Sending-profile preflight", State: "pass"},
		{Gate: "F", Name: "Volume reconciliation", State: "fail", Detail: "planned 85826 vs yesterday 639440 (13%) — below the 60% collapse floor"},
	}}
}

// TestHandleDeployCampaign_RedGateBlocksWithoutOverride pins the REQ-007
// negative path: a red gate blocks the id-less deploy with 412 +
// {error, failed_gates, override_hint} and NO campaign row is touched
// (sqlmock expects zero SQL — any statement would fail the test).
func TestHandleDeployCampaign_RedGateBlocksWithoutOverride(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	service.gateEvalFn = redGates

	body, _ := json.Marshal(gateTestInput("Gate Blocked Deploy"))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body = %s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	errMsg, _ := payload["error"].(string)
	if !strings.Contains(errMsg, "B") || !strings.Contains(errMsg, "F") {
		t.Fatalf("expected error naming gates B and F, got %q", errMsg)
	}
	failed, ok := payload["failed_gates"].([]any)
	if !ok || len(failed) != 2 {
		t.Fatalf("expected 2 failed_gates, got %#v", payload["failed_gates"])
	}
	if hint, _ := payload["override_hint"].(string); !strings.Contains(hint, "gate_override") {
		t.Fatalf("expected actionable override_hint, got %#v", payload["override_hint"])
	}

	// No status change: zero SQL statements were expected or executed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("deploy must not touch the DB when blocked by gates: %v", err)
	}
}

// TestHandleDeployCampaign_PromotePathRedGateBlocks mirrors the negative path
// for the Draft Board promote flow (payload carries campaign_id): promotion to
// scheduled is gated exactly like the id-less deploy; the draft row stays a
// draft (zero SQL).
func TestHandleDeployCampaign_PromotePathRedGateBlocks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	service.gateEvalFn = redGates

	input := gateTestInput("Gate Blocked Promotion")
	input.CampaignID = uuid.New().String()
	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body = %s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if _, ok := payload["failed_gates"].([]any); !ok {
		t.Fatalf("expected failed_gates in promote-path response, got %#v", payload)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("promotion must not touch the DB when blocked by gates: %v", err)
	}
}

// TestHandleDeployCampaign_OverrideRequiresReason: a gate_override with an
// empty/whitespace reason does NOT clear red gates.
func TestHandleDeployCampaign_OverrideRequiresReason(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	service.gateEvalFn = redGates

	body, _ := json.Marshal(struct {
		engine.PMTACampaignInput
		GateOverride map[string]string `json:"gate_override"`
	}{gateTestInput("Empty Reason Override"), map[string]string{"reason": "   "}})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412 (empty reason must not override); body = %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// TestHandleDeployCampaign_OverrideProceedsAndAudits pins the escape hatch:
// red gates + gate_override{reason} → the deploy proceeds (202) AND the
// override is audit-logged — both the [GateOverride] log line and the
// best-effort row in mailing_send_day_gate_attestations.
func TestHandleDeployCampaign_OverrideProceedsAndAudits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	service.gateEvalFn = redGates

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	// 1. Audit row upsert (gate='OVERRIDE', keyed by campaign name).
	mock.ExpectExec("INSERT INTO mailing_send_day_gate_attestations").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 2. Normal reservation sequence — behavior past the gate is unchanged.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id::text, status\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT id::text FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ok"))

	body, _ := json.Marshal(struct {
		engine.PMTACampaignInput
		GateOverride map[string]string `json:"gate_override"`
	}{gateTestInput("Overridden Deploy"), map[string]string{"reason": "deliberate small re-mail board — operator approved"}})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload["status"] != "finalizing_audience" {
		t.Fatalf("expected status=finalizing_audience, got %#v", payload["status"])
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "[GateOverride]") ||
		!strings.Contains(logged, "gates=[B,F]") ||
		!strings.Contains(logged, "deliberate small re-mail board") {
		t.Fatalf("expected [GateOverride] audit line with gates + reason, got: %s", logged)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations (audit row + reservation): %v", err)
	}
}

// TestHandleDeployCampaign_GatesAllGreenRealEvaluator drives the REAL
// evaluateSendDayGates (gateEvalFn nil) with healthy gate sources and proves
// green gates are invisible: identical 202 behavior, no override needed.
// Query order pinned by sqlmock: Gate A attestations → Gate B wave counts →
// (Gate F skipped: uncapped payload) → reservation sequence.
func TestHandleDeployCampaign_GatesAllGreenRealEvaluator(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	service.gateEvalFn = nil // real evaluator

	fresh := time.Now()
	// Gate A — both servers attested pass within the current MDT send-day.
	mock.ExpectQuery("mailing_send_day_gate_attestations").
		WillReturnRows(sqlmock.NewRows([]string{"server_key", "state", "last_checked_at", "message"}).
			AddRow("server_a", "pass", fresh, "ok").
			AddRow("server_b", "pass", fresh, "ok"))
	// Gate B — janitor counts under threshold.
	mock.ExpectQuery("FROM mailing_campaign_waves").
		WillReturnRows(sqlmock.NewRows([]string{"zombies", "expired", "due_now", "planned", "enqueued", "running"}).
			AddRow(3, 1, 0, 12, 2, 1))
	// Gate C in-process; Gate D via preflightFn (pass); Gate F exempt (uncapped).
	// Reservation sequence — unchanged behavior.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id::text, status\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT id::text FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ok"))

	body, _ := json.Marshal(gateTestInput("Green Gates Deploy"))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (green gates must be invisible); body = %s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload["already_existed"] != false {
		t.Fatalf("expected already_existed=false, got %#v", payload["already_existed"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// TestHandleDeployCampaign_GateEnforcementKillSwitch proves the one-line
// rollback works: with DISABLE_SEND_DAY_GATE_ENFORCEMENT=true, red gates do
// NOT block (no 412, no override required) and the deploy proceeds through
// the normal reservation sequence — with the bypass logged for the audit
// trail. Wave-1 manager review CHANGES item (REQ-007a).
func TestHandleDeployCampaign_GateEnforcementKillSwitch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	service.gateEvalFn = redGates

	t.Setenv("DISABLE_SEND_DAY_GATE_ENFORCEMENT", "true")

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	// Normal reservation sequence — NO audit-override row (no override was
	// supplied; the kill switch is a config bypass, not an operator override).
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id::text, status\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT id::text FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ok"))

	body, _ := json.Marshal(gateTestInput("Kill Switch Deploy"))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (kill switch must bypass the 412); body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(logBuf.String(), "enforcement DISABLED by kill switch") {
		t.Fatalf("expected kill-switch bypass to be logged, got: %s", logBuf.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestHandleDeployCampaign_InternalCallerBypassesGates is a production
// regression guard. Gate enforcement shipped 2026-07-13 and immediately blocked
// the partner-drip orchestrator, which deploys a wave group every few minutes
// in-process (WrapPMTACampaignDeploy) through this same handler: 27 wave-group
// deploys failed with `412 send-day gates failed: A` because no operator had
// attested PMTA host health that day, and partner touches stopped.
//
// The six gates describe an OPERATOR SEND-DAY BOARD (host attestation, wave
// janitor, volume vs yesterday). Continuous automation has none of those
// semantics, so an X-Internal-Caller deploy must pass through red gates —
// audit-logged, never silently.
func TestHandleDeployCampaign_InternalCallerBypassesGates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	service.gateEvalFn = redGates // every gate red

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	// Normal reservation proceeds — no 412, no override row.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id::text, status\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
	mock.ExpectExec("INSERT INTO mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT id::text FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ok"))

	body, _ := json.Marshal(gateTestInput("[partner-drip] jarvis_att cp ses"))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	req.Header.Set("X-Internal-Caller", "partner_drip_orchestrator")
	rr := httptest.NewRecorder()

	service.HandleDeployCampaign(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 — internal automation must not be gated; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(logBuf.String(), "bypass: internal caller") {
		t.Fatalf("expected an audit line for the internal bypass, got: %s", logBuf.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
