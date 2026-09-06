package worker

// BotIPNominator — "behaviour finds them, infrastructure blocks them", ported
// from the laptop cron agents/jobs/bot_ip_nominate.py (commit e57deebe) into
// the server fleet. The Python is the spec; the rule, the SQL, the containment
// lookup and the upsert are reproduced here verbatim, not redesigned.
//
// THE BEHAVIOUR — link-agnostic (operator 2026-09-06: "it should be behavior,
// not which links"): the SAME subscriber fetched TWO OR MORE DISTINCT link_url
// values within botNominateCoClickS seconds. A person clicks one link; a sweep
// fetches several. DISTINCT is load-bearing: one click writes up to ~8 rows
// (duplicate logging), so repeats of ONE link are a human and two DIFFERENT
// links inside 5s are the bot.
//
// AN IP IS NOMINATED WHEN, inside the window:
//   - >= botNominateMinSubs distinct subscribers clicked from it, AND
//   - >= botNominateMinRate of those subscribers showed the burst signature.
//
// A single-subscriber IP is NEVER nominated: that is a home connection.
//
// TWO SOURCES, ONE RULE
//   - pg   — every tick (15 min), the last BOT_NOMINATE_PG_WINDOW_MIN minutes of
//     mailing_tracking_events. event_type is 'clicked' (PAST tense; 'click' is
//     a silent zero in Postgres).
//   - lake — once a day at ~03:10 America/Denver, the last BOT_NOMINATE_LAKE_DAYS
//     days of ignite_analytics.email_events via the Athena reader
//     (internal/analytics/reader_bot_nominate.go). event_type is 'click' there,
//     partition column dt, timestamps event_epoch_ms, IP source_ip.
//
// CONTAINMENT, NOT EXACT MATCH: before writing, every nominee is resolved with
// ignite_ip_class(ip::inet). An IP already inside a blocked RANGE is already
// scanner to the gateway (narrowest-match) and must not be re-added.
//
// THE WRITE is the Python's apply(): one transaction, INSERT ... ON CONFLICT
// (cidr) DO UPDATE ... WHERE class <> 'scanner', evidence_source =
// 'bot-nominate-<YYYYMMDD>' (UTC). /32 for v4, /128 for v6 (Postgres rejects a
// v6 with /32). Revert any day with ONE statement:
//
//	DELETE FROM ignite_ip_classification WHERE evidence_source='bot-nominate-<YYYYMMDD>'
//
// The offer gateway (internal/tracking/gateway.go IPClassifier) reloads the
// table on its own 15-minute cadence, so a written row takes effect within
// one reload — the SUMMARY line says so.
//
// HOLD-CRITICAL: this writes to the table the money-path gateway enforces from.
//
// Kill switch: BOT_NOMINATE_ENABLED — code default OFF; a disabled tick logs
// one line and touches nothing (no lock, no query, no heartbeat).
// Env, all read at tick time: BOT_NOMINATE_ENABLED, BOT_NOMINATE_PG_WINDOW_MIN
// (30), BOT_NOMINATE_LAKE_DAYS (30).
//
// LOGGING is the operator's stated requirement: every tick, one line per step,
// prefix "[BotNominate]", so a reviewer can trace a pass end to end from
// CloudWatch. Errors are logged with the step they happened in and never
// swallowed. There is no Go change-ledger writer in internal/ (the Python's
// change_ledger.record has no counterpart here), so the SUMMARY line is the
// record of each write.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	botNominateWorkerName = "bot_ip_nominator"
	botNominateLockKey    = "bot_ip_nominator"

	// DefaultBotNominateInterval is the PG cadence: last N minutes, every 15.
	DefaultBotNominateInterval = 15 * time.Minute

	// The rule. These three render into BOTH queries and gate the Go-side
	// filter, so the SQL and the fixture cannot disagree.
	botNominateMinSubs   = 2
	botNominateMinRate   = 0.8
	botNominateCoClickS  = 5
	botNominateRuleLabel = ">=2 subs, >=80% burst: 2+ distinct links <=5s"

	defaultBotNominatePGWindowMin = 30
	defaultBotNominateLakeDays    = 30

	// Lake pass fires on the first tick at/after 03:10 America/Denver and no
	// later than one hour after, once per Denver day per process.
	botNominateLakeHour   = 3
	botNominateLakeMinute = 10
	botNominateLakeWindow = time.Hour

	// Budgets. PG nomination mirrors the Python's statement_timeout_ms=280_000;
	// lookup/upsert mirror its 120s. The lake query is bounded by the Athena
	// poll loop in analytics.Reader.runQuery.
	botNominatePGBudget     = 280 * time.Second
	botNominateLookupBudget = 120 * time.Second
	botNominateUpsertBudget = 120 * time.Second
	botNominateLakeBudget   = 10 * time.Minute
)

