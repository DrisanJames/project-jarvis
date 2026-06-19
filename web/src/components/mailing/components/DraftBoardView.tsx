// Draft Board — sending-domain-centric view of "what we're mailing".
//
// The mental model (operator spec, 2026-06-19): everything stems from the
// SENDING DOMAIN. A domain encapsulates the campaigns it is mailing, each tied
// to a send time (MT). Per campaign we surface creative · subject · preheader,
// and a breakdown by our logical ISPs of the volume target / cap — i.e. volume
// targets by ISP, by sending domain. Pure read view over existing endpoints:
//   GET /api/mailing/campaigns?status=&limit=          (roster + time + audience)
//   GET /api/mailing/pmta-campaign/{id}/edit-data       (per-ISP quotas + creative)
//
// No new backend, no draft persistence — it reads whatever is staged/scheduled
// for the selected day and organises it top-down.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { useAuth } from '../../../contexts/AuthContext';
import { ALL_TARGET_ISPS } from './send-day-planner/constants';

const DEFAULT_ORG = '00000000-0000-0000-0000-000000000001';

// Logical ISP display labels (canonical order from the send-day planner).
const ISP_LABEL: Record<string, string> = {
  microsoft: 'Microsoft', gmail: 'Gmail', apple: 'Apple', yahoo: 'Yahoo',
  comcast: 'Comcast', charter: 'Charter', att: 'AT&T', aol: 'AOL',
  cox: 'Cox', sbcglobal: 'SBCGlobal', other: 'Other',
};
const ISP_ORDER: string[] = [...ALL_TARGET_ISPS];

interface CampaignRow {
  id: string;
  name: string;
  subject: string;
  status: string;
  total_recipients: number;
  from_email: string;
  from_name: string;
  scheduled_at: string | null;
  created_at: string;
  preview_text: string;
  profile_name?: string;
  segment_names?: string[];
}

interface IspQuota { isp: string; volume: number; }
interface Variant { subject?: string; preview_text?: string; from_name?: string; html_content?: string; }
interface EditData {
  sending_domain?: string;
  target_isps?: string[];
  isp_quotas?: IspQuota[];
  variants?: Variant[];
}

const STATUS_PALETTE: Record<string, { bg: string; bd: string; fg: string }> = {
  draft:      { bg: 'rgba(120,120,120,0.12)', bd: 'rgba(120,120,120,0.4)', fg: '#9ca3af' },
  scheduled:  { bg: 'rgba(0,176,255,0.12)',   bd: 'rgba(0,176,255,0.45)', fg: '#00b0ff' },
  preparing:  { bg: 'rgba(250,204,21,0.12)',  bd: 'rgba(250,204,21,0.45)', fg: '#facc15' },
  finalizing_audience: { bg: 'rgba(250,204,21,0.12)', bd: 'rgba(250,204,21,0.45)', fg: '#facc15' },
  sending:    { bg: 'rgba(168,85,247,0.14)',  bd: 'rgba(168,85,247,0.5)',  fg: '#c084fc' },
  sent:       { bg: 'rgba(0,184,148,0.12)',   bd: 'rgba(0,184,148,0.45)', fg: '#00b894' },
  completed:  { bg: 'rgba(0,184,148,0.12)',   bd: 'rgba(0,184,148,0.45)', fg: '#00b894' },
  failed:     { bg: 'rgba(233,69,96,0.12)',   bd: 'rgba(233,69,96,0.5)',  fg: '#e94560' },
  cancelled:  { bg: 'rgba(180,180,180,0.10)', bd: 'rgba(180,180,180,0.4)', fg: '#bdbdbd' },
};

const StatusPill: React.FC<{ status: string }> = ({ status }) => {
  const p = STATUS_PALETTE[status] ?? { bg: 'rgba(120,120,120,0.12)', bd: 'rgba(120,120,120,0.4)', fg: '#9ca3af' };
  return (
    <span style={{
      display: 'inline-block', padding: '2px 8px', borderRadius: 999,
      background: p.bg, border: `1px solid ${p.bd}`, color: p.fg,
      fontSize: 10, fontWeight: 700, letterSpacing: 0.5, whiteSpace: 'nowrap',
    }}>{(status || 'unknown').toUpperCase().replace(/_/g, ' ')}</span>
  );
};

