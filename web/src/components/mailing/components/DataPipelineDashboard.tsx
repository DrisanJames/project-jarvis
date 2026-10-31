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
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '50vh', color: 'var(--j-text-muted)', gap: 10, fontSize: 14 }}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading list import data...
      </div>
    );
  }

  const s = stats;

  return (
    <div className="manager-page" style={{ padding: '24px 32px', maxWidth: 1400 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 28 }}>
        <div>
          <h2 style={{ color: 'var(--j-text)', margin: 0, fontSize: 22, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faDatabase} style={{ marginRight: 10, color: 'var(--j-violet)' }} />
            List Import
          </h2>
          <p style={{ color: 'var(--j-text-2)', margin: '6px 0 0', fontSize: 13 }}>
            Automated list import, email verification & list replenishment
          </p>
        </div>
        <button
          onClick={handleTrigger}
          disabled={triggering}
          style={{
            background: 'linear-gradient(135deg, var(--j-violet-strong), var(--j-violet-strong))',
            color: 'var(--j-text)',
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
        <StatCard icon={faCheckCircle} label="Total Verified" value={s?.total_verified ?? 0} color="var(--j-success)" sub="Added to lists" />
        <StatCard icon={faBan} label="Total Suppressed" value={s?.total_suppressed ?? 0} color="var(--j-hard)" sub="Sent to global suppression" />
        <StatCard icon={faCopy} label="Total Deduped" value={s?.total_deduped ?? 0} color="var(--j-soft)" sub="Already existed" />
        <StatCard icon={faFolderOpen} label="Files Available" value={s?.files_available ?? 0} color="var(--j-violet)" sub={`${s?.files_processed ?? 0} processed`} />
        <StatCard icon={faSync} label="Total Runs" value={s?.total_runs ?? 0} color="var(--j-cyan)" sub={s?.last_run_at ? `Last: ${new Date(s.last_run_at).toLocaleDateString()}` : 'Never run'} />
      </div>

      {/* Today vs Yesterday */}
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 28 }}>
        <DayCard title="Today" data={s?.today} />
        <DayCard title="Yesterday" data={s?.yesterday} />
      </div>

      {/* Chart */}
      {chartData.length > 0 && (
        <div style={{ background: 'color-mix(in srgb, var(--j-shade) 60%, transparent)', borderRadius: 12, border: '1px solid color-mix(in srgb, var(--j-text-muted) 20%, transparent)', padding: 20, marginBottom: 28 }}>
          <h3 style={{ color: 'var(--j-text)', margin: '0 0 16px', fontSize: 15, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faChartBar} style={{ marginRight: 8, color: 'var(--j-violet)' }} />
            30-Day List Import Activity
          </h3>
          <ResponsiveContainer width="100%" height={280}>
            <BarChart data={chartData}>
              <CartesianGrid strokeDasharray="3 3" stroke="color-mix(in srgb, var(--j-text-muted) 15%, transparent)" />
              <XAxis dataKey="date" tick={{ fill: 'var(--j-text-muted)', fontSize: 11 }} tickFormatter={d => d.slice(5)} />
              <YAxis tick={{ fill: 'var(--j-text-muted)', fontSize: 11 }} />
              <RechartsTooltip
                contentStyle={{ background: 'var(--j-surface-2)', border: '1px solid color-mix(in srgb, var(--j-text-muted) 30%, transparent)', borderRadius: 8, fontSize: 12 }}
                labelStyle={{ color: 'var(--j-text)' }}
              />
              <Legend wrapperStyle={{ fontSize: 12 }} />
              <Bar dataKey="verified" name="Verified" fill="var(--j-success)" radius={[3, 3, 0, 0]} />
              <Bar dataKey="suppressed" name="Suppressed" fill="var(--j-hard-fill)" radius={[3, 3, 0, 0]} />
              <Bar dataKey="deduped" name="Deduped" fill="var(--j-soft-fill)" radius={[3, 3, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        </div>
      )}

      {/* Domain Health Table */}
      {health.length > 0 && (
        <div style={{ background: 'color-mix(in srgb, var(--j-shade) 60%, transparent)', borderRadius: 12, border: '1px solid color-mix(in srgb, var(--j-text-muted) 20%, transparent)', padding: 20, marginBottom: 28 }}>
          <h3 style={{ color: 'var(--j-text)', margin: '0 0 16px', fontSize: 15, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faServer} style={{ marginRight: 8, color: 'var(--j-cyan)' }} />
            Domain / Mailbox Provider Health
          </h3>
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
              <thead>
                <tr style={{ borderBottom: '1px solid color-mix(in srgb, var(--j-text-muted) 20%, transparent)' }}>
                  <th style={thStyle}>Sending Domain</th>
                  <th style={thStyle}>Mailbox Provider</th>
                  <th style={thStyle}>List</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Subscribers</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Files Available</th>
                </tr>
              </thead>
              <tbody>
                {health.map((h, i) => (
                  <tr key={i} style={{ borderBottom: '1px solid color-mix(in srgb, var(--j-text-muted) 10%, transparent)' }}>
                    <td style={tdStyle}>{h.sending_domain}</td>
                    <td style={tdStyle}>
                      <span style={{ background: ispColor(h.isp), color: 'var(--j-on-accent)', padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 600 }}>
                        {h.isp.toUpperCase()}
                      </span>
                    </td>
                    <td style={{ ...tdStyle, color: 'var(--j-text-muted)' }}>{h.list_name}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontWeight: 600, fontFamily: 'monospace' }}>{h.subscriber_count.toLocaleString()}</td>
                    <td style={{ ...tdStyle, textAlign: 'right' }}>
                      <span style={{ color: h.files_available > 0 ? 'var(--j-success)' : 'var(--j-hard)', fontWeight: 600, fontFamily: 'monospace' }}>
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
      <div style={{ background: 'color-mix(in srgb, var(--j-shade) 60%, transparent)', borderRadius: 12, border: '1px solid color-mix(in srgb, var(--j-text-muted) 20%, transparent)', padding: 20 }}>
        <h3 style={{ color: 'var(--j-text)', margin: '0 0 16px', fontSize: 15, fontWeight: 600 }}>
          <FontAwesomeIcon icon={faSync} style={{ marginRight: 8, color: 'var(--j-violet)' }} />
          Run History
        </h3>
        {runs.length === 0 ? (
          <p style={{ color: 'var(--j-text-faint)', fontSize: 13 }}>No import runs yet.</p>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
              <thead>
                <tr style={{ borderBottom: '1px solid color-mix(in srgb, var(--j-text-muted) 20%, transparent)' }}>
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
                  <tr key={r.id} style={{ borderBottom: '1px solid color-mix(in srgb, var(--j-text-muted) 10%, transparent)' }}>
                    <td style={tdStyle}>{new Date(r.started_at).toLocaleString()}</td>
                    <td style={tdStyle}>
                      <StatusBadge status={r.status} />
                    </td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontFamily: 'monospace' }}>{r.files_processed}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontFamily: 'monospace', color: 'var(--j-success)' }}>{r.emails_verified.toLocaleString()}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontFamily: 'monospace', color: 'var(--j-hard)' }}>{r.emails_suppressed.toLocaleString()}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', fontFamily: 'monospace', color: 'var(--j-soft)' }}>{r.emails_deduped.toLocaleString()}</td>
                    <td style={{ ...tdStyle, fontFamily: 'monospace', color: 'var(--j-text-muted)' }}>
                      {r.completed_at ? formatDuration(r.started_at, r.completed_at) : '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div style={{ textAlign: 'right', marginTop: 16, color: 'var(--j-text-faint)', fontSize: 11 }}>
        List Import Dashboard v{PAGE_VERSION}
      </div>
    </div>
  );
};

const StatCard: React.FC<{ icon: any; label: string; value: number; color: string; sub: string }> = ({ icon, label, value, color, sub }) => (
  <div style={{
    background: 'color-mix(in srgb, var(--j-shade) 60%, transparent)',
    borderRadius: 12,
    border: '1px solid color-mix(in srgb, var(--j-text-muted) 20%, transparent)',
    padding: '18px 20px',
  }}>
    <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 8 }}>
      <FontAwesomeIcon icon={icon} style={{ color, fontSize: 16 }} />
      <span style={{ color: 'var(--j-text-muted)', fontSize: 12, fontWeight: 500 }}>{label}</span>
    </div>
    <div style={{ fontSize: 26, fontWeight: 700, color: 'var(--j-text)', fontFamily: 'monospace' }}>
      {value.toLocaleString()}
    </div>
    <div style={{ color: 'var(--j-text-faint)', fontSize: 11, marginTop: 4 }}>{sub}</div>
  </div>
);

const DayCard: React.FC<{ title: string; data?: { verified: number; suppressed: number; deduped: number; files: number } }> = ({ title, data }) => (
  <div style={{
    background: 'color-mix(in srgb, var(--j-shade) 60%, transparent)',
    borderRadius: 12,
    border: '1px solid color-mix(in srgb, var(--j-text-muted) 20%, transparent)',
    padding: '18px 20px',
  }}>
    <h4 style={{ color: 'var(--j-text-muted)', margin: '0 0 12px', fontSize: 13, fontWeight: 600, textTransform: 'uppercase', letterSpacing: 0.5 }}>{title}</h4>
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 12 }}>
      <MiniStat label="Verified" value={data?.verified ?? 0} color="var(--j-success)" />
      <MiniStat label="Suppressed" value={data?.suppressed ?? 0} color="var(--j-hard)" />
      <MiniStat label="Deduped" value={data?.deduped ?? 0} color="var(--j-soft)" />
      <MiniStat label="Files" value={data?.files ?? 0} color="var(--j-violet)" />
    </div>
  </div>
);

