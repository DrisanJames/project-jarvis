# METRIC CONTRACT — the one way every number is computed

**Status: BINDING for all portal screens and analytics endpoints** (operator directive 2026-07-01:
"unify this learning so future agents understand"). Any screen, endpoint, or agent that displays or
computes email-performance numbers MUST follow this contract. When two surfaces disagree, the one
that violates this contract is the one that's wrong. Companion doc: `PORTAL_DESIGN_SYSTEM.md`
(how numbers are *displayed*); CLAUDE.md §9/§11 (repo-wide rules); the audience-knowledge MCP
catalog encodes the same rules for agents doing ad-hoc analysis.

Every rule below was verified against production data (dates noted). The recurring root cause of
"dashboard says X, reporting says Y" is a screen hand-rolling one of these rules differently —
do not re-derive them; reference this file.

---

## 1. Sources of truth — who answers which question

| Question | Source | NOT this |
|---|---|---|
| Delivery outcomes (delivered/bounce/deferral) | **Athena lake** `ignite_analytics.email_events`, `source IN ('pmta','ses')` | PG campaign counters (dead since 2026-05-29); the lake's `app` stream (PG mirror — duplicates) |
| Opens/clicks, RAW (machine incl.) | Lake `source='app'` (the PG-tracking mirror) — or PG `mailing_tracking_events` directly | Lake `pmta` rows (carry none) or `ses` rows alone (webhook slice only, ~1% of opens) |
| Opens/clicks, HUMAN | PG `mailing_tracking_events` + `ignite_event_verdict(user_agent, ip_address)` | `is_machine_open` / `is_machine_click` columns — **INERT** (verified 2026-06-24) |
| Per-campaign delivered for CPM/billing | PG `mailing_tracking_events` (100% campaign-attributed) | Lake (loses ~20% of PMTA rows to blank campaign_id) |
| Campaign metadata/status | Campaign Center (`mailing_campaigns` row, campaign-summary endpoint) | Its aggregate counter columns (`sent_count`, `open_count`, … — stale) |

**The `app` stream rule (double-count footgun).** The lake's `source='app'` rows are a continuous
mirror of PG `mailing_tracking_events` — including `delivered`/`attempted`/bounce rows that
duplicate the pmta/ses truth, and open/click rows that duplicate the SES webhook's. Verified
2026-07-01: one campaign showed delivered=17,448 under BOTH `pmta` and `app`; SES opens appeared
identically (616=616) in `ses` and `app`. Therefore:
- Delivery metrics: `source IN ('pmta','ses')` — never unfiltered, never `app`.
- Engagement metrics: `app` ONLY — and when merging with delivery rows, STRIP the delivery set's
  own open/click slice first (see `mergeDeliveryAndEngagement`, EventLakeExplorer.tsx).

## 2. Counting

- Lake counts are **`COUNT(DISTINCT event_uid)`**, never `COUNT(*)` — the PMTA HTTP bridge
  redelivers events.
- **Attempted is DERIVED**: `delivered + hard + soft + untyped bounces`. Raw `attempted` events
  exist on the SES pipe ONLY (PMTA emits none) — displaying the raw count next to blended
  delivered produced delivered ≫ attempted (microsoft 187,824 vs 522,526, 2026-07-01). Label
  derived attempted as such (`Attempted*` + tooltip).
- Every rate DISCLOSES its denominator (tooltip or subtext). Delivery/hard/soft/complaint rates
  divide by derived attempted; open/click rates divide by delivered; CTOR divides by opens.
- **Never divide metrics with different scopes** (e.g. all-ISP opens over ISP-filtered delivered
  → >100% rates). If a filter applies to the denominator but the numerator's source can't honor
  it, SUPPRESS the rate and label the mismatch.

## 3. Bounce taxonomy (read-time reclassification)

