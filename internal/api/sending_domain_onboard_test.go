package api

// Onboard-endpoint tests (audit Tier-3; contract §6). sqlmock only.
// Covers the transaction shape, the created payload, idempotency (AS-7.1
// negative path: re-POST must NOT create a second profile), and required-
// field rejection.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func onboardGetProfileCols() []string {
	return []string{"id", "organization_id", "name", "description", "vendor_type",
		"from_name", "from_email", "reply_email",
		"api_key", "api_endpoint",
		"smtp_host", "smtp_port", "smtp_username", "smtp_encryption",
		"sending_domain", "bounce_domain", "tracking_domain",
		"spf_verified", "dkim_verified", "dmarc_verified", "domain_verified", "credentials_verified",
		"last_verification_at", "verification_error",
		"hourly_limit", "daily_limit", "current_hourly_count", "current_daily_count",
		"ip_pool", "pool_prefix", "status", "is_default", "created_at", "updated_at",
		"via_ses", "ses_configuration_set", "ses_tenant_name", "routing_mode"}
}

func onboardProfileRow(now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows(onboardGetProfileCols()).AddRow(
		"aaaaaaaa-0000-0000-0000-000000000001", spTestOrg, "Wcl Heloc (SES Tenant)", nil, "pmta",
		"Wcl Heloc", "hello@em.wcl-heloc.com", "reply@em.wcl-heloc.com",
		nil, "http://15.204.101.125:19099",
		"15.204.101.125", 587, nil, "tls",
		"em.wcl-heloc.com", nil, "t.em.wcl-heloc.com",
		false, false, false, false, false,
		nil, nil,
		1000, 25000, 0, 0,
		"wcl-heloc-ses-pool", nil, "draft", false, now, now,
		true, "wcl-heloc", "wcl-heloc", nil,
	)
}

