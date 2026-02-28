package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

const (
	listID            = "f492768b-ea52-4e22-813a-16deb2b5c261"
	expectedCount     = 52183541
	firstHash         = "0000031e53065df5edf6218cbc938d93"
	lastHash          = "fffffe4512b746da14dc76a7bf7794e3"
	expectedListName  = "Sams Club Suppression"
)

type checkResult struct {
	Name    string
	Passed  bool
	Detail  string
	Elapsed time.Duration
}

func main() {
	host := envOrDefault("DB_HOST", "localhost")
	port := envOrDefault("DB_PORT", "5432")
	user := envOrDefault("DB_USER", "ignite")
	pass := envOrDefault("DB_PASSWORD", "ignite_secret")
	dbname := envOrDefault("DB_NAME", "ignite")

	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", host, port, user, pass, dbname)

	fmt.Println("=========================================================")
	fmt.Println(" Sams Club Suppression File Upload Verification")
	fmt.Println("=========================================================")
	fmt.Printf("Target list_id:     %s\n", listID)
	fmt.Printf("Expected count:     %d\n", expectedCount)
	fmt.Printf("First hash:         %s\n", firstHash)
	fmt.Printf("Last hash:          %s\n", lastHash)
	fmt.Println("---------------------------------------------------------")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	db.SetMaxOpenConns(3)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot connect to database: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓ Database connection established")
	fmt.Println()

	var results []checkResult

	// Check 1: Suppression list exists with correct metadata
	results = append(results, checkListExists(ctx, db))

	// Check 2: entry_count matches expected value
	results = append(results, checkEntryCount(ctx, db))

	// Check 3: Actual row count in mailing_suppression_entries
	results = append(results, checkActualRowCount(ctx, db))

	// Check 4: entry_count matches actual row count
	results = append(results, checkCountsMatch(ctx, db))

	// Check 5: First hash exists
	results = append(results, checkHashExists(ctx, db, "first", firstHash))

	// Check 6: Last hash exists
	results = append(results, checkHashExists(ctx, db, "last", lastHash))

	// Check 7: Index on md5_hash column
	results = append(results, checkIndexExists(ctx, db, "md5_hash"))

	// Check 8: Index on list_id column
	results = append(results, checkIndexExists(ctx, db, "list_id"))

	// Print report
	fmt.Println()
	fmt.Println("=========================================================")
	fmt.Println(" VERIFICATION REPORT")
	fmt.Println("=========================================================")

	allPassed := true
	for i, r := range results {
		status := "PASS ✓"
		if !r.Passed {
			status = "FAIL ✗"
			allPassed = false
		}
		fmt.Printf("  [%d] %-45s %s  (%s)\n", i+1, r.Name, status, r.Elapsed.Round(time.Millisecond))
		if r.Detail != "" {
			for _, line := range strings.Split(r.Detail, "\n") {
				fmt.Printf("      %s\n", line)
			}
		}
	}

	fmt.Println("=========================================================")
	if allPassed {
		fmt.Println("  OVERALL: PASS ✓  — All verifications succeeded")
		fmt.Println("=========================================================")
		os.Exit(0)
	} else {
		fmt.Println("  OVERALL: FAIL ✗  — One or more verifications failed")
		fmt.Println("=========================================================")
		os.Exit(1)
	}
}

func checkListExists(ctx context.Context, db *sql.DB) checkResult {
	start := time.Now()
	name := "Suppression list exists"

	var listName string
	var orgID string
	err := db.QueryRowContext(ctx,
		`SELECT name, organization_id FROM mailing_suppression_lists WHERE id = $1`, listID,
	).Scan(&listName, &orgID)

	if err == sql.ErrNoRows {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("No suppression list found with id %s", listID), Elapsed: time.Since(start)}
	}
	if err != nil {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Query error: %v", err), Elapsed: time.Since(start)}
	}

	detail := fmt.Sprintf("name=%q, org_id=%s", listName, orgID)
	return checkResult{Name: name, Passed: true, Detail: detail, Elapsed: time.Since(start)}
}

