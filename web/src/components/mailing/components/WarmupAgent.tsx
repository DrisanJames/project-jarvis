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
  faFire, faGaugeHigh, faListCheck, faBookOpen, faFileLines, faRotate, faArrowUp, faArrowDown, faHand,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha, pageStyle, tdStyle, thStyle, numTd, numTh, tableStyle, btnStyle } from '../shared/theme';
import { Panel, SectionHeader, Stat, SectionError, EmptyState, Pill, LivePill, ProgressBar } from '../shared/ui';
import { SubNav } from '../shared/SubNav';

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
const pct = (num: number, den: number) => (den > 0 ? `${((100 * num) / den).toFixed(1)}%` : '—');

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

type Scorecard = {
  runs: number; ticks: number; daily: number; holds: number; moves: number;
  rateUp: number; rateDown: number; capMoves: number; lessons: number; supply: number; pauses: number;
  llmUnavailable: number; runtimeErrors: number; topups: number; injected: number; steps: number; tokensIn: number; tokensOut: number;
  models: Record<string, number>; lastRun?: JournalRow;
};
function scorecard(journal: JournalRow[]): Scorecard {
  const s: Scorecard = { runs: 0, ticks: 0, daily: 0, holds: 0, moves: 0, rateUp: 0, rateDown: 0, capMoves: 0, lessons: 0, supply: 0, pauses: 0, llmUnavailable: 0, runtimeErrors: 0, topups: 0, injected: 0, steps: 0, tokensIn: 0, tokensOut: 0, models: {} };
  for (const r of journal) {
    if (r.kind.startsWith('run:')) {
      s.runs += 1; if (r.kind === 'run:tick') s.ticks += 1; if (r.kind === 'run:daily') s.daily += 1;
      const acts = (r.actions ?? []).filter((a) => !READ_TOOLS.has(a.tool));
      if (acts.length) s.moves += 1; else s.holds += 1;
      s.steps += r.steps ?? 0; s.tokensIn += r.usage?.in ?? 0; s.tokensOut += r.usage?.out ?? 0;
      if (r.model) s.models[r.model] = (s.models[r.model] || 0) + 1;
      s.lastRun = r;
    } else if (r.kind === 'set_rate') {
      for (const c of r.changed ?? []) { if (c.from == null || c.to > c.from) s.rateUp += 1; else if (c.to < c.from) s.rateDown += 1; }
    } else if (r.kind === 'set_daily_volume') s.capMoves += 1;
    else if (r.kind === 'lesson') s.lessons += 1;
    else if (r.kind === 'request_supply') s.supply += 1;
    else if (r.kind === 'PAUSE') s.pauses += 1;
    else if (r.kind === 'llm_unavailable') s.llmUnavailable += 1;
    else if (r.kind === 'runtime_error') s.runtimeErrors += 1;
    else if (r.kind === 'topup') { s.topups += 1; s.injected += r.added ?? 0; }
  }
  return s;
}

type Lesson = { date: string; title: string; body: string };
function parseLessons(md: string | null): Lesson[] {
  if (!md) return [];
  const out: Lesson[] = [];
  const re = /^### (\d{4}-\d{2}-\d{2}) — (.+)$/gm;
  const idx: { start: number; date: string; title: string }[] = [];
  let m: RegExpExecArray | null;
  while ((m = re.exec(md))) idx.push({ start: m.index, date: m[1], title: m[2] });
  idx.forEach((h, i) => {
    const end = i + 1 < idx.length ? idx[i + 1].start : md.length;
    const body = md.slice(h.start, end).split('\n').slice(1).join('\n').trim();
    out.push({ date: h.date, title: h.title, body });
  });
  return out.reverse();
}

