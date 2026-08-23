package api

// Tests for the deploy-time offer gate + attach-offer repair (offer_gate.go).
//
// The gate's contract: a deploy with no offer_id is refused 422 BEFORE any
// DB work (zero writes — strict sqlmock proves it), with three sanctioned
// passes: KUMO-WARM/NEWSLETTER editorial names, the partner drip orchestrator
// (X-Internal-Caller), and the OFFER_DEPLOY_GATE_DISABLED kill switch (loud).

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// ogInput is a valid deploy payload with NO offer_id.
func ogInput(name string) engine.PMTACampaignInput {
	scheduledAt := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	return engine.PMTACampaignInput{
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

func ogPostDeploy(t *testing.T, svc *PMTACampaignService, input engine.PMTACampaignInput, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(input)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	svc.HandleDeployCampaign(rr, req)
	return rr
}

// expectReservation arms the standard successful-reservation SQL sequence
// (mirrors TestHandleDeployCampaign_ReservesAndReturns202).
func expectReservation(mock sqlmock.Sqlmock) {
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
}

// ── Pure gate semantics ─────────────────────────────────────────────────────

func TestOfferGateCheck_Semantics(t *testing.T) {
	cases := []struct {
		name     string
		input    engine.PMTACampaignInput
		caller   string
		wantOK   bool
		wantHint string // substring of the reason (exempt passes log it; fails carry it)
	}{
		{"offer set passes silently", engine.PMTACampaignInput{OfferID: "abc", Name: "aug22 - RB - W1 - globe"}, "", true, ""},
		{"no offer blocks", engine.PMTACampaignInput{Name: "aug22 - Rates Bazar - W1-ENG - globe"}, "", false, "no offer_id"},
		{"KUMO-WARM name exempt", engine.PMTACampaignInput{Name: "aug22 - aadwd - KUMO-WARM - newsletter"}, "", true, "offer-exempt name"},
		{"newsletter name exempt", engine.PMTACampaignInput{Name: "aug22 - DB - Daily Newsletter"}, "", true, "offer-exempt name"},
		{"drip caller exempt", engine.PMTACampaignInput{Name: "[partner-drip] jarvis_att cp ses"}, "partner_drip_orchestrator", true, "creative-row level"},
		{"other internal caller still blocked", engine.PMTACampaignInput{Name: "some automation"}, "other_worker", false, "no offer_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := offerGateCheck(tc.input, tc.caller)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if tc.wantHint != "" && !strings.Contains(reason, tc.wantHint) {
				t.Fatalf("reason %q does not contain %q", reason, tc.wantHint)
			}
		})
	}
}

func TestOfferGateCheck_KillSwitchFailsOpen(t *testing.T) {
	t.Setenv(offerGateKillSwitch, "1")
	ok, reason := offerGateCheck(engine.PMTACampaignInput{Name: "aug22 - RB - W1 - globe"}, "")
	if !ok {
		t.Fatalf("kill switch must fail open, got blocked (%q)", reason)
	}
	if !strings.Contains(reason, "BYPASSED") {
		t.Fatalf("kill-switch pass must be loud, got reason %q", reason)
	}
}

// ── Deploy-path enforcement ─────────────────────────────────────────────────

// TestHandleDeployCampaign_NoOfferBlocked422ZeroWrites is the core negative
// path: a no-offer deploy is refused 422 and performs ZERO SQL — sqlmock is
// strict and carries no expectations, so any statement fails the test.
func TestHandleDeployCampaign_NoOfferBlocked422ZeroWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)

	rr := ogPostDeploy(t, service, ogInput("aug22 - Rates Bazar - RR 06:01"), nil)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{"no offer_id", "suppression", "OFFER_DEPLOY_GATE_DISABLED"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("422 body must contain %q, got: %s", want, rr.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("blocked deploy must perform ZERO SQL: %v", err)
	}
}

