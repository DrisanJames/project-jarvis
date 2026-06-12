import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCircle, faFilter, faPaperPlane, faExclamationTriangle,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

// DripPerformancePanel — the live "how are the drips actually doing" view on
// the Data Partners screen. Two cadences against one endpoint:
//   - funnel (queue lifecycle + multi-touch state) every 30s — whole-queue scan
//   - waves (per-wave delivery stats from tracking events) every 10s — stats
//     re-aggregate server-side on each poll, so numbers move as PMTA
//     accounting events are ingested ("stats as they are received")
// Bounces are ALWAYS split hard (red) vs soft (amber) — never combined.

const VERTICAL_LABEL: Record<string, string> = {
  refi_heloc: 'Refi / HELOC',
  personal_loans: 'Personal Loans',
  tax_relief: 'Tax Relief',
  remodel: 'Remodel',
};

const WAVE_POLL_MS = 10_000;
const FUNNEL_POLL_MS = 30_000;

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
  followups_due: number;
}

interface FunnelISP {
  vertical: string;
  isp: string;
  ready: number;
  mailed: number;
  sent_24h: number;
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

export const DripPerformancePanel: React.FC = () => {
  const [funnel, setFunnel] = useState<FunnelVertical[]>([]);
  const [funnelISP, setFunnelISP] = useState<FunnelISP[]>([]);
  const [waves, setWaves] = useState<Wave[]>([]);
  const [lastWaveFetch, setLastWaveFetch] = useState<Date | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [verticalFilter, setVerticalFilter] = useState<string>('all');
  // Tick state so the "updated Xs ago" label re-renders between polls.
  const [, setTick] = useState(0);
  const prevWaves = useRef<Map<string, Wave>>(new Map());
  const [changedIds, setChangedIds] = useState<Set<string>>(new Set());

  const fetchWaves = useCallback(() => {
    apiFetch('/api/mailing/data-partners/drip-performance?include=waves&hours=48&limit=40', { credentials: 'include' })
      .then(r => r.json())
      .then(data => {
        if (data?.waves_error) { setError(`Waves: ${data.waves_error}`); return; }
        if (!Array.isArray(data?.waves)) return;
        const next: Wave[] = data.waves;
        // Highlight rows whose live counters moved since the previous poll so
        // the operator can SEE stats arriving.
        const changed = new Set<string>();
        next.forEach(wv => {
          const prev = prevWaves.current.get(wv.campaign_id);
          if (prev && (
            prev.sent !== wv.sent || prev.delivered !== wv.delivered ||
            prev.opens !== wv.opens || prev.clicks !== wv.clicks ||
            prev.hard_bounces !== wv.hard_bounces || prev.soft_bounces !== wv.soft_bounces
          )) changed.add(wv.campaign_id);
        });
        prevWaves.current = new Map(next.map(wv => [wv.campaign_id, wv]));
        setChangedIds(changed);
        setWaves(next);
        setLastWaveFetch(new Date());
        setError(null);
      })
      .catch(err => setError(String(err)));
  }, []);

  const fetchFunnel = useCallback(() => {
    apiFetch('/api/mailing/data-partners/drip-performance?include=funnel', { credentials: 'include' })
      .then(r => r.json())
      .then(data => {
        if (data?.funnel_error) { setError(`Funnel: ${data.funnel_error}`); return; }
        if (Array.isArray(data?.funnel)) setFunnel(data.funnel);
        if (Array.isArray(data?.funnel_isp)) setFunnelISP(data.funnel_isp);
      })
      .catch(err => setError(String(err)));
  }, []);

  useEffect(() => {
    fetchWaves();
    fetchFunnel();
    const tw = setInterval(fetchWaves, WAVE_POLL_MS);
    const tf = setInterval(fetchFunnel, FUNNEL_POLL_MS);
    const tt = setInterval(() => setTick(t => t + 1), 1000);
    return () => { clearInterval(tw); clearInterval(tf); clearInterval(tt); };
  }, [fetchWaves, fetchFunnel]);

  const visibleWaves = verticalFilter === 'all' ? waves : waves.filter(wv => wv.vertical === verticalFilter);
  const verticalOptions = ['all', ...Array.from(new Set(waves.map(wv => wv.vertical).filter(Boolean)))];
  const secondsAgo = lastWaveFetch ? Math.max(0, Math.round((Date.now() - lastWaveFetch.getTime()) / 1000)) : null;

  return (
    <div style={{ marginBottom: 28 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', borderBottom: '1px solid rgba(120,150,200,0.18)', paddingBottom: 6, marginBottom: 12 }}>
        <h3 style={{ color: '#dbeafe', margin: 0 }}>Drip Performance</h3>
        <div style={{ display: 'flex', alignItems: 'center', gap: 14, fontSize: 12 }}>
          <span style={{ color: '#10b981', display: 'inline-flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faCircle} style={{ fontSize: 8 }} beat={secondsAgo !== null && secondsAgo < 3} />
            LIVE — wave stats re-aggregate from tracking events every {WAVE_POLL_MS / 1000}s
            {secondsAgo !== null && <span style={{ color: 'rgba(180,210,240,0.55)' }}>(updated {secondsAgo}s ago)</span>}
          </span>
        </div>
      </div>

      {error && (
        <div style={{ background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)', padding: 10, borderRadius: 6, marginBottom: 12, fontSize: 13 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {error}
        </div>
      )}

      {/* ───── Per-vertical lifecycle funnels ───── */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))', gap: 14, marginBottom: 18 }}>
        {funnel.map(v => <FunnelCard key={v.vertical} v={v} isps={funnelISP.filter(i => i.vertical === v.vertical)} />)}
        {funnel.length === 0 && (
          <div style={{ color: 'rgba(180,210,240,0.55)', fontSize: 13 }}>No queue data yet.</div>
        )}
      </div>

      {/* ───── Live wave stream ───── */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
        <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.7)' }}>
          <FontAwesomeIcon icon={faPaperPlane} style={{ marginRight: 6 }} />
          Waves — last 48h, newest first. Rows flash when new events land. Delivery numbers come from PMTA accounting events, not campaign counters.
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12 }}>
          <FontAwesomeIcon icon={faFilter} style={{ color: 'rgba(180,210,240,0.55)' }} />
          <select value={verticalFilter} onChange={e => setVerticalFilter(e.target.value)} style={selectStyle}>
            {verticalOptions.map(v => (
              <option key={v} value={v}>{v === 'all' ? 'All verticals' : (VERTICAL_LABEL[v] ?? v)}</option>
            ))}
          </select>
        </div>
      </div>

      <table style={tableStyle}>
        <thead>
          <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
            <th style={th}>Wave</th>
            <th style={th}>Vertical / Brand</th>
            <th style={thNum}>Recipients</th>
            <th style={thNum}>Sent</th>
            <th style={thNum}>Delivered</th>
            <th style={thNum} title="Hard bounces — permanent failures, reputation risk">Hard</th>
            <th style={thNum} title="Soft bounces — usually transient">Soft</th>
            <th style={thNum} title="Deferrals — ISP slow-walking">Defer</th>
            <th style={thNum} title="Raw opens — includes Apple MPP / scanner machine traffic">Opens</th>
            <th style={thNum}>Clicks</th>
            <th style={th}>Status</th>
          </tr>
        </thead>
        <tbody>
          {visibleWaves.map(wv => {
            const attempted = Math.max(wv.sent, wv.total_recipients);
            const delivPct = attempted > 0 ? (wv.delivered / attempted) * 100 : null;
            const hardPct = attempted > 0 ? (wv.hard_bounces / attempted) * 100 : null;
            const flash = changedIds.has(wv.campaign_id);
            return (
              <tr key={wv.campaign_id} style={{ background: flash ? 'rgba(16,185,129,0.08)' : undefined, transition: 'background 1.5s ease' }}>
                <td style={td}>
                  <div style={{ fontWeight: 600 }}>{relativeTime(wv.scheduled_at)}</div>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>
                    {wv.partner_name || wv.partner_slug}{wv.dataset_name ? ` / ${wv.dataset_name}` : ''}
                  </div>
                </td>
                <td style={td}>
                  <div>{VERTICAL_LABEL[wv.vertical] ?? wv.vertical}</div>
                  <div style={{ fontSize: 11, color: '#a5b4fc', fontFamily: 'ui-monospace, monospace' }}>{wv.brand.toUpperCase()}</div>
                </td>
                <td style={tdNum}>{wv.total_recipients.toLocaleString()}</td>
                <td style={tdNum}>{wv.sent.toLocaleString()}</td>
                <td style={tdNum}>
                  <span style={{ color: '#10b981', fontWeight: 600 }}>{wv.delivered.toLocaleString()}</span>
                  {delivPct !== null && <span style={pctStyle}> {delivPct.toFixed(0)}%</span>}
                </td>
                <td style={tdNum}>
                  <span style={{ color: '#ef4444', fontWeight: wv.hard_bounces > 0 ? 700 : 400 }}>{wv.hard_bounces.toLocaleString()}</span>
                  {hardPct !== null && hardPct >= 1 && <span style={{ ...pctStyle, color: '#ef4444' }}> {hardPct.toFixed(1)}%</span>}
                </td>
                <td style={tdNum}><span style={{ color: '#f59e0b' }}>{wv.soft_bounces.toLocaleString()}</span></td>
                <td style={tdNum}><span style={{ color: 'rgba(180,210,240,0.6)' }}>{wv.deferred.toLocaleString()}</span></td>
                <td style={tdNum}>{wv.opens.toLocaleString()}</td>
                <td style={tdNum}><span style={{ color: '#a78bfa', fontWeight: wv.clicks > 0 ? 600 : 400 }}>{wv.clicks.toLocaleString()}</span></td>
                <td style={td}><WaveStatus status={wv.status} /></td>
              </tr>
            );
          })}
          {visibleWaves.length === 0 && (
            <tr><td style={{ ...td, textAlign: 'center', color: 'rgba(180,210,240,0.5)' }} colSpan={11}>No waves in the last 48h.</td></tr>
          )}
        </tbody>
      </table>
    </div>
  );
};

// ───── Funnel card ─────

const FunnelCard: React.FC<{ v: FunnelVertical; isps: FunnelISP[] }> = ({ v, isps }) => {
  const touchTotal = v.touch_1 + v.touch_2 + v.touch_3 + v.touch_4;
  const topISPs = [...isps].sort((a, b) => b.sent_24h - a.sent_24h).slice(0, 5);
  return (
    <div style={card}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 10 }}>
        <h4 style={{ margin: 0, color: '#dbeafe', fontSize: 14 }}>{VERTICAL_LABEL[v.vertical] ?? v.vertical}</h4>
        <span style={{ fontSize: 12, color: '#10b981', fontWeight: 600 }}>{v.sent_24h.toLocaleString()} sent / 24h</span>
      </div>

      {/* Lifecycle: pending → ready → mailed */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 6, marginBottom: 10 }}>
        <MiniStat label="Pending EO" value={v.pending_eo} accent="#a78bfa" />
        <MiniStat label="Ready" value={v.ready} accent="#10b981" />
        <MiniStat label="Mailed" value={v.mailed} accent="#6366f1" />
        <MiniStat label="Due now" value={v.followups_due} accent={v.followups_due > 0 ? '#f59e0b' : undefined} title="Follow-up touches past their next_touch_at" />
      </div>

      {/* Multi-touch state machine */}
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 4 }}>
        Touch journey ({touchTotal.toLocaleString()} in drip)
      </div>
      <TouchBar t1={v.touch_1} t2={v.touch_2} t3={v.touch_3} t4={v.touch_4} />
      <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, marginTop: 8 }}>
        <span title="Recipients who opened/clicked — exited the drip as a win">
          Engaged: <b style={{ color: '#10b981' }}>{v.engaged.toLocaleString()}</b>
        </span>
        <span title="All touches exhausted without engagement">
          Completed: <b style={{ color: 'rgba(180,210,240,0.8)' }}>{v.completed.toLocaleString()}</b>
        </span>
      </div>

      {topISPs.length > 0 && (
        <>
          <div style={{ height: 1, background: 'rgba(120,150,200,0.12)', margin: '10px 0' }} />
          <table style={{ width: '100%', fontSize: 11, borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ color: 'rgba(180,210,240,0.5)', textTransform: 'uppercase', letterSpacing: 0.5 }}>
                <th style={ispTh}>ISP</th>
                <th style={ispThNum}>Ready</th>
                <th style={ispThNum}>Sent 24h</th>
              </tr>
            </thead>
            <tbody>
              {topISPs.map(i => (
                <tr key={i.isp}>
                  <td style={ispTd}>{i.isp}</td>
                  <td style={ispTdNum}>{i.ready.toLocaleString()}</td>
                  <td style={{ ...ispTdNum, color: i.sent_24h > 0 ? '#10b981' : undefined }}>{i.sent_24h.toLocaleString()}</td>
                </tr>
              ))}
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

const MiniStat: React.FC<{ label: string; value: number; accent?: string; title?: string }> = ({ label, value, accent, title }) => (
  <div title={title} style={{ background: 'rgba(0,0,0,0.2)', padding: 6, borderRadius: 4, textAlign: 'center' }}>
    <div style={{ fontSize: 9, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 15, fontWeight: 700, marginTop: 2, color: accent ?? '#dbeafe' }}>{value.toLocaleString()}</div>
  </div>
);

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
const pctStyle: React.CSSProperties = { fontSize: 11, color: 'rgba(180,210,240,0.5)' };
const selectStyle: React.CSSProperties = {
  background: 'rgba(0,0,0,0.25)', color: 'rgba(220,235,250,0.9)',
  border: '1px solid rgba(120,150,200,0.25)', borderRadius: 4, padding: '4px 8px', fontSize: 12,
};
const ispTh: React.CSSProperties = { textAlign: 'left', padding: '2px 4px', fontWeight: 500 };
const ispThNum: React.CSSProperties = { textAlign: 'right', padding: '2px 4px', fontWeight: 500 };
const ispTd: React.CSSProperties = { padding: '2px 4px' };
const ispTdNum: React.CSSProperties = { padding: '2px 4px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
