import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faExclamationTriangle, faHandPointer, faSackDollar, faEnvelopeOpenText, faArrowTrendUp, faArrowTrendDown, faMinus } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

// PreviousActivationsPanel (v2) — per previous data activation (dataset), how the
// records mailed in the window engaged with the PARTNER'S OWN mail (accurate,
// campaign-scoped), plus conversions, so data-partner performance can be judged.
// Rank by clicks + conversions; opens are machine-inflated; revenue is $0 by design.

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
  conv_window: number;
  conv_lifetime: number;
  clicks_prev: number;
  trend: 'up' | 'down' | 'flat';
  low_sample: boolean;
}
interface Resp {
  days: number;
  activations: Activation[];
  totals: { mailed: number; opens: number; opens_pct: number; clicks: number; clicks_pct: number; bounced: number; bounce_pct: number; conv_window: number; conv_lifetime: number };
}

const fmt = (n: number) => n.toLocaleString();

export const PreviousActivationsPanel: React.FC = () => {
  const [data, setData] = useState<Resp | null>(null);
  const [days, setDays] = useState(7);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchData = useCallback(() => {
    setLoading(true); setError(null);
    apiFetch(`/api/mailing/data-partners/previous-activations?days=${days}`, { credentials: 'include' })
      .then(r => r.json())
      .then(d => { if (d?.error) { setError(d.error); setData(null); } else setData(d); })
      .catch(err => setError(String(err)))
      .finally(() => setLoading(false));
  }, [days]);
  useEffect(() => { fetchData(); }, [fetchData]);

  if (loading && !data) return <div style={{ color: 'rgba(180,210,240,0.65)', padding: 30 }}><FontAwesomeIcon icon={faSpinner} spin /> Loading previous activations…</div>;
  if (error) return <div style={{ color: '#ef4444', padding: 20 }}><FontAwesomeIcon icon={faExclamationTriangle} /> {error}</div>;
  if (!data) return null;
  const t = data.totals;

  const card = (label: string, value: string, sub: string, accent: string, icon: typeof faHandPointer) => (
    <div style={{ background: 'rgba(0,0,0,0.22)', padding: '12px 14px', borderRadius: 8, minWidth: 150, flex: 1 }}>
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
          <div style={{ fontSize: 15, fontWeight: 700, color: '#dbeafe' }}>Previous Activations</div>
          <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>
            Engagement with each data partner's OWN mail over the last {data.days} days — rank by clicks + conversions
          </div>
        </div>
        <div style={{ display: 'flex', gap: 6 }}>
          {[7, 14, 30].map(d => (
            <button key={d} onClick={() => setDays(d)} style={{ background: d === days ? 'rgba(99,102,241,0.35)' : 'rgba(0,0,0,0.25)', color: d === days ? '#e0e7ff' : 'rgba(180,210,240,0.7)', border: '1px solid rgba(120,150,200,0.25)', borderRadius: 6, padding: '5px 12px', fontSize: 12, cursor: 'pointer' }}>{d}d</button>
          ))}
        </div>
      </div>

      <div style={{ display: 'flex', gap: 10, marginBottom: 14, flexWrap: 'wrap' }}>
        {card('Mailed', fmt(t.mailed), `${data.activations.length} activations`, '#dbeafe', faEnvelopeOpenText)}
        {card('Clicks', fmt(t.clicks), `${t.clicks_pct}% of mailed`, '#a78bfa', faHandPointer)}
        {card('Conversions (lifetime)', fmt(t.conv_lifetime), `${t.conv_window} in-window`, '#10b981', faSackDollar)}
      </div>

      <div style={{ overflowX: 'auto', border: '1px solid rgba(120,150,200,0.15)', borderRadius: 8 }}>
        <table style={{ width: '100%', borderCollapse: 'collapse', minWidth: 880 }}>
          <thead><tr>
            <th style={th}>Partner</th>
            <th style={th}>Activation</th>
            <th style={th}>Vertical</th>
            <th style={{ ...th, textAlign: 'right' }}>Mailed</th>
            <th style={{ ...th, textAlign: 'right' }}>Open %</th>
            <th style={{ ...th, textAlign: 'right' }}>Click %</th>
            <th style={{ ...th, textAlign: 'right' }}>Bounce %</th>
            <th style={{ ...th, textAlign: 'right' }}>Conv 7d</th>
            <th style={{ ...th, textAlign: 'right' }}>Conv life</th>
            <th style={{ ...th, textAlign: 'center' }}>Trend</th>
          </tr></thead>
          <tbody>
            {data.activations.length === 0 && <tr><td colSpan={10} style={{ ...txt, textAlign: 'center', color: 'rgba(180,210,240,0.5)', padding: 24 }}>No activations mailed in the last {data.days} days.</td></tr>}
            {data.activations.map(a => (
              <tr key={a.dataset_id} style={{ borderBottom: '1px solid rgba(120,150,200,0.08)' }}>
                <td style={txt}>{a.partner_name}</td>
                <td style={txt}>{a.dataset_name}{a.low_sample && <span title="Low sample (<1k mailed) — high variance" style={{ color: '#f59e0b', marginLeft: 5, fontSize: 11 }}>⚠</span>}</td>
                <td style={{ ...txt, color: 'rgba(180,210,240,0.7)' }}>{a.vertical}</td>
                <td style={num}>{fmt(a.mailed)}</td>
                <td style={{ ...num, color: 'rgba(180,210,240,0.55)' }} title="Opens are ~90% machine — weak signal">{a.opens_pct}%</td>
                <td style={{ ...num, color: '#a78bfa' }}>{a.clicks_pct}%</td>
                <td style={{ ...num, color: a.bounce_pct >= 20 ? '#ef4444' : a.bounce_pct >= 8 ? '#f59e0b' : 'rgba(180,210,240,0.7)' }}>{a.bounce_pct}%</td>
                <td style={num}>{a.conv_window}</td>
                <td style={{ ...num, color: '#10b981', fontWeight: a.conv_lifetime > 0 ? 700 : 400 }}>{a.conv_lifetime}</td>
                <td style={{ ...num, textAlign: 'center' }}>{trendIcon(a.trend)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div style={{ marginTop: 12, fontSize: 11, color: 'rgba(180,210,240,0.5)', lineHeight: 1.6 }}>
        <div><b>How to read:</b> rank by <span style={{ color: '#a78bfa' }}>clicks</span> + <span style={{ color: '#10b981' }}>conversions</span>. Engagement is on the partner's OWN mail (not cross-brand).</div>
        <div>• <b>Open %</b> is ~90% Apple-MPP/machine industry-wide — treat as weak.</div>
        <div>• <b>Conv 7d</b> = conversions for records mailed in-window (lags, often ~0). <b>Conv life</b> = all conversions ever tied to the dataset's records — use to rank.</div>
        <div>• Conversion revenue is $0 by design (CPM deals pay per-send). <b>Cost/CPM per record isn't in the system</b> — needed from partner deal terms for true ROI.</div>
      </div>
    </div>
  );
};
