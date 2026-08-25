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
- **preflight_validation** = `bounce_cat = 'validation'` (SES address-validation blocked the send
  BEFORE it reached the remote MX; PG mirror: `mailing_tracking_events.bounce_type='validation'`,
  suppression reason `ses_address_validation`). Counted in NEITHER hard nor soft — no remote
  rejection occurred. Kept as its own class so validator quality is comparable
  (EmailOversight false-passes = these + genuine hards on EO-verified sends). Added 2026-07-12
  (v2.6) after the ATT W1-T1 tranche's 32 "hard bounces" turned out to be 100% this class.
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

## 10. Click Funnels / click-drip lanes (2026-08-25)

**Binding for** the Click Funnels screen, `/api/mailing/click-funnels/*`, and
`ClickFunnelSnapshotWorker`. Written after a prod audit found every number on the screen
arithmetically correct and six of them semantically wrong.

### 10.1 The three families — never mix them in one row

A click-drip lane produces three kinds of number with three different time bases. The audit's
root cause was rendering them side by side as if they shared one window.

| Family | Question | Time basis | Example |
|---|---|---|---|
| **COHORT** | how did enrollments that *started* in period P turn out? | `enrolled_at` ∈ P | completion rate, cohort conversion rate |
| **ACTIVITY** | what *happened* during period P? | `event_at` / `executed_at` ∈ P | opens, clicks, sends, deferrals, errors |
| **STATE** | what is true *right now*? | point-in-time, no window | active, waiting-at-node, retrying |

A screen MUST label which family each number belongs to. A COHORT metric and an ACTIVITY metric
may never share a denominator.

### 10.2 Cohort maturity — the completion denominator

A lane's ladder has a fixed duration (`Σ delay nodes`; the estate journey `click-drip-4touch-72h`
= 1h+5h+18h+48h = **72h**). An enrollment younger than that has not had the chance to complete, so
including it in a completion denominator understates completion.

**Rule: completion and sequence-outcome rates are computed over MATURE enrollments only** —
`enrolled_at <= NOW() - (ladder_duration + 24h grace)`. The grace covers executor lag and deferral
retry. Immature enrollments are reported separately as "in flight", never folded into the rate.

Verified on offer 420, 2026-08-25:

| Cohort | n | complete | rate |
|---|---|---|---|
| Mature (>96h) | 4,887 | 3,980 | **81.4%** |
| Immature (<96h) | 732 | 158 | *not a rate — in flight* |
| Both blended (WRONG) | 5,615 | 4,138 | 73.6% |

### 10.3 Administrative exits are not funnel attrition

`mailing_journey_enrollments.exit_reason` mixes two unrelated things: behavioral exits (the lane
did something) and administrative exits (an operator bulk-purged the lane). Blending them makes a
healthy lane read as dead — offer 420 showed 130,993 "exited early" of which **130,070 (99.3%)**
were two June operator purges.

**Rule: exits are reported in three buckets, never one.**

| Bucket | Definition |
|---|---|
| `behavioral` | machine-click gate, scanner scrub, suppression, engagement watcher |
| `administrative` | operator-directed bulk exits (retire / lane-separation / QA cleanup) |
| `converted` | exited because the goal was met (see 10.4 — also counted as a conversion) |

Administrative exits are EXCLUDED from every rate denominator and shown as a separate disclosure
line. A lane whose administrative exits exceed 10% of enrollments must render that fact.

### 10.4 Outcome buckets overlap — say so

`status IN ('active','converted','exited')` tiles to the total, but `converted_at` conversions
carry `status='exited', exit_reason='converted'` — so conversions are a SUBSET of exits, not a
fourth bucket. Any stacked/tiled presentation must either exclude converted from the exit bucket
or disclose the overlap. Never present the four as disjoint.

Two competing completion sources exist and disagree: `COUNT(*) FILTER (status IN ('converted',
'completed'))` (4,316) vs the goal node's distinct execution-log reach (4,330). **The enrollment
status is canonical**; the goal-node count is a flow diagnostic and is labelled as such.

### 10.5 Conversion attribution — three numbers, never one

Most click-drip conversions are caused by the ORIGINAL click, not by a drip touch. Offer 420,
lifetime: 73 post-enrollment conversions, of which **52 (71%) had no drip send at all** before
converting and 21 (29%) followed one.

**Rule: a lane reports three conversion figures.**

| Figure | Definition |
|---|---|
| `conversions_post_enrollment` | `converted_at IS NOT NULL` — everything after enrollment |
| `conversions_pre_touch` | converted before the lane's first email executed (credit: the click) |
| `conversions_drip_attributed` | converted at/after a non-error email touch executed |

Only `conversions_drip_attributed` may be divided by a touch denominator. Per-touch conversion is
**last-touch within a declared lookback** (default 72h = the ladder); a conversion outside the
lookback of any touch is lane-attributed, never touch-attributed. Per-touch conversion rate is
suppressed (rendered `—`) when `conversions_drip_attributed` for that node is 0.

`time_to_goal` splits the same way and is never a single number:
`enrollment→conversion`, `first_send→conversion`, `last_touch→conversion`.

### 10.6 Denominators — accepted ≠ delivered

Click-drip mail is handed to SES and books `relayed_to_ses`, not `delivered`; on offer 420's
touch 1 that is 2,894 relayed vs 38 delivered. Using `delivered` alone collapses the denominator
to ~1% of real volume; using `delivered + relayed` measures ACCEPTED, not inbox-placed. No `ses`
row carries the shadow campaign id, so true delivery for this lane is **unknown**.

