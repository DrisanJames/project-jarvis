package tracking

import (
	"context"
	"sync"
	"testing"
)

// fakeSuppressor records SuppressScoped calls.
type fakeSuppressor struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeSuppressor) SuppressScoped(_ context.Context, _, _, _, _, _, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil
}

// TestSetBrandSuppressorIsSafeAndVisibleAfterStartOrder pins the 2026-08-21
// fix: the hub is wired LATE (after the consumer is already polling), from a
// different goroutine than the readers. The getter must observe the late
// write, and the race detector must stay quiet — a plain field here is
// exactly the defect that ran every boot with unsubscribes unenforced.
func TestSetBrandSuppressorIsSafeAndVisibleAfterStartOrder(t *testing.T) {
	c := &Consumer{done: make(chan struct{})}

	if got := c.getBrandSuppressor(); got != nil {
		t.Fatalf("suppressor should start nil, got %T", got)
	}

	// Concurrent readers simulating the poll workers, then a late writer.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = c.getBrandSuppressor()
				}
			}
		}()
	}

	fs := &fakeSuppressor{}
	c.SetBrandSuppressor(fs) // the late pass
	close(stop)
	wg.Wait()

	got := c.getBrandSuppressor()
	if got == nil {
		t.Fatal("late-wired suppressor not visible through the getter — the enforced-set path would still be skipped")
	}
	if err := got.SuppressScoped(context.Background(), "a@b.c", "b.c", "user_unsubscribe", "test", "", "", ""); err != nil {
		t.Fatalf("SuppressScoped: %v", err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.calls != 1 {
		t.Fatalf("expected 1 SuppressScoped call, got %d", fs.calls)
	}
}
