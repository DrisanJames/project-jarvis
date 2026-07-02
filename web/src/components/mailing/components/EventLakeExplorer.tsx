// Event Lake Explorer
//
// PAGE_VERSION 2.0 — decision-grade analytics over the S3/Athena email-event
// lake (internal/analytics/reader.go + internal/api/handlers_analytics_lake.go).
// Built for ISP management and campaign-scheduling decisions.
//
// Backend (all READ ONLY, called via apiFetch):
//   GET /api/mailing/analytics/lake/status
//       → { enabled_write, enabled_read, sent, failed, dropped }   (always works)
//   GET /api/mailing/analytics/lake/events?dt=&campaign_id=&isp_group=&event_type=&limit=
//       → { events: LakeEvent[] }  OR  { disabled: true, events: [] }   (limit 1–1000)
//   GET /api/mailing/analytics/lake/breakdown?from=YYYY-MM-DD&to=YYYY-MM-DD&group_by=d1,d2&limit=N
//       + optional equality filters: campaign_id, isp_group, event_type, brand,
//         email_domain, route_type, source, bounce_cat, vmta, pool, variant
//       group_by: 1–3 dims from {dt, event_type, isp_group, brand, email_domain,
//         route_type, source, bounce_cat, vmta, pool, suppression_reason, dsn_code,
//         variant, campaign_id}; limit clamps 1–5000 (default 1000)
//       → 200 { group_by, from, to, rows: [{keys, count}], truncated }
//       → 200 { disabled: true, rows: [] } when lake read is dark
//       → 400 { error } on validation failure (surfaced verbatim in toasts)
//       Counts are COUNT(DISTINCT event_uid) — accurate and dedup-safe.
//   GET /api/mailing/analytics/campaign-summary/{id}   (NOT under /lake)
//       → CampaignSummaryDetail — Campaign Center tracking-derived truth, used
//         by the Campaign Lookup tab for lake-vs-tracking reconciliation.
//
// Event vocabulary (event_type): attempted, delivered, relayed_to_ses,
// hard_bounce, soft_bounce, delivery_delay, complaint, open, click — plus
// possibly others; unknown types are rendered generically, never dropped.
//
// Rate conventions (every rate shows or tooltips its denominator):
//   delivery/hard/soft/complaint rate denominator = attempted when attempted>0,
//   else delivered+hard_bounce+soft_bounce (the label says which was used).
//   open_rate = open/delivered · click_rate = click/delivered · CTOR = click/open.
//   HARD RULE: hard bounce is ALWAYS red #ef4444, soft bounce ALWAYS amber
//   #f59e0b, and the two are NEVER summed into a combined "bounces" number.
//   relayed_to_ses is a PMTA→SES relay handoff, NOT a recipient delivery — it is
//   shown as a separate informational metric and never counted as delivered.
//
// The lake is DISABLED BY DEFAULT (ships dark): when enabled_read is false we
// render the status strip + a friendly dark card and issue NO queries.
//
// Athena queries cost ~1–3s each, so a module-level in-memory cache (keyed by
// the full query string + run nonce, FIFO-capped at 64 entries so stale-nonce
// keys age out instead of accumulating) serves tab flips and re-renders;
// explicit Run/Refresh actions bypass it. Raw /events queries are only ever
// issued by explicit button clicks, so they are not cached at all. Each panel
// shows fetch timing and a truncation banner when the server clamps the result.
//
// 2.0 review fixes: dimension pivot is keyed to the dim it was fetched for;
// Overview/Dimensions clear stale data and show inline errors (with retry) on
// fetch failure; campaign reconciliation skips empty lake windows, drops the
// un-reconcilable Complaints row, and omits Opens/Clicks for PMTA-direct mail
// (the lake's only open/click emit site is the SES events webhook); a 200 body
// without a `campaign` key is treated as a failed campaign-summary lookup;
// module caches are bounded.
//
// 2.1 (2026-06-09): Campaign Lookup gains a find-by-name picker backed by
// GET /api/mailing/analytics/campaign-summary?limit=200 (recent-activity
// order, rollup rows excluded) — no more hunting UUIDs; the raw UUID field
// stays as the escape hatch, and Recent chips show campaign names when the
// picker list knows them.

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faSyncAlt, faExclamationTriangle, faDatabase,
  faCircle, faSearch, faMoon, faInfoCircle, faTable, faLayerGroup,
  faChartLine, faChevronDown, faChevronRight,
  faCheckCircle, faTimesCircle, faBullseye, faHistory, faImages,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Area, Line, XAxis, YAxis,
  CartesianGrid, Tooltip,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';
import { useToast } from '../shared/ToastSystem';
import { colors as theme } from '../shared/theme';
// Unified filter layer (PORTAL_DESIGN_SYSTEM.md §3) — extracted FROM this
// screen's Toolbar; this file is the first consumer. Local aliases keep the
// original names so per-tab code reads unchanged.
import {
  FilterBar,
  denverToday, daysAgoDenver,
  lakeFilterParams as filterParams,
  lakeFilterParamsNoTransport as filterParamsNoTransport,
  COMMON_ISP_GROUPS,
} from '../shared/filters';
import type { Transport, LakeFilterDraft, AppliedLakeFilters } from '../shared/filters';

const PAGE_VERSION = '2.2';

// ═══════════════════════════════════════════════════════════════════════════
// TYPES (match backend JSON keys exactly)
// ═══════════════════════════════════════════════════════════════════════════

interface LakeStatus {
  enabled_write: boolean;
  enabled_read: boolean;
  sent: number;
  failed: number;
  dropped: number;
}

// Mirrors internal/analytics/lake_emitter.go Event struct json tags.
interface LakeEvent {
  event_uid: string;
  recipient_send_id: string;
  campaign_id: string;
  subscriber_id: string;
  email: string;
  email_domain: string;
  brand: string;
  isp_group: string;
  route_type: string;
  event_type: string;
  suppression_reason: string;
  vmta: string;
  pool: string;
  bounce_cat: string;
  dsn_code: string;
  dsn_diag: string;
  link_url: string;
  source_ip: string;
  variant: string;
  event_at: string;
  event_epoch_ms: number;
  ingested_at: string;
  source: string;
  dt: string;
}

interface EventsResponse {
  disabled?: boolean;
  events: LakeEvent[];
}

interface BreakdownRow {
  keys: Record<string, string>;
  count: number;
}

interface BreakdownResponse {
  disabled?: boolean;
  group_by?: string[];
  from?: string;
  to?: string;
  rows: BreakdownRow[];
  truncated?: boolean;
}

// Local re-declaration of the subset of CampaignSummaryDetail we render
// (do NOT import from CampaignPortal — see its lines 100-260 for the source).
interface CCDetailCampaign {
  id: string;
  name: string;
  status: string;
  route_type: string;
  scheduled_at?: string;
  created_at?: string;
  subject?: string;
  preheader?: string;
  from_name?: string;
  from_email?: string;
  campaign_type?: string;
  sending_profile?: string;
  sending_domain?: string;
  ip_pool?: string;
  targeted: number;
  sent: number;
  relayed_to_ses: number;
  delivered: number;
  hard_bounce: number;
  // v1.4 splits sender-side ISP blocks of VALID recipients (DSN 5.3/5.4/5.5/5.7
  // + policy/routing/connection) out of hard_bounce. The lake's ClassifyBounce
  // does NOT make this split — lake hard_bounce ≈ CC hard_bounce + reputation_block.
  reputation_block?: number;
  soft_bounce: number;
  // NOTE: the campaign-summary DETAIL endpoint has no complaints field (only
  // the list endpoint does) — do not add one here; it would always be undefined.
  unique_opens: number;
  total_opens: number;
  unique_clicks: number;
  total_clicks: number;
  delivery_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  open_rate: number;
  click_rate: number;
  ctor: number;
  metrics_source?: string;
  data_as_of?: string;
}

interface CCDetail {
  api_version?: string;
  // The handler can return HTTP 200 with {api_version, error} and NO campaign
  // key — consumers MUST treat a missing campaign object as a failed lookup.
  campaign?: CCDetailCampaign;
  denominators?: Record<string, string>;
  error?: string;
}

// Aggregated event counts after classifying event_type buckets.
interface EventCounts {
  attempted: number;
  delivered: number;
  relayed: number;       // relayed_to_ses — informational, NOT a delivery
  hard: number;          // hard_bounce — ALWAYS red, never summed with soft
  soft: number;          // soft_bounce — ALWAYS amber, never summed with hard
  delays: number;        // delivery_delay
  complaints: number;
  opens: number;
  clicks: number;
  bouncedUntyped: number; // ses-source 'bounced' (no hard/soft split)
  unsubs: number;
  other: number;         // unknown event types (rendered, never dropped)
  total: number;
}

// Computed rates with explicit denominator provenance.
// v2.2: 'attempted' is emitted by the SES pipe ONLY (PMTA emits no attempted
// event yet), so raw attempted can never be a blended denominator — it
// produced the infamous 229% delivery rate. The honest blended denominator is
// derived: delivered + hard + soft + untyped bounces (= attempt outcomes).
interface Rates {
  denom: number;
  denomLabel: 'attempted (derived: delivered+bounces)' | 'delivered+bounces';
  delivery: number | null;
  hard: number | null;
  soft: number | null;
  complaint: number | null;
  open: number | null;    // denominator: delivered
  click: number | null;   // denominator: delivered
  ctor: number | null;    // denominator: opens
}

interface MetricRow {
  key: string;
  counts: EventCounts;
  rates: Rates;
}

interface DailyPoint {
  day: string; // America/Denver operating day (populated from local_dt)
  attempted: number;
  delivered: number;
  opens: number;
  clicks: number;
  hardPct: number | null;
  softPct: number | null;
  compPct: number | null;
}

interface FetchMeta {
  fetchedAt: number;   // epoch ms
  durationMs: number;
  fromCache: boolean;
}

interface CachedPayload<T> {
  data: T;
  meta: FetchMeta;
}

// Filter types now live in shared/filters.tsx (Transport, RouteTypeFilter,
// LakeFilterDraft, AppliedLakeFilters). Aliased to the original local names to
// avoid renaming churn across the tabs.
type DraftFilters = LakeFilterDraft;
type AppliedFilters = AppliedLakeFilters;

interface RecentCampaign {
  id: string;
  at: string; // ISO timestamp of last lookup
}

type TabId = 'overview' | 'dimensions' | 'campaign' | 'raw' | 'creatives';

type SortDir = 'asc' | 'desc';
interface SortState { col: string; dir: SortDir }

// ═══════════════════════════════════════════════════════════════════════════
// STYLE TOKENS — mapped onto the shared indigo design system (shared/theme.ts)
// so this screen matches the Delivery Queue gold standard (same pattern as
// AudienceAnalytics / AudienceCadenceByCell). The local key names are kept
// (the styles object + per-tab code reference COLORS.*), but every value now
// resolves to a canonical indigo token. accent/accentAlt/good/warn/danger stay
// 6-digit hex because call sites append hex-alpha suffixes (COLORS.accent +
// '66'); border/borderStrong are full rgba values used directly. HARD_RED /
// SOFT_AMBER below remain separate per the hard bounce-color rule.
// ═══════════════════════════════════════════════════════════════════════════

const COLORS = {
  bgDeep:        theme.appBgSolid,          // #0a0e1c
  bgPanel:       theme.panelBgSolid,        // #0f1629
  bgPanelAlt:    '#131a2e',                 // slightly lifted indigo-slate surface
  border:        theme.hairline,            // rgba(99,102,241,0.15)
  borderStrong:  theme.panelBorderStrong,   // rgba(99,102,241,0.30)
  textPrimary:   theme.text,                // #e5e7eb
  textSecondary: theme.textMuted,           // #94a3b8
  textMuted:     theme.textFaint,           // #64748b
  accent:        theme.indigo400,           // #818cf8
  accentAlt:     theme.indigo300,           // #a5b4fc
  accentPink:    theme.indigo200,           // #c7d2fe (was pink → indigo light)
  good:          theme.success,             // #22c55e
  warn:          theme.warning,             // #f59e0b
  danger:        theme.danger,              // #ef4444
};

// HARD RULE colors (repo CLAUDE.md): hard bounce red, soft bounce amber. Always.
const HARD_RED = '#ef4444';
const SOFT_AMBER = '#f59e0b';
const INFO_BLUE = '#60a5fa';
const OPEN_CYAN = '#22d3ee';
const CLICK_VIOLET = '#a78bfa';
const COMPLAINT_ROSE = '#fb7185';

// ═══════════════════════════════════════════════════════════════════════════
// API LAYER — module-level cache + abortable fetch helpers
// ═══════════════════════════════════════════════════════════════════════════

// Bounded FIFO cache: Map preserves insertion order, so evicting from the
// front drops the oldest entries — including every prior-nonce key once an
// explicit Run bumps the nonce and makes them unreachable. Entries can hold up
// to 5000 rows each, so an unbounded map would grow with every Run forever.
const CACHE_MAX_ENTRIES = 64;
const breakdownCache = new Map<string, CachedPayload<BreakdownResponse>>();

function breakdownCacheSet(key: string, payload: CachedPayload<BreakdownResponse>): void {
  breakdownCache.set(key, payload);
  while (breakdownCache.size > CACHE_MAX_ENTRIES) {
    const oldest = breakdownCache.keys().next().value;
    if (oldest === undefined) break;
    breakdownCache.delete(oldest);
  }
}

interface BreakdownParams {
  from: string;
  to: string;
  groupBy: string[];
  limit?: number;
  filters?: Record<string, string>;
}

function breakdownURL(p: BreakdownParams): string {
  const qs = new URLSearchParams();
  qs.set('from', p.from);
  qs.set('to', p.to);
  qs.set('group_by', p.groupBy.join(','));
  qs.set('limit', String(p.limit ?? 5000));
  if (p.filters) {
    // Stable key order so the cache key is deterministic.
    for (const k of Object.keys(p.filters).sort()) {
      const v = (p.filters[k] || '').trim();
      if (v) qs.set(k, v);
    }
  }
  return `/api/mailing/analytics/lake/breakdown?${qs.toString()}`;
}

// Parse a non-OK response: 400s carry {error} which we surface verbatim.
async function throwHttpError(res: Response): Promise<never> {
  let msg = `HTTP ${res.status}`;
  try {
    const body: { error?: string } = await res.json();
    if (body && typeof body.error === 'string' && body.error) msg = body.error;
  } catch { /* non-JSON error body — keep HTTP status message */ }
  throw new Error(msg);
}

async function fetchBreakdown(
  params: BreakdownParams,
  nonce: number,
  opts?: { signal?: AbortSignal; bypass?: boolean }
): Promise<CachedPayload<BreakdownResponse>> {
  const url = breakdownURL(params);
  const key = `${url}#n${nonce}`;
  if (!opts?.bypass) {
    const hit = breakdownCache.get(key);
    if (hit) return { data: hit.data, meta: { ...hit.meta, fromCache: true } };
  }
  const t0 = performance.now();
  const res = await apiFetch(url, { signal: opts?.signal });
  if (!res.ok) await throwHttpError(res);
  const data: BreakdownResponse = await res.json();
  const payload: CachedPayload<BreakdownResponse> = {
    data: { ...data, rows: Array.isArray(data.rows) ? data.rows : [] },
    meta: { fetchedAt: Date.now(), durationMs: Math.round(performance.now() - t0), fromCache: false },
  };
  breakdownCacheSet(key, payload);
  return payload;
}

// ── Engagement summary (HUMAN opens/clicks from PG + verdict) ────────────────
// The Range Overview KPI strip sources its open/click tiles from THIS endpoint,
// not the lake breakdown. Internal-tracking opens/clicks live only in Postgres
// (mailing_tracking_events); the lake carries only SES-webhook engagement and
// its is_machine_* columns are inert, so the lake read reported ~3 clicks for a
// day that actually had hundreds of human clicks. The backend classifies humans
// with ignite_event_verdict() and excludes asset-CDN link rows.
// See internal/api/handlers_analytics_engagement.go.
interface EngSummary {
  raw_opens: number;
  human_opens: number;
  human_openers: number;
  raw_clicks: number;
  human_clicks: number;
  human_clickers: number;
}

// Honors the toolbar brand (sending-domain) filter; transport/ISP are not
// applied (engagement is not a transport property). Not cached — it is one cheap
// aggregate per Run, and staleness here would silently misreport conversions.
async function fetchEngagement(
  from: string,
  to: string,
  brand: string,
  signal?: AbortSignal
): Promise<EngSummary> {
  const qs = new URLSearchParams({ from, to });
  if (brand.trim()) qs.set('brand', brand.trim());
  const res = await apiFetch(`/api/mailing/analytics/engagement?${qs.toString()}`, { signal });
  if (!res.ok) await throwHttpError(res);
  return res.json();
}

// ── Deferral funnel (deferred → recovered / pending / bounced, per ISP) ──────
// Tells the lifecycle of throttle-deferred messages, NOT the raw retry-event
// count (the "Deferral Retry Events" KPI is per-RETRY and one message emits
// dozens). FAIL-SOFT like fetchEngagement: a failure degrades the readout +
// the two ISP columns to "—" without breaking the screen.
interface DeferralFunnel {
  total: { deferred: number; recovered: number; pending: number; bounced: number };
  rows: { isp_group: string; deferred: number; recovered: number; pending: number; bounced: number }[];
}

// Honors the toolbar brand (sending-domain) filter (same qs idiom as
// fetchEngagement). Not cached — one cheap aggregate per Run.
async function fetchDeferralFunnel(
  from: string,
  to: string,
  brand: string,
  signal?: AbortSignal
): Promise<DeferralFunnel> {
  const qs = new URLSearchParams({ from, to });
  if (brand.trim()) qs.set('brand', brand.trim());
  const res = await apiFetch(`/api/mailing/analytics/deferral-funnel?${qs.toString()}`, { signal });
  if (!res.ok) await throwHttpError(res);
  return res.json();
}

interface EventsParams {
  dt?: string;
  campaign_id?: string;
  isp_group?: string;
  event_type?: string;
  limit: number;
}

// NOT cached: every /events query is an explicit button click (Raw Events
// "Query", Campaign Lookup), so a cache could never serve a hit — the previous
// write-only eventsCache was removed rather than given a read path.
async function fetchLakeEvents(
  params: EventsParams,
  opts?: { signal?: AbortSignal }
): Promise<CachedPayload<EventsResponse>> {
  const qs = new URLSearchParams();
  if (params.dt) qs.set('dt', params.dt);
  if (params.campaign_id?.trim()) qs.set('campaign_id', params.campaign_id.trim());
  if (params.isp_group?.trim()) qs.set('isp_group', params.isp_group.trim());
  if (params.event_type?.trim()) qs.set('event_type', params.event_type.trim());
  qs.set('limit', String(params.limit));
  const url = `/api/mailing/analytics/lake/events?${qs.toString()}`;
  const t0 = performance.now();
  const res = await apiFetch(url, { signal: opts?.signal });
  if (!res.ok) await throwHttpError(res);
  const data: EventsResponse = await res.json();
  return {
    data: { ...data, events: Array.isArray(data.events) ? data.events : [] },
    meta: { fetchedAt: Date.now(), durationMs: Math.round(performance.now() - t0), fromCache: false },
  };
}

async function fetchCampaignSummary(
  id: string,
  signal?: AbortSignal
): Promise<CCDetail> {
  const res = await apiFetch(`/api/mailing/analytics/campaign-summary/${encodeURIComponent(id)}`, { signal });
  if (!res.ok) await throwHttpError(res);
  return (await res.json()) as CCDetail;
}

const isAbortError = (e: unknown): boolean =>
  e instanceof DOMException ? e.name === 'AbortError'
    : e instanceof Error ? e.name === 'AbortError'
    : false;

// ═══════════════════════════════════════════════════════════════════════════
// HELPERS — formatting, colors, classification, rates
// ═══════════════════════════════════════════════════════════════════════════

const fmt = (n: number) => (n ?? 0).toLocaleString('en-US');
const fmtPct = (v: number | null, digits = 2): string => (v == null ? '—' : `${v.toFixed(digits)}%`);
const fmtCompact = (n: number) => Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 }).format(n);
const fmtClock = (t: number) => new Date(t).toLocaleTimeString('en-US', { hour12: false });

// UTC day helper — addresses the PHYSICAL UTC partition (the Raw Events tab's
// dt= filter targets the lake's UTC partition column). Do NOT use it for
// operating-day reporting; use the Denver helpers below for that. (The former
// daysAgoUTC helper was removed once the Denver sweep left it with no callers —
// the Raw tab only ever used todayUTC() for its max= bound.)
const todayUTC = () => new Date().toISOString().slice(0, 10);

// Operating-day helpers (denverToday / daysAgoDenver) are imported from
// shared/filters.tsx — the one place the Denver date math lives.

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const isUUID = (s: string) => UUID_RE.test(s.trim());

const truncate = (s: string, max: number): string => {
  if (!s) return '';
  return s.length > max ? s.slice(0, max - 1) + '…' : s;
};

const fmtEventAt = (e: LakeEvent): string => {
  if (e.event_epoch_ms && e.event_epoch_ms > 0) {
    return new Date(e.event_epoch_ms).toLocaleString('en-US', { hour12: false, dateStyle: 'short', timeStyle: 'medium' });
  }
  if (e.event_at) return e.event_at;
  return '—';
};

