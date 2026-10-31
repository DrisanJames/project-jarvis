// Reporting — Overview, Dimensions, Growth, Campaign Lookup and Conversions.
//
// Reads (all read only, via apiFetch):
//   GET /api/mailing/analytics/lake/status
//   GET /api/mailing/analytics/lake/cube?from=&to=[&dim=&brand=&refresh=1]
//       One query per date range. Overview and Dimensions slice it in the
//       browser (reporting/cube.ts), so filters and Rows never reach the server.
//       dim=email_domain|vmta|pool is one extra query per dim, cached.
//   GET /api/mailing/analytics/lake/breakdown — hourly trend + Campaign Lookup
//   GET /api/mailing/analytics/lake/events — Campaign Lookup latest events
//   GET /api/mailing/analytics/deferral-funnel?from=&to= — fail-soft, cached
//   GET /api/mailing/analytics/campaign-tree + /campaign-tree/campaigns
//   GET /api/mailing/analytics/campaign-summary/{id}
//
// Number rules (docs/METRIC_CONTRACT.md): Sent = delivered + bounces. Open and
// click rates divide by delivered. Hard bounce is always red, soft bounce always
// amber, and the two are never summed. Passed to SES is not a delivery.

import React, { useEffect, useMemo, useRef, useState, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faSyncAlt, faExclamationTriangle, faCircle, faInfoCircle,
  faLayerGroup, faChartLine, faChevronDown, faChevronRight,
  faCheckCircle, faTimesCircle, faBullseye, faCrosshairs, faSeedling,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Area, Line, XAxis, YAxis,
  CartesianGrid, Tooltip, ReferenceLine,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';
import { useToast } from '../shared/ToastSystem';
import { colors as theme } from '../shared/theme';
import {
  FilterBar,
  denverToday, daysAgoDenver,
  lakeFilterParams as filterParams,
} from '../shared/filters';
import type { LakeFilterDraft, AppliedLakeFilters } from '../shared/filters';
import OfferAlignmentTab from '../offeralignment/OfferAlignmentTab';
import GrowthTab from '../growth/GrowthTab';
import {
  apexOptions, brandApex, buildCampaignTree, decodeCube, emailGroup, groupRows,
  ispLabel, laneLabel, sliceCube, typeTotals,
} from '../reporting/cube';
import type { CampaignTreeRow, CubePayload, CubeRow } from '../reporting/cube';

// ═══════════════════════════════════════════════════════════════════════════
// TYPES (match backend JSON keys exactly)
// ═══════════════════════════════════════════════════════════════════════════

interface LakeStatus {
  enabled_read: boolean;
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

// Rates. Every attempt ends one way, so delivered + bounces = sent.
interface Rates {
  denom: number;
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
  day: string; // America/Denver operating day, or hour on the hourly trend
  attempted: number;
  delivered: number;
  opens: number;
  clicks: number;
  hardPct: number | null;
  softPct: number | null;
  compPct: number | null;
}

type DraftFilters = LakeFilterDraft;
type AppliedFilters = AppliedLakeFilters;

type TabId = 'overview' | 'dimensions' | 'growth' | 'campaign' | 'alignment';

type SortDir = 'asc' | 'desc';
interface SortState { col: string; dir: SortDir }

// ═══════════════════════════════════════════════════════════════════════════
// STYLE TOKENS — mapped onto the shared theme tokens.
// ═══════════════════════════════════════════════════════════════════════════

const COLORS = {
  bgDeep:        theme.appBgSolid,
  bgPanel:       theme.panelBgSolid,
  bgPanelAlt:    'var(--j-surface-2)',
  border:        theme.hairline,
  borderStrong:  theme.panelBorderStrong,
  textPrimary:   theme.text,
  textSecondary: theme.textMuted,
  textMuted:     theme.textFaint,
  accent:        theme.indigo400,
  accentAlt:     theme.indigo300,
  accentPink:    theme.indigo200,
  good:          theme.success,
  warn:          theme.warning,
  danger:        theme.danger,
};

// HARD RULE colors (repo CLAUDE.md): hard bounce red, soft bounce amber. Always.
const HARD_RED = 'var(--j-hard)';
const SOFT_AMBER = 'var(--j-soft)';
const INFO_BLUE = 'var(--j-blue)';
const OPEN_CYAN = 'var(--j-cyan)';
const CLICK_VIOLET = 'var(--j-violet)';
const COMPLAINT_ROSE = 'var(--j-danger)';

// ═══════════════════════════════════════════════════════════════════════════
// API LAYER
// ═══════════════════════════════════════════════════════════════════════════

// Parse a non-OK response: 400s carry {error} which we surface verbatim.
async function throwHttpError(res: Response): Promise<never> {
  let msg = `HTTP ${res.status}`;
  try {
    const body: { error?: string } = await res.json();
    if (body && typeof body.error === 'string' && body.error) msg = body.error;
  } catch { /* non-JSON error body — keep HTTP status message */ }
  throw new Error(msg);
}

const isAbortError = (e: unknown): boolean =>
  e instanceof DOMException ? e.name === 'AbortError'
    : e instanceof Error ? e.name === 'AbortError'
    : false;

function fifoSet<T>(cache: Map<string, T>, key: string, value: T, max: number): void {
  cache.set(key, value);
  while (cache.size > max) {
    const oldest = cache.keys().next().value;
    if (oldest === undefined) break;
    cache.delete(oldest);
  }
}

// ── The cube ─────────────────────────────────────────────────────────────────
// The server caches for 5 minutes and shares one query between callers; this
// cache keeps tab flips and Rows changes off the network entirely. Only a new
// date range or Refresh fetches.
interface LoadedCube { rows: CubeRow[]; truncated: boolean }
interface CubeState extends LoadedCube { loading: boolean; error: string }

const EMPTY_CUBE: CubeState = { rows: [], truncated: false, loading: false, error: '' };
const CUBE_CACHE_MAX = 16;
const cubeCache = new Map<string, LoadedCube>();

async function fetchCube(
  q: { from: string; to: string; dim: string; brand: string },
  refresh: boolean,
  signal: AbortSignal
): Promise<LoadedCube> {
  const qs = new URLSearchParams({ from: q.from, to: q.to });
  if (q.dim) qs.set('dim', q.dim);
  if (q.brand) qs.set('brand', q.brand);
  const key = qs.toString();
  const hit = refresh ? undefined : cubeCache.get(key);
  if (hit) return hit;
  if (refresh) qs.set('refresh', '1');
  const res = await apiFetch(`/api/mailing/analytics/lake/cube?${qs.toString()}`, { signal });
  if (!res.ok) await throwHttpError(res);
  const body: CubePayload = await res.json();
  const out: LoadedCube = { rows: decodeCube(body), truncated: !!body.truncated };
  fifoSet(cubeCache, key, out, CUBE_CACHE_MAX);
  return out;
}

function useCube(enabled: boolean, from: string, to: string, refreshTick: number, dim = '', brand = ''): CubeState {
  const [state, setState] = useState<CubeState>(EMPTY_CUBE);
  const seenTick = useRef(refreshTick);
  useEffect(() => {
    const refresh = seenTick.current !== refreshTick;
    seenTick.current = refreshTick;
    if (!enabled) return;
    const ctl = new AbortController();
    // Refresh keeps the numbers on screen under the spinner; a new range clears
    // them so old numbers never sit under new dates.
    setState((s) => ({ ...(refresh ? s : EMPTY_CUBE), loading: true, error: '' }));
    fetchCube({ from, to, dim, brand }, refresh, ctl.signal)
      .then((c) => setState({ ...c, loading: false, error: '' }))
      .catch((e) => {
        if (isAbortError(e)) return;
        setState({ ...EMPTY_CUBE, error: e instanceof Error ? e.message : String(e) });
      });
    return () => ctl.abort();
  }, [enabled, from, to, dim, brand, refreshTick]);
  return state;
}

// ── Deferral funnel (delayed → delivered / still trying / bounced, per ISP) ──
// Fail-soft: a failure hides the tile and columns. It has no brand or sending
// route, so those filters hide it too rather than show a number for everything.
interface DeferralCounts { deferred: number; recovered: number; pending: number; bounced: number }
interface DeferralFunnel {
  total: DeferralCounts;
  rows: (DeferralCounts & { isp_group: string })[];
}

const funnelCache = new Map<string, DeferralFunnel>();

function useFunnel(enabled: boolean, from: string, to: string, refreshTick: number): DeferralFunnel | null {
  const [funnel, setFunnel] = useState<DeferralFunnel | null>(null);
  const seenTick = useRef(refreshTick);
  useEffect(() => {
    const refresh = seenTick.current !== refreshTick;
    seenTick.current = refreshTick;
    if (!enabled) return;
    const key = `${from}|${to}`;
    const hit = refresh ? undefined : funnelCache.get(key);
    if (hit) { setFunnel(hit); return; }
    const ctl = new AbortController();
    setFunnel(null);
    (async () => {
      try {
        const qs = new URLSearchParams({ from, to });
        const res = await apiFetch(`/api/mailing/analytics/deferral-funnel?${qs.toString()}`, { signal: ctl.signal });
        if (!res.ok) await throwHttpError(res);
        const body: DeferralFunnel = await res.json();
        fifoSet(funnelCache, key, body, CUBE_CACHE_MAX);
        setFunnel(body);
      } catch (e) {
        if (!isAbortError(e)) console.warn('[Reporting] deferral funnel failed:', e instanceof Error ? e.message : e);
      }
    })();
    return () => ctl.abort();
  }, [enabled, from, to, refreshTick]);
  return funnel;
}

// ── Breakdown (hourly trend + Campaign Lookup) ───────────────────────────────
const BREAKDOWN_CACHE_MAX = 64;
const breakdownCache = new Map<string, BreakdownResponse>();

interface BreakdownParams {
  from: string;
  to: string;
  groupBy: string[];
  filters?: Record<string, string>;
}

async function fetchBreakdown(
  p: BreakdownParams,
  opts: { signal?: AbortSignal; bypass?: boolean }
): Promise<BreakdownResponse> {
  const qs = new URLSearchParams({ from: p.from, to: p.to, group_by: p.groupBy.join(','), limit: '5000' });
  for (const k of Object.keys(p.filters ?? {}).sort()) {
    const v = (p.filters?.[k] || '').trim();
    if (v) qs.set(k, v);
  }
  const url = `/api/mailing/analytics/lake/breakdown?${qs.toString()}`;
  const hit = opts.bypass ? undefined : breakdownCache.get(url);
  if (hit) return hit;
  const res = await apiFetch(url, { signal: opts.signal });
  if (!res.ok) await throwHttpError(res);
  const data: BreakdownResponse = await res.json();
  const out = { ...data, rows: Array.isArray(data.rows) ? data.rows : [] };
  fifoSet(breakdownCache, url, out, BREAKDOWN_CACHE_MAX);
  return out;
}

async function fetchLakeEvents(campaignId: string, signal: AbortSignal): Promise<LakeEvent[]> {
  const qs = new URLSearchParams({ campaign_id: campaignId, limit: '100' });
  const res = await apiFetch(`/api/mailing/analytics/lake/events?${qs.toString()}`, { signal });
  if (!res.ok) await throwHttpError(res);
  const data: EventsResponse = await res.json();
  return Array.isArray(data.events) ? data.events : [];
}

async function fetchCampaignSummary(id: string, signal: AbortSignal): Promise<CCDetail> {
  const res = await apiFetch(`/api/mailing/analytics/campaign-summary/${encodeURIComponent(id)}`, { signal });
  if (!res.ok) await throwHttpError(res);
  return (await res.json()) as CCDetail;
}

// ═══════════════════════════════════════════════════════════════════════════
// HELPERS — formatting, colors, classification, rates
// ═══════════════════════════════════════════════════════════════════════════

const fmt = (n: number) => (n ?? 0).toLocaleString('en-US');
const fmtPct = (v: number | null, digits = 2): string => (v == null ? '—' : `${v.toFixed(digits)}%`);
const fmtCompact = (n: number) => Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 }).format(n);

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

