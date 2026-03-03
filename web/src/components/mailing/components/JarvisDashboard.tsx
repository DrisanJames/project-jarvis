import React, { useState, useEffect, useCallback } from 'react';

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
  total_bounces: number;
  open_rate: number;
  click_rate: number;
  conversion_rate: number;
  total_revenue?: number;
  revenue_per_send?: number;
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
  milestone: '#fdcb6e',
  action: '#00e5ff',
  decision: '#00b0ff',
  info: 'rgba(180,210,240,0.65)',
  warning: '#f97316',
  error: '#e94560',
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
  pending: 'rgba(180,210,240,0.65)',
  sent: '#00e5ff',
  delivered: '#00b894',
  opened: '#fdcb6e',
  clicked: '#00b0ff',
  converted: '#00b894',
  bounced: '#e94560',
  failed: '#e94560',
  suppressed: 'rgba(180,210,240,0.65)',
};

export const JarvisDashboard: React.FC = () => {
  const [campaign, setCampaign] = useState<JarvisCampaign | null>(null);
  const [idle, setIdle] = useState(false);
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [logFilter, setLogFilter] = useState<string>('all');

  const fetchStatus = useCallback(async () => {
    try {
      const res = await fetch(`${API}/status`);
      const data = await res.json();
      if (data.status === 'idle') {
        setIdle(true);
        setCampaign(null);
      } else {
        setIdle(false);
        setCampaign(data);
      }
    } catch (e) {
      console.error('Failed to fetch Jarvis status:', e);
    }
  }, []);

  useEffect(() => {
    fetchStatus();
    if (!autoRefresh) return;
    const interval = setInterval(fetchStatus, 5000);
    return () => clearInterval(interval);
  }, [fetchStatus, autoRefresh]);

  const handleAction = async (action: string) => {
    await fetch(`${API}/${action}`, { method: 'POST' });
    fetchStatus();
  };

  if (idle) {
    return (
      <div style={styles.container}>
        <div style={styles.header}>
          <h2 style={styles.title}>🤖 JARVIS — Autonomous Campaign Orchestrator</h2>
          <p style={styles.subtitle}>No campaign is currently running.</p>
        </div>
        <div style={styles.idleCard}>
          <p style={{ fontSize: 48, margin: 0 }}>🔋</p>
          <h3 style={{ color: '#e0e6f0', margin: '10px 0' }}>Jarvis is Standing By</h3>
          <p style={{ color: 'rgba(180,210,240,0.65)', fontSize: 14 }}>
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

  const filteredLogs = logFilter === 'all'
    ? campaign.log
    : campaign.log.filter(l => l.level === logFilter);

  return (
    <div style={styles.container}>
      {/* Header */}
      <div style={styles.header}>
        <div>
          <h2 style={styles.title}>🤖 JARVIS — Autonomous Campaign Orchestrator</h2>
          <p style={styles.subtitle}>
            Campaign <code>{campaign.id.slice(0, 8)}</code> — {campaign.offer_name} (Offer {campaign.offer_id})
          </p>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <span style={{
            ...styles.statusBadge,
            background: campaign.status === 'running' ? '#00b894' : campaign.status === 'completed' ? '#00e5ff' : '#fdcb6e',
          }}>
            {campaign.status === 'running' ? '● LIVE' : campaign.status.toUpperCase()}
          </span>
          <label style={{ display: 'flex', alignItems: 'center', gap: 4, color: 'rgba(180,210,240,0.65)', fontSize: 12, cursor: 'pointer' }}>
            <input type="checkbox" checked={autoRefresh} onChange={e => setAutoRefresh(e.target.checked)} />
            Auto-refresh
          </label>
        </div>
      </div>

      {/* Metrics Bar */}
      <div style={styles.metricsBar}>
        <MetricCard label="Total Sent" value={campaign.metrics.total_sent} color="#00e5ff" />
        <MetricCard label="Delivered" value={campaign.metrics.total_delivered} sub={campaign.metrics.total_sent > 0 ? `${((campaign.metrics.total_delivered / campaign.metrics.total_sent) * 100).toFixed(1)}%` : '–'} color="#00e5ff" />
        <MetricCard label="Opens" value={campaign.metrics.total_opens} sub={`${campaign.metrics.open_rate.toFixed(1)}%`} color="#fdcb6e" />
        <MetricCard label="Clicks" value={campaign.metrics.total_clicks} sub={`${campaign.metrics.click_rate.toFixed(1)}%`} color="#00b0ff" />
        <MetricCard label="Conversions" value={campaign.metrics.total_conversions} sub={`${campaign.metrics.conversion_rate.toFixed(1)}%`} color="#00b894" />
        {campaign.metrics.total_revenue !== undefined && campaign.metrics.total_revenue > 0 && (
          <MetricCard label="Revenue" value={`$${campaign.metrics.total_revenue.toFixed(2)}`} sub={`$${(campaign.metrics.revenue_per_send || 0).toFixed(4)}/send`} color="#00b894" />
        )}
        <MetricCard label="Elapsed" value={`${elapsed}m`} sub={`${remaining}m left`} color="rgba(180,210,240,0.65)" />
      </div>

      {/* Funnel Visualization */}
      <div style={{ display: 'flex', gap: 0, marginBottom: 16, background: '#0a0f1a', borderRadius: 10, overflow: 'hidden', border: '1px solid #0d1526' }}>
        {[
          { label: 'SENT', val: campaign.metrics.total_sent, color: '#00e5ff' },
          { label: 'DELIVERED', val: campaign.metrics.total_delivered, color: '#00e5ff' },
          { label: 'OPENED', val: campaign.metrics.total_opens, color: '#fdcb6e' },
          { label: 'CLICKED', val: campaign.metrics.total_clicks, color: '#00b0ff' },
          { label: 'CONVERTED', val: campaign.metrics.total_conversions, color: '#00b894' },
        ].map((step, idx, arr) => {
          const prevVal = idx > 0 ? arr[idx - 1].val : step.val;
          const rate = prevVal > 0 ? (step.val / prevVal) * 100 : 0;
          const widthPct = campaign.metrics.total_sent > 0 ? Math.max(8, (step.val / campaign.metrics.total_sent) * 100) : 20;
          return (
            <div key={step.label} style={{ flex: `0 0 ${widthPct}%`, minWidth: 60, padding: '8px 10px', borderRight: '1px solid #0d152622', transition: 'flex 0.5s ease' }}>
              <div style={{ fontSize: 9, fontWeight: 700, letterSpacing: '0.06em', color: step.color, opacity: 0.8 }}>{step.label}</div>
              <div style={{ fontSize: 18, fontWeight: 700, color: '#e0e6f0', fontFamily: 'monospace' }}>{step.val}</div>
              {idx > 0 && (
                <div style={{ fontSize: 9, color: rate >= 50 ? '#00b894' : rate >= 20 ? '#fdcb6e' : '#e94560', fontFamily: 'monospace', fontWeight: 600 }}>
                  {rate.toFixed(1)}% {rate >= 50 ? '▲' : rate >= 20 ? '●' : '▼'}
                </div>
              )}
            </div>
          );
        })}
        {campaign.metrics.total_bounces > 0 && (
          <div style={{ flex: '0 0 auto', minWidth: 60, padding: '8px 10px', borderLeft: '2px solid #e9456044' }}>
            <div style={{ fontSize: 9, fontWeight: 700, letterSpacing: '0.06em', color: '#e94560', opacity: 0.8 }}>BOUNCED</div>
            <div style={{ fontSize: 18, fontWeight: 700, color: '#e94560', fontFamily: 'monospace' }}>{campaign.metrics.total_bounces}</div>
            <div style={{ fontSize: 9, color: '#e94560', fontFamily: 'monospace', fontWeight: 600 }}>
              {campaign.metrics.total_sent > 0 ? ((campaign.metrics.total_bounces / campaign.metrics.total_sent) * 100).toFixed(1) : 0}%
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
          <button style={{ ...styles.btn, background: '#fdcb6e' }} onClick={() => handleAction('pause')}>⏸ Pause</button>
        )}
        {campaign.status === 'paused' && (
          <button style={{ ...styles.btn, background: '#00b894' }} onClick={() => handleAction('resume')}>▶ Resume</button>
        )}
        <button style={{ ...styles.btn, background: '#e94560' }} onClick={() => handleAction('stop')}>⏹ Stop Campaign</button>
        <button style={{ ...styles.btn, background: '#0d1526' }} onClick={fetchStatus}>🔄 Refresh</button>
      </div>

      {/* Recipients Grid */}
      <div style={styles.section}>
        <h3 style={styles.sectionTitle}>📬 Recipients ({campaign.recipients.length})</h3>
        <div style={styles.recipientsGrid}>
          {campaign.recipients.map(r => (
            <div key={r.email} style={styles.recipientCard}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <span style={{ color: '#e0e6f0', fontWeight: 600, fontSize: 13 }}>{r.email}</span>
                <span style={{
                  ...styles.recipientStatus,
                  background: (statusColors[r.status] || 'rgba(180,210,240,0.65)') + '22',
                  color: statusColors[r.status] || 'rgba(180,210,240,0.65)',
                }}>{r.status.toUpperCase()}</span>
              </div>
              <div style={{ display: 'flex', gap: 16, marginTop: 6, fontSize: 11, color: 'rgba(180,210,240,0.65)' }}>
                <span>ISP: <strong>{r.isp}</strong></span>
                <span>ESP: <strong>{r.esp || '—'}</strong></span>
                <span>Sends: <strong>{r.send_count}</strong></span>
              </div>
              {r.subject && (
                <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', marginTop: 4, fontStyle: 'italic' }}>
                  Subject: &quot;{r.subject}&quot;
                </div>
              )}
            </div>
          ))}
        </div>
      </div>

      {/* Creatives */}
      <div style={styles.section}>
        <h3 style={styles.sectionTitle}>🎨 Creatives ({campaign.creatives.length})</h3>
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
          {campaign.creatives.map(c => (
            <div key={c.id} style={styles.creativeCard}>
              <div style={{ color: '#e0e6f0', fontWeight: 600, fontSize: 12 }}>{c.name}</div>
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', marginTop: 4 }}>
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
                  background: logFilter === f ? 'rgba(0,200,255,0.08)' : 'transparent',
                  color: logFilter === f ? '#e0e6f0' : 'rgba(180,210,240,0.65)',
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
              <span style={{ ...styles.logLevel, color: levelColors[entry.level] || 'rgba(180,210,240,0.65)' }}>
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
    <div style={{ color: 'rgba(180,210,240,0.65)', fontSize: 11, marginTop: 4 }}>{label}</div>
    {sub && <div style={{ color: 'rgba(180,210,240,0.65)', fontSize: 10 }}>{sub}</div>}
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
    color: '#e0e6f0',
    fontSize: 20,
    fontWeight: 700,
    margin: 0,
  },
  subtitle: {
    color: 'rgba(180,210,240,0.65)',
    fontSize: 13,
    margin: '4px 0 0',
  },
  statusBadge: {
    padding: '4px 12px',
    borderRadius: 12,
    fontSize: 11,
    fontWeight: 700,
    color: '#fff',
  },
  metricsBar: {
    display: 'flex',
    gap: 12,
    marginBottom: 16,
  },
  metricCard: {
    flex: 1,
    background: '#0d1526',
    borderRadius: 10,
    padding: '14px 16px',
    textAlign: 'center' as const,
  },
  progressContainer: {
    height: 24,
    background: '#0d1526',
    borderRadius: 12,
    position: 'relative' as const,
    overflow: 'hidden',
    marginBottom: 16,
  },
  progressBar: {
    height: '100%',
    background: 'linear-gradient(90deg, #00e5ff, #00b0ff)',
    borderRadius: 12,
    transition: 'width 0.5s ease',
  },
  progressText: {
    position: 'absolute' as const,
    top: '50%',
    left: '50%',
    transform: 'translate(-50%, -50%)',
    color: '#e0e6f0',
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
    color: '#fff',
    fontSize: 13,
    fontWeight: 600,
    cursor: 'pointer',
  },
  section: {
    marginBottom: 24,
  },
  sectionTitle: {
    color: '#e0e6f0',
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
    background: '#0d1526',
    borderRadius: 10,
    padding: '12px 16px',
    border: '1px solid rgba(0,200,255,0.08)',
  },
  recipientStatus: {
    padding: '2px 8px',
    borderRadius: 8,
    fontSize: 10,
    fontWeight: 700,
  },
  creativeCard: {
    background: '#0d1526',
    borderRadius: 8,
    padding: '10px 14px',
    border: '1px solid rgba(0,200,255,0.08)',
    minWidth: 200,
  },
  logContainer: {
    background: '#0a0f1a',
    borderRadius: 10,
    padding: 12,
    maxHeight: 400,
    overflowY: 'auto' as const,
    border: '1px solid #0d1526',
    fontFamily: '"JetBrains Mono", "Fira Code", monospace',
  },
  logEntry: {
    display: 'flex',
    gap: 8,
    alignItems: 'baseline',
    padding: '3px 0',
    fontSize: 11,
    borderBottom: '1px solid #0d152622',
  },
  logTime: {
    color: 'rgba(180,210,240,0.45)',
    minWidth: 70,
    flexShrink: 0,
  },
  logLevel: {
    minWidth: 80,
    fontWeight: 600,
    flexShrink: 0,
  },
  logComponent: {
    color: 'rgba(180,210,240,0.65)',
    minWidth: 100,
    flexShrink: 0,
  },
  logMessage: {
    color: '#e0e6f0',
    wordBreak: 'break-word' as const,
  },
  filterBtn: {
    padding: '3px 10px',
    borderRadius: 6,
    border: '1px solid rgba(0,200,255,0.08)',
    fontSize: 11,
    cursor: 'pointer',
  },
  idleCard: {
    background: '#0d1526',
    borderRadius: 12,
    padding: 40,
    textAlign: 'center' as const,
    marginTop: 40,
  },
  codeBlock: {
    display: 'inline-block',
    background: '#0a0f1a',
    padding: '8px 16px',
    borderRadius: 8,
    color: '#00b894',
    fontSize: 13,
    marginTop: 12,
    fontFamily: '"JetBrains Mono", monospace',
  },
};
