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
  faChartLine, faChevronDown, faChevronRight, faTimes,
  faCheckCircle, faTimesCircle, faBullseye, faHistory,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Bar, Line, XAxis, YAxis,
  CartesianGrid, Tooltip,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';
import { useToast } from '../shared/ToastSystem';

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
  dt: string;
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

type RouteTypeFilter = '' | 'pmta_direct' | 'ses' | 'ses_tenant';

interface DraftFilters {
  from: string;
  to: string;
  ispGroup: string;
  brand: string;
  routeType: RouteTypeFilter;
}

interface AppliedFilters extends DraftFilters {
  nonce: number; // bumped on every explicit Run — bypasses the module cache
}

interface RecentCampaign {
  id: string;
  at: string; // ISO timestamp of last lookup
}

type TabId = 'overview' | 'dimensions' | 'campaign' | 'raw';

type SortDir = 'asc' | 'desc';
interface SortState { col: string; dir: SortDir }

// ═══════════════════════════════════════════════════════════════════════════
// STYLE TOKENS (match Welcome Audience Health / Analytics dark theme)
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

const todayUTC = () => new Date().toISOString().slice(0, 10);
const daysAgoUTC = (n: number) => new Date(Date.now() - n * 86400000).toISOString().slice(0, 10);

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

// ── Route funnel (v2.2) ─────────────────────────────────────────────────────
// Pivots the group_by=source,route_type,event_type companion query into the
// per-route funnel: PMTA-direct (attempted derived) vs SES relay (attempted
// native). Bounce figures here come from pmta+ses sources only by
// construction — the 'app' source never enters this pivot's funnel rows.
function RouteFunnelPanel({ rows }: { rows: BreakdownRow[] }) {
  const get = (src: string, rt: string | null, et: string): number =>
    rows.reduce((a, r) => {
      if ((r.keys['source'] ?? '') !== src) return a;
      if (rt !== null && (r.keys['route_type'] ?? '') !== rt) return a;
      return (r.keys['event_type'] ?? '') === et ? a + r.count : a;
    }, 0);

  const pmtaDelivered = get('pmta', 'pmta_direct', 'delivered');
  const pmtaHard = get('pmta', 'pmta_direct', 'hard_bounce');
  const pmtaSoft = get('pmta', 'pmta_direct', 'soft_bounce');
  const pmtaAttempted = pmtaDelivered + pmtaHard + pmtaSoft;
  const handoffs = get('pmta', null, 'relayed_to_ses');
  const sesAttempted = get('ses', null, 'attempted');
  const sesDelivered = get('ses', null, 'delivered');
  const sesBounced = get('ses', null, 'bounced') + get('ses', null, 'hard_bounce') + get('ses', null, 'soft_bounce');
  if (pmtaAttempted === 0 && sesAttempted === 0) return null;

  const pct = (n: number, d: number) => (d > 0 ? `${((n / d) * 100).toFixed(1)}%` : '—');
  const cell: React.CSSProperties = { padding: '6px 14px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
  const head: React.CSSProperties = { ...cell, color: '#9ca3af', fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5 };
  const rowLabel: React.CSSProperties = { ...cell, textAlign: 'left', fontWeight: 600 };

  return (
    <div style={{ marginTop: 20 }}>
      <div style={styles.subPanelTitle}>
        Route funnel — performance coupled per sending route
      </div>
      <table style={{ borderCollapse: 'collapse', width: '100%', fontSize: 13 }}>
        <thead>
          <tr>
            <th style={{ ...head, textAlign: 'left' }}>Route</th>
            <th style={head}>Attempted</th>
            <th style={head}>Delivered</th>
            <th style={head}>Hard</th>
            <th style={head}>Soft / Bounced</th>
            <th style={head}>Accept rate</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td style={rowLabel}>Direct</td>
            <td style={cell} title="derived: delivered + bounces (direct sends do not record a separate attempted event)">{fmt(pmtaAttempted)}*</td>
            <td style={cell}>{fmt(pmtaDelivered)}</td>
            <td style={cell}>{fmt(pmtaHard)}</td>
            <td style={cell}>{fmt(pmtaSoft)}</td>
            <td style={cell}>{pct(pmtaDelivered, pmtaAttempted)}</td>
          </tr>
          <tr>
            <td style={rowLabel}>Relay route</td>
            <td style={cell} title={`recorded attempted; cross-check: ${fmt(handoffs)} relay handoffs`}>{fmt(sesAttempted)}</td>
            <td style={cell}>{fmt(sesDelivered)}</td>
            <td style={cell} colSpan={2}>{fmt(sesBounced)}</td>
            <td style={cell}>{pct(sesDelivered, sesAttempted)}</td>
          </tr>
          <tr>
            <td style={{ ...rowLabel, borderTop: '1px solid #374151' }}>TOTAL</td>
            <td style={{ ...cell, borderTop: '1px solid #374151' }}>{fmt(pmtaAttempted + sesAttempted)}</td>
            <td style={{ ...cell, borderTop: '1px solid #374151' }}>{fmt(pmtaDelivered + sesDelivered)}</td>
            <td style={{ ...cell, borderTop: '1px solid #374151' }} colSpan={2}>{fmt(pmtaHard + pmtaSoft + sesBounced)}</td>
            <td style={{ ...cell, borderTop: '1px solid #374151' }}>{pct(pmtaDelivered + sesDelivered, pmtaAttempted + sesAttempted)}</td>
          </tr>
        </tbody>
      </table>
      <div style={{ fontSize: 11, color: '#9ca3af', marginTop: 6 }}>
        * Direct-route attempted is derived (delivered+bounces) until a separate attempted event is recorded.
        {' '}Relay-route attempted ≈ handoffs ({fmt(handoffs)}) is the pipeline-consistency check.
      </div>
    </div>
  );
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
// PIVOT — breakdown rows → metric rows / daily series / event mix
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
      dt: dtKey,
      attempted: c.attempted,
      delivered: c.delivered,
      opens: c.opens,
      clicks: c.clicks,
      hardPct: r.hard,
      softPct: r.soft,
      compPct: r.complaint,
    };
  });
}

