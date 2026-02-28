import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faDatabase, faCheckCircle, faTimesCircle, faSpinner,
  faSync, faPlay, faFileImport, faExclamationTriangle,
  faChartBar, faServer, faClock, faFileAlt,
} from '@fortawesome/free-solid-svg-icons';

interface NormalizerStatus {
  initialized: boolean;
  healthy: boolean;
  running: boolean;
  last_run_at?: string;
  files: { total: number; completed: number; failed: number; processing: number };
  records: { imported: number; errors: number };
}

interface ImportLog {
  id: string;
  original_key: string;
  renamed_key: string;
  classification: string;
  record_count: number;
  error_count: number;
  status: string;
  error_message?: string;
  original_exists: boolean;
  processed_at?: string;
  created_at: string;
}

interface QualityBreakdown {
  verification_status: string;
  count: number;
  avg_quality: number;
}

interface DomainBreakdown {
  domain_group: string;
  count: number;
}

const STATUS_COLORS: Record<string, string> = {
  completed: '#22c55e',
  processing: '#f59e0b',
  failed: '#ef4444',
  pending: '#6b7280',
};

const CLASSIFICATION_COLORS: Record<string, string> = {
  mailable: '#3b82f6',
  suppression: '#ef4444',
  warmup: '#f59e0b',
};

