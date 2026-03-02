# IGNITE Mailing Platform — Comprehensive Audit

**Date**: 2026-03-01
**Purpose**: Identify all gaps preventing production-ready PMTA campaign orchestration.
**Scope**: End-to-end flow from campaign creation through delivery, tracking, dashboards, and data persistence.

---

## EXECUTIVE SUMMARY

The platform has substantial infrastructure built, but there are **23 issues** preventing a
fully orchestrated production send. The problems fall into 5 categories:

1. **Broken data pipelines** — endpoints returning wrong data or 404s
2. **Missing integrations** — UI fetches that don't match backend routes
3. **In-memory-only state** — data that disappears on restart
4. **SparkPost legacy artifacts** — hardcoded defaults that don't apply to PMTA
5. **Suppression philosophy gaps** — bounces not reaching global suppression in all code paths

---

## ISSUE 1: PMTA Campaign Deploy Creates Draft But Does NOT Send

**File**: `internal/api/handlers_pmta_campaign.go:468-559`
**Severity**: CRITICAL

`HandleDeployCampaign` creates a `mailing_campaigns` record with `status = 'draft'` and returns.
It does NOT transition the campaign to `'sending'` or invoke any sending process.

The actual send logic is in `campaign_builder_send_sync.go:HandleSendCampaign`, which expects:
- A `sending_profile_id` on the campaign (the PMTA wizard does NOT set this)
- A `list_id` on the campaign (the PMTA wizard uses `mailing_campaign_lists` join table instead)
- The campaign to be in `'draft'` or `'scheduled'` status

**What's missing**:
- After deploy, nothing triggers `HandleSendCampaign` or any async sender
- The `sending_profile_id` is never set — the wizard stores `sending_domain` but not the profile
- `HandleSendCampaign` reads subscribers via `cb.getSubscribers(ctx, listID, segmentID, ...)` which uses `campaign.ListID` (single list) — not the `mailing_campaign_lists` join table the wizard writes to
- No scheduler picks up campaigns in `'draft'` status automatically

**Fix needed**: The PMTA wizard deploy must either:
(a) Call `HandleSendCampaign` after creation, or
(b) Set status to `'scheduled'` and have a background worker poll for scheduled campaigns, or
(c) Create a new `HandlePMTASend` that reads from the join tables

---

## ISSUE 2: Subscriber Query Mismatch — getSubscribers vs. Campaign Lists

**File**: `internal/api/campaign_builder_helpers.go` (getSubscribers function)
**Severity**: CRITICAL

The `HandleSendCampaign` calls `cb.getSubscribers(ctx, listID, segmentID, maxRecipients)` using
a single `list_id` from `mailing_campaigns.list_id`. But the PMTA wizard writes list associations
to `mailing_campaign_lists` (join table) — the `list_id` column is NULL.

For 400k subscribers across all ISPs, the send function would find ZERO subscribers because it
queries with `NULL` list_id.

**Fix needed**: `getSubscribers` (or a PMTA-specific version) must read from
`mailing_campaign_lists` when `campaign.list_id IS NULL`.

---

## ISSUE 3: ISP-Based Send Segmentation Does Not Exist

**Severity**: HIGH

The user expects the system to "create campaigns as it relates to the ISP and strategy." Currently:
- The PMTA wizard stores `target_isps` as JSON on the campaign
- The send loop iterates ALL subscribers regardless of ISP
- No per-ISP sub-campaigns are created
- No per-ISP throttling or pool routing happens during the send

For a 400k send across 8 ISPs, you'd want:
- ISP-based recipient segmentation at send time
- Per-ISP rate limiting (gmail: 500/hr initially, yahoo: 300/hr, etc.)
- Per-ISP pool routing in PMTA (gmail-pool, yahoo-pool, etc.)
- Campaign Center showing sub-campaigns per ISP

