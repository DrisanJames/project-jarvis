package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contract_fulfilment_qa_test.go — WP-E (QA) additions for the worker package.
// NEW FILE ONLY: nothing here rewrites a builder's test.
//
// Three gaps closed:
//
//	1. The 2026-09-09 IP-layer ruling is proved for warmup_daily_limit = 0 with
//	   sends ALREADY on the clock. The builders' zero-limit test uses
//	   TodaySent = 0, which the old gate (`TodaySent >= limit`) also refused —
//	   but only the TodaySent > 0 form is what the estate actually carries.
//	2. Rule 4's "never rolls forward" is proved against the REAL claim
//	   predicate in send_worker.go, not against a status list retyped in a test.
//	3. applyZeroCaps is proved to be a true pass-through — the same map, not an
//	   equal copy — when nothing is contracted at zero, which is what "off and
//	   shadow are byte-identical on the send path" actually requires.

// -----------------------------------------------------------------------------
// 1. THE IP LAYER DOES NOT JUDGE VOLUME — the shapes the estate really carries
// -----------------------------------------------------------------------------

// FAILS ON PRE-CHANGE HEAD. warmup_daily_limit = 0 with sends already on the
// clock is the STANDBY shape: internal/api/handlers_pool_isolation.go:299-302
// designates a pool's standby IP with `status='warmup', warmup_daily_limit=0`,
// and 15 IPs at limit 50 against 16.5k of campaign is the usf/hlj shape that
// dead-lettered 26,757 messages on 2026-09-08.
//
// TestVMTAPoolNext_ZeroWarmupLimit_StillSelected (esp_pmta_test.go) uses
// TodaySent = 0. That distinction matters: the removed gate was
// `TodaySent >= int64(WarmupDailyLimit)`, which is true at 0 >= 0 as well, so
// both forms were refused — but only the TodaySent > 0 form is what a live pool
// looks like an hour into a send, and a future re-introduction written as
// `TodaySent > limit` would pass the existing test and fail this one.
func TestQA_VMTAPoolNext_ZeroLimitWithSendsAlreadyOnTheClock(t *testing.T) {
	cases := []struct {
		name  string
		entry vmtaEntry
	}{
		{"standby designation: limit 0, already sent today", vmtaEntry{
			ID: "sb1", Hostname: "mta-db-sb1", Status: "warmup", WarmupDailyLimit: 0, TodaySent: 12_345}},
		{"limit 0, exactly one send", vmtaEntry{
			ID: "sb1", Hostname: "mta-db-sb1", Status: "warmup", WarmupDailyLimit: 0, TodaySent: 1}},
		{"the usf/hlj shape: limit 50, 16.5k on the clock", vmtaEntry{
			ID: "sb1", Hostname: "mta-db-sb1", Status: "warmup", WarmupDailyLimit: 50, TodaySent: 16_500}},
		{"an ACTIVE IP with a zero limit", vmtaEntry{
			ID: "sb1", Hostname: "mta-db-sb1", Status: "active", WarmupDailyLimit: 0, TodaySent: 999}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ips := []vmtaEntry{tc.entry}
			pool := buildTestPool(map[string][]vmtaEntry{"yahoo": ips}, ips, "db")
			for i := 0; i < 10; i++ {
				ip, err := pool.next("yahoo")
				require.NoError(t, err, "call %d refused on volume", i)
				assert.Equal(t, "sb1", ip.ID)
			}
		})
	}
}

// The pool-wide negative control WP-A's source pin does not cover: it reads
// esp_pmta.go and esp_pmta_api.go only, so a per-IP volume gate reintroduced in
// any OTHER sender in this package (send_worker.go, send_worker_v2.go, a kumo
// or SES path) would not trip it. The ruling is about the IP LAYER, not about
// two files.
//
// Comments and the transport-indicator list in send_worker.go's
// isTransportError are exempt: the indicators are now dead strings (nothing
// produces them any more — see the WP-E sweep) and deleting them is a separate
// decision from proving nothing emits them.
func TestQA_NoWorkerCodePathRefusesASendOnPerIPVolume(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	// The exact comparison the removed gate used, plus the error text it
	// produced. Written as fragments so a reformatted reintroduction still trips.
	forbidden := []struct{ frag, why string }{
		{"exhausted warmup limits", "the 2026-09-08/09 dead-letter string"},
		{"TodaySent >= int64(", "the removed per-IP volume comparison"},
		{"TodaySent > int64(", "a per-IP volume comparison"},
		{".TodaySent >=", "a per-IP volume comparison"},
		{".TodaySent >", "a per-IP volume comparison"},
	}
	// send_worker.go keeps the two now-dead indicator strings in
	// isTransportError; that is a classifier input, not a refusal.
	exemptLines := []string{`"all ips exhausted",`, `"deferring send",`}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		require.NoError(t, err, "read %s", name)
		scanned++
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			exempt := false
			for _, ex := range exemptLines {
				if trimmed == ex {
					exempt = true
				}
			}
			if exempt {
				continue
			}
			for _, f := range forbidden {
				assert.NotContains(t, line, f.frag,
					"%s: %q reappeared — %s. The IP layer does not judge volume (operator ruling 2026-09-09); volume is decided by the drip contract or the campaign quota upstream.",
					name, f.frag, f.why)
			}
		}
	}
	require.Greater(t, scanned, 100, "the sweep read %d files — it must be reading the whole package", scanned)
}