const EVENT_LABELS: Record<string, string> = {
  relayed_to_ses: 'Passed to SES', delivery_delay: 'Retry', hard_bounce: 'Hard bounce', soft_bounce: 'Soft bounce',
};
const eventLabel = (t: string): string => {
  const k = (t || '').toLowerCase();
  if (EVENT_LABELS[k]) return EVENT_LABELS[k];
  return k ? k[0].toUpperCase() + k.slice(1).replace(/_/g, ' ') : '—';
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

function computeRates(c: EventCounts): Rates {
  const denom = c.delivered + c.hard + c.soft + c.bouncedUntyped;
  const r = (n: number): number | null => (denom > 0 ? (n / denom) * 100 : null);
  return {
    denom,
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

const sentTitle = (r: Rates): string => `Out of ${fmt(r.denom)} sent`;
const deliveredTitle = (c: EventCounts): string => `Out of ${fmt(c.delivered)} delivered`;

// Heat tints — subtle background washes flagging out-of-band rates.
const TINT_RED = 'color-mix(in srgb, var(--j-hard-fill) 14%, transparent)';
const TINT_AMBER = 'color-mix(in srgb, var(--j-soft-fill) 11%, transparent)';
const heatHard = (p: number | null): string | undefined =>
  p == null ? undefined : p > 1 ? TINT_RED : p > 0.5 ? TINT_AMBER : undefined;
const heatComp = (p: number | null): string | undefined =>
  p == null ? undefined : p > 0.1 ? TINT_RED : p > 0.05 ? TINT_AMBER : undefined;
const heatDel = (p: number | null): string | undefined =>
  p == null ? undefined : p < 90 ? TINT_RED : p < 97 ? TINT_AMBER : undefined;

// ═══════════════════════════════════════════════════════════════════════════
// PIVOT — rows → metric rows / daily + hourly series
// ═══════════════════════════════════════════════════════════════════════════

function seriesPoints(byKey: Map<string, Record<string, number>>): DailyPoint[] {
  return Array.from(byKey.keys()).sort().map((key) => {
    const c = countsFromTypeMap(byKey.get(key) || {});
    const r = computeRates(c);
    return {
      day: key,
      attempted: r.denom,
      delivered: c.delivered,
      opens: c.opens,
      clicks: c.clicks,
      hardPct: r.hard,
      softPct: r.soft,
      compPct: r.complaint,
    };
  });
}

function dailySeries(rows: CubeRow[]): DailyPoint[] {
  const byDay = new Map<string, Record<string, number>>();
  groupRows(rows, (r) => r.day).forEach((g, day) => {
    if (day) byDay.set(day, typeTotals(g));
  });
  return seriesPoints(byDay);
}

function hourlySeries(rows: BreakdownRow[]): DailyPoint[] {
  const byHr = new Map<string, Record<string, number>>();
  for (const r of rows) {
    const hrKey = r.keys['local_hour'] ?? '';
    const et = (r.keys['event_type'] ?? '').toLowerCase();
    let m = byHr.get(hrKey);
    if (!m) { m = {}; byHr.set(hrKey, m); }
    m[et] = (m[et] || 0) + r.count;
  }
  return seriesPoints(byHr);
}

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

function totalsFromBreakdown(rows: BreakdownRow[]): MetricRow {
  const m: Record<string, number> = {};
  for (const r of rows) {
    const et = (r.keys['event_type'] ?? '').toLowerCase();
    m[et] = (m[et] || 0) + r.count;
  }
  return makeMetricRow('TOTAL', m);
}

// Delivery rows and source='app' rows counted once each: open and click come only
// from the app stream, everything else only from the delivery rows.
function mergeDeliveryAndEngagement(deliveryRows: BreakdownRow[], engRows: BreakdownRow[]): BreakdownRow[] {
  const isEng = (r: BreakdownRow) => {
    const et = (r.keys['event_type'] ?? '').toLowerCase();
    return et === 'open' || et === 'click';
  };
  return [...deliveryRows.filter((r) => !isEng(r)), ...engRows.filter(isEng)];
}

// ═══════════════════════════════════════════════════════════════════════════
// SMALL SHARED COMPONENTS
// ═══════════════════════════════════════════════════════════════════════════

const LoadingRow: React.FC<{ label?: string }> = ({ label }) => (
  <div style={styles.sectionLoading}>
    <FontAwesomeIcon icon={faSpinner} spin /> {label || 'Loading…'}
  </div>
);

const EmptyRow: React.FC<{ label: string }> = ({ label }) => (
  <div style={styles.sectionEmpty}>{label}</div>
);

const ErrorRow: React.FC<{ error: string; onRetry: () => void }> = ({ error, onRetry }) => (
  <div style={styles.errorShell}>
    <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
    Could not load: {error}
    <button style={styles.inlineRetry} onClick={onRetry}>Try again</button>
  </div>
);

const TruncationBanner: React.FC<{ truncated?: boolean }> = ({ truncated }) => {
  if (!truncated) return null;
  return (
    <div style={styles.truncBanner}>
      <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
      Too much data to show it all, so totals are low. Pick fewer days.
    </div>
  );
};

// Covers a panel while the cube loads or a filter re-slices it.
const BusyOverlay: React.FC<{ show: boolean; label: string }> = ({ show, label }) => {
  if (!show) return null;
  return (
    <div style={styles.busyOverlay} role="status" aria-live="polite">
      <div style={styles.busyPill}><FontAwesomeIcon icon={faSpinner} spin /> {label}</div>
    </div>
  );
};

const SLICE_PULSE_MS = 250;

// True for a moment after `key` changes, so an instant slice still reads as a
// response to the click.
function useSlicePulse(key: string): boolean {
  const [on, setOn] = useState(false);
  const last = useRef(key);
  useEffect(() => {
    if (last.current === key) return;
    last.current = key;
    setOn(true);
    const t = setTimeout(() => setOn(false), SLICE_PULSE_MS);
    return () => clearTimeout(t);
  }, [key]);
  return on;
}

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

// KPI card: big count + optional rate, with what the rate is out of on hover.
const KpiCard: React.FC<{
  label: string;
  value: number;
  color: string;
  rate?: number | null;
  rateTitle?: string;
  title?: string;
  extra?: string;
}> = ({ label, value, color, rate, rateTitle, title, extra }) => (
  <div style={styles.kpiCard} title={title}>
    <div style={styles.kpiLabel}>{label}</div>
    <div style={{ ...styles.kpiValue, color }}>{fmt(value)}</div>
    {rate !== undefined && <div style={{ ...styles.kpiRate, color }} title={rateTitle}>{fmtPct(rate)}</div>}
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
  { id: 'delivered', label: 'Delivered', kind: 'bar', axis: 'left', color: COLORS.good },
  { id: 'attempted', label: 'Sent', kind: 'bar', axis: 'left', color: COLORS.accent },
  { id: 'opens', label: 'Opens', kind: 'bar', axis: 'left', color: OPEN_CYAN },
  { id: 'clicks', label: 'Clicks', kind: 'bar', axis: 'left', color: CLICK_VIOLET },
  { id: 'hardPct', label: 'Hard bounce %', kind: 'line', axis: 'right', color: HARD_RED },
  { id: 'softPct', label: 'Soft bounce %', kind: 'line', axis: 'right', color: SOFT_AMBER },
  { id: 'compPct', label: 'Complaint %', kind: 'line', axis: 'right', color: COMPLAINT_ROSE },
];

// Dashed lines on the % axis, shown with their series (operator 2026-07-02).
const TREND_BASELINES: { seriesId: string; y: number; label: string; color: string }[] = [
  { seriesId: 'compPct', y: 0.3, label: 'Complaint limit 0.3%', color: COMPLAINT_ROSE },
  { seriesId: 'softPct', y: 10, label: 'Soft bounce limit 10%', color: SOFT_AMBER },
];

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
      <CartesianGrid stroke="color-mix(in srgb, var(--j-text-muted) 12%, transparent)" vertical={false} />
      <XAxis
        dataKey="day"
        tickFormatter={(v: string) => (typeof v === 'string' ? v.slice(5) : String(v))}
        tick={{ fill: 'var(--j-text-muted)', fontSize: 11 }}
        interval="preserveStartEnd"
        minTickGap={40}
        axisLine={{ stroke: COLORS.borderStrong }}
        tickLine={false}
      />
      <YAxis
        yAxisId="left"
        tickFormatter={(v: number) => fmtCompact(v)}
        tick={{ fill: 'var(--j-text-muted)', fontSize: 11 }}
        axisLine={false}
        tickLine={false}
        width={52}
      />
      <YAxis
        yAxisId="right"
        orientation="right"
        tickFormatter={(v: number) => `${v}%`}
        tick={{ fill: 'var(--j-text-muted)', fontSize: 11 }}
        axisLine={false}
        tickLine={false}
        width={44}
      />
      <Tooltip content={<ChartTip />} cursor={{ stroke: 'color-mix(in srgb, var(--j-text-muted) 30%, transparent)', strokeWidth: 1 }} />
      {volSeries.map((s) => (
        <Area key={s.id} yAxisId="left" dataKey={s.id} name={s.label} type="monotone"
          stroke={s.color} strokeWidth={2} fill={`url(#lakeTrendFill-${s.id})`}
          dot={false} activeDot={{ r: 3, fill: s.color, strokeWidth: 0 }} connectNulls />
      ))}
      {TREND_SERIES.filter((s) => s.kind === 'line' && visible.has(s.id)).map((s) => (
        <Line key={s.id} yAxisId="right" dataKey={s.id} name={s.label} stroke={s.color}
          strokeWidth={2} dot={{ r: 2, fill: s.color, strokeWidth: 0 }} connectNulls type="monotone" />
      ))}
      {/* Dashed baseline/threshold lines — drawn last so they sit on top of the
          rate lines; each only when its companion rate series is visible. */}
      {TREND_BASELINES.filter((b) => visible.has(b.seriesId)).map((b) => (
        <ReferenceLine
          key={b.seriesId}
          yAxisId="right"
          y={b.y}
          stroke={b.color}
          strokeDasharray="5 4"
          strokeOpacity={0.85}
          ifOverflow="extendDomain"
          label={{ value: b.label, position: 'insideTopRight', fill: b.color, fontSize: 10, fontWeight: 600 }}
        />
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
            borderColor: on ? `color-mix(in srgb, ${s.color} 40%, transparent)` : COLORS.borderStrong,
            background: on ? `color-mix(in srgb, ${s.color} 8%, transparent)` : 'transparent',
          }}
        >
          <FontAwesomeIcon icon={faCircle} style={{ fontSize: 7 }} />
          {s.label}
        </button>
      );
    })}
  </div>
);

