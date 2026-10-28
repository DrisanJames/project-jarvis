// WarmupReadable.tsx — the human-readable layer of the warmup agent screen (operator 2026-10-27: "Make everything in an
// understandable format … Microsoft 940 1D1B, I have no understanding as to what it means"). Three rules:
//   1. Names, never codes: a domain prefix (dbz) shows as its domain (dbzt100.com); an ISP reply code (TSS09, RL000010)
//      carries its meaning on hover and in a "what the codes mean" key; units are written out ("per IP per hour").
//   2. One number per labelled column: delivered / deferred / bounced are separate columns, never "940 · 1d · 1b".
//   3. The agent's daily report is laid out as sections (headline first, per-domain tables merged into one table you
//      filter by mailbox provider) instead of a wall of markdown. Nothing is re-computed: every number is the agent's.
// Code meanings: the ISPs' own reply wording as quoted in the agent's journal (S844 "4.7.651 temporarily rate limited due
// to IP reputation", S3150 "part of their network is on our block list", TSS09 5.7.2 "permanently deferred") and the
// agent's field manual (agents/warmup/knowledge/field_manual.md). A code with neither stays unexplained, never guessed.

import React, { useMemo, useState } from 'react';
import { colors, alpha, tdStyle, thStyle, numTd, numTh, tableStyle } from '../shared/theme';
import { Panel, SectionHeader, EmptyState, Pill } from '../shared/ui';
import { SubNav } from '../shared/SubNav';

// ───────────────────────────── names

// colo2's eight sending domains (agents/warmup/instances/colo2.json: prefix → apex, IPs per domain).
export const WARMUP_DOMAINS: Record<string, { domain: string; ips: number }> = {
  ufo: { domain: 'ufotv.org', ips: 15 }, mdz: { domain: 'mdeitz.com', ips: 15 }, pnd: { domain: 'poundfinance.com', ips: 15 },
  hty: { domain: 'holytoledo.net', ips: 15 }, dbz: { domain: 'dbzt100.com', ips: 15 }, tti: { domain: 'ttinc.net', ips: 15 },
  wcl: { domain: 'wcl-online.com', ips: 17 }, gmc: { domain: 'getmecoupons.net', ips: 17 },
};
export const domainName = (prefix: string) => WARMUP_DOMAINS[prefix]?.domain ?? prefix;

export const ISP_ORDER = ['microsoft', 'apple', 'comcast', 'charter', 'other', 'yahoo_family'] as const;
export const ISP_LABEL: Record<string, string> = {
  microsoft: 'Microsoft', apple: 'Apple', comcast: 'Comcast', charter: 'Charter', other: 'Other ISPs', yahoo_family: 'Yahoo family',
};
export const ispName = (g: string) => ISP_LABEL[g] ?? g;

// ───────────────────────────── reply codes

export const REPLY_CODES: Record<string, string> = {
  '250': 'Accepted: the ISP took the message (delivered).',
  TSS04: 'Yahoo: this IP hit Yahoo’s volume limit right now. Temporary; ease or hold the speed.',
  TSS09: 'Yahoo: “permanently deferred” (5.7.2). An IP-wide block: Yahoo refuses mail from these IPs.',
  TS01: 'Yahoo: temporary deferral, retried later.', TS02: 'Yahoo: temporary deferral, retried later.',
  TS03: 'Yahoo: permanent block on this IP.',
  '4.7.650': 'Microsoft: sending too fast from this IP. Temporary.', S775: 'Microsoft: sending too fast from this IP. Temporary.',
  S844: 'Microsoft: “temporarily rate limited due to IP reputation” (4.7.651). Slow down on this IP.',
  '4.7.651': 'Microsoft: “temporarily rate limited due to IP reputation” (S844). Slow down on this IP.',
  AS762: 'Microsoft: “Server busy” (4.7.500). Microsoft-side, retried later.', '4.7.500': 'Microsoft: “Server busy” (AS762). Microsoft-side, retried later.',
  S3150: 'Microsoft: “part of their network is on our block list” (5.7.1). Microsoft is judging the whole IP range.',
  S3115: 'Microsoft: too many connections at once. Fix the connections, not the speed.',
  CONNECT_TIMEOUT: 'The connection to the ISP timed out. Retried; not a speed signal.',
  S3140: 'Microsoft: IP is on Microsoft’s block list. Needs a delist request.', '5.7.606': 'Microsoft: IP is on Microsoft’s block list. Needs a delist request.',
  HCM1: 'Apple: early-warning deferral, retried later.', HCM2: 'Apple: IP rejected for reputation.',
  HM08: 'Apple: content rejected (creative × audience). Not a volume signal.',
  CS01: 'Apple: connection limit.', CS02: 'Apple: connection limit.', CS03: 'Apple: connection limit.',
  RL000001: 'Comcast: rate limited ("Throttled"). Hold or ease.', RL000003: 'Comcast: rate limited ("Throttled"). Hold or ease.',
  RL000010: 'Comcast: rate limited ("Throttled"). Hold or ease.', BL000010: 'Comcast: IP is on a block list Comcast uses.',
  'AUP#In': 'Charter: refuses the connection (554) on the IP range’s reputation.', AUP: 'Charter: policy refusal.', CLOUDMARK_CSI: 'Charter: IP listed by Cloudmark CSI (reputation).', 'Cloudmark CSI': 'Charter: IP listed by Cloudmark CSI (reputation).',
  USER_UNKNOWN: 'The mailbox does not exist: a dead record, not a sending problem.',
  MAILBOX_DISABLED: 'The mailbox is disabled: a dead record, not a sending problem.',
  SENDER_REJECTED: 'The receiver refuses our sender address. Check the sending domain’s DNS.',
  '4.7.1': 'Generic temporary refusal, retried later.',
};
const CODE_RE = new RegExp(`(${Object.keys(REPLY_CODES).sort((a, b) => b.length - a.length).map((k) => k.replace(/\./g, '\\.')).join('|')})`, 'g');

// The agent writes in shorthand ("S844 at 120: 20m 146/300 (49%) … transfail … per 23:15Z"); spell it out.
function utcToMt(hh: string, mm: string): string {
  const d = new Date(); d.setUTCHours(Number(hh), Number(mm), 0, 0);
  return `${new Intl.DateTimeFormat('en-US', { timeZone: 'America/Denver', hour: '2-digit', minute: '2-digit', hour12: false }).format(d)} MT`;
}
const PREFIX_RE = new RegExp(`(?<![\\w-])(${Object.keys(WARMUP_DOMAINS).join('|')})(?![\\w-])`, 'g');
export function humanize(t: string): string {
  return t
    .replace(/\b(\d{1,2}):(\d{2})Z\b/g, (_, h, m) => utcToMt(h, m))
    .replace(/~(\d+)m\b/g, '~$1 min')
    .replace(/\b(\d+)m delta\b/g, 'in the last $1 min')
    .replace(/\b(\d+)m\b(?![\w%])/g, 'in the last $1 min')
    .replace(/\b(\d[\d,]*)\/(\d[\d,]*) \((\d+(?:\.\d+)?)%\)/g, '$1 of $2 ($3%)')
    .replace(/\btransfails?\b/g, 'deferred')
    .replace(/\bMS\b/g, 'Microsoft')
    .replace(/\/IP\/h\b/g, ' per IP per hour')
    .replace(/\s*->\s*/g, ' → ')
    .replace(/\s*\(tick\)/g, '')
    .replace(/\bcap (\d[\d,]*)/g, 'daily target $1')
    .replace(PREFIX_RE, (m) => domainName(m));
}

const codeStyle: React.CSSProperties = { borderBottom: `1px dotted ${colors.indigo300}`, cursor: 'help', color: colors.indigo200 };

