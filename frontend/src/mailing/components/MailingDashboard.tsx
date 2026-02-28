import React, { useEffect } from 'react';
import { useDashboard } from '../hooks/useMailingApi';
import './MailingDashboard.css';

interface StatCardProps {
  title: string;
  value: string | number;
  subtitle?: string;
  trend?: number;
  color?: string;
}

const StatCard: React.FC<StatCardProps> = ({ title, value, subtitle, trend, color = '#3b82f6' }) => (
  <div className="stat-card" style={{ borderTopColor: color }}>
    <div className="stat-title">{title}</div>
    <div className="stat-value">{typeof value === 'number' ? value.toLocaleString() : value}</div>
    {subtitle && <div className="stat-subtitle">{subtitle}</div>}
    {trend !== undefined && (
      <div className={`stat-trend ${trend >= 0 ? 'positive' : 'negative'}`}>
        {trend >= 0 ? '↑' : '↓'} {Math.abs(trend).toFixed(1)}%
      </div>
    )}
  </div>
);

interface CapacityBarProps {
  used: number;
  total: number;
  label: string;
}

const CapacityBar: React.FC<CapacityBarProps> = ({ used, total, label }) => {
  const percentage = total > 0 ? (used / total) * 100 : 0;
  const color = percentage > 90 ? '#ef4444' : percentage > 70 ? '#f59e0b' : '#22c55e';

  return (
    <div className="capacity-bar-container">
      <div className="capacity-label">
        <span>{label}</span>
        <span>{used.toLocaleString()} / {total.toLocaleString()}</span>
      </div>
      <div className="capacity-bar-bg">
        <div
          className="capacity-bar-fill"
          style={{ width: `${Math.min(percentage, 100)}%`, backgroundColor: color }}
        />
      </div>
    </div>
  );
};

export const MailingDashboard: React.FC = () => {
  const { data, loading, error, refetch } = useDashboard();

  useEffect(() => {
    refetch();
    const interval = setInterval(refetch, 30000);
    return () => clearInterval(interval);
  }, [refetch]);

  if (loading && !data) {
    return (
      <div className="mailing-dashboard loading">
        <div className="loading-spinner" />
        <p>Loading dashboard...</p>
      </div>
    );
  }

  if (error) {
    return (
      <div className="mailing-dashboard error">
        <p>Error: {error}</p>
        <button onClick={refetch}>Retry</button>
      </div>
    );
  }

  if (!data) {
    return null;
  }

  return (
    <div className="mailing-dashboard">
      <header className="dashboard-header">
        <h1>Mailing Dashboard</h1>
        <button className="refresh-btn" onClick={refetch} disabled={loading}>
          {loading ? 'Refreshing...' : 'Refresh'}
        </button>
      </header>

      <section className="dashboard-section">
        <h2>Overview</h2>
        <div className="stat-grid">
          <StatCard
            title="Total Subscribers"
            value={data.overview.total_subscribers}
            color="#3b82f6"
          />
          <StatCard
            title="Total Lists"
            value={data.overview.total_lists}
            color="#8b5cf6"
          />
          <StatCard
            title="Total Campaigns"
            value={data.overview.total_campaigns}
            color="#10b981"
          />
        </div>
      </section>

      <section className="dashboard-section">
        <h2>Daily Capacity</h2>
        <CapacityBar
          used={data.overview.daily_used}
          total={data.overview.daily_capacity}
          label="Emails Sent Today"
        />
      </section>

      <section className="dashboard-section">
        <h2>Performance</h2>
        <div className="stat-grid">
          <StatCard
            title="Total Sent"
            value={data.performance.total_sent}
            color="#3b82f6"
          />
          <StatCard
            title="Total Opens"
            value={data.performance.total_opens}
            subtitle={`${data.performance.open_rate.toFixed(1)}% open rate`}
            color="#22c55e"
          />
          <StatCard
            title="Total Clicks"
            value={data.performance.total_clicks}
            subtitle={`${data.performance.click_rate.toFixed(1)}% click rate`}
            color="#f59e0b"
          />
          <StatCard
            title="Total Revenue"
            value={`$${data.performance.total_revenue.toFixed(2)}`}
            color="#10b981"
          />
        </div>
      </section>

      <section className="dashboard-section">
        <h2>Recent Campaigns</h2>
        <div className="campaigns-table-container">
          <table className="campaigns-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Status</th>
                <th>Sent</th>
                <th>Opens</th>
                <th>Clicks</th>
                <th>Revenue</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {data.recent_campaigns.map((campaign) => (
                <tr key={campaign.id}>
                  <td className="campaign-name">{campaign.name}</td>
                  <td>
                    <span className={`status-badge status-${campaign.status}`}>
                      {campaign.status}
                    </span>
                  </td>
                  <td>{campaign.sent_count.toLocaleString()}</td>
                  <td>
                    {campaign.open_count.toLocaleString()}
                    {campaign.sent_count > 0 && (
                      <span className="rate">
                        ({((campaign.open_count / campaign.sent_count) * 100).toFixed(1)}%)
                      </span>
                    )}
                  </td>
                  <td>
                    {campaign.click_count.toLocaleString()}
                    {campaign.sent_count > 0 && (
                      <span className="rate">
                        ({((campaign.click_count / campaign.sent_count) * 100).toFixed(1)}%)
                      </span>
                    )}
                  </td>
                  <td>${campaign.revenue.toFixed(2)}</td>
                  <td>{new Date(campaign.created_at).toLocaleDateString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  );
};

export default MailingDashboard;
