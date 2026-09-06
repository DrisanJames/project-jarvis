package tracking

// =============================================================================
// The offer gateway — class lookup, not scoring
// =============================================================================
//
// One question, one table. `ignite_ip_classification` (cmd/server/
// ip_classification.go, commit 0e6087e7) answers what a prefix DOES, resolved
// narrowest-match-wins by ignite_ip_class(). The gateway asks that question and
// acts on the answer:
//
//	class = 'scanner'                                   -> WITHHOLD
//	everything else — including 'unresolved', 'hosting',
//	'vpn-or-proxy', 'residential-or-mobile', 'unknown',
//	and NULL (no row at all)                            -> FORWARD
//
// 'unresolved' is NOT a placeholder and NOT a tie to be broken. It is the
// recorded fact that the 18 seeded /32s carry BOTH scanner sweeps AND real
// people, so no single label about them is true. Forwarding them is the
// deliberate choice: the cost of a scanner reaching an advertiser is noise, the
// cost of withholding a real person's click is a lost conversion the human
// cannot retry. Do not add a heuristic here to guess between them.
//
// DEFAULT IS SHADOW. Unset GATEWAY_ENFORCE and this unit withholds NOTHING — it
// records what it WOULD have withheld and forwards every request. Both env vars
// are read at CALL time, never captured at construction, so enforcement can be
// armed or dropped by an ECS env change without a code deploy.
//
// ── THE FIVE INVARIANTS ──────────────────────────────────────────────────────
//
//  1. DECIDE BEFORE THE ADVERTISER IS CONTACTED. There is no HTTP client on this
//     path and none may be added: fetching the destination server-side to
//     resolve or inspect it would itself register a click on the advertiser's
//     counter, which is the exact event this gateway exists to suppress.
//
//  2. NO-STORE ON EVERY RESPONSE, forwarded and withheld alike (writeNoStore).
//     A withheld 204 that an intermediary caches and replays to a later human
//     request for the same URL denies a real person their click silently and
//     unrecoverably. CloudFront in front of us is already
//     Managed-CachingDisabled; these headers are for the intermediaries we do
//     not control.
//
//  3. UNSUBSCRIBE AND PREFERENCE DESTINATIONS ARE EXEMPT, checked BEFORE the
//     class lookup — see isExemptDestination.
//
//  4. TELEMETRY IS PUBLISHED FOR WITHHELD REQUESTS TOO. We suppress the
//     ADVERTISER hop, never our own visibility. TrackingEvent.GatewayAction
//     carries "withheld" / "shadow_withheld" so a suppressed click is still a
//     row in our data and is distinguishable from a forwarded one.
//
//  5. FAIL OPEN EVERYWHERE. Nil classifier, no DATABASE_URL, DB unreachable,
//     failed load, empty table, unparseable IP, no matching row — every one of
//     those forwards. The ONLY thing that withholds is a live, positive
//     'scanner' match with enforcement explicitly armed.
//
// WITHHELD RESPONSE IS 204 No Content: no Location, no body, no advertiser
// resources — and NEVER the brand site or any of our own content. Serving a
// scanner different CONTENT from a human is the 2026-07-22 cloaking mistake
// this estate already made and got ruled on. 204 is not different content; it
// is the absence of the advertiser handoff.

