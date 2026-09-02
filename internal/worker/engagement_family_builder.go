package worker

// EngagementFamilyBuilder — nightly IN-PLATFORM builder of two operator-ruled
// segment families (operator rulings 2026-07-26):
//
//  1. GMAIL-ENG-<CODE>-OPEN30D / GMAIL-ENG-<CODE>-CLK60D — per sending brand
//     (the 16 broadcast brands), gmail-ISP recipients only: subscribers with an
//     open in the last 30 days / a QUALIFYING navigational click in the last
//     60 days on that brand's sending domains. Replaces the never-armed
//     agents/jobs/gmail_openers_recalc.py (cron 12:37 died on a missing
//     IGNITE_ADMIN_KEY since birth); the operator ruled the segment logic
//     ships IN-PLATFORM: "it should be running along with the segment refresh
//     worker. Why is this a separate process?"
//  2. KUMO-ALLTIME-<CODE>-ENG — per kumo warmup brand (9 brands), ALL-TIME
//     openers ∪ action-clickers on that brand's sending domain, every ISP.
//     The kumo brands' 30D Clickers pools were 1-19 members — too thin to
//     anchor a daily warm send: "Since we're actively mailing these domains
//     ensure they have all time openers and clickers they are targeting daily
//     for each send."
//
// CLICK-COHORT DOCTRINE (operator 2026-07-19; agents/dbknowledge/_db.py
// PG_CLICK_ACTION): AN ASSET/RESOURCE FETCH IS NOT A CLICK — ~93% of raw
// event_type='clicked' rows are stylesheet/font/CDN fetches. The click inlet
// here mirrors the canonical PG_CLICK_NONNAV + PG_CLICK_ASSET exclusions
// (unsub/optout/preference/privacy, synthetic 'everflow-import:' markers,
// unresolved t.em tracker self-references, asset extensions, CDN hosts).
// NO verdict/heuristic gating (signal-grading + 2026-07-21 actor-blind pivot).
//
// REBUILD SEMANTICS — full nightly rebuild, DELETE + INSERT..SELECT inside ONE
// transaction per segment (the SegmentMaterializer's materializeSegmentCore
// idiom, NOT the VerifiedHumansLedger's accretive model): these are WINDOWED
// families, so aged-out members MUST be expelled — an add-only ledger would
// never shrink the 30D/60D windows, and a diff pass adds complexity for no
// gain at these sizes (per-brand gmail engagement is thousands of rows; the
// kumo pools are tiny). One tx per segment keeps the lock/WAL footprint small
// and leaves each segment either old-complete or new-complete on a mid-pass
// crash — never half-built. Re-run/double-fire safe: the rebuild is a pure
// function of the event window, and the whole pass is distlock-gated.
//
// The target segments are created if missing (org-scoped, deterministic
// naming), segment_type='static' + keep_active=TRUE — the two flags that keep
// them out of BOTH the nightly SegmentMaterializer (dynamic-only) AND
// SegmentCleanupWorker.purgeStaticSnapshots (same contract as the seeded
// "<brand> Verified Humans" segments; see cmd/server/main.go
// seed_verified_humans_segments). Registry: family patterns 'GMAIL-ENG-%' and
// 'KUMO-ALLTIME-%' are seeded in mailing_segment_registry with sla_hours=30
// and heartbeat_worker='engagement_family_builder'.
//
// PARTITIONS: mailing_tracking_events is month-partitioned. Every scan carries
// `event_at >= $from AND event_at < $to` on the parent table so the planner
// prunes to exactly the partitions the window touches (the sibling workers'
// idiom). "All-time" for kumo is bounded below by kumoAllTimeEpoch
// (2026-01-01 — the first partition month), which keeps pruning effective and
// IS all-time for this table.
//
// Schedule: daily 05:00 UTC (23:00 Denver — calm window, after the partner
// rollup's 03:10 and the verified-humans 03:40, before the ~06:35Z lake-
// standard build and the 6am reports). NO boot pass during send hours (the
// 2026-07-13 wave-scheduler starvation lesson); ENGAGEMENT_FAMILY_BOOT_PASS=true
// forces an operator-supervised boot catch-up. Kill switch:
// DISABLE_ENGAGEMENT_FAMILY_BUILDER=true.
//
// Single-writer: distlock (Redis preferred, PG advisory fallback). Heartbeat +
// run summary land in mailing_worker_heartbeats / mailing_worker_runs; each
// rebuilt segment stamps mailing_segment_build_ledger and refreshes the
// cached subscriber_count (the SegmentMaterializer's bookkeeping contract).

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	engagementFamilyWorkerName = "engagement_family_builder"
	engagementFamilyLockKey    = "engagement-family-builder"

	// Daily anchor: 05:00 UTC (see schedule note in the package comment).
	engagementFamilyAnchorHour = 5
	engagementFamilyAnchorMin  = 0

	// Window widths ruled by the operator 2026-07-26: "30D openers and 60D
	// clickers for gmail ... for each sending domain."
	gmailOpenWindowDays  = 30
	gmailClickWindowDays = 60

	// Pause between per-segment rebuilds so this analytics pass never starves
	// the send path on the shared PRIMARY (2026-07-13 lesson; the
	// verifiedHumansChunkPause idiom).
	engagementFamilySegmentPause = 3 * time.Second
)

