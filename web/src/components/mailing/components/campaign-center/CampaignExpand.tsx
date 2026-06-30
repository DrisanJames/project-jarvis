import React, { useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faExclamationTriangle, faLink, faBullseye, faClock,
  faChartLine, faUpRightFromSquare,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Area, Line, XAxis, YAxis, Tooltip,
  Legend, CartesianGrid, ReferenceLine,
} from 'recharts';
import { apiFetch } from '../../shared/apiFetch';
import { colors as theme } from '../../shared/theme';
import {
  DetailResponse, TimeseriesResponse, fmtDenverTime, fmtWindow, numFmt,
} from './types';

// CampaignExpand — the inline accordion body for one campaign row (Round-3
// §1.3). Lazily fetched on expand:
//   a. send-pace graph (recharts) with wave-start markers
//   b. KPI strip (tracking-event derived; hard/soft ALWAYS split)
//   c. per-ISP-family mini table
//   d. top 5 clicked links
//   e. subject / preheader / from / segments / throttle-window facts
// All data comes from /campaigns/{id}/detail + /campaigns/{id}/timeseries
// (campaign_timeseries.go) — campaign counters are never used.

const PANEL: React.CSSProperties = {
  background: 'rgba(15,22,41,0.85)',
  border: '1px solid rgba(99,102,241,0.15)',
  borderRadius: 10,
  padding: '12px 14px',
};

const LABEL: React.CSSProperties = {
  fontSize: 10.5, textTransform: 'uppercase', letterSpacing: 0.5,
  color: 'rgba(180,210,240,0.45)', fontWeight: 600,
};

const SERIES_COLORS: Record<string, string> = {
  attempted: theme.indigo400,
  delivered: theme.success,
  opens: theme.indigo300,
  clicks: '#c084fc',
  unsubscribes: theme.warning,
  bounces: theme.danger,
};

const ISP_LABELS: Record<string, string> = {
  gmail: 'Gmail', microsoft: 'Microsoft', yahoo: 'Yahoo', apple: 'Apple',
  aol: 'AOL', comcast: 'Comcast', att: 'AT&T', charter: 'Charter/Spectrum', other: 'Other',
};

const SourceBadge: React.FC<{ source?: string; routeType?: string }> = ({ source, routeType }) => (
  <span style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
    <span
      title="Metrics source — current view reads our tracking only (Reporting integration is a follow-up)"
      style={{
        fontSize: '0.62rem', fontWeight: 700, letterSpacing: 0.5, padding: '2px 7px',
        borderRadius: 6, border: '1px solid #34d39955', background: '#34d3991a', color: '#34d399',
      }}
    >
      {(source || 'pg').toUpperCase()}
    </span>
    {routeType && (
      <span
        style={{
          fontSize: '0.62rem', fontWeight: 700, letterSpacing: 0.5, padding: '2px 7px',
          borderRadius: 6,
          border: `1px solid ${routeType === 'ses' ? '#38bdf855' : '#a78bfa55'}`,
          background: routeType === 'ses' ? '#38bdf81a' : '#a78bfa1a',
          color: routeType === 'ses' ? '#38bdf8' : '#a78bfa',
        }}
      >
        {routeType.toUpperCase()}
      </span>
    )}
  </span>
);

const KpiChip: React.FC<{ label: string; value: string; sub?: string; color?: string; tip?: string }> =
  ({ label, value, sub, color, tip }) => (
    <div title={tip} style={{ ...PANEL, padding: '8px 12px', minWidth: 96, flex: '0 0 auto' }}>
      <div style={{ fontSize: 16, fontWeight: 700, color: color || '#e0e6f0' }}>{value}</div>
      <div style={LABEL}>{label}</div>
      {sub && <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.55)' }}>{sub}</div>}
    </div>
  );

