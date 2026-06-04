package api

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// TestCopyInPlanRecipientsTypes validates that the exact value types used by
// batchInsertPlanRecipients survive the pq.CopyIn driver conversion against the
// real mailing_campaign_plan_recipients column types. It runs only when
// COPY_VALIDATE_DSN is set, uses a TEMP table cloned from the real table (no
// FKs, no persistence), and rolls everything back. Manual/one-off; skipped in CI.
func TestCopyInPlanRecipientsTypes(t *testing.T) {
	dsn := os.Getenv("COPY_VALIDATE_DSN")
	if dsn == "" {
		t.Skip("COPY_VALIDATE_DSN not set; skipping live COPY validation")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE _pr_copytest (LIKE mailing_campaign_plan_recipients INCLUDING DEFAULTS)`); err != nil {
		t.Fatalf("create temp: %v", err)
	}

	stmt, err := tx.PrepareContext(ctx, pq.CopyIn(
		"_pr_copytest",
		"id", "campaign_id", "isp_plan_id", "subscriber_id", "email", "recipient_isp",
		"selection_rank", "audience_source_type", "audience_source_id", "status", "created_at",
	))
	if err != nil {
		t.Fatalf("prepare copy: %v", err)
	}

	createdAt := time.Now().UTC()
	// Row 1: non-nil source id. Row 2: nil source id (NULL path).
	rows := []struct {
		source interface{}
	}{
		{source: uuid.New()},
		{source: nil},
	}
	for i, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			uuid.New(), uuid.New(), uuid.New(),
			uuid.New(), "validate@example.com", "gmail",
			i+1, "segment", r.source,
			"selected", createdAt,
		); err != nil {
			stmt.Close()
			t.Fatalf("buffer row %d: %v", i, err)
		}
	}
	if _, err := stmt.ExecContext(ctx); err != nil {
		stmt.Close()
		t.Fatalf("flush: %v", err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM _pr_copytest`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(rows) {
		t.Fatalf("expected %d rows copied, got %d", len(rows), n)
	}
	t.Logf("COPY validated: %d rows (uuid.UUID, nil source, time.Time) round-tripped against real column types", n)
}
