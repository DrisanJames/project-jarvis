// Reader is the READ side of the Phase 1 S3 event lake. The WRITE side
// (lake_emitter.go) fans per-recipient events to Firehose ->
// s3://ignite-analytics-lake -> Glue table ignite_analytics.email_events
// (partitioned by source, dt). This file adds an Athena-backed query layer on
// top of that table.
//
// Design contract (mirrors the emitter):
//   - DISABLED BY DEFAULT. InitReader is a no-op unless an Athena results
//     output location is provided (wired from env ANALYTICS_ATHENA_OUTPUT in
//     main.go). When disabled, Summary/RecentEvents/Breakdown return a clear error and
//     ReaderEnabled() reports false — zero behaviour change for a server that
//     doesn't set the new env vars. Safe to ship dark.
//   - READ ONLY. This package never writes to the lake (that's the emitter)
//     and never touches Postgres. Athena is a separate store.
//   - SQL-injection safe. Every caller-supplied filter is strictly validated
//     (regex/UUID/clamp) before it is ever placed in a query, then rendered as a
//     single-quoted literal via sqlStr. No untrusted string reaches SQL.
package analytics

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	atypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
)

// Reader owns the Athena client + query configuration for the read layer.
type Reader struct {
	client    *athena.Client
	database  string
	workgroup string
	output    string // S3 results location, e.g. s3://bucket/athena-results/
	region    string
}

var (
	readerMu sync.RWMutex
	reader   *Reader
)

// Validation patterns. Anything caller-supplied must match exactly one of
// these (or be empty) before it is allowed near a query.
var (
	dtRe    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	tokenRe = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// dottedRe admits dot-containing identifiers (email domains like
	// "yahoo.co.uk", VMTA/pool names like "db-gmail-pool.1"). Still no quotes,
	// spaces, or SQL specials — safe for sqlStr interpolation.
	dottedRe = regexp.MustCompile(`^[a-zA-Z0-9_.\-]{1,255}$`)
)

const (
	defaultDatabase  = "ignite_analytics"
	defaultWorkgroup = "primary"
	lakeTable        = "email_events"
)

// SummaryRow is one aggregate bucket from Summary.
type SummaryRow struct {
	EventType string `json:"event_type"`
	Count     int64  `json:"count"`
}

// EventFilter is the (validated) filter set for RecentEvents. All fields are
// optional; empty fields are not applied as predicates.
type EventFilter struct {
	Dt         string // ^\d{4}-\d{2}-\d{2}$
	CampaignID string // UUID
	ISPGroup   string // ^[a-zA-Z0-9_\-]+$
	EventType  string // ^[a-zA-Z0-9_\-]+$
	Limit      int    // clamped to [1,1000]
}

// BreakdownRow is one GROUP BY bucket from Breakdown. Keys maps each requested
// dimension name to its value for the bucket (in no particular order — the
// caller knows the GroupBy order it asked for).
type BreakdownRow struct {
	Keys  map[string]string `json:"keys"`
	Count int64             `json:"count"`
}

// BreakdownFilter is the (validated) input for Breakdown. From/To are required
// dt bounds; GroupBy must name 1..3 whitelisted dimensions; Eq holds optional
// equality predicates (empty values are skipped, not applied).
type BreakdownFilter struct {
	From     string            // YYYY-MM-DD, required
	To       string            // YYYY-MM-DD, required
	GroupBy  []string          // 1..3 dims from breakdownDims
	Eq       map[string]string // optional equality predicates, column -> value
	SourceIn []string          // optional; values must be in srcAllow {pmta,ses,kumo}; emits source IN (...)
	// CampaignIDs scopes the breakdown to a campaign set: emits
	// campaign_id IN ('<uuid>', ...). Every value must be a UUID (uuidRe) —
	// copying the validated-IN-list precedent from reader_audience.go
	// (buildAudienceMemberEventsSQL). Capped at maxBreakdownCampaignIDs.
	CampaignIDs []string
	Limit       int // clamped to [1,5000], default 1000 if <=0
	// DedupDelayByEmail counts delivery_delay events by DISTINCT recipient
	// email instead of event_uid. Delay notifications are PER RETRY — measured
	// 2026-07-08: 276,053 delivery_delay events = 106,411 unique mailboxes in
	// one day (2.6x inflation) — so event_uid-counted "deferred" overstates
	// how much mail is actually held up and falsely trips throttle badges.
	// Other event types keep event_uid semantics; the two count domains are
	// prefix-tagged so they can never collide inside one DISTINCT.
	DedupDelayByEmail bool
	// DedupEngagementByRecipient counts open/click by DISTINCT RECIPIENT —
	// the standard UNIQUE open/click metric. Raw event counts inflate badly on
	// these two: measured 2026-08-05 on one click-drip node, 244 click events
	// came from 27 recipients (9x), which is how a funnel node rendered a 533%
	// click rate.
	//
	// The key is subscriber_id, NOT email: open/click ride source='app' where
	// email is NULL on ~87% of rows (measured 2026-08-05: 4.21M opens ->
	// 78,382 distinct emails vs 102,020 distinct subscriber_ids). Keying on
	// email collapses a node's uniques toward 1. Rows with no subscriber_id
	// fall back to event_uid so they are counted, never dropped.
	DedupEngagementByRecipient bool
}

// maxBreakdownCampaignIDs bounds the campaign IN-list so a pathological caller
// can't build an unbounded query (Athena query strings have a 256KB ceiling;
// 2000 quoted UUIDs is ~78KB — comfortable headroom).
const maxBreakdownCampaignIDs = 2000