// BotNominee is one IP matching the signature, with its evidence.
type BotNominee struct {
	IP      string
	Subs    int64
	CoClick int64
	Clicks  int64
}

// BotNominateLakeReader is the Athena seam (tests inject a fake).
type BotNominateLakeReader interface {
	Nominate(ctx context.Context, days int) ([]BotNominee, error)
}

// botNominateAnalyticsReader adapts the analytics package reader.
type botNominateAnalyticsReader struct{}

func (botNominateAnalyticsReader) Nominate(ctx context.Context, days int) ([]BotNominee, error) {
	rows, err := analytics.BotNominateLake(ctx, days, botNominateMinSubs, botNominateMinRate, botNominateCoClickS)
	if err != nil {
		return nil, err
	}
	out := make([]BotNominee, 0, len(rows))
	for _, r := range rows {
		out = append(out, BotNominee{IP: r.IP, Subs: r.Subs, CoClick: r.CoClick, Clicks: r.Clicks})
	}
	return out, nil
}

// BotNominateResult is what one pass did. Returned by RunOnce so tests and
// tooling can assert on it; the SUMMARY log line is rendered from it.
type BotNominateResult struct {
	Source         string // "pg" | "lake"
	Window         string // "30m" | "30d"
	Status         string // "ok" | "error" | "disabled" | "lake-disabled"
	Step           string // step the error happened in, if any
	Err            error
	Rows           int // rows the query returned
	Matched        int // rows passing the rule
	Dropped        int // rows the source returned that fail the rule (defensive; expected 0)
	SkippedScanner int // containment hits — already scanner to the gateway
	Invalid        int // unparseable IPs (logged, never written)
	New            int // nominees not yet scanner
	Written        int // rows the upsert actually changed
	Tag            string
	Nominees       []BotNominee
}

// ── env (read at tick time) ─────────────────────────────────────────────────

// BotNominateEnabled is the kill switch. Code default OFF; the deploy turns it
// on explicitly via deploy/env.manifest.json (BOT_NOMINATE_ENABLED=1).
func BotNominateEnabled() bool { return envFlagOn("BOT_NOMINATE_ENABLED") }

// BotNominatePGWindowMin is the PG lookback in minutes (default 30).
func BotNominatePGWindowMin() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("BOT_NOMINATE_PG_WINDOW_MIN"))); err == nil && v > 0 {
		return v
	}
	return defaultBotNominatePGWindowMin
}

// BotNominateLakeDays is the lake lookback in days (default 30).
func BotNominateLakeDays() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("BOT_NOMINATE_LAKE_DAYS"))); err == nil && v > 0 {
		return v
	}
	return defaultBotNominateLakeDays
}

// ── the rule, the SQL, the cidr ─────────────────────────────────────────────

// botNominateRule is the nomination predicate, identical to the WHERE clause
// both queries render. Applied again in Go so a row that reaches the writer
// has provably passed it: a single-subscriber IP is never nominated.
func botNominateRule(subs, coclick int64) bool {
	if subs < botNominateMinSubs {
		return false
	}
	return float64(coclick)/float64(subs) >= botNominateMinRate
}

