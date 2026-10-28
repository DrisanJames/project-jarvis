// AgentsOps.tsx — the scheduling agent and the data agent as the operator sees
// them: what each decided, repaired or paged, with the agent's own words.
//
// PAGE_VERSION 1 (2026-10-27) — operator: "Consider the other sessions'
// agentic work as well as part of the screen you're working to ship."
//
// One endpoint, raw bytes: GET /api/mailing/agents/overview?date= bundles the
// jarvis-agents S3 state mirror (scheduling journal + supervisor state, data
// agent passes + state, the day's change-ledger rows). Everything shown is
// derived here from those objects; nothing is computed server-side.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCalendarCheck, faDatabase, faRotate, faScroll, faTriangleExclamation } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha, tdStyle, thStyle, numTd, numTh, tableStyle, btnStyle } from '../shared/theme';
import { Panel, SectionHeader, Stat, SectionError, EmptyState, Pill } from '../shared/ui';
import { SesLaneToday, type SesLaneReport } from './SesLaneToday';
import { KumoLaneToday, type KumoLaneReport } from './KumoLaneToday';
import { PmtaDailyRead, type PmtaRead } from './PmtaDailyRead';

// ───────────────────────────── API shapes (agents/jobs/scheduling_agent.py · scheduling_supervisor.py · data_agent.py)

type Gate = { verdict: string; acc?: number; blk?: number; dfr?: number; msgs?: number };
type VolumeDecision = { lane: string; date: string; posture: string; gates?: Record<string, Gate>; needs_you?: string[]; decision?: string; rule?: string };
type Readiness = { program: string; date: string; verdict: string; cells?: number; cancelled?: number; planned?: number; sent?: number; detail?: string; build_at?: string; problems?: string[]; note?: string };
type Deal = { name: string; token?: string | null; budget?: number; ecpm?: number; owed?: number; sent?: number; remaining?: number; need_per_day?: number; days_left?: number | null; finishes?: boolean };
type SchedJournal = {
  plan_date: string; yesterday?: string; tomorrow?: string; live?: boolean; started?: string; finished?: string; mandate_version?: number;
  needs_you?: string[]; cpm?: Deal[]; volume?: VolumeDecision[]; volume_apply?: { changed?: unknown[]; live?: boolean };
  rules?: { changed?: Record<string, unknown>; needs_you?: string[]; live?: boolean; ledger?: string };
  scorer?: { dates?: string[]; mode?: string; rc?: number; lines?: string[] }[]; board_summary?: string; readiness?: Readiness[];
};
type SupervisorProgram = { attempts?: number; paged?: boolean; outcome?: string; last_at?: string; last_rcs?: number[] };
type SupervisorState = Record<string, Record<string, SupervisorProgram>>;
type Check = { name: string; verdict: string; measure: string; problems?: string[]; action?: string; data?: Record<string, unknown> };
type Pass = { at: string; live?: boolean; checks: Check[]; repairs?: string[] };
type DataJournal = { date: string; passes: Pass[]; latest?: Pass };
type DataState = { last_pass?: { at: string; checks: { name: string; verdict: string; measure: string }[]; repairs?: string[] }; last_verdicts?: Record<string, string>; days?: Record<string, { attempts?: Record<string, number>; paged?: string[]; repairs?: { at: string; repairs: string[] }[] }> };
type LedgerRow = { id: string; ts: string; actor?: string; kind: string; target: string; why?: string; before?: unknown; after?: unknown; revert?: unknown; brain_claims?: number[]; no_claim_reason?: string };
type GmailCell = { code: string; sender: string; cap: number; recipients: number; status: string; waves: number; campaign_id: string;
  attempted?: number; delivered?: number; bounced?: number; blocked?: number; first_block_mt?: string | null; complaints?: number; opens?: number; human_clicks?: number };
type GmailLane = {
  date: string; as_of: string;
  today: { cells: GmailCell[]; totals: Record<string, number> };
  schedule: { day: string; due: string[]; missing: string[]; cells: GmailCell[]; total: number }[];
  ramp: { levels: Record<string, number>; holds: Record<string, unknown>; sets: Record<string, number> };
  rules?: string[];
};
type AgentsOverview = {
  date: string; fetched_at: string;
  scheduling: { plan_for_date: SchedJournal | null; plan_tonight: SchedJournal | null; supervisor: SupervisorState | null };
  data: { journal: DataJournal | null; state: DataState | null };
  gmail?: { lane: GmailLane | null };
  pmta?: { read: PmtaRead | null };
  ledger: LedgerRow[]; errors?: Record<string, string>;
  ses?: { lane: SesLaneReport | null };
  kumo?: { lane: KumoLaneReport | null };
};

const VERDICT_COLOR: Record<string, string> = { PASS: colors.success, OK: colors.success, STEP: colors.success, WARN: colors.warning, HOLD: colors.warning, PENDING: colors.idle, FAIL: colors.danger, ERROR: colors.danger, CUT: colors.danger };
const vcolor = (v: string) => VERDICT_COLOR[v.split(' ')[0]] ?? colors.idle;
const fmt = (n: number | undefined | null) => (n == null ? '—' : n.toLocaleString('en-US'));
function denverTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return new Intl.DateTimeFormat('en-US', { timeZone: 'America/Denver', hour: '2-digit', minute: '2-digit', hour12: false }).format(d);
}
function denverToday(): string {
  const parts = new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver', year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(new Date());
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? '';
  return `${get('year')}-${get('month')}-${get('day')}`;
}

export function useAgentsOverview(date: string) {
  const [data, setData] = useState<AgentsOverview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => {
    setLoading(true); setError(null);
    try {
      const res = await apiFetch(`/api/mailing/agents/overview?date=${encodeURIComponent(date)}`);
      if (!res.ok) { const b = await res.json().catch(() => ({})); throw new Error((b as { error?: string }).error || `HTTP ${res.status}`); }
      setData((await res.json()) as AgentsOverview);
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    finally { setLoading(false); }
  }, [date]);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => {
    if (date !== denverToday()) return;
    const id = window.setInterval(() => { void load(); }, 60_000);
    return () => window.clearInterval(id);
  }, [date, load]);
  return { data, loading, error, reload: load };
}

