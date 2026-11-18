package worker

import "testing"

func TestComcastUnknownAsValid(t *testing.T) {
	unknown := eoOutcome{kind: outcomeRetry, resultID: 11, result: "Unknown"}
	cases := []struct {
		name, isp, env string
		in             eoOutcome
		wantKind       eoOutcomeKind
		wantResult     string
		wantCode       int
	}{
		{"comcast unknown becomes valid", "comcast", "", unknown, outcomeReady, "Verified", 11},
		{"case and space tolerant", " Comcast ", "", unknown, outcomeReady, "Verified", 11},
		{"microsoft unknown still retries", "microsoft", "", unknown, outcomeRetry, "Unknown", 11},
		{"sbcglobal unknown still retries", "sbcglobal", "", unknown, outcomeRetry, "Unknown", 11},
		{"empty isp still retries", "", "", unknown, outcomeRetry, "Unknown", 11},
		{"comcast Retry code 0 still retries", "comcast", "", eoOutcome{kind: outcomeRetry, resultID: 0, result: "Retry"}, outcomeRetry, "Retry", 0},
		{"comcast transport error still retries", "comcast", "", eoOutcome{kind: outcomeRetry, result: "error: timeout"}, outcomeRetry, "error: timeout", 0},
		{"comcast undeliverable still suppressed", "comcast", "", eoOutcome{kind: outcomeSuppress, resultID: 2, result: "Undeliverable"}, outcomeSuppress, "Undeliverable", 2},
		{"comcast verified untouched", "comcast", "", eoOutcome{kind: outcomeReady, resultID: 1, result: "Verified"}, outcomeReady, "Verified", 1},
		{"off switch", "comcast", "0", unknown, outcomeRetry, "Unknown", 11},
	}
	for _, c := range cases {
		got := comcastUnknownAsValid(c.isp, c.in, c.env)
		if got.kind != c.wantKind || got.result != c.wantResult || got.resultID != c.wantCode {
			t.Errorf("%s: got kind=%v result=%q code=%d", c.name, got.kind, got.result, got.resultID)
		}
	}
}