/** Plain text with every known reply code wrapped in a hover explanation and **bold** kept. */
export const Explain: React.FC<{ text: string }> = ({ text }) => {
  const out: React.ReactNode[] = [];
  let k = 0;
  for (const [bi, chunk] of text.split(/(\*\*[^*]+\*\*)/g).entries()) {
    const bold = chunk.startsWith('**') && chunk.endsWith('**') && chunk.length > 4;
    const body = humanize(bold ? chunk.slice(2, -2) : chunk.replace(/`([^`]+)`/g, '$1'));
    const parts = body.split(CODE_RE).map((p, j) => (REPLY_CODES[p] && j % 2 === 1
      ? <span key={`${bi}-${j}`} style={codeStyle} title={REPLY_CODES[p]}>{p}</span>
      : p));
    out.push(bold ? <strong key={k++} style={{ color: colors.heading }}>{parts}</strong> : <React.Fragment key={k++}>{parts}</React.Fragment>);
  }
  return <>{out}</>;
};

export function codesIn(text: string): string[] {
  const seen = new Set<string>();
  for (const m of text.matchAll(CODE_RE)) if (m[1] !== '250') seen.add(m[1]);
  return [...seen];
}

export const CodeKey: React.FC<{ codes: string[] }> = ({ codes }) => codes.length === 0 ? null : (
  <Panel style={{ marginTop: 14 }}>
    <SectionHeader title="What the reply codes mean" right={<span style={{ fontSize: 11, color: colors.textMuted }}>hover any underlined code for the same note</span>} />
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(300px, 1fr))', gap: '6px 18px' }}>
      {codes.map((c) => (
        <div key={c} style={{ fontSize: 12, color: colors.text, lineHeight: 1.45 }}>
          <span style={{ fontWeight: 700, color: colors.indigo200, marginRight: 6 }}>{c}</span>{REPLY_CODES[c]}
        </div>
      ))}
    </div>
  </Panel>
);

// ───────────────────────────── formatting

const fmt = (n: number) => n.toLocaleString('en-US');
// A share bar where more is better (the shared ProgressBar colours a full bar as a warning).
export const ShareBar: React.FC<{ pct: number; color?: string }> = ({ pct, color = colors.success }) => (
  <div style={{ height: 6, borderRadius: 3, background: alpha(colors.indigo500, '22'), overflow: 'hidden' }}>
    <div style={{ width: `${Math.max(0, Math.min(1, pct)) * 100}%`, height: '100%', background: color }} />
  </div>
);
const pctOf = (num: number, den: number, d = 1) => (den > 0 ? `${((100 * num) / den).toFixed(d)}%` : '—');
const hourLabel = (h: string) => { const n = Number(h); return `${String(n).padStart(2, '0')}:00–${String((n + 1) % 24).padStart(2, '0')}:00`; };
function denverClock(iso?: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const day = new Intl.DateTimeFormat('en-US', { timeZone: 'America/Denver', month: 'short', day: 'numeric' }).format(d);
  const t = new Intl.DateTimeFormat('en-US', { timeZone: 'America/Denver', hour: '2-digit', minute: '2-digit', hour12: false }).format(d);
  return `${day}, ${t} MT`;
}

const chipRow = (items: { key: string; label: string }[], active: string, onChange: (k: string) => void, label: string) => (
  <div style={{ marginBottom: 12 }}><SubNav items={items} active={active} onChange={onChange} ariaLabel={label} /></div>
);

// ───────────────────────────── Delivery

export type Totals = { delivered: number; transfail: number; fail: number };

export const DeliveryView: React.FC<{ byGroup: Record<string, Totals>; byHour: Record<string, Record<string, Totals>> }> = ({ byGroup, byHour }) => {
  const groups = ISP_ORDER.filter((g) => byGroup[g]);
  const [isp, setIsp] = useState<string>('all');
  const hours = Object.keys(byHour).sort();
  if (hours.length === 0) return <Panel><EmptyState title="No deliveries recorded for this date" hint="The box's log has no rows yet." /></Panel>;
  const pick = (row: Record<string, Totals>): Totals => (isp === 'all'
    ? Object.values(row).reduce((a, c) => ({ delivered: a.delivered + c.delivered, transfail: a.transfail + c.transfail, fail: a.fail + c.fail }), { delivered: 0, transfail: 0, fail: 0 })
    : row[isp] ?? { delivered: 0, transfail: 0, fail: 0 });
  const legend = (
    <div style={{ fontSize: 12, color: colors.textMuted, lineHeight: 1.5, marginTop: 8 }}>
      <strong style={{ color: colors.text }}>Delivered</strong>: the ISP accepted the message. <strong style={{ color: colors.warningText }}>Deferred</strong>: the ISP asked us to try again later; the box retries on its own, so this is the ISP's signal, not lost mail. <strong style={{ color: colors.dangerText }}>Bounced</strong>: refused for good (dead mailbox or a block). Percentages are shares of every message the box attempted.
    </div>
  );
  return (
    <>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Today by mailbox provider" right={<span style={{ fontSize: 11, color: colors.textMuted }}>source: the box's own delivery log, read every 15 minutes</span>} />
        <table style={tableStyle}>
          <thead><tr><th style={thStyle}>Mailbox provider</th><th style={numTh}>Delivered</th><th style={numTh}>Deferred (retrying)</th><th style={numTh}>Bounced</th><th style={numTh}>Delivered %</th><th style={numTh}>Deferred %</th><th style={numTh}>Bounced %</th></tr></thead>
          <tbody>{groups.map((g) => {
            const c = byGroup[g]; const n = c.delivered + c.transfail + c.fail;
            return (
              <tr key={g} onClick={() => setIsp(g)} style={{ cursor: 'pointer', background: isp === g ? alpha(colors.indigo500, '14') : undefined }}>
                <td style={{ ...tdStyle, fontWeight: 600 }}>{ispName(g)}</td>
                <td style={numTd}>{fmt(c.delivered)}</td>
                <td style={{ ...numTd, color: c.transfail ? colors.warningText : colors.textFaint }}>{fmt(c.transfail)}</td>
                <td style={{ ...numTd, color: c.fail ? colors.dangerText : colors.textFaint }}>{fmt(c.fail)}</td>
                <td style={numTd}>{pctOf(c.delivered, n)}</td>
                <td style={{ ...numTd, color: n && c.transfail / n > 0.1 ? colors.warningText : colors.text }}>{pctOf(c.transfail, n)}</td>
                <td style={{ ...numTd, color: n && c.fail / n > 0.05 ? colors.dangerText : colors.text }}>{pctOf(c.fail, n)}</td>
              </tr>
            );
          })}</tbody>
        </table>
        {legend}
      </Panel>
      <Panel>
        <SectionHeader title={`Hour by hour · ${isp === 'all' ? 'all mailbox providers' : ispName(isp)}`} right={<span style={{ fontSize: 11, color: colors.textMuted }}>Denver time · click a provider above or pick one here</span>} />
        {chipRow([{ key: 'all', label: 'All providers' }, ...groups.map((g) => ({ key: g, label: ispName(g) }))], isp, setIsp, 'Mailbox provider')}
        <div style={{ overflowX: 'auto' }}>
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Hour (MT)</th><th style={numTh}>Delivered</th><th style={numTh}>Deferred (retrying)</th><th style={numTh}>Bounced</th><th style={numTh}>Delivered %</th><th style={{ ...thStyle, width: 180 }}>Delivered share</th></tr></thead>
            <tbody>{hours.map((h) => {
              const c = pick(byHour[h]); const n = c.delivered + c.transfail + c.fail;
              if (n === 0) return <tr key={h}><td style={tdStyle}>{hourLabel(h)}</td><td colSpan={5} style={{ ...tdStyle, color: colors.textFaint }}>nothing sent to {ispName(isp)} this hour</td></tr>;
              return (
                <tr key={h}>
                  <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{hourLabel(h)}</td>
                  <td style={numTd}>{fmt(c.delivered)}</td>
                  <td style={{ ...numTd, color: c.transfail ? colors.warningText : colors.textFaint }}>{fmt(c.transfail)}</td>
                  <td style={{ ...numTd, color: c.fail ? colors.dangerText : colors.textFaint }}>{fmt(c.fail)}</td>
                  <td style={numTd}>{pctOf(c.delivered, n)}</td>
                  <td style={tdStyle}><ShareBar pct={n ? c.delivered / n : 0} color={n && c.delivered / n < 0.9 ? colors.warning : colors.success} /></td>
                </tr>
              );
            })}</tbody>
          </table>
        </div>
      </Panel>
    </>
  );
};

// ───────────────────────────── Speed & targets

type Rate = { per_ip_hour: number; previous?: number | null; reason?: string; set_at?: string };
type JitSlot = { prefix: string; groups: Record<string, number> };
type Tick = { at: string; delta?: { by: Record<string, Record<string, Totals>> } | null };
type VolumeChange = { at: string; kind: string; group?: string; changed?: { prefix: string; from?: number | null; to: number }[]; reason?: string };

const agoText = (ms: number) => { const m = Math.round(ms / 60000); return m < 1 ? 'just now' : m < 60 ? `${m} min ago` : `${Math.floor(m / 60)} h ${m % 60} min ago`; };

export const SpeedTargetsView: React.FC<{
  date: string; rates: Record<string, Record<string, Rate>> | null; plan: Record<string, Record<string, number>> | undefined;
  jit: Record<string, JitSlot> | null; ticks: Tick[]; journal: VolumeChange[];
}> = ({ date, rates, plan, jit, ticks, journal }) => {
  const prefixes = useMemo(() => [...new Set([...Object.keys(rates ?? {}), ...Object.keys(plan ?? {})])].sort((a, b) => domainName(a).localeCompare(domainName(b))), [rates, plan]);
  const groups = ISP_ORDER.filter((g) => prefixes.some((p) => rates?.[p]?.[g] || plan?.[p]?.[g] != null));
  const [isp, setIsp] = useState<string>(groups[0] ?? 'microsoft');
  const [open, setOpen] = useState<Record<string, boolean>>({});

  // "Now" = the box's latest reading (so a past date is judged at its own last hour, not the wall clock).
  const nowMs = useMemo(() => Math.max(0, ...ticks.map((t) => new Date(t.at).getTime()).filter((x) => !Number.isNaN(x))), [ticks]);
  const hourAgo = nowMs - 60 * 60 * 1000;
  const actual = useMemo(() => {
    const day: Record<string, Record<string, Totals>> = {}; const hour: Record<string, Record<string, Totals>> = {};
    for (const t of ticks) {
      const inHour = new Date(t.at).getTime() > hourAgo;
      for (const [p, gs] of Object.entries(t.delta?.by ?? {})) for (const [g, c] of Object.entries(gs)) {
        for (const tgt of inHour ? [day, hour] : [day]) {
          const x = ((tgt[p] ||= {})[g] ||= { delivered: 0, transfail: 0, fail: 0 });
          x.delivered += c.delivered || 0; x.transfail += c.transfail || 0; x.fail += c.fail || 0;
        }
      }
    }
    return { day, hour };
  }, [ticks, hourAgo]);
  const queued = useMemo(() => {
    const out: Record<string, Record<string, number>> = {};
    for (const s of Object.values(jit ?? {})) for (const [g, n] of Object.entries(s.groups)) ((out[s.prefix] ||= {})[g] = (out[s.prefix]?.[g] ?? 0) + n);
    return out;
  }, [jit]);
  const targetMoves = useMemo(() => {
    const out: Record<string, { at: string; from?: number | null; to: number }> = {};
    for (const r of journal) if (r.kind === 'set_daily_volume' && r.group && new Date(r.at).getTime() > hourAgo)
      for (const c of r.changed ?? []) out[`${c.prefix}|${r.group}`] = { at: r.at, from: c.from, to: c.to };
    return out;
  }, [journal, hourAgo]);

  if (prefixes.length === 0) return <Panel><EmptyState title="The agent has not set any speeds or targets yet" /></Panel>;
  const z: Totals = { delivered: 0, transfail: 0, fail: 0 };
  const sum = (f: (p: string) => number) => prefixes.reduce((a, p) => a + f(p), 0);
  const totTarget = sum((p) => plan?.[p]?.[isp] ?? 0);
  const totDelivered = sum((p) => actual.day[p]?.[isp]?.delivered ?? 0);
  const totHour = sum((p) => actual.hour[p]?.[isp]?.delivered ?? 0);
  const totPace = sum((p) => (rates?.[p]?.[isp]?.per_ip_hour ?? 0) * (WARMUP_DOMAINS[p]?.ips ?? 0));
  const changedNow = prefixes.filter((p) => { const r = rates?.[p]?.[isp]; return (r?.set_at && new Date(r.set_at).getTime() > hourAgo) || targetMoves[`${p}|${isp}`]; }).length;
  const reasons = prefixes.map((p) => rates?.[p]?.[isp]?.reason ?? '').join(' ');
  const asOf = nowMs ? denverClock(new Date(nowMs).toISOString()) : '';

  const tile = (label: string, value: React.ReactNode, sub: string, tone?: string) => (
    <div style={{ border: `1px solid ${colors.panelBorder}`, borderRadius: 8, padding: '10px 14px', background: alpha(colors.indigo500, '0d') }}>
      <div style={{ fontSize: 11, color: colors.textMuted, letterSpacing: 0.4 }}>{label}</div>
      <div style={{ fontSize: 22, fontWeight: 700, color: tone ?? colors.heading, fontVariantNumeric: 'tabular-nums' }}>{value}</div>
      <div style={{ fontSize: 11, color: colors.textMuted }}>{sub}</div>
    </div>
  );

  return (
    <>
      <Panel>
        <SectionHeader title={`Speed & targets · ${ispName(isp)} · ${date}`} right={<span style={{ fontSize: 11, color: colors.textMuted }}>{asOf ? `box read ${asOf} · "last hour" = the 60 min before it` : ''}</span>} />
        {chipRow(groups.map((g) => ({ key: g, label: ispName(g) })), isp, setIsp, 'Mailbox provider')}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 10, marginBottom: 14 }}>
          {tile('Delivered vs target today', totTarget ? pctOf(totDelivered, totTarget, 0) : '—', `${fmt(totDelivered)} delivered of ${fmt(totTarget)} target`)}
          {tile('Delivered in the last hour', fmt(totHour), `allowed pace ${fmt(totPace)} / hour across ${prefixes.length} domains`)}
          {tile('Changed in the last hour', `${changedNow} of ${prefixes.length}`, changedNow ? 'domains with a new speed or target' : 'no speed or target changes', changedNow ? colors.warningText : colors.successText)}
        </div>
        <div style={{ overflowX: 'auto' }}>
          <table style={tableStyle}>
            <thead><tr>
              <th style={thStyle}>Sending domain</th>
              <th style={numTh}>Target today</th><th style={numTh}>Delivered (actual)</th><th style={{ ...thStyle, width: 170 }}>Actual vs target</th>
              <th style={numTh} title="handed to the box, waiting or retrying">Queued</th>
              <th style={numTh}>Speed now</th><th style={numTh} title="delivered in the 60 min before the box read vs speed × IPs">Last hour: actual / allowed</th>
              <th style={thStyle}>Change in the last hour</th>
            </tr></thead>
            <tbody>{prefixes.map((p) => {
              const r = rates?.[p]?.[isp]; const target = plan?.[p]?.[isp]; const ips = WARMUP_DOMAINS[p]?.ips ?? 0;
              const dayT = actual.day[p]?.[isp] ?? z; const hourT = actual.hour[p]?.[isp] ?? z;
              const q = Math.max(0, (queued[p]?.[isp] ?? 0) - dayT.delivered - dayT.fail);
              const pace = r ? r.per_ip_hour * ips : 0;
              const speedMoved = r?.set_at && new Date(r.set_at).getTime() > hourAgo && r.previous != null && r.previous !== r.per_ip_hour;
              const tm = targetMoves[`${p}|${isp}`];
              const moves: React.ReactNode[] = [];
              if (speedMoved && r) moves.push(<span key="s" style={{ color: r.per_ip_hour > (r.previous ?? 0) ? colors.successText : colors.warningText }}>speed {r.per_ip_hour > (r.previous ?? 0) ? 'raised' : 'eased'} {r.previous} → {r.per_ip_hour} · {agoText(nowMs - new Date(r.set_at!).getTime())}</span>);
              if (tm) moves.push(<span key="t" style={{ color: colors.indigo200 }}>target {tm.from == null ? 'set' : tm.to > tm.from ? 'raised' : 'lowered'} {tm.from != null ? `${fmt(tm.from)} → ` : ''}{fmt(tm.to)} · {agoText(nowMs - new Date(tm.at).getTime())}</span>);
              const key = `${p}|${isp}`;
              return (
                <React.Fragment key={p}>
                  <tr>
                    <td style={{ ...tdStyle, fontWeight: 600, whiteSpace: 'nowrap', borderBottom: 'none' }} title={`prefix ${p} · ${ips} IPs`}>{domainName(p)}</td>
                    <td style={{ ...numTd, borderBottom: 'none' }}>{target == null ? <span style={{ color: colors.textFaint }}>none</span> : target === 0 ? <span style={{ color: colors.textFaint }}>not mailed</span> : fmt(target)}</td>
                    <td style={{ ...numTd, borderBottom: 'none' }}>{fmt(dayT.delivered)}</td>
                    <td style={{ ...tdStyle, borderBottom: 'none' }}>{target ? <div><div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 3 }}>{pctOf(dayT.delivered, target, 0)} of target</div><ShareBar pct={dayT.delivered / target} color={colors.indigo400} /></div> : '—'}</td>
                    <td style={{ ...numTd, color: q ? colors.text : colors.textFaint, borderBottom: 'none' }}>{fmt(q)}</td>
                    <td style={{ ...numTd, whiteSpace: 'nowrap', borderBottom: 'none' }}>{r ? <><strong>{r.per_ip_hour}</strong> <span style={{ fontSize: 11, color: colors.textMuted }}>per IP per hour</span></> : <span style={{ color: colors.textFaint }}>not set</span>}</td>
                    <td style={{ ...numTd, whiteSpace: 'nowrap', borderBottom: 'none' }}>{r ? <>{fmt(hourT.delivered)} <span style={{ color: colors.textMuted }}>/ {fmt(pace)}</span> <span style={{ fontSize: 11, color: colors.textMuted }}>({pctOf(hourT.delivered, pace, 0)})</span></> : '—'}</td>
                    <td style={{ ...tdStyle, fontSize: 12, borderBottom: 'none' }}>{moves.length ? <div style={{ display: 'grid', gap: 2 }}>{moves}</div> : <span style={{ color: colors.textFaint }}>no change{r?.set_at ? ` · speed last set ${denverClock(r.set_at)}` : ''}</span>}</td>
                  </tr>
                  <tr>
                    <td colSpan={8} style={{ ...tdStyle, paddingTop: 0, fontSize: 12, color: colors.textMuted, cursor: r?.reason ? 'pointer' : undefined }} onClick={() => setOpen((o) => ({ ...o, [key]: !o[key] }))}>
                      {r?.reason ? <div style={open[key] ? { whiteSpace: 'pre-wrap' } : { whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', maxWidth: 1200 }}><span style={{ color: colors.textFaint }}>Why: </span><Explain text={r.reason} /></div> : null}
                    </td>
                  </tr>
                </React.Fragment>
              );
            })}</tbody>
          </table>
        </div>
        <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 8, lineHeight: 1.5 }}>
          <strong style={{ color: colors.text }}>Target today</strong> is the most the agent will send to {ispName(isp)} from that domain today. <strong style={{ color: colors.text }}>Delivered (actual)</strong> is what {ispName(isp)} accepted, from the box's own log. <strong style={{ color: colors.text }}>Queued</strong> is handed to the box but not yet delivered or bounced. <strong style={{ color: colors.text }}>Last hour: actual / allowed</strong> compares the last hour's deliveries with the speed × the domain's IPs. Click a "Why" line to read the agent's reason in full.
        </div>
      </Panel>
      <CodeKey codes={codesIn(reasons)} />
    </>
  );
};

// ───────────────────────────── Daily report

type Block =
  | { t: 'p'; text: string } | { t: 'ul'; items: string[] } | { t: 'table'; hdr: string[]; rows: string[][] }
  | { t: 'merged'; isp: number; hdr: string[]; rows: string[][] } | { t: 'h3'; text: string };
type Section = { title: string; blocks: Block[] };

const HEADER_WORDS: Record<string, string> = {
  Deliv: 'Delivered', 'Def%': 'Deferred %', 'Bnc%': 'Bounced %', 'Rate/IP/h': 'Speed (per IP per hour)', Rate: 'Speed (per IP per hour)',
  Cap: 'Daily target', 'Daily cap (box)': 'Daily target (all domains)', Codes: 'Reply codes', 'Top response': 'Most common reply', Group: 'Mailbox provider', ISP: 'Mailbox provider',
};
const headerWord = (h: string) => HEADER_WORDS[h.replace(/\*\*/g, '').trim()] ?? h;

function parseReport(md: string): { title: string; sections: Section[] } {
  const lines = md.split('\n');
  let title = ''; const sections: Section[] = []; let cur: Section | null = null; let i = 0;
  const push = (b: Block) => { if (!cur) { cur = { title: '', blocks: [] }; sections.push(cur); } cur.blocks.push(b); };
  const cells = (l: string) => l.replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
  while (i < lines.length) {
    const l = lines[i];
    if (/^# /.test(l)) { title = l.slice(2).trim(); i += 1; continue; }
    if (/^## /.test(l)) { cur = { title: l.slice(3).trim(), blocks: [] }; sections.push(cur); i += 1; continue; }
    if (/^### /.test(l)) { push({ t: 'h3', text: l.slice(4).trim() }); i += 1; continue; }
    if (l.startsWith('|') && i + 1 < lines.length && /^\|[\s\-:|]+\|$/.test(lines[i + 1])) {
      const hdr = cells(l); i += 2; const rows: string[][] = [];
      while (i < lines.length && lines[i].startsWith('|')) { rows.push(cells(lines[i])); i += 1; }
      push({ t: 'table', hdr, rows }); continue;
    }
    if (/^\s*- /.test(l)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*- /.test(lines[i])) {
        const indent = /^\s+- /.test(lines[i]);
        const text = lines[i].replace(/^\s*- /, '');
        if (indent && items.length) items[items.length - 1] += `\n${text}`; else items.push(text);
        i += 1;
      }
      push({ t: 'ul', items }); continue;
    }
    if (l.trim()) push({ t: 'p', text: l.trim() });
    i += 1;
  }
  // Merge runs of "### <domain prefix>" + a table with the same header into one table with a Domain column.
  for (const s of sections) {
    const out: Block[] = []; let k = 0;
    while (k < s.blocks.length) {
      const b = s.blocks[k]; const nx = s.blocks[k + 1];
      if (b.t === 'h3' && WARMUP_DOMAINS[b.text] && nx?.t === 'table') {
        const hdr = nx.hdr; const rows: string[][] = [];
        while (k < s.blocks.length) {
          const h = s.blocks[k]; const tb = s.blocks[k + 1];
          if (h?.t !== 'h3' || !WARMUP_DOMAINS[h.text] || tb?.t !== 'table' || tb.hdr.join('|') !== hdr.join('|')) break;
          for (const r of tb.rows) rows.push([domainName(h.text), ...r]);
          k += 2;
        }
        const ispCol = hdr.findIndex((h) => /^(ISP|Group)$/i.test(h));
        out.push({ t: 'merged', isp: ispCol >= 0 ? ispCol + 1 : -1, hdr: ['Sending domain', ...hdr], rows });
        continue;
      }
      out.push(b); k += 1;
    }
    s.blocks = out;
  }
  return { title, sections };
}

const PCT_COL = /%$/;
const NUM_CELL = /^[\d,.~%→\s-]+$/;
const ReportTable: React.FC<{ hdr: string[]; rows: string[][] }> = ({ hdr, rows }) => {
  // A column is numeric (right-aligned) when most of its cells are numbers; one "120 probe" doesn't flip it.
  const numCol = hdr.map((_, j) => j > 0 && rows.filter((r) => NUM_CELL.test(r[j] ?? '')).length * 2 > rows.length);
  return (
    <div style={{ overflowX: 'auto', margin: '6px 0 12px' }}>
      <table style={tableStyle}>
        <thead><tr>{hdr.map((h, j) => <th key={j} style={numCol[j] ? numTh : thStyle}>{headerWord(h)}</th>)}</tr></thead>
        <tbody>{rows.map((r, ri) => (
          <tr key={ri}>{r.map((c, j) => {
            const c0 = c === '250' ? 'accepted (250)' : c;
            const shown = numCol[j] && PCT_COL.test(hdr[j] ?? '') && /^\d[\d.]*$/.test(c0) ? `${c0}%` : c0;
            return <td key={j} style={numCol[j] ? numTd : { ...tdStyle, fontWeight: j === 0 ? 600 : undefined }}><Explain text={shown} /></td>;
          })}</tr>
        ))}</tbody>
      </table>
    </div>
  );
};

const MergedTable: React.FC<{ b: Extract<Block, { t: 'merged' }> }> = ({ b }) => {
  const isps = b.isp >= 0 ? [...new Set(b.rows.map((r) => r[b.isp]))] : [];
  const [isp, setIsp] = useState<string>(isps[0] ?? '');
  if (b.isp < 0) return <ReportTable hdr={b.hdr} rows={b.rows} />;
  const hdr = b.hdr.filter((_, j) => j !== b.isp);
  const rows = b.rows.filter((r) => r[b.isp] === isp).map((r) => r.filter((_, j) => j !== b.isp));
  return (
    <div>
      {chipRow(isps.map((x) => ({ key: x, label: x })), isp, setIsp, 'Mailbox provider')}
      <ReportTable hdr={hdr} rows={rows} />
    </div>
  );
};

const SECTION_OPEN = /headline|change|estimate|supply|learned/i;

export const DailyReportView: React.FC<{ report: string | null }> = ({ report }) => {
  const parsed = useMemo(() => (report ? parseReport(report) : null), [report]);
  const [closed, setClosed] = useState<Record<string, boolean>>({});
  if (!report || !parsed) return <Panel><EmptyState title="No report for this date" hint="The agent writes it at its 04:00 MT daily run; a date before the agent went live has none." /></Panel>;
  const headline = parsed.sections.find((s) => /headline/i.test(s.title));
  const rest = parsed.sections.filter((s) => s !== headline);
  return (
    <>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title={parsed.title.replace(/^colo2 daily report:\s*/i, 'Daily report · ') || 'Daily report'} right={<span style={{ fontSize: 11, color: colors.textMuted }}>written by the agent at its 04:00 MT run</span>} />
        {headline && (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(260px, 1fr))', gap: 10 }}>
            {headline.blocks.flatMap((b) => (b.t === 'ul' ? b.items : b.t === 'p' ? [b.text] : [])).map((it, j) => (
              <div key={j} style={{ borderLeft: `3px solid ${/blocked|throttl|fail|stuck/i.test(it) ? colors.warning : colors.indigo400}`, background: alpha(colors.indigo500, '0d'), padding: '8px 12px', borderRadius: 6, fontSize: 13, lineHeight: 1.45, color: colors.text }}>
                <Explain text={it} />
              </div>
            ))}
          </div>
        )}
      </Panel>
      {rest.map((s, si) => {
        const key = s.title || String(si);
        const isClosed = closed[key] ?? !SECTION_OPEN.test(s.title);
        return (
          <Panel key={key} style={{ marginBottom: 12 }}>
            <div onClick={() => setClosed((c) => ({ ...c, [key]: !isClosed }))} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', cursor: 'pointer' }}>
              <div style={{ fontSize: 14, fontWeight: 700, color: colors.heading }}><Explain text={s.title || 'Notes'} /></div>
              <Pill color={colors.idle} style={{ fontSize: 10 }}>{isClosed ? 'show' : 'hide'}</Pill>
            </div>
            {!isClosed && (
              <div style={{ marginTop: 10, fontSize: 13, color: colors.text }}>
                {s.blocks.map((b, j) => {
                  if (b.t === 'p') return <p key={j} style={{ margin: '4px 0 8px', lineHeight: 1.5 }}><Explain text={b.text} /></p>;
                  if (b.t === 'h3') return <div key={j} style={{ fontSize: 13, fontWeight: 700, color: colors.indigo300, margin: '12px 0 4px' }}><Explain text={WARMUP_DOMAINS[b.text] ? domainName(b.text) : b.text} /></div>;
                  if (b.t === 'ul') return <ul key={j} style={{ margin: '4px 0 10px', paddingLeft: 20 }}>{b.items.map((it, n) => <li key={n} style={{ margin: '3px 0', lineHeight: 1.45, whiteSpace: 'pre-line' }}><Explain text={it} /></li>)}</ul>;
                  if (b.t === 'merged') return <MergedTable key={j} b={b} />;
                  return <ReportTable key={j} hdr={b.hdr} rows={b.rows} />;
                })}
              </div>
            )}
          </Panel>
        );
      })}
      <CodeKey codes={codesIn(report)} />
    </>
  );
};

// ───────────────────────────── Lessons

type LessonRow = { at?: string; title?: string; evidence?: string; change?: string };
type ParsedLesson = { date: string; title: string; saw: string; change: string; notes: string; topic: string; at?: string };

const TOPICS: { key: string; label: string; re: RegExp }[] = [
  { key: 'yahoo', label: 'Yahoo family', re: /yahoo|TSS\d/i },
  { key: 'microsoft', label: 'Microsoft', re: /microsoft|\bMS\b|S844|S3150|S775/ },
  { key: 'comcast', label: 'Comcast', re: /comcast|RL0000/i },
  { key: 'apple', label: 'Apple', re: /apple|HCM\d|HM08/i },
  { key: 'charter', label: 'Charter', re: /charter|AUP#/i },
  { key: 'other', label: 'Other ISPs', re: /'other'|long tail/i },
  { key: 'supply', label: 'Supply', re: /reservoir|supply|drawable/i },
];
// The topic is the ISP the TITLE names first ("Microsoft accepts … while Yahoo blocks it" is a Microsoft lesson).
function topicOf(title: string, change: string): string {
  for (const text of [title, change]) {
    let best: { key: string; at: number } | null = null;
    for (const t of TOPICS) { const m = t.re.exec(text); if (m && (!best || m.index < best.at)) best = { key: t.key, at: m.index }; }
    if (best) return best.key;
  }
  return 'method';
}
const TOPIC_LABEL: Record<string, string> = { ...Object.fromEntries(TOPICS.map((t) => [t.key, t.label])), method: 'How the agent works' };

function parseFieldManual(md: string | null, journal: LessonRow[]): { lessons: ParsedLesson[]; knowledge: { title: string; items: string[] }[] } {
  if (!md) return { lessons: [], knowledge: [] };
  const li = md.indexOf('\n## Lessons');
  const head = li >= 0 ? md.slice(0, li) : md; const tail = li >= 0 ? md.slice(li) : '';
  const knowledge: { title: string; items: string[] }[] = [];
  for (const part of head.split(/\n## /).slice(1)) {
    const [t, ...rest] = part.split('\n'); const items: string[] = [];
    for (const l of rest) { if (/^- /.test(l)) items.push(l.slice(2)); else if (/^\s+\S/.test(l) && items.length) items[items.length - 1] += ` ${l.trim()}`; }
    knowledge.push({ title: t.trim(), items });
  }
  const byTitle = new Map(journal.filter((r) => r.title).map((r) => [r.title!.trim(), r]));
  const lessons: ParsedLesson[] = [];
  for (const part of tail.split(/\n### /).slice(1)) {
    const [h, ...rest] = part.split('\n');
    const m = /^(\d{4}-\d{2}-\d{2}) — (.+)$/.exec(h.trim()); if (!m) continue;
    const fields: Record<string, string> = {}; let cur = 'notes';
    for (const l of rest) {
      const f = /^- (Evidence|What changes):\s*(.*)$/.exec(l);
      if (f) { cur = f[1] === 'Evidence' ? 'saw' : 'change'; fields[cur] = f[2]; continue; }
      if (!l.trim()) continue;
      const text = l.replace(/^\s*-\s*/, '').trim();
      fields[cur] = fields[cur] ? `${fields[cur]} ${text}` : text;
    }
    const j = byTitle.get(m[2].trim());
    lessons.push({ date: m[1], title: m[2].trim(), saw: fields.saw ?? j?.evidence ?? '', change: fields.change ?? j?.change ?? '', notes: fields.notes ?? '', topic: topicOf(m[2], fields.change ?? ''), at: j?.at });
  }
  return { lessons: lessons.reverse(), knowledge };
}

export const LessonsView: React.FC<{ fieldManual: string | null; journal: LessonRow[]; date: string }> = ({ fieldManual, journal, date }) => {
  const { lessons, knowledge } = useMemo(() => parseFieldManual(fieldManual, journal), [fieldManual, journal]);
  const [topic, setTopic] = useState('all');
  const [openSaw, setOpenSaw] = useState<Record<number, boolean>>({});
  const [openK, setOpenK] = useState<Record<string, boolean>>({});
  if (lessons.length === 0 && knowledge.length === 0) return <Panel><EmptyState title="The agent has not written any lessons yet" /></Panel>;
  const counts: Record<string, number> = {};
  for (const l of lessons) counts[l.topic] = (counts[l.topic] ?? 0) + 1;
  const topics = [...TOPICS.map((t) => t.key), 'method'].filter((k) => counts[k]);
  const shown = lessons.filter((l) => topic === 'all' || l.topic === topic);
  const newToday = lessons.filter((l) => l.date === date).length;
  const allText = lessons.map((l) => `${l.title} ${l.saw} ${l.change}`).join(' ');
  return (
    <>
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="What the agent has learned" right={<span style={{ fontSize: 11, color: colors.textMuted }}>{lessons.length} lessons · {newToday} written on {date} · newest first</span>} />
        <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 10, lineHeight: 1.5 }}>
          Each lesson is something the agent measured on this box and now acts on. The highlighted line is <strong style={{ color: colors.text }}>what it does differently</strong>; open <strong style={{ color: colors.text }}>what it saw</strong> for the evidence.
        </div>
        {chipRow([{ key: 'all', label: `All (${lessons.length})` }, ...topics.map((k) => ({ key: k, label: `${TOPIC_LABEL[k]} (${counts[k]})` }))], topic, setTopic, 'Lesson topic')}
        <div style={{ display: 'grid', gap: 10 }}>
          {shown.map((l) => {
            const i = lessons.indexOf(l);
            return (
              <div key={i} style={{ border: `1px solid ${colors.panelBorder}`, borderRadius: 8, padding: '12px 14px', background: alpha(colors.indigo500, '0d') }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 6 }}>
                  <Pill color={colors.indigo400} style={{ fontSize: 10 }}>{TOPIC_LABEL[l.topic]}</Pill>
                  {l.date === date && <Pill color={colors.success} style={{ fontSize: 10 }}>new today</Pill>}
                  <span style={{ fontSize: 11, color: colors.textMuted }}>{l.at ? denverClock(l.at) : l.date}</span>
                </div>
                <div style={{ fontSize: 14, fontWeight: 700, color: colors.heading, lineHeight: 1.4, marginBottom: 8 }}><Explain text={l.title} /></div>
                {l.change && (
                  <div style={{ borderLeft: `3px solid ${colors.success}`, background: alpha(colors.success, '0d'), padding: '8px 12px', borderRadius: 6, fontSize: 13, lineHeight: 1.5, color: colors.text }}>
                    <div style={{ fontSize: 10, fontWeight: 700, letterSpacing: 0.5, color: colors.successText, marginBottom: 3 }}>WHAT IT DOES DIFFERENTLY NOW</div>
                    <Explain text={l.change} />
                  </div>
                )}
                {l.notes && <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 8, lineHeight: 1.5 }}><Explain text={l.notes} /></div>}
                {l.saw && (
                  <div style={{ marginTop: 8 }}>
                    <span onClick={() => setOpenSaw((o) => ({ ...o, [i]: !o[i] }))} style={{ fontSize: 12, color: colors.indigo300, cursor: 'pointer' }}>{openSaw[i] ? '▾ hide what it saw' : '▸ what it saw'}</span>
                    {openSaw[i] && <div style={{ fontSize: 12, color: colors.text, marginTop: 6, lineHeight: 1.55, paddingLeft: 12, borderLeft: `2px solid ${colors.hairline}` }}><Explain text={l.saw} /></div>}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      </Panel>
      {knowledge.length > 0 && (
        <Panel>
          <SectionHeader title="What the agent started with" right={<span style={{ fontSize: 11, color: colors.textMuted }}>seeded 2026-10-26 from platform history and ISP postmaster material · click to open</span>} />
          <div style={{ display: 'grid', gap: 8 }}>
            {knowledge.map((k) => (
              <div key={k.title} style={{ border: `1px solid ${colors.divider}`, borderRadius: 6, padding: '8px 12px' }}>
                <div onClick={() => setOpenK((o) => ({ ...o, [k.title]: !o[k.title] }))} style={{ display: 'flex', justifyContent: 'space-between', cursor: 'pointer', fontSize: 13, fontWeight: 600, color: colors.heading }}>
                  <span>{k.title}</span><span style={{ fontSize: 11, color: colors.textMuted, fontWeight: 400 }}>{k.items.length} points {openK[k.title] ? '▾' : '▸'}</span>
                </div>
                {openK[k.title] && <ul style={{ margin: '8px 0 2px', paddingLeft: 20 }}>{k.items.map((it, n) => <li key={n} style={{ fontSize: 12, color: colors.text, lineHeight: 1.5, margin: '3px 0' }}><Explain text={it} /></li>)}</ul>}
              </div>
            ))}
          </div>
        </Panel>
      )}
      <CodeKey codes={codesIn(allText)} />
    </>
  );
};

// ───────────────────────────── Overview: forecast + key findings, performance look-back

export type DayTotals = { date: string; byGroup: Record<string, Totals>; target: Record<string, number>; firstAt?: string | undefined };

function denverHM(ms: number): number {
  const parts = new Intl.DateTimeFormat('en-US', { timeZone: 'America/Denver', hour: '2-digit', minute: '2-digit', hour12: false }).formatToParts(new Date(ms));
  const h = Number(parts.find((p) => p.type === 'hour')?.value ?? 0) % 24; const m = Number(parts.find((p) => p.type === 'minute')?.value ?? 0);
  return h + m / 60;
}
const attemptsOf = (c: Totals) => c.delivered + c.transfail + c.fail;
const sumTotals = (rows: Totals[]): Totals => rows.reduce((a, c) => ({ delivered: a.delivered + c.delivered, transfail: a.transfail + c.transfail, fail: a.fail + c.fail }), { delivered: 0, transfail: 0, fail: 0 });

/** The agent's own report sections, by title regex (headline bullets, the 1M/day estimate). */
function reportBullets(report: string | null, title: RegExp): string[] {
  if (!report) return [];
  const s = parseReport(report).sections.find((x) => title.test(x.title));
  return s ? s.blocks.flatMap((b) => (b.t === 'ul' ? b.items : b.t === 'p' ? [b.text] : [])) : [];
}

export const ForecastFindings: React.FC<{
  date: string; isToday: boolean; ticks: Tick[]; today: Record<string, Totals>; target: Record<string, number>;
  prev?: DayTotals; report: string | null; lessonsToday: string[];
}> = ({ date, isToday, ticks, today, target, prev, report, lessonsToday }) => {
  const nowMs = Math.max(0, ...ticks.map((t) => new Date(t.at).getTime()).filter((x) => !Number.isNaN(x)));
  const hourAgo = nowMs - 3600_000;
  const lastHour = sumTotals(ticks.filter((t) => new Date(t.at).getTime() > hourAgo).flatMap((t) => Object.values(t.delta?.by ?? {}).flatMap((gs) => Object.values(gs))));
  const dayAll = sumTotals(Object.values(today));
  const targetAll = Object.values(target).reduce((a, b) => a + b, 0);
  const hoursLeft = isToday && nowMs ? Math.max(0, 24 - denverHM(nowMs)) : 0;
  const projected = Math.min(targetAll || Infinity, dayAll.delivered + lastHour.delivered * hoursLeft);
  const prevAll = prev ? sumTotals(Object.values(prev.byGroup)) : null;
  const ratio = prevAll && prevAll.delivered ? projected / prevAll.delivered : null;
  const prevStartH = prev?.firstAt ? denverHM(new Date(prev.firstAt).getTime()) : 0;
  const prevPartial = prevStartH >= 6;  // the day before started mid-day: a partial day, not a like-for-like base
  const vsText = ratio == null ? '—' : ratio >= 3 ? `${ratio.toFixed(0)}× more` : `${ratio >= 1 ? '+' : ''}${((ratio - 1) * 100).toFixed(0)}%`;

  const findings: { tone: string; text: string }[] = [];
  findings.push({ tone: colors.indigo400, text: `**${fmt(dayAll.delivered)} delivered${isToday ? ' so far' : ''}**, ${pctOf(dayAll.delivered, targetAll, 0)} of today's target of ${fmt(targetAll)}.` });
  for (const g of ISP_ORDER) {
    const c = today[g]; if (!c) continue; const n = attemptsOf(c); if (n < 20) continue;
    if (c.fail / n >= 0.9) findings.push({ tone: colors.danger, text: `**${ispName(g)} ${g === 'other' ? 'are' : 'is'} blocking us:** ${fmt(c.fail)} of ${fmt(n)} attempts bounced, ${fmt(c.delivered)} delivered.` });
    else if (c.transfail / n > 0.1) findings.push({ tone: colors.warning, text: `**${ispName(g)} ${g === 'other' ? 'are' : 'is'} slowing us:** ${pctOf(c.transfail, n, 0)} of attempts deferred (${fmt(c.transfail)}). The box retries them.` });
    else if (c.fail / n > 0.05) findings.push({ tone: colors.warning, text: `**${ispName(g)} bounces are high:** ${pctOf(c.fail, n, 1)} of attempts (${fmt(c.fail)}).` });
  }
  const top = ISP_ORDER.filter((g) => today[g]).sort((a, b) => today[b].delivered - today[a].delivered)[0];
  if (top && dayAll.delivered) findings.push({ tone: colors.success, text: `**${ispName(top)} carries the volume:** ${pctOf(today[top].delivered, dayAll.delivered, 0)} of everything delivered today.` });
  for (const t of lessonsToday.slice(-3).reverse()) findings.push({ tone: colors.indigo300, text: `Learned today: ${t}` });
  if (lessonsToday.length > 3) findings.push({ tone: colors.idle, text: `+${lessonsToday.length - 3} more lessons today on the Lessons tab.` });

  const headline = reportBullets(report, /headline/i);
  const million = reportBullets(report, /1M|estimate|forecast/i);
  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(420px, 1fr))', gap: 14, marginBottom: 14 }}>
      <Panel>
        <SectionHeader title="Forecast" right={<span style={{ fontSize: 11, color: colors.textMuted }}>{isToday && nowMs ? `from the box read at ${denverClock(new Date(nowMs).toISOString())}` : `${date} is closed`}</span>} />
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(170px, 1fr))', gap: 10, marginBottom: 10 }}>
          {[
            { l: isToday ? 'Expected by midnight' : 'Delivered that day', v: fmt(Math.round(projected)), s: isToday ? `if the last hour's pace (${fmt(lastHour.delivered)}/hour) holds for ${hoursLeft.toFixed(1)} more hours; capped at the target` : 'final' },
            { l: 'Versus the day before', v: vsText, s: prev && prevAll ? `${prev.date}: ${fmt(prevAll.delivered)} delivered${prevPartial ? ` (partial day: first send ${String(Math.floor(prevStartH)).padStart(2, '0')}:00 MT)` : ''}` : 'no earlier day on record' },
            { l: 'Target reached', v: targetAll ? pctOf(projected, targetAll, 0) : '—', s: targetAll ? `of ${fmt(targetAll)} at that pace` : 'no target set' },
          ].map((x) => (
            <div key={x.l} style={{ border: `1px solid ${colors.panelBorder}`, borderRadius: 8, padding: '10px 14px', background: alpha(colors.indigo500, '0d') }}>
              <div style={{ fontSize: 11, color: colors.textMuted }}>{x.l}</div>
              <div style={{ fontSize: 22, fontWeight: 700, color: colors.heading, fontVariantNumeric: 'tabular-nums' }}>{x.v}</div>
              <div style={{ fontSize: 11, color: colors.textMuted, lineHeight: 1.4 }}>{x.s}</div>
            </div>
          ))}
        </div>
        {million.length > 0 && (
          <div style={{ fontSize: 12, color: colors.text, lineHeight: 1.5 }}>
            <div style={{ fontSize: 11, fontWeight: 700, color: colors.indigo300, letterSpacing: 0.4, marginBottom: 4 }}>THE AGENT'S OWN FORECAST (04:00 MT REPORT)</div>
            <ul style={{ margin: 0, paddingLeft: 18 }}>{million.map((m, i) => <li key={i} style={{ margin: '2px 0', whiteSpace: 'pre-line' }}><Explain text={m} /></li>)}</ul>
          </div>
        )}
      </Panel>
      <Panel>
        <SectionHeader title="Key findings" right={<span style={{ fontSize: 11, color: colors.textMuted }}>measured from today's delivery log · plus the agent's morning headline</span>} />
        <div style={{ display: 'grid', gap: 6 }}>
          {findings.map((f, i) => <div key={i} style={{ borderLeft: `3px solid ${f.tone}`, padding: '4px 10px', fontSize: 13, lineHeight: 1.45, color: colors.text }}><Explain text={f.text} /></div>)}
        </div>
        {headline.length > 0 && (
          <div style={{ marginTop: 10, fontSize: 12, color: colors.textMuted, lineHeight: 1.5 }}>
            <div style={{ fontSize: 11, fontWeight: 700, color: colors.indigo300, letterSpacing: 0.4, marginBottom: 4 }}>THE AGENT'S MORNING HEADLINE</div>
            <ul style={{ margin: 0, paddingLeft: 18 }}>{headline.map((h, i) => <li key={i} style={{ margin: '2px 0' }}><Explain text={h} /></li>)}</ul>
          </div>
        )}
      </Panel>
    </div>
  );
};

