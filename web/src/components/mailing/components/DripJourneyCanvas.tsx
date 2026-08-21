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
  faDiagramProject, faSlidersH, faChartLine, faSitemap, faHourglassHalf, faClock,
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
// Property Ledger read (existing, property_ledger.go). This ONE response carries
// both the brand ⇄ sending-domain pairs for the selector AND the introduction
// ledger itself — the per-(brand × ISP) daily_budget/hold rows the drip
// orchestrator actually enforces (internal/worker/partner_drip_brand_budgets.go),
// plus SES VDM telemetry and the global emergency hold. Nothing extra is
// fetched to render the ledger panel; this payload was already on the wire and
// was being discarded.

// SES Virtual Deliverability Manager telemetry for one (sending domain × ISP)
// lane. UNIQUE-count semantics over COMPLETE UTC days — it is NOT the same
// population as the Denver-day lake snapshot and the two never arbitrate.
interface LedgerVDM {
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  delivered_pct: number | null;
  open_rate_vdm: number | null;
  click_rate_vdm: number | null;
  sent_7d: number;
  delivered_7d: number;
  opens_7d: number;
  clicks_7d: number;
  delivered_pct_7d: number | null;
  open_rate_vdm_7d: number | null;
  click_rate_vdm_7d: number | null;
}

// A pending budget proposal awaiting operator approval. Approving is a LIVE
// WRITE and stays on the Property Ledger screen — this panel only surfaces that
// one is waiting, so a pending decision cannot hide.
interface LedgerProposal {
  id: string;
  proposed_budget: number;
  base_budget: number;
  basis: string;
  created_at: string;
  expires_at: string;
}

interface LedgerRow {
  brand: string;
  sending_domain: string;
  isp: string;
  daily_budget: number;
  hold: boolean;
  hold_reason?: string;
  held_since?: string;
  notes?: string;
  updated_by?: string;
  updated_at: string;
  approved_by?: string;
  approved_at?: string;
  min_budget?: number;
  max_budget?: number;
  lock_version: number;
  pending_budget?: number;
  pending_effective_day?: string;
  proposal?: LedgerProposal;
  // null = the counter rollup has not materialised a cell for this lane today.
  // That is ABSENT, never 0 — a 0 would read as "nothing introduced yet".
  introduced_today: number | null;
  introduced_as_of?: string;
  vdm?: LedgerVDM;
}

interface LedgerRunInfo {
  day: string;
  status: string;
  started_at: string;
  finished_at?: string;
}

