package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func getTestDB(t *testing.T) *sql.DB {
	t.Helper()

	host := envOrDefault("TEST_DB_HOST", "localhost")
	port := envOrDefault("TEST_DB_PORT", "5432")
	user := envOrDefault("TEST_DB_USER", "ignite")
	pass := envOrDefault("TEST_DB_PASSWORD", "ignite_secret")
	dbname := envOrDefault("TEST_DB_NAME", "ignite")

	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", host, port, user, pass, dbname)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("skipping: cannot open database: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		t.Skipf("skipping: cannot connect to database: %v", err)
	}

	return db
}

func TestCheckListExists(t *testing.T) {
	db := getTestDB(t)
	defer db.Close()

	ctx := context.Background()
	result := checkListExists(ctx, db)

	t.Logf("checkListExists: passed=%v, detail=%s, elapsed=%s", result.Passed, result.Detail, result.Elapsed)
	// We don't assert pass/fail since it depends on data state;
	// we just verify it doesn't panic and returns a valid result.
	if result.Name == "" {
		t.Error("expected non-empty check name")
	}
}

func TestCheckEntryCount(t *testing.T) {
	db := getTestDB(t)
	defer db.Close()

	ctx := context.Background()
	result := checkEntryCount(ctx, db)

	t.Logf("checkEntryCount: passed=%v, detail=%s, elapsed=%s", result.Passed, result.Detail, result.Elapsed)
	if result.Name == "" {
		t.Error("expected non-empty check name")
	}
}

func TestCheckHashExists(t *testing.T) {
	db := getTestDB(t)
	defer db.Close()

	ctx := context.Background()

	t.Run("first_hash", func(t *testing.T) {
		result := checkHashExists(ctx, db, "first", firstHash)
		t.Logf("checkHashExists(first): passed=%v, detail=%s, elapsed=%s", result.Passed, result.Detail, result.Elapsed)
	})

	t.Run("last_hash", func(t *testing.T) {
		result := checkHashExists(ctx, db, "last", lastHash)
		t.Logf("checkHashExists(last): passed=%v, detail=%s, elapsed=%s", result.Passed, result.Detail, result.Elapsed)
	})

	t.Run("nonexistent_hash", func(t *testing.T) {
		result := checkHashExists(ctx, db, "fake", "00000000000000000000000000000000")
		t.Logf("checkHashExists(fake): passed=%v, detail=%s, elapsed=%s", result.Passed, result.Detail, result.Elapsed)
		// A completely fake hash should not exist
	})
}

func TestCheckIndexExists(t *testing.T) {
	db := getTestDB(t)
	defer db.Close()

	ctx := context.Background()

	t.Run("md5_hash", func(t *testing.T) {
		result := checkIndexExists(ctx, db, "md5_hash")
		t.Logf("checkIndexExists(md5_hash): passed=%v, detail=%s, elapsed=%s", result.Passed, result.Detail, result.Elapsed)
	})

	t.Run("list_id", func(t *testing.T) {
		result := checkIndexExists(ctx, db, "list_id")
		t.Logf("checkIndexExists(list_id): passed=%v, detail=%s, elapsed=%s", result.Passed, result.Detail, result.Elapsed)
	})
}

func TestEnvOrDefault(t *testing.T) {
	t.Run("returns_default_when_unset", func(t *testing.T) {
		val := envOrDefault("VERIFY_SUPPRESSION_TEST_NONEXISTENT_VAR", "fallback")
		if val != "fallback" {
			t.Errorf("expected 'fallback', got %q", val)
		}
	})

	t.Run("returns_env_when_set", func(t *testing.T) {
		os.Setenv("VERIFY_SUPPRESSION_TEST_VAR", "custom")
		defer os.Unsetenv("VERIFY_SUPPRESSION_TEST_VAR")

		val := envOrDefault("VERIFY_SUPPRESSION_TEST_VAR", "fallback")
		if val != "custom" {
			t.Errorf("expected 'custom', got %q", val)
		}
	})
}
