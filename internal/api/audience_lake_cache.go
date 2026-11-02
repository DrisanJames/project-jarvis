package api

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// The Audience screen reads a daily snapshot, or a full scan of
// mailing_subscribers when no snapshot exists, so an answer holds for hours.
// Identical requests share one run and one stored response; ?fresh=1 (the
// screen's Refresh) recomputes. An abandoned tab does not cancel a shared run.
const (
	audienceCacheTTL        = 6 * time.Hour
	audienceCacheMaxEntries = 256
	audienceCacheRunBudget  = 3 * time.Minute
)

type audienceCacheEntry struct {
	status int
	body   []byte
	at     time.Time
}

type audienceResponseCache struct {
	mu     sync.Mutex
	m      map[string]audienceCacheEntry
	flight singleflight.Group
}

var audienceCache = &audienceResponseCache{m: make(map[string]audienceCacheEntry)}

func (c *audienceResponseCache) get(key string, now time.Time) (audienceCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || now.Sub(e.at) >= audienceCacheTTL {
		return audienceCacheEntry{}, false
	}
	return e, true
}

func (c *audienceResponseCache) put(key string, e audienceCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= audienceCacheMaxEntries {
		oldestKey, oldest := "", e.at
		for k, v := range c.m {
			if v.at.Before(oldest) {
				oldestKey, oldest = k, v.at
			}
		}
		delete(c.m, oldestKey)
	}
	c.m[key] = e
}

type audienceCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *audienceCapture) Header() http.Header         { return a.header }
func (a *audienceCapture) WriteHeader(code int)        { a.status = code }
func (a *audienceCapture) Write(b []byte) (int, error) { return a.body.Write(b) }

// audienceCached wraps a read-only Audience handler. Only a 200 is stored.
func audienceCached(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		fresh := q.Get("fresh") == "1"
		q.Del("fresh")
		key := r.URL.Path + "?" + q.Encode()

		if !fresh {
			if e, ok := audienceCache.get(key, time.Now()); ok {
				writeAudienceEntry(w, e, "hit")
				return
			}
		}
		v, _, _ := audienceCache.flight.Do(key, func() (interface{}, error) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), audienceCacheRunBudget)
			defer cancel()
			rec := &audienceCapture{header: make(http.Header), status: http.StatusOK}
			next(rec, r.WithContext(ctx))
			e := audienceCacheEntry{status: rec.status, body: rec.body.Bytes(), at: time.Now()}
			if e.status == http.StatusOK {
				audienceCache.put(key, e)
			}
			return e, nil
		})
		writeAudienceEntry(w, v.(audienceCacheEntry), "miss")
	}
}

func writeAudienceEntry(w http.ResponseWriter, e audienceCacheEntry, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Audience-Cache", state)
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}
