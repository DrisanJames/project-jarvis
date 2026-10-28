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
const ShareBar: React.FC<{ pct: number; color?: string }> = ({ pct, color = colors.success }) => (
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

export const SpeedTargetsView: React.FC<{
  date: string; rates: Record<string, Record<string, Rate>> | null; plan: Record<string, Record<string, number>> | undefined;
  jit: Record<string, JitSlot> | null;
}> = ({ date, rates, plan, jit }) => {
  const prefixes = useMemo(() => [...new Set([...Object.keys(rates ?? {}), ...Object.keys(plan ?? {})])].sort((a, b) => domainName(a).localeCompare(domainName(b))), [rates, plan]);
  const groups = ISP_ORDER.filter((g) => prefixes.some((p) => rates?.[p]?.[g] || plan?.[p]?.[g] != null));
  const [isp, setIsp] = useState<string>(groups[0] ?? 'microsoft');
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const sent = useMemo(() => {
    const out: Record<string, Record<string, number>> = {};
    for (const s of Object.values(jit ?? {})) for (const [g, n] of Object.entries(s.groups)) ((out[s.prefix] ||= {})[g] = (out[s.prefix]?.[g] ?? 0) + n);
    return out;
  }, [jit]);
  if (prefixes.length === 0) return <Panel><EmptyState title="The agent has not set any speeds or targets yet" /></Panel>;
  const reasons = prefixes.map((p) => rates?.[p]?.[isp]?.reason ?? '').join(' ');
  return (
    <>
      <Panel>
        <SectionHeader title={`Speed & targets · ${ispName(isp)} · ${date}`} right={<span style={{ fontSize: 11, color: colors.textMuted }}>speed = the pace the agent allows · target = the most it will send today</span>} />
        {chipRow(groups.map((g) => ({ key: g, label: ispName(g) })), isp, setIsp, 'Mailbox provider')}
        <div style={{ overflowX: 'auto' }}>
          <table style={tableStyle}>
            <thead><tr>
              <th style={thStyle}>Sending domain</th><th style={numTh}>Speed now</th><th style={thStyle}>Last change</th>
              <th style={numTh} title="speed × the domain's IPs">Domain pace</th><th style={numTh}>Today's target</th><th style={{ ...thStyle, width: 190 }}>Sent toward target</th><th style={thStyle}>The agent's reason</th>
            </tr></thead>
            <tbody>{prefixes.map((p) => {
              const r = rates?.[p]?.[isp]; const target = plan?.[p]?.[isp]; const done = sent[p]?.[isp] ?? 0;
              const ips = WARMUP_DOMAINS[p]?.ips;
              const change = !r ? '—' : r.previous == null ? `first set ${denverClock(r.set_at)}`
                : r.per_ip_hour > r.previous ? `raised from ${r.previous} · ${denverClock(r.set_at)}`
                : r.per_ip_hour < r.previous ? `eased from ${r.previous} · ${denverClock(r.set_at)}`
                : `unchanged since ${denverClock(r.set_at)}`;
              const tone = !r || r.previous == null ? colors.textMuted : r.per_ip_hour > r.previous ? colors.successText : r.per_ip_hour < r.previous ? colors.warningText : colors.textMuted;
              const key = `${p}|${isp}`;
              return (
                <tr key={p}>
                  <td style={{ ...tdStyle, fontWeight: 600, whiteSpace: 'nowrap' }} title={`prefix ${p}${ips ? ` · ${ips} IPs` : ''}`}>{domainName(p)}</td>
                  <td style={{ ...numTd, whiteSpace: 'nowrap' }}>{r ? <><strong>{r.per_ip_hour}</strong> <span style={{ fontSize: 11, color: colors.textMuted }}>per IP per hour</span></> : <span style={{ color: colors.textFaint }}>not set</span>}</td>
                  <td style={{ ...tdStyle, fontSize: 12, color: tone, whiteSpace: 'nowrap' }}>{change}</td>
                  <td style={{ ...numTd, whiteSpace: 'nowrap' }}>{r && ips ? <>{fmt(r.per_ip_hour * ips)} <span style={{ fontSize: 11, color: colors.textMuted }}>/ hour on {ips} IPs</span></> : '—'}</td>
                  <td style={numTd}>{target == null ? <span style={{ color: colors.textFaint }}>none</span> : target === 0 ? <span style={{ color: colors.textFaint }}>not mailed today</span> : fmt(target)}</td>
                  <td style={tdStyle}>{target ? <div><div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 3, fontVariantNumeric: 'tabular-nums' }}>{fmt(done)} of {fmt(target)} · {pctOf(done, target, 0)}</div><ShareBar pct={done / target} color={colors.indigo400} /></div> : '—'}</td>
                  <td style={{ ...tdStyle, fontSize: 12, color: colors.text, maxWidth: 420, cursor: r?.reason ? 'pointer' : undefined }} onClick={() => setOpen((o) => ({ ...o, [key]: !o[key] }))}>
                    {r?.reason ? <div style={open[key] ? { whiteSpace: 'pre-wrap' } : { display: '-webkit-box', WebkitLineClamp: 2, WebkitBoxOrient: 'vertical', overflow: 'hidden' }}><Explain text={r.reason} /></div> : <span style={{ color: colors.textFaint }}>—</span>}
                  </td>
                </tr>
              );
            })}</tbody>
          </table>
        </div>
        <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 8, lineHeight: 1.5 }}>
          <strong style={{ color: colors.text }}>Speed</strong> is the most messages each IP may send to {ispName(isp)} in an hour. <strong style={{ color: colors.text }}>Domain pace</strong> is that speed times the domain's IPs. <strong style={{ color: colors.text }}>Today's target</strong> is the day's ceiling; sending stops when it is reached. Click a reason to read it in full.
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