// -----------------------------------------------------------------------------
// 2. RULE 4 — an expired row is unreachable from the REAL claim predicate
// -----------------------------------------------------------------------------

// TestRule4_ExpiredStatusIsOneTheJanitorReaps (dripsupply) proves the status
// against a list of strings retyped inside that test. That list can drift from
// send_worker.go without anything noticing, and the whole value of rule 4 is
// that the send worker CANNOT pick the row up again tomorrow.
//
// This reads the predicate itself. claimISPForOne builds `eligibleStatuses` as
// Go string literals (send_worker.go:894-907) rather than as a constant, so a
// source read is the only way to assert against the thing that actually runs;
// the alternative — a live claim against a seeded queue — needs the campaign
// join and half the mailing schema, and would still be asserting the same fact.
func TestQA_Rule4_ExpiredStatusIsNotClaimableByTheSendWorker(t *testing.T) {
	src, err := os.ReadFile("send_worker.go")
	require.NoError(t, err)
	body := string(src)

	i := strings.Index(body, "func (p *SendWorkerPool) claimISPForOne(")
	require.Greater(t, i, 0, "claimISPForOne not found — update this guard alongside the refactor")
	j := strings.Index(body[i:], "\n\trows, err := p.db.QueryContext")
	require.Greater(t, j, 0, "the eligible-status block moved")
	predicate := body[i : i+j]

	// The predicate must still pick up the two live statuses, or this test is
	// asserting against something that no longer claims anything.
	assert.Contains(t, predicate, `q.status = 'queued'`,
		"the claim predicate no longer picks up 'queued' — this guard is pinned to the wrong block")
	assert.Contains(t, predicate, `q.status = 'failed_retryable'`,
		"the claim predicate no longer picks up 'failed_retryable' — rule 4 has nothing left to expire")

	// And it must NOT pick up the terminal status window expiry parks rows in.
	assert.NotContains(t, predicate, dripsupply.ExpiredAtWindowStatus,
		"send_worker.go's claim predicate accepts %q, the status ExpireRetryablesAtWindowClose writes — an expired row would ship against TOMORROW's contract, which is the exact leak rule 4 closes",
		dripsupply.ExpiredAtWindowStatus)

	// The reason string is a discriminator, not a status: it must never become
	// something the claim keys on.
	assert.NotContains(t, predicate, dripsupply.ExpiredAtWindowReason,
		"the claim predicate keys on error_message — the discriminator must stay a label, not a control")
}

// -----------------------------------------------------------------------------
// 3. THE CONTRACTED-ZERO GATE IS INERT WHEN NOTHING IS ENFORCED
// -----------------------------------------------------------------------------

// "off and shadow stay byte-identical on the send path" is only true if
// applyZeroCaps hands the caller's own map straight back. TestApplyZeroCaps
// asserts reflect.DeepEqual, which an allocating copy also satisfies — and a
// copy is not byte-identical: grantWaveCapacity's callers compare the returned
// map against the chain's map by identity in places, and an extra allocation on
// every wave of every tick is a cost the `off` path is supposed to be free of.
//
// The set is empty in off/shadow (dripsupply's
// TestRule5_ContractedZeroGateIsInertWhenNothingIsEnforced pins that half), so
// this is the other half: empty set in, same map out.
func TestQA_ApplyZeroCaps_IsAPassThroughNotACopyWhenNothingIsZeroed(t *testing.T) {
	caps := map[string]int{"aol": 4000, "microsoft": 2500, "gmail": 1000}

	for _, zeros := range [][]string{nil, {}} {
		got := applyZeroCaps(caps, zeros)
		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(caps).Pointer() {
			t.Errorf("applyZeroCaps(caps, %v) returned a COPY — with nothing to zero the `off`/`shadow` path must be byte-identical, not merely equal", zeros)
		}
	}

	// A nil cap map means "the chain set no per-ISP caps". With nothing to zero
	// that must survive as nil, not become an empty map: the two are different
	// answers to "did anything cap this wave".
	if got := applyZeroCaps(nil, nil); got != nil {
		t.Errorf("applyZeroCaps(nil, nil) = %v, want nil — an unset cap map must stay unset", got)
	}
}
