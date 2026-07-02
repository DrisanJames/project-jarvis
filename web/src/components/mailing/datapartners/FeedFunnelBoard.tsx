import React, { useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRoute, faArrowRightArrowLeft, faTriangleExclamation, faXmark, faLayerGroup } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { labelForVertical } from './verticalLabels';

// FeedFunnelBoard — the Data Partners Overview decision surface. It answers, in
// one value-ranked view: which data FEEDS route into which FUNNELS, how each
// feed is performing (records → mailed → engaged), how engaged the data is, and
// which stream is most valuable — so a mis-routed or dead stream is obvious.
// Backed by GET /data-partners/feed-funnel (partner_clean_queue lineage). Value
// = engagement rate (engaged_at / mailed); re-route flags are advisory.

interface FeedRow {
  partner_id: string;
  partner_name: string;
  dataset_id: string;
  dataset_name: string;
  vertical: string;
  records: number;
  mailed: number;
  engaged: number;
  eng_rate_pct: number;
  last_mailed_at: string | null;
}
interface LadderTouch {
  vertical: string;
  touch_number: number;
  creative_filename: string;
  subject_line: string;
  from_name: string;
}
interface FeedFunnelResp {
  feeds?: FeedRow[];
  ladders?: LadderTouch[];
  feeds_error?: string;
  ladders_error?: string;
}

// A feed is flagged a RE-ROUTE candidate when it has meaningful mail volume but
// engages far below the best-performing feed in the SAME funnel — i.e. the funnel
// works, but this stream doesn't work in it. DEAD = ingested records, zero mailed.
const MIN_MAILED_FOR_FLAG = 500;
const REROUTE_RATIO = 0.25; // < 25% of the funnel's best feed eng% → move candidate

const fmt = (n: number) => n.toLocaleString();
const pct = (n: number) => `${n.toFixed(2)}%`;

interface FunnelGroup {
  vertical: string;
  feeds: FeedRow[];
  records: number;
  mailed: number;
  engaged: number;
  engRate: number;
  bestEng: number; // best per-feed eng% among feeds with enough volume
}

