// Audience Analytics
//
// PAGE_VERSION 1.0 — a true audience-analytics surface (acquisition, churn,
// source performance, growth, member lookup) over the Athena-backed audience
// lake. Sibling screen to EventLakeExplorer.tsx — shares its dark tokens,
// styles-object pattern, capped fetch cache, AbortController discipline,
// heat-tinted sortable matrices and rate conventions.
//
// Backend (all READ ONLY, called via apiFetch, all under
// /api/mailing/analytics/lake/audience):
//   GET /status
//       → { enabled_read, latest_dt, rows }      latest_dt='' = no snapshot yet
//   GET /breakdown?dt=&group_by=a,b&limit=&acquired_from=&acquired_to=
//                 &churned_from=&churned_to=&<equality filters>
//       → { dt, group_by, rows: [{keys, count, avg_engagement}], truncated,
//           empty?, disabled? }
//       group_by dims (1–3): status, isp, email_domain, verification_status,
//         engagement_band, churn_reason, source, data_source, source_system,
//         acquired_dt, churned_dt, list_id. Equality filter params share those
//         names. dt omitted → server uses the latest snapshot.
//   GET /source-performance?dim=&from=&to=&dt=&limit=&<same eq filters>
//       → { dim, from, to, audience_dt, rows: [{value, event_type, count}],
//           truncated, disabled? }
//       dim ∈ {source, data_source, source_system, isp, verification_status,
//         engagement_band, acquired_dt, status}. from/to = email-events window.
//   GET /first-touch?from=&to=
//       → { from, to, rows: [{dt, count}] } — members whose FIRST-ever lake
//         event (attempted/delivered/relayed) fell on that day. "First touch
//         within lake history", NOT lifetime.
//   GET /member?email=&dt=&events_limit=
//       → { email, audience_dt, profiles: AudienceProfile[], events: LakeEvent[] }
//         profiles is per-list rows for the email (can be >1).
//
// Rate conventions (every rate shows or tooltips its denominator):
//   delivery/hard/soft/complaint rate denominator = attempted when attempted>0,
//   else delivered+hard_bounce+soft_bounce (the label says which was used).
//   open_rate = open/delivered · click_rate = click/delivered · CTOR = click/open.
//   HARD RULE: hard bounce is ALWAYS red #ef4444, soft bounce ALWAYS amber
//   #f59e0b, and the two are NEVER summed into a combined "bounces" number.
//
// DATA CAVEAT (footnoted wherever opens/clicks render): open/click events
// exist in the lake only for SES-routed mail and app-tracking backfill —
// open/click columns are DIRECTIONAL, not absolute.
//
// Athena queries cost ~1–3s each, so a module-level in-memory cache (keyed by
// the full query URL + run nonce, FIFO-capped at 64 entries) serves tab flips
// and re-renders; explicit Refresh actions bypass it. Member lookups are only
// ever issued by explicit submits, so they are not cached at all. Each panel
// shows fetch timing and a truncation banner when the server clamps results.

import React, { Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faSyncAlt, faExclamationTriangle, faUsers, faCircle,
  faSearch, faSeedling, faInfoCircle, faChartLine, faLayerGroup,
  faChevronDown, faChevronRight, faUser, faHistory, faInbox, faMoon,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Bar, Line, XAxis, YAxis,
  CartesianGrid, Tooltip,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';
import { useToast } from '../shared/ToastSystem';

const WelcomeAudienceHealth = React.lazy(() =>
  import('./WelcomeAudienceHealth').then((m) => ({ default: m.WelcomeAudienceHealth })));

const PAGE_VERSION = '1.0';

// ═══════════════════════════════════════════════════════════════════════════
// TYPES (match backend JSON keys exactly)
// ═══════════════════════════════════════════════════════════════════════════

interface AudienceLakeStatus {
  enabled_read: boolean;
  latest_dt: string;   // '' when no snapshot has been taken yet
  rows: number;
}

interface AudBreakdownRow {
  keys: Record<string, string>;
  count: number;
  avg_engagement: number;
}

interface AudBreakdownResponse {
  dt?: string;
  group_by?: string[];
  rows: AudBreakdownRow[];
  truncated?: boolean;
  empty?: boolean;
  disabled?: boolean;
}

interface SourcePerfRow {
  value: string;
  event_type: string;
  count: number;
}

interface SourcePerfResponse {
  dim?: string;
  from?: string;
  to?: string;
  audience_dt?: string;
  rows: SourcePerfRow[];
  truncated?: boolean;
  disabled?: boolean;
}

interface FirstTouchRow {
  dt: string;
  count: number;
  // Cohort activation: of the members first-touched on dt, how many have at
  // least one open / click anywhere in lake history. opened/count is the
  // activation rate used to judge acquisition-quality thresholds.
  opened: number;
  clicked: number;
}

interface FirstTouchResponse {
  from?: string;
  to?: string;
  rows: FirstTouchRow[];
  disabled?: boolean;
}

// Per-list audience snapshot row for one email. Empty string = never/unknown.
interface AudienceProfile {
  id: string;
  organization_id: string;
  list_id: string;
  email: string;
  email_domain: string;
  isp: string;
  status: string;
  source: string;
  data_source: string;
  source_system: string;
  source_detail: string;
  acquired_at: string;
  acquired_dt: string;
  imported_at: string;
  created_at: string;
  subscribed_at: string;
  unsubscribed_at: string;
  hard_bounced_at: string;
  complained_at: string;
  churned_dt: string;
  churn_reason: string;
  engagement_score: number;
  engagement_band: string;
  total_opens: number;
  total_clicks: number;
  total_emails_received: number;
  last_open_at: string;
  last_click_at: string;
  last_email_at: string;
  data_quality_score: number;
  verification_status: string;
  is_bot: boolean;
  churn_risk_score: number;
}

// Mirrors internal/analytics/lake_emitter.go Event struct json tags
// (same interface as EventLakeExplorer.tsx — local copy, no cross-import).
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

interface MemberResponse {
  email: string;
  audience_dt?: string;
  profiles: AudienceProfile[];
  events: LakeEvent[];
  disabled?: boolean;
}

// Aggregated email-event counts after classifying event_type buckets.
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
  other: number;         // unknown event types (rendered, never dropped)
  total: number;
}

// Computed rates with explicit denominator provenance.
interface Rates {
  denom: number;
  denomLabel: 'attempted' | 'delivered+bounces';
  delivery: number | null;
  hard: number | null;
  soft: number | null;
  complaint: number | null;
  open: number | null;    // denominator: delivered
  click: number | null;   // denominator: delivered
  ctor: number | null;    // denominator: opens
}

interface SrcMetricRow {
  key: string;
  counts: EventCounts;
  rates: Rates;
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

interface RecentMember {
  email: string;
  at: string; // ISO timestamp of last lookup
}

type TabId = 'overview' | 'sources' | 'growth' | 'member' | 'welcome';

type SortDir = 'asc' | 'desc';
interface SortState { col: string; dir: SortDir }

// ═══════════════════════════════════════════════════════════════════════════
// STYLE TOKENS (identical palette to EventLakeExplorer — sibling screens)
// ═══════════════════════════════════════════════════════════════════════════

const COLORS = {
  bgDeep:        '#0a0e1a',
  bgPanel:       '#0f1424',
  bgPanelAlt:    '#131a2e',
  border:        'rgba(255,255,255,0.06)',
  borderStrong:  'rgba(255,255,255,0.12)',
  textPrimary:   '#e2e8f0',
  textSecondary: '#94a3b8',
  textMuted:     '#64748b',
  accent:        '#818cf8',
  accentAlt:     '#a78bfa',
  accentPink:    '#f472b6',
  good:          '#34d399',
  warn:          '#fbbf24',
  danger:        '#f87171',
};

// HARD RULE colors (repo CLAUDE.md): hard bounce red, soft bounce amber. Always.
const HARD_RED = '#ef4444';
const SOFT_AMBER = '#f59e0b';
const INFO_BLUE = '#60a5fa';
const OPEN_CYAN = '#22d3ee';
const CLICK_VIOLET = '#a78bfa';
const COMPLAINT_ROSE = '#fb7185';
const COLD_SLATE = '#64748b';

// Stacked-series palette for acquisition by data_source (top 6 + other).
const STACK_PALETTE = ['#818cf8', '#22d3ee', '#f472b6', '#34d399', '#fbbf24', '#a78bfa'];
const OTHER_GRAY = '#64748b';

// ═══════════════════════════════════════════════════════════════════════════
// API LAYER — module-level capped cache + abortable fetch helpers
// ═══════════════════════════════════════════════════════════════════════════

const API_BASE = '/api/mailing/analytics/lake/audience';

// Bounded FIFO cache: Map preserves insertion order, so evicting from the
// front drops the oldest entries (including prior-nonce keys). Entries can
// hold thousands of rows each, so the map must not grow unbounded.
const CACHE_MAX_ENTRIES = 64;
const audCache = new Map<string, CachedPayload<unknown>>();

function audCacheSet(key: string, payload: CachedPayload<unknown>): void {
  audCache.set(key, payload);
  while (audCache.size > CACHE_MAX_ENTRIES) {
    const oldest = audCache.keys().next().value;
    if (oldest === undefined) break;
    audCache.delete(oldest);
  }
}

// Parse a non-OK response: error bodies carry {error} which we surface verbatim.
async function throwHttpError(res: Response): Promise<never> {
  let msg = `HTTP ${res.status}`;
  try {
    const body: { error?: string } = await res.json();
    if (body && typeof body.error === 'string' && body.error) msg = body.error;
  } catch { /* non-JSON error body — keep HTTP status message */ }
  throw new Error(msg);
}

async function fetchCachedJSON<T>(
  url: string,
  nonce: number,
  opts?: { signal?: AbortSignal; bypass?: boolean }
): Promise<CachedPayload<T>> {
  const key = `${url}#n${nonce}`;
  if (!opts?.bypass) {
    const hit = audCache.get(key);
    if (hit) return { data: hit.data as T, meta: { ...hit.meta, fromCache: true } };
  }
  const t0 = performance.now();
  const res = await apiFetch(url, { signal: opts?.signal });
  if (!res.ok) await throwHttpError(res);
  const data = (await res.json()) as T;
  const payload: CachedPayload<T> = {
    data,
    meta: { fetchedAt: Date.now(), durationMs: Math.round(performance.now() - t0), fromCache: false },
  };
  audCacheSet(key, payload as CachedPayload<unknown>);
  return payload;
}

interface AudBreakdownParams {
  groupBy: string[];
  limit?: number;
  dt?: string;            // omitted → server uses latest snapshot
  acquiredFrom?: string;
  acquiredTo?: string;
  churnedFrom?: string;
  churnedTo?: string;
  filters?: Record<string, string>;
}

function audBreakdownURL(p: AudBreakdownParams): string {
  const qs = new URLSearchParams();
  if (p.dt) qs.set('dt', p.dt);
  qs.set('group_by', p.groupBy.join(','));
  qs.set('limit', String(p.limit ?? 5000));
  if (p.acquiredFrom) qs.set('acquired_from', p.acquiredFrom);
  if (p.acquiredTo) qs.set('acquired_to', p.acquiredTo);
  if (p.churnedFrom) qs.set('churned_from', p.churnedFrom);
  if (p.churnedTo) qs.set('churned_to', p.churnedTo);
  if (p.filters) {
    // Stable key order so the cache key is deterministic.
    for (const k of Object.keys(p.filters).sort()) {
      const v = (p.filters[k] || '').trim();
      if (v) qs.set(k, v);
    }
  }
  return `${API_BASE}/breakdown?${qs.toString()}`;
}

function normBreakdown(d: AudBreakdownResponse): AudBreakdownResponse {
  return { ...d, rows: Array.isArray(d.rows) ? d.rows : [] };
}

function sourcePerfURL(p: { dim: string; from: string; to: string; limit?: number; filters?: Record<string, string> }): string {
  const qs = new URLSearchParams();
  qs.set('dim', p.dim);
  qs.set('from', p.from);
  qs.set('to', p.to);
  qs.set('limit', String(p.limit ?? 5000));
  if (p.filters) {
    for (const k of Object.keys(p.filters).sort()) {
      const v = (p.filters[k] || '').trim();
      if (v) qs.set(k, v);
    }
  }
  return `${API_BASE}/source-performance?${qs.toString()}`;
}

function firstTouchURL(from: string, to: string): string {
  const qs = new URLSearchParams();
  qs.set('from', from);
  qs.set('to', to);
  return `${API_BASE}/first-touch?${qs.toString()}`;
}

// NOT cached: every /member lookup is an explicit submit, so a cache could
// never serve a hit across renders that matters — and members change daily.
async function fetchMember(
  email: string,
  eventsLimit: number,
  signal?: AbortSignal
): Promise<CachedPayload<MemberResponse>> {
  const qs = new URLSearchParams();
  qs.set('email', email);
  qs.set('events_limit', String(eventsLimit));
  const t0 = performance.now();
  const res = await apiFetch(`${API_BASE}/member?${qs.toString()}`, { signal });
  if (!res.ok) await throwHttpError(res);
  const data = (await res.json()) as MemberResponse;
  return {
    data: {
      ...data,
      profiles: Array.isArray(data.profiles) ? data.profiles : [],
      events: Array.isArray(data.events) ? data.events : [],
    },
    meta: { fetchedAt: Date.now(), durationMs: Math.round(performance.now() - t0), fromCache: false },
  };
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

const todayUTC = () => new Date().toISOString().slice(0, 10);
const daysAgoUTC = (n: number) => new Date(Date.now() - n * 86400000).toISOString().slice(0, 10);

const truncate = (s: string, max: number): string => {
  if (!s) return '';
  return s.length > max ? s.slice(0, max - 1) + '…' : s;
};

// Empty string = never/unknown across all AudienceProfile timestamp fields.
const fmtTs = (s: string): string => {
  if (!s) return '—';
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return s; // already a plain dt like 2026-05-01
  return d.toLocaleString('en-US', { hour12: false, dateStyle: 'short', timeStyle: 'short' });
};

const fmtEventAt = (e: LakeEvent): string => {
  if (e.event_epoch_ms && e.event_epoch_ms > 0) {
    return new Date(e.event_epoch_ms).toLocaleString('en-US', { hour12: false, dateStyle: 'short', timeStyle: 'medium' });
  }
  if (e.event_at) return e.event_at;
  return '—';
};

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const isEmail = (s: string) => EMAIL_RE.test(s.trim());

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
  'delivery_delay', 'complaint', 'open', 'click',
]);

