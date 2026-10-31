import { describe, expect, it } from 'vitest';
import {
  apexOptions, brandApex, buildCampaignTree, decodeCube, emailGroup, groupRows, laneLabel, sliceCube, typeTotals,
} from './cube';
import type { CubeRow } from './cube';

const row = (lane: string, isp: string, brand: string, event: string, count: number, day = '2026-10-30'): CubeRow => ({
  day, lane, isp, brand, event, count, value: '',
});

const rows: CubeRow[] = [
  row('ses', 'gmail', 'discountblog.com', 'delivered', 100),
  row('ses', 'gmail', 'em.discountblog.com', 'open', 40),
  row('pmta_a', 'yahoo', 'aa.discountblog.com', 'delivered', 50),
  row('pmta_b', 'yahoo', 'notdiscountblog.com', 'delivered', 7),
  row('colo1', 'microsoft', 'em.hfcl.net', 'delivered', 30),
  row('colo2', 'microsoft', 'em.hfcl.net', 'click', 3),
  row('', 'gmail', 'em.hfcl.net', 'open', 9),
];

describe('decodeCube', () => {
  it('reads rows through the dictionary by column name', () => {
    const out = decodeCube({
      columns: ['local_dt', 'lane', 'isp', 'brand', 'event_type', 'count'],
      dict: { local_dt: ['2026-10-30'], lane: ['ses', 'pmta_a'], isp: ['gmail'], brand: ['a.com'], event_type: ['open'] },
      rows: [[0, 1, 0, 0, 0, 12]],
    });
    expect(out).toEqual([{ day: '2026-10-30', lane: 'pmta_a', isp: 'gmail', brand: 'a.com', event: 'open', value: '', count: 12 }]);
  });
  it('is empty for a disabled reply', () => {
    expect(decodeCube({ disabled: true })).toEqual([]);
  });
});

describe('sliceCube', () => {
  const total = (s: Parameters<typeof sliceCube>[1]) => sliceCube(rows, s).reduce((n, r) => n + r.count, 0);
  const all = { transport: 'combined' as const, ispGroup: '', brand: '' };

  it('keeps every row, unmatched lanes included, on Combined', () => {
    expect(total(all)).toBe(239);
  });
  it('unmatched opens leave once a sending route is picked', () => {
    expect(total({ ...all, transport: 'ses' })).toBe(140);
  });
  it('groups the boxes under MTA and Kumo', () => {
    expect(total({ ...all, transport: 'mta' })).toBe(57);
    expect(total({ ...all, transport: 'kumo' })).toBe(33);
  });
  it('a box is only that box', () => {
    expect(total({ ...all, transport: 'pmta_a' })).toBe(50);
    expect(total({ ...all, transport: 'colo2' })).toBe(3);
  });
  it('filters by provider', () => {
    expect(total({ ...all, ispGroup: 'Yahoo ' })).toBe(57);
  });
  it('a root domain takes in its sub domains but not look-alikes', () => {
    expect(total({ ...all, brand: 'discountblog.com' })).toBe(190);
  });
  it('a sub domain is only that sub domain', () => {
    expect(total({ ...all, brand: 'aa.discountblog.com' })).toBe(50);
  });
  it('combines filters', () => {
    expect(total({ transport: 'ses', ispGroup: 'gmail', brand: 'hfcl.net' })).toBe(0);
  });
});

describe('labels and groups', () => {
  it('names the sending routes exactly', () => {
    expect(['ses', 'pmta_a', 'pmta_b', 'colo1', 'colo2'].map(laneLabel)).toEqual(['SES', 'PMTA A', 'PMTA B', 'COLO 1', 'COLO 2']);
  });
  it('reduces a sub domain to its root', () => {
    expect(brandApex('aa.discountblog.com')).toBe('discountblog.com');
    expect(brandApex('EM.us-finance.com')).toBe('us-finance.com');
    expect(brandApex('hfcl.net')).toBe('hfcl.net');
  });
  it('puts email domains in their logical group', () => {
    expect(emailGroup('hotmail.co.uk')).toBe('Microsoft');
    expect(emailGroup('aol.com')).toBe('Yahoo family');
    expect(emailGroup('yahoo.fr')).toBe('Yahoo family');
    expect(emailGroup('sbcglobal.net')).toBe('AT&T');
    expect(emailGroup('nc.rr.com')).toBe('Charter/Spectrum');
    expect(emailGroup('acme.io')).toBe('Other');
  });
  it('lists root domains biggest first', () => {
    expect(apexOptions(rows)[0]).toBe('discountblog.com');
    expect(apexOptions(rows)).toContain('hfcl.net');
  });
});

describe('rollups', () => {
  it('totals by event type', () => {
    expect(typeTotals(rows).delivered).toBe(187);
  });
  it('groups by any key', () => {
    expect(groupRows(rows, (r) => brandApex(r.brand)).get('hfcl.net')?.length).toBe(3);
  });
});

describe('buildCampaignTree', () => {
  it('nests root domain, sub domain and day, newest day first', () => {
    const tree = buildCampaignTree([
      { sending_domain: 'em.a.com', day: '2026-10-29', campaigns: 2, sent: 10 },
      { sending_domain: 'em.a.com', day: '2026-10-30', campaigns: 1, sent: 5 },
      { sending_domain: 'aa.a.com', day: '2026-10-30', campaigns: 1, sent: 40 },
      { sending_domain: 'em.b.com', day: '2026-10-30', campaigns: 1, sent: 1 },
    ]);
    expect(tree.map((a) => a.apex)).toEqual(['a.com', 'b.com']);
    expect(tree[0]).toMatchObject({ campaigns: 4, sent: 55 });
    expect(tree[0].subs.map((s) => s.domain)).toEqual(['aa.a.com', 'em.a.com']);
    expect(tree[0].subs[1].days.map((d) => d.day)).toEqual(['2026-10-30', '2026-10-29']);
  });
});
