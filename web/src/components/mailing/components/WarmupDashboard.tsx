import React, { useState, useEffect, useCallback } from 'react';

const PAGE_VERSION = '1.0';

interface ISPMetrics {
  sent: number;
  delivered: number;
  bounced: number;
  deferred: number;
}

interface TrendEntry {
  date: string;
  sent: number;
  delivered: number;
  bounced: number;
  bounce_rate: number;
}

interface IPData {
  id: string;
  ip: string;
  hostname: string;
  pool: string;
  status: string;
  warmup_day: number;
  warmup_daily_limit: number;
  today_sent: number;
  total_sent: number;
  total_delivered: number;
  total_bounced: number;
  reputation_score: number;
  today_bounce_rate: number;
  today_complaint_rate: number;
  progress_pct: number;
  warmup_started_at?: string;
  isp_breakdown: Record<string, ISPMetrics>;
  '7d_trend': TrendEntry[];
}

interface PoolSummary {
  pool: string;
  active_ips: number;
  total_sent_today: number;
  avg_bounce_rate: number;
}

interface DashboardData {
  ips: IPData[];
  pool_summary: PoolSummary[];
  total: number;
  api_version: string;
}

const ISP_COLORS: Record<string, string> = {
  gmail: '#ea4335',
  yahoo: '#6001d2',
  microsoft: '#00a4ef',
  apple: '#a2aaad',
  comcast: '#ed1c24',
  att: '#00a8e0',
  cox: '#0072ce',
  charter: '#0070c0',
  other: '#6b7280',
};

const KNOWN_ISPS = ['gmail', 'yahoo', 'microsoft', 'apple', 'comcast', 'att', 'cox', 'charter'];

