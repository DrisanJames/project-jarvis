// BrandWaveGrid — the REAL plan as brand columns × wave rows.
//
// Operator spec (2026-06-20): everywhere brands are displayed, show ALL brands and
// let the operator scroll HORIZONTALLY across them. This renders the live plan for a
// send-date as a matrix: each brand (sending domain) is a column you scroll across;
// each row is a wave (W1..W4, plus any standalone sends). A cell aggregates that
// brand×wave's real campaigns (offer · summed volume · status · split count). Pure
// read view over the campaigns endpoint — no synthesis, mirrors what is staged.
//
// Reused by the Send-Day Planner (under the gates) and intended as the standard
// all-brands layout wherever a brand axis appears.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { useAuth } from '../../../contexts/AuthContext';

const DEFAULT_ORG = '00000000-0000-0000-0000-000000000001';
const TZ = 'America/Denver';

interface CampaignRow {
  id: string;
  name: string;
  status: string;
  total_recipients: number;
  from_email: string;
  scheduled_at: string | null;
}

interface Cell {
  offer: string;
  volume: number;
  count: number;            // # of split campaigns aggregated (ISP/tier splits)
  statuses: Set<string>;
  firstScheduled: string | null;
}

const STATUS_FG: Record<string, string> = {
  draft: '#9ca3af', scheduled: '#00b0ff', preparing: '#facc15',
  finalizing_audience: '#facc15', sending: '#c084fc', sent: '#00b894',
  completed: '#00b894', failed: '#e94560', cancelled: '#bdbdbd',
};

const num = (n: number): string => (n || 0).toLocaleString();
const domainOf = (email: string): string =>
  (email && email.includes('@') ? email.split('@')[1] : '(no domain)').toLowerCase();
const mtTime = (iso: string | null): string => {
  if (!iso) return '—';
  try { return new Date(iso).toLocaleString('en-US', { timeZone: TZ, hour: 'numeric', minute: '2-digit', hour12: true }); }
  catch { return '—'; }
};

// Brand DISPLAY label from the name ("jun20 - Discount Blog - W1-... - offer" -> "Discount
// Blog"). This is for display only — brand IDENTITY (column key / aggregation key) is the
// authoritative from_email DOMAIN, never this positional parse, so a hyphenated label or a
// stray " - " can't collapse two domains or mis-bucket a campaign.
const brandLabelOf = (name: string): string => {
  const parts = (name || '').split(' - ');
  return parts.length >= 2 ? parts.slice(1, parts.length - 2).join(' - ').trim() || parts[1].trim() : '';
};
// A nice label fallback derived from the domain (em.discountblog.com -> "discountblog.com").
const labelFromDomain = (d: string): string => d.replace(/^(em|m)\./, '');
// Offer = the last " - " segment ("liberty-mutual"); if that segment is actually a routing/
// wave token (3-segment standalone names), infer the offer from keywords in the whole name.
const offerOf = (name: string): string => {
  const parts = (name || '').split(' - ');
  const last = (parts[parts.length - 1] || '').trim();
  if (parts.length < 4 || /^W\d/i.test(last) || /SPICY|MSFT|GMAIL|YAHOO|SES|SEED|TIGHT|STD/i.test(last)) {
    const lc = (name || '').toLowerCase();
    for (const k of ['liberty', 'sams', 'empire', 'metal', 'sbli', 'quicken', 'ndr']) if (lc.includes(k)) return k;
  }
  return last;
};
// Wave key: find W<n> ANYWHERE in the name ("W4-LIB2-TIGHT" -> "W4"); SPICY if present; else a
// single "OTHER" bucket — never one row per arbitrary routing token (no per-token explosion).
const waveOf = (name: string): string => {
  const up = (name || '').toUpperCase();
  const m = up.match(/\bW(\d+)/);
  if (m) return `W${m[1]}`;
  if (up.includes('SPICY')) return 'SPICY';
  return 'OTHER';
};
const waveRank = (w: string): number => {
  const m = w.match(/^W(\d+)$/);
  if (m) return Number(m[1]);
  if (w === 'SPICY') return 90;
  return 99;
};

