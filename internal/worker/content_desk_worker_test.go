package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/contentdesk"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

type fakeCDQueue struct {
	mu      sync.Mutex
	pending []contentdesk.PendingArticle
	reads   int
	stopped []string
}

func (q *fakeCDQueue) PendingArticles(context.Context, int) ([]contentdesk.PendingArticle, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reads++
	return q.pending, nil
}

func (q *fakeCDQueue) StopRun(_ context.Context, _, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = append(q.stopped, id)
	return nil
}

type fakeCDRunner struct {
	err     error
	runs    int32
	active  int32
	maxSeen int32
	hold    time.Duration
}

func (r *fakeCDRunner) Run(context.Context, string, string) error {
	atomic.AddInt32(&r.runs, 1)
	n := atomic.AddInt32(&r.active, 1)
	for {
		m := atomic.LoadInt32(&r.maxSeen)
		if n <= m || atomic.CompareAndSwapInt32(&r.maxSeen, m, n) {
			break
		}
	}
	time.Sleep(r.hold)
	atomic.AddInt32(&r.active, -1)
	return r.err
}

type fakeCDLock struct{ ok bool }

func (l fakeCDLock) Acquire(context.Context) (bool, error) { return l.ok, nil }
func (l fakeCDLock) Release(context.Context) error         { return nil }

func newFakeCDWorker(q *fakeCDQueue, r *fakeCDRunner, enabled, lockOK bool) *ContentDeskWorker {
	return &ContentDeskWorker{queue: q, runner: r, concurrency: 2, interval: time.Minute,
		enabled: func() bool { return enabled },
		newLock: func(string) distlock.DistLock { return fakeCDLock{ok: lockOK} }}
}

func pending(n int) []contentdesk.PendingArticle {
	out := make([]contentdesk.PendingArticle, n)
	for i := range out {
		out[i] = contentdesk.PendingArticle{OrgID: "o", ArticleID: fmt.Sprintf("a%d", i)}
	}
	return out
}

func TestContentDeskWorker_KillSwitchOffDoesNothing(t *testing.T) {
	q, r := &fakeCDQueue{pending: pending(3)}, &fakeCDRunner{}
	if got := newFakeCDWorker(q, r, false, true).tick(context.Background()); got != "disabled" {
		t.Fatalf("tick = %q", got)
	}
	if q.reads != 0 || r.runs != 0 {
		t.Fatalf("disabled must not read the queue or run: reads=%d runs=%d", q.reads, r.runs)
	}
}

func TestContentDeskWorker_LockHeldElsewhereSkips(t *testing.T) {
	q, r := &fakeCDQueue{pending: pending(2)}, &fakeCDRunner{}
	newFakeCDWorker(q, r, true, false).tick(context.Background())
	if r.runs != 0 {
		t.Fatalf("a held article lock must skip the run, got %d runs", r.runs)
	}
}

func TestContentDeskWorker_ExhaustedStageDequeues(t *testing.T) {
	q := &fakeCDQueue{pending: pending(1)}
	r := &fakeCDRunner{err: fmt.Errorf("research: %w", contentdesk.ErrRunExhausted)}
	newFakeCDWorker(q, r, true, true).tick(context.Background())
	if len(q.stopped) != 1 || q.stopped[0] != "a0" {
		t.Fatalf("an exhausted article must be dequeued, stopped=%v", q.stopped)
	}
}

func TestContentDeskWorker_BudgetHoldKeepsQueued(t *testing.T) {
	q := &fakeCDQueue{pending: pending(1)}
	r := &fakeCDRunner{err: fmt.Errorf("research: %w", contentdesk.ErrBudgetExceeded)}
	newFakeCDWorker(q, r, true, true).tick(context.Background())
	if len(q.stopped) != 0 {
		t.Fatal("budget exhaustion must keep the article queued for tomorrow")
	}
}

func TestContentDeskWorker_ConcurrencyIsBounded(t *testing.T) {
	q, r := &fakeCDQueue{pending: pending(6)}, &fakeCDRunner{hold: 20 * time.Millisecond}
	newFakeCDWorker(q, r, true, true).tick(context.Background())
	if r.runs != 6 || r.maxSeen > 2 {
		t.Fatalf("runs=%d maxConcurrent=%d (limit 2)", r.runs, r.maxSeen)
	}
}

func TestContentDeskConcurrencyEnv(t *testing.T) {
	t.Setenv(EnvContentDeskConcurrency, "")
	if ContentDeskConcurrency() != 2 {
		t.Fatal("default 2")
	}
	t.Setenv(EnvContentDeskConcurrency, "50")
	if ContentDeskConcurrency() != 8 {
		t.Fatal("clamped to 8")
	}
}