// kumoAllTimeEpoch bounds the "all-time" kumo scan below at the first
// mailing_tracking_events partition month (2026-01; see
// verified_humans_ledger.go verifiedHumansBackfillDays note). A lower bound
// keeps partition pruning effective and is equivalent to all-time for this
// table.
var kumoAllTimeEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// engagementBrand pairs a canonical brand code (agents/registry/
// brand_metadata.py BRAND_REGISTRY key — same authority as
// internal/api/money_link_check.go brandCodeRoot) with its apex brand root
// (internal/pkg/brand.OwnedDomains entry). Kept in lock-step with both.
type engagementBrand struct {
	code string // canonical uppercase brand code (deterministic segment naming)
	apex string // brand root; scans match sending_domain = apex OR '%.'||apex
}

// gmailEngagementBrands — the 16 broadcast sending brands (CLAUDE.md §7).
var gmailEngagementBrands = []engagementBrand{
	{"DB", "discountblog.com"},
	{"HT", "historythinking.com"},
	{"MH", "myownhealth.net"},
	{"QF", "quizfiesta.com"},
	{"BW", "businessweeklypro.com"},
	{"FC", "financialcalculate.com"},
	{"CP", "consumerpro.net"},
	{"HW", "homewarrantyservices.org"},
	{"RR", "refinanceratesusa.com"},
	{"TT", "thingoftheday.org"},
	{"YI", "yourinsurancehub.com"},
	{"MR", "myrepairdiy.com"},
	{"CI", "casainsure.com"},
	{"LP", "learnpersonalloans.com"},
	{"RB", "ratesbazar.com"},
	{"WF", "warrantyforyou.com"},
}

// kumoAllTimeBrands — the 9 KumoMTA warmup brands (brand.OwnedDomains Jun-2026
// Kumo expansions; codes from brand_metadata.py).
var kumoAllTimeBrands = []engagementBrand{
	{"MP", "mypersonalfinancial.com"},
	{"PD", "paymydebit.com"},
	{"TR", "theretirementblog.com"},
	{"BCC", "bestcreditcare.com"},
	{"USF", "us-finance.com"},
	{"YFB", "yourfinancialblog.com"},
	{"HLJ", "homeloansbyjaime.com"},
	{"FTH", "firsttimebuyerhomeloan.com"},
	{"HTM", "hometracmortgage.com"},
}

// ── Canonical click predicate (mirrored from agents/dbknowledge/_db.py) ──────
//
// PG_CLICK_NONNAV: NON-qualifying clicks — empty link_url, unsub/preference/
// compliance links, synthetic 'everflow-import:' markers, and unresolved t.em
// tracker self-references (destination unproven).
const engagementClickNonNavSQL = `(COALESCE(te.link_url,'') = ''
	  OR te.link_url ~* 'unsub|optout|opt-out|preference|/privacy'
	  OR te.link_url ~* '^everflow-import:'
	  OR (te.link_url ~* '^https?://t\.em\.' AND te.link_url !~* '^https?://(t|trk)\.e?m\.[^/]+/o/'))`

