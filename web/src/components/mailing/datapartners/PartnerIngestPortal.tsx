import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faPlus, faSync, faSpinner, faExclamationTriangle, faPause, faPlay, faKey, faChartBar,
} from '@fortawesome/free-solid-svg-icons';
import { PartnerOnboardingWizard } from './PartnerOnboardingWizard';
import { DatasetKeysPanel } from './DatasetKeysPanel';
import { BatchInspector } from './BatchInspector';
import { PartnerQualityReport } from './PartnerQualityReport';
import { ISPDistributionPanel } from './ISPDistributionPanel';
import { AuditLogPanel } from './AuditLogPanel';
import { PreviousActivationsPanel } from './PreviousActivationsPanel';
import { ReservoirPanel } from './ReservoirPanel';
import { apiFetch } from '../shared/apiFetch';
import { labelForVertical } from './verticalLabels';

// 1.4: shared vertical-label map (both DP screens agree), governed base-slug
// re-lookup, filtered landing cards (hide 0/0 governed-pointer/followup noise).
// 2.0: lives inside Data Center. Drip-era tabs left the screen (all datasets
// sit behind the send pause; partner data mails through the daily gate), the
// sections fetch only what they show, and the poll cannot overlap itself.
export const PAGE_VERSION = '2.0';
const POLL_MS = 60_000;

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
  paused_emergency: boolean;   // SENDING pause — drip/broadcast claims stop
  paused_reason: string;
  intake_paused: boolean;      // INTAKE pause — the partner door closes (independent, brain #3823)
  intake_paused_reason: string;
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

export type PartnerSection = 'reservoir' | 'partners' | 'batches' | 'activations' | 'audit';