// ─── Metrics pivot table ────────────────────────────────────────────────────

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

const ENGAGEMENT_COL_IDS = new Set(['opens', 'open_pct', 'clicks', 'click_pct']);

const METRIC_COLS: ColDef[] = [
  {
    id: 'attempted', label: 'Sent', value: (r) => r.rates.denom,
    render: (r) => numCell(r.rates.denom),
    title: () => 'Delivered plus bounces',
  },
  { id: 'delivered', label: 'Delivered', value: (r) => r.counts.delivered, render: (r) => numCell(r.counts.delivered, COLORS.good) },
  {
    id: 'del_pct', label: 'Delivered %', value: (r) => r.rates.delivery,
    render: (r) => rateCell(r.rates.delivery, COLORS.good),
    tint: (r) => heatDel(r.rates.delivery), title: (r) => sentTitle(r.rates),
  },
  { id: 'hard', label: 'Hard bounce', value: (r) => r.counts.hard, render: (r) => numCell(r.counts.hard, HARD_RED) },
  {
    id: 'hard_pct', label: 'Hard %', value: (r) => r.rates.hard,
    render: (r) => rateCell(r.rates.hard, HARD_RED),
    tint: (r) => heatHard(r.rates.hard), title: (r) => sentTitle(r.rates),
  },
  { id: 'soft', label: 'Soft bounce', value: (r) => r.counts.soft, render: (r) => numCell(r.counts.soft, SOFT_AMBER) },
  {
    id: 'soft_pct', label: 'Soft %', value: (r) => r.rates.soft,
    render: (r) => rateCell(r.rates.soft, SOFT_AMBER), title: (r) => sentTitle(r.rates),
  },
  {
    id: 'delays', label: 'Retries', value: (r) => r.counts.delays,
    render: (r) => numCell(r.counts.delays, COLORS.warn), title: () => 'Each try again. One email can retry many times.',
  },
  { id: 'complaints', label: 'Complaints', value: (r) => r.counts.complaints, render: (r) => numCell(r.counts.complaints, COMPLAINT_ROSE) },
  {
    id: 'comp_pct', label: 'Complaint %', value: (r) => r.rates.complaint,
    render: (r) => rateCell(r.rates.complaint, COMPLAINT_ROSE),
    tint: (r) => heatComp(r.rates.complaint), title: (r) => sentTitle(r.rates),
  },
  { id: 'opens', label: 'Opens', value: (r) => r.counts.opens, render: (r) => numCell(r.counts.opens, OPEN_CYAN) },
  {
    id: 'open_pct', label: 'Open %', value: (r) => r.rates.open,
    render: (r) => rateCell(r.rates.open, OPEN_CYAN), title: (r) => deliveredTitle(r.counts),
  },
  { id: 'clicks', label: 'Clicks', value: (r) => r.counts.clicks, render: (r) => numCell(r.counts.clicks, CLICK_VIOLET) },
  {
    id: 'click_pct', label: 'Click %', value: (r) => r.rates.click,
    render: (r) => rateCell(r.rates.click, CLICK_VIOLET), title: (r) => deliveredTitle(r.counts),
  },
];

// cols must be the table's FULL column list (METRIC_COLS + any extraCols).
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

const notMatched = (k: string): string => k || 'Not matched';