// The scanner-safe money-link redirect (/o/<brand>/..., cmd/tracking
// HandleOfferRedirect) records the TRACKER host as link_url, so the t.em
// self-reference rule swallowed every money-link click on t.em-tracked brands
// (2026-07-22 → 2026-09-01; 138 clickers in 30d had no other qualifying
// click). A /o/ path is a proven navigation to the offer — it is an ACTION
// click. Mirrors agents/dbknowledge/_db.py PG_CLICK_NONNAV.
// PG_CLICK_ASSET: asset/resource fetches — file-extension or known asset/CDN
// host. ~93% of raw 'clicked' events (fonts.googleapis.com dominates).
const engagementClickAssetSQL = `(te.link_url ~* '\.(css|js|woff2?|ttf|otf|eot|png|jpe?g|gif|svg|ico|webp|map)([?#]|$)'
	  OR te.link_url ~* '(fonts\.g|cdn\.|cloudfront|akamai|fastly|jsdelivr|unpkg|gstatic)')`

// PG_CLICK_ACTION: the qualifying navigational/commercial click predicate.
const engagementActionClickSQL = `(te.event_type = 'clicked'
	  AND NOT ` + engagementClickNonNavSQL + `
	  AND NOT ` + engagementClickAssetSQL + `)`

// gmailRecipientSQL scopes to gmail-ISP subscribers (operator: "for gmail").
const gmailRecipientSQL = `split_part(lower(s.email), '@', 2) IN ('gmail.com', 'googlemail.com')`

// ── Member-rebuild statements ────────────────────────────────────────────────
// All three run INSIDE the per-segment rebuild transaction, immediately after
// `DELETE FROM mailing_segment_members WHERE segment_id = $1::uuid`.
// $1 = segment_id, $2 = window from (timestamptz), $3 = window to (exclusive),
// $4 = sending-domain apex. The te scan rides idx_tracking_events_segment_v2
// (event_type, event_at, sending_domain, subscriber_id); event_at bounds prune
// the monthly partitions.

// gmailOpenersInsertSQL — GMAIL-ENG-<CODE>-OPEN30D members.
const gmailOpenersInsertSQL = `
	INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at)
	SELECT DISTINCT $1::uuid, te.subscriber_id, s.email, NOW()
	FROM mailing_tracking_events te
	JOIN mailing_subscribers s ON s.id = te.subscriber_id
	WHERE te.event_type = 'opened'
	  AND te.subscriber_id IS NOT NULL
	  AND te.event_at >= $2 AND te.event_at < $3
	  AND (te.sending_domain = $4 OR te.sending_domain LIKE '%.' || $4)
	  AND ` + gmailRecipientSQL + `
	  AND s.email IS NOT NULL AND s.email <> ''
	ON CONFLICT (segment_id, subscriber_id) DO NOTHING`

// gmailClickersInsertSQL — GMAIL-ENG-<CODE>-CLK60D members (ACTION clicks
// only; a raw event_type='clicked' count is vendor telemetry, never a cohort).
const gmailClickersInsertSQL = `
	INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at)
	SELECT DISTINCT $1::uuid, te.subscriber_id, s.email, NOW()
	FROM mailing_tracking_events te
	JOIN mailing_subscribers s ON s.id = te.subscriber_id
	WHERE ` + engagementActionClickSQL + `
	  AND te.subscriber_id IS NOT NULL
	  AND te.event_at >= $2 AND te.event_at < $3
	  AND (te.sending_domain = $4 OR te.sending_domain LIKE '%.' || $4)
	  AND ` + gmailRecipientSQL + `
	  AND s.email IS NOT NULL AND s.email <> ''
	ON CONFLICT (segment_id, subscriber_id) DO NOTHING`