export const PartnerIngestPortal: React.FC<{ section: PartnerSection }> = ({ section }) => {
  const [dashboard, setDashboard] = useState<DashboardResponse | null>(null);
  const [datasets, setDatasets] = useState<DatasetSummary[]>([]);
  const [countsAsOf, setCountsAsOf] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Pause/resume failures live in their own state so the 30s dashboard poll
  // (which resets `error`) can never wipe a failed-emergency-stop banner.
  // A failed emergency stop must never look like success (REQ-004).
  const [actionError, setActionError] = useState<string | null>(null);
  const [wizardOpen, setWizardOpen] = useState(false);
  const [inspectingBatchId, setInspectingBatchId] = useState<string | null>(null);
  const [distributionDataset, setDistributionDataset] = useState<{ id: string; name: string } | null>(null);
  const [keysDataset, setKeysDataset] = useState<{ id: string; name: string } | null>(null);
  const [reportDatasetId, setReportDatasetId] = useState<string>('');
  const [showRawPosts, setShowRawPosts] = useState(false);

  const needsDatasets = section === 'partners' || section === 'batches';
  const needsBatches = section === 'batches';
  const inFlight = useRef(false);

  const fetchAll = useCallback(() => {
    if (!needsDatasets || inFlight.current) return;
    inFlight.current = true;
    setLoading(true);
    setError(null);
    Promise.all([
      needsBatches
        ? apiFetch('/api/mailing/data-partners/dashboard?verticals=0', { credentials: 'include' }).then(r => r.json())
        : Promise.resolve(null),
      apiFetch('/api/mailing/data-partners/datasets', { credentials: 'include' }).then(r => r.json()),
    ])
      .then(([dash, ds]) => {
        // Defensive: an unauthenticated response is `{error:"unauthorized"}`,
        // not a DashboardResponse — keep the rendered tree safe.
        if (dash && Array.isArray(dash.recent_batches)) {
          setDashboard(dash);
        } else if (dash?.error) {
          setError(`Batches fetch: ${dash.error}`);
        }
        if (ds?.error) setError(`Datasets fetch: ${ds.error}`);
        setDatasets(ds?.datasets ?? []);
        setCountsAsOf(ds?.counts_as_of ?? null);
      })
      .catch(err => setError(String(err)))
      .finally(() => {
        inFlight.current = false;
        setLoading(false);
      });
  }, [needsDatasets, needsBatches]);

  useEffect(() => {
    fetchAll();
    const t = setInterval(fetchAll, POLL_MS);
    return () => clearInterval(t);
  }, [fetchAll]);

  const handlePauseDataset = async (id: string) => {
    if (!window.confirm('Stop SENDING for this dataset? Drip and broadcast claims halt at the next safe point; partner intake keeps landing.')) return;
    const reason = window.prompt('Reason for stopping sending:', 'operator emergency stop') ?? '';
    try {
      const res = await apiFetch(`/api/mailing/data-partners/datasets/${id}/emergency-stop`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ reason }),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => null);
        setActionError(`EMERGENCY STOP FAILED (HTTP ${res.status}${body?.error ? `: ${body.error}` : ''}) — the dataset is NOT paused and its queued records are still sending. Retry or stop it server-side.`);
        return;
      }
      setActionError(null);
    } catch (err) {
      setActionError(`EMERGENCY STOP FAILED (${String(err)}) — the dataset is NOT paused and its queued records are still sending. Retry or stop it server-side.`);
      return;
    }
    fetchAll();
  };

  const handleResumeDataset = async (id: string) => {
    try {
      const res = await apiFetch(`/api/mailing/data-partners/datasets/${id}/resume`, { method: 'POST', credentials: 'include' });
      if (!res.ok) {
        const body = await res.json().catch(() => null);
        setActionError(`Resume failed (HTTP ${res.status}${body?.error ? `: ${body.error}` : ''}) — the dataset is still emergency-stopped.`);
        return;
      }
      setActionError(null);
    } catch (err) {
      setActionError(`Resume failed (${String(err)}) — the dataset is still emergency-stopped.`);
      return;
    }
    fetchAll();
  };

  // Intake pause/resume — the INTAKE door only (partner_datasets.intake_paused).
  // Independent of the sending stop above (operator ruling, brain #3823).
  const handleIntakePause = async (id: string) => {
    if (!window.confirm('Pause INTAKE for this dataset? Partner posts and CSV uploads are refused and inbound batches stop slicing. Sending is unaffected.')) return;
    const reason = window.prompt('Reason for pausing intake:', 'operator intake pause') ?? '';
    try {
      const res = await apiFetch(`/api/mailing/data-partners/datasets/${id}/intake-pause`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ reason }),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => null);
        setActionError(`INTAKE PAUSE FAILED (HTTP ${res.status}${body?.error ? `: ${body.error}` : ''}) — the partner door is still open. Retry or pause it server-side.`);
        return;
      }
      setActionError(null);
    } catch (err) {
      setActionError(`INTAKE PAUSE FAILED (${String(err)}) — the partner door is still open. Retry or pause it server-side.`);
      return;
    }
    fetchAll();
  };

  const handleIntakeResume = async (id: string) => {
    try {
      const res = await apiFetch(`/api/mailing/data-partners/datasets/${id}/intake-resume`, { method: 'POST', credentials: 'include' });
      if (!res.ok) {
        const body = await res.json().catch(() => null);
        setActionError(`Intake resume failed (HTTP ${res.status}${body?.error ? `: ${body.error}` : ''}) — intake is still paused.`);
        return;
      }
      setActionError(null);
    } catch (err) {
      setActionError(`Intake resume failed (${String(err)}) — intake is still paused.`);
      return;
    }
    fetchAll();
  };

  const activeTab = section;

  return (
    <div style={{ color: 'var(--j-text)' }}>
      {error && (
        <div style={{ background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)', padding: 12, borderRadius: 6, marginBottom: 12 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {error}
        </div>
      )}

      {actionError && (
        <div style={{ background: 'rgba(239,68,68,0.28)', border: '1px solid rgba(239,68,68,0.6)', padding: 12, borderRadius: 6, marginBottom: 12, fontWeight: 600 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {actionError}{' '}
          <button onClick={() => setActionError(null)} style={{ ...iconBtn, marginLeft: 8 }}>Dismiss</button>
        </div>
      )}

      {loading && needsDatasets && datasets.length === 0 && (
        <div style={{ textAlign: 'center', padding: 60, color: 'var(--j-text-muted)' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading…
        </div>
      )}

      {activeTab === 'partners' && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12 }}>
          <div style={{ fontSize: 12, color: 'var(--j-text-muted)', marginRight: 'auto' }}>
            {countsAsOf
              ? `Ready and mailed counts measured ${new Date(countsAsOf).toLocaleTimeString()} (background scan, about every 10 minutes).`
              : 'Ready and mailed counts appear after the first background scan.'}
          </div>
          <button onClick={fetchAll} style={iconBtn}>
            <FontAwesomeIcon icon={faSync} spin={loading} /> Refresh
          </button>
          <button onClick={() => setWizardOpen(true)} style={primaryBtn}>
            <FontAwesomeIcon icon={faPlus} /> Onboard Partner
          </button>
        </div>
      )}

      {activeTab === 'partners' && (
        <div>
          <table style={tableStyle}>
            <thead>
              <tr style={{ background: 'color-mix(in srgb, var(--j-text-muted) 6%, transparent)' }}>
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
                    <div style={{ fontSize: 11, color: 'var(--j-text-muted)' }}>{d.name}</div>
                  </td>
                  <td style={td}>{labelForVertical(d.vertical)}</td>
                  <td style={tdNum}>{d.flush_window_hours}h</td>
                  <td style={tdNum}>{countsAsOf ? d.ready_queue_count.toLocaleString() : '—'}</td>
                  <td style={tdNum}>{countsAsOf ? d.mailed_count.toLocaleString() : '—'}</td>
                  <td style={td}>
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
                      {d.paused_emergency && (
                        <span style={{ color: 'var(--j-soft)', fontSize: 11, fontWeight: 700 }} title={d.paused_reason || 'sending stopped'}>
                          <FontAwesomeIcon icon={faPause} /> SENDING PAUSED
                        </span>
                      )}
                      {d.intake_paused && (
                        <span style={{ color: 'var(--j-danger)', fontSize: 11, fontWeight: 700 }} title={d.intake_paused_reason || 'intake paused'}>
                          <FontAwesomeIcon icon={faPause} /> INTAKE PAUSED
                        </span>
                      )}
                      {!d.paused_emergency && !d.intake_paused && <span style={{ color: 'var(--j-success)' }}>active</span>}
                    </div>
                  </td>
                  <td style={td}>
                    <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end' }}>
                      <button
                        onClick={() => setDistributionDataset({ id: d.id, name: `${d.partner_name} / ${d.name}` })}
                        style={ghostBtn}
                        title="Mailbox provider distribution and throughput"
                      >
                        <FontAwesomeIcon icon={faChartBar} /> Providers
                      </button>
                      <button
                        onClick={() => setKeysDataset(k => (k?.id === d.id ? null : { id: d.id, name: `${d.partner_name} / ${d.name}` }))}
                        style={ghostBtn}
                        title="API keys for this dataset — list, rotate, revoke"
                      >
                        <FontAwesomeIcon icon={faKey} /> Keys
                      </button>
                      {d.paused_emergency ? (
                        <button onClick={() => handleResumeDataset(d.id)} style={ghostBtn} title="Resume drip/broadcast claims (sending only)">
                          <FontAwesomeIcon icon={faPlay} /> Resume sending
                        </button>
                      ) : (
                        <button onClick={() => handlePauseDataset(d.id)} style={dangerBtn} title="Emergency stop: halts drip/broadcast claims. Intake keeps landing.">
                          <FontAwesomeIcon icon={faPause} /> Stop sending
                        </button>
                      )}
                      {d.intake_paused ? (
                        <button onClick={() => handleIntakeResume(d.id)} style={ghostBtn} title="Reopen the partner door (API posts, CSV uploads, slicing)">
                          <FontAwesomeIcon icon={faPlay} /> Resume intake
                        </button>
                      ) : (
                        <button onClick={() => handleIntakePause(d.id)} style={ghostBtn} title="Close the partner door: API posts and CSV uploads refused, batches stop slicing. Sending unaffected.">
                          <FontAwesomeIcon icon={faPause} /> Pause intake
                        </button>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          {keysDataset && (
            <DatasetKeysPanel
              datasetId={keysDataset.id}
              datasetName={keysDataset.name}
              onClose={() => setKeysDataset(null)}
            />
          )}
        </div>
      )}

      {activeTab === 'batches' && (
        <div>
          {/* Feed picker → quality report (the partner-reportable view) */}
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 14 }}>
            <span style={{ fontSize: 12, color: 'var(--j-text-muted)' }}>Feed:</span>
            <select aria-label="Feed"
              value={reportDatasetId || (datasets[0]?.id ?? '')}
              onChange={e => setReportDatasetId(e.target.value)}
              style={selectInput}
            >
              {datasets.map(d => (
                <option key={d.id} value={d.id}>{d.partner_name} / {d.name} ({labelForVertical(d.vertical)})</option>
              ))}
            </select>
          </div>

          {(reportDatasetId || datasets[0]?.id) ? (
            <PartnerQualityReport datasetId={reportDatasetId || datasets[0].id} />
          ) : (
            <div style={{ color: 'var(--j-text-muted)', fontSize: 13 }}>No datasets onboarded yet.</div>
          )}

          {/* Raw per-POST stream, collapsed by default — debugging only */}
          <div style={{ marginTop: 22 }}>
            <button onClick={() => setShowRawPosts(v => !v)} style={ghostBtn}>
              {showRawPosts ? 'Hide' : 'Show'} raw inbound posts (one row per partner API call — debugging)
            </button>
            {showRawPosts && dashboard && (
              <table style={{ ...tableStyle, marginTop: 10 }}>
                <thead>
                  <tr style={{ background: 'color-mix(in srgb, var(--j-text-muted) 6%, transparent)' }}>
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
                      <td style={td}>{labelForVertical(b.vertical)}</td>
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
            )}
          </div>
        </div>
      )}

      {activeTab === 'reservoir' && <ReservoirPanel />}

      {activeTab === 'activations' && <PreviousActivationsPanel />}
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

const StatusBadge: React.FC<{ status: string; emergencyStopped: boolean }> = ({ status, emergencyStopped }) => {
  if (emergencyStopped) {
    return <span style={{ color: 'var(--j-soft)', fontWeight: 600 }}>Paused</span>;
  }
  const colors: Record<string, string> = {
    received: 'var(--j-blue)',
    slicing: 'var(--j-violet)',
    slicing_complete: 'var(--j-success)',
    validating: 'var(--j-violet)',
    completed: 'var(--j-success)',
    failed: 'var(--j-hard)',
  };
  return <span style={{ color: colors[status] ?? 'var(--j-text-2)', fontWeight: 500 }}>{status}</span>;
};

// ============ Styles ============

const primaryBtn: React.CSSProperties = {
  background: 'linear-gradient(135deg, var(--j-violet-strong) 0%, var(--j-violet-strong) 100%)',
  color: 'var(--j-text)', border: 'none', padding: '8px 14px', borderRadius: 6, cursor: 'pointer',
  fontWeight: 600, display: 'inline-flex', alignItems: 'center', gap: 8,
};
const iconBtn: React.CSSProperties = {
  background: 'color-mix(in srgb, var(--j-text-muted) 8%, transparent)', color: 'var(--j-text)',
  border: '1px solid color-mix(in srgb, var(--j-text-muted) 18%, transparent)', padding: '8px 14px', borderRadius: 6,
  cursor: 'pointer', display: 'inline-flex', alignItems: 'center', gap: 8,
};
const ghostBtn: React.CSSProperties = {
  background: 'transparent', color: 'var(--j-violet)',
  border: '1px solid color-mix(in srgb, var(--j-violet-strong) 40%, transparent)', padding: '4px 10px', borderRadius: 4,
  cursor: 'pointer', fontSize: 12,
};
const dangerBtn: React.CSSProperties = {
  background: 'rgba(239,68,68,0.12)', color: 'var(--j-hard)',
  border: '1px solid rgba(239,68,68,0.35)', padding: '4px 10px', borderRadius: 4,
  cursor: 'pointer', fontSize: 12,
};
const selectInput: React.CSSProperties = {
  background: 'color-mix(in srgb, var(--j-shade) 62.5%, transparent)', color: 'var(--j-text)',
  border: '1px solid color-mix(in srgb, var(--j-text-muted) 25%, transparent)', borderRadius: 6, padding: '6px 10px', fontSize: 13,
  minWidth: 320,
};
const tableStyle: React.CSSProperties = {
  width: '100%', borderCollapse: 'collapse', background: 'color-mix(in srgb, var(--j-shade) 35%, transparent)',
  borderRadius: 8, overflow: 'hidden', fontSize: 13,
};
const th: React.CSSProperties = {
  textAlign: 'left', padding: '10px 12px', color: 'var(--j-text-muted)',
  fontWeight: 500, fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
};
const td: React.CSSProperties = { padding: '10px 12px', borderTop: '1px solid color-mix(in srgb, var(--j-text-muted) 10%, transparent)' };
const tdNum: React.CSSProperties = { ...td, fontVariantNumeric: 'tabular-nums', textAlign: 'right' };
const tdMono: React.CSSProperties = { ...td, fontFamily: 'ui-monospace, monospace', fontSize: 12 };
const tr: React.CSSProperties = {};