**Fix needed**: The send pipeline must:
1. Segment recipients by ISP domain
2. Create per-ISP sub-campaigns (or at minimum, track per-ISP metrics)
3. Route through ISP-specific PMTA pools
4. Honor warmup limits per-ISP

---

## ISSUE 4: Servers Tab Shows Nothing — Wrong API Endpoint

**File**: `web/src/components/mailing/pages/MailingPortal.tsx:732`
**Severity**: HIGH

The Servers tab fetches `fetch('/api/mailing/pmta/servers')` but the backend route is
registered at `/api/mailing/pmta-servers` (with a hyphen, not a slash).

Result: The fetch returns 404, `pmtaServers` is always empty, and the Servers tab shows
"No PMTA servers detected."

**Fix needed**: Change the frontend fetch URL from `pmta/servers` to `pmta-servers`.
OR add a route alias on the backend.

---

## ISSUE 5: Sidebar "PMTA Offline" — Dashboard Checks Wrong Tables

**File**: `internal/api/mailing_handlers_full.go:370-382`
**Severity**: HIGH

The dashboard checks `mailing_delivery_servers` (legacy table) and `mailing_sending_profiles`
for PMTA entries. But PMTA servers are registered in `mailing_pmta_servers` (a completely
separate table).

**Fix needed**: The dashboard PMTA connectivity check should query `mailing_pmta_servers`
instead of (or in addition to) `mailing_delivery_servers`.

---

## ISSUE 6: Global Suppression Not Receiving Bounces from Sync Send Path

**File**: `internal/api/campaign_builder_send_sync.go:279-281`
**Severity**: CRITICAL

When `sendViaSMTP` fails (bounce), the code increments `failed++` but does NOT:
- Record the bounce in `mailing_tracking_events`
- Call `globalHub.Suppress()` for the bounced email
- Update the inbox profile with bounce data

Only successful sends trigger tracking events. Failed sends are silently counted and lost.

Bounces only reach Global Suppression via the PMTA Ingestor webhook
(`internal/engine/ingest.go:routeToGlobalSuppression`) — but that requires PMTA to POST
accounting records to our webhook endpoint, which requires PMTA to be configured with our
webhook URL.

**Fix needed**:
1. On SMTP send failure, classify the error (hard bounce, soft bounce, connection error)
2. Call `globalHub.Suppress()` for hard bounces immediately
3. Record bounce events in `mailing_tracking_events`
4. Update inbox profiles with bounce data

---

## ISSUE 7: Unsubscribes Go to Legacy Table, Not Global Suppression Directly

**File**: `internal/api/mailing_tracking.go:207-211`
**Severity**: MEDIUM (partially mitigated)

`HandleTrackUnsubscribe` writes to `mailing_suppressions` (legacy table) but does NOT
directly call `globalHub.Suppress()`. The global suppression only receives unsubscribes
through the in-memory tracking event callback (`server_routes_mailing.go:650-651`).

This means:
- If the tracking event callback isn't wired (e.g., during startup race), unsubscribes
  are only in the legacy table
- The `HandleTrackUnsubscribe` handler should call `globalHub.Suppress()` directly as
  the primary path, not rely on the callback

**Fix needed**: Add direct `globalHub.Suppress()` call in `HandleTrackUnsubscribe`.

---

## ISSUE 8: Inbox Intel Is DB-Persisted BUT Profiles Are Not Created for All Sends

**File**: `campaign_builder_send_sync.go:258-262`
**Severity**: MEDIUM

Inbox profiles (`mailing_inbox_profiles`) ARE persisted in the database — this is good.
However, they are only created/updated during the SYNC send path. The tracking handlers
(opens, clicks) also update them (`mailing_tracking.go:65-69, 150-155`).

**Gap**: If sends happen through a different path (async worker, external SMTP), profiles
aren't created until the first open/click. There's no batch creation from the import pipeline.

**Fix needed**: The data import pipeline should bootstrap `mailing_inbox_profiles` entries
when importing subscribers (the domain and ISP are already known from the email).

---

