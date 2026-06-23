import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCircle, faPaperPlane, faExclamationTriangle, faInbox,
  faChevronRight, faChevronDown,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Bar, Line, XAxis, YAxis,
  Tooltip, Legend, CartesianGrid,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';

// DripPerformancePanel — the live "how are the drips actually doing" view on
// the Data Partners screen. Polling cadences against one endpoint:
//   - funnel + inbound roll-up + (vertical×brand) wave rollup + hourly series
//     every 30s — the heavier grouped scans
//   - 24h totals + the expanded group's individual waves every 10s — these
//     re-aggregate server-side from tracking events on each poll, so numbers
//     move as PMTA accounting events are ingested ("stats as they are received")
// Bounces are ALWAYS split hard (red) vs soft (amber) — never combined.
// Rates are computed against SENT (mail actually dispatched), never planned.

const VERTICAL_LABEL: Record<string, string> = {
  refi_heloc: 'Refi / HELOC',
  personal_loans: 'Personal Loans',
  tax_relief: 'Tax Relief',
  remodel: 'Remodel',
  direct_offer: 'Direct Offer',
  samsclub_internal: "Sam's Club (internal)",
  clickers_samsclub: "Sam's Club (clickers)",
  metal_roofing_signal: 'Metal Roofing (signal)',
};

const FAST_POLL_MS = 10_000;
const SLOW_POLL_MS = 30_000;
const WINDOW_HOURS = 48;

interface FunnelVertical {
  vertical: string;
  pending_eo: number;
  hold: number;
  ready: number;
  claimed: number;
  mailed: number;
  sent_24h: number;
  touch_1: number;
  touch_2: number;
  touch_3: number;
  touch_4: number;
  engaged: number;
  completed: number;
  in_progress?: number;
  churned?: number;
  drip_total?: number;
  conversions?: number;
  followups_due: number;
  // EO billing: SUM(eo_attempts) — one increment per EmailOversight call —
  // plus how many rows were validated in the last 24h.
  eo_credits_total: number;
  eo_validated_24h: number;
}

interface FunnelISP {
  vertical: string;
  isp: string;
  ready: number;
  mailed: number;
  sent_24h: number;
}

interface Totals24h {
  waves: number;
  recipients: number;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  deferred: number;
}

interface Ingest24h {
  dataset_id: string;
  dataset_name: string;
  partner_name: string;
  vertical: string;
  posts: number;
  records: number;
  last_received: string | null;
  in_flight: number;
  stopped: number;
}

interface RollupGroup {
  vertical: string;
  brand: string;
  partner_slug: string;
  waves: number;
  recipients: number;
  active: number;
  last_wave_at: string;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  deferred: number;
}

interface SeriesPoint {
  hour: string;
  vertical: string;
  brand: string;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
}

interface Wave {
  campaign_id: string;
  name: string;
  vertical: string;
  brand: string;
  partner_slug: string;
  partner_name: string;
  dataset_id: string;
  dataset_name: string;
  status: string;
  scheduled_at: string;
  total_recipients: number;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  deferred: number;
}

const groupKey = (vertical: string, brand: string) => `${vertical}|${brand}`;