function countsFromTypeMap(m: Record<string, number>): EventCounts {
  const g = (k: string) => m[k] || 0;
  let other = 0;
  let total = 0;
  for (const [k, v] of Object.entries(m)) {
    total += v;
    if (!KNOWN_TYPES.has(k)) other += v; // unknown → 'other' bucket, never dropped
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
    other,
    total,
  };
}

// Rate convention: denominator = attempted when attempted>0, else
// delivered+hard+soft. The label records which was used so every rendered
// rate can disclose its denominator.
function computeRates(c: EventCounts): Rates {
  const useAttempted = c.attempted > 0;
  const denom = useAttempted ? c.attempted : c.delivered + c.hard + c.soft;
  const denomLabel: Rates['denomLabel'] = useAttempted ? 'attempted' : 'delivered+bounces';
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

const denomTitle = (r: Rates): string => `denominator: ${r.denomLabel} (${fmt(r.denom)})`;
const deliveredTitle = (c: EventCounts): string => `denominator: delivered (${fmt(c.delivered)})`;

// Heat tints — subtle background washes flagging out-of-band rates.
// Hard% >1 red / >0.5 amber · Comp% >0.1 / >0.05 · Del% <90 / <97.
const heatHard = (p: number | null): string | undefined =>
  p == null ? undefined : p > 1 ? 'rgba(239,68,68,0.14)' : p > 0.5 ? 'rgba(245,158,11,0.11)' : undefined;
const heatComp = (p: number | null): string | undefined =>
  p == null ? undefined : p > 0.1 ? 'rgba(239,68,68,0.14)' : p > 0.05 ? 'rgba(245,158,11,0.11)' : undefined;
const heatDel = (p: number | null): string | undefined =>
  p == null ? undefined : p < 90 ? 'rgba(239,68,68,0.14)' : p < 97 ? 'rgba(245,158,11,0.11)' : undefined;

// Pivot /source-performance rows (value × event_type) into one metric row per
// dim value + a pinned TOTAL row.
function pivotSourcePerf(rows: SourcePerfRow[]): { rows: SrcMetricRow[]; totals: SrcMetricRow } {
  const byVal = new Map<string, Record<string, number>>();
  const totalMap: Record<string, number> = {};
  for (const r of rows) {
    const k = r.value ?? '';
    const et = (r.event_type ?? '').toLowerCase();
    let m = byVal.get(k);
    if (!m) { m = {}; byVal.set(k, m); }
    m[et] = (m[et] || 0) + r.count;
    totalMap[et] = (totalMap[et] || 0) + r.count;
  }
  const out: SrcMetricRow[] = [];
  byVal.forEach((m, k) => {
    const counts = countsFromTypeMap(m);
    out.push({ key: k, counts, rates: computeRates(counts) });
  });
  const tCounts = countsFromTypeMap(totalMap);
  return { rows: out, totals: { key: 'TOTAL', counts: tCounts, rates: computeRates(tCounts) } };
}

// ── Audience chip colors ────────────────────────────────────────────────────

const statusColor = (s: string): string => {
  const k = (s || '').toLowerCase();
  if (k === 'confirmed' || k === 'active' || k === 'subscribed') return COLORS.good;
  if (k === 'unsubscribed') return COLORS.accent;
  if (k.includes('bounce')) return HARD_RED;
  if (k.includes('complain')) return COMPLAINT_ROSE;
  if (k === 'pending' || k === 'unconfirmed') return COLORS.warn;
  return COLORS.accentAlt;
};

// Engagement band chips: hot=green, warm=amber, cold=slate.
const bandColor = (b: string): string => {
  const k = (b || '').toLowerCase();
  if (k === 'hot') return COLORS.good;
  if (k === 'warm') return COLORS.warn;
  if (k === 'cold') return COLD_SLATE;
  return COLORS.accentAlt;
};

// Churn-reason series: unsubscribed indigo, hard_bounced red, complained rose.
const churnReasonColor = (r: string): string => {
  const k = (r || '').toLowerCase();
  if (k.includes('unsub')) return COLORS.accent;
  if (k.includes('hard') || k.includes('bounce')) return HARD_RED;
  if (k.includes('complain')) return COMPLAINT_ROSE;
  return OTHER_GRAY;
};

// ── Stacked-series builder (acquisition / churn trends) ────────────────────
// Rolls everything beyond the top N series keys (by total volume) into an
// 'other' bucket, computed client-side. Rows with an empty date key are
// excluded from the chart (counted so the footnote can disclose them).

interface StackedSeries {
  data: Array<Record<string, string | number>>;
  keys: string[];                 // series keys in render order (top-N + 'other')
  excludedNoDate: number;         // members whose date key was empty
}

function buildStackedSeries(
  rows: AudBreakdownRow[],
  dateKey: string,
  dimKey: string,
  topN: number
): StackedSeries {
  const dimTotals = new Map<string, number>();
  let excludedNoDate = 0;
  for (const r of rows) {
    if (!(r.keys[dateKey] ?? '')) { excludedNoDate += r.count; continue; }
    const v = r.keys[dimKey] || '(empty)';
    dimTotals.set(v, (dimTotals.get(v) || 0) + r.count);
  }
  const top = Array.from(dimTotals.entries()).sort((a, b) => b[1] - a[1]).slice(0, topN).map(([k]) => k);
  const topSet = new Set(top);
  const hasOther = dimTotals.size > top.length;
  const byDt = new Map<string, Record<string, number>>();
  for (const r of rows) {
    const dt = r.keys[dateKey] ?? '';
    if (!dt) continue;
    const raw = r.keys[dimKey] || '(empty)';
    const series = topSet.has(raw) ? raw : 'other';
    let m = byDt.get(dt);
    if (!m) { m = {}; byDt.set(dt, m); }
    m[series] = (m[series] || 0) + r.count;
  }
  const keys = hasOther ? [...top, 'other'] : top;
  const data = Array.from(byDt.keys()).sort().map((dt) => {
    const m = byDt.get(dt) || {};
    const point: Record<string, string | number> = { dt };
    for (const k of keys) point[k] = m[k] || 0;
    return point;
  });
  return { data, keys, excludedNoDate };
}

const stackColor = (idx: number, key: string): string =>
  key === 'other' ? OTHER_GRAY : STACK_PALETTE[idx % STACK_PALETTE.length];

// ── Recent member lookups (localStorage) ────────────────────────────────────

const RECENT_MEMBERS_KEY = 'audience-recent-members';

function loadRecentMembers(): RecentMember[] {
  try {
    const raw = localStorage.getItem(RECENT_MEMBERS_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed
      .filter((x): x is RecentMember =>
        !!x && typeof x === 'object' &&
        typeof (x as RecentMember).email === 'string' &&
        typeof (x as RecentMember).at === 'string')
      .slice(0, 8);
  } catch {
    return [];
  }
}

function saveRecentMember(email: string): RecentMember[] {
  const next = [{ email, at: new Date().toISOString() },
    ...loadRecentMembers().filter((r) => r.email !== email)].slice(0, 8);
  try { localStorage.setItem(RECENT_MEMBERS_KEY, JSON.stringify(next)); } catch { /* quota — ignore */ }
  return next;
}

// ═══════════════════════════════════════════════════════════════════════════
// SMALL SHARED COMPONENTS
// ═══════════════════════════════════════════════════════════════════════════

const LoadingRow: React.FC<{ label?: string }> = ({ label }) => (
  <div style={styles.sectionLoading}>
    <FontAwesomeIcon icon={faSpinner} spin /> {label || 'Querying Athena…'}
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
      Result truncated at limit {fmt(limit)} — totals below are an undercount. Narrow the window or add filters.
    </div>
  );
};

// Inline error panel with Retry — stale data is always cleared before this renders.
const ErrorPanel: React.FC<{ label: string; error: string; onRetry: () => void }> = ({ label, error, onRetry }) => (
  <div style={styles.errorShell}>
    <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
    {label}: {error}
    <button style={styles.inlineRetry} onClick={onRetry}>Retry</button>
  </div>
);

// {disabled:true} / {empty:true} friendly states (never rendered as errors).
const FriendlyState: React.FC<{ kind: 'disabled' | 'empty' }> = ({ kind }) => (
  <div style={styles.lakeOnlyNotice}>
    <FontAwesomeIcon icon={faInfoCircle} style={{ marginRight: 8, color: COLORS.warn }} />
    {kind === 'disabled'
      ? 'Audience lake read layer is disabled on the server — this panel will populate once it is enabled.'
      : 'The audience snapshot returned no rows for this query.'}
  </div>
);

const RefreshBtn: React.FC<{ loading: boolean; onClick: () => void }> = ({ loading, onClick }) => (
  <button style={styles.refreshBtn} onClick={onClick} disabled={loading}>
    <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} /> Refresh
  </button>
);

