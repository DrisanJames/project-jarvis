// WarmupAgent.tsx — the operator's window onto the Bedrock AgentCore warmup
// agent (mailing-saas agents/warmup): an LLM that owns the per-IP sending rate
// and the daily cap for every sending domain x ISP group on a KumoMTA box, and
// a sender that tops recipients up to those rates every 15 minutes.
//
// PAGE_VERSION 1 (2026-10-27) — operator: "I want to understand how this
// system is performing from an agentic standpoint."
//
// Rules this screen honours:
//   - One endpoint, raw bytes: GET /api/mailing/warmup-agent/overview bundles
//     the agent's own S3 objects (rates, plan, journal, ticks, lessons, report).
//     Every number here is derived from those objects in this file, so the
//     portal reads exactly what the agent reads.
//   - Agentic stance first: who decided what, with the reason the agent gave,
//     and what the ISPs said back. Delivery numbers follow.
//   - Absence of data is "no rows", never a zero. Loading / error / empty are
//     three displays (PORTAL_DESIGN_SYSTEM §1.6).
//   - Timestamps render in America/Denver; the raw UTC instant is in title=.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faFire, faGaugeHigh, faListCheck, faRotate, faArrowUp, faArrowDown, faHand,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha, pageStyle, tdStyle, thStyle, numTd, numTh, tableStyle, btnStyle } from '../shared/theme';
import { Panel, SectionHeader, Stat, SectionError, EmptyState, Pill, LivePill } from '../shared/ui';
import { SubNav } from '../shared/SubNav';
import { DeliveryView, SpeedTargetsView, DailyReportView, LessonsView, Explain, domainName, ForecastFindings, PerformanceLookback, CheckinNote, ShareBar, type DayTotals } from './WarmupReadable';

// ───────────────────────────── API shapes (mirror of agents/warmup/state.py)

type Rate = { per_ip_hour: number; connections?: number; previous?: number | null; reason?: string; set_at?: string };
type Rates = Record<string, Record<string, Rate>>;                 // prefix -> group -> rate
type Plan = Record<string, Record<string, Record<string, number>>>; // date -> prefix -> group -> cap
type JitSlot = { prefix: string; groups: Record<string, number>; at: string; slot: string };
type Action = { tool: string; input?: Record<string, unknown>; status?: string };
type JournalRow = {
  at: string; kind: string; model?: string; steps?: number; actions?: Action[];
  usage?: { in?: number; out?: number }; final?: string;
  group?: string; changed?: { prefix: string; from?: number | null; to: number }[]; refused?: unknown[]; reason?: string;
  title?: string; evidence?: string; change?: string; records?: number; error?: string; slot?: string; added?: number;
  domains?: Record<string, { n?: number; held?: Record<string, string>; skip?: string; http?: number; error?: string | null }>;
  released_failed?: string[];
};
type TickRow = { at: string; delta?: { minutes: number; by: Record<string, Record<string, { delivered: number; transfail: number; fail: number }>> } | null; scheduled?: Record<string, number> };
type Overview = {
  instance: string; date: string; fetched_at: string;
  rates: Rates | null; plan: Plan | null; flags: { paused?: boolean; reason?: string } | null;
  supply_requests: Record<string, { records: number; reason: string; at: string }> | null;
  jit: Record<string, JitSlot> | null; box_last: { at?: string; scheduled?: Record<string, number> } | null;
  journal: JournalRow[]; ticks: TickRow[]; field_manual: string | null; report: string | null;
  errors?: Record<string, string>;
};

const GROUPS = ['microsoft', 'apple', 'comcast', 'charter', 'other', 'yahoo_family'] as const;
const GROUP_LABEL: Record<string, string> = {
  microsoft: 'Microsoft', apple: 'Apple', comcast: 'Comcast', charter: 'Charter', other: 'Other', yahoo_family: 'Yahoo family',
};
const READ_TOOLS = new Set(['read_box', 'read_responses', 'read_history', 'read_state', 'read_supply', 'read_journal', 'read_knowledge', 'read_campaigns', 'read_injection', 'fetch_url', 'journal_note']);

