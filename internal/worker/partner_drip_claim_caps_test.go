package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A zero-capped KNOWN ISP class must stay in the caps CTE so the claim SQL keeps
// it in its own bucket (rn <= 0 ⇒ excluded) instead of re-bucketing it as
// 'other'. Regression for the 2026-08-27 gmail leak: brand-budget holds,
// dataset daily_cap=0, gmail brand routing and budget exhaustion all set a cap
// to 0, and dropping the row let those records claim under the 'other' cap.
func TestCapsValuesClauses_ZeroCapKnownISPStaysInCTE(t *testing.T) {
	clauses, args, positive := capsValuesClauses(map[string]int{"gmail": 0, "other": 400, "yahoo": -5}, 3)
	if len(clauses) != 3 {
		t.Fatalf("want 3 VALUES rows (zero and negative caps included), got %d: %v", len(clauses), clauses)
	}
	if positive != 1 {
		t.Fatalf("want exactly 1 positive class, got %d", positive)
	}
	if len(args) != 6 {
		t.Fatalf("want 6 flat args, got %d: %v", len(args), args)
	}
	seen := map[string]int{}
	for i := 0; i < len(args); i += 2 {
		seen[args[i].(string)] = args[i+1].(int)
	}
	if seen["gmail"] != 0 || seen["yahoo"] != 0 || seen["other"] != 400 {
		t.Fatalf("caps not preserved/clamped: %v", seen)
	}
	joined := strings.Join(clauses, ",")
	for _, ph := range []string{"$3", "$4", "$5", "$6", "$7", "$8"} {
		if !strings.Contains(joined, ph) {
			t.Fatalf("placeholder %s missing from %s", ph, joined)
		}
	}
}

func TestCapsValuesClauses_NoPositiveMeansNothingClaimable(t *testing.T) {
	clauses, _, positive := capsValuesClauses(map[string]int{"gmail": 0, "microsoft": 0}, 4)
	if positive != 0 {
		t.Fatalf("want 0 positive, got %d", positive)
	}
	if len(clauses) != 2 {
		t.Fatalf("zero-cap rows must still be emitted, got %d", len(clauses))
	}
}

// End-to-end on the built statement: a zero-capped KNOWN isp must reach the
// caps CTE, so the bucket CASE (IN (SELECT isp FROM caps)) keeps it in its own
// partition and `rn <= c.cap` with cap 0 admits nothing — instead of the class
// falling through to the 'other' bucket and claiming at the 'other' cap.
func TestClaimRecordsByISPCaps_ZeroCapKnownISPKeepsItsOwnBucket(t *testing.T) {
	var gotSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
			if strings.Contains(actualSQL, "WITH caps") {
				gotSQL = actualSQL
			}
			return nil
		})))
	require.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`WITH caps`).WillReturnRows(sqlmock.NewRows(
		[]string{"id", "email", "email_md5", "isp_family", "dataset_id", "partner_id", "batch_id", "extra_metadata"}))
	mock.ExpectCommit()

	// gmail held at 0 alongside a live aol cap — the exact shape the AOL
	// companion pass produces once the domain governor zeroes a cell.
	_, err = po.claimRecordsByISPCaps(context.Background(), "internal_auto_insurance_aol_rotate", "ht",
		map[string]int{"gmail": 0, "aol": 200}, 500)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	require.NotEmpty(t, gotSQL, "claim query was never issued")
	for _, ph := range []string{"$3", "$4", "$5", "$6"} {
		assert.Contains(t, gotSQL, ph, "both caps rows (including the zero-capped gmail) must be in the VALUES list")
	}
	assert.NotContains(t, gotSQL, "$7", "only the two supplied classes may be bound")
	assert.Contains(t, gotSQL, "IN (SELECT isp FROM caps)",
		"a class present in caps must bucket to itself, never to 'other'")
	assert.Contains(t, gotSQL, "r.rn <= c.cap",
		"a cap of 0 must exclude the class by rank, which is what makes the zero row load-bearing")
}
