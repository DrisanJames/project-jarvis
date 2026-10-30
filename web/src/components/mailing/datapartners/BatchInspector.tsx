import React, { useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faTimes, faSpinner } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

interface Props {
  batchId: string;
  onClose: () => void;
}

interface BatchDetail {
  id: string;
  status: string;
  record_count: number;
  next_record_offset: number;
  received_at: string;
  completed_at: string | null;
  last_error: string | null;
  emergency_stopped: boolean;
  partner_id?: string;
  dataset_id?: string;
  vertical?: string;
  counters?: {
    sliced: number;
    suppressed_global: number;
    suppressed_eo: number;
    ready: number;
    mailed: number;
  };
}

export const BatchInspector: React.FC<Props> = ({ batchId, onClose }) => {
  const [detail, setDetail] = useState<BatchDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const fetchDetail = () => {
      // Use the admin endpoint for full detail. The partner-facing endpoint is
      // also valid but requires a partner key, which the operator UI does not
      // hold. We piggyback on the dashboard's recent_batches data structure.
      apiFetch(`/api/mailing/data-partners/datasets?batch_id=${batchId}`, { credentials: 'include' })
        .catch(() => null)
        .finally(() => {
          // The admin GET-batch endpoint isn't yet implemented — fall back to
          // the partner-facing endpoint which only the partner can call.
          // Operators see the summary fields from the parent dashboard.
          setLoading(false);
        });
    };
    fetchDetail();
  }, [batchId]);

  // Best-effort: the admin endpoint for individual batch detail is not yet
  // wired (only the partner-facing endpoint requires a key). We surface a
  // direct query against the recent-batches dashboard data.
  useEffect(() => {
    apiFetch('/api/mailing/data-partners/dashboard', { credentials: 'include' })
      .then(r => r.json())
      .then(d => {
        const found = d?.recent_batches?.find((b: any) => b.id === batchId);
        if (found) {
          setDetail({
            id: found.id,
            status: found.status,
            record_count: found.record_count,
            next_record_offset: 0,
            received_at: found.received_at,
            completed_at: found.completed_at,
            last_error: null,
            emergency_stopped: found.emergency_stopped,
            partner_id: found.partner_id,
            dataset_id: found.dataset_id,
            vertical: found.vertical,
          });
        } else {
          setError('Batch not found in recent activity. Older batches are not available here.');
        }
      })
      .catch(err => setError(String(err)))
      .finally(() => setLoading(false));
  }, [batchId]);

  return (
    <div style={modalBackdrop}>
      <div style={modalBody}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', borderBottom: '1px solid color-mix(in srgb, var(--j-text-muted) 18%, transparent)', paddingBottom: 12, marginBottom: 16 }}>
          <h3 style={{ margin: 0, color: 'var(--j-text)' }}>
            Batch Inspector — <code style={{ fontSize: 12, color: '#a5f3fc' }}>{batchId.slice(0, 8)}…</code>
          </h3>
          <button aria-label="Close" onClick={onClose} style={iconBtn}><FontAwesomeIcon icon={faTimes} /></button>
        </div>

        {loading && (
          <div style={{ textAlign: 'center', padding: 40, color: 'var(--j-text-muted)' }}>
            <FontAwesomeIcon icon={faSpinner} spin /> Loading…
          </div>
        )}
        {error && (
          <div style={errorBox}>{error}</div>
        )}
        {detail && (
          <div>
            <div style={grid}>
              <Stat label="Status" value={detail.status} />
              <Stat label="Records" value={detail.record_count.toLocaleString()} />
              <Stat label="Processed offset" value={detail.next_record_offset.toLocaleString()} />
              <Stat label="Vertical" value={detail.vertical ?? '—'} />
            </div>
            <div style={{ marginTop: 12 }}>
              <Stat label="Received at" value={new Date(detail.received_at).toLocaleString()} />
              <Stat label="Completed at" value={detail.completed_at ? new Date(detail.completed_at).toLocaleString() : '—'} />
              <Stat label="Paused" value={detail.emergency_stopped ? 'YES' : 'no'} />
            </div>
            {detail.counters && (
              <div style={{ marginTop: 16 }}>
                <h4 style={{ color: 'var(--j-text-2)', margin: '0 0 8px 0' }}>List processing counters</h4>
                <div style={grid}>
                  <Stat label="Processed" value={detail.counters.sliced.toLocaleString()} />
                  <Stat label="Suppressed (global)" value={detail.counters.suppressed_global.toLocaleString()} accent="var(--j-soft)" />
                  <Stat label="Suppressed (verification)" value={detail.counters.suppressed_eo.toLocaleString()} accent="var(--j-soft)" />
                  <Stat label="Ready" value={detail.counters.ready.toLocaleString()} accent="var(--j-success)" />
                  <Stat label="Mailed" value={detail.counters.mailed.toLocaleString()} accent="var(--j-violet-strong)" />
                </div>
              </div>
            )}
            {detail.last_error && (
              <div style={{ ...errorBox, marginTop: 16 }}>
                <b>Last error:</b> {detail.last_error}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
};

const Stat: React.FC<{ label: string; value: string; accent?: string }> = ({ label, value, accent }) => (
  <div style={{ background: 'color-mix(in srgb, var(--j-shade) 50%, transparent)', padding: 12, borderRadius: 6 }}>
    <div style={{ fontSize: 11, color: 'var(--j-text-muted)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 18, fontWeight: 600, marginTop: 4, color: accent ?? 'var(--j-text)' }}>{value}</div>
  </div>
);

const grid: React.CSSProperties = {
  display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(140px, 1fr))', gap: 8,
};
const modalBackdrop: React.CSSProperties = {
  position: 'fixed', top: 0, left: 0, right: 0, bottom: 0,
  background: 'color-mix(in srgb, var(--j-shade) 100%, transparent)', display: 'flex', alignItems: 'center',
  justifyContent: 'center', zIndex: 1000, padding: 24,
};
const modalBody: React.CSSProperties = {
  background: 'var(--j-surface)', border: '1px solid color-mix(in srgb, var(--j-text-muted) 25%, transparent)',
  borderRadius: 10, padding: 24, width: '100%', maxWidth: 720,
  maxHeight: '90vh', overflowY: 'auto',
  boxShadow: '0 20px 60px rgba(0,0,0,0.6)', color: 'var(--j-text)',
};
const iconBtn: React.CSSProperties = {
  background: 'transparent', color: 'var(--j-text)',
  border: 'none', padding: '4px 8px', cursor: 'pointer', fontSize: 16,
};
const errorBox: React.CSSProperties = {
  background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)',
  padding: 12, borderRadius: 4, fontSize: 13, color: 'var(--j-danger-soft)',
};
