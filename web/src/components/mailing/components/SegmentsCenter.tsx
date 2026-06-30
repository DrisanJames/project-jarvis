/**
 * SegmentsCenter — the operator-grade Segments screen.
 *
 * PAGE_VERSION 3.0 (2026-06-10) — complete rebuild of the old SegmentsManager
 * (ListPortal.tsx v2.3.0). Production has ~31,600 segments of which ~30,977
 * are machine-generated partner_wave_static wave snapshots; the operated set
 * is ~350. The old screen client-filtered the entire list as a card grid.
 * This screen fetches its own data with server-side filters and defaults to
 * EXCLUDING machine rows.
 *
 * Server contract (segmentation API v2.4.0):
 *   GET /api/mailing/v2/segments
 *       ?q=&status=active|archived|inactive|all&categories=a,b
 *       &exclude_categories=a,b&limit=&offset=&include_counts=1
 *     include_counts=1 → {"segments":[...], "total":N, "category_counts":{...}}
 *     (total honors every filter ignoring limit/offset; category_counts honors
 *     q/status but ignores category filters). Without include_counts the
 *     legacy bare array is returned — both shapes are parsed here.
 *   POST /v2/segments/{id}/refresh?force=
 *     202 {"status":"started","mode":"lake"} → spinner + 5s list polling
 *     200 {"status":"ok","mode":"materializer","count":N} → row patched
 *     409 build slot busy · 503 lake reader disabled · 400/404 errors
 *     blocked_delta rows refresh with force=true behind a confirm.
 *   POST /v2/segments/build-request {"event","windows","scope",...} →
 *     201 {"segments":[{id,name,window_days}],"building":true}
 *   GET  /v2/segments/{id}/members.csv?format=txt — member sample (streamed,
 *     aborted after the first ~25 lines) + the Export CSV drawer action.
 *     NOTE: GET /{id}/subscribers is NOT used — it executes the live segment
 *     query (heavy; unfiltered for lake_spec rows) and returns only
 *     subscriber UUIDs, no emails.
 *   DELETE /v2/segments/{id} — archive.
 *
 * REMOVED vs the old SegmentsManager (v2.3.0):
 *   - the card grid (replaced by a dense sortable table);
 *   - per-card legacy Recalculate (POST /{id}/recalculate) — the ledger-aware
 *     Refresh is the only rebuild affordance; the server rejects legacy
 *     recalculate for lake rows anyway;
 *   - the client-side Type (dynamic/static) dropdown filter;
 *   - the client-side category <select> (replaced by server-driven chips);
 *   - whole-list client filtering of ListPortal's 31k-row segments prop;
 *   - the dead "Preview Subscribers" no-op button;
 *   - per-card build_source badges and SYSTEM card chrome (still surfaced in
 *     the detail drawer / Built tooltip).
 *
 * KEPT from v2.3.0: the Request Segment modal (event × windows × scope →
 * build-request), the shared 5s/5-min single-interval build polling, and the
 * minimal local toast stack.
 */

import React, { useState, useEffect, useCallback, useRef, useMemo } from 'react';
import { createPortal } from 'react-dom';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCrosshairs,
  faSearch,
  faPlus,
  faRocket,
  faSpinner,
  faSyncAlt,
  faCopy,
  faCheck,
  faTimes,
  faPencilAlt,
  faEye,
  faDownload,
  faBoxArchive,
  faSort,
  faSortUp,
  faSortDown,
  faArrowTrendUp,
  faArrowTrendDown,
  faChartLine,
  faTable,
  faMinus,
  faExclamationTriangle,
  faEnvelopeOpen,
  faHandPointer,
} from '@fortawesome/free-solid-svg-icons';
import { SEGMENT_CATEGORIES_BY_ID, SegmentCategoryMeta } from './segCategoryMetadata';
import {
  colors,
  alpha,
  panelStyle,
  btnStyle,
  cardGrid,
  thStyle,
  tdStyle,
  numTh,
  numTd,
  tableStyle,
} from '../shared/theme';
import { Panel, Stat, SectionError, EmptyState, Pill, LivePill, PortalKeyframes } from '../shared/ui';
import { usePolling } from '../shared/usePolling';

// PAGE_VERSION 3.1 (2026-06-29) — indigo redesign. The DEFAULT view is now the
// brand-grouped Engagement Growth board (7D Openers + 30D Clickers per brand,
// from GET /v2/segments/engagement-growth), modeled on the Delivery Queue's
// Stat-tile layout. The full ~350-row operated catalog (search / refresh /
// archive / export / request-segment) is preserved behind the "All Segments"
// toggle. The whole screen is restyled onto the shared indigo tokens.
export const SEGMENTS_PAGE_VERSION = '3.1';

// Engagement growth board polls a cheap read (ledger point-read) — slow cadence.
const GROWTH_POLL_MS = 30_000;

// ============================================================================
// TYPES
// ============================================================================

/**
 * One row from GET /v2/segments. Structurally a superset of ListPortal's
 * Segment interface so rows can be handed to onNavigate('edit-segment', …).
 */
export interface SegmentRow {
  id: string;
  name: string;
  description?: string;
  list_id?: string;
  category?: string;
  segment_type: 'dynamic' | 'static';
  subscriber_count: number;
  status: 'active' | 'draft' | 'archived';
  is_system?: boolean;
  created_at: string;
  updated_at: string;
  materialized_count?: number;
  materialized_at?: string;
  audience_count?: number;
  audience_source?: 'materialized' | 'cached';
  last_calculated_at?: string;
  last_built_at?: string | null;
  build_source?: string | null;
  last_build_status?: 'ok' | 'failed' | 'running' | 'blocked_delta' | null;
  last_build_ms?: number | null;
  last_delta_pct?: number | null;
}

interface SegmentsCenterProps {
  onNavigate: (view: 'create-segment' | 'edit-segment', list?: undefined, segment?: SegmentRow) => void;
  orgFetch: (url: string, options?: RequestInit) => Promise<Response>;
  animateIn: boolean;
}

type StatusFilter = 'active' | 'archived' | 'all';
type SortKey = 'name' | 'members' | 'built';

interface Toast { id: number; kind: 'ok' | 'error' | 'info'; msg: string }

// ============================================================================
// CONSTANTS
// ============================================================================

/** Machine-generated partner-drip wave snapshots — excluded by default. */
const MACHINE_CATEGORY = 'partner_wave_static';
const PAGE_LIMIT = 500;
/** Member-sample size shown in the drawer (members.csv stream, then abort). */
const SAMPLE_LIMIT = 25;

// Lake builds run async server-side; poll the list endpoint every 5s for up
// to 5 minutes per build (failed fetches count toward the budget).
const BUILD_POLL_INTERVAL_MS = 5_000;
const BUILD_POLL_MAX_MS = 5 * 60_000;

const BUILD_REQUEST_PRESET_WINDOWS = [3, 7, 14, 30];
const BUILD_REQUEST_MAX_WINDOWS = 8;
const BUILD_REQUEST_KNOWN_ISPS = ['gmail', 'yahoo', 'microsoft', 'aol', 'comcast', 'att', 'charter', 'cox', 'verizon', 'icloud'];

// Lake-built rows: the ledger Refresh (async 202) is their only rebuild path.
const LAKE_BUILD_SOURCES = ['lake-builder', 'lake-standard', 'lake-engaged'];
const isLakeRow = (s: SegmentRow): boolean => LAKE_BUILD_SOURCES.includes(s.build_source || '');

const BUILD_STATUS_DOT: Record<string, string> = {
  ok: '#10b981',
  failed: '#ef4444',
  blocked_delta: '#f59e0b',
};

const BUILD_STATUS_TOOLTIP: Record<string, string> = {
  ok: 'last build ok',
  failed: 'last build failed',
  running: 'build running',
  blocked_delta: 'blocked: count changed >50% — Refresh becomes Force (confirms first)',
};

/** Inline badge palette per category (the portal does not use Tailwind). */
const CATEGORY_COLORS: Record<string, { bg: string; fg: string; border: string }> = {
  engagement_brand: { bg: 'rgba(99,102,241,0.15)', fg: '#a5b4fc', border: 'rgba(129,140,248,0.4)' },
  engagement_global: { bg: 'rgba(168,85,247,0.15)', fg: '#d8b4fe', border: 'rgba(192,132,252,0.4)' },
  engagement_isp: { bg: 'rgba(59,130,246,0.15)', fg: '#93c5fd', border: 'rgba(96,165,250,0.4)' },
  engagement_vertical: { bg: 'rgba(6,182,212,0.15)', fg: '#67e8f9', border: 'rgba(34,211,238,0.4)' },
  framework: { bg: 'rgba(16,185,129,0.15)', fg: '#6ee7b7', border: 'rgba(52,211,153,0.4)' },
  funnel: { bg: 'rgba(20,184,166,0.15)', fg: '#5eead4', border: 'rgba(45,212,191,0.4)' },
  cohort_static: { bg: 'rgba(245,158,11,0.15)', fg: '#fcd34d', border: 'rgba(251,191,36,0.4)' },
  suppression_exclusion: { bg: 'rgba(244,63,94,0.15)', fg: '#fda4af', border: 'rgba(251,113,133,0.4)' },
  partner_wave_static: { bg: 'rgba(100,116,139,0.18)', fg: '#94a3b8', border: 'rgba(148,163,184,0.35)' },
  legacy_snapshot: { bg: 'rgba(113,113,122,0.18)', fg: '#a1a1aa', border: 'rgba(161,161,170,0.35)' },
  uncategorized: { bg: 'rgba(82,82,91,0.25)', fg: '#a1a1aa', border: 'rgba(113,113,122,0.4)' },
};

