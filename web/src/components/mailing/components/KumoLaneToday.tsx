// KumoLaneToday.tsx — the Kumo Colo1 day as the operator asks for it daily (operator 2026-10-26: "I only want today, no
// aggregate"; 2026-10-27: the Kumo screen must display the information requested daily, as a supplemental write).
// Displays agents/reporting/kumo_lane_today.py's state file raw (every 30 min, state/kumo_lane/<D>.json via
// GET /api/mailing/agents/overview → kumo.lane); nothing is computed here but formatting. Colo1 only — the scheduling
// journal's nx readiness mixes PMTA + Kumo cells; the board below does not.

import React from 'react';
import { faServer } from '@fortawesome/free-solid-svg-icons';
import { colors, tdStyle, thStyle, numTd, numTh, tableStyle } from '../shared/theme';
import { Panel, SectionHeader, EmptyState, Pill, Stat } from '../shared/ui';

type BoardRow = { slot: string; program: string; offer: string; cells: number; planned: number; sent: number; delivering: number; status: string; yesterday: number | null; change_pct: number | null };
type OutRow = {
  slot: string; program: string; offer: string; attempted: number; delivered: number; dlv_pct: number | null; hard: number; soft: number;
  hard_pct: number | null; soft_pct: number | null; opens: number; opens_h: number; clicks: number; clicks_h: number; unsubs: number;
  complaints: number; cmp_pct: number | null; conv: number; payout: number;
};
type IspRow = { isp: string; group: string; attempted: number; delivered: number; dlv_pct: number | null; hard: number; soft: number; hm08: number };
type Decision = { lane: string; for: string; decision: string; rule: string; gate_date: string | null; gates_stale: boolean; gates: { key: string; verdict: string; why: string }[] };
export type KumoLaneReport = {
  date: string; as_of: string; host: string; cells: number; board: BoardRow[]; groups: Record<string, OutRow[]>; isps: IspRow[];
  apple_hm08: { domain: string; attempted: number; hm08: number; hm08_pct: number | null }[];
  cap: { segment: string; members: number | null; rebuilt_mt?: string | null; age_h?: number | null; stale?: boolean };
  ramp: { freeze?: string | null; freeze_resume?: string | null; growth?: number | null; family_growth?: number | null; family_start?: string | null };
  decisions: Decision[]; ledger: { id: string; ts: string; actor?: string; kind: string; target: string; why?: string }[]; note?: string;
};

const GROUP_ORDER = ['Non-Yahoo Family', 'Yahoo Family'];
// Delivery floors from the lane doctrine (core.md §5.9 rule 6): ≥98% delivered, family ≥99%; complaints ≤0.05%.
const FLOOR: Record<string, number> = { 'Non-Yahoo Family': 98, 'Yahoo Family': 99 };
const n = (v: number | null | undefined, d = 2) => (v == null ? '—' : v.toLocaleString('en-US', { minimumFractionDigits: d, maximumFractionDigits: d }));
const int = (v: number | null | undefined) => (v == null ? '—' : v.toLocaleString('en-US'));
const sub = { fontSize: 13, fontWeight: 600, color: colors.text, margin: '10px 0 6px' } as const;
const nowrap = { ...tdStyle, whiteSpace: 'nowrap' } as const;

const Board: React.FC<{ rows: BoardRow[] }> = ({ rows }) => (
  <div style={{ overflowX: 'auto' }}>
    <table style={tableStyle}>
      <thead><tr>
        <th style={thStyle}>Slot</th><th style={thStyle}>Program</th><th style={thStyle}>Offer</th><th style={numTh}>Cells</th>
        <th style={numTh}>Planned</th><th style={numTh}>Sent</th><th style={numTh}>Yesterday</th><th style={numTh}>Change</th><th style={thStyle}>Status</th>
      </tr></thead>
      <tbody>{rows.map((b) => (
        <tr key={`${b.slot}|${b.program}|${b.offer}`}>
          <td style={nowrap}>{b.slot}</td><td style={nowrap}>{b.program}</td><td style={nowrap}>{b.offer}</td><td style={numTd}>{b.cells}</td>
          <td style={numTd}>{int(b.planned)}</td><td style={numTd}>{int(b.sent)}</td><td style={numTd}>{int(b.yesterday)}</td>
          <td style={{ ...numTd, color: b.change_pct != null && b.change_pct < 0 ? colors.warningText : undefined }}>{b.change_pct == null ? '—' : `${b.change_pct > 0 ? '+' : ''}${n(b.change_pct, 1)}%`}</td>
          <td style={{ ...nowrap, fontSize: 12, color: colors.textMuted }}>{b.status}</td>
        </tr>
      ))}</tbody>
    </table>
  </div>
);

