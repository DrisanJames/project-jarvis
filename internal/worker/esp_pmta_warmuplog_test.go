package worker

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The 2026-07-29 SEV-1: mailing_ip_warmup_log.actual_sent sat at 0 across all
// 12,354 rows from 2026-03-18 because the counter's INSERT omitted the NOT NULL
// planned_volume/warmup_day columns. Postgres evaluates NOT NULL BEFORE the
// ON CONFLICT arbiter, so the statement errored 23502 on every send and the
// DO UPDATE never ran — silently disabling BOTH warm-up brakes:
//   - WarmupScheduler's 3%-bounce / 0.1%-complaint auto-pause (gates on
//     actual_sent > 10, internal/pmta/warmup.go)
//   - vmtaPool.next()'s per-IP daily cap (reads TodaySent)
//
// Reproduced on local dev postgres:
//
//	OLD: ERROR null value in column "planned_volume" violates not-null constraint
//	NEW: two executions -> actual_sent=2, planned_volume/warmup_day from the IP row
//
// These pins fail if either writer regresses to the column-omitting form or
// stops surfacing the error.
func warmupLogInsertsIn(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	re := regexp.MustCompile(`(?s)INSERT INTO mailing_ip_warmup_log.*?ON CONFLICT`)
	found := re.FindAllString(string(src), -1)
	if len(found) == 0 {
		t.Fatalf("%s: no mailing_ip_warmup_log INSERT found — did the counter move?", path)
	}
	return found
}

func TestWarmupLogInsertSuppliesNotNullColumns(t *testing.T) {
	for _, path := range []string{"esp_pmta_api.go", "esp_pmta.go"} {
		for _, stmt := range warmupLogInsertsIn(t, path) {
			for _, col := range []string{"planned_volume", "warmup_day"} {
				if !strings.Contains(stmt, col) {
					t.Errorf("%s: warmup-log INSERT omits NOT NULL column %q — "+
						"this is the 23502 defect that disabled both warm-up brakes:\n%s",
						path, col, stmt)
				}
			}
		}
	}
}

func TestWarmupLogInsertErrorIsSurfaced(t *testing.T) {
	// The api sender previously called ExecContext with NO error variable, so
	// four months of failures were invisible. Both writers must capture it.
	for _, path := range []string{"esp_pmta_api.go", "esp_pmta.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		s := string(src)
		idx := strings.Index(s, "INSERT INTO mailing_ip_warmup_log")
		if idx < 0 {
			t.Fatalf("%s: warmup-log INSERT not found", path)
		}
		window := s[max0(idx-260):idx]
		if !strings.Contains(window, "err") {
			t.Errorf("%s: warmup-log INSERT result is discarded — errors must be "+
				"captured and logged (the defect was silent for 4 months)", path)
		}
	}
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}