export const WarmupDashboard: React.FC = () => {
  const [data, setData] = useState<DashboardData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [selectedPool, setSelectedPool] = useState<string>('all');
  const [autoRefresh, setAutoRefresh] = useState(true);

  const fetchData = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/warmup/dashboard');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json = await res.json();
      setData(json);
      setError('');
    } catch (e: any) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchData();
    if (!autoRefresh) return;
    const interval = setInterval(fetchData, 30000);
    return () => clearInterval(interval);
  }, [fetchData, autoRefresh]);

  if (loading) return <div style={styles.loading}>Loading warmup data...</div>;
  if (error) return <div style={styles.error}>Error: {error}</div>;
  if (!data) return null;

  const filteredIPs = selectedPool === 'all'
    ? data.ips
    : data.ips.filter(ip => ip.pool === selectedPool);

  const pools = Array.from(new Set(data.ips.map(ip => ip.pool).filter(Boolean)));

  return (
    <div style={styles.container}>
      <div style={styles.header}>
        <h2 style={styles.title}>IP Warmup Dashboard</h2>
        <div style={styles.controls}>
          <select
            value={selectedPool}
            onChange={e => setSelectedPool(e.target.value)}
            style={styles.select}
          >
            <option value="all">All Pools ({data.total} IPs)</option>
            {pools.map(p => (
              <option key={p} value={p}>{p}</option>
            ))}
          </select>
          <label style={styles.toggleLabel}>
            <input
              type="checkbox"
              checked={autoRefresh}
              onChange={e => setAutoRefresh(e.target.checked)}
            />
            Auto-refresh (30s)
          </label>
          <button onClick={fetchData} style={styles.refreshBtn}>Refresh</button>
        </div>
      </div>

      {/* Pool Health Cards */}
      <div style={styles.poolGrid}>
        {data.pool_summary.map(pool => (
          <div
            key={pool.pool}
            style={{
              ...styles.poolCard,
              borderLeft: `4px solid ${pool.avg_bounce_rate > 0.02 ? '#ef4444' : pool.avg_bounce_rate > 0.01 ? '#f59e0b' : '#22c55e'}`,
            }}
            onClick={() => setSelectedPool(pool.pool === selectedPool ? 'all' : pool.pool)}
          >
            <div style={styles.poolName}>{pool.pool}</div>
            <div style={styles.poolStats}>
              <span>{pool.active_ips} IPs</span>
              <span>{pool.total_sent_today.toLocaleString()} sent today</span>
              <span style={{ color: pool.avg_bounce_rate > 0.02 ? '#ef4444' : '#94a3b8' }}>
                {(pool.avg_bounce_rate * 100).toFixed(1)}% bounce
              </span>
            </div>
          </div>
        ))}
      </div>

      {/* ISP Delivery Heatmap */}
      <div style={styles.section}>
        <h3 style={styles.sectionTitle}>ISP Delivery Heatmap</h3>
        <div style={styles.heatmapContainer}>
          <table style={styles.heatmapTable}>
            <thead>
              <tr>
                <th style={styles.heatmapHeader}>IP</th>
                {KNOWN_ISPS.map(isp => (
                  <th key={isp} style={{ ...styles.heatmapHeader, color: ISP_COLORS[isp] }}>
                    {isp.charAt(0).toUpperCase() + isp.slice(1)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {filteredIPs.map(ip => (
                <tr key={ip.id}>
                  <td style={styles.heatmapIP} title={ip.hostname}>{ip.ip}</td>
                  {KNOWN_ISPS.map(isp => {
                    const metrics = ip.isp_breakdown?.[isp];
                    if (!metrics || metrics.sent === 0) {
                      return <td key={isp} style={styles.heatmapCell}>—</td>;
                    }
                    const deliveryRate = metrics.delivered / (metrics.delivered + metrics.bounced || 1);
                    const bg = deliveryRate >= 0.95 ? 'rgba(34,197,94,0.25)'
                      : deliveryRate >= 0.8 ? 'rgba(245,158,11,0.25)'
                      : 'rgba(239,68,68,0.25)';
                    return (
                      <td key={isp} style={{ ...styles.heatmapCell, background: bg }}
                        title={`Sent: ${metrics.sent}, Del: ${metrics.delivered}, Bnc: ${metrics.bounced}, Def: ${metrics.deferred}`}
                      >
                        {(deliveryRate * 100).toFixed(0)}%
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {/* IP Detail Table */}
      <div style={styles.section}>
        <h3 style={styles.sectionTitle}>IP Warmup Status</h3>
        <table style={styles.table}>
          <thead>
            <tr>
              <th style={styles.th}>IP</th>
              <th style={styles.th}>Hostname</th>
              <th style={styles.th}>Pool</th>
              <th style={styles.th}>Status</th>
              <th style={styles.th}>Day</th>
              <th style={styles.th}>Today / Limit</th>
              <th style={styles.th}>Total Sent</th>
              <th style={styles.th}>Total Del.</th>
              <th style={styles.th}>Bounce %</th>
              <th style={styles.th}>7d Trend</th>
            </tr>
          </thead>
          <tbody>
            {filteredIPs.map(ip => (
              <tr key={ip.id} style={styles.tr}>
                <td style={styles.td}>{ip.ip}</td>
                <td style={{ ...styles.td, fontSize: 11, maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis' }}
                  title={ip.hostname}>
                  {ip.hostname}
                </td>
                <td style={styles.td}>{ip.pool}</td>
                <td style={styles.td}>
                  <span style={{
                    ...styles.statusBadge,
                    background: ip.status === 'active' ? 'rgba(34,197,94,0.2)' : 'rgba(245,158,11,0.2)',
                    color: ip.status === 'active' ? '#22c55e' : '#f59e0b',
                  }}>
                    {ip.status}
                  </span>
                </td>
                <td style={styles.td}>{ip.warmup_day}</td>
                <td style={styles.td}>
                  <div style={styles.progressContainer}>
                    <div style={{
                      ...styles.progressBar,
                      width: `${Math.min(100, (ip.today_sent / Math.max(ip.warmup_daily_limit, 1)) * 100)}%`,
                      background: ip.today_sent >= ip.warmup_daily_limit ? '#ef4444' : '#6366f1',
                    }} />
                    <span style={styles.progressText}>
                      {ip.today_sent.toLocaleString()} / {ip.warmup_daily_limit.toLocaleString()}
                    </span>
                  </div>
                </td>
                <td style={styles.td}>{ip.total_sent.toLocaleString()}</td>
                <td style={styles.td}>{ip.total_delivered.toLocaleString()}</td>
                <td style={{
                  ...styles.td,
                  color: ip.today_bounce_rate > 0.02 ? '#ef4444' : ip.today_bounce_rate > 0.01 ? '#f59e0b' : '#94a3b8',
                }}>
                  {(ip.today_bounce_rate * 100).toFixed(1)}%
                </td>
                <td style={styles.td}>
                  <MiniSparkline data={ip['7d_trend'] || []} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {/* Warmup Progression Recommendations */}
      <div style={styles.section}>
        <h3 style={styles.sectionTitle}>Warmup Progression</h3>
        <div style={styles.recoGrid}>
          {filteredIPs.filter(ip => ip.status === 'warmup').map(ip => {
            const trend = ip['7d_trend'] || [];
            const recentBounceRates = trend.slice(-3).map(t => t.bounce_rate);
            const allBelow1 = recentBounceRates.length >= 3 && recentBounceRates.every(r => r < 0.01);
            const recommendation = allBelow1
              ? `Safe to increase to ${(ip.warmup_daily_limit * 2).toLocaleString()}/day`
              : recentBounceRates.some(r => r > 0.05)
              ? 'Hold — bounce rate too high'
              : 'Continue at current limit';
            const recoColor = allBelow1 ? '#22c55e' : recentBounceRates.some(r => r > 0.05) ? '#ef4444' : '#94a3b8';

            return (
              <div key={ip.id} style={styles.recoCard}>
                <div style={styles.recoIP}>{ip.ip}</div>
                <div style={{ ...styles.recoText, color: recoColor }}>{recommendation}</div>
                <div style={styles.recoDetail}>
                  Current: {ip.warmup_daily_limit.toLocaleString()}/day &middot; Day {ip.warmup_day}
                </div>
              </div>
            );
          })}
        </div>
      </div>

      <div style={styles.footer}>v{PAGE_VERSION}</div>
    </div>
  );
};

const MiniSparkline: React.FC<{ data: TrendEntry[] }> = ({ data }) => {
  if (!data || data.length === 0) return <span style={{ color: '#475569' }}>—</span>;

  const values = data.map(d => d.sent);
  const max = Math.max(...values, 1);
  const width = 80;
  const height = 20;

  const points = values.map((v, i) => {
    const x = (i / Math.max(values.length - 1, 1)) * width;
    const y = height - (v / max) * height;
    return `${x},${y}`;
  }).join(' ');

  return (
    <svg width={width} height={height} style={{ display: 'block' }}>
      <polyline
        points={points}
        fill="none"
        stroke="#6366f1"
        strokeWidth="1.5"
        strokeLinejoin="round"
      />
    </svg>
  );
};

const styles: Record<string, React.CSSProperties> = {
  container: {
    padding: '24px 32px',
    color: '#e2e8f0',
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace',
  },
  header: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginBottom: 24,
  },
  title: {
    fontSize: 22,
    fontWeight: 700,
    color: '#f1f5f9',
    margin: 0,
  },
  controls: {
    display: 'flex',
    alignItems: 'center',
    gap: 12,
  },
  select: {
    background: '#1e293b',
    color: '#e2e8f0',
    border: '1px solid #334155',
    borderRadius: 6,
    padding: '6px 10px',
    fontSize: 13,
  },
  toggleLabel: {
    fontSize: 12,
    color: '#94a3b8',
    display: 'flex',
    alignItems: 'center',
    gap: 4,
  },
  refreshBtn: {
    background: '#4f46e5',
    color: '#fff',
    border: 'none',
    borderRadius: 6,
    padding: '6px 14px',
    fontSize: 12,
    cursor: 'pointer',
  },
  loading: {
    color: '#94a3b8',
    textAlign: 'center' as const,
    padding: 48,
    fontSize: 14,
  },
  error: {
    color: '#ef4444',
    textAlign: 'center' as const,
    padding: 48,
    fontSize: 14,
  },
  poolGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fill, minmax(260px, 1fr))',
    gap: 12,
    marginBottom: 24,
  },
  poolCard: {
    background: '#0f172a',
    border: '1px solid #1e293b',
    borderRadius: 8,
    padding: '14px 16px',
    cursor: 'pointer',
    transition: 'border-color 0.15s',
  },
  poolName: {
    fontSize: 14,
    fontWeight: 600,
    color: '#f1f5f9',
    marginBottom: 6,
  },
  poolStats: {
    display: 'flex',
    gap: 16,
    fontSize: 12,
    color: '#94a3b8',
  },
  section: {
    marginBottom: 28,
  },
  sectionTitle: {
    fontSize: 16,
    fontWeight: 600,
    color: '#cbd5e1',
    marginBottom: 12,
    margin: 0,
    paddingBottom: 8,
    borderBottom: '1px solid #1e293b',
  },
  heatmapContainer: {
    overflowX: 'auto' as const,
    marginTop: 12,
  },
  heatmapTable: {
    width: '100%',
    borderCollapse: 'collapse' as const,
    fontSize: 12,
  },
  heatmapHeader: {
    padding: '8px 10px',
    textAlign: 'center' as const,
    fontWeight: 600,
    fontSize: 11,
    textTransform: 'uppercase' as const,
    color: '#64748b',
    borderBottom: '1px solid #1e293b',
  },
  heatmapIP: {
    padding: '6px 10px',
    fontFamily: 'monospace',
    fontSize: 11,
    color: '#94a3b8',
    borderBottom: '1px solid #0f172a',
    whiteSpace: 'nowrap' as const,
  },
  heatmapCell: {
    padding: '6px 10px',
    textAlign: 'center' as const,
    fontFamily: 'monospace',
    fontSize: 11,
    color: '#e2e8f0',
    borderBottom: '1px solid #0f172a',
  },
  table: {
    width: '100%',
    borderCollapse: 'collapse' as const,
    fontSize: 12,
    marginTop: 12,
  },
  th: {
    padding: '8px 10px',
    textAlign: 'left' as const,
    fontWeight: 600,
    fontSize: 11,
    textTransform: 'uppercase' as const,
    color: '#64748b',
    borderBottom: '1px solid #1e293b',
  },
  tr: {
    borderBottom: '1px solid #0f172a',
  },
  td: {
    padding: '8px 10px',
    color: '#cbd5e1',
    whiteSpace: 'nowrap' as const,
  },
  statusBadge: {
    padding: '2px 8px',
    borderRadius: 4,
    fontSize: 11,
    fontWeight: 600,
  },
  progressContainer: {
    position: 'relative' as const,
    width: 120,
    height: 18,
    background: '#1e293b',
    borderRadius: 4,
    overflow: 'hidden' as const,
  },
  progressBar: {
    position: 'absolute' as const,
    top: 0,
    left: 0,
    height: '100%',
    borderRadius: 4,
    transition: 'width 0.3s',
  },
  progressText: {
    position: 'relative' as const,
    zIndex: 1,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100%',
    fontSize: 10,
    color: '#e2e8f0',
    fontFamily: 'monospace',
  },
  recoGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fill, minmax(240px, 1fr))',
    gap: 10,
    marginTop: 12,
  },
  recoCard: {
    background: '#0f172a',
    border: '1px solid #1e293b',
    borderRadius: 8,
    padding: '12px 14px',
  },
  recoIP: {
    fontFamily: 'monospace',
    fontSize: 13,
    color: '#f1f5f9',
    marginBottom: 4,
  },
  recoText: {
    fontSize: 13,
    fontWeight: 600,
    marginBottom: 4,
  },
  recoDetail: {
    fontSize: 11,
    color: '#64748b',
  },
  footer: {
    textAlign: 'center' as const,
    color: '#334155',
    fontSize: 11,
    marginTop: 32,
  },
};
