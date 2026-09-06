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
// ── GATE 2: SESSION-SHAPE FANOUT (GATEWAY_FANOUT_ENABLED, ships OFF) ─────────
//
// IP alone cannot clear Microsoft. Click sessions grouped by (subscriber,
// campaign) over 7 days, counting DISTINCT link_url:
//
//	residential (known humans)  7,044 sessions  1.27 links  76.4% single  1850s span
//	Microsoft                  19,840 sessions  1.89 links  30.5% single   547s span
//	AWS (already blocked)       6,162 sessions  2.16 links  10.7% single    62s span
//
// Microsoft is a genuinely MIXED population on shared addresses — the person and
// the sweep arrive from the same IP — so no IP rule separates them. SHAPE does: a
// sweep pulls several DISTINCT links within seconds; a person clicks one and
// stops. Gate 2 withholds only when ALL of:
//
//	1. the class is 'unresolved' OR 'hosting' — NEVER an unclassified/NULL
//	   address. NULL is the residential population (76% single-link humans) and
//	   is never gated on shape, no matter what it does.
//	2. this (subscriber, campaign) has already produced >= GATEWAY_FANOUT_LINKS
//	   (default 4) DISTINCT destinations, and
//	3. all of them fall inside one GATEWAY_FANOUT_WINDOW_S (default 60s) window.
//
// THE FIRST THREE FETCHES OF A SWEEP ARE FORWARDED, DELIBERATELY. That is what
// guarantees a human who clicks once — or twice, or three times — is never gated.
// We are cutting the TAIL of a sweep, not trying to catch its head. Raising the
// threshold is always safe; lowering it below 4 starts costing human clicks.
//
// Repeats of the SAME destination never count: only distinct destinations do, so
// a human reloading a link is not a fanout.
//
// SINGLE-INSTANCE STATE, ON PURPOSE. The counter is process-local (ignite-
// tracking-service is desiredCount=1). If the service is ever scaled out, each
// task sees only its own share of a session, so the gate fires LESS — never
// more. Degradation is toward forwarding, which is the fail-open direction.
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
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClassScanner is the ONE class that withholds. Every other value of
// ignite_ip_classification.class forwards, so this is deliberately a single
// constant and not a set — a new class added to the table's CHECK constraint
// forwards by default, which is the safe direction.
const ClassScanner = "scanner"

// ClassUnresolved and ClassHosting are the ONLY classes gate 2 (session-shape
// fanout) may act on. Everything else — and above all the no-row NULL that is
// the residential population — is outside the shape gate entirely.
const (
	ClassUnresolved = "unresolved"
	ClassHosting    = "hosting"
)

const (
	// GatewayEnforceEnv opts IN to actually withholding. Unset (the shipped
	// default) = shadow: classify, log, publish the marker, withhold nothing.
	GatewayEnforceEnv = "GATEWAY_ENFORCE"
	// GatewayDisabledEnv turns the gateway off entirely — no lookup, no
	// marker, no shadow logging. Kills gate 2 with everything else.
	GatewayDisabledEnv = "GATEWAY_DISABLED"

	// GatewayFanoutEnabledEnv arms GATE 2 (session-shape fanout). Unset (the
	// shipped default) = the gate does not run at all: no counter, no map, no
	// memory. Independent of GatewayEnforceEnv, which still governs whether a
	// gate-2 hit actually withholds or only shadow-records.
	GatewayFanoutEnabledEnv = "GATEWAY_FANOUT_ENABLED"
	// GatewayFanoutLinksEnv is the DISTINCT-destination threshold, default 4.
	// Raising it is always safe; below 4 starts costing human clicks.
	GatewayFanoutLinksEnv = "GATEWAY_FANOUT_LINKS"
	// GatewayFanoutWindowEnv is the window in SECONDS, default 60.
	GatewayFanoutWindowEnv = "GATEWAY_FANOUT_WINDOW_S"
)

// Gate 2 defaults. Applied whenever the env var is unset, empty, unparseable or
// non-positive — a typo degrades to the measured default, never to 0 (which
// would withhold on the first click).
const (
	defaultFanoutLinks  = 4
	defaultFanoutWindow = 60 * time.Second
)