const cellStatus = (s: Set<string>): string => {
  for (const k of ['failed', 'sending', 'preparing', 'finalizing_audience', 'scheduled', 'sent', 'completed', 'draft', 'cancelled']) {
    if (s.has(k)) return k;
  }
  return [...s][0] ?? 'unknown';
};

interface BrandWaveGridProps {
  date: string;               // controlled MT send-date (yyyy-mm-dd)
  excludeDrip?: boolean;      // default true (planner view)
}

export const BrandWaveGrid: React.FC<BrandWaveGridProps> = ({ date, excludeDrip = true }) => {
  const { organization } = useAuth();
  const orgId = organization?.id ?? DEFAULT_ORG;
  const headers = useMemo(() => ({ 'X-Organization-ID': orgId }), [orgId]);

  const [rows, setRows] = useState<CampaignRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true); setError(null);
    try {
      const qs = `&scheduled_date=${date}` + (excludeDrip ? '&exclude_drip=true' : '');
      const PAGE = 200;
      const acc: CampaignRow[] = [];
      const seen = new Set<string>();   // dedupe by id — a backend that ignores `page` would
      for (let page = 1; page <= 30; page++) {   // otherwise re-add page 1 and inflate volumes
        const r = await fetch(`/api/mailing/campaigns?limit=${PAGE}&page=${page}${qs}`, { headers });
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        const j = await r.json();
        const items: CampaignRow[] = Array.isArray(j) ? j : (j.data ?? j.campaigns ?? j.items ?? []);
        let added = 0;
        for (const it of items) {
          if (it.id && !seen.has(it.id)) { seen.add(it.id); acc.push(it); added++; }
        }
        const pg = j.pagination;
        if (items.length < PAGE || added === 0 || (pg && pg.has_more === false) || (pg && page >= pg.total_pages)) break;
      }
      setRows(acc);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load plan');
      setRows([]);
    } finally {
      setLoading(false);
    }
  }, [headers, date, excludeDrip]);

  useEffect(() => { void load(); }, [load]);

  // Build the (domain × wave) matrix. Brand IDENTITY = the authoritative from_email DOMAIN
  // (so two domains can never collapse into one column, and a stray dash in a label can't
  // mis-bucket). Cancelled campaigns are excluded from the plan view.
  const { brands, waves, matrix, totalsByBrand } = useMemo(() => {
    const active = rows.filter(r => r.status !== 'cancelled');
    const mtx: Record<string, Record<string, Cell>> = {};   // wave -> domain -> cell
    const labelOfDomain = new Map<string, string>();        // domain -> display label
    const waveSet = new Set<string>();
    const totals: Record<string, number> = {};
    for (const r of active) {
      const domain = domainOf(r.from_email);
      const wave = waveOf(r.name);
      if (!labelOfDomain.has(domain)) labelOfDomain.set(domain, brandLabelOf(r.name) || labelFromDomain(domain));
      waveSet.add(wave);
      const row = (mtx[wave] ??= {});
      const cell = (row[domain] ??= { offer: offerOf(r.name), volume: 0, count: 0, statuses: new Set(), firstScheduled: r.scheduled_at });
      cell.volume += r.total_recipients || 0;
      cell.count += 1;
      cell.statuses.add(r.status);
      if (r.scheduled_at && (!cell.firstScheduled || new Date(r.scheduled_at).getTime() < new Date(cell.firstScheduled).getTime())) {
        cell.firstScheduled = r.scheduled_at;
      }
      totals[domain] = (totals[domain] || 0) + (r.total_recipients || 0);
    }
    const domainList = [...labelOfDomain.keys()].sort((a, b) => (totals[b] || 0) - (totals[a] || 0)); // biggest first
    const waveList = [...waveSet].sort((a, b) => waveRank(a) - waveRank(b) || a.localeCompare(b));
    return {
      brands: domainList.map(domain => ({ domain, label: labelOfDomain.get(domain) ?? labelFromDomain(domain) })),
      waves: waveList, matrix: mtx, totalsByBrand: totals,
    };
  }, [rows]);

  const grandTotal = useMemo(() => Object.values(totalsByBrand).reduce((s, n) => s + n, 0), [totalsByBrand]);

  if (loading) return <div style={msg}>Loading the real plan for {date}…</div>;
  if (error) return <div style={{ ...msg, color: '#e94560' }}>Failed to load plan: {error}</div>;
  if (!brands.length) return <div style={msg}>No campaigns scheduled for {date}. (Nothing staged yet for this date.)</div>;

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 12, marginBottom: 8, flexWrap: 'wrap' }}>
        <span style={{ fontSize: 13, fontWeight: 700, color: 'rgba(220,235,250,0.92)' }}>
          {brands.length} brands · {waves.length} waves · {num(grandTotal)} planned
        </span>
        <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>← scroll horizontally for all brands →</span>
      </div>
      <div style={{ overflowX: 'auto', border: '1px solid rgba(0,200,255,0.12)', borderRadius: 8, background: 'rgba(13,21,38,0.45)' }}>
        <table style={{ borderCollapse: 'separate', borderSpacing: 0, minWidth: '100%' }}>
          <thead>
            <tr>
              <th style={{ ...thBase, ...stickyLeft, textAlign: 'left' }}>Wave</th>
              {brands.map(b => (
                <th key={b.domain} style={thBase}>
                  <div style={{ fontWeight: 700, color: '#00e5ff', fontSize: 12 }}>{b.label}</div>
                  <div style={{ fontSize: 9, color: 'rgba(180,210,240,0.5)' }}>{b.domain}</div>
                  <div style={{ fontSize: 9, color: 'rgba(180,210,240,0.7)' }}>{num(totalsByBrand[b.domain] || 0)}</div>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {waves.map(w => {
              // representative time for the wave row label = earliest scheduled in that wave
              const times = brands.map(b => matrix[w]?.[b.domain]?.firstScheduled).filter(Boolean) as string[];
              const repTime = times.sort((a, b) => new Date(a).getTime() - new Date(b).getTime())[0] ?? null;
              return (
                <tr key={w}>
                  <td style={{ ...tdBase, ...stickyLeft }}>
                    <div style={{ fontWeight: 700, fontSize: 12, color: 'rgba(220,235,250,0.92)' }}>{w}</div>
                    <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.6)' }}>{mtTime(repTime)} MT</div>
                  </td>
                  {brands.map(b => {
                    const cell = matrix[w]?.[b.domain];
                    if (!cell) return <td key={b.domain} style={{ ...tdBase, color: 'rgba(180,210,240,0.25)', textAlign: 'center' }}>—</td>;
                    const st = cellStatus(cell.statuses);
                    return (
                      <td key={b.domain} style={tdBase}>
                        <div style={{ fontSize: 11, color: 'rgba(220,235,250,0.9)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', maxWidth: 130 }}>{cell.offer}</div>
                        <div style={{ fontSize: 13, fontWeight: 700, color: '#cbd5f5' }}>{num(cell.volume)}</div>
                        <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                          <span style={{ fontSize: 9, fontWeight: 700, color: STATUS_FG[st] ?? '#9ca3af', textTransform: 'uppercase' }}>{st.replace(/_/g, ' ')}</span>
                          {cell.count > 1 && <span style={{ fontSize: 9, color: 'rgba(180,210,240,0.5)' }}>×{cell.count}</span>}
                        </div>
                      </td>
                    );
                  })}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
};

export default BrandWaveGrid;

const msg: React.CSSProperties = { padding: 18, fontSize: 13, color: 'rgba(180,210,240,0.75)' };
const thBase: React.CSSProperties = {
  padding: '8px 12px', borderBottom: '1px solid rgba(0,200,255,0.2)', borderRight: '1px solid rgba(0,200,255,0.06)',
  background: 'rgba(13,21,38,0.95)', minWidth: 130, verticalAlign: 'top',
};
const tdBase: React.CSSProperties = {
  padding: '8px 12px', borderBottom: '1px solid rgba(0,200,255,0.06)', borderRight: '1px solid rgba(0,200,255,0.06)',
  verticalAlign: 'top', minWidth: 130,
};
const stickyLeft: React.CSSProperties = {
  position: 'sticky', left: 0, zIndex: 1, background: 'rgba(10,16,30,0.98)',
  borderRight: '1px solid rgba(0,200,255,0.25)', minWidth: 90,
};