// BuildBotNominatePGSQL renders the Postgres nomination query for the last
// `minutes` minutes — nominate_pg() verbatim. PAST-tense 'clicked'. The burst
// CTE is a self-join on subscriber: b.link_url <> a.link_url within the window.
func BuildBotNominatePGSQL(minutes int) string {
	if minutes <= 0 {
		minutes = defaultBotNominatePGWindowMin
	}
	return fmt.Sprintf(`
    WITH ev AS (SELECT subscriber_id, ip_address, link_url, event_at FROM mailing_tracking_events
                WHERE event_type='clicked' AND event_at >= NOW() - INTERVAL '%d minutes' AND ip_address IS NOT NULL),
    mo AS (SELECT subscriber_id, ip_address, link_url, event_at FROM ev),
    cc AS (SELECT DISTINCT a.subscriber_id FROM mo a JOIN mo b ON b.subscriber_id=a.subscriber_id
           AND b.link_url <> a.link_url AND abs(EXTRACT(EPOCH FROM (b.event_at-a.event_at))) <= %d),
    per AS (SELECT host(mo.ip_address) ip, count(DISTINCT mo.subscriber_id) subs,
                   count(DISTINCT cc.subscriber_id) coclick, count(*) clicks
            FROM mo LEFT JOIN cc USING (subscriber_id) GROUP BY 1)
    SELECT ip, subs, coclick, clicks FROM per
    WHERE subs >= %d AND coclick::numeric/subs >= %s ORDER BY clicks DESC`,
		minutes, botNominateCoClickS,
		botNominateMinSubs, strconv.FormatFloat(botNominateMinRate, 'f', -1, 64))
}

// botNominateHostCIDR renders the host prefix: /32 for v4, /128 for v6 — a v6
// address with /32 is rejected by Postgres. Mirrors ipaddress.ip_address(ip)
// .version: a v4-mapped v6 literal ("::ffff:1.2.3.4") is version 6 there too.
func botNominateHostCIDR(ip string) (string, error) {
	ip = strings.TrimSpace(ip)
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("not an IP address: %q", ip)
	}
	if strings.Contains(ip, ":") {
		return ip + "/128", nil
	}
	return ip + "/32", nil
}

// botNominateEvidence is the evidence jsonb, field order as the Python's
// dict(x, rule=..., source=...).
type botNominateEvidence struct {
	IP      string `json:"ip"`
	Subs    int64  `json:"subs"`
	CoClick int64  `json:"coclick"`
	Clicks  int64  `json:"clicks"`
	Rule    string `json:"rule"`
	Source  string `json:"source"`
}

func botNominateEvidenceJSON(n BotNominee, source string) string {
	// json.Marshal would HTML-escape the rule's '<' and '>' as \u003c/\u003e;
	// the evidence must read verbatim, as the Python writes it.
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(botNominateEvidence{IP: n.IP, Subs: n.Subs, CoClick: n.CoClick, Clicks: n.Clicks,
		Rule: botNominateRuleLabel, Source: source})
	return strings.TrimRight(b.String(), "\n")
}

// botNominateTag is the evidence_source tag for the UTC day.
func botNominateTag(now time.Time) string {
	return "bot-nominate-" + now.UTC().Format("20060102")
}

const botNominateLookupSQL = `SELECT t.ip, ignite_ip_class(t.ip::inet) FROM unnest($1::text[]) AS t(ip)`

const botNominateUpsertSQL = `INSERT INTO ignite_ip_classification (cidr,class,attributes,confidence,evidence_source,evidence,note)
              VALUES ($1::cidr,'scanner',ARRAY['observed'],'confirmed',$2,$3::jsonb,'behaviour-nominated scanner')
              ON CONFLICT (cidr) DO UPDATE SET class='scanner', confidence='confirmed', evidence_source=EXCLUDED.evidence_source,
                evidence=EXCLUDED.evidence, last_confirmed_at=now(), updated_at=now()
              WHERE ignite_ip_classification.class <> 'scanner'`

// ── the worker ──────────────────────────────────────────────────────────────

// BotIPNominator owns the 15-minute PG pass and the daily lake pass. Construct
// via NewBotIPNominator, then Start(ctx) once at boot.
type BotIPNominator struct {
	db       *sql.DB
	redis    *redis.Client
	interval time.Duration
	loc      *time.Location
	now      func() time.Time

	lake          BotNominateLakeReader
	lakeAvailable func() bool

	mu          sync.Mutex
	lastLakeDay string // Denver day of the last lake pass this process ran
}

