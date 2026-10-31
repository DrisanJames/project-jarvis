// The Reporting cube, decoded and sliced in the browser. One fetch per date
// range (internal/analytics/reader_cube.go); every filter and row grouping after
// that is computed here, so changing them never goes back to the server.

import type { Transport } from '../shared/filters';

export interface CubeRow {
  day: string;
  lane: string;
  isp: string;
  brand: string;
  event: string;
  value: string; // the picked column on a Sending Server / Pool / Email Domain cube
  count: number;
}

export interface CubePayload {
  disabled?: boolean;
  columns?: string[];
  dict?: Record<string, string[]>;
  rows?: number[][];
  truncated?: boolean;
  fetched_at?: string;
}

export function decodeCube(p: CubePayload): CubeRow[] {
  const cols = p.columns ?? [];
  const dict = p.dict ?? {};
  const at = (name: string) => cols.indexOf(name);
  const read = (row: number[], name: string): string => {
    const i = at(name);
    return i < 0 ? '' : (dict[name]?.[row[i]] ?? '');
  };
  const countAt = at('count');
  return (p.rows ?? []).map((row) => ({
    day: read(row, 'local_dt'),
    lane: read(row, 'lane'),
    isp: read(row, 'isp'),
    brand: read(row, 'brand'),
    event: read(row, 'event_type'),
    value: read(row, 'value'),
    count: row[countAt] ?? 0,
  }));
}

const TRANSPORT_LANES: Record<Transport, string[] | null> = {
  combined: null,
  ses: ['ses'],
  mta: ['pmta_a', 'pmta_b', 'mta'],
  pmta_a: ['pmta_a'],
  pmta_b: ['pmta_b'],
  kumo: ['colo1', 'colo2'],
  colo1: ['colo1'],
  colo2: ['colo2'],
};

// brandApex reduces a sending domain to its root: em.discountblog.com and
// aa.discountblog.com both become discountblog.com.
export function brandApex(domain: string): string {
  const parts = domain.trim().toLowerCase().split('.').filter(Boolean);
  return parts.length <= 2 ? parts.join('.') : parts.slice(-2).join('.');
}

export const matchesBrand = (domain: string, filter: string): boolean => {
  const d = domain.toLowerCase();
  const f = filter.trim().toLowerCase();
  return !f || d === f || d.endsWith(`.${f}`);
};

export interface CubeSlice {
  transport: Transport;
  ispGroup: string;
  brand: string;
}

export function sliceCube(rows: CubeRow[], s: CubeSlice): CubeRow[] {
  const lanes = TRANSPORT_LANES[s.transport];
  const isp = s.ispGroup.trim().toLowerCase();
  const brand = s.brand.trim();
  return rows.filter((r) => (
    (!lanes || lanes.includes(r.lane))
    && (!isp || r.isp === isp)
    && (!brand || matchesBrand(r.brand, brand))
  ));
}

export function groupRows(rows: CubeRow[], keyOf: (r: CubeRow) => string): Map<string, CubeRow[]> {
  const out = new Map<string, CubeRow[]>();
  for (const r of rows) {
    const k = keyOf(r);
    const list = out.get(k);
    if (list) list.push(r);
    else out.set(k, [r]);
  }
  return out;
}

export function typeTotals(rows: CubeRow[]): Record<string, number> {
  const m: Record<string, number> = {};
  for (const r of rows) m[r.event] = (m[r.event] || 0) + r.count;
  return m;
}

const LANE_LABELS: Record<string, string> = {
  ses: 'SES', pmta_a: 'PMTA A', pmta_b: 'PMTA B', colo1: 'COLO 1', colo2: 'COLO 2', mta: 'Other PMTA',
};
export const laneLabel = (lane: string): string => LANE_LABELS[lane] ?? 'Not matched';

const ISP_LABELS: Record<string, string> = {
  gmail: 'Gmail', microsoft: 'Microsoft', yahoo: 'Yahoo', aol: 'AOL', apple: 'Apple', comcast: 'Comcast',
  att: 'AT&T', sbcglobal: 'SBCGlobal', cox: 'Cox', charter: 'Charter/Spectrum', verizon: 'Verizon', other: 'Other',
};
export const ispLabel = (isp: string): string => ISP_LABELS[isp] ?? isp;