// KPI card: big count + optional share/extra notes.
const KpiCard: React.FC<{
  label: string;
  value: number;
  color: string;
  pct?: number | null;
  pctLabel?: string;
  extra?: string;
}> = ({ label, value, color, pct, pctLabel, extra }) => (
  <div style={styles.kpiCard}>
    <div style={styles.kpiLabel}>{label}</div>
    <div style={{ ...styles.kpiValue, color }}>{fmt(value)}</div>
    {pct !== undefined && (
      <div style={{ ...styles.kpiRate, color }}>{pctLabel || 'share'}: {fmtPct(pct, 1)}</div>
    )}
    {extra && <div style={styles.kpiDenom}>{extra}</div>}
  </div>
);

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
            {typeof p.value === 'number' ? fmt(p.value) : String(p.value ?? '—')}
          </span>
        </div>
      ))}
    </div>
  );
};

// Static legend chips for stacked charts.
const StackLegend: React.FC<{ keys: string[] }> = ({ keys }) => (
  <div style={styles.seriesToggleRow}>
    {keys.map((k, i) => {
      const color = stackColor(i, k);
      return (
        <span key={k} style={{ ...styles.legendChip, color, borderColor: color + '66', background: color + '14' }}>
          <FontAwesomeIcon icon={faCircle} style={{ fontSize: 7 }} />
          {k || '(empty)'}
        </span>
      );
    })}
  </div>
);

// Stacked daily bar chart (acquisition / churn / growth).
const StackedBars: React.FC<{
  data: Array<Record<string, string | number>>;
  keys: string[];
  colorFor: (idx: number, key: string) => string;
  height: number;
}> = ({ data, keys, colorFor, height }) => (
  <ResponsiveContainer width="100%" height={height}>
    <ComposedChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
      <CartesianGrid stroke="rgba(255,255,255,0.06)" vertical={false} />
      <XAxis
        dataKey="dt"
        tickFormatter={(v: string) => (typeof v === 'string' ? v.slice(5) : String(v))}
        tick={{ fill: COLORS.textMuted, fontSize: 10 }}
        axisLine={{ stroke: COLORS.borderStrong }}
        tickLine={false}
      />
      <YAxis
        tickFormatter={(v: number) => fmtCompact(v)}
        tick={{ fill: COLORS.textMuted, fontSize: 10 }}
        axisLine={false}
        tickLine={false}
        width={48}
      />
      <Tooltip content={<ChartTip />} cursor={{ fill: 'rgba(255,255,255,0.04)' }} />
      {keys.map((k, i) => (
        <Bar key={k} stackId="stack" dataKey={k} name={k || '(empty)'} fill={colorFor(i, k)}
          fillOpacity={0.8} maxBarSize={28} />
      ))}
    </ComposedChart>
  </ResponsiveContainer>
);

const MetaItem: React.FC<{ label: string; value?: string; mono?: boolean }> = ({ label, value, mono }) => (
  <div>
    <div style={styles.metaLabel}>{label}</div>
    <div style={{ ...styles.metaValue, fontFamily: mono ? 'monospace' : undefined, fontSize: mono ? 12 : 13 }}>
      {value || '—'}
    </div>
  </div>
);

// Shared per-panel fetch state holder — clears stale data on error so old
// numbers never render under new labels.
interface PanelData<T> {
  data: T;
  meta: FetchMeta;
}

// ═══════════════════════════════════════════════════════════════════════════
// TAB 1 — OVERVIEW
// ═══════════════════════════════════════════════════════════════════════════

// ─── KPI strip: status mix + engagement bands (confirmed only) ─────────────

interface KpiStripFetched {
  statusRows: AudBreakdownRow[];
  bandRows: AudBreakdownRow[];
  flags: { disabled: boolean; empty: boolean };
  metaA: FetchMeta;
  metaB: FetchMeta;
}