**Rule:** the rate base is `accepted = delivered + relayed_to_ses`, and it MUST be labelled
"accepted" wherever shown. `delivered` alone is never the base for a click-drip touch. Rates are
shown with their numerator and denominator, never bare.

### 10.7 Click quality — four values, not one "human click"

`is_machine_click` is INERT (§1, re-verified 2026-08-25: 524,300 clicks over 7 days, **zero**
`true`; 77,758 `NULL`). "Human click" computed from it equals raw click by construction and is
forbidden.

Until a canonical verdict lands in the lake, a touch reports:

| Value | Definition |
|---|---|
| `clicks_raw` | distinct subscribers with a click event |
| `clicks_classified` | those carrying a non-NULL `is_machine_click` |
| `clicks_qualified` | classified AND `is_machine_click = false` |
| `classification_coverage` | `clicks_classified / clicks_raw` |

When `classification_coverage` < 0.99 the UI must show coverage next to the qualified figure.
`clicks_qualified` may NOT be presented as a human/quality signal while coverage is unverified —
it is labelled "unclassified" until a real verdict exists.

### 10.8 Step-through denominator

Step-through is `reached(node) / reached(first node in the graph that logs execution)`, which is
the first DELAY node, not `total_enrolled` — 6,881 of offer 420's 135,882 enrollments never
logged there. **Rule:** the denominator is stated in the label ("of N who entered the ladder"),
and step-through is an ACTIVITY-family metric over the selected window, with counts shown
alongside the rate.

### 10.9 Send errors — enrollments, not rows

`mailing_journey_execution_log` writes one row per ATTEMPT. Offer 420's touch 4 showed "26,904
send errors" from **4 distinct enrollments**, 3 of them retrying every ~2 minutes for 13 days.

**Rule:** the primary figure is `distinct enrollments affected`; attempts are secondary
("4 mailboxes · 26,908 attempts"). A node with attempts/enrollment > 20 is a stuck-retry
condition and must render as an operational alert, not a metric.

### 10.10 Rounding

Percentages ROUND half-up to 2dp. Truncation (`float64(int(f*100))/100`) is forbidden — it biased
every rate on the funnel screen downward by up to 0.01pp (conv 0.0186% rendered 0.01%).
`round2` in `mailing_profiles.go` truncates and is used by 24 other call sites; click-funnel code
uses `roundPct` and does not change `round2`.

### 10.11 Freshness — what "live" can honestly mean

Measured ingest lag (`ingested_at − event_at`), 7 days to 2026-08-25:

| source | <1h | 6–24h | 1–2d | 2–7d |
|---|---|---|---|---|
| pmta / kumo | 100% | — | — | — |
| ses | 99.85% | 0.13% | — | 135 events |
| **app** (the ONLY source of open/click) | **27.7%** | **60.8%** | **8.6%** | — |

**Consequence:** click-drip engagement can never be fresher than roughly 6 hours regardless of
snapshot cadence, and a 2-day incremental window has no margin. The snapshot uses a **3-day
rolling incremental** plus a **7-day daily reconciliation**, and every payload carries per-source
watermarks. A screen MUST render `metrics_through`, not a "live" badge.

### 10.12 Creative version scope

A touch's metrics belong to the creative version that earned them. Version identity is the shadow
campaign id, `uuid.NewSHA1(ns, "click-drip-shadow-offer-<offer>-node-<node>[-v-<hash>]")`.

⚠️ **Migration hazard:** `ContentHash` was never populated, so every production campaign id was
minted from the hashless seed. Introducing the hash CHANGES the id and detaches all historical
lake events. The hashless id is therefore registered as **version 0** of each touch and remains a
permanent alias; pre-cutover metrics are labelled `blended` (all creative versions mixed) and are
never presented as belonging to the current copy.

### 10.13 Sources — what may be read at request time

| Data | Source | Request-time read? |
|---|---|---|
| delivery / engagement per touch | Athena lake | **no** — snapshot only |
| flow, state, exits, conversions | Postgres (system of record) | **no** — snapshot only |
| lane config, graph, copy, proof refs | Postgres | **no** — snapshot only |

The screen reads **one S3 snapshot** and nothing else. This is a snapshot-backed operational read
model: Athena is the sole source of every EMAIL number; Postgres remains the system of record for
journey state; neither is queried on the request path. Emitting journey state into the lake
(making Athena sole source for flow too) is the target state, not this contract.

## Amendment log

- 2026-07-01: initial contract, consolidated from the Reporting-screen fix set (commits f76582f,
  dd4cb0f, ca0b3c0, cc303d5) and the audience-knowledge MCP catalog rules.
- 2026-07-07: §9 Offer Alignment — offer-keyed delivery/engagement/conversion cells, stamped vs
  inferred attribution disclosure, PG-vs-lake scope rule, badge thresholds, blank-campaign_id
  lake-loss footnote.
- 2026-08-25: §10 Click Funnels — cohort/activity/state families, cohort maturity, administrative
  vs behavioral exits, outcome overlap, the three conversion figures, accepted-not-delivered
  denominators, the four click-quality values (is_machine_click re-verified inert), step-through
  denominator, errors as enrollments, round-not-truncate, measured ingest-lag freshness floor,
  creative-version identity + the ContentHash migration hazard, and the snapshot-only read rule.