// Exact event_type → color map (hard rule colors), with heuristic fallback
// for unknown types so nothing is ever dropped or rendered colorless.
const EVENT_TYPE_COLORS: Record<string, string> = {
  attempted: COLORS.accent,
  delivered: COLORS.good,
  relayed_to_ses: INFO_BLUE,
  hard_bounce: HARD_RED,
  soft_bounce: SOFT_AMBER,
  delivery_delay: COLORS.warn,
  complaint: COMPLAINT_ROSE,
  open: OPEN_CYAN,
  click: CLICK_VIOLET,
};

const eventTypeColor = (t: string): string => {
  const k = (t || '').toLowerCase();
  if (EVENT_TYPE_COLORS[k]) return EVENT_TYPE_COLORS[k];
  if (k.includes('hard') || k.includes('fail') || k.includes('drop')) return HARD_RED;
  if (k.includes('soft') || k.includes('defer') || k.includes('suppress')) return SOFT_AMBER;
  if (k.includes('bounce')) return HARD_RED;
  if (k.includes('open') || k.includes('click') || k.includes('deliver') || k.includes('sent')) return COLORS.good;
  return COLORS.accent;
};

const KNOWN_TYPES = new Set([
  'attempted', 'delivered', 'relayed_to_ses', 'hard_bounce', 'soft_bounce',
  'delivery_delay', 'complaint', 'open', 'click', 'bounced', 'unsubscribe',
]);

function countsFromTypeMap(m: Record<string, number>): EventCounts {
  const g = (k: string) => m[k] || 0;
  let other = 0;
  let total = 0;
  for (const [k, v] of Object.entries(m)) {
    total += v;
    if (!KNOWN_TYPES.has(k)) other += v;
  }
  return {
    attempted: g('attempted'),
    delivered: g('delivered'),
    relayed: g('relayed_to_ses'),
    hard: g('hard_bounce'),
    soft: g('soft_bounce'),
    delays: g('delivery_delay'),
    complaints: g('complaint'),
    opens: g('open'),
    clicks: g('click'),
    bouncedUntyped: g('bounced'),
    unsubs: g('unsubscribe'),
    other,
    total,
  };
}

// Rate convention (v2.2): denominator = DERIVED attempted =
// delivered + hard + soft + untyped bounces. Raw 'attempted' events exist
// only on the SES pipe (PMTA emits none), so using them blended produced
// >200% delivery rates. Every attempt has exactly one terminal outcome, so
// outcomes ARE attempts — per route AND blended.
function computeRates(c: EventCounts): Rates {
  const denom = c.delivered + c.hard + c.soft + c.bouncedUntyped;
  const denomLabel: Rates['denomLabel'] = 'attempted (derived: delivered+bounces)';
  const r = (n: number): number | null => (denom > 0 ? (n / denom) * 100 : null);
  return {
    denom,
    denomLabel,
    delivery: r(c.delivered),
    hard: r(c.hard),
    soft: r(c.soft),
    complaint: r(c.complaints),
    open: c.delivered > 0 ? (c.opens / c.delivered) * 100 : null,
    click: c.delivered > 0 ? (c.clicks / c.delivered) * 100 : null,
    ctor: c.opens > 0 ? (c.clicks / c.opens) * 100 : null,
  };
}

function makeMetricRow(key: string, typeMap: Record<string, number>): MetricRow {
  const counts = countsFromTypeMap(typeMap);
  return { key, counts, rates: computeRates(counts) };
}

const denomTitle = (r: Rates): string => `denominator: ${r.denomLabel} (${fmt(r.denom)})`;
const deliveredTitle = (c: EventCounts): string => `denominator: delivered (${fmt(c.delivered)})`;

// Heat tints — subtle background washes flagging out-of-band rates.
const heatHard = (p: number | null): string | undefined =>
  p == null ? undefined : p > 1 ? 'rgba(239,68,68,0.14)' : p > 0.5 ? 'rgba(245,158,11,0.11)' : undefined;
const heatComp = (p: number | null): string | undefined =>
  p == null ? undefined : p > 0.1 ? 'rgba(239,68,68,0.14)' : p > 0.05 ? 'rgba(245,158,11,0.11)' : undefined;
const heatDel = (p: number | null): string | undefined =>
  p == null ? undefined : p < 90 ? 'rgba(239,68,68,0.14)' : p < 97 ? 'rgba(245,158,11,0.11)' : undefined;

// ═══════════════════════════════════════════════════════════════════════════
// PIVOT — breakdown rows → metric rows / daily + hourly series
// ═══════════════════════════════════════════════════════════════════════════

// Pivot group_by=<dim>,event_type rows into one MetricRow per dim value + totals.
function pivotByDim(rows: BreakdownRow[], dim: string): { rows: MetricRow[]; totals: MetricRow } {
  const byKey = new Map<string, Record<string, number>>();
  const totalMap: Record<string, number> = {};
  for (const r of rows) {
    const k = r.keys[dim] ?? '';
    const et = (r.keys['event_type'] ?? '').toLowerCase();
    let m = byKey.get(k);
    if (!m) { m = {}; byKey.set(k, m); }
    m[et] = (m[et] || 0) + r.count;
    totalMap[et] = (totalMap[et] || 0) + r.count;
  }
  const out: MetricRow[] = [];
  byKey.forEach((m, k) => out.push(makeMetricRow(k, m)));
  return { rows: out, totals: makeMetricRow('TOTAL', totalMap) };
}

// Pivot group_by=local_dt,event_type rows into a sorted daily series for
// charting. Days are America/Denver (v2.2); falls back to the UTC dt key for
// older callers.
function dailySeries(rows: BreakdownRow[]): DailyPoint[] {
  const byDt = new Map<string, Record<string, number>>();
  for (const r of rows) {
    const dtKey = r.keys['local_dt'] ?? r.keys['dt'] ?? '';
    const et = (r.keys['event_type'] ?? '').toLowerCase();
    let m = byDt.get(dtKey);
    if (!m) { m = {}; byDt.set(dtKey, m); }
    m[et] = (m[et] || 0) + r.count;
  }
  const dts = Array.from(byDt.keys()).sort();
  return dts.map((dtKey) => {
    const c = countsFromTypeMap(byDt.get(dtKey) || {});
    const r = computeRates(c);
    return {
      day: dtKey,
      attempted: r.denom, // derived (delivered+bounces) — raw attempted is SES-only
      delivered: c.delivered,
      opens: c.opens,
      clicks: c.clicks,
      hardPct: r.hard,
      softPct: r.soft,
      compPct: r.complaint,
    };
  });
}

// Pivot group_by=local_hour,event_type rows into a sorted hourly series for
// charting. Same shape as dailySeries but bucketed on the local_hour key
// ('YYYY-MM-DD HH:00', already America/Denver). Sorted ascending.
function hourlySeries(rows: BreakdownRow[]): DailyPoint[] {
  const byHr = new Map<string, Record<string, number>>();
  for (const r of rows) {
    const hrKey = r.keys['local_hour'] ?? '';
    const et = (r.keys['event_type'] ?? '').toLowerCase();
    let m = byHr.get(hrKey);
    if (!m) { m = {}; byHr.set(hrKey, m); }
    m[et] = (m[et] || 0) + r.count;
  }
  const hrs = Array.from(byHr.keys()).sort();
  return hrs.map((hrKey) => {
    const c = countsFromTypeMap(byHr.get(hrKey) || {});
    const r = computeRates(c);
    return {
      day: hrKey,
      attempted: r.denom, // derived (delivered+bounces) — raw attempted is SES-only
      delivered: c.delivered,
      opens: c.opens,
      clicks: c.clicks,
      hardPct: r.hard,
      softPct: r.soft,
      compPct: r.complaint,
    };
  });
}

// Sum the per-dt series back into overall counts (Overview KPI strip).
function totalsFromBreakdown(rows: BreakdownRow[]): MetricRow {
  const m: Record<string, number> = {};
  for (const r of rows) {
    const et = (r.keys['event_type'] ?? '').toLowerCase();
    m[et] = (m[et] || 0) + r.count;
  }
  return makeMetricRow('TOTAL', m);
}

// mergeDeliveryAndEngagement combines a DELIVERY breakdown (source pmta+ses)
// with an ENGAGEMENT breakdown (source='app', the PG-tracking mirror) into one
// row set with each event counted exactly once:
//   - open/click are taken ONLY from the app stream. The delivery rows' own
//     open/click slice (the SES webhook's) is STRIPPED first — SES opens are
//     persisted to PG and mirrored into the app stream, so keeping both counted
//     every SES open twice (verified 2026-07-01: campaign 4d3bd63f had open=616
//     under source=ses AND open=616 under source=app for the same sends).
//   - everything else (delivered/bounces/delays/complaints/…) comes ONLY from
//     the delivery rows; the app stream's delivered/attempted/bounce rows are
//     PG-mirror duplicates and are dropped.
function mergeDeliveryAndEngagement(deliveryRows: BreakdownRow[], engRows: BreakdownRow[]): BreakdownRow[] {
  const isEng = (r: BreakdownRow) => {
    const et = (r.keys['event_type'] ?? '').toLowerCase();
    return et === 'open' || et === 'click';
  };
  return [...deliveryRows.filter((r) => !isEng(r)), ...engRows.filter(isEng)];
}

// transportSources maps the toolbar Transport selector to the lake `source`
// values it covers. combined = pmta + ses (the headline excludes the duplicate
// 'app' source by construction); mta = pmta; ses = ses.
function transportSources(t: Transport): Set<string> {
  if (t === 'mta') return new Set(['pmta']);
  if (t === 'ses') return new Set(['ses']);
  return new Set(['pmta', 'ses']);
}

// routeRowsForTransport projects the route-funnel breakdown (local_dt × source ×
// route_type × event_type) down to the transport-filtered (local_dt, event_type)
// rows that drive the headline KPIs + trend. SINGLE SOURCE OF TRUTH: the
// headline and the route funnel are now derived from the SAME query, so they can
// never disagree. (They used to be two separate breakdown fetches with separate
// cache keys — the headline could serve a stale "cached" value while the funnel
// was fresh, so Delivered differed by thousands on the same screen.)
function routeRowsForTransport(rows: BreakdownRow[], transport: Transport): BreakdownRow[] {
  const srcs = transportSources(transport);
  return rows
    .filter((r) => srcs.has(r.keys['source'] ?? ''))
    .map((r) => ({
      keys: { local_dt: r.keys['local_dt'] ?? '', event_type: r.keys['event_type'] ?? '' },
      count: r.count,
    }));
}

// ═══════════════════════════════════════════════════════════════════════════
// HELPERS — recent-campaign localStorage + filter plumbing
// ═══════════════════════════════════════════════════════════════════════════

const RECENT_KEY = 'event-lake-recent-campaigns';