const MetricsTable: React.FC<{
  dimLabel: string;
  rows: MetricRow[];
  totals: MetricRow;
  sort: SortState;
  onSort: (col: string) => void;
  labelOf?: (key: string) => string;
  hideTotal?: boolean;
  hideEngagement?: boolean;
  expandedKey?: string | null;
  onToggleExpand?: (key: string) => void;
  renderExpanded?: (key: string) => React.ReactNode;
  // Trailing columns keyed off MetricRow.key, reading state via the closure.
  extraCols?: ColDef[];
}> = ({ dimLabel, rows, totals, sort, onSort, labelOf = notMatched, hideTotal, hideEngagement, expandedKey, onToggleExpand, renderExpanded, extraCols }) => {
  const cols = useMemo(() => [
    ...METRIC_COLS.filter((c) => !hideEngagement || !ENGAGEMENT_COL_IDS.has(c.id)),
    ...(extraCols ?? []),
  ], [extraCols, hideEngagement]);
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
    <div style={styles.tableWrap} tabIndex={0}>
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
          {!hideTotal && (
            <tr style={{ ...styles.tr, background: COLORS.bgPanelAlt }}>
              <td style={{ ...styles.td, fontWeight: 700, color: COLORS.textPrimary }}>Total</td>
              {renderCells(totals, true)}
            </tr>
          )}
          {sorted.map((r) => {
            const isOpen = expandedKey === r.key;
            return (
              <React.Fragment key={r.key || '(empty)'}>
                <tr
                  style={{ ...styles.tr, cursor: expandable ? 'pointer' : 'default', background: isOpen ? 'color-mix(in srgb, var(--j-violet) 7%, transparent)' : undefined }}
                  onClick={expandable ? () => onToggleExpand(r.key) : undefined}
                >
                  <td style={{ ...styles.td, whiteSpace: 'nowrap', color: COLORS.textPrimary, fontWeight: 500 }}>
                    {expandable && (
                      <FontAwesomeIcon
                        icon={isOpen ? faChevronDown : faChevronRight}
                        style={{ fontSize: 9, marginRight: 8, color: COLORS.textMuted }}
                      />
                    )}
                    {labelOf(r.key)}
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

const flipSort = (col: string) => (s: SortState): SortState =>
  s.col === col ? { col, dir: s.dir === 'desc' ? 'asc' : 'desc' } : { col, dir: 'desc' };

// ═══════════════════════════════════════════════════════════════════════════
// TAB COMPONENTS
// ═══════════════════════════════════════════════════════════════════════════

const recoveredPct = (f: DeferralCounts | null | undefined): number | null => {
  if (!f) return null;
  const resolved = f.recovered + f.bounced;
  return resolved > 0 ? (f.recovered / resolved) * 100 : null;
};

const RECOVERED_TITLE = 'Of the delayed emails that finished, how many were delivered';

// The funnel has no brand or sending route, so it only shows without them.
const funnelApplies = (a: AppliedFilters): boolean => !a.brand.trim() && a.transport === 'combined';

// ─── Tab 1: Overview ────────────────────────────────────────────────────────

const OverviewTab: React.FC<{
  applied: AppliedFilters;
  rows: CubeRow[];
  cube: CubeState;
  funnel: DeferralFunnel | null;
  refreshTick: number;
  onRetry: () => void;
}> = ({ applied, rows, cube, funnel, refreshTick, onRetry }) => {
  const { addToast } = useToast();
  const [trendGrain, setTrendGrain] = useState<'day' | 'hour'>('day');
  const [hourlyRows, setHourlyRows] = useState<BreakdownRow[]>([]);
  const [visible, setVisible] = useState<Set<string>>(
    () => new Set(['delivered', 'hardPct', 'softPct', 'compPct'])
  );

  const totals = useMemo(() => makeMetricRow('TOTAL', typeTotals(rows)), [rows]);
  const daily = useMemo(() => dailySeries(rows), [rows]);
  // Tapping Day always shows the last 7 days of the range (operator 2026-07-02).
  const dailyTrend = useMemo(() => daily.slice(-7), [daily]);

  const ispMetrics = useMemo(() => {
    const out: MetricRow[] = [];
    groupRows(rows, (r) => r.isp).forEach((g, isp) => out.push(makeMetricRow(isp, typeTotals(g))));
    return out.sort((a, b) => b.rates.denom - a.rates.denom);
  }, [rows]);

  // Inclusive range length in days. Parse 'YYYY-MM-DD' at UTC noon so DST never
  // shifts the day count. Hourly trend is only allowed for ≤72h ranges.
  const rangeDays = useMemo(() => {
    const a = Date.parse(`${applied.from}T12:00:00Z`);
    const b = Date.parse(`${applied.to}T12:00:00Z`);
    if (Number.isNaN(a) || Number.isNaN(b)) return 1;
    return Math.round((b - a) / 86400000) + 1;
  }, [applied.from, applied.to]);

  // The hourly trend is its own query, only ever on the Hour grain.
  const seenTick = useRef(refreshTick);
  useEffect(() => {
    const bypass = seenTick.current !== refreshTick;
    seenTick.current = refreshTick;
    if (trendGrain !== 'hour' || rangeDays > 3) return;
    const ctl = new AbortController();
    const range = { from: applied.from, to: applied.to, groupBy: ['local_hour', 'event_type'] };
    const scoped = !funnelApplies(applied);
    const engFilters: Record<string, string> = { source: 'app', isp_group: applied.ispGroup };
    Promise.all([
      fetchBreakdown({ ...range, filters: { ...filterParams(applied), brand: brandApex(applied.brand) } }, { signal: ctl.signal, bypass }),
      // Opens and clicks carry no route, and only the exact sending domain.
      scoped ? null : fetchBreakdown({ ...range, filters: engFilters }, { signal: ctl.signal, bypass }),
    ]).then(([deliv, eng]) => {
      setHourlyRows(mergeDeliveryAndEngagement(deliv.rows, eng ? eng.rows : []));
    }).catch((e) => {
      if (isAbortError(e)) return;
      setHourlyRows([]);
      addToast({ type: 'error', title: 'Hourly trend failed', message: e instanceof Error ? e.message : String(e) });
    });
    return () => ctl.abort();
  }, [trendGrain, rangeDays, applied.from, applied.to, applied.transport, applied.ispGroup, applied.brand, refreshTick, addToast]); // eslint-disable-line react-hooks/exhaustive-deps

  const hourly = useMemo(() => hourlySeries(hourlyRows), [hourlyRows]);

  const toggleSeries = (id: string) => setVisible((v) => {
    const next = new Set(v);
    if (next.has(id)) next.delete(id); else next.add(id);
    return next;
  });

  const funnelByIsp = useMemo(
    () => new Map((funnel?.rows ?? []).map((r) => [r.isp_group, r])),
    [funnel]
  );
  const showFunnel = !!funnel && funnelApplies(applied);
  const ispFilter = applied.ispGroup.trim().toLowerCase();
  const funnelScope = !showFunnel ? null : ispFilter ? (funnelByIsp.get(ispFilter) ?? null) : funnel.total;

  const c = totals.counts;
  const r = totals.rates;
  const sent = sentTitle(r);
  const deliv = deliveredTitle(c);

  const ispCell: React.CSSProperties = { padding: '6px 14px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
  const ispHead: React.CSSProperties = { ...ispCell, color: 'var(--j-text-muted)', fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5 };
  const ispRowLabel: React.CSSProperties = { ...ispCell, textAlign: 'left', fontWeight: 600 };
  const ispFoot: React.CSSProperties = { ...ispCell, borderTop: '1px solid var(--j-border-strong)' };

  return (
    <div style={styles.panel}>
      <TruncationBanner truncated={cube.truncated} />
      {cube.error ? <ErrorRow error={cube.error} onRetry={onRetry} />
        : cube.rows.length === 0 && cube.loading ? <div style={{ minHeight: 120 }} />
        : c.total === 0 ? <EmptyRow label="Nothing to show for these filters." />
        : (
          <>
            <div style={styles.kpiGrid}>
              <KpiCard label="Sent" value={r.denom} color={COLORS.accent} title="Delivered plus bounces" />
              <KpiCard label="Delivered" value={c.delivered} color={COLORS.good} rate={r.delivery} rateTitle={sent} />
              <KpiCard label="Hard bounce" value={c.hard} color={HARD_RED} rate={r.hard} rateTitle={sent} />
              <KpiCard label="Soft bounce" value={c.soft} color={SOFT_AMBER} rate={r.soft} rateTitle={sent} />
              <KpiCard label="Retries" value={c.delays} color={COLORS.warn} title="Each try again. One email can retry many times." />
              {funnelScope && (
                <KpiCard label="Delayed, then delivered" value={funnelScope.recovered} color={COLORS.good}
                  rate={recoveredPct(funnelScope)} rateTitle={RECOVERED_TITLE}
                  extra={`${fmt(funnelScope.deferred)} delayed · ${fmt(funnelScope.pending)} still trying`} />
              )}
              <KpiCard label="Complaints" value={c.complaints} color={COMPLAINT_ROSE} rate={r.complaint} rateTitle={sent} />
              <KpiCard label="Opens" value={c.opens} color={OPEN_CYAN} rate={r.open} rateTitle={deliv} title="Every open, including machines" />
              <KpiCard label="Clicks" value={c.clicks} color={CLICK_VIOLET} rate={r.click} rateTitle={deliv} title="Every click, including machines" />
              <KpiCard label="Passed to SES" value={c.relayed} color={INFO_BLUE} title="Handed to SES to deliver. Not counted as delivered." />
            </div>

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
                          background: active ? `color-mix(in srgb, ${COLORS.accent} 12%, transparent)` : 'transparent',
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
                dailyTrend.length === 0 ? <EmptyRow label="No days to show." /> : (
                  <TrendChart data={dailyTrend} visible={visible} height={300} />
                )
              ) : rangeDays > 3 ? (
                <EmptyRow label="Hours show for 3 days or less." />
              ) : hourly.length === 0 ? (
                <EmptyRow label="No hours to show." />
              ) : (
                <TrendChart data={hourly} visible={visible} height={300} />
              )}
            </div>

            <div style={{ marginTop: 20 }}>
              <div style={styles.subPanelTitle}>By email provider</div>
              <div style={styles.tableWrap} tabIndex={0}>
                <table style={{ borderCollapse: 'collapse', width: '100%', fontSize: 13 }}>
                  <thead>
                    <tr>
                      <th style={{ ...ispHead, textAlign: 'left' }}>Provider</th>
                      <th style={ispHead}>Sent</th>
                      <th style={ispHead}>Delivered</th>
                      <th style={ispHead}>Delivered %</th>
                      <th style={ispHead}>Hard %</th>
                      <th style={ispHead}>Soft %</th>
                      <th style={ispHead}>Retries</th>
                      {showFunnel && <th style={ispHead}>Delayed</th>}
                      {showFunnel && <th style={ispHead} title={RECOVERED_TITLE}>Recovered</th>}
                    </tr>
                  </thead>
                  <tbody>
                    {ispMetrics.map((m) => {
                      const f = funnelByIsp.get(m.key);
                      return (
                        <tr key={m.key}>
                          <td style={ispRowLabel}>{ispLabel(m.key)}</td>
                          <td style={ispCell}>{fmt(m.rates.denom)}</td>
                          <td style={ispCell}>{fmt(m.counts.delivered)}</td>
                          <td style={{ ...ispCell, background: heatDel(m.rates.delivery) }} title={sentTitle(m.rates)}>{fmtPct(m.rates.delivery)}</td>
                          <td style={{ ...ispCell, color: HARD_RED, background: heatHard(m.rates.hard) }} title={sentTitle(m.rates)}>{fmtPct(m.rates.hard)}</td>
                          <td style={{ ...ispCell, color: SOFT_AMBER }} title={sentTitle(m.rates)}>{fmtPct(m.rates.soft)}</td>
                          <td style={ispCell}>{fmt(m.counts.delays)}</td>
                          {showFunnel && <td style={ispCell}>{fmt(f?.deferred ?? 0)}</td>}
                          {showFunnel && <td style={ispCell} title={RECOVERED_TITLE}>{fmtPct(recoveredPct(f))}</td>}
                        </tr>
                      );
                    })}
                    <tr>
                      <td style={{ ...ispRowLabel, borderTop: '1px solid var(--j-border-strong)' }}>Total</td>
                      <td style={ispFoot}>{fmt(r.denom)}</td>
                      <td style={ispFoot}>{fmt(c.delivered)}</td>
                      <td style={{ ...ispFoot, background: heatDel(r.delivery) }} title={sent}>{fmtPct(r.delivery)}</td>
                      <td style={{ ...ispFoot, color: HARD_RED, background: heatHard(r.hard) }} title={sent}>{fmtPct(r.hard)}</td>
                      <td style={{ ...ispFoot, color: SOFT_AMBER }} title={sent}>{fmtPct(r.soft)}</td>
                      <td style={ispFoot}>{fmt(c.delays)}</td>
                      {showFunnel && <td style={ispFoot}>{fmt(funnelScope?.deferred ?? 0)}</td>}
                      {showFunnel && <td style={ispFoot} title={RECOVERED_TITLE}>{fmtPct(recoveredPct(funnelScope))}</td>}
                    </tr>
                  </tbody>
                </table>
              </div>
            </div>
          </>
        )}
    </div>
  );
};

// ─── Tab 2: Dimensions ──────────────────────────────────────────────────────

const ROW_DIMS: Array<{ id: string; label: string }> = [
  { id: 'isp', label: 'Email Provider' },
  { id: 'brand', label: 'Brand' },
  { id: 'email_domain', label: 'Email Domain' },
  { id: 'lane', label: 'Sending Route' },
  { id: 'vmta', label: 'Sending Server' },
  { id: 'pool', label: 'Pool' },
];

// These need a column the main cube leaves out, so each is one cached query.
const DIM_CUBES = new Set(['email_domain', 'vmta', 'pool']);
// Opens and clicks are not tied to a server or pool.
const NO_ENGAGEMENT_DIMS = new Set(['vmta', 'pool']);

// One level of a drill-down: how rows group, and how a group reads.
interface RowLevel {
  header: string;
  keyOf: (r: CubeRow) => string;
  labelOf?: (key: string) => string;
}

const LANE_LEVEL: RowLevel = { header: 'Sending route', keyOf: (r) => r.lane, labelOf: laneLabel };
const DIM_LEVELS: Record<string, RowLevel[]> = {
  isp: [{ header: 'Email provider', keyOf: (r) => r.isp, labelOf: ispLabel }],
  brand: [
    { header: 'Brand', keyOf: (r) => brandApex(r.brand) },
    { header: 'Sub domain', keyOf: (r) => r.brand },
    LANE_LEVEL,
  ],
  email_domain: [
    { header: 'Email group', keyOf: (r) => emailGroup(r.value) },
    { header: 'Email domain', keyOf: (r) => r.value },
  ],
  lane: [LANE_LEVEL, { header: 'Brand', keyOf: (r) => brandApex(r.brand) }],
  vmta: [{ header: 'Sending server', keyOf: (r) => r.value, labelOf: (k) => k || 'No server' }],
  pool: [{ header: 'Pool', keyOf: (r) => r.value, labelOf: (k) => k || 'No pool' }],
};

const ROW_TREND_SERIES = new Set(['delivered', 'hardPct', 'softPct', 'compPct']);

const RowDetail: React.FC<{ levels: RowLevel[]; rows: CubeRow[]; hideEngagement?: boolean }> = ({ levels, rows, hideEngagement }) => {
  const daily = useMemo(() => dailySeries(rows), [rows]);
  return (
    <div>
      {daily.length > 0 && <TrendChart data={daily} visible={ROW_TREND_SERIES} height={170} />}
      {levels.length > 0 && (
        <div style={{ marginTop: daily.length > 0 ? 12 : 0 }}>
          <DimTable levels={levels} rows={rows} hideEngagement={hideEngagement} nested />
        </div>
      )}
    </div>
  );
};

const DimTable: React.FC<{
  levels: RowLevel[];
  rows: CubeRow[];
  hideEngagement?: boolean;
  nested?: boolean;
  extraCols?: ColDef[];
}> = ({ levels, rows, hideEngagement, nested, extraCols }) => {
  const [level, ...next] = levels;
  const [sort, setSort] = useState<SortState>({ col: 'attempted', dir: 'desc' });
  const [expandedKey, setExpandedKey] = useState<string | null>(null);
  const groups = useMemo(() => groupRows(rows, level.keyOf), [rows, level]);
  const metricRows = useMemo(() => {
    const out: MetricRow[] = [];
    groups.forEach((g, k) => out.push(makeMetricRow(k, typeTotals(g))));
    return out;
  }, [groups]);
  const totals = useMemo(() => makeMetricRow('TOTAL', typeTotals(rows)), [rows]);
  const canExpand = next.length > 0 || rows.some((r) => r.day !== '');
  return (
    <MetricsTable
      dimLabel={level.header}
      rows={metricRows}
      totals={totals}
      sort={sort}
      onSort={(col) => setSort(flipSort(col))}
      labelOf={level.labelOf}
      hideTotal={nested}
      hideEngagement={hideEngagement}
      expandedKey={expandedKey}
      onToggleExpand={canExpand ? (k) => setExpandedKey((cur) => (cur === k ? null : k)) : undefined}
      renderExpanded={(k) => <RowDetail levels={next} rows={groups.get(k) ?? []} hideEngagement={hideEngagement} />}
      extraCols={extraCols}
    />
  );
};

const DimensionsTab: React.FC<{
  applied: AppliedFilters;
  rows: CubeRow[];
  cube: CubeState;
  funnel: DeferralFunnel | null;
  refreshTick: number;
  onRetry: () => void;
}> = ({ applied, rows, cube, funnel, refreshTick, onRetry }) => {
  const [dim, setDim] = useState('isp');
  const needsDimCube = DIM_CUBES.has(dim);
  const dimCube = useCube(needsDimCube, applied.from, applied.to, refreshTick, needsDimCube ? dim : '', applied.brand.trim().toLowerCase());
  const dimRows = useMemo(
    () => sliceCube(dimCube.rows, { transport: applied.transport, ispGroup: applied.ispGroup, brand: '' }),
    [dimCube.rows, applied.transport, applied.ispGroup]
  );
  const source = needsDimCube ? dimCube : cube;
  const pulsing = useSlicePulse(dim);

  const funnelCols: ColDef[] | undefined = useMemo(() => {
    if (dim !== 'isp' || !funnel || !funnelApplies(applied)) return undefined;
    const byIsp = new Map(funnel.rows.map((r) => [r.isp_group, r]));
    const ispFilter = applied.ispGroup.trim().toLowerCase();
    const lookup = (r: MetricRow): DeferralCounts | null => (
      r.key === 'TOTAL' && !ispFilter ? funnel.total : byIsp.get(r.key === 'TOTAL' ? ispFilter : r.key) ?? null
    );
    return [
      {
        id: 'deferred', label: 'Delayed',
        value: (r) => lookup(r)?.deferred ?? null,
        render: (r) => numCell(lookup(r)?.deferred ?? 0, SOFT_AMBER),
      },
      {
        id: 'recovered_pct', label: 'Recovered %',
        value: (r) => recoveredPct(lookup(r)),
        render: (r) => rateCell(recoveredPct(lookup(r)), COLORS.good),
        title: () => RECOVERED_TITLE,
      },
    ];
  }, [dim, funnel, applied]);

  const shown = needsDimCube ? dimRows : rows;
  return (
    <div style={{ ...styles.panel, position: 'relative' }}>
      <BusyOverlay show={(needsDimCube && dimCube.loading) || pulsing} label={dimCube.loading ? 'Loading…' : 'Updating…'} />
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
                borderColor: active ? `color-mix(in srgb, ${COLORS.accent} 40%, transparent)` : COLORS.borderStrong,
                background: active ? `color-mix(in srgb, ${COLORS.accent} 8%, transparent)` : 'transparent',
              }}
              onClick={() => setDim(d.id)}
            >
              {d.label}
            </button>
          );
        })}
      </div>

      <TruncationBanner truncated={source.truncated} />
      {source.error ? <ErrorRow error={source.error} onRetry={onRetry} />
        : shown.length === 0 && source.loading ? <div style={{ minHeight: 120 }} />
        : shown.length === 0 ? <EmptyRow label="Nothing to show for these filters." />
        : (
          <DimTable
            key={dim}
            levels={DIM_LEVELS[dim]}
            rows={shown}
            hideEngagement={NO_ENGAGEMENT_DIMS.has(dim)}
            extraCols={funnelCols}
          />
        )}
    </div>
  );
};

