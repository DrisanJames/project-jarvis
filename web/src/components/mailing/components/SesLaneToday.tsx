// SesLaneToday.tsx — the SES offer board today, as the operator asks for it daily (operator 2026-10-26: "I only want
// today, stop providing aggregates"; 2026-10-27: the SES screen must show it). One row per slot × offer, Yahoo Family
// vs Non-Yahoo Family, no totals. Displays agents/reporting/ses_lane_today.py's state file raw (every 30 min,
// state/ses_lane/<D>.json via GET /api/mailing/agents/overview → ses.lane); nothing is computed here but formatting.

import React from 'react';
import { faEnvelopeOpenText } from '@fortawesome/free-solid-svg-icons';
import { colors, tdStyle, thStyle, numTd, numTh, tableStyle } from '../shared/theme';
import { Panel, SectionHeader, EmptyState, Pill } from '../shared/ui';

export type SesLaneRow = {
  slot: string; offer: string; cells: number; status: string; delivered: number; attempted: number;
  dlv_pct: number | null; hard_pct: number | null; cmp_pct: number | null; open_pct: number | null; click_pct: number | null;
  conv: number; payout: number | null; basis: 'cpa' | 'cpm est' | 'not tracked'; per_k: number | null;
};
export type SesLaneReport = {
  date: string; as_of: string; cells: number; groups: Record<string, SesLaneRow[]>;
  upcoming: { slot: string; offer: string; cells: number; status: string; recipients: number }[];
  untracked: string[]; cpm: Record<string, number>; note?: string;
};

const GROUP_ORDER = ['Non-Yahoo Family', 'Yahoo Family'];
// Delivery floors from the lane doctrine (core.md §5.9 rule 6): ≥98% delivered, family ≥99%; complaints ≤0.05%.
const FLOOR: Record<string, number> = { 'Non-Yahoo Family': 98, 'Yahoo Family': 99 };
const n = (v: number | null | undefined, d = 2) => (v == null ? '—' : v.toLocaleString('en-US', { minimumFractionDigits: d, maximumFractionDigits: d }));
const int = (v: number) => v.toLocaleString('en-US');

const GroupTable: React.FC<{ group: string; rows: SesLaneRow[] }> = ({ group, rows }) => (
  <div style={{ marginBottom: 14 }}>
    <div style={{ fontSize: 13, fontWeight: 600, color: colors.text, margin: '4px 0 6px' }}>{group}</div>
    {rows.length === 0 ? <EmptyState title="Nothing delivered yet" /> : (
      <div style={{ overflowX: 'auto' }}>
        <table style={tableStyle}>
          <thead><tr>
            <th style={thStyle}>Slot</th><th style={thStyle}>Offer</th><th style={numTh}>Cells</th><th style={thStyle}>Status</th>
            <th style={numTh}>Delivered</th><th style={numTh}>Dlv %</th><th style={numTh}>Hard %</th><th style={numTh}>Cmp %</th>
            <th style={numTh}>Open %</th><th style={numTh}>Click %</th><th style={numTh}>Conv</th><th style={numTh}>Payout</th><th style={numTh}>$/1k</th>
          </tr></thead>
          <tbody>{rows.map((r) => (
            <tr key={`${r.slot}|${r.offer}`}>
              <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{r.slot}</td>
              <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{r.offer}</td>
              <td style={numTd}>{r.cells}</td>
              <td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted, whiteSpace: 'nowrap' }}>{r.status}</td>
              <td style={numTd}>{int(r.delivered)}</td>
              <td style={{ ...numTd, color: r.dlv_pct != null && r.dlv_pct < FLOOR[group] ? colors.warningText : undefined }}>{n(r.dlv_pct)}</td>
              <td style={numTd}>{n(r.hard_pct)}</td>
              <td style={{ ...numTd, color: r.cmp_pct != null && r.cmp_pct > 0.05 ? colors.danger : undefined }}>{n(r.cmp_pct, 3)}</td>
              <td style={numTd}>{n(r.open_pct)}</td>
              <td style={numTd}>{n(r.click_pct, 3)}</td>
              <td style={numTd}>{r.conv}</td>
              <td style={numTd}>{r.basis === 'not tracked'
                ? <Pill color={colors.warning} style={{ fontSize: 10 }}>not tracked</Pill>
                : <span title={r.basis === 'cpm est' ? 'CPM deal: delivered ÷ 1,000 × eCPM goal' : 'Everflow conversions'}>${n(r.payout, 0)}{r.basis === 'cpm est' ? ' cpm' : ''}</span>}</td>
              <td style={numTd}>{n(r.per_k)}</td>
            </tr>
          ))}</tbody>
        </table>
      </div>
    )}
  </div>
);

export const SesLaneToday: React.FC<{ report: SesLaneReport | null | undefined; date: string }> = ({ report, date }) => (
  <Panel style={{ marginBottom: 14 }}>
    <SectionHeader title={`SES offer board · ${date}`} icon={faEnvelopeOpenText}
      right={<span style={{ fontSize: 11, color: colors.textMuted }}>{report ? `as of ${report.as_of} · ${report.cells} cells · slot × offer, no totals` : 'ses_lane_today · every 30 min'}</span>} />
    {!report ? <EmptyState title="No SES board read for this date" hint="agents/reporting/ses_lane_today.py writes it every 30 minutes 04:10–21:40 MT." /> : (
      <>
        {GROUP_ORDER.filter((g) => report.groups[g]).map((g) => <GroupTable key={g} group={g} rows={report.groups[g]} />)}
        {report.upcoming.length > 0 && (
          <div style={{ marginBottom: 10 }}>
            <div style={{ fontSize: 13, fontWeight: 600, color: colors.text, margin: '4px 0 6px' }}>No deliveries yet</div>
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>Slot</th><th style={thStyle}>Offer</th><th style={numTh}>Cells</th><th style={numTh}>Recipients</th><th style={thStyle}>Status</th></tr></thead>
              <tbody>{report.upcoming.map((u) => <tr key={`${u.slot}|${u.offer}`}><td style={tdStyle}>{u.slot}</td><td style={tdStyle}>{u.offer}</td><td style={numTd}>{u.cells}</td><td style={numTd}>{int(u.recipients)}</td><td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted }}>{u.status}</td></tr>)}</tbody>
            </table>
          </div>
        )}
        {report.untracked.length > 0 && (
          <div style={{ fontSize: 12, color: colors.warningText }}>Not tracked — no CPM deal and no Everflow conversion in 60 days: {report.untracked.join(', ')}. Payout for these reads $0 whatever they earn.</div>
        )}
        {report.note && <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 6 }}>{report.note}</div>}
      </>
    )}
  </Panel>
);

export default SesLaneToday;
