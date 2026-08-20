// DripJourneyCanvas — the drip journey a sending domain is enrolled in, drawn
// as a canvas, with the levers that change how fast it flows.
//
// Operator brief (2026-08-19): "I click the ledger tab and select the domain
// discountblog and see the drip associated as well as reporting I can review in
// order to set the quotas of that drip … The subsequent touches are presented
// poorly. I want to see the duration in which another touch would occur and
// what that touch is going to be from a content perspective. I want canvas
// style where you see the rectangle that calls out the touch, then a
// conditional shape for delay, then another rectangle etc, all connected by a
// line, and that line should have numbers calling out how many audience members
// are awaiting the next touch."
//
// Screen order follows the standing display doctrine — IN MOTION first
// (journey canvas: who is mid-ladder right now), then PLAN-AHEAD (the per-ISP
// caps that decide tomorrow's flow), then HISTORY (the 7/14/30-day scoreboard).
// Roster membership (which domains ride this drip) sits last because it is a
// structural edit, not a daily one.
//
// Honesty rules this screen holds to (PORTAL_DESIGN_SYSTEM §1.6, METRIC_CONTRACT):
//   - Four distinct displays: loading / error+Retry / empty / data. A failed
//     fetch NEVER renders as "no data".
//   - An unknown is "—", never 0. A missing edge is "not reported", not "0
//     waiting"; a reported 0 is drawn as a thin line labelled "0 waiting".
//   - Every rate names its denominator in its label, and each server-sent rate
//     is re-derived client-side from the counts shown; a disagreement is
//     flagged with * rather than silently displayed.
//   - Write surfaces render READ-ONLY (never hidden, never faked) when the
//     server reports write_enabled=false, and name the env var it reported.
//
// Wiring note: this file is a leaf component only — the tab wiring
// (TabId union → tabs array → renderContent() case) is the tech lead's touch.

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faDiagramProject, faSlidersH, faChartLine, faSitemap, faHourglassHalf,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, pageStyle, panelTitleStyle, thStyle, tdStyle, numTd, numTh,
  tableStyle, btnStyle, cardGrid, stateColor,
} from '../shared/theme';
import {
  Panel, SectionHeader, Stat, SectionError, EmptyState, Pill, PortalKeyframes,
} from '../shared/ui';
import { FilterChip, daysAgoDenver, denverToday } from '../shared/filters';
import { useToast } from '../shared/ToastSystem';
import {
  buildThrottleDiff, buildThrottlePayload, validateThrottleRows, type ThrottleRow,
} from './throttleDiff';

const PAGE_VERSION = 'drip-journey-canvas v1 — 2026-08-19';

// ── API shapes ──────────────────────────────────────────────────────────────
// Property Ledger read (existing, property_ledger.go) — used ONLY to enumerate
// the drip roster's brand ⇄ sending-domain pairs for the selector.

interface LedgerRowLite {
  brand: string;
  sending_domain: string;
}
interface LedgerResponseLite {
  rows: LedgerRowLite[];
}

// Roster (…/property-ledger/roster) — which verticals (drips) a brand rides.
interface RosterRow {
  vertical: string;
  brand: string;
  weight: number;
  active: boolean;
  sort_order: number;
  updated_at?: string;
  updated_by?: string;
}
interface RosterResponse {
  rows: RosterRow[];
  write_enabled: boolean;
  write_flag_env?: string;
}

// Journey (…/property-ledger/journey) — the ladder itself.
interface JourneyTouch {
  touch: number;
  subject_line: string;
  preheader: string;
  from_name: string;
  creative_filename: string;
  offer_id?: string;
  active: boolean;
  configured: boolean;
}
interface JourneyEdgeISP {
  isp: string;
  waiting: number;
}
interface JourneyEdge {
  from_touch: number;
  to_touch: number;
  waiting: number;
  soonest?: string | null;
  latest?: string | null;
  by_isp?: JourneyEdgeISP[];
}
interface JourneyResponse {
  brand: string;
  sending_domain: string;
  vertical: string;
  delay_hours: number;
  max_touches: number;
  touches: JourneyTouch[];
  edges: JourneyEdge[];
  totals: { in_flight?: number | null; due_now?: number | null };
  generated_at?: string;
}

// Stats (…/property-ledger/stats) — lane × ISP × day scoreboard.
interface StatsRow {
  day: string;
  isp: string;
  sent: number;
  delivered_pg: number;
  opens: number;
  clicks: number;
  open_rate: number;
  click_rate: number;
}
interface StatsResponse {
  vertical: string;
  days: number;
  rows: StatsRow[];
}

// Throttle (…/property-ledger/throttle?domain=…) — VERIFIED against
// internal/api/property_lane_supply.go HandleLaneThrottle (the query param is
// `domain`, NOT dataset_id; the response is per-BRAND with a feeds[] array,
// each feed carrying its own vertical).
interface ThrottleOverride {
  isp: string;
  pct_override: number;
  max_per_wave: number;
  daily_cap?: number | null;
  updated_at: string;
  updated_by?: string;
}
interface ThrottleFeed {
  dataset_id: string;
  name: string;
  vertical: string;
  status: string;
  paused_emergency: boolean;
  supply_release_daily_cap: number;
  shared_brands: string[];
  overrides: ThrottleOverride[];
  default_isps: string[];
}
interface ThrottleResponse {
  domain: string;
  brand: string;
  sending_domain: string;
  as_of: string;
  write_enabled: boolean;
  write_flag_env: string;
  write_endpoint: string;
  replacement_note: string;
  enforcement_note: string;
  cap_systems_note: string;
  feeds: ThrottleFeed[];
}

// ── Formatting helpers — an unknown is NEVER 0 ──────────────────────────────

const UNKNOWN = '—';

const num = (n: number | null | undefined): string =>
  typeof n === 'number' && Number.isFinite(n) ? Math.round(n).toLocaleString() : UNKNOWN;

const ratePct = (r: number | null | undefined): string =>
  typeof r === 'number' && Number.isFinite(r) ? `${(r * 100).toFixed(2)}%` : UNKNOWN;

const derive = (n: number, d: number): number | null =>
  Number.isFinite(n) && Number.isFinite(d) && d > 0 ? n / d : null;

const shortTime = (iso: string | null | undefined): string => {
  if (!iso) return UNKNOWN;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return UNKNOWN;
  return d.toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  });
};

const durationUntil = (iso: string | null | undefined): string => {
  if (!iso) return UNKNOWN;
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return UNKNOWN;
  const mins = Math.round((t - Date.now()) / 60000);
  if (mins <= 0) return 'due now';
  if (mins < 60) return `in ${mins}m`;
  const h = Math.floor(mins / 60);
  if (h < 48) return `in ${h}h ${mins % 60}m`;
  return `in ${Math.floor(h / 24)}d ${h % 24}h`;
};

// ── Fetch plumbing ──────────────────────────────────────────────────────────

async function getJSON<T>(url: string, signal: AbortSignal): Promise<T> {
  const r = await apiFetch(url, { signal });
  if (!r.ok) {
    let msg = `HTTP ${r.status}`;
    try {
      const j = (await r.json()) as { error?: unknown };
      if (j && typeof j.error === 'string') msg = `HTTP ${r.status}: ${j.error}`;
    } catch { /* non-JSON error body */ }
    throw new Error(msg);
  }
  return (await r.json()) as T;
}

interface Resource<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  reload: () => void;
}

/**
 * useResource — one fetch, four honest states. `key` doubles as the dependency
 * (null = the fetch is not applicable yet, e.g. no drip selected). Data is
 * cleared on a new key so a stale payload never sits under a new selection,
 * and an error NEVER leaves an empty-looking success behind it.
 */
function useResource<T>(
  key: string | null,
  fetcher: (signal: AbortSignal) => Promise<T>,
): Resource<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  useEffect(() => {
    if (key === null) {
      setData(null);
      setError(null);
      setLoading(false);
      return;
    }
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    setData(null);
    void (async () => {
      try {
        const v = await fetcherRef.current(ac.signal);
        if (ac.signal.aborted) return;
        setData(v);
      } catch (e) {
        if (ac.signal.aborted) return;
        setError(e instanceof Error ? e.message : 'load failed');
      } finally {
        if (!ac.signal.aborted) setLoading(false);
      }
    })();
    return () => ac.abort();
  }, [key, nonce]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);
  return { data, loading, error, reload };
}

// ── Small shared-state primitives ───────────────────────────────────────────

