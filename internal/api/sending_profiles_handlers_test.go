package api

// Sending-profile SES-routing tests (SENDING-DOMAIN-ONBOARDING-AUDIT.md
// G1/G4/G5; contract tasks/eng-team/coalition/SENDING-DOMAIN-API-CONTRACT.md).
// sqlmock + httptest only — no DB, no AWS, no DNS (AS-7). Negative paths
// included per AS-7.1: the via_ses gate must be SHOWN tripping.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

const spTestOrg = "00000000-0000-0000-0000-000000000001"

func spRouter(svc *SendingProfileService) *chi.Mux {
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	return r
}

func newSPService(t *testing.T) (*SendingProfileService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSendingProfileService(db), mock
}

// ── G1 negative path: via_ses without routing fields must 400 ────────────

func TestCreateProfileViaSESMissingFields400(t *testing.T) {
	svc, mock := newSPService(t)

	body := `{"organization_id":"` + spTestOrg + `","name":"X","vendor_type":"pmta",
		"from_name":"X","from_email":"x@em.x.com","via_ses":true}`
	req := httptest.NewRequest(http.MethodPost, "/sending-profiles/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ses_configuration_set") {
		t.Fatalf("error should name the missing fields, got %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no SQL should run on a rejected create: %v", err)
	}
}

func TestCreateProfileInvalidRoutingMode400(t *testing.T) {
	svc, _ := newSPService(t)

	body := `{"organization_id":"` + spTestOrg + `","name":"X","vendor_type":"pmta",
		"from_name":"X","from_email":"x@em.x.com","routing_mode":"carrier-pigeon"}`
	req := httptest.NewRequest(http.MethodPost, "/sending-profiles/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// ── G1 positive path: create persists the four new columns ───────────────

func TestCreateProfileViaSESPersistsSESColumns(t *testing.T) {
	svc, mock := newSPService(t)

	mock.ExpectQuery(`INSERT INTO mailing_sending_profiles`).
		WithArgs(
			spTestOrg, "WCL", nil, "pmta", "WCL", "hello@em.wcl-heloc.com", nil,
			nil, nil, nil, nil, 587, nil, nil, "tls",
			"em.wcl-heloc.com", nil, "t.em.wcl-heloc.com", nil, 10000, 100000, false,
			true, "wcl-heloc", "wcl-heloc", nil,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("aaaaaaaa-0000-0000-0000-000000000001"))

	body := `{"organization_id":"` + spTestOrg + `","name":"WCL","vendor_type":"pmta",
		"from_name":"WCL","from_email":"hello@em.wcl-heloc.com",
		"sending_domain":"em.wcl-heloc.com","tracking_domain":"t.em.wcl-heloc.com",
		"via_ses":true,"ses_configuration_set":"wcl-heloc","ses_tenant_name":"wcl-heloc"}`
	req := httptest.NewRequest(http.MethodPost, "/sending-profiles/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ── G1 update: merged-state validation must trip ─────────────────────────

func TestUpdateProfileViaSESMergedValidation400(t *testing.T) {
	svc, mock := newSPService(t)
	const pid = "aaaaaaaa-0000-0000-0000-000000000001"

	// Current row has no SES fields; flipping via_ses on alone must 400.
	mock.ExpectQuery(`SELECT COALESCE\(via_ses, false\), ses_configuration_set, ses_tenant_name`).
		WithArgs(pid, spTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"via_ses", "cs", "tn", "dv", "dkv"}).AddRow(false, nil, nil, false, false))

	req := httptest.NewRequest(http.MethodPut, "/sending-profiles/"+pid, strings.NewReader(`{"via_ses":true}`))
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestUpdateProfileViaSESWithFieldsOK(t *testing.T) {
	svc, mock := newSPService(t)
	const pid = "aaaaaaaa-0000-0000-0000-000000000001"

	mock.ExpectQuery(`SELECT COALESCE\(via_ses, false\), ses_configuration_set, ses_tenant_name`).
		WithArgs(pid, spTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"via_ses", "cs", "tn", "dv", "dkv"}).AddRow(false, nil, nil, false, false))
	mock.ExpectExec(`UPDATE mailing_sending_profiles SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"via_ses":true,"ses_configuration_set":"wcl-heloc","ses_tenant_name":"wcl-heloc"}`
	req := httptest.NewRequest(http.MethodPut, "/sending-profiles/"+pid, strings.NewReader(body))
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ── DoD #4: activation guard — a via_ses profile cannot reach 'active'
//    without a passing SES verify (negative + positive + non-via_ses paths) ──

// Negative path: PUT status=active on an unverified via_ses row must 400 and
// run NO update.
func TestUpdateProfileViaSESActivateWithoutVerify400(t *testing.T) {
	svc, mock := newSPService(t)
	const pid = "aaaaaaaa-0000-0000-0000-000000000001"

	// Current row: via_ses=true but domain/dkim NOT verified.
	mock.ExpectQuery(`SELECT COALESCE\(via_ses, false\), ses_configuration_set, ses_tenant_name`).
		WithArgs(pid, spTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"via_ses", "cs", "tn", "dv", "dkv"}).AddRow(true, "wcl-heloc", "wcl-heloc", false, false))

	req := httptest.NewRequest(http.MethodPut, "/sending-profiles/"+pid, strings.NewReader(`{"status":"active"}`))
	req.Header.Set("X-Organization-ID", spTestOrg)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unverified via_ses cannot activate); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "verify") {
		t.Fatalf("error should name the verify precondition, got %s", rec.Body.String())
	}
	// The refusal must trip BEFORE any UPDATE — no UPDATE was expected.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no UPDATE may run on a refused activation: %v", err)
	}
}

