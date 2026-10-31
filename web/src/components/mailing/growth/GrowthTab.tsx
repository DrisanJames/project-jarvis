// Growth — the daily send-program growth series.
//
// One row per Denver day for a (sending domain × mailbox provider) slice, with
// day-over-day changes. Each day expands into its per-ISP breakdown, or
// per-sending-domain when a provider filter is applied. Dates, brand and provider
// come from the Reporting page's one filter.
//
// Backend (READ ONLY, via apiFetch):
//   GET /api/mailing/growth/daily?from=&to=&sending_domain=&isp=
//   GET /api/mailing/growth/day?day=&sending_domain=&isp=&by=isp|sending_domain
//
// Both read mailing_growth_daily (worker.GrowthRollupWorker), keyed by root
// domain, so this tab works without the lake read layer.
//
// Open and click rates are EVENTS ÷ delivered and can pass 100%. Hard and soft
// bounces are never summed.

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faArrowUp, faArrowDown, faMinus, faDownload,
  faChartLine, faTable, faChevronRight, faChevronDown,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Area, Line, XAxis, YAxis,
  CartesianGrid, Tooltip, Legend,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';
import { colors as theme, thStyle, tdStyle, numTd, numTh, tableStyle, btnStyle } from '../shared/theme';
import { Panel, SectionHeader, Stat, SectionError, EmptyState } from '../shared/ui';
import type { AppliedLakeFilters } from '../shared/filters';
import { brandApex } from '../reporting/cube';

// Series palette — the shared chart tones (PORTAL_DESIGN_SYSTEM §2).
const C_VOLUME = 'var(--j-violet)'; // indigo — delivered
const C_OPEN = 'var(--j-cyan)';   // cyan   — open rate
const C_CLICK = 'var(--j-violet)';  // violet — click rate
const C_ACTION = 'var(--j-success)'; // emerald— action-click rate
const C_COMPLAINT = 'var(--j-danger)';

// ═══════════════════════════════════════════════════════════════════════════
// TYPES (match backend JSON keys exactly — internal/api/growth_daily.go)
// ═══════════════════════════════════════════════════════════════════════════

interface GrowthRow {
  day: string;
  delivered: number;
  attempted: number;
  hard_bounce: number;
  soft_bounce: number;
  reputation_block: number;
  complaints: number;
  unsubs: number;
  open_events: number;
  open_subs: number;
  click_events: number;
  click_subs: number;
  action_click_subs: number;
  delivery_pct: number;
  open_pct: number;
  click_pct: number;
  action_click_pct: number;
  complaint_pct: number;
  unsub_pct: number;
  hard_pct: number;
  soft_pct: number;
  delta_delivered: number | null;
  delta_delivered_pct: number | null;
  delta_open_pct: number | null;
  delta_click_pct: number | null;
}

interface GrowthTotals {
  delivered: number; attempted: number; hard_bounce: number; soft_bounce: number;
  reputation_block: number; complaints: number; unsubs: number;
  open_events: number; click_events: number;
  action_click_subs: number; open_pct: number; click_pct: number;
  action_click_pct: number; complaint_pct: number; unsub_pct: number; days: number;
}

interface GrowthResponse {
  version: string;
  from: string;
  to: string;
  sending_domain: string;
  isp: string;
  rows: GrowthRow[];
  totals: GrowthTotals;
  coverage: { last_computed_at: string };
}

// Single-day drill-down (internal/api/growth_daily.go HandleDayBreakdown).
interface BreakdownRow {
  key: string;
  delivered: number;
  attempted: number;
  hard_bounce: number;
  soft_bounce: number;
  reputation_block: number;
  complaints: number;
  unsubs: number;
  open_events: number;
  open_subs: number;
  click_events: number;
  click_subs: number;
  action_click_subs: number;
  share_pct: number;
  delivery_pct: number;
  open_pct: number;
  click_pct: number;
  action_click_pct: number;
  complaint_pct: number;
  unsub_pct: number;
  hard_pct: number;
  soft_pct: number;
}

interface BreakdownResponse {
  version: string;
  day: string;
  by: 'isp' | 'sending_domain';
  rows: BreakdownRow[];
}

type BreakdownState =
  | { status: 'loading' }
  | { status: 'error'; message: string }
  | { status: 'ready'; data: BreakdownResponse };

// ═══════════════════════════════════════════════════════════════════════════
// FORMATTING
// ═══════════════════════════════════════════════════════════════════════════