export const PerformanceLookback: React.FC<{ date: string; today: Record<string, Totals>; target: Record<string, number>; history: DayTotals[]; loading: boolean }> = ({ date, today, target, history, loading }) => {
  const days: DayTotals[] = [{ date, byGroup: today, target }, ...history];
  const groups = ISP_ORDER.filter((g) => days.some((d) => d.byGroup[g]));
  const cell = (c: Totals | undefined, t: number | undefined) => {
    if (!c) return <td style={{ ...numTd, color: colors.textFaint }}>—</td>;
    const n = attemptsOf(c);
    return (
      <td style={numTd}>
        <div style={{ fontWeight: 600 }}>{fmt(c.delivered)}</div>
        <div style={{ fontSize: 11, color: colors.textMuted }}>{pctOf(c.delivered, n, 0)} delivered{t ? ` · ${pctOf(c.delivered, t, 0)} of target` : ''}</div>
      </td>
    );
  };
  return (
    <Panel>
      <SectionHeader title="Performance · today and earlier days" right={<span style={{ fontSize: 11, color: colors.textMuted }}>{loading ? 'loading earlier days…' : `${days.length} day${days.length === 1 ? '' : 's'} on record (the agent began 2026-10-26)`}</span>} />
      {groups.length === 0 ? <EmptyState title="No deliveries recorded" /> : (
        <div style={{ overflowX: 'auto' }}>
          <table style={tableStyle}>
            <thead><tr><th style={thStyle}>Mailbox provider</th>{days.map((d, i) => <th key={d.date} style={numTh}>{i === 0 ? `${d.date} (selected)` : d.date}</th>)}</tr></thead>
            <tbody>
              {groups.map((g) => <tr key={g}><td style={{ ...tdStyle, fontWeight: 600 }}>{ispName(g)}</td>{days.map((d) => <React.Fragment key={d.date}>{cell(d.byGroup[g], d.target[g])}</React.Fragment>)}</tr>)}
              <tr><td style={{ ...tdStyle, fontWeight: 700 }}>All providers</td>{days.map((d) => <React.Fragment key={d.date}>{cell(sumTotals(Object.values(d.byGroup)), Object.values(d.target).reduce((a, b) => a + b, 0))}</React.Fragment>)}</tr>
            </tbody>
          </table>
        </div>
      )}
      <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 6 }}>Delivered count, then delivered as a share of attempts and of that day's target. Deferred and bounced detail for each day is on the Delivery tab.</div>
    </Panel>
  );
};

