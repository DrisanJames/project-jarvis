// Growth — the daily send-program growth series.
//
// TAB_VERSION 1.0 (2026-07-29). Replaces the hand-kept growth spreadsheet: one
// row per Denver day for a (sending domain × mailbox provider) slice, over ANY
// historical range, with the day-over-day deltas the operator tracks.
//
// Backend (READ ONLY, via apiFetch):
//   GET /api/mailing/growth/daily?from=&to=&sending_domain=&isp=
//       → { rows: GrowthRow[], totals, coverage, notes }
//   GET /api/mailing/growth/filters
//       → { sending_domains, isps, min_day, max_day }
//
// Both read mailing_growth_daily (worker.GrowthRollupWorker): delivery from the
// event lake, engagement from platform tracking. PG-backed, so this tab works
// regardless of whether the lake READ layer is enabled — the rollup already
// did the lake reads.
//
// RATE CONVENTION (matches the API and the operator's sheet): open/click rates
// are EVENTS ÷ delivered, so they can legitimately exceed 100% — one scanner
// opening one message many times. "Action clicks" are unique mailboxes on
// navigational links only (the governing cohort) and are the number that means
// engagement. Hard and soft bounces are never summed. Every rate discloses its
// denominator in the header tooltip (PORTAL_DESIGN_SYSTEM §1.5).

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faArrowUp, faArrowDown, faMinus, faDownload,
  faChartLine, faTable, faInfoCircle,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Area, Line, XAxis, YAxis,
  CartesianGrid, Tooltip, Legend,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';
import { colors as theme, panelTitleStyle, thStyle, tdStyle, numTd, numTh, tableStyle, btnStyle } from '../shared/theme';
import { Panel, SectionHeader, Stat, SectionError, EmptyState } from '../shared/ui';
import { FilterBar, denverToday, daysAgoDenver } from '../shared/filters';
import type { LakeFilterDraft, AppliedLakeFilters } from '../shared/filters';

const TAB_VERSION = '1.0';

// Series palette — the shared chart tones (PORTAL_DESIGN_SYSTEM §2).
const C_VOLUME = '#818cf8'; // indigo — delivered
const C_OPEN = '#22d3ee';   // cyan   — open rate
const C_CLICK = '#a78bfa';  // violet — click rate
const C_ACTION = '#34d399'; // emerald— action-click rate
const C_COMPLAINT = '#fb7185';

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
  coverage: { lake_epoch: string; domain_attribution_from: string; last_computed_at: string };
  notes: string[];
}

interface GrowthFilters {
  sending_domains: string[];
  isps: string[];
  min_day: string;
  max_day: string;
}

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
    return <span style={{ color: theme.textFaint }} title="No prior day in range — ratio undefined">n/a</span>;
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
// TAB
// ═══════════════════════════════════════════════════════════════════════════

const emptyDraft = (): LakeFilterDraft => ({
  from: daysAgoDenver(29),
  to: denverToday(),
  ispGroup: '',
  brand: '',
  routeType: '',
  transport: 'combined',
});