const nf = new Intl.NumberFormat('en-US');
const fmtInt = (n: number | null | undefined) => (n == null ? '—' : nf.format(n));
const fmtPct = (n: number | null | undefined, dp = 2) =>
  n == null ? '—' : `${n.toFixed(dp)}%`;
const fmtDay = (d: string) => {
  // 'YYYY-MM-DD' → 'Jul 29' — parsed as UTC noon so no timezone slips a day.
  const t = new Date(`${d}T12:00:00Z`);
  return t.toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' });
};

// deltaCell renders a signed percentage with the direction glyph. `goodUp`
// flips the color semantics for metrics where a rise is bad (complaints).
const DeltaCell: React.FC<{ value: number | null; goodUp?: boolean }> = ({ value, goodUp = true }) => {
  if (value == null) {
    return <span style={{ color: theme.textFaint }} title="No day before this one in the range">n/a</span>;
  }
  const flat = Math.abs(value) < 0.005;
  const up = value > 0;
  const good = flat ? null : goodUp === up;
  const color = flat ? theme.textMuted : good ? theme.successText : theme.dangerText;
  return (
    <span style={{ color, whiteSpace: 'nowrap' }}>
      <FontAwesomeIcon icon={flat ? faMinus : up ? faArrowUp : faArrowDown} style={{ fontSize: 9, marginRight: 4 }} />
      {value > 0 ? '+' : ''}{value.toFixed(2)}%
    </span>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// CSV EXPORT — the same numbers the table shows, so the spreadsheet the
// operator kept by hand can be replaced by a download instead of retyping.
// ═══════════════════════════════════════════════════════════════════════════

const CSV_COLUMNS: Array<[string, (r: GrowthRow) => string | number]> = [
  ['Date', (r) => r.day],
  ['Delivered', (r) => r.delivered],
  ['Delivered Δ', (r) => r.delta_delivered ?? ''],
  ['Delivered Δ%', (r) => r.delta_delivered_pct ?? ''],
  ['Open rate %', (r) => r.open_pct],
  ['Open rate Δ%', (r) => r.delta_open_pct ?? ''],
  ['Click rate %', (r) => r.click_pct],
  ['Click rate Δ%', (r) => r.delta_click_pct ?? ''],
  ['Action clicks', (r) => r.action_click_subs],
  ['Action click %', (r) => r.action_click_pct],
  ['Complaints', (r) => r.complaints],
  ['Complaint %', (r) => r.complaint_pct],
  ['Unsubs', (r) => r.unsubs],
  ['Unsub %', (r) => r.unsub_pct],
  ['Hard bounces', (r) => r.hard_bounce],
  ['Soft bounces', (r) => r.soft_bounce],
  ['Reputation blocks', (r) => r.reputation_block],
  ['Open events', (r) => r.open_events],
  ['Open mailboxes', (r) => r.open_subs],
  ['Click events', (r) => r.click_events],
  ['Attempted', (r) => r.attempted],
];

function toCSV(rows: GrowthRow[]): string {
  const head = CSV_COLUMNS.map(([h]) => h).join(',');
  const body = rows.map((r) => CSV_COLUMNS.map(([, get]) => get(r)).join(',')).join('\n');
  return `${head}\n${body}\n`;
}

function downloadCSV(name: string, csv: string) {
  const blob = new Blob([csv], { type: 'text/csv;charset=utf-8;' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

// ═══════════════════════════════════════════════════════════════════════════
// DAY DRILL-DOWN — the sub-table a day row expands into. Same column
// conventions as the parent table; adds "Share" (slice ÷ day delivered).
// ═══════════════════════════════════════════════════════════════════════════

const OPEN_TITLE = 'Every open out of delivered. Can pass 100%.';
const CLICK_TITLE = 'Every click out of delivered.';
const ACTION_TITLE = 'People who clicked a real link. Each person counts once.';

const BreakdownSubTable: React.FC<{ state: BreakdownState }> = ({ state }) => {
  if (state.status === 'loading') {
    return (
      <div style={{ color: theme.textMuted, padding: '10px 4px', fontSize: 12 }}>
        <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 8 }} /> Loading…
      </div>
    );
  }
  if (state.status === 'error') {
    return (
      <div style={{ color: theme.dangerText, padding: '10px 4px', fontSize: 12 }}>
        Could not load: {state.message}. Close and open the day to try again.
      </div>
    );
  }
  const { data } = state;
  const dimLabel = data.by === 'isp' ? 'Provider' : 'Brand';
  if (data.rows.length === 0) {
    return (
      <div style={{ color: theme.textMuted, padding: '10px 4px', fontSize: 12 }}>
        Nothing to show for this day.
      </div>
    );
  }
  return (
    <table style={{ ...tableStyle, margin: '4px 0' }}>
      <thead>
        <tr>
          <th style={thStyle}>{dimLabel}</th>
          <th style={numTh}>Delivered</th>
          <th style={numTh} title="Share of the day's delivered">Share</th>
          <th style={numTh} title={OPEN_TITLE}>Open %</th>
          <th style={numTh} title={CLICK_TITLE}>Click %</th>
          <th style={numTh} title={ACTION_TITLE}>Action clicks</th>
          <th style={numTh} title="Action clicks out of delivered">Action %</th>
          <th style={numTh}>Complaints</th>
          <th style={numTh} title="Out of delivered">Complaint %</th>
          <th style={numTh}>Unsubscribes</th>
          <th style={numTh}>Hard bounce</th>
          <th style={numTh}>Soft bounce</th>
        </tr>
      </thead>
      <tbody>
        {data.rows.map((b) => (
          <tr key={b.key}>
            <td style={{ ...tdStyle, whiteSpace: 'nowrap', color: theme.heading }}>{b.key}</td>
            <td style={numTd}>{fmtInt(b.delivered)}</td>
            <td style={numTd}>{fmtPct(b.share_pct, 1)}</td>
            <td style={{ ...numTd, color: C_OPEN }}>{fmtPct(b.open_pct)}</td>
            <td style={{ ...numTd, color: C_CLICK }}>{fmtPct(b.click_pct)}</td>
            <td style={{ ...numTd, color: C_ACTION }}>{fmtInt(b.action_click_subs)}</td>
            <td style={numTd}>{fmtPct(b.action_click_pct)}</td>
            <td style={numTd}>{fmtInt(b.complaints)}</td>
            <td style={numTd}>{fmtPct(b.complaint_pct)}</td>
            <td style={numTd}>{fmtInt(b.unsubs)}</td>
            <td style={{ ...numTd, color: b.hard_bounce > 0 ? theme.danger : theme.textMuted }}>{fmtInt(b.hard_bounce)}</td>
            <td style={{ ...numTd, color: b.soft_bounce > 0 ? theme.warning : theme.textMuted }}>{fmtInt(b.soft_bounce)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB
// ═══════════════════════════════════════════════════════════════════════════

export const GrowthTab: React.FC<{ applied: AppliedLakeFilters }> = ({ applied }) => {
  const { from, to } = applied;
  // The rollup keys on the root domain, so a sub domain reads as its root.
  const domain = brandApex(applied.brand);
  const isp = applied.ispGroup.trim().toLowerCase();

  const [data, setData] = useState<GrowthResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [chartMetric, setChartMetric] = useState<'rates' | 'volume'>('rates');

  // Day drill-down: which day is expanded + a per-day cache of its breakdown.
  // Both reset whenever the applied filters change — a cached breakdown for a
  // different slice would silently disagree with the row above it.
  const [expandedDay, setExpandedDay] = useState<string | null>(null);
  const [breakdowns, setBreakdowns] = useState<Record<string, BreakdownState>>({});

  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async () => {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setLoading(true);
    setError('');
    try {
      const qs = new URLSearchParams({ from, to });
      if (domain) qs.set('sending_domain', domain);
      if (isp) qs.set('isp', isp);
      const res = await apiFetch(`/api/mailing/growth/daily?${qs.toString()}`, { signal: ac.signal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const body = (await res.json()) as GrowthResponse & { error?: string };
      if (body.error) throw new Error(body.error);
      setData(body);
    } catch (e) {
      if ((e as Error).name === 'AbortError') return;
      setData(null);
      setError((e as Error).message || 'fetch failed');
    } finally {
      if (!ac.signal.aborted) setLoading(false);
    }
  }, [from, to, domain, isp]);

  useEffect(() => { void load(); }, [load]);

  useEffect(() => () => abortRef.current?.abort(), []);

  // New slice → stale drill-downs. Collapse and drop the cache.
  useEffect(() => {
    setExpandedDay(null);
    setBreakdowns({});
  }, [from, to, domain, isp]);

  // The drill-down dimension: by ISP normally; when a provider filter is
  // already applied, splitting by ISP would return one row, so split by
  // sending domain instead.
  const breakdownBy: 'isp' | 'sending_domain' = isp ? 'sending_domain' : 'isp';

  const toggleDay = useCallback((day: string) => {
    setExpandedDay((cur) => (cur === day ? null : day));
    setBreakdowns((cache) => {
      // Loaded or in flight → keep. An error retries on the next expand.
      if (cache[day] && cache[day].status !== 'error') return cache;
      const qs = new URLSearchParams({ day, by: breakdownBy });
      if (domain) qs.set('sending_domain', domain);
      if (isp) qs.set('isp', isp);
      void (async () => {
        try {
          const res = await apiFetch(`/api/mailing/growth/day?${qs.toString()}`);
          if (!res.ok) throw new Error(`HTTP ${res.status}`);
          const body = (await res.json()) as BreakdownResponse & { error?: string };
          if (body.error) throw new Error(body.error);
          setBreakdowns((c) => ({ ...c, [day]: { status: 'ready', data: body } }));
        } catch (e) {
          setBreakdowns((c) => ({ ...c, [day]: { status: 'error', message: (e as Error).message || 'fetch failed' } }));
        }
      })();
      return { ...cache, [day]: { status: 'loading' } };
    });
  }, [domain, isp, breakdownBy]);

  const rows = data?.rows ?? [];
  const totals = data?.totals;

  const chartData = useMemo(() => rows.map((r) => ({
    day: fmtDay(r.day),
    delivered: r.delivered,
    open_pct: r.open_pct,
    click_pct: r.click_pct,
    action_click_pct: r.action_click_pct,
  })), [rows]);

  const scopeLabel = `${domain || 'All brands'} · ${isp || 'All providers'}`;

  return (
    <div>
      {error && <SectionError label="Growth" error={error} onRetry={() => void load()} />}

      {loading && !data && (
        <Panel style={{ marginBottom: 16 }}>
          <div style={{ color: theme.textMuted, padding: '12px 0' }}>
            <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 8 }} /> Loading…
          </div>
        </Panel>
      )}

      {data && totals && (
        <>
          {/* ─── Headline KPIs (≤8 tiles, design system §1.1) ─── */}
          <Panel style={{ marginBottom: 16 }}>
            <SectionHeader
              title={`Growth — ${scopeLabel}`}
              icon={faChartLine}
            />
            <div style={{ display: 'flex', gap: 28, flexWrap: 'wrap' }}>
              <Stat label="Delivered" value={fmtInt(totals.delivered)}
                sub={`${fmtInt(totals.attempted)} sent`} />
              <Stat label="Open rate" value={fmtPct(totals.open_pct)} color={C_OPEN} title={OPEN_TITLE} />
              <Stat label="Click rate" value={fmtPct(totals.click_pct)} color={C_CLICK} title={CLICK_TITLE} />
              <Stat label="Action clicks" value={fmtInt(totals.action_click_subs)} color={C_ACTION}
                sub={fmtPct(totals.action_click_pct)} title={ACTION_TITLE} />
              <Stat label="Complaints" value={fmtInt(totals.complaints)} color={C_COMPLAINT}
                sub={fmtPct(totals.complaint_pct)} title="Out of delivered" />
              <Stat label="Unsubscribes" value={fmtInt(totals.unsubs)}
                sub={fmtPct(totals.unsub_pct)} title="Out of delivered" />
              <Stat label="Hard bounce" value={fmtInt(totals.hard_bounce)} color={theme.danger}
                sub={totals.reputation_block > 0
                  ? <span style={{ color: theme.warningText }}>+{fmtInt(totals.reputation_block)} blocked</span>
                  : undefined}
                title="Address is bad. Blocks by the provider show apart." />
              <Stat label="Soft bounce" value={fmtInt(totals.soft_bounce)} color={theme.warning}
                title="A short-term failure." />
            </div>
          </Panel>

          {/* ─── Trend ─── */}
          <Panel style={{ marginBottom: 16 }}>
            <SectionHeader
              title="Trend"
              icon={faChartLine}
              right={
                <div style={{ display: 'flex', gap: 6 }}>
                  {(['rates', 'volume'] as const).map((m) => (
                    <button key={m}
                      style={{
                        ...btnStyle,
                        padding: '4px 10px', fontSize: 11,
                        color: chartMetric === m ? theme.indigo400 : theme.textMuted,
                        borderColor: chartMetric === m ? `color-mix(in srgb, ${theme.indigo400} 40%, transparent)` : theme.panelBorderStrong,
                        background: chartMetric === m ? `color-mix(in srgb, ${theme.indigo400} 8%, transparent)` : 'transparent',
                      }}
                      onClick={() => setChartMetric(m)}
                    >
                      {m === 'rates' ? 'Rates' : 'Volume'}
                    </button>
                  ))}
                </div>
              }
            />
            {rows.length === 0 ? (
              <EmptyState title="No days to show" hint="Pick more days or clear a filter." />
            ) : (
              <div style={{ height: 280 }}>
                <ResponsiveContainer width="100%" height="100%">
                  <ComposedChart data={chartData} margin={{ top: 8, right: 12, bottom: 4, left: 4 }}>
                    <CartesianGrid stroke="color-mix(in srgb, var(--j-text) 6%, transparent)" vertical={false} />
                    <XAxis dataKey="day" tick={{ fill: theme.textMuted, fontSize: 11 }} tickLine={false} axisLine={false} minTickGap={18} />
                    <YAxis tick={{ fill: theme.textMuted, fontSize: 11 }} tickLine={false} axisLine={false}
                      tickFormatter={(v: number) => (chartMetric === 'rates' ? `${v}%` : v >= 1000 ? `${Math.round(v / 1000)}k` : `${v}`)} />
                    <Tooltip
                      contentStyle={{ background: theme.panelBgSolid, border: `1px solid ${theme.panelBorderStrong}`, borderRadius: 8, fontSize: 12 }}
                      labelStyle={{ color: theme.heading }}
                      formatter={(v: number, n: string) => [chartMetric === 'rates' ? `${Number(v).toFixed(2)}%` : nf.format(Number(v)), n]}
                    />
                    <Legend wrapperStyle={{ fontSize: 11, color: theme.textMuted }} />
                    {chartMetric === 'volume' ? (
                      <Area type="monotone" dataKey="delivered" name="Delivered" stroke={C_VOLUME}
                        fill={`color-mix(in srgb, ${C_VOLUME} 20%, transparent)`} strokeWidth={2} />
                    ) : (
                      <>
                        <Line type="monotone" dataKey="open_pct" name="Open rate" stroke={C_OPEN} strokeWidth={2} dot={false} />
                        <Line type="monotone" dataKey="click_pct" name="Click rate" stroke={C_CLICK} strokeWidth={2} dot={false} />
                        <Line type="monotone" dataKey="action_click_pct" name="Action click rate" stroke={C_ACTION} strokeWidth={2} dot={false} />
                      </>
                    )}
                  </ComposedChart>
                </ResponsiveContainer>
              </div>
            )}
          </Panel>

          {/* ─── The table (the spreadsheet, replaced) ─── */}
          <Panel>
            <SectionHeader
              title="Day by day"
              icon={faTable}
              right={
                <button
                  style={{ ...btnStyle, padding: '4px 10px', fontSize: 11 }}
                  disabled={rows.length === 0}
                  onClick={() => downloadCSV(
                    `growth_${domain || 'all'}_${isp || 'all'}_${data.from}_${data.to}.csv`,
                    toCSV(rows),
                  )}
                >
                  <FontAwesomeIcon icon={faDownload} style={{ marginRight: 6 }} /> CSV
                </button>
              }
            />
            {rows.length === 0 ? (
              <EmptyState title="Nothing to show" hint="Pick more days or clear a filter." />
            ) : (
              <div style={{ overflowX: 'auto' }} tabIndex={0}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={{ ...thStyle, width: 28 }} />
                      <th style={thStyle}>Date</th>
                      <th style={numTh}>Delivered</th>
                      <th style={numTh} title="Change from the day before">Change</th>
                      <th style={numTh} title="Change from the day before">Change %</th>
                      <th style={numTh} title={OPEN_TITLE}>Open %</th>
                      <th style={numTh} title="Change from the day before">Open change</th>
                      <th style={numTh} title={CLICK_TITLE}>Click %</th>
                      <th style={numTh} title="Change from the day before">Click change</th>
                      <th style={numTh} title={ACTION_TITLE}>Action clicks</th>
                      <th style={numTh} title="Action clicks out of delivered">Action %</th>
                      <th style={numTh}>Complaints</th>
                      <th style={numTh} title="Out of delivered">Complaint %</th>
                      <th style={numTh}>Unsubscribes</th>
                      <th style={numTh} title="Out of delivered">Unsubscribe %</th>
                      <th style={numTh}>Hard bounce</th>
                      <th style={numTh}>Soft bounce</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((r) => (
                      <React.Fragment key={r.day}>
                      <tr onClick={() => toggleDay(r.day)} style={{ cursor: 'pointer' }}>
                        <td style={{ ...tdStyle, width: 28, color: expandedDay === r.day ? theme.indigo400 : theme.textFaint }}>
                          <FontAwesomeIcon icon={expandedDay === r.day ? faChevronDown : faChevronRight} style={{ fontSize: 10 }} />
                        </td>
                        <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{r.day}</td>
                        <td style={numTd}>{fmtInt(r.delivered)}</td>
                        <td style={numTd}>{r.delta_delivered == null ? <span style={{ color: theme.textFaint }}>n/a</span>
                          : <span style={{ color: r.delta_delivered >= 0 ? theme.successText : theme.dangerText }}>
                            {r.delta_delivered > 0 ? '+' : ''}{fmtInt(r.delta_delivered)}
                          </span>}</td>
                        <td style={numTd}><DeltaCell value={r.delta_delivered_pct} /></td>
                        <td style={{ ...numTd, color: C_OPEN }}>{fmtPct(r.open_pct)}</td>
                        <td style={numTd}><DeltaCell value={r.delta_open_pct} /></td>
                        <td style={{ ...numTd, color: C_CLICK }}>{fmtPct(r.click_pct)}</td>
                        <td style={numTd}><DeltaCell value={r.delta_click_pct} /></td>
                        <td style={{ ...numTd, color: C_ACTION }}>{fmtInt(r.action_click_subs)}</td>
                        <td style={numTd}>{fmtPct(r.action_click_pct)}</td>
                        <td style={numTd}>{fmtInt(r.complaints)}</td>
                        <td style={numTd}>{fmtPct(r.complaint_pct)}</td>
                        <td style={numTd}>{fmtInt(r.unsubs)}</td>
                        <td style={numTd}>{fmtPct(r.unsub_pct)}</td>
                        <td style={{ ...numTd, color: r.hard_bounce > 0 ? theme.danger : theme.textMuted }}>{fmtInt(r.hard_bounce)}</td>
                        <td style={{ ...numTd, color: r.soft_bounce > 0 ? theme.warning : theme.textMuted }}>{fmtInt(r.soft_bounce)}</td>
                      </tr>
                      {expandedDay === r.day && breakdowns[r.day] && (
                        <tr>
                          <td colSpan={17} style={{ ...tdStyle, padding: '4px 8px 10px 32px', background: 'color-mix(in srgb, var(--j-violet) 4%, transparent)' }}>
                            <BreakdownSubTable state={breakdowns[r.day]} />
                          </td>
                        </tr>
                      )}
                      </React.Fragment>
                    ))}
                  </tbody>
                  <tfoot>
                    <tr>
                      <td style={tdStyle} />
                      <td style={{ ...tdStyle, fontWeight: 700, color: theme.heading }}>Total</td>
                      <td style={{ ...numTd, fontWeight: 700 }}>{fmtInt(totals.delivered)}</td>
                      <td style={numTd}>—</td>
                      <td style={numTd}>—</td>
                      <td style={{ ...numTd, fontWeight: 700, color: C_OPEN }}>{fmtPct(totals.open_pct)}</td>
                      <td style={numTd}>—</td>
                      <td style={{ ...numTd, fontWeight: 700, color: C_CLICK }}>{fmtPct(totals.click_pct)}</td>
                      <td style={numTd}>—</td>
                      <td style={{ ...numTd, fontWeight: 700, color: C_ACTION }}>{fmtInt(totals.action_click_subs)}</td>
                      <td style={numTd}>{fmtPct(totals.action_click_pct)}</td>
                      <td style={{ ...numTd, fontWeight: 700 }}>{fmtInt(totals.complaints)}</td>
                      <td style={numTd}>{fmtPct(totals.complaint_pct)}</td>
                      <td style={{ ...numTd, fontWeight: 700 }}>{fmtInt(totals.unsubs)}</td>
                      <td style={numTd}>{fmtPct(totals.unsub_pct)}</td>
                      <td style={{ ...numTd, fontWeight: 700, color: theme.danger }}>{fmtInt(totals.hard_bounce)}</td>
                      <td style={{ ...numTd, fontWeight: 700, color: theme.warning }}>{fmtInt(totals.soft_bounce)}</td>
                    </tr>
                  </tfoot>
                </table>
              </div>
            )}

          </Panel>
        </>
      )}
    </div>
  );
};

export default GrowthTab;