// ───────────────────────────── The agent's check-in note, as a card (operator 2026-10-27: "clean this section up … make it fun")

type NoteSection = { title: string; body: string; items: { label: string; text: string }[] };
const PROBLEM = /fail|error|missed|503|weren't|wasn't|stuck|blocked|down\b/i;
const SECTION_ICON: [RegExp, string][] = [
  [PROBLEM, '🚨'], [/microsoft/i, '🪟'], [/apple/i, '🍎'], [/yahoo/i, '💜'], [/comcast/i, '📡'], [/charter/i, '🛰️'],
  [/other|long tail/i, '🌐'], [/supply|reservoir|records/i, '📦'], [/freeze|keeping|hold/i, '🧊'], [/plan|next/i, '🧭'],
];
const ITEM_ICON: [RegExp, string][] = [
  [/delivered/i, '📬'], [/block/i, '🧱'], [/dead|USER_UNKNOWN|mailbox/i, '👻'], [/rate.?limit|S844|S775|throttl/i, '🚦'],
  [/defer|delay/i, '⏳'], [/connection|timeout/i, '🔌'], [/bounce/i, '↩️'], ...SECTION_ICON,
];
const iconFor = (t: string, table: [RegExp, string][], dflt: string) => table.find(([re]) => re.test(t))?.[1] ?? dflt;
// Bold the numbers so a scan finds them (text already wrapped in ** is left alone).
const boldNumbers = (t: string) => t.split(/(\*\*[^*]+\*\*)/g).map((c) => (c.startsWith('**') ? c : c.replace(/(~?\d[\d,]*(?:\.\d+)?%?(?:–\d[\d,]*%?)?)/g, '**$1**'))).join('');

