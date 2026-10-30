import React, { useState, useEffect, useCallback } from 'react';
import { apiFetch } from '../shared/apiFetch';

/* ═══════════════════════════════════════════════════════════════════════════
   JARVIS — Autonomous AI Campaign Orchestrator Dashboard
   Real-time monitoring of the AI-driven sending engine
   ═══════════════════════════════════════════════════════════════════════════ */

const API = '/api/mailing/jarvis';

interface JarvisLogEntry {
  timestamp: string;
  level: string;
  component: string;
  message: string;
  data?: any;
}

interface JarvisRecipient {
  email: string;
  domain: string;
  isp: string;
  suppressed: boolean;
  status: string;
  last_sent_at: string | null;
  last_open_at: string | null;
  last_click_at: string | null;
  send_count: number;
  message_ids: string[];
  esp: string;
  creative_id: number;
  subject: string;
}

interface JarvisCreative {
  id: number;
  name: string;
  subject: string;
  sends: number;
  opens: number;
  clicks: number;
}

interface JarvisMetrics {
  total_sent: number;
  total_delivered: number;
  total_opens: number;
  total_clicks: number;
  total_conversions: number;
  total_hard_bounces: number;
  total_soft_bounces: number;
  open_rate: number;
  click_rate: number;
  conversion_rate: number;
  total_revenue?: number;
  revenue_per_send?: number;
  sent?: number;
  delivered?: number;
  opens?: number;
  clicks?: number;
  hard_bounces?: number;
  soft_bounces?: number;
}

interface JarvisCampaign {
  id: string;
  offer_id: string;
  offer_name: string;
  status: string;
  started_at: string | null;
  ends_at: string | null;
  recipients: JarvisRecipient[];
  creatives: JarvisCreative[];
  tracking_link: string;
  suppression_list_id: string;
  log: JarvisLogEntry[];
  metrics: JarvisMetrics;
  current_round: number;
  max_rounds: number;
  goal_conversions: number;
}

const levelColors: Record<string, string> = {
  milestone: 'var(--j-warn)',
  action: 'var(--j-cyan)',
  decision: 'var(--j-blue)',
  info: 'color-mix(in srgb, var(--j-text) 65%, transparent)',
  warning: 'var(--j-warn)',
  error: 'var(--j-danger)',
};

const levelIcons: Record<string, string> = {
  milestone: '⭐',
  action: '⚡',
  decision: '🧠',
  info: 'ℹ️',
  warning: '⚠️',
  error: '❌',
};

const statusColors: Record<string, string> = {
  pending: 'color-mix(in srgb, var(--j-text) 65%, transparent)',
  sent: 'var(--j-cyan)',
  delivered: 'var(--j-success)',
  opened: 'var(--j-warn)',
  clicked: 'var(--j-blue)',
  converted: 'var(--j-success)',
  bounced: 'var(--j-danger)',
  failed: 'var(--j-danger)',
  suppressed: 'color-mix(in srgb, var(--j-text) 65%, transparent)',
};

