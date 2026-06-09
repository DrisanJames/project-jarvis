import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faDatabase, faCheckCircle, faBan, faCopy,
  faPlay, faSpinner, faExclamationTriangle, faSync,
  faChartBar, faServer, faFolderOpen,
} from '@fortawesome/free-solid-svg-icons';
import {
  BarChart, Bar, XAxis, YAxis, CartesianGrid,
  Tooltip as RechartsTooltip, Legend, ResponsiveContainer,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';

const PAGE_VERSION = '1.0';

interface PipelineStats {
  total_runs: number;
  last_run_at: string | null;
  last_run_status: string;
  total_verified: number;
  total_suppressed: number;
  total_deduped: number;
  total_files: number;
  files_available: number;
  files_processed: number;
  today: { verified: number; suppressed: number; deduped: number; files: number };
  yesterday: { verified: number; suppressed: number; deduped: number; files: number };
}

interface PipelineRun {
  id: string;
  started_at: string;
  completed_at: string | null;
  status: string;
  files_processed: number;
  emails_total: number;
  emails_verified: number;
  emails_suppressed: number;
  emails_deduped: number;
  error_message: string | null;
  notification_sent: boolean;
}

interface DomainHealth {
  sending_domain: string;
  isp: string;
  list_id: string;
  list_name: string;
  subscriber_count: number;
  files_available: number;
}

interface ChartDay {
  date: string;
  verified: number;
  suppressed: number;
  deduped: number;
  files: number;
  runs: number;
}

export const DataPipelineDashboard: React.FC = () => {
  const [stats, setStats] = useState<PipelineStats | null>(null);
  const [runs, setRuns] = useState<PipelineRun[]>([]);
  const [health, setHealth] = useState<DomainHealth[]>([]);
  const [chartData, setChartData] = useState<ChartDay[]>([]);
  const [loading, setLoading] = useState(true);
  const [triggering, setTriggering] = useState(false);

  const fetchAll = useCallback(() => {
    Promise.all([
      apiFetch('/api/mailing/pipeline/stats', { credentials: 'include' }).then(r => r.json()),
      apiFetch('/api/mailing/pipeline/runs', { credentials: 'include' }).then(r => r.json()),
      apiFetch('/api/mailing/pipeline/health', { credentials: 'include' }).then(r => r.json()),
      apiFetch('/api/mailing/pipeline/chart', { credentials: 'include' }).then(r => r.json()),
    ]).then(([statsRes, runsRes, healthRes, chartRes]) => {
      setStats(statsRes);
      setRuns(runsRes?.runs ?? []);
      setHealth(healthRes?.domain_health ?? []);
      setChartData(chartRes?.chart_data ?? []);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, []);

  useEffect(() => {
    fetchAll();
    const interval = setInterval(fetchAll, 60000);
    return () => clearInterval(interval);
  }, [fetchAll]);

  const handleTrigger = async () => {
    setTriggering(true);
    try {
      await apiFetch('/api/mailing/pipeline/trigger', {
        method: 'POST',
        credentials: 'include',
      });
      setTimeout(fetchAll, 3000);
    } finally {
      setTriggering(false);
    }
  };

  if (loading) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '50vh', color: 'rgba(180,210,240,0.65)', gap: 10, fontSize: 14 }}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading pipeline data...
      </div>
    );
  }

  const s = stats;

  return (
    <div className="manager-page" style={{ padding: '24px 32px', maxWidth: 1400 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 28 }}>
        <div>
          <h2 style={{ color: '#e2e8f0', margin: 0, fontSize: 22, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faDatabase} style={{ marginRight: 10, color: '#818cf8' }} />
            Data Ingestion Pipeline
          </h2>
          <p style={{ color: 'rgba(148,163,184,0.8)', margin: '6px 0 0', fontSize: 13 }}>
            Automated S3 ingestion, EmailOversight validation & list replenishment
          </p>
        </div>
        <button
          onClick={handleTrigger}
          disabled={triggering}
          style={{
            background: 'linear-gradient(135deg, #6366f1, #8b5cf6)',
            color: '#fff',
            border: 'none',
            borderRadius: 8,
            padding: '10px 20px',
            fontSize: 13,
            fontWeight: 600,
            cursor: triggering ? 'wait' : 'pointer',
            display: 'flex',
            alignItems: 'center',
            gap: 8,
            opacity: triggering ? 0.7 : 1,
          }}
        >
          <FontAwesomeIcon icon={triggering ? faSpinner : faPlay} spin={triggering} />
          {triggering ? 'Triggering...' : 'Run Now'}
        </button>
      </div>

      {/* Stats Cards */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 16, marginBottom: 28 }}>
        <StatCard icon={faCheckCircle} label="Total Verified" value={s?.total_verified ?? 0} color="#22c55e" sub="Added to lists" />
        <StatCard icon={faBan} label="Total Suppressed" value={s?.total_suppressed ?? 0} color="#ef4444" sub="Sent to global suppression" />
        <StatCard icon={faCopy} label="Total Deduped" value={s?.total_deduped ?? 0} color="#f59e0b" sub="Already existed" />
        <StatCard icon={faFolderOpen} label="Files Available" value={s?.files_available ?? 0} color="#818cf8" sub={`${s?.files_processed ?? 0} processed`} />
        <StatCard icon={faSync} label="Total Runs" value={s?.total_runs ?? 0} color="#06b6d4" sub={s?.last_run_at ? `Last: ${new Date(s.last_run_at).toLocaleDateString()}` : 'Never run'} />
      </div>

      {/* Today vs Yesterday */}
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 28 }}>
        <DayCard title="Today" data={s?.today} />
        <DayCard title="Yesterday" data={s?.yesterday} />
      </div>

      {/* Chart */}
      {chartData.length > 0 && (
        <div style={{ background: 'rgba(15,23,42,0.6)', borderRadius: 12, border: '1px solid rgba(100,116,139,0.2)', padding: 20, marginBottom: 28 }}>
          <h3 style={{ color: '#e2e8f0', margin: '0 0 16px', fontSize: 15, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faChartBar} style={{ marginRight: 8, color: '#818cf8' }} />
            30-Day Pipeline Activity
          </h3>
          <ResponsiveContainer width="100%" height={280}>
            <BarChart data={chartData}>
              <CartesianGrid strokeDasharray="3 3" stroke="rgba(100,116,139,0.15)" />
              <XAxis dataKey="date" tick={{ fill: '#94a3b8', fontSize: 11 }} tickFormatter={d => d.slice(5)} />
              <YAxis tick={{ fill: '#94a3b8', fontSize: 11 }} />
              <RechartsTooltip
                contentStyle={{ background: '#1e293b', border: '1px solid rgba(100,116,139,0.3)', borderRadius: 8, fontSize: 12 }}
                labelStyle={{ color: '#e2e8f0' }}
              />
              <Legend wrapperStyle={{ fontSize: 12 }} />
              <Bar dataKey="verified" name="Verified" fill="#22c55e" radius={[3, 3, 0, 0]} />
              <Bar dataKey="suppressed" name="Suppressed" fill="#ef4444" radius={[3, 3, 0, 0]} />
              <Bar dataKey="deduped" name="Deduped" fill="#f59e0b" radius={[3, 3, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        </div>
      )}

      {/* Domain Health Table */}
      {health.length > 0 && (
        <div style={{ background: 'rgba(15,23,42,0.6)', borderRadius: 12, border: '1px solid rgba(100,116,139,0.2)', padding: 20, marginBottom: 28 }}>
          <h3 style={{ color: '#e2e8f0', margin: '0 0 16px', fontSize: 15, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faServer} style={{ marginRight: 8, color: '#06b6d4' }} />
            Domain / ISP Health
          </h3>
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
              <thead>
                <tr style={{ borderBottom: '1px solid rgba(100,116,139,0.2)' }}>
                  <th style={thStyle}>Sending Domain</th>
                  <th style={thStyle}>ISP</th>
                  <th style={thStyle}>List</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Subscribers</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Files Available</th>
                </tr>
              </thead>
              <tbody>
                {health.map((h, i) => (
                  <tr key={i} style={{ borderBottom: '1px solid rgba(100,116,139,0.1)' }}>
                    <td style={tdStyle}>{h.sending_domain}</td>
                    <td style={tdStyle}>
                      <span style={{ background: ispColor(h.isp), color: '#fff', padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 600 }}>
                        {h.isp.toUpperCase()}
                      </span>
                    </td>
                    <td style={{ ...tdStyle, color: '#94a3b8' }}>{h.list_name}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontWeight: 600, fontFamily: 'monospace' }}>{h.subscriber_count.toLocaleString()}</td>
                    <td style={{ ...tdStyle, textAlign: 'right' }}>
                      <span style={{ color: h.files_available > 0 ? '#22c55e' : '#ef4444', fontWeight: 600, fontFamily: 'monospace' }}>
                        {h.files_available}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Run History */}
      <div style={{ background: 'rgba(15,23,42,0.6)', borderRadius: 12, border: '1px solid rgba(100,116,139,0.2)', padding: 20 }}>
        <h3 style={{ color: '#e2e8f0', margin: '0 0 16px', fontSize: 15, fontWeight: 600 }}>
          <FontAwesomeIcon icon={faSync} style={{ marginRight: 8, color: '#818cf8' }} />
          Run History
        </h3>
        {runs.length === 0 ? (
          <p style={{ color: '#64748b', fontSize: 13 }}>No pipeline runs yet.</p>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
              <thead>
                <tr style={{ borderBottom: '1px solid rgba(100,116,139,0.2)' }}>
                  <th style={thStyle}>Started</th>
                  <th style={thStyle}>Status</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Files</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Verified</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Suppressed</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Deduped</th>
                  <th style={thStyle}>Duration</th>
                </tr>
              </thead>
              <tbody>
                {runs.map(r => (
                  <tr key={r.id} style={{ borderBottom: '1px solid rgba(100,116,139,0.1)' }}>
                    <td style={tdStyle}>{new Date(r.started_at).toLocaleString()}</td>
                    <td style={tdStyle}>
                      <StatusBadge status={r.status} />
                    </td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontFamily: 'monospace' }}>{r.files_processed}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontFamily: 'monospace', color: '#22c55e' }}>{r.emails_verified.toLocaleString()}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontFamily: 'monospace', color: '#ef4444' }}>{r.emails_suppressed.toLocaleString()}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontFamily: 'monospace', color: '#f59e0b' }}>{r.emails_deduped.toLocaleString()}</td>
                    <td style={{ ...tdStyle, fontFamily: 'monospace', color: '#94a3b8' }}>
                      {r.completed_at ? formatDuration(r.started_at, r.completed_at) : '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div style={{ textAlign: 'right', marginTop: 16, color: '#475569', fontSize: 11 }}>
        Data Pipeline Dashboard v{PAGE_VERSION}
      </div>
    </div>
  );
};

const StatCard: React.FC<{ icon: any; label: string; value: number; color: string; sub: string }> = ({ icon, label, value, color, sub }) => (
  <div style={{
    background: 'rgba(15,23,42,0.6)',
    borderRadius: 12,
    border: '1px solid rgba(100,116,139,0.2)',
    padding: '18px 20px',
  }}>
    <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 8 }}>
      <FontAwesomeIcon icon={icon} style={{ color, fontSize: 16 }} />
      <span style={{ color: '#94a3b8', fontSize: 12, fontWeight: 500 }}>{label}</span>
    </div>
    <div style={{ fontSize: 26, fontWeight: 700, color: '#e2e8f0', fontFamily: 'monospace' }}>
      {value.toLocaleString()}
    </div>
    <div style={{ color: '#64748b', fontSize: 11, marginTop: 4 }}>{sub}</div>
  </div>
);