interface LedgerResponse {
  day: string;
  rows: LedgerRow[];
  vdm?: { day: string; window_start: string; note: string };
  global_hold: { value: boolean; reason: string; since: string; lock_version: number };
  runs: { counter: LedgerRunInfo | null; vdm: LedgerRunInfo | null; reconciliation: LedgerRunInfo | null };
  alerts_enabled: boolean;
  // Cap-breach detector + automatic shutoff (operator 2026-08-20). Surfaced so
  // a disabled / detect-only detector is VISIBLE rather than silently inert.
  cap_breach?: {
    enabled: boolean;
    detect_only: boolean;
    threshold_pct: number;
    min_excess: number;
    actor: string;
    note: string;
  };
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

// Snapshot (…/property-ledger/snapshot?vertical=&brand=) — VERIFIED against
// internal/api/property_lane_snapshot.go + internal/worker/lane_snapshot.go.
// The lake path for TODAY only, served from a 5-minute Athena→JSON snapshot
// (it reads a file, so it answers in ~0s). It does NOT arbitrate with /stats
// and never falls back to it: /stats is the Postgres past-tense path over
// mailing_tracking_events, this is present-tense lake truth for the day so far.
//
// Nullability is load-bearing here, so every nullable field is typed nullable:
//   rows: null   → NO snapshot has been captured (property_lane_snapshot.go:143)
//   rows: []     → a snapshot exists and this lane had no activity today
//   open_uniq: null + engagement_available:false → the transport emits no
//                  open/click into the lake at all (kumo). ABSENT, never 0.
interface SnapshotRow {
  organization_id: string;
  vertical: string;
  brand: string;
  isp: string;
  source: string;
  attempted: number;
  delivered: number;
  bounced: number;
  open_uniq: number | null;
  click_uniq: number | null;
  open_events: number | null;
  click_events: number | null;
  engagement_available: boolean;
  campaigns: number;
}
interface SnapshotSourceTotal {
  source: string;
  attempted: number;
  delivered: number;
  bounced: number;
  open_uniq: number | null;
  click_uniq: number | null;
  open_events: number | null;
  click_events: number | null;
  engagement_available: boolean;
}
interface SnapshotResponse {
  available: boolean;
  state: string;              // 'ready' | 'no_snapshot_yet' — there is no third
  message?: string;
  day: string;
  captured_at: string;        // '' in the not-ready state
  age_seconds: number | null;
  vertical: string;
  brand?: string;
  rows: SnapshotRow[] | null;
  source_totals: SnapshotSourceTotal[] | null;
  unmapped: { campaigns: number; events: number };
  source: string;
  storage?: string;
  notes: string[] | null;
  generated_at: string;
}

// Supply (…/property-ledger/supply?domain=…) — VERIFIED against
// internal/api/property_lane_supply.go HandleLaneSupply. The AVAILABLE-DATA
// read: what is physically sitting in partner_clean_queue for this sending
// domain's feeds right now, and where in the cleaning pipeline it is.
//
// Two contract facts that must survive into the UI:
//   * `ready` means EO Verified (1) + Complainer (7) — the validator's
//     markReady path. It is NOT "everything not yet mailed".
//   * supply is a DATASET fact SHARED across the rotation's brands
//     (shared_brands). It is never domain-owned inventory, so a domain cannot
//     plan against ready_total as if it owned it.
// The endpoint 503s while idx_pcq_dataset_status_mailed is still building —
// that is a warming state, surfaced as the server's message, never as zero.
interface SupplyISP { isp: string; ready: number }
interface SupplyFeed {
  dataset_id: string;
  name: string;
  vertical: string;
  status: string;
  daily_cap: number;
  paused_emergency: boolean;
  shared_brands: string[];
  tranche_total: number;
  cleaning: number;
  pending_eo: number;
  eo_in_flight: number;
  ready_total: number;
  ready_by_isp: SupplyISP[];
  held: number;
  suppressed: number;
  dead_letter: number;
  mailed_lifetime: number;
  mailed_today: number;
}
interface SupplyResponse {
  domain: string;
  brand: string;
  sending_domain: string;
  non_ledger: boolean;
  as_of: string;
  denver_day: string;
  ready_semantics: string;
  supply_note: string;
  feeds: SupplyFeed[];
}

// Ingestion (…/property-ledger/ingestion?days=N) — VERIFIED against
// internal/api/property_ledger_ingestion.go. Cold-intake TREND: how many new
// records each sending domain actually took per Denver day. Estate-wide by
// design — sizing a growth ask means comparing domains and spotting the ones
// with no feed at all, so this is NOT scoped to the selection.
//
// `per_day` deliberately excludes TODAY (today is still filling and would drag
// every rate down). `rate_days` is how many complete days it averaged over.
// This is a different question from the supply queue above: intake RATE over
// time vs what is claimable right now.
interface IngestionRow {
  brand: string;
  sending_domain: string;
  by_day: Record<string, number>;
  window_total: number;
  per_day: number;
}
interface IngestionResponse {
  from: string;
  to: string;
  days: string[];
  rate_days: number;
  rows: IngestionRow[];
  unmapped: { vertical: string; records: number }[];
  estate_total: number;
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

/**
 * deriveN — `derive` for nullable numerators. A NULL numerator is an absent
 * measurement, so the rate is null (renders as "—"), never 0%.
 */
const deriveN = (n: number | null | undefined, d: number | null | undefined): number | null =>
  typeof n === 'number' && typeof d === 'number' ? derive(n, d) : null;

/**
 * shareSub — the "· 12.34% of <what>" tail appended to a Stat's sub line.
 * Returns '' when the denominator cannot support a rate, so a missing
 * denominator prints nothing rather than a fabricated 0%.
 */
const shareSub = (n: number | null | undefined, d: number | null | undefined, ofWhat: string): string => {
  const r = deriveN(n, d);
  return r == null ? '' : ` · ${ratePct(r)} of ${ofWhat}`;
};

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

/** "3m ago" for a snapshot age. A null age is UNKNOWN, never "just now". */
const ageLabel = (seconds: number | null | undefined): string => {
  if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds < 0) return UNKNOWN;
  if (seconds < 90) return `${Math.round(seconds)}s ago`;
  const m = Math.round(seconds / 60);
  if (m < 90) return `${m}m ago`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m ago`;
};

/**
 * eng — an engagement cell. When the transport reports no engagement into the
 * lake at all (kumo: engagement_available=false, fields null) this renders
 * "n/a", never 0. A source that CAN report and genuinely saw nothing comes back
 * engagement_available=true with a real 0, and that 0 is shown as 0.
 */
const eng = (v: number | null | undefined, available: boolean): React.ReactNode =>
  available
    ? <span>{num(v)}</span>
    : <span style={{ color: colors.textFaint }} title="This transport emits no open/click rows into the lake (kumo). The value is ABSENT, not zero.">n/a</span>;

/**
 * Derived attempted for the snapshot's delivery rate. The snapshot's raw
 * `attempted` is the lake's raw attempted event, which exists on the SES pipe
 * ONLY (METRIC_CONTRACT §2) — dividing delivered by it produces the >100%
 * rates that contract was written to stop. So the rate divides by
 * delivered + bounced instead, and is labelled and starred as derived.
 */
const derivedAttempted = (delivered: number, bounced: number): number =>
  (Number.isFinite(delivered) ? delivered : 0) + (Number.isFinite(bounced) ? bounced : 0);

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
  // History scope. 'domain' passes brand= to the stats endpoint (server-side —
  // the response has no brand column so it cannot be filtered client-side).
  const [statsScope, setStatsScope] = useState<'domain' | 'drip'>('domain');
  // Which aggregated day row is expanded to its per-ISP breakdown.
  const [openStatDay, setOpenStatDay] = useState<string | null>(null);
  const [snapScope, setSnapScope] = useState<'domain' | 'drip'>('domain');
  // '' = every sending domain in the snapshot. Client-side filter over the
  // snapshot rows; it never refetches, so it cannot change the measurement.
  const [snapBrand, setSnapBrand] = useState<string>('');
  const [editingFeed, setEditingFeed] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [showRoster, setShowRoster] = useState(false);

  const notify = useCallback((s: string) => {
    setNotice(s);
    toast.addToast({ type: 'info', title: 'Drip journey', message: s });
  }, [toast]);

  // 1. Brand ⇄ sending-domain pairs, from the existing Property Ledger read.
  const ledger = useResource<LedgerResponse>(
    'ledger',
    (signal) => getJSON<LedgerResponse>('/api/mailing/pmta-campaign/property-ledger', signal),
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

  // 1b. The INTRODUCTION LEDGER rows for the selected domain — the per-ISP
  // daily_budget/hold cells the drip orchestrator enforces every wave
  // (internal/worker/partner_drip_brand_budgets.go). Same payload as `domains`;
  // no extra fetch. Sorted by spend so the lanes actually moving sit on top,
  // then by cap, so a fully-held domain reads as a wall of holds.
  const ledgerRows = useMemo(() => {
    const rows = (ledger.data?.rows ?? []).filter((r) => r.brand === brand);
    return rows.slice().sort((a, b) => {
      const ai = a.introduced_today ?? -1;
      const bi = b.introduced_today ?? -1;
      if (ai !== bi) return bi - ai;
      if (a.daily_budget !== b.daily_budget) return b.daily_budget - a.daily_budget;
      return a.isp.localeCompare(b.isp);
    });
  }, [ledger.data, brand]);

  // Domain-level roll-up of the ledger. `introduced` is a FLOOR when any cell
  // has no counter row yet — that is surfaced, never silently summed as 0.
  const ledgerTotals = useMemo(() => {
    let budget = 0, introduced = 0, held = 0, uncounted = 0;
    for (const r of ledgerRows) {
      budget += Number.isFinite(r.daily_budget) ? r.daily_budget : 0;
      if (typeof r.introduced_today === 'number') introduced += r.introduced_today;
      else uncounted += 1;
      if (r.hold) held += 1;
    }
    return { budget, introduced, held, uncounted, cells: ledgerRows.length };
  }, [ledgerRows]);

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

  // 4a. TODAY — the lake snapshot. Its own resource, so the slow Postgres
  // /stats read below can never delay or blank this panel. `brand` is optional
  // on the endpoint (property_lane_snapshot.go:107): passing it scopes the
  // server-side per-source rollup to this domain, omitting it rolls up the
  // whole drip. The rollup is always the SERVER's — never recomputed here,
  // because its nullable-engagement accumulation is part of the contract.
  const snapshot = useResource<SnapshotResponse>(
    vertical ? `snapshot:${vertical}:${snapScope === 'domain' ? brand ?? '' : '*'}` : null,
    (signal) => getJSON<SnapshotResponse>(
      `/api/mailing/pmta-campaign/property-ledger/snapshot?vertical=${encodeURIComponent(vertical ?? '')}`
      + (snapScope === 'domain' && brand ? `&brand=${encodeURIComponent(brand)}` : ''),
      signal,
    ),
  );

  // 4b. HISTORY — the scoreboard. SLOW (Postgres, ~10s warm / 25s cold), so it
  // is deliberately a separate resource rendered in a separate panel.
  // 4. History. HandleLaneStats accepts an optional `brand` (property_lane_stats.go:637)
  // and the response carries NO brand column, so scoping MUST happen server-side —
  // the panel cannot filter a whole-drip payload down to one domain after the fact.
  // Default is this domain: the unscoped whole-drip read is the "dump" the operator
  // could not use, and is now an explicit opt-in.
  const stats = useResource<StatsResponse>(
    vertical ? `stats:${vertical}:${days}:${statsScope === 'domain' ? (brand ?? '') : 'all'}` : null,
    (signal) => getJSON<StatsResponse>(
      `/api/mailing/pmta-campaign/property-ledger/stats?vertical=${encodeURIComponent(vertical ?? '')}&days=${days}`
      + (statsScope === 'domain' && brand ? `&brand=${encodeURIComponent(brand)}` : ''),
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

  // 5b. AVAILABLE DATA — the live queue behind those levers, per sending
  // domain. Keyed by `domain` like the throttle read; scoped to the selected
  // drip below so the numbers answer "what can THIS lane still introduce".
  const supply = useResource<SupplyResponse>(
    sendingDomain ? `supply:${sendingDomain}` : null,
    (signal) => getJSON<SupplyResponse>(
      `/api/mailing/pmta-campaign/property-ledger/supply?domain=${encodeURIComponent(sendingDomain ?? '')}`,
      signal,
    ),
  );

  // 5c. Cold-intake TREND, estate-wide (see the type note: sizing an ask means
  // comparing domains, so this is deliberately not scoped to the selection).
  const ingestion = useResource<IngestionResponse>(
    `ingestion:${days}`,
    (signal) => getJSON<IngestionResponse>(
      `/api/mailing/pmta-campaign/property-ledger/ingestion?days=${days}`, signal),
  );

  const ingestionRows = useMemo(
    () => (ingestion.data?.rows ?? []).slice().sort((a, b) => b.per_day - a.per_day),
    [ingestion.data],
  );

  const supplyFeeds = useMemo(
    () => (supply.data?.feeds ?? []).filter((f) => !vertical || f.vertical === vertical),
    [supply.data, vertical],
  );

  // Domain-level roll-up of the queue, plus the per-ISP ready split summed
  // across this drip's feeds — the breakdown of what is actually claimable.
  const supplyTotals = useMemo(() => {
    let tranche = 0, cleaning = 0, pendingEO = 0, inFlight = 0, ready = 0;
    let held = 0, suppressed = 0, deadLetter = 0, mailedToday = 0, cap = 0;
    const byISP = new Map<string, number>();
    const shared = new Set<string>();
    for (const f of supplyFeeds) {
      tranche += f.tranche_total; cleaning += f.cleaning; pendingEO += f.pending_eo;
      inFlight += f.eo_in_flight; ready += f.ready_total; held += f.held;
      suppressed += f.suppressed; deadLetter += f.dead_letter;
      mailedToday += f.mailed_today; cap += f.daily_cap;
      for (const i of f.ready_by_isp ?? []) byISP.set(i.isp, (byISP.get(i.isp) ?? 0) + i.ready);
      for (const b of f.shared_brands ?? []) shared.add(b);
    }
    const isps = Array.from(byISP.entries())
      .map(([isp, r]) => ({ isp, ready: r }))
      .sort((a, b) => b.ready - a.ready);
    return {
      tranche, cleaning, pendingEO, inFlight, ready, held, suppressed, deadLetter,
      mailedToday, cap, isps, shared: Array.from(shared).sort(), feeds: supplyFeeds.length,
    };
  }, [supplyFeeds]);

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

  // Snapshot sub-states. rows===null (no snapshot captured) and rows===[] (a
  // snapshot exists and this lane did nothing today) are DIFFERENT displays.
  const snap = snapshot.data;

  // Every sending domain present in THIS snapshot, for the in-panel filter.
  // Built from the rows themselves so the filter can only ever offer a value
  // that exists in the data — an empty list means the snapshot is single-brand
  // and the filter is hidden rather than shown with one dead option.
  const snapBrands = useMemo(() => {
    const seen = new Set<string>();
    for (const r of snap?.rows ?? []) if (r.brand) seen.add(r.brand);
    return Array.from(seen).sort();
  }, [snap]);

  // Reset the filter whenever the underlying selection changes, so a domain
  // filter can never silently persist onto a different drip's snapshot.
  useEffect(() => { setSnapBrand(''); }, [vertical, brand, snapScope]);

  const snapRows = useMemo(
    () => (snap?.rows ?? [])
      .filter((r) => !snapBrand || r.brand === snapBrand)
      .slice()
      .sort((a, b) => b.delivered - a.delivered),
    [snap, snapBrand],
  );

  /**
   * snapTotals — the per-source aggregate for the rows CURRENTLY SHOWN.
   *
   * This is a faithful client-side replay of the server's
   * laneSnapshotSourceTotals (internal/api/property_lane_snapshot.go:185), so
   * that the aggregate always describes the filtered selection rather than the
   * unfiltered scope. Two rules are copied verbatim and matter:
   *   - there is NO cross-source total (source='app' mirrors ses/pmta);
   *   - a nullable engagement total stays null until a row actually reports it,
   *     so "absent" never collapses into 0.
   */
  const snapTotals = useMemo((): SnapshotSourceTotal[] => {
    const order: string[] = [];
    const by = new Map<string, SnapshotSourceTotal>();
    const add = (cur: number | null, v: number | null | undefined): number | null =>
      typeof v === 'number' ? (cur ?? 0) + v : cur;
    for (const row of snapRows) {
      let t = by.get(row.source);
      if (!t) {
        t = {
          source: row.source, attempted: 0, delivered: 0, bounced: 0,
          open_uniq: null, click_uniq: null, open_events: null, click_events: null,
          engagement_available: false,
        };
        by.set(row.source, t);
        order.push(row.source);
      }
      t.attempted += row.attempted;
      t.delivered += row.delivered;
      t.bounced += row.bounced;
      if (!row.engagement_available) continue;
      t.engagement_available = true;
      t.open_uniq = add(t.open_uniq, row.open_uniq);
      t.click_uniq = add(t.click_uniq, row.click_uniq);
      t.open_events = add(t.open_events, row.open_events);
      t.click_events = add(t.click_events, row.click_events);
    }
    return order.map((k) => by.get(k)!);
  }, [snapRows]);

  const snapScopeLabel = snapScope === 'domain' ? (sendingDomain ?? brand ?? 'this domain') : 'the whole drip (all brands)';
  const snapFilterLabel = snapBrand ? `${snapBrand} only` : snapScopeLabel;

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

  /**
   * statsByDay — one row per Denver day, ISPs folded in, newest first.
   *
   * Rates are DERIVED FROM THE SUMMED COUNTS (opens ÷ delivered for the whole
   * day), never averaged across the per-ISP rates the server sent. Averaging
   * rates would weight a 12-recipient ISP the same as a 400k one; every
   * roll-up on this screen divides totals instead.
   */
  const statsByDay = useMemo(() => {
    const by = new Map<string, { day: string; sent: number; delivered: number; opens: number; clicks: number; isps: StatsRow[] }>();
    for (const r of statRows) {
      let d = by.get(r.day);
      if (!d) { d = { day: r.day, sent: 0, delivered: 0, opens: 0, clicks: 0, isps: [] }; by.set(r.day, d); }
      d.sent += Number.isFinite(r.sent) ? r.sent : 0;
      d.delivered += Number.isFinite(r.delivered_pg) ? r.delivered_pg : 0;
      d.opens += Number.isFinite(r.opens) ? r.opens : 0;
      d.clicks += Number.isFinite(r.clicks) ? r.clicks : 0;
      d.isps.push(r);
    }
    for (const d of by.values()) d.isps.sort((a, b) => b.sent - a.sent);
    return Array.from(by.values()).sort((a, b) => b.day.localeCompare(a.day));
  }, [statRows]);

  // Each day's immediately older day, for the aggregated direction arrows.
  const prevByDay = useMemo(() => {
    const asc = statsByDay.slice().reverse();
    const m = new Map<string, typeof asc[number]>();
    for (let i = 1; i < asc.length; i++) m.set(asc[i].day, asc[i - 1]);
    return m;
  }, [statsByDay]);

  // A day that vanishes from a re-scoped window must not stay "expanded".
  useEffect(() => {
    if (openStatDay && !statsByDay.some((d) => d.day === openStatDay)) setOpenStatDay(null);
  }, [statsByDay, openStatDay]);

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
      return <span style={{ fontSize: 9, color: colors.textFaint }} title="No prior period in the window, or the current rate has no denominator — direction UNKNOWN, not flat.">{UNKNOWN}</span>;
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
                  sub={`next_touch_at already passed${shareSub(j.totals?.due_now, j.totals?.in_flight, 'in flight')}`}
                  color={(j.totals?.due_now ?? 0) > 0 ? colors.warningText : colors.text}
                  title="Rows whose next touch is already due — the backlog the orchestrator will claim from on its next wave. The percentage is of IN FLIGHT: a rising share means the ladder is claiming slower than it enrols."
                />
                <Stat
                  label="Waiting on connectors"
                  value={num(waitingSum)}
                  sub={(edgesReported === expectedEdges
                    ? `sum of all ${edgesReported} reported edges`
                    : `sum of ${edgesReported} of ${expectedEdges} edges — ${expectedEdges - edgesReported} not reported`)
                    + shareSub(waitingSum, j.totals?.in_flight, 'in flight')}
                  color={edgesReported === expectedEdges ? colors.text : colors.warningText}
                  title="Sum of the per-edge waiting counts drawn on the canvas. When edges are missing this is a FLOOR, not the total — and so is its percentage of in-flight."
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

      {/* ── 2. TODAY — the lake snapshot (answers in ~0s) ─────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="Today so far — lake snapshot"
          icon={faClock}
          right={
            <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
              <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}
                title="Scopes the server-side per-source rollup. 'This domain' passes brand= to the endpoint; 'Whole drip' omits it and rolls up every brand on the vertical.">
                Scope
                <select style={selectStyle} value={snapScope}
                  onChange={(e) => setSnapScope(e.target.value === 'drip' ? 'drip' : 'domain')}>
                  <option value="domain">This domain</option>
                  <option value="drip">Whole drip (all brands)</option>
                </select>
              </label>
              {snapBrands.length > 1 && (
                <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}
                  title="Filters the captured snapshot CLIENT-SIDE. It never refetches, so it cannot change the measurement — the cards and the table below both narrow to the domain picked here, and the coverage caveat still describes the whole capture.">
                  Sending domain
                  <select style={selectStyle} value={snapBrand} onChange={(e) => setSnapBrand(e.target.value)}>
                    <option value="">All ({snapBrands.length})</option>
                    {snapBrands.map((b) => <option key={b} value={b}>{b}</option>)}
                  </select>
                </label>
              )}
              {snap?.available && (
                <span
                  style={{ fontSize: 11, color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}
                  title={`Snapshot captured ${snap.captured_at || UNKNOWN} (UTC), covering Denver day ${snap.day}. The worker re-captures every 5 minutes; this is a SNAPSHOT, not a live read.`}
                >
                  as of {ageLabel(snap.age_seconds)} · captured {shortTime(snap.captured_at)} · day {snap.day}
                  {snap.storage ? ` · ${snap.storage}` : ''}
                </span>
              )}
            </div>
          }
        />
        <AsyncPanel
          label="today's lake snapshot"
          res={snapshot}
          isEmpty={!snap}
          emptyTitle="No response from the snapshot endpoint"
          emptyHint="The request resolved without a payload — treat this as unverified, not as a quiet day."
        >
          {snap && !snap.available && (
            <div
              style={{
                fontSize: 12, color: colors.warningText,
                background: alpha(colors.warning, '14'),
                border: `1px solid ${alpha(colors.warning, '44')}`,
                borderRadius: 6, padding: '10px 12px',
              }}
            >
              <div style={{ fontWeight: 700, letterSpacing: 0.5, marginBottom: 4 }}>
                NO SNAPSHOT CAPTURED YET — state: {snap.state || UNKNOWN}
              </div>
              <div style={{ lineHeight: 1.6 }}>
                {snap.message || 'The server reported no snapshot and supplied no message.'}
              </div>
              <div style={{ marginTop: 6, color: colors.textMuted }}>
                No numbers are shown for today. This is an absence of measurement — <b>not</b> a day
                with zero activity.
              </div>
            </div>
          )}

          {snap?.available && snap.rows === null && (
            <div
              style={{
                fontSize: 12, color: colors.warningText,
                background: alpha(colors.warning, '14'),
                border: `1px solid ${alpha(colors.warning, '44')}`,
                borderRadius: 6, padding: '10px 12px',
              }}
            >
              The snapshot reports <b>ready</b> but carries a null row set — the server contract says
              a captured snapshot always returns an array. Treat today as UNMEASURED and check the
              lane_snapshot heartbeat.
            </div>
          )}

          {snap?.available && snap.rows !== null && snap.rows.length === 0 && (
            <EmptyState
              title="No activity for this lane today"
              hint={`The snapshot captured ${shortTime(snap.captured_at)} covers Denver day ${snap.day} and contains no rows for ${snapScopeLabel}. This lane has not mailed today as of that capture — measured, not missing.`}
            />
          )}

          {/* The capture HAS rows but the domain filter excludes them all. That is
              a filter result, not an empty measurement, and must not read as one. */}
          {snap?.available && (snap.rows ?? []).length > 0 && snapRows.length === 0 && (
            <EmptyState
              title={`No rows for ${snapBrand} in this capture`}
              hint={`The snapshot holds ${(snap.rows ?? []).length} row(s) for other sending domains but none for ${snapBrand}. Clear the sending-domain filter to see them — this is the filter excluding data, not the lane being quiet.`}
            />
          )}

          {snap?.available && snapRows.length > 0 && (
            <>
              {/* Per-source totals for the ROWS SHOWN. There is deliberately NO
                  cross-source total: source='app' mirrors ses/pmta. */}
              <div style={{ fontSize: 11, color: colors.heading, fontWeight: 700, letterSpacing: 0.5, marginBottom: 8 }}>
                TODAY, AGGREGATED — {snapFilterLabel.toUpperCase()}
              </div>
              <div style={{ ...cardGrid(230), marginBottom: 12 }}>
                {snapTotals.map((t) => {
                  const den = derivedAttempted(t.delivered, t.bounced);
                  const isMirror = t.source === 'app';
                  return (
                    <div
                      key={t.source}
                      style={{
                        border: `1px solid ${isMirror ? alpha(colors.warning, '44') : colors.hairline}`,
                        borderRadius: 8, padding: '9px 11px',
                        background: isMirror ? alpha(colors.warning, '0d') : 'transparent',
                      }}
                    >
                      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
                        <Pill color={isMirror ? colors.warning : colors.indigo400} style={{ fontSize: 9 }}>
                          {t.source}
                        </Pill>
                        {!t.engagement_available && (
                          <span style={{ fontSize: 9, color: colors.textFaint }} title="This transport emits no open/click rows into the lake.">
                            no engagement in lake
                          </span>
                        )}
                      </div>
                      <div style={{ display: 'flex', gap: 14, flexWrap: 'wrap' }}>
                        <Stat
                          label="Delivered"
                          value={num(t.delivered)}
                          sub={`${ratePct(derive(t.delivered, den))} of delivered+bounced*`}
                          title="Delivery rate denominator is DERIVED (delivered + bounced) — the snapshot's raw attempted exists on the SES pipe only, so dividing by it produces >100% rates (METRIC_CONTRACT §2)."
                        />
                        <Stat
                          label="Bounced (raw)"
                          value={num(t.bounced)}
                          sub="not split H/S"
                          color={colors.textMuted}
                          title="This snapshot carries a single raw 'bounced' event count with NO hard/soft taxonomy (lane_snapshot.go:740). Hard and soft are never summed by this platform — so this number is shown uncoloured and must NOT be read as a hard-bounce figure. Use the Reporting screen for the reclassified split."
                        />
                        <Stat
                          label="Opens (uniq*)"
                          value={eng(t.open_uniq, t.engagement_available)}
                          sub={t.engagement_available
                            ? `${num(t.open_events)} events${shareSub(t.open_uniq, t.delivered, 'delivered*')}`
                            : 'absent, not zero'}
                          title="Sum of PER-CAMPAIGN distinct subscribers — not a lane-level distinct count. The percentage divides by THIS SOURCE'S delivered; a source that carries no delivery rows (app) shows no percentage rather than borrowing another source's denominator."
                        />
                        <Stat
                          label="Clicks (uniq*)"
                          value={eng(t.click_uniq, t.engagement_available)}
                          sub={t.engagement_available
                            ? `${num(t.click_events)} events${shareSub(t.click_uniq, t.delivered, 'delivered*')}`
                            : 'absent, not zero'}
                          title="Sum of PER-CAMPAIGN distinct subscribers — not a lane-level distinct count. The percentage divides by THIS SOURCE'S delivered."
                        />
                        <Stat
                          label="Click-to-open"
                          value={t.engagement_available ? ratePct(deriveN(t.click_uniq, t.open_uniq)) : eng(null, false)}
                          sub="clicks ÷ opens"
                          title="The one engagement rate that needs NO delivery denominator, so it is comparable across every source on this row — including the app mirror, which carries no delivery rows at all. Both sides are per-campaign distinct subscribers (uniq*), and both include machine traffic."
                        />
                      </div>
                      {isMirror && (
                        <div style={{ fontSize: 10, color: colors.warningText, marginTop: 6, lineHeight: 1.5 }}>
                          <b>PG→lake mirror of our OWN pixel/redirect layer</b> (<code>mailing_tracking_events</code>,
                          pushed every 10 min by the <code>pg_to_lake</code> job). Read it as first-party
                          engagement — it is the ONLY open/click view for pmta- and kumo-routed mail,
                          which emit none into the lake.
                          {' '}<b>Delivered and Bounced read 0 here by construction, not by measurement:</b> the
                          current-day mirror emits only <code>opened/clicked/unsubscribed</code>
                          {' '}(agents/jobs/pg_to_lake.py — bounces and deliveries are the accounting pipe's).
                          {' '}<b>Its opens and clicks DO overlap <code>ses</code></b> — measured over the
                          drip lanes' own 3,951 campaigns on 2026-08-19/20, <b>78% of open and 94% of click
                          (campaign × subscriber) pairs appear in BOTH sources</b>. Never add this card to
                          another one, and never read app + ses as separate audiences.
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
              <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 12 }}>
                Totals are shown <b>per source</b> and there is deliberately no combined figure:
                <code> source='app'</code> mirrors ses/pmta, so one summed number would be wrong by
                construction. Pick the source you mean. These cards aggregate exactly the rows in the
                table below — {snapBrand
                  ? <>filtered to <b>{snapBrand}</b>, {snapRows.length} of {(snap.rows ?? []).length} captured rows</>
                  : <>all {snapRows.length} captured rows across {snapBrands.length} sending domain(s)</>}.
              </div>

              <div style={{ overflowX: 'auto' }}>
                <table style={{ ...tableStyle, minWidth: 1180 }}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Brand</th>
                      <th style={thStyle}>ISP</th>
                      <th style={thStyle}>Source</th>
                      <th style={numTh} title="Distinct campaigns contributing to this cell today.">Campaigns</th>
                      <th style={numTh} title="RAW lake attempted events. These exist on the SES pipe ONLY — PMTA emits none (METRIC_CONTRACT §2), so a pmta row reads near zero here. Never use it as a delivery denominator.">
                        Attempted (raw, SES-only)
                      </th>
                      <th style={numTh}>Delivered</th>
                      <th style={numTh} title="Denominator = DERIVED attempted (delivered + bounced) for this row.">
                        Delivered %* (of delivered+bounced)
                      </th>
                      <th style={numTh} title="Raw 'bounced' events — this snapshot carries no hard/soft taxonomy, so the two are NOT split here and this must not be read as a hard-bounce number.">
                        Bounced (raw, not split)
                      </th>
                      <th style={numTh} title="Sum of per-campaign distinct subscribers — not a lane-level distinct. 'n/a' means the transport emits no opens into the lake.">
                        Opens (uniq*)
                      </th>
                      <th style={numTh} title="Opens (uniq*) ÷ this row's OWN delivered. Blank where the source carries no delivery rows (app) — never borrowed from another source.">
                        Open %*
                      </th>
                      <th style={numTh} title="Sum of per-campaign distinct subscribers — not a lane-level distinct. 'n/a' means the transport emits no clicks into the lake.">
                        Clicks (uniq*)
                      </th>
                      <th style={numTh} title="Clicks (uniq*) ÷ this row's OWN delivered. Blank where the source carries no delivery rows (app).">
                        Click %*
                      </th>
                      <th style={numTh} title="Clicks ÷ opens — the only engagement rate that needs no delivery denominator, so it is comparable across every source including the app mirror.">
                        Click-to-open
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {snapRows.map((r) => {
                      const den = derivedAttempted(r.delivered, r.bounced);
                      return (
                        <tr key={`${r.brand}|${r.isp}|${r.source}`}>
                          <td style={tdStyle}>{r.brand}</td>
                          <td style={tdStyle}>{r.isp}</td>
                          <td style={tdStyle}>
                            <Pill color={r.source === 'app' ? colors.warning : colors.indigo400} style={{ fontSize: 9 }}>
                              {r.source}
                            </Pill>
                          </td>
                          <td style={numTd}>{num(r.campaigns)}</td>
                          <td style={numTd}>{num(r.attempted)}</td>
                          <td style={numTd}>{num(r.delivered)}</td>
                          <td style={numTd} title={`delivered ${num(r.delivered)} / (delivered + bounced) ${num(den)}`}>
                            {ratePct(derive(r.delivered, den))}
                          </td>
                          <td style={{ ...numTd, color: colors.textMuted }}>{num(r.bounced)}</td>
                          <td style={numTd} title={r.engagement_available ? `${num(r.open_events)} open events` : undefined}>
                            {eng(r.open_uniq, r.engagement_available)}
                          </td>
                          <td style={numTd} title={`opens ${num(r.open_uniq)} / this row's delivered ${num(r.delivered)}`}>
                            {ratePct(deriveN(r.open_uniq, r.delivered))}
                          </td>
                          <td style={numTd} title={r.engagement_available ? `${num(r.click_events)} click events` : undefined}>
                            {eng(r.click_uniq, r.engagement_available)}
                          </td>
                          <td style={numTd} title={`clicks ${num(r.click_uniq)} / this row's delivered ${num(r.delivered)}`}>
                            {ratePct(deriveN(r.click_uniq, r.delivered))}
                          </td>
                          <td style={numTd} title={`clicks ${num(r.click_uniq)} / opens ${num(r.open_uniq)}`}>
                            {ratePct(deriveN(r.click_uniq, r.open_uniq))}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>

