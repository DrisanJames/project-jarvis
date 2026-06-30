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
import { SideShelf } from './shared/SideShelf';
import { colors, thStyle, tdStyle, numTd, numTh, tableStyle } from '../shared/theme';
import { SectionHeader, SectionError, EmptyState, Pill } from '../shared/ui';

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
  ids: string[];            // campaign ids in this brand×wave (for the per-ISP drill-down)
}

const STATUS_FG: Record<string, string> = {
  draft: '#9ca3af', scheduled: '#00b0ff', preparing: '#facc15',
  finalizing_audience: '#facc15', sending: '#c084fc', sent: '#00b894',
  completed: '#00b894', failed: '#e94560', cancelled: '#bdbdbd',
};

// Logical ISP display order + labels (canonical, matches the Draft Board).
const ISP_ORDER: string[] = ['microsoft', 'gmail', 'apple', 'yahoo', 'comcast', 'charter', 'att', 'aol', 'cox', 'sbcglobal', 'verizon', 'other'];
const ISP_LABEL: Record<string, string> = {
  microsoft: 'Microsoft', gmail: 'Gmail', apple: 'Apple', yahoo: 'Yahoo', comcast: 'Comcast',
  charter: 'Charter', att: 'AT&T', aol: 'AOL', cox: 'Cox', sbcglobal: 'SBCGlobal', verizon: 'Verizon', other: 'Other',
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

  // Brand×wave drill-down: clicking a volume cell opens a SideShelf showing
  //  (a) ISP COMPOSITION — summed isp_quotas across the cell's split campaigns,
  //      read from each campaign's pmta_config.campaign_input.isp_quotas
  //      ([{isp, volume}]) via the existing edit-data endpoint; and
  //  (b) SEGMENTS USED — the audience (segment/list ids) the campaigns target,
  //      with human labels pulled from GET /api/mailing/campaigns/{id}
  //      (segment_name / list_name), falling back to ids where no name exists.
  type Sel = { wave: string; domain: string; label: string; offer: string; volume: number; ids: string[] };
  interface Audience { id: string; name?: string }
  interface Drill { isp: Record<string, number>; targeted: string[]; segments: Audience[]; lists: Audience[] }
  type DrillState = 'loading' | 'error' | Drill;
  const [selected, setSelected] = useState<Sel | null>(null);
  const [drill, setDrill] = useState<Record<string, DrillState>>({});
  useEffect(() => { setSelected(null); }, [date]);   // clear the drill-down when the date changes

  const openCell = useCallback(async (sel: Sel) => {
    setSelected(sel);
    const key = `${sel.wave}|${sel.domain}`;
    const cached = drill[key];
    if (cached && cached !== 'error') return;          // already loaded — just reopen the shelf
    setDrill(prev => ({ ...prev, [key]: 'loading' }));
    try {
      const isp: Record<string, number> = {};
      const targeted = new Set<string>();
      const segMap = new Map<string, Audience>();
      const listMap = new Map<string, Audience>();
      const add = (m: Map<string, Audience>, id: unknown, name?: unknown) => {
        const sid = id == null ? '' : String(id).trim();
        if (!sid) return;
        const nm = typeof name === 'string' && name.trim() ? name.trim() : undefined;
        const prev = m.get(sid);
        m.set(sid, { id: sid, name: nm ?? prev?.name });
      };
      for (const id of sel.ids) {
        // (a) ISP composition — pmta_config.campaign_input.isp_quotas (edit-data).
        const re = await fetch(`/api/mailing/pmta-campaign/${id}/edit-data`, { headers });
        if (re.ok) {
          const je = await re.json();
          const ci = je.campaign_input ?? je;
          for (const q of (ci.isp_quotas ?? [])) if (q && q.isp) isp[q.isp] = (isp[q.isp] || 0) + (q.volume || 0);
          for (const t of (ci.target_isps ?? [])) {
            const nm = typeof t === 'string' ? t : (t?.name ?? t?.isp);
            if (nm) targeted.add(String(nm).toLowerCase());
          }
          for (const s of (ci.inclusion_segments ?? [])) add(segMap, s);
          for (const l of (ci.inclusion_lists ?? [])) add(listMap, l);
        }
        // (b) Human audience labels — GET /api/mailing/campaigns/{id}.
        const rc = await fetch(`/api/mailing/campaigns/${id}`, { headers });
        if (rc.ok) {
          const jc = await rc.json();
          const sids: unknown[] = Array.isArray(jc.segment_ids) ? jc.segment_ids : (jc.segment_id ? [jc.segment_id] : []);
          sids.forEach((s, i) => add(segMap, s, i === 0 ? jc.segment_name : undefined));
          const lids: unknown[] = Array.isArray(jc.list_ids) ? jc.list_ids : (jc.list_id ? [jc.list_id] : []);
          lids.forEach((l, i) => add(listMap, l, i === 0 ? jc.list_name : undefined));
        }
      }
      setDrill(prev => ({ ...prev, [key]: { isp, targeted: [...targeted], segments: [...segMap.values()], lists: [...listMap.values()] } }));
    } catch {
      setDrill(prev => ({ ...prev, [key]: 'error' }));
    }
  }, [headers, drill]);

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
      const cell = (row[domain] ??= { offer: offerOf(r.name), volume: 0, count: 0, statuses: new Set(), firstScheduled: r.scheduled_at, ids: [] });
      cell.volume += r.total_recipients || 0;
      cell.count += 1;
      if (r.id) cell.ids.push(r.id);
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

  if (loading) return <div style={msg}>Loading the plan for {date}…</div>;
  if (error) return <div style={{ ...msg, color: '#e94560' }}>Failed to load plan: {error}</div>;
  if (!brands.length) return <div style={msg}>No campaigns scheduled for {date}. (Nothing staged yet for this date.)</div>;

  return (
    <div>
      <style>{`.bwg-cell{transition:background 120ms ease} .bwg-cell:hover{background:rgba(99,102,241,0.12)}`}</style>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 12, marginBottom: 8, flexWrap: 'wrap' }}>
        <span style={{ fontSize: 13, fontWeight: 700, color: 'rgba(220,235,250,0.92)' }}>
          {brands.length} brands · {waves.length} send batches · {num(grandTotal)} planned
        </span>
        <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>← scroll horizontally for all brands →</span>
      </div>
      <div style={{ overflowX: 'auto', border: '1px solid rgba(0,200,255,0.12)', borderRadius: 8, background: 'rgba(13,21,38,0.45)' }}>
        <table style={{ borderCollapse: 'separate', borderSpacing: 0, minWidth: '100%' }}>
          <thead>
            <tr>
              <th style={{ ...thBase, ...stickyLeft, textAlign: 'left' }}>Send batch</th>
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
                    const isSel = selected?.wave === w && selected?.domain === b.domain;
                    const st = cellStatus(cell.statuses);
                    const clickable = cell.volume > 0;
                    // Cell shows just the DECLARED VOLUME (+ offer); click to drill into the
                    // per-ISP composition + segments. Zero-volume cells aren't clickable.
                    const inner = (
                      <>
                        <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.7)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', maxWidth: 120 }}>{cell.offer}</div>
                        <div style={{ fontSize: 15, fontWeight: 700, color: STATUS_FG[st] ?? '#cbd5f5' }}>{num(cell.volume)}</div>
                      </>
                    );
                    return (
                      <td key={b.domain} style={{ ...tdBase, ...(isSel ? { background: 'rgba(0,229,255,0.08)', outline: '1px solid rgba(0,229,255,0.5)' } : {}) }}>
                        {clickable ? (
                          <button
                            className="bwg-cell"
                            onClick={() => openCell({ wave: w, domain: b.domain, label: b.label, offer: cell.offer, volume: cell.volume, ids: cell.ids })}
                            title="Click for ISP composition + segments"
                            style={{ all: 'unset', cursor: 'pointer', display: 'block', width: '100%', borderRadius: 4 }}
                          >
                            {inner}
                          </button>
                        ) : (
                          <div style={{ display: 'block', width: '100%' }}>{inner}</div>
                        )}
                      </td>
                    );
                  })}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {selected && (() => {
        const key = `${selected.wave}|${selected.domain}`;
        const d = drill[key];
        return (
          <SideShelf
            onClose={() => setSelected(null)}
            title={
              <span>
                <span style={{ color: colors.indigo300 }}>{selected.label}</span>
                {' · '}{selected.wave}{' · '}{selected.offer}
              </span>
            }
          >
            <div style={{ padding: 18, display: 'flex', flexDirection: 'column', gap: 22 }}>
              <div style={{ fontSize: 12, color: colors.textMuted }}>
                <b style={{ color: colors.text, fontSize: 15, fontVariantNumeric: 'tabular-nums' }}>{num(selected.volume)}</b> declared
                {' · '}{selected.ids.length} split campaign{selected.ids.length === 1 ? '' : 's'}
              </div>

              {/* ISP COMPOSITION */}
              <div>
                <SectionHeader title="ISP Composition" />
                {d === 'loading' ? (
                  <EmptyState title="Loading volumes by mailbox provider…" />
                ) : d === 'error' ? (
                  <SectionError label="ISP composition" />
                ) : (() => {
                  const rows = Object.entries(d.isp)
                    .filter(([, v]) => v > 0)
                    .sort((a, b) => b[1] - a[1] || ISP_ORDER.indexOf(a[0]) - ISP_ORDER.indexOf(b[0]));
                  const total = rows.reduce((s, [, v]) => s + v, 0);
                  if (total === 0) {
                    const t = d.targeted.map(i => ISP_LABEL[i] ?? i).join(', ');
                    return <EmptyState title="Uncapped (audience-bound)" hint={t ? `Targets ${t}` : 'No per-provider quotas declared'} />;
                  }
                  return (
                    <table style={tableStyle}>
                      <thead>
                        <tr>
                          <th style={thStyle}>Mailbox Provider</th>
                          <th style={numTh}>Volume</th>
                          <th style={numTh}>Share</th>
                        </tr>
                      </thead>
                      <tbody>
                        {rows.map(([isp, vol]) => (
                          <tr key={isp}>
                            <td style={{ ...tdStyle, fontWeight: 600, color: colors.heading }}>{ISP_LABEL[isp] ?? isp}</td>
                            <td style={numTd}>{num(vol)}</td>
                            <td style={numTd}>{((vol / total) * 100).toFixed(1)}%</td>
                          </tr>
                        ))}
                      </tbody>
                      <tfoot>
                        <tr>
                          <td style={{ ...tdStyle, fontWeight: 700, color: colors.indigo200 }}>Σ by provider</td>
                          <td style={{ ...numTd, fontWeight: 700, color: colors.indigo200 }}>{num(total)}</td>
                          <td style={numTd}>100%</td>
                        </tr>
                      </tfoot>
                    </table>
                  );
                })()}
              </div>

              {/* SEGMENTS USED */}
              <div>
                <SectionHeader title="Segments Used" />
                {d === 'loading' ? (
                  <EmptyState title="Loading audience…" />
                ) : d === 'error' ? (
                  <SectionError label="Segments" />
                ) : (d.segments.length === 0 && d.lists.length === 0) ? (
                  <EmptyState title="No segments or lists referenced" hint="These campaigns expose no audience ids." />
                ) : (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
                    {d.segments.length > 0 && (
                      <div>
                        <div style={shelfLabel}>Segments</div>
                        <div style={pillWrap}>
                          {d.segments.map(s => (
                            <Pill key={s.id} color={colors.indigo400} style={{ textTransform: 'none' }}>
                              <span title={s.id}>{s.name ?? shortId(s.id)}</span>
                            </Pill>
                          ))}
                        </div>
                      </div>
                    )}
                    {d.lists.length > 0 && (
                      <div>
                        <div style={shelfLabel}>Inclusion lists</div>
                        <div style={pillWrap}>
                          {d.lists.map(l => (
                            <Pill key={l.id} color={colors.indigo300} style={{ textTransform: 'none' }}>
                              <span title={l.id}>{l.name ?? shortId(l.id)}</span>
                            </Pill>
                          ))}
                        </div>
                      </div>
                    )}
                    <div style={{ fontSize: 10, color: colors.textFaint }}>
                      Labels from campaign detail (segment_name / list_name); a short id is shown where a name isn&apos;t exposed.
                    </div>
                  </div>
                )}
              </div>
            </div>
          </SideShelf>
        );
      })()}
    </div>
  );
};

export default BrandWaveGrid;

const shortId = (id: string): string => (id.length > 12 ? `${id.slice(0, 8)}…` : id);
const shelfLabel: React.CSSProperties = {
  fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.5, color: colors.textMuted, marginBottom: 7,
};
const pillWrap: React.CSSProperties = { display: 'flex', flexWrap: 'wrap', gap: 8 };

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