// breakdownDims is the closed set of columns Breakdown may GROUP BY or filter
// on, each paired with the validation pattern its Eq values must satisfy.
// Anything outside this map is rejected by name before SQL construction —
// caller text never reaches a column position.
var breakdownDims = map[string]*regexp.Regexp{
	"dt":                 dtRe,
	"event_type":         tokenRe,
	"isp_group":          tokenRe,
	"isp":                tokenRe,  // CLEAN ISP — computed from the real recipient domain (see ispExpr)
	"brand":              dottedRe, // brands are apex domains ("discountblog.com")
	"email_domain":       dottedRe, // recipient domains contain dots
	"route_type":         tokenRe,
	"source":             tokenRe,
	"bounce_cat":         tokenRe,
	"vmta":               dottedRe, // VMTA names may contain dots
	"pool":               dottedRe, // pool names may contain dots
	"suppression_reason": tokenRe,
	"dsn_code":           tokenRe,
	"variant":            tokenRe,
	"campaign_id":        uuidRe,
	// v2.2 (2026-06-12): operating-day buckets + human/machine click split.
	"local_dt":         dtRe,        // America/Denver calendar day (computed)
	"local_hour":       localHourRe, // America/Denver calendar hour bucket "YYYY-MM-DD HH:00" (computed)
	"is_machine_click": boolRe,      // boolean column — rendered unquoted in SQL
	// v2.3 (2026-07-07, Offer Alignment): diagnostic-family classification.
	// dsn_family is COMPUTED from dsn_diag (see dsnFamilyExpr); values are
	// bracket codes (HM08) or dotted DSN codes (4.7.650) — dottedRe.
	// dsn_diag is the raw free-text diagnostic — groupable so evidence views
	// can surface a sample diagnostic per family; Eq values are restricted by
	// dsnDiagRe (no quotes/backslashes/newlines) before sqlStr interpolation.
	"dsn_family": dottedRe,
	"dsn_diag":   dsnDiagRe,
}

// dsnDiagRe admits free-text diagnostic strings as Eq values (spaces, brackets,
// punctuation) while excluding quotes, backslashes and newlines — safe for
// sqlStr interpolation (which additionally doubles any quote, defense in depth).
var dsnDiagRe = regexp.MustCompile(`^[^'\\\n\r]{1,300}$`)

var boolRe = regexp.MustCompile(`^(true|false)$`)