const Outcomes: React.FC<{ group: string; rows: OutRow[] }> = ({ group, rows }) => (
  <>
    <div style={sub}>{group}</div>
    {rows.length === 0 ? <EmptyState title="Nothing delivered yet" /> : (
      <div style={{ overflowX: 'auto' }}>
        <table style={tableStyle}>
          <thead><tr>
            <th style={thStyle}>Slot</th><th style={thStyle}>Program</th><th style={thStyle}>Offer</th><th style={numTh}>Attempted</th>
            <th style={numTh}>Dlv %</th><th style={numTh}>Hard</th><th style={numTh}>Soft</th><th style={numTh} title="all / human (verdict filter)">Opens</th>
            <th style={numTh} title="all / human">Clicks</th><th style={numTh}>Unsubs</th><th style={numTh}>Cmp</th><th style={numTh}>Conv</th><th style={numTh}>Payout</th>
          </tr></thead>
          <tbody>{rows.map((r) => (
            <tr key={`${r.slot}|${r.program}|${r.offer}`}>
              <td style={nowrap}>{r.slot}</td><td style={nowrap}>{r.program}</td><td style={nowrap}>{r.offer}</td>
              <td style={numTd}>{int(r.attempted)}</td>
              <td style={{ ...numTd, color: r.dlv_pct != null && r.dlv_pct < FLOOR[group] ? colors.warningText : undefined }}>{n(r.dlv_pct)}</td>
              <td style={{ ...numTd, color: r.hard ? colors.danger : undefined }}>{int(r.hard)}</td>
              <td style={numTd}>{int(r.soft)}</td>
              <td style={numTd}>{int(r.opens)} / {int(r.opens_h)}</td>
              <td style={numTd}>{int(r.clicks)} / {int(r.clicks_h)}</td>
              <td style={numTd}>{int(r.unsubs)}</td>
              <td style={{ ...numTd, color: r.cmp_pct != null && r.cmp_pct > 0.05 ? colors.danger : undefined }}>{int(r.complaints)}</td>
              <td style={numTd}>{r.conv}</td>
              <td style={numTd}>{r.payout ? `$${n(r.payout, 0)}` : '—'}</td>
            </tr>
          ))}</tbody>
        </table>
      </div>
    )}
  </>
);

