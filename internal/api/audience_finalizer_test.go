package api

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// envGuard sets AUDIENCE_WORKER_COUNT (or unsets it when value=="") and
// returns a restore fn the test should defer. Prevents env state from leaking
// across tests in the same process.
func envGuard(t *testing.T, key, value string) func() {
	t.Helper()
	prev, hadPrev := os.LookupEnv(key)
	if value == "" {
		_ = os.Unsetenv(key)
	} else {
		_ = os.Setenv(key, value)
	}
	return func() {
		if hadPrev {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	}
}

func TestAudienceWorkerCount_Default(t *testing.T) {
	defer envGuard(t, "AUDIENCE_WORKER_COUNT", "")()

	got := audienceWorkerCount()
	if got != 4 {
		t.Fatalf("expected default 4, got %d", got)
	}
}

func TestAudienceWorkerCount_FromEnv(t *testing.T) {
	defer envGuard(t, "AUDIENCE_WORKER_COUNT", "8")()

	got := audienceWorkerCount()
	if got != 8 {
		t.Fatalf("expected 8 from env, got %d", got)
	}
}

func TestAudienceWorkerCount_OutOfRange_Clamps(t *testing.T) {
	// Values outside the supported [1,16] range fall back to the default 4.
	cases := []string{"99", "0", "-1", "17", "1000"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			defer envGuard(t, "AUDIENCE_WORKER_COUNT", v)()
			got := audienceWorkerCount()
			if got != 4 {
				t.Fatalf("env=%s expected fallback 4, got %d", v, got)
			}
		})
	}
}

func TestAudienceWorkerCount_InvalidEnv(t *testing.T) {
	cases := []string{"notanumber", "4.5", "four", " ", "1e2"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			defer envGuard(t, "AUDIENCE_WORKER_COUNT", v)()
			got := audienceWorkerCount()
			if got != 4 {
				t.Fatalf("env=%q expected fallback 4, got %d", v, got)
			}
		})
	}
}

// waitForGoroutineCount polls runtime.NumGoroutine until either the predicate
// is satisfied or the deadline passes. Returns the last observed count.
func waitForGoroutineCount(deadline time.Duration, ok func(n int) bool) int {
	end := time.Now().Add(deadline)
	last := runtime.NumGoroutine()
	for time.Now().Before(end) {
		last = runtime.NumGoroutine()
		if ok(last) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	return last
}

func TestStartAudienceWorker_SpawnsCorrectGoroutineCount(t *testing.T) {
	defer envGuard(t, "AUDIENCE_WORKER_COUNT", "2")()

	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Allow stray goroutines from prior tests to settle so the baseline is
	// stable. Without this, leaked goroutines from earlier tests in the
	// package can throw off the count.
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	svc.StartAudienceWorker(ctx)

	// Wait for the spawned worker goroutines to be observable. Each worker
	// goroutine sits in a select on a 10s+stagger time.After during this
	// window, so they will not exit before we measure.
	got := waitForGoroutineCount(500*time.Millisecond, func(n int) bool {
		return n >= baseline+2
	})

	if got < baseline+2 {
		t.Fatalf("expected at least %d goroutines (baseline %d + 2 workers), got %d",
			baseline+2, baseline, got)
	}
	// Tolerance for runtime bookkeeping goroutines (timers, netpoll).
	if got > baseline+10 {
		t.Fatalf("unexpectedly large goroutine delta: baseline %d, got %d (workers=2)", baseline, got)
	}

	cancel()

	// Drain back toward baseline within 1s. Workers exit through the
	// ctx.Done() branch in the initial-delay select.
	final := waitForGoroutineCount(1*time.Second, func(n int) bool {
		return n <= baseline+1
	})
	if final > baseline+1 {
		t.Fatalf("workers did not stop after cancel: baseline %d, final %d", baseline, final)
	}
}

func TestStartAudienceWorker_AllStopOnContextCancel(t *testing.T) {
	defer envGuard(t, "AUDIENCE_WORKER_COUNT", "4")()

	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)

	ctx, cancel := context.WithCancel(context.Background())

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	svc.StartAudienceWorker(ctx)

	// Confirm workers actually started.
	started := waitForGoroutineCount(500*time.Millisecond, func(n int) bool {
		return n >= baseline+4
	})
	if started < baseline+4 {
		t.Fatalf("expected baseline+4 goroutines after start, got %d (baseline=%d)", started, baseline)
	}

	cancel()

	final := waitForGoroutineCount(1*time.Second, func(n int) bool {
		return n <= baseline+1
	})
	if final > baseline+1 {
		t.Fatalf("expected goroutines to drain to baseline %d within 1s; final=%d", baseline, final)
	}
}