const LedgerTable: React.FC<{ rows: LedgerRow[]; title: string }> = ({ rows, title }) => (
  <Panel>
    <SectionHeader title={title} icon={faScroll} right={<span style={{ fontSize: 11, color: colors.textMuted }}>{rows.length} live changes · change ledger</span>} />
    {rows.length === 0 ? <EmptyState title="No live changes recorded" hint="Every live change an agent makes lands in agents/jobs/change_ledger with a revert; none for this filter today." /> : (
      <table style={tableStyle}>
        <thead><tr><th style={thStyle}>UTC</th><th style={thStyle}>Kind</th><th style={thStyle}>What changed</th><th style={thStyle}>Why</th><th style={thStyle}>Brain</th></tr></thead>
        <tbody>
          {rows.slice().reverse().map((r) => (
            <tr key={r.id}>
              <td style={{ ...tdStyle, whiteSpace: 'nowrap' }} title={r.id}>{r.ts.slice(11, 16)}</td>
              <td style={tdStyle}><Pill color={colors.indigo400} style={{ fontSize: 10 }}>{r.kind}</Pill></td>
              <td style={{ ...tdStyle, maxWidth: 520 }}>{r.target}</td>
              <td style={{ ...tdStyle, maxWidth: 360, color: colors.textMuted, fontSize: 12 }}>{r.why}</td>
              <td style={tdStyle}>{r.brain_claims?.length ? r.brain_claims.map((c) => `#${c}`).join(' ') : <span style={{ color: colors.textFaint }}>{r.no_claim_reason ? 'no claim' : '—'}</span>}</td>
            </tr>
          ))}
        </tbody>
      </table>
    )}
  </Panel>
);

// ───────────────────────────── scheduling agent