const EMAIL_GROUPS: Array<[string, string[]]> = [
  ['Gmail', ['gmail.com', 'googlemail.com']],
  ['Microsoft', ['outlook.com', 'hotmail.com', 'live.com', 'msn.com', 'hotmail.co.uk', 'windowslive.com', 'passport.com', 'outlook.co.uk']],
  ['Yahoo family', ['yahoo.com', 'ymail.com', 'rocketmail.com', 'yahoo.co.uk', 'yahoo.ca', 'aol.com', 'aim.com']],
  ['Apple', ['icloud.com', 'me.com', 'mac.com']],
  ['Comcast', ['comcast.net', 'xfinity.com']],
  ['Cox', ['cox.net']],
  ['Charter/Spectrum', ['charter.net', 'spectrum.net', 'rr.com']],
  ['AT&T', ['att.net', 'sbcglobal.net', 'bellsouth.net', 'ameritech.net', 'pacbell.net', 'swbell.net', 'flash.net', 'nvbell.net', 'snet.net', 'prodigy.net']],
  ['Verizon', ['verizon.net']],
];
const EMAIL_GROUP_BY_DOMAIN = new Map(EMAIL_GROUPS.flatMap(([g, ds]) => ds.map((d) => [d, g] as [string, string])));

export function emailGroup(domain: string): string {
  const d = domain.trim().toLowerCase();
  const hit = EMAIL_GROUP_BY_DOMAIN.get(d);
  if (hit) return hit;
  if (d.endsWith('.rr.com')) return 'Charter/Spectrum';
  if (d.startsWith('yahoo.')) return 'Yahoo family';
  return 'Other';
}

// apexOptions lists the root domains in the data, biggest first, for the
// filter's suggestions.
export function apexOptions(rows: CubeRow[]): string[] {
  const size = new Map<string, number>();
  for (const r of rows) {
    if (!r.brand) continue;
    const a = brandApex(r.brand);
    size.set(a, (size.get(a) || 0) + r.count);
  }
  return Array.from(size.entries()).sort((a, b) => b[1] - a[1]).map(([a]) => a);
}

// Campaign Lookup's tree: root domain → sub domain → day, biggest first, newest
// day first.
export interface CampaignTreeRow {
  sending_domain: string;
  day: string;
  campaigns: number;
  sent: number;
}

export interface CampaignTreeSub {
  domain: string;
  campaigns: number;
  sent: number;
  days: CampaignTreeRow[];
}

export interface CampaignTreeApex {
  apex: string;
  campaigns: number;
  sent: number;
  subs: CampaignTreeSub[];
}

export function buildCampaignTree(rows: CampaignTreeRow[]): CampaignTreeApex[] {
  const apexes = new Map<string, Map<string, CampaignTreeRow[]>>();
  for (const r of rows) {
    const a = brandApex(r.sending_domain);
    let subs = apexes.get(a);
    if (!subs) { subs = new Map(); apexes.set(a, subs); }
    const days = subs.get(r.sending_domain);
    if (days) days.push(r);
    else subs.set(r.sending_domain, [r]);
  }
  const sum = (ns: number[]) => ns.reduce((n, v) => n + v, 0);
  const bySent = <T extends { sent: number }>(a: T, b: T) => b.sent - a.sent;
  return Array.from(apexes.entries()).map(([apex, subMap]) => {
    const subs = Array.from(subMap.entries()).map(([domain, days]) => ({
      domain,
      campaigns: sum(days.map((d) => d.campaigns)),
      sent: sum(days.map((d) => d.sent)),
      days: [...days].sort((a, b) => b.day.localeCompare(a.day)),
    })).sort(bySent);
    return { apex, campaigns: sum(subs.map((s) => s.campaigns)), sent: sum(subs.map((s) => s.sent)), subs };
  }).sort(bySent);
}