export const CampaignExpand: React.FC<{
  campaignId: string;
  onOpenModal: (id: string) => void;
}> = ({ campaignId, onOpenModal }) => {
  const [detail, setDetail] = useState<DetailResponse | null>(null);
  const [series, setSeries] = useState<TimeseriesResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    // Both fetches are lazy — they only happen because this row was expanded.
    // Bucket selection (1m when window <6h old, else 15m) lives server-side.
    Promise.all([
      apiFetch(`/api/mailing/campaigns/${campaignId}/detail`).then(r => r.json()),
      apiFetch(`/api/mailing/campaigns/${campaignId}/timeseries`).then(r => r.json()),
    ])
      .then(([d, t]: [DetailResponse, TimeseriesResponse]) => {
        if (cancelled) return;
        if (d.error) throw new Error(d.error);
        setDetail(d);
        setSeries(t.error ? { series: [], waves: [], notes: t.error } : t);
      })
      .catch((e) => { if (!cancelled) setError(e instanceof Error ? e.message : 'fetch failed'); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [campaignId]);

  const chartData = useMemo(() => (series?.series || []).map(b => ({
    ts: new Date(b.t).getTime(),
    attempted: b.attempted,
    delivered: b.delivered,
    opens: b.opens,
    clicks: b.clicks,
    unsubscribes: b.unsubscribes,
    bounces: b.bounces,
  })), [series]);

  const fmtTick = (ts: number) => new Date(ts).toLocaleTimeString('en-US', {
    timeZone: 'America/Denver', hour: 'numeric', minute: '2-digit',
  });

  if (loading) {
    return (
      <div style={{ padding: '18px 14px', color: 'rgba(180,210,240,0.6)', fontSize: 13 }}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading inline analytics…
      </div>
    );
  }
  if (error || !detail) {
    return (
      <div style={{ padding: '14px', color: '#fca5a5', fontSize: 13 }}>
        <FontAwesomeIcon icon={faExclamationTriangle} /> Inline analytics unavailable ({error || 'no data'}).
      </div>
    );
  }

  const c = detail.campaign;
  const k = detail.kpis;
  const win = detail.window;
  const waves = series?.waves || detail.waves || [];
  const isSes = (c?.route_type || '') === 'ses';

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 10, padding: '12px 6px 14px' }}>
      {/* header strip: source + window facts + escape hatch to legacy modal */}
      <div style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: 10, fontSize: 12, color: '#94a3b8' }}>
        <SourceBadge source={detail.source} routeType={c?.route_type} />
        <span title="Send window (Denver)">
          <FontAwesomeIcon icon={faClock} style={{ marginRight: 5 }} />
          {fmtWindow(win?.window_start, win?.window_end)}
        </span>
        <span title="First send batch">
          1st send {fmtDenverTime(win?.first_wave_at || c?.scheduled_at)} · {win?.wave_count ?? 0} send batches
        </span>
        {win?.throttle_speed && <span>send rate {win.throttle_speed}{win.throttle_rate_per_minute ? ` @${win.throttle_rate_per_minute}/min` : ''}{win.throttle_duration_hours ? ` over ${win.throttle_duration_hours}h` : ''}</span>}
        <button
          onClick={() => onOpenModal(campaignId)}
          title="Open the full campaign record (creative versions, audience funnel, pending count, pause/cancel actions)"
          style={{
            marginLeft: 'auto', display: 'inline-flex', alignItems: 'center', gap: 6,
            background: 'rgba(99,102,241,0.08)', border: '1px solid rgba(99,102,241,0.25)',
            color: '#a5b4fc', borderRadius: 7, padding: '4px 10px', fontSize: 11.5,
            fontWeight: 600, cursor: 'pointer',
          }}
        >
          <FontAwesomeIcon icon={faUpRightFromSquare} /> Full record
        </button>
      </div>

      {isSes && (
        <div style={{
          fontSize: 11.5, color: '#7dd3fc', background: 'rgba(56,189,248,0.07)',
          border: '1px solid rgba(56,189,248,0.25)', borderRadius: 8, padding: '6px 10px',
        }}>
          Relayed route: our tracking events are sparse on this route — delivered/opens here are a lower
          bound. Full Reporting figures are a follow-up.
        </div>
      )}

      {/* (b) KPI strip */}
      {k && (
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
          <KpiChip label="Delivered" value={`${k.delivered_rate.toFixed(1)}%`} sub={`${numFmt(k.delivered)} of ${numFmt(Math.max(k.attempted, k.delivered + k.hard_bounces + k.soft_bounces))}`} color="#22c55e" tip="delivered / max(attempted, delivered+hard+soft)" />
          <KpiChip label="Human Open" value={`${k.human_open_rate.toFixed(1)}%`} sub={`${numFmt(k.human_opens)} uniq`} color="#38bdf8" tip="unique human opens / delivered (Apple Mail privacy & automated opens excluded)" />
          <KpiChip label="Human Click" value={`${k.human_click_rate.toFixed(2)}%`} sub={`${numFmt(k.human_clicks)} uniq`} color="#c084fc" tip="unique clicks / delivered" />
          <KpiChip label="CTOR" value={`${k.ctor.toFixed(1)}%`} tip="unique clicks / unique human opens" />
          <KpiChip label="Hard" value={`${k.hard_bounce_rate.toFixed(2)}%`} sub={numFmt(k.hard_bounces)} color="#ef4444" tip="True hard bounces (invalid addresses)" />
          <KpiChip label="Soft" value={`${k.soft_bounce_rate.toFixed(2)}%`} sub={numFmt(k.soft_bounces)} color="#f59e0b" tip="transient bounces" />
          <KpiChip label="Complaints" value={numFmt(k.complaints)} color={k.complaints > 0 ? '#ef4444' : undefined} />
          <KpiChip label="Unsubs" value={numFmt(k.unsubscribes)} />
          <KpiChip label="Deferrals" value={numFmt(k.deferrals)} color="#60a5fa" />
          {k.conversions !== null && k.conversions !== undefined && (
            <KpiChip label="Conversions" value={numFmt(k.conversions)} color="#34d399" tip={`conversions in the campaign window${c?.offer_name ? ` for ${c.offer_name}` : ''}`} />
          )}
        </div>
      )}

      {/* (a) send-pace graph with wave markers */}
      <div style={PANEL}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
          <FontAwesomeIcon icon={faChartLine} style={{ color: '#818cf8' }} />
          <span style={{ fontSize: 12.5, fontWeight: 600, color: '#c7d2fe' }}>
            Send pace · {series?.bucket || '—'} buckets
          </span>
          <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.45)' }}>
            send batches marked · times in Denver
          </span>
        </div>
        {chartData.length === 0 ? (
          <div style={{ fontSize: 12.5, color: 'rgba(180,210,240,0.5)', padding: '14px 0' }}>
            No tracking events in the send window yet.
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={240}>
            <ComposedChart data={chartData} margin={{ top: 8, right: 16, bottom: 0, left: 0 }}>
              <defs>
                <linearGradient id="campaignPaceFill" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stopColor={theme.indigo500} stopOpacity={0.55} />
                  <stop offset="100%" stopColor={theme.indigo500} stopOpacity={0.05} />
                </linearGradient>
              </defs>
              <CartesianGrid stroke="rgba(148,163,184,0.12)" vertical={false} />
              <XAxis
                dataKey="ts" type="number" scale="time" domain={['dataMin', 'dataMax']}
                tickFormatter={fmtTick} tick={{ fill: '#94a3b8', fontSize: 11 }}
                interval="preserveStartEnd" minTickGap={40}
              />
              <YAxis tick={{ fill: '#94a3b8', fontSize: 11 }} width={52} allowDecimals={false} />
              <Tooltip
                labelFormatter={(v) => fmtTick(Number(v))}
                contentStyle={{
                  background: '#0f172a', border: '1px solid rgba(99,102,241,0.4)',
                  borderRadius: 8, color: '#e5e7eb', fontSize: 12,
                }}
                labelStyle={{ color: '#dbeafe' }}
              />
              <Legend wrapperStyle={{ fontSize: 12, color: '#94a3b8' }} />
              {waves.map(w => (
                <ReferenceLine
                  key={`w${w.wave_number}-${w.scheduled_at}`}
                  x={new Date(w.scheduled_at).getTime()}
                  stroke={theme.indigo500} strokeDasharray="4 4" strokeOpacity={0.55}
                />
              ))}
              <Area type="monotone" dataKey="attempted" name="Attempted" stroke={SERIES_COLORS.attempted} strokeWidth={2} fill="url(#campaignPaceFill)" />
              <Line type="monotone" dataKey="delivered" name="Delivered" stroke={SERIES_COLORS.delivered} dot={false} strokeWidth={2} />
              <Line type="monotone" dataKey="opens" name="Opens (human)" stroke={SERIES_COLORS.opens} dot={false} strokeWidth={1.6} />
              <Line type="monotone" dataKey="clicks" name="Clicks" stroke={SERIES_COLORS.clicks} dot={false} strokeWidth={1.6} />
              <Line type="monotone" dataKey="unsubscribes" name="Unsubs" stroke={SERIES_COLORS.unsubscribes} dot={false} strokeWidth={1.4} />
              <Line type="monotone" dataKey="bounces" name="Bounces" stroke={SERIES_COLORS.bounces} dot={false} strokeWidth={1.4} />
            </ComposedChart>
          </ResponsiveContainer>
        )}
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'minmax(320px, 1.2fr) minmax(280px, 1fr)', gap: 10 }}>
        {/* (c) per-ISP mini table */}
        <div style={PANEL}>
          <div style={{ ...LABEL, marginBottom: 6 }}>Per-mailbox-provider (recipient)</div>
          {(detail.isp_breakdown || []).length === 0 ? (
            <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.5)' }}>No per-mailbox-provider events yet.</div>
          ) : (
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
              <thead>
                <tr style={{ color: 'rgba(180,210,240,0.45)', textAlign: 'right' }}>
                  <th style={{ textAlign: 'left', padding: '3px 6px' }}>Mailbox Provider</th>
                  <th style={{ padding: '3px 6px' }}>Delivered</th>
                  <th style={{ padding: '3px 6px' }}>H. Opens</th>
                  <th style={{ padding: '3px 6px' }}>H. Clicks</th>
                  <th style={{ padding: '3px 6px', color: '#ef4444' }}>Hard</th>
                  <th style={{ padding: '3px 6px', color: '#f59e0b' }}>Soft</th>
                  <th style={{ padding: '3px 6px' }}>Defer</th>
                </tr>
              </thead>
              <tbody>
                {(detail.isp_breakdown || []).map(row => (
                  <tr key={row.isp} style={{ borderTop: '1px solid rgba(99,102,241,0.08)', textAlign: 'right', color: '#cbd5e1' }}>
                    <td style={{ textAlign: 'left', padding: '4px 6px', fontWeight: 600, color: '#e0e6f0' }}>
                      {ISP_LABELS[row.isp] || row.isp}
                    </td>
                    <td style={{ padding: '4px 6px' }}>{numFmt(row.delivered)}</td>
                    <td style={{ padding: '4px 6px' }}>{numFmt(row.human_opens)}</td>
                    <td style={{ padding: '4px 6px' }}>{numFmt(row.human_clicks)}</td>
                    <td style={{ padding: '4px 6px', color: row.hard > 0 ? '#ef4444' : undefined }}>{numFmt(row.hard)}</td>
                    <td style={{ padding: '4px 6px', color: row.soft > 0 ? '#f59e0b' : undefined }}>{numFmt(row.soft)}</td>
                    <td style={{ padding: '4px 6px' }}>{numFmt(row.deferrals)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          {/* (d) top clicked links */}
          <div style={PANEL}>
            <div style={{ ...LABEL, marginBottom: 6 }}>Top clicked links</div>
            {(detail.top_links || []).length === 0 ? (
              <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.5)' }}>No clicks yet.</div>
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
                {(detail.top_links || []).map(l => (
                  <div key={l.url} style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 12 }}>
                    <FontAwesomeIcon icon={faLink} style={{ color: '#64748b', fontSize: 10 }} />
                    <span
                      title={l.url}
                      style={{
                        flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                        color: '#93c5fd',
                      }}
                    >
                      {l.url}
                    </span>
                    <span style={{ color: '#e0e6f0', fontWeight: 600 }}>{numFmt(l.clicks)}</span>
                    <span style={{ color: 'rgba(180,210,240,0.45)', fontSize: 11 }}>({numFmt(l.unique_clicks)} uniq)</span>
                  </div>
                ))}
              </div>
            )}
          </div>

          {/* (e) content + segments facts */}
          <div style={PANEL}>
            <div style={{ ...LABEL, marginBottom: 6 }}>Content &amp; audience</div>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 5, fontSize: 12.5, color: '#cbd5e1' }}>
              <div><span style={LABEL}>Subject </span>{c?.subject || '—'}</div>
              <div><span style={LABEL}>Preview Text </span><span style={{ fontStyle: 'italic', opacity: 0.8 }}>{c?.preheader || '—'}</span></div>
              <div><span style={LABEL}>From </span>{c?.from_name || '—'}{c?.from_email ? ` <${c.from_email}>` : ''}</div>
              <div><span style={LABEL}>Domain </span>{c?.sending_domain || '—'} · {c?.sending_profile || '—'}</div>
              {c?.offer_name && <div><span style={LABEL}>Offer </span>{c.offer_name}</div>}
              <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', alignItems: 'center' }}>
                <span style={LABEL}>Segments</span>
                {(detail.segments?.inclusion || []).map(sname => (
                  <span key={`in-${sname}`} style={{
                    fontSize: 11, padding: '1px 8px', borderRadius: 10,
                    background: 'rgba(34,197,94,0.1)', border: '1px solid rgba(34,197,94,0.3)', color: '#86efac',
                  }}>
                    <FontAwesomeIcon icon={faBullseye} style={{ marginRight: 4, fontSize: 9 }} />{sname}
                  </span>
                ))}
                {(detail.segments?.exclusion || []).map(sname => (
                  <span key={`ex-${sname}`} style={{
                    fontSize: 11, padding: '1px 8px', borderRadius: 10,
                    background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.3)', color: '#fca5a5',
                  }}>
                    − {sname}
                  </span>
                ))}
                {(detail.segments?.inclusion || []).length === 0 && (detail.segments?.exclusion || []).length === 0 && (
                  <span style={{ color: 'rgba(180,210,240,0.45)' }}>none recorded</span>
                )}
              </div>
            </div>
          </div>
        </div>
      </div>

      {k?.data_as_of && (
        <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.4)' }}>
          Source: tracking events · last event {fmtDenverTime(k.data_as_of)} MT
        </div>
      )}
    </div>
  );
};

export default CampaignExpand;