func TestHandleDeployCampaign_KumoWarmNameExemptFromOfferGate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	expectReservation(mock)

	rr := ogPostDeploy(t, service, ogInput("aug22 - aadwd - KUMO-WARM - newsletter"), nil)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (KUMO-WARM editorial is offer-exempt); body = %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestHandleDeployCampaign_DripCallerExemptFromOfferGate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	expectReservation(mock)

	rr := ogPostDeploy(t, service, ogInput("[partner-drip] internal_auto v3 wave"),
		map[string]string{"X-Internal-Caller": "partner_drip_orchestrator"})

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (drip offers bind at the creative row); body = %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestHandleDeployCampaign_OfferGateKillSwitchBypassesWithLog(t *testing.T) {
	t.Setenv(offerGateKillSwitch, "1")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	expectReservation(mock)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	rr := ogPostDeploy(t, service, ogInput("aug22 - Rates Bazar - RR 06:01"), nil)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (kill switch fails open); body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(logBuf.String(), "OFFER GATE BYPASSED") {
		t.Fatalf("kill-switch bypass must be loudly logged, got: %s", logBuf.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// ── attach-offer repair endpoint ────────────────────────────────────────────

const aoCampID = "cccccccc-0000-4000-8000-000000000001"
const aoOfferID = "0f0f0f0f-0f0f-40f0-80f0-0f0f0f0f0f0f"

func aoPost(t *testing.T, svc *PMTACampaignService, campaignID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/mailing/pmta-campaign/"+campaignID+"/attach-offer", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("campaignId", campaignID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	svc.HandleAttachOffer(rr, req)
	return rr
}

// TestAttachOffer_HappyPathRestampsAttribution: confirmed attach lands
// offer_id + offer_key on the row, re-runs the attribution stamp (payload
// path: attribution_source='payload'), audit-logs, and the response carries
// the suppression caveat.
func TestAttachOffer_HappyPathRestampsAttribution(t *testing.T) {
	t.Setenv(autoTagKillSwitch, "1") // tags are the stamp's tail; not under test
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)

	// 1. Campaign load: no current offer, empty blob (pre-blob campaign).
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(aoCampID, defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "offer_id", "offer_key", "pmta_config"}).
			AddRow("aug22 - Rates Bazar - RR 06:01", "scheduled", nil, "", ""))
	// 2. Offer existence/name.
	mock.ExpectQuery(`SELECT name, COALESCE\(status, 'draft'\)`).
		WithArgs(aoOfferID, defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status"}).AddRow("Globe Life", "active"))
	// 3. offer_key resolution (landing_page_slug wins; no slug-map hop).
	mock.ExpectQuery(`landing_page_slug`).
		WithArgs(aoOfferID, defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"slug", "everflow"}).AddRow("globe", ""))
	// 4. The attach UPDATE.
	mock.ExpectExec(`UPDATE mailing_campaigns`).
		WithArgs(aoCampID, aoOfferID, "globe", defaultOrgID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 5. Re-stamp: idempotency probe says NOT stamped → full payload-path stamp.
	mock.ExpectQuery(`SELECT attribution_source IS NOT NULL`).
		WithArgs(aoCampID).
		WillReturnRows(sqlmock.NewRows([]string{"stamped"}).AddRow(false))
	// 6. Stamp's own offer_key resolution (shared helper).
	mock.ExpectQuery(`landing_page_slug`).
		WithArgs(aoOfferID, defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"slug", "everflow"}).AddRow("globe", ""))
	// 7. The stamp UPDATE — attribution_source='payload' proves the re-stamp.
	mock.ExpectExec(`UPDATE mailing_campaigns`).
		WithArgs(aoCampID, aoOfferID, "globe", nil, nil, "payload").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 8. Audit row.
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rr := aoPost(t, svc, aoCampID, `{"offer_id":"`+aoOfferID+`","confirmed":true}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["suppression_caveat"] != attachOfferSuppressionCaveat {
		t.Fatalf("suppression_caveat must ALWAYS ride the response, got %v", out["suppression_caveat"])
	}
	if out["offer_key"] != "globe" || out["offer_id"] != aoOfferID {
		t.Fatalf("offer identity wrong: %v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestAttachOffer_UnconfirmedDryPreviewZeroWrites: no confirmed flag → the
// preview reads (campaign, offer, key) and writes NOTHING; caveat present.
func TestAttachOffer_UnconfirmedDryPreviewZeroWrites(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(aoCampID, defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "offer_id", "offer_key", "pmta_config"}).
			AddRow("aug22 - Rates Bazar - RR 06:01", "scheduled", nil, "", ""))
	mock.ExpectQuery(`SELECT name, COALESCE\(status, 'draft'\)`).
		WithArgs(aoOfferID, defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status"}).AddRow("Globe Life", "active"))
	mock.ExpectQuery(`landing_page_slug`).
		WithArgs(aoOfferID, defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"slug", "everflow"}).AddRow("globe", ""))

	rr := aoPost(t, svc, aoCampID, `{"offer_id":"`+aoOfferID+`"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"dry_run":true`) {
		t.Fatalf("expected dry_run preview, got %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), attachOfferSuppressionCaveat) {
		t.Fatalf("dry preview must carry the suppression caveat: %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("preview must not write: %v", err)
	}
}

// TestAttachOffer_TerminalStatus409: a finished send cannot be repaired by
// attach — 409 before any offer lookup or write, caveat still present.
func TestAttachOffer_TerminalStatus409(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(aoCampID, defaultOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "offer_id", "offer_key", "pmta_config"}).
			AddRow("aug21 - RB - done", "sent", nil, "", ""))

	rr := aoPost(t, svc, aoCampID, `{"offer_id":"`+aoOfferID+`","confirmed":true}`)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), attachOfferSuppressionCaveat) {
		t.Fatalf("409 must still carry the caveat: %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("terminal refusal must not touch the offer or write: %v", err)
	}
}

// ── Day Cards rebuild: offer override + pre-cancel gate ─────────────────────

// dcNoOfferBlob is dcConfigBlob minus the offer_id — the pre-gate campaign
// shape the rebuild flow has to repair.
func dcNoOfferBlob(t *testing.T, name string) string {
	t.Helper()
	sched := time.Date(2026, 8, 22, 7, 1, 0, 0, time.UTC)
	end := sched.Add(8 * time.Hour)
	input := map[string]interface{}{
		"name":           name,
		"sending_domain": "em.ratesbazar.com",
		"send_mode":      "scheduled",
		"scheduled_at":   sched.Format(time.RFC3339),
		"variants": []map[string]interface{}{
			{"variant_name": "A", "subject": "s1", "from_name": "Deal Desk", "html_content": "<b>x</b>"},
		},
		"isp_plans": []map[string]interface{}{
			{"isp": "gmail", "cadence": map[string]interface{}{"mode": "single"},
				"time_spans": []map[string]interface{}{
					{"type": "absolute", "start_at": sched.Format(time.RFC3339),
						"end_at": end.Format(time.RFC3339), "source": "duration-calc"},
				}},
		},
	}
	b, err := json.Marshal(map[string]interface{}{"campaign_input": input})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	return string(b)
}

// TestDayCardsRebuild_OfferOverrideLandsInInput: overrides.offer_id is
// validated against mailing_offers and lands on the sibling's deploy input.
func TestDayCardsRebuild_OfferOverrideLandsInInput(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	newID := "ffffffff-0000-0000-0000-000000000010"
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(dcCampID, dcOrg).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "sent_count", "pmta_config"}).
			AddRow("aug22-RB-gmail-CLK", "scheduled", 0, dcNoOfferBlob(t, "aug22-RB-gmail-CLK")))
	mock.ExpectQuery(`SELECT 1 FROM mailing_offers`).
		WithArgs(aoOfferID, dcOrg).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	mock.ExpectExec(`SET status = 'cancelled'`).
		WithArgs(dcCampID, dcOrg).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_queue`).
		WithArgs(dcCampID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	var deployedInput engine.PMTACampaignInput
	svc := &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployedInput = input
			mock.ExpectExec(`jsonb_set`).
				WithArgs(newID, dcCampID, dcOrg).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
				WillReturnResult(sqlmock.NewResult(1, 1))
			return newID, "finalizing_audience", false, nil
		}}

	rec := dcPostRebuild(t, svc,
		`{"campaign_id":"`+dcCampID+`","confirmed":true,"overrides":{"offer_id":"`+aoOfferID+`"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if deployedInput.OfferID != aoOfferID {
		t.Fatalf("overrides.offer_id must land on the sibling input, got %q", deployedInput.OfferID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestDayCardsRebuild_NoOfferConfirmedRefusedBeforeCancel: a confirmed
// rebuild whose input would fail the offer gate is refused 422 BEFORE the
// cancel — never the 502 half-state with the original already cancelled.
func TestDayCardsRebuild_NoOfferConfirmedRefusedBeforeCancel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// ONLY the load query — no cancel UPDATE may run.
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WithArgs(dcCampID, dcOrg).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "sent_count", "pmta_config"}).
			AddRow("aug22-RB-gmail-CLK", "scheduled", 0, dcNoOfferBlob(t, "aug22-RB-gmail-CLK")))

	deployCalled := false
	svc := &PMTACampaignService{db: db,
		dayCardsDeployFn: func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
			deployCalled = true
			return "", "", false, nil
		}}

	rec := dcPostRebuild(t, svc, `{"campaign_id":"`+dcCampID+`","confirmed":true}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "offer_id") {
		t.Fatalf("422 must name the offer gate: %s", rec.Body.String())
	}
	if deployCalled {
		t.Fatalf("deploy must not run for a gate-refused rebuild")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no cancel write may run before the gate: %v", err)
	}
}
