import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faExclamationTriangle, faHandPointer, faSackDollar, faEnvelopeOpenText, faHeartCrack, faArrowTrendUp, faArrowTrendDown, faMinus, faChevronRight, faChevronDown, faBullseye } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

// PreviousActivationsPanel (v3) — per previous data activation (dataset), the
// LIFETIME engagement footprint of the dataset's records on the PARTNER'S OWN
// mail, so a partner is judged on the whole activation (not a rolling slice).
// All headline metrics share one timeframe (lifetime); recent_mailed/clicks are
// a separate freshness signal. Engagement is scoped to the dataset's own
// campaigns (partner_dataset_id, all touches) joined on subscriber+campaign — no
// cross-dataset leak. Rank by clicks + conversions; opens are machine-inflated;
// bounce% is lifetime list-health (the partner-quality tell); revenue is $0 (CPM).

interface Activation {
  dataset_id: string;
  dataset_name: string;
  partner_name: string;
  vertical: string;
  total_records: number;
  mailed: number;
  opens: number; opens_pct: number;
  clicks: number; clicks_pct: number;
  bounced: number; bounce_pct: number;
  conv_lifetime: number;
  recent_mailed: number;
  recent_clicks: number;
  recent_days: number;
  trend: 'up' | 'down' | 'flat';
  low_sample: boolean;
}
interface Resp {
  days: number;
  scope: string;
  activations: Activation[];
  totals: { mailed: number; opens: number; opens_pct: number; clicks: number; clicks_pct: number; bounced: number; bounce_pct: number; conv_lifetime: number };
}

// Per-offer funnel for one dataset (the expand-row drill-down).
interface OfferRow {
  offer_key: string;
  offer_name: string;
  everflow_offer_id: string;
  clickers: number;
  clicks: number;
  ctr_pct: number;
  conversions: number;
  conv_pct: number;
  click_to_conv_pct: number;
}
interface OfferPerfResp {
  dataset_id: string;
  dataset_name: string;
  mailed: number;
  offers: OfferRow[];
}

const fmt = (n: number) => n.toLocaleString();
const bounceColor = (p: number) => p >= 30 ? '#ef4444' : p >= 15 ? '#f59e0b' : 'rgba(180,210,240,0.7)';

