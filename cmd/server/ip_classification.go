package main

// =============================================================================
// IP classification table (REQ — 2026-09-05)
// =============================================================================
// ignite_datacenter_ranges answers ONE question: who OWNS this range. It is a
// containment table of provider prefixes, and it is deliberately coarse — the
// bootstrap seed in datacenter_classifier.go carries blanket /16s
// (74.179.0.0/16, 135.232.0.0/16) because that is the granularity Azure
// publishes ownership at.
//
// The verdict path treats "owned by a cloud provider" as "machine". Measured on
// prod: those two /16s span 131,072 addresses, we have ever observed 62 of
// them, and over 7 days they cause 134,067 clicks to be stamped 'datacenter' of
// which only 11,036 come from addresses that actually behave like scanners.
// ~123k human clicks a week are written off as machine.
//
// ignite_ip_classification is the SEPARATE table that answers the other
// question: what does this range DO. It is address-granular by design (the
// observed /32s), curated, and resolved NARROWEST-MATCH-WINS, so a single
// observed /32 can carve a real verdict out of a blanket /16 without touching
// the ownership table.
//
// SCOPE OF THIS UNIT — read before extending it:
//   - ignite_ip_is_datacenter() is NOT modified. Same name, arity, return type,
//     same body. It feeds igniteEventVerdictBody, which trg_set_click_verdict
//     materializes BEFORE INSERT into mailing_tracking_events.click_verdict;
//     changing it would rewrite classification for every future click row.
//   - The verdict function is NOT repointed at this table. That is a separate,
//     operator-gated unit. Until then this table is read only by whoever calls
//     ignite_ip_class() explicitly.
//   - Nothing here derives 'scanner' from ignite_datacenter_ranges. Ownership
//     is never behaviour: every row seeded from that table lands as 'hosting'.
//
// All four statements are catalog-only or touch <= 41 rows, so they fit the 5s
// per-statement budget of runStartupMigrations (an over-budget statement there
// is logged "skipped, will retry next boot" and is silently absent forever).
// They are registered as FOUR SEPARATE entries because migrationSkipProbe
// classifies by leading keyword (migration_skip.go:41) — a combined
// CREATE TABLE + CREATE INDEX string is probed as CREATE TABLE, and once the
// table exists the index would never land.
// =============================================================================

// igniteIPClassificationDDL — the behaviour table. cidr PRIMARY KEY mirrors
// ignite_datacenter_ranges so a row is addressed by its prefix, at any masklen:
// a /32 and the /16 that contains it are two independent, coexisting rows, and
// ignite_ip_class picks the narrower.
//
// class is the behaviour; 'unresolved' is a first-class answer and NOT a
// placeholder — it is the correct verdict for a prefix carrying BOTH scanner
// sweeps and real people, where any single label would be a lie.
// confidence records how the class was established, and evidence/evidence_source
// record from what, so a later curation pass can tell an operator assertion
// apart from measured traffic.
//
// is_active (not a DELETE) keeps retirement reversible and auditable.
const igniteIPClassificationDDL = `CREATE TABLE IF NOT EXISTS ignite_ip_classification (
	cidr              cidr PRIMARY KEY,
	class             text NOT NULL,
	attributes        text[] NOT NULL DEFAULT '{}',
	confidence        text NOT NULL DEFAULT 'assumed',
	evidence_source   text NOT NULL DEFAULT 'operator',
	evidence          jsonb NOT NULL DEFAULT '{}'::jsonb,
	last_confirmed_at timestamptz NOT NULL DEFAULT now(),
	first_seen        timestamptz NOT NULL DEFAULT now(),
	updated_at        timestamptz NOT NULL DEFAULT now(),
	is_active         boolean NOT NULL DEFAULT true,
	note              text,
	CONSTRAINT ignite_ip_class_chk CHECK (class IN ('scanner','hosting','vpn-or-proxy','residential-or-mobile','unresolved','unknown')),
	CONSTRAINT ignite_ip_conf_chk CHECK (confidence IN ('confirmed','probable','assumed'))
)`

// igniteIPClassificationGistDDL — GiST over inet_ops drives the <<= containment
// scan in ignite_ip_class. The cidr PRIMARY KEY's btree cannot serve it.
const igniteIPClassificationGistDDL = `CREATE INDEX IF NOT EXISTS idx_ignite_ip_classification_gist
	ON ignite_ip_classification USING gist (cidr inet_ops)`

