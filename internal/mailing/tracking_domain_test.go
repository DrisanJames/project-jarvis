package mailing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestValidateDomainFormat(t *testing.T) {
	tests := []struct {
		name    string
		domain  string
		wantErr bool
	}{
		{"valid subdomain", "track.example.com", false},
		{"valid multi-subdomain", "mail.track.example.com", false},
		{"valid with numbers", "track1.example2.com", false},
		{"valid with hyphen", "my-track.example.com", false},
		{"empty domain", "", true},
		{"no dot", "localhost", true},
		{"starts with hyphen", "-track.example.com", true},
		{"ends with hyphen", "track-.example.com", true},
		{"invalid character underscore", "track_test.example.com", true},
		{"invalid character space", "track test.example.com", true},
		{"empty label", "track..example.com", true},
		{"label too long", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmn.example.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDomainFormat(tt.domain)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateDomainFormat(%q) error = %v, wantErr %v", tt.domain, err, tt.wantErr)
			}
		})
	}
}

func TestGenerateVerificationToken(t *testing.T) {
	token1, err := generateVerificationToken()
	if err != nil {
		t.Fatalf("generateVerificationToken() error = %v", err)
	}

	if len(token1) != 32 {
		t.Errorf("generateVerificationToken() token length = %d, want 32", len(token1))
	}

	// Verify tokens are unique
	token2, err := generateVerificationToken()
	if err != nil {
		t.Fatalf("generateVerificationToken() error = %v", err)
	}

	if token1 == token2 {
		t.Error("generateVerificationToken() generated duplicate tokens")
	}
}

func TestDNSRecordsJSON_Scan(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		want     int
		wantErr  bool
	}{
		{
			name:    "nil input",
			input:   nil,
			want:    0,
			wantErr: false,
		},
		{
			name:    "valid json",
			input:   []byte(`[{"type":"CNAME","name":"track.example.com","value":"tracking.platform.com","status":"pending"}]`),
			want:    1,
			wantErr: false,
		},
		{
			name:    "empty array",
			input:   []byte(`[]`),
			want:    0,
			wantErr: false,
		},
		{
			name:    "invalid json",
			input:   []byte(`{invalid}`),
			want:    0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d DNSRecordsJSON
			err := d.Scan(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("DNSRecordsJSON.Scan() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(d) != tt.want {
				t.Errorf("DNSRecordsJSON.Scan() len = %d, want %d", len(d), tt.want)
			}
		})
	}
}

func TestTrackingDomainService_RegisterDomain(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock: %v", err)
	}
	defer db.Close()

	service := NewTrackingDomainService(db, "tracking.platform.com", "https://tracking.platform.com")

	t.Run("successful registration", func(t *testing.T) {
		// Expect check for existing domain
		mock.ExpectQuery("SELECT id FROM mailing_tracking_domains WHERE domain").
			WithArgs("track.example.com").
			WillReturnError(sql.ErrNoRows)

		// Expect insert
		mock.ExpectExec("INSERT INTO mailing_tracking_domains").
			WillReturnResult(sqlmock.NewResult(1, 1))

		domain, err := service.RegisterDomain(context.Background(), "org-123", "track.example.com")
		if err != nil {
			t.Errorf("RegisterDomain() error = %v", err)
			return
		}

		if domain.Domain != "track.example.com" {
			t.Errorf("RegisterDomain() domain = %s, want track.example.com", domain.Domain)
		}

		if domain.Verified {
			t.Error("RegisterDomain() domain should not be verified initially")
		}

		if len(domain.DNSRecords) != 2 {
			t.Errorf("RegisterDomain() DNS records count = %d, want 2", len(domain.DNSRecords))
		}

		// Verify DNS records structure
		hasCNAME := false
		hasTXT := false
		for _, record := range domain.DNSRecords {
			if record.Type == "CNAME" && record.Status == "pending" {
				hasCNAME = true
			}
			if record.Type == "TXT" && record.Status == "pending" {
				hasTXT = true
			}
		}

		if !hasCNAME || !hasTXT {
			t.Error("RegisterDomain() missing required DNS records")
		}
	})

	t.Run("domain already exists", func(t *testing.T) {
		mock.ExpectQuery("SELECT id FROM mailing_tracking_domains WHERE domain").
			WithArgs("existing.example.com").
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("existing-id"))

		_, err := service.RegisterDomain(context.Background(), "org-123", "existing.example.com")
		if err == nil {
			t.Error("RegisterDomain() expected error for existing domain")
		}
	})

	t.Run("invalid domain format", func(t *testing.T) {
		_, err := service.RegisterDomain(context.Background(), "org-123", "invalid_domain")
		if err == nil {
			t.Error("RegisterDomain() expected error for invalid domain")
		}
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %s", err)
	}
}

