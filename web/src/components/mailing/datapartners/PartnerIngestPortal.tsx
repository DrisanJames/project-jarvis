import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faPlus, faSync, faSpinner, faExclamationTriangle, faPause, faPlay,
  faSeedling, faFingerprint, faRoute, faChartBar, faClipboardList,
} from '@fortawesome/free-solid-svg-icons';
import { PartnerOnboardingWizard } from './PartnerOnboardingWizard';
import { BatchInspector } from './BatchInspector';
import { DripStateCard } from './DripStateCard';
import { DripPerformancePanel } from './DripPerformancePanel';
import { ISPDistributionPanel } from './ISPDistributionPanel';
import { AuditLogPanel } from './AuditLogPanel';
import { apiFetch } from '../shared/apiFetch';

const PAGE_VERSION = '1.1';

const VERTICAL_LABEL: Record<string, string> = {
  refi_heloc: 'Refi / HELOC',
  personal_loans: 'Personal Loans',
  tax_relief: 'Tax Relief',
  remodel: 'Remodel',
};

interface VerticalState {
  vertical: string;
  next_brand_index: number;
  last_wave_at: string | null;
  last_wave_brand: string | null;
  last_wave_size: number | null;
  ready_queue: number;
  pending_eo: number;
  mailed_total: number;
}

interface BatchSummary {
  id: string;
  dataset_id: string;
  dataset_name: string;
  partner_id: string;
  partner_name: string;
  vertical: string;
  status: string;
  record_count: number;
  received_at: string;
  completed_at: string | null;
  emergency_stopped: boolean;
}

interface DatasetSummary {
  id: string;
  partner_id: string;
  partner_name: string;
  partner_slug: string;
  name: string;
  slug: string;
  vertical: string;
  flush_window_hours: number;
  paused_emergency: boolean;
  paused_reason: string;
  status: string;
  created_at: string;
  batch_count: number;
  ready_queue_count: number;
  mailed_count: number;
}

interface DashboardResponse {
  verticals: VerticalState[];
  recent_batches: BatchSummary[];
}

type TabId = 'overview' | 'partners' | 'batches' | 'creatives' | 'audit';

