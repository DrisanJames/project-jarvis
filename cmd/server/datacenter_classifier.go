package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"
	"time"
)

// =============================================================================
// Materialized, table-driven Microsoft/Azure datacenter click classifier
// (2026-07-06). Replaces the inline /8 CIDR sledgehammers in the datacenter
// branch of ignite_event_verdict() with a GiST-indexed containment lookup
// against ignite_datacenter_ranges, and MATERIALIZES the per-click verdict
// into mailing_tracking_events.click_verdict at write time so hot analytics /
// segment-recalc paths read a column instead of calling the function per row
// (a naive inline STABLE table-lookup measured 5.7x slower over 615k rows).
//
// Design (per senior review):
//   - Materialize the verdict into click_verdict (nullable TEXT). apple-mpp is
//     spare, datacenter is purge — a boolean can't carry that, hence TEXT.
//   - Provider-generic schema (provider, service_tag); Microsoft/Azure rows are
//     seeded here as a SMALL bootstrap. The full AzureCloud ServiceTags snapshot
//     is loaded out-of-band by scripts/load_azure_datacenter_ranges.sh (COPY),
//     NEVER inlined into the 5s migration slice.
//   - Apple stays special-cased in the function (mpp / private-relay branches),
//     never in the table.
//   - The verdict function is now STABLE (it reads a table) rather than
//     IMMUTABLE. No index or generated column depends on it (verified in prod
//     2026-07-06), so the volatility change is safe.
//
// The DDL below is the single source of truth: the migration entries in
// runStartupMigrations() reference these constants, and checkVerdictFunctionDrift
// compares the LIVE stored body against the *Body constants so a boot-time
// CREATE OR REPLACE never silently reverts an operator hot-patch unnoticed.
// =============================================================================

// igniteDatacenterRangesDDL — provider-generic containment table.
const igniteDatacenterRangesDDL = `CREATE TABLE IF NOT EXISTS ignite_datacenter_ranges (
	cidr        cidr PRIMARY KEY,
	provider    text NOT NULL DEFAULT 'microsoft',
	service_tag text,
	source      text NOT NULL DEFAULT 'seed',
	first_seen  timestamptz NOT NULL DEFAULT now(),
	updated_at  timestamptz NOT NULL DEFAULT now()
)`

// igniteDatacenterRangesGistDDL — GiST over inet_ops drives the <<= containment.
const igniteDatacenterRangesGistDDL = `CREATE INDEX IF NOT EXISTS idx_ignite_datacenter_ranges_gist
	ON ignite_datacenter_ranges USING gist (cidr inet_ops)`

