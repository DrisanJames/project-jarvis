package preferences

// Real-Postgres check of the store SQL (sqlmock never types SQL — see memory
// feedback-shadow-first-send-path-automation). Skipped unless
// PREFS_TEST_PG_DSN points at a DISPOSABLE database (local apex-postgres);
// everything runs inside one transaction that is rolled back.

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// StartupDDL mirrors the runStartupMigrations entries in cmd/server/main.go.
var startupDDL = []string{
	`CREATE TABLE IF NOT EXISTS mailing_subscriber_preferences (
		id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		organization_id UUID NOT NULL,
		email_hash      TEXT NOT NULL,
		brand_root      TEXT,
		paused_until    TIMESTAMPTZ,
		frequency       TEXT NOT NULL DEFAULT 'normal' CHECK (frequency IN ('normal','reduced','weekly')),
		topics          JSONB NOT NULL DEFAULT '{}'::jsonb,
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		source          TEXT
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriber_prefs_org_hash_brand
		ON mailing_subscriber_preferences (organization_id, email_hash, (COALESCE(brand_root, '')))`,
}

// txDB adapts a *sql.Tx to the *sql.DB-shaped calls the store makes, by
// running them on a single-connection pool pinned inside the transaction.
func TestStoreSQL_RealPostgres(t *testing.T) {
	dsn := os.Getenv("PREFS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("PREFS_TEST_PG_DSN not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // every statement below shares the one session
	ctx := context.Background()
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	mustExec("BEGIN")
	defer db.ExecContext(ctx, "ROLLBACK")
	for _, q := range startupDDL {
		mustExec(q)
	}
	// Idempotent: the second boot re-runs both.
	for _, q := range startupDDL {
		mustExec(q)
	}

	h := NewHub(db, tOrg)
	hash := EmailHash("human@example.com")
	until := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)

	// all-brands pause, then overwrite it (ON CONFLICT on the expression index)
	if err := Upsert(ctx, db, h, Preference{OrgID: tOrg, EmailHash: hash, PausedUntil: &until, Frequency: FrequencyNormal, Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(ctx, db, h, Preference{OrgID: tOrg, EmailHash: hash, PausedUntil: &until, Frequency: FrequencyWeekly, Source: "test"}); err != nil {
		t.Fatal(err)
	}
	// a brand row alongside it, with a topic and no pause (nil param typing)
	if err := Upsert(ctx, db, h, Preference{OrgID: tOrg, EmailHash: hash, BrandRoot: "discountblog.com", Frequency: FrequencyReduced, Topics: map[string]bool{"loans": false}, Source: "test"}); err != nil {
		t.Fatal(err)
	}

	rows, err := Get(ctx, db, tOrg, hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (all-brands + brand), got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		switch r.BrandRoot {
		case "":
			if r.Frequency != FrequencyWeekly || r.PausedUntil == nil || !r.PausedUntil.Equal(until) {
				t.Fatalf("all-brands row not overwritten: %+v", r)
			}
		case "discountblog.com":
			if r.PausedUntil != nil || r.Topics["loans"] != false || len(r.Topics) != 1 {
				t.Fatalf("brand row wrong: %+v", r)
			}
		default:
			t.Fatalf("unexpected row %+v", r)
		}
	}

	fresh := NewHub(db, tOrg)
	if err := fresh.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if fresh.Status().LoadedRows != 2 {
		t.Fatalf("load: want 2 rows (pause + topic opt-out), got %d", fresh.Status().LoadedRows)
	}
	if ok, r := fresh.ShouldSend("human@example.com", "quizfiesta.com", "", time.Now()); ok || r != "paused" {
		t.Fatalf("loaded hub: ok=%v reason=%q", ok, r)
	}
}