import (
	"context"
	"database/sql"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ClassScanner is the ONE class that withholds. Every other value of
// ignite_ip_classification.class forwards, so this is deliberately a single
// constant and not a set — a new class added to the table's CHECK constraint
// forwards by default, which is the safe direction.
const ClassScanner = "scanner"

const (
	// GatewayEnforceEnv opts IN to actually withholding. Unset (the shipped
	// default) = shadow: classify, log, publish the marker, withhold nothing.
	GatewayEnforceEnv = "GATEWAY_ENFORCE"
	// GatewayDisabledEnv turns the gateway off entirely — no lookup, no
	// marker, no shadow logging.
	GatewayDisabledEnv = "GATEWAY_DISABLED"
)

// Telemetry markers for TrackingEvent.GatewayAction. A forwarded request
// carries "" so its event stays byte-identical to today's on the wire.
const (
	GatewayActionWithheld       = "withheld"
	GatewayActionShadowWithheld = "shadow_withheld"
)

// envTrue reads a flag at CALL time. Deliberately not cached: the operator
// arms and disarms enforcement with an env change, not a deploy.
func envTrue(k string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func gatewayEnforcing() bool { return envTrue(GatewayEnforceEnv) }
func gatewayDisabled() bool  { return envTrue(GatewayDisabledEnv) }

// -----------------------------------------------------------------------------
// Exempt destinations (invariant 3)
// -----------------------------------------------------------------------------

// gatewayExemptTokens are matched against the DESTINATION URL, before any class
// lookup. Withholding an unsubscribe or preference-centre click is a CAN-SPAM /
// RFC 8058 problem, not a lost conversion, so this check runs first and is
// deliberately biased toward over-matching: an over-exempted request is simply
// FORWARDED, which is the gateway's own fail-open default.
//
// This is not theoretical. internal/mailing/tracking.go:338 and :372 both say
// "Skip tracking/unsubscribe URLs" but test only for "/track/", so unsubscribe
// destinations ARE wrapped and DO arrive here (1,757 such clicks in 3 days).
// Fixing that wrapper is a DIFFERENT unit with a different blast radius — this
// gateway must be correct whether or not it is ever fixed.
//
// The tokens are matched against the whole lowercased URL rather than only the
// path, again because the failure mode of a false match (forward) is harmless
// and the failure mode of a miss (withhold an unsubscribe) is not.
var gatewayExemptTokens = []string{
	"affiliateaccesskey",
	"samestoreteam",
	"unsubscribe",
	"preference",
}

func isExemptDestination(dest string) bool {
	d := strings.ToLower(dest)
	if d == "" {
		return false
	}
	for _, t := range gatewayExemptTokens {
		if strings.Contains(d, t) {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// In-memory mirror of ignite_ip_classification
// -----------------------------------------------------------------------------

// ipClassEntry is one active row: a prefix and the behaviour class asserted for
// it.
type ipClassEntry struct {
	net   *net.IPNet
	class string
}

// IPClassifier is an in-memory, RWMutex-guarded snapshot of the active rows in
// ignite_ip_classification, refreshed on a bounded background goroutine. It
// mirrors SmartLinkDictionary (dictionary.go) deliberately — same nil-db mode,
// same build-then-swap reload, same single cancellable goroutine — because that
// is how this service already reads a table, and the click path must never do a
// query.
//
// Design invariants:
//   - RESOLUTION IS NARROWEST-MATCH-WINS, matching ignite_ip_class(). The set is
//     sorted by prefix length DESC once at load, and Classify returns the FIRST
//     containing match. A /32 therefore beats the /16 that contains it, which is
//     the entire reason the table exists: a curated observed address carves a
//     real verdict out of a blanket ownership prefix.
//   - NO QUERY ON THE HOT PATH. The whole table is tens of rows; the click path
//     walks memory only.
//   - A FAILED OR EMPTY LOAD KEEPS THE PREVIOUS SET. An empty result is far
//     likelier a mistake (wrong DB, truncated table, mid-migration boot) than a
//     real state, and clearing the set would change behaviour silently. Note the
//     direction: keeping a stale set can only keep withholding known scanners,
//     never start withholding someone new.
//   - db == nil is a first-class mode (no DATABASE_URL, or open/ping failed):
//     no goroutine, Classify always returns "", every request forwards.
type IPClassifier struct {
	db      *sql.DB
	refresh time.Duration

	// loadFn produces the next full snapshot. Defaults to queryDB; overridable
	// in tests to inject a fake loader without a live DB.
	loadFn func(context.Context) ([]ipClassEntry, error)

	mu      sync.RWMutex
	entries []ipClassEntry

	cancel context.CancelFunc
}

// ipClassQuery reads the same rows ignite_ip_class() resolves over. is_active is
// the retirement switch (the table never DELETEs), so an inactive row must not
// reach memory.
const ipClassQuery = `SELECT cidr::text, class FROM ignite_ip_classification WHERE is_active`

// NewIPClassifier loads the table once synchronously (so the set is warm before
// the first click) and then reloads every refresh interval on one background
// goroutine. If db is nil it returns a safe empty no-op: no goroutine, Classify
// always "", the gateway forwards everything.
func NewIPClassifier(db *sql.DB, refresh time.Duration) *IPClassifier {
	if refresh <= 0 {
		refresh = 15 * time.Minute
	}
	c := &IPClassifier{db: db, refresh: refresh}
	c.loadFn = c.queryDB

	if db == nil {
		log.Printf("ip classifier: no DB handle — offer gateway forwards every request")
		return c
	}

	// Initial synchronous load. A failure is non-fatal: an empty set forwards
	// everything until the first successful refresh.
	if err := c.reloadOnce(context.Background()); err != nil {
		log.Printf("ip classifier: initial load failed (gateway forwards everything until next refresh): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.loop(ctx)
	return c
}

func (c *IPClassifier) loop(ctx context.Context) {
	t := time.NewTicker(c.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.reloadOnce(ctx); err != nil {
				log.Printf("ip classifier: reload failed (keeping prior %d prefixes): %v", c.Len(), err)
			}
		}
	}
}

// reloadOnce builds the next snapshot via loadFn and swaps it in ONLY on
// success AND only when it is non-empty. On error, or on an empty result, the
// prior snapshot is untouched.
func (c *IPClassifier) reloadOnce(ctx context.Context) error {
	next, err := c.loadFn(ctx)
	if err != nil {
		return err
	}
	if len(next) == 0 {
		log.Printf("ip classifier: load returned 0 prefixes — keeping prior %d (an empty table is likelier a mistake than a real state)", c.Len())
		return nil
	}
	// Narrowest first, once, at load — Classify then returns the first
	// containing match and never has to compare mask lengths per request.
	sort.SliceStable(next, func(i, j int) bool {
		oi, _ := next[i].net.Mask.Size()
		oj, _ := next[j].net.Mask.Size()
		return oi > oj
	})
	c.mu.Lock()
	c.entries = next
	c.mu.Unlock()
	log.Printf("ip classifier: loaded %d active prefixes from ignite_ip_classification", len(next))
	return nil
}

// queryDB is the DB-backed loader. Any query/scan error aborts the WHOLE build
// so the caller keeps the prior snapshot; an individual unparseable row is
// skipped rather than failing the reload.
func (c *IPClassifier) queryDB(ctx context.Context) ([]ipClassEntry, error) {
	if c.db == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	rows, err := c.db.QueryContext(ctx, ipClassQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var next []ipClassEntry
	for rows.Next() {
		var cidr, class string
		if err := rows.Scan(&cidr, &class); err != nil {
			return nil, err
		}
		_, n, perr := net.ParseCIDR(strings.TrimSpace(cidr))
		if perr != nil || n == nil {
			continue // unusable row — skip, don't fail the reload
		}
		next = append(next, ipClassEntry{net: n, class: strings.ToLower(strings.TrimSpace(class))})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return next, nil
}

// Classify returns the behaviour class for ipStr, or "" when the classifier is
// nil/empty, the address is unparseable, or no active prefix contains it.
// "" is the NULL of ignite_ip_class(): "we have no row", which forwards.
//
// Nil-receiver safe so the handler can hold a nil classifier without
// special-casing — the same contract SmartLinkDictionary.Lookup carries.
func (c *IPClassifier) Classify(ipStr string) string {
	if c == nil {
		return ""
	}
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.entries {
		if e.net.Contains(ip) {
			return e.class // sorted narrowest-first: first hit is the answer
		}
	}
	return ""
}

// Len is the current snapshot size (for logging/observability).
func (c *IPClassifier) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Close cancels the background refresh goroutine. Idempotent and nil-safe.
func (c *IPClassifier) Close() {
	if c != nil && c.cancel != nil {
		c.cancel()
	}
}

// -----------------------------------------------------------------------------
// The decision
// -----------------------------------------------------------------------------

// GatewayDecision is what a click handler acts on. It is kept whole rather than
// collapsed to a bool so the log line and the telemetry marker can say WHY.
type GatewayDecision struct {
	// Withhold is true ONLY for: gateway enabled, destination not exempt,
	// class == 'scanner', and GATEWAY_ENFORCE armed. Everything else forwards.
	Withhold bool
	// Class is the resolved class, "" when there is no matching row (or the
	// gateway is disabled).
	Class string
	// Exempt records that the destination short-circuited the lookup.
	Exempt bool
	// Shadow records a request that WOULD have been withheld but was forwarded
	// because enforcement is not armed. This is the default posture.
	Shadow bool
	// Action is the TrackingEvent.GatewayAction marker: "" for a normal
	// forward, "shadow_withheld", or "withheld".
	Action string
}

// Decide applies the rule to one request. Nil-receiver safe (a nil classifier
// forwards). Pure with respect to the advertiser: it reads the resolved
// destination string and in-memory state, and contacts nothing.
func (c *IPClassifier) Decide(ip, destination string) GatewayDecision {
	if gatewayDisabled() {
		return GatewayDecision{}
	}
	// Invariant 3: exemption is checked BEFORE the class lookup, so an
	// unsubscribe link cannot be withheld even from a confirmed scanner
	// address.
	if isExemptDestination(destination) {
		return GatewayDecision{Exempt: true}
	}
	class := c.Classify(ip)
	if class != ClassScanner {
		// 'unresolved', 'hosting', 'vpn-or-proxy', 'residential-or-mobile',
		// 'unknown' and no-row all land here. Deliberate — see the file header.
		return GatewayDecision{Class: class}
	}
	if !gatewayEnforcing() {
		return GatewayDecision{Class: class, Shadow: true, Action: GatewayActionShadowWithheld}
	}
	return GatewayDecision{Class: class, Withhold: true, Action: GatewayActionWithheld}
}

// -----------------------------------------------------------------------------
// Responses
// -----------------------------------------------------------------------------

// writeNoStore stamps invariant 2 on a gateway response. Called at the TOP of
// each click handler so every exit — forward, withhold, bad-link, dictionary
// miss, panic fallback — carries it.
func writeNoStore(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate, private, max-age=0")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
}

// writeWithheld ends a withheld request: 204, no Location, no body, nothing of
// ours and nothing of the advertiser's. The caller has already stamped
// writeNoStore and published the telemetry event.
func writeWithheld(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}