export const DripPerformancePanel: React.FC = () => {
  const [funnel, setFunnel] = useState<FunnelVertical[]>([]);
  const [funnelISP, setFunnelISP] = useState<FunnelISP[]>([]);
  const [ingest, setIngest] = useState<Ingest24h[]>([]);
  const [rollup, setRollup] = useState<RollupGroup[]>([]);
  const [series, setSeries] = useState<SeriesPoint[]>([]);
  const [totals, setTotals] = useState<Totals24h | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [groupWaves, setGroupWaves] = useState<Wave[]>([]);
  const [lastLiveFetch, setLastLiveFetch] = useState<Date | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Tick state so the "updated Xs ago" label re-renders between polls.
  const [, setTick] = useState(0);
  const expandedRef = useRef<string | null>(null);
  expandedRef.current = expanded;

  // Fast poll: 24h totals always; the expanded group's individual waves too.
  const fetchLive = useCallback(() => {
    const exp = expandedRef.current;
    let url = '/api/mailing/data-partners/drip-performance?include=totals';
    if (exp) {
      const [v, b] = exp.split('|');
      url = `/api/mailing/data-partners/drip-performance?include=totals,waves&hours=${WINDOW_HOURS}&limit=20&vertical=${encodeURIComponent(v)}&brand=${encodeURIComponent(b)}`;
    }
    apiFetch(url, { credentials: 'include' })
      .then(r => r.json())
      .then(data => {
        if (data?.totals_24h) setTotals(data.totals_24h);
        if (exp && Array.isArray(data?.waves) && expandedRef.current === exp) setGroupWaves(data.waves);
        setLastLiveFetch(new Date());
        if (data?.waves_error) setError(`Waves: ${data.waves_error}`); else setError(null);
      })
      .catch(err => setError(String(err)));
  }, []);

  // Slow poll: funnel + ingest roll-up + (vertical×brand) rollup + chart series.
  const fetchSlow = useCallback(() => {
    apiFetch(`/api/mailing/data-partners/drip-performance?include=funnel,rollup&hours=${WINDOW_HOURS}`, { credentials: 'include' })
      .then(r => r.json())
      .then(data => {
        if (data?.funnel_error) { setError(`Funnel: ${data.funnel_error}`); return; }
        if (data?.rollup_error) { setError(`Rollup: ${data.rollup_error}`); return; }
        if (Array.isArray(data?.funnel)) setFunnel(data.funnel);
        if (Array.isArray(data?.funnel_isp)) setFunnelISP(data.funnel_isp);
        if (Array.isArray(data?.ingest_24h)) setIngest(data.ingest_24h);
        if (Array.isArray(data?.rollup)) setRollup(data.rollup);
        if (Array.isArray(data?.series)) setSeries(data.series);
      })
      .catch(err => setError(String(err)));
  }, []);

  useEffect(() => {
    fetchLive();
    fetchSlow();
    const tl = setInterval(fetchLive, FAST_POLL_MS);
    const ts = setInterval(fetchSlow, SLOW_POLL_MS);
    const tt = setInterval(() => setTick(t => t + 1), 1000);
    return () => { clearInterval(tl); clearInterval(ts); clearInterval(tt); };
  }, [fetchLive, fetchSlow]);

  // Refresh the detail immediately when a group is expanded.
  const toggleGroup = (key: string) => {
    setGroupWaves([]);
    setExpanded(prev => {
      const next = prev === key ? null : key;
      expandedRef.current = next;
      if (next) setTimeout(fetchLive, 0);
      return next;
    });
  };

  const secondsAgo = lastLiveFetch ? Math.max(0, Math.round((Date.now() - lastLiveFetch.getTime()) / 1000)) : null;

  // Cross-vertical funnel roll-up for the overall strip.
  const sum = (f: (v: FunnelVertical) => number) => funnel.reduce((a, v) => a + f(v), 0);
  const overall = {
    ready: sum(v => v.ready),
    inDrip: sum(v => v.touch_1 + v.touch_2 + v.touch_3 + v.touch_4),
    engaged: sum(v => v.engaged),
    dueNow: sum(v => v.followups_due),
  };

  // Order groups by 24h-window send volume, biggest lanes first.
  const sortedRollup = [...rollup].sort((a, b) => b.sent - a.sent || b.recipients - a.recipients);

  return (
    <div style={{ marginBottom: 28 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', borderBottom: '1px solid rgba(120,150,200,0.18)', paddingBottom: 6, marginBottom: 12 }}>
        <h3 style={{ color: '#dbeafe', margin: 0 }}>Follow-Up Performance</h3>
        <div style={{ display: 'flex', alignItems: 'center', gap: 14, fontSize: 12 }}>
          <span style={{ color: '#10b981', display: 'inline-flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faCircle} style={{ fontSize: 8 }} beat={secondsAgo !== null && secondsAgo < 3} />
            LIVE — refreshed from delivery activity every {FAST_POLL_MS / 1000}s
            {secondsAgo !== null && <span style={{ color: 'rgba(180,210,240,0.55)' }}>(updated {secondsAgo}s ago)</span>}
          </span>
        </div>
      </div>

      {error && (
        <div style={{ background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)', padding: 10, borderRadius: 6, marginBottom: 12, fontSize: 13 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {error}
        </div>
      )}

      {/* ───── Overall — all verticals, last 24h ───── */}
      {totals && <OverallStrip totals={totals} overall={overall} />}

      {/* ───── Per-vertical lifecycle funnels ───── */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))', gap: 14, marginBottom: 18 }}>
        {funnel.map(v => <FunnelCard key={v.vertical} v={v} isps={funnelISP.filter(i => i.vertical === v.vertical)} />)}
        {funnel.length === 0 && (
          <div style={{ color: 'rgba(180,210,240,0.55)', fontSize: 13 }}>No queue data yet.</div>
        )}
      </div>

      {/* ───── Wave rollup by vertical × brand ───── */}
      <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.7)', marginBottom: 8 }}>
        <FontAwesomeIcon icon={faPaperPlane} style={{ marginRight: 6 }} />
        Send batches — last {WINDOW_HOURS}h, rolled up per <b>vertical × brand</b>, biggest senders first. Expand a lane for its
        hourly delivery graph and individual send batches. Rates are against sent, not planned.
      </div>

      <table style={tableStyle}>
        <thead>
          <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
            <th style={{ ...th, width: 26 }}></th>
            <th style={th}>Vertical / Brand</th>
            <th style={thNum} title="Send batches in window (currently queued/sending)">Batches</th>
            <th style={{ ...th, minWidth: 140 }} title="Mail dispatched so far / planned recipients">Sent Progress</th>
            <th style={thNum} title="Delivered, as % of sent">Delivered</th>
            <th style={thNum} title="Hard bounces — permanent failures, reputation risk (% of sent)">Hard</th>
            <th style={thNum} title="Soft bounces — usually transient">Soft</th>
            <th style={thNum} title="Raw opens — includes automated privacy and scanner traffic">Opens</th>
            <th style={thNum}>Clicks</th>
            <th style={th}>Last batch</th>
          </tr>
        </thead>
        <tbody>
          {sortedRollup.map(g => {
            const key = groupKey(g.vertical, g.brand);
            const isOpen = expanded === key;
            return (
              <React.Fragment key={key}>
                <tr onClick={() => toggleGroup(key)} style={{ cursor: 'pointer', background: isOpen ? 'rgba(99,102,241,0.10)' : undefined }}>
                  <td style={td}><FontAwesomeIcon icon={isOpen ? faChevronDown : faChevronRight} style={{ color: '#6366f1', fontSize: 12 }} /></td>
                  <td style={td}>
                    <div>{VERTICAL_LABEL[g.vertical] ?? g.vertical}</div>
                    <div style={{ fontSize: 11, color: '#a5b4fc', fontFamily: 'ui-monospace, monospace' }}>{g.brand.toUpperCase()}</div>
                  </td>
                  <td style={tdNum}>
                    {g.waves.toLocaleString()}
                    {g.active > 0 && <span style={{ fontSize: 11, color: '#60a5fa', marginLeft: 5 }}>({g.active} live)</span>}
                  </td>
                  <td style={td}><SentProgress sent={g.sent} planned={g.recipients} status={g.active > 0 ? 'sending' : 'sent'} /></td>
                  <td style={tdNum}><RateCell count={g.delivered} denom={g.sent} color="#10b981" bold /></td>
                  <td style={tdNum}><RateCell count={g.hard_bounces} denom={g.sent} color="#ef4444" bold={g.hard_bounces > 0} warnAt={1} /></td>
                  <td style={tdNum}><RateCell count={g.soft_bounces} denom={g.sent} color="#f59e0b" /></td>
                  <td style={tdNum}>{g.opens.toLocaleString()}</td>
                  <td style={tdNum}><span style={{ color: '#a78bfa', fontWeight: g.clicks > 0 ? 600 : 400 }}>{g.clicks.toLocaleString()}</span></td>
                  <td style={td}>{relativeTime(g.last_wave_at)}</td>
                </tr>
                {isOpen && (
                  <tr>
                    <td colSpan={10} style={{ padding: 0, borderTop: 'none' }}>
                      <GroupDetail group={g} series={series.filter(s => s.vertical === g.vertical && s.brand === g.brand)} waves={groupWaves} />
                    </td>
                  </tr>
                )}
              </React.Fragment>
            );
          })}
          {sortedRollup.length === 0 && (
            <tr><td style={{ ...td, textAlign: 'center', color: 'rgba(180,210,240,0.5)' }} colSpan={10}>No send batches in the last {WINDOW_HOURS}h.</td></tr>
          )}
        </tbody>
      </table>

      {/* ───── Inbound flow roll-up ───── */}
      <div style={{ marginTop: 22 }}>
        <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.7)', marginBottom: 8 }}>
          <FontAwesomeIcon icon={faInbox} style={{ marginRight: 6 }} />
          <b style={{ color: '#dbeafe' }}>Inbound Flow — last 24h.</b> Partners post leads in real time, often <b>one record per API call</b> — each "batch" is one POST, not a send. This rolls them up per feed; per-post detail lives in the Inbound Batches tab.
        </div>
        <table style={tableStyle}>
          <thead>
            <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
              <th style={th}>Partner / Dataset</th>
              <th style={th}>Vertical</th>
              <th style={thNum} title="API posts received in the last 24h">Posts</th>
              <th style={thNum} title="Total lead records across those posts">Leads</th>
              <th style={thNum} title="Posts still being processed/verified">In-flight</th>
              <th style={th}>Last received</th>
            </tr>
          </thead>
          <tbody>
            {ingest.map(g => (
              <tr key={g.dataset_id}>
                <td style={td}>
                  <div style={{ fontWeight: 600 }}>{g.partner_name}</div>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>{g.dataset_name}</div>
                </td>
                <td style={td}>{VERTICAL_LABEL[g.vertical] ?? g.vertical}</td>
                <td style={tdNum}>{g.posts.toLocaleString()}</td>
                <td style={tdNum}><b style={{ color: '#10b981' }}>{g.records.toLocaleString()}</b></td>
                <td style={tdNum}>
                  {g.stopped > 0
                    ? <span style={{ color: '#f59e0b', fontWeight: 600 }}>{g.stopped} stopped</span>
                    : <span style={{ color: g.in_flight > 0 ? '#a78bfa' : 'rgba(180,210,240,0.5)' }}>{g.in_flight.toLocaleString()}</span>}
                </td>
                <td style={td}>{g.last_received ? relativeTime(g.last_received) : '—'}</td>
              </tr>
            ))}
            {ingest.length === 0 && (
              <tr><td style={{ ...td, textAlign: 'center', color: 'rgba(180,210,240,0.5)' }} colSpan={6}>No inbound posts in the last 24h.</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
};

// ───── Expanded group: chart + individual waves ─────

const GroupDetail: React.FC<{ group: RollupGroup; series: SeriesPoint[]; waves: Wave[] }> = ({ group, series, waves }) => {
  const chartData = series.map(s => ({
    hour: new Date(s.hour).toLocaleTimeString([], { hour: 'numeric' }),
    Sent: s.sent,
    Delivered: s.delivered,
    Hard: s.hard_bounces,
    Soft: s.soft_bounces,
    Opens: s.opens,
    Clicks: s.clicks,
  }));

  return (
    <div style={{ background: 'rgba(0,0,0,0.22)', borderTop: '1px solid rgba(99,102,241,0.25)', borderBottom: '1px solid rgba(99,102,241,0.25)', padding: '14px 16px' }}>
      <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', marginBottom: 6 }}>
        Hourly delivery & performance — {VERTICAL_LABEL[group.vertical] ?? group.vertical} / {group.brand.toUpperCase()} (delivery activity as received)
      </div>
      {chartData.length > 0 ? (
        <ResponsiveContainer width="100%" height={210}>
          <ComposedChart data={chartData} margin={{ top: 4, right: 12, left: -14, bottom: 0 }}>
            <CartesianGrid stroke="rgba(120,150,200,0.12)" vertical={false} />
            <XAxis dataKey="hour" tick={{ fill: 'rgba(180,210,240,0.6)', fontSize: 11 }} stroke="rgba(120,150,200,0.25)" />
            <YAxis yAxisId="vol" tick={{ fill: 'rgba(180,210,240,0.6)', fontSize: 11 }} stroke="rgba(120,150,200,0.25)" />
            <YAxis yAxisId="eng" orientation="right" tick={{ fill: 'rgba(180,210,240,0.6)', fontSize: 11 }} stroke="rgba(120,150,200,0.25)" />
            <Tooltip
              contentStyle={{ background: '#0f1c33', border: '1px solid rgba(120,150,200,0.3)', borderRadius: 6, fontSize: 12 }}
              labelStyle={{ color: '#dbeafe' }}
            />
            <Legend wrapperStyle={{ fontSize: 12 }} />
            <Bar yAxisId="vol" dataKey="Sent" fill="#6366f1" radius={[2, 2, 0, 0]} />
            <Bar yAxisId="vol" dataKey="Delivered" fill="#10b981" radius={[2, 2, 0, 0]} />
            <Line yAxisId="eng" type="monotone" dataKey="Hard" stroke="#ef4444" strokeWidth={2} dot={false} />
            <Line yAxisId="eng" type="monotone" dataKey="Soft" stroke="#f59e0b" strokeWidth={2} dot={false} />
            <Line yAxisId="eng" type="monotone" dataKey="Opens" stroke="#60a5fa" strokeWidth={2} dot={false} />
            <Line yAxisId="eng" type="monotone" dataKey="Clicks" stroke="#a78bfa" strokeWidth={2} dot={false} />
          </ComposedChart>
        </ResponsiveContainer>
      ) : (
        <div style={{ color: 'rgba(180,210,240,0.5)', fontSize: 12, padding: '18px 0' }}>No activity recorded for this lane yet — the graph fills in as delivery activity arrives.</div>
      )}

      <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', margin: '12px 0 6px' }}>
        Individual send batches (newest first, live)
      </div>
      <table style={{ ...tableStyle, background: 'rgba(15,30,60,0.5)' }}>
        <thead>
          <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
            <th style={th}>Batch</th>
            <th style={{ ...th, minWidth: 130 }}>Sent Progress</th>
            <th style={thNum}>Delivered</th>
            <th style={thNum}>Hard</th>
            <th style={thNum}>Soft</th>
            <th style={thNum}>Defer</th>
            <th style={thNum}>Opens</th>
            <th style={thNum}>Clicks</th>
            <th style={th}>Status</th>
          </tr>
        </thead>
        <tbody>
          {waves.map(wv => (
            <tr key={wv.campaign_id}>
              <td style={td}>
                <div style={{ fontWeight: 600 }}>{relativeTime(wv.scheduled_at)}</div>
                <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>
                  {wv.partner_name || wv.partner_slug}{wv.dataset_name ? ` / ${wv.dataset_name}` : ''}
                </div>
              </td>
              <td style={td}><SentProgress sent={wv.sent} planned={wv.total_recipients} status={wv.status} /></td>
              <td style={tdNum}><RateCell count={wv.delivered} denom={wv.sent} color="#10b981" bold /></td>
              <td style={tdNum}><RateCell count={wv.hard_bounces} denom={wv.sent} color="#ef4444" bold={wv.hard_bounces > 0} warnAt={1} /></td>
              <td style={tdNum}><span style={{ color: '#f59e0b' }}>{wv.soft_bounces.toLocaleString()}</span></td>
              <td style={tdNum}><span style={{ color: 'rgba(180,210,240,0.6)' }}>{wv.deferred.toLocaleString()}</span></td>
              <td style={tdNum}>{wv.opens.toLocaleString()}</td>
              <td style={tdNum}><span style={{ color: '#a78bfa', fontWeight: wv.clicks > 0 ? 600 : 400 }}>{wv.clicks.toLocaleString()}</span></td>
              <td style={td}><WaveStatus status={wv.status} /></td>
            </tr>
          ))}
          {waves.length === 0 && (
            <tr><td style={{ ...td, textAlign: 'center', color: 'rgba(180,210,240,0.5)' }} colSpan={9}>Loading send batches…</td></tr>
          )}
        </tbody>
      </table>
    </div>
  );
};

// ───── Overall strip ─────

const OverallStrip: React.FC<{
  totals: Totals24h;
  overall: { ready: number; inDrip: number; engaged: number; dueNow: number };
}> = ({ totals, overall }) => {
  const pct = (n: number) => totals.sent > 0 ? `${((n / totals.sent) * 100).toFixed(1)}%` : '—';
  return (
    <div style={overallBox}>
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', textTransform: 'uppercase', letterSpacing: 0.6, marginBottom: 8 }}>
        Overall — all follow-ups, last 24h ({totals.waves.toLocaleString()} send batches)
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(110px, 1fr))', gap: 8 }}>
        <BigStat label="Sent" value={totals.sent.toLocaleString()} />
        <BigStat label="Delivered" value={totals.delivered.toLocaleString()} sub={pct(totals.delivered)} accent="#10b981" />
        <BigStat label="Hard bounce" value={totals.hard_bounces.toLocaleString()} sub={pct(totals.hard_bounces)} accent="#ef4444" />
        <BigStat label="Soft bounce" value={totals.soft_bounces.toLocaleString()} sub={pct(totals.soft_bounces)} accent="#f59e0b" />
        <BigStat label="Opens" value={totals.opens.toLocaleString()} title="Raw opens — includes automated privacy and scanner traffic" />
        <BigStat label="Clicks" value={totals.clicks.toLocaleString()} accent="#a78bfa" />
        <BigStat label="Ready pending" value={overall.ready.toLocaleString()} title="Verified leads waiting for their first send" />
        <BigStat label="In follow-up" value={overall.inDrip.toLocaleString()} title="Recipients somewhere in the T1–T4 journey" />
        <BigStat label="Engaged" value={overall.engaged.toLocaleString()} accent="#10b981" title="Opened/clicked — exited the follow-up sequence as a win" />
        <BigStat label="Due now" value={overall.dueNow.toLocaleString()} accent={overall.dueNow > 0 ? '#f59e0b' : undefined} title="Follow-up touches past their scheduled time" />
      </div>
    </div>
  );
};

const BigStat: React.FC<{ label: string; value: string; sub?: string; accent?: string; title?: string }> = ({ label, value, sub, accent, title }) => (
  <div title={title} style={{ background: 'rgba(0,0,0,0.22)', padding: '10px 8px', borderRadius: 6, textAlign: 'center' }}>
    <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 18, fontWeight: 700, marginTop: 2, color: accent ?? '#dbeafe' }}>{value}</div>
    {sub && <div style={{ fontSize: 11, color: accent ?? 'rgba(180,210,240,0.55)', marginTop: 1 }}>{sub}</div>}
  </div>
);

// ───── Shared cells ─────

// SentProgress renders "sent / planned" with a fill bar. While a lane is
// actively sending the bar visibly grows poll over poll.
const SentProgress: React.FC<{ sent: number; planned: number; status: string }> = ({ sent, planned, status }) => {
  const denom = Math.max(planned, sent);
  const pct = denom > 0 ? Math.min(100, (sent / denom) * 100) : 0;
  const active = status === 'sending';
  return (
    <div>
      <div style={{ fontVariantNumeric: 'tabular-nums', fontSize: 13 }}>
        <b>{sent.toLocaleString()}</b>
        <span style={{ color: 'rgba(180,210,240,0.5)' }}> / {denom.toLocaleString()}</span>
      </div>
      <div style={{ height: 4, borderRadius: 2, background: 'rgba(0,0,0,0.3)', marginTop: 3, overflow: 'hidden' }}>
        <div style={{
          width: `${pct}%`, height: '100%',
          background: active ? '#60a5fa' : (pct >= 100 ? '#10b981' : '#6366f1'),
          transition: 'width 0.8s ease',
        }} />
      </div>
    </div>
  );
};

// RateCell shows a count with its rate against SENT. Shows "—" for the rate
// until any mail has actually gone out, so a queued lane never reads as 0%.
const RateCell: React.FC<{ count: number; denom: number; color: string; bold?: boolean; warnAt?: number }> = ({ count, denom, color, bold, warnAt }) => {
  const rate = denom > 0 ? (count / denom) * 100 : null;
  const hot = warnAt !== undefined && rate !== null && rate >= warnAt;
  return (
    <span>
      <span style={{ color, fontWeight: bold || hot ? 700 : 400 }}>{count.toLocaleString()}</span>
      <span style={{ fontSize: 11, color: hot ? color : 'rgba(180,210,240,0.5)', marginLeft: 4 }}>
        {rate === null ? '—' : `${rate.toFixed(rate >= 10 ? 0 : 1)}%`}
      </span>
    </span>
  );
};

// ───── Funnel card ─────

const FunnelCard: React.FC<{ v: FunnelVertical; isps: FunnelISP[] }> = ({ v, isps }) => {
  const touchTotal = v.touch_1 + v.touch_2 + v.touch_3 + v.touch_4;
  // Three mutually-exclusive outcome buckets that partition the drip and sum to
  // 100%: engaged (a win) + in-progress (still mailing) + churned (exhausted,
  // no engagement). Backend supplies in_progress/churned; fall back to deriving
  // them so an older payload still renders sensibly.
  const inProgress = v.in_progress ?? Math.max(0, touchTotal - v.engaged - v.completed);
  const churned = v.churned ?? Math.max(0, touchTotal - v.engaged - inProgress);
  // Rate denominator = the mailed-into-drip population (the bucket sum), NOT
  // touch_count — touch_count can be 0 on mailed rows and would break the rates.
  const dripDenom = v.drip_total ?? (v.engaged + inProgress + churned);
  // Show ALL ISPs (sent first, then ready) so the column reconciles to the
  // "sent / 24h" headline and large held backlogs (e.g. gmail) stay visible.
  const allISPs = [...isps].sort((a, b) => (b.sent_24h - a.sent_24h) || (b.ready - a.ready));
  return (
    <div style={card}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 10 }}>
        <h4 style={{ margin: 0, color: '#dbeafe', fontSize: 14 }}>{VERTICAL_LABEL[v.vertical] ?? v.vertical}</h4>
        <span style={{ fontSize: 12, color: '#10b981', fontWeight: 600 }}>{v.sent_24h.toLocaleString()} sent / 24h</span>
      </div>

      {/* Lifecycle: pending → ready → mailed */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(5, 1fr)', gap: 6, marginBottom: 10 }}>
        <MiniStat label="Pending verification" value={v.pending_eo} accent="#a78bfa" />
        <MiniStat
          label="Verification credits"
          value={v.eo_credits_total ?? 0}
          accent="#60a5fa"
          sub={`${(v.eo_validated_24h ?? 0).toLocaleString()} val. 24h`}
          title="Email verification calls consumed (billed per call); sub-text = leads validated in the last 24h"
        />
        <MiniStat label="Ready" value={v.ready} accent="#10b981" />
        <MiniStat label="Mailed" value={v.mailed} accent="#6366f1" />
        <MiniStat label="Due now" value={v.followups_due} accent={v.followups_due > 0 ? '#f59e0b' : undefined} title="Follow-up touches past their next scheduled time" />
      </div>

      {/* Multi-touch state machine */}
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 4 }}>
        Touch journey ({touchTotal.toLocaleString()} in follow-up)
      </div>
      <TouchBar t1={v.touch_1} t2={v.touch_2} t3={v.touch_3} t4={v.touch_4} />
      <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, marginTop: 8 }}>
        <span title="Clicked — exited the follow-up sequence as a win">
          Engaged: <b style={{ color: '#10b981' }}>{v.engaged.toLocaleString()}</b>
        </span>
        <span title="Still being mailed — touches 1-3, not yet engaged or exhausted">
          In progress: <b style={{ color: '#60a5fa' }}>{inProgress.toLocaleString()}</b>
        </span>
        <span title="Conversions tied to this dataset's records — CPM, $0 revenue by design">
          Conv: <b style={{ color: (v.conversions ?? 0) > 0 ? '#f59e0b' : 'rgba(180,210,240,0.55)' }}>{(v.conversions ?? 0).toLocaleString()}</b>
        </span>
      </div>
      {/* Three outcome buckets — engaged + in-progress + churn partition the drip
          and sum to 100% (denominator = records in drip, not mailed). */}
      <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, marginTop: 4 }}>
        <span
          title="activation = clicked and exited the follow-up sequence as a win"
          style={{ color: '#10b981', fontWeight: 600 }}
        >
          Activation {ratePct(v.engaged, dripDenom)}
        </span>
        <span
          title="in progress = still being mailed (touches 1-3), no outcome yet"
          style={{ color: '#60a5fa', fontWeight: 600 }}
        >
          In-Progress {ratePct(inProgress, dripDenom)}
        </span>
        <span
          title="churn = exhausted all touches without ever engaging"
          style={{ color: churned > 0 ? '#f59e0b' : 'rgba(180,210,240,0.55)', fontWeight: 600 }}
        >
          Churn {ratePct(churned, dripDenom)}
        </span>
      </div>

      {allISPs.length > 0 && (
        <>
          <div style={{ height: 1, background: 'rgba(120,150,200,0.12)', margin: '10px 0' }} />
          <table style={{ width: '100%', fontSize: 11, borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ color: 'rgba(180,210,240,0.5)', textTransform: 'uppercase', letterSpacing: 0.5 }}>
                <th style={ispTh}>Mailbox provider</th>
                <th style={ispThNum}>Ready</th>
                <th style={ispThNum}>Sent 24h</th>
              </tr>
            </thead>
            <tbody>
              {allISPs.map(i => (
                <tr key={i.isp}>
                  <td style={ispTd}>{i.isp}</td>
                  <td style={{ ...ispTdNum, color: i.ready > 50000 ? '#f59e0b' : undefined }} title={i.ready > 50000 ? 'large held/queued pending' : undefined}>{i.ready.toLocaleString()}</td>
                  <td style={{ ...ispTdNum, color: i.sent_24h > 0 ? '#10b981' : undefined }}>{i.sent_24h.toLocaleString()}</td>
                </tr>
              ))}
              <tr style={{ borderTop: '1px solid rgba(120,150,200,0.18)' }}>
                <td style={{ ...ispTd, color: 'rgba(180,210,240,0.5)', textTransform: 'uppercase', letterSpacing: 0.5, fontSize: 10 }}>Total</td>
                <td style={{ ...ispTdNum, color: 'rgba(180,210,240,0.7)' }}>{allISPs.reduce((s, i) => s + i.ready, 0).toLocaleString()}</td>
                <td style={{ ...ispTdNum, color: 'rgba(180,210,240,0.7)' }}>{allISPs.reduce((s, i) => s + i.sent_24h, 0).toLocaleString()}</td>
              </tr>
            </tbody>
          </table>
        </>
      )}
    </div>
  );
};

