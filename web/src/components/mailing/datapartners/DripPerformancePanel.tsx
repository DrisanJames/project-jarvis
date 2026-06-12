import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCircle, faFilter, faPaperPlane, faExclamationTriangle, faInbox,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

// DripPerformancePanel — the live "how are the drips actually doing" view on
// the Data Partners screen. Two cadences against one endpoint:
//   - funnel + inbound-flow roll-up every 30s — whole-queue scan
//   - waves + 24h totals every 10s — re-aggregated server-side from tracking
//     events on each poll, so numbers move as PMTA accounting events are
//     ingested ("stats as they are received")
// Bounces are ALWAYS split hard (red) vs soft (amber) — never combined.
// Rates are computed against SENT (mail actually dispatched), never against
// planned recipients — a wave mid-send shows honest progress, not a fake
// low delivery rate.

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
  const [ingest, setIngest] = useState<Ingest24h[]>([]);
  const [waves, setWaves] = useState<Wave[]>([]);
  const [totals, setTotals] = useState<Totals24h | null>(null);
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
        if (data?.totals_24h) setTotals(data.totals_24h);
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
        if (Array.isArray(data?.ingest_24h)) setIngest(data.ingest_24h);
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

  // Cross-vertical funnel roll-up for the overall strip.
  const sum = (f: (v: FunnelVertical) => number) => funnel.reduce((a, v) => a + f(v), 0);
  const overall = {
    ready: sum(v => v.ready),
    inDrip: sum(v => v.touch_1 + v.touch_2 + v.touch_3 + v.touch_4),
    engaged: sum(v => v.engaged),
    dueNow: sum(v => v.followups_due),
  };

  return (
    <div style={{ marginBottom: 28 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', borderBottom: '1px solid rgba(120,150,200,0.18)', paddingBottom: 6, marginBottom: 12 }}>
        <h3 style={{ color: '#dbeafe', margin: 0 }}>Drip Performance</h3>
        <div style={{ display: 'flex', alignItems: 'center', gap: 14, fontSize: 12 }}>
          <span style={{ color: '#10b981', display: 'inline-flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faCircle} style={{ fontSize: 8 }} beat={secondsAgo !== null && secondsAgo < 3} />
            LIVE — re-aggregated from tracking events every {WAVE_POLL_MS / 1000}s
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

      {/* ───── Live wave stream ───── */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
        <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.7)' }}>
          <FontAwesomeIcon icon={faPaperPlane} style={{ marginRight: 6 }} />
          Waves — last 48h, newest first. <b>Sent</b> = mail dispatched so far (fills in while a wave sends); rates are against sent, not planned. Rows flash as new events land.
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
            <th style={{ ...th, minWidth: 140 }} title="Mail dispatched so far / planned recipients">Sent Progress</th>
            <th style={thNum} title="Delivered, as % of sent">Delivered</th>
            <th style={thNum} title="Hard bounces — permanent failures, reputation risk (% of sent)">Hard</th>
            <th style={thNum} title="Soft bounces — usually transient">Soft</th>
            <th style={thNum} title="Deferrals — ISP slow-walking">Defer</th>
            <th style={thNum} title="Raw opens — includes Apple MPP / scanner machine traffic">Opens</th>
            <th style={thNum}>Clicks</th>
            <th style={th}>Status</th>
          </tr>
        </thead>
        <tbody>
          {visibleWaves.map(wv => {
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
                <td style={td}><SentProgress sent={wv.sent} planned={wv.total_recipients} status={wv.status} /></td>
                <td style={tdNum}><RateCell count={wv.delivered} denom={wv.sent} color="#10b981" bold /></td>
                <td style={tdNum}><RateCell count={wv.hard_bounces} denom={wv.sent} color="#ef4444" bold={wv.hard_bounces > 0} warnAt={1} /></td>
                <td style={tdNum}><RateCell count={wv.soft_bounces} denom={wv.sent} color="#f59e0b" /></td>
                <td style={tdNum}><span style={{ color: 'rgba(180,210,240,0.6)' }}>{wv.deferred.toLocaleString()}</span></td>
                <td style={tdNum}>{wv.opens.toLocaleString()}</td>
                <td style={tdNum}><span style={{ color: '#a78bfa', fontWeight: wv.clicks > 0 ? 600 : 400 }}>{wv.clicks.toLocaleString()}</span></td>
                <td style={td}><WaveStatus status={wv.status} /></td>
              </tr>
            );
          })}
          {visibleWaves.length === 0 && (
            <tr><td style={{ ...td, textAlign: 'center', color: 'rgba(180,210,240,0.5)' }} colSpan={10}>No waves in the last 48h.</td></tr>
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
              <th style={thNum} title="Posts still being sliced/validated">In-flight</th>
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

// ───── Overall strip ─────

const OverallStrip: React.FC<{
  totals: Totals24h;
  overall: { ready: number; inDrip: number; engaged: number; dueNow: number };
}> = ({ totals, overall }) => {
  const pct = (n: number) => totals.sent > 0 ? `${((n / totals.sent) * 100).toFixed(1)}%` : '—';
  return (
    <div style={overallBox}>
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', textTransform: 'uppercase', letterSpacing: 0.6, marginBottom: 8 }}>
        Overall — all drips, last 24h ({totals.waves.toLocaleString()} waves)
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(110px, 1fr))', gap: 8 }}>
        <BigStat label="Sent" value={totals.sent.toLocaleString()} />
        <BigStat label="Delivered" value={totals.delivered.toLocaleString()} sub={pct(totals.delivered)} accent="#10b981" />
        <BigStat label="Hard bounce" value={totals.hard_bounces.toLocaleString()} sub={pct(totals.hard_bounces)} accent="#ef4444" />
        <BigStat label="Soft bounce" value={totals.soft_bounces.toLocaleString()} sub={pct(totals.soft_bounces)} accent="#f59e0b" />
        <BigStat label="Opens" value={totals.opens.toLocaleString()} title="Raw opens — includes Apple MPP / scanner machine traffic" />
        <BigStat label="Clicks" value={totals.clicks.toLocaleString()} accent="#a78bfa" />
        <BigStat label="Ready backlog" value={overall.ready.toLocaleString()} title="Cleaned leads waiting for their first wave" />
        <BigStat label="In drip" value={overall.inDrip.toLocaleString()} title="Recipients somewhere in the T1–T4 journey" />
        <BigStat label="Engaged" value={overall.engaged.toLocaleString()} accent="#10b981" title="Opened/clicked — exited the drip as a win" />
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

// ───── Wave cells ─────

// SentProgress renders "sent / planned" with a fill bar. While a wave is
// sending the bar visibly grows poll over poll; a finished wave reads as a
// plain complete count.
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
// until any mail has actually gone out, so a queued wave never reads as 0%.
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
const selectStyle: React.CSSProperties = {
  background: 'rgba(0,0,0,0.25)', color: 'rgba(220,235,250,0.9)',
  border: '1px solid rgba(120,150,200,0.25)', borderRadius: 4, padding: '4px 8px', fontSize: 12,
};
const ispTh: React.CSSProperties = { textAlign: 'left', padding: '2px 4px', fontWeight: 500 };
const ispThNum: React.CSSProperties = { textAlign: 'right', padding: '2px 4px', fontWeight: 500 };
const ispTd: React.CSSProperties = { padding: '2px 4px' };
const ispTdNum: React.CSSProperties = { padding: '2px 4px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