function parseNote(text: string): { lead: string[]; sections: NoteSection[]; tail: string[] } {
  const paras = text.split(/\n\s*\n/).map((p) => p.trim()).filter(Boolean);
  const lead: string[] = []; const tail: string[] = []; const sections: NoteSection[] = [];
  for (const p of paras) {
    const lines = p.split('\n');
    const head = /^\*\*(.+?):?\*\*:?\s*(.*)$/.exec(lines[0]);
    const bullets = lines.filter((l) => /^\s*- /.test(l));
    if (head) {
      const items: NoteSection['items'] = [];
      for (const l of lines.slice(1)) {
        if (/^\s{2,}- /.test(l) && items.length) { items[items.length - 1].text += `\n${l.replace(/^\s*- /, '')}`; continue; }
        const b = /^\s*- (?:\*\*(.+?):?\*\*:?\s*)?(.*)$/.exec(l); if (b) items.push({ label: b[1] ?? '', text: b[2] });
      }
      sections.push({ title: head[1].replace(/:$/, ''), body: head[2], items });
    } else if (bullets.length && bullets.length === lines.length) {
      sections.push({ title: 'Notes', body: '', items: bullets.map((l) => { const b = /^\s*- (?:\*\*(.+?):?\*\*:?\s*)?(.*)$/.exec(l)!; return { label: b[1] ?? '', text: b[2] }; }) });
    } else if (sections.length === 0) lead.push(p); else tail.push(p);
  }
  return { lead, sections, tail };
}

