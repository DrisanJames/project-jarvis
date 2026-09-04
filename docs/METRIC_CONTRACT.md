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
| Opens/clicks, RAW (machine incl.) | Lake `source='app'` (the PG-tracking mirror) — or PG `mailing_tracking_events` directly | Lake `pmta` rows (carry none). ⚠️ The "`ses` is only ~1% of opens" claim here was **stale**: 2026-09-01..03 `source='ses'` carried 2,733,470 opens / 61,565 clicks. `ses` alone is still the wrong RAW answer (SES-routed slice only) but is the ONLY correct basis for a VDM comparison (§12) |
| Opens/clicks, HUMAN | PG `mailing_tracking_events` + `ignite_event_verdict(user_agent, ip_address)` | `is_machine_open` (still INERT, deliberately — §12.5) / `is_machine_click` (lake `ses` rows populated from 2026-09-04, §12.5). Both are LABELS, never audience filters (§12.1) |
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

### 2.1 The `sent` writer — ONE, and it is the send worker (2026-09-01, REQ-086)

**`mailing_tracking_events.event_type='sent'` has exactly one writer on the campaign send path:
`SendWorkerPool.markSent`** (`internal/worker/send_worker.go`, the `trackSQL` INSERT). It writes
one row per message, at submission, for **every** transport — the ESP switch above it dispatches
`pmta`/`kumo`/`ses`/`sparkpost`/`mailgun`/`sendgrid` into the same `markSent`. That is what makes
`sent` a denominator you may compare across lanes.

**Nothing else may write `sent` for a queue-dispatched message.** In particular the SES `Send`
event notification does **not**: `handlers_ses_events.go` records it as **`ses_accepted`**, an
event type nothing divides by. Why not the obvious alternatives:

- *Keep it as `sent`* — it existed only for SES, arrived minutes after submission, and was dropped
  outright when the `campaign_id` MessageTag was absent (`persistSESEvent` returns early on an
  empty campaign tag). A row that exists for one transport and silently vanishes without a tag
  cannot be a denominator; and writing it alongside the worker's row double-counted every
  SES-relayed message (see the break below).
- *Re-type it `relayed_to_ses`* — **wrong**: `internal/engine/ingest.go` already emits
  `relayed_to_ses` from the PMTA accounting files for the PMTA→SES handoff, and
  `mailing_campaign_summary.go` counts it. Reusing that type would have moved the double-count
  rather than removed it.

**Lake identity is unchanged.** `analytics.CanonicalEventType` maps both `sent` and `ses_accepted`
to the lake's `attempted`, so `source='ses'` raw-attempted rows look exactly as they did
(`reader_lane_snapshot`, `reader_audience` first-touch). Per §2 that raw stream is still **not** a
denominator — attempted stays DERIVED.

Two writers that are NOT on this path and are NOT duplicates (each is the only writer for its own
message): the synchronous Campaign-Center send loop (`campaign_builder_send_sync.go`) and the
legacy SparkPost path (`mailing_sending.go`). The `backfill_sent_from_queue_v2` startup migration
is `NOT EXISTS`-guarded.

#### KNOWN BREAK — the `sent` double-count window, 2026-06-05 → 2026-09-01 (not backfilled)

From commit `c81916b` (2026-06-05), which first persisted the SES `Send` notification as a
tracking event, until the deploy of REQ-086, **every SES-relayed or SES-direct message with a
`campaign_id` MessageTag has TWO `sent` rows** — one from the send worker (`gen_random_uuid`, a
v4 id, carries `metadata`/`offer_id`/`partner_dataset_id`) and one from the SES handler
(a deterministic **v5** id, no offer/dataset stamp, arriving seconds-to-minutes later).

Measured on prod (`substring(id::text,15,1)` = the UUID version, the exact writer discriminator):

| Denver day | worker rows (v4) | SES rows (v5) |
|---|---|---|
| 2026-06-04 (pre) | 856,681 | 48 |
| 2026-06-06 | 160,113 | 125,387 |
| 2026-07-15 | 501,128 | 482,287 |
| 2026-08-15 | 996,025 | 945,185 |
| 2026-08-31 | 1,982,282 | 1,771,963 |

Last 24 h to 2026-09-01: 1,503,347 (subscriber × campaign) pairs carried BOTH rows, 374,999 the
worker row only (non-SES lanes), 4,300 the SES row only — of which 99.2% were window-boundary
artifacts (the worker row fell outside the window) and 25 were genuine orphans (0.0017%).