// kumoAllTimeInsertSQL — KUMO-ALLTIME-<CODE>-ENG members: all-time openers ∪
// action-clickers on the brand's sends, every recipient ISP.
const kumoAllTimeInsertSQL = `
	INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at)
	SELECT DISTINCT $1::uuid, q.subscriber_id, s.email, NOW()
	FROM (
		SELECT te.subscriber_id
		FROM mailing_tracking_events te
		WHERE te.event_type = 'opened'
		  AND te.subscriber_id IS NOT NULL
		  AND te.event_at >= $2 AND te.event_at < $3
		  AND (te.sending_domain = $4 OR te.sending_domain LIKE '%.' || $4)
		UNION
		SELECT te.subscriber_id
		FROM mailing_tracking_events te
		WHERE ` + engagementActionClickSQL + `
		  AND te.subscriber_id IS NOT NULL
		  AND te.event_at >= $2 AND te.event_at < $3
		  AND (te.sending_domain = $4 OR te.sending_domain LIKE '%.' || $4)
	) q
	JOIN mailing_subscribers s ON s.id = q.subscriber_id
	WHERE s.email IS NOT NULL AND s.email <> ''
	ON CONFLICT (segment_id, subscriber_id) DO NOTHING`

// engagementFamilyEnsureSegmentSQL creates a family segment if missing, per
// org (org-scoped, deterministic naming — the seed_verified_humans_segments
// contract: segment_type='static' + keep_active=TRUE keep it out of both the
// SegmentMaterializer and the cleanup purge; the planner streams members
// straight from mailing_segment_members, so 'static' works end-to-end).
// Idempotent via the NOT EXISTS guard on (organization_id, name).
// $1 = name, $2 = description, $3 = sending-domain apex, $4 = category.
const engagementFamilyEnsureSegmentSQL = `
	INSERT INTO mailing_segments (
		id, organization_id, name, description, segment_type, conditions,
		status, keep_active, category, subscriber_count, created_at, updated_at
	)
	SELECT gen_random_uuid(), o.organization_id, $1::text, $2::text, 'static',
		jsonb_build_array(jsonb_build_object('group', 0, 'field', 'sending_domain', 'operator', 'equals', 'value', $3::text)),
		'active', TRUE, $4::text, 0, NOW(), NOW()
	FROM (SELECT DISTINCT organization_id FROM mailing_segments) o
	WHERE NOT EXISTS (
		SELECT 1 FROM mailing_segments m
		WHERE m.organization_id = o.organization_id
		  AND m.name = $1::text
	)`

// engagementFamilyListSQL resolves the family segment ids to rebuild. Reads
// the rows back from the DB (never trusts in-memory ids) so an operator-
// created or multi-org segment is picked up automatically.
const engagementFamilyListSQL = `
	SELECT s.id::text, s.name
	FROM mailing_segments s
	WHERE s.name = ANY($1::text[])
	  AND s.segment_type = 'static'
	  AND s.status = 'active'
	ORDER BY s.name`

// engagementFamilyLedgerUpsertSQL stamps mailing_segment_build_ledger after a
// successful rebuild. Inlined (not api.UpsertSegmentLedger) because
// internal/api already imports internal/worker — a worker→api import would be
// a cycle; internal/api/segment_ledger.go remains the canonical
// implementation (same idiom as PartnerDripOrchestrator.upsertWaveSegmentLedger).
// $1 = segment_id, $2 = member count, $3 = build ms.
const engagementFamilyLedgerUpsertSQL = `
	INSERT INTO mailing_segment_build_ledger
		(segment_id, subscriber_count, last_built_at, build_source,
		 last_build_status, last_build_ms, last_delta_pct, last_error, updated_at)
	VALUES ($1::uuid, $2, NOW(), 'engagement-family', 'ok', $3, 0, NULL, NOW())
	ON CONFLICT (segment_id) DO UPDATE SET
		subscriber_count  = EXCLUDED.subscriber_count,
		last_built_at     = NOW(),
		build_source      = EXCLUDED.build_source,
		last_build_status = EXCLUDED.last_build_status,
		last_build_ms     = EXCLUDED.last_build_ms,
		last_delta_pct    = EXCLUDED.last_delta_pct,
		last_error        = NULL,
		updated_at        = NOW()`

// engagementFamilySpec is one deterministic (name → scan) build target.
type engagementFamilySpec struct {
	name        string
	description string
	apex        string
	category    string
	insertSQL   string
	windowDays  int // 0 = all-time (kumoAllTimeEpoch lower bound)
	allTime     bool
}