function denverToday(): string {
  const parts = new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver', year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(new Date());
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? '';
  return `${get('year')}-${get('month')}-${get('day')}`;
}
function denverTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return new Intl.DateTimeFormat('en-US', { timeZone: 'America/Denver', hour: '2-digit', minute: '2-digit', hour12: false }).format(d);
}
function denverHour(iso: string): string {
  const d = new Date(iso);
  return new Intl.DateTimeFormat('en-US', { timeZone: 'America/Denver', hour: '2-digit', hour12: false }).format(d);
}
const fmt = (n: number) => n.toLocaleString('en-US');

// ───────────────────────────── derived views

type Totals = { delivered: number; transfail: number; fail: number };
function sumTicks(ticks: TickRow[]): { byGroup: Record<string, Totals>; byHour: Record<string, Record<string, Totals>>; total: Totals } {
  const byGroup: Record<string, Totals> = {};
  const byHour: Record<string, Record<string, Totals>> = {};
  const total: Totals = { delivered: 0, transfail: 0, fail: 0 };
  for (const t of ticks) {
    if (!t.delta?.by) continue;
    const h = denverHour(t.at);
    for (const groups of Object.values(t.delta.by)) {
      for (const [g, c] of Object.entries(groups)) {
        const bg = (byGroup[g] ||= { delivered: 0, transfail: 0, fail: 0 });
        const bh = ((byHour[h] ||= {})[g] ||= { delivered: 0, transfail: 0, fail: 0 });
        for (const k of ['delivered', 'transfail', 'fail'] as const) { bg[k] += c[k] || 0; bh[k] += c[k] || 0; total[k] += c[k] || 0; }
      }
    }
  }
  return { byGroup, byHour, total };
}

function injectedByGroup(jit: Record<string, JitSlot> | null): Record<string, number> {
  const out: Record<string, number> = {};
  for (const s of Object.values(jit ?? {})) for (const [g, n] of Object.entries(s.groups)) out[g] = (out[g] || 0) + n;
  return out;
}

function capByGroup(plan: Plan | null, date: string): Record<string, number> {
  const out: Record<string, number> = {};
  for (const groups of Object.values(plan?.[date] ?? {})) for (const [g, n] of Object.entries(groups)) out[g] = (out[g] || 0) + n;
  return out;
}