**Consequence for historical reads.** Any figure whose denominator is a raw
`COUNT(*) WHERE event_type='sent'` over that window is inflated on SES-routed mail by up to 2×,
so the rates built on it (open %, click %, delivery %, click-rate pacing) read up to ~50% low.
Affected surfaces (all read the same column; none distinguished the writer):
`agents/reporting/ecosystem_status.py`, `auto_lane_daily.py`, `auto_lane_money_clicks.py`,
`partner_lane_report.py`, `drip_lane_isp_report.py`, `agents/jobs/scanner_weed.py`,
`warm_touch_history_stamp.py`, `partner_burned_record_salvage.py`, and the portal handlers in
`mailing_analytics*.go`, `metrics.go`, `campaign_timeseries.go`, `campaign_builder_analytics.go`,
`handlers_offer_alignment.go`, `offer_center_handlers.go`, `outbox_engine_status.go`,
`governance_handlers.go`, `property_lane_stats.go`, `send_baselines.go`,
`audience_cadence_by_cell.go`, `site_events_handler.go`, `marketing_agent_tool_dispatch.go`.

Counts that use `COUNT(DISTINCT subscriber_id)` or `bool_or(event_type='sent')` were **never**
affected (`partner_lane_report.py`, `drip_lane_isp_report.py`, `property_lane_stats.go`,
`mailing_campaign_summary.go`, `mailing_analytics_promoted.go`), and neither was the
send-liveness gate (`agents/jobs/scaffold.verify_send_liveness`), which is a `count(*) > 0`
presence check the worker row satisfies first.

**No backfill** — the duplicate rows are left in place unless the operator asks for a purge. To
read that window correctly, add `AND substring(id::text,15,1)='4'` (worker rows only), or count
`COUNT(DISTINCT subscriber_id)`.

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

## 11. Drip supply chain (REQ-118, 2026-09-03)

The Supply tab (`web/src/components/mailing/components/Supply*.tsx`) projects the mediators' own
records. It computes nothing: every number below is read from `/api/mailing/supply/*`
(`internal/api/drip_supply_handlers.go`), which is the only implementation. A number that disagrees
with this screen is wrong on the other screen.

### 11.1 Every number carries a label — the closed vocabulary

Each response carries a `labels` map naming what kind of number each field is. The vocabulary is
closed (`dripLabelVocab`, six values) and the UI renders the label in the cell's tooltip:

| label | means | changes when |
|---|---|---|
| `contracted` | the static contract's policy value | only at a Denver midnight, by operator/lead action |
| `effective` | contracted after governors reduced it | every tick (governors reduce, never raise) |
| `planned` | the daily planner's frozen award | once a day, at plan freeze |
| `reserved` | capacity held by a live reservation, not yet submitted | per reservation |
| `actual` | measured after the fact (committed, sent, ledgered, revenue) | as events land |
| `forecast` | expected, not measured (pending EO, expected arrivals) | as the supply controller re-projects |

**`contracted` vs `effective` is the single most misread pair.** `contracted` is what policy allows;
`effective = min(contracted, governor ceilings)` where the governors are the ISP throttle state, the
SES daily-quota remainder for SES-routed ISPs, the domain's health band and the gmail hold. A
governor value ABOVE the contract is ignored. `effective < contracted` is normal operation, not a
fault — the binding governor is named in `effective_reason` and the UI always renders it next to the
number.

### 11.2 The five demand states per lane×ISP

Never collapse these into "how much did the lane get". They answer different questions:

1. **desired** (`contracted`) — the dispatch contract's `desired_daily_intros` for the ISP. Absent
   ISP = 0 desired ("not wanted"), which is NOT the same as an excluded ISP.
2. **awarded_firm** (`planned`) — the planner's award backed by mailable supply that exists now.
3. **awarded_provisional** (`planned`) — the award the supply controller must still deliver before
   need time (pending EO × yield + expected arrivals + remail credit).
4. **supply_backed** (`planned`) — the award supply can actually stand behind.
5. **unserved** (`planned`) — `desired − firm − provisional`, always carrying an `unserved_reason`
   from the closed set `supply | domain_capacity | max_intro_share | followup_reserve |
   negative_contribution | governor | no_contract`. Unserved without its reason is not reportable.

`followups_reserved` is not one of the five: due follow-ups are OBLIGATIONS reserved before any
discretionary intro, and intros and follow-ups share ONE domain×ISP balance.