// igniteIPClassificationSeedDDL — three INSERTs, in this order, one entry.
//
//  1. Every ignite_datacenter_ranges row -> class 'hosting'. NEVER 'scanner':
//     that table proves who OWNS a range, never what it DOES. confidence
//     'probable' when the range was observed in prod traffic, 'assumed' for the
//     published-aggregate seed rows.
//  2. The two /32s measured detonating like scanners.
//  3. The 18 /32s that carry BOTH scanner sweeps AND real people — which is
//     exactly why they are 'unresolved' and neither 'scanner' nor
//     'residential-or-mobile'. Each is narrower than the hosting /16 that
//     contains it, so narrowest-match resolution lifts them out of it.
//
// ON CONFLICT (cidr) DO NOTHING on all three — never DO UPDATE. This statement
// is not recognized by migrationSkipProbe (leading keyword INSERT), so it
// re-executes on EVERY boot; DO UPDATE would make each boot revert operator
// curation back to the seed values.
//
// Statement 1 is bounded by the row count of ignite_datacenter_ranges (41 in
// prod at the time of writing). If the full AzureCloud ServiceTags snapshot is
// ever loaded by scripts/load_azure_datacenter_ranges.sh (thousands of
// prefixes), re-measure this against the 5s budget before the next boot.
const igniteIPClassificationSeedDDL = `
	INSERT INTO ignite_ip_classification (cidr, class, confidence, evidence_source, evidence, note)
	SELECT r.cidr,
	       'hosting',
	       CASE WHEN r.source = 'observed' THEN 'probable' ELSE 'assumed' END,
	       'ignite_datacenter_ranges',
	       jsonb_build_object('provider', r.provider, 'service_tag', r.service_tag, 'source', r.source),
	       'ownership row: provider owns this range; behaviour not established'
	FROM ignite_datacenter_ranges r
	ON CONFLICT (cidr) DO NOTHING;

	INSERT INTO ignite_ip_classification (cidr, class, confidence, evidence_source, note) VALUES
		('135.232.20.148/32','scanner','confirmed','observed-traffic','observed scanner detonation inside the 135.232.0.0/16 hosting range'),
		('74.179.67.166/32','scanner','confirmed','observed-traffic','observed scanner detonation inside the 74.179.0.0/16 hosting range')
	ON CONFLICT (cidr) DO NOTHING;

	INSERT INTO ignite_ip_classification (cidr, class, confidence, evidence_source, note) VALUES
		('74.179.70.43/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('72.153.231.69/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('135.232.20.64/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('9.169.124.5/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('135.232.19.45/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('9.169.124.16/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('74.179.68.52/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('74.179.67.139/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('72.153.231.49/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('72.153.153.63/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('135.232.20.90/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('9.169.124.20/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('135.232.20.92/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('9.169.124.21/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('135.232.20.130/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('72.153.231.68/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('74.179.70.72/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people'),
		('9.169.124.10/32','unresolved','confirmed','observed-traffic','mixed traffic: scanner sweeps AND real people')
	ON CONFLICT (cidr) DO NOTHING`

// igniteIPClassFnDDL — narrowest-match-wins accessor. STABLE PARALLEL SAFE
// mirrors ignite_ip_is_datacenter (it reads a table, so not IMMUTABLE).
// ORDER BY masklen DESC is the whole point: a /32 row beats the /16 that
// contains it, so a single observed address overrides a blanket ownership
// prefix without editing ignite_datacenter_ranges. Returns NULL for an
// unclassified address — NULL is "we have no row", distinct from the 'unknown'
// class, which is an assertion that we looked and could not tell.
//
// ⚠️ THE SECOND PARAMETER HAS A DEFAULT. A later CREATE OR REPLACE that CHANGES
// THE PARAMETER LIST does not replace this function — PostgreSQL identifies a
// function by (name, argument types), so a different signature creates an
// OVERLOAD alongside it. Two overloads that can both accept one argument make
// the bare ignite_ip_class(ip) call ambiguous and it starts erroring at runtime.
// To change the signature: DROP FUNCTION ignite_ip_class(inet, interval) first,
// in the same migration entry, before the CREATE.
const igniteIPClassFnDDL = `CREATE OR REPLACE FUNCTION ignite_ip_class(ip inet, max_age interval DEFAULT NULL)
	RETURNS text LANGUAGE sql STABLE PARALLEL SAFE AS
$ipcls$
	SELECT c.class FROM ignite_ip_classification c
	WHERE c.is_active AND ip <<= c.cidr
	  AND (max_age IS NULL OR c.last_confirmed_at > now() - max_age)
	ORDER BY masklen(c.cidr) DESC LIMIT 1
$ipcls$`