// igniteDatacenterSeedDDL — SMALL bootstrap seed. Observed prod /24s + the major
// documented AzureCloud aggregates (preferring /12-/16 over the old /8s that
// reclassified real human Windows clicks). The two non-Microsoft ranges preserve
// the datacenter coverage the live verdict body already had (AWS 3/8, OVH
// 54.39/16) so this change does not silently regress non-MS scanner detection.
// The full AzureCloud set (thousands of prefixes, incl. IPv6) is loaded by
// scripts/load_azure_datacenter_ranges.sh. ON CONFLICT keeps re-runs cheap.
const igniteDatacenterSeedDDL = `INSERT INTO ignite_datacenter_ranges (cidr, provider, service_tag, source) VALUES
	-- Observed in prod scanner detonations (tightened from the old /8 sledgehammers)
	('9.169.0.0/16','microsoft','AzureCloud','observed'),
	('74.179.0.0/16','microsoft','AzureCloud','observed'),
	('135.232.0.0/16','microsoft','AzureCloud','observed'),
	('72.153.231.0/24','microsoft','AzureCloud','observed'),
	-- Major documented AzureCloud IPv4 aggregates (bootstrap; full set via loader)
	('13.64.0.0/11','microsoft','AzureCloud','seed'),
	('13.104.0.0/14','microsoft','AzureCloud','seed'),
	('20.33.0.0/16','microsoft','AzureCloud','seed'),
	('20.34.0.0/15','microsoft','AzureCloud','seed'),
	('20.36.0.0/14','microsoft','AzureCloud','seed'),
	('20.40.0.0/13','microsoft','AzureCloud','seed'),
	('20.48.0.0/12','microsoft','AzureCloud','seed'),
	('20.64.0.0/10','microsoft','AzureCloud','seed'),
	('20.128.0.0/16','microsoft','AzureCloud','seed'),
	('20.135.0.0/16','microsoft','AzureCloud','seed'),
	('20.150.0.0/15','microsoft','AzureCloud','seed'),
	('20.190.0.0/16','microsoft','AzureCloud','seed'),
	('20.192.0.0/10','microsoft','AzureCloud','seed'),
	('40.64.0.0/10','microsoft','AzureCloud','seed'),
	('40.74.0.0/15','microsoft','AzureCloud','seed'),
	('40.90.0.0/16','microsoft','AzureCloud','seed'),
	('40.112.0.0/13','microsoft','AzureCloud','seed'),
	('40.120.0.0/14','microsoft','AzureCloud','seed'),
	('52.145.0.0/16','microsoft','AzureCloud','seed'),
	('52.146.0.0/15','microsoft','AzureCloud','seed'),
	('52.148.0.0/14','microsoft','AzureCloud','seed'),
	('52.152.0.0/13','microsoft','AzureCloud','seed'),
	('52.160.0.0/11','microsoft','AzureCloud','seed'),
	('52.224.0.0/11','microsoft','AzureCloud','seed'),
	('104.40.0.0/13','microsoft','AzureCloud','seed'),
	('104.208.0.0/13','microsoft','AzureCloud','seed'),
	('51.4.0.0/15','microsoft','AzureCloud','seed'),
	('51.8.0.0/16','microsoft','AzureCloud','seed'),
	('51.10.0.0/15','microsoft','AzureCloud','seed'),
	('51.104.0.0/15','microsoft','AzureCloud','seed'),
	('51.132.0.0/16','microsoft','AzureCloud','seed'),
	('51.140.0.0/14','microsoft','AzureCloud','seed'),
	('4.128.0.0/12','microsoft','AzureCloud','seed'),
	('4.144.0.0/12','microsoft','AzureCloud','seed'),
	-- Preserve non-Microsoft datacenter ranges from the live verdict body (no regression)
	('3.0.0.0/8','amazon','AWS','seed_legacy'),
	('54.39.0.0/16','ovh','OVH','seed_legacy'),
	-- Azure IPv6 major aggregate (bootstrap; full set via loader). ip <<= cidr is
	-- family-aware: an IPv6 click IP is tested only against IPv6 rows.
	('2603:1000::/24','microsoft','AzureCloud','seed')
ON CONFLICT (cidr) DO NOTHING`

// igniteIPIsDatacenterDDL — STABLE PARALLEL SAFE containment helper.
const igniteIPIsDatacenterDDL = `CREATE OR REPLACE FUNCTION ignite_ip_is_datacenter(ip inet) RETURNS boolean
	LANGUAGE sql STABLE PARALLEL SAFE AS
$dcfn$ SELECT EXISTS (SELECT 1 FROM ignite_datacenter_ranges r WHERE ip <<= r.cidr) $dcfn$`