// localHourRe validates a local_hour value used as an Eq filter and documents
// the bucket shape "YYYY-MM-DD HH:00" (Denver calendar hour).
var localHourRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:00$`)

// srcAllow is the fixed allowlist for the SourceIn ("Combined" transport)
// filter. Only real transports may appear in the source IN (...) clause;
// anything else is rejected before SQL construction. "kumo" added 2026-07-20
// (kumo-visibility): the KumoMTA transport has emitted source=kumo lake rows
// since 2026-06-17; rows are Athena-visible once the Glue partition-projection
// enum (projection.source.values) includes kumo.
var srcAllow = map[string]bool{"pmta": true, "ses": true, "kumo": true}

// localDtExpr converts event_epoch_ms to the America/Denver calendar day.
// The operating day is Denver (CLAUDE.md §6); dt partitions stay UTC.
const localDtExpr = "date_format(from_unixtime(event_epoch_ms/1000) AT TIME ZONE 'America/Denver', '%Y-%m-%d')"

// localHourExpr converts event_epoch_ms to the America/Denver calendar HOUR
// bucket ("YYYY-MM-DD HH:00"). Like localDtExpr it is Denver-derived, so it
// triggers the same ±1-day dt-partition widening (an hour bucket can straddle a
// UTC day boundary).
const localHourExpr = "date_format(from_unixtime(event_epoch_ms/1000) AT TIME ZONE 'America/Denver', '%Y-%m-%d %H:00')"

// ispExpr is the CLEAN ISP classification — computed from the REAL recipient domain.
// Primary source is the `email` field (set on every pmta/ses send row); when it is
// BLANK we fall back to `email_domain`. This matters for engagement: source='app'
// open/click rows (the open-pixel / clicker-tracker stream) carry NO `email` but DO
// carry a clean `email_domain` (verified 2026-06-24: hotmail/icloud/gmail/… — no
// *.queue pollution), so without the fallback every app open collapsed into 'other'
// (151k opens, 2790% open rate on the ISP matrix). The fallback only fires when email
// is empty, so pmta/ses delivery rows (email set) NEVER touch the polluted email_domain
// path — the *.queue mis-bucketing the stored fields suffer (aol.queue/att.queue/…) is
// avoided exactly because email is present on those rows. Mirrors gen_cap_planner.ISP_CASE.
const ispDomainExpr = "lower(COALESCE(NULLIF(split_part(email, '@', 2), ''), email_domain))"
const ispExpr = "CASE" +
	" WHEN " + ispDomainExpr + " IN ('outlook.com','hotmail.com','live.com','msn.com','hotmail.co.uk','windowslive.com','passport.com','outlook.co.uk') THEN 'microsoft'" +
	" WHEN " + ispDomainExpr + " IN ('gmail.com','googlemail.com') THEN 'gmail'" +
	" WHEN " + ispDomainExpr + " IN ('yahoo.com','ymail.com','rocketmail.com','yahoo.co.uk','yahoo.ca') THEN 'yahoo'" +
	" WHEN " + ispDomainExpr + " IN ('icloud.com','me.com','mac.com') THEN 'apple'" +
	" WHEN " + ispDomainExpr + " = 'comcast.net' THEN 'comcast'" +
	" WHEN " + ispDomainExpr + " = 'aol.com' THEN 'aol'" +
	" WHEN " + ispDomainExpr + " = 'att.net' THEN 'att'" +
	" WHEN " + ispDomainExpr + " = 'sbcglobal.net' THEN 'sbcglobal'" +
	" WHEN " + ispDomainExpr + " = 'cox.net' THEN 'cox'" +
	" WHEN " + ispDomainExpr + " IN ('charter.net','spectrum.net') THEN 'charter'" +
	" WHEN " + ispDomainExpr + " = 'verizon.net' THEN 'verizon'" +
	" ELSE 'other' END"

// brandExpr is the CLEAN brand classification (apex domain). The stored `brand`
// column is authoritative when present — but only the PG→lake backfill (source
// 'app') has historically set it; the live pmta/ses emitters wrote brand=”
// (verified 2026-07-01: 100% of pmta+ses rows blank), which collapsed every
// delivery metric on the Brand dimension into one "(empty)" row. For blank
// rows we fall back to the brand code embedded in the VMTA name — every pmta
// row carries one ("mta-<code>-<pool>N" / "vmta-<code>-ses"; zero blank-vmta
// pmta rows in the lake). The code→apex map mirrors the VMTA seeds in
// cmd/server/main.go (seed rows "mta-bw-gn1.mail.em.businessweeklypro.com" …).
// ses-source rows have no vmta, so their history stays ” — write-time
// enrichment (Brand set at Emit) covers them from 2026-07 forward. Values are
// apex domains, matching brand.OwnedDomains and the stored app-row values.
const brandCodeExpr = "CASE split_part(vmta, '-', 2)" +
	" WHEN 'db' THEN 'discountblog.com'" +
	" WHEN 'qf' THEN 'quizfiesta.com'" +
	" WHEN 'ht' THEN 'historythinking.com'" +
	" WHEN 'mh' THEN 'myownhealth.net'" +
	" WHEN 'bw' THEN 'businessweeklypro.com'" +
	" WHEN 'fc' THEN 'financialcalculate.com'" +
	" WHEN 'cp' THEN 'consumerpro.net'" +
	" WHEN 'hw' THEN 'homewarrantyservices.org'" +
	" WHEN 'rr' THEN 'refinanceratesusa.com'" +
	" WHEN 'tt' THEN 'thingoftheday.org'" +
	" WHEN 'yi' THEN 'yourinsurancehub.com'" +
	" WHEN 'mr' THEN 'myrepairdiy.com'" +
	" WHEN 'ci' THEN 'casainsure.com'" +
	" WHEN 'lp' THEN 'learnpersonalloans.com'" +
	" WHEN 'rb' THEN 'ratesbazar.com'" +
	" WHEN 'wf' THEN 'warrantyforyou.com'" +
	" ELSE '' END"
const brandExpr = "COALESCE(NULLIF(brand, ''), " + brandCodeExpr + ")"

// eventTypeExpr re-derives bounce classification from the raw `bounce_cat` at
// READ time so hard / reputation / soft is consistent with the canonical
// taxonomy (api.HardBounceCategories / api.ReputationBlockCategories) regardless
// of what the historical ingest-time ClassifyBounce stamped. ONLY hard is true
// list hygiene (invalid/dead/disabled mailbox). Reputation/policy/connection
// blocks (Apple HM08, per-IP 5.7.1 rejections, etc.) become 'reputation_block'
// — a DISTINCT event_type the dashboards count in NEITHER hard nor soft (the
// operator's flush forces the deferred-blocked queue to bounce, which used to
// inflate "hard" ~10x: 30,929 policy-related vs 2,678 true hard on 2026-06-24).
// Only bounce rows are touched; every other event_type passes through unchanged.
// Applied in the projection (SELECT/GROUP BY); event_type Eq filters still match
// the stored value, but no breakdown caller filters event_type on bounce rows.
// v2.4 (2026-07-08): 'bounced' added to the reclassified set — SES-sourced
// bounces arrive as raw event_type='bounced' + bounce_cat (the catalog's
// "two spellings" footgun); without it every SES bounce passed through as an
// unclassified 'bounced' bucket and campaign-scoped hard/soft read ZERO for
// SES-routed offers (ps8241, 2026-07-08: 3,751 invisible bounces per 2k
// campaigns). bounce_cat 'hard' maps via the existing list; plain 'soft'
// falls to the ELSE.
const eventTypeExpr = "CASE WHEN event_type IN ('bounced','hard_bounce','soft_bounce','reputation_block','administrative') THEN " +
	"(CASE" +
	// Administrative queue flush (operator `pmta flush`): PMTA stamps every flushed
	// message with diagnostic 'x-pmta;deleted by administrator' (bounce_cat='other',
	// 5.0.0). These are NOT recipient bounces — they're operator cancellations, and
	// they dominate the raw soft-bounce number (~99.9% on flush days). Re-class to a
	// distinct 'administrative' bucket the dashboards count in NEITHER soft, hard,
	// reputation, NOR attempted (the UI's countsFromTypeMap only sums the known
	// types, so 'administrative' is excluded everywhere). MUST be tested first so a
	// flush with bounce_cat='other' doesn't fall through to soft_bounce.
	" WHEN lower(dsn_diag) LIKE '%deleted by administrator%' THEN 'administrative'" +
	" WHEN bounce_cat IN ('hard','bad-mailbox','bad-domain','inactive-mailbox') THEN 'hard_bounce'" +
	" WHEN bounce_cat IN ('spam-related','policy-related','routing-errors','no-answer-from-host','bad-connection') THEN 'reputation_block'" +
	// SES pre-flight address-validation blocks (bounce_cat='validation', v2.6):
	// the send never reached the remote MX. Own bucket, counted in NEITHER hard
	// nor soft — kept distinct so EmailOversight-vs-AWS-validation quality is
	// queryable. MUST precede the soft_bounce catch-all.
	" WHEN bounce_cat = 'validation' THEN 'preflight_validation'" +
	" ELSE 'soft_bounce' END)" +
	" ELSE event_type END"

// dsnFamilyExpr is the COMPUTED diagnostic-family classification for bounce /
// deferral evidence (Offer Alignment, 2026-07-07). It buckets the raw dsn_diag
// free text into the operator-recognisable families:
//   - Apple/ISP bracket codes embedded in the diagnostic: [HM08] [HCM2] [CS01]
//     [CS02] [PH01] (Apple local-policy / per-IP reputation / content blocks).
//   - Microsoft: 4.7.650 (velocity throttle), S3140 (network block),
//     S3150 (throttle/mitigation).
//   - Gmail rate/auth deferrals: 4.7.0, 4.7.28.
//   - List quality: 5.1.1 (invalid mailbox), 5.2.2 (mailbox full).
//
// Rows with no matching pattern fall back to COALESCE(NULLIF(dsn_code,”),'other')
// so a classified DSN code still buckets sensibly. NULL dsn_diag makes every
// LIKE/regexp predicate non-true in Presto, so NULL rows reach the ELSE arm.
// Order matters only within the substring checks: 4.7.650 is tested before
// 4.7.0 / 4.7.28 (none is a substring of another, but the specific-first order
// keeps intent obvious).
const dsnFamilyExpr = "CASE" +
	" WHEN regexp_like(dsn_diag, '\\[HM08\\]') THEN 'HM08'" +
	" WHEN regexp_like(dsn_diag, '\\[HCM2\\]') THEN 'HCM2'" +
	" WHEN regexp_like(dsn_diag, '\\[CS01\\]') THEN 'CS01'" +
	" WHEN regexp_like(dsn_diag, '\\[CS02\\]') THEN 'CS02'" +
	" WHEN regexp_like(dsn_diag, '\\[PH01\\]') THEN 'PH01'" +
	" WHEN dsn_diag LIKE '%4.7.650%' THEN '4.7.650'" +
	" WHEN dsn_diag LIKE '%4.7.28%' THEN '4.7.28'" +
	" WHEN dsn_diag LIKE '%4.7.0%' THEN '4.7.0'" +
	" WHEN dsn_diag LIKE '%S3140%' THEN 'S3140'" +
	" WHEN dsn_diag LIKE '%S3150%' THEN 'S3150'" +
	" WHEN dsn_diag LIKE '%5.1.1%' THEN '5.1.1'" +
	" WHEN dsn_diag LIKE '%5.2.2%' THEN '5.2.2'" +
	" ELSE COALESCE(NULLIF(dsn_code,''),'other') END"

// dimSelect / dimGroup render a dimension for the SELECT and GROUP BY lists.
// Plain columns pass through; local_dt is computed (aliased in SELECT, raw
// expression in GROUP BY — Athena does not allow grouping by the alias when
// it shadows nothing).
func dimSelect(d string) string {
	switch d {
	case "local_dt":
		return localDtExpr + " AS local_dt"
	case "local_hour":
		return localHourExpr + " AS local_hour"
	case "isp":
		return ispExpr + " AS isp"
	case "brand":
		return brandExpr + " AS brand"
	case "event_type":
		return eventTypeExpr + " AS event_type"
	case "dsn_family":
		return dsnFamilyExpr + " AS dsn_family"
	}
	return d
}

func dimGroup(d string) string {
	switch d {
	case "local_dt":
		return localDtExpr
	case "local_hour":
		return localHourExpr
	case "isp":
		return ispExpr
	case "brand":
		return brandExpr
	case "event_type":
		return eventTypeExpr
	case "dsn_family":
		return dsnFamilyExpr
	}
	return d
}

// shiftDt moves a YYYY-MM-DD string by days; on parse failure returns the
// input unchanged (validation upstream makes that unreachable in practice).
func shiftDt(s string, days int) string {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s
	}
	return t.AddDate(0, 0, days).Format("2006-01-02")
}

// InitReader wires the global reader. output == "" leaves the reader DISABLED
// (Summary/RecentEvents error, ReaderEnabled() == false). database/workgroup
// fall back to their defaults when empty. region falls back to the default
// chain when empty. Safe to call once at server start.
func InitReader(ctx context.Context, database, workgroup, output, region string) error {
	if output == "" {
		log.Printf("[analytics-lake-read] disabled (ANALYTICS_ATHENA_OUTPUT unset)")
		return nil
	}
	if database == "" {
		database = defaultDatabase
	}
	if workgroup == "" {
		workgroup = defaultWorkgroup
	}
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return err
	}
	r := &Reader{
		client:    athena.NewFromConfig(cfg),
		database:  database,
		workgroup: workgroup,
		output:    output,
		region:    region,
	}
	readerMu.Lock()
	reader = r
	readerMu.Unlock()
	log.Printf("[analytics-lake-read] enabled database=%s workgroup=%s output=%s region=%s", database, workgroup, output, region)
	return nil
}

// ReaderEnabled reports whether the read layer is configured.
func ReaderEnabled() bool {
	readerMu.RLock()
	defer readerMu.RUnlock()
	return reader != nil
}

func getReader() *Reader {
	readerMu.RLock()
	defer readerMu.RUnlock()
	return reader
}

// errDisabled is returned by the public query functions when the reader is not
// configured.
var errDisabled = fmt.Errorf("lake read disabled")

// validateDt returns an error if dt is non-empty and not a YYYY-MM-DD date.
func validateDt(name, dt string) error {
	if dt == "" {
		return nil
	}
	if !dtRe.MatchString(dt) {
		return fmt.Errorf("invalid %s: must be YYYY-MM-DD", name)
	}
	return nil
}

// validateToken returns an error if v is non-empty and not a safe token.
func validateToken(name, v string) error {
	if v == "" {
		return nil
	}
	if !tokenRe.MatchString(v) {
		return fmt.Errorf("invalid %s: must match ^[a-zA-Z0-9_-]+$", name)
	}
	return nil
}

// validateUUID returns an error if v is non-empty and not a UUID.
func validateUUID(name, v string) error {
	if v == "" {
		return nil
	}
	if !uuidRe.MatchString(v) {
		return fmt.Errorf("invalid %s: must be a UUID", name)
	}
	return nil
}

func clampLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > 1000 {
		return 1000
	}
	return n
}

// sqlStr renders an already-validated value as a single-quoted Athena string
// literal. Every caller validates its input with the regexes above (no quotes
// or specials can pass), so this is injection-safe; the ” replacement is
// defense-in-depth. We interpolate literals rather than use Athena
// ExecutionParameters because Athena evaluates a date-shaped parameter like
// 2026-06-03 as integer arithmetic, which breaks BETWEEN against the string
// `dt` partition column (TYPE_MISMATCH: varchar BETWEEN integer and integer).
func sqlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Summary aggregates event counts by event_type over [fromDt, toDt]
// (inclusive). Both bounds are required and must be YYYY-MM-DD.
//
// SQL (validated dates interpolated as quoted literals; see sqlStr):
//
//	SELECT event_type, COUNT(*) c FROM email_events
//	WHERE dt BETWEEN '<from>' AND '<to>'
//	GROUP BY event_type ORDER BY c DESC
func (r *Reader) Summary(ctx context.Context, fromDt, toDt string) ([]SummaryRow, error) {
	if err := validateDt("from", fromDt); err != nil {
		return nil, err
	}
	if err := validateDt("to", toDt); err != nil {
		return nil, err
	}
	if fromDt == "" || toDt == "" {
		return nil, fmt.Errorf("from and to dates are required")
	}
	// Mirror Breakdown's counting semantics exactly: COUNT(DISTINCT event_uid)
	// (the PMTA bridge redelivers events; a plain COUNT(*) inflates by the
	// redelivery factor) and the read-time bounce reclassification
	// (eventTypeExpr) — otherwise Summary's hard/soft buckets include the
	// policy-block and admin-flush inflation Breakdown strips out, and the two
	// endpoints disagree over the same range.
	sql := "SELECT " + eventTypeExpr + " AS event_type, COUNT(DISTINCT event_uid) c FROM " + lakeTable +
		" WHERE dt BETWEEN " + sqlStr(fromDt) + " AND " + sqlStr(toDt) +
		" GROUP BY " + eventTypeExpr + " ORDER BY c DESC"
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]SummaryRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		c, _ := strconv.ParseInt(row[1], 10, 64)
		out = append(out, SummaryRow{EventType: row[0], Count: c})
	}
	return out, nil
}

// RecentEvents returns the most-recent events matching the (validated) filter,
// newest first. The SELECT column order matches scanEvent below.
func (r *Reader) RecentEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	if err := validateDt("dt", f.Dt); err != nil {
		return nil, err
	}
	if err := validateUUID("campaign_id", f.CampaignID); err != nil {
		return nil, err
	}
	if err := validateToken("isp_group", f.ISPGroup); err != nil {
		return nil, err
	}
	if err := validateToken("event_type", f.EventType); err != nil {
		return nil, err
	}
	limit := clampLimit(f.Limit)

	// Build the WHERE clause from validated filters only. Every value was
	// regex-validated above (no quotes/specials can pass), so it is rendered as a
	// single-quoted literal via sqlStr rather than an Athena ExecutionParameter
	// (which would mis-evaluate date-shaped values as integer arithmetic).
	where := ""
	addPred := func(col, val string) {
		if val == "" {
			return
		}
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += col + " = " + sqlStr(val)
	}
	addPred("dt", f.Dt)
	addPred("campaign_id", f.CampaignID)
	addPred("isp_group", f.ISPGroup)
	addPred("event_type", f.EventType)

	// limit is an integer we fully control (clamped to [1,1000]); Athena does
	// not accept a bound parameter in the LIMIT position, so it is rendered
	// from the validated int via strconv — never from caller text.
	sql := "SELECT event_uid, recipient_send_id, campaign_id, subscriber_id, email, " +
		"email_domain, brand, isp_group, route_type, event_type, suppression_reason, " +
		"vmta, pool, bounce_cat, dsn_code, dsn_diag, link_url, source_ip, variant, " +
		"event_at, event_epoch_ms, ingested_at, source, dt FROM " + lakeTable +
		where + " ORDER BY event_epoch_ms DESC LIMIT " + strconv.Itoa(limit)

	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, scanEvent(row))
	}
	return out, nil
}

// scanEvent maps a result row (string cells) back into an Event. Column order
// MUST match the SELECT list in RecentEvents.
func scanEvent(row []string) Event {
	get := func(i int) string {
		if i < len(row) {
			return row[i]
		}
		return ""
	}
	epoch, _ := strconv.ParseInt(get(20), 10, 64)
	return Event{
		EventUID:          get(0),
		RecipientSendID:   get(1),
		CampaignID:        get(2),
		SubscriberID:      get(3),
		Email:             get(4),
		EmailDomain:       get(5),
		Brand:             get(6),
		ISPGroup:          get(7),
		RouteType:         get(8),
		EventType:         get(9),
		SuppressionReason: get(10),
		VMTA:              get(11),
		Pool:              get(12),
		BounceCat:         get(13),
		DSNCode:           get(14),
		DSNDiag:           get(15),
		LinkURL:           get(16),
		SourceIP:          get(17),
		Variant:           get(18),
		EventAt:           get(19),
		EventEpochMs:      epoch,
		IngestedAt:        get(21),
		Source:            get(22),
		Dt:                get(23),
	}
}

// clampBreakdownLimit clamps a Breakdown LIMIT to [1,5000], defaulting to 1000
// when the caller passes <=0. Exported (via ClampBreakdownLimit) so the HTTP
// handler can compute the same effective limit for its "truncated" flag.
func clampBreakdownLimit(n int) int {
	if n <= 0 {
		return 1000
	}
	if n > 5000 {
		return 5000
	}
	return n
}

// ClampBreakdownLimit is the exported form of clampBreakdownLimit so handlers
// can mirror the effective LIMIT without duplicating the clamp rules.
func ClampBreakdownLimit(n int) int { return clampBreakdownLimit(n) }

// validateBreakdownFilter checks every caller-supplied piece of a
// BreakdownFilter against the whitelist/regexes and returns the deduplicated
// GroupBy list (original order preserved). Nothing reaches SQL construction
// until this passes.
func validateBreakdownFilter(f BreakdownFilter) ([]string, error) {
	if f.From == "" || f.To == "" {
		return nil, fmt.Errorf("from and to dates are required")
	}
	if err := validateDt("from", f.From); err != nil {
		return nil, err
	}
	if err := validateDt("to", f.To); err != nil {
		return nil, err
	}
	// Both dates are YYYY-MM-DD, so lexical order == chronological order. An
	// inverted range would make BETWEEN silently return zero rows, which reads
	// as "no events in this range" — reject it explicitly instead.
	if f.From > f.To {
		return nil, fmt.Errorf("invalid range: from %s is after to %s", f.From, f.To)
	}

	// GroupBy: whitelist every dim, dedupe preserving order, require 1..3.
	seen := make(map[string]bool, len(f.GroupBy))
	dims := make([]string, 0, len(f.GroupBy))
	for _, d := range f.GroupBy {
		if _, ok := breakdownDims[d]; !ok {
			return nil, fmt.Errorf("invalid group_by dimension %q", d)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		dims = append(dims, d)
	}
	if len(dims) == 0 {
		return nil, fmt.Errorf("group_by requires at least one dimension")
	}
	// Up to 4 dims: the route-funnel companion query groups by
	// local_dt × source × route_type × event_type (all low-cardinality, so the
	// result stays far under the 5000-row LIMIT). 4 is the ceiling.
	if len(dims) > 4 {
		return nil, fmt.Errorf("group_by allows at most 4 dimensions, got %d", len(dims))
	}

	// Eq: keys must be whitelisted dims; values must match that dim's pattern.
	// Empty values are skipped here too (they are not predicates) so a bad key
	// with an empty value still surfaces as an error while an empty value on a
	// valid key is silently ignored — matching the EventFilter convention.
	for col, val := range f.Eq {
		re, ok := breakdownDims[col]
		if !ok {
			return nil, fmt.Errorf("invalid filter column %q", col)
		}
		if val == "" {
			continue
		}
		if !re.MatchString(val) {
			return nil, fmt.Errorf("invalid value for %s", col)
		}
	}

	// SourceIn: the "Combined" transport filter. Every value must be in the
	// fixed allowlist {pmta,ses,kumo} — no caller text reaches SQL otherwise.
	for _, s := range f.SourceIn {
		if !srcAllow[s] {
			return nil, fmt.Errorf("invalid source_in value %q", s)
		}
	}

	// CampaignIDs: every id must be a UUID before it can reach SQL (mirrors
	// buildAudienceMemberEventsSQL in reader_audience.go — but those ids are
	// lake data and silently dropped; these are caller input, so a malformed
	// id is an explicit error). The list is capped defensively.
	if len(f.CampaignIDs) > maxBreakdownCampaignIDs {
		return nil, fmt.Errorf("campaign_ids allows at most %d ids, got %d", maxBreakdownCampaignIDs, len(f.CampaignIDs))
	}
	for _, id := range f.CampaignIDs {
		if !uuidRe.MatchString(id) {
			return nil, fmt.Errorf("invalid campaign_ids value %q: must be a UUID", id)
		}
	}
	return dims, nil
}

// buildBreakdownSQL validates f and renders the full Athena query. Factored
// out of Breakdown so the SQL shape is unit-testable without an Athena client.
//
// SQL shape (validated literals only, via sqlStr):
//
//	SELECT <dims...>, COUNT(DISTINCT event_uid) c FROM email_events
//	WHERE dt BETWEEN '<from>' AND '<to>' [AND <col> = '<val>'...]
//	GROUP BY <dims> ORDER BY c DESC LIMIT <n>
//
// COUNT(DISTINCT event_uid) is deliberate: the PMTA HTTP bridge can redeliver
// the same event, and the lake collapses those duplicates by event_uid (see
// lake_emitter.go) — a plain COUNT(*) would inflate counts by the redelivery
// factor. Eq predicates are rendered in sorted column order so the output is
// deterministic. LIMIT is rendered from the clamped int via strconv (Athena
// cannot bind a parameter in the LIMIT position), same as RecentEvents.
func buildBreakdownSQL(f BreakdownFilter) (string, error) {
	dims, err := validateBreakdownFilter(f)
	if err != nil {
		return "", err
	}

	// local_dt / local_hour semantics: From/To are AMERICA/DENVER days. Both are
	// Denver-derived buckets, so widen the UTC dt partition bound by ±1 day (a
	// Denver day — and any hour bucket inside it — spans two UTC partitions) and
	// add the precise local-day predicate. local_hour must trigger this too:
	// without it, hour buckets at the UTC day boundary would be dropped.
	usesLocal := f.Eq["local_dt"] != "" || f.Eq["local_hour"] != ""
	for _, d := range dims {
		if d == "local_dt" || d == "local_hour" {
			usesLocal = true
		}
	}
	dtFrom, dtTo := f.From, f.To
	if usesLocal {
		dtFrom, dtTo = shiftDt(f.From, -1), shiftDt(f.To, 1)
	}

	selects := make([]string, len(dims))
	groups := make([]string, len(dims))
	for i, d := range dims {
		selects[i] = dimSelect(d)
		groups[i] = dimGroup(d)
	}

	countExpr := "COUNT(DISTINCT event_uid)"
	switch {
	case f.DedupDelayByEmail && f.DedupEngagementByRecipient:
		countExpr = "COUNT(DISTINCT CASE " +
			"WHEN event_type = 'delivery_delay' THEN 'e:' || email " +
			"WHEN event_type IN ('open','click') AND subscriber_id IS NOT NULL " +
			"THEN 's:' || event_type || ':' || subscriber_id " +
			"ELSE 'u:' || event_uid END)"
	case f.DedupDelayByEmail:
		countExpr = "COUNT(DISTINCT IF(event_type = 'delivery_delay', 'e:' || email, 'u:' || event_uid))"
	case f.DedupEngagementByRecipient:
		countExpr = "COUNT(DISTINCT CASE " +
			"WHEN event_type IN ('open','click') AND subscriber_id IS NOT NULL " +
			"THEN 's:' || event_type || ':' || subscriber_id " +
			"ELSE 'u:' || event_uid END)"
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(strings.Join(selects, ", "))
	b.WriteString(", ")
	b.WriteString(countExpr)
	b.WriteString(" c FROM ")
	b.WriteString(lakeTable)
	b.WriteString(" WHERE dt BETWEEN ")
	b.WriteString(sqlStr(dtFrom))
	b.WriteString(" AND ")
	b.WriteString(sqlStr(dtTo))
	if usesLocal {
		b.WriteString(" AND ")
		b.WriteString(localDtExpr)
		b.WriteString(" BETWEEN ")
		b.WriteString(sqlStr(f.From))
		b.WriteString(" AND ")
		b.WriteString(sqlStr(f.To))
	}

	// SourceIn ("Combined" transport): emit "AND source IN (v1, v2)". Values are
	// allowlist-validated above; dedupe + sort for stable SQL, render each
	// through sqlStr. Placed before the Eq loop so existing Eq ordering and the
	// tests that assert it are unchanged.
	if len(f.SourceIn) > 0 {
		seenSrc := make(map[string]bool, len(f.SourceIn))
		vals := make([]string, 0, len(f.SourceIn))
		for _, s := range f.SourceIn {
			if seenSrc[s] {
				continue
			}
			seenSrc[s] = true
			vals = append(vals, s)
		}
		sort.Strings(vals)
		quoted := make([]string, len(vals))
		for i, v := range vals {
			quoted[i] = sqlStr(v)
		}
		b.WriteString(" AND source IN (")
		b.WriteString(strings.Join(quoted, ", "))
		b.WriteString(")")
	}

	// CampaignIDs (Offer Alignment campaign-set scoping): every id was
	// uuidRe-validated above; dedupe preserving order, render each through
	// sqlStr. Placed after SourceIn / before the Eq loop so existing Eq
	// ordering (and the tests that assert it) are unchanged.
	if len(f.CampaignIDs) > 0 {
		seenID := make(map[string]bool, len(f.CampaignIDs))
		quotedIDs := make([]string, 0, len(f.CampaignIDs))
		for _, id := range f.CampaignIDs {
			if seenID[id] {
				continue
			}
			seenID[id] = true
			quotedIDs = append(quotedIDs, sqlStr(id))
		}
		b.WriteString(" AND campaign_id IN (")
		b.WriteString(strings.Join(quotedIDs, ", "))
		b.WriteString(")")
	}

	// Equality predicates: validated above, rendered as quoted literals in
	// sorted column order (map iteration is random; tests assert exact SQL).
	// is_machine_click is a boolean column (unquoted); local_dt is the
	// computed Denver-day expression.
	cols := make([]string, 0, len(f.Eq))
	for col, val := range f.Eq {
		if val == "" {
			continue
		}
		cols = append(cols, col)
	}
	sort.Strings(cols)
	for _, col := range cols {
		b.WriteString(" AND ")
		switch col {
		case "is_machine_click":
			b.WriteString(col)
			b.WriteString(" = ")
			b.WriteString(f.Eq[col]) // validated true|false — unquoted boolean
		case "local_dt":
			b.WriteString(localDtExpr)
			b.WriteString(" = ")
			b.WriteString(sqlStr(f.Eq[col]))
		case "local_hour":
			b.WriteString(localHourExpr)
			b.WriteString(" = ")
			b.WriteString(sqlStr(f.Eq[col]))
		case "isp":
			b.WriteString(ispExpr)
			b.WriteString(" = ")
			b.WriteString(sqlStr(f.Eq[col]))
		case "brand":
			b.WriteString(brandExpr)
			b.WriteString(" = ")
			b.WriteString(sqlStr(f.Eq[col]))
		case "dsn_family":
			b.WriteString(dsnFamilyExpr)
			b.WriteString(" = ")
			b.WriteString(sqlStr(f.Eq[col]))
		default:
			b.WriteString(col)
			b.WriteString(" = ")
			b.WriteString(sqlStr(f.Eq[col]))
		}
	}

	b.WriteString(" GROUP BY ")
	b.WriteString(strings.Join(groups, ", "))
	b.WriteString(" ORDER BY c DESC LIMIT ")
	b.WriteString(strconv.Itoa(clampBreakdownLimit(f.Limit)))
	return b.String(), nil
}

// Breakdown runs a generic GROUP BY aggregation over [From,To] and returns one
// row per dimension-value combination, highest count first. The first
// len(GroupBy) result cells are the key values (in GroupBy order, after
// dedupe); the last cell is the COUNT(DISTINCT event_uid).
func (r *Reader) Breakdown(ctx context.Context, f BreakdownFilter) ([]BreakdownRow, error) {
	dims, err := validateBreakdownFilter(f)
	if err != nil {
		return nil, err
	}
	sql, err := buildBreakdownSQL(f)
	if err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]BreakdownRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < len(dims)+1 {
			continue
		}
		keys := make(map[string]string, len(dims))
		for i, d := range dims {
			keys[d] = row[i]
		}
		c, _ := strconv.ParseInt(row[len(dims)], 10, 64)
		out = append(out, BreakdownRow{Keys: keys, Count: c})
	}
	return out, nil
}

// runQuery executes sql against Athena and returns the column headers and the
// data rows (header row stripped). All values are interpolated as validated
// quoted literals by the callers (see sqlStr), not bound parameters. Bounded by ctx.
func (r *Reader) runQuery(ctx context.Context, sql string) (cols []string, rows [][]string, err error) {
	start := &athena.StartQueryExecutionInput{
		QueryString: aws.String(sql),
		QueryExecutionContext: &atypes.QueryExecutionContext{
			Database: aws.String(r.database),
		},
		WorkGroup: aws.String(r.workgroup),
		ResultConfiguration: &atypes.ResultConfiguration{
			OutputLocation: aws.String(r.output),
		},
	}
	startOut, err := r.client.StartQueryExecution(ctx, start)
	if err != nil {
		return nil, nil, err
	}
	qid := startOut.QueryExecutionId

	// Poll until the query reaches a terminal state, bounded by ctx.
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		exec, gerr := r.client.GetQueryExecution(ctx, &athena.GetQueryExecutionInput{
			QueryExecutionId: qid,
		})
		if gerr != nil {
			return nil, nil, gerr
		}
		state := exec.QueryExecution.Status.State
		switch state {
		case atypes.QueryExecutionStateSucceeded:
			goto results
		case atypes.QueryExecutionStateFailed, atypes.QueryExecutionStateCancelled:
			reason := ""
			if exec.QueryExecution.Status.StateChangeReason != nil {
				reason = *exec.QueryExecution.Status.StateChangeReason
			}
			return nil, nil, fmt.Errorf("athena query %s: %s", state, reason)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

results:
	firstPage := true
	var nextToken *string
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		in := &athena.GetQueryResultsInput{QueryExecutionId: qid}
		if nextToken != nil {
			in.NextToken = nextToken
		}
		res, rerr := r.client.GetQueryResults(ctx, in)
		if rerr != nil {
			return nil, nil, rerr
		}
		if res.ResultSet != nil {
			for i, rrow := range res.ResultSet.Rows {
				cells := make([]string, 0, len(rrow.Data))
				for _, d := range rrow.Data {
					if d.VarCharValue == nil {
						cells = append(cells, "")
					} else {
						cells = append(cells, *d.VarCharValue)
					}
				}
				// The very first row of the very first page is the column
				// header — capture it, then skip.
				if firstPage && i == 0 {
					cols = cells
					continue
				}
				rows = append(rows, cells)
			}
		}
		firstPage = false
		if res.NextToken == nil {
			break
		}
		nextToken = res.NextToken
	}
	return cols, rows, nil
}

// --- package-level convenience wrappers (used by HTTP handlers) ---

// Summary runs against the global reader. Returns errDisabled when the reader
// is not configured.
func Summary(ctx context.Context, fromDt, toDt string) ([]SummaryRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.Summary(ctx, fromDt, toDt)
}

// RecentEvents runs against the global reader. Returns errDisabled when the
// reader is not configured.
func RecentEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.RecentEvents(ctx, f)
}

// Breakdown runs against the global reader. Returns errDisabled when the
// reader is not configured.
func Breakdown(ctx context.Context, f BreakdownFilter) ([]BreakdownRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.Breakdown(ctx, f)
}

// IsDisabledErr reports whether err is the "lake read disabled" sentinel so
// handlers can return a graceful 200 instead of a 5xx.
func IsDisabledErr(err error) bool {
	return err == errDisabled
}