// OfferBreakdown — the per-offer funnel shown when a dataset row is expanded.
// Which offers the dataset's audience clicked (human, money-slug attributed) and
// converted on, with CTR / conversion rate / click→conv over the dataset's reach.
const OfferBreakdown: React.FC<{ id: string; loading: boolean; err?: string; resp?: OfferPerfResp }> = ({ loading, err, resp }) => {
  const sth: React.CSSProperties = { textAlign: 'right', padding: '6px 12px', fontSize: 9.5, textTransform: 'uppercase', letterSpacing: 0.4, color: 'rgba(180,210,240,0.5)', whiteSpace: 'nowrap' };
  const snum: React.CSSProperties = { padding: '6px 12px', fontSize: 12.5, color: '#dbeafe', textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
  if (loading) return <div style={{ padding: '14px 18px', color: 'rgba(180,210,240,0.6)', fontSize: 12 }}><FontAwesomeIcon icon={faSpinner} spin /> Resolving offer funnel…</div>;
  if (err) return <div style={{ padding: '14px 18px', color: '#ef4444', fontSize: 12 }}><FontAwesomeIcon icon={faExclamationTriangle} /> {err}</div>;
  if (!resp) return null;
  if (!resp.offers || resp.offers.length === 0) return <div style={{ padding: '14px 18px', color: 'rgba(180,210,240,0.5)', fontSize: 12 }}>No offer-attributed human clicks or conversions for this activation yet.</div>;
  return (
    <div style={{ padding: '10px 18px 14px 30px' }}>
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)', marginBottom: 8 }}>
        <FontAwesomeIcon icon={faBullseye} style={{ color: '#f59e0b', marginRight: 6 }} />
        Offer funnel — rates over <b>{fmt(resp.mailed)}</b> mailed reach · clicks are <b>human</b> (verdict-filtered, money-slug attributed) · conversions are postback truth
      </div>
      <table style={{ width: '100%', borderCollapse: 'collapse' }}>
        <thead><tr>
          <th style={{ ...sth, textAlign: 'left' }}>Offer</th>
          <th style={sth}>Clickers</th>
          <th style={sth}>Clicks</th>
          <th style={sth}>CTR %</th>
          <th style={sth}>Conv</th>
          <th style={sth}>Conv %</th>
          <th style={sth}>Click→Conv %</th>
        </tr></thead>
        <tbody>
          {resp.offers.map((o, i) => (
            <tr key={o.offer_key || o.offer_name + i} style={{ borderTop: '1px solid rgba(120,150,200,0.08)' }}>
              <td style={{ padding: '6px 12px', fontSize: 12.5, color: '#dbeafe' }}>{o.offer_name}{o.everflow_offer_id && <span style={{ color: 'rgba(180,210,240,0.4)', fontSize: 11, marginLeft: 6 }}>EF {o.everflow_offer_id}</span>}</td>
              <td style={{ ...snum, color: '#a78bfa' }}>{fmt(o.clickers)}</td>
              <td style={{ ...snum, color: 'rgba(180,210,240,0.6)' }}>{fmt(o.clicks)}</td>
              <td style={snum}>{o.ctr_pct}%</td>
              <td style={{ ...snum, color: '#10b981', fontWeight: o.conversions > 0 ? 700 : 400 }}>{o.conversions}</td>
              <td style={{ ...snum, color: o.conv_pct > 0 ? '#10b981' : undefined }}>{o.conv_pct}%</td>
              <td style={{ ...snum, color: 'rgba(180,210,240,0.85)' }}>{o.click_to_conv_pct}%</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
};

export const PreviousActivationsPanel: React.FC = () => {
  const [data, setData] = useState<Resp | null>(null);
  const [days, setDays] = useState(7);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Per-offer drill-down state (expand a dataset row to see its offer funnel).
  const [expanded, setExpanded] = useState<string | null>(null);
  const [offers, setOffers] = useState<Record<string, OfferPerfResp>>({});
  const [offerLoading, setOfferLoading] = useState<string | null>(null);
  const [offerErr, setOfferErr] = useState<Record<string, string>>({});

  const fetchData = useCallback(() => {
    setLoading(true); setError(null);
    apiFetch(`/api/mailing/data-partners/previous-activations?days=${days}`, { credentials: 'include' })
      .then(r => r.json())
      .then(d => { if (d?.error) { setError(d.error); setData(null); } else setData(d); })
      .catch(err => setError(String(err)))
      .finally(() => setLoading(false));
  }, [days]);
  useEffect(() => { fetchData(); }, [fetchData]);

  const toggleRow = useCallback((id: string) => {
    if (expanded === id) { setExpanded(null); return; }
    setExpanded(id);
    if (!offers[id]) {
      setOfferLoading(id);
      apiFetch(`/api/mailing/data-partners/datasets/${id}/offer-performance`, { credentials: 'include' })
        .then(r => r.json())
        .then(d => {
          if (d?.error) setOfferErr(e => ({ ...e, [id]: d.error }));
          else setOffers(o => ({ ...o, [id]: d }));
        })
        .catch(err => setOfferErr(e => ({ ...e, [id]: String(err) })))
        .finally(() => setOfferLoading(l => (l === id ? null : l)));
    }
  }, [expanded, offers]);

  if (loading && !data) return <div style={{ color: 'rgba(180,210,240,0.65)', padding: 30 }}><FontAwesomeIcon icon={faSpinner} spin /> Computing lifetime engagement footprint… (~10s)</div>;
  if (error) return <div style={{ color: '#ef4444', padding: 20 }}><FontAwesomeIcon icon={faExclamationTriangle} /> {error}</div>;
  if (!data) return null;
  const t = data.totals;

  const card = (label: string, value: string, sub: string, accent: string, icon: typeof faHandPointer) => (
    <div style={{ background: 'rgba(0,0,0,0.22)', padding: '12px 14px', borderRadius: 8, minWidth: 140, flex: 1 }}>
      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase', letterSpacing: 0.5 }}>
        <FontAwesomeIcon icon={icon} style={{ marginRight: 6, color: accent }} />{label}
      </div>
      <div style={{ fontSize: 22, fontWeight: 700, color: accent }}>{value}</div>
      {sub && <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)', marginTop: 2 }}>{sub}</div>}
    </div>
  );
  const th: React.CSSProperties = { textAlign: 'left', padding: '8px 10px', fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.5, color: 'rgba(180,210,240,0.55)', borderBottom: '1px solid rgba(120,150,200,0.18)', whiteSpace: 'nowrap' };
  const num: React.CSSProperties = { padding: '8px 10px', fontSize: 13, color: '#dbeafe', textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
  const txt: React.CSSProperties = { padding: '8px 10px', fontSize: 13, color: '#dbeafe' };
  const trendIcon = (tr: string) => tr === 'up' ? <FontAwesomeIcon icon={faArrowTrendUp} style={{ color: '#10b981' }} /> : tr === 'down' ? <FontAwesomeIcon icon={faArrowTrendDown} style={{ color: '#ef4444' }} /> : <FontAwesomeIcon icon={faMinus} style={{ color: 'rgba(180,210,240,0.4)' }} />;

  return (
    <div style={{ padding: '4px 2px' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 14, flexWrap: 'wrap', gap: 10 }}>
        <div>
          <div style={{ fontSize: 15, fontWeight: 700, color: '#dbeafe' }}>Previous Activations <span style={{ fontSize: 10, fontWeight: 600, color: '#10b981', background: 'rgba(16,185,129,0.12)', padding: '2px 7px', borderRadius: 4, marginLeft: 6, textTransform: 'uppercase', letterSpacing: 0.5 }}>Lifetime</span></div>
          <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>
            Lifetime engagement of each data partner's records on their OWN mail — judge the whole activation, rank by clicks + conversions, weigh bounce
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.45)', textTransform: 'uppercase', letterSpacing: 0.5 }}>Recent window</span>
          <div style={{ display: 'flex', gap: 6 }}>
            {[7, 14, 30].map(d => (
              <button key={d} onClick={() => setDays(d)} style={{ background: d === days ? 'rgba(99,102,241,0.35)' : 'rgba(0,0,0,0.25)', color: d === days ? '#e0e7ff' : 'rgba(180,210,240,0.7)', border: '1px solid rgba(120,150,200,0.25)', borderRadius: 6, padding: '5px 12px', fontSize: 12, cursor: 'pointer' }}>{d}d</button>
            ))}
          </div>
        </div>
      </div>

      <div style={{ display: 'flex', gap: 10, marginBottom: 14, flexWrap: 'wrap' }}>
        {card('Mailed (lifetime)', fmt(t.mailed), `${data.activations.length} activations`, '#dbeafe', faEnvelopeOpenText)}
        {card('Clicks', fmt(t.clicks), `${t.clicks_pct}% of mailed`, '#a78bfa', faHandPointer)}
        {card('Bounced', fmt(t.bounced), `${t.bounce_pct}% — list health`, bounceColor(t.bounce_pct), faHeartCrack)}
        {card('Conversions', fmt(t.conv_lifetime), 'lifetime, CPM', '#10b981', faSackDollar)}
      </div>

      <div style={{ overflowX: 'auto', border: '1px solid rgba(120,150,200,0.15)', borderRadius: 8 }}>
        <table style={{ width: '100%', borderCollapse: 'collapse', minWidth: 900 }}>
          <thead><tr>
            <th style={th}>Partner</th>
            <th style={th}>Activation</th>
            <th style={th}>Vertical</th>
            <th style={{ ...th, textAlign: 'right' }}>Mailed</th>
            <th style={{ ...th, textAlign: 'right' }}>Open %</th>
            <th style={{ ...th, textAlign: 'right' }}>Click %</th>
            <th style={{ ...th, textAlign: 'right' }}>Bounce %</th>
            <th style={{ ...th, textAlign: 'right' }}>Conv</th>
            <th style={{ ...th, textAlign: 'right' }}>Recent {data.days}d</th>
            <th style={{ ...th, textAlign: 'center' }}>Trend</th>
          </tr></thead>
          <tbody>
            {data.activations.length === 0 && <tr><td colSpan={10} style={{ ...txt, textAlign: 'center', color: 'rgba(180,210,240,0.5)', padding: 24 }}>No mailed activations found.</td></tr>}
            {data.activations.map(a => {
              const isOpen = expanded === a.dataset_id;
              return (
              <React.Fragment key={a.dataset_id}>
              <tr onClick={() => toggleRow(a.dataset_id)} title="Click for the per-offer funnel (clicks, CTR, conversions)" style={{ borderBottom: '1px solid rgba(120,150,200,0.08)', cursor: 'pointer', background: isOpen ? 'rgba(99,102,241,0.08)' : undefined }}>
                <td style={txt}><FontAwesomeIcon icon={isOpen ? faChevronDown : faChevronRight} style={{ marginRight: 8, fontSize: 10, color: 'rgba(180,210,240,0.5)' }} />{a.partner_name}</td>
                <td style={txt}>{a.dataset_name}{a.low_sample && <span title="Low sample (<1k lifetime mailed) — high variance" style={{ color: '#f59e0b', marginLeft: 5, fontSize: 11 }}>⚠</span>}</td>
                <td style={{ ...txt, color: 'rgba(180,210,240,0.7)' }}>{a.vertical}</td>
                <td style={num} title={`${fmt(a.total_records)} imported`}>{fmt(a.mailed)}</td>
                <td style={{ ...num, color: 'rgba(180,210,240,0.45)' }} title={`${fmt(a.opens)} openers — ~90% machine, weak signal`}>{a.opens_pct}%</td>
                <td style={{ ...num, color: '#a78bfa' }} title={`${fmt(a.clicks)} clickers`}>{a.clicks_pct}%</td>
                <td style={{ ...num, fontWeight: a.bounce_pct >= 30 ? 700 : 400, color: bounceColor(a.bounce_pct) }} title={`${fmt(a.bounced)} bounced — lifetime list health`}>{a.bounce_pct}%</td>
                <td style={{ ...num, color: '#10b981', fontWeight: a.conv_lifetime > 0 ? 700 : 400 }}>{a.conv_lifetime}</td>
                <td style={{ ...num, color: 'rgba(180,210,240,0.6)' }} title={`${fmt(a.recent_mailed)} mailed / ${fmt(a.recent_clicks)} clicked in last ${a.recent_days}d`}>{fmt(a.recent_mailed)} / {fmt(a.recent_clicks)}</td>
                <td style={{ ...num, textAlign: 'center' }} title="Recent clicks vs prior period">{trendIcon(a.trend)}</td>
              </tr>
              {isOpen && (
                <tr>
                  <td colSpan={10} style={{ padding: 0, background: 'rgba(0,0,0,0.28)', borderBottom: '1px solid rgba(120,150,200,0.18)' }}>
                    <OfferBreakdown id={a.dataset_id} loading={offerLoading === a.dataset_id} err={offerErr[a.dataset_id]} resp={offers[a.dataset_id]} />
                  </td>
                </tr>
              )}
              </React.Fragment>
              );
            })}
          </tbody>
        </table>
      </div>

      <div style={{ marginTop: 12, fontSize: 11, color: 'rgba(180,210,240,0.5)', lineHeight: 1.6 }}>
        <div><b>How to read:</b> all headline columns are <b>lifetime</b> (the whole activation), scoped to the partner's OWN campaigns. Rank by <span style={{ color: '#a78bfa' }}>clicks</span> + <span style={{ color: '#10b981' }}>conversions</span>; penalize <span style={{ color: '#ef4444' }}>bounce</span>.</div>
        <div>• <b>Mailed</b> = distinct records ever mailed (hover for imported total). <b>Recent {data.days}d</b> = mailed / clicked in the chosen window (freshness only — never mixed into the lifetime rates).</div>
        <div>• <b>Bounce %</b> is the best list-quality tell — dirty lists run 30-47%. <b>Open %</b> is ~90% automated machine opens — treat as weak.</div>
        <div>• Conversion revenue is $0 by design (CPM deals pay per-send). <b>Cost/CPM per record isn't in the system</b> — needed from partner deal terms for true ROI.</div>
      </div>
    </div>
  );
};
