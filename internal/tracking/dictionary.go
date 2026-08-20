package tracking

import (
	"context"
	"database/sql"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"
)

// smartLinkEntry is one resolved offer link. Destination is the raw
// offer_url_template (may contain {{subscriber.id}} / {{brand.domain}} /
// {{campaign}} mustache placeholders); attribution params are appended at
// serve time by renderOfferDestination. RiskProfile is lowercased ("low" |
// "high"); "high" gets the branded bridge page, everything else a 302.
type smartLinkEntry struct {
	Destination string
	RiskProfile string
	BrandRoot   string
	Slug        string
}

// SmartLinkDictionary is an in-memory, RWMutex-guarded snapshot of the active
// rows in mailing_smart_links, refreshed on a bounded background goroutine.
//
// Design invariants (this is the LIVE redirect service — see the file header
// discipline):
//   - A failed reload NEVER blanks the map: reloadOnce builds the next snapshot
//     fully and only swaps on success, so a transient DB error keeps the last
//     good snapshot serving.
//   - db == nil is a first-class, graceful mode: Lookup always misses, no
//     goroutine is started, and the service still serves /track/* and the /o/
//     fallback exactly as before. This is the default when DATABASE_URL is
//     unset or the connection can't be opened.
//   - Exactly one background goroutine; cancellable via Close() for clean
//     shutdown.
type SmartLinkDictionary struct {
	db      *sql.DB
	refresh time.Duration

	// loadFn produces the next full snapshot. Defaults to queryDB; overridable
	// in tests to inject a fake loader without a live DB.
	loadFn func(context.Context) (map[string]smartLinkEntry, error)

	mu      sync.RWMutex
	entries map[string]smartLinkEntry

	cancel context.CancelFunc
}

const smartLinkQuery = `SELECT hash, offer_url_template, COALESCE(risk_profile,'low'), brand_root
FROM mailing_smart_links
WHERE hash IS NOT NULL AND status='active'`