export const DataNormalizerPanel: React.FC = () => {
  const [status, setStatus] = useState<NormalizerStatus | null>(null);
  const [logs, setLogs] = useState<ImportLog[]>([]);
  const [totalLogs, setTotalLogs] = useState(0);
  const [page, setPage] = useState(0);
  const [triggering, setTriggering] = useState(false);
  const [qualityData, setQualityData] = useState<QualityBreakdown[]>([]);
  const [domainData, setDomainData] = useState<DomainBreakdown[]>([]);
  const [subscriberCount, setSubscriberCount] = useState(0);

  const fetchStatus = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/data-normalizer/status', { credentials: 'include' });
      if (res.ok) setStatus(await res.json());
    } catch { /* server unreachable */ }
  }, []);

  const fetchLogs = useCallback(async () => {
    try {
      const res = await fetch(`/api/mailing/data-normalizer/logs?limit=20&offset=${page * 20}`, { credentials: 'include' });
      if (res.ok) {
        const data = await res.json();
        setLogs(data.logs || []);
        if (data.logs?.length === 20) setTotalLogs(Math.max(totalLogs, (page + 1) * 20 + 1));
      }
    } catch { /* */ }
  }, [page, totalLogs]);

  const fetchQuality = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/data-normalizer/quality-breakdown', { credentials: 'include' });
      if (res.ok) {
        const data = await res.json();
        setQualityData(data.verification || []);
        setDomainData(data.domains || []);
        setSubscriberCount(data.total_subscribers || 0);
      }
    } catch { /* */ }
  }, []);

  useEffect(() => {
    fetchStatus();
    fetchLogs();
    fetchQuality();
    const interval = setInterval(() => {
      fetchStatus();
      fetchLogs();
      fetchQuality();
    }, 10000);
    return () => clearInterval(interval);
  }, [fetchStatus, fetchLogs, fetchQuality]);

  const handleTrigger = async () => {
    setTriggering(true);
    try {
      await fetch('/api/mailing/data-normalizer/trigger', { method: 'POST', credentials: 'include' });
      setTimeout(fetchStatus, 2000);
    } catch { /* */ }
    setTriggering(false);
  };

  const progressPct = status?.files
    ? Math.round(((status.files.completed + status.files.failed) / Math.max(status.files.total, 1)) * 100)
    : 0;

  const formatNumber = (n: number) => n.toLocaleString();

  const formatTime = (iso?: string) => {
    if (!iso) return '—';
    const d = new Date(iso);
    return d.toLocaleString();
  };

  const maxDomain = Math.max(...domainData.map(d => d.count), 1);

  return (
    <div style={{ padding: '24px', color: '#e2e8f0', maxWidth: 1200 }}>
      <style>{`@keyframes pulse-count { 0%,100% { opacity: 1; } 50% { opacity: 0.5; } }`}</style>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 24 }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 22, fontWeight: 700 }}>
            <FontAwesomeIcon icon={faDatabase} style={{ marginRight: 10, color: '#818cf8' }} />
            S3 Data Normalizer
          </h2>
          <p style={{ margin: '4px 0 0', fontSize: 13, color: '#8b8fa3' }}>
            Imports and normalizes CSV data from the jvc-email-data S3 bucket
          </p>
        </div>
        <button
          onClick={handleTrigger}
          disabled={triggering || status?.running}
          style={{
            background: status?.running ? '#374151' : 'linear-gradient(135deg, #6366f1, #8b5cf6)',
            color: '#fff',
            border: 'none',
            borderRadius: 8,
            padding: '10px 20px',
            fontSize: 14,
            fontWeight: 600,
            cursor: status?.running ? 'not-allowed' : 'pointer',
            display: 'flex',
            alignItems: 'center',
            gap: 8,
          }}
        >
          <FontAwesomeIcon icon={status?.running ? faSpinner : faPlay} spin={status?.running} />
          {status?.running ? 'Processing…' : 'Trigger Import'}
        </button>
      </div>

      {/* Status Cards */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 16, marginBottom: 24 }}>
        <StatCard
          icon={faFileImport}
          label="Total Files"
          value={status?.files.total ?? 0}
          color="#818cf8"
        />
        <StatCard
          icon={faCheckCircle}
          label="Completed"
          value={status?.files.completed ?? 0}
          color="#22c55e"
        />
        <StatCard
          icon={faSpinner}
          label="Processing"
          value={status?.files.processing ?? 0}
          color="#f59e0b"
        />
        <StatCard
          icon={faTimesCircle}
          label="Failed"
          value={status?.files.failed ?? 0}
          color="#ef4444"
        />
        <StatCard
          icon={faFileAlt}
          label="Records Imported"
          value={formatNumber(status?.records.imported ?? 0)}
          color="#3b82f6"
        />
        <StatCard
          icon={faExclamationTriangle}
          label="Record Errors"
          value={formatNumber(status?.records.errors ?? 0)}
          color={status?.records.errors ? '#ef4444' : '#6b7280'}
        />
        <StatCard
          icon={faServer}
          label="Subscribers"
          value={formatNumber(subscriberCount)}
          color="#8b5cf6"
        />
        <StatCard
          icon={faClock}
          label="Last Run"
          value={status?.last_run_at ? new Date(status.last_run_at).toLocaleTimeString() : '—'}
          color="#6b7280"
          small
        />
      </div>

      {/* Progress Bar */}
      <div style={{ marginBottom: 24 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 6, fontSize: 13, color: '#8b8fa3' }}>
          <span>Import Progress</span>
          <span>{progressPct}% ({(status?.files.completed ?? 0) + (status?.files.failed ?? 0)} / {status?.files.total ?? 0} files)</span>
        </div>
        <div style={{ height: 10, borderRadius: 5, background: '#1e1e2e', overflow: 'hidden' }}>
          <div style={{
            height: '100%',
            width: `${progressPct}%`,
            borderRadius: 5,
            background: 'linear-gradient(90deg, #6366f1, #22c55e)',
            transition: 'width 0.5s ease',
          }} />
        </div>
      </div>

      {/* Charts Row */}
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 24 }}>
        {/* Quality Breakdown */}
        <div style={{ background: '#1a1a2e', borderRadius: 12, padding: 20, border: '1px solid #2a2a3e' }}>
          <h3 style={{ margin: '0 0 16px', fontSize: 15, fontWeight: 600, color: '#c4b5fd' }}>
            <FontAwesomeIcon icon={faChartBar} style={{ marginRight: 8 }} />
            Verification Quality
          </h3>
          {qualityData.length === 0 ? (
            <p style={{ color: '#6b7280', fontSize: 13 }}>No data yet</p>
          ) : (
            qualityData.map(q => {
              const maxQ = Math.max(...qualityData.map(x => x.count), 1);
              const pct = (q.count / maxQ) * 100;
              const color = q.verification_status === 'verified' ? '#22c55e'
                : q.verification_status === 'risky' ? '#f59e0b'
                : q.verification_status === 'invalid' ? '#ef4444'
                : '#6b7280';
              return (
                <div key={q.verification_status || 'null'} style={{ marginBottom: 10 }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 13, marginBottom: 3 }}>
                    <span style={{ color, fontWeight: 600, textTransform: 'capitalize' }}>
                      {q.verification_status || 'unverified'}
                    </span>
                    <span style={{ color: '#8b8fa3' }}>{formatNumber(q.count)} (avg {q.avg_quality})</span>
                  </div>
                  <div style={{ height: 6, borderRadius: 3, background: '#2a2a3e' }}>
                    <div style={{ height: '100%', width: `${pct}%`, borderRadius: 3, background: color, transition: 'width 0.5s' }} />
                  </div>
                </div>
              );
            })
          )}
        </div>

        {/* Domain Group Breakdown */}
        <div style={{ background: '#1a1a2e', borderRadius: 12, padding: 20, border: '1px solid #2a2a3e' }}>
          <h3 style={{ margin: '0 0 16px', fontSize: 15, fontWeight: 600, color: '#c4b5fd' }}>
            <FontAwesomeIcon icon={faChartBar} style={{ marginRight: 8 }} />
            Domain Groups
          </h3>
          {domainData.length === 0 ? (
            <p style={{ color: '#6b7280', fontSize: 13 }}>No data yet</p>
          ) : (
            domainData.slice(0, 10).map(d => {
              const pct = (d.count / maxDomain) * 100;
              return (
                <div key={d.domain_group} style={{ marginBottom: 10 }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 13, marginBottom: 3 }}>
                    <span style={{ color: '#e2e8f0', fontWeight: 600, textTransform: 'capitalize' }}>{d.domain_group}</span>
                    <span style={{ color: '#8b8fa3' }}>{formatNumber(d.count)}</span>
                  </div>
                  <div style={{ height: 6, borderRadius: 3, background: '#2a2a3e' }}>
                    <div style={{ height: '100%', width: `${pct}%`, borderRadius: 3, background: '#818cf8', transition: 'width 0.5s' }} />
                  </div>
                </div>
              );
            })
          )}
        </div>
      </div>

      {/* Import Log Table */}
      <div style={{ background: '#1a1a2e', borderRadius: 12, padding: 20, border: '1px solid #2a2a3e' }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
          <h3 style={{ margin: 0, fontSize: 15, fontWeight: 600, color: '#c4b5fd' }}>
            <FontAwesomeIcon icon={faFileImport} style={{ marginRight: 8 }} />
            Import Log
          </h3>
          <button
            onClick={() => { fetchLogs(); fetchStatus(); }}
            style={{ background: 'none', border: '1px solid #3a3a4e', borderRadius: 6, color: '#8b8fa3', padding: '6px 12px', cursor: 'pointer', fontSize: 12 }}
          >
            <FontAwesomeIcon icon={faSync} style={{ marginRight: 6 }} /> Refresh
          </button>
        </div>

        <div style={{ overflowX: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
            <thead>
              <tr style={{ borderBottom: '1px solid #2a2a3e' }}>
                <th style={thStyle}>File</th>
                <th style={thStyle}>Type</th>
                <th style={thStyle}>Status</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Records</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Errors</th>
                <th style={thStyle}>Processed</th>
              </tr>
            </thead>
            <tbody>
              {logs.length === 0 ? (
                <tr><td colSpan={6} style={{ padding: 20, textAlign: 'center', color: '#6b7280' }}>No import records yet</td></tr>
              ) : (
                logs.map(log => (
                  <tr key={log.id} style={{ borderBottom: '1px solid #1e1e2e' }}>
                    <td style={tdStyle} title={log.original_key}>
                      <span style={{ maxWidth: 300, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', display: 'inline-block' }}>
                        {log.original_key.split('/').pop()}
                      </span>
                    </td>
                    <td style={tdStyle}>
                      <span style={{
                        background: CLASSIFICATION_COLORS[log.classification] + '22',
                        color: CLASSIFICATION_COLORS[log.classification],
                        padding: '2px 8px',
                        borderRadius: 4,
                        fontSize: 11,
                        fontWeight: 600,
                        textTransform: 'uppercase',
                      }}>
                        {log.classification}
                      </span>
                    </td>
                    <td style={tdStyle}>
                      <span style={{
                        display: 'inline-flex',
                        alignItems: 'center',
                        gap: 5,
                        color: STATUS_COLORS[log.status],
                        fontWeight: 600,
                        fontSize: 12,
                      }}>
                        <FontAwesomeIcon
                          icon={log.status === 'completed' ? faCheckCircle : log.status === 'processing' ? faSpinner : log.status === 'failed' ? faTimesCircle : faClock}
                          spin={log.status === 'processing'}
                          style={{ fontSize: 11 }}
                        />
                        {log.status}
                      </span>
                      {log.error_message && (
                        <div style={{ fontSize: 11, color: '#ef4444', marginTop: 2, maxWidth: 250, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={log.error_message}>
                          {log.error_message}
                        </div>
                      )}
                    </td>
                    <td style={{
                      ...tdStyle,
                      textAlign: 'right',
                      fontVariantNumeric: 'tabular-nums',
                      animation: log.status === 'processing' ? 'pulse-count 2s ease-in-out infinite' : undefined,
                    }}>
                      {formatNumber(log.record_count)}
                      {log.status === 'processing' && log.record_count > 0 && (
                        <span style={{ color: '#f59e0b', fontSize: 10, marginLeft: 4 }}>LIVE</span>
                      )}
                    </td>
                    <td style={{ ...tdStyle, textAlign: 'right', color: log.error_count > 0 ? '#ef4444' : '#6b7280', fontVariantNumeric: 'tabular-nums' }}>
                      {formatNumber(log.error_count)}
                    </td>
                    <td style={{ ...tdStyle, color: '#8b8fa3', fontSize: 12 }}>
                      {formatTime(log.processed_at)}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>

        {/* Pagination */}
        {logs.length > 0 && (
          <div style={{ display: 'flex', justifyContent: 'center', gap: 8, marginTop: 16 }}>
            <button
              onClick={() => setPage(Math.max(0, page - 1))}
              disabled={page === 0}
              style={pageBtnStyle(page > 0)}
            >
              Previous
            </button>
            <span style={{ color: '#8b8fa3', fontSize: 13, padding: '6px 12px' }}>Page {page + 1}</span>
            <button
              onClick={() => setPage(page + 1)}
              disabled={logs.length < 20}
              style={pageBtnStyle(logs.length === 20)}
            >
              Next
            </button>
          </div>
        )}
      </div>
    </div>
  );
};

const StatCard: React.FC<{ icon: any; label: string; value: string | number; color: string; small?: boolean }> = ({ icon, label, value, color, small }) => (
  <div style={{
    background: '#1a1a2e',
    borderRadius: 12,
    padding: '16px 18px',
    border: '1px solid #2a2a3e',
    display: 'flex',
    alignItems: 'center',
    gap: 14,
  }}>
    <div style={{
      width: 40, height: 40, borderRadius: 10,
      background: color + '18',
      display: 'flex', alignItems: 'center', justifyContent: 'center',
      color, fontSize: 16,
    }}>
      <FontAwesomeIcon icon={icon} />
    </div>
    <div>
      <div style={{ fontSize: 11, color: '#8b8fa3', marginBottom: 2, textTransform: 'uppercase', letterSpacing: '0.5px' }}>{label}</div>
      <div style={{ fontSize: small ? 13 : 20, fontWeight: 700, color: '#e2e8f0', fontVariantNumeric: 'tabular-nums' }}>{value}</div>
    </div>
  </div>
);

const thStyle: React.CSSProperties = {
  textAlign: 'left',
  padding: '10px 12px',
  color: '#8b8fa3',
  fontSize: 11,
  fontWeight: 600,
  textTransform: 'uppercase',
  letterSpacing: '0.5px',
};

const tdStyle: React.CSSProperties = {
  padding: '10px 12px',
  color: '#e2e8f0',
};

const pageBtnStyle = (enabled: boolean): React.CSSProperties => ({
  background: enabled ? '#2a2a3e' : '#1a1a2e',
  color: enabled ? '#e2e8f0' : '#4b5563',
  border: '1px solid #3a3a4e',
  borderRadius: 6,
  padding: '6px 14px',
  fontSize: 12,
  cursor: enabled ? 'pointer' : 'not-allowed',
});