Lake reads re-derive bounce classes from `bounce_cat` + `dsn_diag` (`eventTypeExpr`, reader.go):
- **hard_bounce** = true list hygiene ONLY: `bounce_cat IN ('hard','bad-mailbox','bad-domain','inactive-mailbox')`.
- **reputation_block** = `spam-related / policy-related / routing-errors / no-answer-from-host /
  bad-connection` — sender-side blocks of VALID recipients. Counted in NEITHER hard nor soft.
- **administrative** = operator `pmta flush` (`dsn_diag LIKE '%deleted by administrator%'`) —
  cancellations, not bounces; excluded from everything including attempted.
- **Hard and soft are NEVER summed** into one "bounces" number. Hard renders `#ef4444`, soft
  `#f59e0b`, always (CLAUDE.md §6).
- `delivery_delay` rows are per-RETRY events (many per message) — never present them as unique
  deferred messages; the deferral-funnel endpoint computes per-message lifecycle.

## 4. Windows and time

- User-facing day buckets are **America/Denver** (`local_dt` in the lake; `event_at AT TIME ZONE
  'America/Denver'` in PG). The lake's `dt` partition is UTC plumbing — widen partitions ±1 day
  and re-filter on the Denver predicate (see `buildBreakdownSQL`).
- PG range queries on partitioned tables MUST also bound the raw partition key (`event_at >= from-1d`)
  or the planner scans every monthly partition (the engagement 15s-timeout incident).

## 5. Scoping

- **Brand = apex domain** (`discountblog.com`), resolved by `internal/pkg/brand.Root()`. In the
  lake use the computed `brand` dimension (`brandExpr`: stored brand, else the VMTA brand-code) —
  the raw column is blank on all pmta/ses rows before 2026-07. In PG match sending_domain
  exact-or-dot-suffix — never substring `ILIKE` (wildcard + infix hazards).
- **ISP = the clean classifier** (`ispExpr` in the lake / `isp.GroupFromDomain` in Go), computed
  from the real recipient domain. The raw `isp_group` column carries PMTA `*.queue` pollution —
  never key user-facing rows on it. (Exception: the deferral funnel joins on raw isp_group today;
  migration pending.)
- **Org**: every PG handler resolves via `GetOrgIDFromRequest`; the lake is not org-partitioned.

## 6. Engagement semantics (which number to show where)

- **RAW** opens/clicks (machine/MPP included) are the operational signal — always labeled
  "raw"/"machine incl.".
- **HUMAN** = verdict-filtered, shown as companion subtext (`human N (M openers)`), computed only
  via `ignite_event_verdict()`.
- ~90% of opens are Apple-MPP/scanner machine traffic — never present raw opens as "people opened".
- SES click-tracking fires on stylesheet/font/asset URLs — machine-click URL filtering applies
  (`isMachineClickURL`) at ingest; the verdict is the only human filter at read.

## 7. Backend surface (where the contract is implemented)

- `GET /api/mailing/analytics/lake/breakdown` — generic lake GROUP BY (whitelisted dims incl.
  computed `isp`, `brand`, `local_dt`, `local_hour`, reclassified `event_type`; `source_in` for
  the pmta+ses transport set). The Eq column list in `HandleLakeBreakdown` MUST include every
  whitelisted dim (the dropped-`isp` bug rendered global data labeled as one ISP, fixed 2026-07-01).
- `GET /api/mailing/analytics/engagement` — PG+verdict raw/human opens/clicks (Denver windows,
  anchored brand match).
- `GET /api/mailing/analytics/deferral-funnel` — per-message deferral lifecycle (fate events
  scanned to +4 days past the window).
- `GET /api/mailing/analytics/campaign-summary/…` — Campaign-Center tracking-derived truth.
- Anything new: extend these or the audience-knowledge MCP — do NOT hand-roll a new query shape
  in a screen-specific handler without checking this contract first.

## 8. Verification discipline

