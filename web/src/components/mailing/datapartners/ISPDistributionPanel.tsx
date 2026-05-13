import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faSync, faExclamationTriangle, faChartBar } from '@fortawesome/free-solid-svg-icons';

interface ISPOverride {
  isp: string;
  pct_override: number;
  max_per_wave: number;
}

interface ThroughputResponse {
  dataset_id: string;
  flush_window_hours: number;
  ready_queue_total: number;
  isp_breakdown: Record<string, number>;
  oldest_ingest_at: string | null;
  waves_remaining: number;
  recommended_wave_size: number;
  isp_overrides: ISPOverride[];
}

interface DistributionPanelProps {
  datasetId: string;
  datasetName: string;
  onClose: () => void;
}

const ISP_FAMILIES = ['gmail', 'yahoo', 'aol', 'microsoft', 'comcast', 'att', 'other'];

const ISP_COLORS: Record<string, string> = {
  gmail: '#ef4444',
  yahoo: '#a855f7',
  aol: '#06b6d4',
  microsoft: '#3b82f6',
  comcast: '#84cc16',
  att: '#f59e0b',
  other: '#94a3b8',
};

interface OverrideEdit {
  isp: string;
  pct: string;
  cap: string;
}

export const ISPDistributionPanel: React.FC<DistributionPanelProps> = ({ datasetId, datasetName, onClose }) => {
  const [data, setData] = useState<ThroughputResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [edits, setEdits] = useState<Record<string, OverrideEdit>>({});
  const [saving, setSaving] = useState(false);

  const fetchThroughput = useCallback(() => {
    setLoading(true);
    setError(null);
    fetch(`/api/mailing/data-partners/datasets/${datasetId}/throughput`, { credentials: 'include' })
      .then(r => r.json())
      .then((d: ThroughputResponse) => {
        setData(d);
        const initialEdits: Record<string, OverrideEdit> = {};
        ISP_FAMILIES.forEach(isp => {
          const override = d.isp_overrides?.find(o => o.isp === isp);
          initialEdits[isp] = {
            isp,
            pct: override ? String(Math.round(override.pct_override * 100)) : '',
            cap: override?.max_per_wave ? String(override.max_per_wave) : '',
          };
        });
        setEdits(initialEdits);
      })
      .catch(err => setError(String(err)))
      .finally(() => setLoading(false));
  }, [datasetId]);

  useEffect(() => { fetchThroughput(); }, [fetchThroughput]);

  const totalQueue = data?.ready_queue_total ?? 0;

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    const overrides = ISP_FAMILIES
      .map(isp => edits[isp])
      .filter(e => e.pct !== '' || e.cap !== '')
      .map(e => {
        const pct = e.pct === '' ? 0 : Math.max(0, Math.min(100, parseInt(e.pct, 10))) / 100;
        const cap = e.cap === '' ? 0 : Math.max(0, parseInt(e.cap, 10));
        return { isp: e.isp, pct_override: pct, max_per_wave: cap };
      });
    try {
      const res = await fetch(`/api/mailing/data-partners/datasets/${datasetId}/isp-distribution`, {
        method: 'PUT',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ overrides }),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        setError(body.error ?? `HTTP ${res.status}`);
        setSaving(false);
        return;
      }
      setSaving(false);
      fetchThroughput();
    } catch (err) {
      setError(String(err));
      setSaving(false);
    }
  };

  const updateEdit = (isp: string, key: 'pct' | 'cap', value: string) => {
    setEdits(prev => ({ ...prev, [isp]: { ...prev[isp], [key]: value } }));
  };

  return (
    <div style={modalBackdrop}>
      <div style={modalBody}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 16 }}>
          <div>
            <h3 style={{ margin: 0, color: '#dbeafe' }}>
              <FontAwesomeIcon icon={faChartBar} /> ISP Distribution — {datasetName}
            </h3>
            <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', marginTop: 4 }}>
              Override ISP percentages and per-wave caps. Empty = use natural distribution from queue.
            </div>
          </div>
          <button onClick={fetchThroughput} style={iconBtn}><FontAwesomeIcon icon={faSync} spin={loading} /> Refresh</button>
        </div>

        {error && (
          <div style={errorBox}>
            <FontAwesomeIcon icon={faExclamationTriangle} /> {error}
          </div>
        )}

        {loading && !data && (
          <div style={{ textAlign: 'center', padding: 40, color: 'rgba(180,210,240,0.65)' }}>
            <FontAwesomeIcon icon={faSpinner} spin /> Loading…
          </div>
        )}

        {data && (
          <>
            <div style={metricsGrid}>
              <Metric label="Ready queue" value={totalQueue.toLocaleString()} />
              <Metric label="Flush window" value={`${data.flush_window_hours}h`} />
              <Metric label="Waves remaining" value={String(data.waves_remaining)} />
              <Metric label="Recommended wave size" value={data.recommended_wave_size.toLocaleString()} />
            </div>

            <h4 style={{ color: '#cbd5f5', marginBottom: 8 }}>Live ISP breakdown (ready queue)</h4>
            <div style={{ marginBottom: 16, display: 'flex', height: 24, borderRadius: 4, overflow: 'hidden', border: '1px solid rgba(120,150,200,0.2)' }}>
              {ISP_FAMILIES.map(isp => {
                const count = data.isp_breakdown[isp] ?? 0;
                const pct = totalQueue > 0 ? (count / totalQueue) * 100 : 0;
                if (pct === 0) return null;
                return (
                  <div
                    key={isp}
                    title={`${isp}: ${count.toLocaleString()} (${pct.toFixed(1)}%)`}
                    style={{ width: `${pct}%`, background: ISP_COLORS[isp] ?? '#94a3b8', display: 'flex', alignItems: 'center', justifyContent: 'center', fontSize: 10, color: 'white', fontWeight: 600 }}
                  >
                    {pct >= 8 ? `${isp} ${pct.toFixed(0)}%` : ''}
                  </div>
                );
              })}
            </div>

            <h4 style={{ color: '#cbd5f5', marginBottom: 8 }}>Overrides (per-wave)</h4>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
              <thead>
                <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
                  <th style={th}>ISP</th>
                  <th style={th}>Live %</th>
                  <th style={th}>Override %</th>
                  <th style={th}>Max per wave</th>
                </tr>
              </thead>
              <tbody>
                {ISP_FAMILIES.map(isp => {
                  const count = data.isp_breakdown[isp] ?? 0;
                  const livePct = totalQueue > 0 ? (count / totalQueue) * 100 : 0;
                  const e = edits[isp] ?? { isp, pct: '', cap: '' };
                  return (
                    <tr key={isp} style={tr}>
                      <td style={td}>
                        <span style={{ display: 'inline-block', width: 10, height: 10, borderRadius: 2, background: ISP_COLORS[isp] ?? '#94a3b8', marginRight: 8 }} />
                        <b>{isp}</b>
                      </td>
                      <td style={tdNum}>
                        {count.toLocaleString()} <span style={{ color: 'rgba(180,210,240,0.55)' }}>({livePct.toFixed(1)}%)</span>
                      </td>
                      <td style={td}>
                        <input
                          type="number"
                          min={0}
                          max={100}
                          placeholder="auto"
                          value={e.pct}
                          onChange={ev => updateEdit(isp, 'pct', ev.target.value)}
                          style={inputSmall}
                        />
                        <span style={{ marginLeft: 4, color: 'rgba(180,210,240,0.55)' }}>%</span>
                      </td>
                      <td style={td}>
                        <input
                          type="number"
                          min={0}
                          placeholder="no cap"
                          value={e.cap}
                          onChange={ev => updateEdit(isp, 'cap', ev.target.value)}
                          style={inputSmall}
                        />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>

            <div style={{ display: 'flex', gap: 8, marginTop: 16, justifyContent: 'flex-end' }}>
              <button onClick={onClose} style={ghostBtn}>Close</button>
              <button onClick={handleSave} disabled={saving} style={primaryBtn}>
                {saving && <FontAwesomeIcon icon={faSpinner} spin />} Save (next wave)
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  );
};

const Metric: React.FC<{ label: string; value: string }> = ({ label, value }) => (
  <div style={metricCard}>
    <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 22, color: '#dbeafe', fontWeight: 600, marginTop: 4 }}>{value}</div>
  </div>
);

const modalBackdrop: React.CSSProperties = {
  position: 'fixed', top: 0, left: 0, right: 0, bottom: 0,
  background: 'rgba(0,0,0,0.65)', display: 'flex', alignItems: 'center',
  justifyContent: 'center', zIndex: 1000, padding: 24,
};
const modalBody: React.CSSProperties = {
  background: '#0f1c33', border: '1px solid rgba(120,150,200,0.25)',
  borderRadius: 10, padding: 24, width: '100%', maxWidth: 720,
  boxShadow: '0 20px 60px rgba(0,0,0,0.6)',
  maxHeight: '90vh', overflow: 'auto',
};
const metricsGrid: React.CSSProperties = {
  display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(140px, 1fr))',
  gap: 12, marginBottom: 20,
};
const metricCard: React.CSSProperties = {
  background: 'rgba(120,150,200,0.06)', border: '1px solid rgba(120,150,200,0.15)',
  padding: '10px 14px', borderRadius: 6,
};
const primaryBtn: React.CSSProperties = {
  background: 'linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%)',
  color: 'white', border: 'none', padding: '8px 14px', borderRadius: 6, cursor: 'pointer',
  fontWeight: 600, display: 'inline-flex', alignItems: 'center', gap: 8,
};
const iconBtn: React.CSSProperties = {
  background: 'rgba(120,150,200,0.08)', color: 'rgba(220,235,250,0.85)',
  border: '1px solid rgba(120,150,200,0.18)', padding: '6px 12px', borderRadius: 6,
  cursor: 'pointer', fontSize: 12,
};
const ghostBtn: React.CSSProperties = {
  background: 'transparent', color: '#cbd5f5',
  border: '1px solid rgba(120,150,200,0.3)', padding: '8px 14px', borderRadius: 6,
  cursor: 'pointer',
};
const th: React.CSSProperties = {
  textAlign: 'left', padding: '8px 12px', color: 'rgba(180,210,240,0.65)',
  fontWeight: 500, fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
};
const td: React.CSSProperties = { padding: '8px 12px', borderTop: '1px solid rgba(120,150,200,0.1)' };
const tdNum: React.CSSProperties = { ...td, fontVariantNumeric: 'tabular-nums', textAlign: 'right' };
const tr: React.CSSProperties = {};
const inputSmall: React.CSSProperties = {
  width: 80, padding: '4px 8px', background: 'rgba(0,0,0,0.25)',
  border: '1px solid rgba(120,150,200,0.25)', borderRadius: 4, color: 'white', fontSize: 13,
};
const errorBox: React.CSSProperties = {
  background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)',
  padding: 8, borderRadius: 4, marginBottom: 12, fontSize: 12,
};