type Span = { first: number; last: number };
type Scorecard = {
  runs: number; ticks: number; daily: number; holds: number; moves: number;
  rateUp: number; rateDown: number; rateSpan: Record<string, Span>;          // per group: first rate seen today → latest
  capMoves: number; capSpan: Record<string, Span>;                            // per group: first cap set today → latest (per domain)
  lessons: { title: string; change?: string }[]; supply: Record<string, number>; pauses: number;
  llmUnavailable: number; runtimeErrors: number; topups: number; injected: number; steps: number; tokensIn: number; tokensOut: number;
  models: Record<string, number>; lastRun?: JournalRow;
};
function scorecard(journal: JournalRow[]): Scorecard {
  const s: Scorecard = { runs: 0, ticks: 0, daily: 0, holds: 0, moves: 0, rateUp: 0, rateDown: 0, rateSpan: {}, capMoves: 0, capSpan: {}, lessons: [], supply: {}, pauses: 0, llmUnavailable: 0, runtimeErrors: 0, topups: 0, injected: 0, steps: 0, tokensIn: 0, tokensOut: 0, models: {} };
  for (const r of journal) {
    if (r.kind.startsWith('run:')) {
      s.runs += 1; if (r.kind === 'run:tick') s.ticks += 1; if (r.kind === 'run:daily') s.daily += 1;
      const acts = (r.actions ?? []).filter((a) => !READ_TOOLS.has(a.tool));
      if (acts.length) s.moves += 1; else s.holds += 1;
      s.steps += r.steps ?? 0; s.tokensIn += r.usage?.in ?? 0; s.tokensOut += r.usage?.out ?? 0;
      if (r.model) s.models[r.model] = (s.models[r.model] || 0) + 1;
      s.lastRun = r;
    } else if (r.kind === 'set_rate' && r.group) {
      for (const c of r.changed ?? []) {
        if (c.from == null || c.to > c.from) s.rateUp += 1; else if (c.to < c.from) s.rateDown += 1;
        const sp = (s.rateSpan[r.group] ||= { first: c.from ?? c.to, last: c.to }); sp.last = c.to;
      }
    } else if (r.kind === 'set_daily_volume' && r.group) {
      s.capMoves += 1;
      for (const c of r.changed ?? []) { const sp = (s.capSpan[r.group] ||= { first: c.from ?? c.to, last: c.to }); sp.last = c.to; }
    } else if (r.kind === 'lesson') s.lessons.push({ title: r.title ?? '', change: r.change });
    else if (r.kind === 'request_supply' && r.group) s.supply[r.group] = (s.supply[r.group] || 0) + (r.records ?? 0);
    else if (r.kind === 'PAUSE') s.pauses += 1;
    else if (r.kind === 'llm_unavailable') s.llmUnavailable += 1;
    else if (r.kind === 'runtime_error') s.runtimeErrors += 1;
    else if (r.kind === 'topup') { s.topups += 1; s.injected += r.added ?? 0; }
  }
  return s;
}
const spanText = (sp: Record<string, Span>, unit: string) => Object.entries(sp).map(([g, v]) => `${GROUP_LABEL[g] ?? g} ${fmt(v.first)} → ${fmt(v.last)}${unit}`).join(' · ');

// ───────────────────────────── component

type View = 'overview' | 'decisions' | 'rates' | 'delivery' | 'lessons' | 'report';
const VIEWS: { key: View; label: string }[] = [
  { key: 'overview', label: 'Overview' }, { key: 'decisions', label: 'Decisions' }, { key: 'rates', label: 'Speed & targets' },
  { key: 'delivery', label: 'Delivery' }, { key: 'lessons', label: 'Lessons' }, { key: 'report', label: 'Daily report' },
];