function loadRecentCampaigns(): RecentCampaign[] {
  try {
    const raw = localStorage.getItem(RECENT_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed
      .filter((x): x is RecentCampaign =>
        !!x && typeof x === 'object' &&
        typeof (x as RecentCampaign).id === 'string' &&
        typeof (x as RecentCampaign).at === 'string')
      .slice(0, 8);
  } catch {
    return [];
  }
}

function saveRecentCampaign(id: string): RecentCampaign[] {
  const next = [{ id, at: new Date().toISOString() },
    ...loadRecentCampaigns().filter((r) => r.id !== id)].slice(0, 8);
  try { localStorage.setItem(RECENT_KEY, JSON.stringify(next)); } catch { /* quota — ignore */ }
  return next;
}

// Filter → query-param mapping (filterParams / filterParamsNoTransport) is
// imported from shared/filters.tsx (lakeFilterParams / …NoTransport) — the
// canonical brand/isp_group/route_type/source contract lives there. The
// transport-aware variant keeps the cache key (built from the serialized URL)
// correct automatically; the no-transport variant serves the RouteFunnelPanel
// companion query, which must always read both transports.

// Dims the app engagement stream CANNOT attribute: app open/click rows carry
// source='app' and no sending route/vmta/pool, so grouping engagement by any
// of these keys every open into one "(empty)" (or 'app') bucket while the real
// rows read 0 — misleading, not informative. For these dims the matrix and the
// row expansions show DELIVERY ONLY and skip the engagement merge entirely
// (QA gate finding, 2026-07-01: expanding the ses row under Rows=Source
// re-introduced the SES-webhook double count the merge exists to prevent).
const SENDING_SIDE_DIMS = new Set(['source', 'route_type', 'vmta', 'pool']);

const ROW_DIMS: Array<{ id: string; label: string }> = [
  { id: 'isp', label: 'Mailbox Provider' },          // clean — from real recipient domain (truthful)
  { id: 'isp_group', label: 'Mailbox Provider (raw)' }, // raw stored field — carries PMTA *.queue noise
  { id: 'brand', label: 'Brand' },
  { id: 'email_domain', label: 'Email Domain' },
  { id: 'route_type', label: 'Sending Route' },
  { id: 'source', label: 'Source' },
  { id: 'vmta', label: 'Sending Server' },
  { id: 'pool', label: 'Pool' },
  { id: 'variant', label: 'Variant' },
];

// ═══════════════════════════════════════════════════════════════════════════
// SMALL SHARED COMPONENTS
// ═══════════════════════════════════════════════════════════════════════════

const EnableBadge: React.FC<{ label: string; enabled: boolean }> = ({ label, enabled }) => (
  <div style={{
    ...styles.badge,
    color: enabled ? COLORS.good : COLORS.textMuted,
    borderColor: enabled ? COLORS.good + '55' : COLORS.borderStrong,
    background: enabled ? COLORS.good + '12' : 'transparent',
  }}>
    <FontAwesomeIcon icon={faCircle} style={{ fontSize: 8 }} />
    {label}: {enabled ? 'on' : 'off'}
  </div>
);

const Counter: React.FC<{ label: string; value: number; color: string }> = ({ label, value, color }) => (
  <div style={styles.counter}>
    <div style={{ ...styles.counterValue, color }}>{fmt(value)}</div>
    <div style={styles.counterLabel}>{label}</div>
  </div>
);

const LoadingRow: React.FC<{ label?: string }> = ({ label }) => (
  <div style={styles.sectionLoading}>
    <FontAwesomeIcon icon={faSpinner} spin /> {label || 'Querying analytics database…'}
  </div>
);

const EmptyRow: React.FC<{ label: string }> = ({ label }) => (
  <div style={styles.sectionEmpty}>{label}</div>
);

// "fetched HH:MM:SS · Nms (· cached)" per-panel timing stamp.
const TimingNote: React.FC<{ meta: FetchMeta | null }> = ({ meta }) => {
  if (!meta) return null;
  return (
    <span style={styles.timingNote}>
      fetched {fmtClock(meta.fetchedAt)} · {fmt(meta.durationMs)}ms{meta.fromCache ? ' · cached' : ''}
    </span>
  );
};

const TruncationBanner: React.FC<{ truncated?: boolean; limit: number }> = ({ truncated, limit }) => {
  if (!truncated) return null;
  return (
    <div style={styles.truncBanner}>
      <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
      Result truncated at limit {fmt(limit)} — totals below are an undercount. Narrow the date range or add filters.
    </div>
  );
};

// The removable filter chip lives in shared/filters.tsx (FilterChip).

// Dark-theme recharts tooltip.
interface ChartTipProps {
  active?: boolean;
  label?: string | number;
  payload?: Array<{ name?: string; value?: number | string; color?: string }>;
}
const ChartTip: React.FC<ChartTipProps> = ({ active, label, payload }) => {
  if (!active || !payload || payload.length === 0) return null;
  return (
    <div style={styles.chartTip}>
      <div style={styles.chartTipTitle}>{String(label ?? '')}</div>
      {payload.map((p, i) => (
        <div key={i} style={styles.chartTipRow}>
          <span style={{ ...styles.chartTipDot, background: p.color || COLORS.accent }} />
          <span style={{ color: COLORS.textSecondary }}>{p.name}</span>
          <span style={{ marginLeft: 'auto', fontVariantNumeric: 'tabular-nums', color: COLORS.textPrimary }}>
            {typeof p.value === 'number'
              ? (String(p.name || '').includes('%') ? `${p.value.toFixed(3)}%` : fmt(p.value))
              : String(p.value ?? '—')}
          </span>
        </div>
      ))}
    </div>
  );
};

// KPI card: big count + optional rate with its denominator note.
const KpiCard: React.FC<{
  label: string;
  value: number;
  color: string;
  rate?: number | null;
  rateLabel?: string;
  denomNote?: string;
  extra?: string;
}> = ({ label, value, color, rate, rateLabel, denomNote, extra }) => (
  <div style={styles.kpiCard}>
    <div style={styles.kpiLabel}>{label}</div>
    <div style={{ ...styles.kpiValue, color }}>{fmt(value)}</div>
    {rate !== undefined && (
      <div style={{ ...styles.kpiRate, color }} title={denomNote}>
        {rateLabel || 'rate'}: {fmtPct(rate)}
      </div>
    )}
    {denomNote && <div style={styles.kpiDenom}>{denomNote}</div>}
    {extra && <div style={styles.kpiDenom}>{extra}</div>}
  </div>
);

// ─── Daily trend chart (Overview + row expansions) ─────────────────────────

interface TrendSeriesDef {
  id: string;
  label: string;
  kind: 'bar' | 'line';
  axis: 'left' | 'right';
  color: string;
}

const TREND_SERIES: TrendSeriesDef[] = [
  { id: 'delivered', label: 'delivered', kind: 'bar', axis: 'left', color: COLORS.good },
  { id: 'attempted', label: 'attempted', kind: 'bar', axis: 'left', color: COLORS.accent },
  { id: 'opens', label: 'opens', kind: 'bar', axis: 'left', color: OPEN_CYAN },
  { id: 'clicks', label: 'clicks', kind: 'bar', axis: 'left', color: CLICK_VIOLET },
  { id: 'hardPct', label: 'hard %', kind: 'line', axis: 'right', color: HARD_RED },
  { id: 'softPct', label: 'soft %', kind: 'line', axis: 'right', color: SOFT_AMBER },
  { id: 'compPct', label: 'complaint %', kind: 'line', axis: 'right', color: COMPLAINT_ROSE },
];

// Delivery-Queue gold-standard style: volume series render as smooth gradient
// AREAS (matching OutboxDashboard's Throughput chart), rate series stay as
// monotone lines on the right axis. One gradient <def> per volume-series color.
const TrendChart: React.FC<{
  data: DailyPoint[];
  visible: Set<string>;
  height: number;
}> = ({ data, visible, height }) => {
  const volSeries = TREND_SERIES.filter((s) => s.kind === 'bar' && visible.has(s.id));
  return (
  <ResponsiveContainer width="100%" height={height}>
    <ComposedChart data={data} margin={{ top: 8, right: 16, bottom: 0, left: 0 }}>
      <defs>
        {volSeries.map((s) => (
          <linearGradient key={s.id} id={`lakeTrendFill-${s.id}`} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={s.color} stopOpacity={0.5} />
            <stop offset="100%" stopColor={s.color} stopOpacity={0.04} />
          </linearGradient>
        ))}
      </defs>
      <CartesianGrid stroke="rgba(148,163,184,0.12)" vertical={false} />
      <XAxis
        dataKey="day"
        tickFormatter={(v: string) => (typeof v === 'string' ? v.slice(5) : String(v))}
        tick={{ fill: '#94a3b8', fontSize: 11 }}
        interval="preserveStartEnd"
        minTickGap={40}
        axisLine={{ stroke: COLORS.borderStrong }}
        tickLine={false}
      />
      <YAxis
        yAxisId="left"
        tickFormatter={(v: number) => fmtCompact(v)}
        tick={{ fill: '#94a3b8', fontSize: 11 }}
        axisLine={false}
        tickLine={false}
        width={52}
      />
      <YAxis
        yAxisId="right"
        orientation="right"
        tickFormatter={(v: number) => `${v}%`}
        tick={{ fill: '#94a3b8', fontSize: 11 }}
        axisLine={false}
        tickLine={false}
        width={44}
      />
      <Tooltip content={<ChartTip />} cursor={{ stroke: 'rgba(148,163,184,0.3)', strokeWidth: 1 }} />
      {volSeries.map((s) => (
        <Area key={s.id} yAxisId="left" dataKey={s.id} name={s.label} type="monotone"
          stroke={s.color} strokeWidth={2} fill={`url(#lakeTrendFill-${s.id})`}
          dot={false} activeDot={{ r: 3, fill: s.color, strokeWidth: 0 }} connectNulls />
      ))}
      {TREND_SERIES.filter((s) => s.kind === 'line' && visible.has(s.id)).map((s) => (
        <Line key={s.id} yAxisId="right" dataKey={s.id} name={s.label} stroke={s.color}
          strokeWidth={2} dot={{ r: 2, fill: s.color, strokeWidth: 0 }} connectNulls type="monotone" />
      ))}
    </ComposedChart>
  </ResponsiveContainer>
  );
};

// Series-visibility toggle chips for the trend chart.
const SeriesToggles: React.FC<{
  visible: Set<string>;
  onToggle: (id: string) => void;
}> = ({ visible, onToggle }) => (
  <div style={styles.seriesToggleRow}>
    {TREND_SERIES.map((s) => {
      const on = visible.has(s.id);
      return (
        <button
          key={s.id}
          onClick={() => onToggle(s.id)}
          style={{
            ...styles.seriesToggle,
            color: on ? s.color : COLORS.textMuted,
            borderColor: on ? s.color + '66' : COLORS.borderStrong,
            background: on ? s.color + '14' : 'transparent',
          }}
        >
          <FontAwesomeIcon icon={faCircle} style={{ fontSize: 7 }} />
          {s.label}
        </button>
      );
    })}
  </div>
);

// ─── Metrics pivot table (Dimensions tab + Campaign per-ISP matrix) ─────────

interface ColDef {
  id: string;
  label: string;
  value: (r: MetricRow) => number | null;
  render: (r: MetricRow) => React.ReactNode;
  tint?: (r: MetricRow) => string | undefined;
  title?: (r: MetricRow) => string | undefined;
}

const numCell = (n: number, color?: string): React.ReactNode => (
  <span style={{ color: color || COLORS.textPrimary, fontVariantNumeric: 'tabular-nums' }}>{fmt(n)}</span>
);
const rateCell = (v: number | null, color?: string): React.ReactNode => (
  <span style={{ color: color || COLORS.textSecondary, fontVariantNumeric: 'tabular-nums' }}>{fmtPct(v)}</span>
);

const METRIC_COLS: ColDef[] = [
  // DERIVED attempted (= rates.denom = delivered + hard + soft + untyped
  // bounces), NOT the raw 'attempted' event count: raw attempted events exist
  // on the SES pipe ONLY (PMTA emits none), so the raw count read as an
  // undercount whenever PMTA traffic was in view — microsoft showed
  // "attempted 187,824 / delivered 522,526", delivered ≫ attempted. The
  // Overview KPI strip already displays the derived number; this makes the
  // matrix agree with it and with its own Del% denominator.
  {
    id: 'attempted', label: 'Attempted*', value: (r) => r.rates.denom,
    render: (r) => numCell(r.rates.denom),
    title: () => 'derived: delivered + bounces — raw attempted events exist on the relay route only',
  },
  { id: 'delivered', label: 'Delivered', value: (r) => r.counts.delivered, render: (r) => numCell(r.counts.delivered, COLORS.good) },
  {
    id: 'del_pct', label: 'Del%', value: (r) => r.rates.delivery,
    render: (r) => rateCell(r.rates.delivery, COLORS.good),
    tint: (r) => heatDel(r.rates.delivery), title: (r) => denomTitle(r.rates),
  },
  { id: 'hard', label: 'Hard', value: (r) => r.counts.hard, render: (r) => numCell(r.counts.hard, HARD_RED) },
  {
    id: 'hard_pct', label: 'Hard%', value: (r) => r.rates.hard,
    render: (r) => rateCell(r.rates.hard, HARD_RED),
    tint: (r) => heatHard(r.rates.hard), title: (r) => denomTitle(r.rates),
  },
  { id: 'soft', label: 'Soft', value: (r) => r.counts.soft, render: (r) => numCell(r.counts.soft, SOFT_AMBER) },
  {
    id: 'soft_pct', label: 'Soft%', value: (r) => r.rates.soft,
    render: (r) => rateCell(r.rates.soft, SOFT_AMBER), title: (r) => denomTitle(r.rates),
  },
  { id: 'delays', label: 'Delays', value: (r) => r.counts.delays, render: (r) => numCell(r.counts.delays, COLORS.warn) },
  { id: 'complaints', label: 'Compl', value: (r) => r.counts.complaints, render: (r) => numCell(r.counts.complaints, COMPLAINT_ROSE) },
  {
    id: 'comp_pct', label: 'Comp%', value: (r) => r.rates.complaint,
    render: (r) => rateCell(r.rates.complaint, COMPLAINT_ROSE),
    tint: (r) => heatComp(r.rates.complaint), title: (r) => denomTitle(r.rates),
  },
  { id: 'opens', label: 'Opens', value: (r) => r.counts.opens, render: (r) => numCell(r.counts.opens, OPEN_CYAN) },
  {
    id: 'open_pct', label: 'Open%', value: (r) => r.rates.open,
    render: (r) => rateCell(r.rates.open, OPEN_CYAN), title: (r) => deliveredTitle(r.counts),
  },
  { id: 'clicks', label: 'Clicks', value: (r) => r.counts.clicks, render: (r) => numCell(r.counts.clicks, CLICK_VIOLET) },
  {
    id: 'click_pct', label: 'Click%', value: (r) => r.rates.click,
    render: (r) => rateCell(r.rates.click, CLICK_VIOLET), title: (r) => deliveredTitle(r.counts),
  },
];

// cols must be the table's FULL column list (METRIC_COLS + any extraCols):
// resolving sort.col against METRIC_COLS alone silently fell through to the
// name sort whenever an extra column's header (Deferred / Recovered%) was
// clicked — the arrow toggled but rows ordered alphabetically.
function sortMetricRows(rows: MetricRow[], sort: SortState, cols: ColDef[]): MetricRow[] {
  const col = cols.find((c) => c.id === sort.col);
  const sorted = [...rows];
  sorted.sort((a, b) => {
    let cmp: number;
    if (!col) {
      cmp = a.key.localeCompare(b.key); // sort by dim value
    } else {
      const av = col.value(a); const bv = col.value(b);
      const an = av == null ? -Infinity : av;
      const bn = bv == null ? -Infinity : bv;
      cmp = an === bn ? a.key.localeCompare(b.key) : an < bn ? -1 : 1;
    }
    return sort.dir === 'asc' ? cmp : -cmp;
  });
  return sorted;
}

const MetricsTable: React.FC<{
  dimLabel: string;
  rows: MetricRow[];
  totals: MetricRow;
  sort: SortState;
  onSort: (col: string) => void;
  expandedKey?: string | null;
  onToggleExpand?: (key: string) => void;
  renderExpanded?: (key: string) => React.ReactNode;
  // Optional trailing columns (e.g. the deferral funnel, keyed off MetricRow.key
  // — which is the dim value). Their value/render read external state via the
  // ColDef closure; not in METRIC_COLS so they never leak to other tables.
  extraCols?: ColDef[];
}> = ({ dimLabel, rows, totals, sort, onSort, expandedKey, onToggleExpand, renderExpanded, extraCols }) => {
  const cols = useMemo(() => [...METRIC_COLS, ...(extraCols ?? [])], [extraCols]);
  const sorted = useMemo(() => sortMetricRows(rows, sort, cols), [rows, sort, cols]);
  const expandable = !!onToggleExpand;
  const arrow = (col: string) => sort.col === col ? (sort.dir === 'desc' ? ' ▾' : ' ▴') : '';
  const renderCells = (r: MetricRow, isTotal: boolean) => cols.map((c) => (
    <td
      key={c.id}
      style={{
        ...styles.td, textAlign: 'right', whiteSpace: 'nowrap',
        background: !isTotal && c.tint ? c.tint(r) : undefined,
        fontWeight: isTotal ? 700 : 400,
      }}
      title={c.title ? c.title(r) : undefined}
    >
      {c.render(r)}
    </td>
  ));
  return (
    <div style={styles.tableWrap}>
      <table style={styles.table}>
        <thead>
          <tr>
            <th style={{ ...styles.th, textAlign: 'left', cursor: 'pointer' }} onClick={() => onSort('__key')}>
              {dimLabel}{arrow('__key')}
            </th>
            {cols.map((c) => (
              <th key={c.id} style={{ ...styles.th, textAlign: 'right', cursor: 'pointer' }} onClick={() => onSort(c.id)}>
                {c.label}{arrow(c.id)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {/* Totals row pinned on top */}
          <tr style={{ ...styles.tr, background: COLORS.bgPanelAlt }}>
            <td style={{ ...styles.td, fontWeight: 700, color: COLORS.textPrimary }}>
              TOTAL ({fmt(rows.length)} {rows.length === 1 ? 'value' : 'values'})
            </td>
            {renderCells(totals, true)}
          </tr>
          {sorted.map((r) => {
            const isOpen = expandedKey === r.key;
            const canExpand = expandable && r.key !== '';
            return (
              <React.Fragment key={r.key || '(empty)'}>
                <tr
                  style={{ ...styles.tr, cursor: canExpand ? 'pointer' : 'default', background: isOpen ? 'rgba(129,140,248,0.07)' : undefined }}
                  onClick={canExpand ? () => onToggleExpand(r.key) : undefined}
                  title={expandable && r.key === '' ? 'Cannot expand an empty dimension value (no equality filter possible)' : undefined}
                >
                  <td style={{ ...styles.td, whiteSpace: 'nowrap', color: COLORS.textPrimary, fontWeight: 500 }}>
                    {expandable && (
                      <FontAwesomeIcon
                        icon={isOpen ? faChevronDown : faChevronRight}
                        style={{ fontSize: 9, marginRight: 8, color: canExpand ? COLORS.textMuted : 'transparent' }}
                      />
                    )}
                    {r.key || '(empty)'}
                  </td>
                  {renderCells(r, false)}
                </tr>
                {isOpen && renderExpanded && (
                  <tr style={styles.tr}>
                    <td colSpan={cols.length + 1} style={styles.expandCell}>
                      {renderExpanded(r.key)}
                    </td>
                  </tr>
                )}
              </React.Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB COMPONENTS
// ═══════════════════════════════════════════════════════════════════════════

// ─── Shared toolbar (applies to Overview + Dimensions) ─────────────────────
// The toolbar is now the shared FilterBar (shared/filters.tsx) — the unified
// filter standard extracted from this screen. Rendered in the root component
// below with all four fields enabled.

// ─── Tab 1: Overview ────────────────────────────────────────────────────────

const OverviewTab: React.FC<{ applied: AppliedFilters }> = ({ applied }) => {
  const { addToast } = useToast();
  const [routeRows, setRouteRows] = useState<BreakdownRow[]>([]);
  const [ispRows, setIspRows] = useState<BreakdownRow[]>([]);
  const [eng, setEng] = useState<EngSummary | null>(null);
  const [funnel, setFunnel] = useState<DeferralFunnel | null>(null);
  const [trendGrain, setTrendGrain] = useState<'day' | 'hour'>('day');
  const [hourlyRows, setHourlyRows] = useState<BreakdownRow[]>([]);
  const [meta, setMeta] = useState<FetchMeta | null>(null);
  const [truncated, setTruncated] = useState(false);
  const [loading, setLoading] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState('');
  const [visible, setVisible] = useState<Set<string>>(
    () => new Set(['delivered', 'hardPct', 'softPct', 'compPct'])
  );
  const abortRef = useRef<AbortController | null>(null);
  const hourlyAbortRef = useRef<AbortController | null>(null);
  // Refresh must also refresh the HOURLY trend: load(true) bypasses the cache
  // for its own fetches, but the hourly series lives in a separate effect whose
  // deps don't change on Refresh (nonce only bumps on Run) and whose fetch
  // never bypassed — so hourly bars stayed frozen at first-fetch values.
  // Bumping this counter re-triggers the effect; the ref carries the one-shot
  // bypass across to it.
  const [hourlyRefresh, setHourlyRefresh] = useState(0);
  const hourlyBypassRef = useRef(false);

  const load = useCallback(async (bypass: boolean) => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    if (bypass) {
      hourlyBypassRef.current = true;
      setHourlyRefresh((n) => n + 1);
    }
    try {
      // SINGLE SOURCE OF TRUTH: ONE breakdown (local_dt × source × route_type ×
      // event_type) drives BOTH the headline KPIs and the route funnel, so they
      // can never disagree. The headline is this query's rows filtered to the
      // toolbar transport (routeRowsForTransport); the funnel reads the same rows
      // split by source/route_type. (Previously the headline was a SEPARATE
      // breakdown with its own cache key — it could serve a stale "cached" value
      // while the funnel was fresh, so Delivered differed by thousands.) limit
      // 5000 covers multi-day ranges (days × sources × routes × event_types).
      // Companion query powers the per-ISP table; the engagement summary powers
      // the open/click KPIs from Postgres+verdict (the lake's open/click slice is
      // ~3 orders of magnitude low and its is_machine_* flags are inert — see
      // fetchEngagement). The engagement fetch is FAIL-SOFT: a failure degrades
      // the two KPI tiles to "—" without breaking the delivery card.
      const [routeRes, ispRes, engRes, funnelRes] = await Promise.all([
        fetchBreakdown(
          { from: applied.from, to: applied.to, groupBy: ['local_dt', 'source', 'route_type', 'event_type'], limit: 5000, filters: filterParamsNoTransport(applied) },
          applied.nonce,
          { signal: ctl.signal, bypass }
        ),
        fetchBreakdown(
          // Per-ISP deliverability table. Uses the transport-aware filterParams
          // so it honors the toolbar brand=sending-domain AND transport selectors
          // (the operator's "filterable by sending domain" requirement).
          // limit 1000, not 100: raw isp_group carries dozens of PMTA *.queue
          // values × ~8 event types — 100 buckets truncated the tail and
          // silently under-reported some providers' rows.
          { from: applied.from, to: applied.to, groupBy: ['isp_group', 'event_type'], limit: 1000, filters: filterParams(applied) },
          applied.nonce,
          { signal: ctl.signal, bypass }
        ),
        fetchEngagement(applied.from, applied.to, applied.brand, ctl.signal).catch((e) => {
          if (isAbortError(e)) throw e; // let the outer catch swallow aborts uniformly
          // fail-soft — KPIs show "—", card still renders; log so a persistently
          // failing engagement endpoint is visible in the console.
          console.warn('[Overview] engagement summary failed:', e instanceof Error ? e.message : e);
          return null;
        }),
        // Deferral funnel — same fail-soft contract as the engagement fetch: a
        // failure degrades the readout + the two ISP columns to "—", the card
        // and table still render.
        fetchDeferralFunnel(applied.from, applied.to, applied.brand, ctl.signal).catch((e) => {
          if (isAbortError(e)) throw e; // let the outer catch swallow aborts uniformly
          console.warn('[Overview] deferral funnel failed:', e instanceof Error ? e.message : e);
          return null;
        }),
      ]);
      setRouteRows(routeRes.data.rows);
      setIspRows(ispRes.data.rows);
      setEng(engRes);
      setFunnel(funnelRes);
      setTruncated(!!routeRes.data.truncated);
      setMeta(routeRes.meta);
      setLoaded(true);
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      // Clear stale data — old numbers must never render under the
      // newly-applied range/filter labels after the toast fades.
      setRouteRows([]);
      setIspRows([]);
      setEng(null);
      setFunnel(null);
      setMeta(null);
      setTruncated(false);
      setLoaded(false);
      setError(msg);
      addToast({ type: 'error', title: 'Overview breakdown failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [applied, addToast]);

  useEffect(() => {
    load(false);
    return () => abortRef.current?.abort();
  }, [load]);

  // Inclusive range length in days. Parse 'YYYY-MM-DD' at UTC noon so DST never
  // shifts the day count. Hourly trend is only allowed for ≤72h ranges.
  const rangeDays = useMemo(() => {
    const a = Date.parse(`${applied.from}T12:00:00Z`);
    const b = Date.parse(`${applied.to}T12:00:00Z`);
    if (Number.isNaN(a) || Number.isNaN(b)) return 1;
    return Math.round((b - a) / 86400000) + 1;
  }, [applied.from, applied.to]);

  // Separate hourly fetch — kept OUT of the main Promise.all so we never scan
  // hourly partitions while on the Day grain. Only fires for ≤72h ranges once
  // the day-grain load has succeeded. Mirrors load()'s abort discipline.
  useEffect(() => {
    if (trendGrain !== 'hour' || rangeDays > 3 || !loaded) return;
    hourlyAbortRef.current?.abort();
    const ctl = new AbortController();
    hourlyAbortRef.current = ctl;
    // One-shot: consume the Refresh-initiated bypass so grain flips and other
    // re-runs still serve the cache.
    const bypass = hourlyBypassRef.current;
    hourlyBypassRef.current = false;
    (async () => {
      try {
        const hres = await fetchBreakdown(
          { from: applied.from, to: applied.to, groupBy: ['local_hour', 'event_type'], limit: 5000, filters: filterParams(applied) },
          applied.nonce,
          { signal: ctl.signal, bypass }
        );
        setHourlyRows(hres.data.rows);
      } catch (e) {
        if (isAbortError(e)) return;
        const msg = e instanceof Error ? e.message : String(e);
        setHourlyRows([]);
        addToast({ type: 'error', title: 'Hourly trend failed', message: msg });
      }
    })();
    return () => ctl.abort();
  }, [trendGrain, rangeDays, loaded, applied, addToast, hourlyRefresh]);

  // Route funnel reads across the backend's widened dt window (06-23..06-25),
  // so filter to in-range Denver days before pivoting — this is what makes the
  // funnel's TOTAL reconcile to the headline Delivered. RouteFunnelPanel.get()
  // ignores keys it doesn't read, so summing across the kept local_dt rows is
  // correct with no change inside the panel.
  const routeRowsInRange = useMemo(
    () => routeRows.filter((r) => {
      const d = r.keys['local_dt'] ?? '';
      return d >= applied.from && d <= applied.to;
    }),
    [routeRows, applied.from, applied.to]
  );

  // Headline KPIs + trend derive from the SAME route-funnel rows, filtered to
  // the toolbar transport — so the Range Overview Delivered and the route funnel
  // TOTAL reconcile by construction (one query, one cache key).
  const headlineRows = useMemo(
    () => routeRowsForTransport(routeRowsInRange, applied.transport),
    [routeRowsInRange, applied.transport]
  );
  const totals = useMemo(() => totalsFromBreakdown(headlineRows), [headlineRows]);
  const daily = useMemo(() => dailySeries(headlineRows), [headlineRows]);
  // The Day trend is a fixed 7-day view regardless of the selected range
  // (operator 2026-07-02): tapping Day always shows the trailing 7 days. Longer
  // ranges no longer bleed the trend out; KPIs/ISP table still honor the range.
  const dailyTrend = useMemo(() => daily.slice(-7), [daily]);

  // Per-ISP deliverability rows (group_by=isp_group,event_type), grouped into
  // one MetricRow per isp_group, sorted by derived Attempted desc, with a TOTAL.
  const ispMetrics = useMemo(() => {
    const byIsp = new Map<string, Record<string, number>>();
    const totalMap: Record<string, number> = {};
    for (const r of ispRows) {
      const k = (r.keys['isp_group'] ?? '') || '(unknown)';
      const et = (r.keys['event_type'] ?? '').toLowerCase();
      let m = byIsp.get(k);
      if (!m) { m = {}; byIsp.set(k, m); }
      m[et] = (m[et] || 0) + r.count;
      totalMap[et] = (totalMap[et] || 0) + r.count;
    }
    const out: MetricRow[] = [];
    byIsp.forEach((m, k) => out.push(makeMetricRow(k, m)));
    out.sort((a, b) => b.rates.denom - a.rates.denom);
    return { rows: out, totals: makeMetricRow('TOTAL', totalMap) };
  }, [ispRows]);

  // Deferral funnel keyed by isp_group, joined into the per-ISP table by m.key
  // (= the isp_group value). Missing entry → the row shows "—" / 0. Blank
  // isp_group is normalized to '(unknown)' to match ispMetrics' row key —
  // without it the (unknown) row's deferrals silently read 0 while the TOTAL
  // still included them.
  const funnelByIsp = useMemo(
    () => new Map((funnel?.rows ?? []).map((r) => [r.isp_group || '(unknown)', r])),
    [funnel]
  );

  const hourly = useMemo(() => hourlySeries(hourlyRows), [hourlyRows]);

  const toggleSeries = (id: string) => setVisible((v) => {
    const next = new Set(v);
    if (next.has(id)) next.delete(id); else next.add(id);
    return next;
  });

  const c = totals.counts;
  const r = totals.rates;
  const dn = denomTitle(r);

  // The engagement summary (PG) and deferral funnel are brand-scoped but NOT
  // isp-scoped, while every delivery tile honors the isp_group filter. With an
  // ISP filter active: the deferral KPI narrows to that ISP's funnel row (the
  // funnel is per-ISP, so this is exact), and the open/click tiles suppress
  // their rate (all-ISP numerator over an ISP-scoped delivered denominator once
  // showed >100%) and say so.
  const ispScoped = applied.ispGroup.trim() !== '';
  // funnel===null (fetch failed) must read "unavailable" in BOTH branches —
  // the isp-scoped zero-object is only for "funnel loaded, no row for this ISP".
  const funnelScope = funnel === null
    ? null
    : ispScoped
      ? (funnelByIsp.get(applied.ispGroup.trim()) ?? { deferred: 0, recovered: 0, pending: 0, bounced: 0 })
      : funnel.total;

  // ISP table cell styling — mirrors RouteFunnelPanel's tabular-nums idiom.
  const ispCell: React.CSSProperties = { padding: '6px 14px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
  const ispHead: React.CSSProperties = { ...ispCell, color: '#9ca3af', fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5 };
  const ispRowLabel: React.CSSProperties = { ...ispCell, textAlign: 'left', fontWeight: 600 };

  return (
    <div>
      <div style={styles.panel}>
        <div style={styles.panelHeader}>
          <div>
            <h2 style={styles.panelTitle}>
              <FontAwesomeIcon icon={faChartLine} style={{ marginRight: 8, color: COLORS.accentAlt }} />
              Range Overview
            </h2>
            <p style={styles.panelSubtitle}>
              Days are <b>America/Denver</b> over {applied.from} → {applied.to}. Counts are de-duplicated per event.
              Attempted is DERIVED (delivered+bounces) — direct sends do not record a separate attempted event;
              recorded attempted events exist on the relay route only. Relayed is a relay handoff,
              not a recipient delivery. Open/Click KPIs are RAW recorded events (machine incl.; human
              verdict counts in subtext) from tracking — the Trend chart's open/click series are
              separate raw lake counts. <TimingNote meta={meta} />
            </p>
          </div>
          <button style={styles.refreshBtn} onClick={() => load(true)} disabled={loading}>
            <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} /> Refresh
          </button>
        </div>
        <TruncationBanner truncated={truncated} limit={5000} />
        {error ? (
          <div style={styles.errorShell}>
            <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
            Overview breakdown failed: {error}
            <button style={styles.inlineRetry} onClick={() => load(true)}>Retry</button>
          </div>
        ) : loading && !loaded ? <LoadingRow /> : !loaded ? <EmptyRow label="Waiting for first query…" /> : c.total === 0 ? (
          <EmptyRow label="No events in this range with the active filters." />
        ) : (
          <>
            {/* KPI strip */}
            <div style={styles.kpiGrid}>
              <KpiCard label="Attempted (derived)" value={r.denom} color={COLORS.accent}
                extra="delivered + bounces; direct sends do not record a separate attempted event" />
              <KpiCard label="Delivered" value={c.delivered} color={COLORS.good}
                rate={r.delivery} rateLabel="delivery" denomNote={dn} />
              <KpiCard label="Hard Bounce" value={c.hard} color={HARD_RED}
                rate={r.hard} rateLabel="hard" denomNote={dn} />
              <KpiCard label="Soft Bounce" value={c.soft} color={SOFT_AMBER}
                rate={r.soft} rateLabel="soft" denomNote={dn} />
              <KpiCard label="Retry attempts" value={c.delays} color={COLORS.warn}
                extra="every retry attempt — a deferred message retries several times; see Deferred for unique messages" />
              {/* Deferral lifecycle — sibling to the per-retry KPI above (this one
                  is per-MESSAGE: deferred messages → recovered/pending/bounced).
                  Recovery % is on RESOLVED deferrals (recovered/(recovered+bounced))
                  — many deferrals are still retrying, so recovered/deferred reads a
                  misleadingly low %. Fail-soft: funnel===null shows 0 / "unavailable". */}
              <KpiCard label="Deferred → Recovered" value={funnelScope ? funnelScope.recovered : 0} color={COLORS.good}
                rate={funnelScope && (funnelScope.recovered + funnelScope.bounced) > 0 ? (funnelScope.recovered / (funnelScope.recovered + funnelScope.bounced)) * 100 : null}
                rateLabel="recovered (of resolved)"
                extra={funnelScope
                  ? `${fmt(funnelScope.deferred)} deferred · ${fmt(funnelScope.recovered)} delivered (${fmtPct((funnelScope.recovered + funnelScope.bounced) > 0 ? (funnelScope.recovered / (funnelScope.recovered + funnelScope.bounced)) * 100 : null)} of resolved) · ${fmt(funnelScope.pending)} in flight · ${fmt(funnelScope.bounced)} bounced${ispScoped ? ` · ${applied.ispGroup.trim()} only` : ''}`
                  : 'deferral funnel unavailable'} />
              <KpiCard label="Complaints" value={c.complaints} color={COMPLAINT_ROSE}
                rate={r.complaint} rateLabel="complaint" denomNote={dn} />
              {/* Opens/Clicks are RAW recorded events (machine traffic included),
                  sourced from Postgres tracking — NOT the lake (whose open/click
                  slice is ~3 orders of magnitude low and whose is_machine_* flags
                  are inert). Human (verdict-filtered) counts are in the subtext;
                  the verdict is the only click filter — no asset-host layer. */}
              {/* With an isp_group filter active the rate is SUPPRESSED: the
                  engagement numerator is all-ISP (the PG summary has no ISP
                  scope) while c.delivered is ISP-scoped — that ratio once
                  rendered open rates over 100%. */}
              <KpiCard label="Opens (raw)" value={eng ? eng.raw_opens : 0} color={OPEN_CYAN}
                rate={!ispScoped && eng && c.delivered > 0 ? (eng.raw_opens / c.delivered) * 100 : null}
                rateLabel="open" denomNote={ispScoped ? 'rate n/a — opens are all-ISP, delivered is isp-filtered' : deliveredTitle(c)}
                extra={eng ? `machine incl. · human ${fmt(eng.human_opens)} (${fmt(eng.human_openers)} openers)${ispScoped ? ' · ALL ISPs (isp filter not applied)' : ''}` : 'engagement unavailable'} />
              <KpiCard label="Clicks (raw)" value={eng ? eng.raw_clicks : 0} color={CLICK_VIOLET}
                rate={!ispScoped && eng && c.delivered > 0 ? (eng.raw_clicks / c.delivered) * 100 : null}
                rateLabel="click" denomNote={ispScoped ? 'rate n/a — clicks are all-ISP, delivered is isp-filtered' : deliveredTitle(c)}
                extra={eng ? `machine incl. · human ${fmt(eng.human_clicks)} (${fmt(eng.human_clickers)} clickers) · CTOR ${fmtPct(eng.human_opens > 0 ? (eng.human_clicks / eng.human_opens) * 100 : null)}${ispScoped ? ' · ALL ISPs (isp filter not applied)' : ''}` : 'engagement unavailable'} />
              <KpiCard label="Relayed" value={c.relayed} color={INFO_BLUE}
                extra="relay handoff — not a delivery" />
            </div>

            {/* Trend — Day | Hour (hourly only for ≤72h ranges) */}
            <div style={{ marginTop: 20 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 10 }}>
                <div style={{ ...styles.subPanelTitle, marginBottom: 0 }}>Trend</div>
                <div style={styles.segmented}>
                  {([
                    { id: 'day' as const, label: 'Day' },
                    { id: 'hour' as const, label: 'Hour' },
                  ]).map((opt) => {
                    const active = trendGrain === opt.id;
                    return (
                      <button
                        key={opt.id}
                        type="button"
                        onClick={() => setTrendGrain(opt.id)}
                        style={{
                          ...styles.segmentedBtn,
                          color: active ? COLORS.accent : COLORS.textSecondary,
                          background: active ? COLORS.accent + '1f' : 'transparent',
                          fontWeight: active ? 600 : 400,
                        }}
                      >
                        {opt.label}
                      </button>
                    );
                  })}
                </div>
              </div>
              <SeriesToggles visible={visible} onToggle={toggleSeries} />
              {trendGrain === 'day' ? (
                dailyTrend.length === 0 ? <EmptyRow label="No daily datapoints." /> : (
                  <TrendChart data={dailyTrend} visible={visible} height={300} />
                )
              ) : rangeDays > 3 ? (
                <EmptyRow label="3 day ranges only" />
              ) : hourly.length === 0 ? (
                <EmptyRow label="No hourly datapoints." />
              ) : (
                <TrendChart data={hourly} visible={visible} height={300} />
              )}
            </div>

            {/* Deliverability by ISP — respects the brand (sending domain) and
                transport filters via filterParams on the companion query. */}
            <div style={{ marginTop: 20 }}>
              <div style={{ ...styles.subPanelTitle, marginBottom: 4 }}>Deliverability by ISP</div>
              <div style={{ fontSize: 11, color: COLORS.textMuted, marginBottom: 10 }}>
                Respects the brand (sending domain) and transport filters above.
              </div>
              {ispMetrics.rows.length === 0 ? <EmptyRow label="No ISP rows in this range." /> : (
                <table style={{ borderCollapse: 'collapse', width: '100%', fontSize: 13 }}>
                  <thead>
                    <tr>
                      <th style={{ ...ispHead, textAlign: 'left' }}>ISP</th>
                      <th style={ispHead}>Attempted*</th>
                      <th style={ispHead}>Delivered</th>
                      <th style={ispHead}>Delivery%</th>
                      <th style={ispHead}>Hard%</th>
                      <th style={ispHead}>Soft%</th>
                      <th style={ispHead}>Deferral events</th>
                      <th style={ispHead}>Deferred</th>
                      <th style={ispHead}>Recovered%</th>
                    </tr>
                  </thead>
                  <tbody>
                    {ispMetrics.rows.map((m) => (
                      <tr key={m.key}>
                        <td style={ispRowLabel}>{m.key}</td>
                        <td style={ispCell} title="derived: delivered + bounces">{fmt(m.rates.denom)}</td>
                        <td style={ispCell}>{fmt(m.counts.delivered)}</td>
                        <td style={{ ...ispCell, background: heatDel(m.rates.delivery) }} title={denomTitle(m.rates)}>{fmtPct(m.rates.delivery)}</td>
                        <td style={{ ...ispCell, color: HARD_RED, background: heatHard(m.rates.hard) }} title={denomTitle(m.rates)}>{fmtPct(m.rates.hard)}</td>
                        <td style={{ ...ispCell, color: SOFT_AMBER }} title={denomTitle(m.rates)}>{fmtPct(m.rates.soft)}</td>
                        <td style={ispCell}>{fmt(m.counts.delays)}</td>
                        <td style={ispCell}>{fmt(funnelByIsp.get(m.key)?.deferred ?? 0)}</td>
                        {(() => {
                          const f = funnelByIsp.get(m.key);
                          const resolved = f ? f.recovered + f.bounced : 0;
                          return (
                            <td style={ispCell} title="recovered / (recovered + bounced) — resolved deferrals only">
                              {f && resolved > 0
                                ? `${fmt(f.recovered)} (${fmtPct((f.recovered / resolved) * 100)})`
                                : '—'}
                            </td>
                          );
                        })()}
                      </tr>
                    ))}
                    <tr>
                      <td style={{ ...ispRowLabel, borderTop: '1px solid #374151' }}>TOTAL</td>
                      <td style={{ ...ispCell, borderTop: '1px solid #374151' }}>{fmt(ispMetrics.totals.rates.denom)}</td>
                      <td style={{ ...ispCell, borderTop: '1px solid #374151' }}>{fmt(ispMetrics.totals.counts.delivered)}</td>
                      <td style={{ ...ispCell, borderTop: '1px solid #374151', background: heatDel(ispMetrics.totals.rates.delivery) }} title={denomTitle(ispMetrics.totals.rates)}>{fmtPct(ispMetrics.totals.rates.delivery)}</td>
                      <td style={{ ...ispCell, borderTop: '1px solid #374151', color: HARD_RED, background: heatHard(ispMetrics.totals.rates.hard) }} title={denomTitle(ispMetrics.totals.rates)}>{fmtPct(ispMetrics.totals.rates.hard)}</td>
                      <td style={{ ...ispCell, borderTop: '1px solid #374151', color: SOFT_AMBER }} title={denomTitle(ispMetrics.totals.rates)}>{fmtPct(ispMetrics.totals.rates.soft)}</td>
                      <td style={{ ...ispCell, borderTop: '1px solid #374151' }}>{fmt(ispMetrics.totals.counts.delays)}</td>
                      {/* funnelScope, not funnel.total: with an isp_group filter
                          the rows above are one ISP — an all-ISP TOTAL wouldn't foot. */}
                      <td style={{ ...ispCell, borderTop: '1px solid #374151' }}>{fmt(funnelScope?.deferred ?? 0)}</td>
                      <td style={{ ...ispCell, borderTop: '1px solid #374151' }} title="recovered / (recovered + bounced) — resolved deferrals only">
                        {funnelScope && (funnelScope.recovered + funnelScope.bounced) > 0
                          ? `${fmt(funnelScope.recovered)} (${fmtPct((funnelScope.recovered / (funnelScope.recovered + funnelScope.bounced)) * 100)})`
                          : '—'}
                      </td>
                    </tr>
                  </tbody>
                </table>
              )}
              <div style={{ fontSize: 11, color: COLORS.textMuted, marginTop: 6 }}>
                * Attempted is derived (delivered + bounces) — direct sends record no separate attempted event.
                {' '}Deferral events are raw per-retry delay events, not unique messages.
              </div>
            </div>
          </>
        )}
      </div>
    </div>
  );
};

// ─── Tab 2: Dimensions ──────────────────────────────────────────────────────

const RowTrendExpansion: React.FC<{
  dim: string;
  value: string;
  applied: AppliedFilters;
}> = ({ dim, value, applied }) => {
  const { addToast } = useToast();
  const [data, setData] = useState<{ rows: BreakdownRow[]; meta: FetchMeta; truncated: boolean } | null>(null);
  const [loading, setLoading] = useState(false);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    // Mirror the matrix: delivery from pmta+ses, engagement (open/click) from the
    // RAW source='app' stream (MPP/machine included), scoped to this row's value.
    // Sending-side dims skip engagement entirely (SENDING_SIDE_DIMS): app rows
    // carry no such attribution, and for dim='source' the row value would
    // OVERWRITE source='app' — expanding the ses row then re-fetched the
    // SES-webhook opens the merge strips (double count).
    const engBlind = SENDING_SIDE_DIMS.has(dim);
    const engFilters: Record<string, string> = { source: 'app' };
    if (applied.brand.trim()) engFilters.brand = applied.brand.trim();
    if (applied.ispGroup.trim()) engFilters.isp_group = applied.ispGroup.trim();
    if (!engBlind) engFilters[dim] = value; // the row's dimension value is authoritative
    Promise.all([
      fetchBreakdown(
        {
          from: applied.from, to: applied.to, groupBy: ['local_dt', 'event_type'], limit: 5000,
          filters: { ...filterParams(applied), [dim]: value },
        },
        applied.nonce,
        { signal: ctl.signal }
      ),
      engBlind
        ? Promise.resolve(null)
        : fetchBreakdown(
          { from: applied.from, to: applied.to, groupBy: ['local_dt', 'event_type'], limit: 5000, filters: engFilters },
          applied.nonce,
          { signal: ctl.signal }
        ),
    ]).then(([delivRes, engRes]) => {
      setData({
        rows: mergeDeliveryAndEngagement(delivRes.data.rows, engRes ? engRes.data.rows : []),
        meta: delivRes.meta,
        truncated: !!(delivRes.data.truncated || (engRes && engRes.data.truncated)),
      });
    }).catch((e) => {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      addToast({ type: 'error', title: `Trend for ${dim}=${value} failed`, message: msg });
    }).finally(() => setLoading(false));
    return () => ctl.abort();
  }, [dim, value, applied, addToast]);

  if (loading && !data) return <LoadingRow label={`Loading daily trend for ${value}…`} />;
  if (!data) return <EmptyRow label="No trend data." />;

  const daily = dailySeries(data.rows);
  const totals = totalsFromBreakdown(data.rows);
  const tc = totals.counts; const tr = totals.rates;
  return (
    <div>
      <div style={styles.expandHeader}>
        <span style={{ color: COLORS.textPrimary, fontWeight: 600 }}>{dim}={value}</span>
        <span style={styles.expandStat} title="derived: delivered + bounces">attempted {fmt(tr.denom)}</span>
        <span style={styles.expandStat}>delivered {fmt(tc.delivered)} ({fmtPct(tr.delivery)})</span>
        <span style={{ ...styles.expandStat, color: HARD_RED }} title={denomTitle(tr)}>hard {fmtPct(tr.hard)}</span>
        <span style={{ ...styles.expandStat, color: SOFT_AMBER }} title={denomTitle(tr)}>soft {fmtPct(tr.soft)}</span>
        <span style={{ ...styles.expandStat, color: COMPLAINT_ROSE }} title={denomTitle(tr)}>compl {fmtPct(tr.complaint)}</span>
        <span style={{ ...styles.expandStat, color: OPEN_CYAN }} title={deliveredTitle(tc)}>open {fmtPct(tr.open)}</span>
        <span style={{ ...styles.expandStat, color: CLICK_VIOLET }} title={deliveredTitle(tc)}>click {fmtPct(tr.click)}</span>
        <TimingNote meta={data.meta} />
      </div>
      <TruncationBanner truncated={data.truncated} limit={5000} />
      {daily.length === 0 ? <EmptyRow label="No daily datapoints for this value." /> : (
        <TrendChart
          data={daily}
          visible={new Set(['delivered', 'hardPct', 'softPct', 'compPct'])}
          height={170}
        />
      )}
    </div>
  );
};

// Fetched breakdown rows TOGETHER with the dimension they were fetched for —
// set atomically so the pivot can never run rows from a previous dimension
// (keys = {old_dim, event_type}) against a newly-picked one, which collapsed
// the table into a single "(empty)" row carrying the old grand totals.
interface DimFetched {
  rows: BreakdownRow[];
  dim: string;
  meta: FetchMeta;
  truncated: boolean;
}

const DimensionsTab: React.FC<{ applied: AppliedFilters }> = ({ applied }) => {
  const { addToast } = useToast();
  const [dim, setDim] = useState('isp');
  const [fetched, setFetched] = useState<DimFetched | null>(null);
  const [funnel, setFunnel] = useState<DeferralFunnel | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [sort, setSort] = useState<SortState>({ col: 'attempted', dir: 'desc' });
  const [expandedKey, setExpandedKey] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  // The deferral funnel is only meaningful when the rows ARE mailbox providers
  // (the funnel is keyed by isp_group). Other dims (brand, vmta, …) get no
  // funnel columns.
  const ispDim = dim === 'isp' || dim === 'isp_group';

  const load = useCallback(async (bypass: boolean) => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    setExpandedKey(null);
    try {
      // Delivery (delivered/bounce/delays) reads pmta+ses. Engagement
      // (open/click) reads source='app' — the open-pixel + clicker-tracker
      // stream where MPP and everything that survives the UPSTREAM filtering
      // (segments, pixel, clicker) lands. We merge ONLY the app open/click rows
      // (app's 'delivered'/'bounce' are duplicates) so the matrix shows the RAW
      // engagement signal, machine traffic included — no verdict/MPP filter. The
      // recipient-side filters (brand, isp_group) carry over. Sending-side
      // dims (SENDING_SIDE_DIMS) skip engagement entirely — app rows carry no
      // route/vmta/pool/source attribution, so grouping engagement by them
      // dumped every open into one "(empty)"/'app' bucket while real rows
      // read 0 (QA gate finding, 2026-07-01).
      const engBlind = SENDING_SIDE_DIMS.has(dim);
      const engFilters: Record<string, string> = { source: 'app' };
      if (applied.brand.trim()) engFilters.brand = applied.brand.trim();
      if (applied.ispGroup.trim()) engFilters.isp_group = applied.ispGroup.trim();
      // Deferral funnel only for mailbox-provider dims (keyed by isp_group).
      // Same fail-soft contract as OverviewTab's funnel fetch: a failure degrades
      // the two funnel columns to "—" / 0 without breaking the matrix.
      const wantFunnel = dim === 'isp' || dim === 'isp_group';
      const [delivRes, engRes, funnelRes] = await Promise.all([
        fetchBreakdown(
          { from: applied.from, to: applied.to, groupBy: [dim, 'event_type'], limit: 5000, filters: filterParams(applied) },
          applied.nonce,
          { signal: ctl.signal, bypass }
        ),
        engBlind
          ? Promise.resolve(null)
          : fetchBreakdown(
            { from: applied.from, to: applied.to, groupBy: [dim, 'event_type'], limit: 5000, filters: engFilters },
            applied.nonce,
            { signal: ctl.signal, bypass }
          ),
        wantFunnel
          ? fetchDeferralFunnel(applied.from, applied.to, applied.brand, ctl.signal).catch((e) => {
              if (isAbortError(e)) throw e; // let the outer catch swallow aborts uniformly
              console.warn('[Dimensions] deferral funnel failed:', e instanceof Error ? e.message : e);
              return null;
            })
          : Promise.resolve(null),
      ]);
      setFunnel(funnelRes);
      setFetched({
        rows: mergeDeliveryAndEngagement(delivRes.data.rows, engRes ? engRes.data.rows : []),
        dim,
        meta: delivRes.meta,
        truncated: !!(delivRes.data.truncated || (engRes && engRes.data.truncated)),
      });
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      // Clear stale data — keeping the previous dimension's rows here left the
      // table stuck on old totals (mislabeled as the new dimension) forever.
      setFetched(null);
      setError(msg);
      addToast({ type: 'error', title: 'Dimension breakdown failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [applied, dim, addToast]);

  useEffect(() => {
    load(false);
    return () => abortRef.current?.abort();
  }, [load]);

  // Only pivot rows fetched for the CURRENTLY selected dimension.
  const fresh = fetched !== null && fetched.dim === dim;
  const pivot = useMemo(
    () => (fetched && fetched.dim === dim ? pivotByDim(fetched.rows, fetched.dim) : null),
    [fetched, dim]
  );

  const onSort = (col: string) => setSort((s) =>
    s.col === col ? { col, dir: s.dir === 'desc' ? 'asc' : 'desc' } : { col, dir: 'desc' });

  const dimLabel = ROW_DIMS.find((d) => d.id === dim)?.label || dim;

  // Deferral funnel keyed by isp_group, joined into the matrix by MetricRow.key
  // (= the dim value, identical to isp_group for the main providers). The TOTAL
  // row (key 'TOTAL') reads funnel.total. Recovery % is RESOLVED-based
  // (recovered/(recovered+bounced)) — same formula as the Overview funnel.
  const funnelByIsp = useMemo(
    () => new Map((funnel?.rows ?? []).map((r) => [r.isp_group, r])),
    [funnel]
  );
  const funnelCols: ColDef[] = useMemo(() => {
    const lookup = (r: MetricRow) =>
      r.key === 'TOTAL' ? (funnel?.total ?? null) : (funnelByIsp.get(r.key) ?? null);
    const resolvedPct = (f: { recovered: number; bounced: number } | null): number | null => {
      if (!f) return null;
      const resolved = f.recovered + f.bounced;
      return resolved > 0 ? (f.recovered / resolved) * 100 : null;
    };
    return [
      {
        id: 'deferred', label: 'Deferred',
        value: (r) => lookup(r)?.deferred ?? null,
        render: (r) => numCell(lookup(r)?.deferred ?? 0, SOFT_AMBER),
      },
      {
        id: 'recovered_pct', label: 'Recovered%',
        value: (r) => resolvedPct(lookup(r)),
        render: (r) => {
          const f = lookup(r);
          return f && (f.recovered + f.bounced) > 0
            ? rateCell(resolvedPct(f), COLORS.good)
            : rateCell(null);
        },
        title: () => 'recovered / (recovered + bounced) — resolved deferrals only',
      },
    ];
  }, [funnel, funnelByIsp]);

  return (
    <div style={styles.panel}>
      <div style={styles.panelHeader}>
        <div>
          <h2 style={styles.panelTitle}>
            <FontAwesomeIcon icon={faLayerGroup} style={{ marginRight: 8, color: COLORS.accentAlt }} />
            Dimension Matrix
          </h2>
          <p style={styles.panelSubtitle}>
            group_by={dim},event_type over {applied.from} → {applied.to}. Rate heat: Hard%&gt;1 / Comp%&gt;0.1 / Del%&lt;90 red ·
            Hard%&gt;0.5 / Comp%&gt;0.05 / Del%&lt;97 amber. Opens/Clicks are RAW (open-pixel + clicker
            tracker, machine/MPP included — no verdict filter); delivery reads pmta+ses. Brand is derived
            (stored brand, else the sending server's brand code) — under Brand, "(empty)" is SES-routed
            history from before brand stamping (2026-07); new events carry it. Sending-side dims
            (Source / Sending Route / Server / Pool) show delivery only — engagement events carry no
            sending attribution, so their Opens/Clicks columns read 0 by design. Click a row for
            its daily trend. <TimingNote meta={fresh && fetched ? fetched.meta : null} />
          </p>
        </div>
        <button style={styles.refreshBtn} onClick={() => load(true)} disabled={loading}>
          <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} /> Refresh
        </button>
      </div>

      <div style={styles.dimPickerRow}>
        <span style={styles.chipRowLabel}>Rows:</span>
        {ROW_DIMS.map((d) => {
          const active = dim === d.id;
          return (
            <button
              key={d.id}
              style={{
                ...styles.presetBtn,
                color: active ? COLORS.accent : COLORS.textSecondary,
                borderColor: active ? COLORS.accent + '66' : COLORS.borderStrong,
                background: active ? COLORS.accent + '14' : 'transparent',
              }}
              onClick={() => setDim(d.id)}
            >
              {d.label}
            </button>
          );
        })}
      </div>

      <TruncationBanner truncated={fresh && !!fetched?.truncated} limit={5000} />
      {error ? (
        <div style={styles.errorShell}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          Dimension breakdown failed: {error}
          <button style={styles.inlineRetry} onClick={() => load(true)}>Retry</button>
        </div>
      ) : loading || (fetched !== null && !fresh) ? (
        // Also covers the gap where the picker changed but the reload for the
        // new dimension hasn't landed yet (fetched.dim !== dim).
        <LoadingRow />
      ) : !pivot ? (
        <EmptyRow label="Waiting for first query…" />
      ) : pivot.rows.length === 0 ? (
        <EmptyRow label="No events in this range with the active filters." />
      ) : (
        <MetricsTable
          dimLabel={dimLabel}
          rows={pivot.rows}
          totals={pivot.totals}
          sort={sort}
          onSort={onSort}
          expandedKey={expandedKey}
          onToggleExpand={(k) => setExpandedKey((cur) => (cur === k ? null : k))}
          renderExpanded={(k) => <RowTrendExpansion dim={dim} value={k} applied={applied} />}
          extraCols={ispDim ? funnelCols : undefined}
        />
      )}
    </div>
  );
};

// ─── Tab 3: Campaign Lookup ─────────────────────────────────────────────────

interface LookupResult {
  id: string;
  typeTotals: MetricRow | null;        // (a) group_by=event_type
  typeTotalsMeta: FetchMeta | null;
  typeTruncated: boolean;
  ispRows: BreakdownRow[];             // (b) group_by=isp,event_type
  ispMeta: FetchMeta | null;
  ispTruncated: boolean;
  // engOk: the (a)-side app-engagement fetch succeeded. When false, opens/
  // clicks in typeTotals are 0 because the FETCH failed, not because the lake
  // recorded none — the recon Opens/Clicks rows are omitted in that state.
  engOk: boolean;
  cc: CCDetail | null;                 // (c) campaign-summary — null when degraded
  ccError: string;
  events: LakeEvent[];                 // (d) recent events
  eventsMeta: FetchMeta | null;
}

interface ReconRow {
  label: string;
  lake: number;
  cc: number | null;
  color?: string;
  note?: string;
}

const reconTone = (lake: number, cc: number | null): { tone: 'good' | 'warn' | 'bad' | 'na'; delta: number | null; deltaPct: number | null } => {
  if (cc == null) return { tone: 'na', delta: null, deltaPct: null };
  const delta = lake - cc;
  if (lake === 0 && cc === 0) return { tone: 'good', delta: 0, deltaPct: 0 };
  const base = cc !== 0 ? Math.abs(cc) : Math.abs(lake);
  const deltaPct = (delta / base) * 100;
  const abs = Math.abs(deltaPct);
  return { tone: abs <= 2 ? 'good' : abs <= 10 ? 'warn' : 'bad', delta, deltaPct };
};

const STATUS_CHIP_COLORS: Record<string, string> = {
  sent: COLORS.good, sending: COLORS.good, scheduled: COLORS.accent,
  preparing: COLORS.accentAlt, finalizing_audience: COLORS.accentAlt,
  draft: COLORS.textMuted, paused: COLORS.warn, cancelled: COLORS.textMuted,
  failed: HARD_RED,
};

// ─── Campaign picker (find-by-name instead of pasting a UUID) ──────────────
//
// Backed by GET /api/mailing/analytics/campaign-summary?limit=200 — the same
// fast, pre-aggregated list Campaign Center uses (sorted by recent activity,
// 30s server cache). Partner-drip rollup rows carry a synthetic
// "drip-rollup:<tag>" id that is NOT a campaign UUID, so they are excluded.

interface CampaignPickRow {
  id: string;
  name: string;
  status: string;
  scheduled_at?: string;
  route_type?: string;
  sent?: number;
  is_rollup?: boolean;
}

interface CampaignPickResponse {
  campaigns?: CampaignPickRow[];
  error?: string;
}

// Module-level cache: the picker list is shared across tab visits and only
// refetched when older than a minute (the server caches for 30s anyway).
let campaignListCache: { rows: CampaignPickRow[]; at: number } | null = null;

async function fetchCampaignList(signal: AbortSignal): Promise<CampaignPickRow[]> {
  if (campaignListCache && Date.now() - campaignListCache.at < 60_000) {
    return campaignListCache.rows;
  }
  const res = await apiFetch('/api/mailing/analytics/campaign-summary?limit=200', { signal });
  const json: CampaignPickResponse = await res.json();
  if (!res.ok || json.error) throw new Error(json.error || `HTTP ${res.status}`);
  const rows = (json.campaigns || []).filter((c) => !c.is_rollup && isUUID(c.id));
  campaignListCache = { rows, at: Date.now() };
  return rows;
}

const CampaignTab: React.FC<{
  request: { id: string; nonce: number } | null;
}> = ({ request }) => {
  const { addToast } = useToast();
  const [input, setInput] = useState('');
  const [from, setFrom] = useState(daysAgoDenver(29));
  const [to, setTo] = useState(denverToday());
  const [recent, setRecent] = useState<RecentCampaign[]>(() => loadRecentCampaigns());
  const [result, setResult] = useState<LookupResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [ispSort, setIspSort] = useState<SortState>({ col: 'attempted', dir: 'desc' });
  const abortRef = useRef<AbortController | null>(null);
  const rangeRef = useRef({ from, to });
  rangeRef.current = { from, to };

  // Campaign picker: search the recent-activity campaign list by name instead
  // of pasting a UUID.
  const [pickQuery, setPickQuery] = useState('');
  const [pickOpen, setPickOpen] = useState(false);
  const [pickRows, setPickRows] = useState<CampaignPickRow[]>([]);
  const [pickLoading, setPickLoading] = useState(false);
  const [pickError, setPickError] = useState('');
  const pickAbortRef = useRef<AbortController | null>(null);

  const loadPickList = useCallback(async () => {
    pickAbortRef.current?.abort();
    const ctl = new AbortController();
    pickAbortRef.current = ctl;
    setPickLoading(true);
    setPickError('');
    try {
      const rows = await fetchCampaignList(ctl.signal);
      if (ctl.signal.aborted) return;
      setPickRows(rows);
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      setPickError(msg);
      addToast({ type: 'error', title: 'Campaign list failed', message: msg });
    } finally {
      if (pickAbortRef.current === ctl) setPickLoading(false);
    }
  }, [addToast]);

  useEffect(() => {
    loadPickList();
    return () => pickAbortRef.current?.abort();
  }, [loadPickList]);

  const pickMatches = useMemo(() => {
    const q = pickQuery.trim().toLowerCase();
    const base = q === ''
      ? pickRows
      : pickRows.filter((c) => c.name.toLowerCase().includes(q) || c.id.toLowerCase().includes(q));
    return base.slice(0, 12);
  }, [pickRows, pickQuery]);

  const runLookup = useCallback(async (rawId: string) => {
    const id = rawId.trim();
    if (!isUUID(id)) {
      addToast({ type: 'error', title: 'Invalid campaign id', message: 'Expected a UUID (8-4-4-4-12 hex).' });
      return;
    }
    setRecent(saveRecentCampaign(id));
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    const { from: f, to: t } = rangeRef.current;
    // Delivery reads pmta+ses ONLY; engagement (open/click) reads the source='app'
    // PG-tracking mirror — same split as the Dimensions matrix. An unfiltered
    // campaign query counted every event twice wherever the app mirror exists
    // (verified 2026-07-01: campaign ec245ce4 showed delivered=17,448 under BOTH
    // source=pmta and source=app — the old funnel displayed 34,896).
    const delivFilters = { campaign_id: id, source_in: 'pmta,ses' };
    const engLakeFilters = { campaign_id: id, source: 'app' };
    const [a, aEng, b, bEng, c, d] = await Promise.allSettled([
      fetchBreakdown({ from: f, to: t, groupBy: ['event_type'], limit: 5000, filters: delivFilters }, 0, { signal: ctl.signal, bypass: true }),
      fetchBreakdown({ from: f, to: t, groupBy: ['event_type'], limit: 5000, filters: engLakeFilters }, 0, { signal: ctl.signal, bypass: true }),
      fetchBreakdown({ from: f, to: t, groupBy: ['isp', 'event_type'], limit: 5000, filters: delivFilters }, 0, { signal: ctl.signal, bypass: true }),
      fetchBreakdown({ from: f, to: t, groupBy: ['isp', 'event_type'], limit: 5000, filters: engLakeFilters }, 0, { signal: ctl.signal, bypass: true }),
      fetchCampaignSummary(id, ctl.signal),
      fetchLakeEvents({ campaign_id: id, limit: 100 }, { signal: ctl.signal }),
    ]);
    if (ctl.signal.aborted) return;

    const res: LookupResult = {
      id,
      typeTotals: null, typeTotalsMeta: null, typeTruncated: false,
      ispRows: [], ispMeta: null, ispTruncated: false,
      engOk: false,
      cc: null, ccError: '',
      events: [], eventsMeta: null,
    };
    // Engagement fetches are FAIL-SOFT: a failure degrades opens/clicks to 0
    // for the funnel/matrix without dropping the delivery truth — but it must
    // be VISIBLE (toast) and the reconciliation must not render a red -100%
    // "lake lost your opens" row off a failed fetch (QA gate, 2026-07-01).
    const aEngRows = aEng.status === 'fulfilled' ? aEng.value.data.rows : [];
    const bEngRows = bEng.status === 'fulfilled' ? bEng.value.data.rows : [];
    res.engOk = aEng.status === 'fulfilled';
    if (aEng.status === 'rejected' && !isAbortError(aEng.reason)) {
      addToast({ type: 'error', title: 'Campaign engagement query failed', message: aEng.reason instanceof Error ? aEng.reason.message : String(aEng.reason) });
    }
    if (bEng.status === 'rejected' && !isAbortError(bEng.reason)) {
      addToast({ type: 'error', title: 'Per-provider engagement query failed', message: bEng.reason instanceof Error ? bEng.reason.message : String(bEng.reason) });
    }
    if (a.status === 'fulfilled') {
      res.typeTotals = totalsFromBreakdown(mergeDeliveryAndEngagement(a.value.data.rows, aEngRows));
      res.typeTotalsMeta = a.value.meta;
      res.typeTruncated = !!(a.value.data.truncated || (aEng.status === 'fulfilled' && aEng.value.data.truncated));
    } else if (!isAbortError(a.reason)) {
      addToast({ type: 'error', title: 'Campaign funnel query failed', message: a.reason instanceof Error ? a.reason.message : String(a.reason) });
    }
    if (b.status === 'fulfilled') {
      res.ispRows = mergeDeliveryAndEngagement(b.value.data.rows, bEngRows);
      res.ispMeta = b.value.meta;
      res.ispTruncated = !!(b.value.data.truncated || (bEng.status === 'fulfilled' && bEng.value.data.truncated));
    } else if (!isAbortError(b.reason)) {
      addToast({ type: 'error', title: 'Campaign per-provider query failed', message: b.reason instanceof Error ? b.reason.message : String(b.reason) });
    }
    if (c.status === 'fulfilled') {
      // The handler can return HTTP 200 with {api_version, error} and NO
      // campaign key — without a usable campaign object this is a failed
      // lookup, surfaced via the lake-only notice.
      if (c.value.campaign && typeof c.value.campaign === 'object') {
        res.cc = c.value;
      } else {
        res.ccError = (typeof c.value.error === 'string' && c.value.error) || 'campaign not found';
      }
    } else {
      res.ccError = c.reason instanceof Error ? c.reason.message : String(c.reason);
    }
    if (d.status === 'fulfilled') {
      res.events = d.value.data.events;
      res.eventsMeta = d.value.meta;
    } else if (!isAbortError(d.reason)) {
      addToast({ type: 'error', title: 'Campaign events query failed', message: d.reason instanceof Error ? d.reason.message : String(d.reason) });
    }
    setResult(res);
    setLoading(false);
  }, [addToast]);

  // Cross-tab navigation: Raw Events campaign_id click prefills + auto-runs.
  useEffect(() => {
    if (request) {
      setInput(request.id);
      runLookup(request.id);
    }
  }, [request, runLookup]);

  useEffect(() => () => abortRef.current?.abort(), []);

  const ispPivot = useMemo(() => pivotByDim(result?.ispRows || [], 'isp'), [result]);
  const onIspSort = (col: string) => setIspSort((s) =>
    s.col === col ? { col, dir: s.dir === 'desc' ? 'asc' : 'desc' } : { col, dir: 'desc' });

  const cc = result?.cc?.campaign || null;
  const lakeC = result?.typeTotals?.counts || null;
  const lakeR = result?.typeTotals?.rates || null;

  // Opens/Clicks now come from the source='app' engagement stream (the
  // PG-tracking mirror), so they exist for EVERY route — the old "SES-routed
  // only" gate dated from before the tracking→lake mirror carried engagement.
  // No Complaints row: the campaign-summary DETAIL endpoint has no complaints
  // field, so it could never reconcile (lake complaints live in the funnel KPIs).
  const reconRows: ReconRow[] = (lakeC && cc) ? [
    { label: 'Delivered', lake: lakeC.delivered, cc: cc.delivered },
    // The lake's ClassifyBounce folds policy/routing/connection categories into
    // hard_bounce, while Campaign Center v1.4 splits those out as
    // reputation_block — so the comparable tracking number is the SUM.
    { label: 'Hard bounce *', lake: lakeC.hard, cc: cc.hard_bounce + (cc.reputation_block ?? 0), color: HARD_RED, note: 'tracking = hard bounces + provider blocks (analytics folds provider blocks into hard)' },
    { label: 'Soft bounce', lake: lakeC.soft, cc: cc.soft_bounce, color: SOFT_AMBER },
    // Only when the engagement fetch SUCCEEDED — otherwise lake opens/clicks
    // are 0 because the fetch failed, and the rows would render a red -100%
    // "divergence" that is actually a transient error (QA gate, 2026-07-01).
    ...(result?.engOk ? [
      { label: 'Opens *', lake: lakeC.opens, cc: cc.unique_opens, color: OPEN_CYAN, note: 'analytics = total open events (app stream, machine incl.); tracking = unique opens' },
      { label: 'Clicks *', lake: lakeC.clicks, cc: cc.unique_clicks, color: CLICK_VIOLET, note: 'analytics = total click events (app stream, machine incl.); tracking = unique clicks' },
    ] : []),
  ] : [];

  return (
    <div>
      {/* Lookup controls */}
      <div style={styles.panel}>
        <div style={styles.panelHeader}>
          <div>
            <h2 style={styles.panelTitle}>
              <FontAwesomeIcon icon={faBullseye} style={{ marginRight: 8, color: COLORS.accentPink }} />
              Campaign Lookup
            </h2>
            <p style={styles.panelSubtitle}>
              Analytics funnel + per-provider matrix + reconciliation against the Campaign Center tracking-derived truth.
              Analytics queries scan {from} → {to}.
            </p>
          </div>
        </div>
        <div style={styles.eventFilterBar}>
          {/* Find-by-name picker — the primary path. The raw UUID field stays
              as the escape hatch (deep links, ids from logs). */}
          <label style={{ ...styles.fieldLabel, position: 'relative' }}>find campaign (name)
            <input
              type="text" value={pickQuery} placeholder="Search recent campaigns…"
              onChange={(e) => { setPickQuery(e.target.value); setPickOpen(true); }}
              onFocus={() => setPickOpen(true)}
              onBlur={() => setTimeout(() => setPickOpen(false), 150)}
              style={{ ...styles.input, width: 340 }}
            />
            {pickOpen && (
              <div style={styles.pickerDropdown}>
                {pickLoading ? (
                  <div style={styles.pickerNote}><FontAwesomeIcon icon={faSpinner} spin /> Loading campaigns…</div>
                ) : pickError ? (
                  <div style={{ ...styles.pickerNote, color: COLORS.danger }}>{pickError}</div>
                ) : pickMatches.length === 0 ? (
                  <div style={styles.pickerNote}>No campaigns match "{pickQuery}".</div>
                ) : (
                  pickMatches.map((c) => (
                    <div
                      key={c.id}
                      style={styles.pickerItem}
                      // onMouseDown (not onClick) so it fires before the input's
                      // blur closes the dropdown.
                      onMouseDown={() => {
                        setInput(c.id);
                        setPickQuery(c.name);
                        setPickOpen(false);
                        runLookup(c.id);
                      }}
                    >
                      <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                        <span style={{ color: COLORS.textPrimary, fontSize: 12, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', flex: 1 }} title={c.name}>
                          {c.name || '(unnamed)'}
                        </span>
                        <span style={{
                          ...styles.typeChip, fontSize: 10, padding: '1px 6px',
                          color: STATUS_CHIP_COLORS[c.status] || COLORS.accent,
                          borderColor: (STATUS_CHIP_COLORS[c.status] || COLORS.accent) + '55',
                        }}>{c.status}</span>
                      </div>
                      <div style={{ display: 'flex', gap: 10, fontSize: 10, color: COLORS.textMuted, marginTop: 2 }}>
                        <span>{c.scheduled_at ? new Date(c.scheduled_at).toLocaleString('en-US', { hour12: false, month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '—'}</span>
                        {typeof c.sent === 'number' && <span>sent {fmt(c.sent)}</span>}
                        {c.route_type && <span>{c.route_type}</span>}
                        <span style={{ fontFamily: 'monospace' }}>{truncate(c.id, 12)}</span>
                      </div>
                    </div>
                  ))
                )}
              </div>
            )}
          </label>
          <label style={styles.fieldLabel}>campaign_id (UUID)
            <input
              type="text" value={input} placeholder="or paste a UUID"
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => { if (e.key === 'Enter') runLookup(input); }}
              style={{ ...styles.input, width: 280, fontFamily: 'monospace' }}
            />
          </label>
          <label style={styles.fieldLabel}>From
            <input type="date" value={from} max={to} onChange={(e) => setFrom(e.target.value)} style={styles.input} />
          </label>
          <label style={styles.fieldLabel}>To
            <input type="date" value={to} min={from} max={denverToday()} onChange={(e) => setTo(e.target.value)} style={styles.input} />
          </label>
          <button style={styles.primaryBtn} onClick={() => runLookup(input)} disabled={loading}>
            <FontAwesomeIcon icon={loading ? faSpinner : faSearch} spin={loading} /> Lookup
          </button>
        </div>
        {recent.length > 0 && (
          <div style={styles.chipRow}>
            <span style={styles.chipRowLabel}><FontAwesomeIcon icon={faHistory} style={{ marginRight: 4 }} />Recent:</span>
            {recent.map((rc) => {
              // Prefer the campaign's NAME when the picker list knows it —
              // raw UUIDs are unreadable; the id stays in the tooltip.
              const known = pickRows.find((c) => c.id === rc.id);
              return (
                <button
                  key={rc.id}
                  style={{ ...styles.recentChip, fontFamily: known ? 'inherit' : 'monospace' }}
                  title={`${rc.id} — looked up ${new Date(rc.at).toLocaleString('en-US', { hour12: false })}`}
                  onClick={() => { setInput(rc.id); runLookup(rc.id); }}
                >
                  {known ? truncate(known.name, 32) : truncate(rc.id, 14)}
                  <span style={{ color: COLORS.textMuted, marginLeft: 6, fontSize: 10 }}>
                    {new Date(rc.at).toLocaleString('en-US', { hour12: false, month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}
                  </span>
                </button>
              );
            })}
          </div>
        )}
      </div>

      {loading && <div style={styles.panel}><LoadingRow label="Running campaign lookup (4 parallel queries)…" /></div>}

      {!loading && result && (
        <>
          {/* Campaign header card — Campaign Center metadata, degrades to lake-only */}
          <div style={styles.panel}>
            {cc ? (
              <div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
                  <span style={{ fontSize: 17, fontWeight: 700, color: COLORS.textPrimary }}>{cc.name || '(unnamed campaign)'}</span>
                  <span style={{
                    ...styles.typeChip,
                    color: STATUS_CHIP_COLORS[cc.status] || COLORS.accent,
                    borderColor: (STATUS_CHIP_COLORS[cc.status] || COLORS.accent) + '55',
                  }}>
                    {cc.status}
                  </span>
                  <span style={{ ...styles.typeChip, color: INFO_BLUE, borderColor: INFO_BLUE + '55' }}>{cc.route_type}</span>
                </div>
                <div style={styles.metaGrid}>
                  <MetaItem label="subject" value={cc.subject} />
                  <MetaItem label="from" value={cc.from_name && cc.from_email ? `${cc.from_name} <${cc.from_email}>` : cc.from_email || cc.from_name} />
                  <MetaItem label="scheduled_at" value={cc.scheduled_at ? new Date(cc.scheduled_at).toLocaleString('en-US', { hour12: false }) : undefined} />
                  <MetaItem label="sending_domain" value={cc.sending_domain} />
                  <MetaItem label="ip_pool" value={cc.ip_pool} />
                  <MetaItem label="campaign_id" value={result.id} mono />
                </div>
              </div>
            ) : (
              <div style={styles.lakeOnlyNotice}>
                <FontAwesomeIcon icon={faInfoCircle} style={{ marginRight: 8, color: COLORS.warn }} />
                Analytics-only view — Campaign Center summary unavailable for <code style={styles.code}>{result.id}</code>
                {result.ccError ? ` (${result.ccError})` : ''}. Metadata and reconciliation are hidden; analytics metrics below are still authoritative for raw event counts.
              </div>
            )}
          </div>

          {/* Lake funnel KPIs */}
          <div style={styles.panel}>
            <div style={styles.panelHeader}>
              <div>
                <h2 style={styles.panelTitle}>Analytics Funnel</h2>
                <p style={styles.panelSubtitle}>
                  group_by=event_type, campaign_id={truncate(result.id, 14)}, {from} → {to}.
                  Delivery reads pmta+ses; Opens/Clicks are RAW recorded events from the tracking
                  stream (machine incl.). <TimingNote meta={result.typeTotalsMeta} />
                </p>
              </div>
            </div>
            <TruncationBanner truncated={result.typeTruncated} limit={5000} />
            {lakeC && lakeR ? (
              lakeC.total === 0 ? <EmptyRow label="No analytics events for this campaign in the selected range — widen the date range." /> : (
                <div style={styles.kpiGrid}>
                  <KpiCard label="Attempted (derived)" value={lakeR.denom} color={COLORS.accent}
                    extra="delivered + bounces; direct sends do not record a separate attempted event" />
                  <KpiCard label="Delivered" value={lakeC.delivered} color={COLORS.good} rate={lakeR.delivery} rateLabel="delivery" denomNote={denomTitle(lakeR)} />
                  <KpiCard label="Hard Bounce" value={lakeC.hard} color={HARD_RED} rate={lakeR.hard} rateLabel="hard" denomNote={denomTitle(lakeR)} />
                  <KpiCard label="Soft Bounce" value={lakeC.soft} color={SOFT_AMBER} rate={lakeR.soft} rateLabel="soft" denomNote={denomTitle(lakeR)} />
                  <KpiCard label="Delays" value={lakeC.delays} color={COLORS.warn} />
                  <KpiCard label="Complaints" value={lakeC.complaints} color={COMPLAINT_ROSE} rate={lakeR.complaint} rateLabel="complaint" denomNote={denomTitle(lakeR)} />
                  <KpiCard label="Opens" value={lakeC.opens} color={OPEN_CYAN} rate={lakeR.open} rateLabel="open" denomNote={deliveredTitle(lakeC)} />
                  <KpiCard label="Clicks" value={lakeC.clicks} color={CLICK_VIOLET} rate={lakeR.click} rateLabel="click" denomNote={deliveredTitle(lakeC)}
                    extra={`CTOR: ${fmtPct(lakeR.ctor)}`} />
                  <KpiCard label="Relayed" value={lakeC.relayed} color={INFO_BLUE} extra="relay handoff — not a delivery" />
                </div>
              )
            ) : <EmptyRow label="Analytics funnel query failed — see toast." />}
          </div>

          {/* Per-ISP matrix */}
          <div style={styles.panel}>
            <div style={styles.panelHeader}>
              <div>
                <h2 style={styles.panelTitle}>Per-Provider Matrix</h2>
                <p style={styles.panelSubtitle}>
                  group_by=isp,event_type (clean provider, classified from the real recipient domain)
                  scoped to this campaign. <TimingNote meta={result.ispMeta} />
                </p>
              </div>
            </div>
            <TruncationBanner truncated={result.ispTruncated} limit={5000} />
            {ispPivot.rows.length === 0 ? <EmptyRow label="No per-provider analytics rows for this campaign in range." /> : (
              <MetricsTable
                dimLabel="Mailbox Provider"
                rows={ispPivot.rows}
                totals={ispPivot.totals}
                sort={ispSort}
                onSort={onIspSort}
              />
            )}
          </div>

          {/* Reconciliation: lake vs Campaign Center */}
          {cc && lakeC && (
            <div style={styles.panel}>
              <div style={styles.panelHeader}>
                <div>
                  <h2 style={styles.panelTitle}>Reconciliation — Analytics vs Campaign Center</h2>
                  <p style={styles.panelSubtitle}>
                    Green = within 2% · amber = 2–10% · red = beyond 10%. Analytics counts are de-duplicated per event over {from} → {to};
                    Campaign Center is the tracking-derived truth ({cc.metrics_source || 'tracking'}).
                  </p>
                </div>
              </div>
              {lakeC.total === 0 ? (
                // An empty lake window is NOT data loss — don't render Lake=0
                // vs real CC counts as a wall of red mismatches.
                <div style={styles.lakeOnlyNotice}>
                  <FontAwesomeIcon icon={faInfoCircle} style={{ marginRight: 8, color: COLORS.warn }} />
                  No analytics events for this campaign in the queried window ({from} → {to}) — reconciliation
                  skipped. The campaign likely predates the lookup window (or the analytics history itself); widen the
                  date range and rerun to compare against Campaign Center.
                </div>
              ) : (
                <div style={styles.tableWrap}>
                  <table style={styles.table}>
                    <thead>
                      <tr>
                        <th style={{ ...styles.th, textAlign: 'left' }}>Metric</th>
                        <th style={{ ...styles.th, textAlign: 'right' }}>Analytics</th>
                        <th style={{ ...styles.th, textAlign: 'right' }}>Campaign Center</th>
                        <th style={{ ...styles.th, textAlign: 'right' }}>Δ</th>
                        <th style={{ ...styles.th, textAlign: 'right' }}>Δ%</th>
                        <th style={{ ...styles.th, textAlign: 'center' }}>Match</th>
                      </tr>
                    </thead>
                    <tbody>
                      {reconRows.map((row) => {
                        const t = reconTone(row.lake, row.cc);
                        const toneColor = t.tone === 'good' ? COLORS.good : t.tone === 'warn' ? SOFT_AMBER : t.tone === 'bad' ? HARD_RED : COLORS.textMuted;
                        const icon = t.tone === 'good' ? faCheckCircle : t.tone === 'bad' ? faTimesCircle : faExclamationTriangle;
                        return (
                          <tr key={row.label} style={styles.tr} title={row.note}>
                            <td style={{ ...styles.td, color: row.color || COLORS.textPrimary, fontWeight: 500 }}>{row.label}</td>
                            <td style={{ ...styles.td, textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmt(row.lake)}</td>
                            <td style={{ ...styles.td, textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{row.cc == null ? '—' : fmt(row.cc)}</td>
                            <td style={{ ...styles.td, textAlign: 'right', fontVariantNumeric: 'tabular-nums', color: toneColor }}>
                              {t.delta == null ? '—' : `${t.delta > 0 ? '+' : ''}${fmt(t.delta)}`}
                            </td>
                            <td style={{ ...styles.td, textAlign: 'right', fontVariantNumeric: 'tabular-nums', color: toneColor }}>
                              {t.deltaPct == null ? '—' : `${t.deltaPct > 0 ? '+' : ''}${t.deltaPct.toFixed(2)}%`}
                            </td>
                            <td style={{ ...styles.td, textAlign: 'center' }}>
                              {t.tone === 'na' ? <span style={{ color: COLORS.textMuted }}>n/a</span>
                                : <FontAwesomeIcon icon={icon} style={{ color: toneColor }} />}
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                  <div style={styles.tableFooterNote}>
                    * Opens/clicks compare different units by design: analytics counts <em>total</em> open/click events
                    (every pixel fire / link hit is its own event, machine traffic included), while Campaign Center reports
                    <em> unique</em> opens/clicks (deduped per recipient) — expect analytics ≥ tracking; a large gap usually
                    means heavy re-opens or bot scanning, not data loss. CC total opens={fmt(cc.total_opens)} · total
                    clicks={fmt(cc.total_clicks)} for reference.
                  </div>
                </div>
              )}
            </div>
          )}

          {/* Recent events */}
          <div style={styles.panel}>
            <div style={styles.panelHeader}>
              <div>
                <h2 style={styles.panelTitle}>Recent Events (newest first, limit 100)</h2>
                <p style={styles.panelSubtitle}><TimingNote meta={result.eventsMeta} /></p>
              </div>
            </div>
            {result.events.length === 0 ? <EmptyRow label="No recent analytics events for this campaign." /> : (
              <div style={styles.tableWrap}>
                <table style={styles.table}>
                  <thead>
                    <tr>
                      <th style={{ ...styles.th, textAlign: 'left' }}>event_at</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>event_type</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Mailbox Provider</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>email_domain</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Sending Server</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>bounce_cat</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>dsn_code</th>
                    </tr>
                  </thead>
                  <tbody>
                    {result.events.map((e, i) => (
                      <tr key={e.event_uid || String(i)} style={styles.tr}>
                        <td style={{ ...styles.td, whiteSpace: 'nowrap' }}>{fmtEventAt(e)}</td>
                        <td style={styles.td}>
                          <span style={{ ...styles.typeChip, color: eventTypeColor(e.event_type), borderColor: eventTypeColor(e.event_type) + '55' }}>
                            {e.event_type || '—'}
                          </span>
                        </td>
                        <td style={styles.td}>{e.isp_group || '—'}</td>
                        <td style={styles.td}>{e.email_domain || '—'}</td>
                        <td style={styles.td}>{e.vmta || '—'}</td>
                        <td style={styles.td}>{e.bounce_cat || '—'}</td>
                        <td style={styles.td}>{e.dsn_code || '—'}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </>
      )}

      {!loading && !result && (
        <div style={styles.panel}>
          <EmptyRow label="Enter a campaign UUID and run a lookup — or pick a recent campaign above." />
        </div>
      )}
    </div>
  );
};

const MetaItem: React.FC<{ label: string; value?: string; mono?: boolean }> = ({ label, value, mono }) => (
  <div>
    <div style={styles.metaLabel}>{label}</div>
    <div style={{ ...styles.metaValue, fontFamily: mono ? 'monospace' : undefined, fontSize: mono ? 12 : 13 }}>
      {value || '—'}
    </div>
  </div>
);

// ─── Tab 4: Raw Events ──────────────────────────────────────────────────────

// Detail fields shown when a raw event row is expanded.
const EVENT_DETAIL_FIELDS: Array<{ label: string; get: (e: LakeEvent) => string }> = [
  { label: 'email', get: (e) => e.email },
  { label: 'recipient_send_id', get: (e) => e.recipient_send_id },
  { label: 'subscriber_id', get: (e) => e.subscriber_id },
  { label: 'vmta', get: (e) => e.vmta },
  { label: 'pool', get: (e) => e.pool },
  { label: 'dsn_diag', get: (e) => e.dsn_diag },
  { label: 'source_ip', get: (e) => e.source_ip },
  { label: 'variant', get: (e) => e.variant },
  { label: 'suppression_reason', get: (e) => e.suppression_reason },
  { label: 'route_type', get: (e) => e.route_type },
  { label: 'source', get: (e) => e.source },
  { label: 'event_uid', get: (e) => e.event_uid },
  { label: 'ingested_at', get: (e) => e.ingested_at },
  { label: 'dt', get: (e) => e.dt },
  { label: 'link_url', get: (e) => e.link_url },
];

const RawEventsTab: React.FC<{
  onOpenCampaign: (id: string) => void;
}> = ({ onOpenCampaign }) => {
  const { addToast } = useToast();
  const [dt, setDt] = useState('');
  const [campaignId, setCampaignId] = useState('');
  const [ispGroup, setIspGroup] = useState('');
  const [eventType, setEventType] = useState('');
  const [limit, setLimit] = useState(100);
  const [events, setEvents] = useState<LakeEvent[]>([]);
  const [loading, setLoading] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [meta, setMeta] = useState<FetchMeta | null>(null);
  const [expandedUid, setExpandedUid] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const runQuery = useCallback(async () => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setExpandedUid(null);
    try {
      const res = await fetchLakeEvents(
        { dt: dt || undefined, campaign_id: campaignId, isp_group: ispGroup, event_type: eventType, limit },
        { signal: ctl.signal }
      );
      setEvents(res.data.events);
      setMeta(res.meta);
      setLoaded(true);
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      addToast({ type: 'error', title: 'Event query failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [dt, campaignId, ispGroup, eventType, limit, addToast]);

  useEffect(() => () => abortRef.current?.abort(), []);

  return (
    <div style={styles.panel}>
      <div style={styles.panelHeader}>
        <div>
          <h2 style={styles.panelTitle}>
            <FontAwesomeIcon icon={faTable} style={{ marginRight: 8, color: COLORS.accentPink }} />
            Raw Events
          </h2>
          <p style={styles.panelSubtitle}>
            Newest first. Filters are validated server-side (dt = YYYY-MM-DD, campaign_id = UUID,
            isp_group / event_type = tokens). Limit clamps to 1–1000. Click a row to expand all fields;
            click an event_type / isp_group cell to filter by it; the campaign_id button jumps to Campaign Lookup. <TimingNote meta={meta} />
          </p>
        </div>
      </div>

      <div style={styles.eventFilterBar}>
        <label style={styles.fieldLabel} title="Physical UTC partition column — intentionally NOT the Denver operating day">dt (UTC partition)
          <input type="date" value={dt} max={todayUTC()} onChange={(e) => setDt(e.target.value)} style={styles.input} />
        </label>
        <label style={styles.fieldLabel}>campaign_id
          <input type="text" value={campaignId} placeholder="UUID" onChange={(e) => setCampaignId(e.target.value)} style={{ ...styles.input, width: 280 }} />
        </label>
        <label style={styles.fieldLabel}>isp_group
          <input type="text" list="elx-raw-isp-groups" value={ispGroup} placeholder="gmail" onChange={(e) => setIspGroup(e.target.value)} style={{ ...styles.input, width: 120 }} />
          {/* Local datalist: the old shared Toolbar provided a global
              "elx-isp-groups" datalist this input silently depended on; the
              shared FilterBar now generates its ids, so this tab owns its own. */}
          <datalist id="elx-raw-isp-groups">
            {COMMON_ISP_GROUPS.map((g) => <option key={g} value={g} />)}
          </datalist>
        </label>
        <label style={styles.fieldLabel}>event_type
          <input type="text" value={eventType} placeholder="delivered" onChange={(e) => setEventType(e.target.value)} style={{ ...styles.input, width: 140 }} />
        </label>
        <label style={styles.fieldLabel}>limit
          <input
            type="number" min={1} max={1000} value={limit}
            onChange={(e) => setLimit(Math.max(1, Math.min(1000, Number(e.target.value) || 1)))}
            style={{ ...styles.input, width: 90 }}
          />
        </label>
        <button style={styles.primaryBtn} onClick={runQuery} disabled={loading}>
          <FontAwesomeIcon icon={loading ? faSpinner : faSearch} spin={loading} /> Query
        </button>
      </div>

      {loading ? <LoadingRow /> : !loaded ? (
        <EmptyRow label="Set filters (optional) and run a query to load recent events." />
      ) : events.length === 0 ? (
        <EmptyRow label="No events matched these filters." />
      ) : (
        <div style={styles.tableWrap}>
          <table style={styles.table}>
            <thead>
              <tr>
                <th style={{ ...styles.th, width: 24 }} />
                <th style={{ ...styles.th, textAlign: 'left' }}>event_at</th>
                <th style={{ ...styles.th, textAlign: 'left' }}>event_type</th>
                <th style={{ ...styles.th, textAlign: 'left' }}>brand</th>
                <th style={{ ...styles.th, textAlign: 'left' }}>Mailbox Provider</th>
                <th style={{ ...styles.th, textAlign: 'left' }}>email_domain</th>
                <th style={{ ...styles.th, textAlign: 'left' }}>campaign_id</th>
                <th style={{ ...styles.th, textAlign: 'left' }}>bounce_cat</th>
                <th style={{ ...styles.th, textAlign: 'left' }}>dsn_code</th>
                <th style={{ ...styles.th, textAlign: 'left' }}>link_url</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e, i) => {
                const uid = e.event_uid || `row-${i}`;
                const isOpen = expandedUid === uid;
                return (
                  <React.Fragment key={uid}>
                    <tr
                      style={{ ...styles.tr, cursor: 'pointer', background: isOpen ? 'rgba(129,140,248,0.07)' : undefined }}
                      onClick={() => setExpandedUid((cur) => (cur === uid ? null : uid))}
                    >
                      <td style={{ ...styles.td, color: COLORS.textMuted }}>
                        <FontAwesomeIcon icon={isOpen ? faChevronDown : faChevronRight} style={{ fontSize: 9 }} />
                      </td>
                      <td style={{ ...styles.td, whiteSpace: 'nowrap' }}>{fmtEventAt(e)}</td>
                      <td style={styles.td}>
                        <button
                          style={{ ...styles.typeChip, color: eventTypeColor(e.event_type), borderColor: eventTypeColor(e.event_type) + '55', background: 'transparent', cursor: 'pointer' }}
                          title="Filter this tab by this event type"
                          onClick={(ev) => { ev.stopPropagation(); setEventType(e.event_type); }}
                        >
                          {e.event_type || '—'}
                        </button>
                      </td>
                      <td style={styles.td}>{e.brand || '—'}</td>
                      <td style={styles.td}>
                        {e.isp_group ? (
                          <button
                            style={styles.cellLinkBtn}
                            title="Filter this tab by this mailbox provider"
                            onClick={(ev) => { ev.stopPropagation(); setIspGroup(e.isp_group); }}
                          >
                            {e.isp_group}
                          </button>
                        ) : '—'}
                      </td>
                      <td style={styles.td}>{e.email_domain || '—'}</td>
                      <td style={{ ...styles.td, fontFamily: 'monospace', fontSize: 11 }}>
                        {e.campaign_id ? (
                          <button
                            style={{ ...styles.cellLinkBtn, fontFamily: 'monospace', fontSize: 11 }}
                            title={`Open Campaign Lookup for ${e.campaign_id}`}
                            onClick={(ev) => { ev.stopPropagation(); onOpenCampaign(e.campaign_id); }}
                          >
                            {truncate(e.campaign_id, 14)}
                          </button>
                        ) : '—'}
                      </td>
                      <td style={styles.td}>{e.bounce_cat || '—'}</td>
                      <td style={styles.td}>{e.dsn_code || '—'}</td>
                      <td style={{ ...styles.td, maxWidth: 240 }} title={e.link_url}>
                        {e.link_url ? truncate(e.link_url, 40) : '—'}
                      </td>
                    </tr>
                    {isOpen && (
                      <tr style={styles.tr}>
                        <td colSpan={10} style={styles.expandCell}>
                          <div style={styles.detailGrid}>
                            {EVENT_DETAIL_FIELDS.map((f) => (
                              <div key={f.label}>
                                <div style={styles.metaLabel}>{f.label}</div>
                                <div style={{ ...styles.metaValue, fontFamily: 'monospace', fontSize: 11, wordBreak: 'break-all' }}>
                                  {f.get(e) || '—'}
                                </div>
                              </div>
                            ))}
                          </div>
                        </td>
                      </tr>
                    )}
                  </React.Fragment>
                );
              })}
            </tbody>
          </table>
          <div style={styles.tableFooterNote}>
            Showing {events.length} event{events.length === 1 ? '' : 's'} (limit {limit}).
          </div>
        </div>
      )}
    </div>
  );
};

// ─── Tab 5: Creatives ───────────────────────────────────────────────────────
//
// PG-backed (NOT the Athena lake) creative-performance reporting. For a chosen
// offer it lists every creative × subject × sending-domain combo with money-link
// clicks & conversions, so creative×domain combos are directly comparable.
//
// Backend (both READ ONLY, called via apiFetch):
//   GET /api/mailing/analytics/creatives/offers?start_date=&end_date=
//       → { offers: CreativeOfferOpt[] }   (sorted by money_clicks desc)
//   GET /api/mailing/analytics/creatives?offer=&start_date=&end_date=&view=creative|subject
//       → CreativesReport
// Dates are sent as start_date / end_date (YYYY-MM-DD, America/Denver).

interface CreativeOfferOpt {
  slug: string;
  offer_name: string | null;
  everflow_offer_id: number;
  money_clicks: number;
}

interface CreativeRow {
  creative_key: string;
  creative_label: string;
  subject: string;
  sending_domain: string;
  from_name: string;
  delivered: number;
  clicks: number;
  clickers: number;
  click_rate: number;   // fraction (0–1) — clicks/delivered, approximate
  conversions: number;
  conv_rate: number;    // fraction (0–1) — conversions/clickers (efficiency)
  revenue: number;
  sample_campaign_id: string; // a campaign carrying this creative, for the HTML preview
  has_html: boolean;          // false ⇒ drip reminder (no stored html to preview)
}

interface CreativesReport {
  view: 'creative' | 'subject';
  offer: string;
  from: string;
  to: string;
  delivered_source: string;
  truncated: boolean;
  unattributed_conversions: number;
  rows: CreativeRow[];
}

interface CreativesMeta {
  unattributed_conversions: number;
  truncated: boolean;
  delivered_source: string;
}

type CreativeView = 'creative' | 'subject';

// Sortable numeric columns (left text col sorts on label/subject separately).
// Clickers (unique people) leads over raw Clicks; Conv/clicker is the honest
// efficiency metric. Delivered is marked approximate (campaign-counter rollup);
// the old Click-rate column was dropped — it divided by that approximate
// Delivered and misled.
const CREATIVE_NUM_COLS: Array<{ id: keyof CreativeRow; label: string }> = [
  { id: 'delivered', label: 'Delivered*' },
  { id: 'clicks', label: 'Clicks' },
  { id: 'clickers', label: 'Clickers' },
  { id: 'conversions', label: 'Conversions' },
  { id: 'conv_rate', label: 'Conv/clicker' },
  { id: 'revenue', label: 'Revenue' },
];

const fmtMoney = (n: number): string =>
  (n ?? 0).toLocaleString('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2 });

async function fetchCreativeOffers(
  from: string,
  to: string,
  signal?: AbortSignal
): Promise<CreativeOfferOpt[]> {
  const qs = new URLSearchParams({ start_date: from, end_date: to });
  const res = await apiFetch(`/api/mailing/analytics/creatives/offers?${qs.toString()}`, { signal });
  if (!res.ok) await throwHttpError(res);
  const json: { offers?: CreativeOfferOpt[] } = await res.json();
  return Array.isArray(json.offers) ? json.offers : [];
}

async function fetchCreativesReport(
  offer: string,
  view: CreativeView,
  from: string,
  to: string,
  signal?: AbortSignal
): Promise<CreativesReport> {
  const qs = new URLSearchParams({ offer, start_date: from, end_date: to, view });
  const res = await apiFetch(`/api/mailing/analytics/creatives?${qs.toString()}`, { signal });
  if (!res.ok) await throwHttpError(res);
  const json: CreativesReport = await res.json();
  return { ...json, rows: Array.isArray(json.rows) ? json.rows : [] };
}

interface CreativePreviewData {
  campaign_id: string;
  subject: string;
  from_name: string;
  html: string;
  has_html: boolean;
}

async function fetchCreativePreview(campaignId: string, signal?: AbortSignal): Promise<CreativePreviewData> {
  const qs = new URLSearchParams({ campaign_id: campaignId });
  const res = await apiFetch(`/api/mailing/analytics/creatives/preview?${qs.toString()}`, { signal });
  if (!res.ok) await throwHttpError(res);
  return (await res.json()) as CreativePreviewData;
}

const CreativesTab: React.FC = () => {
  const { addToast } = useToast();

  const [offers, setOffers] = useState<CreativeOfferOpt[]>([]);
  const [offer, setOffer] = useState('');
  const [view, setView] = useState<CreativeView>('creative');
  const [from, setFrom] = useState(daysAgoDenver(13)); // 14-day window inclusive
  const [to, setTo] = useState(denverToday());
  const [rows, setRows] = useState<CreativeRow[]>([]);
  const [meta, setMeta] = useState<CreativesMeta | null>(null);
  const [sort, setSort] = useState<SortState>({ col: 'clickers', dir: 'desc' });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  // Creative HTML preview modal ("see the creative").
  const [preview, setPreview] = useState<{
    open: boolean; loading: boolean; subject: string; from_name: string; html: string; note: string; error: string;
  } | null>(null);
  const previewAbortRef = useRef<AbortController | null>(null);

  const openPreview = useCallback((row: CreativeRow) => {
    previewAbortRef.current?.abort();
    const subject = row.subject || '(no subject)';
    const fromName = row.from_name || '';
    if (!row.has_html || !row.sample_campaign_id) {
      // Two distinct cases: a drip reminder (has a sample campaign but NULL html —
      // it reuses the clicked creative at send time), or a row with no in-range
      // campaign to point at (e.g. a conversion-only row from the full outer join).
      const note = !row.sample_campaign_id
        ? 'No in-range campaign to preview for this row (e.g. a conversion attributed from outside the selected window).'
        : 'This is a drip reminder — it reuses the clicked creative at send time, so there is no stored HTML to preview.';
      setPreview({ open: true, loading: false, subject, from_name: fromName, html: '', note, error: '' });
      return;
    }
    const ctl = new AbortController();
    previewAbortRef.current = ctl;
    setPreview({ open: true, loading: true, subject, from_name: fromName, html: '', note: '', error: '' });
    fetchCreativePreview(row.sample_campaign_id, ctl.signal)
      .then((p) => {
        if (ctl.signal.aborted) return;
        setPreview({ open: true, loading: false, subject: p.subject || subject, from_name: p.from_name || fromName,
          html: p.html || '', note: p.has_html ? '' : 'No stored HTML for this campaign.', error: '' });
      })
      .catch((e) => {
        if (isAbortError(e)) return;
        setPreview({ open: true, loading: false, subject, from_name: fromName, html: '', note: '',
          error: e instanceof Error ? e.message : String(e) });
      });
  }, []);

  const offersAbortRef = useRef<AbortController | null>(null);
  const reportAbortRef = useRef<AbortController | null>(null);
  // Track the selected offer without re-triggering the offers fetch when it changes.
  const offerRef = useRef(offer);
  offerRef.current = offer;

  // ── Load the offers list (on mount + whenever the window changes) ──
  const loadOffers = useCallback(async () => {
    offersAbortRef.current?.abort();
    const ctl = new AbortController();
    offersAbortRef.current = ctl;
    try {
      const list = await fetchCreativeOffers(from, to, ctl.signal);
      if (ctl.signal.aborted) return;
      setOffers(list);
      // Auto-select the highest-volume offer when none is selected (or the
      // current selection vanished from the new window's list).
      const cur = offerRef.current;
      if (list.length > 0 && (!cur || !list.some((o) => o.slug === cur))) {
        setOffer(list[0].slug);
      } else if (list.length === 0) {
        setOffer('');
      }
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      addToast({ type: 'error', title: 'Creative offers list failed', message: msg });
    }
  }, [from, to, addToast]);

  useEffect(() => {
    loadOffers();
    return () => offersAbortRef.current?.abort();
  }, [loadOffers]);

  // ── Load the creatives report (offer/view/from/to) ──
  const loadReport = useCallback(async () => {
    if (!offer) {
      setRows([]);
      setMeta(null);
      setError('');
      return;
    }
    reportAbortRef.current?.abort();
    const ctl = new AbortController();
    reportAbortRef.current = ctl;
    setLoading(true);
    setError('');
    try {
      const rep = await fetchCreativesReport(offer, view, from, to, ctl.signal);
      if (ctl.signal.aborted) return;
      setRows(rep.rows);
      setMeta({
        unattributed_conversions: rep.unattributed_conversions ?? 0,
        truncated: !!rep.truncated,
        delivered_source: rep.delivered_source || '',
      });
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      // Clear stale rows so the error shell isn't shown over an old offer's data.
      setRows([]);
      setMeta(null);
      setError(msg);
      addToast({ type: 'error', title: 'Creative report failed', message: msg });
    } finally {
      if (reportAbortRef.current === ctl) setLoading(false);
    }
  }, [offer, view, from, to, addToast]);

  useEffect(() => {
    loadReport();
    return () => reportAbortRef.current?.abort();
  }, [loadReport]);

  const onSort = (col: string) => setSort((s) =>
    s.col === col ? { col, dir: s.dir === 'desc' ? 'asc' : 'desc' } : { col, dir: 'desc' });

  // Client-side sort. The left text column sorts by label (creative view) or
  // subject (subject view); numeric columns sort by their raw value.
  const sortedRows = useMemo(() => {
    const copy = [...rows];
    const dir = sort.dir === 'desc' ? -1 : 1;
    copy.sort((a, b) => {
      let av: number | string;
      let bv: number | string;
      if (sort.col === '__label') {
        av = view === 'creative' ? a.creative_label : a.subject;
        bv = view === 'creative' ? b.creative_label : b.subject;
      } else if (sort.col === '__domain') {
        av = a.sending_domain;
        bv = b.sending_domain;
      } else {
        av = (a[sort.col as keyof CreativeRow] as number) ?? 0;
        bv = (b[sort.col as keyof CreativeRow] as number) ?? 0;
      }
      if (typeof av === 'string' && typeof bv === 'string') {
        return av.localeCompare(bv) * dir;
      }
      return ((av as number) - (bv as number)) * dir;
    });
    return copy;
  }, [rows, sort, view]);

  const totals = useMemo(() => rows.reduce(
    (acc, r) => {
      acc.delivered += r.delivered;
      acc.clicks += r.clicks;
      acc.clickers += r.clickers;
      acc.conversions += r.conversions;
      acc.revenue += r.revenue;
      return acc;
    },
    { delivered: 0, clicks: 0, clickers: 0, conversions: 0, revenue: 0 }
  ), [rows]);

  const sortIndicator = (col: string) => (sort.col === col ? (sort.dir === 'desc' ? ' ↓' : ' ↑') : '');
  const today = denverToday();

  return (
    <div style={styles.panel}>
      <div style={styles.panelHeader}>
        <div>
          <h2 style={styles.panelTitle}>
            <FontAwesomeIcon icon={faImages} style={{ marginRight: 8, color: COLORS.accentAlt }} />
            Creative Performance
          </h2>
          <p style={styles.panelSubtitle}>
            Clicks = money-link clicks (source_id=email); <b>Clickers</b> = unique people (the trustworthy
            denominator — raw clicks include repeat-clicks &amp; unflagged bots). Conversions use last-click
            attribution (~58% tie to a money click; the rest shown as Unattributed). <b>Conv/clicker</b> =
            conversions ÷ clickers. Delivered* is approximate (campaign counters, under-counts SES). Click any
            creative to preview its HTML. Days are America/Denver.
          </p>
        </div>
        <button style={styles.refreshBtn} onClick={() => { loadOffers(); loadReport(); }} disabled={loading}>
          <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} /> Refresh
        </button>
      </div>

      {/* ── Controls ── */}
      <div style={styles.eventFilterBar}>
        <label style={styles.fieldLabel}>Offer
          <select
            value={offer}
            onChange={(e) => setOffer(e.target.value)}
            style={{ ...styles.input, width: 320 }}
          >
            {offers.length === 0 && <option value="">(no offers in range)</option>}
            {offers.map((o) => (
              <option key={o.slug} value={o.slug}>
                {(o.offer_name ?? `(unmapped) ${o.slug}`) + ` (${fmt(o.money_clicks)})`}
              </option>
            ))}
          </select>
        </label>

        <label style={styles.fieldLabel}>View
          <div style={styles.segmented}>
            {(['creative', 'subject'] as CreativeView[]).map((v, i) => {
              const active = view === v;
              return (
                <button
                  key={v}
                  style={{
                    ...styles.segmentedBtn,
                    borderRight: i === 0 ? `1px solid ${COLORS.border}` : 'none',
                    color: active ? COLORS.accent : COLORS.textSecondary,
                    background: active ? COLORS.accent + '14' : 'transparent',
                    fontWeight: active ? 700 : 500,
                  }}
                  onClick={() => setView(v)}
                >
                  {v === 'creative' ? 'Creative' : 'Subject'}
                </button>
              );
            })}
          </div>
        </label>

        <label style={styles.fieldLabel}>From
          <input type="date" value={from} max={to} onChange={(e) => setFrom(e.target.value)} style={styles.input} />
        </label>
        <label style={styles.fieldLabel}>To
          <input type="date" value={to} min={from} max={today} onChange={(e) => setTo(e.target.value)} style={styles.input} />
        </label>
        <button style={styles.primaryBtn} onClick={() => loadReport()} disabled={loading || !offer}>
          <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} /> Refresh report
        </button>
      </div>

      {/* ── Unattributed badge + truncation ── */}
      {meta && meta.unattributed_conversions > 0 && (
        <div style={{
          padding: '8px 12px', marginBottom: 12, borderRadius: 8,
          background: COLORS.accent + '12', border: `1px solid ${COLORS.accent}33`,
          color: COLORS.textSecondary, fontSize: 12,
          display: 'inline-flex', alignItems: 'center', gap: 8,
        }}>
          <FontAwesomeIcon icon={faInfoCircle} style={{ color: COLORS.accent }} />
          {fmt(meta.unattributed_conversions)} conversion{meta.unattributed_conversions === 1 ? '' : 's'} could not be tied to a money click — not shown below.
        </div>
      )}
      <TruncationBanner truncated={!!meta?.truncated} limit={5000} />

      {/* ── Body ── */}
      {error ? (
        <div style={styles.errorShell}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          Creative report failed: {error}
          <button style={styles.inlineRetry} onClick={() => loadReport()}>Retry</button>
        </div>
      ) : loading ? (
        <LoadingRow label="Loading creative performance…" />
      ) : !offer ? (
        <EmptyRow label="No offers had money-link clicks in this range." />
      ) : sortedRows.length === 0 ? (
        <EmptyRow label="No creatives for this offer in the selected range." />
      ) : (
        <div style={styles.tableWrap}>
          <table style={styles.table}>
            <thead>
              <tr>
                <th style={{ ...styles.th, textAlign: 'left', cursor: 'pointer' }} onClick={() => onSort('__label')}>
                  {view === 'creative' ? 'Creative' : 'Subject'}{sortIndicator('__label')}
                </th>
                <th style={{ ...styles.th, textAlign: 'left', cursor: 'pointer' }} onClick={() => onSort('__domain')}>
                  Sending domain{sortIndicator('__domain')}
                </th>
                {CREATIVE_NUM_COLS.map((c) => (
                  <th key={c.id} style={{ ...styles.th, textAlign: 'right', cursor: 'pointer' }} onClick={() => onSort(c.id)}>
                    {c.label}{sortIndicator(c.id)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {sortedRows.map((r, idx) => {
                // For the creative view, draw a subtle top rule whenever the
                // creative_key changes so the operator scans one creative across
                // its sending domains. (Only meaningful when not sorted away from
                // the natural grouping, but harmless otherwise.)
                const prev = idx > 0 ? sortedRows[idx - 1] : null;
                const groupBreak = view === 'creative' && prev !== null && prev.creative_key !== r.creative_key;
                const rowStyle: React.CSSProperties = {
                  ...styles.tr,
                  ...(groupBreak ? { borderTop: `2px solid ${COLORS.borderStrong}` } : {}),
                };
                const leftLabel = view === 'creative' ? r.creative_label : r.subject;
                return (
                  <tr key={`${r.creative_key}|${r.sending_domain}|${idx}`} style={rowStyle}>
                    <td style={{
                      ...styles.td, maxWidth: 380,
                      borderLeft: view === 'creative' ? `2px solid ${COLORS.accentAlt}33` : undefined,
                    }} title={leftLabel}>
                      <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
                        <span style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{leftLabel || '—'}</span>
                        {r.from_name ? (
                          <span style={{ fontSize: 11, color: COLORS.textMuted }}>from: {r.from_name}</span>
                        ) : null}
                        <button
                          onClick={() => openPreview(r)}
                          style={{
                            background: 'none', border: 'none', padding: 0, cursor: 'pointer',
                            color: r.has_html ? COLORS.accent : COLORS.textMuted, fontSize: 11,
                            textAlign: 'left', width: 'fit-content',
                          }}
                        >
                          <FontAwesomeIcon icon={faImages} style={{ marginRight: 4 }} />
                          {r.has_html ? 'View creative' : 'Drip — no stored html'}
                        </button>
                      </div>
                    </td>
                    <td style={{ ...styles.td, whiteSpace: 'nowrap', color: COLORS.textSecondary }} title={r.sending_domain}>
                      {r.sending_domain || '—'}
                    </td>
                    <td style={{ ...styles.td, textAlign: 'right', color: COLORS.textMuted }} title="approximate (campaign counters)">{fmt(r.delivered)}</td>
                    <td style={{ ...styles.td, textAlign: 'right', color: CLICK_VIOLET }}>{fmt(r.clicks)}</td>
                    <td style={{ ...styles.td, textAlign: 'right', color: COLORS.textSecondary }}>{fmt(r.clickers)}</td>
                    <td style={{ ...styles.td, textAlign: 'right', color: COLORS.good, fontWeight: r.conversions > 0 ? 700 : 400 }}>{fmt(r.conversions)}</td>
                    <td style={{ ...styles.td, textAlign: 'right', color: COLORS.textSecondary }}>{fmtPct(r.conv_rate * 100)}</td>
                    <td style={{ ...styles.td, textAlign: 'right' }}>{fmtMoney(r.revenue)}</td>
                  </tr>
                );
              })}
            </tbody>
            <tfoot>
              <tr style={{ ...styles.tr, borderTop: `2px solid ${COLORS.borderStrong}` }}>
                <td style={{ ...styles.td, fontWeight: 700, color: COLORS.textPrimary }}>Total</td>
                <td style={{ ...styles.td, color: COLORS.textMuted }}>{sortedRows.length} row{sortedRows.length === 1 ? '' : 's'}</td>
                <td style={{ ...styles.td, textAlign: 'right', color: COLORS.textMuted }}>{fmt(totals.delivered)}</td>
                <td style={{ ...styles.td, textAlign: 'right', fontWeight: 700, color: CLICK_VIOLET }}>{fmt(totals.clicks)}</td>
                <td style={{ ...styles.td, textAlign: 'right', fontWeight: 700 }}>{fmt(totals.clickers)}</td>
                <td style={{ ...styles.td, textAlign: 'right', fontWeight: 700, color: COLORS.good }}>{fmt(totals.conversions)}</td>
                <td style={{ ...styles.td, textAlign: 'right', color: COLORS.textMuted }}>
                  {fmtPct(totals.clickers > 0 ? (totals.conversions / totals.clickers) * 100 : null)}
                </td>
                <td style={{ ...styles.td, textAlign: 'right', fontWeight: 700 }}>{fmtMoney(totals.revenue)}</td>
              </tr>
            </tfoot>
          </table>
          <div style={styles.tableFooterNote}>
            *Delivered is approximate — summed from campaign counters (under-counts SES relay; a campaign carrying
            multiple offers counts once per offer). Clicks = raw money-link clicks; Clickers = unique people;
            Conv/clicker = conversions ÷ clickers (last-click attributed). Click each creative to preview its HTML.
            {meta?.delivered_source ? ` Source: ${meta.delivered_source}.` : ''}
          </div>
        </div>
      )}

      {/* ── Creative HTML preview modal ("see the creative") ── */}
      {preview?.open && (
        <div
          onClick={() => setPreview(null)}
          style={{
            position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.7)', zIndex: 1000,
            display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24,
          }}
        >
          <div onClick={(e) => e.stopPropagation()} style={{
            background: COLORS.bgDeep, border: `1px solid ${COLORS.borderStrong}`, borderRadius: 10,
            width: 'min(760px, 95vw)', maxHeight: '92vh', display: 'flex', flexDirection: 'column', overflow: 'hidden',
          }}>
            <div style={{ padding: '12px 16px', borderBottom: `1px solid ${COLORS.borderStrong}`, display: 'flex', alignItems: 'flex-start', gap: 12 }}>
              <div style={{ flex: 1 }}>
                <div style={{ fontWeight: 700, color: COLORS.textPrimary, fontSize: 14 }}>{preview.subject || '(no subject)'}</div>
                {preview.from_name ? <div style={{ fontSize: 12, color: COLORS.textMuted, marginTop: 2 }}>from: {preview.from_name}</div> : null}
              </div>
              <button onClick={() => setPreview(null)} style={{ background: 'none', border: 'none', color: COLORS.textSecondary, cursor: 'pointer', fontSize: 20, lineHeight: 1 }}>×</button>
            </div>
            <div style={{ flex: 1, overflow: 'auto', background: '#fff', minHeight: 200 }}>
              {preview.loading ? (
                <div style={{ padding: 40, textAlign: 'center', color: '#666' }}>
                  <FontAwesomeIcon icon={faSpinner} spin /> Loading creative…
                </div>
              ) : preview.error ? (
                <div style={{ padding: 24, color: HARD_RED }}>Preview failed: {preview.error}</div>
              ) : preview.note ? (
                <div style={{ padding: 24, color: '#444', fontSize: 13 }}>{preview.note}</div>
              ) : (
                <iframe
                  title="creative-preview"
                  sandbox=""
                  srcDoc={preview.html}
                  style={{ width: '100%', height: '72vh', border: 'none', background: '#fff' }}
                />
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// MAIN
// ═══════════════════════════════════════════════════════════════════════════

const TABS: Array<{ id: TabId; label: string; icon: typeof faChartLine }> = [
  { id: 'overview', label: 'Overview', icon: faChartLine },
  { id: 'dimensions', label: 'Dimensions', icon: faLayerGroup },
  { id: 'campaign', label: 'Campaign Lookup', icon: faBullseye },
  { id: 'raw', label: 'Raw Events', icon: faTable },
  { id: 'creatives', label: 'Creatives', icon: faImages },
];

export const EventLakeExplorer: React.FC = () => {
  const { addToast } = useToast();

  const [status, setStatus] = useState<LakeStatus | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState('');

  const [activeTab, setActiveTab] = useState<TabId>('overview');
  const [visited, setVisited] = useState<Set<TabId>>(() => new Set<TabId>(['overview']));

  const [draft, setDraft] = useState<DraftFilters>(() => ({
    from: daysAgoDenver(6), to: denverToday(), ispGroup: '', brand: '', routeType: '', transport: 'combined',
  }));
  const [applied, setApplied] = useState<AppliedFilters>(() => ({
    from: daysAgoDenver(6), to: denverToday(), ispGroup: '', brand: '', routeType: '', transport: 'combined', nonce: 0,
  }));

  const [lookupReq, setLookupReq] = useState<{ id: string; nonce: number } | null>(null);

  const readEnabled = !!status?.enabled_read;

  const fetchStatus = useCallback(async () => {
    setStatusLoading(true);
    try {
      const res = await apiFetch('/api/mailing/analytics/lake/status');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: LakeStatus = await res.json();
      setStatus(json);
      setStatusError('');
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setStatusError(msg);
      addToast({ type: 'error', title: 'Event reporting status failed', message: msg });
    } finally {
      setStatusLoading(false);
    }
  }, [addToast]);

  // On mount: status only. All lake queries are gated on enabled_read.
  useEffect(() => { fetchStatus(); }, [fetchStatus]);

  const selectTab = useCallback((id: TabId) => {
    setActiveTab(id);
    setVisited((v) => {
      if (v.has(id)) return v;
      const next = new Set(v);
      next.add(id);
      return next;
    });
  }, []);

  // Explicit Run: apply the draft and bump nonce so the module cache is bypassed.
  const onRun = useCallback((next: DraftFilters) => {
    setApplied((prev) => ({ ...next, nonce: prev.nonce + 1 }));
  }, []);

  // Cross-tab: Raw Events campaign_id → Campaign Lookup with auto-run.
  const openCampaign = useCallback((id: string) => {
    setLookupReq((prev) => ({ id, nonce: (prev?.nonce ?? 0) + 1 }));
    selectTab('campaign');
  }, [selectTab]);

  // ── Loading shell (initial status fetch) ──
  if (statusLoading && !status) {
    return (
      <div style={styles.loadingShell}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading event reporting status…
      </div>
    );
  }

  return (
    <div style={styles.page}>
      {/* ─── Header ──────────────────────────────────────────────── */}
      <div style={styles.header}>
        <div>
          <h1 style={styles.title}>
            <FontAwesomeIcon icon={faDatabase} style={{ color: COLORS.accent, marginRight: 10 }} />
            Event Explorer
          </h1>
          <p style={styles.subtitle}>
            Decision-grade analytics over the email-event reporting database — per-recipient sends, bounces,
            suppressions, opens and clicks. Counts are de-duplicated per event; every rate discloses its denominator.
          </p>
        </div>
        <button style={styles.refreshBtn} onClick={fetchStatus} disabled={statusLoading}>
          <FontAwesomeIcon icon={statusLoading ? faSpinner : faSyncAlt} spin={statusLoading} /> Refresh status
        </button>
      </div>

      {statusError && (
        <div style={styles.errorShell}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          Failed to load status: {statusError}
          <button style={styles.inlineRetry} onClick={fetchStatus}>Retry</button>
        </div>
      )}

      {/* ─── Status strip ───────────────────────────────────────── */}
      {status && (
        <div style={{ marginBottom: 20 }}>
          <div style={{ ...styles.subPanelTitle, marginBottom: 4 }}>Analytics lake ingestion</div>
          <div style={{ fontSize: 11, color: COLORS.textMuted, marginBottom: 10 }}>
            Live Firehose pipeline feeding this screen — if writing is off, the numbers below go stale.
          </div>
          <div style={{ ...styles.statusStrip, marginBottom: 0 }}>
            <EnableBadge label="Writing events" enabled={status.enabled_write} />
            <EnableBadge label="Read layer" enabled={status.enabled_read} />
            <div style={styles.statusDivider} />
            <Counter label="Ingested" value={status.sent} color={COLORS.good} />
            <Counter label="Failed" value={status.failed} color={COLORS.danger} />
            <Counter label="Dropped" value={status.dropped} color={COLORS.warn} />
          </div>
        </div>
      )}

      {/* ─── DARK empty state ──────────────────────────────────── */}
      {/* Even when the Athena read layer is off the Creatives tab still works
          (it is PG-backed, not lake-backed), so the dark card now reads as a
          notice scoped to the lake tabs rather than the whole screen. */}
      {status && !readEnabled && (
        <div style={styles.darkCard}>
          <FontAwesomeIcon icon={faMoon} style={{ fontSize: 28, color: COLORS.accentAlt }} />
          <div style={{ flex: 1 }}>
            <div style={styles.darkTitle}>Event reporting read layer is off</div>
            <div style={styles.darkBody}>
              The analytics database read layer is not configured. Enable the analytics results location on the
              server to turn on breakdown &amp; event queries. Until then the lake tabs show the write-side
              counters above; their query controls stay disabled. The Creatives tab is database-backed and works
              regardless.
            </div>
            <div style={styles.darkHint}>
              <FontAwesomeIcon icon={faInfoCircle} style={{ color: COLORS.textMuted, marginRight: 6 }} />
              Write side {status.enabled_write ? 'is recording events to the analytics pipeline' : 'is also off (analytics pipeline not configured)'}.
            </div>
          </div>
        </div>
      )}

      {/* ─── Tab nav (always rendered once status loads) ───────── */}
      {/* The nav strip is NOT gated on readEnabled so the PG-backed Creatives
          tab stays reachable when the lake read layer is dark. */}
      {status && (
        <div style={styles.tabNav}>
          {TABS.map((t) => {
            const active = activeTab === t.id;
            return (
              <button
                key={t.id}
                style={{
                  ...styles.tabBtn,
                  color: active ? COLORS.textPrimary : COLORS.textSecondary,
                  borderBottomColor: active ? COLORS.accent : 'transparent',
                  background: active ? 'rgba(129,140,248,0.07)' : 'transparent',
                }}
                onClick={() => selectTab(t.id)}
              >
                <FontAwesomeIcon icon={t.icon} style={{ marginRight: 8, color: active ? COLORS.accent : COLORS.textMuted }} />
                {t.label}
              </button>
            );
          })}
        </div>
      )}

      {/* ─── Lake tabs + their Toolbar (only when read enabled) ── */}
      {status && readEnabled && (
        <>
          <FilterBar draft={draft} setDraft={setDraft} applied={applied} onRun={onRun}
            activeLabel="Active (Overview & Dimensions):" />

          {/* Visited tabs stay mounted (state + cache preserved); only the active one is visible. */}
          {visited.has('overview') && (
            <div style={{ display: activeTab === 'overview' ? 'block' : 'none' }}>
              <OverviewTab applied={applied} />
            </div>
          )}
          {visited.has('dimensions') && (
            <div style={{ display: activeTab === 'dimensions' ? 'block' : 'none' }}>
              <DimensionsTab applied={applied} />
            </div>
          )}
          {visited.has('campaign') && (
            <div style={{ display: activeTab === 'campaign' ? 'block' : 'none' }}>
              <CampaignTab request={lookupReq} />
            </div>
          )}
          {visited.has('raw') && (
            <div style={{ display: activeTab === 'raw' ? 'block' : 'none' }}>
              <RawEventsTab onOpenCampaign={openCampaign} />
            </div>
          )}
        </>
      )}

      {/* ─── Creatives tab — PG-backed, NOT gated on readEnabled ── */}
      {status && visited.has('creatives') && (
        <div style={{ display: activeTab === 'creatives' ? 'block' : 'none' }}>
          <CreativesTab />
        </div>
      )}

      {/* ─── Footer / version stripe ───────────────────────────── */}
      <div style={styles.footer}>
        <span>Page: Event Explorer v{PAGE_VERSION}</span>
        <span>Source: analytics database (email events)</span>
        <span>Read {readEnabled ? 'enabled' : 'off'}</span>
      </div>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// STYLES
// ═══════════════════════════════════════════════════════════════════════════

const styles: Record<string, React.CSSProperties> = {
  page: {
    padding: '24px 28px 64px',
    background: COLORS.bgDeep,
    minHeight: '100vh',
    color: COLORS.textPrimary,
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif',
  },
  loadingShell: {
    display: 'flex', alignItems: 'center', justifyContent: 'center',
    height: '50vh', color: COLORS.textSecondary, gap: 10, fontSize: 14,
    background: COLORS.bgDeep,
  },
  errorShell: {
    margin: '0 0 20px', padding: 16, background: COLORS.bgPanel, borderRadius: 8,
    color: COLORS.danger, display: 'flex', alignItems: 'center', gap: 8,
    border: `1px solid ${COLORS.danger}33`,
  },
  inlineRetry: {
    marginLeft: 'auto', background: 'transparent', color: COLORS.danger,
    border: `1px solid ${COLORS.danger}55`, padding: '4px 12px',
    borderRadius: 6, fontSize: 12, cursor: 'pointer',
  },
  header: {
    display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start',
    marginBottom: 20, gap: 16,
  },
  title: {
    margin: 0, fontSize: 24, fontWeight: 700, color: COLORS.textPrimary,
    letterSpacing: -0.3, display: 'flex', alignItems: 'center',
  },
  subtitle: {
    margin: '6px 0 0', fontSize: 13, color: COLORS.textSecondary, maxWidth: 820, lineHeight: 1.5,
  },
  code: {
    fontFamily: 'monospace', fontSize: 12, color: COLORS.accentAlt,
    background: 'rgba(167,139,250,0.1)', padding: '1px 6px', borderRadius: 4,
  },
  refreshBtn: {
    background: COLORS.bgPanel, color: COLORS.textPrimary,
    border: `1px solid ${COLORS.borderStrong}`, padding: '8px 16px',
    borderRadius: 6, fontSize: 13, cursor: 'pointer',
    display: 'flex', alignItems: 'center', gap: 8, height: 36, whiteSpace: 'nowrap',
  },
  statusStrip: {
    display: 'flex', alignItems: 'center', gap: 16, flexWrap: 'wrap',
    padding: '14px 18px', background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`, borderRadius: 10, marginBottom: 20,
  },
  statusDivider: {
    width: 1, height: 32, background: COLORS.borderStrong, margin: '0 4px',
  },
  badge: {
    display: 'inline-flex', alignItems: 'center', gap: 6,
    padding: '5px 12px', borderRadius: 999, fontSize: 12, fontWeight: 600,
    border: '1px solid', textTransform: 'uppercase', letterSpacing: 0.4,
  },
  counter: { display: 'flex', flexDirection: 'column', alignItems: 'flex-start' },
  counterValue: { fontSize: 22, fontWeight: 700, lineHeight: 1, fontVariantNumeric: 'tabular-nums' },
  counterLabel: {
    fontSize: 10, color: COLORS.textMuted, textTransform: 'uppercase',
    letterSpacing: 0.6, marginTop: 4,
  },
  darkCard: {
    display: 'flex', gap: 18, alignItems: 'flex-start',
    padding: 24, background: COLORS.bgPanel,
    border: `1px solid ${COLORS.borderStrong}`, borderRadius: 12,
    marginBottom: 24,
  },
  darkTitle: { fontSize: 16, fontWeight: 700, color: COLORS.textPrimary, marginBottom: 6 },
  darkBody: { fontSize: 13, color: COLORS.textSecondary, lineHeight: 1.6, maxWidth: 760 },
  darkHint: { fontSize: 12, color: COLORS.textMuted, marginTop: 12 },

  // ── Toolbar ── (toolbar/toolbarRow/presetRow/chip/chipX moved to
  // shared/filters.tsx with the FilterBar; the keys below are still used by
  // the per-tab filter rows)
  presetBtn: {
    border: '1px solid', borderRadius: 999, padding: '6px 12px',
    fontSize: 12, fontWeight: 600, cursor: 'pointer', background: 'transparent',
    whiteSpace: 'nowrap',
  },
  chipRow: {
    display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginTop: 12,
  },
  chipRowLabel: {
    fontSize: 11, color: COLORS.textMuted, textTransform: 'uppercase', letterSpacing: 0.5,
  },
  recentChip: {
    display: 'inline-flex', alignItems: 'center',
    background: 'transparent', border: `1px solid ${COLORS.borderStrong}`,
    color: COLORS.textSecondary, borderRadius: 999, padding: '4px 10px',
    fontSize: 11, fontFamily: 'monospace', cursor: 'pointer',
  },

  // ── Campaign picker dropdown ──
  pickerDropdown: {
    position: 'absolute', top: '100%', left: 0, zIndex: 30,
    width: 420, maxHeight: 320, overflowY: 'auto', marginTop: 4,
    background: COLORS.bgPanelAlt, border: `1px solid ${COLORS.borderStrong}`,
    borderRadius: 8, boxShadow: '0 8px 24px rgba(0,0,0,0.5)',
  },
  pickerItem: {
    padding: '8px 12px', cursor: 'pointer',
    borderBottom: `1px solid ${COLORS.border}`,
  },
  pickerNote: {
    padding: '12px 14px', fontSize: 12, color: COLORS.textMuted,
    display: 'flex', alignItems: 'center', gap: 8,
  },

  // ── Tab nav ──
  tabNav: {
    display: 'flex', gap: 4, borderBottom: `1px solid ${COLORS.borderStrong}`,
    marginBottom: 20,
  },
  tabBtn: {
    background: 'transparent', border: 'none',
    borderBottom: '2px solid transparent',
    padding: '10px 18px', fontSize: 13, fontWeight: 600, cursor: 'pointer',
    borderTopLeftRadius: 6, borderTopRightRadius: 6,
  },

  // ── Panels ──
  panel: {
    background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`, borderRadius: 10,
    padding: 20, marginBottom: 20,
  },
  panelHeader: {
    display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start',
    marginBottom: 16, gap: 16, flexWrap: 'wrap',
  },
  panelTitle: { margin: 0, fontSize: 16, fontWeight: 600, color: COLORS.textPrimary },
  panelSubtitle: { margin: '4px 0 0', fontSize: 12, color: COLORS.textSecondary, maxWidth: 820, lineHeight: 1.5 },
  subPanelTitle: {
    fontSize: 12, fontWeight: 700, color: COLORS.textSecondary,
    textTransform: 'uppercase', letterSpacing: 0.6, marginBottom: 10,
  },
  timingNote: { fontSize: 11, color: COLORS.textMuted, fontVariantNumeric: 'tabular-nums' },
  truncBanner: {
    padding: '10px 14px', marginBottom: 14, borderRadius: 8,
    background: 'rgba(245,158,11,0.10)', border: `1px solid ${SOFT_AMBER}44`,
    color: SOFT_AMBER, fontSize: 12, display: 'flex', alignItems: 'center',
  },
  eventFilterBar: {
    display: 'flex', alignItems: 'flex-end', gap: 12, flexWrap: 'wrap',
    padding: '14px 16px', background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.border}`, borderRadius: 8, marginBottom: 16,
  },
  dimPickerRow: {
    display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 14,
  },
  fieldLabel: {
    display: 'flex', flexDirection: 'column', gap: 4,
    fontSize: 10, color: COLORS.textMuted, textTransform: 'uppercase', letterSpacing: 0.5,
  },
  input: {
    background: COLORS.bgDeep, color: COLORS.textPrimary,
    border: `1px solid ${COLORS.borderStrong}`, borderRadius: 6,
    padding: '7px 10px', fontSize: 13, outline: 'none',
  },
  segmented: {
    display: 'inline-flex', background: COLORS.bgDeep,
    border: `1px solid ${COLORS.borderStrong}`, borderRadius: 6, overflow: 'hidden',
  },
  segmentedBtn: {
    border: 'none', borderRight: `1px solid ${COLORS.border}`,
    padding: '7px 12px', fontSize: 13, cursor: 'pointer',
    outline: 'none', whiteSpace: 'nowrap',
  },
  primaryBtn: {
    background: COLORS.accent, color: '#0a0e1a',
    border: 'none', padding: '8px 16px', borderRadius: 6,
    fontSize: 13, fontWeight: 600, cursor: 'pointer',
    display: 'flex', alignItems: 'center', gap: 8, height: 34,
  },
  sectionLoading: {
    display: 'flex', alignItems: 'center', justifyContent: 'center',
    gap: 8, padding: '32px 0', color: COLORS.textSecondary, fontSize: 13,
  },
  sectionEmpty: {
    padding: '32px 0', textAlign: 'center', color: COLORS.textMuted, fontSize: 13,
  },

  // ── KPI strip ──
  kpiGrid: {
    display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(170px, 1fr))', gap: 12,
  },
  kpiCard: {
    padding: 14, background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.border}`, borderRadius: 8,
  },
  kpiLabel: {
    fontSize: 10, color: COLORS.textMuted, textTransform: 'uppercase', letterSpacing: 0.6,
  },
  kpiValue: { fontSize: 24, fontWeight: 700, marginTop: 6, fontVariantNumeric: 'tabular-nums' },
  kpiRate: { fontSize: 12, fontWeight: 600, marginTop: 4, fontVariantNumeric: 'tabular-nums' },
  kpiDenom: { fontSize: 10, color: COLORS.textMuted, marginTop: 3, lineHeight: 1.4 },

  // ── Series toggles + chart tooltip ──
  seriesToggleRow: { display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 10 },
  seriesToggle: {
    display: 'inline-flex', alignItems: 'center', gap: 6,
    border: '1px solid', borderRadius: 999, padding: '4px 10px',
    fontSize: 11, fontWeight: 600, cursor: 'pointer',
  },
  chartTip: {
    background: '#0d1226', border: `1px solid ${COLORS.borderStrong}`,
    borderRadius: 8, padding: '10px 12px', fontSize: 12, minWidth: 170,
    boxShadow: '0 8px 24px rgba(0,0,0,0.5)',
  },
  chartTipTitle: { color: COLORS.textPrimary, fontWeight: 700, marginBottom: 6, fontVariantNumeric: 'tabular-nums' },
  chartTipRow: { display: 'flex', alignItems: 'center', gap: 8, padding: '2px 0' },
  chartTipDot: { width: 8, height: 8, borderRadius: 999, flexShrink: 0 },

  // ── Tables ──
  tableWrap: { overflowX: 'auto' },
  table: { width: '100%', borderCollapse: 'collapse', fontSize: 13 },
  th: {
    padding: '8px 12px', color: COLORS.textMuted,
    fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
    borderBottom: `1px solid ${COLORS.borderStrong}`, fontWeight: 600, whiteSpace: 'nowrap',
    userSelect: 'none',
  },
  tr: { borderBottom: `1px solid ${COLORS.border}` },
  td: {
    padding: '9px 12px', color: COLORS.textPrimary, textAlign: 'left',
    overflow: 'hidden', textOverflow: 'ellipsis', fontVariantNumeric: 'tabular-nums',
  },
  typeChip: {
    display: 'inline-block', padding: '2px 8px', borderRadius: 999,
    border: '1px solid', fontSize: 11, fontWeight: 600, whiteSpace: 'nowrap',
  },
  cellLinkBtn: {
    background: 'transparent', border: 'none', color: COLORS.accent,
    cursor: 'pointer', fontSize: 13, padding: 0, textDecoration: 'underline',
    textDecorationColor: COLORS.accent + '44', textUnderlineOffset: 3,
  },
  tableFooterNote: { padding: '10px 4px 0', fontSize: 11, color: COLORS.textMuted, lineHeight: 1.5 },
  expandCell: {
    padding: '14px 16px', background: 'rgba(255,255,255,0.02)',
  },
  expandHeader: {
    display: 'flex', alignItems: 'center', gap: 14, flexWrap: 'wrap',
    fontSize: 12, marginBottom: 10,
  },
  expandStat: { color: COLORS.textSecondary, fontVariantNumeric: 'tabular-nums' },
  detailGrid: {
    display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))', gap: 12,
  },

  // ── Campaign lookup ──
  metaGrid: {
    display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))',
    gap: 14, marginTop: 14,
  },
  metaLabel: {
    fontSize: 10, color: COLORS.textMuted, textTransform: 'uppercase', letterSpacing: 0.5,
    marginBottom: 3,
  },
  metaValue: { fontSize: 13, color: COLORS.textPrimary, lineHeight: 1.4, wordBreak: 'break-word' },
  lakeOnlyNotice: {
    padding: '12px 14px', borderRadius: 8,
    background: 'rgba(251,191,36,0.08)', border: `1px solid ${COLORS.warn}33`,
    color: COLORS.textSecondary, fontSize: 13, lineHeight: 1.6,
  },

  footer: {
    marginTop: 32, padding: '8px 4px',
    fontSize: 11, color: COLORS.textMuted,
    borderTop: `1px solid ${COLORS.border}`,
    display: 'flex', gap: 16, flexWrap: 'wrap',
  },
};

export default EventLakeExplorer;