// NewSmartLinkDictionary loads the dictionary once synchronously (so the map is
// warm before the first request) and then reloads every refresh interval on a
// single background goroutine. If db is nil it returns a safe empty no-op: no
// goroutine, Lookup always false.
func NewSmartLinkDictionary(db *sql.DB, refresh time.Duration) *SmartLinkDictionary {
	if refresh <= 0 {
		refresh = 60 * time.Second
	}
	d := &SmartLinkDictionary{
		db:      db,
		refresh: refresh,
		entries: make(map[string]smartLinkEntry),
	}
	d.loadFn = d.queryDB

	if db == nil {
		log.Printf("smartlink dictionary: no DB handle — running as empty no-op (offer /o/ links fall back to brand root)")
		return d
	}

	// Initial synchronous load. A failure here is non-fatal: we serve an empty
	// map (every /o/ hit falls back to brand root) until the first successful
	// refresh repopulates it.
	if err := d.reloadOnce(context.Background()); err != nil {
		log.Printf("smartlink dictionary: initial load failed (serving empty until next refresh): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	go d.loop(ctx)
	return d
}

func (d *SmartLinkDictionary) loop(ctx context.Context) {
	t := time.NewTicker(d.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.reloadOnce(ctx); err != nil {
				log.Printf("smartlink dictionary: reload failed (keeping prior %d entries): %v", d.Len(), err)
			}
		}
	}
}

// reloadOnce builds the next snapshot via loadFn and swaps it in ONLY on
// success. On error the prior snapshot is untouched.
func (d *SmartLinkDictionary) reloadOnce(ctx context.Context) error {
	next, err := d.loadFn(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.entries = next
	d.mu.Unlock()
	log.Printf("smartlink dictionary: loaded %d active links", len(next))
	return nil
}

// queryDB is the DB-backed loader. It reads the full active set into a fresh
// map; any scan/query error aborts the WHOLE build (caller keeps the prior
// snapshot). offer_url_template and brand_root are read defensively as nullable
// so a stray NULL can't fail the entire reload.
func (d *SmartLinkDictionary) queryDB(ctx context.Context) (map[string]smartLinkEntry, error) {
	if d.db == nil {
		return map[string]smartLinkEntry{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	rows, err := d.db.QueryContext(ctx, smartLinkQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	next := make(map[string]smartLinkEntry)
	for rows.Next() {
		var hash string
		var tmpl, brand sql.NullString
		var risk string
		if err := rows.Scan(&hash, &tmpl, &risk, &brand); err != nil {
			return nil, err
		}
		hash = strings.TrimSpace(hash)
		dest := strings.TrimSpace(tmpl.String)
		if hash == "" || dest == "" {
			continue // unusable row — skip, don't fail the reload
		}
		next[hash] = smartLinkEntry{
			Destination: dest,
			RiskProfile: strings.ToLower(strings.TrimSpace(risk)),
			BrandRoot:   strings.TrimSpace(brand.String),
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return next, nil
}

// Lookup returns the entry for hash. Nil-receiver safe (a nil dictionary always
// misses) so the handler can hold a nil dict without special-casing.
func (d *SmartLinkDictionary) Lookup(hash string) (smartLinkEntry, bool) {
	if d == nil {
		return smartLinkEntry{}, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	e, ok := d.entries[hash]
	return e, ok
}

// Len is the current snapshot size (for logging/observability).
func (d *SmartLinkDictionary) Len() int {
	if d == nil {
		return 0
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.entries)
}

// Close cancels the background refresh goroutine. Idempotent and nil-safe.
func (d *SmartLinkDictionary) Close() {
	if d != nil && d.cancel != nil {
		d.cancel()
	}
}

// renderOfferDestination produces the final redirect target for an offer link.
//
//  1. Substitute mustache placeholders in the template with URL-encoded values.
//  2. ENSURE the Everflow attribution params exist — source_id=email,
//     sub1=<subscriberID>, sub2=<brandRoot>, sub3=<campaignID> — appending each
//     only if ABSENT so an operator who baked a param into the template wins and
//     nothing is duplicated. Parsing/serializing via net/url preserves any other
//     existing params and encodes values correctly.
//
// This is the server-side attribution that makes Everflow work from the
// scanner-safe path (the mustache render + sub-tags the email HTML used to carry).
//
// token is the OPAQUE per-recipient passthrough ({{token}}). It exists so an
// advertiser whose landing URL carries its own per-recipient id (e.g.
// AutoCoveragePoint's ?tokenid=<tid>) can be routed through this gateway — and
// therefore through the high-risk bridge — WITHOUT losing partner attribution.
// The gateway never interprets it and never looks it up: it arrives on the
// request URL (?t=), is sanitized to the RFC 3986 unreserved set by
// sanitizeOfferToken, and is substituted verbatim. An EMPTY token is a
// first-class case: {{token}} renders empty, the destination stays a valid URL,
// and a recipient with no id still reaches the offer. A template WITHOUT
// {{token}} is byte-identical to the pre-token output.
func renderOfferDestination(template, subscriberID, brandRoot, campaignID, token string) string {
	rendered := template
	rendered = strings.ReplaceAll(rendered, "{{subscriber.id}}", url.QueryEscape(subscriberID))
	rendered = strings.ReplaceAll(rendered, "{{brand.domain}}", url.QueryEscape(brandRoot))
	rendered = strings.ReplaceAll(rendered, "{{campaign}}", url.QueryEscape(campaignID))
	rendered = strings.ReplaceAll(rendered, "{{token}}", url.QueryEscape(token))

	u, err := url.Parse(rendered)
	if err != nil {
		// Unparseable after substitution — best effort: hand back the rendered
		// string so the redirect still goes somewhere rather than 500.
		return rendered
	}

	q := u.Query()
	setIfAbsent := func(k, v string) {
		if _, ok := q[k]; !ok {
			q.Set(k, v)
		}
	}
	setIfAbsent("source_id", "email")
	setIfAbsent("sub1", subscriberID)
	setIfAbsent("sub2", brandRoot)
	setIfAbsent("sub3", campaignID)
	u.RawQuery = q.Encode()
	return u.String()
}