// buildEngagementFamilySpecs enumerates every segment this worker owns:
// 16 brands × {OPEN30D, CLK60D} + 9 kumo brands × {ENG} = 41 specs.
func buildEngagementFamilySpecs() []engagementFamilySpec {
	specs := make([]engagementFamilySpec, 0, len(gmailEngagementBrands)*2+len(kumoAllTimeBrands))
	for _, b := range gmailEngagementBrands {
		specs = append(specs, engagementFamilySpec{
			name: fmt.Sprintf("GMAIL-ENG-%s-OPEN30D", b.code),
			description: fmt.Sprintf(
				"Gmail-ISP subscribers with an open in the last %dd on %s sends. Rebuilt nightly 05:00 UTC by EngagementFamilyBuilder (operator ruling 2026-07-26 — in-platform gmail recalc, per sending domain).",
				gmailOpenWindowDays, b.apex),
			apex:       b.apex,
			category:   "engagement_isp",
			insertSQL:  gmailOpenersInsertSQL,
			windowDays: gmailOpenWindowDays,
		})
		specs = append(specs, engagementFamilySpec{
			name: fmt.Sprintf("GMAIL-ENG-%s-CLK60D", b.code),
			description: fmt.Sprintf(
				"Gmail-ISP subscribers with a qualifying navigational click in the last %dd on %s sends (canonical click-action predicate — asset fetches and unsub/compliance clicks excluded). Rebuilt nightly 05:00 UTC by EngagementFamilyBuilder (operator ruling 2026-07-26).",
				gmailClickWindowDays, b.apex),
			apex:       b.apex,
			category:   "engagement_isp",
			insertSQL:  gmailClickersInsertSQL,
			windowDays: gmailClickWindowDays,
		})
	}
	for _, b := range kumoAllTimeBrands {
		specs = append(specs, engagementFamilySpec{
			name: fmt.Sprintf("KUMO-ALLTIME-%s-ENG", b.code),
			description: fmt.Sprintf(
				"ALL-TIME openers plus action-clickers on %s sends (kumo warmup brand; every recipient ISP). Rebuilt nightly 05:00 UTC by EngagementFamilyBuilder (operator ruling 2026-07-26 — the 30D pools were 1-19 members, too thin for the daily warm).",
				b.apex),
			apex:      b.apex,
			category:  "engagement_brand",
			insertSQL: kumoAllTimeInsertSQL,
			allTime:   true,
		})
	}
	return specs
}

// engagementFamilyWindow computes the [from, to) scan bounds for a windowed
// family: `to` is today's 00:00 UTC day boundary (deterministic across the
// pass; keeps partition pruning on clean day edges), `from` is `days` before.
func engagementFamilyWindow(now time.Time, days int) (time.Time, time.Time) {
	to := now.UTC().Truncate(24 * time.Hour)
	return to.AddDate(0, 0, -days), to
}

// kumoAllTimeWindow computes the [epoch, today) bounds for an all-time family.
func kumoAllTimeWindow(now time.Time) (time.Time, time.Time) {
	to := now.UTC().Truncate(24 * time.Hour)
	return kumoAllTimeEpoch, to
}

// partitionMonthsTouched counts the monthly mailing_tracking_events partitions
// a [from, to) window touches (to is exclusive). Logged per pass so an
// unexpectedly wide scan is visible in ops output.
func partitionMonthsTouched(from, to time.Time) int {
	if !from.Before(to) {
		return 0
	}
	last := to.Add(-time.Nanosecond)
	return (last.Year()-from.Year())*12 + int(last.Month()) - int(from.Month()) + 1
}

// EngagementFamilyBuilder owns the nightly family rebuild pass.
type EngagementFamilyBuilder struct {
	db    *sql.DB
	redis *redis.Client
}

// NewEngagementFamilyBuilder constructs the worker. redisClient may be nil —
// the distlock falls back to a PG advisory lock (same contract as siblings).
func NewEngagementFamilyBuilder(db *sql.DB, redisClient *redis.Client) *EngagementFamilyBuilder {
	return &EngagementFamilyBuilder{db: db, redis: redisClient}
}