## ISSUE 9: ISP Agent Intelligence Is Hydrated from Sends — But Only for Sync Sends

**File**: `campaign_builder_send_sync.go:265-273`
**Severity**: MEDIUM

`mailing_isp_agents` records are UPSERTED during the sync send loop, which is correct.
However, the UPSERT key is `(organization_id, domain)` — this means one agent per
recipient domain, NOT one per ISP group.

For gmail.com you get one agent, for googlemail.com you get another. The ISP Intelligence
dashboard expects agents grouped by ISP (gmail, yahoo, microsoft, etc.), not by individual
domain.

**Fix needed**: The ISP agent UPSERT should use the ISP group name (e.g., "gmail") as the
domain, or there should be an aggregation layer that rolls up per-domain agents into ISP groups.

---

## ISSUE 10: Analytics Center Has Revenue Section (Should Be Removed)

**File**: `web/src/components/mailing/components/AnalyticsCenter.tsx`
**Severity**: LOW

The Analytics Center has:
- Revenue KPI card (line 274-278)
- Revenue per email metric
- Revenue report section
- `fmtCurrency` formatting for revenue data

Per user directive: "We should remove the revenue section. We should only focus on
deliverability. Never intermix deliverability with revenue."

**Fix needed**: Remove all revenue-related UI elements from AnalyticsCenter.

---

## ISSUE 11: Analytics Center Shows SparkPost Daily Cap

**File**: `internal/api/mailing_handlers_full.go:292-298`
**Severity**: MEDIUM

The dashboard handler computes `dailyCapacity` from `mailing_sending_profiles` daily limits.
If no profiles have limits, it defaults to `500000` (SparkPost legacy default).

The user wants this replaced with PMTA pool-based capacity showing:
- Volume per pool (gmail-pool, yahoo-pool, etc.)
- Open rates per pool
- Delivery rates per pool

**Fix needed**: Replace SparkPost capacity with PMTA pool stats from `mailing_ip_addresses`
and `mailing_pmta_servers`.

---

## ISSUE 12: Content Library → PMTA Wizard Template Loading Works (Verified)

**File**: `web/src/components/mailing/components/PMTACampaignWizard.tsx:258-264, 482-487, 497-498`
**Severity**: NONE (Working correctly)

The "Load Template" button in Step 3 of the PMTA wizard:
1. Fetches `/api/mailing/templates` on step entry
2. Shows a template picker overlay
3. Populates subject, from_name, and html_content when selected

**Status**: This works correctly. User can create templates in Content Library and
select them during campaign creation.

---

## ISSUE 13: Audience Estimate Uses Static ISP Distribution, Not Real Data

**File**: `internal/api/handlers_pmta_campaign.go:427-457`
**Severity**: MEDIUM

`HandleEstimateAudience` uses hardcoded percentage distribution:
- Gmail: 30%, Yahoo: 15%, Microsoft: 20%, Apple: 10%, etc.

Instead of actually counting subscribers by domain group and mapping to ISPs.

For a 400k list, the ISP breakdown shown in the wizard is an approximation, not reality.

**Fix needed**: Query actual ISP distribution from subscribers using domain mapping or
the `domain_group` custom field from imports.

---

## ISSUE 14: Campaign Scheduling Exists But No Background Worker Picks It Up

**File**: `internal/api/campaign_builder_actions.go:40-44`
**Severity**: HIGH

`HandleScheduleCampaign` sets campaign status to `'scheduled'` with a `scheduled_at` timestamp.
But there is no background goroutine that polls for campaigns where
`status = 'scheduled' AND scheduled_at <= NOW()` and starts the send.

The worker exists at `internal/worker/campaign_scheduler.go` but it uses a different model
(async send worker) and may not be wired to the current campaign flow.

**Fix needed**: Wire a scheduler goroutine in `main.go` that polls for scheduled campaigns
and triggers the send process.

---

## ISSUE 15: Mission Control Shows Simulation Data When No Active Campaign