// ─── Tab 3: Campaign Lookup ─────────────────────────────────────────────────

interface LookupResult {
  id: string;
  typeTotals: MetricRow | null;
  ispRows: BreakdownRow[];
  truncated: boolean;
  // false: opens and clicks read 0 because the query failed, not because there
  // were none, so the comparison leaves them out.
  engOk: boolean;
  cc: CCDetail | null;                 // null when Campaign Center has no summary
  events: LakeEvent[];
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

interface TreeCampaign {
  id: string;
  name: string;
  status: string;
  scheduled_at: string;
  sent: number;
}

interface TreeList { loading: boolean; error: string; rows: TreeCampaign[]; truncated: boolean }

// Results for a campaign read 14 days from its send day.
const LOOKUP_WINDOW_DAYS = 14;

function lookupEnd(day: string): string {
  const d = new Date(`${day}T12:00:00Z`);
  d.setUTCDate(d.getUTCDate() + LOOKUP_WINDOW_DAYS - 1);
  const end = d.toISOString().slice(0, 10);
  const today = denverToday();
  return end < today ? end : today;
}

const fmtTreeDay = (d: string): string =>
  new Date(`${d}T12:00:00Z`).toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' });

const TreeRowBtn: React.FC<{
  depth: number;
  open: boolean;
  label: string;
  campaigns: number;
  sent: number;
  onClick: () => void;
}> = ({ depth, open, label, campaigns, sent, onClick }) => (
  <button type="button" style={{ ...styles.treeRow, paddingLeft: 12 + depth * 20 }} aria-expanded={open} onClick={onClick}>
    <FontAwesomeIcon icon={open ? faChevronDown : faChevronRight} style={{ fontSize: 9, color: COLORS.textMuted, width: 12 }} />
    <span style={{ fontWeight: depth === 0 ? 600 : 500 }}>{label}</span>
    <span style={styles.treeStat}>{fmt(campaigns)} {campaigns === 1 ? 'campaign' : 'campaigns'} · {fmt(sent)} sent</span>
  </button>
);

const CampaignTab: React.FC<{ applied: AppliedFilters }> = ({ applied }) => {
  const { addToast } = useToast();
  const [treeRows, setTreeRows] = useState<CampaignTreeRow[]>([]);
  const [treeLoading, setTreeLoading] = useState(false);
  const [treeError, setTreeError] = useState('');
  const [treeTick, setTreeTick] = useState(0);
  const [openKeys, setOpenKeys] = useState<Set<string>>(() => new Set());
  const [lists, setLists] = useState<Record<string, TreeList>>({});
  const [selected, setSelected] = useState('');
  const [result, setResult] = useState<LookupResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [ispSort, setIspSort] = useState<SortState>({ col: 'attempted', dir: 'desc' });
  const abortRef = useRef<AbortController | null>(null);
  const resultRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const ctl = new AbortController();
    setTreeLoading(true);
    setTreeError('');
    (async () => {
      try {
        const qs = new URLSearchParams({ from: applied.from, to: applied.to });
        const res = await apiFetch(`/api/mailing/analytics/campaign-tree?${qs.toString()}`, { signal: ctl.signal });
        if (!res.ok) await throwHttpError(res);
        const body: { rows?: CampaignTreeRow[] } = await res.json();
        setTreeRows(body.rows ?? []);
        setTreeLoading(false);
      } catch (e) {
        if (isAbortError(e)) return;
        setTreeRows([]);
        setTreeError(e instanceof Error ? e.message : String(e));
        setTreeLoading(false);
      }
    })();
    return () => ctl.abort();
  }, [applied.from, applied.to, treeTick]);

  useEffect(() => () => abortRef.current?.abort(), []);

  const tree = useMemo(() => buildCampaignTree(treeRows), [treeRows]);

  const toggle = (key: string) => setOpenKeys((cur) => {
    const next = new Set(cur);
    if (next.has(key)) next.delete(key); else next.add(key);
    return next;
  });

  const openDay = (domain: string, day: string) => {
    const key = `${domain}|${day}`;
    toggle(`d:${key}`);
    if (lists[key] && !lists[key].error) return;
    setLists((c) => ({ ...c, [key]: { loading: true, error: '', rows: [], truncated: false } }));
    void (async () => {
      try {
        const qs = new URLSearchParams({ sending_domain: domain, day });
        const res = await apiFetch(`/api/mailing/analytics/campaign-tree/campaigns?${qs.toString()}`);
        if (!res.ok) await throwHttpError(res);
        const body: { campaigns?: TreeCampaign[]; truncated?: boolean } = await res.json();
        setLists((c) => ({ ...c, [key]: { loading: false, error: '', rows: body.campaigns ?? [], truncated: !!body.truncated } }));
      } catch (e) {
        setLists((c) => ({ ...c, [key]: { loading: false, error: e instanceof Error ? e.message : String(e), rows: [], truncated: false } }));
      }
    })();
  };

  const runLookup = useCallback(async (id: string, day: string) => {
    setSelected(id);
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setLoading(true);
    requestAnimationFrame(() => resultRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' }));
    const from = day;
    const to = lookupEnd(day);
    const opts = { signal: ctl.signal, bypass: true };
    // Delivery from the three transports, opens and clicks from source='app'
    // (the same split as the cube), so nothing counts twice.
    const delivFilters = { campaign_id: id, source_in: 'pmta,ses,kumo' };
    const engFilters = { campaign_id: id, source: 'app' };
    const [a, aEng, b, bEng, c, d] = await Promise.allSettled([
      fetchBreakdown({ from, to, groupBy: ['event_type'], filters: delivFilters }, opts),
      fetchBreakdown({ from, to, groupBy: ['event_type'], filters: engFilters }, opts),
      fetchBreakdown({ from, to, groupBy: ['isp', 'event_type'], filters: delivFilters }, opts),
      fetchBreakdown({ from, to, groupBy: ['isp', 'event_type'], filters: engFilters }, opts),
      fetchCampaignSummary(id, ctl.signal),
      fetchLakeEvents(id, ctl.signal),
    ]);
    if (ctl.signal.aborted) return;

    const fail = (title: string, reason: unknown) => {
      if (!isAbortError(reason)) addToast({ type: 'error', title, message: reason instanceof Error ? reason.message : String(reason) });
    };
    const res: LookupResult = { id, typeTotals: null, ispRows: [], truncated: false, engOk: false, cc: null, events: [] };
    const aEngRows = aEng.status === 'fulfilled' ? aEng.value.rows : [];
    const bEngRows = bEng.status === 'fulfilled' ? bEng.value.rows : [];
    res.engOk = aEng.status === 'fulfilled';
    if (aEng.status === 'rejected') fail('Could not load opens and clicks', aEng.reason);
    else if (bEng.status === 'rejected') fail('Could not load opens and clicks', bEng.reason);
    if (a.status === 'fulfilled') {
      res.typeTotals = totalsFromBreakdown(mergeDeliveryAndEngagement(a.value.rows, aEngRows));
      res.truncated = !!a.value.truncated;
    } else fail('Could not load campaign results', a.reason);
    if (b.status === 'fulfilled') {
      res.ispRows = mergeDeliveryAndEngagement(b.value.rows, bEngRows);
    } else fail('Could not load results by provider', b.reason);
    // The handler can answer 200 with no campaign key: no summary, not a result.
    if (c.status === 'fulfilled' && c.value.campaign && typeof c.value.campaign === 'object') res.cc = c.value;
    if (d.status === 'fulfilled') res.events = d.value;
    else fail('Could not load latest events', d.reason);
    setResult(res);
    setLoading(false);
  }, [addToast]);

  const ispPivot = useMemo(() => pivotByDim(result?.ispRows || [], 'isp'), [result]);

  const cc = result?.cc?.campaign || null;
  const lakeC = result?.typeTotals?.counts || null;
  const lakeR = result?.typeTotals?.rates || null;

  // No Complaints row: the campaign-summary detail has no complaints field.
  const reconRows: ReconRow[] = (lakeC && cc) ? [
    { label: 'Delivered', lake: lakeC.delivered, cc: cc.delivered },
    // Reporting counts provider blocks as hard bounces; Campaign Center splits them.
    { label: 'Hard bounce', lake: lakeC.hard, cc: cc.hard_bounce + (cc.reputation_block ?? 0), color: HARD_RED, note: 'Campaign Center number includes provider blocks' },
    { label: 'Soft bounce', lake: lakeC.soft, cc: cc.soft_bounce, color: SOFT_AMBER },
    ...(result?.engOk ? [
      { label: 'Opens *', lake: lakeC.opens, cc: cc.unique_opens, color: OPEN_CYAN },
      { label: 'Clicks *', lake: lakeC.clicks, cc: cc.unique_clicks, color: CLICK_VIOLET },
    ] : []),
  ] : [];

  return (
    <div>
      <div style={{ ...styles.panel, position: 'relative' }}>
        <h2 style={{ ...styles.panelTitle, marginBottom: 12 }}>
          <FontAwesomeIcon icon={faBullseye} style={{ marginRight: 8, color: COLORS.accentPink }} />
          Pick a campaign
        </h2>
        {treeError ? <ErrorRow error={treeError} onRetry={() => setTreeTick((n) => n + 1)} />
          : treeLoading && tree.length === 0 ? <LoadingRow />
          : tree.length === 0 ? <EmptyRow label="No sent campaigns in these dates." />
          : (
            <div style={styles.tree}>
              {tree.map((a) => {
                const aOpen = openKeys.has(`a:${a.apex}`);
                return (
                  <div key={a.apex}>
                    <TreeRowBtn depth={0} open={aOpen} label={a.apex || 'No domain'} campaigns={a.campaigns} sent={a.sent} onClick={() => toggle(`a:${a.apex}`)} />
                    {aOpen && a.subs.map((s) => {
                      const sOpen = openKeys.has(`s:${s.domain}`);
                      return (
                        <div key={s.domain}>
                          <TreeRowBtn depth={1} open={sOpen} label={s.domain || 'No domain'} campaigns={s.campaigns} sent={s.sent} onClick={() => toggle(`s:${s.domain}`)} />
                          {sOpen && s.days.map((d) => {
                            const key = `${s.domain}|${d.day}`;
                            const dOpen = openKeys.has(`d:${key}`);
                            const list = lists[key];
                            return (
                              <div key={d.day}>
                                <TreeRowBtn depth={2} open={dOpen} label={fmtTreeDay(d.day)} campaigns={d.campaigns} sent={d.sent} onClick={() => openDay(s.domain, d.day)} />
                                {dOpen && list?.loading && <LoadingRow />}
                                {dOpen && list?.error && <div style={{ ...styles.treeNote, color: COLORS.danger }}>Could not load: {list.error}</div>}
                                {dOpen && list?.rows.map((c) => (
                                  <button
                                    key={c.id}
                                    type="button"
                                    style={{
                                      ...styles.treeRow, paddingLeft: 92,
                                      background: selected === c.id ? 'color-mix(in srgb, var(--j-violet) 10%, transparent)' : 'transparent',
                                    }}
                                    aria-pressed={selected === c.id}
                                    onClick={() => runLookup(c.id, d.day)}
                                  >
                                    <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={c.name}>{c.name || 'No name'}</span>
                                    <span style={styles.treeStat}>{c.scheduled_at} · {fmt(c.sent)} sent</span>
                                  </button>
                                ))}
                                {dOpen && list?.truncated && <div style={styles.treeNote}>Showing the first 500.</div>}
                              </div>
                            );
                          })}
                        </div>
                      );
                    })}
                  </div>
                );
              })}
            </div>
          )}
      </div>

      <div ref={resultRef} style={{ scrollMarginTop: 16 }}>
      {loading && <div style={styles.panel}><LoadingRow /></div>}

      {!loading && result && (
        <>
          <div style={styles.panel}>
            {cc ? (
              <div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
                  <span style={{ fontSize: 17, fontWeight: 700, color: COLORS.textPrimary }}>{cc.name || 'No name'}</span>
                  <span style={{
                    ...styles.typeChip,
                    color: STATUS_CHIP_COLORS[cc.status] || COLORS.accent,
                    borderColor: `color-mix(in srgb, ${(STATUS_CHIP_COLORS[cc.status] || COLORS.accent)} 33%, transparent)`,
                  }}>
                    {cc.status}
                  </span>
                </div>
                <div style={styles.metaGrid}>
                  <MetaItem label="Subject" value={cc.subject} />
                  <MetaItem label="From" value={cc.from_name && cc.from_email ? `${cc.from_name} <${cc.from_email}>` : cc.from_email || cc.from_name} />
                  <MetaItem label="Scheduled" value={cc.scheduled_at ? new Date(cc.scheduled_at).toLocaleString('en-US', { hour12: false }) : undefined} />
                  <MetaItem label="Sending domain" value={cc.sending_domain} />
                  <MetaItem label="IP pool" value={cc.ip_pool} />
                  <MetaItem label="Campaign ID" value={result.id} mono />
                </div>
              </div>
            ) : (
              <div style={styles.lakeOnlyNotice}>
                <FontAwesomeIcon icon={faInfoCircle} style={{ marginRight: 8, color: COLORS.warn }} />
                Campaign Center has no summary for this campaign.
              </div>
            )}
          </div>

          <div style={styles.panel}>
            <h2 style={{ ...styles.panelTitle, marginBottom: 12 }}>Results</h2>
            <TruncationBanner truncated={result.truncated} />
            {lakeC && lakeR ? (
              lakeC.total === 0 ? <EmptyRow label="No results for this campaign yet." /> : (
                <div style={styles.kpiGrid}>
                  <KpiCard label="Sent" value={lakeR.denom} color={COLORS.accent} title="Delivered plus bounces" />
                  <KpiCard label="Delivered" value={lakeC.delivered} color={COLORS.good} rate={lakeR.delivery} rateTitle={sentTitle(lakeR)} />
                  <KpiCard label="Hard bounce" value={lakeC.hard} color={HARD_RED} rate={lakeR.hard} rateTitle={sentTitle(lakeR)} />
                  <KpiCard label="Soft bounce" value={lakeC.soft} color={SOFT_AMBER} rate={lakeR.soft} rateTitle={sentTitle(lakeR)} />
                  <KpiCard label="Retries" value={lakeC.delays} color={COLORS.warn} title="Each try again. One email can retry many times." />
                  <KpiCard label="Complaints" value={lakeC.complaints} color={COMPLAINT_ROSE} rate={lakeR.complaint} rateTitle={sentTitle(lakeR)} />
                  <KpiCard label="Opens" value={lakeC.opens} color={OPEN_CYAN} rate={lakeR.open} rateTitle={deliveredTitle(lakeC)} title="Every open, including machines" />
                  <KpiCard label="Clicks" value={lakeC.clicks} color={CLICK_VIOLET} rate={lakeR.click} rateTitle={deliveredTitle(lakeC)} title="Every click, including machines" />
                  <KpiCard label="Passed to SES" value={lakeC.relayed} color={INFO_BLUE} title="Handed to SES to deliver. Not counted as delivered." />
                </div>
              )
            ) : <EmptyRow label="Could not load results." />}
          </div>

          <div style={styles.panel}>
            <h2 style={{ ...styles.panelTitle, marginBottom: 12 }}>By email provider</h2>
            {ispPivot.rows.length === 0 ? <EmptyRow label="No provider results yet." /> : (
              <MetricsTable
                dimLabel="Provider"
                rows={ispPivot.rows}
                totals={ispPivot.totals}
                sort={ispSort}
                onSort={(col) => setIspSort(flipSort(col))}
                labelOf={(k) => (k ? ispLabel(k) : 'Not matched')}
              />
            )}
          </div>

          {cc && lakeC && lakeC.total > 0 && (
            <div style={styles.panel}>
              <h2 style={styles.panelTitle}>Compare with Campaign Center</h2>
              <p style={styles.panelSubtitle}>Green is within 2%. Amber is within 10%. Red is more than 10% apart.</p>
              <div style={{ ...styles.tableWrap, marginTop: 12 }} tabIndex={0}>
                <table style={styles.table}>
                  <thead>
                    <tr>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Number</th>
                      <th style={{ ...styles.th, textAlign: 'right' }}>Reporting</th>
                      <th style={{ ...styles.th, textAlign: 'right' }}>Campaign Center</th>
                      <th style={{ ...styles.th, textAlign: 'right' }}>Difference</th>
                      <th style={{ ...styles.th, textAlign: 'right' }}>Difference %</th>
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
                  * Reporting counts every open and click. Campaign Center counts each person once.
                </div>
              </div>
            </div>
          )}

          <div style={styles.panel}>
            <h2 style={{ ...styles.panelTitle, marginBottom: 12 }}>Latest events</h2>
            {result.events.length === 0 ? <EmptyRow label="No events yet." /> : (
              <div style={styles.tableWrap} tabIndex={0}>
                <table style={styles.table}>
                  <thead>
                    <tr>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Time</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Event</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Provider</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Email domain</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Sending server</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Bounce type</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Code</th>
                    </tr>
                  </thead>
                  <tbody>
                    {result.events.map((e, i) => (
                      <tr key={e.event_uid || String(i)} style={styles.tr}>
                        <td style={{ ...styles.td, whiteSpace: 'nowrap' }}>{fmtEventAt(e)}</td>
                        <td style={styles.td}>
                          <span style={{ ...styles.typeChip, color: eventTypeColor(e.event_type), borderColor: `color-mix(in srgb, ${eventTypeColor(e.event_type)} 33%, transparent)` }}>
                            {eventLabel(e.event_type)}
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
      </div>
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

// ═══════════════════════════════════════════════════════════════════════════
// MAIN
// ═══════════════════════════════════════════════════════════════════════════

const TABS: Array<{ id: TabId; label: string; icon: typeof faChartLine }> = [
  { id: 'overview', label: 'Overview', icon: faChartLine },
  { id: 'dimensions', label: 'Dimensions', icon: faLayerGroup },
  { id: 'growth', label: 'Growth', icon: faSeedling },
  { id: 'campaign', label: 'Campaign Lookup', icon: faBullseye },
  { id: 'alignment', label: 'Conversions', icon: faCrosshairs },
];

// What the one filter bar shows on each tab. Conversions has its own control.
const TAB_FILTERS: Partial<Record<TabId, { brand?: boolean; isp?: boolean; transport?: boolean }>> = {
  overview: { isp: true, brand: true, transport: true },
  dimensions: { isp: true, brand: true, transport: true },
  growth: { isp: true, brand: true },
  campaign: {},
};

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
  const [refreshTick, setRefreshTick] = useState(0);

  const readEnabled = !!status?.enabled_read;
  const refresh = useCallback(() => setRefreshTick((n) => n + 1), []);

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
      addToast({ type: 'error', title: 'Could not load reporting', message: msg });
    } finally {
      setStatusLoading(false);
    }
  }, [addToast]);

  // On mount: status only. Every other read waits for enabled_read.
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

  const onRun = useCallback((next: DraftFilters) => {
    setApplied((prev) => ({ ...next, nonce: prev.nonce + 1 }));
  }, []);

  const cubeWanted = readEnabled && (visited.has('overview') || visited.has('dimensions'));
  const cube = useCube(cubeWanted, applied.from, applied.to, refreshTick);
  const funnel = useFunnel(cubeWanted, applied.from, applied.to, refreshTick);
  const sliced = useMemo(
    () => sliceCube(cube.rows, { transport: applied.transport, ispGroup: applied.ispGroup, brand: applied.brand }),
    [cube.rows, applied.transport, applied.ispGroup, applied.brand]
  );
  const brandOptions = useMemo(() => apexOptions(cube.rows), [cube.rows]);
  const pulsing = useSlicePulse(`${applied.transport}|${applied.ispGroup}|${applied.brand}`);
  const busyLabel = cube.loading ? 'Loading. This can take up to a minute.' : 'Updating…';

  if (statusLoading && !status) {
    return (
      <div style={styles.loadingShell}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading…
      </div>
    );
  }

  const barFields = TAB_FILTERS[activeTab];
  const showBar = !!barFields && (readEnabled || activeTab === 'growth');
  const showRefresh = activeTab === 'overview' || activeTab === 'dimensions';

  return (
    <div style={styles.page}>
      {statusError && <ErrorRow error={statusError} onRetry={fetchStatus} />}

      {status && !readEnabled && (
        <div style={styles.offLine}>Reporting is off. Growth and Conversions still work.</div>
      )}

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
                  background: active ? 'color-mix(in srgb, var(--j-violet) 7%, transparent)' : 'transparent',
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

      {showBar && (
        <FilterBar
          draft={draft}
          setDraft={setDraft}
          applied={applied}
          onRun={onRun}
          show={barFields}
          applyTransportNow
          brandOptions={brandOptions}
          extraFields={showRefresh ? (
            <button style={styles.refreshBtn} onClick={refresh} disabled={cube.loading}>
              <FontAwesomeIcon icon={cube.loading ? faSpinner : faSyncAlt} spin={cube.loading} /> Refresh
            </button>
          ) : undefined}
        />
      )}

      {/* Visited tabs stay mounted; only the active one is visible. */}
      {status && readEnabled && (
        <>
          {visited.has('overview') && (
            <div style={{ display: activeTab === 'overview' ? 'block' : 'none', position: 'relative', minHeight: 120 }}>
              <BusyOverlay show={cube.loading || pulsing} label={busyLabel} />
              <OverviewTab applied={applied} rows={sliced} cube={cube} funnel={funnel} refreshTick={refreshTick} onRetry={refresh} />
            </div>
          )}
          {visited.has('dimensions') && (
            <div style={{ display: activeTab === 'dimensions' ? 'block' : 'none', position: 'relative', minHeight: 120 }}>
              <BusyOverlay show={cube.loading || pulsing} label={busyLabel} />
              <DimensionsTab applied={applied} rows={sliced} cube={cube} funnel={funnel} refreshTick={refreshTick} onRetry={refresh} />
            </div>
          )}
          {visited.has('campaign') && (
            <div style={{ display: activeTab === 'campaign' ? 'block' : 'none' }}>
              <CampaignTab applied={applied} />
            </div>
          )}
        </>
      )}

      {status && visited.has('growth') && (
        <div style={{ display: activeTab === 'growth' ? 'block' : 'none' }}>
          <GrowthTab applied={applied} />
        </div>
      )}

      {status && visited.has('alignment') && (
        <div style={{ display: activeTab === 'alignment' ? 'block' : 'none' }}>
          <OfferAlignmentTab />
        </div>
      )}
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
    border: `1px solid color-mix(in srgb, ${COLORS.danger} 20%, transparent)`,
  },
  inlineRetry: {
    marginLeft: 'auto', background: 'transparent', color: COLORS.danger,
    border: `1px solid color-mix(in srgb, ${COLORS.danger} 33%, transparent)`, padding: '4px 12px',
    borderRadius: 6, fontSize: 12, cursor: 'pointer',
  },
  refreshBtn: {
    background: COLORS.bgPanel, color: COLORS.textPrimary,
    border: `1px solid ${COLORS.borderStrong}`, padding: '8px 16px',
    borderRadius: 6, fontSize: 13, cursor: 'pointer',
    display: 'flex', alignItems: 'center', gap: 8, height: 34, whiteSpace: 'nowrap',
  },
  offLine: { fontSize: 13, color: COLORS.textSecondary, marginBottom: 16 },
  busyOverlay: {
    position: 'absolute', inset: 0, zIndex: 5, display: 'flex', alignItems: 'flex-start',
    justifyContent: 'center', paddingTop: 24, borderRadius: 10,
    background: 'color-mix(in srgb, var(--j-bg) 55%, transparent)',
  },
  busyPill: {
    display: 'inline-flex', alignItems: 'center', gap: 8, padding: '8px 14px',
    borderRadius: 999, fontSize: 13, fontWeight: 600, color: COLORS.textPrimary,
    background: COLORS.bgPanel, border: `1px solid ${COLORS.borderStrong}`,
    position: 'sticky', top: 16,
  },

  presetBtn: {
    border: '1px solid', borderRadius: 999, padding: '6px 12px',
    fontSize: 12, fontWeight: 600, cursor: 'pointer', background: 'transparent',
    whiteSpace: 'nowrap',
  },
  chipRowLabel: {
    fontSize: 11, color: COLORS.textMuted, textTransform: 'uppercase', letterSpacing: 0.5,
  },

  // ── Campaign tree ──
  tree: { border: `1px solid ${COLORS.border}`, borderRadius: 8, overflow: 'hidden' },
  treeRow: {
    display: 'flex', alignItems: 'center', gap: 8, width: '100%', padding: '9px 14px',
    background: 'transparent', border: 'none', borderBottom: `1px solid ${COLORS.border}`,
    color: COLORS.textPrimary, fontSize: 13, textAlign: 'left', cursor: 'pointer',
  },
  treeStat: {
    marginLeft: 'auto', paddingLeft: 12, flexShrink: 0, fontSize: 12,
    color: COLORS.textSecondary, fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap',
  },
  treeNote: { padding: '8px 14px 8px 92px', fontSize: 12, color: COLORS.textSecondary },

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
  panelTitle: { margin: 0, fontSize: 16, fontWeight: 600, color: COLORS.textPrimary },
  panelSubtitle: { margin: '4px 0 0', fontSize: 12, color: COLORS.textSecondary, maxWidth: 820, lineHeight: 1.5 },
  subPanelTitle: {
    fontSize: 12, fontWeight: 700, color: COLORS.textSecondary,
    textTransform: 'uppercase', letterSpacing: 0.6, marginBottom: 10,
  },
  truncBanner: {
    padding: '10px 14px', marginBottom: 14, borderRadius: 8,
    background: 'color-mix(in srgb, var(--j-soft-fill) 10%, transparent)', border: `1px solid color-mix(in srgb, ${SOFT_AMBER} 27%, transparent)`,
    color: SOFT_AMBER, fontSize: 12, display: 'flex', alignItems: 'center',
  },
  dimPickerRow: {
    display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 14,
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
    background: 'var(--j-surface)', border: `1px solid ${COLORS.borderStrong}`,
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
  tableFooterNote: { padding: '10px 4px 0', fontSize: 11, color: COLORS.textMuted, lineHeight: 1.5 },
  expandCell: {
    padding: '14px 16px', background: 'color-mix(in srgb, var(--j-text) 2%, transparent)',
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
    background: 'color-mix(in srgb, var(--j-warn) 8%, transparent)', border: `1px solid color-mix(in srgb, ${COLORS.warn} 20%, transparent)`,
    color: COLORS.textSecondary, fontSize: 13, lineHeight: 1.6,
  },
};

export default EventLakeExplorer;