### 11.3 Fill rate

**`fill_rate = committed ÷ desired`.** When `desired = 0` the fill rate is **null (unknown)** and
renders as "unknown" — never 100% (a lane that wants nothing did not succeed) and never 0% (it did
not fail). A lane with demand and no committed capacity is `null` too, and the health rule treats
that as amber, not green.

### 11.4 The three eCPMs, and the maturity that gates them

All three share the same denominator — **messages in the cohort** — and all use a **7-day attribution
window**. Revenue = `mailing_everflow_conversions` payout attributed by `campaign_id`, counted
**UNJOINED** (three `mailing_offers` rows share EF 162 and a join fans it ×3), plus
`drip_manual_revenue`.

| metric | formula | used for |
|---|---|---|
| Gross eCPM | revenue ÷ messages × 1000 | reporting |
| **Dispatch contribution eCPM** | (revenue − send cost) ÷ messages × 1000 — **EO cost is SUNK** | the planner's RANK of already-mailable inventory |
| Fully loaded net eCPM | (revenue − send − EO − acquisition − infra share) ÷ messages × 1000 | reporting only; **infra share is NULL** until OVH + IPXO monthlies are supplied, so this figure is legitimately unknown |

Plus **cleaning value** = expected revenue per raw record − acquisition − expected EO ÷ yield −
expected send cost over the ladder. That is the supply controller's ordering metric, not a rate.

**Maturity is part of the number.** `mature` = the cohort is ≥ 7 days old; `incomplete` = the window
has not closed; `unknown` = no data. **Only a mature figure ranks**, and a lane below the minimum
sample (20k messages OR 5 conversions) inherits the estate median for its record class — the response
flags that with `inherited: true` and the UI says "inherited estate median" rather than presenting it
as the lane's own performance. An immature figure ranks as 0, never negative.

### 11.5 Units — records vs messages, never summed

The **Supply Ledger counts unique RECORDS**. The **Capacity Ledger counts MESSAGES**. One record
becomes one intro plus up to four follow-ups, so the two are not comparable and are never added. Each
table on the Supply tab states its unit in the panel header.

### 11.6 Health colour — one implementation

`health` and `health_reason` come from the API (`dripHealthColour`). The rule: **grey** if paused ·
**red** if two consecutive `zero`/`failed` tick outcomes while `desired > 0` · **amber** if fill rate
< 80% (or fill rate is unknown while `desired > 0`) · **green** otherwise. The UI maps the string to
a colour token and never re-derives it — two implementations of a colour rule is two colour rules.

### 11.7 Unknown is not zero

Every count on this surface is nullable and **null renders as "unknown", muted, never 0**. The
distinction is load-bearing:

- **no `drip_capacity_balance` row for the day** ⇒ capacity is UNKNOWN (the midnight rebuild may not
  have run), not "every domain capped at 0";
- **no `drip_lane_balance` row** ⇒ demand is UNKNOWN, not "no lane wants anything";
- **`stranded_claims` null** ⇒ the 48h orphan-claim count exceeded the 20s statement budget (the
  reap index may still be building) — unknown, not zero;
- **an absent Supply-Ledger event key IS a measured zero** — that ledger is append-only, so "no
  `VALIDATION_ORDERED` row today" really is $0.00 ordered. This is the one place a missing row means
  zero, and the UI says so in the cell's tooltip.

Responses name what they could not compute in a `degraded[]` array; the UI prints those verbatim in
the header strip. Every response also carries `as_of` and the `contract_versions` it was computed
against — a supply number without its `as_of` and contract version is a number about an unknown
moment under unknown policy.

## 12. The two engagement bases — and which one governs (2026-09-04)

Engagement has **two legitimate counts**, and they differ by ~37% on opens.
Neither is wrong. Using the wrong one for the wrong purpose is.

| | **INCLUSIVE** — the operating basis | **VDM-COMPARABLE** — the reconciliation lens |
|---|---|---|
| Definition | every open/click **EVENT**, counted on the day the **event** happened | **UNIQUE engaged MESSAGES**, attributed to the day the message was **sent**, `source='ses'` only |
| Governs | audience selection, engagement tiers, every segment definition, the engaged-tier anchors | deliverability reporting and reconciliation with AWS SES VDM |
| Where | PG `mailing_tracking_events` / lake open+click event counts; `human_engagement` MCP tool | `vdm_engagement` MCP tool; canonical SQL `agents/dbknowledge/_db.py:vdm_comparable_engagement()` |
| May feed an audience query | **YES — this is the only one that may** | **NEVER** |