**File**: `web/src/components/mailing/components/MissionControl.tsx`
**Severity**: LOW

When no live campaign is running, Mission Control falls back to simulation data or shows
empty state. The user expects it to "display in realtime all of the given metrics" during an
active send.

The polling is already in place (every 1.5s), and the real-time event flow from
`onTrackingEvent` → `CampaignEventTracker` → Mission Control is wired. But it only works
during an active send when the in-memory tracker has data.

**Status**: Works correctly during active sends. Empty between sends is expected behavior.

---

## ISSUE 16: Consciousness Should Auto-Populate from Historical Campaign Data

**Severity**: MEDIUM

The Consciousness layer already:
- Restores persisted state from S3 on startup (added in previous session)
- Monitors campaigns every 30 seconds
- Generates philosophies from conviction stream

**Gap**: If no campaigns have been sent since server start and S3 state is empty,
Consciousness shows nothing. It should load historical campaign metrics from the database
and generate initial observations.

**Fix needed**: On startup, query `mailing_campaigns` for recent completed campaigns and
seed initial thoughts/philosophies from their metrics.

---

## ISSUE 17: Segments Are Not Auto-Created from Send Analysis

**Severity**: HIGH

The user expects: "if the system felt it needed to segment the data, then I should see the
segments appear within the list and Segments section."

Currently, segments are ONLY created manually via the UI or API. There is no automatic
segmentation from send performance (e.g., "Gmail High Engagers", "Yahoo Bounced", etc.).

**Fix needed**: Add a post-campaign analysis step that:
1. Examines per-ISP open/click/bounce rates
2. Creates smart segments (e.g., "High Engagement - Gmail", "Inactive 30d", "Bounced")
3. These segments should appear in the Lists & Segments section
4. Future campaigns can target these segments

---

## ISSUE 18: Data Import Already Processes Small Files First (Verified)

**File**: `internal/datanorm/normalizer.go:188-198`
**Severity**: NONE (Working correctly)

The `processQueue` function orders by `file_size ASC` when the `file_size` column exists
(which it does via migration 035). Small files are processed first.

**Status**: Working correctly. Last check showed 60 files completed, 430k records imported.

---

## ISSUE 19: Offers Navigation Should Be Hidden

**File**: `web/src/components/mailing/pages/MailingPortal.tsx:67`
**Severity**: LOW

Per user directive: "Let's hide the Offers navigation link for now."

**Fix needed**: Remove or hide the `offers` tab from the sidebar navigation.

---

## ISSUE 20: Global Suppression Dashboard May Show Misleading Data

**Severity**: MEDIUM

The `GlobalSuppressionHub.LoadFromDB()` loads from `mailing_global_suppressions` table.
The data import pipeline (`datanorm/importer.go:importSuppression`) writes to
`mailing_global_suppressions` (suppression-classified files from S3).

**Potential issue**: The import pipeline may be importing ALL suppression data files into
`mailing_global_suppressions`, which makes the hub think all those emails are suppressed.
If the suppression files are too aggressive (e.g., old data, invalid classifications),
it could be "suppressing everything" as the user reports.

**Investigation needed**: Check `data_import_log` for suppression-classified files and
their record counts. Compare `mailing_global_suppressions` count to actual bad email count.

---

## ISSUE 21: No Per-ISP Pool Routing in SMTP Send

**File**: `internal/api/mailing_handlers_full.go` (sendViaSMTP function)
**Severity**: HIGH

`sendViaSMTP` sends directly to the PMTA server on a single connection with no pool routing.
PMTA normally routes to different IP pools based on VMTA assignment in the email headers
(`X-PMTA-VirtualMTA` or envelope sender routing).

For proper ISP warmup, emails to gmail.com should route through `gmail-pool`,
yahoo.com through `yahoo-pool`, etc.

**Fix needed**: Add `X-PMTA-VirtualMTA` header based on recipient ISP group, or use
envelope sender routing that maps to PMTA pools.

---