const TOUCH_COLORS = ['#6366f1', '#8b5cf6', '#a78bfa', '#c4b5fd'];

const TouchBar: React.FC<{ t1: number; t2: number; t3: number; t4: number }> = ({ t1, t2, t3, t4 }) => {
  const parts = [t1, t2, t3, t4];
  const total = parts.reduce((a, b) => a + b, 0);
  return (
    <div>
      <div style={{ display: 'flex', height: 10, borderRadius: 5, overflow: 'hidden', background: 'rgba(0,0,0,0.25)' }}>
        {total > 0 && parts.map((n, i) => (
          n > 0 ? <div key={i} style={{ width: `${(n / total) * 100}%`, background: TOUCH_COLORS[i] }} title={`T${i + 1}: ${n.toLocaleString()}`} /> : null
        ))}
      </div>
      <div style={{ display: 'flex', gap: 10, marginTop: 4, fontSize: 11, color: 'rgba(180,210,240,0.65)' }}>
        {parts.map((n, i) => (
          <span key={i} style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
            <span style={{ width: 8, height: 8, borderRadius: 2, background: TOUCH_COLORS[i], display: 'inline-block' }} />
            T{i + 1} {n.toLocaleString()}
          </span>
        ))}
      </div>
    </div>
  );
};

const MiniStat: React.FC<{ label: string; value: number; accent?: string; title?: string; sub?: string }> = ({ label, value, accent, title, sub }) => (
  <div title={title} style={{ background: 'rgba(0,0,0,0.2)', padding: 6, borderRadius: 4, textAlign: 'center' }}>
    <div style={{ fontSize: 9, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 15, fontWeight: 700, marginTop: 2, color: accent ?? '#dbeafe' }}>{value.toLocaleString()}</div>
    {sub && <div style={{ fontSize: 9, color: 'rgba(180,210,240,0.55)', marginTop: 1 }}>{sub}</div>}
  </div>
);

