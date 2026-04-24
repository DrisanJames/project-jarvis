import React, { useState, useEffect, useMemo, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBroadcastTower, faSpinner, faSyncAlt,
  faClock, faChartArea, faHeart, faSkullCrossbones,
} from '@fortawesome/free-solid-svg-icons';
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid,
  Tooltip as RechartsTooltip, Legend, ResponsiveContainer,
} from 'recharts';

// HarvestStreamDashboard — always-on Welcome Harvest stream analytics.
//
// Over-built by design: renders 5 panels from a single /harvest-performance
// response so the operator has surgical visibility with ZERO gaps:
//   1. KPI strip       — overall totals and rates
//   2. Engagement vs   — single ratio card, hard-bounce / complaint damage
//      Damage             against unique opens+clicks
//   3. ISP health      — per-ISP table (sent/delivered/hard/soft/unique
//      table             opens/unique clicks/rates, color-coded)
//   4. Hour-of-day     — 24-col heatmap per ISP showing optimal send hours
//      heatmap
//   5. Time-series     — stacked line chart, overall + per-ISP; switches
//      chart             between 1h / 3h / 5h buckets
//
// All data shaping happens in useMemo hooks so Recharts gets stable refs
// and doesn't re-render on every keystroke.

// ─── Types ───────────────────────────────────────────────────────────────────

interface HarvestMetrics {
  sent: number;
  delivered: number;
  hard_bounces: number;
  soft_bounces: number;
  opens: number;
  unique_opens: number;
  clicks: number;
  unique_clicks: number;
  complaints: number;
  unsubs: number;
  deferred: number;
  mpp_opens: number;
  open_rate: number;
  click_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  complaint_rate: number;
  delivery_rate: number;
}

interface ISPRow   { isp: string; display_name: string; metrics: HarvestMetrics; }
interface DomRow   { sending_domain: string; metrics: HarvestMetrics; }
interface TSRow    { ts_utc: string; ts_mst: string; metrics: HarvestMetrics; }
interface HourRow  { hour: number; isp: string; metrics: HarvestMetrics; }
interface CampRow  { campaign_id: string; name: string; sending_domain: string; metrics: HarvestMetrics; }
interface EngVsDmg { engagement: number; damage: number; ratio: number; hard_bounces: number; complaints: number; unique_opens: number; unique_clicks: number; }

export interface HarvestResponse {
  api_version: string;
  campaign_prefix: string;
  window: { start_utc: string; end_utc: string; hours: number; bucket: string; bucket_seconds: number };
  filters: { isp: string; sending_domain: string };
  overall: HarvestMetrics;
  by_isp: ISPRow[];
  by_sending_domain: DomRow[];
  time_series: TSRow[];
  time_series_by_isp: Record<string, TSRow[]>;
  hour_of_day: HourRow[];
  by_campaign: CampRow[];
  engagement_vs_damage: EngVsDmg;
}

type BucketChoice = '1h' | '3h' | '5h';
type HoursChoice  = 24 | 72 | 120;

// ─── Helpers ────────────────────────────────────────────────────────────────

const fmt = (n: number): string => {
  if (n == null || isNaN(n)) return '0';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
};
const pct = (n: number | undefined | null): string =>
  (n == null || isNaN(n)) ? '0.0%' : n.toFixed(2) + '%';

// Rate color rules (mailing-saas bounce-metrics conventions):
//   Hard bounce:  <1 good, <2 ok, else bad
//   Soft bounce:  <5 good, <10 ok, else bad
//   Complaint:    <0.1 good, <0.3 ok, else bad
//   Open rate:    >20 good, >10 ok, else bad
//   Click rate:   >3 good, >1 ok, else bad
const hardColor   = (r: number) => r < 1   ? '#10b981' : r < 2   ? '#f59e0b' : '#ef4444';
const softColor   = (r: number) => r < 5   ? '#10b981' : r < 10  ? '#f59e0b' : '#ef4444';
const cmpColor    = (r: number) => r < 0.1 ? '#10b981' : r < 0.3 ? '#f59e0b' : '#ef4444';
const openColor   = (r: number) => r > 20  ? '#10b981' : r > 10  ? '#f59e0b' : '#94a3b8';
const clickColor  = (r: number) => r > 3   ? '#10b981' : r > 1   ? '#f59e0b' : '#94a3b8';