## ISSUE 22: Tracking Events Not Being Recorded for Opens in DB Consistently

**File**: `internal/api/mailing_tracking.go:62-95`
**Severity**: MEDIUM

The `HandleTrackOpen` handler:
1. Fires in-memory tracker (line 63)
2. Inserts into `mailing_tracking_events` (line 67)
3. Updates `mailing_campaigns.open_count` (line 80)
4. Updates `mailing_inbox_profiles` (line 87)
5. Updates engagement score (line 94)
6. Updates ISP agent (line 95)

All of these are fire-and-forget DB calls. If the DB is slow (which it has been during
imports), some of these writes may silently fail. The open pixel still returns
immediately (which is correct), but the data is lost.

**Fix needed**: Consider queuing these writes (SQS or local buffer) so they survive
DB pressure. Or at minimum, log errors from these writes.

---

## ISSUE 23: Campaign Center Should Show Per-ISP Breakdown

**Severity**: MEDIUM

The Campaign Center (`CampaignPortal.tsx`) shows campaigns as a flat list with aggregate
metrics (sent, opened, clicked). The user expects to see per-ISP breakdowns.

The `mailing_tracking_events` table has the data needed (ISP can be derived from email
domain), but the campaign detail view doesn't query for per-ISP metrics.

**Fix needed**: Add per-ISP metrics to the campaign detail API endpoint.

---

## PRIORITY ORDER FOR FIXES

### Phase 1 — Must Fix Before First Production Send (Blocking)
1. **ISSUE 1**: Deploy must trigger send (or schedule)
2. **ISSUE 2**: getSubscribers must read from campaign_lists join table
3. **ISSUE 6**: Record bounces in tracking + suppress globally
4. **ISSUE 4**: Fix Servers tab endpoint URL (`pmta/servers` → `pmta-servers`)
5. **ISSUE 5**: Fix PMTA connectivity check to query `mailing_pmta_servers`
6. **ISSUE 21**: Add PMTA pool routing headers for ISP-specific delivery

### Phase 2 — Required for ISP-Aware Orchestration
7. **ISSUE 3**: ISP-based send segmentation and per-ISP sub-campaigns
8. **ISSUE 14**: Background scheduler for campaign sends
9. **ISSUE 7**: Direct global suppression from unsubscribe handler
10. **ISSUE 13**: Real ISP distribution in audience estimate
11. **ISSUE 11**: Replace SparkPost cap with PMTA pool capacity

### Phase 3 — Dashboard & Analytics Polish
12. **ISSUE 10**: Remove revenue section from Analytics
13. **ISSUE 19**: Hide Offers navigation
14. **ISSUE 23**: Per-ISP breakdown in Campaign Center
15. **ISSUE 9**: Fix ISP agent grouping (domain → ISP group)
16. **ISSUE 17**: Auto-create segments from send analysis
17. **ISSUE 16**: Seed Consciousness from historical data

### Phase 4 — Resilience & Scale
18. **ISSUE 8**: Bootstrap inbox profiles from import pipeline
19. **ISSUE 22**: Queue tracking writes for DB resilience
20. **ISSUE 20**: Audit global suppression data sources

---

## CURRENT SYSTEM STATE

- **Database**: PostgreSQL 16.10, RDS (burstable t3.micro — resource constrained)
- **PMTA Server**: 15.204.101.125 (OVH), port 587, confirmed working via direct SMTP
- **Import Progress**: 60/214 files completed, 430,700 records imported, actively processing
- **Subscriber Count**: ~396,675 in mailing_subscribers
- **Campaigns**: 3 existing (from test sends)
- **Global Suppressions**: Loaded from `mailing_global_suppressions` — count TBD
- **Tracking URL**: Set via GitHub Secrets (should be `https://projectjarvis.io`)
- **Sidebar Status**: Shows "PMTA Offline" (due to Issue 5)
- **Trigger Fix**: O(1) increment trigger applied — imports are 100x faster now