// Start runs a daily pass at 05:00 UTC (calm window). No send-hours boot pass
// unless ENGAGEMENT_FAMILY_BOOT_PASS=true. Runs until ctx is cancelled; call
// via `go w.Start(ctx)`.
func (w *EngagementFamilyBuilder) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[EngagementFamilyBuilder] disabled (db missing)")
		return
	}
	if os.Getenv("DISABLE_ENGAGEMENT_FAMILY_BUILDER") == "true" {
		log.Printf("[EngagementFamilyBuilder] disabled via DISABLE_ENGAGEMENT_FAMILY_BUILDER")
		return
	}
	log.Printf("[EngagementFamilyBuilder] started (%d gmail specs + %d kumo specs, daily %02d:%02d UTC)",
		len(gmailEngagementBrands)*2, len(kumoAllTimeBrands), engagementFamilyAnchorHour, engagementFamilyAnchorMin)

	if os.Getenv("ENGAGEMENT_FAMILY_BOOT_PASS") == "true" {
		log.Printf("[EngagementFamilyBuilder] ENGAGEMENT_FAMILY_BOOT_PASS=true — running a boot pass NOW (operator-forced; contends with the send path)")
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Minute):
		}
		w.RunOnce(ctx)
	} else {
		log.Printf("[EngagementFamilyBuilder] no boot pass (send-path safety) — first pass at the %02d:%02d UTC calm window",
			engagementFamilyAnchorHour, engagementFamilyAnchorMin)
	}

	for {
		timer := time.NewTimer(w.timeUntilNextTick(time.Now().UTC()))
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("[EngagementFamilyBuilder] stopping")
			return
		case <-timer.C:
			w.RunOnce(ctx)
		}
	}
}

// timeUntilNextTick computes the sleep until the next 05:00 UTC.
func (w *EngagementFamilyBuilder) timeUntilNextTick(now time.Time) time.Duration {
	target := time.Date(now.Year(), now.Month(), now.Day(),
		engagementFamilyAnchorHour, engagementFamilyAnchorMin, 0, 0, time.UTC)
	if !now.Before(target) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// RunOnce executes one full pass under the distributed lock. Exported so tests
// and operational tooling can trigger a pass directly.
func (w *EngagementFamilyBuilder) RunOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	lock := distlock.NewLock(w.redis, w.db, engagementFamilyLockKey, 45*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[EngagementFamilyBuilder] lock acquire error: %v", err)
		return
	}
	if !acquired {
		log.Printf("[EngagementFamilyBuilder] another instance holds the lock — skipping pass")
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[EngagementFamilyBuilder] lock release error: %v", err)
		}
	}()

	start := time.Now()
	built, failed := 0, 0
	hbStatus, hbErr := "ok", ""
	defer func() {
		EmitHeartbeat(ctx, w.db, engagementFamilyWorkerName, int((24 * time.Hour).Seconds()), hbStatus, hbErr)
		runStatus := "ok"
		if failed > 0 {
			runStatus = "partial"
		}
		if hbStatus == "error" {
			runStatus = "failed"
		}
		RecordWorkerRun(ctx, w.db, engagementFamilyWorkerName, start, runStatus,
			built, failed, "GMAIL-ENG-* + KUMO-ALLTIME-* nightly family rebuilds")
	}()

	specs := buildEngagementFamilySpecs()
	specByName := make(map[string]engagementFamilySpec, len(specs))
	names := make([]string, 0, len(specs))
	for _, sp := range specs {
		specByName[sp.name] = sp
		names = append(names, sp.name)
	}

	// Ensure every target segment exists (org-scoped, idempotent).
	for _, sp := range specs {
		if ctx.Err() != nil {
			return
		}
		if err := w.ensureSegment(ctx, sp); err != nil {
			hbStatus, hbErr = "error", err.Error()
			log.Printf("[EngagementFamilyBuilder] ensure segment %q: %v", sp.name, err)
			return
		}
	}

	// Resolve the ids to rebuild (reads the rows back — never trusts memory).
	targets, err := w.listTargets(ctx, names)
	if err != nil {
		hbStatus, hbErr = "error", err.Error()
		log.Printf("[EngagementFamilyBuilder] list targets: %v", err)
		return
	}

	now := time.Now()
	for _, tgt := range targets {
		if ctx.Err() != nil {
			return
		}
		sp, ok := specByName[tgt.name]
		if !ok {
			continue
		}
		var from, to time.Time
		if sp.allTime {
			from, to = kumoAllTimeWindow(now)
		} else {
			from, to = engagementFamilyWindow(now, sp.windowDays)
		}
		buildStart := time.Now()
		count, err := w.rebuildSegment(ctx, tgt.id, sp.insertSQL, from, to, sp.apex)
		if err != nil {
			// Per-segment isolation: one family's failure never stops the
			// pass; the failed segment keeps last night's members (the
			// rebuild tx rolled back whole).
			failed++
			log.Printf("[EngagementFamilyBuilder] rebuild %q (%s) failed: %v", tgt.name, shortID(tgt.id), err)
			continue
		}
		built++
		w.recordBuild(ctx, tgt.id, count, int(time.Since(buildStart).Milliseconds()))
		log.Printf("[EngagementFamilyBuilder] %s — %d members (window [%s, %s), %d partition month(s))",
			tgt.name, count, from.Format("2006-01-02"), to.Format("2006-01-02"), partitionMonthsTouched(from, to))

		// Pace the rebuilds so this scan never starves the send path.
		select {
		case <-ctx.Done():
			return
		case <-time.After(engagementFamilySegmentPause):
		}
	}
	log.Printf("[EngagementFamilyBuilder] pass complete: %d built, %d failed, %s",
		built, failed, time.Since(start).Round(time.Millisecond))
}