// NewBotIPNominator wires the worker. redisClient may be nil — the distlock
// falls back to a PG advisory lock (the sibling contract, see lane_snapshot.go).
func NewBotIPNominator(db *sql.DB, redisClient *redis.Client) *BotIPNominator {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[BotNominate] America/Denver tz unavailable (%v) — lake pass will key off UTC", err)
		loc = time.UTC
	}
	return &BotIPNominator{
		db:            db,
		redis:         redisClient,
		interval:      DefaultBotNominateInterval,
		loc:           loc,
		now:           time.Now,
		lake:          botNominateAnalyticsReader{},
		lakeAvailable: analytics.ReaderEnabled,
	}
}

// WithInterval overrides the tick cadence (tests). Call before Start.
func (w *BotIPNominator) WithInterval(d time.Duration) *BotIPNominator {
	if d > 0 {
		w.interval = d
	}
	return w
}

// SetLakeReader injects the Athena seam (tests). Call before Start.
func (w *BotIPNominator) SetLakeReader(r BotNominateLakeReader) *BotIPNominator {
	w.lake = r
	w.lakeAvailable = func() bool { return true }
	return w
}

// SetNow injects the clock (tests). Call before Start.
func (w *BotIPNominator) SetNow(f func() time.Time) *BotIPNominator {
	if f != nil {
		w.now = f
	}
	return w
}

// Start runs the tick loop until ctx is cancelled. Non-blocking; no-op if db
// is nil.
func (w *BotIPNominator) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[BotNominate] disabled (db missing)")
		return
	}
	go func() {
		log.Printf("Bot IP nominator started (interval=%s, pg_window=%dm, lake_days=%d, lake_at=%02d:%02d %s, enabled=%v)",
			w.interval, BotNominatePGWindowMin(), BotNominateLakeDays(), botNominateLakeHour, botNominateLakeMinute, w.loc, BotNominateEnabled())
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-ctx.Done():
				log.Printf("[BotNominate] context cancelled, stopping")
				return
			}
		}
	}()
}

// tick is one leased pass: the PG pass every tick, the lake pass when due.
// Returns the tick status ("disabled" | "lock-held" | "lock-error" | "ran")
// so tests can pin the kill switch: disabled = no lock, no query, nothing.
func (w *BotIPNominator) tick(ctx context.Context) string {
	if ctx.Err() != nil {
		return "cancelled"
	}
	pgWindow := fmt.Sprintf("%dm", BotNominatePGWindowMin())
	if !BotNominateEnabled() {
		log.Printf("[BotNominate] tick start source=pg window=%s enabled=false lock=skipped — BOT_NOMINATE_ENABLED unset, no query issued", pgWindow)
		return "disabled"
	}
	lock := distlock.NewLock(w.redis, w.db, botNominateLockKey, w.interval)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[BotNominate] ERROR step=lock source=pg window=%s: %v", pgWindow, err)
		return "lock-error"
	}
	if !acquired {
		log.Printf("[BotNominate] tick start source=pg window=%s enabled=true lock=held-elsewhere — skipping", pgWindow)
		return "lock-held"
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[BotNominate] ERROR step=lock-release: %v", err)
		}
	}()
	log.Printf("[BotNominate] tick start source=pg window=%s enabled=true lock=acquired", pgWindow)
	w.RunOnce(ctx, "pg")

	if day, due := w.lakeDue(w.now()); due {
		log.Printf("[BotNominate] tick start source=lake window=%dd enabled=true lock=acquired (daily pass for %s)", BotNominateLakeDays(), day)
		res := w.RunOnce(ctx, "lake")
		if res.Status != "lake-disabled" {
			w.mu.Lock()
			w.lastLakeDay = day
			w.mu.Unlock()
		}
	}
	return "ran"
}

