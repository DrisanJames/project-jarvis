package api

import (
	"testing"
	"time"
)

func TestLedgerLineOnDenverDay(t *testing.T) {
	loc, _ := time.LoadLocation("America/Denver")
	day, _ := time.Parse("20060102", "20261027")
	pre := []string{`"id": "cl-20261027`, `"id":"cl-20261027`, `"id": "cl-20261028`, `"id":"cl-20261028`}
	cases := map[string]bool{
		`{"id": "cl-20261027-150101-aaaa", "ts": "2026-10-27T15:01:01+00:00"}`: true,  // 09:01 MT 10-27
		`{"id": "cl-20261028-033543-0178", "ts": "2026-10-28T03:35:43+00:00"}`: true,  // 21:35 MT 10-27 (UTC id says 10-28)
		`{"id": "cl-20261027-030101-cccc", "ts": "2026-10-27T03:01:01+00:00"}`: false, // 21:01 MT 10-26
		`{"id": "cl-20261028-090000-eeee", "ts": "2026-10-28T09:00:00+00:00"}`: false, // 03:00 MT 10-28
		`{"id":"cl-20261027-150101-ffff","ts":"2026-10-27T15:01:01+00:00"}`:    true,
		`{"id": "cl-20261026-150101-gggg", "ts": "2026-10-27T15:01:01+00:00"}`: false, // prefix outside the scan
		`{"id": "cl-20261027-150101-hhhh", "ts": "bad"}`:                       false,
	}
	for line, want := range cases {
		if got := ledgerLineOnDenverDay([]byte(line), pre, day, loc); got != want {
			t.Errorf("%s: got %v want %v", line, got, want)
		}
	}
}
