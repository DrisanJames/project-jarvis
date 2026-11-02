package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func audienceGet(h http.HandlerFunc, url string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

func TestAudienceCachedServesRepeatsFromOneRun(t *testing.T) {
	var runs int32
	h := audienceCached(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&runs, 1)
		w.Write([]byte(`{"rows":[]}`))
	})
	u := "/api/mailing/analytics/lake/audience/test-repeat?group_by=status"
	if got := audienceGet(h, u).Header().Get("X-Audience-Cache"); got != "miss" {
		t.Fatalf("first request: %q", got)
	}
	second := audienceGet(h, u)
	if got := second.Header().Get("X-Audience-Cache"); got != "hit" {
		t.Fatalf("second request: %q", got)
	}
	if second.Body.String() != `{"rows":[]}` {
		t.Fatalf("body: %q", second.Body.String())
	}
	if audienceGet(h, u+"&fresh=1"); atomic.LoadInt32(&runs) != 2 {
		t.Fatalf("fresh=1 must recompute: runs=%d", runs)
	}
	if audienceGet(h, "/api/mailing/analytics/lake/audience/test-repeat?group_by=isp"); atomic.LoadInt32(&runs) != 3 {
		t.Fatalf("a different query must run: runs=%d", runs)
	}
}

func TestAudienceCachedSharesOneConcurrentRun(t *testing.T) {
	var runs int32
	release := make(chan struct{})
	h := audienceCached(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&runs, 1)
		<-release
		w.Write([]byte(`{}`))
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			audienceGet(h, "/api/mailing/analytics/lake/audience/test-shared")
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := atomic.LoadInt32(&runs); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

func TestAudienceCachedKeepsErrorsOut(t *testing.T) {
	var runs int32
	h := audienceCached(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&runs, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"x"}`))
	})
	u := "/api/mailing/analytics/lake/audience/test-error"
	if got := audienceGet(h, u).Code; got != http.StatusBadRequest {
		t.Fatalf("status %d", got)
	}
	audienceGet(h, u)
	if n := atomic.LoadInt32(&runs); n != 2 {
		t.Fatalf("an error must not be stored: runs=%d", n)
	}
}

func TestAudienceCacheExpiresAndEvicts(t *testing.T) {
	c := &audienceResponseCache{m: make(map[string]audienceCacheEntry)}
	now := time.Now()
	c.put("a", audienceCacheEntry{at: now.Add(-audienceCacheTTL)})
	if _, ok := c.get("a", now); ok {
		t.Fatal("expired entry served")
	}
	for i := 0; i < audienceCacheMaxEntries+5; i++ {
		c.put(string(rune(1000+i)), audienceCacheEntry{at: now.Add(time.Duration(i) * time.Second)})
	}
	if len(c.m) > audienceCacheMaxEntries {
		t.Fatalf("cache grew to %d", len(c.m))
	}
}