New or changed metric surfaces must be verified the way this contract was built: run the actual
SQL against the lake/PG for a fixed window and reconcile against the Reporting screen
(EventLakeExplorer) for the same window before shipping. If the numbers can't reconcile, the PR
must say why (unit difference, scope difference) in the UI itself (tooltip/subtext), not in a
commit message.

## 9. Offer Alignment (2026-07-07)

Surfaces: `GET /api/mailing/offer-alignment/{matrix,offer,evidence}` + the
`mailing_offer_alignment_snapshot` worker (`internal/api/offer_alignment_snapshot.go`,
`handlers_offer_alignment.go`). Every number below is scoped to an **offer campaign-ID set**.

**Offer identity & attribution.** The offer key is the stamped
`mailing_campaigns.offer_key` (deploy-time stamping, `attribution_source`
'payload' | 'name_inferred'). Historical campaigns (offer_key NULL) are **inferred**: campaign-name
" - <offer>" suffix match (against the key and the slug-map offer_name) ∪ slug-anchored money
clicks. Every response carries `attribution: {stamped_campaigns, inferred_campaigns}`;
`attribution_coverage` on matrix rows is **campaign-count based** (stamped / (stamped+inferred)) —
NOT delivery-weighted (a delivery-weighted split would need a per-campaign lake group-by that
blows the row cap).

**Delivery cells (lake).** `source IN ('pmta','ses')`, `COUNT(DISTINCT event_uid)`, scoped by
`campaign_id IN (<set>)` (reader `BreakdownFilter.CampaignIDs`, per-UUID validated, 2000 ids per
Athena call). Drip-scale sets are fetched in FULL via chunked calls — chunks are disjoint campaign
sets so per-chunk DISTINCT counts sum exactly (the earlier truncate-to-2000 behaviour read ~10% of
a 20k-campaign offer and reported 100k delivered on 2M+ mailed; removed 2026-07-08). Buckets from
the read-time `eventTypeExpr` reclassification: delivered / hard / soft / reputation_block;
`deferred` = **unique deferring mailboxes** (`DedupDelayByEmail`: delivery_delay counted by
DISTINCT recipient email — the raw events are per-RETRY, measured 2.6× inflated on 2026-07-08:
276,053 events = 106,411 mailboxes).
`attempted` is DERIVED = delivered + hard + soft + reputation_block. `block_rate` =
reputation_block / attempted. **Footnote metric:** ~20% of PMTA lake rows carry a blank
campaign_id — campaign-set-scoped delivery therefore UNDERCOUNTS by up to that share
(`unattributed_delivery`); this is a floor, not a truth-drift.

**Engagement cells (PG).** `mailing_tracking_events`, human =
`ignite_verdict_is_human(ignite_event_verdict(user_agent, ip_address))`; clicks restricted to
money links (`link_url ILIKE '%source_id=email%'`); `clickers` = COUNT(DISTINCT subscriber_id).
`clicker_rate` denominator = the **PG sent-event count** for the same campaign set + window.
**Scope rule: engagement rates are PG-scoped and delivery rates are lake-scoped — the two stores
are never divided into each other** (§2 "never divide metrics with different scopes"). ISP via the
canonical clean classifier over `recipient_domain` (mirrors `ISP_CASE_PG` /
`analytics.ispExpr` buckets exactly).

**Creatives panel.** creative identity = md5(html_content); `delivered` per creative × ISP is the
**PG sent-event count** (the lake cannot key delivery by creative; per §1 PG tracking is the
per-campaign delivery proxy) — its `clicker_rate` is therefore a same-store PG fraction. Rows
whose campaign group carries no stamped offer_key are flagged `inferred`.

**Conversions.** conv UNION = `mailing_offer_suppressions` reason='converted' (postback truth,
revenue 0) ∪ `mailing_cpm_manual_conversions` (revenue column exists but — verified against prod
2026-07-07 — every row ever written is $0; `mailing_revenue_attributions` is empty), offer-scoped
via the slug map's everflow ids (unmapped offers report 0 conversions rather than mis-tally other
offers'). Conversion × ISP uses the converting subscriber's email domain; conversion × data_source
uses `mailing_subscribers.data_source`. Creative attribution = LATERAL most-recent in-window
money-link click (90-day lookback).