func checkEntryCount(ctx context.Context, db *sql.DB) checkResult {
	start := time.Now()
	name := fmt.Sprintf("List entry_count = %d", expectedCount)

	var entryCount int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(entry_count, 0) FROM mailing_suppression_lists WHERE id = $1`, listID,
	).Scan(&entryCount)

	if err != nil {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Query error: %v", err), Elapsed: time.Since(start)}
	}

	passed := entryCount == int64(expectedCount)
	detail := fmt.Sprintf("entry_count=%d, expected=%d", entryCount, expectedCount)
	if !passed {
		diff := entryCount - int64(expectedCount)
		detail += fmt.Sprintf(" (diff=%+d)", diff)
	}
	return checkResult{Name: name, Passed: passed, Detail: detail, Elapsed: time.Since(start)}
}

func checkActualRowCount(ctx context.Context, db *sql.DB) checkResult {
	start := time.Now()
	name := "Actual row count in mailing_suppression_entries"

	fmt.Printf("  Counting rows (this may take a while for %d rows)...\n", expectedCount)

	var actualCount int64
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = $1`, listID,
	).Scan(&actualCount)

	if err != nil {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Query error: %v", err), Elapsed: time.Since(start)}
	}

	passed := actualCount == int64(expectedCount)
	detail := fmt.Sprintf("actual_rows=%d, expected=%d", actualCount, expectedCount)
	if !passed {
		diff := actualCount - int64(expectedCount)
		detail += fmt.Sprintf(" (diff=%+d)", diff)
	}
	return checkResult{Name: name, Passed: passed, Detail: detail, Elapsed: time.Since(start)}
}

func checkCountsMatch(ctx context.Context, db *sql.DB) checkResult {
	start := time.Now()
	name := "entry_count matches actual row count"

	var entryCount, actualCount int64

	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(entry_count, 0) FROM mailing_suppression_lists WHERE id = $1`, listID,
	).Scan(&entryCount)
	if err != nil {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Query error (entry_count): %v", err), Elapsed: time.Since(start)}
	}

	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = $1`, listID,
	).Scan(&actualCount)
	if err != nil {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Query error (COUNT): %v", err), Elapsed: time.Since(start)}
	}

	passed := entryCount == actualCount
	detail := fmt.Sprintf("entry_count=%d, actual_rows=%d", entryCount, actualCount)
	if !passed {
		diff := entryCount - actualCount
		detail += fmt.Sprintf(" (diff=%+d)", diff)
	}
	return checkResult{Name: name, Passed: passed, Detail: detail, Elapsed: time.Since(start)}
}

func checkHashExists(ctx context.Context, db *sql.DB, label string, hash string) checkResult {
	start := time.Now()
	name := fmt.Sprintf("Spot-check %s hash (%s)", label, hash)

	var exists bool
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM mailing_suppression_entries WHERE list_id = $1 AND md5_hash = $2)`,
		listID, hash,
	).Scan(&exists)

	if err != nil {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Query error: %v", err), Elapsed: time.Since(start)}
	}

	if !exists {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Hash %s NOT found in table", hash), Elapsed: time.Since(start)}
	}
	return checkResult{Name: name, Passed: true, Detail: "Hash found", Elapsed: time.Since(start)}
}

func checkIndexExists(ctx context.Context, db *sql.DB, columnName string) checkResult {
	start := time.Now()
	name := fmt.Sprintf("Index exists on %s column", columnName)

	query := `
		SELECT indexname, indexdef
		FROM pg_indexes
		WHERE tablename = 'mailing_suppression_entries'
		  AND indexdef ILIKE '%' || $1 || '%'
		ORDER BY indexname
	`

	rows, err := db.QueryContext(ctx, query, columnName)
	if err != nil {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Query error: %v", err), Elapsed: time.Since(start)}
	}
	defer rows.Close()

	var indexes []string
	for rows.Next() {
		var idxName, idxDef string
		if err := rows.Scan(&idxName, &idxDef); err != nil {
			return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Scan error: %v", err), Elapsed: time.Since(start)}
		}
		indexes = append(indexes, fmt.Sprintf("%s: %s", idxName, idxDef))
	}
	if err := rows.Err(); err != nil {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("Rows error: %v", err), Elapsed: time.Since(start)}
	}

	if len(indexes) == 0 {
		return checkResult{Name: name, Passed: false, Detail: fmt.Sprintf("No index found covering column %s", columnName), Elapsed: time.Since(start)}
	}

	detail := fmt.Sprintf("Found %d index(es):\n%s", len(indexes), strings.Join(indexes, "\n"))
	return checkResult{Name: name, Passed: true, Detail: detail, Elapsed: time.Since(start)}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
