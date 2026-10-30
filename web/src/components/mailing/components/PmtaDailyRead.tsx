// PmtaDailyRead.tsx — the PMTA A / PMTA B daily read, supplemental to the scheduling agent's view
// (operator 2026-10-27: "confirm it … displays the information I request of you daily. This should be a
// supplemental write").
//
// Source: agents/reporting/pmta_daily_read.py (off-box, hourly) → S3 state/pmta_read/<D>.json → bundled raw
// by GET /api/mailing/agents/overview as `pmta.read`. Delivery is the lake (pmta_lanes: one row per message,
// final outcome delivered > blocked > list_hard > soft); cells are PG by scheduled Denver date. Nothing is
// computed server-side; the per-server and Yahoo-family splits are derived here.

import React, { useMemo, useState } from 'react';
import { faServer } from '@fortawesome/free-solid-svg-icons';
import { colors, tdStyle, thStyle, numTd, numTh, tableStyle, btnStyle } from '../shared/theme';
import { Panel, SectionHeader, Stat, EmptyState, Pill } from '../shared/ui';

type Group = 'family' | 'non_family';
type LaneRow = {
  server: string; lane: string; isp: string; group: Group; messages: number; delivered: number; blocked: number; list_hard: number;
  soft: number; pending: number; deferred: number; first_try: number; terminal: number;
  accepted_pct: number | null; blocked_pct: number | null; deferred_pct: number | null; first_try_pct: number | null; verdict: string;
};
type TokenRow = { server: string; lane: string; isp: string; group: Group; tok: string; cls: string; messages: number; events: number };
type IpRow = { server: string; lane: string; isp: string; ip: string; flagged: number; delivered: number };
type Delivery = { date: string; as_of: string; settled: Record<string, boolean>; lanes: LaneRow[]; tokens: TokenRow[]; flagged_ips: IpRow[] };
type CellRow = { server: string; group: Group; slot: string; cells: number; planned: number; sent: number; status: Record<string, number> };
export type PmtaRead = {
  date: string; generated_at: string; family_isps: string[];
  today: Delivery; yesterday: Delivery | null;
  cells: { today: CellRow[]; tomorrow: CellRow[] };
  levels: {
    cold_freeze?: string | null; cold_freeze_resume?: string | null; max_dod?: number | null;
    nx_family_hold_total: number; nx_family_hold_by_isp: Record<string, number>; nx_family_lanes: number;
    mk_hold: Record<string, number>; mk_hold_total: number;
  };
};

const GROUP_LABEL: Record<Group, string> = { family: 'Yahoo family', non_family: 'Non-yahoo family' };
const VERDICT_COLOR: Record<string, string> = { STEP: colors.success, HOLD: colors.warning, CUT: colors.danger, THIN: colors.idle };
const vcolor = (v: string) => VERDICT_COLOR[v.split(' ')[0]] ?? colors.idle;
const fmt = (n: number | null | undefined) => (n == null ? '—' : n.toLocaleString('en-US'));
const pct = (n: number, d: number) => (d ? Math.round((1000 * n) / d) / 10 : null);
const pctS = (v: number | null | undefined) => (v == null ? '—' : `${v.toFixed(1)}%`);
function mt(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : new Intl.DateTimeFormat('en-US', { timeZone: 'America/Denver', hour: '2-digit', minute: '2-digit', hour12: false }).format(d);
}

function sumLanes(rows: LaneRow[]) {
  const t = rows.reduce((a, r) => ({
    messages: a.messages + r.messages, delivered: a.delivered + r.delivered, blocked: a.blocked + r.blocked,
    list_hard: a.list_hard + r.list_hard, soft: a.soft + r.soft, deferred: a.deferred + r.deferred, first_try: a.first_try + r.first_try,
  }), { messages: 0, delivered: 0, blocked: 0, list_hard: 0, soft: 0, deferred: 0, first_try: 0 });
  const terminal = t.delivered + t.blocked + t.list_hard + t.soft;
  return { ...t, terminal, acc: pct(t.delivered, terminal), blk: pct(t.blocked, terminal), dfr: pct(t.deferred, t.messages), first: pct(t.first_try, t.delivered) };
}
const sumCells = (rows: CellRow[]) => rows.reduce((a, r) => ({ cells: a.cells + r.cells, planned: a.planned + r.planned, sent: a.sent + r.sent }), { cells: 0, planned: 0, sent: 0 });