const DEFAULT_CATEGORY_COLOR = { bg: 'rgba(0,200,255,0.08)', fg: 'rgba(180,210,240,0.8)', border: 'rgba(0,200,255,0.25)' };

// ============================================================================
// HELPERS
// ============================================================================

const categoryMetaFor = (cat: string): SegmentCategoryMeta | undefined =>
  (SEGMENT_CATEGORIES_BY_ID as Record<string, SegmentCategoryMeta>)[cat];

/** Short label for chips/badges. Falls back to the raw id for categories not
 * in the static metadata (e.g. 'engaged-model', 'data-partner'). */
const categoryLabel = (cat: string): string => {
  if (cat === MACHINE_CATEGORY) return 'Auto-generated';
  return categoryMetaFor(cat)?.shortLabel ?? cat;
};

const categoryTitle = (cat: string): string => {
  const meta = categoryMetaFor(cat);
  return meta ? `${meta.label} — ${meta.description}` : cat;
};

const fmtRel = (iso?: string | null): string => {
  if (!iso) return 'never';
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return 'never';
  const m = Math.floor((Date.now() - t) / 60000);
  if (m < 1) return 'just now';
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.floor(h / 24);
  if (d < 30) return `${d}d ago`;
  return new Date(iso).toLocaleDateString();
};

const fmtAbs = (iso?: string | null): string => {
  if (!iso) return '—';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString();
};

/** The number the operator should trust: ledger/materialized when present. */
const memberCount = (s: SegmentRow): number =>
  s.audience_count ?? s.materialized_count ?? s.subscriber_count ?? 0;

const builtIso = (s: SegmentRow): string | null =>
  s.last_built_at ?? s.materialized_at ?? s.last_calculated_at ?? null;

const fmtInt = (n: number | null | undefined): string =>
  n == null || Number.isNaN(n) ? '0' : Math.round(n).toLocaleString();

// ============================================================================
// ENGAGEMENT GROWTH BOARD (default view)
// ============================================================================
//
// Brand-grouped current size + last-build delta for the per-brand engagement
// segments the operator actively mails. Data from GET
// /v2/segments/engagement-growth (segmentation_handlers.go).
//
// GROWTH SIGNAL: last_delta_pct is the BUILD-TO-BUILD delta (current audience
// vs the previous build) — NOT a time-windowed trend. There is no per-day
// segment-size history table yet, so the UI is honest about this and labels it
// "Δ since last build", never "7-day growth".
// TODO(growth-history): a daily segment-size snapshot table would let this
// board draw real trend lines.

interface GrowthMetric {
  segment_id: string;
  name: string;
  count: number;
  delta_pct: number | null;
  built_at: string | null;
  source: 'materialized' | 'cached';
  status?: string | null;
}

interface GrowthBrand {
  brand: string;
  /** Full sending-domain apex resolved server-side (e.g. discountblog.com); '' / absent when the brand code has no known mapping. */
  domain?: string;
  seven_day_openers: GrowthMetric | null;
  seven_day_clickers: GrowthMetric | null;
  thirty_day_openers: GrowthMetric | null;
  thirty_day_clickers: GrowthMetric | null;
  cells: Record<string, GrowthMetric | null>;
  segment_count: number;
  headline_count: number;
}

interface GrowthResponse {
  brands: GrowthBrand[];
  brand_count: number;
  segment_count: number;
  growth_signal: string;
  growth_label: string;
  note: string;
  generated_at: string;
  api_version: string;
}

type GrowthSort = 'size' | 'growth';

/**
 * Brand code → full sending-domain apex. Mirrors the authoritative server map
 * (`brandCodeRoot` in internal/api/money_link_check.go, itself kept in sync with
 * internal/pkg/brand.OwnedDomains + brand_metadata.py). The backend now returns
 * `domain` per brand; this is a client-side fallback so an un-redeployed server
 * still shows full domains. Unknown codes fall back to the raw code.
 */
const BRAND_CODE_TO_DOMAIN: Record<string, string> = {
  DB: 'discountblog.com',
  HT: 'historythinking.com',
  MH: 'myownhealth.net',
  QF: 'quizfiesta.com',
  BW: 'businessweeklypro.com',
  FC: 'financialcalculate.com',
  CP: 'consumerpro.net',
  HW: 'homewarrantyservices.org',
  RR: 'refinanceratesusa.com',
  TT: 'thingoftheday.org',
  YI: 'yourinsurancehub.com',
  MR: 'myrepairdiy.com',
  CI: 'casainsure.com',
  LP: 'learnpersonalloans.com',
  RB: 'ratesbazar.com',
  WF: 'warrantyforyou.com',
  MP: 'mypersonalfinancial.com',
  PD: 'paymydebit.com',
  TR: 'theretirementblog.com',
};

/** Full sending domain for a brand: server `domain` first, then the local map,
 * finally the raw code so an unmapped brand is shown (never hidden). */
const brandDomain = (b: GrowthBrand): string =>
  (b.domain && b.domain.trim()) || BRAND_CODE_TO_DOMAIN[b.brand.trim().toUpperCase()] || b.brand;

/** Cell order for the expandable per-brand detail table. */
const GROWTH_CELL_ORDER = [
  '7D Openers', '14D Openers', '30D Openers', '60D Openers',
  '7D Clickers', '14D Clickers', '30D Clickers', '60D Clickers',
];

/** The strongest |delta| across a brand's cells — used for the "by growth" sort. */
const brandPeakDelta = (b: GrowthBrand): number => {
  let peak = -Infinity;
  Object.values(b.cells).forEach((c) => {
    if (c && c.delta_pct != null && c.delta_pct > peak) peak = c.delta_pct;
  });
  return peak === -Infinity ? -Infinity : peak;
};

// Build-to-build delta badge: up = green ▲, down = red ▼, flat = idle, none = —.
const DeltaBadge: React.FC<{ pct: number | null | undefined; size?: 'sm' | 'lg' }> = ({ pct, size = 'sm' }) => {
  const fontSize = size === 'lg' ? 13 : 11;
  if (pct == null) {
    return <span style={{ fontSize, color: colors.textFaint }} title="No previous build to compare against">— new</span>;
  }
  const up = pct > 0.05;
  const down = pct < -0.05;
  const color = up ? colors.success : down ? colors.danger : colors.idle;
  const icon = up ? faArrowTrendUp : down ? faArrowTrendDown : faMinus;
  return (
    <span
      style={{ display: 'inline-flex', alignItems: 'center', gap: 4, fontSize, fontWeight: 700, color, fontVariantNumeric: 'tabular-nums' }}
      title="Δ since last build (current audience size vs the previous build)"
    >
      <FontAwesomeIcon icon={icon} style={{ fontSize: fontSize - 1 }} />
      {pct > 0 ? '+' : ''}{pct.toFixed(1)}%
    </span>
  );
};

// One headline metric tile (7D Openers / 30D Clickers): count + delta.
const MetricTile: React.FC<{ label: string; icon: typeof faEnvelopeOpen; metric: GrowthMetric | null }> = ({ label, icon, metric }) => (
  <div
    style={{
      flex: 1,
      minWidth: 140,
      background: alpha(colors.indigo500, '0d'),
      border: `1px solid ${colors.hairline}`,
      borderRadius: 8,
      padding: '10px 12px',
    }}
  >
    <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 10, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.5 }}>
      <FontAwesomeIcon icon={icon} style={{ color: colors.indigo400 }} />
      {label}
    </div>
    {metric ? (
      <>
        <div
          style={{ fontSize: 10, color: colors.textFaint, marginTop: 1, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}
          title={metric.name}
        >
          {metric.name}
        </div>
        <div style={{ fontSize: 22, fontWeight: 800, color: colors.heading, fontVariantNumeric: 'tabular-nums', marginTop: 3 }}>
          {fmtInt(metric.count)}
        </div>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginTop: 3 }}>
          <DeltaBadge pct={metric.delta_pct} />
          <span style={{ fontSize: 10, color: colors.textFaint }} title={metric.source === 'materialized' ? `Built ${fmtAbs(metric.built_at)}` : 'Cached estimate — not yet built via ledger'}>
            {metric.source === 'materialized' ? fmtRel(metric.built_at) : 'cached'}
          </span>
        </div>
      </>
    ) : (
      <div style={{ fontSize: 13, color: colors.textFaint, marginTop: 8 }}>not configured</div>
    )}
  </div>
);