const TZ = 'America/Denver';
const domainOf = (email: string): string => (email && email.includes('@') ? email.split('@')[1] : '(no domain)').toLowerCase();
const mtDate = (iso: string | null): string => {
  if (!iso) return '';
  try { return new Date(iso).toLocaleDateString('en-CA', { timeZone: TZ }); } catch { return ''; }
};
const mtTime = (iso: string | null): string => {
  if (!iso) return '—';
  try { return new Date(iso).toLocaleString('en-US', { timeZone: TZ, hour: 'numeric', minute: '2-digit', hour12: true }); }
  catch { return '—'; }
};
const num = (n: number): string => (n || 0).toLocaleString();

const STATUS_OPTIONS = ['all', 'draft', 'scheduled', 'sending', 'sent', 'completed', 'cancelled', 'failed'];

export const DraftBoardView: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id ?? DEFAULT_ORG;

  const todayMT = useMemo(() => {
    try { return new Date().toLocaleDateString('en-CA', { timeZone: TZ }); } catch { return ''; }
  }, []);

  const [date, setDate] = useState<string>(todayMT);
  const [statusFilter, setStatusFilter] = useState<string>('all');
  const [includeDrip, setIncludeDrip] = useState<boolean>(false);
  const [rows, setRows] = useState<CampaignRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [details, setDetails] = useState<Record<string, EditData | 'loading' | 'error'>>({});

  const headers = useMemo(() => ({ 'X-Organization-ID': orgId }), [orgId]);

  const load = useCallback(async () => {
    setLoading(true); setError(null);
    try {
      // Server-side filter to the selected MT send date + drip exclusion, so we
      // fetch exactly the day's plan rather than paging the whole campaign table.
      const qs = `&scheduled_date=${date}`
        + (statusFilter === 'all' ? '' : `&status=${encodeURIComponent(statusFilter)}`)
        + (includeDrip ? '' : '&exclude_drip=true');
      // The endpoint paginates by `page` (200/page). Walk pages until the server
      // says there are no more (a day can exceed 200 with ISP-split campaigns).
      const PAGE = 200;
      const acc: CampaignRow[] = [];
      for (let page = 1; page <= 30; page++) {
        const r = await fetch(`/api/mailing/campaigns?limit=${PAGE}&page=${page}${qs}`, { headers });
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        const j = await r.json();
        const items: CampaignRow[] = Array.isArray(j) ? j : (j.data ?? j.campaigns ?? j.items ?? []);
        acc.push(...items);
        const pg = j.pagination;
        if (items.length < PAGE || (pg && pg.has_more === false) || (pg && page >= pg.total_pages)) break;
      }
      setRows(acc);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load campaigns');
      setRows([]);
    } finally {
      setLoading(false);
    }
  }, [headers, statusFilter, includeDrip, date]);

  useEffect(() => { void load(); }, [load]);

  const fetchDetail = useCallback(async (id: string) => {
    setDetails(prev => (prev[id] && prev[id] !== 'error' ? prev : { ...prev, [id]: 'loading' }));
    try {
      const r = await fetch(`/api/mailing/pmta-campaign/${id}/edit-data`, { headers });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const j = await r.json();
      const ci: EditData = j.campaign_input ?? j;
      setDetails(prev => ({ ...prev, [id]: ci }));
    } catch {
      setDetails(prev => ({ ...prev, [id]: 'error' }));
    }
  }, [headers]);

  const toggle = useCallback((id: string) => {
    setExpanded(prev => {
      const next = new Set(prev);
      if (next.has(id)) { next.delete(id); }
      else { next.add(id); if (!details[id] || details[id] === 'error') void fetchDetail(id); }
      return next;
    });
  }, [details, fetchDetail]);

  // Filter to the selected MT day, then group by sending domain.
  // Bucket by scheduled_at ONLY — that's the campaign's actual MST send time.
  // Unscheduled campaigns (no scheduled_at) aren't "being mailed at a time",
  // so they don't belong under a send date (using created_at would pull in
  // other days' drafts created today).
  const grouped = useMemo(() => {
    const inDay = rows.filter(c => mtDate(c.scheduled_at) === date);
    const byDomain = new Map<string, CampaignRow[]>();
    for (const c of inDay) {
      const d = domainOf(c.from_email);
      if (!byDomain.has(d)) byDomain.set(d, []);
      byDomain.get(d)!.push(c);
    }
    const out = Array.from(byDomain.entries()).map(([domain, camps]) => ({
      domain,
      campaigns: camps.slice().sort((a, b) =>
        (a.scheduled_at || a.created_at).localeCompare(b.scheduled_at || b.created_at)),
      audience: camps.reduce((s, c) => s + (c.total_recipients || 0), 0),
    }));
    out.sort((a, b) => a.domain.localeCompare(b.domain));
    return out;
  }, [rows, date]);

  const totals = useMemo(() => ({
    domains: grouped.length,
    campaigns: grouped.reduce((s, g) => s + g.campaigns.length, 0),
    audience: grouped.reduce((s, g) => s + g.audience, 0),
  }), [grouped]);

  const previewCreative = useCallback((html?: string) => {
    if (!html) return;
    const blob = new Blob([html], { type: 'text/html' });
    const url = URL.createObjectURL(blob);
    window.open(url, '_blank', 'noopener');
    setTimeout(() => URL.revokeObjectURL(url), 60_000);
  }, []);

  return (
    <div style={{ padding: 18, color: 'rgba(220,235,250,0.92)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 14, flexWrap: 'wrap', marginBottom: 14 }}>
        <h2 style={{ margin: 0, fontSize: 18, fontWeight: 700 }}>Draft Board · by Sending Domain</h2>
        <input
          type="date" value={date} onChange={e => setDate(e.target.value)}
          style={{
            background: 'rgba(13,21,38,0.9)', color: 'rgba(220,235,250,0.95)',
            border: '1px solid rgba(0,200,255,0.25)', borderRadius: 6, padding: '6px 10px', fontSize: 13,
          }}
        />
        <select
          value={statusFilter} onChange={e => setStatusFilter(e.target.value)}
          style={{
            background: 'rgba(13,21,38,0.9)', color: 'rgba(220,235,250,0.95)',
            border: '1px solid rgba(0,200,255,0.25)', borderRadius: 6, padding: '6px 10px', fontSize: 13,
          }}
        >
          {STATUS_OPTIONS.map(s => <option key={s} value={s}>{s === 'all' ? 'All statuses' : s}</option>)}
        </select>
        <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, color: 'rgba(180,210,240,0.75)', cursor: 'pointer' }}>
          <input type="checkbox" checked={includeDrip} onChange={e => setIncludeDrip(e.target.checked)} />
          Include partner-drip
        </label>
        <button onClick={() => void load()} style={ghostBtn}>{loading ? 'Loading…' : 'Refresh'}</button>
        <div style={{ marginLeft: 'auto', fontSize: 12, color: 'rgba(180,210,240,0.75)' }}>
          {totals.domains} domains · {totals.campaigns} campaigns · <span style={{ color: '#00e5ff', fontWeight: 600 }}>{num(totals.audience)}</span> planned sends
        </div>
      </div>

      {error && (
        <div style={{ padding: 12, borderRadius: 8, background: 'rgba(233,69,96,0.08)', border: '1px solid rgba(233,69,96,0.4)', color: '#e94560', fontSize: 13, marginBottom: 12 }}>
          {error}
        </div>
      )}

      {!loading && !error && grouped.length === 0 && (
        <div style={{ padding: 24, textAlign: 'center', color: 'rgba(180,210,240,0.55)', fontSize: 13 }}>
          No campaigns for {date} (MT){statusFilter !== 'all' ? ` with status "${statusFilter}"` : ''}.
        </div>
      )}

      <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
        {grouped.map(group => (
          <div key={group.domain} style={{
            background: 'rgba(13,21,38,0.7)', border: '1px solid rgba(0,200,255,0.15)', borderRadius: 10, overflow: 'hidden',
          }}>
            {/* Domain header */}
            <div style={{
              display: 'flex', alignItems: 'center', gap: 12, padding: '10px 14px',
              background: 'linear-gradient(135deg, rgba(99,102,241,0.16), rgba(139,92,246,0.12))',
              borderBottom: '1px solid rgba(0,200,255,0.12)',
            }}>
              <span style={{ fontSize: 15, fontWeight: 700, color: 'rgba(225,235,250,0.96)' }}>{group.domain}</span>
              <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)' }}>{group.campaigns.length} campaign{group.campaigns.length === 1 ? '' : 's'}</span>
              <span style={{ marginLeft: 'auto', fontSize: 12, color: '#00e5ff', fontWeight: 600 }}>{num(group.audience)} planned sends</span>
            </div>

            {/* Campaign rows */}
            <div style={{ display: 'flex', flexDirection: 'column' }}>
              {group.campaigns.map(c => {
                const isOpen = expanded.has(c.id);
                const det = details[c.id];
                return (
                  <div key={c.id} style={{ borderTop: '1px solid rgba(255,255,255,0.04)' }}>
                    <button
                      onClick={() => toggle(c.id)}
                      style={{
                        width: '100%', display: 'flex', alignItems: 'center', gap: 12, padding: '10px 14px',
                        background: isOpen ? 'rgba(0,229,255,0.04)' : 'transparent', border: 'none', cursor: 'pointer',
                        textAlign: 'left', color: 'inherit',
                      }}
                    >
                      <span style={{ width: 86, fontSize: 12, fontWeight: 600, color: 'rgba(200,225,250,0.9)' }}>
                        {mtTime(c.scheduled_at || c.created_at)} <span style={{ color: 'rgba(180,210,240,0.5)' }}>MT</span>
                      </span>
                      <StatusPill status={c.status} />
                      <span style={{ flex: 1, minWidth: 0 }}>
                        <span style={{ display: 'block', fontSize: 13, fontWeight: 600, color: 'rgba(220,235,250,0.95)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                          {c.subject || '(no subject)'}
                        </span>
                        <span style={{ display: 'block', fontSize: 11, color: 'rgba(180,210,240,0.5)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                          {c.name}
                        </span>
                      </span>
                      <span style={{ fontSize: 13, fontWeight: 600, color: '#00e5ff', whiteSpace: 'nowrap' }}>{num(c.total_recipients)}</span>
                      <span style={{ fontSize: 12, color: 'rgba(180,210,240,0.6)', width: 14, textAlign: 'center' }}>{isOpen ? '▾' : '▸'}</span>
                    </button>

                    {isOpen && (
                      <div style={{ padding: '4px 14px 16px 100px' }}>
                        {/* Creative · subject · preheader */}
                        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 18, marginBottom: 12, fontSize: 12 }}>
                          <Field label="Subject" value={det && det !== 'loading' && det !== 'error' ? (det.variants?.[0]?.subject || c.subject) : c.subject} />
                          <Field label="Preheader" value={det && det !== 'loading' && det !== 'error' ? (det.variants?.[0]?.preview_text || c.preview_text || '—') : (c.preview_text || '—')} />
                          <Field label="From" value={`${c.from_name || '—'} <${c.from_email}>`} />
                          {det && det !== 'loading' && det !== 'error' && det.variants?.[0]?.html_content && (
                            <div>
                              <div style={fieldLabel}>Creative</div>
                              <button onClick={() => previewCreative(det.variants?.[0]?.html_content)} style={linkBtn}>
                                Preview HTML ({Math.round((det.variants[0].html_content!.length) / 1024)} KB) ↗
                              </button>
                            </div>
                          )}
                        </div>

                        {/* Per-ISP volume targets / caps */}
                        <div style={fieldLabel}>Volume targets by ISP</div>
                        {det === 'loading' && <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.55)' }}>Loading ISP plan…</div>}
                        {det === 'error' && <div style={{ fontSize: 12, color: '#e94560' }}>Could not load ISP plan (edit-data unavailable for this campaign).</div>}
                        {det && det !== 'loading' && det !== 'error' && (
                          <IspTable det={det} totalAudience={c.total_recipients} />
                        )}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          </div>
        ))}
      </div>

      <div style={{ marginTop: 18, fontSize: 10, color: 'rgba(180,210,240,0.35)' }}>
        Draft Board v1.1 · domain → campaign(MT) → per-ISP volume targets · "planned sends" = cumulative recipients across a domain's campaigns/waves (a member in multiple waves counts each time, so it exceeds unique reach) · times in America/Denver (MT)
      </div>
    </div>
  );
};

const IspTable: React.FC<{ det: EditData; totalAudience: number }> = ({ det, totalAudience }) => {
  const quotaByIsp = new Map<string, number>();
  (det.isp_quotas || []).forEach(q => quotaByIsp.set(q.isp, q.volume));
  // The ISPs this campaign actually targets, in canonical order.
  const targeted = (det.target_isps && det.target_isps.length ? det.target_isps : (det.isp_quotas || []).map(q => q.isp));
  const ordered = ISP_ORDER.filter(i => targeted.includes(i)).concat(targeted.filter(i => !ISP_ORDER.includes(i)));
  const anyCap = (det.isp_quotas || []).some(q => (q.volume || 0) > 0);

  return (
    <table style={{ borderCollapse: 'collapse', fontSize: 12, marginTop: 6, minWidth: 320 }}>
      <thead>
        <tr style={{ color: 'rgba(180,210,240,0.6)', textAlign: 'left' }}>
          <th style={thCell}>ISP</th>
          <th style={{ ...thCell, textAlign: 'right' }}>{anyCap ? 'Volume target / cap' : 'Cap'}</th>
        </tr>
      </thead>
      <tbody>
        {ordered.map(isp => {
          const v = quotaByIsp.get(isp) ?? 0;
          return (
            <tr key={isp} style={{ borderTop: '1px solid rgba(255,255,255,0.05)' }}>
              <td style={tdCell}>{ISP_LABEL[isp] || isp}</td>
              <td style={{ ...tdCell, textAlign: 'right', color: v > 0 ? '#00e5ff' : 'rgba(180,210,240,0.55)', fontWeight: v > 0 ? 600 : 400 }}>
                {v > 0 ? num(v) : 'Uncapped'}
              </td>
            </tr>
          );
        })}
        <tr style={{ borderTop: '1px solid rgba(0,200,255,0.18)' }}>
          <td style={{ ...tdCell, fontWeight: 700 }}>Audience (planned)</td>
          <td style={{ ...tdCell, textAlign: 'right', color: '#00e5ff', fontWeight: 700 }}>{num(totalAudience)}</td>
        </tr>
      </tbody>
    </table>
  );
};

const Field: React.FC<{ label: string; value: string }> = ({ label, value }) => (
  <div style={{ minWidth: 0, maxWidth: 360 }}>
    <div style={fieldLabel}>{label}</div>
    <div style={{ fontSize: 12, color: 'rgba(220,235,250,0.9)', overflowWrap: 'anywhere' }}>{value}</div>
  </div>
);

const fieldLabel: React.CSSProperties = { fontSize: 10, fontWeight: 700, letterSpacing: 0.6, textTransform: 'uppercase', color: 'rgba(180,210,240,0.5)', marginBottom: 3 };
const thCell: React.CSSProperties = { padding: '4px 14px 4px 0', fontWeight: 600 };
const tdCell: React.CSSProperties = { padding: '4px 14px 4px 0', color: 'rgba(220,235,250,0.9)' };
const ghostBtn: React.CSSProperties = { background: 'rgba(0,176,255,0.12)', color: '#00b0ff', border: '1px solid rgba(0,176,255,0.35)', padding: '6px 12px', borderRadius: 6, fontSize: 12, fontWeight: 600, cursor: 'pointer' };
const linkBtn: React.CSSProperties = { background: 'transparent', color: '#00b0ff', border: 'none', padding: 0, fontSize: 12, fontWeight: 600, cursor: 'pointer', textDecoration: 'underline' };

export default DraftBoardView;