const DeliveryTable: React.FC<{ rows: LaneRow[] }> = ({ rows }) => (
  <div style={{ overflowX: 'auto' }}>
    <table style={tableStyle}>
      <thead><tr>
        <th style={thStyle}>Lane · ISP</th><th style={numTh}>Messages</th><th style={numTh}>Delivered</th><th style={numTh}>Accepted</th>
        <th style={numTh}>First try</th><th style={numTh}>Deferred</th><th style={numTh}>Blocked</th><th style={numTh}>Blk %</th>
        <th style={numTh}>Hard</th><th style={numTh}>Pending</th><th style={thStyle}>Verdict</th>
      </tr></thead>
      <tbody>
        {(['family', 'non_family'] as Group[]).map((g) => {
          const rs = rows.filter((r) => r.group === g).sort((a, b) => b.messages - a.messages);
          if (!rs.length) return null;
          const t = sumLanes(rs);
          return (
            <React.Fragment key={g}>
              <tr style={{ background: 'color-mix(in srgb, var(--j-violet-strong) 8%, transparent)' }}>
                <td style={{ ...tdStyle, fontWeight: 700 }}>{GROUP_LABEL[g]}</td>
                <td style={{ ...numTd, fontWeight: 700 }}>{fmt(t.messages)}</td><td style={{ ...numTd, fontWeight: 700 }}>{fmt(t.delivered)}</td>
                <td style={{ ...numTd, fontWeight: 700 }}>{pctS(t.acc)}</td><td style={numTd}>{pctS(t.first)}</td><td style={numTd}>{pctS(t.dfr)}</td>
                <td style={numTd}>{fmt(t.blocked)}</td><td style={numTd}>{pctS(t.blk)}</td><td style={numTd}>{fmt(t.list_hard)}</td>
                <td style={numTd}>{fmt(rs.reduce((a, r) => a + r.pending, 0))}</td><td style={tdStyle} />
              </tr>
              {rs.map((r) => (
                <tr key={`${r.lane}|${r.isp}`}>
                  <td style={{ ...tdStyle, paddingLeft: 22 }}>{r.lane} · {r.isp}</td>
                  <td style={numTd}>{fmt(r.messages)}</td><td style={numTd}>{fmt(r.delivered)}</td><td style={numTd}>{pctS(r.accepted_pct)}</td>
                  <td style={numTd}>{pctS(r.first_try_pct)}</td>
                  <td style={{ ...numTd, color: (r.deferred_pct ?? 0) > 10 ? colors.warningText : colors.text }}>{pctS(r.deferred_pct)}</td>
                  <td style={numTd}>{fmt(r.blocked)}</td>
                  <td style={{ ...numTd, color: (r.blocked_pct ?? 0) >= 0.5 ? colors.dangerText : colors.text }}>{pctS(r.blocked_pct)}</td>
                  <td style={numTd}>{fmt(r.list_hard)}</td><td style={numTd}>{fmt(r.pending)}</td>
                  <td style={tdStyle}><Pill color={vcolor(r.verdict)} style={{ fontSize: 10 }}>{r.verdict}</Pill></td>
                </tr>
              ))}
            </React.Fragment>
          );
        })}
      </tbody>
    </table>
  </div>
);

const CellsTable: React.FC<{ rows: CellRow[]; empty: string }> = ({ rows, empty }) => (
  rows.length === 0 ? <EmptyState title={empty} /> : (
    <table style={tableStyle}>
      <thead><tr><th style={thStyle}>Slot</th><th style={numTh}>Cells</th><th style={numTh}>Planned</th><th style={numTh}>Sent</th><th style={numTh}>Sent %</th><th style={thStyle}>Status</th></tr></thead>
      <tbody>
        {(['family', 'non_family'] as Group[]).map((g) => {
          const rs = rows.filter((r) => r.group === g);
          if (!rs.length) return null;
          const t = sumCells(rs);
          return (
            <React.Fragment key={g}>
              <tr style={{ background: 'color-mix(in srgb, var(--j-violet-strong) 8%, transparent)' }}>
                <td style={{ ...tdStyle, fontWeight: 700 }}>{GROUP_LABEL[g]}</td><td style={{ ...numTd, fontWeight: 700 }}>{fmt(t.cells)}</td>
                <td style={{ ...numTd, fontWeight: 700 }}>{fmt(t.planned)}</td><td style={{ ...numTd, fontWeight: 700 }}>{fmt(t.sent)}</td>
                <td style={numTd}>{pctS(pct(t.sent, t.planned))}</td><td style={tdStyle} />
              </tr>
              {rs.map((r) => (
                <tr key={r.slot}>
                  <td style={{ ...tdStyle, paddingLeft: 22 }}>{r.slot}</td><td style={numTd}>{fmt(r.cells)}</td><td style={numTd}>{fmt(r.planned)}</td>
                  <td style={numTd}>{fmt(r.sent)}</td><td style={numTd}>{pctS(pct(r.sent, r.planned))}</td>
                  <td style={{ ...tdStyle, fontSize: 12, color: colors.textMuted }}>{Object.entries(r.status).map(([k, v]) => `${v} ${k}`).join(' · ')}</td>
                </tr>
              ))}
            </React.Fragment>
          );
        })}
      </tbody>
    </table>
  )
);