// Total per raw event_type (including unknown types), sorted desc — for the mix panel.
function eventMix(rows: BreakdownRow[]): Array<{ type: string; count: number }> {
  const m = new Map<string, number>();
  for (const r of rows) {
    const et = (r.keys['event_type'] ?? '(none)') || '(none)';
    m.set(et, (m.get(et) || 0) + r.count);
  }
  return Array.from(m.entries())
    .map(([type, count]) => ({ type, count }))
    .sort((a, b) => b.count - a.count);
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

// Global toolbar filters → breakdown filter params.
function filterParams(f: AppliedFilters): Record<string, string> {
  const out: Record<string, string> = {};
  if (f.ispGroup.trim()) out.isp_group = f.ispGroup.trim();
  if (f.brand.trim()) out.brand = f.brand.trim();
  if (f.routeType) out.route_type = f.routeType;
  return out;
}

const COMMON_ISP_GROUPS = ['gmail', 'yahoo', 'microsoft', 'aol', 'comcast', 'charter', 'att', 'verizon', 'other'];

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

// Removable filter chip.
const Chip: React.FC<{ label: string; onRemove?: () => void; tone?: string }> = ({ label, onRemove, tone }) => (
  <span style={{ ...styles.chip, borderColor: (tone || COLORS.accent) + '55', color: tone || COLORS.accent }}>
    {label}
    {onRemove && (
      <button style={styles.chipX} onClick={onRemove} title="Remove filter">
        <FontAwesomeIcon icon={faTimes} />
      </button>
    )}
  </span>
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

const TrendChart: React.FC<{
  data: DailyPoint[];
  visible: Set<string>;
  height: number;
}> = ({ data, visible, height }) => (
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
        yAxisId="left"
        tickFormatter={(v: number) => fmtCompact(v)}
        tick={{ fill: COLORS.textMuted, fontSize: 10 }}
        axisLine={false}
        tickLine={false}
        width={48}
      />
      <YAxis
        yAxisId="right"
        orientation="right"
        tickFormatter={(v: number) => `${v}%`}
        tick={{ fill: COLORS.textMuted, fontSize: 10 }}
        axisLine={false}
        tickLine={false}
        width={44}
      />
      <Tooltip content={<ChartTip />} cursor={{ fill: 'rgba(255,255,255,0.04)' }} />
      {TREND_SERIES.filter((s) => s.kind === 'bar' && visible.has(s.id)).map((s) => (
        <Bar key={s.id} yAxisId="left" dataKey={s.id} name={s.label} fill={s.color}
          fillOpacity={0.75} radius={[2, 2, 0, 0]} maxBarSize={28} />
      ))}
      {TREND_SERIES.filter((s) => s.kind === 'line' && visible.has(s.id)).map((s) => (
        <Line key={s.id} yAxisId="right" dataKey={s.id} name={s.label} stroke={s.color}
          strokeWidth={2} dot={{ r: 2, fill: s.color, strokeWidth: 0 }} connectNulls type="monotone" />
      ))}
    </ComposedChart>
  </ResponsiveContainer>
);

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
  { id: 'attempted', label: 'Attempted', value: (r) => r.counts.attempted, render: (r) => numCell(r.counts.attempted) },
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

function sortMetricRows(rows: MetricRow[], sort: SortState): MetricRow[] {
  const col = METRIC_COLS.find((c) => c.id === sort.col);
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
}> = ({ dimLabel, rows, totals, sort, onSort, expandedKey, onToggleExpand, renderExpanded }) => {
  const sorted = useMemo(() => sortMetricRows(rows, sort), [rows, sort]);
  const expandable = !!onToggleExpand;
  const arrow = (col: string) => sort.col === col ? (sort.dir === 'desc' ? ' ▾' : ' ▴') : '';
  const renderCells = (r: MetricRow, isTotal: boolean) => METRIC_COLS.map((c) => (
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
            {METRIC_COLS.map((c) => (
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
                    <td colSpan={METRIC_COLS.length + 1} style={styles.expandCell}>
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

const PRESETS: Array<{ label: string; from: () => string; to: () => string }> = [
  { label: 'Today', from: () => todayUTC(), to: () => todayUTC() },
  { label: 'Yesterday', from: () => daysAgoUTC(1), to: () => daysAgoUTC(1) },
  { label: '7D', from: () => daysAgoUTC(6), to: () => todayUTC() },
  { label: '14D', from: () => daysAgoUTC(13), to: () => todayUTC() },
  { label: '30D', from: () => daysAgoUTC(29), to: () => todayUTC() },
];

const Toolbar: React.FC<{
  draft: DraftFilters;
  setDraft: React.Dispatch<React.SetStateAction<DraftFilters>>;
  applied: AppliedFilters;
  onRun: (next: DraftFilters) => void;
}> = ({ draft, setDraft, applied, onRun }) => {
  const chips: Array<{ label: string; tone?: string; onRemove?: () => void }> = [
    { label: `${applied.from} → ${applied.to}` },
  ];
  if (applied.ispGroup.trim()) chips.push({
    label: `isp_group=${applied.ispGroup.trim()}`, tone: COLORS.accentAlt,
    onRemove: () => { const next = { ...draft, ispGroup: '' }; setDraft(next); onRun(next); },
  });
  if (applied.brand.trim()) chips.push({
    label: `brand=${applied.brand.trim()}`, tone: COLORS.accentPink,
    onRemove: () => { const next = { ...draft, brand: '' }; setDraft(next); onRun(next); },
  });
  if (applied.routeType) chips.push({
    label: `route_type=${applied.routeType}`, tone: INFO_BLUE,
    onRemove: () => { const next = { ...draft, routeType: '' as RouteTypeFilter }; setDraft(next); onRun(next); },
  });

  return (
    <div style={styles.toolbar}>
      <div style={styles.toolbarRow}>
        <div style={styles.presetRow}>
          {PRESETS.map((p) => {
            const active = draft.from === p.from() && draft.to === p.to();
            return (
              <button
                key={p.label}
                style={{
                  ...styles.presetBtn,
                  color: active ? COLORS.accent : COLORS.textSecondary,
                  borderColor: active ? COLORS.accent + '66' : COLORS.borderStrong,
                  background: active ? COLORS.accent + '14' : 'transparent',
                }}
                onClick={() => setDraft((d) => ({ ...d, from: p.from(), to: p.to() }))}
              >
                {p.label}
              </button>
            );
          })}
        </div>
        <label style={styles.fieldLabel}>From
          <input type="date" value={draft.from} max={draft.to}
            onChange={(e) => setDraft((d) => ({ ...d, from: e.target.value }))} style={styles.input} />
        </label>
        <label style={styles.fieldLabel}>To
          <input type="date" value={draft.to} min={draft.from} max={todayUTC()}
            onChange={(e) => setDraft((d) => ({ ...d, to: e.target.value }))} style={styles.input} />
        </label>
        <label style={styles.fieldLabel}>isp_group
          <input type="text" list="elx-isp-groups" value={draft.ispGroup} placeholder="all"
            onChange={(e) => setDraft((d) => ({ ...d, ispGroup: e.target.value }))}
            style={{ ...styles.input, width: 120 }} />
        </label>
        <datalist id="elx-isp-groups">
          {COMMON_ISP_GROUPS.map((g) => <option key={g} value={g} />)}
        </datalist>
        <label style={styles.fieldLabel}>brand
          <input type="text" value={draft.brand} placeholder="all"
            onChange={(e) => setDraft((d) => ({ ...d, brand: e.target.value }))}
            style={{ ...styles.input, width: 150 }} />
        </label>
        <label style={styles.fieldLabel}>route_type
          <select value={draft.routeType}
            onChange={(e) => setDraft((d) => ({ ...d, routeType: e.target.value as RouteTypeFilter }))}
            style={{ ...styles.input, width: 130 }}>
            <option value="">all</option>
            <option value="pmta_direct">pmta_direct</option>
            <option value="ses">ses</option>
            <option value="ses_tenant">ses_tenant</option>
          </select>
        </label>
        <button style={styles.primaryBtn} onClick={() => onRun(draft)}>
          <FontAwesomeIcon icon={faSearch} /> Run
        </button>
      </div>
      <div style={styles.chipRow}>
        <span style={styles.chipRowLabel}>Active (Overview &amp; Dimensions):</span>
        {chips.map((c, i) => <Chip key={i} label={c.label} tone={c.tone} onRemove={c.onRemove} />)}
      </div>
    </div>
  );
};

// ─── Tab 1: Overview ────────────────────────────────────────────────────────

const OverviewTab: React.FC<{ applied: AppliedFilters }> = ({ applied }) => {
  const { addToast } = useToast();
  const [rows, setRows] = useState<BreakdownRow[]>([]);
  const [routeRows, setRouteRows] = useState<BreakdownRow[]>([]);
  const [humanClicks, setHumanClicks] = useState<number>(0);
  const [meta, setMeta] = useState<FetchMeta | null>(null);
  const [truncated, setTruncated] = useState(false);
  const [loading, setLoading] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState('');
  const [visible, setVisible] = useState<Set<string>>(
    () => new Set(['delivered', 'hardPct', 'softPct', 'compPct'])
  );
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async (bypass: boolean) => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    setError('');
    try {
      // v2.2: days are America/Denver (local_dt); two companion queries power
      // the per-route funnel (source,route_type) and the human-verdict click
      // count (is_machine_click=false). They share the abort signal.
      const [res, routeRes, humanRes] = await Promise.all([
        fetchBreakdown(
          { from: applied.from, to: applied.to, groupBy: ['local_dt', 'event_type'], limit: 5000, filters: filterParams(applied) },
          applied.nonce,
          { signal: ctl.signal, bypass }
        ),
        fetchBreakdown(
          { from: applied.from, to: applied.to, groupBy: ['source', 'route_type', 'event_type'], limit: 500, filters: filterParams(applied) },
          applied.nonce,
          { signal: ctl.signal, bypass }
        ),
        fetchBreakdown(
          {
            from: applied.from, to: applied.to, groupBy: ['event_type'], limit: 10,
            filters: { ...filterParams(applied), event_type: 'click', is_machine_click: 'false' },
          },
          applied.nonce,
          { signal: ctl.signal, bypass }
        ),
      ]);
      setRows(res.data.rows);
      setRouteRows(routeRes.data.rows);
      setHumanClicks(humanRes.data.rows.reduce((a, x) => a + x.count, 0));
      setTruncated(!!res.data.truncated);
      setMeta(res.meta);
      setLoaded(true);
    } catch (e) {
      if (isAbortError(e)) return;
      const msg = e instanceof Error ? e.message : String(e);
      // Clear stale data — old numbers must never render under the
      // newly-applied range/filter labels after the toast fades.
      setRows([]);
      setRouteRows([]);
      setHumanClicks(0);
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

  const totals = useMemo(() => totalsFromBreakdown(rows), [rows]);
  const daily = useMemo(() => dailySeries(rows), [rows]);
  const mix = useMemo(() => eventMix(rows), [rows]);
  const mixTotal = useMemo(() => mix.reduce((a, m) => a + m.count, 0), [mix]);

  const toggleSeries = (id: string) => setVisible((v) => {
    const next = new Set(v);
    if (next.has(id)) next.delete(id); else next.add(id);
    return next;
  });

  const c = totals.counts;
  const r = totals.rates;
  const dn = denomTitle(r);

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
              not a recipient delivery. <TimingNote meta={meta} />
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
              <KpiCard label="Deferral Retry Events" value={c.delays} color={COLORS.warn}
                extra="per-RETRY events, not unique messages — throttle ISPs emit dozens per message" />
              <KpiCard label="Complaints" value={c.complaints} color={COMPLAINT_ROSE}
                rate={r.complaint} rateLabel="complaint" denomNote={dn} />
              <KpiCard label="Opens (raw)" value={c.opens} color={OPEN_CYAN}
                rate={r.open} rateLabel="open" denomNote={deliveredTitle(c)}
                extra="raw events — machine traffic included" />
              <KpiCard label="Clicks (raw)" value={c.clicks} color={CLICK_VIOLET}
                rate={r.click} rateLabel="click" denomNote={deliveredTitle(c)}
                extra={`human-flagged: ${fmt(humanClicks)} · CTOR(raw): ${fmtPct(r.ctor)}`} />
              <KpiCard label="Relayed" value={c.relayed} color={INFO_BLUE}
                extra="relay handoff — not a delivery" />
            </div>

            {/* Route funnel — attempted/delivered/bounces with route as a
                dimension; bounce numbers read pmta+ses sources ONLY (the
                'app' source carries engagement; its 2026-06-11 bounce rows
                are known duplicates). */}
            <RouteFunnelPanel rows={routeRows} />


            {/* Daily trend */}
            <div style={{ marginTop: 20 }}>
              <div style={styles.subPanelTitle}>Daily trend</div>
              <SeriesToggles visible={visible} onToggle={toggleSeries} />
              {daily.length === 0 ? <EmptyRow label="No daily datapoints." /> : (
                <TrendChart data={daily} visible={visible} height={300} />
              )}
            </div>

            {/* Event mix */}
            <div style={{ marginTop: 20 }}>
              <div style={styles.subPanelTitle}>Event mix ({fmt(mixTotal)} events)</div>
              <div style={styles.mixList}>
                {mix.map((m) => {
                  const color = eventTypeColor(m.type);
                  const share = mixTotal > 0 ? (m.count / mixTotal) * 100 : 0;
                  return (
                    <div key={m.type} style={styles.mixRow}>
                      <div style={{ ...styles.mixType, color }}>{m.type}</div>
                      <div style={styles.mixBarTrack}>
                        <div style={{ ...styles.mixBarFill, width: `${Math.max(share, 0.5)}%`, background: color }} />
                      </div>
                      <div style={styles.mixCount}>{fmt(m.count)}</div>
                      <div style={styles.mixShare}>{share.toFixed(1)}%</div>
                    </div>
                  );
                })}
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
    fetchBreakdown(
      {
        from: applied.from, to: applied.to, groupBy: ['dt', 'event_type'], limit: 5000,
        filters: { ...filterParams(applied), [dim]: value },
      },
      applied.nonce,
      { signal: ctl.signal }
    ).then((res) => {
      setData({ rows: res.data.rows, meta: res.meta, truncated: !!res.data.truncated });
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
        <span style={styles.expandStat}>attempted {fmt(tc.attempted)}</span>
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
      const res = await fetchBreakdown(
        { from: applied.from, to: applied.to, groupBy: [dim, 'event_type'], limit: 5000, filters: filterParams(applied) },
        applied.nonce,
        { signal: ctl.signal, bypass }
      );
      setFetched({ rows: res.data.rows, dim, meta: res.meta, truncated: !!res.data.truncated });
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
            Hard%&gt;0.5 / Comp%&gt;0.05 / Del%&lt;97 amber. Click a row for its daily trend. <TimingNote meta={fresh && fetched ? fetched.meta : null} />
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
  ispRows: BreakdownRow[];             // (b) group_by=isp_group,event_type
  ispMeta: FetchMeta | null;
  ispTruncated: boolean;
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
  const [from, setFrom] = useState(daysAgoUTC(29));
  const [to, setTo] = useState(todayUTC());
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
    const lakeFilters = { campaign_id: id };
    const [a, b, c, d] = await Promise.allSettled([
      fetchBreakdown({ from: f, to: t, groupBy: ['event_type'], limit: 5000, filters: lakeFilters }, 0, { signal: ctl.signal, bypass: true }),
      fetchBreakdown({ from: f, to: t, groupBy: ['isp', 'event_type'], limit: 5000, filters: lakeFilters }, 0, { signal: ctl.signal, bypass: true }),
      fetchCampaignSummary(id, ctl.signal),
      fetchLakeEvents({ campaign_id: id, limit: 100 }, { signal: ctl.signal }),
    ]);
    if (ctl.signal.aborted) return;

    const res: LookupResult = {
      id,
      typeTotals: null, typeTotalsMeta: null, typeTruncated: false,
      ispRows: [], ispMeta: null, ispTruncated: false,
      cc: null, ccError: '',
      events: [], eventsMeta: null,
    };
    if (a.status === 'fulfilled') {
      res.typeTotals = totalsFromBreakdown(a.value.data.rows);
      res.typeTotalsMeta = a.value.meta;
      res.typeTruncated = !!a.value.data.truncated;
    } else if (!isAbortError(a.reason)) {
      addToast({ type: 'error', title: 'Campaign funnel query failed', message: a.reason instanceof Error ? a.reason.message : String(a.reason) });
    }
    if (b.status === 'fulfilled') {
      res.ispRows = b.value.data.rows;
      res.ispMeta = b.value.meta;
      res.ispTruncated = !!b.value.data.truncated;
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

  // The lake's ONLY open/click emit site is the SES events webhook — PMTA-direct
  // campaigns have NO lake open/click events (the internal tracking pixel does
  // not emit to the lake), so lake 0 < tracking is expected, not a mismatch.
  // Opens/Clicks recon rows are therefore only rendered for SES-routed mail.
  const sesRouted = cc?.route_type === 'ses' || cc?.route_type === 'ses_tenant';

  // No Complaints row: the campaign-summary DETAIL endpoint has no complaints
  // field, so it could never reconcile (lake complaints live in the funnel KPIs).
  const reconRows: ReconRow[] = (lakeC && cc) ? [
    { label: 'Delivered', lake: lakeC.delivered, cc: cc.delivered },
    // The lake's ClassifyBounce folds policy/routing/connection categories into
    // hard_bounce, while Campaign Center v1.4 splits those out as
    // reputation_block — so the comparable tracking number is the SUM.
    { label: 'Hard bounce *', lake: lakeC.hard, cc: cc.hard_bounce + (cc.reputation_block ?? 0), color: HARD_RED, note: 'tracking = hard bounces + provider blocks (analytics folds provider blocks into hard)' },
    { label: 'Soft bounce', lake: lakeC.soft, cc: cc.soft_bounce, color: SOFT_AMBER },
    ...(sesRouted ? [
      { label: 'Opens *', lake: lakeC.opens, cc: cc.unique_opens, color: OPEN_CYAN, note: 'analytics = total open events; tracking = unique opens' },
      { label: 'Clicks *', lake: lakeC.clicks, cc: cc.unique_clicks, color: CLICK_VIOLET, note: 'analytics = total click events; tracking = unique clicks' },
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
            <input type="date" value={to} min={from} max={todayUTC()} onChange={(e) => setTo(e.target.value)} style={styles.input} />
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
                  group_by=event_type, campaign_id={truncate(result.id, 14)}, {from} → {to}. <TimingNote meta={result.typeTotalsMeta} />
                </p>
              </div>
            </div>
            <TruncationBanner truncated={result.typeTruncated} limit={5000} />
            {lakeC && lakeR ? (
              lakeC.total === 0 ? <EmptyRow label="No analytics events for this campaign in the selected range — widen the date range." /> : (
                <div style={styles.kpiGrid}>
                  <KpiCard label="Attempted" value={lakeC.attempted} color={COLORS.accent} />
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
                  group_by=isp_group,event_type scoped to this campaign. <TimingNote meta={result.ispMeta} />
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
                    {sesRouted ? (
                      <>
                        * Opens/clicks compare different units by design: analytics counts <em>total</em> open/click events
                        (every pixel fire / link hit is its own event), while Campaign Center reports <em>unique</em> opens/clicks
                        (deduped per recipient) — expect analytics ≥ tracking; a large gap usually means heavy re-opens or bot scanning,
                        not data loss. CC total opens={fmt(cc.total_opens)} · total clicks={fmt(cc.total_clicks)} for reference.
                      </>
                    ) : (
                      <>
                        Open/click events for direct-route mail are tracked in Campaign Center only (the internal
                        tracking pixel does not feed analytics), so Opens/Clicks rows are omitted here.
                      </>
                    )}
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
        <label style={styles.fieldLabel}>dt (day)
          <input type="date" value={dt} max={todayUTC()} onChange={(e) => setDt(e.target.value)} style={styles.input} />
        </label>
        <label style={styles.fieldLabel}>campaign_id
          <input type="text" value={campaignId} placeholder="UUID" onChange={(e) => setCampaignId(e.target.value)} style={{ ...styles.input, width: 280 }} />
        </label>
        <label style={styles.fieldLabel}>isp_group
          <input type="text" list="elx-isp-groups" value={ispGroup} placeholder="gmail" onChange={(e) => setIspGroup(e.target.value)} style={{ ...styles.input, width: 120 }} />
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

// ═══════════════════════════════════════════════════════════════════════════
// MAIN
// ═══════════════════════════════════════════════════════════════════════════

const TABS: Array<{ id: TabId; label: string; icon: typeof faChartLine }> = [
  { id: 'overview', label: 'Overview', icon: faChartLine },
  { id: 'dimensions', label: 'Dimensions', icon: faLayerGroup },
  { id: 'campaign', label: 'Campaign Lookup', icon: faBullseye },
  { id: 'raw', label: 'Raw Events', icon: faTable },
];

export const EventLakeExplorer: React.FC = () => {
  const { addToast } = useToast();

  const [status, setStatus] = useState<LakeStatus | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState('');

  const [activeTab, setActiveTab] = useState<TabId>('overview');
  const [visited, setVisited] = useState<Set<TabId>>(() => new Set<TabId>(['overview']));

  const [draft, setDraft] = useState<DraftFilters>(() => ({
    from: daysAgoUTC(6), to: todayUTC(), ispGroup: '', brand: '', routeType: '',
  }));
  const [applied, setApplied] = useState<AppliedFilters>(() => ({
    from: daysAgoUTC(6), to: todayUTC(), ispGroup: '', brand: '', routeType: '', nonce: 0,
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
        <div style={styles.statusStrip}>
          <EnableBadge label="Write" enabled={status.enabled_write} />
          <EnableBadge label="Read" enabled={status.enabled_read} />
          <div style={styles.statusDivider} />
          <Counter label="Sent" value={status.sent} color={COLORS.good} />
          <Counter label="Failed" value={status.failed} color={COLORS.danger} />
          <Counter label="Dropped" value={status.dropped} color={COLORS.warn} />
        </div>
      )}

      {/* ─── DARK empty state ──────────────────────────────────── */}
      {status && !readEnabled && (
        <div style={styles.darkCard}>
          <FontAwesomeIcon icon={faMoon} style={{ fontSize: 28, color: COLORS.accentAlt }} />
          <div style={{ flex: 1 }}>
            <div style={styles.darkTitle}>Event reporting read layer is off</div>
            <div style={styles.darkBody}>
              The analytics database read layer is not configured. Enable the analytics results location on the
              server to turn on breakdown &amp; event queries. Until then this screen shows the write-side
              counters above; query controls stay disabled.
            </div>
            <div style={styles.darkHint}>
              <FontAwesomeIcon icon={faInfoCircle} style={{ color: COLORS.textMuted, marginRight: 6 }} />
              Write side {status.enabled_write ? 'is recording events to the analytics pipeline' : 'is also off (analytics pipeline not configured)'}.
            </div>
          </div>
        </div>
      )}

      {/* ─── Toolbar + tabs (only when read enabled) ───────────── */}
      {status && readEnabled && (
        <>
          <Toolbar draft={draft} setDraft={setDraft} applied={applied} onRun={onRun} />

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

  // ── Toolbar ──
  toolbar: {
    background: COLORS.bgPanel, border: `1px solid ${COLORS.border}`,
    borderRadius: 10, padding: '14px 18px', marginBottom: 16,
  },
  toolbarRow: {
    display: 'flex', alignItems: 'flex-end', gap: 12, flexWrap: 'wrap',
  },
  presetRow: { display: 'flex', gap: 6, alignItems: 'flex-end', paddingBottom: 1 },
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
  chip: {
    display: 'inline-flex', alignItems: 'center', gap: 6,
    border: '1px solid', borderRadius: 999, padding: '4px 10px',
    fontSize: 12, fontWeight: 500, fontVariantNumeric: 'tabular-nums',
  },
  chipX: {
    background: 'transparent', border: 'none', color: 'inherit',
    cursor: 'pointer', fontSize: 10, padding: 0, display: 'inline-flex',
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

  // ── Event mix ──
  mixList: { display: 'flex', flexDirection: 'column', gap: 6 },
  mixRow: { display: 'flex', alignItems: 'center', gap: 12 },
  mixType: { width: 140, fontFamily: 'monospace', fontSize: 12, textAlign: 'right', flexShrink: 0 },
  mixBarTrack: {
    flex: 1, height: 16, background: 'rgba(255,255,255,0.04)',
    borderRadius: 4, overflow: 'hidden',
  },
  mixBarFill: { height: '100%', borderRadius: 4, opacity: 0.8, minWidth: 2 },
  mixCount: { width: 110, textAlign: 'right', fontVariantNumeric: 'tabular-nums', fontSize: 12, color: COLORS.textPrimary, flexShrink: 0 },
  mixShare: { width: 52, textAlign: 'right', fontVariantNumeric: 'tabular-nums', fontSize: 11, color: COLORS.textMuted, flexShrink: 0 },

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