// Telemetry markers for TrackingEvent.GatewayAction. A forwarded request
// carries "" so its event stays byte-identical to today's on the wire.
const (
	GatewayActionWithheld       = "withheld"
	GatewayActionShadowWithheld = "shadow_withheld"
	// Gate 2 carries its OWN markers so a shape withhold is separable from an
	// IP-class withhold in every downstream report. Never collapse these into
	// the two above.
	GatewayActionWithheldFanout       = "withheld_fanout"
	GatewayActionShadowWithheldFanout = "shadow_withheld_fanout"
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

// envPositiveInt reads a bounded knob at CALL time, same contract as envTrue.
// Anything unusable (unset, empty, non-numeric, <= 0) yields def.
func envPositiveInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func gatewayEnforcing() bool { return envTrue(GatewayEnforceEnv) }
func gatewayDisabled() bool  { return envTrue(GatewayDisabledEnv) }

func gatewayFanoutEnabled() bool { return envTrue(GatewayFanoutEnabledEnv) }
func gatewayFanoutLinks() int    { return envPositiveInt(GatewayFanoutLinksEnv, defaultFanoutLinks) }
func gatewayFanoutWindow() time.Duration {
	return time.Duration(envPositiveInt(GatewayFanoutWindowEnv, int(defaultFanoutWindow/time.Second))) * time.Second
}

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

	// fanout is GATE 2's per-(subscriber, campaign) distinct-destination
	// counter. It hangs off this struct rather than getting its own object so
	// the handler and cmd/tracking wiring are unchanged — the gateway is one
	// decision surface with two gates, not two things to wire. Zero value is
	// usable: &IPClassifier{} works, and a nil *IPClassifier never touches it.
	fanout sessionFanout

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
	// forward, "shadow_withheld"/"withheld" (gate 1, IP class), or
	// "shadow_withheld_fanout"/"withheld_fanout" (gate 2, session shape).
	Action string
	// Fanout is the DISTINCT-destination count that gate 2 observed for this
	// (subscriber, campaign) inside the window. 0 when gate 2 did not run.
	// Log/observability only — nothing branches on it.
	Fanout int
}

// Decide applies the gates to one request with NO session identity, so gate 2
// (session-shape fanout) cannot run and only the IP-class rule applies. Kept as
// the narrow entrypoint for callers that have no (subscriber, campaign) — and
// for the nil-safety contract every existing test pins.
func (c *IPClassifier) Decide(ip, destination string) GatewayDecision {
	return c.DecideSession(ip, destination, "", "")
}

// DecideSession applies BOTH gates to one request. Nil-receiver safe (a nil
// classifier forwards). Pure with respect to the advertiser: it reads the
// resolved destination string and in-memory state, and contacts nothing —
// invariant 1 holds for gate 2 exactly as it does for gate 1.
//
// Order is load-bearing:
//
//	disabled       -> forward, no marker at all
//	exempt dest    -> forward (invariant 3, BEFORE any classification)
//	class scanner  -> gate 1
//	otherwise      -> gate 2 (only for 'unresolved'/'hosting')
//	otherwise      -> forward
func (c *IPClassifier) DecideSession(ip, destination, subscriber, campaign string) GatewayDecision {
	if gatewayDisabled() {
		return GatewayDecision{}
	}
	// Invariant 3: exemption is checked BEFORE the class lookup, so an
	// unsubscribe link cannot be withheld even from a confirmed scanner
	// address — and it is never counted toward a fanout either.
	if isExemptDestination(destination) {
		return GatewayDecision{Exempt: true}
	}
	class := c.Classify(ip)
	if class == ClassScanner {
		if !gatewayEnforcing() {
			return GatewayDecision{Class: class, Shadow: true, Action: GatewayActionShadowWithheld}
		}
		return GatewayDecision{Class: class, Withhold: true, Action: GatewayActionWithheld}
	}
	// GATE 2. 'vpn-or-proxy', 'residential-or-mobile', 'unknown' and the no-row
	// NULL fall straight through this to the forward below — see the header.
	if d, hit := c.decideFanout(class, destination, subscriber, campaign, time.Now()); hit {
		return d
	}
	return GatewayDecision{Class: class}
}