// igniteEventVerdictBody is the canonical SQL body of ignite_event_verdict —
// verbatim what Postgres stores as prosrc. checkVerdictFunctionDrift compares
// the live prosrc against this (the drift check references this constant, so
// it moves in lockstep with any edit here by construction).
//
// VERSION 2 (REQ-045, 2026-07-13). Semantic changes vs the 2026-07-06 v1 body:
//   1. proxy-view UA branch moved ABOVE the google-egress IP branch. Gmail's
//      image proxy fetches originate FROM the Google egress blocks, so under
//      v1 a GoogleImageProxy UA + 66.249.x IP — the definition of a view-time
//      read — classified 'google-egress' (S1 measured 7d: 34 google-egress vs
//      5 proxy-view for GIP-UA opens). datacenter stays ABOVE proxy-view: no
//      Google ranges exist in ignite_datacenter_ranges (seed + Azure loader),
//      so this preserves datacenter precedence without shadowing the fix.
//   2. NEW class 'ses-tracked' for user_agent='SES-Tracked' — the sentinel
//      .scratch/ses_lake_pg_ingest.py writes because the lake schema retains
//      no UA. Under v1 these fell to ELSE 'unknown' and were discarded as
//      machines everywhere (S1: 3,299 of 3,999 'unknown' 30d clicks). It is a
//      distinct VESSEL class — deliberately NOT bare-human (tier model:
//      probation/T1; today the events are single-brand RRU) and not machine.
//      IP branches above still win: an SES-Tracked click detonating from a
//      datacenter IP is a scanner.
//   3. NEW class 'human-ua-only' for device UA + NULL ip_address (10.7% of 7d
//      opens carry NULL IP — S1): every IP branch is unreachable, so v1's
//      'human' there was UA trust alone. Lower-confidence, T1-eligible not
//      T0; ignite_verdict_is_human('human-ua-only') = false by design.
// Any further semantic change MUST bump igniteVerdictVersion below.
const igniteEventVerdictBody = `
  -- verdict-version: 2 (REQ-045; SELECT ignite_verdict_version())
  SELECT CASE
    -- owned yahoo seed/engagement network (operator-confirmed 2026-06-11)
    WHEN ip <<= inet '75.98.0.0/16' THEN 'farm'
    -- Apple MPP / link-preview machinery: bare UA from Apple egress
    WHEN ua = 'Mozilla/5.0' AND (
         ip <<= inet '146.75.0.0/16' OR ip <<= inet '140.248.0.0/16'
      OR ip <<= inet '2a02:26f7::/32' OR ip <<= inet '2a04:4e41::/32'
      OR ip <<= inet '2a09:bac2::/32' OR ip <<= inet '2a09:bac3::/32')
      THEN 'apple-mpp'
    -- human click routed via iCloud Private Relay (device UA, Apple egress)
    WHEN (ua ILIKE '%iPhone%' OR ua ILIKE '%iPad%' OR ua ILIKE '%Macintosh%') AND (
         ip <<= inet '146.75.0.0/16' OR ip <<= inet '140.248.0.0/16'
      OR ip <<= inet '2a02:26f7::/32' OR ip <<= inet '2a04:4e41::/32'
      OR ip <<= inet '2a09:bac2::/32' OR ip <<= inet '2a09:bac3::/32')
      THEN 'human-relay'
    -- datacenter scanner detonations: table-driven, GiST-indexed containment
    WHEN ignite_ip_is_datacenter(ip) THEN 'datacenter'
    -- view-time webmail proxies (yahoo/gmail image proxy = real read).
    -- ORDER IS LOAD-BEARING: must precede google-egress — the Gmail proxy
    -- fetches FROM those Google blocks (v2 change #1).
    WHEN ua ILIKE '%yahoo%' OR ua ILIKE '%GoogleImageProxy%' THEN 'proxy-view'
    -- Google-side automation (forwarders/scanners; uniform Chrome from Google blocks)
    WHEN ip <<= inet '66.249.0.0/16' OR ip <<= inet '74.125.0.0/16'
      OR ip <<= inet '72.14.0.0/16'  OR ip <<= inet '209.85.0.0/16'
      THEN 'google-egress'
    -- SES-native ingest sentinel: UA not retained in the lake, so this is a
    -- vessel class, neither bare-human nor machine (v2 change #2)
    WHEN ua = 'SES-Tracked' THEN 'ses-tracked'
    -- other bare/headless automation
    WHEN ua = 'Mozilla/5.0' OR ua IS NULL OR ua = '' THEN 'machine-bare'
    WHEN ua ILIKE '%Go-http%' OR ua ILIKE '%python%' OR ua ILIKE '%curl%' THEN 'machine-bare'
    -- device/desktop UA but NO captured IP: UA trust alone, lower confidence
    -- than 'human' (v2 change #3)
    WHEN ip IS NULL AND (ua ILIKE '%Windows NT%' OR ua ILIKE '%Macintosh%' OR ua ILIKE '%iPhone%'
      OR ua ILIKE '%iPad%' OR ua ILIKE '%Android%') THEN 'human-ua-only'
    -- full device/desktop UA from non-flagged egress
    WHEN ua ILIKE '%Windows NT%' OR ua ILIKE '%Macintosh%' OR ua ILIKE '%iPhone%'
      OR ua ILIKE '%iPad%' OR ua ILIKE '%Android%' THEN 'human'
    ELSE 'unknown'
  END
`