// lakeDue reports whether the daily lake pass should run now: local time is
// inside [03:10, 04:10) and this process has not run it for today's local day.
func (w *BotIPNominator) lakeDue(now time.Time) (string, bool) {
	local := now.In(w.loc)
	day := local.Format("2006-01-02")
	start := time.Date(local.Year(), local.Month(), local.Day(), botNominateLakeHour, botNominateLakeMinute, 0, 0, w.loc)
	if local.Before(start) || !local.Before(start.Add(botNominateLakeWindow)) {
		return day, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return day, w.lastLakeDay != day
}

// RunOnce executes one nomination pass for source "pg" or "lake". Exported so
// tests and tooling can drive a pass directly; the caller owns locking.
//
// RE-RUN SAFE BY CONSTRUCTION: the write is an upsert guarded by
// WHERE class <> 'scanner' and preceded by a containment lookup, so a second
// pass over the same window writes zero rows.
func (w *BotIPNominator) RunOnce(ctx context.Context, source string) *BotNominateResult {
	res := &BotNominateResult{Source: source, Tag: botNominateTag(w.now())}
	var window string
	switch source {
	case "pg":
		window = fmt.Sprintf("%dm", BotNominatePGWindowMin())
	case "lake":
		window = fmt.Sprintf("%dd", BotNominateLakeDays())
	default:
		res.Status, res.Step, res.Err = "error", "source", fmt.Errorf("unknown source %q", source)
		log.Printf("[BotNominate] ERROR step=source: %v", res.Err)
		return res
	}
	res.Window = window
	srcLabel := source + ":" + window // evidence.source, as the Python's f"pg:{m}m" / f"lake:{d}d"

	fail := func(step string, err error) *BotNominateResult {
		res.Status, res.Step, res.Err = "error", step, err
		log.Printf("[BotNominate] ERROR step=%s source=%s window=%s: %v", step, source, window, err)
		EmitHeartbeat(ctx, w.db, botNominateWorkerName, int(w.interval.Seconds()), "error", step+": "+err.Error())
		return res
	}

	// (2) the query
	var rows []BotNominee
	var err error
	qStart := w.now()
	switch source {
	case "pg":
		rows, err = w.nominatePG(ctx, BotNominatePGWindowMin())
	case "lake":
		if w.lakeAvailable != nil && !w.lakeAvailable() {
			res.Status = "lake-disabled"
			log.Printf("[BotNominate] query source=lake window=%s skipped — lake reader disabled (ANALYTICS_ATHENA_OUTPUT unset)", window)
			return res
		}
		lctx, lcancel := context.WithTimeout(ctx, botNominateLakeBudget)
		rows, err = w.lake.Nominate(lctx, BotNominateLakeDays())
		lcancel()
		if err != nil && analytics.IsDisabledErr(err) {
			res.Status = "lake-disabled"
			log.Printf("[BotNominate] query source=lake window=%s skipped — lake reader disabled", window)
			return res
		}
	}
	qDur := w.now().Sub(qStart).Round(time.Millisecond)
	if err != nil {
		return fail("query", err)
	}
	res.Rows = len(rows)
	log.Printf("[BotNominate] query source=%s window=%s duration=%s rows_returned=%d", source, window, qDur, len(rows))

	// (3) candidates — the rule, re-applied
	for _, r := range rows {
		if botNominateRule(r.Subs, r.CoClick) {
			res.Nominees = append(res.Nominees, r)
		} else {
			res.Dropped++
			log.Printf("[BotNominate] dropped source=%s ip=%s subs=%d coclick=%d clicks=%d — fails rule (%s)",
				source, r.IP, r.Subs, r.CoClick, r.Clicks, botNominateRuleLabel)
		}
	}
	res.Matched = len(res.Nominees)
	log.Printf("[BotNominate] candidates source=%s window=%s matched=%d dropped=%d rule=%q",
		source, window, res.Matched, res.Dropped, botNominateRuleLabel)
	if res.Matched == 0 {
		res.Status = "ok"
		w.summary(ctx, res)
		return res
	}

	// (4)+(5) containment lookup, then one line per nominee
	ips := make([]string, 0, len(res.Nominees))
	for _, n := range res.Nominees {
		ips = append(ips, n.IP)
	}
	prior, err := w.currentClass(ctx, ips)
	if err != nil {
		return fail("containment", err)
	}
	var newNoms []BotNominee
	for _, n := range res.Nominees {
		cls := prior[n.IP]
		if cls == "" {
			cls = "none"
		}
		if cls == "scanner" {
			res.SkippedScanner++
			log.Printf("[BotNominate] nominee source=%s ip=%s subs=%d coclick=%d clicks=%d prior=scanner → skip (already contained)",
				source, n.IP, n.Subs, n.CoClick, n.Clicks)
			continue
		}
		log.Printf("[BotNominate] nominee source=%s ip=%s subs=%d coclick=%d clicks=%d prior=%s → write",
			source, n.IP, n.Subs, n.CoClick, n.Clicks, cls)
		newNoms = append(newNoms, n)
	}
	res.New = len(newNoms)
	log.Printf("[BotNominate] containment source=%s already_scanner=%d skipped, new=%d", source, res.SkippedScanner, res.New)

	// (6) upsert
	if res.New > 0 {
		uStart := w.now()
		written, invalid, err := w.apply(ctx, newNoms, res.Tag, srcLabel)
		res.Written, res.Invalid = written, invalid
		if err != nil {
			return fail("upsert", err)
		}
		log.Printf("[BotNominate] upsert source=%s rows_written=%d invalid_ip=%d tag=%s duration=%s revert=\"DELETE FROM ignite_ip_classification WHERE evidence_source='%s'\"",
			source, written, invalid, res.Tag, w.now().Sub(uStart).Round(time.Millisecond), res.Tag)
	}

	// (7) summary
	res.Status = "ok"
	w.summary(ctx, res)
	return res
}

// summary is the one-line record of the pass (there is no Go change ledger).
func (w *BotIPNominator) summary(ctx context.Context, res *BotNominateResult) {
	log.Printf("[BotNominate] SUMMARY source=%s window=%s matched=%d new=%d written=%d tag=%s next_gateway_reload<=15m",
		res.Source, res.Window, res.Matched, res.New, res.Written, res.Tag)
	EmitHeartbeat(ctx, w.db, botNominateWorkerName, int(w.interval.Seconds()), "ok",
		fmt.Sprintf("%s matched=%d written=%d", res.Source, res.Matched, res.Written))
}

// nominatePG runs the Postgres nomination query.
func (w *BotIPNominator) nominatePG(ctx context.Context, minutes int) ([]BotNominee, error) {
	qctx, cancel := context.WithTimeout(ctx, botNominatePGBudget)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, BuildBotNominatePGSQL(minutes))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BotNominee
	for rows.Next() {
		var n BotNominee
		if err := rows.Scan(&n.IP, &n.Subs, &n.CoClick, &n.Clicks); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// currentClass resolves each IP through ignite_ip_class(ip::inet) — containment,
// not exact match — and returns ip → class for every IP that has one.
func (w *BotIPNominator) currentClass(ctx context.Context, ips []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ips) == 0 {
		return out, nil
	}
	qctx, cancel := context.WithTimeout(ctx, botNominateLookupBudget)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, botNominateLookupSQL, pq.Array(ips))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ip string
		var cls sql.NullString
		if err := rows.Scan(&ip, &cls); err != nil {
			return nil, err
		}
		if cls.Valid && cls.String != "" {
			out[ip] = cls.String
		}
	}
	return out, rows.Err()
}