export const PartnerIngestPortal: React.FC = () => {
  const [activeTab, setActiveTab] = useState<TabId>('overview');
  const [dashboard, setDashboard] = useState<DashboardResponse | null>(null);
  const [datasets, setDatasets] = useState<DatasetSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [wizardOpen, setWizardOpen] = useState(false);
  const [inspectingBatchId, setInspectingBatchId] = useState<string | null>(null);
  const [distributionDataset, setDistributionDataset] = useState<{ id: string; name: string } | null>(null);

  const fetchAll = useCallback(() => {
    setLoading(true);
    setError(null);
    Promise.all([
      apiFetch('/api/mailing/data-partners/dashboard', { credentials: 'include' }).then(r => r.json()),
      apiFetch('/api/mailing/data-partners/datasets', { credentials: 'include' }).then(r => r.json()),
    ])
      .then(([dash, ds]) => {
        // Defensive: an unauthenticated response is `{error:"unauthorized"}`,
        // not a DashboardResponse — keep the rendered tree safe.
        if (dash && Array.isArray(dash.verticals) && Array.isArray(dash.recent_batches)) {
          setDashboard(dash);
        } else {
          setDashboard(null);
          if (dash && dash.error) setError(`Dashboard fetch: ${dash.error}`);
        }
        setDatasets(ds?.datasets ?? []);
      })
      .catch(err => setError(String(err)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    fetchAll();
    const t = setInterval(fetchAll, 30000);
    return () => clearInterval(t);
  }, [fetchAll]);

  const handlePauseDataset = async (id: string) => {
    if (!window.confirm('Emergency stop this dataset? Slicer will halt at the next slice boundary.')) return;
    const reason = window.prompt('Reason for emergency stop:', 'operator emergency stop') ?? '';
    await apiFetch(`/api/mailing/data-partners/datasets/${id}/emergency-stop`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ reason }),
    });
    fetchAll();
  };

  const handleResumeDataset = async (id: string) => {
    await apiFetch(`/api/mailing/data-partners/datasets/${id}/resume`, { method: 'POST', credentials: 'include' });
    fetchAll();
  };

  const tabs: { id: TabId; label: string; icon: typeof faSeedling }[] = [
    { id: 'overview', label: 'Overview', icon: faRoute },
    { id: 'partners', label: 'Partners & Datasets', icon: faFingerprint },
    { id: 'batches', label: 'Inbound Batches', icon: faSeedling },
    { id: 'creatives', label: 'Drip Creatives', icon: faSeedling },
    { id: 'audit', label: 'Audit Log', icon: faClipboardList },
  ];

  return (
    <div style={{ padding: 24, color: 'rgba(220,235,250,0.92)' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 12 }}>
        <div>
          <h2 style={{ margin: 0, color: '#dbeafe' }}>Data Partner Ingestion</h2>
          <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.65)', marginTop: 4 }}>
            API-key-authenticated inbound API → S3 dormant store → suppression+EO cleaning → 15-min round-robin drip across 4 mature brands. v{PAGE_VERSION}
          </div>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={fetchAll} style={iconBtn}>
            <FontAwesomeIcon icon={faSync} spin={loading} /> Refresh
          </button>
          <button onClick={() => setWizardOpen(true)} style={primaryBtn}>
            <FontAwesomeIcon icon={faPlus} /> Onboard Partner
          </button>
        </div>
      </div>

      {error && (
        <div style={{ background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)', padding: 12, borderRadius: 6, marginBottom: 12 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {error}
        </div>
      )}

      <div style={{ display: 'flex', gap: 4, borderBottom: '1px solid rgba(120,150,200,0.18)', marginBottom: 16 }}>
        {tabs.map(t => (
          <button
            key={t.id}
            onClick={() => setActiveTab(t.id)}
            style={{
              ...tabBtn,
              borderBottom: activeTab === t.id ? '2px solid #6366f1' : '2px solid transparent',
              color: activeTab === t.id ? '#cbd5f5' : 'rgba(180,210,240,0.65)',
            }}
          >
            <FontAwesomeIcon icon={t.icon} /> {t.label}
          </button>
        ))}
      </div>

      {loading && !dashboard && (
        <div style={{ textAlign: 'center', padding: 60, color: 'rgba(180,210,240,0.65)' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading…
        </div>
      )}

      {activeTab === 'overview' && dashboard && (
        <div>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 16, marginBottom: 24 }}>
            {dashboard.verticals.map(v => (
              <DripStateCard
                key={v.vertical}
                vertical={v.vertical}
                verticalLabel={VERTICAL_LABEL[v.vertical] ?? v.vertical}
                readyQueue={v.ready_queue}
                pendingEO={v.pending_eo}
                mailedTotal={v.mailed_total}
                lastWaveAt={v.last_wave_at}
                lastWaveBrand={v.last_wave_brand}
                lastWaveSize={v.last_wave_size}
                nextBrandIndex={v.next_brand_index}
                onChanged={fetchAll}
              />
            ))}
          </div>

          <DripPerformancePanel />

          <h3 style={{ color: '#dbeafe', borderBottom: '1px solid rgba(120,150,200,0.18)', paddingBottom: 6 }}>Recent Batches</h3>
          <table style={tableStyle}>
            <thead>
              <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
                <th style={th}>Received</th>
                <th style={th}>Partner / Dataset</th>
                <th style={th}>Vertical</th>
                <th style={th}>Records</th>
                <th style={th}>Status</th>
                <th style={th}></th>
              </tr>
            </thead>
            <tbody>
              {dashboard.recent_batches.map(b => (
                <tr key={b.id} style={tr}>
                  <td style={td}>{new Date(b.received_at).toLocaleString()}</td>
                  <td style={td}>
                    <div style={{ fontWeight: 600 }}>{b.partner_name}</div>
                    <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>{b.dataset_name}</div>
                  </td>
                  <td style={td}>{VERTICAL_LABEL[b.vertical] ?? b.vertical}</td>
                  <td style={tdNum}>{b.record_count.toLocaleString()}</td>
                  <td style={td}>
                    <StatusBadge status={b.status} emergencyStopped={b.emergency_stopped} />
                  </td>
                  <td style={td}>
                    <button onClick={() => setInspectingBatchId(b.id)} style={ghostBtn}>Inspect</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {activeTab === 'partners' && (
        <div>
          <table style={tableStyle}>
            <thead>
              <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
                <th style={th}>Partner / Dataset</th>
                <th style={th}>Vertical</th>
                <th style={th}>Flush Window</th>
                <th style={th}>Ready Queue</th>
                <th style={th}>Mailed</th>
                <th style={th}>Status</th>
                <th style={th}></th>
              </tr>
            </thead>
            <tbody>
              {datasets.map(d => (
                <tr key={d.id} style={tr}>
                  <td style={td}>
                    <div style={{ fontWeight: 600 }}>{d.partner_name}</div>
                    <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>{d.name}</div>
                  </td>
                  <td style={td}>{VERTICAL_LABEL[d.vertical] ?? d.vertical}</td>
                  <td style={tdNum}>{d.flush_window_hours}h</td>
                  <td style={tdNum}>{d.ready_queue_count.toLocaleString()}</td>
                  <td style={tdNum}>{d.mailed_count.toLocaleString()}</td>
                  <td style={td}>
                    {d.paused_emergency ? (
                      <span style={{ color: '#f59e0b' }}>
                        <FontAwesomeIcon icon={faPause} /> {d.paused_reason || 'paused'}
                      </span>
                    ) : (
                      <span style={{ color: '#10b981' }}>active</span>
                    )}
                  </td>
                  <td style={td}>
                    <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end' }}>
                      <button
                        onClick={() => setDistributionDataset({ id: d.id, name: `${d.partner_name} / ${d.name}` })}
                        style={ghostBtn}
                        title="ISP distribution & throughput"
                      >
                        <FontAwesomeIcon icon={faChartBar} /> ISP
                      </button>
                      {d.paused_emergency ? (
                        <button onClick={() => handleResumeDataset(d.id)} style={ghostBtn}>
                          <FontAwesomeIcon icon={faPlay} /> Resume
                        </button>
                      ) : (
                        <button onClick={() => handlePauseDataset(d.id)} style={dangerBtn}>
                          <FontAwesomeIcon icon={faPause} /> Stop
                        </button>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {activeTab === 'batches' && dashboard && (
        <div>
          <table style={tableStyle}>
            <thead>
              <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
                <th style={th}>Batch ID</th>
                <th style={th}>Partner / Dataset</th>
                <th style={th}>Vertical</th>
                <th style={th}>Status</th>
                <th style={th}>Records</th>
                <th style={th}>Received</th>
                <th style={th}></th>
              </tr>
            </thead>
            <tbody>
              {dashboard.recent_batches.map(b => (
                <tr key={b.id} style={tr}>
                  <td style={tdMono}>{b.id.slice(0, 8)}…</td>
                  <td style={td}>{b.partner_name} / {b.dataset_name}</td>
                  <td style={td}>{VERTICAL_LABEL[b.vertical] ?? b.vertical}</td>
                  <td style={td}><StatusBadge status={b.status} emergencyStopped={b.emergency_stopped} /></td>
                  <td style={tdNum}>{b.record_count.toLocaleString()}</td>
                  <td style={td}>{new Date(b.received_at).toLocaleString()}</td>
                  <td style={td}>
                    <button onClick={() => setInspectingBatchId(b.id)} style={ghostBtn}>Inspect</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {activeTab === 'creatives' && <CreativesPanel />}

      {activeTab === 'audit' && <AuditLogPanel />}

      {wizardOpen && (
        <PartnerOnboardingWizard
          onClose={() => { setWizardOpen(false); fetchAll(); }}
        />
      )}

      {inspectingBatchId && (
        <BatchInspector
          batchId={inspectingBatchId}
          onClose={() => setInspectingBatchId(null)}
        />
      )}

      {distributionDataset && (
        <ISPDistributionPanel
          datasetId={distributionDataset.id}
          datasetName={distributionDataset.name}
          onClose={() => { setDistributionDataset(null); fetchAll(); }}
        />
      )}
    </div>
  );
};

// ============ Creatives panel ============

interface CreativeRow {
  vertical: string;
  brand: string;
  creative_filename: string;
  subject_line: string;
  preheader: string;
  from_name: string;
  active: boolean;
  effective_from: string;
  updated_by: string;
}

const CreativesPanel: React.FC = () => {
  const [creatives, setCreatives] = useState<CreativeRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [editing, setEditing] = useState<{ vertical: string; brand: string } | null>(null);

  const fetchCreatives = useCallback(() => {
    setLoading(true);
    apiFetch('/api/mailing/data-partners/creatives', { credentials: 'include' })
      .then(r => r.json())
      .then(data => setCreatives(data?.creatives ?? []))
      .finally(() => setLoading(false));
  }, []);
  useEffect(() => { fetchCreatives(); }, [fetchCreatives]);

  const grid: Record<string, CreativeRow[]> = {};
  creatives.forEach(c => {
    if (!grid[c.vertical]) grid[c.vertical] = [];
    grid[c.vertical].push(c);
  });

  if (loading) return <div style={{ color: 'rgba(180,210,240,0.65)' }}><FontAwesomeIcon icon={faSpinner} spin /> Loading creatives…</div>;

  return (
    <div>
      <div style={{ marginBottom: 12, fontSize: 13, color: 'rgba(180,210,240,0.7)' }}>
        Hot-swappable creatives for the partner drip. One row per (vertical × brand). Updates take effect on the <b>next wave</b>.
      </div>
      {Object.entries(grid).map(([vertical, rows]) => (
        <div key={vertical} style={{ marginBottom: 24 }}>
          <h3 style={{ color: '#cbd5f5', margin: '0 0 8px 0' }}>{VERTICAL_LABEL[vertical] ?? vertical}</h3>
          <table style={tableStyle}>
            <thead>
              <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
                <th style={th}>Brand</th>
                <th style={th}>Creative</th>
                <th style={th}>Subject</th>
                <th style={th}>Preheader</th>
                <th style={th}>From Name</th>
                <th style={th}>Effective</th>
                <th style={th}></th>
              </tr>
            </thead>
            <tbody>
              {rows.map(c => (
                <tr key={c.brand} style={tr}>
                  <td style={td}><b>{c.brand.toUpperCase()}</b></td>
                  <td style={tdMono}>{c.creative_filename}</td>
                  <td style={td}>{c.subject_line}</td>
                  <td style={td}>{c.preheader || <i style={{ opacity: 0.5 }}>(empty)</i>}</td>
                  <td style={td}>{c.from_name}</td>
                  <td style={td}>{new Date(c.effective_from).toLocaleString()}</td>
                  <td style={td}>
                    <button onClick={() => setEditing({ vertical: c.vertical, brand: c.brand })} style={ghostBtn}>Edit</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
      {editing && (
        <CreativeEditModal
          vertical={editing.vertical}
          brand={editing.brand}
          existing={creatives.find(c => c.vertical === editing.vertical && c.brand === editing.brand)}
          onClose={() => setEditing(null)}
          onSaved={() => { setEditing(null); fetchCreatives(); }}
        />
      )}
    </div>
  );
};

interface CreativeEditModalProps {
  vertical: string;
  brand: string;
  existing?: CreativeRow;
  onClose: () => void;
  onSaved: () => void;
}

const CreativeEditModal: React.FC<CreativeEditModalProps> = ({ vertical, brand, existing, onClose, onSaved }) => {
  const [filename, setFilename] = useState(existing?.creative_filename ?? '');
  const [subject, setSubject] = useState(existing?.subject_line ?? '');
  const [preheader, setPreheader] = useState(existing?.preheader ?? '');
  const [fromName, setFromName] = useState(existing?.from_name ?? '');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    try {
      const res = await apiFetch(`/api/mailing/data-partners/creatives/${vertical}/${brand}`, {
        method: 'PUT',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ creative_filename: filename, subject_line: subject, preheader, from_name: fromName }),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        setError(body.error ?? `HTTP ${res.status}`);
        setSaving(false);
        return;
      }
      onSaved();
    } catch (err) {
      setError(String(err));
      setSaving(false);
    }
  };

  return (
    <div style={modalBackdrop}>
      <div style={modalBody}>
        <h3 style={{ marginTop: 0, color: '#dbeafe' }}>Edit creative — {VERTICAL_LABEL[vertical] ?? vertical} / {brand.toUpperCase()}</h3>
        <label style={fieldLabel}>Creative filename (must exist in docs/emails/)</label>
        <input value={filename} onChange={e => setFilename(e.target.value)} style={input} />
        <label style={fieldLabel}>Subject line</label>
        <input value={subject} onChange={e => setSubject(e.target.value)} style={input} />
        <label style={fieldLabel}>Preheader</label>
        <input value={preheader} onChange={e => setPreheader(e.target.value)} style={input} />
        <label style={fieldLabel}>From name</label>
        <input value={fromName} onChange={e => setFromName(e.target.value)} style={input} />
        {error && <div style={errorBox}>{error}</div>}
        <div style={{ display: 'flex', gap: 8, marginTop: 16, justifyContent: 'flex-end' }}>
          <button onClick={onClose} style={ghostBtn}>Cancel</button>
          <button onClick={handleSave} disabled={saving} style={primaryBtn}>
            {saving && <FontAwesomeIcon icon={faSpinner} spin />} Save (next wave)
          </button>
        </div>
      </div>
    </div>
  );
};

// ============ Status badge ============

const StatusBadge: React.FC<{ status: string; emergencyStopped: boolean }> = ({ status, emergencyStopped }) => {
  if (emergencyStopped) {
    return <span style={{ color: '#f59e0b', fontWeight: 600 }}>EMERGENCY STOP</span>;
  }
  const colors: Record<string, string> = {
    received: '#60a5fa',
    slicing: '#a78bfa',
    slicing_complete: '#10b981',
    validating: '#a78bfa',
    completed: '#10b981',
    failed: '#ef4444',
  };
  return <span style={{ color: colors[status] ?? '#cbd5f5', fontWeight: 500 }}>{status}</span>;
};

// ============ Styles ============

const primaryBtn: React.CSSProperties = {
  background: 'linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%)',
  color: 'white', border: 'none', padding: '8px 14px', borderRadius: 6, cursor: 'pointer',
  fontWeight: 600, display: 'inline-flex', alignItems: 'center', gap: 8,
};
const iconBtn: React.CSSProperties = {
  background: 'rgba(120,150,200,0.08)', color: 'rgba(220,235,250,0.85)',
  border: '1px solid rgba(120,150,200,0.18)', padding: '8px 14px', borderRadius: 6,
  cursor: 'pointer', display: 'inline-flex', alignItems: 'center', gap: 8,
};
const ghostBtn: React.CSSProperties = {
  background: 'transparent', color: '#6366f1',
  border: '1px solid rgba(99,102,241,0.4)', padding: '4px 10px', borderRadius: 4,
  cursor: 'pointer', fontSize: 12,
};
const dangerBtn: React.CSSProperties = {
  background: 'rgba(239,68,68,0.12)', color: '#ef4444',
  border: '1px solid rgba(239,68,68,0.35)', padding: '4px 10px', borderRadius: 4,
  cursor: 'pointer', fontSize: 12,
};
const tabBtn: React.CSSProperties = {
  background: 'transparent', border: 'none', padding: '10px 16px',
  cursor: 'pointer', fontSize: 14, display: 'inline-flex', alignItems: 'center', gap: 8,
};
const tableStyle: React.CSSProperties = {
  width: '100%', borderCollapse: 'collapse', background: 'rgba(15,30,60,0.35)',
  borderRadius: 8, overflow: 'hidden', fontSize: 13,
};
const th: React.CSSProperties = {
  textAlign: 'left', padding: '10px 12px', color: 'rgba(180,210,240,0.65)',
  fontWeight: 500, fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
};
const td: React.CSSProperties = { padding: '10px 12px', borderTop: '1px solid rgba(120,150,200,0.1)' };
const tdNum: React.CSSProperties = { ...td, fontVariantNumeric: 'tabular-nums', textAlign: 'right' };
const tdMono: React.CSSProperties = { ...td, fontFamily: 'ui-monospace, monospace', fontSize: 12 };
const tr: React.CSSProperties = {};
const modalBackdrop: React.CSSProperties = {
  position: 'fixed', top: 0, left: 0, right: 0, bottom: 0,
  background: 'rgba(0,0,0,0.65)', display: 'flex', alignItems: 'center',
  justifyContent: 'center', zIndex: 1000, padding: 24,
};
const modalBody: React.CSSProperties = {
  background: '#0f1c33', border: '1px solid rgba(120,150,200,0.25)',
  borderRadius: 10, padding: 24, width: '100%', maxWidth: 540,
  boxShadow: '0 20px 60px rgba(0,0,0,0.6)',
};
const fieldLabel: React.CSSProperties = {
  display: 'block', fontSize: 12, color: 'rgba(180,210,240,0.7)', margin: '12px 0 4px 0',
};
const input: React.CSSProperties = {
  width: '100%', padding: '8px 10px', background: 'rgba(0,0,0,0.25)',
  border: '1px solid rgba(120,150,200,0.25)', borderRadius: 4, color: 'white', fontSize: 13,
};
const errorBox: React.CSSProperties = {
  background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)',
  padding: 8, borderRadius: 4, marginTop: 12, fontSize: 12,
};