export const SchedulingAgentView: React.FC<{ date: string }> = ({ date }) => {
  const { data, loading, error, reload } = useAgentsOverview(date);
  const J = data?.scheduling.plan_for_date ?? null;
  const T = data?.scheduling.plan_tonight ?? null;
  const sup = data?.scheduling.supervisor?.[date] ?? null;
  const ledger = useMemo(() => (data?.ledger ?? []).filter((r) => r.kind.startsWith('scheduling') || r.kind === 'maximize_conversions' || r.kind.includes('board')), [data]);
  const [plan, setPlan] = useState<'for_date' | 'tonight'>('for_date');
  const P = plan === 'tonight' ? T : J;

  if (error && !data) return <SectionError label="agents overview" error={error} onRetry={() => { void reload(); }} />;
  if (loading && !data) return <div style={{ color: colors.textMuted }}>Loading the scheduling agent's record…</div>;

  const readiness = P?.readiness ?? [];
  const rCounts = readiness.reduce<Record<string, number>>((a, r) => { a[r.verdict] = (a[r.verdict] || 0) + 1; return a; }, {});
  const lanes = P?.volume ?? [];
  const supPrograms = Object.entries(sup ?? {}).filter(([, v]) => (v.attempts ?? 0) > 0 || v.paged || v.outcome);

  return (
    <>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Scheduling agent · plan night" icon={faCalendarCheck}
          right={(
            <div style={{ display: 'flex', gap: 6 }}>
              <button style={{ ...btnStyle, opacity: plan === 'for_date' ? 1 : 0.6 }} onClick={() => setPlan('for_date')}>Plan that built {date}{J ? '' : ' (none)'}</button>
              <button style={{ ...btnStyle, opacity: plan === 'tonight' ? 1 : 0.6 }} onClick={() => setPlan('tonight')}>Tonight's plan{T ? '' : ' (not yet)'}</button>
            </div>
          )} />
        {!P ? <EmptyState title="No plan journal" hint="The scheduling agent writes its journal at the 21:00 MT plan; a date before it went live has none." /> : (
          <>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(140px, 1fr))', gap: 14, marginBottom: 12 }}>
              <Stat label="Plan" value={P.plan_date} sub={`builds ${P.tomorrow ?? '—'} · ${P.live ? 'LIVE' : 'DRY'}`} />
              <Stat label="Ran" value={P.started ? denverTime(P.started) : '—'} sub={P.finished ? `→ ${denverTime(P.finished)} MT` : 'running'} />
              <Stat label="Lanes decided" value={lanes.length} sub={lanes.map((l) => `${l.lane} ${l.posture}`).join(' · ') || '—'} />
              <Stat label="Volume applied" value={P.volume_apply?.changed?.length ?? 0} sub="levels changed (never down)" />
              <Stat label="Rules changed" value={Object.keys(P.rules?.changed ?? {}).length} sub="offer_rules.json fields" />
              <Stat label="Needs you" value={(P.needs_you ?? []).length} color={(P.needs_you ?? []).length ? colors.warning : colors.success} sub="decisions it refused to make alone" />
              <Stat label="Readiness" value={`${rCounts.PASS ?? 0}/${readiness.length}`} sub={`${rCounts.WARN ?? 0} warn · ${rCounts.FAIL ?? 0} fail`} color={rCounts.FAIL ? colors.danger : rCounts.WARN ? colors.warning : colors.success} />
            </div>
            {P.board_summary && <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 8 }}>Board it filled: <span style={{ color: colors.text }}>{P.board_summary}</span></div>}
            {(P.needs_you ?? []).length > 0 && (
              <div style={{ background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`, borderRadius: 8, padding: '8px 12px', fontSize: 12 }}>
                <div style={{ fontWeight: 700, color: colors.warningText, marginBottom: 4 }}><FontAwesomeIcon icon={faTriangleExclamation} /> Needs you</div>
                <ol style={{ margin: 0, paddingLeft: 18 }}>{(P.needs_you ?? []).map((n, i) => <li key={i} style={{ margin: '2px 0' }}>{n}</li>)}</ol>
              </div>
            )}
          </>
        )}
      </Panel>

      {P && lanes.length > 0 && (
        <Panel style={{ marginBottom: 14 }}>
          <SectionHeader title="Volume decisions · lane × gate" right={<span style={{ fontSize: 11, color: colors.textMuted }}>UP only when every gate says STEP; a CUT verdict is held, never applied</span>} />
          {lanes.map((l) => (
            <div key={l.lane + l.date} style={{ marginBottom: 10 }}>
              <div style={{ fontSize: 12, marginBottom: 4 }}><strong>{l.lane}</strong> for {l.date} · posture <Pill color={l.posture === 'grow' ? colors.success : colors.warning} style={{ fontSize: 10 }}>{l.posture}</Pill>{(l.needs_you ?? []).length ? <span style={{ color: colors.warningText, marginLeft: 8 }}>{l.needs_you?.length} held</span> : null}</div>
              {Object.keys(l.gates ?? {}).length > 0 && (
                <div style={{ overflowX: 'auto' }}>
                  <table style={tableStyle}>
                    <thead><tr><th style={thStyle}>Gate</th><th style={thStyle}>Verdict</th><th style={numTh}>Accepted %</th><th style={numTh}>Blocked %</th><th style={numTh}>Deferred %</th><th style={numTh}>Msgs</th></tr></thead>
                    <tbody>{Object.entries(l.gates ?? {}).map(([k, g]) => (
                      <tr key={k}><td style={tdStyle}>{k}</td><td style={tdStyle}><Pill color={vcolor(g.verdict)} style={{ fontSize: 10 }}>{g.verdict}</Pill></td><td style={numTd}>{g.acc ?? '—'}</td><td style={numTd}>{g.blk ?? '—'}</td><td style={numTd}>{g.dfr ?? '—'}</td><td style={numTd}>{fmt(g.msgs)}</td></tr>
                    ))}</tbody>
                  </table>
                </div>
              )}
              {(l.needs_you ?? []).map((n, i) => <div key={i} style={{ fontSize: 12, color: colors.warningText, marginTop: 4 }}>{n}</div>)}
            </div>
          ))}
        </Panel>
      )}

      {P && (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(380px, 1fr))', gap: 14, marginBottom: 14 }}>
          <Panel>
            <SectionHeader title="Offers · what it changed" />
            {!P.rules?.changed || Object.keys(P.rules.changed).length === 0 ? <EmptyState title="No rule changes" hint="offer_rules.json already matched the mandate." /> : (
              <table style={tableStyle}><tbody>{Object.entries(P.rules.changed).map(([k, v]) => <tr key={k}><td style={{ ...tdStyle, fontWeight: 600, whiteSpace: 'nowrap' }}>{k}</td><td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted }}>{typeof v === 'string' ? v : JSON.stringify(v).slice(0, 240)}</td></tr>)}</tbody></table>
            )}
            {(P.scorer ?? []).map((s, i) => (
              <div key={i} style={{ marginTop: 10, fontSize: 12 }}>
                <div style={{ color: colors.textMuted, marginBottom: 2 }}>scorer · {s.mode} · {(s.dates ?? []).join(', ')} · rc {s.rc}</div>
                {(s.lines ?? []).filter((l) => l.startsWith('==')).map((l, j) => <div key={j} style={{ fontFamily: 'ui-monospace, Menlo, monospace', fontSize: 11, color: colors.text }}>{l}</div>)}
              </div>
            ))}
          </Panel>
          <Panel>
            <SectionHeader title="CPM deals it paced" />
            {!P.cpm?.length ? <EmptyState title="No CPM deals" /> : (
              <table style={tableStyle}>
                <thead><tr><th style={thStyle}>Deal</th><th style={numTh}>Sent</th><th style={numTh}>Owed</th><th style={numTh}>Need/day</th><th style={thStyle}>Finishes</th></tr></thead>
                <tbody>{P.cpm.map((d) => <tr key={d.name}><td style={tdStyle}>{d.name}{d.token ? <span style={{ color: colors.textFaint }}> · {d.token}</span> : <span style={{ color: colors.warningText }}> · no token</span>}</td><td style={numTd}>{fmt(d.sent)}</td><td style={numTd}>{fmt(d.owed)}</td><td style={numTd}>{fmt(d.need_per_day)}</td><td style={tdStyle}><Pill color={d.finishes ? colors.success : colors.warning} style={{ fontSize: 10 }}>{d.finishes ? 'yes' : 'at risk'}</Pill></td></tr>)}</tbody>
              </table>
            )}
          </Panel>
        </div>
      )}

      {P && readiness.length > 0 && (
        <Panel style={{ marginBottom: 14 }}>
          <SectionHeader title="Readiness it verified · program × date" />
          <div style={{ overflowX: 'auto' }}>
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>Program</th><th style={thStyle}>Date</th><th style={thStyle}>Verdict</th><th style={numTh}>Cells</th><th style={numTh}>Planned</th><th style={numTh}>Sent</th><th style={thStyle}>Build</th><th style={thStyle}>Detail</th></tr></thead>
              <tbody>{readiness.map((r, i) => (
                <tr key={i}><td style={tdStyle}>{r.program}</td><td style={tdStyle}>{r.date}</td><td style={tdStyle}><Pill color={vcolor(r.verdict)} style={{ fontSize: 10 }}>{r.verdict}</Pill></td><td style={numTd}>{fmt(r.cells)}</td><td style={numTd}>{fmt(r.planned)}</td><td style={numTd}>{fmt(r.sent)}</td><td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{r.build_at ?? '—'}</td><td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted, maxWidth: 420 }}>{(r.problems ?? []).join(' · ') || r.detail || r.note || ''}</td></tr>
              ))}</tbody>
            </table>
          </div>
        </Panel>
      )}

      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title={`Supervisor · repairs attempted on ${date}`} right={<span style={{ fontSize: 11, color: colors.textMuted }}>every 15 min 20:00–07:00 MT · rebuilds a missing cell by name, pages at anchor−30</span>} />
        {supPrograms.length === 0 ? <EmptyState title="Nothing to repair" hint={sup ? 'Every program built on schedule; no attempts, no pages.' : 'No supervisor record for this date.'} /> : (
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Program</th><th style={numTh}>Attempts</th><th style={thStyle}>Outcome</th><th style={thStyle}>Last</th><th style={thStyle}>Paged</th></tr></thead>
            <tbody>{supPrograms.map(([p, v]) => <tr key={p}><td style={tdStyle}>{p}</td><td style={numTd}>{v.attempts ?? 0}</td><td style={tdStyle}>{v.outcome ? <Pill color={vcolor(v.outcome)} style={{ fontSize: 10 }}>{v.outcome}</Pill> : '—'}</td><td style={tdStyle}>{v.last_at ? `${denverTime(v.last_at)} MT · rc ${(v.last_rcs ?? []).join(',')}` : '—'}</td><td style={tdStyle}>{v.paged ? <Pill color={colors.warning} style={{ fontSize: 10 }}>paged</Pill> : '—'}</td></tr>)}</tbody>
          </table>
        )}
      </Panel>

      <LedgerTable rows={ledger} title={`Live changes by the scheduling agent · ${date}`} />
    </>
  );
};

// ───────────────────────────── data agent

export const DataAgentView: React.FC<{ date: string }> = ({ date }) => {
  const { data, loading, error, reload } = useAgentsOverview(date);
  const journal = data?.data.journal ?? null;
  const st = data?.data.state ?? null;
  const passes = journal?.passes ?? [];
  const latest = journal?.latest ?? passes[passes.length - 1];
  const day = st?.days?.[date];
  const ledger = useMemo(() => (data?.ledger ?? []).filter((r) => r.kind === 'data_repair' || r.kind.startsWith('data_agent')), [data]);
  const [openPass, setOpenPass] = useState<string | null>(null);

  if (error && !data) return <SectionError label="agents overview" error={error} onRetry={() => { void reload(); }} />;
  if (loading && !data) return <div style={{ color: colors.textMuted }}>Loading the data agent's record…</div>;

  const counts = (p: Pass) => p.checks.reduce<Record<string, number>>((a, c) => { a[c.verdict] = (a[c.verdict] || 0) + 1; return a; }, {});
  const flips: { at: string; name: string; from: string; to: string }[] = [];
  for (let i = 1; i < passes.length; i += 1) {
    const prev = Object.fromEntries(passes[i - 1].checks.map((c) => [c.name, c.verdict]));
    for (const c of passes[i].checks) if (prev[c.name] && prev[c.name] !== c.verdict) flips.push({ at: passes[i].at, name: c.name, from: prev[c.name], to: c.verdict });
  }
  const repairs = passes.reduce((a, p) => a + (p.repairs?.length ?? 0), 0);
  const livePasses = passes.filter((p) => p.live).length;
  const lastAgo = latest ? Math.round((Date.now() - new Date(latest.at).getTime()) / 60000) : null;

  return (
    <>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Data agent · today" icon={faDatabase} right={latest ? <span style={{ fontSize: 11, color: colors.textMuted }} title={latest.at}>last pass {denverTime(latest.at)} MT{lastAgo != null ? ` · ${lastAgo} min ago` : ''}</span> : null} />
        {passes.length === 0 ? <EmptyState title="No passes for this date" hint="The data agent runs every 30 minutes (:10 / :40 MT); a date before it went live has none." /> : (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(130px, 1fr))', gap: 14 }}>
            <Stat label="Passes" value={passes.length} sub={`${livePasses} live · ${passes.length - livePasses} dry`} />
            <Stat label="Checks / pass" value={latest?.checks.length ?? 0} sub="supply · EO · door · fresh · growth · contracts · segments · suppression · workers" />
            <Stat label="Latest verdicts" value={latest ? `${counts(latest).PASS ?? 0} / ${counts(latest).WARN ?? 0} / ${counts(latest).FAIL ?? 0}` : '—'} sub="pass / warn / fail" color={latest && counts(latest).FAIL ? colors.danger : latest && counts(latest).WARN ? colors.warning : colors.success} />
            <Stat label="Repairs" value={repairs} sub="levers pulled today" color={repairs ? colors.indigo300 : undefined} />
            <Stat label="Verdict flips" value={flips.length} sub="posted live to #jarvis as they happen" />
            <Stat label="Pages" value={(day?.paged ?? []).length} sub="one per check per day → #approvals" color={(day?.paged ?? []).length ? colors.warning : undefined} />
          </div>
        )}
      </Panel>

      {latest && (
        <Panel style={{ marginBottom: 14 }}>
          <SectionHeader title={`Latest pass · ${denverTime(latest.at)} MT`} right={latest.repairs?.length ? <span style={{ fontSize: 11, color: colors.indigo300 }}>repairs: {latest.repairs.join(' · ')}</span> : <span style={{ fontSize: 11, color: colors.textMuted }}>no repairs this pass</span>} />
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Check</th><th style={thStyle}>Verdict</th><th style={thStyle}>Measure</th><th style={thStyle}>Problems · action</th></tr></thead>
            <tbody>{latest.checks.map((c) => (
              <tr key={c.name}>
                <td style={{ ...tdStyle, whiteSpace: 'nowrap', fontWeight: 600 }}>{c.name}</td>
                <td style={tdStyle}><Pill color={vcolor(c.verdict)} style={{ fontSize: 10 }}>{c.verdict}</Pill></td>
                <td style={{ ...tdStyle, fontSize: 12, maxWidth: 460 }}>{c.measure}</td>
                <td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted, maxWidth: 460 }}>
                  {(c.problems ?? []).slice(0, 3).map((p, i) => <div key={i}>· {p}</div>)}
                  {c.action ? <div style={{ color: colors.indigo300, marginTop: 2 }}>→ {c.action}</div> : null}
                </td>
              </tr>
            ))}</tbody>
          </table>
        </Panel>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(380px, 1fr))', gap: 14, marginBottom: 14 }}>
        <Panel>
          <SectionHeader title="Passes · newest first" right={<span style={{ fontSize: 11, color: colors.textMuted }}>click for the pass's checks</span>} />
          {passes.length === 0 ? <EmptyState title="No passes" /> : (
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>MT</th><th style={thStyle}>Mode</th><th style={numTh}>Pass</th><th style={numTh}>Warn</th><th style={numTh}>Fail</th><th style={thStyle}>Repairs</th></tr></thead>
              <tbody>{passes.slice().reverse().map((p) => {
                const c = counts(p); const openIt = openPass === p.at;
                return (
                  <React.Fragment key={p.at}>
                    <tr onClick={() => setOpenPass(openIt ? null : p.at)} style={{ cursor: 'pointer' }}>
                      <td style={{ ...tdStyle, whiteSpace: 'nowrap' }} title={p.at}>{denverTime(p.at)}</td>
                      <td style={tdStyle}>{p.live ? <Pill color={colors.success} style={{ fontSize: 10 }}>live</Pill> : <Pill color={colors.idle} style={{ fontSize: 10 }}>dry</Pill>}</td>
                      <td style={numTd}>{c.PASS ?? 0}</td><td style={{ ...numTd, color: c.WARN ? colors.warningText : colors.text }}>{c.WARN ?? 0}</td><td style={{ ...numTd, color: c.FAIL ? colors.dangerText : colors.text }}>{c.FAIL ?? 0}</td>
                      <td style={{ ...tdStyle, fontSize: 12 }}>{(p.repairs ?? []).join(' · ') || '—'}</td>
                    </tr>
                    {openIt && <tr><td colSpan={6} style={{ ...tdStyle, fontSize: 12, color: colors.textMuted }}>{p.checks.map((ck) => <div key={ck.name}><span style={{ color: vcolor(ck.verdict), fontWeight: 700 }}>{ck.verdict}</span> {ck.name} — {ck.measure}</div>)}</td></tr>}
                  </React.Fragment>
                );
              })}</tbody>
            </table>
          )}
        </Panel>
        <Panel>
          <SectionHeader title="Verdict flips today" />
          {flips.length === 0 ? <EmptyState title="No flips" hint="Every check kept its verdict across today's passes." /> : (
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>MT</th><th style={thStyle}>Check</th><th style={thStyle}>From → to</th></tr></thead>
              <tbody>{flips.slice().reverse().map((f, i) => <tr key={i}><td style={tdStyle}>{denverTime(f.at)}</td><td style={tdStyle}>{f.name}</td><td style={tdStyle}><Pill color={vcolor(f.from)} style={{ fontSize: 10 }}>{f.from}</Pill> → <Pill color={vcolor(f.to)} style={{ fontSize: 10 }}>{f.to}</Pill></td></tr>)}</tbody>
            </table>
          )}
          {day && ((day.repairs ?? []).length > 0 || (day.paged ?? []).length > 0) && (
            <div style={{ marginTop: 12, fontSize: 12, color: colors.textMuted }}>
              {(day.paged ?? []).length > 0 && <div>Paged today: {(day.paged ?? []).join(' · ')}</div>}
              {(day.repairs ?? []).map((r, i) => <div key={i}>{denverTime(r.at)} MT · {r.repairs.join(' · ')}</div>)}
            </div>
          )}
        </Panel>
      </div>

      <LedgerTable rows={ledger} title={`Live changes by the data agent · ${date}`} />
    </>
  );
};

// ───────────────────────────── per-environment view (operator 2026-10-27: "separate by environment as I have")
//
// The scheduling agent decides per lane (pmta_nx gates are keyed "A:<isp>" / "B:<isp>"; kumo_nx), verifies readiness
// per program, and the supervisor repairs per program. Each environment shows only its own lanes / programs.

export type EnvKey = 'kumo' | 'pmta_a' | 'pmta_b' | 'ses' | 'gmail' | 'colo1' | 'colo2';
export const ENV_PROGRAMS: Record<EnvKey, string[]> = {
  kumo: ['kumo_estate', 'nx', 'nx_engaged', 'nx_offer', 'nx_eo_supply'],
  pmta_a: ['nx', 'nx_engaged', 'nx_offer', 'mk_lanes', 'nx_eo_supply'],
  pmta_b: ['nx', 'nx_engaged', 'nx_offer', 'mk_lanes', 'nx_eo_supply'],
  ses: ['engaged', 'cold', 'cold_remail', 'cold_stepper', 'cold_supply_demand', 'family', 'family_stepper', 'family_supply', 'fresh', 'internal_auto', 'internal_auto_size', 'wcl', 'yahoo_ramp', 'nl_yf', 'acquisition'],
  gmail: ['gmail_sidecar'],
  colo1: [],
  colo2: [],
};
const ENV_LANE: Partial<Record<EnvKey, { lanes: string[]; gatePrefix?: string }>> = {
  kumo: { lanes: ['kumo_nx'] }, pmta_a: { lanes: ['pmta_nx', 'mk_yahoo'], gatePrefix: 'A:' }, pmta_b: { lanes: ['pmta_nx', 'mk_yahoo'], gatePrefix: 'B:' },
};
const ENV_NOTE: Record<EnvKey, string> = {
  kumo: 'KumoMTA Colo1 estate — nx lanes + em.* domains. The scheduling agent steps the kumo_nx lane; per-ISP gating stays in nx_ramp_gate.',
  pmta_a: 'PMTA Server A (15.204.101.125) — nx dedicated-IP lanes on 144.225.178.0/25 + mk lanes on 15.204.22.x. Gates keyed A:<isp>.',
  pmta_b: 'PMTA Server B (15.204.107.107) — nx dedicated-IP lanes on 144.225.178.128/25 + mk lanes on 15.204.38.x. Gates keyed B:<isp>.',
  ses: 'SES relay — the engaged board, cold / family / fresh sidecars, WCL and Internal Auto releases, yahoo ramp.',
  gmail: 'Gmail — gm.<apex> SES senders (MH on gt., HT/TRB on their own), the gmail sidecar 01:01 MT, A odd / B even days, levels on operator request only.',
  colo1: 'Colo 1 — no agent assigned yet.',
  colo2: 'Colo 2 (15.204.105.55, 199.235.0.0/24) — the warmup agent owns every speed and target here.',
};

// ───────────────────────────── gmail lane status (operator 2026-10-27: "displays the information I request of you daily")
//
// agents/jobs/gmail_lane_status.py writes state/gmail_lane/<D>.json — the supplemental record: today per property from the
// lake (distinct recipient_send_id; human clicks = is_machine_click false; 5.7.x blocks + the first one's MT time), the
// cells by NAME for today + 3 days, and the gmail_ramp levels/holds. The journal's readiness row is a 21:00 snapshot.

const pct = (n?: number, d?: number) => (n == null || !d ? '—' : `${((100 * n) / d).toFixed(1)}%`);

const GmailLanePanel: React.FC<{ lane: GmailLane | null | undefined; decision?: VolumeDecision }> = ({ lane, decision }) => {
  if (!lane) return <Panel style={{ marginBottom: 14 }}><EmptyState title="No gmail lane status for this day" hint="python3 -m agents.jobs.gmail_lane_status writes state/gmail_lane/<date>.json." /></Panel>;
  const t = lane.today.totals;
  const holds = Object.keys(lane.ramp.holds ?? {});
  const blockedCodes = lane.today.cells.filter((c) => (c.blocked ?? 0) > 0);
  return (
    <>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title={`Gmail lane · today ${lane.date}`} right={<span style={{ fontSize: 11, color: colors.textMuted }}>as of {denverTime(lane.as_of)} MT · lake, ses · NL-GM-NEWSLETTER</span>} />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(160px, 1fr))', gap: 14, marginBottom: 12 }}>
          <Stat label="Mailed" value={fmt(t.attempted)} sub={`${lane.today.cells.length} cells · ${fmt(t.recipients)} planned`} />
          <Stat label="Delivered" value={pct(t.delivered, t.attempted)} sub={fmt(t.delivered)} color={(t.delivered ?? 0) / (t.attempted || 1) < 0.98 ? colors.warning : colors.success} />
          <Stat label="Opens" value={pct(t.opens, t.delivered)} sub={fmt(t.opens)} />
          <Stat label="Human clicks" value={fmt(t.human_clicks)} />
          <Stat label="Complaints" value={fmt(t.complaints)} color={t.complaints ? colors.danger : colors.success} />
          <Stat label="5.7.x blocks" value={fmt(t.blocked)} sub={blockedCodes.map((c) => `${c.code} from ${c.first_block_mt ?? '—'} MT`).join(' · ') || 'none'} color={t.blocked ? colors.danger : colors.success} title="Policy blocks. A 5.7.1 run in the last waves preceded a full block on the next send 3 of 3 times (brain #16762)." />
          <Stat label="Volume decision" value={decision ? (decision.decision ?? decision.posture).toUpperCase() : '—'} sub={decision?.rule ?? `holds: ${holds.join(', ') || 'none'}`} title={`ramp holds: ${holds.join(', ') || 'none'}`} />
        </div>
        <div style={{ overflowX: 'auto' }}>
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Property</th><th style={thStyle}>Sender</th><th style={numTh}>Cap</th><th style={numTh}>Mailed</th><th style={numTh}>Delivered</th><th style={numTh}>Opens</th><th style={numTh}>Human clicks</th><th style={numTh}>Complaints</th><th style={numTh}>5.7.x</th><th style={thStyle}>Ramp</th></tr></thead>
            <tbody>{lane.today.cells.map((c) => (
              <tr key={c.campaign_id}>
                <td style={{ ...tdStyle, fontWeight: 600 }}>{c.code}</td>
                <td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted }}>{c.sender}</td>
                <td style={numTd}>{fmt(c.cap)}</td>
                <td style={numTd}>{fmt(c.attempted)}</td>
                <td style={{ ...numTd, color: c.attempted && (c.delivered ?? 0) / c.attempted < 0.98 ? colors.warningText : undefined }}>{pct(c.delivered, c.attempted)}</td>
                <td style={numTd}>{pct(c.opens, c.delivered)}</td>
                <td style={numTd}>{fmt(c.human_clicks)}</td>
                <td style={numTd}>{fmt(c.complaints)}</td>
                <td style={{ ...numTd, color: c.blocked ? colors.danger : undefined }}>{c.blocked ? `${fmt(c.blocked)} · ${c.first_block_mt ?? ''}` : '0'}</td>
                <td style={tdStyle}>{holds.includes(c.code) ? <Pill color={colors.warning} style={{ fontSize: 10 }}>held</Pill> : <Pill color={colors.success} style={{ fontSize: 10 }}>open</Pill>}</td>
              </tr>
            ))}</tbody>
          </table>
        </div>
      </Panel>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Gmail lane · schedule" right={<span style={{ fontSize: 11, color: colors.textMuted }}>cells verified by name · {(lane.rules ?? []).join(' · ')}</span>} />
        <div style={{ overflowX: 'auto' }}>
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Day</th><th style={numTh}>Cells</th><th style={numTh}>Recipients</th><th style={thStyle}>Per property (cap)</th><th style={thStyle}>Not built</th></tr></thead>
            <tbody>{lane.schedule.map((s) => (
              <tr key={s.day}>
                <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{s.day}</td>
                <td style={numTd}>{s.cells.length}/{s.due.length}</td>
                <td style={numTd}>{fmt(s.total)}</td>
                <td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted }}>{s.cells.map((c) => `${c.code} ${fmt(c.cap)}`).join(' · ') || '—'}</td>
                <td style={tdStyle}>{s.missing.length ? <Pill color={s.day === lane.date ? colors.danger : colors.idle} style={{ fontSize: 10 }}>{s.missing.length === s.due.length ? 'builds 23:50 MT the evening before' : s.missing.join(', ')}</Pill> : '—'}</td>
              </tr>
            ))}</tbody>
          </table>
        </div>
      </Panel>
    </>
  );
};

export const EnvironmentView: React.FC<{ env: EnvKey; date: string }> = ({ env, date }) => {
  const { data, loading, error, reload } = useAgentsOverview(date);
  const J = data?.scheduling.plan_for_date ?? null;
  const T = data?.scheduling.plan_tonight ?? null;
  const [plan, setPlan] = useState<'for_date' | 'tonight'>('for_date');
  const P = plan === 'tonight' ? T : J;
  const programs = ENV_PROGRAMS[env];
  const laneSpec = ENV_LANE[env];
  const lanes = useMemo(() => (P?.volume ?? []).filter((l) => laneSpec && laneSpec.lanes.includes(l.lane)).map((l) => ({
    ...l, gates: Object.fromEntries(Object.entries(l.gates ?? {}).filter(([k, g]) => typeof (g as Gate | undefined)?.verdict === 'string' && (!laneSpec?.gatePrefix || k.startsWith(laneSpec.gatePrefix)))),
  })), [P, laneSpec]);
  const pmtaServer = env === 'pmta_a' ? 'A' : env === 'pmta_b' ? 'B' : null;
  const readiness = useMemo(() => (P?.readiness ?? []).filter((r) => programs.includes(r.program)), [P, programs]);
  const sup = data?.scheduling.supervisor?.[date] ?? null;
  const supPrograms = Object.entries(sup ?? {}).filter(([p, v]) => programs.includes(p) && ((v.attempts ?? 0) > 0 || v.paged || v.outcome));
  const ledger = useMemo(() => (data?.ledger ?? []).filter((r) => (env === 'kumo'
    ? /kumo|NL-COLD-YF|KUMO-/i.test(`${r.kind} ${r.target}`)
    : programs.some((pg) => r.target.toLowerCase().includes(pg) || r.kind.toLowerCase().includes(pg)))), [data, programs, env]);

  if (error && !data) return <SectionError label="agents overview" error={error} onRetry={() => { void reload(); }} />;
  if (loading && !data) return <div style={{ color: colors.textMuted }}>Loading the agents' record…</div>;

  const rCounts = readiness.reduce<Record<string, number>>((a, r) => { a[r.verdict] = (a[r.verdict] || 0) + 1; return a; }, {});
  const gateRows = lanes.flatMap((l) => Object.entries(l.gates).map(([k, g]) => ({ lane: l.lane, posture: l.posture, date: l.date, key: k, ...g })));
  const gCounts = gateRows.reduce<Record<string, number>>((a, g) => { const v = g.verdict.split(' ')[0]; a[v] = (a[v] || 0) + 1; return a; }, {});

  return (
    <>
      <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 12 }}>{ENV_NOTE[env]}</div>
      {env === 'ses' && <SesLaneToday report={data?.ses?.lane} date={date} />}
      {env === 'kumo' && <KumoLaneToday report={data?.kumo?.lane} date={date} />}
      {pmtaServer && <PmtaDailyRead read={data?.pmta?.read ?? null} server={pmtaServer} date={date} />}
      {env === 'gmail' && <GmailLanePanel lane={data?.gmail?.lane} decision={(J?.volume ?? []).find((l) => l.lane === 'gmail')} />}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Decisions · scheduling agent" icon={faCalendarCheck}
          right={(
            <div style={{ display: 'flex', gap: 6 }}>
              <button style={{ ...btnStyle, opacity: plan === 'for_date' ? 1 : 0.6 }} onClick={() => setPlan('for_date')}>Plan that built {date}{J ? '' : ' (none)'}</button>
              <button style={{ ...btnStyle, opacity: plan === 'tonight' ? 1 : 0.6 }} onClick={() => setPlan('tonight')}>Tonight's plan{T ? '' : ' (not yet)'}</button>
            </div>
          )} />
        {!P ? <EmptyState title="No plan journal" hint="The scheduling agent writes its journal at the 21:00 MT plan." /> : (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 14 }}>
            <Stat label="Plan" value={P.plan_date} sub={`ran ${P.started ? denverTime(P.started) : '—'} MT · builds ${P.tomorrow ?? '—'} · ${P.live ? 'LIVE' : 'DRY'}`} />
            {laneSpec && <Stat label="Volume decision" value={lanes.length ? lanes.map((l) => `${l.lane} ${(l.decision ?? l.posture).toUpperCase()}`).join(' · ') : '—'} sub={lanes.length ? `for ${lanes[0].date} · ${gCounts.STEP ?? 0} gates STEP · ${gCounts.HOLD ?? 0} HOLD · ${gCounts.CUT ?? 0} CUT (held, never applied)${lanes.map((l) => (l.rule ? ` · ${l.lane}: ${l.rule}` : '')).join('')}` : 'no decision for this lane'} color={gCounts.CUT ? colors.warning : undefined} title="UP only when every gate says STEP; any HOLD holds the lane; a CUT verdict is held and raised to you, never applied." />}
            <Stat label="Programs ready" value={`${rCounts.PASS ?? 0}/${readiness.length}`} sub={`${rCounts.WARN ?? 0} warn · ${rCounts.FAIL ?? 0} fail · programs: ${programs.join(', ')}`} color={rCounts.FAIL ? colors.danger : rCounts.WARN ? colors.warning : colors.success} />
            <Stat label="Repairs by supervisor" value={supPrograms.length} sub={supPrograms.map(([p, v]) => `${p} ${v.outcome ?? ''}`.trim()).join(' · ') || 'nothing needed repair'} color={supPrograms.some(([, v]) => v.outcome === 'FAIL') ? colors.danger : undefined} />
            <Stat label="Needs you" value={(P.needs_you ?? []).length} color={(P.needs_you ?? []).length ? colors.warning : colors.success} sub={(P.needs_you ?? []).slice(0, 2).join(' · ') || 'nothing escalated'} title={(P.needs_you ?? []).join('\n')} />
          </div>
        )}
      </Panel>

      {gateRows.length > 0 && (
        <Panel style={{ marginBottom: 14 }}>
          <SectionHeader title="Gates the agent read · per mailbox provider" right={<span style={{ fontSize: 11, color: colors.textMuted }}>yesterday's delivery per lane × provider → STEP / HOLD / CUT</span>} />
          <div style={{ overflowX: 'auto' }}>
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>Gate</th><th style={thStyle}>Verdict</th><th style={numTh}>Accepted %</th><th style={numTh}>Blocked %</th><th style={numTh}>Deferred %</th><th style={numTh}>Msgs</th></tr></thead>
              <tbody>{gateRows.map((g) => <tr key={g.key}><td style={tdStyle}>{g.key}</td><td style={tdStyle}><Pill color={vcolor(g.verdict)} style={{ fontSize: 10 }}>{g.verdict}</Pill></td><td style={numTd}>{g.acc ?? '—'}</td><td style={numTd}>{g.blk ?? '—'}</td><td style={numTd}>{g.dfr ?? '—'}</td><td style={numTd}>{fmt(g.msgs)}</td></tr>)}</tbody>
            </table>
          </div>
          {lanes.flatMap((l) => l.needs_you ?? []).map((n, i) => <div key={i} style={{ fontSize: 12, color: colors.warningText, marginTop: 6 }}>{n}</div>)}
        </Panel>
      )}

      {readiness.length > 0 && (
        <Panel style={{ marginBottom: 14 }}>
          <SectionHeader title="Programs · built and verified" right={pmtaServer ? <span style={{ fontSize: 11, color: colors.warningText }}>nx programs count Kumo + PMTA A + B together — server truth is the PMTA daily read above</span> : undefined} />
          <div style={{ overflowX: 'auto' }}>
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>Program</th><th style={thStyle}>Date</th><th style={thStyle}>Verdict</th><th style={numTh}>Cells</th><th style={numTh}>Planned</th><th style={numTh}>Sent</th><th style={thStyle}>Build</th><th style={thStyle}>Detail</th></tr></thead>
              <tbody>{readiness.map((r, i) => (
                <tr key={i}><td style={tdStyle}>{r.program}</td><td style={tdStyle}>{r.date}</td><td style={tdStyle}><Pill color={vcolor(r.verdict)} style={{ fontSize: 10 }}>{r.verdict}</Pill></td><td style={numTd}>{fmt(r.cells)}</td><td style={numTd}>{fmt(r.planned)}</td><td style={numTd}>{fmt(r.sent)}</td><td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{r.build_at ?? '—'}</td><td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted, maxWidth: 420 }}>{(r.problems ?? []).join(' · ') || r.detail || r.note || ''}</td></tr>
              ))}</tbody>
            </table>
          </div>
        </Panel>
      )}

      {supPrograms.length > 0 && (
        <Panel style={{ marginBottom: 14 }}>
          <SectionHeader title={`Supervisor repairs · ${date}`} right={<span style={{ fontSize: 11, color: colors.textMuted }}>every 15 min 20:00–07:00 MT · rebuilds a missing cell by name, pages at anchor−30</span>} />
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Program</th><th style={numTh}>Attempts</th><th style={thStyle}>Outcome</th><th style={thStyle}>Last</th><th style={thStyle}>Paged</th></tr></thead>
            <tbody>{supPrograms.map(([p, v]) => <tr key={p}><td style={tdStyle}>{p}</td><td style={numTd}>{v.attempts ?? 0}</td><td style={tdStyle}>{v.outcome ? <Pill color={vcolor(v.outcome)} style={{ fontSize: 10 }}>{v.outcome}</Pill> : '—'}</td><td style={tdStyle}>{v.last_at ? `${denverTime(v.last_at)} MT · rc ${(v.last_rcs ?? []).join(',')}` : '—'}</td><td style={tdStyle}>{v.paged ? <Pill color={colors.warning} style={{ fontSize: 10 }}>paged</Pill> : '—'}</td></tr>)}</tbody>
          </table>
        </Panel>
      )}

      {env === 'ses' && P && (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(380px, 1fr))', gap: 14, marginBottom: 14 }}>
          <Panel>
            <SectionHeader title="Offers · what the agent changed" />
            {!P.rules?.changed || Object.keys(P.rules.changed).length === 0 ? <EmptyState title="No rule changes" hint="offer_rules.json already matched the mandate." /> : (
              <table style={tableStyle}><tbody>{Object.entries(P.rules.changed).map(([k, v]) => <tr key={k}><td style={{ ...tdStyle, fontWeight: 600, whiteSpace: 'nowrap' }}>{k}</td><td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted }}>{typeof v === 'string' ? v : JSON.stringify(v).slice(0, 240)}</td></tr>)}</tbody></table>
            )}
            {P.board_summary && <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 8 }}>Board it filled: <span style={{ color: colors.text }}>{P.board_summary}</span></div>}
            {(P.scorer ?? []).map((sc, i) => (
              <div key={i} style={{ marginTop: 10, fontSize: 12 }}>
                <div style={{ color: colors.textMuted, marginBottom: 2 }}>scorer · {sc.mode} · {(sc.dates ?? []).join(', ')} · rc {sc.rc}</div>
                {(sc.lines ?? []).filter((l) => l.startsWith('==')).map((l, j) => <div key={j} style={{ fontFamily: 'ui-monospace, Menlo, monospace', fontSize: 11, color: colors.text }}>{l}</div>)}
              </div>
            ))}
          </Panel>
          <Panel>
            <SectionHeader title="CPM deals it paced" />
            {!P.cpm?.length ? <EmptyState title="No CPM deals" /> : (
              <table style={tableStyle}>
                <thead><tr><th style={thStyle}>Deal</th><th style={numTh}>Sent</th><th style={numTh}>Owed</th><th style={numTh}>Need/day</th><th style={thStyle}>Finishes</th></tr></thead>
                <tbody>{P.cpm.map((d) => <tr key={d.name}><td style={tdStyle}>{d.name}{d.token ? <span style={{ color: colors.textFaint }}> · {d.token}</span> : <span style={{ color: colors.warningText }}> · no token</span>}</td><td style={numTd}>{fmt(d.sent)}</td><td style={numTd}>{fmt(d.owed)}</td><td style={numTd}>{fmt(d.need_per_day)}</td><td style={tdStyle}><Pill color={d.finishes ? colors.success : colors.warning} style={{ fontSize: 10 }}>{d.finishes ? 'yes' : 'at risk'}</Pill></td></tr>)}</tbody>
              </table>
            )}
          </Panel>
        </div>
      )}

      <LedgerTable rows={ledger} title={`Live changes touching this environment · ${date}`} />
    </>
  );
};

export const AgentsRefreshButton: React.FC<{ onClick: () => void; loading?: boolean }> = ({ onClick, loading }) => (
  <button style={btnStyle} onClick={onClick} disabled={loading}><FontAwesomeIcon icon={faRotate} spin={!!loading} /> Refresh</button>
);