const DayCard: React.FC<{ title: string; data?: { verified: number; suppressed: number; deduped: number; files: number } }> = ({ title, data }) => (
  <div style={{
    background: 'rgba(15,23,42,0.6)',
    borderRadius: 12,
    border: '1px solid rgba(100,116,139,0.2)',
    padding: '18px 20px',
  }}>
    <h4 style={{ color: '#94a3b8', margin: '0 0 12px', fontSize: 13, fontWeight: 600, textTransform: 'uppercase', letterSpacing: 0.5 }}>{title}</h4>
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 12 }}>
      <MiniStat label="Verified" value={data?.verified ?? 0} color="#22c55e" />
      <MiniStat label="Suppressed" value={data?.suppressed ?? 0} color="#ef4444" />
      <MiniStat label="Deduped" value={data?.deduped ?? 0} color="#f59e0b" />
      <MiniStat label="Files" value={data?.files ?? 0} color="#818cf8" />
    </div>
  </div>
);

const MiniStat: React.FC<{ label: string; value: number; color: string }> = ({ label, value, color }) => (
  <div>
    <div style={{ fontSize: 20, fontWeight: 700, color, fontFamily: 'monospace' }}>{value.toLocaleString()}</div>
    <div style={{ fontSize: 11, color: '#64748b' }}>{label}</div>
  </div>
);

