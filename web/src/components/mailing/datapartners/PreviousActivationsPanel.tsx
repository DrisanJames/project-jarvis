import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faExclamationTriangle, faEnvelopeOpenText, faHandPointer } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

// PreviousActivationsPanel — for each previous data activation (dataset), how many
// of the records mailed in the window reached the OPENERS (7D-Openers) or CLICKERS
// (30D-Clickers) engaged segments, with counts and percentages of the mailed base.
// Segment membership is the platform's canonical, self-cleaning "engaged" definition.

interface Activation {
  dataset_id: string;
  dataset_name: string;
  partner_name: string;
  vertical: string;
  last_mailed_at: string | null;
  mailed: number;
  openers: number;
  openers_pct: number;
  clickers: number;
  clickers_pct: number;
}

interface ActivationsResponse {
  days: number;
  activations: Activation[];
  totals: { mailed: number; openers: number; openers_pct: number; clickers: number; clickers_pct: number };
}

const fmt = (n: number) => n.toLocaleString();

export const PreviousActivationsPanel: React.FC = () => {
  const [data, setData] = useState<ActivationsResponse | null>(null);
  const [days, setDays] = useState(7);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchData = useCallback(() => {
    setLoading(true);
    setError(null);
    apiFetch(`/api/mailing/data-partners/previous-activations?days=${days}`, { credentials: 'include' })
      .then(r => r.json())
      .then(d => {
        if (d?.error) { setError(d.error); setData(null); }
        else setData(d);
      })
      .catch(err => setError(String(err)))
      .finally(() => setLoading(false));
  }, [days]);

  useEffect(() => { fetchData(); }, [fetchData]);

  if (loading && !data) {
    return <div style={{ color: 'rgba(180,210,240,0.65)', padding: 30 }}><FontAwesomeIcon icon={faSpinner} spin /> Loading previous activations…</div>;
  }
  if (error) {
    return <div style={{ color: '#ef4444', padding: 20 }}><FontAwesomeIcon icon={faExclamationTriangle} /> {error}</div>;
  }
  if (!data) return null;

  const t = data.totals;

  const card = (label: string, value: string, sub: string, accent: string, icon?: typeof faEnvelopeOpenText) => (
    <div style={{ background: 'rgba(0,0,0,0.22)', padding: '12px 14px', borderRadius: 8, minWidth: 150, flex: 1 }}>
      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase', letterSpacing: 0.5 }}>
        {icon && <FontAwesomeIcon icon={icon} style={{ marginRight: 6, color: accent }} />}{label}
      </div>
      <div style={{ fontSize: 22, fontWeight: 700, color: accent }}>{value}</div>
      {sub && <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)', marginTop: 2 }}>{sub}</div>}
    </div>
  );

  const th: React.CSSProperties = { textAlign: 'left', padding: '8px 10px', fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.5, color: 'rgba(180,210,240,0.55)', borderBottom: '1px solid rgba(120,150,200,0.18)' };
  const tdNum: React.CSSProperties = { padding: '8px 10px', fontSize: 13, color: '#dbeafe', textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
  const tdTxt: React.CSSProperties = { padding: '8px 10px', fontSize: 13, color: '#dbeafe' };

  return (
    <div style={{ padding: '4px 2px' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 14, flexWrap: 'wrap', gap: 10 }}>
        <div>
          <div style={{ fontSize: 15, fontWeight: 700, color: '#dbeafe' }}>Previous Activations</div>
          <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>
            Records mailed in the last {data.days} days that reached the 7D-Openers / 30D-Clickers segments
          </div>
        </div>
        <div style={{ display: 'flex', gap: 6 }}>
          {[7, 14, 30].map(d => (
            <button key={d} onClick={() => setDays(d)}
              style={{
                background: d === days ? 'rgba(99,102,241,0.35)' : 'rgba(0,0,0,0.25)',
                color: d === days ? '#e0e7ff' : 'rgba(180,210,240,0.7)',
                border: '1px solid rgba(120,150,200,0.25)', borderRadius: 6, padding: '5px 12px', fontSize: 12, cursor: 'pointer',
              }}>{d}d</button>
          ))}
        </div>
      </div>

      <div style={{ display: 'flex', gap: 10, marginBottom: 16, flexWrap: 'wrap' }}>
        {card('Total Mailed', fmt(t.mailed), `${data.activations.length} activations`, '#dbeafe')}
        {card('Openers', fmt(t.openers), `${t.openers_pct}% of mailed`, '#10b981', faEnvelopeOpenText)}
        {card('Clickers', fmt(t.clickers), `${t.clickers_pct}% of mailed`, '#a78bfa', faHandPointer)}
      </div>

      <div style={{ overflowX: 'auto', border: '1px solid rgba(120,150,200,0.15)', borderRadius: 8 }}>
        <table style={{ width: '100%', borderCollapse: 'collapse', minWidth: 720 }}>
          <thead>
            <tr>
              <th style={th}>Partner</th>
              <th style={th}>Activation (Dataset)</th>
              <th style={th}>Vertical</th>
              <th style={{ ...th, textAlign: 'right' }}>Mailed</th>
              <th style={{ ...th, textAlign: 'right' }}>Openers</th>
              <th style={{ ...th, textAlign: 'right' }}>Open %</th>
              <th style={{ ...th, textAlign: 'right' }}>Clickers</th>
              <th style={{ ...th, textAlign: 'right' }}>Click %</th>
            </tr>
          </thead>
          <tbody>
            {data.activations.length === 0 && (
              <tr><td colSpan={8} style={{ ...tdTxt, textAlign: 'center', color: 'rgba(180,210,240,0.5)', padding: 24 }}>No activations mailed in the last {data.days} days.</td></tr>
            )}
            {data.activations.map(a => (
              <tr key={a.dataset_id} style={{ borderBottom: '1px solid rgba(120,150,200,0.08)' }}>
                <td style={tdTxt}>{a.partner_name}</td>
                <td style={tdTxt}>{a.dataset_name}</td>
                <td style={{ ...tdTxt, color: 'rgba(180,210,240,0.7)' }}>{a.vertical}</td>
                <td style={tdNum}>{fmt(a.mailed)}</td>
                <td style={{ ...tdNum, color: '#10b981' }}>{fmt(a.openers)}</td>
                <td style={{ ...tdNum, color: '#10b981' }}>{a.openers_pct}%</td>
                <td style={{ ...tdNum, color: '#a78bfa' }}>{fmt(a.clickers)}</td>
                <td style={{ ...tdNum, color: '#a78bfa' }}>{a.clickers_pct}%</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
};