### 12.1 ⭐ The rule: when in doubt, count MORE

**Operator, 2026-09-04:** *"I would rather over count than under count. If we
over count, we can have a hundred percent certainty that we are not cutting out
audience members, whereas if we under count, there is always a question."*

The inclusive number is high **on purpose**. Do not filter it down to match VDM,
a scanner verdict, a bot label, or any other authority. An undercount classifies
a real human as unengaged and drops them from an audience — the one outcome that
is not recoverable. This is the same rule that already forbids
`ignite_verdict_is_human` from gating an audience (CLAUDE.md §6: a scanner-STORM
filter with a ~20% false-negative floor on proven humans, 35% at Microsoft).

Every machine/scanner signal we store is therefore a **LABEL, not a filter**:
`is_machine_click`, `is_machine_open`, `ses_bot_event`, `click_verdict`. A label
may appear in a reporting lens. A label may never appear in a segment
definition, a planner audience query, or a journey enroller.

### 12.2 Why the two differ — measured, not assumed

Reconciled against AWS SES Virtual Deliverability Manager, 2026-09-01..03,
`source='ses'`, VDM's tracked ISP buckets (sbcglobal folded into att):

| basis | 6-bucket opens | vs VDM (1,790,394) |
|---|---|---|
| raw open EVENTS on the event day | 2,519,711 | **+37.1%** |
| + dedup to one open per message | 2,308,751 | +25.6% |
| + attribute to the SEND cohort | 1,813,506 | **+1.3%** |

Two independent differences compound. Both are counting basis; **neither is bot
filtering**:

1. **VDM counts unique per message.** AWS API reference,
   `BatchGetMetricDataQuery`: *"OPEN — Unique open events for emails including
   open trackers"*, *"CLICK — Unique click events for emails including wrapped
   links"*. Raw event publishing is per event: *"If a recipient opens an email
   multiple times, Amazon SES counts each open as a unique open event"*
   (SES metrics FAQ). Measured: **1.14 open events and 3.35 click events per
   engaged message**.
2. **VDM attributes to the SEND day.** Measured: **28%** of the opens landing in
   2026-09-01..03 belong to messages sent before the window. AWS does not
   document the attribution day; the evidence is that switching to send-cohort
   attribution moves five independently-sized buckets (5,139 to 1.58M) from
   +25.6% to within +1.3%..+4.4% simultaneously.

**Ruled OUT with data — do not re-litigate these:**

- *AWS bot-filters VDM.* No. The VDM dashboard doc still carries the warning
  that Apple MPP *"causes engagement data to look much higher than it typically
  would be"*, and the `isBotEvent` field AWS shipped 2026-08-07 is an annotation
  on raw Open/Click notifications, not a VDM filter.
- *We double-ingest the same open (SES notification + our own pixel) as
  `source='ses'`.* No. There are exactly two `analytics.Emit` call sites —
  `handlers_ses_events.go` (`source='ses'`) and `engine/ingest.go`
  (`pmta`/`kumo`). The pixel path (`mailing_tracking.go`,
  `internal/tracking/consumer.go`) does not emit to the lake at all.
  `COUNT(*)` = `COUNT(DISTINCT event_uid)` = 2,733,470 on the window: zero
  duplicate lake rows.
- *Timezone / day-boundary skew.* No. VDM day buckets are UTC (VDM dashboard
  doc: *"All dates & times are UTC"*) and so is the lake's `dt` partition. A
  boundary skew also cannot produce a uniform +35–45% on six buckets at once.
- *Per-LINK click grain.* No. Unique (message, link) overshoots every bucket
  except yahoo: microsoft +72.8%, att +109.5%, cox +69.6%. Unique-per-message is
  the right grain.

### 12.3 Maturity — a cohort number is not final for ~3 days

The send-cohort metric settles over roughly three days, and the tail is
ISP-shaped. Measured on the 2026-08-25 send cohort, unique engaged messages at
day+0 as a share of the day+7 value:

| bucket | opens d+0 | clicks d+0 |
|---|---|---|
| microsoft | 43% | 63% |
| apple | 67% | 77% |
| att | 63% | 8% |
| yahoo | 55% | **2%** (9 → 445 by day+2) |