export const PmtaDailyRead: React.FC<{ read: PmtaRead | null | undefined; server: 'A' | 'B'; date: string }> = ({ read, server, date }) => {
  const [day, setDay] = useState<'today' | 'yesterday'>('today');
  const D = read ? (day === 'today' ? read.today : read.yesterday) : null;
  const lanes = useMemo(() => (D?.lanes ?? []).filter((r) => r.server === server), [D, server]);
  const tokens = useMemo(() => (D?.tokens ?? []).filter((t) => t.server === server).slice(0, 15), [D, server]);
  const ips = useMemo(() => (D?.flagged_ips ?? []).filter((i) => i.server === server), [D, server]);
  const cToday = useMemo(() => (read?.cells.today ?? []).filter((c) => c.server === server), [read, server]);
  const cTomorrow = useMemo(() => (read?.cells.tomorrow ?? []).filter((c) => c.server === server), [read, server]);

  if (!read) {
    return (
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title={`PMTA daily read · Server ${server}`} icon={faServer} />
        <EmptyState title={`No PMTA read for ${date}`} hint="agents/reporting/pmta_daily_read runs hourly off-box; a date before it went live has none." />
      </Panel>
    );
  }

  const all = sumLanes(lanes);
  const fam = sumLanes(lanes.filter((r) => r.group === 'family'));
  const non = sumLanes(lanes.filter((r) => r.group === 'non_family'));
  const ct = sumCells(cToday);
  const cm = sumCells(cTomorrow);
  const both = sumCells(read.cells.today);
  const settled = D?.settled?.[server];
  const L = read.levels;

  return (
    <>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title={`PMTA daily read · Server ${server}`} icon={faServer}
          right={(
            <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
              <span style={{ fontSize: 11, color: colors.textMuted }} title={read.generated_at}>read {mt(read.generated_at)} MT</span>
              <button style={{ ...btnStyle, opacity: day === 'today' ? 1 : 0.6 }} onClick={() => setDay('today')}>{read.today.date}</button>
              <button style={{ ...btnStyle, opacity: day === 'yesterday' ? 1 : 0.6 }} onClick={() => setDay('yesterday')} disabled={!read.yesterday}>{read.yesterday?.date ?? 'yesterday'}</button>
            </div>
          )} />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 14 }}>
          <Stat label={`Planned ${read.date} · ${server}`} value={fmt(ct.planned)} sub={`${fmt(ct.cells)} cells · sent ${fmt(ct.sent)} (${pctS(pct(ct.sent, ct.planned))}) · A+B ${fmt(both.planned)}`} />
          <Stat label={`Delivered ${D?.date ?? ''}`} value={fmt(all.delivered)} sub={`${pctS(all.acc)} accepted of ${fmt(all.terminal)} terminal`}
            color={all.acc != null && all.acc < 90 ? colors.danger : undefined} />
          <Stat label="Yahoo family" value={pctS(fam.acc)} sub={`${fmt(fam.delivered)} delivered · deferred ${pctS(fam.dfr)} · ${fmt(fam.blocked)} blocked`}
            color={(fam.dfr ?? 0) > 10 ? colors.warning : undefined} />
          <Stat label="Non-yahoo family" value={pctS(non.acc)} sub={`${fmt(non.delivered)} delivered · deferred ${pctS(non.dfr)} · ${fmt(non.blocked)} blocked`}
            color={(non.blk ?? 0) >= 0.5 ? colors.warning : undefined} />
          <Stat label="Lake settled" value={settled ? 'yes' : 'no'} sub={settled ? 'final for the day' : 'PMTA lake lags 13–25 h — numbers still growing'}
            color={settled ? colors.success : colors.warning} />
          <Stat label={`Built for ${(() => { const d = new Date(`${read.date}T12:00:00Z`); d.setUTCDate(d.getUTCDate() + 1); return d.toISOString().slice(0, 10); })()}`}
            value={fmt(cm.planned)} sub={cm.cells ? `${fmt(cm.cells)} cells on ${server}` : 'not built yet (engaged 11:30 · cold 12:15 · mk 17:30 MT)'}
            color={cm.cells ? undefined : colors.warning} />
        </div>
      </Panel>

      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title={`Delivery · Server ${server} · ${D?.date ?? ''}`}
          right={<span style={{ fontSize: 11, color: colors.textMuted }}>lake, one row per message · Yahoo family = {read.family_isps.join(', ')}</span>} />
        {lanes.length === 0 ? <EmptyState title="No delivery rows yet" hint="Nothing from this server has reached the lake for this day." /> : <DeliveryTable rows={lanes} />}
      </Panel>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(420px, 1fr))', gap: 14, marginBottom: 14 }}>
        <Panel>
          <SectionHeader title={`Cells · ${read.date}`} right={<span style={{ fontSize: 11, color: colors.textMuted }}>planned = recipients at finalize</span>} />
          <CellsTable rows={cToday} empty="No cells on this server" />
        </Panel>
        <Panel>
          <SectionHeader title="Cells · tomorrow" />
          <CellsTable rows={cTomorrow} empty="Nothing built yet" />
        </Panel>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(420px, 1fr))', gap: 14, marginBottom: 14 }}>
        <Panel>
          <SectionHeader title="SMTP responses" right={<span style={{ fontSize: 11, color: colors.textMuted }}>top {tokens.length} · messages = campaign × address</span>} />
          {tokens.length === 0 ? <EmptyState title="No non-delivery responses" /> : (
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>Token</th><th style={thStyle}>Class</th><th style={thStyle}>Lane · ISP</th><th style={numTh}>Msgs</th><th style={numTh}>Events</th></tr></thead>
              <tbody>{tokens.map((t, i) => (
                <tr key={i}><td style={{ ...tdStyle, fontFamily: 'ui-monospace, Menlo, monospace', fontSize: 11, maxWidth: 220, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={t.tok}>{t.tok}</td>
                  <td style={tdStyle}><Pill color={['ip_block', 'domain_block', 'content_block'].includes(t.cls) ? colors.danger : t.cls === 'capacity' ? colors.warning : colors.idle} style={{ fontSize: 10 }}>{t.cls}</Pill></td>
                  <td style={tdStyle}>{t.lane} · {t.isp}</td><td style={numTd}>{fmt(t.messages)}</td><td style={numTd}>{fmt(t.events)}</td></tr>
              ))}</tbody>
            </table>
          )}
        </Panel>
        <Panel>
          <SectionHeader title="IPs flagged" right={<span style={{ fontSize: 11, color: colors.textMuted }}>≥25 messages with TSS04 / HCM2 / HM08 / blocklist tokens</span>} />
          {ips.length === 0 ? <EmptyState title="No flagged IPs" /> : (
            <table style={tableStyle}>
              <thead><tr><th style={thStyle}>IP</th><th style={thStyle}>Lane · ISP</th><th style={numTh}>Flagged</th><th style={numTh}>Delivered</th></tr></thead>
              <tbody>{ips.map((i) => <tr key={`${i.ip}|${i.isp}|${i.lane}`}><td style={{ ...tdStyle, fontFamily: 'ui-monospace, Menlo, monospace', fontSize: 12 }}>{i.ip}</td><td style={tdStyle}>{i.lane} · {i.isp}</td><td style={{ ...numTd, color: colors.warningText }}>{fmt(i.flagged)}</td><td style={numTd}>{fmt(i.delivered)}</td></tr>)}</tbody>
            </table>
          )}
        </Panel>
      </div>

      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Standing levels · A + B" right={<span style={{ fontSize: 11, color: colors.textMuted }}>nx_cold_ramp.json (S3) · pmta_group_a.json</span>} />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 14 }}>
          <Stat label="Non-family cold level" value={L.cold_freeze ?? 'stepping'} sub={L.cold_freeze ? `held at that date's level${L.cold_freeze_resume ? ` · resumes ${L.cold_freeze_resume}` : ' · no resume set'} · max day-over-day ${L.max_dod != null ? `${Math.round(L.max_dod * 100)}%` : '—'}` : 'freeze off — ramps daily'} />
          <Stat label="nx Yahoo family hold" value={fmt(L.nx_family_hold_total)} sub={`${L.nx_family_lanes} lanes · ${Object.entries(L.nx_family_hold_by_isp).sort((a, b) => b[1] - a[1]).map(([k, v]) => `${k} ${fmt(v)}`).join(' · ')}`} />
          <Stat label="mk lanes hold" value={fmt(L.mk_hold_total)} sub={Object.entries(L.mk_hold).map(([k, v]) => `${k} ${fmt(v)}`).join(' · ')} />
        </div>
      </Panel>
    </>
  );
};

export default PmtaDailyRead;