const WarmupAgent: React.FC<{ date?: string }> = ({ date: dateProp }) => {
  const [instance] = useState('colo2');
  const [dateState, setDate] = useState(denverToday());
  const date = dateProp ?? dateState;
  const [view, setView] = useState<View>('overview');
  const [data, setData] = useState<Overview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<Record<string, boolean>>({});

  const load = useCallback(async () => {
    setLoading(true); setError(null);
    try {
      const res = await apiFetch(`/api/mailing/warmup-agent/overview?instance=${encodeURIComponent(instance)}&date=${encodeURIComponent(date)}`);
      if (!res.ok) { const b = await res.json().catch(() => ({})); throw new Error((b as { error?: string }).error || `HTTP ${res.status}`); }
      setData((await res.json()) as Overview);
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    finally { setLoading(false); }
  }, [instance, date]);

  useEffect(() => { void load(); }, [load]);

  // Look-back: the six Denver days before the selected one, loaded once per date (not on the 60 s poll). Days with no
  // tick rows (before the agent went live) are dropped.
  const [history, setHistory] = useState<DayTotals[]>([]);
  const [historyLoading, setHistoryLoading] = useState(false);
  useEffect(() => {
    let cancelled = false;
    const days = Array.from({ length: 6 }, (_, i) => { const d = new Date(`${date}T12:00:00Z`); d.setUTCDate(d.getUTCDate() - (i + 1)); return d.toISOString().slice(0, 10); });
    setHistoryLoading(true);
    void Promise.all(days.map(async (d): Promise<DayTotals | null> => {
      try {
        const res = await apiFetch(`/api/mailing/warmup-agent/overview?instance=${encodeURIComponent(instance)}&date=${d}`);
        if (!res.ok) return null;
        const o = (await res.json()) as Overview;
        const t = sumTicks(o.ticks ?? []);
        return Object.keys(t.byGroup).length ? { date: d, byGroup: t.byGroup, target: capByGroup(o.plan, d), firstAt: (o.ticks ?? []).find((x) => x.delta?.by && Object.keys(x.delta.by).length)?.at } : null;
      } catch { return null; }
    })).then((rows) => { if (!cancelled) { setHistory(rows.filter((r): r is DayTotals => r != null)); setHistoryLoading(false); } });
    return () => { cancelled = true; };
  }, [instance, date]);
  useEffect(() => {
    if (date !== denverToday()) return;
    const id = window.setInterval(() => { void load(); }, 60_000);
    return () => window.clearInterval(id);
  }, [date, load]);

  const sc = useMemo(() => scorecard(data?.journal ?? []), [data]);
  const tk = useMemo(() => sumTicks(data?.ticks ?? []), [data]);
  const injected = useMemo(() => injectedByGroup(data?.jit ?? null), [data]);
  const caps = useMemo(() => capByGroup(data?.plan ?? null, data?.date ?? date), [data, date]);
  const runs = useMemo(() => (data?.journal ?? []).filter((r) => r.kind.startsWith('run:') || r.kind === 'llm_unavailable' || r.kind === 'runtime_error').slice().reverse(), [data]);
  const lastTopup = useMemo(() => (data?.journal ?? []).filter((r) => r.kind === 'topup').slice(-1)[0], [data]);

  const lastRunAgoSec = sc.lastRun ? Math.max(0, (Date.now() - new Date(sc.lastRun.at).getTime()) / 1000) : undefined;
  const live = date === denverToday() && lastRunAgoSec != null && lastRunAgoSec < 22 * 60;
  const paused = !!data?.flags?.paused;
  const totalInjected = Object.values(injected).reduce((a, b) => a + b, 0);
  const totalCap = Object.values(caps).reduce((a, b) => a + b, 0);

  const header = (
    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: 12, marginBottom: 16 }}>
      <div>
        <h2 style={{ margin: 0, color: colors.heading, fontSize: 20, display: 'flex', alignItems: 'center', gap: 10 }}>
          <FontAwesomeIcon icon={faFire} style={{ color: colors.indigo400 }} /> Warmup agent on this box
        </h2>
        <div style={{ color: colors.textMuted, fontSize: 12, marginTop: 4 }}>
          An LLM sets the speed (messages per IP per hour) and the day's target for every sending domain × mailbox provider on this box; recipients are injected to that speed every 15 minutes. Everything below is the agent's own record.
        </div>
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        {paused ? <Pill color={colors.danger}>PAUSED{data?.flags?.reason ? ` · ${data.flags.reason}` : ''}</Pill> : <LivePill live={live} agoSeconds={lastRunAgoSec != null ? Math.round(lastRunAgoSec) : undefined} />}
        {dateProp == null && <input type="date" value={date} max={denverToday()} onChange={(e) => setDate(e.target.value)}
          style={{ ...btnStyle, colorScheme: 'dark' }} aria-label="Denver date" />}
        <button style={btnStyle} onClick={() => { void load(); }} disabled={loading}><FontAwesomeIcon icon={faRotate} spin={loading} /> Refresh</button>
      </div>
    </div>
  );

  const wrap = dateProp == null ? pageStyle : undefined;
  if (error && !data) return <div style={wrap}>{header}<SectionError label="warmup agent overview" error={error} onRetry={() => { void load(); }} /></div>;
  if (loading && !data) return <div style={wrap}>{header}<div style={{ color: colors.textMuted }}>Loading the agent's record…</div></div>;
  if (!data) return <div style={wrap}>{header}<EmptyState title="No record" hint="The agent has not written anything for this date." /></div>;

  const model = Object.entries(sc.models).sort((a, b) => b[1] - a[1])[0]?.[0] ?? '—';

  const overview = (
    <>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Decisions · today" icon={faGaugeHigh} right={<span style={{ fontSize: 11, color: colors.textMuted }}>{model} · {fmt(sc.tokensIn)} tokens read · {fmt(sc.tokensOut)} written</span>} />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(210px, 1fr))', gap: 14 }}>
          <Stat label="Check-ins" value={sc.runs} sub={`reviewed the box every 15 min: ${sc.ticks} check-ins + ${sc.daily} morning review`} title="Each check-in reads the last 20 minutes of ISP replies and decides whether to change anything." />
          <Stat label="Changed / held" value={`${sc.moves} / ${sc.holds}`} sub="check-ins that changed a speed or target / that held everything" title="A hold is a decision too: the agent read the replies and chose not to move." />
          <Stat label="Speed changes" value={<span><FontAwesomeIcon icon={faArrowUp} style={{ color: colors.success, fontSize: 14 }} /> {sc.rateUp} <FontAwesomeIcon icon={faArrowDown} style={{ color: colors.warning, fontSize: 14, marginLeft: 8 }} /> {sc.rateDown}</span>}
            sub={`${sc.rateUp} raises · ${sc.rateDown} eases. ${spanText(sc.rateSpan, ' per IP per hour') || 'No speed changes.'}`} title="Messages per IP per hour, set per sending domain × mailbox provider. Counts are per domain (one decision on all 8 domains = 8)." />
          <Stat label="Targets raised" value={sc.capMoves} sub={spanText(sc.capSpan, ' per domain') || 'no target changes'} title="The day's send target per sending domain × mailbox provider. Injection stops at the target; the agent raises it when a provider stays clean." />
          <Stat label="Lessons" value={sc.lessons.length} sub={sc.lessons.length ? 'new today, the latest in Key findings below; all on the Lessons tab' : 'none today'} title={sc.lessons.map((l) => `${l.title}\n${l.change ?? ''}`).join('\n\n') || 'Lessons the agent wrote to its own field manual today.'} />
          <Stat label="Records requested" value={fmt(Object.values(sc.supply).reduce((a, b) => a + b, 0))} sub={Object.entries(sc.supply).map(([g, n]) => `${GROUP_LABEL[g] ?? g} ${fmt(n)}`).join(' · ') || 'no supply requests'} title="New never-mailed records the agent asked the supply job to validate and load." />
          <Stat label="Recipients injected" value={fmt(sc.injected)} sub={`in ${sc.topups} batches, one per check-in, sized to the next hour at the current speed`} />
          <Stat label="Interruptions" value={sc.llmUnavailable + sc.runtimeErrors + sc.pauses} color={sc.llmUnavailable + sc.runtimeErrors + sc.pauses ? colors.danger : colors.success} sub={`${sc.llmUnavailable} model outages · ${sc.runtimeErrors} runtime errors · ${sc.pauses} pauses`} />
        </div>
      </Panel>

      <ForecastFindings date={data.date} isToday={date === denverToday()} ticks={data.ticks ?? []} today={tk.byGroup} target={caps}
        prev={history[0]} report={data.report} lessonsToday={sc.lessons.map((l) => l.title)} />

      <div style={{ marginBottom: 14 }}><PerformanceLookback date={data.date} today={tk.byGroup} target={caps} history={history} loading={historyLoading} /></div>

      <div style={{ display: 'grid', gridTemplateColumns: 'minmax(320px, 1fr) minmax(420px, 2fr)', gap: 14, marginBottom: 14, alignItems: 'start' }}>
        <Panel>
          <SectionHeader title="Targets · today" right={<span style={{ fontSize: 11, color: colors.textMuted }}>{fmt(totalInjected)} of {fmt(totalCap)} injected</span>} />
          {totalCap === 0 ? <EmptyState title="No targets for this date" hint="The agent sets targets at its first check-in of the day; nothing injects without them." /> : (
            <div style={{ display: 'grid', gap: 8 }}>
              {GROUPS.filter((g) => caps[g] != null).map((g) => (
                <div key={g}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, marginBottom: 4, color: colors.text }}>
                    <span style={{ fontWeight: 600 }}>{GROUP_LABEL[g] ?? g}</span>
                    <span style={{ color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}>{fmt(injected[g] ?? 0)} of {fmt(caps[g])} handed to the box{caps[g] === 0 ? ' · not mailed today' : (injected[g] ?? 0) >= caps[g] ? ' · ✓ target reached' : ''}</span>
                  </div>
                  <ShareBar pct={caps[g] ? (injected[g] ?? 0) / caps[g] : 0} color={colors.indigo400} />
                </div>
              ))}
              {lastTopup && (
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 6 }}>
                  Last top-up {denverTime(lastTopup.at)} MT: +{fmt(lastTopup.added ?? 0)}.{' '}
                  {Object.entries(lastTopup.domains ?? {}).slice(0, 1).map(([p, d]) => d.held && Object.keys(d.held).length ? `${domainName(p)} held: ${Object.entries(d.held).map(([g, why]) => `${GROUP_LABEL[g] ?? g} (${why})`).join(', ')}` : '').join('')}
                </div>
              )}
            </div>
          )}
        </Panel>
      <Panel>
        <SectionHeader title="Latest check-in" icon={faListCheck} right={sc.lastRun ? <span style={{ fontSize: 11, color: colors.textMuted }} title={sc.lastRun.at}>{sc.lastRun.kind === 'run:daily' ? 'morning review' : 'check-in'} · {denverTime(sc.lastRun.at)} MT · {sc.lastRun.steps ?? '—'} steps</span> : null} />
        {sc.lastRun?.final ? <CheckinNote text={sc.lastRun.final} /> : <EmptyState title="No runs yet" />}
      </Panel>
      </div>
    </>
  );

  const decisions = (
    <Panel>
      <SectionHeader title="Every check-in, newest first" icon={faListCheck} right={<span style={{ fontSize: 11, color: colors.textMuted }}>{runs.length} check-ins · click a row for the agent's full note</span>} />
      {runs.length === 0 ? <EmptyState title="No runs for this date" /> : (
        <table style={tableStyle}>
          <thead><tr><th style={thStyle}>MT</th><th style={thStyle}>Run</th><th style={thStyle}>Decision</th><th style={thStyle}>Actions the agent took</th><th style={numTh}>Steps</th></tr></thead>
          <tbody>
            {runs.map((r, i) => {
              const key = `${r.at}${r.kind}`;
              const acts = (r.actions ?? []).filter((a) => !READ_TOOLS.has(a.tool));
              const gap = r.kind === 'llm_unavailable' || r.kind === 'runtime_error';
              const note = (r.actions ?? []).find((a) => a.tool === 'journal_note')?.input?.note as string | undefined;
              const summary = (note ?? r.final ?? r.error ?? '').replace(/\s+/g, ' ');
              return (
                <React.Fragment key={key + i}>
                  <tr onClick={() => setOpen((o) => ({ ...o, [key]: !o[key] }))} style={{ cursor: 'pointer', background: gap ? alpha(colors.danger, '0d') : undefined }}>
                    <td style={{ ...tdStyle, whiteSpace: 'nowrap' }} title={r.at}>{denverTime(r.at)}</td>
                    <td style={tdStyle}>{gap ? <Pill color={colors.danger}>{r.kind}</Pill> : r.kind.replace('run:', '')}</td>
                    <td style={tdStyle}>{gap ? '—' : acts.length ? <Pill color={colors.indigo400}>moved</Pill> : <Pill color={colors.idle}><FontAwesomeIcon icon={faHand} /> held</Pill>}</td>
                    <td style={{ ...tdStyle, maxWidth: 560 }}>
                      {acts.length ? (
                        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginBottom: 4 }}>
                          {acts.map((a, j) => {
                            const inp = a.input ?? {};
                            const label = a.tool === 'set_rate' ? `speed ${domainName(String(inp.prefix))} ${GROUP_LABEL[String(inp.group)] ?? inp.group} → ${inp.per_ip_hour} per IP per hour`
                              : a.tool === 'set_daily_volume' ? `target ${domainName(String(inp.prefix))} ${GROUP_LABEL[String(inp.group)] ?? inp.group} → ${fmt(Number(inp.volume))} today`
                              : a.tool === 'request_supply' ? `asked for ${fmt(Number(inp.records))} new ${GROUP_LABEL[String(inp.group)] ?? inp.group} records`
                              : a.tool === 'record_lesson' ? `lesson: ${String(inp.title ?? '').slice(0, 60)}`
                              : a.tool === 'pause_instance' ? 'PAUSE' : a.tool;
                            const color = a.tool === 'pause_instance' ? colors.danger : a.tool === 'set_rate' ? colors.success : a.tool === 'record_lesson' ? colors.warning : colors.indigo400;
                            return <Pill key={j} color={a.status === 'error' ? colors.danger : color} style={{ fontSize: 10 }}>{label}</Pill>;
                          })}
                        </div>
                      ) : null}
                      <div style={{ fontSize: 12, color: colors.textMuted, whiteSpace: open[key] ? 'pre-wrap' : 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                        {open[key] ? (r.final ? <div style={{ whiteSpace: 'normal', marginTop: 6 }}><CheckinNote text={r.final} /></div> : (r.error ?? note ?? '')) : summary.slice(0, 220)}
                      </div>
                    </td>
                    <td style={numTd}>{r.steps ?? '—'}</td>
                  </tr>
                </React.Fragment>
              );
            })}
          </tbody>
        </table>
      )}
    </Panel>
  );

  const ratesView = (
    <>
      <SpeedTargetsView date={data.date} rates={data.rates} plan={data.plan?.[data.date]} jit={data.jit} ticks={data.ticks ?? []} journal={data.journal ?? []} />
      <div style={{ height: 14 }} />
      <Panel>
        <SectionHeader title="Supply the agent asked for" />
        {!data.supply_requests || Object.keys(data.supply_requests).length === 0 ? <EmptyState title="No open requests" hint="The supply job clears requests as it fulfils them." /> : (
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Mailbox provider</th><th style={numTh}>New records asked for</th><th style={thStyle}>Agent's reason</th><th style={thStyle}>Asked</th></tr></thead>
            <tbody>{Object.entries(data.supply_requests).map(([g, r]) => <tr key={g}><td style={tdStyle}>{GROUP_LABEL[g] ?? g}</td><td style={numTd}>{fmt(r.records)}</td><td style={{ ...tdStyle, fontSize: 12 }}><Explain text={r.reason} /></td><td style={tdStyle} title={r.at}>{denverTime(r.at)} MT</td></tr>)}</tbody>
          </table>
        )}
      </Panel>
    </>
  );

  const delivery = <DeliveryView byGroup={tk.byGroup} byHour={tk.byHour} />;

  const lessonsView = <LessonsView fieldManual={data.field_manual} journal={(data.journal ?? []).filter((r) => r.kind === 'lesson')} date={data.date} />;

  const reportView = <DailyReportView report={data.report} />;

  return (
    <div style={dateProp == null ? pageStyle : undefined}>
      {header}
      {data.errors && Object.keys(data.errors).length > 0 && (
        <div style={{ marginBottom: 12 }}>{Object.entries(data.errors).map(([k, e]) => <SectionError key={k} label={k} error={e} onRetry={() => { void load(); }} />)}</div>
      )}
      <SubNav items={VIEWS} active={view} onChange={(id) => setView(id as View)} ariaLabel="Warmup agent views" />
      <div style={{ marginTop: 14 }}>
        {view === 'overview' && overview}
        {view === 'decisions' && decisions}
        {view === 'rates' && ratesView}
        {view === 'delivery' && delivery}
        {view === 'lessons' && lessonsView}
        {view === 'report' && reportView}
      </div>
    </div>
  );
};

export default WarmupAgent;
