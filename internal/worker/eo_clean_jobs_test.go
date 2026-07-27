package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/ignite/sparkpost-monitor/internal/emailoversight"
)

// ── Shared-transport byte-stability (golden) ────────────────────────────────

// TestSharedTransportByteStablePartnerPath pins the factor-out contract: for
// every response shape, PartnerValidator.callEO must return EXACTLY what the
// shared callEOValidation returns — the partner path's behavior is byte-stable
// across the refactor.
func TestSharedTransportByteStablePartnerPath(t *testing.T) {
	shapes := []struct {
		name string
		resp *emailoversight.ValidationResponse
		err  error
	}{
		{"verified", &emailoversight.ValidationResponse{ResultID: 1, Result: "Verified"}, nil},
		{"complainer", &emailoversight.ValidationResponse{ResultID: 7, Result: "Complainer"}, nil},
		{"retry_zero", &emailoversight.ValidationResponse{ResultID: 0, Result: "Retry"}, nil},
		{"unknown", &emailoversight.ValidationResponse{ResultID: 11, Result: "Unknown"}, nil},
		{"undeliverable", &emailoversight.ValidationResponse{ResultID: 4, Result: "Undeliverable"}, nil},
		{"spamtrap", &emailoversight.ValidationResponse{ResultID: 5, Result: "SpamTrap"}, nil},
		{"reject_stamped", &emailoversight.ValidationResponse{Result: "REJECT", Reason: "Suppressed Email Address", ResultID: emailoversight.ResultIDRejected}, nil},
		{"reject_unstamped", &emailoversight.ValidationResponse{Result: "REJECT", Reason: "Suppressed Email Address"}, nil},
		{"reject_no_reason", &emailoversight.ValidationResponse{Result: "REJECT"}, nil},
		{"transport_error", nil, errors.New("dial tcp: timeout")},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeEOValidator{resp: tc.resp, err: tc.err}
			pv := NewPartnerValidator(nil, fake, PartnerValidatorConfig{})
			got := pv.callEO(context.Background(), pendingRecord{email: "x@example.com"})
			want := callEOValidation(context.Background(), fake, "x@example.com")
			if got != want {
				t.Fatalf("callEO=%+v != callEOValidation=%+v — partner path drifted from the shared transport", got, want)
			}
		})
	}
}

// ── Verdict → canonical status/counter mapping ──────────────────────────────

func TestClassifyCleanOutcome(t *testing.T) {
	cases := []struct {
		name         string
		o            eoOutcome
		wantStatus   string
		wantCounter  string
		wantTerminal bool
	}{
		{"verified", eoOutcome{kind: outcomeReady, resultID: 1, result: "Verified"}, "Verified", "verified", true},
		{"complainer", eoOutcome{kind: outcomeReady, resultID: 7, result: "Complainer"}, "Complainer", "complainer", true},
		{"undeliverable", eoOutcome{kind: outcomeSuppress, resultID: 4, result: "Undeliverable"}, "Undeliverable", "undeliverable", true},
		{"spamtrap_other", eoOutcome{kind: outcomeSuppress, resultID: 5, result: "SpamTrap"}, "SpamTrap", "other", true},
		{"reject_suppressed", eoOutcome{kind: outcomeSuppress, resultID: emailoversight.ResultIDRejected, result: "Suppressed Email Address"}, "Suppressed", "other", true},
		{"retry_not_terminal", eoOutcome{kind: outcomeRetry, resultID: 0, result: "Retry"}, "", "", false},
		{"error_not_terminal", eoOutcome{kind: outcomeError, result: "error: boom"}, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, counter, terminal := classifyCleanOutcome(tc.o)
			if status != tc.wantStatus || counter != tc.wantCounter || terminal != tc.wantTerminal {
				t.Fatalf("classifyCleanOutcome = (%q,%q,%v), want (%q,%q,%v)",
					status, counter, terminal, tc.wantStatus, tc.wantCounter, tc.wantTerminal)
			}
		})
	}
}

// ── Cap / budget enforcement math ───────────────────────────────────────────

func TestEOCleanClaimLimit(t *testing.T) {
	cases := []struct {
		name                                    string
		batchLeft, budgetLeft, cap, spent, want int
	}{
		{"uncapped_batch_binds", 200, 1000, 0, 0, 200},
		{"budget_binds", 200, 50, 0, 0, 50},
		{"job_cap_binds", 200, 1000, 100, 40, 60},
		{"job_cap_exhausted", 200, 1000, 100, 100, 0},
		{"job_cap_overspent_never_negative", 200, 1000, 100, 150, 0},
		{"budget_exhausted", 200, 0, 0, 0, 0},
		{"negative_cap_means_uncapped", 200, 300, -1, 999, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eoCleanClaimLimit(tc.batchLeft, tc.budgetLeft, tc.cap, tc.spent); got != tc.want {
				t.Fatalf("eoCleanClaimLimit(%d,%d,%d,%d) = %d, want %d",
					tc.batchLeft, tc.budgetLeft, tc.cap, tc.spent, got, tc.want)
			}
		})
	}
}

// ── Progress accounting ─────────────────────────────────────────────────────