const LoadingRow: React.FC<{ label: string }> = ({ label }) => (
  <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '18px 4px', fontSize: 12, color: colors.textMuted }}>
    <span
      style={{
        width: 12, height: 12, borderRadius: 999,
        border: `2px solid ${alpha(colors.indigo500, '66')}`,
        borderTopColor: colors.indigo300,
        animation: 'djcSpin 800ms linear infinite',
        display: 'inline-block',
      }}
    />
    Loading {label}…
  </div>
);

/**
 * AsyncPanel — the four-state renderer every panel on this screen goes through.
 * Order matters: error is checked BEFORE emptiness so a failed fetch can never
 * be mistaken for "nothing here" (the live bug class this screen must avoid).
 */
const AsyncPanel: React.FC<{
  label: string;
  res: { loading: boolean; error: string | null; reload: () => void };
  isEmpty: boolean;
  emptyTitle: string;
  emptyHint?: string;
  children: React.ReactNode;
}> = ({ label, res, isEmpty, emptyTitle, emptyHint, children }) => {
  if (res.loading) return <LoadingRow label={label} />;
  if (res.error) return <SectionError label={label} error={res.error} onRetry={res.reload} />;
  if (isEmpty) return <EmptyState title={emptyTitle} hint={emptyHint} />;
  return <>{children}</>;
};

const selectStyle: React.CSSProperties = {
  background: 'rgba(255,255,255,0.06)',
  color: colors.text,
  border: `1px solid ${colors.panelBorderStrong}`,
  borderRadius: 6,
  padding: '6px 10px',
  fontSize: 12,
};

const inputStyle: React.CSSProperties = {
  background: 'rgba(255,255,255,0.06)',
  color: colors.text,
  border: `1px solid ${colors.panelBorderStrong}`,
  borderRadius: 5,
  padding: '3px 6px',
  fontSize: 11,
  width: 84,
  fontVariantNumeric: 'tabular-nums',
};

const smallBtn: React.CSSProperties = {
  background: alpha(colors.indigo500, '22'),
  color: colors.indigo200,
  border: `1px solid ${alpha(colors.indigo500, '66')}`,
  borderRadius: 5,
  padding: '3px 9px',
  fontSize: 10,
  fontWeight: 700,
  cursor: 'pointer',
};

const dangerBtn: React.CSSProperties = {
  ...smallBtn,
  background: 'rgba(239,68,68,0.12)',
  color: colors.dangerText,
  border: '1px solid rgba(239,68,68,0.40)',
};

const noteStyle: React.CSSProperties = {
  fontSize: 11,
  color: colors.textMuted,
  lineHeight: 1.6,
  marginTop: 8,
};

/** Read-only banner naming the exact server env var the server reported. */
const ReadOnlyBanner: React.FC<{ envVar?: string; what: string; title?: string }> = ({ envVar, what, title }) => (
  <div
    title={title}
    style={{
      fontSize: 11, fontWeight: 600, color: colors.warningText,
      background: alpha(colors.warning, '14'),
      border: `1px solid ${alpha(colors.warning, '44')}`,
      borderRadius: 6, padding: '6px 10px', marginBottom: 10,
    }}
  >
    {what} is READ-ONLY — the server reports write_enabled=false
    {envVar ? <> (env <code style={{ fontFamily: 'monospace' }}>{envVar}</code> is not set)</> : ' and named no env flag'}.
    Values below are live server state; nothing on this panel will save.
  </div>
);

// ── The canvas ──────────────────────────────────────────────────────────────
// Layout mirrors JourneyBuilder.tsx: one absolutely-positioned SVG
// "connections layer" (lines + arrowheads) with an HTML node layer on top, so
// node text can truncate/wrap normally. Hand-rolled SVG, no new dependency.

const NODE_W = 244;
const NODE_H = 148;
const DIA = 92;           // diamond bounding box
const GAP = 30;
const PAD_X = 16;
const PAD_TOP = 34;
const STEP = NODE_W + GAP + DIA + GAP;

/** Line weight scales with the waiting population; 0 stays a hairline. */
const edgeWeight = (waiting: number | null): number => {
  if (waiting == null || !Number.isFinite(waiting) || waiting <= 0) return 1.25;
  return Math.max(1.5, Math.min(6, 1.5 + Math.log10(waiting) * 1.4));
};

const TouchNode: React.FC<{ touch: JourneyTouch; x: number; y: number }> = ({ touch, x, y }) => {
  const configured = touch.configured;
  const accent = !configured ? colors.textFaint : touch.active ? colors.indigo400 : colors.warning;
  return (
    <div
      style={{
        position: 'absolute', left: x, top: y, width: NODE_W, height: NODE_H,
        boxSizing: 'border-box',
        background: configured ? colors.panelBg : 'rgba(15,30,60,0.16)',
        border: configured
          ? `1px solid ${alpha(colors.indigo500, '44')}`
          : `1px dashed ${alpha(colors.textFaint, '66')}`,
        borderLeft: `4px solid ${accent}`,
        borderRadius: 10,
        padding: '9px 11px',
        opacity: configured ? 1 : 0.62,
        overflow: 'hidden',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 6 }}>
        <span style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.6, color: colors.heading }}>
          TOUCH {touch.touch}
        </span>
        {configured
          ? <Pill color={touch.active ? colors.success : colors.warning} style={{ fontSize: 9, padding: '1px 7px' }}>
              {touch.active ? 'active' : 'inactive'}
            </Pill>
          : <Pill color={colors.textFaint} style={{ fontSize: 9, padding: '1px 7px' }}>none</Pill>}
      </div>

      {!configured ? (
        <div style={{ marginTop: 10, fontSize: 11, color: colors.warningText, lineHeight: 1.5 }}>
          not configured — ladder retires here
          <div style={{ fontSize: 10, color: colors.textFaint, marginTop: 4 }}>
            Subscribers reaching this rung stop; no message is built for them.
          </div>
        </div>
      ) : (
        <>
          <div
            title={touch.subject_line || '(no subject on this touch)'}
            style={{
              marginTop: 6, fontSize: 12, color: colors.text, lineHeight: 1.35,
              display: '-webkit-box', WebkitLineClamp: 2, WebkitBoxOrient: 'vertical',
              overflow: 'hidden',
            }}
          >
            {touch.subject_line || <span style={{ color: colors.warningText }}>(no subject)</span>}
          </div>
          {touch.preheader && (
            <div
              title={touch.preheader}
              style={{ fontSize: 10, color: colors.textFaint, marginTop: 3, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}
            >
              {touch.preheader}
            </div>
          )}
          <div style={{ fontSize: 10, color: colors.textMuted, marginTop: 6 }}>
            from <b style={{ color: colors.indigo200 }}>{touch.from_name || UNKNOWN}</b>
          </div>
          <div
            title={`Creative: ${touch.creative_filename || 'none'}${touch.offer_id ? ` · offer ${touch.offer_id}` : ''}`}
            style={{
              fontSize: 10, fontFamily: 'monospace', color: colors.textFaint, marginTop: 3,
              whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
            }}
          >
            {touch.creative_filename || '(no creative bound)'}
          </div>
        </>
      )}
    </div>
  );
};

const DelayDiamond: React.FC<{ x: number; y: number; hours: number }> = ({ x, y, hours }) => (
  <div style={{ position: 'absolute', left: x, top: y, width: DIA, height: DIA }}>
    <div
      style={{
        position: 'absolute', inset: 10,
        background: alpha(colors.indigo500, '14'),
        border: `1px solid ${alpha(colors.indigo400, '66')}`,
        transform: 'rotate(45deg)',
        borderRadius: 6,
      }}
    />
    <div
      style={{
        position: 'absolute', inset: 0, display: 'flex', flexDirection: 'column',
        alignItems: 'center', justifyContent: 'center', pointerEvents: 'none',
      }}
    >
      <FontAwesomeIcon icon={faHourglassHalf} style={{ fontSize: 10, color: colors.indigo300 }} />
      <div style={{ fontSize: 12, fontWeight: 700, color: colors.indigo200, fontVariantNumeric: 'tabular-nums' }}>
        {Number.isFinite(hours) ? `${hours}h` : UNKNOWN}
      </div>
      <div style={{ fontSize: 8, color: colors.textFaint, letterSpacing: 0.4 }}>WAIT</div>
    </div>
  </div>
);