// igniteEventVerdictDDL builds the CREATE OR REPLACE from the canonical body so
// there is exactly one copy of the branch logic.
const igniteEventVerdictDDL = `CREATE OR REPLACE FUNCTION ignite_event_verdict(ua text, ip inet)
	RETURNS text LANGUAGE sql STABLE PARALLEL SAFE AS $ivfn$` + igniteEventVerdictBody + `$ivfn$`

// igniteVerdictIsHumanBody — canonical body (pure, stays IMMUTABLE).
//
// v2 (REQ-045):
//   - COALESCE(..., false): under v1, `v IN (...)` returned NULL for a NULL
//     verdict, and `NOT (NULL OR x)` silently drops rows from aggregate
//     predicates — the lead hit this live during S7-gmail verification
//     (findings/2026-07-13-S0). The function now NEVER returns NULL, so
//     callers no longer need their own COALESCE wrappers (existing wrappers
//     remain correct).
//   - 'ses-tracked' is deliberately NOT in the human set: it is a vessel/
//     probation class (tier model T1) — the event is real but carries no UA
//     evidence. Consumers that want SES engagement read the class explicitly.
//   - 'human-ua-only' is deliberately NOT in the human set: device UA with no
//     captured IP is lower-confidence (T1-eligible, not T0). NOTE: under v1
//     these rows classified 'human', so verdict-filtered "human" counts drop
//     by the NULL-IP share (~10.7% of opens) at deploy — expected, honest.
const igniteVerdictIsHumanBody = ` SELECT COALESCE(v IN ('human','human-relay','proxy-view'), false) `

const igniteVerdictIsHumanDDL = `CREATE OR REPLACE FUNCTION ignite_verdict_is_human(v text)
	RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $vhfn$` + igniteVerdictIsHumanBody + `$vhfn$`

// igniteVerdictVersionStr — the verdict-semantics version, SELECT'able in prod
// as ignite_verdict_version() so REQ-054's ledger/harvest can record which
// version classified each derived number. Bump on ANY semantic change to
// igniteEventVerdictBody or igniteVerdictIsHumanBody, in the same diff, and
// keep the "-- verdict-version: N" marker inside igniteEventVerdictBody in
// sync (a unit test pins both).
// History: 1 = 2026-07-06 table-driven datacenter body (unversioned);
//          2 = REQ-045 (branch order, ses-tracked, human-ua-only, NULL-safe is_human).
const igniteVerdictVersionStr = "2"

const igniteVerdictVersionBody = ` SELECT ` + igniteVerdictVersionStr + ` `

const igniteVerdictVersionDDL = `CREATE OR REPLACE FUNCTION ignite_verdict_version()
	RETURNS integer LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $vvfn$` + igniteVerdictVersionBody + `$vvfn$`

// clickVerdictColumnDDL — instant, metadata-only add (nullable, no default
// rewrite). Distinct from the inert is_machine_click BOOLEAN: TEXT carries the
// spare/purge distinction (apple-mpp vs datacenter).
const clickVerdictColumnDDL = `ALTER TABLE mailing_tracking_events
	ADD COLUMN IF NOT EXISTS click_verdict TEXT`

// clickVerdictIndexDDL — partial index over clicked rows for the "recent clicks
// split by verdict" analytics pattern; mirrors the accepted sibling
// idx_tracking_events_machine_click (same table, same WHERE, same slice). NOTE:
// mailing_tracking_events is a PARTITIONED parent, so CREATE INDEX CONCURRENTLY
// is not permitted here (it errors on partitioned tables) — hence this rides the
// startup slice like its sibling rather than concurrentIndexSpecs. IF NOT EXISTS
// + the migration skip-probe keep re-runs free; if a first build exceeds the 5s
// budget it is skipped and retried next boot. Best-effort: neither the read
// filter (rides idx_tracking_events_recon) nor the backfill (rides
// idx_tracking_events_segment_v2 on event_type,event_at) depends on it.
const clickVerdictIndexDDL = `CREATE INDEX IF NOT EXISTS idx_mte_click_verdict
	ON mailing_tracking_events (event_at, click_verdict)
	WHERE event_type = 'clicked'`

