package worker

import (
	"strings"
	"testing"
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