const OverviewKpiStrip: React.FC = () => {
  const { addToast } = useToast();
  const [fetched, setFetched] = useState<KpiStripFetched | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async (bypass: boolean) => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    try {
      const [a, b] = await Promise.all([
        fetchCachedJSON<AudBreakdownResponse>(
          audBreakdownURL({ groupBy: ['status'], limit: 1000 }), 0, { signal: ctl.signal, bypass }),
        fetchCachedJSON<AudBreakdownResponse>(
          audBreakdownURL({ groupBy: ['engagement_band'], limit: 1000, filters: { status: 'confirmed' } }),
          0, { signal: ctl.signal, bypass }),
      ]);
      const da = normBreakdown(a.data);
      const db = normBreakdown(b.data);
      setFetched({
        statusRows: da.rows,
        bandRows: db.rows,
        flags: { disabled: !!(da.disabled || db.disabled), empty: !!(da.empty && db.empty) },
        metaA: a.meta,
        metaB: b.meta,
      });
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      setFetched(null); // clear stale data — never stale numbers under new labels
      setError(msg);
      addToast({ type: 'error', title: 'Audience KPI query failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [addToast]);

  useEffect(() => {
    load(false);
    return () => abortRef.current?.abort();
  }, [load]);

  const derived = useMemo(() => {
    if (!fetched) return null;
    const total = fetched.statusRows.reduce((a, r) => a + r.count, 0);
    const sumWhere = (pred: (s: string) => boolean) =>
      fetched.statusRows.filter((r) => pred((r.keys['status'] || '').toLowerCase())).reduce((a, r) => a + r.count, 0);
    const confirmed = sumWhere((s) => s === 'confirmed');
    const unsub = sumWhere((s) => s === 'unsubscribed');
    const bounced = sumWhere((s) => s.includes('bounce'));
    const complained = sumWhere((s) => s.includes('complain'));
    const band = (name: string) => {
      const row = fetched.bandRows.find((r) => (r.keys['engagement_band'] || '').toLowerCase() === name);
      return { count: row?.count ?? 0, avg: row?.avg_engagement ?? null };
    };
    const confirmedBandTotal = fetched.bandRows.reduce((a, r) => a + r.count, 0);
    return { total, confirmed, unsub, bounced, complained, hot: band('hot'), warm: band('warm'), cold: band('cold'), confirmedBandTotal };
  }, [fetched]);

  const pctOf = (n: number, d: number): number | null => (d > 0 ? (n / d) * 100 : null);

  return (
    <div style={styles.panel}>
      <div style={styles.panelHeader}>
        <div>
          <h2 style={styles.panelTitle}>
            <FontAwesomeIcon icon={faUsers} style={{ marginRight: 8, color: COLORS.accentAlt }} />
            Audience KPIs
          </h2>
          <p style={styles.panelSubtitle}>
            Latest snapshot · group_by=status + group_by=engagement_band (status=confirmed).
            Band shares are of confirmed members.{' '}
            {fetched && (
              <span style={styles.timingNote}>
                fetched {fmtClock(fetched.metaB.fetchedAt)} · {fmt(fetched.metaA.durationMs)}ms + {fmt(fetched.metaB.durationMs)}ms
                {fetched.metaA.fromCache && fetched.metaB.fromCache ? ' · cached' : ''}
              </span>
            )}
          </p>
        </div>
        <RefreshBtn loading={loading} onClick={() => load(true)} />
      </div>
      {error ? (
        <ErrorPanel label="Audience KPI query failed" error={error} onRetry={() => load(true)} />
      ) : loading && !fetched ? <LoadingRow /> : !fetched ? <EmptyRow label="Waiting for first query…" /> :
        fetched.flags.disabled ? <FriendlyState kind="disabled" /> :
        fetched.flags.empty || !derived || derived.total === 0 ? <FriendlyState kind="empty" /> : (
        <div style={styles.kpiGrid}>
          {/* Snapshot grain = one row per subscriber-LIST membership (same email
              on 3 lists counts 3×). The Sources tab dedupes to unique people via
              the events join, so its totals will read lower — that's correct. */}
          <KpiCard label="Total Audience (list rows)" value={derived.total} color={COLORS.accent}
            extra="email on N lists counts N×" />
          <KpiCard label="Confirmed" value={derived.confirmed} color={COLORS.good}
            pct={pctOf(derived.confirmed, derived.total)} pctLabel="of total" />
          <KpiCard label="Unsubscribed" value={derived.unsub} color={COLORS.accentAlt}
            pct={pctOf(derived.unsub, derived.total)} pctLabel="of total" />
          <KpiCard label="Bounced" value={derived.bounced} color={HARD_RED}
            pct={pctOf(derived.bounced, derived.total)} pctLabel="of total"
            extra="status contains 'bounce' (hard) — red by hard rule" />
          <KpiCard label="Complained" value={derived.complained} color={COMPLAINT_ROSE}
            pct={pctOf(derived.complained, derived.total)} pctLabel="of total" />
          <KpiCard label="Hot (confirmed)" value={derived.hot.count} color={COLORS.good}
            pct={pctOf(derived.hot.count, derived.confirmedBandTotal)} pctLabel="of confirmed"
            extra={derived.hot.avg == null ? undefined : `avg engagement ${derived.hot.avg.toFixed(1)}`} />
          <KpiCard label="Warm (confirmed)" value={derived.warm.count} color={COLORS.warn}
            pct={pctOf(derived.warm.count, derived.confirmedBandTotal)} pctLabel="of confirmed"
            extra={derived.warm.avg == null ? undefined : `avg engagement ${derived.warm.avg.toFixed(1)}`} />
          <KpiCard label="Cold (confirmed)" value={derived.cold.count} color={COLD_SLATE}
            pct={pctOf(derived.cold.count, derived.confirmedBandTotal)} pctLabel="of confirmed"
            extra={derived.cold.avg == null ? undefined : `avg engagement ${derived.cold.avg.toFixed(1)}`} />
        </div>
      )}
    </div>
  );
};

// ─── Acquisition trend: stacked daily bars by data_source ──────────────────

const ACQ_PRESETS = [30, 60, 90];

const AcquisitionPanel: React.FC = () => {
  const { addToast } = useToast();
  const [rangeDays, setRangeDays] = useState(60);
  const [fetched, setFetched] = useState<PanelData<AudBreakdownResponse> | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async (bypass: boolean) => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    try {
      const res = await fetchCachedJSON<AudBreakdownResponse>(
        audBreakdownURL({
          groupBy: ['acquired_dt', 'data_source'], limit: 5000,
          acquiredFrom: daysAgoUTC(rangeDays - 1),
        }),
        0, { signal: ctl.signal, bypass }
      );
      setFetched({ data: normBreakdown(res.data), meta: res.meta });
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      setFetched(null);
      setError(msg);
      addToast({ type: 'error', title: 'Acquisition trend failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [rangeDays, addToast]);

  useEffect(() => {
    load(false);
    return () => abortRef.current?.abort();
  }, [load]);

  const series = useMemo(
    () => (fetched ? buildStackedSeries(fetched.data.rows, 'acquired_dt', 'data_source', 6) : null),
    [fetched]
  );
  const totalNew = useMemo(
    () => (fetched ? fetched.data.rows.reduce((a, r) => a + ((r.keys['acquired_dt'] ?? '') ? r.count : 0), 0) : 0),
    [fetched]
  );

  return (
    <div style={styles.panel}>
      <div style={styles.panelHeader}>
        <div>
          <h2 style={styles.panelTitle}>
            <FontAwesomeIcon icon={faChartLine} style={{ marginRight: 8, color: COLORS.good }} />
            Acquisition Trend
          </h2>
          <p style={styles.panelSubtitle}>
            New subscriber-list rows per day (an email joining N lists counts N times), stacked by
            data_source (top 6 by volume + other, rolled up client-side).
            group_by=acquired_dt,data_source · acquired_from={daysAgoUTC(rangeDays - 1)}. <TimingNote meta={fetched?.meta ?? null} />
          </p>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <div style={styles.presetRow}>
            {ACQ_PRESETS.map((d) => (
              <button
                key={d}
                style={{
                  ...styles.presetBtn,
                  color: rangeDays === d ? COLORS.accent : COLORS.textSecondary,
                  borderColor: rangeDays === d ? COLORS.accent + '66' : COLORS.borderStrong,
                  background: rangeDays === d ? COLORS.accent + '14' : 'transparent',
                }}
                onClick={() => setRangeDays(d)}
              >
                {d}d
              </button>
            ))}
          </div>
          <RefreshBtn loading={loading} onClick={() => load(true)} />
        </div>
      </div>
      <TruncationBanner truncated={fetched?.data.truncated} limit={5000} />
      {error ? (
        <ErrorPanel label="Acquisition trend failed" error={error} onRetry={() => load(true)} />
      ) : loading && !fetched ? <LoadingRow /> : !fetched ? <EmptyRow label="Waiting for first query…" /> :
        fetched.data.disabled ? <FriendlyState kind="disabled" /> :
        fetched.data.empty || !series || series.data.length === 0 ? (
          <EmptyRow label={`No acquisitions in the last ${rangeDays} days.`} />
        ) : (
        <>
          <div style={styles.kpiInlineRow}>
            <span style={styles.kpiInline}>new subscribers in window: <b style={{ color: COLORS.good }}>{fmt(totalNew)}</b></span>
          </div>
          <StackLegend keys={series.keys} />
          <StackedBars data={series.data} keys={series.keys} colorFor={stackColor} height={280} />
          {series.excludedNoDate > 0 && (
            <div style={styles.tableFooterNote}>
              {fmt(series.excludedNoDate)} member{series.excludedNoDate === 1 ? '' : 's'} with no acquired_dt excluded from the chart.
            </div>
          )}
        </>
      )}
    </div>
  );
};

// ─── Churn trend: stacked daily bars by churn_reason + 7d-vs-7d KPI ────────

const CHURN_WINDOW_DAYS = 60;

const ChurnPanel: React.FC = () => {
  const { addToast } = useToast();
  const [fetched, setFetched] = useState<PanelData<AudBreakdownResponse> | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async (bypass: boolean) => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    try {
      const res = await fetchCachedJSON<AudBreakdownResponse>(
        audBreakdownURL({
          groupBy: ['churned_dt', 'churn_reason'], limit: 5000,
          churnedFrom: daysAgoUTC(CHURN_WINDOW_DAYS - 1),
        }),
        0, { signal: ctl.signal, bypass }
      );
      setFetched({ data: normBreakdown(res.data), meta: res.meta });
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      setFetched(null);
      setError(msg);
      addToast({ type: 'error', title: 'Churn trend failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [addToast]);

  useEffect(() => {
    load(false);
    return () => abortRef.current?.abort();
  }, [load]);

  const series = useMemo(
    () => (fetched ? buildStackedSeries(fetched.data.rows, 'churned_dt', 'churn_reason', 6) : null),
    [fetched]
  );

  // 7d vs prior-7d delta, computed from the same rows (ISO dates compare lexically).
  const weekKpi = useMemo(() => {
    if (!fetched) return null;
    const last7Start = daysAgoUTC(6);
    const prior7Start = daysAgoUTC(13);
    let last7 = 0;
    let prior7 = 0;
    for (const r of fetched.data.rows) {
      const dt = r.keys['churned_dt'] ?? '';
      if (!dt) continue;
      if (dt >= last7Start) last7 += r.count;
      else if (dt >= prior7Start) prior7 += r.count;
    }
    const deltaPct = prior7 > 0 ? ((last7 - prior7) / prior7) * 100 : null;
    return { last7, prior7, deltaPct };
  }, [fetched]);

  return (
    <div style={styles.panel}>
      <div style={styles.panelHeader}>
        <div>
          <h2 style={styles.panelTitle}>
            <FontAwesomeIcon icon={faChartLine} style={{ marginRight: 8, color: HARD_RED }} />
            Churn Trend
          </h2>
          <p style={styles.panelSubtitle}>
            Churned subscriber-list rows per day, stacked by churn_reason (unsubscribed indigo · hard_bounced red ·
            complained rose). group_by=churned_dt,churn_reason · churned_from={daysAgoUTC(CHURN_WINDOW_DAYS - 1)}. <TimingNote meta={fetched?.meta ?? null} />
          </p>
        </div>
        <RefreshBtn loading={loading} onClick={() => load(true)} />
      </div>
      <TruncationBanner truncated={fetched?.data.truncated} limit={5000} />
      {error ? (
        <ErrorPanel label="Churn trend failed" error={error} onRetry={() => load(true)} />
      ) : loading && !fetched ? <LoadingRow /> : !fetched ? <EmptyRow label="Waiting for first query…" /> :
        fetched.data.disabled ? <FriendlyState kind="disabled" /> :
        fetched.data.empty || !series || series.data.length === 0 ? (
          <EmptyRow label={`No churn recorded in the last ${CHURN_WINDOW_DAYS} days.`} />
        ) : (
        <>
          {weekKpi && (
            <div style={styles.kpiInlineRow}>
              <span style={styles.kpiInline}>churned last 7d: <b style={{ color: HARD_RED }}>{fmt(weekKpi.last7)}</b></span>
              <span style={styles.kpiInline}>prior 7d: <b style={{ color: COLORS.textPrimary }}>{fmt(weekKpi.prior7)}</b></span>
              <span style={styles.kpiInline}>
                Δ:{' '}
                <b style={{ color: weekKpi.deltaPct == null ? COLORS.textMuted : weekKpi.deltaPct > 0 ? HARD_RED : COLORS.good }}>
                  {weekKpi.deltaPct == null ? '—' : `${weekKpi.deltaPct > 0 ? '+' : ''}${weekKpi.deltaPct.toFixed(1)}%`}
                </b>
                <span style={{ color: COLORS.textMuted }}> (rising churn = red)</span>
              </span>
            </div>
          )}
          <div style={styles.seriesToggleRow}>
            {series.keys.map((k) => {
              const color = churnReasonColor(k);
              return (
                <span key={k} style={{ ...styles.legendChip, color, borderColor: color + '66', background: color + '14' }}>
                  <FontAwesomeIcon icon={faCircle} style={{ fontSize: 7 }} />
                  {k || '(empty)'}
                </span>
              );
            })}
          </div>
          <StackedBars data={series.data} keys={series.keys} colorFor={(_, k) => churnReasonColor(k)} height={240} />
        </>
      )}
    </div>
  );
};

// ─── Composition: top data_sources with avg engagement ─────────────────────

const COMPOSITION_TOP_N = 12;

const CompositionPanel: React.FC = () => {
  const { addToast } = useToast();
  const [fetched, setFetched] = useState<PanelData<AudBreakdownResponse> | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async (bypass: boolean) => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    try {
      const res = await fetchCachedJSON<AudBreakdownResponse>(
        audBreakdownURL({ groupBy: ['data_source'], limit: 5000 }),
        0, { signal: ctl.signal, bypass }
      );
      setFetched({ data: normBreakdown(res.data), meta: res.meta });
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      setFetched(null);
      setError(msg);
      addToast({ type: 'error', title: 'Composition query failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [addToast]);

  useEffect(() => {
    load(false);
    return () => abortRef.current?.abort();
  }, [load]);

  const sorted = useMemo(
    () => (fetched ? [...fetched.data.rows].sort((a, b) => b.count - a.count) : []),
    [fetched]
  );
  const total = useMemo(() => sorted.reduce((a, r) => a + r.count, 0), [sorted]);
  const top = sorted.slice(0, COMPOSITION_TOP_N);
  const restCount = total - top.reduce((a, r) => a + r.count, 0);
  const maxCount = top.length > 0 ? top[0].count : 0;

  return (
    <div style={styles.panel}>
      <div style={styles.panelHeader}>
        <div>
          <h2 style={styles.panelTitle}>
            <FontAwesomeIcon icon={faLayerGroup} style={{ marginRight: 8, color: COLORS.accentAlt }} />
            Composition by data_source
          </h2>
          <p style={styles.panelSubtitle}>
            Latest snapshot · top {COMPOSITION_TOP_N} sources by member count with snapshot avg_engagement
            beside each. <TimingNote meta={fetched?.meta ?? null} />
          </p>
        </div>
        <RefreshBtn loading={loading} onClick={() => load(true)} />
      </div>
      <TruncationBanner truncated={fetched?.data.truncated} limit={5000} />
      {error ? (
        <ErrorPanel label="Composition query failed" error={error} onRetry={() => load(true)} />
      ) : loading && !fetched ? <LoadingRow /> : !fetched ? <EmptyRow label="Waiting for first query…" /> :
        fetched.data.disabled ? <FriendlyState kind="disabled" /> :
        fetched.data.empty || top.length === 0 ? <FriendlyState kind="empty" /> : (
        <>
          <div style={styles.mixList}>
            {top.map((r) => {
              const name = r.keys['data_source'] || '(empty)';
              const share = total > 0 ? (r.count / total) * 100 : 0;
              const width = maxCount > 0 ? (r.count / maxCount) * 100 : 0;
              return (
                <div key={name} style={styles.mixRow}>
                  <div style={{ ...styles.mixType, color: COLORS.textPrimary }} title={name}>{truncate(name, 22)}</div>
                  <div style={styles.mixBarTrack}>
                    <div style={{ ...styles.mixBarFill, width: `${Math.max(width, 0.5)}%`, background: COLORS.accent }} />
                  </div>
                  <div style={styles.mixCount}>{fmt(r.count)}</div>
                  <div style={styles.mixShare}>{share.toFixed(1)}%</div>
                  <div style={styles.mixEng} title="snapshot avg engagement score (0–100)">
                    eng {r.avg_engagement == null ? '—' : r.avg_engagement.toFixed(1)}
                  </div>
                </div>
              );
            })}
          </div>
          {restCount > 0 && (
            <div style={styles.tableFooterNote}>
              + {fmt(restCount)} members across {fmt(Math.max(sorted.length - top.length, 0))} more data_source values.
            </div>
          )}
        </>
      )}
    </div>
  );
};

const OverviewTab: React.FC = () => (
  <div>
    <OverviewKpiStrip />
    <AcquisitionPanel />
    <ChurnPanel />
    <CompositionPanel />
  </div>
);

// ═══════════════════════════════════════════════════════════════════════════
// TAB 2 — SOURCES (the conversion lever)
// ═══════════════════════════════════════════════════════════════════════════

const SRC_DIMS: Array<{ id: string; label: string }> = [
  { id: 'data_source', label: 'Data Source' },
  { id: 'source', label: 'Source (import file)' },
  { id: 'source_system', label: 'Source System' },
  { id: 'isp', label: 'ISP' },
  { id: 'engagement_band', label: 'Engagement Band' },
  { id: 'verification_status', label: 'Verification' },
  { id: 'acquired_dt', label: 'Acquired Day' },
  { id: 'status', label: 'Status' },
];

const SRC_PRESETS: Array<{ label: string; days: number }> = [
  { label: '7D', days: 7 },
  { label: '14D', days: 14 },
  { label: '30D', days: 30 },
];

interface SrcColDef {
  id: string;
  label: string;
  value: (r: SrcMetricRow) => number | null;
  render: (r: SrcMetricRow) => React.ReactNode;
  tint?: (r: SrcMetricRow) => string | undefined;
  title?: (r: SrcMetricRow) => string | undefined;
}

const numCell = (n: number, color?: string): React.ReactNode => (
  <span style={{ color: color || COLORS.textPrimary, fontVariantNumeric: 'tabular-nums' }}>{fmt(n)}</span>
);
const rateCell = (v: number | null, color?: string): React.ReactNode => (
  <span style={{ color: color || COLORS.textSecondary, fontVariantNumeric: 'tabular-nums' }}>{fmtPct(v)}</span>
);

// Column order per the audience-matrix contract: funnel first, then opens/
// clicks (directional), then hard / soft / complaints (never summed).
const SRC_COLS: SrcColDef[] = [
  { id: 'attempted', label: 'Attempted', value: (r) => r.counts.attempted, render: (r) => numCell(r.counts.attempted) },
  { id: 'delivered', label: 'Delivered', value: (r) => r.counts.delivered, render: (r) => numCell(r.counts.delivered, COLORS.good) },
  {
    id: 'del_pct', label: 'Del%', value: (r) => r.rates.delivery,
    render: (r) => rateCell(r.rates.delivery, COLORS.good),
    tint: (r) => heatDel(r.rates.delivery), title: (r) => denomTitle(r.rates),
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
  {
    id: 'ctor', label: 'CTOR', value: (r) => r.rates.ctor,
    render: (r) => rateCell(r.rates.ctor, CLICK_VIOLET),
    title: (r) => `denominator: opens (${fmt(r.counts.opens)})`,
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
  { id: 'complaints', label: 'Compl', value: (r) => r.counts.complaints, render: (r) => numCell(r.counts.complaints, COMPLAINT_ROSE) },
  {
    id: 'comp_pct', label: 'Comp%', value: (r) => r.rates.complaint,
    render: (r) => rateCell(r.rates.complaint, COMPLAINT_ROSE),
    tint: (r) => heatComp(r.rates.complaint), title: (r) => denomTitle(r.rates),
  },
];

function sortSrcRows(rows: SrcMetricRow[], sort: SortState): SrcMetricRow[] {
  const col = SRC_COLS.find((c) => c.id === sort.col);
  const sorted = [...rows];
  sorted.sort((a, b) => {
    let cmp: number;
    if (!col) {
      cmp = a.key.localeCompare(b.key);
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

// Inline row expansion: the row's full funnel stats (kept simple in lieu of
// cross-tab filter plumbing — see contract comment).
const SrcRowExpansion: React.FC<{ dim: string; row: SrcMetricRow }> = ({ dim, row }) => {
  const c = row.counts;
  const r = row.rates;
  const item = (label: string, value: string, color?: string, title?: string) => (
    <div title={title}>
      <div style={styles.metaLabel}>{label}</div>
      <div style={{ ...styles.metaValue, color: color || COLORS.textPrimary, fontVariantNumeric: 'tabular-nums' }}>{value}</div>
    </div>
  );
  return (
    <div>
      <div style={styles.expandHeader}>
        <span style={{ color: COLORS.textPrimary, fontWeight: 600 }}>{dim}={row.key || '(empty)'}</span>
        <span style={styles.expandStat}>full funnel for this cohort over the selected events window</span>
      </div>
      <div style={styles.detailGrid}>
        {item('attempted', fmt(c.attempted), COLORS.accent)}
        {item('delivered', `${fmt(c.delivered)} (${fmtPct(r.delivery)})`, COLORS.good, denomTitle(r))}
        {item('relayed_to_ses', `${fmt(c.relayed)} — relay handoff, not a delivery`, INFO_BLUE)}
        {item('delivery_delay', fmt(c.delays), COLORS.warn)}
        {item('hard bounce', `${fmt(c.hard)} (${fmtPct(r.hard)})`, HARD_RED, denomTitle(r))}
        {item('soft bounce', `${fmt(c.soft)} (${fmtPct(r.soft)})`, SOFT_AMBER, denomTitle(r))}
        {item('complaints', `${fmt(c.complaints)} (${fmtPct(r.complaint)})`, COMPLAINT_ROSE, denomTitle(r))}
        {item('opens', `${fmt(c.opens)} (${fmtPct(r.open)})`, OPEN_CYAN, deliveredTitle(c))}
        {item('clicks', `${fmt(c.clicks)} (${fmtPct(r.click)})`, CLICK_VIOLET, deliveredTitle(c))}
        {item('CTOR', fmtPct(r.ctor), CLICK_VIOLET, `denominator: opens (${fmt(c.opens)})`)}
        {item('other events', fmt(c.other), COLORS.textSecondary)}
        {item('rate denominator', `${r.denomLabel} (${fmt(r.denom)})`, COLORS.textSecondary)}
      </div>
      <div style={styles.tableFooterNote}>
        Reading: members acquired from the same {dim} tend to perform like this cohort — use it to predict
        how new imports from this source will behave. Opens/clicks are directional (lake covers SES-routed
        mail + app-tracking backfill only).
      </div>
    </div>
  );
};

interface SrcFetched {
  rows: SourcePerfRow[];
  dim: string;          // the dim these rows were fetched for — pivot guard
  from: string;
  to: string;
  audienceDt: string;
  truncated: boolean;
  disabled: boolean;
  meta: FetchMeta;
}

const SourcesTab: React.FC = () => {
  const { addToast } = useToast();
  const [dim, setDim] = useState('data_source');
  const [from, setFrom] = useState(daysAgoUTC(6));
  const [to, setTo] = useState(todayUTC());
  const [fetched, setFetched] = useState<SrcFetched | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [sort, setSort] = useState<SortState>({ col: 'attempted', dir: 'desc' });
  const [expandedKey, setExpandedKey] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async (bypass: boolean) => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    setExpandedKey(null);
    try {
      const res = await fetchCachedJSON<SourcePerfResponse>(
        sourcePerfURL({ dim, from, to, limit: 5000 }),
        0, { signal: ctl.signal, bypass }
      );
      setFetched({
        rows: Array.isArray(res.data.rows) ? res.data.rows : [],
        dim, from, to,
        audienceDt: res.data.audience_dt || '',
        truncated: !!res.data.truncated,
        disabled: !!res.data.disabled,
        meta: res.meta,
      });
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      // Clear stale data — old dim's rows must never render under a new dim label.
      setFetched(null);
      setError(msg);
      addToast({ type: 'error', title: 'Source performance failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [dim, from, to, addToast]);

  useEffect(() => {
    load(false);
    return () => abortRef.current?.abort();
  }, [load]);

  // Only pivot rows fetched for the CURRENTLY selected dim/window.
  const fresh = fetched !== null && fetched.dim === dim && fetched.from === from && fetched.to === to;
  const pivot = useMemo(() => (fresh && fetched ? pivotSourcePerf(fetched.rows) : null), [fresh, fetched]);
  const sorted = useMemo(() => (pivot ? sortSrcRows(pivot.rows, sort) : []), [pivot, sort]);

  const onSort = (col: string) => setSort((s) =>
    s.col === col ? { col, dir: s.dir === 'desc' ? 'asc' : 'desc' } : { col, dir: 'desc' });
  const arrow = (col: string) => sort.col === col ? (sort.dir === 'desc' ? ' ▾' : ' ▴') : '';

  const dimLabel = SRC_DIMS.find((d) => d.id === dim)?.label || dim;

  const renderCells = (r: SrcMetricRow, isTotal: boolean) => SRC_COLS.map((c) => (
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
    <div style={styles.panel}>
      <div style={styles.panelHeader}>
        <div>
          <h2 style={styles.panelTitle}>
            <FontAwesomeIcon icon={faLayerGroup} style={{ marginRight: 8, color: COLORS.accentPink }} />
            Source Performance Matrix
          </h2>
          <p style={styles.panelSubtitle}>
            Email-event outcomes grouped by an audience attribute — the likelihood that someone from the
            same {dimLabel.toLowerCase()} performs the same. Events window {from} → {to}
            {fetched?.audienceDt ? ` · audience snapshot ${fetched.audienceDt}` : ''}.
            Heat: Hard%&gt;1 / Comp%&gt;0.1 / Del%&lt;90 red · Hard%&gt;0.5 / Comp%&gt;0.05 / Del%&lt;97 amber.
            Click a row for its full funnel. <TimingNote meta={fresh && fetched ? fetched.meta : null} />
          </p>
        </div>
        <RefreshBtn loading={loading} onClick={() => load(true)} />
      </div>

      <div style={styles.dimPickerRow}>
        <span style={styles.chipRowLabel}>Dim:</span>
        {SRC_DIMS.map((d) => {
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

      <div style={styles.eventFilterBar}>
        <div style={styles.presetRow}>
          {SRC_PRESETS.map((p) => {
            const active = from === daysAgoUTC(p.days - 1) && to === todayUTC();
            return (
              <button
                key={p.label}
                style={{
                  ...styles.presetBtn,
                  color: active ? COLORS.accent : COLORS.textSecondary,
                  borderColor: active ? COLORS.accent + '66' : COLORS.borderStrong,
                  background: active ? COLORS.accent + '14' : 'transparent',
                }}
                onClick={() => { setFrom(daysAgoUTC(p.days - 1)); setTo(todayUTC()); }}
              >
                {p.label}
              </button>
            );
          })}
        </div>
        <label style={styles.fieldLabel}>From
          <input type="date" value={from} max={to} onChange={(e) => setFrom(e.target.value)} style={styles.input} />
        </label>
        <label style={styles.fieldLabel}>To
          <input type="date" value={to} min={from} max={todayUTC()} onChange={(e) => setTo(e.target.value)} style={styles.input} />
        </label>
        <span style={{ ...styles.timingNote, paddingBottom: 8 }}>events window — the audience side always uses the latest snapshot</span>
      </div>

      <TruncationBanner truncated={fresh && !!fetched?.truncated} limit={5000} />
      {error ? (
        <ErrorPanel label="Source performance failed" error={error} onRetry={() => load(true)} />
      ) : loading || (fetched !== null && !fresh) ? (
        <LoadingRow />
      ) : !pivot ? (
        <EmptyRow label="Waiting for first query…" />
      ) : fetched?.disabled ? (
        <FriendlyState kind="disabled" />
      ) : pivot.rows.length === 0 ? (
        <EmptyRow label="No email events joined to the audience snapshot in this window." />
      ) : (
        <>
          <div style={styles.tableWrap}>
            <table style={styles.table}>
              <thead>
                <tr>
                  <th style={{ ...styles.th, textAlign: 'left', cursor: 'pointer' }} onClick={() => onSort('__key')}>
                    {dimLabel}{arrow('__key')}
                  </th>
                  {SRC_COLS.map((c) => (
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
                    TOTAL ({fmt(pivot.rows.length)} {pivot.rows.length === 1 ? 'value' : 'values'})
                  </td>
                  {renderCells(pivot.totals, true)}
                </tr>
                {sorted.map((r) => {
                  const isOpen = expandedKey === r.key;
                  return (
                    <React.Fragment key={r.key || '(empty)'}>
                      <tr
                        style={{ ...styles.tr, cursor: 'pointer', background: isOpen ? 'rgba(129,140,248,0.07)' : undefined }}
                        onClick={() => setExpandedKey((cur) => (cur === r.key ? null : r.key))}
                      >
                        <td style={{ ...styles.td, whiteSpace: 'nowrap', color: COLORS.textPrimary, fontWeight: 500 }}>
                          <FontAwesomeIcon
                            icon={isOpen ? faChevronDown : faChevronRight}
                            style={{ fontSize: 9, marginRight: 8, color: COLORS.textMuted }}
                          />
                          {r.key || '(empty)'}
                        </td>
                        {renderCells(r, false)}
                      </tr>
                      {isOpen && (
                        <tr style={styles.tr}>
                          <td colSpan={SRC_COLS.length + 1} style={styles.expandCell}>
                            <SrcRowExpansion dim={dim} row={r} />
                          </td>
                        </tr>
                      )}
                    </React.Fragment>
                  );
                })}
              </tbody>
            </table>
          </div>
          <div style={styles.tableFooterNote}>
            Del/Hard/Soft/Comp rates use attempted as the denominator (fallback delivered+hard+soft — hover
            any rate for the denominator actually used); Open/Click rates use delivered; CTOR uses opens.
            DATA CAVEAT: open/click events exist in the lake only for SES-routed mail and app-tracking
            backfill — Opens/Open%/Clicks/Click%/CTOR are directional, not absolute. Hard and soft bounces
            are never summed.
          </div>
        </>
      )}
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB 3 — GROWTH (first touch)
// ═══════════════════════════════════════════════════════════════════════════

const GROWTH_PRESETS = [14, 30, 60];

interface GrowthFetched {
  rows: FirstTouchRow[];
  from: string;
  to: string;
  disabled: boolean;
  meta: FetchMeta;
}

const GrowthTab: React.FC = () => {
  const { addToast } = useToast();
  const [rangeDays, setRangeDays] = useState(30);
  const [fetched, setFetched] = useState<GrowthFetched | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async (bypass: boolean) => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    const from = daysAgoUTC(rangeDays - 1);
    const to = todayUTC();
    try {
      const res = await fetchCachedJSON<FirstTouchResponse>(
        firstTouchURL(from, to), 0, { signal: ctl.signal, bypass });
      setFetched({
        rows: Array.isArray(res.data.rows) ? res.data.rows : [],
        from, to,
        disabled: !!res.data.disabled,
        meta: res.meta,
      });
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      setFetched(null);
      setError(msg);
      addToast({ type: 'error', title: 'First-touch query failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [rangeDays, addToast]);

  useEffect(() => {
    load(false);
    return () => abortRef.current?.abort();
  }, [load]);

  const sortedAsc = useMemo(
    () => (fetched ? [...fetched.rows].sort((a, b) => a.dt.localeCompare(b.dt)) : []),
    [fetched]
  );
  const chartData = useMemo(
    () => sortedAsc.map((r) => ({
      dt: r.dt,
      'new audience mailed': r.count,
      'activated (opened)': r.opened ?? 0,
      'activation %': r.count > 0 ? Number((((r.opened ?? 0) / r.count) * 100).toFixed(2)) : 0,
    })),
    [sortedAsc]
  );
  const todayRow = useMemo(() => {
    const t = todayUTC();
    return fetched?.rows.find((r) => r.dt === t) ?? null; // null if absent
  }, [fetched]);
  const todayCount = todayRow?.count ?? 0;
  const last7 = useMemo(() => {
    const start = daysAgoUTC(6);
    const rows = (fetched?.rows || []).filter((r) => r.dt >= start);
    const count = rows.reduce((a, r) => a + r.count, 0);
    const opened = rows.reduce((a, r) => a + (r.opened ?? 0), 0);
    return { count, opened, pct: count > 0 ? (opened / count) * 100 : null };
  }, [fetched]);
  const windowActivation = useMemo(() => {
    const count = sortedAsc.reduce((a, r) => a + r.count, 0);
    const opened = sortedAsc.reduce((a, r) => a + (r.opened ?? 0), 0);
    return { count, opened, pct: count > 0 ? (opened / count) * 100 : null };
  }, [sortedAsc]);

  return (
    <div style={styles.panel}>
      <div style={styles.panelHeader}>
        <div>
          <h2 style={styles.panelTitle}>
            <FontAwesomeIcon icon={faSeedling} style={{ marginRight: 8, color: COLORS.good }} />
            Growth — First Touch
          </h2>
          <p style={styles.panelSubtitle}>
            Recipients whose FIRST-ever lake event (attempted / delivered / relayed) fell on each day —
            i.e. addresses newly put into rotation — and how many of each day's cohort ACTIVATED
            (≥1 open ever; this is the acquisition signal). Includes seedlist/proof recipients (the
            count is events-side, not joined to the audience snapshot), so treat it as directional. Window {fetched?.from || daysAgoUTC(rangeDays - 1)} → {fetched?.to || todayUTC()}. <TimingNote meta={fetched?.meta ?? null} />
          </p>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <div style={styles.presetRow}>
            {GROWTH_PRESETS.map((d) => (
              <button
                key={d}
                style={{
                  ...styles.presetBtn,
                  color: rangeDays === d ? COLORS.accent : COLORS.textSecondary,
                  borderColor: rangeDays === d ? COLORS.accent + '66' : COLORS.borderStrong,
                  background: rangeDays === d ? COLORS.accent + '14' : 'transparent',
                }}
                onClick={() => setRangeDays(d)}
              >
                {d}d
              </button>
            ))}
          </div>
          <RefreshBtn loading={loading} onClick={() => load(true)} />
        </div>
      </div>
      {error ? (
        <ErrorPanel label="First-touch query failed" error={error} onRetry={() => load(true)} />
      ) : loading && !fetched ? <LoadingRow /> : !fetched ? <EmptyRow label="Waiting for first query…" /> :
        fetched.disabled ? <FriendlyState kind="disabled" /> :
        sortedAsc.length === 0 ? <EmptyRow label="No first-touch datapoints in this window." /> : (
        <>
          <div style={styles.kpiGrid}>
            <KpiCard label="New audience mailed today" value={todayCount} color={COLORS.good}
              pct={todayRow && todayRow.count > 0 ? ((todayRow.opened ?? 0) / todayRow.count) * 100 : null}
              pctLabel="activated" />
            <KpiCard label="Last 7 days" value={last7.count} color={COLORS.accent}
              pct={last7.pct} pctLabel="activated" />
            <KpiCard label="Activated in window" value={windowActivation.opened} color={OPEN_CYAN}
              pct={windowActivation.pct} pctLabel="of first-touched" />
          </div>
          <div style={{ marginTop: 20 }}>
            {/* Grouped bars (cohort size vs activated) + right-axis activation-rate
                line — the threshold-finding view: where the line settles is the
                acquisition quality this source mix supports. */}
            <ResponsiveContainer width="100%" height={280}>
              <ComposedChart data={chartData} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
                <CartesianGrid stroke="rgba(255,255,255,0.06)" vertical={false} />
                <XAxis dataKey="dt"
                  tickFormatter={(v: string) => (typeof v === 'string' ? v.slice(5) : String(v))}
                  tick={{ fill: COLORS.textMuted, fontSize: 10 }}
                  axisLine={{ stroke: COLORS.borderStrong }} tickLine={false} />
                <YAxis yAxisId="vol" tickFormatter={(v: number) => fmtCompact(v)}
                  tick={{ fill: COLORS.textMuted, fontSize: 10 }} axisLine={false} tickLine={false} width={48} />
                <YAxis yAxisId="rate" orientation="right" tickFormatter={(v: number) => `${v}%`}
                  tick={{ fill: COLORS.textMuted, fontSize: 10 }} axisLine={false} tickLine={false} width={44} />
                <Tooltip content={<ChartTip />} cursor={{ fill: 'rgba(255,255,255,0.04)' }} />
                <Bar yAxisId="vol" dataKey="new audience mailed" fill={COLORS.good} fillOpacity={0.75} maxBarSize={22} />
                <Bar yAxisId="vol" dataKey="activated (opened)" fill={OPEN_CYAN} fillOpacity={0.85} maxBarSize={22} />
                <Line yAxisId="rate" type="monotone" dataKey="activation %" stroke={COLORS.warn}
                  strokeWidth={2} dot={false} connectNulls />
              </ComposedChart>
            </ResponsiveContainer>
          </div>
          <div style={{ ...styles.tableWrap, marginTop: 16, maxHeight: 320, overflowY: 'auto' }}>
            <table style={styles.table}>
              <thead>
                <tr>
                  <th style={{ ...styles.th, textAlign: 'left' }}>dt</th>
                  <th style={{ ...styles.th, textAlign: 'right' }}>first-touched members</th>
                  <th style={{ ...styles.th, textAlign: 'right' }}>activated (opened)</th>
                  <th style={{ ...styles.th, textAlign: 'right' }}>activation %</th>
                  <th style={{ ...styles.th, textAlign: 'right' }}>clicked</th>
                  <th style={{ ...styles.th, textAlign: 'right' }}>click %</th>
                </tr>
              </thead>
              <tbody>
                {[...sortedAsc].reverse().map((r) => {
                  const actPct = r.count > 0 ? ((r.opened ?? 0) / r.count) * 100 : null;
                  const clkPct = r.count > 0 ? ((r.clicked ?? 0) / r.count) * 100 : null;
                  return (
                    <tr key={r.dt} style={styles.tr}>
                      <td style={{ ...styles.td, fontVariantNumeric: 'tabular-nums' }}>{r.dt}</td>
                      <td style={{ ...styles.td, textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmt(r.count)}</td>
                      <td style={{ ...styles.td, textAlign: 'right', fontVariantNumeric: 'tabular-nums', color: OPEN_CYAN }}>{fmt(r.opened ?? 0)}</td>
                      <td style={{ ...styles.td, textAlign: 'right', fontVariantNumeric: 'tabular-nums', color: OPEN_CYAN }}>{fmtPct(actPct, 2)}</td>
                      <td style={{ ...styles.td, textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmt(r.clicked ?? 0)}</td>
                      <td style={{ ...styles.td, textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtPct(clkPct, 2)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <div style={styles.tableFooterNote}>
            "First touch" means first touch within lake history, NOT lifetime — members mailed before the
            lake began recording will show their first lake-era send here, even if they are not actually new.
            "Activated" = the cohort member has ≥1 open event anywhere in lake history (clicked likewise).
            Two caveats: young cohorts (the last few days) under-read because members haven't had time to
            open yet, and open/click events reach the lake only for SES-routed mail + the app-tracking
            backfill — PMTA-heavy cohorts read activation low. Compare cohorts against each other, not
            against an absolute target.
          </div>
        </>
      )}
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB 4 — MEMBER LOOKUP
// ═══════════════════════════════════════════════════════════════════════════

const PROFILE_EVENT_DETAIL_FIELDS: Array<{ label: string; get: (e: LakeEvent) => string }> = [
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

const ProfileCard: React.FC<{ profile: AudienceProfile; index: number; total: number }> = ({ profile: p, index, total }) => {
  const sColor = statusColor(p.status);
  const bColor = bandColor(p.engagement_band);
  return (
    <div style={styles.profileCard}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        <span style={{ fontSize: 15, fontWeight: 700, color: COLORS.textPrimary }}>
          {total > 1 ? `List membership ${index + 1} of ${total}` : 'Audience profile'}
        </span>
        <span style={{ ...styles.typeChip, color: sColor, borderColor: sColor + '55' }}>{p.status || 'unknown'}</span>
        <span style={{ ...styles.typeChip, color: bColor, borderColor: bColor + '55' }}>
          {p.engagement_band || 'no band'}
        </span>
        <span style={{ ...styles.typeChip, color: COLORS.accentAlt, borderColor: COLORS.accentAlt + '55' }}>
          engagement {p.engagement_score == null ? '—' : p.engagement_score.toFixed(1)}
        </span>
        {p.is_bot && (
          <span style={{ ...styles.typeChip, color: HARD_RED, borderColor: HARD_RED + '55' }}>BOT</span>
        )}
        {p.churn_reason && (
          <span style={{ ...styles.typeChip, color: churnReasonColor(p.churn_reason), borderColor: churnReasonColor(p.churn_reason) + '55' }}>
            churn: {p.churn_reason}
          </span>
        )}
      </div>

      <div style={styles.blockTitle}>Provenance</div>
      <div style={styles.metaGrid}>
        <MetaItem label="source (import file)" value={p.source} />
        <MetaItem label="data_source" value={p.data_source} />
        <MetaItem label="source_system" value={p.source_system} />
        <MetaItem label="source_detail" value={p.source_detail} />
        <MetaItem label="acquired" value={p.acquired_dt || fmtTs(p.acquired_at)} />
        <MetaItem label="imported_at" value={fmtTs(p.imported_at)} />
        <MetaItem label="created_at" value={fmtTs(p.created_at)} />
        <MetaItem label="list_id" value={p.list_id} mono />
        <MetaItem label="isp / domain" value={[p.isp, p.email_domain].filter(Boolean).join(' · ')} />
        <MetaItem label="verification" value={p.verification_status} />
        <MetaItem label="data quality" value={p.data_quality_score == null ? '' : p.data_quality_score.toFixed(2)} />
        <MetaItem label="churn risk" value={p.churn_risk_score == null ? '' : p.churn_risk_score.toFixed(2)} />
      </div>

      <div style={styles.blockTitle}>Lifecycle</div>
      <div style={styles.metaGrid}>
        <MetaItem label="subscribed_at" value={fmtTs(p.subscribed_at)} />
        <MetaItem label="unsubscribed_at" value={fmtTs(p.unsubscribed_at)} />
        <div>
          <div style={styles.metaLabel}>hard_bounced_at</div>
          <div style={{ ...styles.metaValue, color: p.hard_bounced_at ? HARD_RED : COLORS.textPrimary }}>
            {fmtTs(p.hard_bounced_at)}
          </div>
        </div>
        <div>
          <div style={styles.metaLabel}>complained_at</div>
          <div style={{ ...styles.metaValue, color: p.complained_at ? COMPLAINT_ROSE : COLORS.textPrimary }}>
            {fmtTs(p.complained_at)}
          </div>
        </div>
        <MetaItem label="churned_dt" value={p.churned_dt} />
      </div>

      <div style={styles.blockTitle}>Totals</div>
      <div style={styles.metaGrid}>
        <MetaItem label="emails received" value={fmt(p.total_emails_received ?? 0)} />
        <MetaItem label="total opens" value={fmt(p.total_opens ?? 0)} />
        <MetaItem label="total clicks" value={fmt(p.total_clicks ?? 0)} />
        <MetaItem label="last open" value={fmtTs(p.last_open_at)} />
        <MetaItem label="last click" value={fmtTs(p.last_click_at)} />
        <MetaItem label="last email" value={fmtTs(p.last_email_at)} />
      </div>
    </div>
  );
};

const MemberTab: React.FC = () => {
  const { addToast } = useToast();
  const [input, setInput] = useState('');
  const [recent, setRecent] = useState<RecentMember[]>(() => loadRecentMembers());
  const [result, setResult] = useState<PanelData<MemberResponse> | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [expandedUid, setExpandedUid] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const runLookup = useCallback(async (raw: string) => {
    const email = raw.trim().toLowerCase();
    if (!isEmail(email)) {
      addToast({ type: 'error', title: 'Invalid email', message: 'Enter a valid email address (user@domain.tld).' });
      return;
    }
    setRecent(saveRecentMember(email));
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    setExpandedUid(null);
    try {
      const res = await fetchMember(email, 200, ctl.signal);
      setResult(res);
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      setResult(null); // clear stale member — never another member's data under a new email
      setError(msg);
      addToast({ type: 'error', title: 'Member lookup failed', message: msg });
    } finally {
      if (abortRef.current === ctl) setLoading(false);
    }
  }, [addToast]);

  useEffect(() => () => abortRef.current?.abort(), []);

  const m = result?.data ?? null;

  return (
    <div>
      <div style={styles.panel}>
        <div style={styles.panelHeader}>
          <div>
            <h2 style={styles.panelTitle}>
              <FontAwesomeIcon icon={faUser} style={{ marginRight: 8, color: COLORS.accentPink }} />
              Member Lookup
            </h2>
            <p style={styles.panelSubtitle}>
              One email → every per-list audience profile (identity, provenance, lifecycle, totals)
              plus its lake event history (events_limit 200). <TimingNote meta={result?.meta ?? null} />
            </p>
          </div>
        </div>
        <div style={styles.eventFilterBar}>
          <label style={styles.fieldLabel}>email
            <input
              type="text" value={input} placeholder="user@example.com"
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => { if (e.key === 'Enter') runLookup(input); }}
              style={{ ...styles.input, width: 320 }}
            />
          </label>
          <button style={styles.primaryBtn} onClick={() => runLookup(input)} disabled={loading}>
            <FontAwesomeIcon icon={loading ? faSpinner : faSearch} spin={loading} /> Lookup
          </button>
        </div>
        {recent.length > 0 && (
          <div style={styles.chipRow}>
            <span style={styles.chipRowLabel}><FontAwesomeIcon icon={faHistory} style={{ marginRight: 4 }} />Recent:</span>
            {recent.map((rc) => (
              <button
                key={rc.email}
                style={styles.recentChip}
                title={`looked up ${new Date(rc.at).toLocaleString('en-US', { hour12: false })}`}
                onClick={() => { setInput(rc.email); runLookup(rc.email); }}
              >
                {truncate(rc.email, 28)}
              </button>
            ))}
          </div>
        )}
      </div>

      {error && (
        <div style={styles.panel}>
          <ErrorPanel label="Member lookup failed" error={error} onRetry={() => runLookup(input)} />
        </div>
      )}

      {loading && <div style={styles.panel}><LoadingRow label="Looking up member…" /></div>}

      {!loading && !error && m && (
        <>
          <div style={styles.panel}>
            <div style={styles.panelHeader}>
              <div>
                <h2 style={styles.panelTitle}>{m.email}</h2>
                <p style={styles.panelSubtitle}>
                  {m.profiles.length} list membership{m.profiles.length === 1 ? '' : 's'}
                  {m.audience_dt ? ` · snapshot ${m.audience_dt}` : ''}
                </p>
              </div>
            </div>
            {m.disabled ? <FriendlyState kind="disabled" /> :
              m.profiles.length === 0 ? (
              <EmptyRow label="No audience profile rows for this email in the latest snapshot." />
            ) : (
              m.profiles.map((p, i) => (
                <ProfileCard key={p.id || `${p.list_id}-${i}`} profile={p} index={i} total={m.profiles.length} />
              ))
            )}
          </div>

          <div style={styles.panel}>
            <div style={styles.panelHeader}>
              <div>
                <h2 style={styles.panelTitle}>Event History (newest first, limit 200)</h2>
                <p style={styles.panelSubtitle}>
                  Lake events for this email. Open/click rows exist only for SES-routed mail and
                  app-tracking backfill — absence of opens here is not proof of inactivity.
                </p>
              </div>
            </div>
            {m.events.length === 0 ? <EmptyRow label="No lake events for this email." /> : (
              <div style={styles.tableWrap}>
                <table style={styles.table}>
                  <thead>
                    <tr>
                      <th style={{ ...styles.th, width: 24 }} />
                      <th style={{ ...styles.th, textAlign: 'left' }}>event_at</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>event_type</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>brand</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>isp_group</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>email_domain</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>campaign_id</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>bounce_cat</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>dsn_code</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>link_url</th>
                    </tr>
                  </thead>
                  <tbody>
                    {m.events.map((e, i) => {
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
                              <span style={{ ...styles.typeChip, color: eventTypeColor(e.event_type), borderColor: eventTypeColor(e.event_type) + '55' }}>
                                {e.event_type || '—'}
                              </span>
                            </td>
                            <td style={styles.td}>{e.brand || '—'}</td>
                            <td style={styles.td}>{e.isp_group || '—'}</td>
                            <td style={styles.td}>{e.email_domain || '—'}</td>
                            <td style={{ ...styles.td, fontFamily: 'monospace', fontSize: 11 }}>
                              {e.campaign_id ? truncate(e.campaign_id, 14) : '—'}
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
                                  {PROFILE_EVENT_DETAIL_FIELDS.map((f) => (
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
              </div>
            )}
          </div>
        </>
      )}

      {!loading && !error && !m && (
        <div style={styles.panel}>
          <EmptyRow label="Enter an email and run a lookup — or pick a recent member above." />
        </div>
      )}
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB 5 — WELCOME POOL (embedded operational gauge)
// ═══════════════════════════════════════════════════════════════════════════

const WelcomePoolTab: React.FC = () => (
  <div>
    <div style={{ ...styles.lakeOnlyNotice, marginBottom: 16 }}>
      <FontAwesomeIcon icon={faInfoCircle} style={{ marginRight: 8, color: COLORS.accentAlt }} />
      This is the operational welcome-pool gauge (upload trigger) — embedded unchanged from Welcome Audience Health.
    </div>
    <Suspense fallback={<div style={styles.panel}><LoadingRow label="Loading Welcome Audience Health…" /></div>}>
      <WelcomeAudienceHealth />
    </Suspense>
  </div>
);

// ═══════════════════════════════════════════════════════════════════════════
// MAIN
// ═══════════════════════════════════════════════════════════════════════════

const TABS: Array<{ id: TabId; label: string; icon: typeof faChartLine }> = [
  { id: 'overview', label: 'Overview', icon: faChartLine },
  { id: 'sources', label: 'Sources', icon: faLayerGroup },
  { id: 'growth', label: 'Growth', icon: faSeedling },
  { id: 'member', label: 'Member Lookup', icon: faUser },
  { id: 'welcome', label: 'Welcome Pool', icon: faInbox },
];

export const AudienceAnalytics: React.FC = () => {
  const { addToast } = useToast();

  const [status, setStatus] = useState<AudienceLakeStatus | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState('');

  const [activeTab, setActiveTab] = useState<TabId>('overview');
  const [visited, setVisited] = useState<Set<TabId>>(() => new Set<TabId>(['overview']));

  const readEnabled = !!status?.enabled_read;
  const hasSnapshot = !!status?.latest_dt;

  const fetchStatus = useCallback(async () => {
    setStatusLoading(true);
    try {
      const res = await apiFetch(`${API_BASE}/status`);
      if (!res.ok) await throwHttpError(res);
      const json: AudienceLakeStatus = await res.json();
      setStatus(json);
      setStatusError('');
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setStatusError(msg);
      addToast({ type: 'error', title: 'Audience lake status failed', message: msg });
    } finally {
      setStatusLoading(false);
    }
  }, [addToast]);

  // On mount: status only. All audience-lake queries are gated on
  // enabled_read + a non-empty latest_dt.
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

  // ── Loading shell (initial status fetch) ──
  if (statusLoading && !status) {
    return (
      <div style={styles.loadingShell}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading audience lake status…
      </div>
    );
  }

  return (
    <div style={styles.page}>
      {/* ─── Header ──────────────────────────────────────────────── */}
      <div style={styles.header}>
        <div>
          <h1 style={styles.title}>
            <FontAwesomeIcon icon={faUsers} style={{ color: COLORS.accent, marginRight: 10 }} />
            Audience Analytics
          </h1>
          <p style={styles.subtitle}>
            Acquisition, churn, source performance, growth and member lookup over the Athena-backed
            audience lake — daily per-member snapshots joined to email-event outcomes. Every rate
            discloses its denominator; hard and soft bounces are never combined.
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
        <div style={styles.statusStrip}>
          <div style={{
            ...styles.badge,
            color: readEnabled ? COLORS.good : COLORS.textMuted,
            borderColor: readEnabled ? COLORS.good + '55' : COLORS.borderStrong,
            background: readEnabled ? COLORS.good + '12' : 'transparent',
          }}>
            <FontAwesomeIcon icon={faCircle} style={{ fontSize: 8 }} />
            Read: {readEnabled ? 'on' : 'off'}
          </div>
          <div style={styles.statusDivider} />
          <div style={styles.counter}>
            <div style={{ ...styles.counterValue, color: hasSnapshot ? COLORS.textPrimary : COLORS.textMuted, fontSize: 18 }}>
              {status.latest_dt || '—'}
            </div>
            <div style={styles.counterLabel}>Latest snapshot</div>
          </div>
          <div style={styles.counter}>
            <div style={{ ...styles.counterValue, color: COLORS.accent }}>{fmtCompact(status.rows)}</div>
            <div style={styles.counterLabel}>Snapshot rows</div>
          </div>
        </div>
      )}

      {/* ─── Read-layer dark ───────────────────────────────────── */}
      {status && !readEnabled && (
        <div style={styles.darkCard}>
          <FontAwesomeIcon icon={faMoon} style={{ fontSize: 28, color: COLORS.accentAlt }} />
          <div style={{ flex: 1 }}>
            <div style={styles.darkTitle}>Audience lake read layer is dark</div>
            <div style={styles.darkBody}>
              The Athena-backed audience read layer is not enabled on the server, so this screen issues
              no queries. Once it is enabled the status strip above will flip to Read: on and the
              analytics tabs will activate.
            </div>
          </div>
        </div>
      )}

      {/* ─── First snapshot still seeding ──────────────────────── */}
      {status && readEnabled && !hasSnapshot && (
        <div style={styles.darkCard}>
          <FontAwesomeIcon icon={faSeedling} style={{ fontSize: 28, color: COLORS.good }} />
          <div style={{ flex: 1 }}>
            <div style={styles.darkTitle}>First audience snapshot is still seeding</div>
            <div style={styles.darkBody}>
              The audience lake takes a full per-member snapshot daily at <b>07:00 UTC</b>. None has
              landed yet (latest_dt is empty), so there is nothing to analyze — check back after the
              next run, or hit Refresh status above. No queries are issued until a snapshot exists.
            </div>
            <div style={styles.darkHint}>
              <FontAwesomeIcon icon={faInfoCircle} style={{ color: COLORS.textMuted, marginRight: 6 }} />
              Schedule: daily 07:00 UTC · current snapshot rows: {fmt(status.rows)}
            </div>
          </div>
        </div>
      )}

      {/* ─── Tabs (only when read enabled + snapshot present) ──── */}
      {status && readEnabled && hasSnapshot && (
        <>
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

          {/* Visited tabs stay mounted (state + cache preserved); only the active one is visible. */}
          {visited.has('overview') && (
            <div style={{ display: activeTab === 'overview' ? 'block' : 'none' }}>
              <OverviewTab />
            </div>
          )}
          {visited.has('sources') && (
            <div style={{ display: activeTab === 'sources' ? 'block' : 'none' }}>
              <SourcesTab />
            </div>
          )}
          {visited.has('growth') && (
            <div style={{ display: activeTab === 'growth' ? 'block' : 'none' }}>
              <GrowthTab />
            </div>
          )}
          {visited.has('member') && (
            <div style={{ display: activeTab === 'member' ? 'block' : 'none' }}>
              <MemberTab />
            </div>
          )}
          {visited.has('welcome') && (
            <div style={{ display: activeTab === 'welcome' ? 'block' : 'none' }}>
              <WelcomePoolTab />
            </div>
          )}
        </>
      )}

      {/* ─── Footer / version stripe ───────────────────────────── */}
      <div style={styles.footer}>
        <span>Page: Audience Analytics v{PAGE_VERSION}</span>
        <span>Source: audience lake snapshots + ignite_analytics.email_events (Athena)</span>
        <span>Snapshot: {status?.latest_dt || 'none'}</span>
        <span>Read {readEnabled ? 'enabled' : 'dark'}</span>
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

  // ── Tab nav ──
  tabNav: {
    display: 'flex', gap: 4, borderBottom: `1px solid ${COLORS.borderStrong}`,
    marginBottom: 20, flexWrap: 'wrap',
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
  presetRow: { display: 'flex', gap: 6, alignItems: 'flex-end', paddingBottom: 1 },
  presetBtn: {
    border: '1px solid', borderRadius: 999, padding: '6px 12px',
    fontSize: 12, fontWeight: 600, cursor: 'pointer', background: 'transparent',
    whiteSpace: 'nowrap',
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
  kpiInlineRow: {
    display: 'flex', gap: 18, flexWrap: 'wrap', marginBottom: 12,
    padding: '10px 14px', background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.border}`, borderRadius: 8, fontSize: 13,
  },
  kpiInline: { color: COLORS.textSecondary, fontVariantNumeric: 'tabular-nums' },

  // ── Charts / legends ──
  seriesToggleRow: { display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 10 },
  legendChip: {
    display: 'inline-flex', alignItems: 'center', gap: 6,
    border: '1px solid', borderRadius: 999, padding: '4px 10px',
    fontSize: 11, fontWeight: 600,
  },
  chartTip: {
    background: '#0d1226', border: `1px solid ${COLORS.borderStrong}`,
    borderRadius: 8, padding: '10px 12px', fontSize: 12, minWidth: 170,
    boxShadow: '0 8px 24px rgba(0,0,0,0.5)',
  },
  chartTipTitle: { color: COLORS.textPrimary, fontWeight: 700, marginBottom: 6, fontVariantNumeric: 'tabular-nums' },
  chartTipRow: { display: 'flex', alignItems: 'center', gap: 8, padding: '2px 0' },
  chartTipDot: { width: 8, height: 8, borderRadius: 999, flexShrink: 0 },

  // ── Composition horizontal bars ──
  mixList: { display: 'flex', flexDirection: 'column', gap: 6 },
  mixRow: { display: 'flex', alignItems: 'center', gap: 12 },
  mixType: { width: 160, fontFamily: 'monospace', fontSize: 12, textAlign: 'right', flexShrink: 0 },
  mixBarTrack: {
    flex: 1, height: 16, background: 'rgba(255,255,255,0.04)',
    borderRadius: 4, overflow: 'hidden',
  },
  mixBarFill: { height: '100%', borderRadius: 4, opacity: 0.8, minWidth: 2 },
  mixCount: { width: 110, textAlign: 'right', fontVariantNumeric: 'tabular-nums', fontSize: 12, color: COLORS.textPrimary, flexShrink: 0 },
  mixShare: { width: 52, textAlign: 'right', fontVariantNumeric: 'tabular-nums', fontSize: 11, color: COLORS.textMuted, flexShrink: 0 },
  mixEng: { width: 80, textAlign: 'right', fontVariantNumeric: 'tabular-nums', fontSize: 11, color: COLORS.accentAlt, flexShrink: 0 },

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

  // ── Member lookup ──
  profileCard: {
    padding: 16, background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.border}`, borderRadius: 8, marginBottom: 14,
  },
  blockTitle: {
    fontSize: 11, fontWeight: 700, color: COLORS.textSecondary,
    textTransform: 'uppercase', letterSpacing: 0.6, margin: '16px 0 8px',
  },
  metaGrid: {
    display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))',
    gap: 14,
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

export default AudienceAnalytics;