**Revenue is ESTIMATED, not ledger truth.** Because no in-platform ledger carries per-conversion
dollars, Offer Alignment prices conversions at `resolveOfferConversionPrice` = the offer's CPM-deal
`ecpa_goal` (the same economics the CPM calculator derives: eCPA = budget/conversions), else
`mailing_offers.payout`, else $0 (no price ⇒ revenue stays 0 rather than inventing a number). Any
real ledger dollars, if they ever appear, win over the estimate. The offer profile discloses the
per-conversion price in `notes`. Real per-conversion revenue truth is the Everflow export
(off-platform); importing it (e.g. into `mailing_revenue_attributions`) supersedes this estimate.
`rpm` = revenue / lake delivered × 1000; `epc` = revenue / human_clicks; zero denominators emit 0,
never NaN/null.

**Large-set query discipline (drip-scale offers).** A drip offer's campaign set reaches tens of
thousands of ids (ps8241: 23,722 / 7d). Event scans chunk at 8,000 ids/statement (roundtrip-
dominated below that; every statement stays under the prod 30s statement_timeout). Creative
grouping computes `md5(html_content)` ONCE per campaign (`camp_dim` MATERIALIZED CTE), never per
event row. Consequence disclosed: clicker counts in the creatives panel and data-source panel are
per-campaign/per-chunk DISTINCT summed — a subscriber clicking the same creative across campaigns
(or chunks) counts once per campaign, not once globally.

**Data-source panel.** `mailing_subscribers.data_source` via subscriber join; NULL/'' renders as
"(unattributed)". `invalid_rate` = hard / (delivered + hard), **PG scope** (hard = PG
`event_type='bounced' AND bounce_type='hard'`).

**Badges (delivery-health channel, never blended with performance).** Single source =
`classifyAlignmentCell`: `< 500` attempted ⇒ LOW_VOLUME (`sample_ok=false`); BLOCKING =
block_rate ≥ 10% OR HM08+HCM2+S3140 dsn_family count ≥ 100; THROTTLED = deferred/attempted ≥ 20%
AND block_rate < 10% (capacity language, never reputation); LIST_QUALITY = hard/attempted ≥ 3%
dominated (≥50%) by the 5.1.1 family; else HEALTHY. `dsn_family` is a computed lake dimension
(`dsnFamilyExpr`, reader.go). Hard and soft are never combined; reputation_block is its own class.

**Windows.** Matrix windows are 7 and 30 trailing **America/Denver** days, refreshed every 30m
into `mailing_offer_alignment_snapshot` (staleness disclosed in the response). BOTH windows use
events-in-window semantics over the SAME 30-day-active campaign set — delivery (lake, sliced by
`local_dt`), engagement and conversions (PG, sliced by event time) all share one campaign scope
per cell, so a cell's rates never mix scopes. Consequence: an older campaign's in-window events
(e.g. deferral retries) count in the 7d cells, and the matrix 7d numbers may differ slightly from
a live offer-profile query whose campaign set is resolved from the 7-day window itself. A
per-(org,window) `__meta__` sentinel row marks each successful refresh: a built-but-empty window
serves an empty matrix (designed empty state), never a perpetual "building" status.

## Amendment log

- 2026-07-01: initial contract, consolidated from the Reporting-screen fix set (commits f76582f,
  dd4cb0f, ca0b3c0, cc303d5) and the audience-knowledge MCP catalog rules.
- 2026-07-07: §9 Offer Alignment — offer-keyed delivery/engagement/conversion cells, stamped vs
  inferred attribution disclosure, PG-vs-lake scope rule, badge thresholds, blank-campaign_id
  lake-loss footnote.