func TestTrackingDomainService_GetOrgDomains(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock: %v", err)
	}
	defer db.Close()

	service := NewTrackingDomainService(db, "tracking.platform.com", "https://tracking.platform.com")

	t.Run("returns domains", func(t *testing.T) {
		dnsRecords := []DNSRecord{
			{Type: "CNAME", Name: "track.example.com", Value: "tracking.platform.com", Status: "verified"},
		}
		dnsRecordsJSON, _ := json.Marshal(dnsRecords)

		rows := sqlmock.NewRows([]string{
			"id", "org_id", "domain", "verified", "ssl_provisioned", "ssl_status",
			"cloudfront_id", "cloudfront_domain", "acm_cert_arn", "origin_server",
			"dns_records", "created_at", "updated_at",
		}).AddRow(
			"domain-1", "org-123", "track.example.com", true, true, "provisioned",
			"", "", "", "",
			dnsRecordsJSON, time.Now(), time.Now(),
		)

		mock.ExpectQuery("SELECT id, org_id, domain, verified, ssl_provisioned, COALESCE").
			WithArgs("org-123").
			WillReturnRows(rows)

		domains, err := service.GetOrgDomains(context.Background(), "org-123")
		if err != nil {
			t.Errorf("GetOrgDomains() error = %v", err)
			return
		}

		if len(domains) != 1 {
			t.Errorf("GetOrgDomains() count = %d, want 1", len(domains))
			return
		}

		if domains[0].Domain != "track.example.com" {
			t.Errorf("GetOrgDomains() domain = %s, want track.example.com", domains[0].Domain)
		}

		if !domains[0].Verified {
			t.Error("GetOrgDomains() domain should be verified")
		}
	})

	t.Run("returns empty list", func(t *testing.T) {
		rows := sqlmock.NewRows([]string{
			"id", "org_id", "domain", "verified", "ssl_provisioned", "ssl_status",
			"cloudfront_id", "cloudfront_domain", "acm_cert_arn", "origin_server",
			"dns_records", "created_at", "updated_at",
		})

		mock.ExpectQuery("SELECT id, org_id, domain, verified, ssl_provisioned, COALESCE").
			WithArgs("org-456").
			WillReturnRows(rows)

		domains, err := service.GetOrgDomains(context.Background(), "org-456")
		if err != nil {
			t.Errorf("GetOrgDomains() error = %v", err)
			return
		}

		if domains != nil && len(domains) != 0 {
			t.Errorf("GetOrgDomains() count = %d, want 0", len(domains))
		}
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %s", err)
	}
}

func TestTrackingDomainService_GetTrackingURL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock: %v", err)
	}
	defer db.Close()

	service := NewTrackingDomainService(db, "tracking.platform.com", "https://tracking.platform.com")

	t.Run("returns custom domain with SSL", func(t *testing.T) {
		mock.ExpectQuery("SELECT domain, ssl_provisioned, ssl_status FROM mailing_tracking_domains").
			WithArgs("org-123").
			WillReturnRows(sqlmock.NewRows([]string{"domain", "ssl_provisioned", "ssl_status"}).
				AddRow("track.example.com", true, "provisioned"))

		url, err := service.GetTrackingURL(context.Background(), "org-123")
		if err != nil {
			t.Errorf("GetTrackingURL() error = %v", err)
			return
		}

		expected := "https://track.example.com"
		if url != expected {
			t.Errorf("GetTrackingURL() = %s, want %s", url, expected)
		}
	})

	t.Run("returns custom domain without SSL", func(t *testing.T) {
		mock.ExpectQuery("SELECT domain, ssl_provisioned, ssl_status FROM mailing_tracking_domains").
			WithArgs("org-456").
			WillReturnRows(sqlmock.NewRows([]string{"domain", "ssl_provisioned", "ssl_status"}).
				AddRow("track.example.com", false, ""))

		url, err := service.GetTrackingURL(context.Background(), "org-456")
		if err != nil {
			t.Errorf("GetTrackingURL() error = %v", err)
			return
		}

		expected := "http://track.example.com"
		if url != expected {
			t.Errorf("GetTrackingURL() = %s, want %s", url, expected)
		}
	})

	t.Run("returns default when no custom domain", func(t *testing.T) {
		mock.ExpectQuery("SELECT domain, ssl_provisioned, ssl_status FROM mailing_tracking_domains").
			WithArgs("org-789").
			WillReturnError(sql.ErrNoRows)

		url, err := service.GetTrackingURL(context.Background(), "org-789")
		if err != nil {
			t.Errorf("GetTrackingURL() error = %v", err)
			return
		}

		expected := "https://tracking.platform.com"
		if url != expected {
			t.Errorf("GetTrackingURL() = %s, want %s", url, expected)
		}
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %s", err)
	}
}