// decideFanout is GATE 2. It returns (decision, true) ONLY when the shape rule
// fires; every other path returns hit=false and the caller forwards.
//
// FAIL OPEN AT EVERY STEP: nil receiver, gate not armed, a class outside
// {unresolved, hosting}, a missing subscriber/campaign/destination, or a
// counter that declines to track (at its bound) all return false.
func (c *IPClassifier) decideFanout(class, destination, subscriber, campaign string, now time.Time) (GatewayDecision, bool) {
	if c == nil || !gatewayFanoutEnabled() {
		return GatewayDecision{}, false
	}
	// Condition 1. NULL/unclassified is deliberately absent: that is the
	// residential population and it is NEVER gated on shape.
	if class != ClassUnresolved && class != ClassHosting {
		return GatewayDecision{}, false
	}
	// No session identity, nothing to count — forward.
	if subscriber == "" || campaign == "" || destination == "" {
		return GatewayDecision{}, false
	}
	// Conditions 2 and 3, both answered by one in-memory record: n is the
	// DISTINCT destination count for this (subscriber, campaign) inside the
	// current window, and the window is restarted whenever it has lapsed, so a
	// non-zero n is by construction "all within the window".
	n := c.fanout.record(subscriber, campaign, destination, now, gatewayFanoutWindow())
	if n < gatewayFanoutLinks() {
		return GatewayDecision{}, false
	}
	if !gatewayEnforcing() {
		return GatewayDecision{Class: class, Shadow: true, Action: GatewayActionShadowWithheldFanout, Fanout: n}, true
	}
	return GatewayDecision{Class: class, Withhold: true, Action: GatewayActionWithheldFanout, Fanout: n}, true
}

// -----------------------------------------------------------------------------
// GATE 2 — the per-(subscriber, campaign) fanout counter
// -----------------------------------------------------------------------------

// Bounds. Both exist so a flood cannot grow this map without limit; neither is
// a tuning knob and neither is env-readable.
//
//   - fanoutMaxSessions caps the live session set. AT the cap, a session we
//     have never seen is NOT tracked and its click FORWARDS — the bound is
//     enforced in the fail-open direction, never by evicting a session that is
//     mid-window (which would reset a sweep's counter and forward MORE).
//   - fanoutMaxDestsPerSession caps one session's distinct-destination set. A
//     session that reaches it is already far past any sane threshold, so
//     freezing the count there changes no decision.
const (
	fanoutMaxSessions        = 100_000
	fanoutMaxDestsPerSession = 32
)

// fanoutSession is one (subscriber, campaign) inside ONE window. startedAt is
// the window origin, not the session origin: it is reset whenever the window
// lapses, which is what makes "all N inside 60s" true by construction rather
// than something the caller has to re-derive.
type fanoutSession struct {
	startedAt time.Time
	lastAt    time.Time
	dests     map[string]struct{}
}

// sessionFanout is the process-local counter. Plain Mutex, not RWMutex: every
// operation on it writes. The map is created lazily so the zero value is
// usable and so an unarmed gate allocates NOTHING.
type sessionFanout struct {
	mu        sync.Mutex
	sessions  map[string]*fanoutSession
	lastSweep time.Time
}

// record adds one click and returns the DISTINCT destination count for this
// (subscriber, campaign) within the current window. A return of 0 means "not
// counted" — bad input or at the session bound — and the caller forwards.
//
// Repeats of the same destination do not increment: the measured separator is
// distinct links, and a human reloading one link must never look like a sweep.
func (f *sessionFanout) record(subscriber, campaign, destination string, now time.Time, window time.Duration) int {
	if f == nil || subscriber == "" || campaign == "" || destination == "" || window <= 0 {
		return 0
	}
	key := subscriber + "|" + campaign

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.sessions == nil {
		f.sessions = make(map[string]*fanoutSession)
		f.lastSweep = now
	}
	f.sweepLocked(now, window)

	s := f.sessions[key]
	if s == nil {
		if len(f.sessions) >= fanoutMaxSessions {
			return 0 // at the bound: track nothing new, forward
		}
		s = &fanoutSession{startedAt: now, dests: make(map[string]struct{}, 4)}
		f.sessions[key] = s
	}
	// Window lapsed (or the clock went backwards) -> this click starts a new
	// window with a fresh distinct set. A sweep that paces itself slower than
	// the window is therefore never gated, which is the intended trade.
	if now.Sub(s.startedAt) > window || now.Before(s.startedAt) {
		s.startedAt = now
		s.dests = make(map[string]struct{}, 4)
	}
	s.lastAt = now
	if _, seen := s.dests[destination]; !seen && len(s.dests) < fanoutMaxDestsPerSession {
		s.dests[destination] = struct{}{}
	}
	return len(s.dests)
}

// sweepLocked drops sessions idle longer than one window. Cheap because it runs
// at most once per window — except at the session bound, where it runs on every
// record so the map has a chance to drain before new sessions are refused.
// Caller holds f.mu.
func (f *sessionFanout) sweepLocked(now time.Time, window time.Duration) {
	if now.Sub(f.lastSweep) < window && len(f.sessions) < fanoutMaxSessions {
		return
	}
	f.lastSweep = now
	for k, s := range f.sessions {
		if now.Sub(s.lastAt) > window {
			delete(f.sessions, k)
		}
	}
}

// size is the live session count (tests and observability only).
func (f *sessionFanout) size() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
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