export const CheckinNote: React.FC<{ text: string }> = ({ text }) => {
  const { lead, sections, tail } = useMemo(() => parseNote(text), [text]);
  const trouble = PROBLEM.test(text);
  const moved = /\b(raised|eased|lowered|stepped|set .* to)\b/i.test(lead.join(' ')) && !/no (rate )?changes/i.test(lead.join(' '));
  if (sections.length === 0) return <div style={{ fontSize: 13, whiteSpace: 'pre-wrap', lineHeight: 1.55, color: colors.text }}><Explain text={text} /></div>;
  return (
    <div>
      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 10 }}>
        <Pill color={trouble ? colors.warning : colors.success}>{trouble ? '🟡 One thing needs a look' : '🟢 All clear'}</Pill>
        <Pill color={moved ? colors.indigo400 : colors.idle}>{moved ? '🎛️ Made a move' : '🧘 Held steady'}</Pill>
      </div>
      {lead.length > 0 && (
        <div style={{ display: 'flex', gap: 10, alignItems: 'flex-start', marginBottom: 12 }}>
          <div style={{ fontSize: 22, lineHeight: 1 }} aria-hidden>🤖</div>
          <div style={{ background: alpha(colors.indigo500, '14'), border: `1px solid ${colors.panelBorder}`, borderRadius: '4px 12px 12px 12px', padding: '10px 14px', fontSize: 14, lineHeight: 1.5, color: colors.heading }}>
            {lead.map((p, i) => <div key={i} style={{ marginTop: i ? 6 : 0 }}><Explain text={p} /></div>)}
          </div>
        </div>
      )}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 10 }}>
        {sections.map((s, i) => {
          const bad = PROBLEM.test(`${s.title} ${s.body}`);
          return (
            <div key={i} style={{ border: `1px solid ${bad ? alpha(colors.warning, '66') : colors.panelBorder}`, background: bad ? alpha(colors.warning, '0d') : alpha(colors.indigo500, '0d'), borderRadius: 10, padding: '10px 12px' }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, fontWeight: 700, color: bad ? colors.warningText : colors.heading, marginBottom: 6 }}>
                <span style={{ fontSize: 16 }} aria-hidden>{iconFor(s.title, SECTION_ICON, '📋')}</span><Explain text={s.title} />
              </div>
              {s.body && <div style={{ fontSize: 12.5, lineHeight: 1.5, color: colors.text, marginBottom: s.items.length ? 6 : 0 }}><Explain text={boldNumbers(s.body)} /></div>}
              <div style={{ display: 'grid', gap: 6 }}>
                {s.items.map((it, j) => (
                  <div key={j} style={{ display: 'flex', gap: 8, fontSize: 12.5, lineHeight: 1.45, color: colors.text }}>
                    <span aria-hidden style={{ width: 18, textAlign: 'center' }}>{iconFor(`${it.label} ${it.text}`, ITEM_ICON, '•')}</span>
                    <div style={{ whiteSpace: 'pre-line' }}>{it.label && <span style={{ fontWeight: 700, color: colors.indigo200 }}><Explain text={it.label} />: </span>}<Explain text={boldNumbers(it.text)} /></div>
                  </div>
                ))}
              </div>
            </div>
          );
        })}
      </div>
      {tail.length > 0 && (
        <div style={{ marginTop: 10, display: 'flex', gap: 8, alignItems: 'flex-start', fontSize: 12.5, lineHeight: 1.5, color: colors.text, borderTop: `1px dashed ${colors.hairline}`, paddingTop: 8 }}>
          <span aria-hidden>🧭</span><div><span style={{ fontWeight: 700, color: colors.indigo200 }}>Plan: </span>{tail.map((p, i) => <span key={i}><Explain text={p} /> </span>)}</div>
        </div>
      )}
    </div>
  );
};