A VDM-comparable figure read with less than 3 days of tail is an **undercount,
not a discrepancy**. Label the maturity or do not publish the number.

### 12.4 What did NOT close — yahoo

| bucket | opens vs VDM | clicks vs VDM |
|---|---|---|
| microsoft | +1.3% | +1.8% |
| apple | +2.8% | +0.9% |
| cox | +1.7% | +2.9% |
| aol | +4.4% | −4.3% |
| att | +3.5% | +9.1% |
| **yahoo** | **−15.3%** | **−57.9%** |

Yahoo is the single bucket that does not reconcile, and its click gap is larger
than any counting-basis effect. Ruled out by measurement: per-link grain, the
sbcglobal/bellsouth→yahoo re-fold (moves *both* yahoo and att further from VDM),
and a same-timestamp ingest collision (multi-row `(recipient_send_id, event_at)`
groups exist, so nothing is being dropped there). **Cause unknown.** State it as
open; do not smooth it into a total.

### 12.5 Where the labels are written

- `mailing_tracking_events.is_machine_click` — written by all three click ingest
  paths (`mailing_tracking.go` pixel, `internal/tracking/consumer.go` SQS,
  `handlers_ses_events.go` SES). Fires rarely by design (3 of 289,196 clicks
  over 2026-09-01..03): the classifier is conservative, false negatives
  preferred.
- `mailing_tracking_events.ses_bot_event` (2026-09-04) — AWS's own `isBotEvent`
  label (`"Likely"`/`"Unlikely"`), persisted verbatim for **both** opens and
  clicks. NULL = **UNKNOWN** (no label sent), never "human".
- Lake `is_machine_click` — now set on `source='ses'` **click** rows only
  (`analytics.Event.IsMachineClick`, `*bool` + `omitempty`). It was NULL on
  every row before 2026-09-04 because the SES handler computed the verdict and
  never put it on the lake event. Non-click rows stay NULL so
  `is_machine_click IS NOT NULL` remains an honest coverage measure.
- **NOT written: `is_machine_open`.** It is read by ~15 opt-in `ExcludeMPP`
  filters that are inert only because nothing writes the column. Populating it
  would silently switch those screens from inclusive to filtered counting —
  exactly the undercount §12.1 forbids. Use `ses_bot_event` instead.

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
- 2026-09-01: §2.1 The `sent` writer (REQ-086) — `SendWorkerPool.markSent` named the single
  canonical writer of `event_type='sent'`; the SES `Send` notification re-typed to `ses_accepted`
  (`relayed_to_ses` rejected: `engine/ingest.go` already owns it for the PMTA→SES handoff);
  `CanonicalEventType` maps `ses_accepted` → lake `attempted` so the `source='ses'` lake stream is
  unchanged; the 2026-06-05 → 2026-09-01 double-count documented as a known break with the
  affected-surface list and the `substring(id::text,15,1)='4'` read-around. No backfill.
- 2026-09-03: §11 Drip supply chain (REQ-118) — the six-value label vocabulary and the
  contracted-vs-effective rule (governors reduce, never raise); the five demand states per lane×ISP
  with their closed `unserved_reason` set; fill rate = committed ÷ desired, NULL when desired is 0;
  the three eCPMs on a messages denominator with a 7-day window, unjoined conversion counting, and
  maturity/minimum-sample gating (only mature ranks; below-sample inherits the estate median);
  records vs messages never summed; the health colour as a single API-side implementation; and
  "unknown is not zero", including the one exception (an absent append-only Supply-Ledger event key
  is a measured zero).
- 2026-09-04: §12 The two engagement bases — the inclusive event count named the
  OPERATING basis that governs audience selection, and the VDM-comparable
  unique-message/send-cohort count confined to a reporting lens, under the
  operator's over-count-never-undercount rule. The +37.1% open gap vs AWS VDM
  decomposed and closed to +1.3% (5 of 6 buckets within 5%) by two measured
  causes — VDM's unique-per-message grain and its send-day attribution — with
  AWS bot filtering, lake double-ingestion, timezone skew and per-link click
  grain each ruled OUT with data. Cohort maturity (~3 days, ISP-shaped) pinned;
  yahoo recorded as NOT reconciled. Machine signals restated as LABELS not
  filters: `ses_bot_event` added (AWS `isBotEvent`), the SES click verdict wired
  into the lake, and `is_machine_open` explicitly left unwritten so the ~15 inert
  `ExcludeMPP` filters cannot switch on by accident.