export const KumoLaneToday: React.FC<{ report: KumoLaneReport | null | undefined; date: string }> = ({ report, date }) => {
  if (!report) {
    return (
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title={`Kumo Colo1 · ${date}`} icon={faServer} right={<span style={{ fontSize: 11, color: colors.textMuted }}>kumo_lane_today · every 30 min</span>} />
        <EmptyState title="No Kumo read for this date" hint="agents/reporting/kumo_lane_today.py writes it every 30 minutes 04:15–21:45 MT." />
      </Panel>
    );
  }
  const { cap, ramp } = report;
  const planned = report.board.reduce((a, b) => a + b.planned, 0);
  const yday = report.board.reduce((a, b) => a + (b.yesterday ?? 0), 0);
  return (
    <>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title={`Kumo Colo1 · ${report.date}`} icon={faServer}
          right={<span style={{ fontSize: 11, color: colors.textMuted }}>as of {report.as_of} · {report.cells} cells on {report.host} · slot × program × offer</span>} />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(170px, 1fr))', gap: 14, marginBottom: 6 }}>
          {report.decisions.map((d) => (
            <Stat key={d.lane} label={`Decision · ${d.lane}`} value={d.decision || '—'}
              color={d.gates_stale ? colors.warning : d.decision === 'HOLD' ? colors.warningText : colors.success}
              sub={d.gates_stale ? `gates STALE (${d.gate_date})` : d.gates.length ? `${d.gates.filter((g) => g.verdict === 'STEP').length}/${d.gates.length} ISPs STEP · gates ${d.gate_date ?? ''}` : (d.rule || `for ${d.for}`)}
              title={[d.rule, ...d.gates.map((g) => `${g.key}: ${g.verdict} — ${g.why}`)].filter(Boolean).join('\n')} />
          ))}
          <Stat label="Planned today" value={int(planned)} sub={yday ? `${planned >= yday ? '+' : ''}${n((100 * (planned - yday)) / yday, 1)}% vs yesterday's same rows` : 'no prior-day rows'} />
          <Stat label={cap.segment} value={int(cap.members)} sub={cap.rebuilt_mt ? `rebuilt ${cap.rebuilt_mt} MT · ${n(cap.age_h ?? null, 1)} h old` : 'never rebuilt'}
            color={cap.stale ? colors.danger : undefined} title="5 Kumo sends, never opened → excluded from every Kumo cell (kumo_warm refuses past 26 h)." />
          <Stat label="Ramp" value={ramp.freeze_resume ? `resumed ${ramp.freeze_resume}` : ramp.freeze ? `frozen ${ramp.freeze}` : '—'}
            sub={`cold +${n((ramp.growth ?? 0) * 100, 0)}%/day · family +${n((ramp.family_growth ?? 0) * 100, 0)}%/send`}
            color={ramp.family_growth ? undefined : colors.warningText} title={`nx_cold_ramp.json — freeze.kumo ${ramp.freeze ?? '—'}, freeze_resume.kumo ${ramp.freeze_resume ?? '—'}, family_ab.kumo.growth ${ramp.family_growth ?? '—'} (from ${ramp.family_start ?? '—'})`} />
        </div>
        <div style={sub}>Board</div>
        <Board rows={report.board} />
        {GROUP_ORDER.filter((g) => report.groups[g]).map((g) => <Outcomes key={g} group={g} rows={report.groups[g]} />)}
      </Panel>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(340px, 1fr))', gap: 14, marginBottom: 14 }}>
        <Panel>
          <SectionHeader title="Delivery by ISP" right={<span style={{ fontSize: 11, color: colors.textMuted }}>terminal outcomes, lake source=kumo</span>} />
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>ISP</th><th style={numTh}>Attempted</th><th style={numTh}>Dlv %</th><th style={numTh}>Hard</th><th style={numTh}>Soft</th><th style={numTh}>HM08</th></tr></thead>
            <tbody>{report.isps.map((i) => (
              <tr key={i.isp}>
                <td style={tdStyle}>{i.isp}{i.group === 'Yahoo Family' ? <Pill color={colors.info} style={{ fontSize: 9, marginLeft: 6 }}>family</Pill> : null}</td>
                <td style={numTd}>{int(i.attempted)}</td>
                <td style={{ ...numTd, color: i.dlv_pct != null && i.dlv_pct < FLOOR[i.group] ? colors.warningText : undefined }}>{n(i.dlv_pct)}</td>
                <td style={{ ...numTd, color: i.hard ? colors.danger : undefined }}>{int(i.hard)}</td><td style={numTd}>{int(i.soft)}</td><td style={numTd}>{i.hm08 ? int(i.hm08) : '—'}</td>
              </tr>
            ))}</tbody>
          </table>
        </Panel>
        <Panel>
          <SectionHeader title="Apple HM08 · by sending domain" right={<span style={{ fontSize: 11, color: colors.textMuted }}>content block, not volume</span>} />
          {report.apple_hm08.length === 0 ? <EmptyState title="No Apple outcomes yet" /> : (
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>Domain</th><th style={numTh}>Apple attempted</th><th style={numTh}>HM08</th><th style={numTh}>HM08 %</th></tr></thead>
              <tbody>{report.apple_hm08.map((a) => (
                <tr key={a.domain}><td style={tdStyle}>{a.domain}</td><td style={numTd}>{int(a.attempted)}</td><td style={numTd}>{int(a.hm08)}</td>
                  <td style={{ ...numTd, color: (a.hm08_pct ?? 0) >= 10 ? colors.danger : undefined }}>{n(a.hm08_pct, 1)}</td></tr>
              ))}</tbody>
            </table>
          )}
        </Panel>
      </div>

      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Kumo live changes · today" right={<span style={{ fontSize: 11, color: colors.textMuted }}>change ledger, Kumo rows only (agents + Mac sessions)</span>} />
        {report.ledger.length === 0 ? <EmptyState title="No Kumo live changes recorded today" /> : (
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>When (UTC)</th><th style={thStyle}>Kind</th><th style={thStyle}>Target</th><th style={thStyle}>Why</th><th style={thStyle}>Id</th></tr></thead>
            <tbody>{report.ledger.map((r) => (
              <tr key={r.id}><td style={nowrap}>{r.ts.slice(11, 16)}</td><td style={nowrap}>{r.kind}</td><td style={tdStyle}>{r.target}</td>
                <td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted }}>{r.why}</td><td style={{ ...nowrap, fontSize: 11, color: colors.textFaint }}>{r.id}</td></tr>
            ))}</tbody>
          </table>
        )}
        {report.note && <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 8 }}>{report.note}</div>}
      </Panel>
    </>
  );
};

export default KumoLaneToday;