const JourneyCanvas: React.FC<{
  journey: JourneyResponse;
  openEdge: number | null;
  onToggleEdge: (fromTouch: number) => void;
}> = ({ journey, openEdge, onToggleEdge }) => {
  // The ladder is max_touches rungs long. A rung the server did not describe is
  // rendered as an explicit "not configured" node rather than being omitted —
  // the operator must see WHERE the ladder retires.
  const rungs: JourneyTouch[] = useMemo(() => {
    const byNum = new Map<number, JourneyTouch>();
    for (const t of journey.touches ?? []) byNum.set(t.touch, t);
    const max = Math.max(
      journey.max_touches || 0,
      ...(journey.touches ?? []).map((t) => t.touch),
      1,
    );
    const out: JourneyTouch[] = [];
    for (let i = 1; i <= max; i++) {
      out.push(
        byNum.get(i) ?? {
          touch: i, subject_line: '', preheader: '', from_name: '',
          creative_filename: '', active: false, configured: false,
        },
      );
    }
    return out;
  }, [journey]);

  const edgeFor = useCallback(
    (fromTouch: number): JourneyEdge | null =>
      (journey.edges ?? []).find((e) => e.from_touch === fromTouch) ?? null,
    [journey.edges],
  );

  const width = PAD_X * 2 + (rungs.length - 1) * STEP + NODE_W;
  const height = PAD_TOP + NODE_H + 46;
  const cy = PAD_TOP + NODE_H / 2;

  return (
    <div style={{ overflowX: 'auto', paddingBottom: 6 }}>
      <div style={{ position: 'relative', width, height, minWidth: '100%' }}>
        <svg
          width={width}
          height={height}
          style={{ position: 'absolute', left: 0, top: 0, pointerEvents: 'none' }}
        >
          <defs>
            <marker id="djcArrow" markerWidth="9" markerHeight="7" refX="8" refY="3.5" orient="auto">
              <polygon points="0 0, 9 3.5, 0 7" fill={alpha(colors.indigo400, '66')} />
            </marker>
          </defs>
          {rungs.slice(0, -1).map((t, i) => {
            const e = edgeFor(t.touch);
            const waiting = e ? e.waiting : null;
            const x0 = PAD_X + i * STEP + NODE_W;
            const dx = PAD_X + i * STEP + NODE_W + GAP;
            const x1 = dx + DIA;
            const x2 = PAD_X + (i + 1) * STEP;
            const w = edgeWeight(waiting);
            const stroke = waiting == null
              ? alpha(colors.textFaint, '66')
              : waiting > 0 ? colors.indigo400 : alpha(colors.indigo400, '44');
            return (
              <g key={t.touch}>
                <line
                  x1={x0} y1={cy} x2={dx} y2={cy}
                  stroke={stroke} strokeWidth={w}
                  strokeDasharray={waiting == null ? '4 4' : undefined}
                />
                <line
                  x1={x1} y1={cy} x2={x2} y2={cy}
                  stroke={stroke} strokeWidth={w}
                  strokeDasharray={waiting == null ? '4 4' : undefined}
                  markerEnd="url(#djcArrow)"
                />
              </g>
            );
          })}
        </svg>

        <div style={{ position: 'absolute', left: 0, top: 0, width, height }}>
          {rungs.map((t, i) => (
            <TouchNode key={t.touch} touch={t} x={PAD_X + i * STEP} y={PAD_TOP} />
          ))}
          {rungs.slice(0, -1).map((t, i) => (
            <DelayDiamond
              key={`d${t.touch}`}
              x={PAD_X + i * STEP + NODE_W + GAP}
              y={PAD_TOP + NODE_H / 2 - DIA / 2}
              hours={journey.delay_hours}
            />
          ))}
          {/* Waiting badges ride ON the line — the operator's headline ask. */}
          {rungs.slice(0, -1).map((t, i) => {
            const e = edgeFor(t.touch);
            const waiting = e ? e.waiting : null;
            const known = waiting != null && Number.isFinite(waiting);
            const mid = PAD_X + i * STEP + NODE_W + GAP / 2;
            const isOpen = openEdge === t.touch;
            return (
              <button
                key={`b${t.touch}`}
                type="button"
                onClick={() => onToggleEdge(t.touch)}
                title={
                  known
                    ? `${num(waiting)} subscriber(s) sitting between touch ${t.touch} and touch ${t.touch + 1}. Next due ${shortTime(e?.soonest)} (${durationUntil(e?.soonest)}). Click for the per-ISP split.`
                    : `No edge reported by the server for touch ${t.touch} → ${t.touch + 1}. This is an UNKNOWN, not a zero.`
                }
                style={{
                  position: 'absolute',
                  left: mid - 46,
                  top: PAD_TOP + NODE_H / 2 - 15,
                  width: 92,
                  cursor: 'pointer',
                  background: known ? colors.panelBgSolid : 'rgba(15,30,60,0.9)',
                  border: `1px solid ${isOpen ? colors.indigo400 : known && waiting > 0 ? alpha(colors.indigo400, '66') : alpha(colors.textFaint, '66')}`,
                  borderRadius: 8,
                  padding: '3px 4px',
                  color: colors.text,
                  textAlign: 'center',
                }}
              >
                <div style={{ fontSize: 13, fontWeight: 700, fontVariantNumeric: 'tabular-nums', color: known ? (waiting > 0 ? colors.indigo200 : colors.textMuted) : colors.textFaint }}>
                  {known ? num(waiting) : UNKNOWN}
                </div>
                <div style={{ fontSize: 8, color: colors.textFaint, letterSpacing: 0.4 }}>
                  {known ? 'WAITING' : 'NOT REPORTED'}
                </div>
              </button>
            );
          })}
          {/* Next-due caption under each connector. */}
          {rungs.slice(0, -1).map((t, i) => {
            const e = edgeFor(t.touch);
            if (!e) return null;
            return (
              <div
                key={`s${t.touch}`}
                style={{
                  position: 'absolute',
                  left: PAD_X + i * STEP + NODE_W,
                  top: PAD_TOP + NODE_H / 2 + 22,
                  width: GAP + DIA + GAP,
                  textAlign: 'center',
                  fontSize: 9,
                  color: colors.textFaint,
                  whiteSpace: 'nowrap',
                }}
                title={`soonest ${shortTime(e.soonest)} · latest ${shortTime(e.latest)}`}
              >
                next {durationUntil(e.soonest)}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
};

// ── Throttle editor (quota levers) ──────────────────────────────────────────
// Writes go through the ONE existing writer, PUT
// /api/mailing/data-partners/datasets/{id}/isp-distribution (delete-and-
// replace, audit-logged server-side). The diff/validation/payload logic is the
// shared pure module throttleDiff.ts — not re-derived here.

interface ThrottleEditRow { isp: string; pct: string; wave: string; day: string; }

const editRowsFromFeed = (feed: ThrottleFeed): ThrottleEditRow[] =>
  feed.overrides.map((ov) => ({
    isp: ov.isp,
    pct: String(ov.pct_override),
    wave: ov.max_per_wave > 0 ? String(ov.max_per_wave) : '',
    day: ov.daily_cap == null ? '' : String(ov.daily_cap),
  }));

const parseRows = (rows: ThrottleEditRow[]): ThrottleRow[] =>
  rows.map((r) => ({
    isp: r.isp,
    pct_override: r.pct.trim() === '' ? NaN : Number(r.pct),
    max_per_wave: r.wave.trim() === '' ? 0 : Number(r.wave),
    daily_cap: r.day.trim() === '' ? null : Number(r.day),
  }));

const currentRows = (feed: ThrottleFeed): ThrottleRow[] =>
  feed.overrides.map((ov) => ({
    isp: ov.isp,
    pct_override: ov.pct_override,
    max_per_wave: ov.max_per_wave,
    daily_cap: ov.daily_cap == null ? null : ov.daily_cap,
  }));

const ThrottleEditor: React.FC<{
  feed: ThrottleFeed;
  replacementNote: string;
  onClose: () => void;
  onSaved: () => void;
  onNotice: (s: string) => void;
}> = ({ feed, replacementNote, onClose, onSaved, onNotice }) => {
  const [rows, setRows] = useState<ThrottleEditRow[]>(() => editRowsFromFeed(feed));
  const [addISP, setAddISP] = useState('');
  const [busy, setBusy] = useState(false);

  const proposed = parseRows(rows);
  const errors = validateThrottleRows(proposed);
  const diff = errors.length === 0 ? buildThrottleDiff(currentRows(feed), proposed) : [];
  const changes = diff.filter((d) => d.kind !== 'unchanged');
  const availableISPs = feed.default_isps.filter((g) => !rows.some((r) => r.isp === g));
  const diffColor: Record<string, string> = {
    added: colors.success, removed: colors.danger, changed: colors.warning, unchanged: colors.textFaint,
  };

  const submit = async () => {
    if (errors.length > 0 || busy) return;
    const typed = window.prompt(
      `THROTTLE REPLACE changes LIVE claim routing for this feed on the next orchestrator wave.\n\n${replacementNote}\n\nType the feed name exactly to confirm:\n${feed.name}`,
    );
    if (typed === null) return;
    if (typed !== feed.name) {
      onNotice('Throttle write cancelled — feed name did not match.');
      return;
    }
    setBusy(true);
    try {
      const r = await apiFetch(
        `/api/mailing/data-partners/datasets/${feed.dataset_id}/isp-distribution`,
        { method: 'PUT', body: JSON.stringify(buildThrottlePayload(proposed)) },
      );
      let json: Record<string, unknown> = {};
      try { json = (await r.json()) as Record<string, unknown>; } catch { /* non-JSON error body */ }
      if (!r.ok) {
        onNotice(`Throttle write failed: ${String(json.error ?? r.status)}`);
        return;
      }
      onNotice(`Throttle replaced for "${feed.name}" — ${String(json.override_count ?? proposed.length)} override row(s) now live (next wave picks them up).`);
      onSaved();
    } catch (e) {
      onNotice(`Throttle write failed: ${e instanceof Error ? e.message : 'network error'}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div style={{ marginTop: 10, borderTop: `1px solid ${alpha(colors.warning, '44')}`, paddingTop: 8 }}>
      <div style={{ fontSize: 10, color: colors.warningText, fontWeight: 600, marginBottom: 6 }}>
        {replacementNote}
      </div>
      {rows.map((r, i) => (
        <div key={r.isp} style={{ display: 'flex', gap: 8, alignItems: 'center', padding: '2px 0', flexWrap: 'wrap' }}>
          <span style={{ fontSize: 11, color: colors.text, width: 84, fontWeight: 600 }}>{r.isp}</span>
          <label style={{ fontSize: 9, color: colors.textMuted }}>
            pct of wave (0–1){' '}
            <input
              style={inputStyle} value={r.pct} placeholder="0.4"
              onChange={(e) => setRows((s) => s.map((x, j) => (j === i ? { ...x, pct: e.target.value } : x)))}
            />
          </label>
          <label style={{ fontSize: 9, color: colors.textMuted }} title="Per-wave claim cap for this dataset's waves (replaces the global per-wave cap). Empty = global default.">
            max/wave (per wave){' '}
            <input
              style={inputStyle} value={r.wave} placeholder="default"
              onChange={(e) => setRows((s) => s.map((x, j) => (j === i ? { ...x, wave: e.target.value } : x)))}
            />
          </label>
          <label
            style={{ fontSize: 9, color: colors.textMuted }}
            title="Lane-owned per-ISP DAILY budget. Empty = keep this ISP's existing lane budget (the server preserves it). 0 = hard-suppress the ISP for this lane."
          >
            daily cap (per day, empty = keep){' '}
            <input
              style={inputStyle} value={r.day} placeholder="keep"
              onChange={(e) => setRows((s) => s.map((x, j) => (j === i ? { ...x, day: e.target.value } : x)))}
            />
          </label>
          <button
            type="button"
            style={dangerBtn}
            title="Removes this ISP's row entirely — it falls back to the global defaults and its lane daily budget is deleted with the row."
            onClick={() => setRows((s) => s.filter((_, j) => j !== i))}
          >
            Remove
          </button>
        </div>
      ))}
      <div style={{ display: 'flex', gap: 6, alignItems: 'center', marginTop: 4 }}>
        <select value={addISP} onChange={(e) => setAddISP(e.target.value)} style={{ ...inputStyle, width: 140 }}>
          <option value="">add ISP…</option>
          {availableISPs.map((g) => <option key={g} value={g}>{g}</option>)}
        </select>
        <button
          type="button" style={smallBtn} disabled={addISP === ''}
          onClick={() => {
            if (addISP === '') return;
            setRows((s) => [...s, { isp: addISP, pct: '0', wave: '', day: '' }]);
            setAddISP('');
          }}
        >
          Add override
        </button>
      </div>
      {errors.length > 0 && (
        <div style={{ marginTop: 6 }}>
          {errors.map((e, i) => (
            <div key={i} style={{ fontSize: 10, color: colors.dangerText, fontWeight: 600 }}>{e}</div>
          ))}
        </div>
      )}
      {errors.length === 0 && (
        <div style={{ marginTop: 6 }}>
          <div style={{ fontSize: 10, letterSpacing: 0.6, textTransform: 'uppercase', color: colors.textMuted }}>
            Change preview (current → proposed)
          </div>
          {changes.length === 0 && <div style={{ fontSize: 10, color: colors.textFaint }}>no changes</div>}
          {changes.map((d) => (
            <div key={d.isp} style={{ fontSize: 10, color: diffColor[d.kind], padding: '1px 0' }}>
              <b>{d.isp}</b> — {d.kind}{d.notes.length > 0 ? `: ${d.notes.join('; ')}` : ''}
            </div>
          ))}
        </div>
      )}
      <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
        <button
          type="button" style={dangerBtn}
          disabled={busy || errors.length > 0 || changes.length === 0}
          onClick={() => void submit()}
        >
          {busy ? 'Replacing…' : 'Replace throttle…'}
        </button>
        <button
          type="button"
          style={{ ...smallBtn, background: 'transparent', color: colors.textMuted, border: `1px solid ${colors.hairline}` }}
          disabled={busy} onClick={onClose}
        >
          Cancel
        </button>
      </div>
    </div>
  );
};

// ── Roster membership (add / remove a domain on this drip) ──────────────────

interface MembershipState {
  loading: boolean;
  members: RosterRow[];
  failed: string[];   // brand codes whose roster read failed
  checked: number;
  error: string | null;
}

// ── Screen ──────────────────────────────────────────────────────────────────

export const DripJourneyCanvas: React.FC = () => {
  const toast = useToast();
  const [brand, setBrand] = useState<string | null>(null);
  const [vertical, setVertical] = useState<string | null>(null);
  const [openEdge, setOpenEdge] = useState<number | null>(null);
  const [days, setDays] = useState(7);
  const [editingFeed, setEditingFeed] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [showRoster, setShowRoster] = useState(false);

  const notify = useCallback((s: string) => {
    setNotice(s);
    toast.addToast({ type: 'info', title: 'Drip journey', message: s });
  }, [toast]);

  // 1. Brand ⇄ sending-domain pairs, from the existing Property Ledger read.
  const ledger = useResource<LedgerResponseLite>(
    'ledger',
    (signal) => getJSON<LedgerResponseLite>('/api/mailing/pmta-campaign/property-ledger', signal),
  );

  const domains = useMemo(() => {
    const m = new Map<string, string>(); // brand -> sending_domain
    for (const r of ledger.data?.rows ?? []) {
      if (!r.brand) continue;
      if (!m.has(r.brand)) m.set(r.brand, r.sending_domain || r.brand);
    }
    return Array.from(m.entries())
      .map(([code, domain]) => ({ code, domain }))
      .sort((a, b) => a.domain.localeCompare(b.domain));
  }, [ledger.data]);

  // Default the selector once the roster brands land.
  useEffect(() => {
    if (brand !== null || domains.length === 0) return;
    const db = domains.find((d) => d.code === 'db');
    setBrand(db ? db.code : domains[0].code);
  }, [domains, brand]);

  const sendingDomain = useMemo(
    () => domains.find((d) => d.code === brand)?.domain ?? null,
    [domains, brand],
  );

  // 2. This domain's drips.
  const roster = useResource<RosterResponse>(
    brand ? `roster:${brand}` : null,
    (signal) => getJSON<RosterResponse>(
      `/api/mailing/pmta-campaign/property-ledger/roster?brand=${encodeURIComponent(brand ?? '')}`,
      signal,
    ),
  );

  const drips = useMemo(
    () => (roster.data?.rows ?? []).slice().sort((a, b) => {
      if (a.active !== b.active) return a.active ? -1 : 1;
      if (a.sort_order !== b.sort_order) return a.sort_order - b.sort_order;
      return a.vertical.localeCompare(b.vertical);
    }),
    [roster.data],
  );

  // Auto-select when a domain rides exactly one drip (the dropdown still shows).
  useEffect(() => {
    if (drips.length === 0) { setVertical(null); return; }
    if (vertical && drips.some((d) => d.vertical === vertical)) return;
    const active = drips.filter((d) => d.active);
    setVertical((active.length > 0 ? active[0] : drips[0]).vertical);
  }, [drips, vertical]);

  // 3. The journey.
  const journeyKey = brand && vertical ? `journey:${brand}:${vertical}` : null;
  const journey = useResource<JourneyResponse>(
    journeyKey,
    (signal) => getJSON<JourneyResponse>(
      `/api/mailing/pmta-campaign/property-ledger/journey?brand=${encodeURIComponent(brand ?? '')}&vertical=${encodeURIComponent(vertical ?? '')}`,
      signal,
    ),
  );
  useEffect(() => { setOpenEdge(null); }, [journeyKey]);

  // 4. The scoreboard.
  const stats = useResource<StatsResponse>(
    vertical ? `stats:${vertical}:${days}` : null,
    (signal) => getJSON<StatsResponse>(
      `/api/mailing/pmta-campaign/property-ledger/stats?vertical=${encodeURIComponent(vertical ?? '')}&days=${days}`,
      signal,
    ),
  );

  // 5. The levers. VERIFIED against HandleLaneThrottle: keyed by `domain`, and
  // the response covers ALL of the brand's feeds — filtered here to the drip
  // the operator is looking at.
  const throttle = useResource<ThrottleResponse>(
    sendingDomain ? `throttle:${sendingDomain}` : null,
    (signal) => getJSON<ThrottleResponse>(
      `/api/mailing/pmta-campaign/property-ledger/throttle?domain=${encodeURIComponent(sendingDomain ?? '')}`,
      signal,
    ),
  );

  const feeds = useMemo(
    () => (throttle.data?.feeds ?? []).filter((f) => !vertical || f.vertical === vertical),
    [throttle.data, vertical],
  );

  // 6. Membership across the roster (lazy — only when the panel is opened).
  const [membership, setMembership] = useState<MembershipState>({
    loading: false, members: [], failed: [], checked: 0, error: null,
  });

  const loadMembership = useCallback(async () => {
    if (!vertical || domains.length === 0) return;
    setMembership({ loading: true, members: [], failed: [], checked: 0, error: null });
    const results = await Promise.allSettled(
      domains.map(async (d) => {
        const ac = new AbortController();
        const res = await getJSON<RosterResponse>(
          `/api/mailing/pmta-campaign/property-ledger/roster?brand=${encodeURIComponent(d.code)}`,
          ac.signal,
        );
        return { code: d.code, rows: res.rows ?? [] };
      }),
    );
    const members: RosterRow[] = [];
    const failed: string[] = [];
    results.forEach((r, i) => {
      if (r.status === 'fulfilled') {
        const row = r.value.rows.find((x) => x.vertical === vertical);
        if (row) members.push(row);
      } else {
        failed.push(domains[i].code);
      }
    });
    setMembership({
      loading: false,
      members: members.sort((a, b) => a.brand.localeCompare(b.brand)),
      failed,
      checked: domains.length,
      error: failed.length === domains.length ? 'every roster read failed' : null,
    });
  }, [vertical, domains]);

  useEffect(() => {
    if (!showRoster) return;
    void loadMembership();
  }, [showRoster, loadMembership]);

  const rosterWrite = async (
    action: 'assign' | 'unassign',
    body: Record<string, unknown>,
    label: string,
  ) => {
    try {
      const r = await apiFetch(
        `/api/mailing/pmta-campaign/property-ledger/roster/${action}`,
        { method: 'POST', body: JSON.stringify(body) },
      );
      let json: Record<string, unknown> = {};
      try { json = (await r.json()) as Record<string, unknown>; } catch { /* non-JSON */ }
      if (!r.ok) {
        notify(`${label} failed: ${String(json.error ?? r.status)}`);
        return;
      }
      notify(`${label} — server accepted.`);
      roster.reload();
      void loadMembership();
    } catch (e) {
      notify(`${label} failed: ${e instanceof Error ? e.message : 'network error'}`);
    }
  };

  // ── Derived headline numbers ──────────────────────────────────────────────
  const j = journey.data;
  const configuredCount = (j?.touches ?? []).filter((t) => t.configured).length;
  const edgesReported = j?.edges?.length ?? 0;
  const expectedEdges = j ? Math.max(0, (j.max_touches || (j.touches?.length ?? 0)) - 1) : 0;
  const waitingSum = (j?.edges ?? []).reduce((a, e) => a + (Number.isFinite(e.waiting) ? e.waiting : 0), 0);
  const openEdgeRow = openEdge != null ? (j?.edges ?? []).find((e) => e.from_touch === openEdge) ?? null : null;

  // ── Stats derivation (client-side day-over-day, pinned totals) ────────────
  const statRows = useMemo(() => (stats.data?.rows ?? []).slice(), [stats.data]);

  const prevByIspDay = useMemo(() => {
    // For each ISP, the previous (older) day's row, so a row can show direction.
    const byIsp = new Map<string, StatsRow[]>();
    for (const r of statRows) {
      const list = byIsp.get(r.isp) ?? [];
      list.push(r);
      byIsp.set(r.isp, list);
    }
    const prev = new Map<string, StatsRow>();
    for (const [isp, list] of byIsp) {
      const asc = list.slice().sort((a, b) => a.day.localeCompare(b.day));
      for (let i = 1; i < asc.length; i++) prev.set(`${isp}|${asc[i].day}`, asc[i - 1]);
    }
    return prev;
  }, [statRows]);

  const sortedStats = useMemo(
    () => statRows.slice().sort((a, b) => (a.day === b.day ? a.isp.localeCompare(b.isp) : b.day.localeCompare(a.day))),
    [statRows],
  );

  const statTotals = useMemo(() => {
    const t = { sent: 0, delivered: 0, opens: 0, clicks: 0 };
    for (const r of statRows) {
      t.sent += Number.isFinite(r.sent) ? r.sent : 0;
      t.delivered += Number.isFinite(r.delivered_pg) ? r.delivered_pg : 0;
      t.opens += Number.isFinite(r.opens) ? r.opens : 0;
      t.clicks += Number.isFinite(r.clicks) ? r.clicks : 0;
    }
    return t;
  }, [statRows]);

  const Direction: React.FC<{ cur: number; prev: number | null; unit: 'pp' | 'n' }> = ({ cur, prev, unit }) => {
    if (prev == null || !Number.isFinite(prev) || !Number.isFinite(cur)) {
      return <span style={{ fontSize: 9, color: colors.textFaint }} title="No prior day for this ISP in the window — direction unknown, not flat.">{UNKNOWN}</span>;
    }
    const d = cur - prev;
    if (Math.abs(d) < 1e-9) return <span style={{ fontSize: 9, color: colors.textFaint }}>flat</span>;
    const up = d > 0;
    const text = unit === 'pp' ? `${up ? '+' : ''}${(d * 100).toFixed(2)}pp` : `${up ? '+' : ''}${Math.round(d).toLocaleString()}`;
    return (
      <span style={{ fontSize: 9, color: up ? colors.successText : colors.dangerText, fontVariantNumeric: 'tabular-nums' }}>
        {up ? '▲' : '▼'} {text}
      </span>
    );
  };

  // ── Render ────────────────────────────────────────────────────────────────

  return (
    <div style={pageStyle}>
      <PortalKeyframes />
      <style>{'@keyframes djcSpin{to{transform:rotate(360deg)}}'}</style>

      {/* Header + selectors */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 14, flexWrap: 'wrap', marginBottom: 14 }}>
        <h2 style={{ margin: 0, fontSize: 18, color: colors.heading, display: 'flex', alignItems: 'center', gap: 9 }}>
          <FontAwesomeIcon icon={faDiagramProject} style={{ color: colors.indigo400 }} />
          Drip Journey
        </h2>

        <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}>
          Sending domain
          <select
            style={selectStyle}
            value={brand ?? ''}
            disabled={ledger.loading || !!ledger.error || domains.length === 0}
            onChange={(e) => { setBrand(e.target.value || null); setVertical(null); setShowRoster(false); }}
          >
            {brand === null && <option value="">select a domain…</option>}
            {domains.map((d) => (
              <option key={d.code} value={d.code}>{d.domain} ({d.code})</option>
            ))}
          </select>
        </label>

        <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}
          title="Every drip (vertical) this sending domain is assigned to. Shown even when there is only one, so the operator can see there is only one.">
          Drip
          <select
            style={selectStyle}
            value={vertical ?? ''}
            disabled={roster.loading || !!roster.error || drips.length === 0}
            onChange={(e) => setVertical(e.target.value || null)}
          >
            {drips.length === 0 && <option value="">—</option>}
            {drips.map((d) => (
              <option key={d.vertical} value={d.vertical}>
                {d.vertical}{d.active ? '' : ' (inactive)'} · weight {d.weight}
              </option>
            ))}
          </select>
        </label>

        <button type="button" style={{ ...btnStyle, marginLeft: 'auto' }}
          onClick={() => { ledger.reload(); roster.reload(); journey.reload(); stats.reload(); throttle.reload(); }}>
          Refresh all
        </button>
      </div>

      {ledger.loading && <LoadingRow label="the drip roster's domains" />}
      {ledger.error && (
        <div style={{ marginBottom: 12 }}>
          <SectionError label="Property Ledger domains" error={ledger.error} onRetry={ledger.reload} />
        </div>
      )}
      {!ledger.loading && !ledger.error && domains.length === 0 && (
        <Panel><EmptyState title="No drip roster domains" hint="The Property Ledger returned no rows — nothing to select." /></Panel>
      )}

      {notice && (
        <div style={{
          marginBottom: 12, fontSize: 12, color: colors.indigo200,
          background: alpha(colors.indigo500, '14'),
          border: `1px solid ${alpha(colors.indigo500, '44')}`,
          borderRadius: 6, padding: '7px 10px',
        }}>
          {notice}
        </div>
      )}

      {/* Active scope chips */}
      {brand && (
        <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 14 }}>
          <FilterChip label={`domain ${sendingDomain ?? brand}`} />
          <FilterChip label={`drip ${vertical ?? '(none)'}`} />
          <FilterChip label={`scoreboard ${daysAgoDenver(days - 1)} → ${denverToday()} · America/Denver`} />
        </div>
      )}

      {/* ── 1. IN MOTION — the journey canvas ─────────────────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="Journey — in motion right now"
          icon={faDiagramProject}
          right={j?.generated_at ? <span style={{ fontSize: 11, color: colors.textFaint }}>as of {shortTime(j.generated_at)}</span> : undefined}
        />
        <AsyncPanel
          label="the journey"
          res={journey}
          isEmpty={!j || (j.touches ?? []).length === 0}
          emptyTitle="This drip has no ladder built"
          emptyHint="The server returned a journey with zero touches — never built, as opposed to built-and-empty. Check the vertical's touch copy in the lane content panel."
        >
          {j && (
            <>
              <div style={{ ...cardGrid(150), marginBottom: 14 }}>
                <Stat
                  label="In flight (this drip)"
                  value={num(j.totals?.in_flight)}
                  sub="subscribers on the ladder now"
                  color={colors.indigo200}
                  title="Server-reported total currently enrolled and not yet retired on this vertical."
                />
                <Stat
                  label="Due now"
                  value={num(j.totals?.due_now)}
                  sub="next_touch_at already passed"
                  color={(j.totals?.due_now ?? 0) > 0 ? colors.warningText : colors.text}
                  title="Rows whose next touch is already due — the backlog the orchestrator will claim from on its next wave."
                />
                <Stat
                  label="Waiting on connectors"
                  value={num(waitingSum)}
                  sub={edgesReported === expectedEdges
                    ? `sum of all ${edgesReported} reported edges`
                    : `sum of ${edgesReported} of ${expectedEdges} edges — ${expectedEdges - edgesReported} not reported`}
                  color={edgesReported === expectedEdges ? colors.text : colors.warningText}
                  title="Sum of the per-edge waiting counts drawn on the canvas. When edges are missing this is a FLOOR, not the total."
                />
                <Stat
                  label="Inter-touch delay"
                  value={Number.isFinite(j.delay_hours) ? `${j.delay_hours}h` : UNKNOWN}
                  sub="uniform, every rung"
                  title="The lane's configured delay between consecutive touches — the duration drawn in each diamond."
                />
                <Stat
                  label="Touches configured"
                  value={`${configuredCount} / ${j.max_touches || (j.touches?.length ?? 0)}`}
                  sub={configuredCount < (j.max_touches || 0) ? 'ladder retires early' : 'full ladder'}
                  color={configuredCount < (j.max_touches || 0) ? colors.warningText : colors.successText}
                  title="A rung with no configured copy is where the ladder actually retires, regardless of max_touches."
                />
              </div>

              <JourneyCanvas
                journey={j}
                openEdge={openEdge}
                onToggleEdge={(t) => setOpenEdge((cur) => (cur === t ? null : t))}
              />

              {openEdgeRow && (
                <div style={{
                  marginTop: 10, borderTop: `1px solid ${colors.hairline}`, paddingTop: 10,
                }}>
                  <div style={{ fontSize: 11, color: colors.heading, fontWeight: 700, letterSpacing: 0.5, marginBottom: 6 }}>
                    TOUCH {openEdgeRow.from_touch} → {openEdgeRow.to_touch} · {num(openEdgeRow.waiting)} waiting
                    <span style={{ fontWeight: 400, color: colors.textMuted, marginLeft: 8 }}>
                      soonest {shortTime(openEdgeRow.soonest)} ({durationUntil(openEdgeRow.soonest)}) · latest {shortTime(openEdgeRow.latest)}
                    </span>
                  </div>
                  {(openEdgeRow.by_isp ?? []).length === 0 ? (
                    <div style={{ fontSize: 11, color: colors.textFaint }}>
                      The server reported no per-ISP split for this edge — unknown, not zero.
                    </div>
                  ) : (
                    <div style={{ overflowX: 'auto' }}>
                      <table style={{ ...tableStyle, minWidth: 320, maxWidth: 560 }}>
                        <thead>
                          <tr>
                            <th style={thStyle}>ISP</th>
                            <th style={numTh}>Waiting</th>
                            <th style={numTh} title="Share of this edge's waiting population (denominator = the edge's total waiting).">
                              % of edge
                            </th>
                          </tr>
                        </thead>
                        <tbody>
                          {(openEdgeRow.by_isp ?? []).slice().sort((a, b) => b.waiting - a.waiting).map((row) => (
                            <tr key={row.isp}>
                              <td style={tdStyle}>{row.isp}</td>
                              <td style={numTd}>{num(row.waiting)}</td>
                              <td style={numTd}>{ratePct(derive(row.waiting, openEdgeRow.waiting))}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  )}
                </div>
              )}

              <p style={noteStyle}>
                Each connector's number is the population sitting between those two touches — click it for the
                per-ISP split and the soonest next-due time. A dashed grey connector means the server reported
                <b> no edge</b> for that gap: unknown, not zero. Rungs drawn dashed and muted are
                <b> not configured</b> — the ladder retires there regardless of <code>max_touches</code>.
              </p>
            </>
          )}
        </AsyncPanel>
      </Panel>

      {/* ── 2. PLAN AHEAD — the quota levers ──────────────────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="Quota levers — per-ISP caps for this drip"
          icon={faSlidersH}
          right={throttle.data ? <span style={{ fontSize: 11, color: colors.textFaint }}>as of {shortTime(throttle.data.as_of)}</span> : undefined}
        />
        <AsyncPanel
          label="the throttle configuration"
          res={throttle}
          isEmpty={feeds.length === 0}
          emptyTitle={throttle.data ? 'No feed on this domain belongs to this drip' : 'No throttle data'}
          emptyHint="The throttle read succeeded but returned no feed whose vertical matches the selected drip."
        >
          {throttle.data && (
            <>
              {!throttle.data.write_enabled && (
                <ReadOnlyBanner
                  envVar={throttle.data.write_flag_env}
                  what="Quota editing"
                  title={throttle.data.enforcement_note}
                />
              )}
              <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 10 }} title={throttle.data.cap_systems_note}>
                {throttle.data.cap_systems_note}
              </div>
              {feeds.map((f) => (
                <div
                  key={f.dataset_id}
                  style={{
                    border: `1px solid ${colors.hairline}`, borderRadius: 8,
                    padding: '10px 12px', marginBottom: 10,
                    opacity: f.paused_emergency ? 0.65 : 1,
                  }}
                >
                  <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', marginBottom: 8 }}>
                    <span style={{ fontSize: 13, fontWeight: 700, color: colors.heading }}>{f.name}</span>
                    <Pill color={stateColor(f.paused_emergency ? 'paused' : f.status)} style={{ fontSize: 9 }}>
                      {f.paused_emergency ? 'paused' : f.status || 'unknown'}
                    </Pill>
                    <span
                      style={{ fontSize: 10, fontFamily: 'monospace', color: colors.textFaint }}
                      title={`dataset_id ${f.dataset_id}`}
                    >
                      {f.dataset_id.slice(0, 8)}…
                    </span>
                    <span style={{ fontSize: 11, color: colors.textMuted }} title={throttle.data?.cap_systems_note}>
                      supply release cap: <b style={{ color: colors.text, fontVariantNumeric: 'tabular-nums' }}>
                        {f.supply_release_daily_cap > 0 ? `${num(f.supply_release_daily_cap)}/day` : 'uncapped'}
                      </b> <span style={{ color: colors.textFaint }}>(lane ready-vs-held — a DIFFERENT system from the per-ISP caps below)</span>
                    </span>
                    {f.shared_brands.length > 1 && (
                      <span
                        style={{ fontSize: 10, color: colors.warningText }}
                        title={`This feed's supply is shared across the whole rotation: ${f.shared_brands.join(', ')} — edits here change all of them.`}
                      >
                        shared across {f.shared_brands.length} brands
                      </span>
                    )}
                    <span style={{ marginLeft: 'auto' }}>
                      {throttle.data?.write_enabled ? (
                        <button
                          type="button" style={smallBtn}
                          title={throttle.data.enforcement_note}
                          onClick={() => setEditingFeed((cur) => (cur === f.dataset_id ? null : f.dataset_id))}
                        >
                          {editingFeed === f.dataset_id ? 'Close editor' : 'Edit caps'}
                        </button>
                      ) : (
                        <Pill color={colors.warning} style={{ fontSize: 9 }}>read-only</Pill>
                      )}
                    </span>
                  </div>

                  <div style={{ overflowX: 'auto' }}>
                    <table style={{ ...tableStyle, minWidth: 620 }}>
                      <thead>
                        <tr>
                          <th style={thStyle}>ISP</th>
                          <th style={numTh} title="Fraction of each wave's claim allocated to this ISP (0–1).">
                            Pct of wave (0–1)
                          </th>
                          <th style={numTh} title="Per-wave claim cap for this dataset's waves — replaces the global per-wave cap. Blank = global default.">
                            Max/wave (per wave)
                          </th>
                          <th style={numTh} title="Lane-owned per-ISP DAILY budget. Blank = global default; 0 = hard-suppressed for this lane.">
                            Daily cap (per day)
                          </th>
                          <th style={thStyle}>Last change</th>
                        </tr>
                      </thead>
                      <tbody>
                        {f.overrides.length === 0 && (
                          <tr>
                            <td style={{ ...tdStyle, color: colors.textFaint }} colSpan={5}>
                              No override rows — every ISP on this feed rides the global defaults.
                            </td>
                          </tr>
                        )}
                        {f.overrides.map((ov) => (
                          <tr key={ov.isp}>
                            <td style={tdStyle}>{ov.isp}</td>
                            <td style={numTd}>{Number.isFinite(ov.pct_override) ? ov.pct_override : UNKNOWN}</td>
                            <td style={numTd}>{ov.max_per_wave > 0 ? num(ov.max_per_wave) : <span style={{ color: colors.textFaint }}>default</span>}</td>
                            <td style={numTd}>
                              {ov.daily_cap == null
                                ? <span style={{ color: colors.textFaint }} title="NULL — this ISP rides the global per-brand default.">default</span>
                                : ov.daily_cap === 0
                                  ? <span style={{ color: colors.dangerText, fontWeight: 700 }} title="0 = hard-suppressed for this lane.">0 (suppressed)</span>
                                  : num(ov.daily_cap)}
                            </td>
                            <td style={{ ...tdStyle, fontSize: 11, color: colors.textMuted }}>
                              {shortTime(ov.updated_at)}{ov.updated_by ? ` · ${ov.updated_by}` : ''}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>

                  {f.default_isps.length > 0 && (
                    <div style={{ fontSize: 10, color: colors.textFaint, marginTop: 6 }}>
                      Riding global defaults (no override row): {f.default_isps.join(', ')}
                    </div>
                  )}

                  {throttle.data?.write_enabled && editingFeed === f.dataset_id && (
                    <ThrottleEditor
                      feed={f}
                      replacementNote={throttle.data.replacement_note}
                      onClose={() => setEditingFeed(null)}
                      onSaved={() => { setEditingFeed(null); throttle.reload(); }}
                      onNotice={notify}
                    />
                  )}
                </div>
              ))}
              <p style={noteStyle}>{throttle.data.enforcement_note}</p>
            </>
          )}
        </AsyncPanel>
      </Panel>

      {/* ── 3. HISTORY — the scoreboard ───────────────────────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="Scoreboard — lane × ISP × day"
          icon={faChartLine}
          right={
            <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}>
              Window
              <select style={selectStyle} value={days} onChange={(e) => setDays(Number(e.target.value))}>
                <option value={7}>7 days</option>
                <option value={14}>14 days</option>
                <option value={30}>30 days</option>
              </select>
            </label>
          }
        />
        <AsyncPanel
          label="the scoreboard"
          res={stats}
          isEmpty={statRows.length === 0}
          emptyTitle="No sends recorded for this drip in the window"
          emptyHint={`The stats read succeeded and returned zero rows for the last ${days} days — built empty, not failed. Widen the window or check the lane is releasing supply.`}
        >
          <div style={{ overflowX: 'auto' }}>
            <table style={{ ...tableStyle, minWidth: 940 }}>
              <thead>
                <tr>
                  <th style={thStyle}>Day (Denver)</th>
                  <th style={thStyle}>ISP</th>
                  <th style={numTh} title="Send events recorded for this lane × ISP × day.">Sent</th>
                  <th style={numTh} title="PG tracking-event delivered count — the per-campaign delivery proxy (METRIC_CONTRACT §1). PG confirmation lags and under-counts (Microsoft ~30%); lake delivery truth may read higher.">
                    Delivered* (PG)
                  </th>
                  <th style={numTh} title="Raw opens — machine/MPP traffic included (METRIC_CONTRACT §6). Not 'people opened'.">
                    Opens (raw)
                  </th>
                  <th style={numTh} title="Open rate — denominator = Delivered* (PG) on this row.">
                    Open % (of delivered)
                  </th>
                  <th style={numTh} title="Raw clicks — machine clicks included.">Clicks (raw)</th>
                  <th style={numTh} title="Click rate — denominator = Delivered* (PG) on this row.">
                    Click % (of delivered)
                  </th>
                  <th style={thStyle} title="Change vs this ISP's previous day in the window. '—' means there is no prior day, which is not the same as flat.">
                    vs prior day
                  </th>
                </tr>
              </thead>
              <tbody>
                {sortedStats.map((r) => {
                  const prev = prevByIspDay.get(`${r.isp}|${r.day}`) ?? null;
                  const openDerived = derive(r.opens, r.delivered_pg);
                  const clickDerived = derive(r.clicks, r.delivered_pg);
                  const openMismatch = openDerived != null && Number.isFinite(r.open_rate) && Math.abs(openDerived - r.open_rate) > 0.005;
                  const clickMismatch = clickDerived != null && Number.isFinite(r.click_rate) && Math.abs(clickDerived - r.click_rate) > 0.005;
                  return (
                    <tr key={`${r.day}|${r.isp}`}>
                      <td style={tdStyle}>{r.day}</td>
                      <td style={tdStyle}>{r.isp}</td>
                      <td style={numTd}>{num(r.sent)}</td>
                      <td style={numTd}>{num(r.delivered_pg)}</td>
                      <td style={numTd}>{num(r.opens)}</td>
                      <td style={numTd} title={openMismatch ? `Server open_rate ${ratePct(r.open_rate)} does not reconcile with opens/delivered shown here (${ratePct(openDerived)}) — the server used a different denominator.` : `opens ${num(r.opens)} / delivered ${num(r.delivered_pg)}`}>
                        {ratePct(r.open_rate)}{openMismatch && <span style={{ color: colors.warningText }}>*</span>}
                      </td>
                      <td style={numTd}>{num(r.clicks)}</td>
                      <td style={numTd} title={clickMismatch ? `Server click_rate ${ratePct(r.click_rate)} does not reconcile with clicks/delivered shown here (${ratePct(clickDerived)}) — the server used a different denominator.` : `clicks ${num(r.clicks)} / delivered ${num(r.delivered_pg)}`}>
                        {ratePct(r.click_rate)}{clickMismatch && <span style={{ color: colors.warningText }}>*</span>}
                      </td>
                      <td style={tdStyle}>
                        <span style={{ display: 'inline-flex', gap: 8 }}>
                          <span title="Click-rate direction vs this ISP's prior day.">
                            clk <Direction cur={r.click_rate} prev={prev ? prev.click_rate : null} unit="pp" />
                          </span>
                          <span title="Delivered direction vs this ISP's prior day.">
                            dlv <Direction cur={r.delivered_pg} prev={prev ? prev.delivered_pg : null} unit="n" />
                          </span>
                        </span>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
              <tfoot>
                <tr>
                  <td style={{ ...tdStyle, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }} colSpan={2}>
                    Total ({days}d, all ISPs shown)
                  </td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }}>{num(statTotals.sent)}</td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }}>{num(statTotals.delivered)}</td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }}>{num(statTotals.opens)}</td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }} title="Total opens / total Delivered* (PG) across the rows shown.">
                    {ratePct(derive(statTotals.opens, statTotals.delivered))}
                  </td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }}>{num(statTotals.clicks)}</td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }} title="Total clicks / total Delivered* (PG) across the rows shown.">
                    {ratePct(derive(statTotals.clicks, statTotals.delivered))}
                  </td>
                  <td style={{ ...tdStyle, borderTop: `1px solid ${colors.panelBorderStrong}` }} />
                </tr>
              </tfoot>
            </table>
          </div>
          <p style={noteStyle}>
            <b>Delivered*</b> is the PG tracking-event count — the per-campaign delivery proxy
            (METRIC_CONTRACT §1). It is a confirmation-rate floor, not lake delivery truth, and is
            known to under-count Microsoft by roughly 30%; open/click rates here divide by it, so
            they read high wherever confirmation lags. Opens and clicks are <b>raw</b> — machine and
            MPP traffic included, never "people opened" (§6). This feed carries no bounce columns,
            so no bounce number is shown here at all rather than a misleading combined one; hard and
            soft bounces are never summed (§3).
          </p>
        </AsyncPanel>
      </Panel>

      {/* ── 4. STRUCTURE — roster membership ──────────────────────────────── */}
      <Panel>
        <SectionHeader
          title="Roster — which domains ride this drip"
          icon={faSitemap}
          right={
            <button type="button" style={smallBtn} onClick={() => setShowRoster((s) => !s)} disabled={!vertical}>
              {showRoster ? 'Hide' : 'Show membership'}
            </button>
          }
        />
        {!vertical ? (
          <EmptyState title="Select a drip first" hint="Membership is scoped to one vertical." />
        ) : !showRoster ? (
          <div style={{ fontSize: 11, color: colors.textMuted }}>
            Membership is read on demand — it queries the roster of all {domains.length} ledger domains.
          </div>
        ) : membership.loading ? (
          <LoadingRow label={`membership across ${domains.length} domains`} />
        ) : membership.error ? (
          <SectionError label="Roster membership" error={membership.error} onRetry={() => void loadMembership()} />
        ) : (
          <>
            {membership.failed.length > 0 && (
              <div style={{
                fontSize: 11, color: colors.warningText, marginBottom: 8,
                background: alpha(colors.warning, '14'),
                border: `1px solid ${alpha(colors.warning, '44')}`,
                borderRadius: 6, padding: '6px 10px',
              }}>
                {membership.failed.length} of {membership.checked} roster reads failed
                ({membership.failed.join(', ')}) — this membership list is INCOMPLETE, not empty.
              </div>
            )}
            {roster.data && !roster.data.write_enabled && (
              <ReadOnlyBanner envVar={roster.data.write_flag_env} what="Roster editing" />
            )}
            {membership.members.length === 0 ? (
              <EmptyState
                title="No domain is assigned to this drip"
                hint="Every roster read came back without a row for this vertical."
              />
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table style={{ ...tableStyle, minWidth: 620 }}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Domain</th>
                      <th style={thStyle}>Brand</th>
                      <th style={numTh} title="Rotation weight — the share of this drip's supply the domain draws.">Weight</th>
                      <th style={numTh}>Sort order</th>
                      <th style={thStyle}>State</th>
                      <th style={thStyle}>Last change</th>
                      <th style={thStyle} />
                    </tr>
                  </thead>
                  <tbody>
                    {membership.members.map((m) => {
                      const dom = domains.find((d) => d.code === m.brand);
                      return (
                        <tr key={m.brand} style={{ opacity: m.active ? 1 : 0.6 }}>
                          <td style={tdStyle}>{dom?.domain ?? UNKNOWN}</td>
                          <td style={tdStyle}>{m.brand}</td>
                          <td style={numTd}>{num(m.weight)}</td>
                          <td style={numTd}>{num(m.sort_order)}</td>
                          <td style={tdStyle}>
                            <Pill color={m.active ? colors.success : colors.idle} style={{ fontSize: 9 }}>
                              {m.active ? 'active' : 'disabled'}
                            </Pill>
                          </td>
                          <td style={{ ...tdStyle, fontSize: 11, color: colors.textMuted }}>
                            {shortTime(m.updated_at)}{m.updated_by ? ` · ${m.updated_by}` : ''}
                          </td>
                          <td style={tdStyle}>
                            {roster.data?.write_enabled ? (
                              <button
                                type="button" style={dangerBtn}
                                title="Soft-disables the assignment (the server never deletes the row)."
                                disabled={!m.active}
                                onClick={() => {
                                  if (!window.confirm(`Remove ${dom?.domain ?? m.brand} from ${vertical}?\n\nThis soft-disables the roster row; the drip stops selecting this domain on the next wave.`)) return;
                                  void rosterWrite('unassign', { vertical, brand: m.brand },
                                    `Remove ${m.brand} from ${vertical}`);
                                }}
                              >
                                Remove
                              </button>
                            ) : (
                              <span style={{ fontSize: 10, color: colors.textFaint }}>read-only</span>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
            <AddToDripRow
              vertical={vertical}
              domains={domains}
              members={membership.members}
              writeEnabled={!!roster.data?.write_enabled}
              onAssign={(body, label) => void rosterWrite('assign', body, label)}
            />
            <p style={noteStyle}>
              Removing a domain soft-disables its roster row (the server never deletes it), so history and
              weights survive. Roster changes take effect on the drip orchestrator's next wave.
            </p>
          </>
        )}
      </Panel>

      <div style={{ fontSize: 10, color: colors.textFaint, marginTop: 14 }}>{PAGE_VERSION}</div>
    </div>
  );
};

// ── Add-a-domain row ────────────────────────────────────────────────────────

const AddToDripRow: React.FC<{
  vertical: string;
  domains: Array<{ code: string; domain: string }>;
  members: RosterRow[];
  writeEnabled: boolean;
  onAssign: (body: Record<string, unknown>, label: string) => void;
}> = ({ vertical, domains, members, writeEnabled, onAssign }) => {
  const [code, setCode] = useState('');
  const [weight, setWeight] = useState('1');
  const [sortOrder, setSortOrder] = useState('0');

  const candidates = domains.filter((d) => !members.some((m) => m.brand === d.code && m.active));
  const wNum = Number(weight);
  const sNum = Number(sortOrder);
  const valid = code !== '' && Number.isFinite(wNum) && wNum >= 0 && Number.isInteger(sNum);

  return (
    <div style={{
      marginTop: 12, borderTop: `1px solid ${colors.hairline}`, paddingTop: 10,
      display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap',
    }}>
      <span style={{ ...panelTitleStyle, fontSize: 11 }}>Add a domain to this drip</span>
      <select style={{ ...inputStyle, width: 240 }} value={code} disabled={!writeEnabled}
        onChange={(e) => setCode(e.target.value)}>
        <option value="">select a domain…</option>
        {candidates.map((d) => <option key={d.code} value={d.code}>{d.domain} ({d.code})</option>)}
      </select>
      <label style={{ fontSize: 9, color: colors.textMuted }} title="Rotation weight for this domain on the drip.">
        weight{' '}
        <input style={inputStyle} value={weight} disabled={!writeEnabled}
          onChange={(e) => setWeight(e.target.value)} />
      </label>
      <label style={{ fontSize: 9, color: colors.textMuted }} title="Ordering within the drip's rotation (lower first).">
        sort order{' '}
        <input style={inputStyle} value={sortOrder} disabled={!writeEnabled}
          onChange={(e) => setSortOrder(e.target.value)} />
      </label>
      {writeEnabled ? (
        <button
          type="button" style={smallBtn} disabled={!valid}
          onClick={() => {
            const dom = domains.find((d) => d.code === code);
            if (!window.confirm(`Add ${dom?.domain ?? code} to ${vertical} at weight ${wNum}?\n\nThe drip starts selecting this domain on the orchestrator's next wave.`)) return;
            onAssign(
              { vertical, brand: code, weight: wNum, sort_order: sNum },
              `Add ${code} to ${vertical}`,
            );
            setCode('');
          }}
        >
          Add to drip
        </button>
      ) : (
        <span style={{ fontSize: 10, color: colors.textFaint }}>read-only — server reports roster writes disabled</span>
      )}
    </div>
  );
};

export default DripJourneyCanvas;