const MiniStat: React.FC<{ label: string; value: number; color: string }> = ({ label, value, color }) => (
  <div>
    <div style={{ fontSize: 20, fontWeight: 700, color, fontFamily: 'monospace' }}>{value.toLocaleString()}</div>
    <div style={{ fontSize: 11, color: 'var(--j-text-faint)' }}>{label}</div>
  </div>
);

const StatusBadge: React.FC<{ status: string }> = ({ status }) => {
  const colors: Record<string, { bg: string; text: string }> = {
    running: { bg: 'color-mix(in srgb, var(--j-blue) 15%, transparent)', text: 'var(--j-blue)' },
    completed: { bg: 'color-mix(in srgb, var(--j-success) 15%, transparent)', text: 'var(--j-success)' },
    failed: { bg: 'rgba(239,68,68,0.15)', text: 'var(--j-hard)' },
  };
  const c = colors[status] ?? { bg: 'color-mix(in srgb, var(--j-text-muted) 15%, transparent)', text: 'var(--j-text-muted)' };
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
    apple: 'var(--j-border-strong)',
    comcast: '#e00',
    charter: '#0066cc',
    att: '#009fdb',
    cox: '#f60',
  };
  return colors[isp] ?? 'var(--j-text-faint)';
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
  color: 'var(--j-text-muted)',
  fontWeight: 600,
  fontSize: 11,
  textTransform: 'uppercase',
  letterSpacing: 0.5,
};

const tdStyle: React.CSSProperties = {
  padding: '10px 12px',
  color: 'var(--j-text)',
};