// ensureSegment creates the spec's segment rows where missing (one per org).
func (w *EngagementFamilyBuilder) ensureSegment(ctx context.Context, sp engagementFamilySpec) error {
	ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := w.db.ExecContext(ectx, engagementFamilyEnsureSegmentSQL,
		sp.name, sp.description, sp.apex, sp.category)
	return err
}

// engagementFamilyTarget is one resolved (id, name) rebuild target.
type engagementFamilyTarget struct {
	id, name string
}

// listTargets resolves every family segment id by name.
func (w *EngagementFamilyBuilder) listTargets(ctx context.Context, names []string) ([]engagementFamilyTarget, error) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, engagementFamilyListSQL, pq.Array(names))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []engagementFamilyTarget
	for rows.Next() {
		var t engagementFamilyTarget
		if err := rows.Scan(&t.id, &t.name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// rebuildSegment fully rebuilds one segment's members: DELETE + INSERT..SELECT
// in ONE transaction with a raised statement_timeout (the prod default 30s
// cannot cover a 60d brand scan; the SegmentMaterializer idiom). Returns the
// new member count. On any error the tx rolls back whole — the segment keeps
// its previous members.
func (w *EngagementFamilyBuilder) rebuildSegment(ctx context.Context, segmentID, insertSQL string, from, to time.Time, apex string) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(cctx, "SET LOCAL statement_timeout = '120s'"); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(cctx,
		"DELETE FROM mailing_segment_members WHERE segment_id = $1::uuid", segmentID); err != nil {
		return 0, fmt.Errorf("delete old members: %w", err)
	}
	res, err := tx.ExecContext(cctx, insertSQL, segmentID, from, to, apex)
	if err != nil {
		return 0, fmt.Errorf("insert-select: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()
	return count, nil
}

// recordBuild stamps the build ledger and refreshes the segment's cached
// subscriber_count / last_calculated_at / updated_at (the SegmentMaterializer
// bookkeeping contract). Best-effort: the member rows are the source of truth,
// so failures log and continue.
func (w *EngagementFamilyBuilder) recordBuild(ctx context.Context, segmentID string, count int64, buildMs int) {
	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := w.db.ExecContext(bctx, engagementFamilyLedgerUpsertSQL, segmentID, count, buildMs); err != nil {
		log.Printf("[EngagementFamilyBuilder] build-ledger upsert for %s failed (non-fatal): %v", shortID(segmentID), err)
	}
	if _, err := w.db.ExecContext(bctx, `
		UPDATE mailing_segments
		SET subscriber_count   = $1::int,
		    last_calculated_at = NOW(),
		    updated_at         = NOW()
		WHERE id = $2::uuid`, count, segmentID); err != nil {
		log.Printf("[EngagementFamilyBuilder] subscriber_count refresh for %s failed (non-fatal): %v", shortID(segmentID), err)
	}
}
