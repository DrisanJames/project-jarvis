package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

func cubeStoreCounting(calls *int32, gate chan struct{}) *cubeStore {
	return newCubeStore(func(context.Context, analytics.CubeFilter) (analytics.Cube, error) {
		atomic.AddInt32(calls, 1)
		if gate != nil {
			<-gate
		}
		return analytics.Cube{Rows: []analytics.CubeRow{{LocalDt: "2026-10-30", Lane: "ses", ISP: "gmail", Brand: "em.x.com", Event: "open", Count: 5}}}, nil
	})
}

func TestCubeStore_ConcurrentRequestsShareOneScan(t *testing.T) {
	var calls int32
	gate := make(chan struct{})
	cs := cubeStoreCounting(&calls, gate)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cs.load(context.Background(), "k", analytics.CubeFilter{}, false); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if calls != 1 {
		t.Errorf("scans = %d, want 1", calls)
	}
}

func TestCubeStore_TTLAndRefresh(t *testing.T) {
	var calls int32
	cs := cubeStoreCounting(&calls, nil)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	cs.now = func() time.Time { return now }
	ctx := context.Background()

	cs.load(ctx, "k", analytics.CubeFilter{}, false)
	cs.load(ctx, "k", analytics.CubeFilter{}, false)
	if calls != 1 {
		t.Fatalf("cached read scanned: %d", calls)
	}
	cs.load(ctx, "k", analytics.CubeFilter{}, true)
	if calls != 2 {
		t.Fatalf("refresh must scan: %d", calls)
	}
	now = now.Add(cubeTTL)
	cs.load(ctx, "k", analytics.CubeFilter{}, false)
	if calls != 3 {
		t.Fatalf("expired entry must scan: %d", calls)
	}
}

func TestCubeStore_CallerCancelDoesNotKillTheScan(t *testing.T) {
	var calls int32
	gate := make(chan struct{})
	cs := cubeStoreCounting(&calls, gate)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cs.load(ctx, "k", analytics.CubeFilter{}, false)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller err = %v", err)
	}
	close(gate)
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := cs.get("k"); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("scan result was lost after the caller left")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCubeStore_ErrorsAreNotCached(t *testing.T) {
	var calls int32
	cs := newCubeStore(func(context.Context, analytics.CubeFilter) (analytics.Cube, error) {
		atomic.AddInt32(&calls, 1)
		return analytics.Cube{}, errors.New("athena down")
	})
	for i := 0; i < 2; i++ {
		if _, err := cs.load(context.Background(), "k", analytics.CubeFilter{}, false); err == nil {
			t.Fatal("expected an error")
		}
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestEncodeCube_RoundTrips(t *testing.T) {
	p := encodeCube(analytics.CubeFilter{From: "2026-10-30", To: "2026-10-30"}, analytics.Cube{Rows: []analytics.CubeRow{
		{LocalDt: "2026-10-30", Lane: "ses", ISP: "gmail", Brand: "a.com", Event: "open", Count: 5},
		{LocalDt: "2026-10-30", Lane: "pmta_a", ISP: "gmail", Brand: "a.com", Event: "delivered", Count: 9},
	}})
	if len(p.Dict["local_dt"]) != 1 || len(p.Dict["lane"]) != 2 {
		t.Fatalf("dict not shared: %+v", p.Dict)
	}
	r := p.Rows[1]
	if p.Dict["lane"][r[1]] != "pmta_a" || p.Dict["event_type"][r[4]] != "delivered" || r[5] != 9 {
		t.Errorf("row decodes wrong: %v", r)
	}
}

func TestCubeRangeDays(t *testing.T) {
	if d, err := cubeRangeDays("2026-10-24", "2026-10-30"); err != nil || d != 7 {
		t.Errorf("d=%d err=%v", d, err)
	}
	if _, err := cubeRangeDays("2026-10-31", "2026-10-30"); err == nil {
		t.Error("reversed range must fail")
	}
	if _, err := cubeRangeDays("x", "2026-10-30"); err == nil {
		t.Error("bad date must fail")
	}
}