// Minimal markdown renderer for the agent's report (headings, tables, lists, bold, code).
function inlineMd(t: string): React.ReactNode[] {
  const parts: React.ReactNode[] = [];
  const re = /(\*\*[^*]+\*\*|`[^`]+`)/g;
  let last = 0; let m: RegExpExecArray | null; let k = 0;
  while ((m = re.exec(t))) {
    if (m.index > last) parts.push(t.slice(last, m.index));
    const tok = m[0];
    if (tok.startsWith('**')) parts.push(<strong key={k++}>{tok.slice(2, -2)}</strong>);
    else parts.push(<code key={k++} style={{ fontSize: 12, color: colors.indigo200 }}>{tok.slice(1, -1)}</code>);
    last = m.index + tok.length;
  }
  if (last < t.length) parts.push(t.slice(last));
  return parts;
}
const Markdown: React.FC<{ text: string }> = ({ text }) => {
  const lines = text.split('\n');
  const nodes: React.ReactNode[] = [];
  let i = 0; let k = 0;
  while (i < lines.length) {
    const l = lines[i];
    if (l.startsWith('|') && i + 1 < lines.length && /^\|[\s\-:|]+\|$/.test(lines[i + 1])) {
      const hdr = l.replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
      i += 2; const rows: string[][] = [];
      while (i < lines.length && lines[i].startsWith('|')) { rows.push(lines[i].replace(/^\||\|$/g, '').split('|').map((c) => c.trim())); i += 1; }
      nodes.push(
        <div key={k++} style={{ overflowX: 'auto', margin: '6px 0 12px' }}>
          <table style={tableStyle}>
            <thead><tr>{hdr.map((h, j) => <th key={j} style={thStyle}>{inlineMd(h)}</th>)}</tr></thead>
            <tbody>{rows.map((r, ri) => <tr key={ri}>{r.map((c, j) => <td key={j} style={tdStyle}>{inlineMd(c)}</td>)}</tr>)}</tbody>
          </table>
        </div>,
      );
      continue;
    }
    if (l.startsWith('- ')) {
      const items: string[] = [];
      while (i < lines.length && lines[i].startsWith('- ')) { items.push(lines[i].slice(2)); i += 1; }
      nodes.push(<ul key={k++} style={{ margin: '4px 0 10px', paddingLeft: 20 }}>{items.map((it, j) => <li key={j} style={{ margin: '2px 0' }}>{inlineMd(it)}</li>)}</ul>);
      continue;
    }
    const h = /^(#{1,3})\s+(.*)$/.exec(l);
    if (h) {
      const level = h[1].length;
      nodes.push(<div key={k++} style={{ fontSize: level === 1 ? 17 : level === 2 ? 14 : 13, fontWeight: 700, color: level === 2 ? colors.indigo300 : colors.heading, margin: level === 1 ? '0 0 8px' : '14px 0 6px', textTransform: level === 2 ? 'uppercase' : undefined, letterSpacing: level === 2 ? 0.5 : undefined }}>{inlineMd(h[2])}</div>);
    } else if (l.trim()) {
      nodes.push(<p key={k++} style={{ margin: '4px 0', lineHeight: 1.5 }}>{inlineMd(l)}</p>);
    }
    i += 1;
  }
  return <div style={{ fontSize: 13, color: colors.text }}>{nodes}</div>;
};

// ───────────────────────────── component

type View = 'overview' | 'decisions' | 'rates' | 'delivery' | 'lessons' | 'report';
const VIEWS: { key: View; label: string }[] = [
  { key: 'overview', label: 'Overview' }, { key: 'decisions', label: 'Decisions' }, { key: 'rates', label: 'Rates & caps' },
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
  useEffect(() => {
    if (date !== denverToday()) return;
    const id = window.setInterval(() => { void load(); }, 60_000);
    return () => window.clearInterval(id);
  }, [date, load]);

  const sc = useMemo(() => scorecard(data?.journal ?? []), [data]);
  const tk = useMemo(() => sumTicks(data?.ticks ?? []), [data]);
  const injected = useMemo(() => injectedByGroup(data?.jit ?? null), [data]);
  const caps = useMemo(() => capByGroup(data?.plan ?? null, data?.date ?? date), [data, date]);
  const lessons = useMemo(() => parseLessons(data?.field_manual ?? null), [data]);
  const prefixes = useMemo(() => Object.keys(data?.rates ?? {}).sort(), [data]);
  const runs = useMemo(() => (data?.journal ?? []).filter((r) => r.kind.startsWith('run:') || r.kind === 'llm_unavailable' || r.kind === 'runtime_error').slice().reverse(), [data]);
  const lastTopup = useMemo(() => (data?.journal ?? []).filter((r) => r.kind === 'topup').slice(-1)[0], [data]);

  const lastRunAgoSec = sc.lastRun ? Math.max(0, (Date.now() - new Date(sc.lastRun.at).getTime()) / 1000) : undefined;
  const live = date === denverToday() && lastRunAgoSec != null && lastRunAgoSec < 22 * 60;
  const paused = !!data?.flags?.paused;
  const totalInjected = Object.values(injected).reduce((a, b) => a + b, 0);
  const totalCap = Object.values(caps).reduce((a, b) => a + b, 0);
  const attempted = tk.total.delivered + tk.total.transfail + tk.total.fail;

  const header = (
    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: 12, marginBottom: 16 }}>
      <div>
        <h2 style={{ margin: 0, color: colors.heading, fontSize: 20, display: 'flex', alignItems: 'center', gap: 10 }}>
          <FontAwesomeIcon icon={faFire} style={{ color: colors.indigo400 }} /> Warmup Agent · {instance}
        </h2>
        <div style={{ color: colors.textMuted, fontSize: 12, marginTop: 4 }}>
          An LLM owns every per-IP rate and daily cap on this box; the sender tops recipients up to its rates every 15 minutes. Everything below is the agent's own record.
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
        <SectionHeader title="Agentic scorecard · today" icon={faGaugeHigh} right={<span style={{ fontSize: 11, color: colors.textMuted }}>model {model}</span>} />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(130px, 1fr))', gap: 14 }}>
          <Stat label="Runs" value={sc.runs} sub={`${sc.ticks} ticks · ${sc.daily} daily`} />
          <Stat label="Held / moved" value={`${sc.holds} / ${sc.moves}`} sub="runs with no change / with a change" />
          <Stat label="Rate moves" value={<span><FontAwesomeIcon icon={faArrowUp} style={{ color: colors.success, fontSize: 14 }} /> {sc.rateUp} <FontAwesomeIcon icon={faArrowDown} style={{ color: colors.warning, fontSize: 14, marginLeft: 8 }} /> {sc.rateDown}</span>} sub="per domain x ISP, by the agent" />
          <Stat label="Cap moves" value={sc.capMoves} sub="set_daily_volume calls" />
          <Stat label="Lessons" value={sc.lessons} sub="written to its field manual" />
          <Stat label="Supply asks" value={sc.supply} sub="records requested" />
          <Stat label="Top-ups" value={sc.topups} sub={`${fmt(sc.injected)} recipients injected`} />
          <Stat label="Steps / run" value={sc.runs ? (sc.steps / sc.runs).toFixed(1) : '—'} sub={`${fmt(sc.tokensIn)} in · ${fmt(sc.tokensOut)} out tokens`} />
          <Stat label="Model gaps" value={sc.llmUnavailable + sc.runtimeErrors} color={sc.llmUnavailable + sc.runtimeErrors ? colors.danger : colors.success} sub={`${sc.llmUnavailable} llm · ${sc.runtimeErrors} runtime`} />
          <Stat label="Pauses" value={sc.pauses} color={sc.pauses ? colors.danger : undefined} sub="agent kill-switch uses" />
        </div>
      </Panel>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(380px, 1fr))', gap: 14, marginBottom: 14 }}>
        <Panel>
          <SectionHeader title="What the ISPs said · today" />
          {attempted === 0 ? <EmptyState title="No deliveries recorded" hint="The tick log has no rows for this date." /> : (
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>Group</th><th style={numTh}>Delivered</th><th style={numTh}>Deferred</th><th style={numTh}>Bounced</th><th style={numTh}>Deferred %</th><th style={thStyle}>Rate / IP / h</th></tr></thead>
              <tbody>
                {GROUPS.filter((g) => tk.byGroup[g]).map((g) => {
                  const c = tk.byGroup[g]; const n = c.delivered + c.transfail + c.fail;
                  const rates = prefixes.map((p) => data.rates?.[p]?.[g]?.per_ip_hour).filter((x): x is number => typeof x === 'number');
                  const lo = rates.length ? Math.min(...rates) : null; const hi = rates.length ? Math.max(...rates) : null;
                  return (
                    <tr key={g}>
                      <td style={tdStyle}>{GROUP_LABEL[g] ?? g}</td>
                      <td style={numTd}>{fmt(c.delivered)}</td><td style={numTd}>{fmt(c.transfail)}</td><td style={numTd}>{fmt(c.fail)}</td>
                      <td style={{ ...numTd, color: n && c.transfail / n > 0.1 ? colors.warningText : colors.text }}>{pct(c.transfail, n)}</td>
                      <td style={tdStyle}>{lo == null ? '—' : lo === hi ? lo : `${lo}–${hi}`}</td>
                    </tr>
                  );
                })}
                <tr><td style={{ ...tdStyle, fontWeight: 700 }}>Box</td><td style={{ ...numTd, fontWeight: 700 }}>{fmt(tk.total.delivered)}</td><td style={numTd}>{fmt(tk.total.transfail)}</td><td style={numTd}>{fmt(tk.total.fail)}</td><td style={numTd}>{pct(tk.total.transfail, attempted)}</td><td style={tdStyle} /></tr>
              </tbody>
            </table>
          )}
        </Panel>
        <Panel>
          <SectionHeader title="Budget the agent set · today" right={<span style={{ fontSize: 11, color: colors.textMuted }}>{fmt(totalInjected)} of {fmt(totalCap)} injected</span>} />
          {totalCap === 0 ? <EmptyState title="No caps for this date" hint="The agent sets caps at its first run of the day; nothing injects without them." /> : (
            <div style={{ display: 'grid', gap: 8 }}>
              {GROUPS.filter((g) => caps[g] != null).map((g) => (
                <div key={g}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, marginBottom: 3 }}>
                    <span>{GROUP_LABEL[g] ?? g}</span>
                    <span style={{ color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}>{fmt(injected[g] ?? 0)} / {fmt(caps[g])}{caps[g] === 0 ? ' · not mailed today' : ''}</span>
                  </div>
                  <ProgressBar pct={caps[g] ? Math.min(1, (injected[g] ?? 0) / caps[g]) : 0} />
                </div>
              ))}
              {lastTopup && (
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 6 }}>
                  Last top-up {denverTime(lastTopup.at)} MT: +{fmt(lastTopup.added ?? 0)}.{' '}
                  {Object.entries(lastTopup.domains ?? {}).slice(0, 1).map(([p, d]) => d.held && Object.keys(d.held).length ? `${p} held: ${Object.entries(d.held).map(([g, why]) => `${GROUP_LABEL[g] ?? g} (${why})`).join(', ')}` : '').join('')}
                </div>
              )}
            </div>
          )}
        </Panel>
      </div>

      <Panel>
        <SectionHeader title="Latest run" icon={faListCheck} right={sc.lastRun ? <span style={{ fontSize: 11, color: colors.textMuted }} title={sc.lastRun.at}>{sc.lastRun.kind.replace('run:', '')} · {denverTime(sc.lastRun.at)} MT · {sc.lastRun.steps ?? '—'} steps</span> : null} />
        {sc.lastRun?.final ? <div style={{ fontSize: 13, whiteSpace: 'pre-wrap', lineHeight: 1.5, color: colors.text, maxHeight: 360, overflowY: 'auto' }}>{sc.lastRun.final}</div> : <EmptyState title="No runs yet" />}
      </Panel>
    </>
  );

  const decisions = (
    <Panel>
      <SectionHeader title="Every run, newest first" icon={faListCheck} right={<span style={{ fontSize: 11, color: colors.textMuted }}>{runs.length} runs · click a row for the agent's full note</span>} />
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
                            const label = a.tool === 'set_rate' ? `${inp.prefix} ${GROUP_LABEL[String(inp.group)] ?? inp.group} → ${inp.per_ip_hour}/IP/h`
                              : a.tool === 'set_daily_volume' ? `cap ${inp.prefix} ${GROUP_LABEL[String(inp.group)] ?? inp.group} → ${inp.volume}`
                              : a.tool === 'request_supply' ? `supply ${inp.group} +${inp.records}`
                              : a.tool === 'record_lesson' ? `lesson: ${String(inp.title ?? '').slice(0, 60)}`
                              : a.tool === 'pause_instance' ? 'PAUSE' : a.tool;
                            const color = a.tool === 'pause_instance' ? colors.danger : a.tool === 'set_rate' ? colors.success : a.tool === 'record_lesson' ? colors.warning : colors.indigo400;
                            return <Pill key={j} color={a.status === 'error' ? colors.danger : color} style={{ fontSize: 10 }}>{label}</Pill>;
                          })}
                        </div>
                      ) : null}
                      <div style={{ fontSize: 12, color: colors.textMuted, whiteSpace: open[key] ? 'pre-wrap' : 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                        {open[key] ? (r.final ?? r.error ?? note ?? '') : summary.slice(0, 220)}
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
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Rates the agent holds now · messages per IP per hour" right={<span style={{ fontSize: 11, color: colors.textMuted }}>hover a cell for the agent's reason</span>} />
        {prefixes.length === 0 ? <EmptyState title="No rates set" /> : (
          <div style={{ overflowX: 'auto' }}>
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>Domain</th>{GROUPS.map((g) => <th key={g} style={numTh}>{GROUP_LABEL[g]}</th>)}</tr></thead>
              <tbody>
                {prefixes.map((p) => (
                  <tr key={p}>
                    <td style={{ ...tdStyle, fontWeight: 600 }}>{p}</td>
                    {GROUPS.map((g) => {
                      const r = data.rates?.[p]?.[g];
                      if (!r) return <td key={g} style={{ ...numTd, color: colors.textFaint }}>—</td>;
                      const up = r.previous != null && r.per_ip_hour > r.previous; const down = r.previous != null && r.per_ip_hour < r.previous;
                      return (
                        <td key={g} style={numTd} title={`${r.reason ?? ''}\nset ${r.set_at ?? ''}${r.previous != null ? ` (was ${r.previous})` : ''}`}>
                          <span style={{ color: up ? colors.successText : down ? colors.warningText : colors.text }}>{r.per_ip_hour}</span>
                          {r.previous != null && r.previous !== r.per_ip_hour && <span style={{ fontSize: 10, color: colors.textFaint, marginLeft: 4 }}>{r.previous}→</span>}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title={`Caps for ${data.date} · per domain`} right={<span style={{ fontSize: 11, color: colors.textMuted }}>a cap is the day's budget; the rate is the pace; a missing cap = not mailed</span>} />
        {!data.plan?.[data.date] ? <EmptyState title="No caps for this date" /> : (
          <div style={{ overflowX: 'auto' }}>
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>Domain</th>{GROUPS.map((g) => <th key={g} style={numTh}>{GROUP_LABEL[g]}</th>)}<th style={numTh}>Injected today</th></tr></thead>
              <tbody>
                {Object.keys(data.plan[data.date]).sort().map((p) => {
                  const row = data.plan?.[data.date]?.[p] ?? {};
                  const inj = Object.values(data.jit ?? {}).filter((s) => s.prefix === p).reduce((a, s) => a + Object.values(s.groups).reduce((x, y) => x + y, 0), 0);
                  return (
                    <tr key={p}><td style={{ ...tdStyle, fontWeight: 600 }}>{p}</td>
                      {GROUPS.map((g) => <td key={g} style={{ ...numTd, color: row[g] ? colors.text : colors.textFaint }}>{row[g] != null ? fmt(row[g]) : '—'}</td>)}
                      <td style={numTd}>{fmt(inj)}</td></tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
      <Panel>
        <SectionHeader title="Supply the agent asked for" />
        {!data.supply_requests || Object.keys(data.supply_requests).length === 0 ? <EmptyState title="No open requests" hint="The supply job clears requests as it fulfils them." /> : (
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Group</th><th style={numTh}>Records</th><th style={thStyle}>Agent's reason</th><th style={thStyle}>Asked</th></tr></thead>
            <tbody>{Object.entries(data.supply_requests).map(([g, r]) => <tr key={g}><td style={tdStyle}>{GROUP_LABEL[g] ?? g}</td><td style={numTd}>{fmt(r.records)}</td><td style={tdStyle}>{r.reason}</td><td style={tdStyle} title={r.at}>{denverTime(r.at)} MT</td></tr>)}</tbody>
          </table>
        )}
      </Panel>
    </>
  );

  const hours = Object.keys(tk.byHour).sort();
  const delivery = (
    <Panel>
      <SectionHeader title="Delivered / deferred / bounced by Denver hour" right={<span style={{ fontSize: 11, color: colors.textMuted }}>source: the box's own log (pmta_acct_raw), summed per tick</span>} />
      {hours.length === 0 ? <EmptyState title="No tick deltas for this date" /> : (
        <div style={{ overflowX: 'auto' }}>
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Hour</th>{GROUPS.map((g) => <th key={g} style={numTh}>{GROUP_LABEL[g]}</th>)}<th style={numTh}>Box</th></tr></thead>
            <tbody>
              {hours.map((h) => {
                const row = tk.byHour[h]; const tot = Object.values(row).reduce((a, c) => ({ delivered: a.delivered + c.delivered, transfail: a.transfail + c.transfail, fail: a.fail + c.fail }), { delivered: 0, transfail: 0, fail: 0 });
                const cell = (c?: Totals) => c ? (
                  <span style={{ fontVariantNumeric: 'tabular-nums' }}>
                    {fmt(c.delivered)}
                    {c.transfail ? <span style={{ color: colors.warningText }}> · {fmt(c.transfail)}d</span> : null}
                    {c.fail ? <span style={{ color: colors.dangerText }}> · {fmt(c.fail)}b</span> : null}
                  </span>
                ) : <span style={{ color: colors.textFaint }}>—</span>;
                return <tr key={h}><td style={tdStyle}>{h}:00</td>{GROUPS.map((g) => <td key={g} style={numTd}>{cell(row[g])}</td>)}<td style={{ ...numTd, fontWeight: 600 }}>{cell(tot)}</td></tr>;
              })}
            </tbody>
          </table>
          <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 6 }}>delivered · deferred (d) · bounced (b). Deferred mail is retried by the box; it is the ISP's signal, not lost mail.</div>
        </div>
      )}
    </Panel>
  );

  const lessonsView = (
    <Panel>
      <SectionHeader title="The agent's field manual · lessons it wrote" icon={faBookOpen} right={<span style={{ fontSize: 11, color: colors.textMuted }}>{lessons.length} lessons · newest first</span>} />
      {lessons.length === 0 ? <EmptyState title="No lessons yet" /> : lessons.map((l, i) => (
        <div key={i} style={{ padding: '10px 0', borderBottom: `1px solid ${colors.divider}` }}>
          <div style={{ fontSize: 13, fontWeight: 600, color: colors.heading }}><span style={{ color: colors.textMuted, fontWeight: 400, marginRight: 8 }}>{l.date}</span>{l.title}</div>
          <div style={{ fontSize: 12, color: colors.textMuted, whiteSpace: 'pre-wrap', marginTop: 4, lineHeight: 1.5 }}>{l.body}</div>
        </div>
      ))}
    </Panel>
  );

  const reportView = (
    <Panel>
      <SectionHeader title={`Daily report · written by the agent at the 04:00 MT run`} icon={faFileLines} />
      {data.report ? <Markdown text={data.report} /> : <EmptyState title="No report for this date" hint="The agent writes it at its daily run; a date before the agent went live has none." />}
    </Panel>
  );

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