// TestApplyCleanOutcomeProgressAccounting drives a mixed batch through
// applyCleanOutcome and checks the delta arithmetic the job-counter UPDATE
// receives: validated = terminal verdicts, failed = exhausted retries, and
// retryable items below the cap change nothing.
func TestApplyCleanOutcomeProgressAccounting(t *testing.T) {
	const maxRetries = 3
	var d eoCleanDelta

	steps := []struct {
		o        eoOutcome
		attempts int
		want     eoCleanItemAction
	}{
		{eoOutcome{kind: outcomeReady, resultID: 1, result: "Verified"}, 0, eoCleanActionDone},
		{eoOutcome{kind: outcomeReady, resultID: 7, result: "Complainer"}, 0, eoCleanActionDone},
		{eoOutcome{kind: outcomeSuppress, resultID: 4, result: "Undeliverable"}, 0, eoCleanActionDone},
		{eoOutcome{kind: outcomeSuppress, resultID: 5, result: "Bot"}, 0, eoCleanActionDone},
		{eoOutcome{kind: outcomeRetry, resultID: 0, result: "Retry"}, 0, eoCleanActionRetry},   // attempt 1 of 3 — stays pending
		{eoOutcome{kind: outcomeRetry, resultID: 11, result: "Unknown"}, 2, eoCleanActionFailed}, // attempt 3 of 3 — exhausted
	}
	for i, s := range steps {
		got, _ := applyCleanOutcome(&d, s.o, s.attempts, maxRetries)
		if got != s.want {
			t.Fatalf("step %d: action = %d, want %d", i, got, s.want)
		}
	}
	want := eoCleanDelta{validated: 4, verified: 1, complainer: 1, undeliverable: 1, other: 1, failed: 1}
	if d != want {
		t.Fatalf("delta = %+v, want %+v", d, want)
	}
	// The queued_count decrement the UPDATE receives = items that left pending.
	if left := d.validated + d.failed; left != 5 {
		t.Fatalf("queued decrement = %d, want 5 (4 done + 1 failed; the retry stays pending)", left)
	}
}

// TestApplyCleanOutcomeFailedItemsGetNoVerdict pins that an exhausted-retry
// item is FAILED without a fabricated canonical status — the returned result
// string is the raw retry result (recorded on the item row only; the worker
// writes mailing_eo_validation ONLY on eoCleanActionDone).
func TestApplyCleanOutcomeFailedItemsGetNoVerdict(t *testing.T) {
	var d eoCleanDelta
	action, result := applyCleanOutcome(&d,
		eoOutcome{kind: outcomeRetry, result: "error: dial tcp: timeout"}, 2, 3)
	if action != eoCleanActionFailed {
		t.Fatalf("action = %d, want eoCleanActionFailed", action)
	}
	if result != "error: dial tcp: timeout" {
		t.Fatalf("result = %q — must carry the raw error, never a canonical status", result)
	}
}

// ── Pause semantics (worker side) ───────────────────────────────────────────

// TestDrainSkipsPausedJobs: the drainable-jobs query claims only
// queued/running rows — a paused job's pending items are never claimed, and
// the pass ends cleanly with zero work.
func TestDrainSkipsPausedJobs(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_eo_clean_items`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	// The status filter is the pause gate: paused jobs are excluded here.
	mock.ExpectQuery(`FROM mailing_eo_clean_jobs\s+WHERE status IN \('queued', 'running'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "daily_cap", "status"}))

	w := NewEOCleanJobWorker(db, nil, &fakeEOValidator{}, EOCleanJobConfig{})
	processed, failed, derr := w.drain(context.Background())
	if derr != nil {
		t.Fatalf("drain: %v", derr)
	}
	if processed != 0 || failed != 0 {
		t.Fatalf("processed=%d failed=%d, want 0/0 — no drainable jobs", processed, failed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v — an unexpected query would mean paused items were touched", err)
	}
}

// TestDrainStopsWhenBudgetExhausted: with today's spend at/over the daily
// budget, the pass returns before even listing jobs (no claim, no EO call).
func TestDrainStopsWhenBudgetExhausted(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_eo_clean_items`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(500))

	w := NewEOCleanJobWorker(db, nil, &fakeEOValidator{}, EOCleanJobConfig{DailyBudget: 500})
	processed, failed, derr := w.drain(context.Background())
	if derr != nil || processed != 0 || failed != 0 {
		t.Fatalf("drain = (%d,%d,%v), want (0,0,nil) — budget gate must fire first", processed, failed, derr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/extra expectations: %v — the jobs query must NOT run past an exhausted budget", err)
	}
}

// ── Kill switch ─────────────────────────────────────────────────────────────

// TestEOCleanKillSwitch: DISABLE_EO_CLEAN_JOBS=true makes Start return
// immediately — no ticker, no DB traffic (sqlmock would flag any query).
func TestEOCleanKillSwitch(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Setenv("DISABLE_EO_CLEAN_JOBS", "true")

	w := NewEOCleanJobWorker(db, nil, &fakeEOValidator{}, EOCleanJobConfig{PollInterval: time.Millisecond})
	done := make(chan struct{})
	go func() {
		w.Start(context.Background()) // must return on its own via the switch
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return — kill switch DISABLE_EO_CLEAN_JOBS is inert")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("kill-switched worker touched the DB: %v", err)
	}
}

// TestEOCleanBudgetEnvDefault pins the default and the env override.
func TestEOCleanBudgetEnvDefault(t *testing.T) {
	t.Setenv("EO_CLEAN_DAILY_BUDGET", "")
	if got := eoCleanDailyBudgetFromEnv(); got != eoCleanDefaultDailyBudget {
		t.Fatalf("default budget = %d, want %d", got, eoCleanDefaultDailyBudget)
	}
	t.Setenv("EO_CLEAN_DAILY_BUDGET", "2500")
	if got := eoCleanDailyBudgetFromEnv(); got != 2500 {
		t.Fatalf("env budget = %d, want 2500", got)
	}
	t.Setenv("EO_CLEAN_DAILY_BUDGET", "not-a-number")
	if got := eoCleanDailyBudgetFromEnv(); got != eoCleanDefaultDailyBudget {
		t.Fatalf("invalid env budget = %d, want default %d", got, eoCleanDefaultDailyBudget)
	}
}