const StatusBadge: React.FC<{ status: string }> = ({ status }) => {
  const colors: Record<string, { bg: string; text: string }> = {
    running: { bg: 'rgba(59,130,246,0.15)', text: '#60a5fa' },
    completed: { bg: 'rgba(34,197,94,0.15)', text: '#22c55e' },
    failed: { bg: 'rgba(239,68,68,0.15)', text: '#ef4444' },
  };
  const c = colors[status] ?? { bg: 'rgba(100,116,139,0.15)', text: '#94a3b8' };
  return (
    <span style={{ background: c.bg, color: c.text, padding: '3px 10px', borderRadius: 6, fontSize: 11, fontWeight: 600 }}>
      {status === 'running' && <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 4 }} />}
      {status === 'failed' && <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 4 }} />}
      {status}
    </span>
  );
};

function ispColor(isp: string): string {
  const colors: Record<string, string> = {
    gmail: '#ea4335',
    yahoo: '#6001d2',
    microsoft: '#0078d4',
    apple: '#555',
    comcast: '#e00',
    charter: '#0066cc',
    att: '#009fdb',
    cox: '#f60',
  };
  return colors[isp] ?? '#64748b';
}

function formatDuration(start: string, end: string): string {
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (ms < 60000) return `${Math.round(ms / 1000)}s`;
  if (ms < 3600000) return `${Math.round(ms / 60000)}m`;
  return `${(ms / 3600000).toFixed(1)}h`;
}

const thStyle: React.CSSProperties = {
  textAlign: 'left',
  padding: '10px 12px',
  color: '#94a3b8',
  fontWeight: 600,
  fontSize: 11,
  textTransform: 'uppercase',
  letterSpacing: 0.5,
};

const tdStyle: React.CSSProperties = {
  padding: '10px 12px',
  color: '#e2e8f0',
};