export const FeedFunnelBoard: React.FC = () => {
  const [feeds, setFeeds] = useState<FeedRow[]>([]);
  const [ladders, setLadders] = useState<LadderTouch[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [openFunnel, setOpenFunnel] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    apiFetch('/api/mailing/data-partners/feed-funnel', { credentials: 'include' })
      .then(r => r.json())
      .then((d: FeedFunnelResp) => {
        if (!alive) return;
        if (d.feeds_error) { setErr(d.feeds_error); return; }
        setFeeds(Array.isArray(d.feeds) ? d.feeds : []);
        setLadders(Array.isArray(d.ladders) ? d.ladders : []);
      })
      .catch(e => { if (alive) setErr(e?.message || 'unreachable'); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, []);

  // Group feeds by funnel; order funnels by total engaged (value) desc.
  const funnels = useMemo<FunnelGroup[]>(() => {
    const by = new Map<string, FeedRow[]>();
    for (const f of feeds) {
      const arr = by.get(f.vertical) ?? [];
      arr.push(f);
      by.set(f.vertical, arr);
    }
    const out: FunnelGroup[] = [];
    by.forEach((fs, vertical) => {
      const records = fs.reduce((s, f) => s + f.records, 0);
      const mailed = fs.reduce((s, f) => s + f.mailed, 0);
      const engaged = fs.reduce((s, f) => s + f.engaged, 0);
      const bestEng = Math.max(0, ...fs.filter(f => f.mailed >= MIN_MAILED_FOR_FLAG).map(f => f.eng_rate_pct));
      out.push({
        vertical, feeds: fs.slice().sort((a, b) => b.engaged - a.engaged),
        records, mailed, engaged,
        engRate: mailed > 0 ? (engaged / mailed) * 100 : 0,
        bestEng,
      });
    });
    return out.sort((a, b) => b.engaged - a.engaged);
  }, [feeds]);

  const flagFor = (f: FeedRow, g: FunnelGroup): { kind: 'dead' | 'reroute'; note: string } | null => {
    if (f.records > 0 && f.mailed === 0) return { kind: 'dead', note: 'ingested but never mailed — dead stream' };
    if (f.mailed >= MIN_MAILED_FOR_FLAG && g.bestEng > 0 && f.eng_rate_pct < g.bestEng * REROUTE_RATIO) {
      return { kind: 'reroute', note: `engages ${pct(f.eng_rate_pct)} vs ${pct(g.bestEng)} best in this funnel — re-route candidate` };
    }
    return null;
  };

  if (loading) return <div style={msg}>Loading feed → funnel performance…</div>;
  if (err) return <div style={{ ...msg, color: '#f59e0b' }}>Feed → funnel unavailable: {err}</div>;
  if (funnels.length === 0) return <div style={msg}>No feed activity yet.</div>;

  const grandEngaged = funnels.reduce((s, g) => s + g.engaged, 0);
  const openLadder = openFunnel ? ladders.filter(l => l.vertical === openFunnel).sort((a, b) => a.touch_number - b.touch_number) : [];
  const openGroup = openFunnel ? funnels.find(g => g.vertical === openFunnel) : undefined;

  return (
    <div style={{ marginBottom: 24 }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 10, marginBottom: 4 }}>
        <h3 style={{ margin: 0, color: '#dbeafe', fontSize: 16 }}>
          <FontAwesomeIcon icon={faArrowRightArrowLeft} style={{ marginRight: 8, color: '#6366f1' }} />
          Feeds → Funnels
        </h3>
        <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>
          {funnels.length} funnels · {feeds.length} feed streams · ranked by engagement (most valuable first)
        </span>
      </div>
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', marginBottom: 12 }}>
        Which data partner streams route into which funnel and how each performs. <b>Value = engagement</b> (engaged / mailed).
        <FontAwesomeIcon icon={faTriangleExclamation} style={{ margin: '0 4px 0 8px', color: '#f59e0b' }} /> flags a stream to
        investigate or move. Click a funnel for its touch ladder + contributing feeds.
      </div>

      {funnels.map(g => (
        <div key={g.vertical} style={funnelCard}>
          {/* Funnel header row: the roll-up + click-to-drill */}
          <button type="button" onClick={() => setOpenFunnel(g.vertical)} style={funnelHeader} title="View touch ladder + feeds">
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
              <FontAwesomeIcon icon={faRoute} style={{ color: '#6366f1' }} />
              <span style={{ fontWeight: 700, color: '#dbeafe', fontSize: 14 }}>{labelForVertical(g.vertical)}</span>
              <span style={badge}>{g.feeds.length} feed{g.feeds.length === 1 ? '' : 's'}</span>
            </div>
            <div style={{ display: 'flex', gap: 18, alignItems: 'baseline' }}>
              <Metric label="records" value={fmt(g.records)} />
              <Metric label="mailed" value={fmt(g.mailed)} />
              <Metric label="engaged" value={fmt(g.engaged)} accent="#10b981" />
              <Metric label="eng rate" value={pct(g.engRate)} accent={g.engRate >= 2 ? '#10b981' : g.engRate >= 1 ? '#f59e0b' : '#ef4444'} />
              <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)' }}>
                {grandEngaged > 0 ? `${((g.engaged / grandEngaged) * 100).toFixed(0)}% of value` : ''}
              </span>
            </div>
          </button>

          {/* Feed rows within the funnel */}
          <table style={feedTable}>
            <thead>
              <tr>
                <th style={fth}>Data feed (partner / dataset)</th>
                <th style={fthNum}>Records</th>
                <th style={fthNum}>Mailed</th>
                <th style={fthNum}>Engaged</th>
                <th style={fthNum}>Eng rate</th>
                <th style={fth}></th>
              </tr>
            </thead>
            <tbody>
              {g.feeds.map(f => {
                const flag = flagFor(f, g);
                return (
                  <tr key={`${f.partner_id}|${f.dataset_id}`} style={flag ? { background: flag.kind === 'dead' ? 'rgba(239,68,68,0.06)' : 'rgba(245,158,11,0.06)' } : undefined}>
                    <td style={ftd}>
                      <div style={{ fontWeight: 600, color: '#cbd5f5' }}>{f.partner_name}</div>
                      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>{f.dataset_name}</div>
                    </td>
                    <td style={ftdNum}>{fmt(f.records)}</td>
                    <td style={ftdNum}>{fmt(f.mailed)}</td>
                    <td style={{ ...ftdNum, color: '#10b981' }}>{fmt(f.engaged)}</td>
                    <td style={ftdNum}>
                      <EngBar pct={f.eng_rate_pct} best={g.bestEng} />
                    </td>
                    <td style={ftd}>
                      {flag && (
                        <span style={{ fontSize: 11, color: flag.kind === 'dead' ? '#ef4444' : '#f59e0b' }} title={flag.note}>
                          <FontAwesomeIcon icon={faTriangleExclamation} style={{ marginRight: 4 }} />
                          {flag.kind === 'dead' ? 'dead' : 're-route?'}
                        </span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ))}

      {/* ── Right slide-out: funnel touch ladder + contributing feeds ── */}
      {openFunnel && (
        <>
          <div style={scrim} onClick={() => setOpenFunnel(null)} />
          <div style={drawer}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 14 }}>
              <h3 style={{ margin: 0, color: '#dbeafe', fontSize: 16 }}>
                <FontAwesomeIcon icon={faLayerGroup} style={{ marginRight: 8, color: '#6366f1' }} />
                {labelForVertical(openFunnel)}
              </h3>
              <button type="button" onClick={() => setOpenFunnel(null)} style={closeBtn}><FontAwesomeIcon icon={faXmark} /></button>
            </div>

            <div style={sectionLabel}>Touch ladder <span style={{ fontWeight: 400, color: 'rgba(180,210,240,0.45)' }}>· db lane (representative)</span></div>
            {openLadder.length === 0 ? (
              <div style={{ ...msg, fontSize: 12, color: '#f59e0b' }}>No touch ladder configured for this funnel — the follow-up sequence is missing (touch-1 only).</div>
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginBottom: 20 }}>
                {openLadder.map(t => (
                  <div key={t.touch_number} style={touchRow}>
                    <div style={touchNum}>T{t.touch_number}</div>
                    <div style={{ minWidth: 0 }}>
                      <div style={{ fontSize: 13, color: '#cbd5f5', fontWeight: 600, overflowWrap: 'anywhere' }}>{t.subject_line || '(no subject)'}</div>
                      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)', overflowWrap: 'anywhere' }}>
                        {t.creative_filename || '—'}{t.from_name ? ` · from: ${t.from_name}` : ''}
                      </div>
                    </div>
                  </div>
                ))}
              </div>
            )}

            <div style={sectionLabel}>Data partners routed into this funnel</div>
            <table style={{ ...feedTable, marginTop: 6 }}>
              <thead>
                <tr>
                  <th style={fth}>Partner / dataset</th>
                  <th style={fthNum}>Mailed</th>
                  <th style={fthNum}>Eng rate</th>
                </tr>
              </thead>
              <tbody>
                {(openGroup?.feeds ?? []).map(f => (
                  <tr key={`${f.partner_id}|${f.dataset_id}`}>
                    <td style={ftd}>
                      <div style={{ fontWeight: 600, color: '#cbd5f5' }}>{f.partner_name}</div>
                      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>{f.dataset_name}</div>
                    </td>
                    <td style={ftdNum}>{fmt(f.mailed)}</td>
                    <td style={ftdNum}>{pct(f.eng_rate_pct)}</td>
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

const Metric: React.FC<{ label: string; value: string; accent?: string }> = ({ label, value, accent }) => (
  <div style={{ textAlign: 'right' }}>
    <div style={{ fontSize: 9, color: 'rgba(180,210,240,0.5)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 14, fontWeight: 700, color: accent ?? '#dbeafe', fontVariantNumeric: 'tabular-nums' }}>{value}</div>
  </div>
);

// Engagement bar scaled to the funnel's best feed, so the relative gap is visible.
const EngBar: React.FC<{ pct: number; best: number }> = ({ pct: p, best }) => {
  const w = best > 0 ? Math.min(100, (p / best) * 100) : (p > 0 ? 100 : 0);
  const color = best > 0 && p < best * REROUTE_RATIO ? '#ef4444' : p >= 2 ? '#10b981' : '#f59e0b';
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 6, justifyContent: 'flex-end' }}>
      <span style={{ fontVariantNumeric: 'tabular-nums', color, minWidth: 46, textAlign: 'right' }}>{pct(p)}</span>
      <div style={{ width: 44, height: 5, background: 'rgba(120,150,200,0.15)', borderRadius: 3, overflow: 'hidden' }}>
        <div style={{ width: `${w}%`, height: '100%', background: color }} />
      </div>
    </div>
  );
};

const msg: React.CSSProperties = { padding: 20, color: 'rgba(180,210,240,0.7)', fontSize: 13 };
const funnelCard: React.CSSProperties = { border: '1px solid rgba(120,150,200,0.16)', borderRadius: 10, marginBottom: 12, overflow: 'hidden', background: 'rgba(13,21,38,0.4)' };
const funnelHeader: React.CSSProperties = { all: 'unset', boxSizing: 'border-box', cursor: 'pointer', display: 'flex', width: '100%', justifyContent: 'space-between', alignItems: 'center', gap: 12, padding: '10px 14px', background: 'rgba(99,102,241,0.06)', flexWrap: 'wrap' };
const badge: React.CSSProperties = { fontSize: 10, padding: '2px 8px', borderRadius: 12, background: 'rgba(99,102,241,0.18)', color: '#a5b4fc', whiteSpace: 'nowrap' };
const feedTable: React.CSSProperties = { width: '100%', borderCollapse: 'collapse', fontSize: 13 };
const fth: React.CSSProperties = { textAlign: 'left', padding: '6px 14px', fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.5, color: 'rgba(180,210,240,0.5)', borderBottom: '1px solid rgba(120,150,200,0.1)' };
const fthNum: React.CSSProperties = { ...fth, textAlign: 'right' };
const ftd: React.CSSProperties = { padding: '8px 14px', borderBottom: '1px solid rgba(120,150,200,0.06)', color: 'rgba(220,235,250,0.85)' };
const ftdNum: React.CSSProperties = { ...ftd, textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
const scrim: React.CSSProperties = { position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.5)', zIndex: 998 };
const drawer: React.CSSProperties = { position: 'fixed', top: 0, right: 0, height: '100vh', width: 'min(520px, 94vw)', background: '#0b1120', borderLeft: '1px solid rgba(120,150,200,0.25)', boxShadow: '-8px 0 30px rgba(0,0,0,0.5)', padding: 20, overflowY: 'auto', zIndex: 999 };
const closeBtn: React.CSSProperties = { all: 'unset', cursor: 'pointer', color: 'rgba(180,210,240,0.7)', fontSize: 18, padding: 4 };
const sectionLabel: React.CSSProperties = { fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.6, color: 'rgba(180,210,240,0.6)', fontWeight: 700, marginBottom: 8, marginTop: 6 };
const touchRow: React.CSSProperties = { display: 'flex', gap: 12, alignItems: 'flex-start', padding: '8px 10px', background: 'rgba(15,30,60,0.4)', borderRadius: 8, border: '1px solid rgba(120,150,200,0.12)' };
const touchNum: React.CSSProperties = { flexShrink: 0, width: 30, height: 30, borderRadius: 8, background: 'rgba(99,102,241,0.2)', color: '#a5b4fc', display: 'flex', alignItems: 'center', justifyContent: 'center', fontWeight: 700, fontSize: 12 };