// ratePct renders num/denom as a percent, guarding the empty-lane case so a
// vertical with zero mailed reads "—" instead of NaN%.
const ratePct = (num: number, denom: number): string =>
  denom > 0 ? `${((num / denom) * 100).toFixed(1)}%` : '—';

const WaveStatus: React.FC<{ status: string }> = ({ status }) => {
  const colors: Record<string, string> = {
    sending: '#60a5fa',
    scheduled: '#a78bfa',
    preparing: '#a78bfa',
    finalizing_audience: '#a78bfa',
    sent: '#10b981',
    completed: '#10b981',
    failed: '#ef4444',
    cancelled: 'rgba(180,210,240,0.5)',
  };
  return <span style={{ color: colors[status] ?? '#cbd5f5', fontWeight: 500, fontSize: 12 }}>{status}</span>;
};

function relativeTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  const mins = Math.round((Date.now() - d.getTime()) / 60000);
  if (mins < 0) return `in ${-mins}m`;
  if (mins < 1) return 'just now';
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs}h ${mins % 60}m ago`;
  return d.toLocaleString();
}

// ───── Styles ─────

const card: React.CSSProperties = {
  background: 'linear-gradient(135deg, rgba(15,30,60,0.65) 0%, rgba(20,40,80,0.5) 100%)',
  border: '1px solid rgba(120,150,200,0.18)',
  borderRadius: 10, padding: 14,
};
const overallBox: React.CSSProperties = {
  background: 'linear-gradient(135deg, rgba(30,40,90,0.55) 0%, rgba(50,30,90,0.45) 100%)',
  border: '1px solid rgba(99,102,241,0.35)',
  borderRadius: 10, padding: 14, marginBottom: 16,
};
const tableStyle: React.CSSProperties = {
  width: '100%', borderCollapse: 'collapse', background: 'rgba(15,30,60,0.35)',
  borderRadius: 8, overflow: 'hidden', fontSize: 13,
};
const th: React.CSSProperties = {
  textAlign: 'left', padding: '8px 10px', color: 'rgba(180,210,240,0.65)',
  fontWeight: 500, fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
};
const thNum: React.CSSProperties = { ...th, textAlign: 'right' };
const td: React.CSSProperties = { padding: '8px 10px', borderTop: '1px solid rgba(120,150,200,0.1)' };
const tdNum: React.CSSProperties = { ...td, fontVariantNumeric: 'tabular-nums', textAlign: 'right' };
const ispTh: React.CSSProperties = { textAlign: 'left', padding: '2px 4px', fontWeight: 500 };
const ispThNum: React.CSSProperties = { textAlign: 'right', padding: '2px 4px', fontWeight: 500 };
const ispTd: React.CSSProperties = { padding: '2px 4px' };
const ispTdNum: React.CSSProperties = { padding: '2px 4px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