// Positive path: PUT status=active on a verified via_ses row proceeds to UPDATE.
func TestUpdateProfileViaSESActivateAfterVerifyOK(t *testing.T) {
	svc, mock := newSPService(t)
	const pid = "aaaaaaaa-0000-0000-0000-000000000001"

	mock.ExpectQuery(`SELECT COALESCE\(via_ses, false\), ses_configuration_set, ses_tenant_name`).
		WithArgs(pid, spTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"via_ses", "cs", "tn", "dv", "dkv"}).AddRow(true, "wcl-heloc", "wcl-heloc", true, true))
	mock.ExpectExec(`UPDATE mailing_sending_profiles SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodPut, "/sending-profiles/"+pid, strings.NewReader(`{"status":"active"}`))
	req.Header.Set("X-Organization-ID", spTestOrg)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (verified via_ses may activate); body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Guard must NOT over-reach: a non-via_ses profile activates without any verify.
func TestUpdateProfileNonSESActivateAllowed(t *testing.T) {
	svc, mock := newSPService(t)
	const pid = "aaaaaaaa-0000-0000-0000-000000000001"

	mock.ExpectQuery(`SELECT COALESCE\(via_ses, false\), ses_configuration_set, ses_tenant_name`).
		WithArgs(pid, spTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"via_ses", "cs", "tn", "dv", "dkv"}).AddRow(false, nil, nil, false, false))
	mock.ExpectExec(`UPDATE mailing_sending_profiles SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodPut, "/sending-profiles/"+pid, strings.NewReader(`{"status":"active"}`))
	req.Header.Set("X-Organization-ID", spTestOrg)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (non-via_ses activation is unaffected); body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ── G4: list must not hide api_key-less ses/via_ses rows ─────────────────

func spListCols() []string {
	return []string{"id", "organization_id", "name", "description", "vendor_type",
		"from_name", "from_email", "reply_email",
		"sending_domain", "bounce_domain", "tracking_domain",
		"spf_verified", "dkim_verified", "dmarc_verified", "domain_verified", "credentials_verified",
		"last_verification_at", "verification_error",
		"hourly_limit", "daily_limit", "current_hourly_count", "current_daily_count",
		"ip_pool", "pool_prefix", "status", "is_default", "is_configured",
		"created_at", "updated_at",
		"smtp_host", "smtp_port", "smtp_username", "api_endpoint",
		"via_ses", "ses_configuration_set", "ses_tenant_name", "routing_mode",
		"raw_creative"}
}