// apply upserts the nominees as class='scanner' in one transaction — the
// Python's apply(). Returns rows changed and the count of IPs that could not
// be rendered as a host cidr (logged, skipped, never written). Any SQL error
// rolls the whole batch back.
func (w *BotIPNominator) apply(ctx context.Context, noms []BotNominee, tag, source string) (written, invalid int, err error) {
	uctx, cancel := context.WithTimeout(ctx, botNominateUpsertBudget)
	defer cancel()
	tx, err := w.db.BeginTx(uctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(uctx, "SET LOCAL statement_timeout = '120s'"); err != nil {
		return 0, 0, fmt.Errorf("set statement_timeout: %w", err)
	}
	for _, n := range noms {
		cidr, cerr := botNominateHostCIDR(n.IP)
		if cerr != nil {
			invalid++
			log.Printf("[BotNominate] ERROR step=upsert ip=%q: %v — skipped", n.IP, cerr)
			continue
		}
		var r sql.Result
		r, err = tx.ExecContext(uctx, botNominateUpsertSQL, cidr, tag, botNominateEvidenceJSON(n, source))
		if err != nil {
			return 0, invalid, fmt.Errorf("upsert %s: %w", cidr, err)
		}
		if ra, aerr := r.RowsAffected(); aerr == nil {
			written += int(ra)
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, invalid, fmt.Errorf("commit: %w", err)
	}
	return written, invalid, nil
}