export const GrowthTab: React.FC = () => {
  const [draft, setDraft] = useState<LakeFilterDraft>(emptyDraft);
  const [applied, setApplied] = useState<AppliedLakeFilters>(() => ({ ...emptyDraft(), nonce: 0 }));

  const [data, setData] = useState<GrowthResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [fetchedAt, setFetchedAt] = useState('');
  const [tookMs, setTookMs] = useState(0);
  const [filters, setFilters] = useState<GrowthFilters | null>(null);
  const [chartMetric, setChartMetric] = useState<'rates' | 'volume'>('rates');

  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async (f: AppliedLakeFilters) => {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setLoading(true);
    setError('');
    const t0 = performance.now();
    try {
      const qs = new URLSearchParams({ from: f.from, to: f.to });
      if (f.brand.trim()) qs.set('sending_domain', f.brand.trim());
      if (f.ispGroup.trim()) qs.set('isp', f.ispGroup.trim().toLowerCase());
      const res = await apiFetch(`/api/mailing/growth/daily?${qs.toString()}`, { signal: ac.signal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const body = (await res.json()) as GrowthResponse & { error?: string };
      if (body.error) throw new Error(body.error);
      setData(body);
      setTookMs(Math.round(performance.now() - t0));
      setFetchedAt(new Date().toLocaleTimeString());
    } catch (e) {
      if ((e as Error).name === 'AbortError') return;
      setData(null);
      setError((e as Error).message || 'fetch failed');
    } finally {
      if (!ac.signal.aborted) setLoading(false);
    }
  }, []);

  // Initial + on-Run fetch.
  useEffect(() => { void load(applied); }, [applied, load]);

  // Filter option lists — one cheap fetch, fail-soft (the bar still works with
  // its static datalists if this 500s).
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const res = await apiFetch('/api/mailing/growth/filters');
        if (!res.ok) return;
        const body = (await res.json()) as GrowthFilters;
        if (alive) setFilters(body);
      } catch { /* fail-soft: the shared datalists remain */ }
    })();
    return () => { alive = false; };
  }, []);

  useEffect(() => () => abortRef.current?.abort(), []);

  const onRun = useCallback((next: LakeFilterDraft) => {
    setApplied((prev) => ({ ...next, nonce: prev.nonce + 1 }));
  }, []);

  const rows = data?.rows ?? [];
  const totals = data?.totals;

  const chartData = useMemo(() => rows.map((r) => ({
    day: fmtDay(r.day),
    delivered: r.delivered,
    open_pct: r.open_pct,
    click_pct: r.click_pct,
    action_click_pct: r.action_click_pct,
  })), [rows]);

  const scopeLabel = useMemo(() => {
    const d = applied.brand.trim() || 'all sending domains';
    const i = applied.ispGroup.trim() || 'all providers';
    return `${d} · ${i}`;
  }, [applied.brand, applied.ispGroup]);

  return (
    <div>
      <FilterBar
        draft={draft}
        setDraft={setDraft}
        applied={applied}
        onRun={onRun}
        show={{ brand: true, isp: true }}
        activeLabel="Active (Growth):"
      />

      {/* Known values actually present in the rollup — so a filter never
          returns an empty screen because the value does not exist. */}
      {filters && (
        <div style={{ fontSize: 11, color: theme.textFaint, margin: '-6px 0 14px' }}>
          <FontAwesomeIcon icon={faInfoCircle} style={{ marginRight: 6 }} />
          History available {filters.min_day || '—'} → {filters.max_day || '—'} ·{' '}
          {filters.sending_domains.length} sending domain(s) · providers:{' '}
          {filters.isps.slice(0, 12).join(', ')}
        </div>
      )}

      {error && <SectionError label="Growth series" error={error} onRetry={() => void load(applied)} />}

      {loading && !data && (
        <Panel style={{ marginBottom: 16 }}>
          <div style={{ color: theme.textMuted, padding: '12px 0' }}>
            <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 8 }} /> Loading growth series…
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
              right={
                <span style={{ fontSize: 11, color: theme.textFaint }}>
                  {data.from} → {data.to} · America/Denver · {totals.days} day(s)
                </span>
              }
            />
            <div style={{ display: 'flex', gap: 28, flexWrap: 'wrap' }}>
              <Stat label="Delivered" value={fmtInt(totals.delivered)}
                sub={`${fmtInt(totals.attempted)} attempted*`}
                title="Sum of daily delivered from the event lake (real transports only). Attempted* is derived = delivered + hard + soft." />
              <Stat label="Open rate" value={fmtPct(totals.open_pct)} color={C_OPEN}
                sub="open events ÷ delivered"
                title="EVENTS ÷ delivered — can exceed 100% because one scanner opens one message many times. Machine traffic included." />
              <Stat label="Click rate" value={fmtPct(totals.click_pct)} color={C_CLICK}
                sub="click events ÷ delivered"
                title="EVENTS ÷ delivered. Includes asset fetches; see Action clicks for the governing cohort." />
              <Stat label="Action clicks" value={fmtInt(totals.action_click_subs)} color={C_ACTION}
                sub={`${fmtPct(totals.action_click_pct)} of delivered`}
                title="Unique mailboxes clicking a navigational/commercial link. Asset fetches and unsub/preference links are NOT clicks." />
              <Stat label="Complaints" value={fmtInt(totals.complaints)} color={C_COMPLAINT}
                sub={fmtPct(totals.complaint_pct)}
                title="Complaint events ÷ delivered." />
              <Stat label="Unsubscribes" value={fmtInt(totals.unsubs)}
                sub={fmtPct(totals.unsub_pct)}
                title="Distinct subscribers unsubscribing ÷ delivered." />
              <Stat label="Hard bounces" value={fmtInt(totals.hard_bounce)} color={theme.danger}
                sub={totals.reputation_block > 0
                  ? <span style={{ color: theme.warningText }}>+{fmtInt(totals.reputation_block)} reputation blocks</span>
                  : undefined}
                title="True list hygiene only. NEVER summed with soft bounces. Reputation/policy blocks and administrative queue flushes are counted in NEITHER hard nor soft — blocks are called out separately here." />
              <Stat label="Soft bounces" value={fmtInt(totals.soft_bounce)} color={theme.warning}
                title="Transient. NEVER summed with hard bounces." />
            </div>
            <div style={{ fontSize: 11, color: theme.textFaint, marginTop: 12 }}>
              fetched {fetchedAt} · {tookMs}ms
              {data.coverage.last_computed_at &&
                ` · rollup last computed ${new Date(data.coverage.last_computed_at).toLocaleString()}`}
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
                        borderColor: chartMetric === m ? `${theme.indigo400}66` : theme.panelBorderStrong,
                        background: chartMetric === m ? `${theme.indigo400}14` : 'transparent',
                      }}
                      onClick={() => setChartMetric(m)}
                    >
                      {m === 'rates' ? 'Engagement rates' : 'Volume'}
                    </button>
                  ))}
                </div>
              }
            />
            {rows.length === 0 ? (
              <EmptyState title="No days in this range" hint="Widen the date range or clear the sending-domain / provider filter." />
            ) : (
              <div style={{ height: 280 }}>
                <ResponsiveContainer width="100%" height="100%">
                  <ComposedChart data={chartData} margin={{ top: 8, right: 12, bottom: 4, left: 4 }}>
                    <CartesianGrid stroke="rgba(255,255,255,0.06)" vertical={false} />
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
                        fill={`${C_VOLUME}33`} strokeWidth={2} />
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
                    `growth_${applied.brand.trim() || 'all'}_${applied.ispGroup.trim() || 'all'}_${data.from}_${data.to}.csv`,
                    toCSV(rows),
                  )}
                >
                  <FontAwesomeIcon icon={faDownload} style={{ marginRight: 6 }} /> CSV
                </button>
              }
            />
            {rows.length === 0 ? (
              <EmptyState title="Nothing to show" hint="No rollup rows for this slice yet — history back-fills every 2 hours." />
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Date</th>
                      <th style={numTh} title="Delivered — event lake, real transports only, deduped">Delivered</th>
                      <th style={numTh} title="Change vs the previous day in this range">Δ vol</th>
                      <th style={numTh} title="Change vs the previous day, relative to the previous day">Δ vol %</th>
                      <th style={numTh} title="Open EVENTS ÷ delivered — exceeds 100% when scanners re-open">Open %</th>
                      <th style={numTh} title="Change in open rate vs the previous day">Δ open</th>
                      <th style={numTh} title="Click EVENTS ÷ delivered (includes asset fetches)">Click %</th>
                      <th style={numTh} title="Change in click rate vs the previous day">Δ click</th>
                      <th style={numTh} title="Unique mailboxes clicking a navigational link — the governing cohort">Action clk</th>
                      <th style={numTh} title="Action clicks ÷ delivered">Action %</th>
                      <th style={numTh} title="Complaint events">Compl</th>
                      <th style={numTh} title="Complaints ÷ delivered">Compl %</th>
                      <th style={numTh} title="Distinct subscribers unsubscribing">Unsub</th>
                      <th style={numTh} title="Unsubscribes ÷ delivered">Unsub %</th>
                      <th style={numTh} title="Hard bounces — true list hygiene, never summed with soft">Hard</th>
                      <th style={numTh} title="Soft bounces — transient, never summed with hard">Soft</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((r) => (
                      <tr key={r.day}>
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
                    ))}
                  </tbody>
                  <tfoot>
                    <tr>
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

            {/* Honest caveats — rendered, never hidden. */}
            {data.notes.length > 0 && (
              <ul style={{ margin: '14px 0 0', paddingLeft: 18, fontSize: 11, color: theme.textMuted, lineHeight: 1.6 }}>
                {data.notes.map((n, i) => <li key={i}>{n}</li>)}
              </ul>
            )}
            <div style={{ ...panelTitleStyle, marginTop: 14, fontSize: 10, color: theme.textFaint }}>
              Growth v{TAB_VERSION} · rollup source: mailing_growth_daily · lake history from {data.coverage.lake_epoch}
            </div>
          </Panel>
        </>
      )}
    </div>
  );
};

export default GrowthTab;