func TestTrackingDomainService_DeleteDomain(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock: %v", err)
	}
	defer db.Close()

	service := NewTrackingDomainService(db, "tracking.platform.com", "https://tracking.platform.com")

	t.Run("successful deletion", func(t *testing.T) {
		mock.ExpectExec("DELETE FROM mailing_tracking_domains WHERE id").
			WithArgs("domain-123").
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.DeleteDomain(context.Background(), "domain-123")
		if err != nil {
			t.Errorf("DeleteDomain() error = %v", err)
		}
	})

	t.Run("domain not found", func(t *testing.T) {
		mock.ExpectExec("DELETE FROM mailing_tracking_domains WHERE id").
			WithArgs("nonexistent").
			WillReturnResult(sqlmock.NewResult(0, 0))

		err := service.DeleteDomain(context.Background(), "nonexistent")
		if err == nil {
			t.Error("DeleteDomain() expected error for nonexistent domain")
		}
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %s", err)
	}
}

func TestTrackingDomainService_CheckDomainOwnership(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock: %v", err)
	}
	defer db.Close()

	service := NewTrackingDomainService(db, "tracking.platform.com", "https://tracking.platform.com")

	t.Run("domain owned by org", func(t *testing.T) {
		mock.ExpectQuery("SELECT COUNT").
			WithArgs("domain-123", "org-123").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

		owned, err := service.CheckDomainOwnership(context.Background(), "domain-123", "org-123")
		if err != nil {
			t.Errorf("CheckDomainOwnership() error = %v", err)
			return
		}

		if !owned {
			t.Error("CheckDomainOwnership() = false, want true")
		}
	})

	t.Run("domain not owned by org", func(t *testing.T) {
		mock.ExpectQuery("SELECT COUNT").
			WithArgs("domain-123", "org-456").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

		owned, err := service.CheckDomainOwnership(context.Background(), "domain-123", "org-456")
		if err != nil {
			t.Errorf("CheckDomainOwnership() error = %v", err)
			return
		}

		if owned {
			t.Error("CheckDomainOwnership() = true, want false")
		}
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %s", err)
	}
}

func TestDNSRecord_Structure(t *testing.T) {
	record := DNSRecord{
		Type:   "CNAME",
		Name:   "track.example.com",
		Value:  "tracking.platform.com",
		Status: "pending",
	}

	// Test JSON serialization
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Failed to marshal DNSRecord: %v", err)
	}

	var decoded DNSRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal DNSRecord: %v", err)
	}

	if decoded.Type != record.Type {
		t.Errorf("DNSRecord.Type = %s, want %s", decoded.Type, record.Type)
	}
	if decoded.Name != record.Name {
		t.Errorf("DNSRecord.Name = %s, want %s", decoded.Name, record.Name)
	}
	if decoded.Value != record.Value {
		t.Errorf("DNSRecord.Value = %s, want %s", decoded.Value, record.Value)
	}
	if decoded.Status != record.Status {
		t.Errorf("DNSRecord.Status = %s, want %s", decoded.Status, record.Status)
	}
}

func TestTrackingDomain_Structure(t *testing.T) {
	now := time.Now()
	domain := TrackingDomain{
		ID:             "domain-123",
		OrgID:          "org-456",
		Domain:         "track.example.com",
		Verified:       true,
		SSLProvisioned: true,
		DNSRecords: []DNSRecord{
			{Type: "CNAME", Name: "track.example.com", Value: "tracking.platform.com", Status: "verified"},
			{Type: "TXT", Name: "_verify.track.example.com", Value: "verify=abc123", Status: "verified"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Test JSON serialization
	data, err := json.Marshal(domain)
	if err != nil {
		t.Fatalf("Failed to marshal TrackingDomain: %v", err)
	}

	var decoded TrackingDomain
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal TrackingDomain: %v", err)
	}

	if decoded.ID != domain.ID {
		t.Errorf("TrackingDomain.ID = %s, want %s", decoded.ID, domain.ID)
	}
	if decoded.Domain != domain.Domain {
		t.Errorf("TrackingDomain.Domain = %s, want %s", decoded.Domain, domain.Domain)
	}
	if len(decoded.DNSRecords) != len(domain.DNSRecords) {
		t.Errorf("TrackingDomain.DNSRecords length = %d, want %d", len(decoded.DNSRecords), len(domain.DNSRecords))
	}
}