export const JarvisDashboard: React.FC = () => {
  const [campaign, setCampaign] = useState<JarvisCampaign | null>(null);
  const [idle, setIdle] = useState(false);
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [logFilter, setLogFilter] = useState<string>('all');

  const fetchStatus = useCallback(async () => {
    try {
      const res = await apiFetch(`${API}/status`);
      if (!res.ok) {
        setIdle(true);
        setCampaign(null);
        return;
      }
      const data = await res.json();
      if (data.status === 'idle' || !data.recipients) {
        setIdle(true);
        setCampaign(null);
      } else {
        setIdle(false);
        setCampaign(data);
      }
    } catch (e) {
      console.error('Failed to fetch Jarvis status:', e);
      setIdle(true);
      setCampaign(null);
    }
  }, []);

  useEffect(() => {
    fetchStatus();
    if (!autoRefresh) return;
    const interval = setInterval(fetchStatus, 5000);
    return () => clearInterval(interval);
  }, [fetchStatus, autoRefresh]);

  const handleAction = async (action: string) => {
    await apiFetch(`${API}/${action}`, { method: 'POST' });
    fetchStatus();
  };

  if (idle) {
    return (
      <div style={styles.container}>
        <div style={styles.header}>
          <h2 style={styles.title}>🤖 Jarvis — Your Campaign Assistant</h2>
          <p style={styles.subtitle}>No campaign is currently running.</p>
        </div>
        <div style={styles.idleCard}>
          <p style={{ fontSize: 48, margin: 0 }}>🔋</p>
          <h3 style={{ color: 'var(--j-text)', margin: '10px 0' }}>Jarvis is Standing By</h3>
          <p style={{ color: 'var(--j-text-muted)', fontSize: 14 }}>
            Launch an autonomous campaign via the API to start AI-driven sending.
          </p>
          <code style={styles.codeBlock}>
            POST /api/mailing/jarvis/launch
          </code>
        </div>
      </div>
    );
  }

  if (!campaign) return <div style={styles.container}>Loading...</div>;

  const elapsed = campaign.started_at
    ? Math.round((Date.now() - new Date(campaign.started_at).getTime()) / 60000)
    : 0;
  const remaining = campaign.ends_at
    ? Math.max(0, Math.round((new Date(campaign.ends_at).getTime() - Date.now()) / 60000))
    : 0;
  const progress = campaign.max_rounds > 0
    ? Math.round((campaign.current_round / campaign.max_rounds) * 100)
    : 0;

  const logs = campaign.log || [];
  const filteredLogs = logFilter === 'all'
    ? logs
    : logs.filter(l => l.level === logFilter);

  return (
    <div style={styles.container}>
      {/* Header */}
      <div style={styles.header}>
        <div>
          <h2 style={styles.title}>🤖 Jarvis — Your Campaign Assistant</h2>
          <p style={styles.subtitle}>
            Campaign <code>{campaign.id.slice(0, 8)}</code> — {campaign.offer_name} (Offer {campaign.offer_id})
          </p>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <span style={{
            ...styles.statusBadge,
            background: campaign.status === 'running' ? 'var(--j-success)' : campaign.status === 'completed' ? 'var(--j-cyan)' : 'var(--j-warn)',
          }}>
            {campaign.status === 'running' ? '● LIVE' : campaign.status.toUpperCase()}
          </span>
          <label style={{ display: 'flex', alignItems: 'center', gap: 4, color: 'var(--j-text-muted)', fontSize: 12, cursor: 'pointer' }}>
            <input type="checkbox" checked={autoRefresh} onChange={e => setAutoRefresh(e.target.checked)} />
            Auto-refresh
          </label>
        </div>
      </div>

      {/* Metrics Bar */}
      <div style={styles.metricsBar}>
        <MetricCard label="Total Sent" value={campaign.metrics.total_sent ?? campaign.metrics.sent ?? 0} color="var(--j-cyan)" />
        <MetricCard label="Delivered" value={campaign.metrics.total_delivered ?? campaign.metrics.delivered ?? 0} sub={(campaign.metrics.total_sent ?? campaign.metrics.sent ?? 0) > 0 ? `${(((campaign.metrics.total_delivered ?? campaign.metrics.delivered ?? 0) / (campaign.metrics.total_sent ?? campaign.metrics.sent ?? 1)) * 100).toFixed(1)}%` : '–'} color="var(--j-cyan)" />
        <MetricCard label="Opens" value={campaign.metrics.total_opens ?? campaign.metrics.opens ?? 0} sub={`${(campaign.metrics.open_rate ?? 0).toFixed(1)}%`} color="var(--j-warn)" />
        <MetricCard label="Clicks" value={campaign.metrics.total_clicks ?? campaign.metrics.clicks ?? 0} sub={`${(campaign.metrics.click_rate ?? 0).toFixed(1)}%`} color="var(--j-blue)" />
        <MetricCard label="Conversions" value={campaign.metrics.total_conversions ?? 0} sub={`${(campaign.metrics.conversion_rate ?? 0).toFixed(1)}%`} color="var(--j-success)" />
        {(campaign.metrics.total_revenue ?? 0) > 0 && (
          <MetricCard label="Revenue" value={`$${(campaign.metrics.total_revenue ?? 0).toFixed(2)}`} sub={`$${(campaign.metrics.revenue_per_send ?? 0).toFixed(4)}/send`} color="var(--j-success)" />
        )}
        <MetricCard label="Elapsed" value={`${elapsed}m`} sub={`${remaining}m left`} color="color-mix(in srgb, var(--j-text) 65%, transparent)" />
      </div>

      {/* Funnel Visualization */}
      <div style={{ display: 'flex', gap: 0, marginBottom: 16, background: 'var(--j-bg)', borderRadius: 10, overflow: 'hidden', border: '1px solid var(--j-surface)' }}>
        {[
          { label: 'SENT', val: campaign.metrics.total_sent ?? campaign.metrics.sent ?? 0, color: 'var(--j-cyan)' },
          { label: 'DELIVERED', val: campaign.metrics.total_delivered ?? campaign.metrics.delivered ?? 0, color: 'var(--j-cyan)' },
          { label: 'OPENED', val: campaign.metrics.total_opens ?? campaign.metrics.opens ?? 0, color: 'var(--j-warn)' },
          { label: 'CLICKED', val: campaign.metrics.total_clicks ?? campaign.metrics.clicks ?? 0, color: 'var(--j-blue)' },
          { label: 'CONVERTED', val: campaign.metrics.total_conversions ?? 0, color: 'var(--j-success)' },
        ].map((step, idx, arr) => {
          const prevVal = idx > 0 ? arr[idx - 1].val : step.val;
          const rate = prevVal > 0 ? (step.val / prevVal) * 100 : 0;
          const totalSent = campaign.metrics.total_sent ?? campaign.metrics.sent ?? 0;
          const widthPct = totalSent > 0 ? Math.max(8, (step.val / totalSent) * 100) : 20;
          return (
            <div key={step.label} style={{ flex: `0 0 ${widthPct}%`, minWidth: 60, padding: '8px 10px', borderRight: '1px solid color-mix(in srgb, var(--j-surface) 13.3%, transparent)', transition: 'flex 0.5s ease' }}>
              <div style={{ fontSize: 9, fontWeight: 700, letterSpacing: '0.06em', color: step.color, opacity: 0.8 }}>{step.label}</div>
              <div style={{ fontSize: 18, fontWeight: 700, color: 'var(--j-text)', fontFamily: 'monospace' }}>{step.val}</div>
              {idx > 0 && (
                <div style={{ fontSize: 9, color: rate >= 50 ? 'var(--j-success)' : rate >= 20 ? 'var(--j-warn)' : 'var(--j-danger)', fontFamily: 'monospace', fontWeight: 600 }}>
                  {rate.toFixed(1)}% {rate >= 50 ? '▲' : rate >= 20 ? '●' : '▼'}
                </div>
              )}
            </div>
          );
        })}
        {((campaign.metrics.total_hard_bounces ?? campaign.metrics.hard_bounces ?? 0) + (campaign.metrics.total_soft_bounces ?? campaign.metrics.soft_bounces ?? 0)) > 0 && (
          <div style={{ flex: '0 0 auto', minWidth: 80, padding: '8px 10px', borderLeft: '2px solid color-mix(in srgb, var(--j-danger) 26.7%, transparent)' }}>
            <div style={{ fontSize: 9, fontWeight: 700, letterSpacing: '0.06em', color: 'var(--j-danger)', opacity: 0.8 }}>BOUNCED</div>
            <div style={{ fontSize: 18, fontWeight: 700, fontFamily: 'monospace' }}>
              <span style={{ color: 'var(--j-hard)' }}>{campaign.metrics.total_hard_bounces ?? campaign.metrics.hard_bounces ?? 0}</span> hard / <span style={{ color: 'var(--j-soft)' }}>{campaign.metrics.total_soft_bounces ?? campaign.metrics.soft_bounces ?? 0}</span> soft
            </div>
            <div style={{ fontSize: 9, fontFamily: 'monospace', fontWeight: 600 }}>
              <span style={{ color: 'var(--j-hard)' }}>{(campaign.metrics.total_sent ?? campaign.metrics.sent ?? 0) > 0 ? (((campaign.metrics.total_hard_bounces ?? campaign.metrics.hard_bounces ?? 0) / (campaign.metrics.total_sent ?? campaign.metrics.sent ?? 1)) * 100).toFixed(1) : 0}%</span>
              <span style={{ color: 'var(--j-text-muted)', margin: '0 4px' }}>/</span>
              <span style={{ color: 'var(--j-soft)' }}>{(campaign.metrics.total_sent ?? campaign.metrics.sent ?? 0) > 0 ? (((campaign.metrics.total_soft_bounces ?? campaign.metrics.soft_bounces ?? 0) / (campaign.metrics.total_sent ?? campaign.metrics.sent ?? 1)) * 100).toFixed(1) : 0}%</span>
            </div>
          </div>
        )}
      </div>

      {/* Progress Bar */}
      <div style={styles.progressContainer}>
        <div style={{ ...styles.progressBar, width: `${progress}%` }} />
        <span style={styles.progressText}>{progress}% — Round {campaign.current_round} of {campaign.max_rounds} | Goal: {campaign.metrics.total_conversions}/{campaign.goal_conversions} conversions</span>
      </div>

      {/* Controls */}
      <div style={styles.controls}>
        {campaign.status === 'running' && (
          <button style={{ ...styles.btn, background: 'var(--j-warn)' }} onClick={() => handleAction('pause')}>⏸ Pause</button>
        )}
        {campaign.status === 'paused' && (
          <button style={{ ...styles.btn, background: 'var(--j-success)' }} onClick={() => handleAction('resume')}>▶ Resume</button>
        )}
        <button style={{ ...styles.btn, background: 'var(--j-danger)' }} onClick={() => handleAction('stop')}>⏹ Stop Campaign</button>
        <button style={{ ...styles.btn, background: 'var(--j-surface)' }} onClick={fetchStatus}>🔄 Refresh</button>
      </div>

      {/* Recipients Grid */}
      <div style={styles.section}>
        <h3 style={styles.sectionTitle}>📬 Recipients ({(campaign.recipients || []).length})</h3>
        <div style={styles.recipientsGrid}>
          {(campaign.recipients || []).map(r => (
            <div key={r.email} style={styles.recipientCard}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <span style={{ color: 'var(--j-text)', fontWeight: 600, fontSize: 13 }}>{r.email}</span>
                <span style={{
                  ...styles.recipientStatus,
                  background: (statusColors[r.status] || 'color-mix(in srgb, var(--j-text) 65%, transparent)') + '22',
                  color: statusColors[r.status] || 'var(--j-text-muted)',
                }}>{r.status.toUpperCase()}</span>
              </div>
              <div style={{ display: 'flex', gap: 16, marginTop: 6, fontSize: 11, color: 'var(--j-text-muted)' }}>
                <span>ISP: <strong>{r.isp}</strong></span>
                <span>ESP: <strong>{r.esp || '—'}</strong></span>
                <span>Sends: <strong>{r.send_count}</strong></span>
              </div>
              {r.subject && (
                <div style={{ fontSize: 11, color: 'var(--j-text-muted)', marginTop: 4, fontStyle: 'italic' }}>
                  Subject: &quot;{r.subject}&quot;
                </div>
              )}
            </div>
          ))}
        </div>
      </div>

      {/* Creatives */}
      <div style={styles.section}>
        <h3 style={styles.sectionTitle}>🎨 Creatives ({(campaign.creatives || []).length})</h3>
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
          {(campaign.creatives || []).map(c => (
            <div key={c.id} style={styles.creativeCard}>
              <div style={{ color: 'var(--j-text)', fontWeight: 600, fontSize: 12 }}>{c.name}</div>
              <div style={{ fontSize: 11, color: 'var(--j-text-muted)', marginTop: 4 }}>
                ID: {c.id} | Sends: {c.sends} | Opens: {c.opens} | Clicks: {c.clicks}
              </div>
            </div>
          ))}
        </div>
      </div>

      {/* Log Stream */}
      <div style={styles.section}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
          <h3 style={styles.sectionTitle}>📋 Activity Log ({filteredLogs.length})</h3>
          <div style={{ display: 'flex', gap: 4 }}>
            {['all', 'milestone', 'action', 'decision', 'error'].map(f => (
              <button
                key={f}
                style={{
                  ...styles.filterBtn,
                  background: logFilter === f ? 'color-mix(in srgb, var(--j-cyan) 8%, transparent)' : 'transparent',
                  color: logFilter === f ? 'var(--j-text)' : 'var(--j-text-muted)',
                }}
                onClick={() => setLogFilter(f)}
              >
                {f}
              </button>
            ))}
          </div>
        </div>
        <div style={styles.logContainer}>
          {[...filteredLogs].reverse().map((entry, idx) => (
            <div key={idx} style={styles.logEntry}>
              <span style={styles.logTime}>
                {new Date(entry.timestamp).toLocaleTimeString()}
              </span>
              <span style={{ ...styles.logLevel, color: levelColors[entry.level] || 'var(--j-text-muted)' }}>
                {levelIcons[entry.level] || '●'} {entry.level}
              </span>
              <span style={styles.logComponent}>[{entry.component}]</span>
              <span style={styles.logMessage}>{entry.message}</span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
};

/* ── Metric Card ──────────────────────────────────────────────────────── */
const MetricCard: React.FC<{
  label: string;
  value: string | number;
  sub?: string;
  color: string;
}> = ({ label, value, sub, color }) => (
  <div style={styles.metricCard}>
    <div style={{ color, fontSize: 28, fontWeight: 700, lineHeight: 1 }}>{value}</div>
    <div style={{ color: 'var(--j-text-muted)', fontSize: 11, marginTop: 4 }}>{label}</div>
    {sub && <div style={{ color: 'var(--j-text-muted)', fontSize: 10 }}>{sub}</div>}
  </div>
);

/* ── Styles ───────────────────────────────────────────────────────────── */
const styles: Record<string, React.CSSProperties> = {
  container: {
    padding: 24,
    maxWidth: 1200,
    margin: '0 auto',
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
  },
  header: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'flex-start',
    marginBottom: 20,
  },
  title: {
    color: 'var(--j-text)',
    fontSize: 20,
    fontWeight: 700,
    margin: 0,
  },
  subtitle: {
    color: 'var(--j-text-muted)',
    fontSize: 13,
    margin: '4px 0 0',
  },
  statusBadge: {
    padding: '4px 12px',
    borderRadius: 12,
    fontSize: 11,
    fontWeight: 700,
    color: 'var(--j-text)',
  },
  metricsBar: {
    display: 'flex',
    gap: 12,
    marginBottom: 16,
  },
  metricCard: {
    flex: 1,
    background: 'var(--j-surface)',
    borderRadius: 10,
    padding: '14px 16px',
    textAlign: 'center' as const,
  },
  progressContainer: {
    height: 24,
    background: 'var(--j-surface)',
    borderRadius: 12,
    position: 'relative' as const,
    overflow: 'hidden',
    marginBottom: 16,
  },
  progressBar: {
    height: '100%',
    background: 'linear-gradient(90deg, var(--j-cyan), var(--j-blue))',
    borderRadius: 12,
    transition: 'width 0.5s ease',
  },
  progressText: {
    position: 'absolute' as const,
    top: '50%',
    left: '50%',
    transform: 'translate(-50%, -50%)',
    color: 'var(--j-text)',
    fontSize: 11,
    fontWeight: 600,
  },
  controls: {
    display: 'flex',
    gap: 8,
    marginBottom: 20,
  },
  btn: {
    padding: '8px 16px',
    borderRadius: 8,
    border: 'none',
    color: 'var(--j-text)',
    fontSize: 13,
    fontWeight: 600,
    cursor: 'pointer',
  },
  section: {
    marginBottom: 24,
  },
  sectionTitle: {
    color: 'var(--j-text)',
    fontSize: 15,
    fontWeight: 600,
    margin: '0 0 12px',
  },
  recipientsGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fill, minmax(350px, 1fr))',
    gap: 10,
  },
  recipientCard: {
    background: 'var(--j-surface)',
    borderRadius: 10,
    padding: '12px 16px',
    border: '1px solid color-mix(in srgb, var(--j-cyan) 8%, transparent)',
  },
  recipientStatus: {
    padding: '2px 8px',
    borderRadius: 8,
    fontSize: 10,
    fontWeight: 700,
  },
  creativeCard: {
    background: 'var(--j-surface)',
    borderRadius: 8,
    padding: '10px 14px',
    border: '1px solid color-mix(in srgb, var(--j-cyan) 8%, transparent)',
    minWidth: 200,
  },
  logContainer: {
    background: 'var(--j-bg)',
    borderRadius: 10,
    padding: 12,
    maxHeight: 400,
    overflowY: 'auto' as const,
    border: '1px solid var(--j-surface)',
    fontFamily: '"JetBrains Mono", "Fira Code", monospace',
  },
  logEntry: {
    display: 'flex',
    gap: 8,
    alignItems: 'baseline',
    padding: '3px 0',
    fontSize: 11,
    borderBottom: '1px solid color-mix(in srgb, var(--j-surface) 13.3%, transparent)',
  },
  logTime: {
    color: 'var(--j-text-muted)',
    minWidth: 70,
    flexShrink: 0,
  },
  logLevel: {
    minWidth: 80,
    fontWeight: 600,
    flexShrink: 0,
  },
  logComponent: {
    color: 'var(--j-text-muted)',
    minWidth: 100,
    flexShrink: 0,
  },
  logMessage: {
    color: 'var(--j-text)',
    wordBreak: 'break-word' as const,
  },
  filterBtn: {
    padding: '3px 10px',
    borderRadius: 6,
    border: '1px solid color-mix(in srgb, var(--j-cyan) 8%, transparent)',
    fontSize: 11,
    cursor: 'pointer',
  },
  idleCard: {
    background: 'var(--j-surface)',
    borderRadius: 12,
    padding: 40,
    textAlign: 'center' as const,
    marginTop: 40,
  },
  codeBlock: {
    display: 'inline-block',
    background: 'var(--j-bg)',
    padding: '8px 16px',
    borderRadius: 8,
    color: 'var(--j-success)',
    fontSize: 13,
    marginTop: 12,
    fontFamily: '"JetBrains Mono", monospace',
  },
};