              <div
                style={{ fontSize: 10, color: colors.textFaint, marginTop: 8 }}
                title="Lake activity from campaigns the mapping query could not resolve to a lane. It is DROPPED from every number above rather than attributed to a lane it may not belong to — a coverage caveat, not noise."
              >
                Coverage caveat: {num(snap.unmapped?.campaigns)} campaign(s) ·
                {' '}{num(snap.unmapped?.events)} lake event(s) today did not resolve to any lane and
                are excluded from every number above.
              </div>

              {(snap.notes ?? []).map((n, i) => (
                <div key={i} style={{ fontSize: 10, color: colors.textFaint, marginTop: 4, lineHeight: 1.5 }}>
                  {n}
                </div>
              ))}
            </>
          )}
        </AsyncPanel>
      </Panel>

      {/* ── 2b. THE ENFORCED CAP — the introduction ledger ────────────────── */}
      {/*
        Ported from the Property Ledger screen. This is the ONE thing on that
        screen the drip orchestrator actually reads every wave: per (sending
        domain × ISP), how many NEW-RECORD introductions this domain may absorb
        today, whether the lane is held, and how much of the cap is already
        spent — plus the SES VDM lane scoreboard that justifies moving it.
        Read-only here: every budget/hold/proposal write is a LIVE WRITE and
        stays on the Property Ledger screen behind its own confirmations.
      */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="Introduction ledger — the cap the orchestrator enforces"
          icon={faHourglassHalf}
          right={ledger.data ? (
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
              {(() => {
                const cb = ledger.data.cap_breach;
                if (!cb) return null;
                if (!cb.enabled) {
                  return (
                    <span title="PROPERTY_CAP_BREACH_SHUTOFF_DISABLED is set. Lanes that run away past their cap will NOT be shut off automatically.">
                      <Pill color={colors.warning} style={{ fontSize: 9 }}>auto-shutoff OFF</Pill>
                    </span>
                  );
                }
                if (cb.detect_only) {
                  return (
                    <span title={`PROPERTY_CAP_BREACH_DETECT_ONLY is set — breaches above ${cb.threshold_pct}% of cap are logged but NOTHING is held.`}>
                      <Pill color={colors.warning} style={{ fontSize: 9 }}>auto-shutoff detect-only</Pill>
                    </span>
                  );
                }
                return (
                  <span title={`${cb.note} Threshold ${cb.threshold_pct}% of cap, with an absolute tolerance of ${cb.min_excess} records for normal per-wave overshoot. At most one automatic hold per lane per Denver day, so un-holding is never fought.`}>
                    <Pill color={colors.success} style={{ fontSize: 9 }}>auto-shutoff &gt;{cb.threshold_pct}%</Pill>
                  </span>
                );
              })()}
              <span style={{ fontSize: 11, color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}
                title={`Ledger day is America/Denver. VDM columns cover ${ledger.data.vdm?.window_start || UNKNOWN} → ${ledger.data.vdm?.day || UNKNOWN} (complete UTC days only) and are a DIFFERENT window from the ledger day.`}>
                ledger day {ledger.data.day}
                {ledger.data.vdm?.day ? ` · VDM day ${ledger.data.vdm.day}` : ''}
              </span>
            </span>
          ) : undefined}
        />
        <AsyncPanel
          label="the introduction ledger"
          res={ledger}
          isEmpty={ledgerRows.length === 0}
          emptyTitle="No ledger row for this sending domain"
          emptyHint="partner_drip_brand_budgets has no (brand × ISP) cell for this domain. That is UNGOVERNED, not zero: with no row the overlay leaves the existing welcome-pass cap chain untouched — this domain's drip intro volume is bounded by the other cap systems only."
        >
          {ledger.data?.global_hold?.value && (
            <div style={{
              fontSize: 12, color: colors.dangerText,
              background: alpha(colors.danger, '14'),
              border: `1px solid ${alpha(colors.danger, '44')}`,
              borderRadius: 6, padding: '10px 12px', marginBottom: 12,
            }}>
              <div style={{ fontWeight: 700, letterSpacing: 0.5, marginBottom: 4 }}>
                GLOBAL HOLD IS ON — every cap below is forced to 0
              </div>
              <div style={{ lineHeight: 1.6 }}>
                {ledger.data.global_hold.reason || 'No reason recorded on the flag.'}
                {' '}Set {shortTime(ledger.data.global_hold.since)}. This is the fail-CLOSED emergency
                stop and it outranks the ledger's own kill switch — the per-ISP numbers below are
                what WOULD apply, not what is applying.
              </div>
            </div>
          )}

          <div style={{ ...cardGrid(170), marginBottom: 12 }}>
            <Stat
              label="Cap today"
              value={num(ledgerTotals.budget)}
              sub={`${ledgerTotals.cells} ISP cells`}
              title="Sum of daily_budget across this domain's ISP cells — new-record INTRODUCTIONS (first touches) only. Follow-up touches are not governed by this ledger."
            />
            <Stat
              label="Introduced today"
              value={num(ledgerTotals.introduced)}
              sub={`${ratePct(derive(ledgerTotals.introduced, ledgerTotals.budget))} of cap${ledgerTotals.uncounted > 0 ? ` · FLOOR, ${ledgerTotals.uncounted} cell(s) uncounted` : ''}`}
              color={ledgerTotals.uncounted > 0 ? colors.warningText : colors.text}
              title="Spend counted from partner_clean_queue (mailed_brand, isp_family, mailed_at), materialised by the counter rollup. A cell with no counter row yet is ABSENT, not zero — when any exist this total is a floor and says so."
            />
            <Stat
              label="Headroom"
              value={num(Math.max(0, ledgerTotals.budget - ledgerTotals.introduced))}
              sub="cap − introduced"
              title="What this domain could still introduce today if nothing else binds. Other cap systems (per-wave claim cap, supply release) can bind first — this is a ceiling, not a forecast."
            />
            <Stat
              label="ISP lanes held"
              value={`${ledgerTotals.held} / ${ledgerTotals.cells}`}
              sub={ledgerTotals.held === 0 ? 'none held' : ledgerTotals.held === ledgerTotals.cells ? 'DOMAIN FULLY STOPPED' : 'partially held'}
              color={ledgerTotals.held === 0 ? colors.successText : ledgerTotals.held === ledgerTotals.cells ? colors.dangerText : colors.warningText}
              title="hold = TRUE forces this lane's per-wave cap to 0 with no count query. Every cell held means this domain introduces nothing at all today."
            />
          </div>

          <div style={{ overflowX: 'auto' }}>
            <table style={{ ...tableStyle, minWidth: 1120 }}>
              <thead>
                <tr>
                  <th style={thStyle}>ISP</th>
                  <th style={numTh} title="daily_budget — new-record introductions this (domain × ISP) may absorb per Denver day.">Cap</th>
                  <th style={numTh} title="Introductions counted so far today. Blank = the counter rollup has no cell yet (ABSENT, not zero).">Introduced</th>
                  <th style={numTh} title="Introduced ÷ Cap. Over 100% is possible and real: the cap is applied per wave against the count at that moment, so a large wave can overshoot.">% of cap</th>
                  <th style={numTh} title="Cap − Introduced, floored at 0.">Headroom</th>
                  <th style={thStyle}>Hold</th>
                  <th style={numTh} title="SES VDM sends for this lane yesterday — UNIQUE-count semantics over complete UTC days. A different window and a different population from the Denver-day panels above; the two never arbitrate.">VDM sent (yday)</th>
                  <th style={numTh} title="VDM delivery ÷ send.">Delivered %</th>
                  <th style={numTh} title="VDM unique opens ÷ delivery.">Open %</th>
                  <th style={numTh} title="VDM unique clicks ÷ delivery.">Click %</th>
                  <th style={numTh} title="VDM sends over the trailing 7 complete UTC days. Hover a cell for its 7-day rates.">Sent 7d</th>
                  <th style={thStyle} title="A staged next-day budget change, or a proposal waiting on operator approval. Approving is a LIVE WRITE and stays on the Property Ledger screen.">Pending</th>
                </tr>
              </thead>
              <tbody>
                {ledgerRows.map((r) => {
                  const spent = r.introduced_today;
                  const pctOfCap = deriveN(spent, r.daily_budget);
                  const over = pctOfCap != null && pctOfCap > 1;
                  const stopped = r.hold || r.daily_budget <= 0;
                  // Breach = past the detector's threshold. Mirrors
                  // evalCapBreach (internal/worker/property_cap_breach.go):
                  // ratio AND absolute floor, and only for a POSITIVE declared
                  // budget — a 0/absent cap is never a breach.
                  const cb = ledger.data?.cap_breach;
                  const breached = cb != null && pctOfCap != null && spent != null
                    && r.daily_budget > 0
                    && spent * 100 > r.daily_budget * cb.threshold_pct
                    && spent - r.daily_budget > cb.min_excess;
                  const autoHeld = r.hold && r.updated_by === (cb?.actor || 'cap-breach-detector');
                  return (
                    <tr key={`${r.brand}|${r.isp}`} style={stopped ? { opacity: 0.7 } : undefined}>
                      <td style={tdStyle}>{r.isp}</td>
                      <td style={numTd} title={r.daily_budget <= 0 ? 'Cap 0 — this lane introduces nothing, identical in effect to a hold.' : undefined}>
                        {r.daily_budget <= 0
                          ? <span style={{ color: colors.dangerText, fontWeight: 700 }}>0</span>
                          : num(r.daily_budget)}
                      </td>
                      <td style={numTd} title={r.introduced_as_of ? `counter as of ${shortTime(r.introduced_as_of)}` : 'No counter row for this lane today — the rollup materialises cells periodically. ABSENT, not zero.'}>
                        {spent == null
                          ? <span style={{ color: colors.textFaint }}>no counter yet</span>
                          : num(spent)}
                      </td>
                      <td style={numTd}
                        title={breached
                          ? `BREACH — introduced ${num(spent)} / cap ${num(r.daily_budget)}, past the ${cb?.threshold_pct}% auto-shutoff threshold and the ${cb?.min_excess}-record per-wave tolerance.`
                          : `introduced ${num(spent)} / cap ${num(r.daily_budget)}`}>
                        <span style={breached
                          ? { color: colors.dangerText, fontWeight: 700 }
                          : over ? { color: colors.warningText, fontWeight: 700 } : undefined}>
                          {ratePct(pctOfCap)}
                        </span>
                      </td>
                      <td style={numTd}>
                        {spent == null ? UNKNOWN : num(Math.max(0, r.daily_budget - spent))}
                      </td>
                      <td style={tdStyle}>
                        {r.hold
                          ? <span title={`${r.hold_reason || 'No reason recorded'}${r.held_since ? ` · held since ${shortTime(r.held_since)}` : ''}${autoHeld ? ' · Un-hold on the Property Ledger screen; daily_budget was never modified.' : ''}`}>
                              <Pill color={colors.danger} style={{ fontSize: 9 }}>{autoHeld ? 'auto-held' : 'held'}</Pill>
                              {autoHeld && r.hold_reason && (
                                <div style={{ fontSize: 10, color: colors.dangerText, marginTop: 3, maxWidth: 260, lineHeight: 1.35 }}>
                                  {r.hold_reason}
                                </div>
                              )}
                            </span>
                          : <span style={{ fontSize: 11, color: colors.textFaint }}>—</span>}
                      </td>
                      {r.vdm ? (
                        <>
                          <td style={numTd}>{num(r.vdm.sent)}</td>
                          <td style={numTd} title={`7d: ${ratePct(r.vdm.delivered_pct_7d)} over ${num(r.vdm.sent_7d)} sends`}>{ratePct(r.vdm.delivered_pct)}</td>
                          <td style={numTd} title={`${num(r.vdm.opens)} unique opens · 7d ${ratePct(r.vdm.open_rate_vdm_7d)}`}>{ratePct(r.vdm.open_rate_vdm)}</td>
                          <td style={numTd} title={`${num(r.vdm.clicks)} unique clicks · 7d ${ratePct(r.vdm.click_rate_vdm_7d)}`}>{ratePct(r.vdm.click_rate_vdm)}</td>
                          <td style={numTd} title={`7d: delivered ${ratePct(r.vdm.delivered_pct_7d)} · open ${ratePct(r.vdm.open_rate_vdm_7d)} · click ${ratePct(r.vdm.click_rate_vdm_7d)}`}>{num(r.vdm.sent_7d)}</td>
                        </>
                      ) : (
                        <td style={{ ...tdStyle, color: colors.textFaint, fontSize: 11 }} colSpan={5}
                          title="No complete VDM rows for this lane. VDM covers the ISPs AWS reports on (gmail/yahoo/aol/microsoft/apple/att/cox) — absent coverage, not zero engagement.">
                          no VDM coverage for this lane
                        </td>
                      )}
                      <td style={tdStyle}>
                        {r.pending_budget != null ? (
                          <span style={{ fontSize: 11, color: colors.warningText }}
                            title={`Staged edit — promotes at the Denver day boundary (${r.pending_effective_day || UNKNOWN})`}>
                            → {num(r.pending_budget)} on {r.pending_effective_day || UNKNOWN}
                          </span>
                        ) : r.proposal ? (
                          <span style={{ fontSize: 11, color: colors.indigo200 }}
                            title={`${r.proposal.basis} · proposed ${shortTime(r.proposal.created_at)} · expires ${shortTime(r.proposal.expires_at)}. Approve on the Property Ledger screen — that is a live write.`}>
                            proposal {num(r.proposal.base_budget)} → {num(r.proposal.proposed_budget)}
                          </span>
                        ) : (
                          <span style={{ fontSize: 11, color: colors.textFaint }}>—</span>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>

          <div style={{ display: 'flex', gap: 14, flexWrap: 'wrap', fontSize: 10, color: colors.textFaint, marginTop: 8 }}>
            {(['counter', 'vdm', 'reconciliation'] as const).map((k) => {
              const run = ledger.data?.runs?.[k] ?? null;
              return (
                <span key={k} title={run
                  ? `Last ${k} run — day ${run.day}, status ${run.status}, started ${shortTime(run.started_at)}${run.finished_at ? `, finished ${shortTime(run.finished_at)}` : ', not finished'}`
                  : `No ${k} run row was returned. The numbers this job feeds are of UNKNOWN freshness — treat them as unverified, not as current.`}>
                  {k}: {run ? `${run.status} · ${shortTime(run.started_at)}` : <b>no run reported</b>}
                </span>
              );
            })}
          </div>

          <p style={noteStyle}>
            This is the <b>introduction</b> budget — NEW-RECORD first touches only. Follow-up touches
            on the ladder are not governed by it, so a held lane still mails its existing ladder.
            A lane with <b>no row at all</b> does not appear here and is <b>ungoverned</b> by this
            overlay, which is not the same as capped at 0. The cap is applied per wave as
            <code> min(cap, daily_budget − introduced today)</code>, so <b>% of cap can legitimately
            exceed 100%</b> when a wave overshoots the remaining headroom.
          </p>
          <p style={noteStyle}>
            <b>Read-only here.</b> Budget edits, holds, and proposal approvals are live writes that
            change what mails on the next wave — they stay on the Property Ledger screen behind its
            confirmations. Governed (Kumo) brands are excluded from this ledger entirely;
            <code> partner_property_governor</code> is their single ceiling.
          </p>
        </AsyncPanel>
      </Panel>

      {/* ── 3. PLAN AHEAD — the quota levers ──────────────────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="Quota levers — per-ISP caps for this drip"
          icon={faSlidersH}
          right={throttle.data ? <span style={{ fontSize: 11, color: colors.textFaint }}>as of {shortTime(throttle.data.as_of)}</span> : undefined}
        />

        {/* ── AVAILABLE DATA — the queue the levers act on ──────────────────
            Operator gap, 2026-08-20: the levers were shown with no view of what
            is actually available to spend them on. This is the live
            partner_clean_queue anatomy for THIS sending domain's feeds on THIS
            drip, joined to the introduction ledger so "claimable" and
            "permitted" sit on the same row. */}
        <div style={{ marginBottom: 14 }}>
          <div style={{ fontSize: 11, color: colors.heading, fontWeight: 700, letterSpacing: 0.5, marginBottom: 8 }}>
            AVAILABLE DATA — {(sendingDomain ?? brand ?? 'this domain').toUpperCase()}
            {vertical ? ` · ${vertical}` : ''}
          </div>
          <AsyncPanel
            label="the live supply queue"
            res={supply}
            isEmpty={supplyFeeds.length === 0}
            emptyTitle={supply.data ? 'No feed on this domain belongs to this drip' : 'No supply data'}
            emptyHint="The supply read succeeded but returned no feed whose vertical matches the selected drip. That is a routing fact — this domain has no dataset feeding this drip — not an empty queue."
          >
            <div style={{ ...cardGrid(160), marginBottom: 12 }}>
              <Stat
                label="Ready now"
                value={num(supplyTotals.ready)}
                sub={`claimable${shareSub(supplyTotals.ready, supplyTotals.tranche, 'tranche')}`}
                color={supplyTotals.ready > 0 ? colors.successText : colors.warningText}
                title={supply.data?.ready_semantics ?? 'ready = EO Verified (1) + Complainer (7) — the validator markReady path. NOT "everything unmailed".'}
              />
              <Stat
                label="Due now (ladder)"
                value={num(journey.data?.totals?.due_now)}
                sub="follow-up backlog"
                color={(journey.data?.totals?.due_now ?? 0) > 0 ? colors.warningText : colors.text}
                title="Enrolled rows whose next_touch_at has already passed — the FOLLOW-UP backlog. It is a different population from Ready now: follow-ups are not governed by the introduction ledger and do not consume ready supply."
              />
              <Stat
                label="In cleaning"
                value={num(supplyTotals.cleaning)}
                sub={`pending EO ${num(supplyTotals.pendingEO)} · in flight ${num(supplyTotals.inFlight)}`}
                title="Records inside the EmailOversight validation pipeline. These are NOT claimable and cannot be mailed — a large number here with a small Ready is a validator throughput problem, not a data-supply problem."
              />
              <Stat
                label="Mailed today"
                value={num(supplyTotals.mailedToday)}
                sub={supplyTotals.cap > 0
                  ? `${ratePct(derive(supplyTotals.mailedToday, supplyTotals.cap))} of release cap ${num(supplyTotals.cap)}`
                  : 'no release cap set on these feeds'}
                title="First touches stamped today across this drip's feeds. The percentage is of the SUPPLY-RELEASE daily cap (partner_datasets.daily_cap), which is a different cap system from the per-ISP claim caps below and from the introduction ledger."
              />
              <Stat
                label="Tranche total"
                value={num(supplyTotals.tranche)}
                sub={`held ${num(supplyTotals.held)} · suppressed ${num(supplyTotals.suppressed)} · dead ${num(supplyTotals.deadLetter)}`}
                title="Every record in this drip's datasets, whatever its state. Held rows are parked by the release cap and become ready on later days; suppressed and dead-letter rows never will."
              />
            </div>

            {supplyTotals.isps.length > 0 && (
              <div style={{ overflowX: 'auto', marginBottom: 10 }}>
                <table style={{ ...tableStyle, minWidth: 720 }}>
                  <thead>
                    <tr>
                      <th style={thStyle}>ISP</th>
                      <th style={numTh} title="Claimable records sitting in the queue for this ISP right now, summed across this drip's feeds.">Ready (in queue)</th>
                      <th style={numTh} title="Share of this drip's total ready supply.">% of ready</th>
                      <th style={numTh} title="Today's remaining introduction allowance for this (sending domain × ISP) from the ledger above. '—' means no ledger row: UNGOVERNED by that overlay, not capped at 0.">Ledger headroom</th>
                      <th style={thStyle} title="What actually binds this ISP first — the smaller of supply and permission.">Binding constraint</th>
                    </tr>
                  </thead>
                  <tbody>
                    {supplyTotals.isps.map((row) => {
                      const led = ledgerRows.find((l) => l.isp === row.isp) ?? null;
                      const headroom = led && typeof led.introduced_today === 'number'
                        ? Math.max(0, led.daily_budget - led.introduced_today)
                        : null;
                      const stopped = !!led && (led.hold || led.daily_budget <= 0);
                      let binding: React.ReactNode = <span style={{ color: colors.textFaint }}>unknown</span>;
                      if (stopped) binding = <span style={{ color: colors.dangerText, fontWeight: 700 }}>ledger STOP</span>;
                      else if (!led) binding = <span style={{ color: colors.textMuted }}>supply (ungoverned)</span>;
                      else if (headroom == null) binding = <span style={{ color: colors.textFaint }}>counter not in yet</span>;
                      else if (headroom < row.ready) binding = <span style={{ color: colors.warningText }}>ledger cap</span>;
                      else binding = <span style={{ color: colors.textMuted }}>supply</span>;
                      return (
                        <tr key={row.isp} style={stopped ? { opacity: 0.7 } : undefined}>
                          <td style={tdStyle}>{row.isp}</td>
                          <td style={numTd}>{num(row.ready)}</td>
                          <td style={numTd}>{ratePct(derive(row.ready, supplyTotals.ready))}</td>
                          <td style={numTd} title={led
                            ? `cap ${num(led.daily_budget)} − introduced ${num(led.introduced_today)}`
                            : 'No partner_drip_brand_budgets row for this (domain × ISP).'}>
                            {stopped
                              ? <span style={{ color: colors.dangerText, fontWeight: 700 }}>0 (held)</span>
                              : headroom == null ? UNKNOWN : num(headroom)}
                          </td>
                          <td style={tdStyle}>{binding}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}

            <p style={noteStyle}>
              {supply.data?.supply_note}
              {supplyTotals.shared.length > 1 && (
                <> This drip's supply is shared with <b>{supplyTotals.shared.join(', ')}</b> — a ready
                record counted here can be claimed by any of them, so it is not this domain's to plan against.</>
              )}
              {' '}<b>Ready and Due now are different populations</b>: Ready is unmailed NEW data awaiting a
              first touch and is governed by the introduction ledger; Due now is already-enrolled
              subscribers awaiting a FOLLOW-UP touch, which the ledger does not govern.
            </p>
          </AsyncPanel>
        </div>

        {/* ── COLD INTAKE TREND — the denominator for any growth ask ────────
            docs/JAOS/drip-lanes.md §5: net every ask against standing intake.
            A lane already taking 10k/day needs 40k cleaned to reach 50k, not 50k.
            Ported here 2026-08-20 when the Property Ledger tab was retired. */}
        <div style={{ marginBottom: 14 }}>
          <div style={{ fontSize: 11, color: colors.heading, fontWeight: 700, letterSpacing: 0.5, marginBottom: 8 }}>
            COLD INTAKE — NEW RECORDS PER DAY, BY SENDING DOMAIN (ESTATE-WIDE)
          </div>
          <AsyncPanel
            label="the cold-intake trend"
            res={ingestion}
            isEmpty={ingestionRows.length === 0}
            emptyTitle="No cold intake recorded in this window"
            emptyHint="The ingestion read succeeded and returned no rows — no sending domain took a new record in the window. Measured, not missing."
          >
            {ingestion.data && (
              <>
                <div style={{ ...cardGrid(180), marginBottom: 10 }}>
                  <Stat
                    label="This domain's rate"
                    value={num(ingestionRows.find((r) => r.brand === brand)?.per_day)}
                    sub={`new records/day${ingestionRows.some((r) => r.brand === brand) ? '' : ' — NO FEED'}`}
                    color={ingestionRows.some((r) => r.brand === brand) ? colors.successText : colors.warningText}
                    title="Standing intake for the selected sending domain, averaged over COMPLETE days only. Subtract this from any growth ask before requesting cleaned records — the lane is already taking this much unaided. A domain absent from the table has NO feed delivering to it."
                  />
                  <Stat
                    label="Estate total"
                    value={num(ingestion.data.estate_total)}
                    sub={`over ${ingestion.data.from} → ${ingestion.data.to}`}
                    title="Every new record taken across the estate in the window."
                  />
                  <Stat
                    label="Rate basis"
                    value={`${num(ingestion.data.rate_days)} days`}
                    sub="today EXCLUDED"
                    title="Per-day rates average over COMPLETE Denver days only. Today is still filling and would drag every rate down, so it is excluded from the divisor while still appearing in the by-day columns."
                  />
                  <Stat
                    label="Domains with a feed"
                    value={`${ingestionRows.length} / ${domains.length}`}
                    sub={ingestionRows.length < domains.length ? `${domains.length - ingestionRows.length} taking nothing` : 'all fed'}
                    color={ingestionRows.length < domains.length ? colors.warningText : colors.successText}
                    title="Roster domains that took at least one new record in the window. A domain absent here is not slow — it has no feed at all, which no cap change will fix."
                  />
                </div>

                <div style={{ overflowX: 'auto' }}>
                  <table style={{ ...tableStyle, minWidth: 720 }}>
                    <thead>
                      <tr>
                        <th style={thStyle}>Sending domain</th>
                        <th style={numTh} title="New records per COMPLETE day. This is the number to net a growth ask against.">Per day</th>
                        <th style={numTh} title="Total new records across the whole window.">Window</th>
                        {(ingestion.data.days ?? []).map((d) => (
                          <th key={d} style={{ ...numTh, opacity: d === denverToday() ? 0.55 : 1 }}
                            title={d === denverToday() ? `${d} — TODAY, still filling; excluded from the per-day rate` : d}>
                            {d.slice(5)}
                          </th>
                        ))}
                      </tr>
                    </thead>
                    <tbody>
                      {ingestionRows.map((r) => {
                        const isSel = r.brand === brand;
                        return (
                          <tr key={r.brand} style={isSel ? { background: alpha(colors.indigo400, '14') } : undefined}>
                            <td style={{ ...tdStyle, fontWeight: isSel ? 700 : 400 }}>
                              {r.sending_domain || r.brand}
                            </td>
                            <td style={{ ...numTd, fontWeight: 700 }}>{num(r.per_day)}</td>
                            <td style={numTd}>{num(r.window_total)}</td>
                            {(ingestion.data?.days ?? []).map((d) => (
                              <td key={d} style={{ ...numTd, opacity: d === denverToday() ? 0.55 : 1 }}>
                                {num(r.by_day?.[d] ?? 0)}
                              </td>
                            ))}
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>

                {(ingestion.data.unmapped ?? []).length > 0 && (
                  <div style={{ fontSize: 10, color: colors.textFaint, marginTop: 8 }}
                    title="Ingested records whose vertical does not resolve to a roster sending domain. They are real intake that no domain is credited with — a routing gap, not noise.">
                    Unmapped intake:{' '}
                    {(ingestion.data.unmapped ?? []).map((u) => `${u.vertical} ${num(u.records)}`).join(' · ')}
                  </div>
                )}

                <p style={noteStyle}>
                  <b>Net every growth ask against these rates</b> (drip-lanes doctrine §5): a lane
                  already taking 10k/day needs 40k cleaned to reach 50k, not 50k. A domain missing
                  from this table has <b>no feed</b> — no cap change will move it, it needs a feed
                  assigned on the roster. This is intake RATE over time; the block above is what is
                  claimable right now. Different questions, different denominators.
                </p>
              </>
            )}
          </AsyncPanel>
        </div>

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

      {/* ── 4. HISTORY — the scoreboard (SLOW: Postgres) ──────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="History — by day, expandable to ISP (Postgres)"
          icon={faChartLine}
          right={
            <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
              <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}
                title="Scoped SERVER-SIDE: 'This domain' passes brand= to the stats endpoint. The response carries no brand column, so a whole-drip read cannot be narrowed afterwards — changing this refetches.">
                Scope
                <select style={selectStyle} value={statsScope}
                  onChange={(e) => setStatsScope(e.target.value === 'drip' ? 'drip' : 'domain')}>
                  <option value="domain">This domain</option>
                  <option value="drip">Whole drip (all brands)</option>
                </select>
              </label>
              <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}>
                Window
                <select style={selectStyle} value={days} onChange={(e) => setDays(Number(e.target.value))}>
                  <option value={7}>7 days</option>
                  <option value={14}>14 days</option>
                  <option value={30}>30 days</option>
                </select>
              </label>
            </div>
          }
        />
        <AsyncPanel
          label="the history scoreboard (Postgres — 10-25s)"
          res={stats}
          isEmpty={statRows.length === 0}
          emptyTitle="No sends recorded for this drip in the window"
          emptyHint={`The stats read succeeded and returned zero rows for the last ${days} days${statsScope === 'domain' ? ` scoped to ${sendingDomain ?? brand ?? 'this domain'}` : ' across the whole drip'} — built empty, not failed. Widen the window, or switch Scope to the whole drip to see whether another domain carried this lane.`}
        >
          <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }}>
            One row per Denver day{statsScope === 'domain' ? <> for <b>{sendingDomain ?? brand}</b></> : <> across <b>every domain on this drip</b></>}.
            {' '}Click a day to expand its per-ISP breakdown. Day rates are derived from that day's
            summed counts, never averaged across the ISP rates.
          </div>
          <div style={{ overflowX: 'auto' }}>
            <table style={{ ...tableStyle, minWidth: 1000 }}>
              <thead>
                <tr>
                  <th style={thStyle}>Day (Denver)</th>
                  <th style={numTh} title="Distinct ISP lanes that recorded a send that day. Click the day to see them.">ISPs</th>
                  <th style={numTh} title="Send events recorded across every ISP that day.">Sent</th>
                  <th style={numTh} title="PG tracking-event delivered count — the per-campaign delivery proxy (METRIC_CONTRACT §1). PG confirmation lags and under-counts (Microsoft ~30%); lake delivery truth may read higher.">
                    Delivered* (PG)
                  </th>
                  <th style={numTh} title="Delivered* ÷ Sent for the whole day. A CONFIRMATION rate, not a delivery rate: it is a floor and reads ~30% low at Microsoft.">
                    Confirmed % (of sent)
                  </th>
                  <th style={numTh} title="Raw opens — machine/MPP traffic included (METRIC_CONTRACT §6). Not 'people opened'.">
                    Opens (raw)
                  </th>
                  <th style={numTh} title="The day's total opens ÷ the day's total Delivered*. NOT an average of the per-ISP rates.">
                    Open % (of delivered)
                  </th>
                  <th style={numTh} title="Raw clicks — machine clicks included.">Clicks (raw)</th>
                  <th style={numTh} title="The day's total clicks ÷ the day's total Delivered*. NOT an average of the per-ISP rates.">
                    Click % (of delivered)
                  </th>
                  <th style={thStyle} title="Change vs the previous day in the window. '—' means there is no prior day, which is not the same as flat.">
                    vs prior day
                  </th>
                </tr>
              </thead>
              <tbody>
                {statsByDay.map((d) => {
                  const prev = prevByDay.get(d.day) ?? null;
                  const openRate = derive(d.opens, d.delivered);
                  const clickRate = derive(d.clicks, d.delivered);
                  const expanded = openStatDay === d.day;
                  return (
                    <React.Fragment key={d.day}>
                      <tr
                        onClick={() => setOpenStatDay(expanded ? null : d.day)}
                        style={{ cursor: 'pointer', background: expanded ? alpha(colors.indigo400, '14') : undefined }}
                        title={expanded ? 'Collapse the per-ISP breakdown' : 'Expand this day to its per-ISP rows'}
                      >
                        <td style={{ ...tdStyle, fontWeight: 700 }}>
                          <span style={{ color: colors.indigo200, marginRight: 6, display: 'inline-block', width: 10 }}>
                            {expanded ? '▾' : '▸'}
                          </span>
                          {d.day}
                        </td>
                        <td style={numTd}>{num(d.isps.length)}</td>
                        <td style={numTd}>{num(d.sent)}</td>
                        <td style={numTd}>{num(d.delivered)}</td>
                        <td style={numTd} title={`delivered ${num(d.delivered)} / sent ${num(d.sent)}`}>
                          {ratePct(derive(d.delivered, d.sent))}
                        </td>
                        <td style={numTd}>{num(d.opens)}</td>
                        <td style={numTd} title={`opens ${num(d.opens)} / delivered ${num(d.delivered)}`}>{ratePct(openRate)}</td>
                        <td style={numTd}>{num(d.clicks)}</td>
                        <td style={numTd} title={`clicks ${num(d.clicks)} / delivered ${num(d.delivered)}`}>{ratePct(clickRate)}</td>
                        <td style={tdStyle}>
                          <span style={{ display: 'inline-flex', gap: 8 }}>
                            <span title="Day click-rate direction vs the previous day.">
                              clk <Direction cur={clickRate ?? NaN} prev={prev ? derive(prev.clicks, prev.delivered) : null} unit="pp" />
                            </span>
                            <span title="Day Delivered* direction vs the previous day.">
                              dlv <Direction cur={d.delivered} prev={prev ? prev.delivered : null} unit="n" />
                            </span>
                          </span>
                        </td>
                      </tr>

                      {expanded && d.isps.map((r) => {
                        const ispPrev = prevByIspDay.get(`${r.isp}|${r.day}`) ?? null;
                        const openDerived = derive(r.opens, r.delivered_pg);
                        const clickDerived = derive(r.clicks, r.delivered_pg);
                        const openMismatch = openDerived != null && Number.isFinite(r.open_rate) && Math.abs(openDerived - r.open_rate) > 0.005;
                        const clickMismatch = clickDerived != null && Number.isFinite(r.click_rate) && Math.abs(clickDerived - r.click_rate) > 0.005;
                        return (
                          <tr key={`${r.day}|${r.isp}`} style={{ background: alpha(colors.indigo400, '0d') }}>
                            <td style={{ ...tdStyle, paddingLeft: 26, color: colors.textMuted }}>{r.isp}</td>
                            <td style={numTd} title="Share of this day's sends that went to this ISP.">
                              {ratePct(derive(r.sent, d.sent))}
                            </td>
                            <td style={numTd}>{num(r.sent)}</td>
                            <td style={numTd}>{num(r.delivered_pg)}</td>
                            <td style={numTd} title={`delivered ${num(r.delivered_pg)} / sent ${num(r.sent)}`}>
                              {ratePct(derive(r.delivered_pg, r.sent))}
                            </td>
                            <td style={numTd}>{num(r.opens)}</td>
                            <td style={numTd} title={openMismatch ? `Server open_rate ${ratePct(r.open_rate)} does not reconcile with opens/delivered shown here (${ratePct(openDerived)}) — the server used a different denominator.` : `opens ${num(r.opens)} / delivered ${num(r.delivered_pg)}`}>
                              {ratePct(r.open_rate)}{openMismatch && <span style={{ color: colors.warningText }}>*</span>}
                            </td>
                            <td style={numTd}>{num(r.clicks)}</td>
                            <td style={numTd} title={clickMismatch ? `Server click_rate ${ratePct(r.click_rate)} does not reconcile with clicks/delivered shown here (${ratePct(clickDerived)}) — the server used a different denominator.` : `clicks ${num(r.clicks)} / delivered ${num(r.delivered_pg)}`}>
                              {ratePct(r.click_rate)}{clickMismatch && <span style={{ color: colors.warningText }}>*</span>}
                            </td>
                            <td style={tdStyle}>
                              <span style={{ display: 'inline-flex', gap: 8, fontSize: 10 }}>
                                <span title="This ISP's click-rate direction vs its own prior day.">
                                  clk <Direction cur={r.click_rate} prev={ispPrev ? ispPrev.click_rate : null} unit="pp" />
                                </span>
                                <span title="This ISP's Delivered* direction vs its own prior day.">
                                  dlv <Direction cur={r.delivered_pg} prev={ispPrev ? ispPrev.delivered_pg : null} unit="n" />
                                </span>
                              </span>
                            </td>
                          </tr>
                        );
                      })}
                    </React.Fragment>
                  );
                })}
              </tbody>
              <tfoot>
                <tr>
                  <td style={{ ...tdStyle, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }} colSpan={2}>
                    Total ({days}d, {statsByDay.length} day(s))
                  </td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }}>{num(statTotals.sent)}</td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }}>{num(statTotals.delivered)}</td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }} title="Total Delivered* / total Sent across the window.">
                    {ratePct(derive(statTotals.delivered, statTotals.sent))}
                  </td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }}>{num(statTotals.opens)}</td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }} title="Total opens / total Delivered* across the window.">
                    {ratePct(derive(statTotals.opens, statTotals.delivered))}
                  </td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }}>{num(statTotals.clicks)}</td>
                  <td style={{ ...numTd, fontWeight: 700, borderTop: `1px solid ${colors.panelBorderStrong}` }} title="Total clicks / total Delivered* across the window.">
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
          <p style={noteStyle}>
            <b>This panel and “Today so far” are two different stores and do not arbitrate.</b> This
            one is Postgres (<code>mailing_tracking_events</code>, past-tense event types, delivery
            <i>ingestion</i> counts); the snapshot above is the Athena lake (present-tense event
            types, lake delivery truth, day-so-far only). They will not tie out exactly, and neither
            falls back to the other. This read is the slow one — 10–25s — which is why it loads
            independently and never gates the snapshot panel.
          </p>
        </AsyncPanel>
      </Panel>

      {/* ── 5. STRUCTURE — roster membership ──────────────────────────────── */}
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