func TestListProfilesIncludesSESVendorWithoutAPIKey(t *testing.T) {
	svc, mock := newSPService(t)
	now := time.Now()

	// The fixed predicate must include vendor_type IN ('pmta', 'smtp', 'ses')
	// and the via_ses escape hatch — assert on the SQL itself.
	mock.ExpectQuery(`vendor_type IN \('pmta', 'smtp', 'ses'\) OR COALESCE\(via_ses, false\) = true`).
		WithArgs(spTestOrg).
		WillReturnRows(sqlmock.NewRows(spListCols()).AddRow(
			"aaaaaaaa-0000-0000-0000-000000000001", spTestOrg, "WCL HELOC (SES Tenant)", nil, "ses",
			"WCL", "hello@em.wcl-heloc.com", nil,
			"em.wcl-heloc.com", nil, "t.em.wcl-heloc.com",
			false, false, false, false, false,
			nil, nil,
			1000, 25000, 0, 0,
			"wcl-heloc-ses-pool", nil, "draft", false, false,
			now, now,
			nil, 0, nil, nil,
			true, "wcl-heloc", "wcl-heloc", nil,
			false,
		))

	req := httptest.NewRequest(http.MethodGet, "/sending-profiles/", nil)
	req.Header.Set("X-Organization-ID", spTestOrg)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Profiles []SendingProfile `json:"profiles"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 1 || len(out.Profiles) != 1 {
		t.Fatalf("expected exactly the ses row, got total=%d", out.Total)
	}
	p := out.Profiles[0]
	if !p.ViaSES || p.SESConfigurationSet == nil || *p.SESConfigurationSet != "wcl-heloc" {
		t.Fatalf("SES fields not surfaced in list row: %+v", p)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ── G5: verify writes the four *_verified columns truthfully ─────────────

func TestVerifySESRouteWritesIdentityColumns(t *testing.T) {
	svc, mock := newSPService(t)
	const pid = "aaaaaaaa-0000-0000-0000-000000000001"

	svc.sesIdentity = func(ctx context.Context, domain string) (sesIdentityStatus, error) {
		if domain != "em.wcl-heloc.com" {
			t.Fatalf("verify called for wrong domain %q", domain)
		}
		return sesIdentityStatus{DKIMStatus: "SUCCESS", MailFromStatus: "SUCCESS", VerifiedForSending: true}, nil
	}
	svc.dmarcLookup = func(ctx context.Context, domain string) (bool, error) { return true, nil }

	mock.ExpectQuery(`SELECT vendor_type, COALESCE\(api_key, ''\), smtp_host, COALESCE\(via_ses, false\), sending_domain`).
		WithArgs(pid, spTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"vendor_type", "api_key", "smtp_host", "via_ses", "sending_domain"}).
			AddRow("pmta", "", nil, true, "em.wcl-heloc.com"))
	mock.ExpectExec(`UPDATE mailing_sending_profiles\s+SET credentials_verified`).
		WithArgs(true, true, true, true, true, sqlmock.AnyArg(), nil, "active", sqlmock.AnyArg(), pid, spTestOrg).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodPost, "/sending-profiles/"+pid+"/verify", nil)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"verified", "spf_verified", "dkim_verified", "dmarc_verified", "domain_verified"} {
		if v, ok := out[k].(bool); !ok || !v {
			t.Fatalf("%s should be true, body=%s", k, rec.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestVerifySESRouteAWSFailureIsTruthful(t *testing.T) {
	svc, mock := newSPService(t)
	const pid = "aaaaaaaa-0000-0000-0000-000000000001"

	svc.sesIdentity = func(ctx context.Context, domain string) (sesIdentityStatus, error) {
		return sesIdentityStatus{}, errors.New("AccessDenied")
	}
	svc.dmarcLookup = func(ctx context.Context, domain string) (bool, error) { return false, errors.New("nxdomain") }

	mock.ExpectQuery(`SELECT vendor_type, COALESCE\(api_key, ''\), smtp_host, COALESCE\(via_ses, false\), sending_domain`).
		WithArgs(pid, spTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"vendor_type", "api_key", "smtp_host", "via_ses", "sending_domain"}).
			AddRow("ses", "", nil, false, "em.wcl-heloc.com"))
	mock.ExpectExec(`UPDATE mailing_sending_profiles\s+SET credentials_verified`).
		WithArgs(false, false, false, false, false, sqlmock.AnyArg(), sqlmock.AnyArg(), "pending", sqlmock.AnyArg(), pid, spTestOrg).
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodPost, "/sending-profiles/"+pid+"/verify", nil)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (AWS failure must be a 200 verified=false); body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, _ := out["verified"].(bool); v {
		t.Fatalf("verified must be false on AWS failure, body=%s", rec.Body.String())
	}
	if e, _ := out["error"].(string); !strings.Contains(e, "AccessDenied") {
		t.Fatalf("error must carry the AWS failure, body=%s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
