# Analytics & Campaign Metrics — Feature Complete Fix Guide

## Problem Statement

The Analytics Center time range filters (1h, 24h, today) don't change the data. Campaign Manager has no ISP-level breakdown. The root causes:

1. **Backend ignores sub-day time params**: `HandleAnalyticsOverview` only reads `days` (integer). For 1h and 24h the frontend sends `days=1`, same as "today". Other endpoints (`deliverability`, `engagement`, `comparison`) ignore dates entirely.
2. **Campaign detail has no ISP breakdown**: The detail modal shows total counts only. An existing `HandleCampaignByDomain` endpoint exists but (a) returns 500 and (b) only counts opens/clicks, not sent/delivered/bounced.
3. **Frontend passes params inconsistently**: Some endpoints get `start_date`/`end_date`, others only get `days`.

## Data Sources

All metrics come from two tables:

- **`mailing_campaigns`**: Aggregate counters (sent_count, delivered_count, open_count, click_count, bounce_count, hard_bounce_count, soft_bounce_count, complaint_count). Updated by send workers and ingestor.
- **`mailing_tracking_events`**: Individual events (sent, delivered, opened, clicked, hard_bounce, soft_bounce, complained, unsubscribed). Partitioned by month. Has `campaign_id`, `email`, `event_type`, `event_at`.

For time-range analytics, `mailing_tracking_events` is the source of truth because it has per-event timestamps. For campaign-level totals, `mailing_campaigns` is authoritative.

---

## Fix Plan (ordered)

### 1. Backend: Unified date-range helper

Add a `parseDateRange(r *http.Request) (start, end time.Time)` helper that reads `start_date`, `end_date`, `range_type` from query params and returns a time range. This normalizes date handling across all analytics endpoints.

**File**: `mailing_analytics.go` (top)

**Test**: Call `/api/mailing/analytics/overview?start_date=2026-03-05T08:00:00Z&end_date=2026-03-05T09:00:00Z` and verify totals only include that hour.

### 2. Backend: Fix HandleAnalyticsOverview

Replace the `days` param with `parseDateRange`. Query both `mailing_campaigns` and `mailing_tracking_events` using the parsed range. The daily_trend query groups by hour for sub-day ranges and by day for multi-day ranges.

**File**: `mailing_analytics.go` lines 106-171

**Test**: 
- `?range_type=1h` → only events from last 60 minutes
- `?range_type=today` → only today's events
- `?range_type=7` → last 7 days

### 3. Backend: Fix HandleDeliverabilityReport

Add date filtering to the deliverability report. Use `parseDateRange` to scope `mailing_campaigns` and `mailing_tracking_events` queries.

**File**: `mailing_analytics.go` lines 432-498

**Test**: Compare 1h vs 7d — 1h should show much smaller totals.

### 4. Backend: Fix HandleCampaignComparison

Add date filtering to limit campaigns shown to those within the selected range.

**File**: `mailing_analytics.go` lines 176-238

**Test**: `?range_type=today` should only show campaigns created/started today.

### 5. Backend: Fix & enhance HandleCampaignByDomain

Fix the 500 error (likely a NULL email or scan issue). Enhance to include ALL event types per domain:
- sent, delivered, opened, clicked, hard_bounce, soft_bounce, complained

**File**: `mailing_analytics.go` lines 46-75

**Test**: `/api/mailing/analytics/campaigns/{id}/domains` returns JSON with domain breakdown including bounces and delivery counts.

### 6. Backend: Enhance HandleCampaignStats

Add ISP/domain breakdown and hourly timeline directly into the campaign stats response. The frontend already calls `GET /api/mailing/campaigns/{id}/stats`.

**File**: `campaign_builder_analytics.go`

**Test**: `/api/mailing/campaigns/{id}/stats` now includes `domain_breakdown` array and `hourly_timeline` array alongside the existing metrics.

### 7. Frontend: Fix AnalyticsCenter date params

Ensure ALL fetch calls pass `start_date` and `end_date` (not just `days`). For 1h: `start_date = now - 1h, end_date = now`. For 24h: `start_date = now - 24h`. For today: `start_date = today 00:00, end_date = now`.

**File**: `AnalyticsCenter.tsx`

**Test**: Click "1h" → data changes to show only last hour's events.

### 8. Frontend: CampaignsManager detail modal ISP breakdown

When opening a campaign detail, fetch the enhanced stats (which now includes `domain_breakdown`). Display a table with columns: Domain, Sent, Delivered, Opens, Clicks, Hard Bounces, Soft Bounces, Open Rate, Click Rate.

**File**: `CampaignsManager.tsx`

**Test**: Click "View Details" on campaign → see ISP breakdown table with real data.

---

## Verification Checklist

After deployment, verify against campaign `682b572a-9c5b-408f-a4ce-81a54aa3ff27`:

- [ ] Analytics "1h" shows data only from last 60 min (should be small or zero if no recent activity)
- [ ] Analytics "Today" shows today's sends (2,967 sent_count from this campaign)
- [ ] Analytics "7d" shows wider data including previous campaigns
- [ ] Campaign detail for 682b572a shows ISP breakdown with gmail, yahoo, hotmail, aol, etc.
- [ ] Domain breakdown shows sent count, delivered count, bounce counts per domain
- [ ] Deliverability report respects time range filter
- [ ] Numbers match: sum of domain sent counts ≈ campaign sent_count
