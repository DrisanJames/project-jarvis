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
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/domainagent"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// planTestColumns mirrors domainagent.planColumns scan order.
var planTestColumns = []string{
	"id", "organization_id", "sending_domain", "plan_date", "status",
	"briefing", "slots", "payloads", "deploy_results",
	"approved_by", "approved_at", "created_at", "updated_at",
}

func planTestRow(planID, status, payloadsJSON string) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(planTestColumns).AddRow(
		planID, defaultOrgID, "mail.example.com", now, status,
		"{}", "[]", payloadsJSON, "[]",
		"", nil, now, now,
	)
}

func domainAgentTestPayloadJSON(t *testing.T, name string) string {
	t.Helper()
	scheduledAt := time.Now().UTC().Add(30 * time.Minute).Round(time.Minute)
	input := engine.PMTACampaignInput{
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
	raw, err := json.Marshal([]engine.PMTACampaignInput{input})
	if err != nil {
		t.Fatalf("marshal payloads: %v", err)
	}
	return string(raw)
}

func newApprovePlanRequest(t *testing.T, planID string, ctx context.Context) *http.Request {
	t.Helper()
	body := bytes.NewReader([]byte(`{"approved_by":"operator@test"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/domain-agent/plans/"+planID+"/approve", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("planID", planID)
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

// TestHandleApprovePlan_DeployContinuesAfterClientDisconnect pins the
// detached-context fix (findings/2026-07-13-B §4): the request context is
// cancelled mid-deploy (simulated client disconnect during the first
// payload's preflight), and the deploy loop + progress persistence must
// still complete because they run on context.WithoutCancel — under the old
// ctx := r.Context() wiring every subsequent DB call would fail with
// context.Canceled, stranding the plan at 'approved' with empty results.
func TestHandleApprovePlan_DeployContinuesAfterClientDisconnect(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()

	svc := &PMTACampaignService{
		db:          db,
		orgID:       defaultOrgID,
		suppMatcher: NewSuppressionMatcher(),
		colCache:    &campaignColumnCache{cols: map[string]bool{"pmta_config": true, "execution_mode": true}},
		preflightFn: func(_ context.Context, _ *sql.DB, _, _, _ string) preflightResult {
			// Client disconnects mid-deploy: everything after this point
			// must survive the request context's cancellation.
			cancelReq()
			return preflightResult{OK: true}
		},
		skipBackgroundDeploy: true,
	}
	agent := &DomainAgentAPI{db: db, pmta: svc, plans: domainagent.NewPlanStore(db)}

	planID := uuid.New().String()
	payloadsJSON := domainAgentTestPayloadJSON(t, "DA Disconnect Deploy")

	// 1. plans.Get → compiled plan with one payload
	mock.ExpectQuery("SELECT id, organization_id, sending_domain").
		WillReturnRows(planTestRow(planID, "compiled", payloadsJSON))
	// 2. plans.Approve (compiled → approved)
	mock.ExpectExec("UPDATE mailing_domain_agent_plans").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 3. deployFromInput reservation — runs AFTER the request ctx is
	// cancelled; succeeds only on the detached context.
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
	// 4. per-payload progress persisted (status stays 'approved' in flight)
	mock.ExpectExec("UPDATE mailing_domain_agent_plans").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 5. final results persisted (status → 'deployed')
	mock.ExpectExec("UPDATE mailing_domain_agent_plans").
		WillReturnResult(sqlmock.NewResult(0, 1))

	rr := httptest.NewRecorder()
	agent.HandleApprovePlan(rr, newApprovePlanRequest(t, planID, reqCtx))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Status  string                    `json:"status"`
		Results []domainAgentDeployResult `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload.Status != "deployed" {
		t.Fatalf("expected plan status=deployed despite disconnect, got %q (results: %+v)", payload.Status, payload.Results)
	}
	if len(payload.Results) != 1 || payload.Results[0].CampaignID == "" || payload.Results[0].Status != "finalizing_audience" {
		t.Fatalf("expected 1 deployed result with campaign_id, got %+v", payload.Results)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

// TestHandleApprovePlan_ResumeApprovedPlanConvergesWithoutDuplicates pins the
// recovery path: a plan stranded at status='approved' (crash/disconnect
// mid-deploy) can be re-POSTed to /approve; the store Approve call is
// skipped, the deploy loop re-runs, and a payload whose campaign is already
// live converges via the by-name guard (already_existed, NO second
// mailing_campaigns INSERT) instead of double-deploying.
func TestHandleApprovePlan_ResumeApprovedPlanConvergesWithoutDuplicates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	svc := &PMTACampaignService{
		db:                   db,
		orgID:                defaultOrgID,
		suppMatcher:          NewSuppressionMatcher(),
		colCache:             &campaignColumnCache{cols: map[string]bool{"pmta_config": true, "execution_mode": true}},
		preflightFn:          passingPreflight,
		skipBackgroundDeploy: true,
	}
	agent := &DomainAgentAPI{db: db, pmta: svc, plans: domainagent.NewPlanStore(db)}

	planID := uuid.New().String()
	existingCampaignID := uuid.New().String()
	payloadsJSON := domainAgentTestPayloadJSON(t, "DA Resume Deploy")

	// 1. plans.Get → plan stranded at 'approved' (partial first run).
	//    NO plans.Approve exec follows: resume skips the compiled→approved
	//    transition (sqlmock is strict-ordered — an Approve UPDATE here
	//    would fail against the ExpectBegin below).
	mock.ExpectQuery("SELECT id, organization_id, sending_domain").
		WillReturnRows(planTestRow(planID, "approved", payloadsJSON))
	// 2. deployFromInput reservation: by-name guard matches the campaign the
	//    first run already deployed → early return, rollback, NO INSERT.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id::text, status\\s+FROM mailing_campaigns").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(existingCampaignID, "scheduled"))
	mock.ExpectRollback()
	// 3. per-payload progress + final results persisted
	mock.ExpectExec("UPDATE mailing_domain_agent_plans").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_domain_agent_plans").
		WillReturnResult(sqlmock.NewResult(0, 1))

	rr := httptest.NewRecorder()
	agent.HandleApprovePlan(rr, newApprovePlanRequest(t, planID, context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Status  string                    `json:"status"`
		Results []domainAgentDeployResult `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response json: %v", err)
	}
	if payload.Status != "deployed" {
		t.Fatalf("expected plan status=deployed after resume, got %q (results: %+v)", payload.Status, payload.Results)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("expected 1 result, got %+v", payload.Results)
	}
	res := payload.Results[0]
	if !res.AlreadyExisted {
		t.Fatalf("expected already_existed=true for the converged payload, got %+v", res)
	}
	if res.CampaignID != existingCampaignID {
		t.Fatalf("expected converged campaign_id %s, got %+v", existingCampaignID, res)
	}
	if res.Status != "scheduled" {
		t.Fatalf("expected existing campaign status 'scheduled', got %+v", res)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations (a duplicate INSERT would appear here): %v", err)
	}
}