// ─── Component ──────────────────────────────────────────────────────────────

interface Props {
  orgId?: string;
  /**
   * Optional: override the campaign_prefix filter. Defaults to the backend
   * default ("Welcome Harvest"). Pass null to get cross-campaign totals.
   */
  campaignPrefixOverride?: string | null;
}

async function fetchHarvest(params: URLSearchParams, orgId?: string): Promise<HarvestResponse> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (orgId) headers['X-Organization-ID'] = orgId;
  const res = await fetch(`/api/mailing/analytics/harvest-performance?${params.toString()}`, { headers });
  if (!res.ok) {
    throw new Error(`harvest-performance ${res.status}: ${await res.text()}`);
  }
  return res.json();
}

export const HarvestStreamDashboard: React.FC<Props> = ({ orgId, campaignPrefixOverride }) => {
  const [bucket, setBucket] = useState<BucketChoice>('1h');
  const [hours, setHours]   = useState<HoursChoice>(72);
  const [loading, setLoading] = useState(false);
  const [error, setError]     = useState<string | null>(null);
  const [data, setData]       = useState<HarvestResponse | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const p = new URLSearchParams();
      p.set('hours', String(hours));
      p.set('bucket', bucket);
      if (campaignPrefixOverride !== undefined) {
        // Explicit override wins — including the empty-string "no filter".
        p.set('campaign_prefix', campaignPrefixOverride ?? '');
      }
      const d = await fetchHarvest(p, orgId);
      setData(d);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [hours, bucket, orgId, campaignPrefixOverride]);

  useEffect(() => { load(); }, [load]);

  // Auto-refresh every 60s. Harvest stream is continuously sending, so the
  // dashboard should feel live without hammering the DB.
  useEffect(() => {
    const id = setInterval(load, 60_000);
    return () => clearInterval(id);
  }, [load]);

  // ─── Derived data ──────────────────────────────────────────────────────
  //
  // Build the combined line chart shape: rows indexed by timestamp, one
  // column per ISP for "sent" plus overall. Recharts needs a flat array
  // of rows, not a per-series array of points.
  const chartData = useMemo(() => {
    if (!data) return [];
    const buckets = new Map<string, Record<string, number | string>>();
    for (const row of data.time_series) {
      const ts = row.ts_mst.slice(11, 16); // HH:MM in MST for readability
      buckets.set(row.ts_utc, { ts_utc: row.ts_utc, label: ts, __overall_sent: row.metrics.sent, __overall_delivered: row.metrics.delivered });
    }
    for (const [isp, rows] of Object.entries(data.time_series_by_isp)) {
      for (const row of rows) {
        const b = buckets.get(row.ts_utc) || { ts_utc: row.ts_utc, label: row.ts_mst.slice(11, 16) };
        b[`${isp}_sent`] = row.metrics.sent;
        b[`${isp}_delivered`] = row.metrics.delivered;
        buckets.set(row.ts_utc, b);
      }
    }
    return Array.from(buckets.values()).sort((a, b) => String(a.ts_utc).localeCompare(String(b.ts_utc)));
  }, [data]);

  // Unique ISP keys present in the time series for legend/lines.
  const isps = useMemo(() => (data ? Object.keys(data.time_series_by_isp).sort() : []), [data]);

  // Hour-of-day heatmap: isp-rows × 24 hour columns. We compute a relative
  // intensity per ISP row so a busy ISP like gmail doesn't wash out the
  // scale for a quiet one like sbcglobal.
  const heatmap = useMemo(() => {
    if (!data) return { ispsList: [] as string[], matrix: {} as Record<string, (HourRow | null)[]>, maxByIsp: {} as Record<string, number> };
    const ispsSet = new Set<string>();
    const byIspHour: Record<string, HourRow[]> = {};
    for (const row of data.hour_of_day) {
      ispsSet.add(row.isp);
      (byIspHour[row.isp] ||= []).push(row);
    }
    const ispsList = Array.from(ispsSet).sort();
    const matrix: Record<string, (HourRow | null)[]> = {};
    const maxByIsp: Record<string, number> = {};
    for (const isp of ispsList) {
      const row: (HourRow | null)[] = new Array(24).fill(null);
      let mx = 0;
      for (const r of byIspHour[isp] || []) {
        if (r.hour >= 0 && r.hour < 24) {
          row[r.hour] = r;
          mx = Math.max(mx, r.metrics.sent);
        }
      }
      matrix[isp] = row;
      maxByIsp[isp] = mx || 1;
    }
    return { ispsList, matrix, maxByIsp };
  }, [data]);

  const ispColors = useMemo(() => ({
    gmail:     '#ea4335',
    yahoo:     '#7b1fa2',
    aol:       '#1e88e5',
    microsoft: '#00bcd4',
    apple:     '#64b5f6',
    comcast:   '#00897b',
    att:       '#ff9800',
    sbcglobal: '#8d6e63',
    cox:       '#4caf50',
    charter:   '#e91e63',
    other:     '#9e9e9e',
  } as Record<string, string>), []);

  // ─── Render ────────────────────────────────────────────────────────────

  return (
    <div id="harvest-stream-dashboard" className="ac-card ig-card-hover">
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '15px', flexWrap: 'wrap', gap: '10px' }}>
        <h3 style={{ margin: 0, display: 'flex', alignItems: 'center', gap: '8px' }}>
          <FontAwesomeIcon icon={faBroadcastTower} /> Harvest Stream
          {data?.campaign_prefix && (
            <span style={{ color: '#94a3b8', fontSize: '0.75em', fontWeight: 400 }}>
              filter: {data.campaign_prefix}*
            </span>
          )}
          {data?.api_version && (
            <span style={{ color: '#64748b', fontSize: '0.7em', fontWeight: 400 }}>
              backend v{data.api_version}
            </span>
          )}
        </h3>
        <div style={{ display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' }}>
          <div className="ac-range-selector">
            {([24, 72, 120] as HoursChoice[]).map(h => (
              <button
                key={h}
                className={hours === h ? 'active' : ''}
                onClick={() => setHours(h)}
                title={`Last ${h} hours`}
              >
                {h}h
              </button>
            ))}
          </div>
          <div className="ac-range-selector">
            {(['1h', '3h', '5h'] as BucketChoice[]).map(b => (
              <button
                key={b}
                className={bucket === b ? 'active' : ''}
                onClick={() => setBucket(b)}
                title={`${b} buckets`}
              >
                {b}
              </button>
            ))}
          </div>
          <button onClick={load} className="ig-btn-glow" style={{ padding: '4px 10px', fontSize: '0.8em' }}>
            <FontAwesomeIcon icon={faSyncAlt} spin={loading} /> Refresh
          </button>
        </div>
      </div>

      {error && (
        <div className="ac-empty-mini" style={{ color: '#f87171' }}>
          Error loading harvest analytics: {error}
        </div>
      )}

      {!data && loading && (
        <div className="ac-empty-mini"><FontAwesomeIcon icon={faSpinner} spin /> Loading harvest analytics...</div>
      )}

      {data && (
        <>
          {/* ─── KPI strip + Engagement vs Damage ──────────────────────── */}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: '10px', marginBottom: '20px' }}>
            <KPI label="Sent"         value={fmt(data.overall.sent)}        />
            <KPI label="Delivered"    value={fmt(data.overall.delivered)}   sub={pct(data.overall.delivery_rate)} />
            <KPI label="Open rate"    value={pct(data.overall.open_rate)}   color={openColor(data.overall.open_rate)} sub={`${fmt(data.overall.unique_opens)} uniq`} />
            <KPI label="Click rate"   value={pct(data.overall.click_rate)}  color={clickColor(data.overall.click_rate)} sub={`${fmt(data.overall.unique_clicks)} uniq`} />
            <KPI label="Hard bounce"  value={pct(data.overall.hard_bounce_rate)} color={hardColor(data.overall.hard_bounce_rate)} sub={fmt(data.overall.hard_bounces)} />
            <KPI label="Soft bounce"  value={pct(data.overall.soft_bounce_rate)} color={softColor(data.overall.soft_bounce_rate)} sub={fmt(data.overall.soft_bounces)} />
            <KPI label="Complaint"    value={pct(data.overall.complaint_rate)}   color={cmpColor(data.overall.complaint_rate)} sub={fmt(data.overall.complaints)} />
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '10px', marginBottom: '20px' }}>
            <div style={{ padding: '12px', border: '1px solid rgba(16,185,129,0.3)', borderRadius: '8px', background: 'rgba(16,185,129,0.05)' }}>
              <div style={{ fontSize: '0.75em', color: '#10b981', fontWeight: 600, marginBottom: '4px' }}>
                <FontAwesomeIcon icon={faHeart} /> ENGAGEMENT
              </div>
              <div style={{ fontSize: '1.8em', fontWeight: 700 }}>{fmt(data.engagement_vs_damage.engagement)}</div>
              <div style={{ fontSize: '0.75em', color: '#94a3b8' }}>
                {fmt(data.engagement_vs_damage.unique_opens)} uniq opens + {fmt(data.engagement_vs_damage.unique_clicks)} uniq clicks
              </div>
            </div>
            <div style={{ padding: '12px', border: '1px solid rgba(239,68,68,0.3)', borderRadius: '8px', background: 'rgba(239,68,68,0.05)' }}>
              <div style={{ fontSize: '0.75em', color: '#ef4444', fontWeight: 600, marginBottom: '4px' }}>
                <FontAwesomeIcon icon={faSkullCrossbones} /> DAMAGE
              </div>
              <div style={{ fontSize: '1.8em', fontWeight: 700 }}>{fmt(data.engagement_vs_damage.damage)}</div>
              <div style={{ fontSize: '0.75em', color: '#94a3b8' }}>
                {fmt(data.engagement_vs_damage.hard_bounces)} hard bounces + {fmt(data.engagement_vs_damage.complaints)} complaints
              </div>
              <div style={{ fontSize: '0.8em', marginTop: '6px', color: '#f87171' }}>
                ratio engagement : damage = {data.engagement_vs_damage.ratio === -1 ? '∞' : data.engagement_vs_damage.ratio.toFixed(2)} : 1
              </div>
            </div>
          </div>

          {/* ─── ISP health table ─────────────────────────────────────── */}
          <div className="ac-table-wrap" style={{ marginBottom: '20px' }}>
            <table className="ac-table">
              <thead>
                <tr>
                  <th>ISP</th>
                  <th>Sent</th>
                  <th>Delivered</th>
                  <th>Delivery %</th>
                  <th>Uniq Opens</th>
                  <th>Open %</th>
                  <th>Uniq Clicks</th>
                  <th>Click %</th>
                  <th>Hard B.</th>
                  <th>Hard %</th>
                  <th>Soft %</th>
                  <th>Cmp.</th>
                  <th>Cmp %</th>
                </tr>
              </thead>
              <tbody>
                {data.by_isp.length === 0 ? (
                  <tr><td colSpan={13} style={{ textAlign: 'center', color: '#94a3b8' }}>No ISP activity in this window.</td></tr>
                ) : data.by_isp.map(r => (
                  <tr key={r.isp}>
                    <td style={{ fontWeight: 500 }}>
                      <span style={{ display: 'inline-block', width: '10px', height: '10px', borderRadius: '50%', background: ispColors[r.isp] || '#9e9e9e', marginRight: '6px' }} />
                      {r.display_name}
                    </td>
                    <td>{fmt(r.metrics.sent)}</td>
                    <td>{fmt(r.metrics.delivered)}</td>
                    <td>{pct(r.metrics.delivery_rate)}</td>
                    <td>{fmt(r.metrics.unique_opens)}</td>
                    <td style={{ color: openColor(r.metrics.open_rate) }}>{pct(r.metrics.open_rate)}</td>
                    <td>{fmt(r.metrics.unique_clicks)}</td>
                    <td style={{ color: clickColor(r.metrics.click_rate) }}>{pct(r.metrics.click_rate)}</td>
                    <td style={{ color: r.metrics.hard_bounces > 0 ? '#ef4444' : '#475569' }}>{fmt(r.metrics.hard_bounces)}</td>
                    <td style={{ color: hardColor(r.metrics.hard_bounce_rate) }}>{pct(r.metrics.hard_bounce_rate)}</td>
                    <td style={{ color: softColor(r.metrics.soft_bounce_rate) }}>{pct(r.metrics.soft_bounce_rate)}</td>
                    <td style={{ color: r.metrics.complaints > 0 ? '#ef4444' : '#475569' }}>{fmt(r.metrics.complaints)}</td>
                    <td style={{ color: cmpColor(r.metrics.complaint_rate) }}>{pct(r.metrics.complaint_rate)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {/* ─── Time-series chart ────────────────────────────────────── */}
          <div style={{ marginBottom: '20px' }}>
            <div style={{ fontSize: '0.85em', color: '#94a3b8', marginBottom: '6px' }}>
              <FontAwesomeIcon icon={faChartArea} /> Sent over time · bucket {bucket} · last {hours}h
            </div>
            <ResponsiveContainer width="100%" height={280}>
              <LineChart data={chartData as Array<Record<string, number | string>>}>
                <CartesianGrid strokeDasharray="3 3" stroke="#1e293b" />
                <XAxis dataKey="label" stroke="#94a3b8" fontSize={11} />
                <YAxis stroke="#94a3b8" fontSize={11} />
                <RechartsTooltip
                  contentStyle={{ background: '#0f172a', border: '1px solid #334155', borderRadius: '6px', fontSize: '12px' }}
                  labelStyle={{ color: '#e2e8f0' }}
                />
                <Legend wrapperStyle={{ fontSize: '11px' }} />
                <Line type="monotone" dataKey="__overall_sent" name="Overall" stroke="#fff" strokeWidth={2} dot={false} />
                {isps.map(isp => (
                  <Line
                    key={isp}
                    type="monotone"
                    dataKey={`${isp}_sent`}
                    name={isp}
                    stroke={ispColors[isp] || '#9e9e9e'}
                    strokeWidth={1.5}
                    dot={false}
                  />
                ))}
              </LineChart>
            </ResponsiveContainer>
          </div>

          {/* ─── Hour-of-day heatmap ──────────────────────────────────── */}
          <div style={{ marginBottom: '20px' }}>
            <div style={{ fontSize: '0.85em', color: '#94a3b8', marginBottom: '6px' }}>
              <FontAwesomeIcon icon={faClock} /> Hour-of-day sent volume (MST) · aggregated over {hours}h
            </div>
            <div style={{ overflowX: 'auto' }}>
              <table className="ac-table" style={{ tableLayout: 'fixed', minWidth: '720px', fontSize: '0.75em' }}>
                <thead>
                  <tr>
                    <th style={{ width: '90px' }}>ISP</th>
                    {Array.from({ length: 24 }).map((_, h) => (
                      <th key={h} style={{ textAlign: 'center', padding: '3px', color: h >= 3 && h <= 19 ? '#e2e8f0' : '#64748b' }}>{h}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {heatmap.ispsList.length === 0 ? (
                    <tr><td colSpan={25} style={{ textAlign: 'center', color: '#94a3b8' }}>No hourly data yet.</td></tr>
                  ) : heatmap.ispsList.map(isp => (
                    <tr key={isp}>
                      <td style={{ fontWeight: 500 }}>{isp}</td>
                      {heatmap.matrix[isp].map((cell, h) => {
                        const max = heatmap.maxByIsp[isp] || 1;
                        const v = cell ? cell.metrics.sent : 0;
                        const intensity = v / max;
                        const color = `rgba(59, 130, 246, ${0.1 + intensity * 0.8})`;
                        const openR = cell ? cell.metrics.open_rate : 0;
                        const hardR = cell ? cell.metrics.hard_bounce_rate : 0;
                        return (
                          <td
                            key={h}
                            title={cell ? `${isp} @ ${h}:00 MST\nsent=${v}\ndelivered=${cell.metrics.delivered}\nopen_rate=${openR.toFixed(2)}%\nhard_bounce_rate=${hardR.toFixed(2)}%` : `${isp} @ ${h}:00 MST — no data`}
                            style={{
                              background: v > 0 ? color : 'transparent',
                              textAlign: 'center',
                              padding: '4px 2px',
                              color: intensity > 0.5 ? '#fff' : '#94a3b8',
                              borderRight: '1px solid rgba(255,255,255,0.03)',
                            }}
                          >
                            {v > 0 ? fmt(v) : ''}
                          </td>
                        );
                      })}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div style={{ fontSize: '0.7em', color: '#64748b', marginTop: '4px' }}>
              Cell intensity is relative to the max per ISP. Hover for exact counts and rates.
            </div>
          </div>

          {/* ─── Per-sending-domain table ─────────────────────────────── */}
          <div className="ac-table-wrap" style={{ marginBottom: '20px' }}>
            <div style={{ fontSize: '0.85em', color: '#94a3b8', marginBottom: '6px' }}>
              Per sending domain
            </div>
            <table className="ac-table">
              <thead>
                <tr>
                  <th>Domain</th>
                  <th>Sent</th>
                  <th>Delivered</th>
                  <th>Delivery %</th>
                  <th>Open %</th>
                  <th>Click %</th>
                  <th>Hard %</th>
                  <th>Cmp %</th>
                </tr>
              </thead>
              <tbody>
                {data.by_sending_domain.length === 0 ? (
                  <tr><td colSpan={8} style={{ textAlign: 'center', color: '#94a3b8' }}>No sending-domain activity in this window.</td></tr>
                ) : data.by_sending_domain.map(r => (
                  <tr key={r.sending_domain}>
                    <td style={{ fontWeight: 500 }}>{r.sending_domain}</td>
                    <td>{fmt(r.metrics.sent)}</td>
                    <td>{fmt(r.metrics.delivered)}</td>
                    <td>{pct(r.metrics.delivery_rate)}</td>
                    <td style={{ color: openColor(r.metrics.open_rate) }}>{pct(r.metrics.open_rate)}</td>
                    <td style={{ color: clickColor(r.metrics.click_rate) }}>{pct(r.metrics.click_rate)}</td>
                    <td style={{ color: hardColor(r.metrics.hard_bounce_rate) }}>{pct(r.metrics.hard_bounce_rate)}</td>
                    <td style={{ color: cmpColor(r.metrics.complaint_rate) }}>{pct(r.metrics.complaint_rate)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {/* ─── Per-campaign table ──────────────────────────────────── */}
          <div className="ac-table-wrap">
            <div style={{ fontSize: '0.85em', color: '#94a3b8', marginBottom: '6px' }}>
              Top {data.by_campaign.length} harvest campaigns by send volume
            </div>
            <table className="ac-table">
              <thead>
                <tr>
                  <th>Campaign</th>
                  <th>Domain</th>
                  <th>Sent</th>
                  <th>Delivered</th>
                  <th>Open %</th>
                  <th>Click %</th>
                  <th>Hard %</th>
                  <th>Cmp %</th>
                </tr>
              </thead>
              <tbody>
                {data.by_campaign.length === 0 ? (
                  <tr><td colSpan={8} style={{ textAlign: 'center', color: '#94a3b8' }}>No harvest campaigns found. Deploy via scripts/deploy_welcome_harvest.py.</td></tr>
                ) : data.by_campaign.map(c => (
                  <tr key={c.campaign_id}>
                    <td>{c.name}</td>
                    <td>{c.sending_domain}</td>
                    <td>{fmt(c.metrics.sent)}</td>
                    <td>{fmt(c.metrics.delivered)}</td>
                    <td style={{ color: openColor(c.metrics.open_rate) }}>{pct(c.metrics.open_rate)}</td>
                    <td style={{ color: clickColor(c.metrics.click_rate) }}>{pct(c.metrics.click_rate)}</td>
                    <td style={{ color: hardColor(c.metrics.hard_bounce_rate) }}>{pct(c.metrics.hard_bounce_rate)}</td>
                    <td style={{ color: cmpColor(c.metrics.complaint_rate) }}>{pct(c.metrics.complaint_rate)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
};

// ─── KPI tile helper ─────────────────────────────────────────────────────────

const KPI: React.FC<{ label: string; value: string; sub?: string; color?: string }> = ({ label, value, sub, color }) => (
  <div style={{
    padding: '10px 12px',
    border: '1px solid rgba(255,255,255,0.08)',
    borderRadius: '6px',
    background: 'rgba(255,255,255,0.02)',
  }}>
    <div style={{ fontSize: '0.7em', textTransform: 'uppercase', color: '#94a3b8', letterSpacing: '0.5px', marginBottom: '2px' }}>{label}</div>
    <div style={{ fontSize: '1.4em', fontWeight: 700, color: color || '#e2e8f0' }}>{value}</div>
    {sub && <div style={{ fontSize: '0.7em', color: '#64748b', marginTop: '2px' }}>{sub}</div>}
  </div>
);

export default HarvestStreamDashboard;