// igniteSetClickVerdictFnDDL — write-time materializer. Chosen over extending the
// ~11 Go INSERT sites (mailing_tracking.go, tracking/consumer.go,
// handlers_ses_events.go, engine/ingest.go, send_worker.go, ...): a single
// BEFORE-INSERT trigger on the parent captures EVERY path uniformly — including
// the separately-deployed ignite-tracking-service and any lake replay — with no
// gaps, whereas is_machine_click (populated only in the Go paths) is inert in
// prod precisely because those paths are not the ones writing prod rows. The
// NEW.click_verdict IS NULL guard lets a caller pre-set a verdict without the
// trigger clobbering it.
const igniteSetClickVerdictFnDDL = `CREATE OR REPLACE FUNCTION ignite_set_click_verdict() RETURNS trigger
	LANGUAGE plpgsql AS $trgfn$
BEGIN
	IF NEW.click_verdict IS NULL THEN
		NEW.click_verdict := ignite_event_verdict(NEW.user_agent, NEW.ip_address);
	END IF;
	RETURN NEW;
END;
$trgfn$`

// igniteSetClickVerdictTriggerDDL — guarded so the CREATE (which takes a brief
// ShareRowExclusive lock across the parent + every partition) fires only once,
// not on every boot. The WHEN (event_type='clicked') filter keeps the trigger
// off the high-volume sent/opened/delivered insert paths. PG16 cascades a parent
// row trigger to all existing and future partitions.
const igniteSetClickVerdictTriggerDDL = `DO $trgcreate$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_trigger
		WHERE tgname = 'trg_set_click_verdict' AND NOT tgisinternal
	) THEN
		CREATE TRIGGER trg_set_click_verdict
			BEFORE INSERT ON mailing_tracking_events
			FOR EACH ROW WHEN (NEW.event_type = 'clicked')
			EXECUTE FUNCTION ignite_set_click_verdict();
	END IF;
END
$trgcreate$`