func TestOnboardSendingDomainCreatesEverything(t *testing.T) {
	svc, mock := newSPService(t)
	now := time.Now()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id::text`).
		WithArgs(spTestOrg, "em.wcl-heloc.com").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO mailing_sending_profiles`).
		WithArgs(spTestOrg, "Wcl Heloc (SES Tenant)", "Wcl Heloc",
			"hello@em.wcl-heloc.com", "reply@em.wcl-heloc.com",
			"em.wcl-heloc.com", "15.204.101.125", "http://15.204.101.125:19099", "t.em.wcl-heloc.com",
			1000, 25000, "wcl-heloc-ses-pool", "draft", "wcl-heloc", "wcl-heloc").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("aaaaaaaa-0000-0000-0000-000000000001"))
	mock.ExpectExec(`INSERT INTO mailing_sending_domains`).
		WithArgs(spTestOrg, "em.wcl-heloc.com").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_owned_domains`).
		WithArgs("wcl-heloc.com").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_segment_registry`).
		WithArgs(spTestOrg, "wcl-heloc", "WCL-HELOC-%", "sending-domain onboard (operator)", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT id, organization_id, name, description, vendor_type`).
		WithArgs("aaaaaaaa-0000-0000-0000-000000000001", spTestOrg).
		WillReturnRows(onboardProfileRow(now))

	body := `{"domain":"wcl-heloc.com","ses_configuration_set":"wcl-heloc","ses_tenant_name":"wcl-heloc"}`
	req := httptest.NewRequest(http.MethodPost, "/sending-domains/onboard", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", spTestOrg)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Created          bool           `json:"created"`
		Profile          SendingProfile `json:"profile"`
		OwnedDomain      string         `json:"owned_domain"`
		OwnedDomainAdded bool           `json:"owned_domain_added"`
		SegmentFamily    struct {
			FamilyKey     string `json:"family_key"`
			FamilyPattern string `json:"family_pattern"`
			Created       bool   `json:"created"`
		} `json:"segment_family"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Created || !out.OwnedDomainAdded || !out.SegmentFamily.Created {
		t.Fatalf("created flags wrong: %+v", out)
	}
	if out.OwnedDomain != "wcl-heloc.com" || out.SegmentFamily.FamilyPattern != "WCL-HELOC-%" {
		t.Fatalf("payload wrong: %+v", out)
	}
	if !out.Profile.ViaSES || out.Profile.Status != "draft" {
		t.Fatalf("profile must be via_ses draft: %+v", out.Profile)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestOnboardSendingDomainIdempotent(t *testing.T) {
	svc, mock := newSPService(t)
	now := time.Now()

	mock.ExpectBegin()
	// Probe finds the existing profile — NO INSERT INTO
	// mailing_sending_profiles may follow (the idempotency guarantee).
	mock.ExpectQuery(`SELECT id::text`).
		WithArgs(spTestOrg, "em.wcl-heloc.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "domain_verified", "dkim_verified"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", false, false))
	mock.ExpectExec(`INSERT INTO mailing_sending_domains`).
		WithArgs(spTestOrg, "em.wcl-heloc.com").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO mailing_owned_domains`).
		WithArgs("wcl-heloc.com").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO mailing_segment_registry`).
		WithArgs(spTestOrg, "wcl-heloc", "WCL-HELOC-%", "sending-domain onboard (operator)", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT id, organization_id, name, description, vendor_type`).
		WithArgs("aaaaaaaa-0000-0000-0000-000000000001", spTestOrg).
		WillReturnRows(onboardProfileRow(now))

	body := `{"domain":"wcl-heloc.com","ses_configuration_set":"wcl-heloc","ses_tenant_name":"wcl-heloc"}`
	req := httptest.NewRequest(http.MethodPost, "/sending-domains/onboard", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", spTestOrg)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on idempotent re-POST; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Created          bool `json:"created"`
		OwnedDomainAdded bool `json:"owned_domain_added"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Created || out.OwnedDomainAdded {
		t.Fatalf("re-POST must report created=false: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations (idempotency — nothing may be created twice): %v", err)
	}
}

// DoD #4 activation guard (onboard side): status=active on a brand-new domain
// must 400 — a freshly-onboarded via_ses profile cannot have passed verify, so
// only 'draft' is allowed on create. No profile INSERT may run.
func TestOnboardSendingDomainActiveWithoutVerify400(t *testing.T) {
	svc, mock := newSPService(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id::text`).
		WithArgs(spTestOrg, "em.wcl-heloc.com").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	body := `{"domain":"wcl-heloc.com","ses_configuration_set":"wcl-heloc","ses_tenant_name":"wcl-heloc","status":"active"}`
	req := httptest.NewRequest(http.MethodPost, "/sending-domains/onboard", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", spTestOrg)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (cannot onboard active before verify); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "verify") {
		t.Fatalf("error should name the verify precondition, got %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no profile INSERT may run on a refused active onboard: %v", err)
	}
}

// Positive path: re-onboarding an ALREADY-VERIFIED existing profile with
// status=active is allowed (created=false, no new profile INSERT).
func TestOnboardSendingDomainActiveAfterVerifyOK(t *testing.T) {
	svc, mock := newSPService(t)
	now := time.Now()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id::text`).
		WithArgs(spTestOrg, "em.wcl-heloc.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "domain_verified", "dkim_verified"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", true, true))
	mock.ExpectExec(`INSERT INTO mailing_sending_domains`).
		WithArgs(spTestOrg, "em.wcl-heloc.com").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO mailing_owned_domains`).
		WithArgs("wcl-heloc.com").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO mailing_segment_registry`).
		WithArgs(spTestOrg, "wcl-heloc", "WCL-HELOC-%", "sending-domain onboard (operator)", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT id, organization_id, name, description, vendor_type`).
		WithArgs("aaaaaaaa-0000-0000-0000-000000000001", spTestOrg).
		WillReturnRows(onboardProfileRow(now))

	body := `{"domain":"wcl-heloc.com","ses_configuration_set":"wcl-heloc","ses_tenant_name":"wcl-heloc","status":"active"}`
	req := httptest.NewRequest(http.MethodPost, "/sending-domains/onboard", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", spTestOrg)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (verified existing profile may be active); body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Created bool `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Created {
		t.Fatalf("re-onboard of existing profile must report created=false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestOnboardSendingDomainMissingFields400(t *testing.T) {
	svc, mock := newSPService(t)

	req := httptest.NewRequest(http.MethodPost, "/sending-domains/onboard",
		strings.NewReader(`{"domain":"wcl-heloc.com"}`))
	req.Header.Set("X-Organization-ID", spTestOrg)
	rec := httptest.NewRecorder()
	spRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no SQL may run on a rejected onboard: %v", err)
	}
}
