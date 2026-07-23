package worker

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/moneylink"
)

// SmartLinkEmitter is the SEND-SIDE inverse of the tracking service's
// SmartLinkDictionary (internal/tracking/dictionary.go). The dictionary maps
// hash -> offer_url_template (resolve direction, at redirect time). This
// emitter maps normalize(offer_url_template) -> hash (mint direction, at send
// time), so the send worker can recognise a rendered cratoolpro href and swap
// it for the tracking-layer /o/ URL BEFORE the /track/click wrap.
//
// Design invariants (this feeds the LIVE offer-link hot path — Wave 2):
//   - Opt-in only: NewSmartLinkEmitter(..., enabled=false) or a nil db yields a
//     DISABLED emitter — Lookup always misses, Enabled() is false, and NO
//     background goroutine is started. RewriteClickLinks gates every emit on
//     Enabled(), so a disabled emitter leaves the wrap output byte-identical to
//     the pre-Wave-2 behavior.
//   - A failed reload NEVER blanks the map: reloadOnce builds the next snapshot
//     fully and only swaps on success, so a transient DB error keeps the last
//     good snapshot serving (a blanked map would silently drop /o/ minting and
//     fall every offer back to /track/click — safe, but observably worse).
//   - Exactly one background goroutine; cancellable via Close().
//   - Concurrency: the map is RWMutex-guarded. The package-level
//     offerHashEmitter handle (see send_worker.go) is set ONCE at pool init
//     before any worker goroutine starts and is read-only thereafter.
type SmartLinkEmitter struct {
	enabled bool
	db      *sql.DB
	refresh time.Duration

	// loadFn produces the next full snapshot (normalizedDest -> hash). Defaults
	// to queryDB; overridable in tests to inject a fake loader without a live DB.
	loadFn func(context.Context) (map[string]string, error)

	mu      sync.RWMutex
	entries map[string]string

	cancel context.CancelFunc
}

// smartLinkEmitterQuery reads the active, hash-bearing rows. Mirrors the
// tracking dictionary's smartLinkQuery source (mailing_smart_links) but selects
// only the two columns the inverse map needs.
const smartLinkEmitterQuery = `SELECT hash, offer_url_template
FROM mailing_smart_links
WHERE hash IS NOT NULL AND status='active'`

// NewSmartLinkEmitter builds the emitter. If enabled is false OR db is nil it
// returns a DISABLED emitter: Lookup always misses, no goroutine, no DB access.
// When enabled with a live db it loads once synchronously (so the map is warm
// before the first send) and then reloads every refresh interval on a single
// cancellable background goroutine.
func NewSmartLinkEmitter(db *sql.DB, refresh time.Duration, enabled bool) *SmartLinkEmitter {
	if refresh <= 0 {
		refresh = 60 * time.Second
	}
	e := &SmartLinkEmitter{
		enabled: enabled,
		db:      db,
		refresh: refresh,
		entries: make(map[string]string),
	}
	e.loadFn = e.queryDB

	// Disabled mode: flag off or no DB handle. No goroutine, Lookup always misses.
	if !enabled || db == nil {
		e.enabled = false
		return e
	}

	// Initial synchronous load. A failure here is non-fatal: we serve an empty
	// map (every offer falls back to the /track/click wrap) until the first
	// successful refresh repopulates it.
	if err := e.reloadOnce(context.Background()); err != nil {
		log.Printf("smartlink emitter: initial load failed (serving empty until next refresh): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go e.loop(ctx)
	return e
}

func (e *SmartLinkEmitter) loop(ctx context.Context) {
	t := time.NewTicker(e.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.reloadOnce(ctx); err != nil {
				log.Printf("smartlink emitter: reload failed (keeping prior %d entries): %v", e.Len(), err)
			}
		}
	}
}

// reloadOnce builds the next snapshot via loadFn and swaps it in ONLY on
// success. On error the prior snapshot is untouched.
func (e *SmartLinkEmitter) reloadOnce(ctx context.Context) error {
	next, err := e.loadFn(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.entries = next
	e.mu.Unlock()
	log.Printf("smartlink emitter: loaded %d active offer-link hashes", len(next))
	return nil
}

// queryDB is the DB-backed loader. It reads the full active set into a fresh
// map keyed by normalize(offer_url_template); any scan/query error aborts the
// WHOLE build (caller keeps the prior snapshot). Two active rows that normalize
// to the same destination is a data condition (should be prevented upstream);
// last-writer-wins here, deterministic within a single build.
func (e *SmartLinkEmitter) queryDB(ctx context.Context) (map[string]string, error) {
	if e.db == nil {
		return map[string]string{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	rows, err := e.db.QueryContext(ctx, smartLinkEmitterQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	next := make(map[string]string)
	for rows.Next() {
		var hash string
		var tmpl sql.NullString
		if err := rows.Scan(&hash, &tmpl); err != nil {
			return nil, err
		}
		hash = strings.TrimSpace(hash)
		key := normalizeOfferURL(tmpl.String)
		if hash == "" || key == "" {
			continue // unusable row — skip, don't fail the reload
		}
		next[key] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return next, nil
}

// normalizeOfferURL reduces an offer URL to a stable dictionary key. Its body
// now lives in the shared moneylink package (internal/pkg/moneylink) so the
// send worker (mint direction), the API rewriter, and the Creative Studio
// offer-links reporter all key on the IDENTICAL normalization — the UI's
// "mapped" verdict can never disagree with what this emitter actually mints.
// Kept as a thin package-local wrapper so the emitter's callers and tests are
// unchanged.
func normalizeOfferURL(raw string) string {
	return moneylink.Normalize(raw)
}

// Lookup normalizes rawOfferURL and returns the seeded hash for it, if any.
// Nil-receiver and disabled-emitter safe: both always miss ("", false), so the
// caller (RewriteClickLinks) can hold the handle without special-casing and a
// flag-off deploy mints zero /o/ links.
func (e *SmartLinkEmitter) Lookup(rawOfferURL string) (string, bool) {
	if e == nil || !e.enabled {
		return "", false
	}
	key := normalizeOfferURL(rawOfferURL)
	if key == "" {
		return "", false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	h, ok := e.entries[key]
	return h, ok
}

// Enabled reports whether the emitter is live (flag on AND a DB handle was
// provided). Nil-safe. RewriteClickLinks gates every emit on this so a
// disabled emitter is byte-for-byte transparent.
func (e *SmartLinkEmitter) Enabled() bool {
	return e != nil && e.enabled
}

// Len is the current snapshot size (for logging/observability). Nil-safe.
func (e *SmartLinkEmitter) Len() int {
	if e == nil {
		return 0
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.entries)
}

// Close cancels the background refresh goroutine. Idempotent and nil-safe.
func (e *SmartLinkEmitter) Close() {
	if e != nil && e.cancel != nil {
		e.cancel()
	}
}

// offerHashEmitter is the process-wide send-side emitter handle. It is set
// ONCE via SetOfferHashEmitter at pool init (cmd/server/main.go), before any
// worker goroutine starts, and is READ-ONLY thereafter — so RewriteClickLinks
// reads it without a lock. Default (unset) is nil, which RewriteClickLinks
// treats as "no minting", producing byte-identical output to pre-Wave-2.
var offerHashEmitter *SmartLinkEmitter

// SetOfferHashEmitter installs the process-wide offer-hash emitter. Call once
// at boot, before workers start. Safe to call with a disabled emitter (flag
// off) — RewriteClickLinks still short-circuits on Enabled().
func SetOfferHashEmitter(e *SmartLinkEmitter) {
	offerHashEmitter = e
}