// normalizeSQLBody strips SQL line comments and collapses whitespace so the drift
// check flags SEMANTIC divergence (different branch logic) while ignoring
// cosmetic reformatting. Best-effort: assumes no "--" inside string literals
// (true for both verdict bodies). Warnings only — never fatal.
func normalizeSQLBody(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if idx := strings.Index(ln, "--"); idx >= 0 {
			lines[i] = ln[:idx]
		}
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// checkVerdictFunctionDrift reads the LIVE stored body (pg_proc.prosrc — the
// exact text pg_get_functiondef wraps) and compares it, normalized, against the
// committed canonical body. If they diverge, a boot-time CREATE OR REPLACE is
// about to overwrite an operator hot-patch — that must not happen silently, so
// we log a WARNING. MUST run BEFORE runStartupMigrations applies the functions.
// prosrc is used (not full pg_get_functiondef) on purpose: it is the verbatim
// stored body, whereas matching PG's reformatted header is brittle.
func checkVerdictFunctionDrift(db *sql.DB) {
	fns := []struct{ name, canonical string }{
		{"ignite_event_verdict", igniteEventVerdictBody},
		{"ignite_verdict_is_human", igniteVerdictIsHumanBody},
		{"ignite_verdict_version", igniteVerdictVersionBody},
	}
	for _, f := range fns {
		var live string
		err := db.QueryRow(
			`SELECT prosrc FROM pg_proc WHERE proname = $1 AND pronamespace = 'public'::regnamespace`,
			f.name,
		).Scan(&live)
		switch {
		case err == sql.ErrNoRows:
			// First deploy of this function — nothing to drift from.
			continue
		case err != nil:
			log.Printf("[VerdictDrift] %s: live-body probe failed: %v", f.name, err)
			continue
		}
		if normalizeSQLBody(live) != normalizeSQLBody(f.canonical) {
			log.Printf("[VerdictDrift] WARNING: live %s body DIFFERS from committed source — "+
				"this boot's CREATE OR REPLACE will OVERWRITE a hot-patch. Live(normalized, first 280 chars): %.280s",
				f.name, normalizeSQLBody(live))
		} else {
			log.Printf("[VerdictDrift] %s: live body matches committed source", f.name)
		}
	}
}

// backfillClickVerdict populates click_verdict for historical clicked rows in
// calm-IO-gated, time-windowed batches — never in the 5s migration slice.
// Idempotent (only NULL rows), resumable (interrupted windows retried next boot),
// non-fatal, honors ctx cancellation. Rides idx_tracking_events_segment_v2
// (event_type, event_at) so each window is a range scan, not a full heap scan.
// Kill switch: DISABLE_CLICK_VERDICT_BACKFILL=1.
func backfillClickVerdict(ctx context.Context, db *sql.DB) {
	if os.Getenv("DISABLE_CLICK_VERDICT_BACKFILL") == "1" {
		log.Printf("[ClickVerdictBackfill] disabled via DISABLE_CLICK_VERDICT_BACKFILL=1")
		return
	}

	// Fast idempotent exit: nothing left to classify.
	var one int
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM mailing_tracking_events
		 WHERE event_type = 'clicked' AND click_verdict IS NULL LIMIT 1`).Scan(&one); err == sql.ErrNoRows {
		log.Printf("[ClickVerdictBackfill] no un-classified clicked rows — nothing to do")
		return
	} else if err != nil {
		log.Printf("[ClickVerdictBackfill] pre-check failed (will retry next boot): %v", err)
		return
	}

	var minAt, maxAt sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT min(event_at), max(event_at) FROM mailing_tracking_events
		 WHERE event_type = 'clicked' AND click_verdict IS NULL`).Scan(&minAt, &maxAt); err != nil {
		log.Printf("[ClickVerdictBackfill] bounds probe failed: %v", err)
		return
	}
	if !minAt.Valid || !maxAt.Valid {
		return
	}

	const window = 6 * time.Hour
	var totalUpdated, windows int64
	log.Printf("[ClickVerdictBackfill] starting: %s .. %s in %s windows",
		minAt.Time.UTC().Format(time.RFC3339), maxAt.Time.UTC().Format(time.RFC3339), window)

	for cur := minAt.Time; !cur.After(maxAt.Time); cur = cur.Add(window) {
		if ctx.Err() != nil {
			log.Printf("[ClickVerdictBackfill] context cancelled after %d windows / %d rows", windows, totalUpdated)
			return
		}
		backfillWaitForCalmIO(ctx, db)

		conn, err := db.Conn(ctx)
		if err != nil {
			log.Printf("[ClickVerdictBackfill] connection failed: %v — stopping (retry next boot)", err)
			return
		}
		// Generous per-window budget; this is off the 5s migration path.
		_, _ = conn.ExecContext(ctx, `SET statement_timeout = '60s'`)
		res, err := conn.ExecContext(ctx, `
			UPDATE mailing_tracking_events
			SET click_verdict = ignite_event_verdict(user_agent, ip_address)
			WHERE event_type = 'clicked'
			  AND click_verdict IS NULL
			  AND event_at >= $1 AND event_at < $2`, cur, cur.Add(window))
		conn.Close()
		if err != nil {
			log.Printf("[ClickVerdictBackfill] window %s failed: %v — continuing", cur.UTC().Format(time.RFC3339), err)
			continue
		}
		if n, e := res.RowsAffected(); e == nil {
			totalUpdated += n
		}
		windows++
		time.Sleep(2 * time.Second)
	}
	log.Printf("[ClickVerdictBackfill] complete: %d rows classified across %d windows", totalUpdated, windows)
}

// backfillWaitForCalmIO defers a batch while the primary is IO-saturated, so the
// backfill never piles onto a busy window. Bounded (never blocks boot forever):
// after ~5 minutes of probing it proceeds anyway, since the backfill is small,
// idempotent, and low priority.
func backfillWaitForCalmIO(ctx context.Context, db *sql.DB) {
	for attempt := 0; attempt < 10; attempt++ {
		if ctx.Err() != nil {
			return
		}
		var ioWait int
		err := db.QueryRowContext(ctx, `
			SELECT COUNT(*)::int FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'active' AND wait_event_type = 'IO'
			  AND pid <> pg_backend_pid()`).Scan(&ioWait)
		if err != nil || ioWait < concurrentIndexIOWaitMax {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}
