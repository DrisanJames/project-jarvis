package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestResolveProfileSES_ViaSESTrue confirms a profile flagged with via_ses=true
// returns the configuration set + tenant name. This is the gate that makes
// send_worker.go inject X-SES-CONFIGURATION-SET / X-SES-TENANT and skip
// the internal /track/click + /track/open rewrites.
func TestResolveProfileSES_ViaSESTrue(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	pool := &SendWorkerPool{
		db:              db,
		profileSESCache: make(map[string]profileSESInfo),
	}

	pid := "ses-tenant-profile-id"
	rows := sqlmock.NewRows([]string{"via_ses", "ses_configuration_set", "ses_tenant_name", "raw_creative"}).
		AddRow(true, "discountblog", "discountblog", false)
	mock.ExpectQuery("SELECT COALESCE\\(via_ses, FALSE\\), COALESCE\\(ses_configuration_set, ''\\), COALESCE\\(ses_tenant_name, ''\\)").
		WithArgs(pid).
		WillReturnRows(rows)

	got := pool.resolveProfileSES(context.Background(), pid)
	if !got.ViaSES {
		t.Errorf("ViaSES = false, want true")
	}
	if got.ConfigSet != "discountblog" {
		t.Errorf("ConfigSet = %q, want discountblog", got.ConfigSet)
	}
	if got.TenantName != "discountblog" {
		t.Errorf("TenantName = %q, want discountblog", got.TenantName)
	}

	// Second call must hit the cache (no new query expected).
	got2 := pool.resolveProfileSES(context.Background(), pid)
	if got2 != got {
		t.Errorf("second call returned different value: %+v vs %+v", got2, got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestResolveProfileSES_DefaultPath confirms the default path (Dedicated)
// returns ViaSES=false. Without this guarantee, every existing campaign would
// silently flip to SES routing the moment the column is added — a critical
// regression.
func TestResolveProfileSES_DefaultPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	pool := &SendWorkerPool{
		db:              db,
		profileSESCache: make(map[string]profileSESInfo),
	}

	pid := "dedicated-profile-id"
	rows := sqlmock.NewRows([]string{"via_ses", "ses_configuration_set", "ses_tenant_name", "raw_creative"}).
		AddRow(false, "", "", false)
	mock.ExpectQuery("SELECT COALESCE\\(via_ses, FALSE\\)").
		WithArgs(pid).
		WillReturnRows(rows)

	got := pool.resolveProfileSES(context.Background(), pid)
	if got.ViaSES {
		t.Errorf("ViaSES = true, want false (Dedicated path)")
	}
	if got.ConfigSet != "" {
		t.Errorf("ConfigSet = %q, want empty", got.ConfigSet)
	}
	if got.TenantName != "" {
		t.Errorf("TenantName = %q, want empty", got.TenantName)
	}
}

// TestResolveProfileSES_PreMigrationFallback covers the boot window between
// a deploy that adds the new code and the runStartupMigrations applying the
// new columns. During that window the SELECT will fail with `column "via_ses"
// does not exist`. The handler must absorb the error and return the default
// (Dedicated) path so live sends are not interrupted.
func TestResolveProfileSES_PreMigrationFallback(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	pool := &SendWorkerPool{
		db:              db,
		profileSESCache: make(map[string]profileSESInfo),
	}

	pid := "any-profile"
	mock.ExpectQuery("SELECT COALESCE\\(via_ses, FALSE\\)").
		WithArgs(pid).
		WillReturnError(errors.New(`pq: column "via_ses" does not exist`))

	got := pool.resolveProfileSES(context.Background(), pid)
	if got.ViaSES {
		t.Errorf("pre-migration error must NOT activate SES path; ViaSES = true")
	}
}

// TestResolveProfileSES_EmptyID short-circuits to default without DB.
func TestResolveProfileSES_EmptyID(t *testing.T) {
	pool := &SendWorkerPool{profileSESCache: make(map[string]profileSESInfo)}
	got := pool.resolveProfileSES(context.Background(), "")
	if got.ViaSES {
		t.Errorf("empty profileID must return default (ViaSES=false)")
	}
}