const BrandGrowthCard: React.FC<{ brand: GrowthBrand }> = ({ brand }) => {
  const domain = brandDomain(brand);
  const detailCells = GROWTH_CELL_ORDER.map((k) => [k, brand.cells[k]] as const).filter(([, c]) => c);
  // The two headline windows are already shown as tiles above; the inline table
  // surfaces every OTHER configured window. No click gate — details show by default.
  const extraCells = detailCells.filter(([k]) => !['7D Openers', '30D Clickers'].includes(k));
  return (
    <Panel accent={colors.indigo500}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, marginBottom: 10 }}>
        <h3
          style={{ margin: 0, fontSize: 15, fontWeight: 700, color: colors.heading, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
          title={domain === brand.brand ? brand.brand : `${domain} · ${brand.brand}`}
        >
          {domain}
        </h3>
        <Pill color={colors.indigo400} style={{ flexShrink: 0 }}>{brand.segment_count} seg</Pill>
      </div>
      <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
        <MetricTile label="7D Openers" icon={faEnvelopeOpen} metric={brand.seven_day_openers} />
        <MetricTile label="30D Clickers" icon={faHandPointer} metric={brand.thirty_day_clickers} />
      </div>
      {extraCells.length > 0 && (
        <>
          <div style={{ marginTop: 10, fontSize: 10, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.5 }}>
            More windows ({extraCells.length})
          </div>
          <table style={{ ...tableStyle, marginTop: 6 }}>
            <thead>
              <tr>
                <th style={thStyle}>Window</th>
                <th style={numTh}>Members</th>
                <th style={numTh}>Δ build</th>
                <th style={numTh}>Built</th>
              </tr>
            </thead>
            <tbody>
              {extraCells.map(([k, c]) => (
                <tr key={k}>
                  <td style={{ ...tdStyle, fontWeight: 600, color: colors.indigo200 }} title={c!.name}>{k}</td>
                  <td style={numTd}>{fmtInt(c!.count)}</td>
                  <td style={numTd}><DeltaBadge pct={c!.delta_pct} /></td>
                  <td style={{ ...numTd, color: colors.textMuted }}>{c!.source === 'materialized' ? fmtRel(c!.built_at) : 'cached'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </Panel>
  );
};

const GrowthBoard: React.FC<{
  orgFetchRef: React.MutableRefObject<(url: string, options?: RequestInit) => Promise<Response>>;
  onViewAll: () => void;
  onRequestSegment: () => void;
}> = ({ orgFetchRef, onViewAll, onRequestSegment }) => {
  const [sort, setSort] = useState<GrowthSort>('size');

  const growth = usePolling<GrowthResponse>(
    useCallback(async (signal: AbortSignal) => {
      const res = await orgFetchRef.current('/api/mailing/v2/segments/engagement-growth', { signal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return (await res.json()) as GrowthResponse;
    }, [orgFetchRef]),
    GROWTH_POLL_MS,
  );

  const brands = useMemo(() => {
    const list = growth.data?.brands ? [...growth.data.brands] : [];
    if (sort === 'growth') {
      list.sort((a, b) => brandPeakDelta(b) - brandPeakDelta(a));
    } else {
      list.sort((a, b) => b.headline_count - a.headline_count);
    }
    return list;
  }, [growth.data, sort]);

  // Aggregate hero tiles across all brands.
  const totals = useMemo(() => {
    let openers = 0, clickers = 0, segs = 0;
    (growth.data?.brands ?? []).forEach((b) => {
      if (b.seven_day_openers) openers += b.seven_day_openers.count;
      if (b.thirty_day_clickers) clickers += b.thirty_day_clickers.count;
      segs += b.segment_count;
    });
    return { openers, clickers, segs };
  }, [growth.data]);

  const sortBtn = (key: GrowthSort): React.CSSProperties => ({
    ...btnStyle,
    padding: '5px 10px',
    fontSize: 11,
    background: sort === key ? alpha(colors.indigo500, '33') : alpha(colors.indigo500, '14'),
    color: sort === key ? colors.indigo200 : colors.textMuted,
    borderColor: sort === key ? colors.panelBorderStrong : colors.panelBorder,
  });

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      {/* Hero strip */}
      <section style={{ ...panelStyle, display: 'flex', alignItems: 'center', gap: 24, flexWrap: 'wrap', borderLeft: `4px solid ${colors.indigo500}` }}>
        <div style={{ flex: 1, minWidth: 220 }}>
          <div style={{ fontSize: 14, fontWeight: 700, color: colors.heading, display: 'flex', alignItems: 'center', gap: 8 }}>
            <FontAwesomeIcon icon={faChartLine} style={{ color: colors.indigo400 }} />
            Engagement Growth by Brand
          </div>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4, lineHeight: 1.5 }}>
            7D Openers &amp; 30D Clickers per brand. Growth is{' '}
            <span style={{ color: colors.indigo200, fontWeight: 600 }}>Δ since last build</span>{' '}
            (build-to-build), not a daily trend — no size history exists yet.
          </div>
        </div>
        <Stat label="Brands" value={fmtInt(growth.data?.brand_count ?? 0)} color={colors.indigo200} />
        <Stat label="Tracked segments" value={fmtInt(totals.segs)} color={colors.indigo200} />
        <Stat label="Σ 7D Openers" value={fmtInt(totals.openers)} color={colors.successText} />
        <Stat label="Σ 30D Clickers" value={fmtInt(totals.clickers)} color={colors.successText} />
      </section>

      {/* Controls */}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, flexWrap: 'wrap' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span style={{ fontSize: 11, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.5 }}>Sort</span>
          <button type="button" style={sortBtn('size')} onClick={() => setSort('size')}>Largest</button>
          <button type="button" style={sortBtn('growth')} onClick={() => setSort('growth')}>Fastest growing</button>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <LivePill live={growth.live} agoSeconds={growth.secondsSinceUpdate} />
          <button type="button" style={btnStyle} onClick={growth.refresh} title="Refresh growth board">
            <FontAwesomeIcon icon={faSyncAlt} style={{ marginRight: 6 }} />Refresh
          </button>
        </div>
      </div>

      {growth.error && (
        <SectionError label="Engagement growth" error={growth.error} />
      )}

      {growth.loading && !growth.data ? (
        <div style={{ textAlign: 'center', padding: 60, color: colors.textMuted }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading engagement growth…
        </div>
      ) : brands.length === 0 ? (
        <Panel>
          <EmptyState
            icon={faChartLine}
            title="No per-brand engagement segments yet"
            hint="Request 7D-Openers / 30D-Clickers segments (scope = Brand) to start tracking engagement growth here."
          />
          <div style={{ display: 'flex', justifyContent: 'center', gap: 10, marginTop: 4 }}>
            <button type="button" style={{ ...btnStyle, background: alpha(colors.indigo500, '33') }} onClick={onRequestSegment}>
              <FontAwesomeIcon icon={faRocket} style={{ marginRight: 6 }} />Request Segment
            </button>
            <button type="button" style={btnStyle} onClick={onViewAll}>
              <FontAwesomeIcon icon={faTable} style={{ marginRight: 6 }} />Browse all segments
            </button>
          </div>
        </Panel>
      ) : (
        <div style={cardGrid(320)}>
          {brands.map((b) => <BrandGrowthCard key={b.brand} brand={b} />)}
        </div>
      )}
    </div>
  );
};

// ============================================================================
// COMPONENT
// ============================================================================

export const SegmentsCenter: React.FC<SegmentsCenterProps> = ({ onNavigate, orgFetch, animateIn }) => {
  // --- primary view: brand-grouped growth board (default) vs the full catalog -
  const [view, setView] = useState<'growth' | 'all'>('growth');

  // --- list state -----------------------------------------------------------
  const [rows, setRows] = useState<SegmentRow[]>([]);
  const [total, setTotal] = useState(0);
  const [categoryCounts, setCategoryCounts] = useState<Record<string, number>>({});
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [listError, setListError] = useState<string | null>(null);

  // --- filters --------------------------------------------------------------
  const [searchInput, setSearchInput] = useState('');
  const [q, setQ] = useState('');
  const [status, setStatus] = useState<StatusFilter>('active');
  const [category, setCategory] = useState<string>('all'); // 'all' | category id

  // --- sort -----------------------------------------------------------------
  const [sortKey, setSortKey] = useState<SortKey>('built');
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc');

  // --- row interactions -----------------------------------------------------
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [refreshingId, setRefreshingId] = useState<string | null>(null);
  const [localRunning, setLocalRunning] = useState<Record<string, boolean>>({});
  const [archivingId, setArchivingId] = useState<string | null>(null);

  // --- drawer ---------------------------------------------------------------
  const [drawerId, setDrawerId] = useState<string | null>(null);
  const [sample, setSample] = useState<{ loading: boolean; emails: string[]; error?: string }>({ loading: false, emails: [] });

  // --- toasts ---------------------------------------------------------------
  const [toasts, setToasts] = useState<Toast[]>([]);
  const toastSeqRef = useRef(0);
  const pushToast = useCallback((kind: Toast['kind'], msg: string) => {
    const id = ++toastSeqRef.current;
    setToasts(prev => [...prev, { id, kind, msg }]);
    setTimeout(() => setToasts(prev => prev.filter(t => t.id !== id)), 6000);
  }, []);

  // Keep latest orgFetch visible to long-lived interval closures.
  const orgFetchRef = useRef(orgFetch);
  orgFetchRef.current = orgFetch;

  // --- search debounce (300ms → server q=) -----------------------------------
  useEffect(() => {
    const t = setTimeout(() => setQ(searchInput.trim()), 300);
    return () => clearTimeout(t);
  }, [searchInput]);

  // --- list fetching ----------------------------------------------------------
  const buildListURL = useCallback((offset: number): string => {
    const p = new URLSearchParams();
    if (q) p.set('q', q);
    p.set('status', status);
    if (category === 'all') {
      p.set('exclude_categories', MACHINE_CATEGORY);
    } else {
      p.set('categories', category);
    }
    p.set('limit', String(PAGE_LIMIT));
    p.set('offset', String(offset));
    p.set('include_counts', '1');
    return `/api/mailing/v2/segments?${p.toString()}`;
  }, [q, status, category]);

  const reqSeqRef = useRef(0);
  const fetchList = useCallback(async (offset: number, append: boolean) => {
    const seq = ++reqSeqRef.current;
    if (append) setLoadingMore(true); else setLoading(true);
    setListError(null);
    try {
      const res = await orgFetchRef.current(buildListURL(offset));
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const raw = await res.json();
      if (seq !== reqSeqRef.current) return; // stale response — newer request in flight
      const list: SegmentRow[] = Array.isArray(raw) ? raw : (raw?.segments ?? []);
      const tot: number = typeof raw?.total === 'number' ? raw.total : list.length;
      setRows(prev => (append ? [...prev, ...list] : list));
      setTotal(tot);
      if (raw?.category_counts && typeof raw.category_counts === 'object') {
        setCategoryCounts(raw.category_counts as Record<string, number>);
      }
    } catch {
      if (seq !== reqSeqRef.current) return;
      if (append) {
        // A failed "load more" must not blow away the rows already on
        // screen — surface it as a toast, keep the table.
        pushToast('error', 'Load more failed — try again.');
      } else {
        setListError('Failed to load segments — check the API and retry.');
      }
    } finally {
      if (seq === reqSeqRef.current) { setLoading(false); setLoadingMore(false); }
    }
  }, [buildListURL]);

  const fetchListRef = useRef(fetchList);
  fetchListRef.current = fetchList;

  useEffect(() => { fetchList(0, false); }, [fetchList]);

  const patchRow = (id: string, patch: Partial<SegmentRow>) =>
    setRows(prev => prev.map(r => (r.id === id ? { ...r, ...patch } : r)));

  // --- build polling (single shared interval, 5s tick, 5-min cap) -------------
  const watchedBuildsRef = useRef<Map<string, number>>(new Map()); // id → poll start (ms epoch)
  const pollTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const stopPollingIfIdle = () => {
    if (watchedBuildsRef.current.size === 0 && pollTimerRef.current) {
      clearInterval(pollTimerRef.current);
      pollTimerRef.current = null;
    }
  };

  const clearWatched = useCallback((id: string) => {
    watchedBuildsRef.current.delete(id);
    setLocalRunning(prev => {
      const next = { ...prev };
      delete next[id];
      return next;
    });
  }, []);

  const pollBuildTick = useCallback(async () => {
    const watched = watchedBuildsRef.current;
    if (watched.size === 0) { stopPollingIfIdle(); return; }
    let polled: SegmentRow[] = [];
    try {
      // Status=all + machine excluded: covers every operated segment in one
      // page; watched builds are engagement/lake segments by construction.
      const res = await orgFetchRef.current(
        `/api/mailing/v2/segments?status=all&exclude_categories=${MACHINE_CATEGORY}&limit=${PAGE_LIMIT}&offset=0`,
      );
      const raw = await res.json().catch(() => []);
      polled = Array.isArray(raw) ? raw : (raw?.segments ?? []);
    } catch {
      // Transient failure — still burn each build's 5-min budget so a down
      // API can't keep this interval alive forever.
      const failNow = Date.now();
      let anyExpired = false;
      watched.forEach((startedAt, id) => {
        if (failNow - startedAt < BUILD_POLL_MAX_MS) return;
        clearWatched(id);
        anyExpired = true;
        pushToast('error', `${id}: build status unknown — segment list unreachable for 5 min`);
      });
      if (anyExpired) stopPollingIfIdle();
      return;
    }
    const now = Date.now();
    let anyFinished = false;
    watched.forEach((startedAt, id) => {
      const seg = polled.find(s => s.id === id);
      const segStatus = seg?.last_build_status;
      const timedOut = now - startedAt >= BUILD_POLL_MAX_MS;
      if (seg && segStatus === 'running' && !timedOut) return; // still building
      clearWatched(id);
      anyFinished = true;
      const name = seg?.name || id;
      if (!seg) {
        pushToast('error', `${name}: segment no longer in list`);
      } else if (segStatus === 'running') {
        pushToast('error', `${name}: still building after 5 min — check back later`);
      } else if (segStatus === 'ok') {
        pushToast('ok', `${name}: build complete — ${memberCount(seg).toLocaleString()} members`);
      } else if (segStatus === 'blocked_delta') {
        pushToast('error', `${name}: blocked — count changed >50%, use Force refresh`);
      } else {
        pushToast('error', `${name}: last build failed`);
      }
    });
    if (anyFinished) fetchListRef.current(0, false);
    stopPollingIfIdle();
  }, [clearWatched, pushToast]);

  const startBuildPolling = useCallback((ids: string[]) => {
    const watched = watchedBuildsRef.current;
    const now = Date.now();
    ids.forEach(id => { if (!watched.has(id)) watched.set(id, now); });
    setLocalRunning(prev => {
      const next = { ...prev };
      ids.forEach(id => { next[id] = true; });
      return next;
    });
    if (!pollTimerRef.current) {
      pollTimerRef.current = setInterval(pollBuildTick, BUILD_POLL_INTERVAL_MS);
    }
  }, [pollBuildTick]);

  useEffect(() => () => {
    if (pollTimerRef.current) clearInterval(pollTimerRef.current);
  }, []);

  // --- actions -----------------------------------------------------------------
  const copyId = async (id: string) => {
    try {
      await navigator.clipboard.writeText(id);
      setCopiedId(id);
      setTimeout(() => setCopiedId(prev => (prev === id ? null : prev)), 1500);
    } catch {
      pushToast('error', 'Clipboard unavailable — copy the ID from the Details drawer');
    }
  };

  /**
   * Ledger-aware refresh. Lake rows → 202 + async poll; dynamic rows → 200
   * with a fresh count. blocked_delta rows automatically go through the
   * force=true path behind a confirm.
   */
  const handleRefresh = async (segment: SegmentRow) => {
    const force = segment.last_build_status === 'blocked_delta';
    if (force && !confirm(`Force refresh "${segment.name}"?\n\nThe last build was blocked because the audience count changed by more than 50%. Forcing bypasses that guard and overwrites the current audience.`)) return;
    setRefreshingId(segment.id);
    try {
      const res = await orgFetch(`/api/mailing/v2/segments/${segment.id}/refresh?force=${force ? 'true' : 'false'}`, { method: 'POST' });
      const payload = await res.json().catch(() => ({} as Record<string, unknown>));
      if (res.status === 202) {
        pushToast('info', `${segment.name}: segment build started — watching for completion`);
        patchRow(segment.id, { last_build_status: 'running' });
        startBuildPolling([segment.id]);
      } else if (res.ok) {
        const count = typeof payload?.count === 'number' ? payload.count : undefined;
        const nowIso = new Date().toISOString();
        patchRow(segment.id, {
          ...(typeof count === 'number' ? { audience_count: count, materialized_count: count } : {}),
          audience_source: 'materialized',
          materialized_at: nowIso,
          last_built_at: nowIso,
          last_build_status: 'ok',
        });
        pushToast('ok', `${segment.name}: refreshed${typeof count === 'number' ? ` — ${count.toLocaleString()} members` : ''}`);
      } else if (res.status === 409) {
        pushToast('error', 'another segment build is running — try shortly');
      } else if (res.status === 503) {
        pushToast('error', 'segment refresh is temporarily unavailable');
      } else {
        pushToast('error', `${segment.name}: ${String(payload?.error || `refresh failed (HTTP ${res.status})`)}`);
      }
    } catch {
      pushToast('error', `${segment.name}: refresh request failed`);
    } finally {
      setRefreshingId(null);
    }
  };

  const handleArchive = async (segment: SegmentRow) => {
    if (!confirm(`Archive "${segment.name}"?\n\nCampaigns can no longer target it. This is the DELETE /v2/segments/{id} soft-archive.`)) return;
    setArchivingId(segment.id);
    try {
      const res = await orgFetch(`/api/mailing/v2/segments/${segment.id}`, { method: 'DELETE' });
      if (!res.ok) {
        const payload = await res.json().catch(() => ({} as Record<string, unknown>));
        pushToast('error', `${segment.name}: ${String(payload?.error || `archive failed (HTTP ${res.status})`)}`);
        return;
      }
      pushToast('ok', `${segment.name}: archived`);
      setDrawerId(prev => (prev === segment.id ? null : prev));
      fetchListRef.current(0, false);
    } catch {
      pushToast('error', `${segment.name}: archive request failed`);
    } finally {
      setArchivingId(null);
    }
  };

  // --- drawer: member sample via members.csv stream (abort after N lines) ------
  useEffect(() => {
    if (!drawerId) return;
    const controller = new AbortController();
    let cancelled = false;
    setSample({ loading: true, emails: [] });
    (async () => {
      try {
        // dedupe=false is deliberate: the default dedupe path runs
        // SELECT DISTINCT ... ORDER BY over ALL members before the first byte
        // streams — a full sort on multi-million-member segments. Raw mode
        // streams immediately, so aborting after the sample actually bounds
        // the DB work.
        const res = await orgFetchRef.current(
          `/api/mailing/v2/segments/${drawerId}/members.csv?format=txt&dedupe=false`,
          { signal: controller.signal },
        );
        if (!res.ok || !res.body) {
          if (!cancelled) setSample({ loading: false, emails: [], error: `member sample unavailable (HTTP ${res.status})` });
          return;
        }
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buf = '';
        const emails: string[] = [];
        for (;;) {
          const { done, value } = await reader.read();
          if (value) buf += decoder.decode(value, { stream: true });
          let nl: number;
          while ((nl = buf.indexOf('\n')) >= 0 && emails.length < SAMPLE_LIMIT) {
            const line = buf.slice(0, nl).trim();
            buf = buf.slice(nl + 1);
            if (line) emails.push(line);
          }
          if (emails.length >= SAMPLE_LIMIT) break;
          if (done) {
            const tail = buf.trim();
            if (tail && emails.length < SAMPLE_LIMIT) emails.push(tail);
            break;
          }
        }
        if (!cancelled) setSample({ loading: false, emails });
        controller.abort(); // we only needed the first page — stop the stream
      } catch (err) {
        const aborted = err instanceof DOMException && err.name === 'AbortError';
        if (!cancelled && !aborted) {
          setSample({ loading: false, emails: [], error: 'failed to load member sample' });
        }
      }
    })();
    return () => { cancelled = true; controller.abort(); };
  }, [drawerId]);

  // Esc closes the drawer.
  useEffect(() => {
    if (!drawerId) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setDrawerId(null); };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [drawerId]);

  // --- Request Segment modal (POST /v2/segments/build-request) -----------------
  const [showRequestModal, setShowRequestModal] = useState(false);
  const [reqEvent, setReqEvent] = useState<'open' | 'click'>('open');
  const [reqWindows, setReqWindows] = useState<number[]>([7]);
  const [reqCustomWindow, setReqCustomWindow] = useState('');
  const [reqScope, setReqScope] = useState<'global' | 'brand' | 'isp'>('global');
  const [reqBrandApex, setReqBrandApex] = useState('');
  const [reqIsp, setReqIsp] = useState('');
  const [reqExcludeSeeds, setReqExcludeSeeds] = useState(true);
  const [reqName, setReqName] = useState('');
  const [reqSubmitting, setReqSubmitting] = useState(false);
  const [reqError, setReqError] = useState<string | null>(null);

  const toggleReqWindow = (w: number) => {
    setReqError(null);
    setReqWindows(prev => {
      if (prev.includes(w)) return prev.filter(x => x !== w);
      if (prev.length >= BUILD_REQUEST_MAX_WINDOWS) {
        setReqError(`Max ${BUILD_REQUEST_MAX_WINDOWS} windows per request`);
        return prev;
      }
      return [...prev, w];
    });
  };

  const addCustomReqWindow = () => {
    const n = parseInt(reqCustomWindow, 10);
    // Server rejects windows outside 1..120 — fail fast client-side.
    if (!Number.isFinite(n) || n <= 0 || n > 120) {
      setReqError('Custom window must be between 1 and 120 days');
      return;
    }
    setReqError(null);
    if (!reqWindows.includes(n)) {
      if (reqWindows.length >= BUILD_REQUEST_MAX_WINDOWS) {
        setReqError(`Max ${BUILD_REQUEST_MAX_WINDOWS} windows per request`);
        return;
      }
      setReqWindows(prev => [...prev, n]);
    }
    setReqCustomWindow('');
  };

  const submitBuildRequest = async (e: React.FormEvent) => {
    e.preventDefault();
    setReqError(null);
    if (reqWindows.length === 0) { setReqError('Select at least one window'); return; }
    if (reqScope === 'brand' && !reqBrandApex.trim()) { setReqError('Brand apex is required for brand scope (e.g. discountblog.com)'); return; }
    if (reqScope === 'isp' && !reqIsp.trim()) { setReqError('ISP is required for ISP scope (e.g. gmail)'); return; }
    const body: Record<string, unknown> = {
      event: reqEvent,
      windows: [...reqWindows].sort((a, b) => a - b),
      scope: reqScope,
      exclude_seeds: reqExcludeSeeds,
    };
    if (reqScope === 'brand') body.brand_apex = reqBrandApex.trim();
    if (reqScope === 'isp') body.isp = reqIsp.trim();
    if (reqName.trim()) body.name = reqName.trim();
    setReqSubmitting(true);
    try {
      const res = await orgFetch('/api/mailing/v2/segments/build-request', {
        method: 'POST',
        body: JSON.stringify(body),
      });
      const payload = await res.json().catch(() => ({} as Record<string, unknown>));
      if (res.status === 201) {
        const created: { id: string; name: string; window_days: number }[] = Array.isArray(payload?.segments) ? payload.segments : [];
        pushToast('ok', `${created.length} segment(s) created, building…`);
        setShowRequestModal(false);
        fetchListRef.current(0, false);
        if (created.length > 0) startBuildPolling(created.map(s => s.id));
      } else if (res.status === 409) {
        setReqError('Build in progress — another segment build is already running. Try again shortly.');
      } else if (res.status === 503) {
        setReqError('Segment service is temporarily unavailable — try again later.');
      } else {
        setReqError(String(payload?.error || `Request failed (HTTP ${res.status})`));
      }
    } catch {
      setReqError('Network error submitting build request');
    } finally {
      setReqSubmitting(false);
    }
  };

  const reqChipWindows = Array.from(new Set([...BUILD_REQUEST_PRESET_WINDOWS, ...reqWindows])).sort((a, b) => a - b);

  // --- derived ------------------------------------------------------------------
  const machineCount = categoryCounts[MACHINE_CATEGORY] || 0;
  const operatedCount = Object.entries(categoryCounts)
    .filter(([cat]) => cat !== MACHINE_CATEGORY)
    .reduce((sum, [, n]) => sum + n, 0);

  // Chips: every non-machine category with rows, sorted by count desc; the
  // Machine chip is pinned last and is the ONLY way to see machine rows.
  const chipCategories = useMemo(
    () => Object.entries(categoryCounts)
      .filter(([cat, n]) => cat !== MACHINE_CATEGORY && n > 0)
      .sort((a, b) => b[1] - a[1]),
    [categoryCounts],
  );

  const sortedRows = useMemo(() => {
    const dir = sortDir === 'asc' ? 1 : -1;
    const arr = [...rows];
    arr.sort((a, b) => {
      if (sortKey === 'name') return a.name.localeCompare(b.name) * dir;
      if (sortKey === 'members') return (memberCount(a) - memberCount(b)) * dir;
      // built: nulls always last regardless of direction
      const ta = builtIso(a) ? new Date(builtIso(a) as string).getTime() : null;
      const tb = builtIso(b) ? new Date(builtIso(b) as string).getTime() : null;
      if (ta === null && tb === null) return 0;
      if (ta === null) return 1;
      if (tb === null) return -1;
      return (ta - tb) * dir;
    });
    return arr;
  }, [rows, sortKey, sortDir]);

  const toggleSort = (key: SortKey) => {
    if (sortKey === key) {
      setSortDir(d => (d === 'asc' ? 'desc' : 'asc'));
    } else {
      setSortKey(key);
      setSortDir(key === 'name' ? 'asc' : 'desc');
    }
  };

  const sortIcon = (key: SortKey) =>
    sortKey !== key ? faSort : (sortDir === 'asc' ? faSortUp : faSortDown);

  const drawerRow = drawerId ? rows.find(r => r.id === drawerId) ?? null : null;

  const effectiveBuildStatus = (s: SegmentRow): string | null =>
    localRunning[s.id] ? 'running' : (s.last_build_status ?? null);

  const builtTooltip = (s: SegmentRow): string => {
    const st = effectiveBuildStatus(s);
    return [
      builtIso(s) ? `Built ${fmtAbs(builtIso(s))}` : 'Never built',
      s.build_source ? `source: ${s.build_source}` : null,
      st ? BUILD_STATUS_TOOLTIP[st] ?? st : null,
      typeof s.last_build_ms === 'number' ? `took ${s.last_build_ms.toLocaleString()}ms` : null,
      typeof s.last_delta_pct === 'number' ? `Δ ${s.last_delta_pct > 0 ? '+' : ''}${s.last_delta_pct.toFixed(1)}% vs previous build` : null,
    ].filter(Boolean).join(' · ');
  };

  // --- render helpers --------------------------------------------------------
  const renderCategoryBadge = (cat: string | undefined) => {
    const id = cat || 'uncategorized';
    const c = CATEGORY_COLORS[id] ?? DEFAULT_CATEGORY_COLOR;
    return (
      <span
        title={categoryTitle(id)}
        style={{
          display: 'inline-block',
          fontSize: '0.6rem',
          padding: '2px 7px',
          borderRadius: 10,
          fontWeight: 700,
          letterSpacing: 0.4,
          textTransform: 'uppercase',
          whiteSpace: 'nowrap',
          background: c.bg,
          color: c.fg,
          border: `1px solid ${c.border}`,
        }}
      >
        {categoryLabel(id)}
      </span>
    );
  };

  const renderTypeBadge = (t: SegmentRow['segment_type']) => (
    <span
      style={{
        display: 'inline-block',
        fontSize: '0.6rem',
        padding: '2px 7px',
        borderRadius: 4,
        fontWeight: 600,
        textTransform: 'uppercase',
        letterSpacing: 0.4,
        background: t === 'dynamic' ? 'rgba(0,176,255,0.12)' : 'rgba(0,229,255,0.1)',
        color: t === 'dynamic' ? '#00b0ff' : '#00e5ff',
      }}
    >
      {t}
    </span>
  );

  const renderBuildDot = (s: SegmentRow) => {
    const st = effectiveBuildStatus(s);
    if (st === 'running') return <FontAwesomeIcon icon={faSpinner} spin style={{ color: '#818cf8', fontSize: 11 }} />;
    return (
      <span
        style={{
          display: 'inline-block',
          width: 8,
          height: 8,
          borderRadius: '50%',
          backgroundColor: (st && BUILD_STATUS_DOT[st]) || 'rgba(156,163,175,0.5)',
          boxShadow: st === 'blocked_delta' ? '0 0 6px rgba(245,158,11,0.7)' : undefined,
        }}
      />
    );
  };

  const chipStyle = (selected: boolean): React.CSSProperties => ({
    padding: '4px 12px',
    borderRadius: 14,
    fontSize: '0.72rem',
    fontWeight: 600,
    cursor: 'pointer',
    whiteSpace: 'nowrap',
    background: selected ? alpha(colors.indigo500, '33') : alpha(colors.indigo500, '14'),
    color: selected ? colors.indigo200 : colors.textMuted,
    border: `1px solid ${selected ? colors.panelBorderStrong : colors.panelBorder}`,
  });

  // Inline indigo replacements for the old .action-btn / input / select CSS.
  const actionBtnStyle: React.CSSProperties = {
    background: alpha(colors.indigo500, '14'),
    border: `1px solid ${colors.panelBorder}`,
    color: colors.indigo200,
    borderRadius: 6,
    width: 30,
    height: 30,
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    fontSize: 12,
  };
  const viewToggleStyle = (active: boolean): React.CSSProperties => ({
    display: 'inline-flex',
    alignItems: 'center',
    gap: 7,
    padding: '7px 16px',
    borderRadius: 8,
    border: 'none',
    cursor: 'pointer',
    fontSize: 12,
    fontWeight: 700,
    letterSpacing: 0.4,
    background: active ? 'linear-gradient(135deg,#6366f1 0%,#818cf8 100%)' : 'transparent',
    color: active ? '#fff' : colors.textMuted,
  });

  // ============================================================================
  // RENDER
  // ============================================================================

  return (
    <div className={`segments-manager ${animateIn ? 'animate-in' : ''}`} style={{ color: colors.text }}>
      <PortalKeyframes />

      {/* 1 — Header */}
      <header style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 16, flexWrap: 'wrap', marginBottom: 16 }}>
        <div>
          <h1 style={{ margin: 0, fontSize: 22, color: colors.heading, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faCrosshairs} style={{ color: colors.indigo400 }} />
            Segments
          </h1>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>
            {operatedCount.toLocaleString()} operated · {machineCount.toLocaleString()} auto-generated · v{SEGMENTS_PAGE_VERSION}
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
          <button
            type="button"
            style={btnStyle}
            onClick={() => onNavigate('create-segment')}
            title="Open the dynamic condition builder (funnel / suppression segments)"
          >
            <FontAwesomeIcon icon={faPlus} style={{ marginRight: 6 }} /> New Dynamic Segment
          </button>
          <button
            type="button"
            style={{ ...btnStyle, background: alpha(colors.indigo500, '33'), color: colors.indigo200 }}
            onClick={() => { setReqError(null); setShowRequestModal(true); }}
            title="Request engagement segments from analytics (open/click × windows × scope)"
          >
            <FontAwesomeIcon icon={faRocket} style={{ marginRight: 6 }} /> Request Segment
          </button>
        </div>
      </header>

      {/* View toggle: brand growth board (default) vs the full catalog */}
      <div style={{ display: 'inline-flex', background: colors.panelBg, border: `1px solid ${colors.panelBorder}`, borderRadius: 10, padding: 4, marginBottom: 16, gap: 4 }}>
        <button type="button" style={viewToggleStyle(view === 'growth')} onClick={() => setView('growth')}>
          <FontAwesomeIcon icon={faChartLine} /> Growth
        </button>
        <button type="button" style={viewToggleStyle(view === 'all')} onClick={() => setView('all')}>
          <FontAwesomeIcon icon={faTable} /> All Segments
        </button>
      </div>

      {view === 'growth' ? (
        <GrowthBoard
          orgFetchRef={orgFetchRef}
          onViewAll={() => setView('all')}
          onRequestSegment={() => { setReqError(null); setShowRequestModal(true); }}
        />
      ) : (
        <>
          {/* Toolbar: search + status + refresh */}
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', marginBottom: 12 }}>
            <div style={{ position: 'relative', flex: 1, minWidth: 220 }}>
              <FontAwesomeIcon icon={faSearch} style={{ position: 'absolute', left: 12, top: '50%', transform: 'translateY(-50%)', color: colors.textFaint, fontSize: 12 }} />
              <input
                type="text"
                placeholder="Find a segment by name…"
                value={searchInput}
                onChange={e => setSearchInput(e.target.value)}
                style={{
                  width: '100%',
                  background: colors.panelBg,
                  border: `1px solid ${colors.panelBorder}`,
                  borderRadius: 8,
                  color: colors.text,
                  padding: '8px 12px 8px 32px',
                  fontSize: 13,
                  outline: 'none',
                }}
              />
            </div>
            <select
              value={status}
              onChange={e => setStatus(e.target.value as StatusFilter)}
              title="Status filter (archived hidden by default)"
              style={{
                background: colors.panelBg,
                border: `1px solid ${colors.panelBorder}`,
                borderRadius: 8,
                color: colors.text,
                padding: '8px 12px',
                fontSize: 13,
                cursor: 'pointer',
              }}
            >
              <option value="active">Active</option>
              <option value="archived">Archived</option>
              <option value="all">All Statuses</option>
            </select>
            <button
              type="button"
              style={btnStyle}
              onClick={() => fetchListRef.current(0, false)}
              disabled={loading}
              title="Reload the segment list"
            >
              <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} style={{ marginRight: 6 }} /> Refresh
            </button>
          </div>

          {/* Category chips */}
          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', alignItems: 'center', marginBottom: 14 }}>
            <button type="button" style={chipStyle(category === 'all')} onClick={() => setCategory('all')}>
              All ({operatedCount.toLocaleString()})
            </button>
            {chipCategories.map(([cat, n]) => (
              <button
                key={cat}
                type="button"
                style={chipStyle(category === cat)}
                title={categoryTitle(cat)}
                onClick={() => setCategory(cat)}
              >
                {categoryLabel(cat)} ({n.toLocaleString()})
              </button>
            ))}
            {machineCount > 0 && (
              <button
                type="button"
                style={{ ...chipStyle(category === MACHINE_CATEGORY), opacity: category === MACHINE_CATEGORY ? 1 : 0.65, marginLeft: 'auto' }}
                title="Auto-generated segments from promotional waves. Hidden by default."
                onClick={() => setCategory(MACHINE_CATEGORY)}
              >
                Auto-generated ({machineCount.toLocaleString()})
              </button>
            )}
          </div>

          {/* Table */}
          {listError ? (
            <Panel>
              <EmptyState icon={faExclamationTriangle} title="Couldn’t load segments" hint={listError} />
              <div style={{ display: 'flex', justifyContent: 'center' }}>
                <button type="button" style={{ ...btnStyle, background: alpha(colors.indigo500, '33') }} onClick={() => fetchListRef.current(0, false)}>Retry</button>
              </div>
            </Panel>
          ) : loading ? (
            <div style={{ textAlign: 'center', padding: 60, color: colors.textMuted }}>
              <FontAwesomeIcon icon={faSpinner} spin /> Loading segments…
            </div>
          ) : sortedRows.length === 0 ? (
            <Panel>
              <EmptyState
                icon={faCrosshairs}
                title="No segments found"
                hint={q || category !== 'all' || status !== 'active' ? 'Try adjusting the search, status or category filters.' : 'Request a recency segment or create a dynamic one to get started.'}
              />
            </Panel>
          ) : (
            <Panel style={{ padding: 0, overflowX: 'auto' }}>
              <table style={{ ...tableStyle, tableLayout: 'fixed' }}>
                <thead>
                  <tr>
                    <th style={{ ...thStyle, width: '38%', cursor: 'pointer', userSelect: 'none' }} onClick={() => toggleSort('name')}>
                      Name <FontAwesomeIcon icon={sortIcon('name')} style={{ opacity: sortKey === 'name' ? 0.9 : 0.35 }} />
                    </th>
                    <th style={{ ...thStyle, width: '12%' }}>Category</th>
                    <th style={{ ...thStyle, width: '8%' }}>Type</th>
                    <th style={{ ...numTh, width: '11%', cursor: 'pointer', userSelect: 'none' }} onClick={() => toggleSort('members')}>
                      Members <FontAwesomeIcon icon={sortIcon('members')} style={{ opacity: sortKey === 'members' ? 0.9 : 0.35 }} />
                    </th>
                    <th style={{ ...thStyle, width: '14%', cursor: 'pointer', userSelect: 'none' }} onClick={() => toggleSort('built')}>
                      Built <FontAwesomeIcon icon={sortIcon('built')} style={{ opacity: sortKey === 'built' ? 0.9 : 0.35 }} />
                    </th>
                    <th style={{ ...thStyle, width: '17%' }}>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {sortedRows.map(segment => {
                    const st = effectiveBuildStatus(segment);
                    const busy = refreshingId === segment.id || st === 'running';
                    const archived = segment.status === 'archived';
                    return (
                      <tr key={segment.id}>
                        <td style={{ ...tdStyle, opacity: archived ? 0.45 : 1 }}>
                          <span
                            title={segment.description || segment.name}
                            style={{ display: 'block', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', fontWeight: 500, color: colors.heading }}
                          >
                            {segment.name}
                            {archived && <span style={{ marginLeft: 6, fontSize: '0.6rem', textTransform: 'uppercase', color: colors.textFaint }}>archived</span>}
                          </span>
                        </td>
                        <td style={tdStyle}>{renderCategoryBadge(segment.category)}</td>
                        <td style={tdStyle}>{renderTypeBadge(segment.segment_type)}</td>
                        <td
                          style={{ ...numTd, fontFamily: 'monospace', fontSize: 13 }}
                          title={segment.audience_source === 'materialized'
                            ? 'Current audience size — the count emails are sent to'
                            : 'Cached estimate — Refresh for the latest count'}
                        >
                          {memberCount(segment).toLocaleString()}
                        </td>
                        <td style={tdStyle} title={builtTooltip(segment)}>
                          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 7, fontSize: 13, color: st === 'failed' ? colors.danger : st === 'blocked_delta' ? colors.warning : colors.text }}>
                            {renderBuildDot(segment)}
                            {st === 'running' ? 'building…' : fmtRel(builtIso(segment))}
                          </span>
                        </td>
                        <td style={tdStyle}>
                          <div style={{ display: 'flex', gap: 6 }}>
                            <button
                              type="button"
                              style={copiedId === segment.id ? { ...actionBtnStyle, color: colors.success, background: alpha(colors.success, '14') } : actionBtnStyle}
                              onClick={() => copyId(segment.id)}
                              title={copiedId === segment.id ? 'Copied!' : `Copy segment ID (${segment.id})`}
                            >
                              <FontAwesomeIcon icon={copiedId === segment.id ? faCheck : faCopy} />
                            </button>
                            <button
                              type="button"
                              style={st === 'blocked_delta' ? { ...actionBtnStyle, color: colors.warning } : actionBtnStyle}
                              onClick={() => handleRefresh(segment)}
                              disabled={busy}
                              title={st === 'running'
                                ? 'A build is running for this segment'
                                : st === 'blocked_delta'
                                  ? 'Blocked: count changed >50% — Force refresh (confirms first)'
                                  : isLakeRow(segment)
                                    ? 'Refresh — rebuilds from analytics (may take a moment)'
                                    : 'Refresh — updates the audience count instantly'}
                            >
                              <FontAwesomeIcon icon={busy ? faSpinner : faSyncAlt} spin={busy} />
                            </button>
                            <button
                              type="button"
                              style={actionBtnStyle}
                              onClick={() => setDrawerId(segment.id)}
                              title="Details — metadata, build history, member sample, export"
                            >
                              <FontAwesomeIcon icon={faEye} />
                            </button>
                            {segment.segment_type === 'dynamic' && (
                              <button
                                type="button"
                                style={actionBtnStyle}
                                onClick={() => onNavigate('edit-segment', undefined, segment)}
                                title="Edit conditions (dynamic segment)"
                              >
                                <FontAwesomeIcon icon={faPencilAlt} />
                              </button>
                            )}
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>

              {/* Load more (server paging; matters for the Machine chip) */}
              {rows.length < total && (
                <div style={{ display: 'flex', justifyContent: 'center', padding: 14 }}>
                  <button
                    type="button"
                    style={btnStyle}
                    disabled={loadingMore}
                    onClick={() => fetchListRef.current(rows.length, true)}
                  >
                    <FontAwesomeIcon icon={loadingMore ? faSpinner : faPlus} spin={loadingMore} style={{ marginRight: 6 }} />
                    {loadingMore ? 'Loading…' : `Load more (${rows.length.toLocaleString()} of ${total.toLocaleString()})`}
                  </button>
                </div>
              )}
            </Panel>
          )}
        </>
      )}

      {/* 4 — Detail drawer. Rendered through a PORTAL: the portal's view
          containers animate with transform/opacity (.animate-in), which turns
          them into CSS containing blocks — position:fixed children get clipped
          to the card area and vanish on poll re-renders. document.body is
          immune to all of that. */}
      {drawerRow && createPortal(
        <>
          <div
            onClick={() => setDrawerId(null)}
            style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.45)', zIndex: 1000 }}
          />
          <aside
            style={{
              position: 'fixed',
              top: 0,
              right: 0,
              bottom: 0,
              width: 480,
              maxWidth: '94vw',
              background: '#0d1526',
              borderLeft: '1px solid rgba(0,200,255,0.15)',
              boxShadow: '-12px 0 40px rgba(0,0,0,0.5)',
              zIndex: 1001,
              display: 'flex',
              flexDirection: 'column',
            }}
          >
            {/* drawer header */}
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12, padding: '18px 20px', borderBottom: '1px solid rgba(0,200,255,0.1)' }}>
              <div style={{ minWidth: 0 }}>
                <h3 style={{ margin: 0, fontSize: 16, color: '#e0e6f0', wordBreak: 'break-word' }}>{drawerRow.name}</h3>
                <div style={{ display: 'flex', gap: 6, marginTop: 8, flexWrap: 'wrap', alignItems: 'center' }}>
                  {renderCategoryBadge(drawerRow.category)}
                  {renderTypeBadge(drawerRow.segment_type)}
                  <span className={`status-badge status-${drawerRow.status}`}>{drawerRow.status}</span>
                </div>
              </div>
              <button className="action-btn" onClick={() => setDrawerId(null)} title="Close (Esc)">
                <FontAwesomeIcon icon={faTimes} />
              </button>
            </div>

            {/* drawer body */}
            <div style={{ flex: 1, overflowY: 'auto', padding: '16px 20px', display: 'flex', flexDirection: 'column', gap: 18 }}>
              {/* metadata grid */}
              <div>
                <h4 style={{ margin: '0 0 8px', fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.6, color: 'rgba(180,210,240,0.65)' }}>Metadata</h4>
                <div style={{ display: 'grid', gridTemplateColumns: '110px 1fr', rowGap: 8, columnGap: 10, fontSize: 13, color: '#e0e6f0' }}>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>ID</span>
                  <span style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                    <code style={{ fontSize: 11.5, overflow: 'hidden', textOverflow: 'ellipsis' }}>{drawerRow.id}</code>
                    <button
                      className="action-btn"
                      style={{ width: 24, height: 24, flexShrink: 0, ...(copiedId === drawerRow.id ? { color: '#10b981' } : {}) }}
                      onClick={() => copyId(drawerRow.id)}
                      title="Copy ID"
                    >
                      <FontAwesomeIcon icon={copiedId === drawerRow.id ? faCheck : faCopy} style={{ fontSize: 11 }} />
                    </button>
                  </span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Category</span>
                  <span>{drawerRow.category || 'uncategorized'}</span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Type</span>
                  <span>{drawerRow.segment_type}</span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Status</span>
                  <span>{drawerRow.status}</span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Created</span>
                  <span>{fmtAbs(drawerRow.created_at)}</span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Updated</span>
                  <span>{fmtAbs(drawerRow.updated_at)}</span>
                  {drawerRow.description && (
                    <>
                      <span style={{ color: 'rgba(180,210,240,0.65)' }}>Description</span>
                      <span style={{ whiteSpace: 'pre-wrap' }}>{drawerRow.description}</span>
                    </>
                  )}
                </div>
              </div>

              {/* build block */}
              <div>
                <h4 style={{ margin: '0 0 8px', fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.6, color: 'rgba(180,210,240,0.65)' }}>Last Build</h4>
                <div style={{ display: 'grid', gridTemplateColumns: '110px 1fr', rowGap: 8, columnGap: 10, fontSize: 13, color: '#e0e6f0' }}>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Built</span>
                  <span>{drawerRow.last_built_at ? `${fmtAbs(drawerRow.last_built_at)} (${fmtRel(drawerRow.last_built_at)})` : 'never built via ledger'}</span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Source</span>
                  <span>{drawerRow.build_source || '—'}</span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Status</span>
                  <span style={{ display: 'inline-flex', alignItems: 'center', gap: 7 }}>
                    {renderBuildDot(drawerRow)}
                    {effectiveBuildStatus(drawerRow) || 'no build recorded'}
                  </span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Duration</span>
                  <span>{typeof drawerRow.last_build_ms === 'number' ? `${drawerRow.last_build_ms.toLocaleString()} ms` : '—'}</span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Delta</span>
                  <span>{typeof drawerRow.last_delta_pct === 'number' ? `${drawerRow.last_delta_pct > 0 ? '+' : ''}${drawerRow.last_delta_pct.toFixed(1)}% vs previous build` : '—'}</span>
                  <span style={{ color: 'rgba(180,210,240,0.65)' }}>Members</span>
                  <span style={{ fontVariantNumeric: 'tabular-nums' }}>
                    {memberCount(drawerRow).toLocaleString()}
                    <span style={{ marginLeft: 6, fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>({drawerRow.audience_source || 'cached'})</span>
                  </span>
                </div>
              </div>

              {/* member sample */}
              <div>
                <h4 style={{ margin: '0 0 8px', fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.6, color: 'rgba(180,210,240,0.65)' }}>
                  Member Sample
                  {!sample.loading && !sample.error && (
                    <span style={{ marginLeft: 8, textTransform: 'none', letterSpacing: 0, fontWeight: 400 }}>
                      showing {sample.emails.length.toLocaleString()} of {memberCount(drawerRow).toLocaleString()}
                    </span>
                  )}
                </h4>
                {sample.loading ? (
                  <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>
                    <FontAwesomeIcon icon={faSpinner} spin /> loading sample…
                  </div>
                ) : sample.error ? (
                  <div style={{ fontSize: 13, color: '#f59e0b' }}>{sample.error}</div>
                ) : sample.emails.length === 0 ? (
                  <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>
                    no audience members yet — Refresh the segment, then re-open
                  </div>
                ) : (
                  <div style={{ background: '#0a1020', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: '8px 12px', maxHeight: 220, overflowY: 'auto' }}>
                    {sample.emails.map(email => (
                      <div key={email} style={{ fontFamily: 'monospace', fontSize: 12, lineHeight: '20px', color: '#cbd5e1', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                        {email}
                      </div>
                    ))}
                  </div>
                )}
              </div>
            </div>

            {/* drawer footer */}
            <div style={{ display: 'flex', gap: 10, padding: '14px 20px', borderTop: '1px solid rgba(0,200,255,0.1)', flexWrap: 'wrap' }}>
              <a
                className="btn btn-secondary"
                style={{ textDecoration: 'none' }}
                href={`/api/mailing/v2/segments/${drawerRow.id}/members.csv`}
                download
                title="Export all audience members as CSV"
              >
                <FontAwesomeIcon icon={faDownload} /> Export CSV
              </a>
              {drawerRow.segment_type === 'dynamic' && (
                <button className="btn btn-secondary" onClick={() => onNavigate('edit-segment', undefined, drawerRow)}>
                  <FontAwesomeIcon icon={faPencilAlt} /> Edit
                </button>
              )}
              {drawerRow.category !== MACHINE_CATEGORY && drawerRow.status !== 'archived' && (
                <button
                  className="btn btn-secondary"
                  style={{ marginLeft: 'auto', color: '#fda4af', borderColor: 'rgba(251,113,133,0.4)' }}
                  disabled={archivingId === drawerRow.id}
                  onClick={() => handleArchive(drawerRow)}
                >
                  <FontAwesomeIcon icon={archivingId === drawerRow.id ? faSpinner : faBoxArchive} spin={archivingId === drawerRow.id} /> Archive
                </button>
              )}
            </div>
          </aside>
        </>,
        document.body,
      )}

      {/* 5 — Request Segment modal (POST /v2/segments/build-request).
          Portaled to document.body (see drawer note); the .list-portal wrapper
          keeps the scoped .modal-overlay/.modal-content CSS matching. */}
      {showRequestModal && createPortal(
        <div className="list-portal">
        <div className="modal-overlay" onClick={() => !reqSubmitting && setShowRequestModal(false)}>
          <div className="modal-content" onClick={e => e.stopPropagation()}>
            <div className="modal-header">
              <h3><FontAwesomeIcon icon={faRocket} /> Request Segment</h3>
              <button className="modal-close" onClick={() => setShowRequestModal(false)}>
                <FontAwesomeIcon icon={faTimes} />
              </button>
            </div>
            <form onSubmit={submitBuildRequest}>
              <div className="form-group">
                <label>Event *</label>
                <div style={{ display: 'flex', gap: 18 }}>
                  {(['open', 'click'] as const).map(ev => (
                    <label key={ev} style={{ display: 'flex', alignItems: 'center', gap: 6, fontWeight: 400, cursor: 'pointer' }}>
                      <input
                        type="radio"
                        name="segreq-event"
                        checked={reqEvent === ev}
                        onChange={() => setReqEvent(ev)}
                      />
                      {ev === 'open' ? 'Open' : 'Click'}
                    </label>
                  ))}
                </div>
              </div>

              <div className="form-group">
                <label>Windows (days) * — one segment per window, max {BUILD_REQUEST_MAX_WINDOWS}</label>
                <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', alignItems: 'center' }}>
                  {reqChipWindows.map(w => {
                    const selected = reqWindows.includes(w);
                    return (
                      <button
                        key={w}
                        type="button"
                        onClick={() => toggleReqWindow(w)}
                        style={{
                          padding: '4px 12px',
                          borderRadius: 14,
                          fontSize: '0.75rem',
                          fontWeight: 600,
                          cursor: 'pointer',
                          background: selected ? 'rgba(99,102,241,0.35)' : 'rgba(75,85,99,0.25)',
                          color: selected ? '#c7d2fe' : '#9ca3af',
                          border: selected ? '1px solid rgba(129,140,248,0.7)' : '1px solid rgba(156,163,175,0.3)',
                        }}
                      >
                        {w}d{selected ? ' ✓' : ''}
                      </button>
                    );
                  })}
                  <input
                    type="number"
                    min={1}
                    max={120}
                    placeholder="custom"
                    value={reqCustomWindow}
                    onChange={e => setReqCustomWindow(e.target.value)}
                    onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addCustomReqWindow(); } }}
                    style={{ width: 90 }}
                  />
                  <button type="button" className="btn btn-secondary btn-small" onClick={addCustomReqWindow}>
                    <FontAwesomeIcon icon={faPlus} /> Add
                  </button>
                </div>
              </div>

              <div className="form-group">
                <label>Scope *</label>
                <select value={reqScope} onChange={e => setReqScope(e.target.value as 'global' | 'brand' | 'isp')}>
                  <option value="global">Global (all brands, all providers)</option>
                  <option value="brand">Brand (single sending brand)</option>
                  <option value="isp">Mailbox provider (single recipient provider)</option>
                </select>
              </div>
              {reqScope === 'brand' && (
                <div className="form-group">
                  <label>Brand apex *</label>
                  <input
                    type="text"
                    value={reqBrandApex}
                    onChange={e => setReqBrandApex(e.target.value)}
                    placeholder="e.g. discountblog.com"
                  />
                </div>
              )}
              {reqScope === 'isp' && (
                <div className="form-group">
                  <label>Mailbox provider *</label>
                  <input
                    type="text"
                    value={reqIsp}
                    onChange={e => setReqIsp(e.target.value)}
                    placeholder="gmail, yahoo, microsoft…"
                    list="segreq-isp-options"
                  />
                  <datalist id="segreq-isp-options">
                    {BUILD_REQUEST_KNOWN_ISPS.map(isp => <option key={isp} value={isp} />)}
                  </datalist>
                </div>
              )}

              <div className="form-group">
                <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer' }}>
                  <input
                    type="checkbox"
                    checked={reqExcludeSeeds}
                    onChange={e => setReqExcludeSeeds(e.target.checked)}
                    style={{ width: 'auto' }}
                  />
                  Exclude seed accounts (recommended)
                </label>
              </div>

              <div className="form-group">
                <label>Name (optional — server generates one if blank)</label>
                <input
                  type="text"
                  value={reqName}
                  onChange={e => setReqName(e.target.value)}
                  placeholder="e.g. Gmail-Openers"
                />
              </div>

              {reqError && (
                <div style={{
                  margin: '4px 0 8px',
                  padding: '8px 12px',
                  borderRadius: 8,
                  background: 'rgba(239,68,68,0.12)',
                  border: '1px solid rgba(239,68,68,0.4)',
                  color: '#fca5a5',
                  fontSize: '0.8rem',
                }}>
                  {reqError}
                </div>
              )}

              <div className="modal-actions">
                <button type="button" className="btn btn-secondary" onClick={() => setShowRequestModal(false)} disabled={reqSubmitting}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary" disabled={reqSubmitting || reqWindows.length === 0}>
                  <FontAwesomeIcon icon={reqSubmitting ? faSpinner : faRocket} spin={reqSubmitting} />
                  {reqSubmitting ? 'Requesting…' : `Request ${reqWindows.length || ''} Segment${reqWindows.length === 1 ? '' : 's'}`}
                </button>
              </div>
            </form>
          </div>
        </div>
        </div>,
        document.body,
      )}

      {/* 7 — Minimal local toast stack (portaled — see drawer note) */}
      {toasts.length > 0 && createPortal(
        <div style={{ position: 'fixed', bottom: 24, right: 24, display: 'flex', flexDirection: 'column', gap: 8, zIndex: 1100 }}>
          {toasts.map(t => (
            <div
              key={t.id}
              style={{
                padding: '10px 14px',
                borderRadius: 10,
                fontSize: '0.8rem',
                fontWeight: 600,
                color: '#fff',
                maxWidth: 380,
                boxShadow: '0 8px 24px rgba(0,0,0,0.45)',
                background: t.kind === 'ok'
                  ? 'linear-gradient(135deg, #059669, #10b981)'
                  : t.kind === 'error'
                    ? 'linear-gradient(135deg, #dc2626, #ef4444)'
                    : 'linear-gradient(135deg, #4f46e5, #6366f1)',
              }}
            >
              {t.msg}
            </div>
          ))}
        </div>,
        document.body,
      )}
    </div>
  );
};
