import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { PersonalizedInput } from './PersonalizedInput';
import {
  faEnvelope,
  faChartBar,
  faChartPie,
  faPaperPlane,
  faCalendarAlt,
  faClock,
  faPlay,
  faPause,
  faStop,
  faEye,
  faEdit,
  faCopy,
  faSearch,
  faSync,
  faCheckCircle,
  faTimesCircle,
  faExclamationTriangle,
  faSpinner,
  faEnvelopeOpen,
  faMousePointer,
  faExclamationCircle,
  faUserMinus,
  faBan,
  faArrowUp,
  faArrowDown,
  faArrowRight,
  faTrophy,
  faUsers,
  faPercentage,
  faFileAlt,
  faTachometerAlt,
  faList,
  faCalendarCheck,
  faInfoCircle,
  faPlus,
  faTimes,
  faSave,
  faBullseye,
  faCode,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import './CampaignPortal.css';

// ============================================================================
// TYPES
// ============================================================================

interface Campaign {
  id: string;
  name: string;
  subject: string;
  status: 'draft' | 'scheduled' | 'preparing' | 'sending' | 'paused' | 'completed' | 'completed_with_errors' | 'cancelled' | 'failed';
  total_recipients: number;
  sent_count: number;
  open_count: number;
  click_count: number;
  bounce_count: number;
  complaint_count: number;
  unsubscribe_count: number;
  queued_count?: number;
  scheduled_at?: string;
  started_at?: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
  list_name?: string;
  segment_name?: string;
  profile_name?: string;
  profile_vendor?: string;
  revenue?: number;
  from_name?: string;
  from_email?: string;
  throttle_speed?: string;
}

// Minimum preparation time in minutes (must match backend)
const MIN_PREPARATION_MINUTES = 5;

// Helper function to check if a campaign can be edited
const canEditCampaign = (campaign: Campaign): boolean => {
  // Can always edit drafts
  if (campaign.status === 'draft') return true;
  
  // Cannot edit preparing, sending, completed, cancelled, failed
  if (['preparing', 'sending', 'completed', 'completed_with_errors', 'cancelled', 'failed', 'paused'].includes(campaign.status)) {
    return false;
  }
  
  // For scheduled campaigns, check edit lock window
  if (campaign.status === 'scheduled' && campaign.scheduled_at) {
    const scheduledTime = new Date(campaign.scheduled_at);
    const editLockTime = new Date(scheduledTime.getTime() - MIN_PREPARATION_MINUTES * 60 * 1000);
    return new Date() < editLockTime;
  }
  
  return false;
};

// Helper function to get edit lock info
const getEditLockInfo = (campaign: Campaign): { isLocked: boolean; lockTime?: Date; message?: string } => {
  if (campaign.status !== 'scheduled' || !campaign.scheduled_at) {
    return { isLocked: false };
  }
  
  const scheduledTime = new Date(campaign.scheduled_at);
  const editLockTime = new Date(scheduledTime.getTime() - MIN_PREPARATION_MINUTES * 60 * 1000);
  const isLocked = new Date() >= editLockTime;
  
  if (isLocked) {
    return {
      isLocked: true,
      lockTime: editLockTime,
      message: `Campaign locked for preparation. Scheduled to send at ${scheduledTime.toLocaleString()}`
    };
  }
  
  return {
    isLocked: false,
    lockTime: editLockTime,
    message: `Can edit until ${editLockTime.toLocaleString()}`
  };
};

interface CampaignStats {
  sent: number;
  opens: number;
  clicks: number;
  bounces: number;
  complaints: number;
  unsubscribes: number;
  open_rate: number;
  click_rate: number;
  bounce_rate: number;
  complaint_rate: number;
  unsubscribe_rate: number;
}

interface DashboardStats {
  total_campaigns: number;
  draft_count: number;
  scheduled_count: number;
  sending_count: number;
  completed_count: number;
  total_sent: number;
  total_opens: number;
  total_clicks: number;
  total_bounces: number;
  total_complaints: number;
  total_unsubscribes: number;
  avg_open_rate: number;
  avg_click_rate: number;
  avg_bounce_rate: number;
  avg_complaint_rate: number;
  total_revenue: number;
  recent_campaigns: Campaign[];
  scheduled_campaigns: Campaign[];
  top_campaigns: Campaign[];
}

type ViewType = 'dashboard' | 'campaigns' | 'scheduled' | 'details' | 'create' | 'edit';
type StatusFilter = 'all' | 'draft' | 'scheduled' | 'sending' | 'completed' | 'paused' | 'failed';

const API_BASE = '/api/mailing';

// ============================================================================
// API HELPER WITH ORGANIZATION CONTEXT
// ============================================================================

/**
 * Creates headers with organization context for authenticated API calls
 */
const getOrgHeaders = (organizationId: string | undefined): HeadersInit => {
  const headers: HeadersInit = {
    'Content-Type': 'application/json',
  };
  if (organizationId) {
    headers['X-Organization-ID'] = organizationId;
  }
  return headers;
};

/**
 * Fetch wrapper that includes organization context
 */
const orgFetch = async (
  url: string,
  organizationId: string | undefined,
  options: RequestInit = {}
): Promise<Response> => {
  return fetch(url, {
    ...options,
    headers: {
      ...getOrgHeaders(organizationId),
      ...(options.headers || {}),
    },
    credentials: 'include',
  });
};

// ============================================================================
// HELPER COMPONENTS
// ============================================================================

const StatusBadge: React.FC<{ status: Campaign['status'] }> = ({ status }) => {
  const statusConfig: Record<string, { icon: any; label: string; className: string }> = {
    draft: { icon: faFileAlt, label: 'Draft', className: 'status-draft' },
    scheduled: { icon: faCalendarAlt, label: 'Scheduled', className: 'status-scheduled' },
    preparing: { icon: faClock, label: 'Preparing', className: 'status-preparing' },
    sending: { icon: faSpinner, label: 'Sending', className: 'status-sending' },
    paused: { icon: faPause, label: 'Paused', className: 'status-paused' },
    completed: { icon: faCheckCircle, label: 'Completed', className: 'status-completed' },
    completed_with_errors: { icon: faExclamationTriangle, label: 'Completed w/ Errors', className: 'status-warning' },
    cancelled: { icon: faTimesCircle, label: 'Cancelled', className: 'status-cancelled' },
    failed: { icon: faExclamationCircle, label: 'Failed', className: 'status-failed' },
  };

  const config = statusConfig[status] || statusConfig.draft;

  return (
    <span className={`campaign-status-badge ${config.className}`}>
      <FontAwesomeIcon icon={config.icon} spin={status === 'sending' || status === 'preparing'} />
      {config.label}
    </span>
  );
};

const MetricCard: React.FC<{
  icon: any;
  label: string;
  value: number | string;
  subValue?: string;
  trend?: 'up' | 'down' | 'neutral';
  color?: string;
}> = ({ icon, label, value, subValue, trend, color = 'primary' }) => (
  <div className={`metric-card metric-${color}`}>
    <div className="metric-icon">
      <FontAwesomeIcon icon={icon} />
    </div>
    <div className="metric-content">
      <div className="metric-value">
        {typeof value === 'number' ? value.toLocaleString() : value}
        {trend && (
          <span className={`metric-trend trend-${trend}`}>
            <FontAwesomeIcon icon={trend === 'up' ? faArrowUp : trend === 'down' ? faArrowDown : faArrowRight} />
          </span>
        )}
      </div>
      <div className="metric-label">{label}</div>
      {subValue && <div className="metric-sub">{subValue}</div>}
    </div>
  </div>
);

const ProgressBar: React.FC<{ value: number; max: number; color?: string }> = ({ value, max, color = 'primary' }) => {
  const percentage = max > 0 ? (value / max) * 100 : 0;
  return (
    <div className="progress-bar-container">
      <div className={`progress-bar-fill progress-${color}`} style={{ width: `${Math.min(percentage, 100)}%` }} />
    </div>
  );
};

const RateDisplay: React.FC<{ rate: number; label: string; icon: any; color: string }> = ({ rate, label, icon, color }) => (
  <div className={`rate-display rate-${color}`}>
    <FontAwesomeIcon icon={icon} className="rate-icon" />
    <span className="rate-value">{rate.toFixed(2)}%</span>
    <span className="rate-label">{label}</span>
  </div>
);

// ============================================================================
// DASHBOARD VIEW
// ============================================================================

const CampaignDashboard: React.FC<{
  stats: DashboardStats | null;
  loading: boolean;
  onViewCampaign: (id: string) => void;
  onViewAll: () => void;
  onViewScheduled: () => void;
}> = ({ stats, loading, onViewCampaign, onViewAll, onViewScheduled }) => {
  if (loading) {
    return (
      <div className="loading-state">
        <FontAwesomeIcon icon={faSpinner} spin size="2x" />
        <p>Loading campaign analytics...</p>
      </div>
    );
  }

  if (!stats) {
    return (
      <div className="empty-state">
        <FontAwesomeIcon icon={faEnvelope} size="3x" />
        <h3>No Campaign Data</h3>
        <p>Create your first campaign to see analytics here</p>
      </div>
    );
  }

  return (
    <div className="campaign-dashboard">
      {/* Hero Stats */}
      <div className="hero-stats">
        <MetricCard 
          icon={faEnvelope} 
          label="Total Campaigns" 
          value={stats.total_campaigns}
          subValue={`${stats.completed_count} completed`}
          color="primary"
        />
        <MetricCard 
          icon={faPaperPlane} 
          label="Total Sent" 
          value={stats.total_sent}
          subValue="All time"
          color="blue"
        />
        <MetricCard 
          icon={faEnvelopeOpen} 
          label="Total Opens" 
          value={stats.total_opens}
          subValue={`${stats.avg_open_rate.toFixed(1)}% avg rate`}
          color="green"
        />
        <MetricCard 
          icon={faMousePointer} 
          label="Total Clicks" 
          value={stats.total_clicks}
          subValue={`${stats.avg_click_rate.toFixed(1)}% avg rate`}
          color="purple"
        />
      </div>

      {/* Performance Intelligence Bar */}
      {stats.total_campaigns > 0 && (() => {
        const efficiency = stats.total_sent > 0
          ? ((stats.total_opens / stats.total_sent) * 100)
          : 0;
        const clickToOpen = stats.total_opens > 0
          ? ((stats.total_clicks / stats.total_opens) * 100)
          : 0;
        const deliveryHealth = stats.total_sent > 0
          ? Math.max(0, 100 - (stats.avg_bounce_rate * 10) - (stats.avg_complaint_rate * 1000))
          : 0;
        const engagementScore = Math.min(100, (efficiency * 2) + (clickToOpen * 1.5) + (deliveryHealth * 0.3));
        const grade = engagementScore >= 80 ? 'A' : engagementScore >= 60 ? 'B' : engagementScore >= 40 ? 'C' : engagementScore >= 20 ? 'D' : 'F';
        const gradeColor = engagementScore >= 80 ? '#22c55e' : engagementScore >= 60 ? '#3b82f6' : engagementScore >= 40 ? '#f59e0b' : '#ef4444';

        return (
          <div className="perf-intel-bar">
            <div className="perf-intel-grade" style={{ borderColor: gradeColor }}>
              <span className="perf-intel-grade-letter" style={{ color: gradeColor }}>{grade}</span>
              <span className="perf-intel-grade-score">{engagementScore.toFixed(0)}</span>
            </div>
            <div className="perf-intel-metrics">
              <div className="perf-intel-metric">
                <span className="pim-label">OPEN EFFICIENCY</span>
                <span className="pim-value">{efficiency.toFixed(1)}%</span>
              </div>
              <div className="perf-intel-metric">
                <span className="pim-label">CLICK-TO-OPEN</span>
                <span className="pim-value">{clickToOpen.toFixed(1)}%</span>
              </div>
              <div className="perf-intel-metric">
                <span className="pim-label">DELIVERY HEALTH</span>
                <span className="pim-value" style={{ color: deliveryHealth >= 90 ? '#22c55e' : deliveryHealth >= 70 ? '#f59e0b' : '#ef4444' }}>{deliveryHealth.toFixed(0)}%</span>
              </div>
              <div className="perf-intel-metric">
                <span className="pim-label">CAMPAIGNS</span>
                <span className="pim-value">{stats.total_campaigns}</span>
              </div>
              <div className="perf-intel-metric">
                <span className="pim-label">COMPLETION RATE</span>
                <span className="pim-value">{stats.total_campaigns > 0 ? ((stats.completed_count / stats.total_campaigns) * 100).toFixed(0) : 0}%</span>
              </div>
            </div>
          </div>
        );
      })()}

      {/* Campaign Status Overview */}
      <div className="status-overview">
        <div className="section-header">
          <h3><FontAwesomeIcon icon={faChartPie} /> Campaign Status</h3>
        </div>
        <div className="status-grid">
          <div className="status-item status-draft" onClick={onViewAll}>
            <div className="status-count">{stats.draft_count}</div>
            <div className="status-label">Drafts</div>
          </div>
          <div className="status-item status-scheduled" onClick={onViewScheduled}>
            <div className="status-count">{stats.scheduled_count}</div>
            <div className="status-label">Scheduled</div>
          </div>
          <div className="status-item status-sending">
            <div className="status-count">{stats.sending_count}</div>
            <div className="status-label">Sending</div>
          </div>
          <div className="status-item status-completed">
            <div className="status-count">{stats.completed_count}</div>
            <div className="status-label">Completed</div>
          </div>
        </div>
      </div>

      {/* Performance Rates */}
      <div className="performance-section">
        <div className="section-header">
          <h3><FontAwesomeIcon icon={faPercentage} /> Performance Rates</h3>
        </div>
        <div className="rates-grid">
          <RateDisplay rate={stats.avg_open_rate} label="Open Rate" icon={faEnvelopeOpen} color="green" />
          <RateDisplay rate={stats.avg_click_rate} label="Click Rate" icon={faMousePointer} color="blue" />
          <RateDisplay rate={stats.avg_bounce_rate} label="Bounce Rate" icon={faBan} color="orange" />
          <RateDisplay rate={stats.avg_complaint_rate} label="Complaint Rate" icon={faExclamationTriangle} color="red" />
        </div>
      </div>

      {/* Health Metrics */}
      <div className="health-section">
        <div className="section-header">
          <h3><FontAwesomeIcon icon={faTachometerAlt} /> Deliverability Health</h3>
        </div>
        <div className="health-metrics">
          <div className="health-item">
            <div className="health-label">Bounces</div>
            <div className="health-value">{stats.total_bounces.toLocaleString()}</div>
            <ProgressBar value={stats.total_bounces} max={stats.total_sent} color={stats.avg_bounce_rate > 3 ? 'red' : 'green'} />
          </div>
          <div className="health-item">
            <div className="health-label">Complaints</div>
            <div className="health-value">{stats.total_complaints.toLocaleString()}</div>
            <ProgressBar value={stats.total_complaints} max={stats.total_sent} color={stats.avg_complaint_rate > 0.1 ? 'red' : 'green'} />
          </div>
          <div className="health-item">
            <div className="health-label">Unsubscribes</div>
            <div className="health-value">{stats.total_unsubscribes.toLocaleString()}</div>
            <ProgressBar value={stats.total_unsubscribes} max={stats.total_sent} color="blue" />
          </div>
        </div>
      </div>

      {/* Two Column Layout */}
      <div className="dashboard-columns">
        {/* Scheduled Campaigns */}
        <div className="dashboard-section">
          <div className="section-header">
            <h3><FontAwesomeIcon icon={faCalendarCheck} /> Upcoming Scheduled</h3>
            <button className="section-action" onClick={onViewScheduled}>
              View All <FontAwesomeIcon icon={faArrowRight} />
            </button>
          </div>
          <div className="campaign-list-mini">
            {stats.scheduled_campaigns.length === 0 ? (
              <div className="empty-list">
                <FontAwesomeIcon icon={faCalendarAlt} />
                <span>No scheduled campaigns</span>
              </div>
            ) : (
              stats.scheduled_campaigns.slice(0, 5).map(campaign => (
                <div key={campaign.id} className="campaign-mini-card" onClick={() => onViewCampaign(campaign.id)}>
                  <div className="mini-card-info">
                    <div className="mini-card-name">{campaign.name}</div>
                    <div className="mini-card-meta">
                      <FontAwesomeIcon icon={faClock} />
                      {campaign.scheduled_at ? new Date(campaign.scheduled_at).toLocaleString() : 'Not set'}
                    </div>
                  </div>
                  <div className="mini-card-recipients">
                    <FontAwesomeIcon icon={faUsers} />
                    {campaign.total_recipients?.toLocaleString() || 0}
                  </div>
                </div>
              ))
            )}
          </div>
        </div>

        {/* Top Performing Campaigns */}
        <div className="dashboard-section">
          <div className="section-header">
            <h3><FontAwesomeIcon icon={faTrophy} /> Top Performers</h3>
            <button className="section-action" onClick={onViewAll}>
              View All <FontAwesomeIcon icon={faArrowRight} />
            </button>
          </div>
          <div className="campaign-list-mini">
            {stats.top_campaigns.length === 0 ? (
              <div className="empty-list">
                <FontAwesomeIcon icon={faChartBar} />
                <span>No completed campaigns yet</span>
              </div>
            ) : (
              stats.top_campaigns.slice(0, 5).map((campaign, index) => (
                <div key={campaign.id} className="campaign-mini-card" onClick={() => onViewCampaign(campaign.id)}>
                  <div className="rank-badge">#{index + 1}</div>
                  <div className="mini-card-info">
                    <div className="mini-card-name">{campaign.name}</div>
                    <div className="mini-card-stats">
                      <span className="stat-open">
                        <FontAwesomeIcon icon={faEnvelopeOpen} />
                        {campaign.sent_count > 0 ? ((campaign.open_count / campaign.sent_count) * 100).toFixed(1) : 0}%
                      </span>
                      <span className="stat-click">
                        <FontAwesomeIcon icon={faMousePointer} />
                        {campaign.sent_count > 0 ? ((campaign.click_count / campaign.sent_count) * 100).toFixed(1) : 0}%
                      </span>
                    </div>
                  </div>
                </div>
              ))
            )}
          </div>
        </div>
      </div>

      {/* Recent Campaigns */}
      <div className="recent-section">
        <div className="section-header">
          <h3><FontAwesomeIcon icon={faClock} /> Recent Campaigns</h3>
          <button className="section-action" onClick={onViewAll}>
            View All <FontAwesomeIcon icon={faArrowRight} />
          </button>
        </div>
        <div className="recent-campaigns-table">
          <table>
            <thead>
              <tr>
                <th>Campaign</th>
                <th>Status</th>
                <th>Sent</th>
                <th>Opens</th>
                <th>Clicks</th>
                <th>Date</th>
              </tr>
            </thead>
            <tbody>
              {stats.recent_campaigns.length === 0 ? (
                <tr>
                  <td colSpan={6} className="empty-row">
                    <FontAwesomeIcon icon={faEnvelope} /> No recent campaigns
                  </td>
                </tr>
              ) : (
                stats.recent_campaigns.slice(0, 10).map(campaign => (
                  <tr key={campaign.id} onClick={() => onViewCampaign(campaign.id)}>
                    <td className="campaign-name-cell">
                      <div className="campaign-name">{campaign.name}</div>
                      <div className="campaign-subject">{campaign.subject}</div>
                    </td>
                    <td><StatusBadge status={campaign.status} /></td>
                    <td>{campaign.sent_count?.toLocaleString() || 0}</td>
                    <td>
                      {campaign.open_count?.toLocaleString() || 0}
                      <span className="cell-rate">
                        ({campaign.sent_count > 0 ? ((campaign.open_count / campaign.sent_count) * 100).toFixed(1) : 0}%)
                      </span>
                    </td>
                    <td>
                      {campaign.click_count?.toLocaleString() || 0}
                      <span className="cell-rate">
                        ({campaign.sent_count > 0 ? ((campaign.click_count / campaign.sent_count) * 100).toFixed(1) : 0}%)
                      </span>
                    </td>
                    <td>{new Date(campaign.created_at).toLocaleDateString()}</td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// CAMPAIGNS LIST VIEW
// ============================================================================

const CampaignsList: React.FC<{
  campaigns: Campaign[];
  loading: boolean;
  filter: StatusFilter;
  search: string;
  onFilterChange: (filter: StatusFilter) => void;
  onSearchChange: (search: string) => void;
  onViewCampaign: (id: string) => void;
  onAction: (id: string, action: string) => void;
  onRefresh: () => void;
}> = ({ campaigns, loading, filter, search, onFilterChange, onSearchChange, onViewCampaign, onAction, onRefresh }) => {
  const filteredCampaigns = campaigns.filter(c => {
    const matchesFilter = filter === 'all' || c.status === filter;
    const matchesSearch = search === '' || 
      c.name.toLowerCase().includes(search.toLowerCase()) ||
      c.subject?.toLowerCase().includes(search.toLowerCase());
    return matchesFilter && matchesSearch;
  });

  return (
    <div className="campaigns-list-view">
      {/* Filters and Search */}
      <div className="list-controls">
        <div className="filter-tabs">
          {(['all', 'draft', 'scheduled', 'sending', 'completed', 'paused', 'failed'] as StatusFilter[]).map(f => (
            <button
              key={f}
              className={`filter-tab ${filter === f ? 'active' : ''}`}
              onClick={() => onFilterChange(f)}
            >
              {f === 'all' ? 'All' : f.charAt(0).toUpperCase() + f.slice(1)}
              {f !== 'all' && (
                <span className="filter-count">
                  {campaigns.filter(c => c.status === f).length}
                </span>
              )}
            </button>
          ))}
        </div>
        <div className="search-controls">
          <div className="search-input-wrapper">
            <FontAwesomeIcon icon={faSearch} className="search-icon" />
            <input
              type="text"
              placeholder="Search campaigns..."
              value={search}
              onChange={(e) => onSearchChange(e.target.value)}
            />
          </div>
          <button className="refresh-btn" onClick={onRefresh}>
            <FontAwesomeIcon icon={faSync} spin={loading} />
          </button>
        </div>
      </div>

      {/* Campaign Cards */}
      {loading ? (
        <div className="loading-state">
          <FontAwesomeIcon icon={faSpinner} spin size="2x" />
          <p>Loading campaigns...</p>
        </div>
      ) : filteredCampaigns.length === 0 ? (
        <div className="empty-state">
          <FontAwesomeIcon icon={faEnvelope} size="3x" />
          <h3>No Campaigns Found</h3>
          <p>{filter !== 'all' ? `No ${filter} campaigns` : 'Create your first campaign to get started'}</p>
        </div>
      ) : (
        <div className="campaigns-grid">
          {filteredCampaigns.map(campaign => (
            <div key={campaign.id} className="campaign-card">
                <div className="card-header">
                <StatusBadge status={campaign.status} />
                <div className="card-actions">
                  <button onClick={() => onViewCampaign(campaign.id)} title="View Details">
                    <FontAwesomeIcon icon={faEye} />
                  </button>
                  {canEditCampaign(campaign) && (
                    <button onClick={() => onAction(campaign.id, 'edit')} title="Edit Campaign">
                      <FontAwesomeIcon icon={faEdit} />
                    </button>
                  )}
                  <button onClick={() => onAction(campaign.id, 'duplicate')} title="Duplicate">
                    <FontAwesomeIcon icon={faCopy} />
                  </button>
                  {['scheduled', 'preparing', 'sending'].includes(campaign.status) && (
                    <button onClick={() => onAction(campaign.id, 'pause')} title="Pause">
                      <FontAwesomeIcon icon={faPause} />
                    </button>
                  )}
                  {campaign.status === 'paused' && (
                    <button onClick={() => onAction(campaign.id, 'resume')} title="Resume">
                      <FontAwesomeIcon icon={faPlay} />
                    </button>
                  )}
                  {['scheduled', 'preparing', 'sending', 'paused'].includes(campaign.status) && (
                    <button onClick={() => onAction(campaign.id, 'cancel')} title="Cancel" className="danger">
                      <FontAwesomeIcon icon={faStop} />
                    </button>
                  )}
                </div>
              </div>
              
              <div className="card-body" onClick={() => onViewCampaign(campaign.id)}>
                <h4 className="campaign-name">{campaign.name}</h4>
                <p className="campaign-subject">{campaign.subject}</p>
                
                <div className="campaign-meta">
                  {campaign.profile_name && (
                    <span className="meta-item">
                      <FontAwesomeIcon icon={faPaperPlane} /> {campaign.profile_name}
                    </span>
                  )}
                  {campaign.scheduled_at && (
                    <span className="meta-item">
                      <FontAwesomeIcon icon={faCalendarAlt} /> {new Date(campaign.scheduled_at).toLocaleString()}
                    </span>
                  )}
                </div>
              </div>

              <div className="card-stats">
                <div className="stat">
                  <span className="stat-value">{campaign.sent_count?.toLocaleString() || 0}</span>
                  <span className="stat-label">Sent</span>
                </div>
                <div className="stat">
                  <span className="stat-value">{campaign.open_count?.toLocaleString() || 0}</span>
                  <span className="stat-label">Opens</span>
                </div>
                <div className="stat">
                  <span className="stat-value">{campaign.click_count?.toLocaleString() || 0}</span>
                  <span className="stat-label">Clicks</span>
                </div>
                <div className="stat">
                  <span className="stat-value">
                    {campaign.sent_count > 0 ? ((campaign.open_count / campaign.sent_count) * 100).toFixed(1) : 0}%
                  </span>
                  <span className="stat-label">Open Rate</span>
                </div>
              </div>

              {campaign.status === 'sending' && campaign.total_recipients > 0 && (
                <div className="sending-progress">
                  <ProgressBar value={campaign.sent_count} max={campaign.total_recipients} color="primary" />
                  <span className="progress-text">
                    {campaign.sent_count?.toLocaleString()} / {campaign.total_recipients?.toLocaleString()}
                  </span>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
};

// ============================================================================
// CAMPAIGN DETAILS MODAL
// ============================================================================

const CampaignDetailsModal: React.FC<{
  campaign: Campaign | null;
  stats: CampaignStats | null;
  loading: boolean;
  onClose: () => void;
  onAction: (id: string, action: string) => void;
}> = ({ campaign, stats, loading, onClose, onAction }) => {
  if (!campaign) return null;

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="campaign-details-modal" onClick={e => e.stopPropagation()}>
        <div className="modal-header">
          <div className="modal-title">
            <h2>{campaign.name}</h2>
            <StatusBadge status={campaign.status} />
          </div>
          <button className="close-btn" onClick={onClose}>&times;</button>
        </div>

        <div className="modal-body">
          {loading ? (
            <div className="loading-state">
              <FontAwesomeIcon icon={faSpinner} spin size="2x" />
              <p>Loading campaign details...</p>
            </div>
          ) : (
            <>
              {/* Campaign Info */}
              <div className="details-section">
                <h3><FontAwesomeIcon icon={faInfoCircle} /> Campaign Info</h3>
                <div className="info-grid">
                  <div className="info-item">
                    <span className="info-label">Subject</span>
                    <span className="info-value">{campaign.subject}</span>
                  </div>
                  <div className="info-item">
                    <span className="info-label">From</span>
                    <span className="info-value">{campaign.from_name} &lt;{campaign.from_email}&gt;</span>
                  </div>
                  <div className="info-item">
                    <span className="info-label">Sending Profile</span>
                    <span className="info-value">{campaign.profile_name || 'Default'}</span>
                  </div>
                  <div className="info-item">
                    <span className="info-label">Recipients</span>
                    <span className="info-value">{campaign.total_recipients?.toLocaleString() || 0}</span>
                  </div>
                  {campaign.scheduled_at && (
                    <div className="info-item">
                      <span className="info-label">Scheduled</span>
                      <span className="info-value">{new Date(campaign.scheduled_at).toLocaleString()}</span>
                    </div>
                  )}
                  <div className="info-item">
                    <span className="info-label">Created</span>
                    <span className="info-value">{new Date(campaign.created_at).toLocaleString()}</span>
                  </div>
                </div>
              </div>

              {/* Performance Metrics */}
              {stats && (
                <div className="details-section">
                  <h3><FontAwesomeIcon icon={faChartBar} /> Performance Metrics</h3>
                  <div className="metrics-grid">
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faPaperPlane} className="metric-icon blue" />
                      <div className="metric-value">{stats.sent.toLocaleString()}</div>
                      <div className="metric-label">Sent</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faEnvelopeOpen} className="metric-icon green" />
                      <div className="metric-value">{stats.opens.toLocaleString()}</div>
                      <div className="metric-label">Opens ({stats.open_rate.toFixed(1)}%)</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faMousePointer} className="metric-icon purple" />
                      <div className="metric-value">{stats.clicks.toLocaleString()}</div>
                      <div className="metric-label">Clicks ({stats.click_rate.toFixed(1)}%)</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faBan} className="metric-icon orange" />
                      <div className="metric-value">{stats.bounces.toLocaleString()}</div>
                      <div className="metric-label">Bounces ({stats.bounce_rate.toFixed(1)}%)</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faExclamationTriangle} className="metric-icon red" />
                      <div className="metric-value">{stats.complaints.toLocaleString()}</div>
                      <div className="metric-label">Complaints ({stats.complaint_rate.toFixed(3)}%)</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faUserMinus} className="metric-icon gray" />
                      <div className="metric-value">{stats.unsubscribes.toLocaleString()}</div>
                      <div className="metric-label">Unsubscribes ({stats.unsubscribe_rate.toFixed(2)}%)</div>
                    </div>
                  </div>
                </div>
              )}

              {/* Edit Lock Info */}
              {campaign.status === 'scheduled' && campaign.scheduled_at && (
                <div className={`edit-lock-info ${getEditLockInfo(campaign).isLocked ? 'locked' : ''}`}>
                  <FontAwesomeIcon icon={getEditLockInfo(campaign).isLocked ? faExclamationTriangle : faInfoCircle} />
                  <span>{getEditLockInfo(campaign).message}</span>
                </div>
              )}

              {/* Actions */}
              <div className="details-actions">
                {canEditCampaign(campaign) && (
                  <button className="action-btn secondary" onClick={() => onAction(campaign.id, 'edit')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit Campaign
                  </button>
                )}
                {campaign.status === 'draft' && (
                  <>
                    <button className="action-btn primary" onClick={() => onAction(campaign.id, 'send')}>
                      <FontAwesomeIcon icon={faPaperPlane} /> Send Now
                    </button>
                    <button className="action-btn secondary" onClick={() => onAction(campaign.id, 'schedule')}>
                      <FontAwesomeIcon icon={faCalendarAlt} /> Schedule
                    </button>
                  </>
                )}
                {['scheduled', 'preparing', 'sending'].includes(campaign.status) && (
                  <button className="action-btn warning" onClick={() => onAction(campaign.id, 'pause')}>
                    <FontAwesomeIcon icon={faPause} /> Pause
                  </button>
                )}
                {campaign.status === 'paused' && (
                  <>
                    <button className="action-btn primary" onClick={() => onAction(campaign.id, 'resume')}>
                      <FontAwesomeIcon icon={faPlay} /> Resume
                    </button>
                  </>
                )}
                {['scheduled', 'preparing', 'sending', 'paused'].includes(campaign.status) && (
                  <button className="action-btn danger" onClick={() => onAction(campaign.id, 'cancel')}>
                    <FontAwesomeIcon icon={faStop} /> Cancel
                  </button>
                )}
                <button className="action-btn secondary" onClick={() => onAction(campaign.id, 'duplicate')}>
                  <FontAwesomeIcon icon={faCopy} /> Duplicate
                </button>
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// CAMPAIGN EDITOR - Modern Step-Based Builder
// ============================================================================

import { SearchableMultiSelect, SelectOption } from './SearchableMultiSelect';
import { EmailEditor } from './EmailEditor';
import { EverflowCreativeSelector } from './EverflowCreativeSelector';
import { CampaignPurposeTab, CampaignObjective, DEFAULT_OBJECTIVE } from './CampaignPurposeTab';
import { AgentConfigWizard } from './AgentConfigWizard';
import './SearchableMultiSelect.css';
import './EmailEditor.css';
import './CampaignPurposeTab.css';
import './AgentConfigWizard.css';

type EditorStep = 'purpose' | 'details' | 'audience' | 'content' | 'schedule' | 'review';

interface CampaignEditorProps {
  campaign: Campaign | null;
  onSave: () => void;
  onCancel: () => void;
  initialOffer?: { offerId: string; offerName: string } | null;
  onOfferConsumed?: () => void;
}

const SUBJECT_MAX_LENGTH = 60;
const PREHEADER_MAX_LENGTH = 100;

// Test/Proof Email Result interface
interface TestEmailResult {
  success: boolean;
  message_id?: string;
  sent_at?: string;
  to?: string;
  error?: string;
  vendor?: string;
}

const CampaignEditor: React.FC<CampaignEditorProps> = ({ campaign, onSave, onCancel, initialOffer, onOfferConsumed }) => {
  const { organization } = useAuth();
  const isEditing = campaign !== null;
  const [currentStep, setCurrentStep] = useState<EditorStep>('purpose');
  const [objective, setObjective] = useState<CampaignObjective>(DEFAULT_OBJECTIVE);
  const [formData, setFormData] = useState({
    name: campaign?.name || '',
    subject: campaign?.subject || '',
    preheader: '',
    from_name: campaign?.from_name || '',
    from_email: campaign?.from_email || '',
    reply_to: '',
    scheduled_at: campaign?.scheduled_at ? new Date(campaign.scheduled_at).toISOString().slice(0, 16) : '',
    html_content: '',
    throttle_speed: campaign?.throttle_speed || 'instant',
  });
  const [saving, setSaving] = useState(false);
  const [sendingTest, setSendingTest] = useState(false);
  const [profiles, setProfiles] = useState<Array<{ id: string; name: string; from_name: string; from_email: string; vendor_type: string; is_default?: boolean }>>([]);
  const [selectedProfile, setSelectedProfile] = useState('');
  const [segments, setSegments] = useState<SelectOption[]>([]);
  const [selectedSegments, setSelectedSegments] = useState<string[]>([]);
  const [suppressionLists, setSuppressionLists] = useState<SelectOption[]>([]);
  const [selectedSuppressions, setSelectedSuppressions] = useState<string[]>([]);
  const [selectedSuppressionSegments, setSelectedSuppressionSegments] = useState<string[]>([]);
  const [estimatedReach, setEstimatedReach] = useState(0);
  
  // AI Agent Wizard Mode
  const [useAgentWizard, setUseAgentWizard] = useState(true);
  
  // Everflow Creative Mode — when a creative is applied, bypass TipTap and use raw HTML
  const [creativeMode, setCreativeMode] = useState(false);
  const [creativeCodeView, setCreativeCodeView] = useState(false);

  // Test/Proof Email Modal State
  const [showTestModal, setShowTestModal] = useState(false);
  const [testEmails, setTestEmails] = useState('');
  const [testSubjectPrefix, setTestSubjectPrefix] = useState('[TEST] ');
  const [testResult, setTestResult] = useState<TestEmailResult | null>(null);
  const [testHistory, setTestHistory] = useState<TestEmailResult[]>([]);

  const steps: { id: EditorStep; label: string; icon: any }[] = [
    { id: 'purpose', label: 'Purpose', icon: faBullseye },
    { id: 'details', label: 'Details', icon: faFileAlt },
    { id: 'audience', label: 'Audience', icon: faUsers },
    { id: 'content', label: 'Content', icon: faEnvelope },
    { id: 'schedule', label: 'Schedule', icon: faCalendarAlt },
    { id: 'review', label: 'Review', icon: faCheckCircle },
  ];

  // Load data on mount
  useEffect(() => {
    const loadData = async () => {
      try {
        // First, load all reference data
        const [profilesRes, segmentsRes, suppressionsRes] = await Promise.all([
          orgFetch(`${API_BASE}/sending-profiles`, organization?.id),
          orgFetch(`${API_BASE}/segments`, organization?.id),
          orgFetch(`${API_BASE}/suppression-lists`, organization?.id),
        ]);
        
        const [profilesData, segmentsData, suppressionsData] = await Promise.all([
          profilesRes.json(),
          segmentsRes.json(),
          suppressionsRes.json(),
        ]);
        
        setProfiles(profilesData.profiles || []);
        
        // Transform segments for SearchableMultiSelect
        const segmentOptions: SelectOption[] = (segmentsData.segments || []).map((s: any, i: number) => ({
          id: s.id,
          name: s.name,
          count: s.subscriber_count || 0,
          category: s.category || 'All Segments',
          isFavorite: i < 3,
          isRecent: i < 5,
          type: 'segment' as const,
        }));
        setSegments(segmentOptions);
        
        // Transform suppression lists
        const suppressionOptions: SelectOption[] = (suppressionsData.lists || suppressionsData || []).map((s: any) => ({
          id: s.id,
          name: s.name,
          count: s.entry_count || 0,
          category: s.is_global ? 'Global' : 'Campaign',
          isLocked: s.is_global,
          isRequired: s.is_global,
          type: 'suppression' as const,
        }));
        setSuppressionLists(suppressionOptions);
        
        // Now load campaign data if editing
        if (isEditing && campaign) {
          const campaignRes = await orgFetch(`${API_BASE}/campaigns/${campaign.id}`, organization?.id);
          const data = await campaignRes.json();
          
          // Set form data with all campaign fields
          // Convert UTC scheduled_at to local time for datetime-local input
          let localScheduledAt = '';
          if (data.scheduled_at) {
            const utcDate = new Date(data.scheduled_at);
            // Format as local datetime-local value (YYYY-MM-DDTHH:mm)
            const year = utcDate.getFullYear();
            const month = String(utcDate.getMonth() + 1).padStart(2, '0');
            const day = String(utcDate.getDate()).padStart(2, '0');
            const hours = String(utcDate.getHours()).padStart(2, '0');
            const minutes = String(utcDate.getMinutes()).padStart(2, '0');
            localScheduledAt = `${year}-${month}-${day}T${hours}:${minutes}`;
          }
          
          setFormData(prev => ({
            ...prev,
            name: data.name || prev.name,
            subject: data.subject || prev.subject,
            preheader: data.preview_text || data.preheader || '',
            from_name: data.from_name || prev.from_name,
            from_email: data.from_email || prev.from_email,
            reply_to: data.reply_email || '',
            html_content: data.html_content || '',
            scheduled_at: localScheduledAt,
          }));
          
          // Set sending profile
          if (data.sending_profile_id) {
            setSelectedProfile(data.sending_profile_id);
          }
          
          // Set selected segments (from segment_ids array or fallback to segment_id)
          const campaignSegmentIds = data.segment_ids || (data.segment_id ? [data.segment_id] : []);
          if (campaignSegmentIds.length > 0) {
            setSelectedSegments(campaignSegmentIds);
          }
          
          // Set selected suppression lists
          const globalSuppressions = suppressionOptions.filter(s => s.isLocked).map(s => s.id);
          const campaignSuppressionListIds = data.suppression_list_ids || [];
          // Merge global suppressions with campaign-specific ones
          const allSuppressions = [...new Set([...globalSuppressions, ...campaignSuppressionListIds])];
          setSelectedSuppressions(allSuppressions);
          
          // Set suppression segments
          if (data.suppression_segment_ids && data.suppression_segment_ids.length > 0) {
            setSelectedSuppressionSegments(data.suppression_segment_ids);
          }
          
          // Load campaign objective
          try {
            const objectiveRes = await orgFetch(`${API_BASE}/campaigns/${campaign.id}/objective`, organization?.id);
            if (objectiveRes.ok) {
              const objectiveData = await objectiveRes.json();
              if (objectiveData && objectiveData.purpose) {
                setObjective(objectiveData);
              }
            }
          } catch (objErr) {
            console.log('No objective found for campaign, using defaults');
          }
        } else {
          // New campaign - set defaults
          const globalSuppressions = suppressionOptions.filter(s => s.isLocked).map(s => s.id);
          setSelectedSuppressions(globalSuppressions);
          
          if (profilesData.profiles?.length > 0) {
            const defaultProfile = profilesData.profiles.find((p: any) => p.is_default) || profilesData.profiles[0];
            setSelectedProfile(defaultProfile.id);
            setFormData(prev => ({
              ...prev,
              from_name: defaultProfile.from_name,
              from_email: defaultProfile.from_email,
            }));
          }
        }
      } catch (err) {
        console.error('Failed to load editor data:', err);
      }
    };
    
    loadData();
  }, [isEditing, campaign, organization]);

  // Calculate estimated reach when segments change
  useEffect(() => {
    const total = segments
      .filter(s => selectedSegments.includes(s.id))
      .reduce((sum, s) => sum + (s.count || 0), 0);
    setEstimatedReach(total);
  }, [selectedSegments, segments]);

  const handleProfileChange = (profileId: string) => {
    setSelectedProfile(profileId);
    const profile = profiles.find(p => p.id === profileId);
    if (profile) {
      setFormData(prev => ({
        ...prev,
        from_name: profile.from_name,
        from_email: profile.from_email,
      }));
    }
  };

  const handleSubmit = async () => {
    if (!formData.name || !formData.subject) {
      alert('Please fill in campaign name and subject');
      return;
    }

    setSaving(true);
    try {
      // Capture Everflow creative metadata if present
      const efCreative = (window as any).__everflowCreative;
      const payload: Record<string, any> = {
        name: formData.name,
        subject: formData.subject,
        preview_text: formData.preheader, // Backend expects preview_text
        html_content: formData.html_content,
        from_name: formData.from_name,
        from_email: formData.from_email,
        reply_to: formData.reply_to,
        sending_profile_id: selectedProfile,
        segment_ids: selectedSegments,
        suppression_list_ids: selectedSuppressions,
        suppression_segment_ids: selectedSuppressionSegments,
        scheduled_at: formData.scheduled_at ? new Date(formData.scheduled_at).toISOString() : null,
      };
      // Include Everflow creative info in campaign payload
      if (efCreative) {
        payload.everflow_creative_id = efCreative.creativeId;
        payload.everflow_offer_id = efCreative.offerId;
        payload.tracking_link_template = efCreative.trackingLink;
      }

      let res;
      let campaignId = campaign?.id;
      
      if (isEditing && campaign) {
        res = await orgFetch(`${API_BASE}/campaigns/${campaign.id}`, organization?.id, {
          method: 'PUT',
          body: JSON.stringify(payload),
        });
      } else {
        res = await orgFetch(`${API_BASE}/campaigns`, organization?.id, {
          method: 'POST',
          body: JSON.stringify(payload),
        });
      }

      if (res.ok) {
        // Get the campaign ID from response for new campaigns
        if (!isEditing) {
          const newCampaign = await res.json();
          campaignId = newCampaign.id;
        }
        
        // Save the campaign objective
        if (campaignId && objective.purpose) {
          try {
            const objectiveMethod = isEditing ? 'PUT' : 'POST';
            await orgFetch(`${API_BASE}/campaigns/${campaignId}/objective`, organization?.id, {
              method: objectiveMethod,
              body: JSON.stringify(objective),
            });
          } catch (objErr) {
            console.error('Failed to save campaign objective:', objErr);
            // Don't fail the whole save for objective errors
          }
        }
        
        onSave();
      } else {
        const error = await res.json();
        alert(`Failed to save campaign: ${error.error || 'Unknown error'}`);
      }
    } catch (err) {
      console.error('Failed to save campaign:', err);
      alert('Failed to save campaign');
    } finally {
      setSaving(false);
    }
  };

  const handleSendTest = async () => {
    if (!testEmails.trim()) return;
    
    setSendingTest(true);
    setTestResult(null);
    
    // Parse email addresses (comma or newline separated)
    const emailList = testEmails
      .split(/[,\n]/)
      .map(e => e.trim())
      .filter(e => e && e.includes('@'));
    
    if (emailList.length === 0) {
      setTestResult({ success: false, error: 'Please enter valid email addresses' });
      setSendingTest(false);
      return;
    }

    try {
      // Send test emails one by one
      const results: TestEmailResult[] = [];
      for (const email of emailList) {
        const response = await orgFetch('/api/mailing/send-test', organization?.id, {
          method: 'POST',
          body: JSON.stringify({
            to: email,
            subject: `${testSubjectPrefix}${formData.subject}`,
            from_name: formData.from_name,
            from_email: formData.from_email,
            html_content: formData.html_content,
            sending_profile_id: selectedProfile || undefined,
            preheader: formData.preheader,
          }),
        });

        const data = await response.json();
        results.push({ ...data, to: email });
      }
      
      // Show result for last email sent
      const lastResult = results[results.length - 1];
      setTestResult(lastResult);
      
      // Add successful sends to history
      const successful = results.filter(r => r.success);
      if (successful.length > 0) {
        setTestHistory(prev => [...successful, ...prev].slice(0, 10));
      }
    } catch (err) {
      setTestResult({ success: false, error: 'Network error - please try again' });
    } finally {
      setSendingTest(false);
    }
  };

  const openTestModal = () => {
    setTestResult(null);
    setShowTestModal(true);
  };

  const goToStep = (step: EditorStep) => setCurrentStep(step);
  const nextStep = () => {
    const currentIndex = steps.findIndex(s => s.id === currentStep);
    if (currentIndex < steps.length - 1) {
      setCurrentStep(steps[currentIndex + 1].id);
    }
  };
  const prevStep = () => {
    const currentIndex = steps.findIndex(s => s.id === currentStep);
    if (currentIndex > 0) {
      setCurrentStep(steps[currentIndex - 1].id);
    }
  };

  const isStepComplete = (step: EditorStep): boolean => {
    switch (step) {
      case 'purpose':
        return !!objective.purpose; // Always has a default value
      case 'details':
        return !!(formData.name && formData.subject && selectedProfile);
      case 'audience':
        return selectedSegments.length > 0;
      case 'content':
        return !!formData.html_content;
      case 'schedule':
        return true; // Schedule is optional
      case 'review':
        return true;
      default:
        return false;
    }
  };

  return (
    <div className="campaign-builder">
      {/* Header */}
      <div className="cb-header">
        <div className="cb-header-left">
          <button className="cb-back-btn" onClick={onCancel} aria-label="Close campaign editor">
            <FontAwesomeIcon icon={faTimes} />
          </button>
          <div className="cb-title-area">
            <h1>{isEditing ? 'Edit Campaign' : 'Create Campaign'}</h1>
            {formData.name && <span className="cb-campaign-name">{formData.name}</span>}
          </div>
        </div>
        <div className="cb-header-actions">
          <button className="cb-btn cb-btn-secondary" onClick={handleSendTest} disabled={sendingTest || !formData.html_content}>
            <FontAwesomeIcon icon={sendingTest ? faSpinner : faPaperPlane} spin={sendingTest} />
            Send Test
          </button>
          <button className="cb-btn cb-btn-primary" onClick={handleSubmit} disabled={saving}>
            <FontAwesomeIcon icon={saving ? faSpinner : faSave} spin={saving} />
            {saving ? 'Saving...' : 'Save Campaign'}
          </button>
        </div>
      </div>

      {/* Progress Steps (ARIA-accessible) */}
      <nav aria-label="Campaign wizard steps" className="cb-steps-nav">
        <ol role="tablist" className="cb-steps">
          {steps.map((step, index) => {
            const isActive = currentStep === step.id;
            const isComplete = isStepComplete(step.id);
            return (
              <li key={step.id} role="presentation">
                <button
                  role="tab"
                  id={`tab-${step.id}`}
                  aria-selected={isActive}
                  aria-current={isActive ? 'step' : undefined}
                  aria-controls={`panel-${step.id}`}
                  aria-label={`Step ${index + 1}: ${step.label}${isComplete ? ' (complete)' : ''}`}
                  className={`cb-step ${isActive ? 'active' : ''} ${isComplete ? 'complete' : ''}`}
                  onClick={() => goToStep(step.id)}
                >
                  <span className="cb-step-number">
                    {isComplete && !isActive ? (
                      <FontAwesomeIcon icon={faCheckCircle} />
                    ) : (
                      index + 1
                    )}
                  </span>
                  <span className="cb-step-label">{step.label}</span>
                </button>
              </li>
            );
          })}
        </ol>
      </nav>

      {/* Content Area */}
      <div className="cb-content" role="tabpanel" id={`panel-${currentStep}`} aria-labelledby={`tab-${currentStep}`}>
        {/* Step 1: Purpose — AI Agent Wizard or Classic Mode */}
        {currentStep === 'purpose' && (
          <div className="cb-step-content">
            <div style={{ display: 'flex', justifyContent: 'flex-end', marginBottom: 12, gap: 8 }}>
              <button
                onClick={() => setUseAgentWizard(true)}
                className={`cb-mode-btn ${useAgentWizard ? 'active' : ''}`}
                style={{
                  padding: '6px 14px', borderRadius: 8, border: useAgentWizard ? '1px solid #8b5cf6' : '1px solid #333',
                  background: useAgentWizard ? 'rgba(139,92,246,0.15)' : 'transparent', color: useAgentWizard ? '#a78bfa' : '#888',
                  cursor: 'pointer', fontSize: 12, fontWeight: 600
                }}
              >
                🤖 AI Agent Wizard
              </button>
              <button
                onClick={() => setUseAgentWizard(false)}
                className={`cb-mode-btn ${!useAgentWizard ? 'active' : ''}`}
                style={{
                  padding: '6px 14px', borderRadius: 8, border: !useAgentWizard ? '1px solid #3b82f6' : '1px solid #333',
                  background: !useAgentWizard ? 'rgba(59,130,246,0.15)' : 'transparent', color: !useAgentWizard ? '#60a5fa' : '#888',
                  cursor: 'pointer', fontSize: 12, fontWeight: 600
                }}
              >
                📋 Classic Mode
              </button>
            </div>
            {useAgentWizard ? (
              <AgentConfigWizard
                initialOffer={initialOffer}
                onOfferConsumed={onOfferConsumed}
                onComplete={(config) => {
                  // When agent wizard completes, pre-fill form data and advance
                  if (config?.offer_name) {
                    setFormData(prev => ({ ...prev, name: `AI Campaign: ${config.offer_name}` }));
                  }
                  setCurrentStep('content');
                }}
                onCancel={() => setUseAgentWizard(false)}
              />
            ) : (
              <CampaignPurposeTab
                objective={objective}
                onChange={setObjective}
              />
            )}
          </div>
        )}

        {/* Step 2: Details */}
        {currentStep === 'details' && (
          <div className="cb-step-content">
            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faFileAlt} /> Campaign Details</h2>
              <div className="cb-form-grid">
                <div className="cb-form-group">
                  <label htmlFor="cb-campaign-name">Campaign Name <span className="required" aria-hidden="true">*</span></label>
                  <input
                    id="cb-campaign-name"
                    type="text"
                    value={formData.name}
                    onChange={e => setFormData(prev => ({ ...prev, name: e.target.value }))}
                    placeholder="e.g., February Newsletter"
                    className="cb-input"
                    aria-required="true"
                  />
                  {!formData.name.trim() && <small className="cb-field-error" role="alert">Campaign name is required</small>}
                </div>
                <div className="cb-form-group">
                  <PersonalizedInput
                    label="Subject Line"
                    required
                    value={formData.subject}
                    onChange={value => setFormData(prev => ({ ...prev, subject: value }))}
                    placeholder="e.g., Hey {{ first_name }}, Your Weekly Update is Here!"
                    maxLength={SUBJECT_MAX_LENGTH}
                    showAISuggestions={true}
                    aiContext={{
                      htmlContent: formData.html_content,
                      campaignType: 'newsletter',
                      tone: 'professional',
                    }}
                  />
                </div>
                <div className="cb-form-group full-width">
                  <PersonalizedInput
                    label="Preview Text (Preheader)"
                    value={formData.preheader}
                    onChange={value => setFormData(prev => ({ ...prev, preheader: value }))}
                    placeholder="e.g., {{ first_name }}, see what's new this week..."
                    maxLength={PREHEADER_MAX_LENGTH}
                    hint="This text appears after the subject line in most email clients"
                    showAISuggestions={true}
                    aiSuggestionType="preheader"
                    aiContext={{
                      subjectLine: formData.subject,
                      htmlContent: formData.html_content,
                      tone: 'professional',
                    }}
                  />
                </div>
              </div>
            </div>

            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faPaperPlane} /> Sending Profile</h2>
              <div className="cb-form-grid">
                <div className="cb-form-group">
                  <label>ESP Profile <span className="required">*</span></label>
                  <select value={selectedProfile} onChange={e => handleProfileChange(e.target.value)} className="cb-select">
                    <option value="">Select a sending profile</option>
                    {profiles.map(p => (
                      <option key={p.id} value={p.id}>
                        {p.name} ({p.vendor_type})
                      </option>
                    ))}
                  </select>
                </div>
                <div className="cb-form-group">
                  <label>From Name</label>
                  <input
                    type="text"
                    value={formData.from_name}
                    onChange={e => setFormData(prev => ({ ...prev, from_name: e.target.value }))}
                    placeholder="Sender name"
                    className="cb-input"
                  />
                </div>
                <div className="cb-form-group">
                  <label>From Email</label>
                  <input
                    type="email"
                    value={formData.from_email}
                    onChange={e => setFormData(prev => ({ ...prev, from_email: e.target.value }))}
                    placeholder="sender@example.com"
                    className="cb-input"
                  />
                </div>
                <div className="cb-form-group">
                  <label>Reply-To Email (optional)</label>
                  <input
                    type="email"
                    value={formData.reply_to}
                    onChange={e => setFormData(prev => ({ ...prev, reply_to: e.target.value }))}
                    placeholder="reply@example.com"
                    className="cb-input"
                  />
                </div>
              </div>
            </div>
          </div>
        )}

        {/* Step 2: Audience */}
        {currentStep === 'audience' && (
          <div className="cb-step-content">
            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faUsers} /> Target Audience</h2>
              <p className="cb-section-desc">Select one or more segments to send this campaign to. Segments can span multiple lists.</p>
              
              <SearchableMultiSelect
                options={segments}
                selected={selectedSegments}
                onChange={setSelectedSegments}
                label="Target Segments"
                type="segment"
                placeholder="Search segments by name..."
                emptyMessage="No segments available. Create segments in the Segments section."
                showEstimate={true}
              />
            </div>

            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faBan} /> Suppression Lists</h2>
              <p className="cb-section-desc">Contacts on these lists will be excluded from this send. Global suppressions are always applied.</p>
              
              <SearchableMultiSelect
                options={suppressionLists}
                selected={selectedSuppressions}
                onChange={setSelectedSuppressions}
                label="Suppression Lists"
                type="suppression"
                placeholder="Search suppression lists..."
                emptyMessage="No suppression lists available."
                showEstimate={true}
              />
            </div>

            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faBan} /> Suppression Segments</h2>
              <p className="cb-section-desc">Contacts matching these segment conditions will be excluded. Use this to exclude audiences previously targeted or with specific attributes.</p>
              
              <SearchableMultiSelect
                options={segments.map(s => ({ ...s, type: 'suppression' as const }))}
                selected={selectedSuppressionSegments}
                onChange={setSelectedSuppressionSegments}
                label="Suppression Segments"
                type="suppression"
                placeholder="Search segments to exclude..."
                emptyMessage="No segments available."
                showEstimate={true}
              />
              {selectedSuppressionSegments.length > 0 && (
                <div className="cb-suppression-warning">
                  <FontAwesomeIcon icon={faBan} />
                  <span>{selectedSuppressionSegments.length} segment(s) will be excluded from this send</span>
                </div>
              )}
            </div>

            {estimatedReach > 0 && (
              <div className="cb-audience-summary">
                <FontAwesomeIcon icon={faUsers} />
                <div>
                  <strong>Estimated Reach</strong>
                  <span>{estimatedReach.toLocaleString()} subscribers</span>
                </div>
              </div>
            )}
          </div>
        )}

        {/* Step 3: Content */}
        {currentStep === 'content' && (
          <div className="cb-step-content cb-content-step">
            <div className="cb-section full-height">
              <h2><FontAwesomeIcon icon={faEnvelope} /> Email Content</h2>
              
              {/* Everflow Creative Selector - Pull creatives from network */}
              <EverflowCreativeSelector
                organizationId={organization?.id}
                onCreativeSelect={(html, creativeId, offerId, trackingLink) => {
                  setFormData(prev => ({ ...prev, html_content: html }));
                  setCreativeMode(true);
                  setCreativeCodeView(false);
                  // Store the everflow creative metadata for campaign save
                  (window as any).__everflowCreative = { creativeId, offerId, trackingLink };
                }}
              />

              {/* Creative Mode: Raw HTML preview + code editor (bypasses TipTap) */}
              {creativeMode ? (
                <div className="cb-creative-content">
                  <style>{`
                    .cb-creative-content { border: 1px solid rgba(255,255,255,0.1); border-radius: 10px; overflow: hidden; background: #1a1a2e; }
                    .cb-creative-toolbar { display: flex; align-items: center; gap: 10px; padding: 10px 14px; border-bottom: 1px solid rgba(255,255,255,0.1); background: rgba(255,255,255,0.03); }
                    .cb-creative-toolbar .cb-ct-badge { background: #27ae60; color: white; padding: 3px 10px; border-radius: 6px; font-size: 11px; font-weight: 600; display: flex; align-items: center; gap: 5px; }
                    .cb-creative-toolbar .cb-ct-label { color: rgba(255,255,255,0.5); font-size: 12px; flex: 1; }
                    .cb-creative-toolbar button { background: rgba(255,255,255,0.08); border: 1px solid rgba(255,255,255,0.15); color: #e0e0e0; padding: 5px 12px; border-radius: 6px; cursor: pointer; font-size: 12px; }
                    .cb-creative-toolbar button:hover { background: rgba(255,255,255,0.12); }
                    .cb-creative-toolbar button.active { background: #6c5ce7; border-color: #6c5ce7; color: white; }
                    .cb-creative-toolbar .cb-ct-switch { background: rgba(231,76,60,0.15); border-color: rgba(231,76,60,0.3); color: #e74c3c; }
                    .cb-creative-toolbar .cb-ct-switch:hover { background: rgba(231,76,60,0.25); }
                    .cb-creative-preview iframe { width: 100%; height: 600px; border: none; background: white; }
                    .cb-creative-code textarea { width: 100%; height: 500px; background: #0d1117; color: #c9d1d9; border: none; padding: 14px; font-family: 'Fira Code', 'Consolas', monospace; font-size: 13px; line-height: 1.5; resize: vertical; outline: none; }
                  `}</style>
                  <div className="cb-creative-toolbar">
                    <span className="cb-ct-badge">
                      <FontAwesomeIcon icon={faCheckCircle} /> Everflow Creative Applied
                    </span>
                    <span className="cb-ct-label">
                      Raw email HTML preserved with tracking links intact
                    </span>
                    <button
                      className={!creativeCodeView ? 'active' : ''}
                      onClick={() => setCreativeCodeView(false)}
                    >
                      <FontAwesomeIcon icon={faEye} /> Preview
                    </button>
                    <button
                      className={creativeCodeView ? 'active' : ''}
                      onClick={() => setCreativeCodeView(true)}
                    >
                      <FontAwesomeIcon icon={faCode} /> HTML Code
                    </button>
                    <button
                      className="cb-ct-switch"
                      onClick={() => setCreativeMode(false)}
                    >
                      Switch to WYSIWYG
                    </button>
                  </div>
                  {!creativeCodeView ? (
                    <div className="cb-creative-preview">
                      <iframe
                        srcDoc={formData.html_content}
                        title="Creative Preview"
                        sandbox="allow-same-origin"
                      />
                    </div>
                  ) : (
                    <div className="cb-creative-code">
                      <textarea
                        value={formData.html_content}
                        onChange={(e) => setFormData(prev => ({ ...prev, html_content: e.target.value }))}
                        spellCheck={false}
                      />
                    </div>
                  )}
                </div>
              ) : (
                <EmailEditor
                  content={formData.html_content}
                  onChange={(content) => setFormData(prev => ({ ...prev, html_content: content }))}
                  placeholder="Start designing your email..."
                  minHeight={500}
                />
              )}
            </div>
          </div>
        )}

        {/* Step 4: Schedule */}
        {currentStep === 'schedule' && (
          <div className="cb-step-content">
            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faCalendarAlt} /> Schedule Send</h2>
              <p className="cb-section-desc">Choose when to send this campaign. Leave empty to save as draft.</p>
              
              <div className="cb-schedule-options">
                <label className="cb-schedule-option">
                  <input
                    type="radio"
                    name="schedule"
                    checked={!formData.scheduled_at}
                    onChange={() => setFormData(prev => ({ ...prev, scheduled_at: '' }))}
                  />
                  <div className="cb-schedule-option-content">
                    <FontAwesomeIcon icon={faFileAlt} />
                    <div>
                      <strong>Save as Draft</strong>
                      <span>Save and send later manually</span>
                    </div>
                  </div>
                </label>
                
                <label className="cb-schedule-option">
                  <input
                    type="radio"
                    name="schedule"
                    checked={!!formData.scheduled_at}
                    onChange={() => {
                      const defaultTime = new Date();
                      defaultTime.setHours(defaultTime.getHours() + 1);
                      defaultTime.setMinutes(0);
                      setFormData(prev => ({
                        ...prev,
                        scheduled_at: defaultTime.toISOString().slice(0, 16)
                      }));
                    }}
                  />
                  <div className="cb-schedule-option-content">
                    <FontAwesomeIcon icon={faCalendarAlt} />
                    <div>
                      <strong>Schedule for Later</strong>
                      <span>Set a specific date and time</span>
                    </div>
                  </div>
                </label>
              </div>

              {formData.scheduled_at && (
                <div className="cb-schedule-picker">
                  <label htmlFor="cb-send-datetime">Send Date & Time <span className="required" aria-hidden="true">*</span></label>
                  <input
                    id="cb-send-datetime"
                    type="datetime-local"
                    value={formData.scheduled_at}
                    onChange={e => setFormData(prev => ({ ...prev, scheduled_at: e.target.value }))}
                    className="cb-input cb-datetime"
                    min={new Date(Date.now() + 6 * 60000).toISOString().slice(0, 16)}
                    aria-required="true"
                  />
                  {formData.scheduled_at && new Date(formData.scheduled_at) < new Date() && (
                    <small className="cb-field-error" role="alert">
                      <FontAwesomeIcon icon={faExclamationCircle} /> Scheduled time must be in the future
                    </small>
                  )}
                  <small className="cb-hint">
                    <FontAwesomeIcon icon={faInfoCircle} /> Campaigns must be scheduled at least 5 minutes in advance
                  </small>
                </div>
              )}
            </div>

            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faTachometerAlt} /> Send Speed</h2>
              <p className="cb-section-desc">Control how quickly your campaign is delivered. Slower speeds are better for ISP reputation.</p>
              <div className="cb-throttle-grid">
                {[
                  { value: 'instant', label: 'Instant', rate: 'Full speed' },
                  { value: 'gentle', label: 'Gentle', rate: '~500/min' },
                  { value: 'moderate', label: 'Moderate', rate: '~250/min' },
                  { value: 'careful', label: 'Careful', rate: '~125/min' },
                ].map(opt => (
                  <button
                    key={opt.value}
                    type="button"
                    className={`cb-throttle-option ${formData.throttle_speed === opt.value ? 'active' : ''}`}
                    onClick={() => setFormData(prev => ({ ...prev, throttle_speed: opt.value }))}
                    aria-pressed={formData.throttle_speed === opt.value}
                  >
                    <div className="throttle-label">{opt.label}</div>
                    <div className="throttle-rate">{opt.rate}</div>
                  </button>
                ))}
              </div>
              {estimatedReach > 0 && (
                <div className="cb-send-estimate">
                  <FontAwesomeIcon icon={faInfoCircle} />
                  Estimated send duration: <strong>
                    {(() => {
                      const rate = formData.throttle_speed === 'gentle' ? 500 :
                        formData.throttle_speed === 'moderate' ? 250 :
                        formData.throttle_speed === 'careful' ? 125 : 50000;
                      const minutes = Math.ceil(estimatedReach / rate);
                      if (minutes < 60) return `~${minutes} min`;
                      const h = Math.floor(minutes / 60);
                      const m = minutes % 60;
                      return `~${h}h ${m}m`;
                    })()}
                  </strong> for {estimatedReach.toLocaleString()} recipients
                </div>
              )}
            </div>
          </div>
        )}

        {/* Step 5: Review */}
        {currentStep === 'review' && (
          <div className="cb-step-content">
            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faCheckCircle} /> Review Campaign</h2>
              <p className="cb-section-desc">Review your campaign settings before saving.</p>
              
              <div className="cb-review-grid">
                <div className="cb-review-card cb-review-purpose">
                  <h3><FontAwesomeIcon icon={faBullseye} /> Campaign Purpose</h3>
                  <dl>
                    <dt>Purpose</dt>
                    <dd>
                      {objective.purpose === 'data_activation' 
                        ? 'Data Activation'
                        : 'Offer Revenue'
                      }
                    </dd>
                    {objective.purpose === 'data_activation' && (
                      <>
                        <dt>Goal</dt>
                        <dd>
                          {objective.activation_goal === 'warm_new_data' && 'Warm New Data'}
                          {objective.activation_goal === 'reactivate_cold' && 'Reactivate Cold'}
                          {objective.activation_goal === 'domain_warmup' && 'Domain Warmup'}
                          {!objective.activation_goal && <em>Not set</em>}
                        </dd>
                      </>
                    )}
                    {objective.purpose === 'offer_revenue' && (
                      <>
                        <dt>Model</dt>
                        <dd>{objective.offer_model?.toUpperCase() || <em>Not set</em>}</dd>
                        {objective.budget_limit && (
                          <>
                            <dt>Budget</dt>
                            <dd>${objective.budget_limit.toLocaleString()}</dd>
                          </>
                        )}
                        {objective.ecpm_target && (
                          <>
                            <dt>ECM Target</dt>
                            <dd>${objective.ecpm_target.toFixed(2)}</dd>
                          </>
                        )}
                      </>
                    )}
                    <dt>AI Optimization</dt>
                    <dd>
                      {objective.ai_optimization_enabled || objective.ai_throughput_optimization 
                        ? <span className="cb-status-good"><FontAwesomeIcon icon={faCheckCircle} /> Enabled</span>
                        : <span className="cb-status-warn">Disabled</span>
                      }
                    </dd>
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('purpose')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>

                <div className="cb-review-card">
                  <h3>Campaign Details</h3>
                  <dl>
                    <dt>Name</dt>
                    <dd>{formData.name || <em>Not set</em>}</dd>
                    <dt>Subject</dt>
                    <dd>{formData.subject || <em>Not set</em>}</dd>
                    <dt>Preview Text</dt>
                    <dd>{formData.preheader || <em>Not set</em>}</dd>
                    <dt>From</dt>
                    <dd>{formData.from_name} &lt;{formData.from_email}&gt;</dd>
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('details')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>

                <div className="cb-review-card">
                  <h3>Audience</h3>
                  <dl>
                    <dt>Segments</dt>
                    <dd>
                      {selectedSegments.length > 0 
                        ? segments.filter(s => selectedSegments.includes(s.id)).map(s => s.name).join(', ')
                        : <em>No segments selected</em>
                      }
                    </dd>
                    <dt>Estimated Reach</dt>
                    <dd>{estimatedReach.toLocaleString()} subscribers</dd>
                    <dt>Suppression Lists</dt>
                    <dd>{selectedSuppressions.length} list(s) applied</dd>
                    <dt>Suppression Segments</dt>
                    <dd>{selectedSuppressionSegments.length} segment(s) excluded</dd>
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('audience')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>

                <div className="cb-review-card">
                  <h3>Content</h3>
                  <dl>
                    <dt>Status</dt>
                    <dd>
                      {formData.html_content ? (
                        <span className="cb-status-good"><FontAwesomeIcon icon={faCheckCircle} /> Content ready</span>
                      ) : (
                        <span className="cb-status-warn"><FontAwesomeIcon icon={faExclamationTriangle} /> No content</span>
                      )}
                    </dd>
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('content')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>

                <div className="cb-review-card">
                  <h3>Schedule & Delivery</h3>
                  <dl>
                    <dt>Send Time</dt>
                    <dd>
                      {formData.scheduled_at 
                        ? new Date(formData.scheduled_at).toLocaleString()
                        : 'Save as draft'
                      }
                    </dd>
                    <dt>Send Speed</dt>
                    <dd style={{ textTransform: 'capitalize' }}>{formData.throttle_speed || 'Instant'}</dd>
                    {estimatedReach > 0 && (
                      <>
                        <dt>Estimated Duration</dt>
                        <dd>
                          {(() => {
                            const rate = formData.throttle_speed === 'gentle' ? 500 :
                              formData.throttle_speed === 'moderate' ? 250 :
                              formData.throttle_speed === 'careful' ? 125 : 50000;
                            const minutes = Math.ceil(estimatedReach / rate);
                            if (minutes < 60) return `~${minutes} min`;
                            const h = Math.floor(minutes / 60);
                            const m = minutes % 60;
                            return `~${h}h ${m}m`;
                          })()}
                        </dd>
                      </>
                    )}
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('schedule')} aria-label="Edit schedule">
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>
              </div>
            </div>
          </div>
        )}
      </div>

      {/* Footer Navigation */}
      <div className="cb-footer" role="navigation" aria-label="Campaign step navigation">
        <button 
          className="cb-btn cb-btn-secondary" 
          onClick={prevStep}
          disabled={currentStep === 'details'}
          aria-label="Go to previous step"
        >
          <FontAwesomeIcon icon={faArrowUp} style={{ transform: 'rotate(-90deg)' }} /> Previous
        </button>
        <div className="cb-footer-center">
          <button 
            className="cb-btn cb-btn-outline" 
            onClick={openTestModal}
            disabled={!formData.html_content || !formData.subject}
            title={!formData.html_content ? 'Add email content first' : 'Send a test/proof email'}
          >
            <FontAwesomeIcon icon={faPaperPlane} /> Send Test/Proof
          </button>
          <span className="cb-step-indicator">Step {steps.findIndex(s => s.id === currentStep) + 1} of {steps.length}</span>
        </div>
        {currentStep !== 'review' ? (
          <button className="cb-btn cb-btn-primary" onClick={nextStep} aria-label="Go to next step">
            Next <FontAwesomeIcon icon={faArrowRight} />
          </button>
        ) : (
          <button className="cb-btn cb-btn-success" onClick={handleSubmit} disabled={saving} aria-label={formData.scheduled_at ? 'Schedule campaign' : 'Save draft'}>
            <FontAwesomeIcon icon={saving ? faSpinner : faCheckCircle} spin={saving} />
            {saving ? 'Saving...' : (formData.scheduled_at ? 'Schedule Campaign' : 'Save Draft')}
          </button>
        )}
      </div>

      {/* Test/Proof Email Modal */}
      {showTestModal && (
        <div className="cb-modal-overlay" onClick={() => setShowTestModal(false)} role="dialog" aria-modal="true" aria-label="Send test email">
          <div className="cb-modal cb-test-modal" onClick={e => e.stopPropagation()}>
            <div className="cb-modal-header">
              <h2><FontAwesomeIcon icon={faPaperPlane} /> Send Test/Proof Email</h2>
              <button className="cb-modal-close" onClick={() => setShowTestModal(false)} aria-label="Close test email dialog">
                <FontAwesomeIcon icon={faTimes} />
              </button>
            </div>
            
            <div className="cb-modal-body">
              <p className="cb-modal-desc">
                Send a test email to preview how your campaign will look in real inboxes.
              </p>

              <div className="cb-test-form">
                <div className="cb-form-group">
                  <label>
                    <FontAwesomeIcon icon={faEnvelope} /> Recipient Email(s)
                    <span className="cb-label-hint">Separate multiple emails with commas</span>
                  </label>
                  <textarea
                    className="cb-input cb-textarea"
                    value={testEmails}
                    onChange={e => setTestEmails(e.target.value)}
                    placeholder="Enter email addresses...&#10;example@gmail.com, colleague@company.com"
                    rows={3}
                  />
                </div>

                <div className="cb-form-group">
                  <label>
                    <FontAwesomeIcon icon={faFileAlt} /> Subject Line Prefix
                    <span className="cb-label-hint">Added before your subject to identify test emails</span>
                  </label>
                  <input
                    type="text"
                    className="cb-input"
                    value={testSubjectPrefix}
                    onChange={e => setTestSubjectPrefix(e.target.value)}
                    placeholder="[TEST] "
                  />
                </div>

                <div className="cb-test-preview">
                  <h4>Preview</h4>
                  <div className="cb-preview-item">
                    <span className="cb-preview-label">Subject:</span>
                    <span className="cb-preview-value">{testSubjectPrefix}{formData.subject}</span>
                  </div>
                  <div className="cb-preview-item">
                    <span className="cb-preview-label">From:</span>
                    <span className="cb-preview-value">{formData.from_name} &lt;{formData.from_email}&gt;</span>
                  </div>
                  <div className="cb-preview-item">
                    <span className="cb-preview-label">Via:</span>
                    <span className="cb-preview-value">
                      {profiles.find(p => p.id === selectedProfile)?.name || 'Default Profile'} 
                      ({profiles.find(p => p.id === selectedProfile)?.vendor_type?.toUpperCase() || 'ESP'})
                    </span>
                  </div>
                </div>

                {testResult && (
                  <div className={`cb-test-result ${testResult.success ? 'success' : 'error'}`}>
                    {testResult.success ? (
                      <>
                        <FontAwesomeIcon icon={faCheckCircle} />
                        <div>
                          <strong>Test email sent successfully!</strong>
                          <p>Sent to: {testResult.to}</p>
                          {testResult.message_id && <p className="cb-message-id">ID: {testResult.message_id}</p>}
                        </div>
                      </>
                    ) : (
                      <>
                        <FontAwesomeIcon icon={faTimesCircle} />
                        <div>
                          <strong>Failed to send test email</strong>
                          <p>{testResult.error}</p>
                        </div>
                      </>
                    )}
                  </div>
                )}

                {testHistory.length > 0 && (
                  <div className="cb-test-history">
                    <h4>Recent Test Sends</h4>
                    <ul>
                      {testHistory.map((item, idx) => (
                        <li key={idx}>
                          <FontAwesomeIcon icon={faCheckCircle} className="cb-history-icon" />
                          <span className="cb-history-email">{item.to}</span>
                          <span className="cb-history-time">
                            {item.sent_at ? new Date(item.sent_at).toLocaleTimeString() : 'Just now'}
                          </span>
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
              </div>
            </div>

            <div className="cb-modal-footer">
              <button 
                className="cb-btn cb-btn-secondary" 
                onClick={() => setShowTestModal(false)}
              >
                Close
              </button>
              <button 
                className="cb-btn cb-btn-primary" 
                onClick={handleSendTest}
                disabled={sendingTest || !testEmails.trim()}
              >
                <FontAwesomeIcon icon={sendingTest ? faSpinner : faPaperPlane} spin={sendingTest} />
                {sendingTest ? 'Sending...' : 'Send Test Email'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// ============================================================================
// MAIN CAMPAIGN PORTAL
// ============================================================================

export const CampaignPortal: React.FC<{
  initialOffer?: { offerId: string; offerName: string } | null;
  onOfferConsumed?: () => void;
}> = ({ initialOffer, onOfferConsumed }) => {
  const { organization } = useAuth();
  const [view, setView] = useState<ViewType>(initialOffer ? 'create' : 'dashboard');

  // When initialOffer changes externally, switch to create view
  useEffect(() => {
    if (initialOffer) {
      setView('create');
    }
  }, [initialOffer]);
  const [campaigns, setCampaigns] = useState<Campaign[]>([]);
  const [dashboardStats, setDashboardStats] = useState<DashboardStats | null>(null);
  const [selectedCampaign, setSelectedCampaign] = useState<Campaign | null>(null);
  const [selectedCampaignStats, setSelectedCampaignStats] = useState<CampaignStats | null>(null);
  const [loading, setLoading] = useState(true);
  const [detailsLoading, setDetailsLoading] = useState(false);
  const [filter, setFilter] = useState<StatusFilter>('all');
  const [search, setSearch] = useState('');

  // Fetch dashboard stats
  const fetchDashboardStats = useCallback(async () => {
    setLoading(true);
    try {
      // Fetch campaigns
      const campaignsRes = await orgFetch(`${API_BASE}/campaigns`, organization?.id);
      const campaignsData = await campaignsRes.json();
      const allCampaigns: Campaign[] = campaignsData.campaigns || [];

      // Calculate stats
      const stats: DashboardStats = {
        total_campaigns: allCampaigns.length,
        draft_count: allCampaigns.filter(c => c.status === 'draft').length,
        scheduled_count: allCampaigns.filter(c => c.status === 'scheduled').length,
        sending_count: allCampaigns.filter(c => c.status === 'sending').length,
        completed_count: allCampaigns.filter(c => c.status === 'completed' || c.status === 'completed_with_errors').length,
        total_sent: allCampaigns.reduce((sum, c) => sum + (c.sent_count || 0), 0),
        total_opens: allCampaigns.reduce((sum, c) => sum + (c.open_count || 0), 0),
        total_clicks: allCampaigns.reduce((sum, c) => sum + (c.click_count || 0), 0),
        total_bounces: allCampaigns.reduce((sum, c) => sum + (c.bounce_count || 0), 0),
        total_complaints: allCampaigns.reduce((sum, c) => sum + (c.complaint_count || 0), 0),
        total_unsubscribes: allCampaigns.reduce((sum, c) => sum + (c.unsubscribe_count || 0), 0),
        avg_open_rate: 0,
        avg_click_rate: 0,
        avg_bounce_rate: 0,
        avg_complaint_rate: 0,
        total_revenue: allCampaigns.reduce((sum, c) => sum + (c.revenue || 0), 0),
        recent_campaigns: [...allCampaigns].sort((a, b) => 
          new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
        ).slice(0, 10),
        scheduled_campaigns: allCampaigns
          .filter(c => c.status === 'scheduled')
          .sort((a, b) => {
            const aDate = a.scheduled_at ? new Date(a.scheduled_at).getTime() : 0;
            const bDate = b.scheduled_at ? new Date(b.scheduled_at).getTime() : 0;
            return aDate - bDate;
          }),
        top_campaigns: allCampaigns
          .filter(c => c.status === 'completed' && c.sent_count > 0)
          .sort((a, b) => {
            const aRate = a.sent_count > 0 ? a.open_count / a.sent_count : 0;
            const bRate = b.sent_count > 0 ? b.open_count / b.sent_count : 0;
            return bRate - aRate;
          })
          .slice(0, 5),
      };

      // Calculate average rates
      const completedWithSent = allCampaigns.filter(c => c.sent_count > 0);
      if (completedWithSent.length > 0) {
        stats.avg_open_rate = (stats.total_opens / stats.total_sent) * 100;
        stats.avg_click_rate = (stats.total_clicks / stats.total_sent) * 100;
        stats.avg_bounce_rate = (stats.total_bounces / stats.total_sent) * 100;
        stats.avg_complaint_rate = (stats.total_complaints / stats.total_sent) * 100;
      }

      setDashboardStats(stats);
      setCampaigns(allCampaigns);
    } catch (err) {
      console.error('Failed to fetch dashboard stats:', err);
    } finally {
      setLoading(false);
    }
  }, []);

  // Fetch campaign details
  const fetchCampaignDetails = useCallback(async (id: string) => {
    setDetailsLoading(true);
    try {
      const [campaignRes, statsRes] = await Promise.all([
        orgFetch(`${API_BASE}/campaigns/${id}`, organization?.id),
        orgFetch(`${API_BASE}/campaigns/${id}/stats`, organization?.id),
      ]);
      const campaign = await campaignRes.json();
      const stats = await statsRes.json();
      setSelectedCampaign(campaign);
      setSelectedCampaignStats(stats);
    } catch (err) {
      console.error('Failed to fetch campaign details:', err);
    } finally {
      setDetailsLoading(false);
    }
  }, [organization]);

  // Handle campaign action
  const handleAction = useCallback(async (id: string, action: string) => {
    try {
      if (action === 'edit') {
        // Load campaign and switch to edit view
        const res = await orgFetch(`${API_BASE}/campaigns/${id}`, organization?.id);
        const campaign = await res.json();
        setSelectedCampaign(campaign);
        setView('edit');
        return;
      } else if (action === 'duplicate') {
        await orgFetch(`${API_BASE}/campaigns/${id}/duplicate`, organization?.id, { method: 'POST' });
      } else if (action === 'pause') {
        await orgFetch(`${API_BASE}/campaigns/${id}/pause`, organization?.id, { method: 'POST' });
      } else if (action === 'resume') {
        await orgFetch(`${API_BASE}/campaigns/${id}/resume`, organization?.id, { method: 'POST' });
      } else if (action === 'cancel') {
        await orgFetch(`${API_BASE}/campaigns/${id}/cancel`, organization?.id, { method: 'POST' });
      } else if (action === 'send') {
        await orgFetch(`${API_BASE}/campaigns/${id}/send`, organization?.id, { method: 'POST' });
      }
      // Refresh data
      fetchDashboardStats();
      if (selectedCampaign?.id === id) {
        fetchCampaignDetails(id);
      }
    } catch (err) {
      console.error(`Failed to ${action} campaign:`, err);
    }
  }, [fetchDashboardStats, fetchCampaignDetails, selectedCampaign, organization]);

  // Initial load
  useEffect(() => {
    fetchDashboardStats();
  }, [fetchDashboardStats]);
  
  // Auto-refresh for sending/scheduled campaigns (separate effect to avoid dependency loop)
  useEffect(() => {
    const hasSendingCampaigns = campaigns.some(c => c.status === 'sending' || c.status === 'scheduled');
    if (!hasSendingCampaigns) return;
    
    const interval = setInterval(() => {
      fetchDashboardStats();
    }, 15000);
    
    return () => clearInterval(interval);
  }, [campaigns.length, fetchDashboardStats]); // Only re-run when campaign count changes

  // Handle view campaign
  const handleViewCampaign = (id: string) => {
    fetchCampaignDetails(id);
    setView('details');
  };

  return (
    <div className="campaign-portal">
      {/* Header */}
      <div className="portal-header">
        <div className="header-title">
          <FontAwesomeIcon icon={faEnvelope} className="header-icon" />
          <div>
            <h1>Campaign Center</h1>
            <p>Create, manage and monitor your email campaigns</p>
          </div>
        </div>
        <div className="header-actions">
          <button 
            className="create-campaign-btn" 
            onClick={() => {
              setSelectedCampaign(null);
              setView('create');
            }}
          >
            <FontAwesomeIcon icon={faPlus} /> New Campaign
          </button>
          <button 
            className="refresh-btn" 
            onClick={fetchDashboardStats}
            disabled={loading}
          >
            <FontAwesomeIcon icon={faSync} spin={loading} /> Refresh
          </button>
        </div>
      </div>

      {/* Navigation Tabs */}
      <div className="portal-nav">
        <button 
          className={`nav-tab ${view === 'dashboard' ? 'active' : ''}`}
          onClick={() => setView('dashboard')}
        >
          <FontAwesomeIcon icon={faTachometerAlt} /> Dashboard
        </button>
        <button 
          className={`nav-tab ${view === 'campaigns' ? 'active' : ''}`}
          onClick={() => setView('campaigns')}
        >
          <FontAwesomeIcon icon={faList} /> All Campaigns
          <span className="nav-count">{campaigns.length}</span>
        </button>
        <button 
          className={`nav-tab ${view === 'scheduled' ? 'active' : ''}`}
          onClick={() => { setView('campaigns'); setFilter('scheduled'); }}
        >
          <FontAwesomeIcon icon={faCalendarCheck} /> Scheduled
          <span className="nav-count">{campaigns.filter(c => c.status === 'scheduled').length}</span>
        </button>
      </div>

      {/* Main Content */}
      <div className="portal-content">
        {view === 'dashboard' && (
          <CampaignDashboard
            stats={dashboardStats}
            loading={loading}
            onViewCampaign={handleViewCampaign}
            onViewAll={() => setView('campaigns')}
            onViewScheduled={() => { setView('campaigns'); setFilter('scheduled'); }}
          />
        )}

        {(view === 'campaigns' || view === 'scheduled') && (
          <CampaignsList
            campaigns={campaigns}
            loading={loading}
            filter={filter}
            search={search}
            onFilterChange={setFilter}
            onSearchChange={setSearch}
            onViewCampaign={handleViewCampaign}
            onAction={handleAction}
            onRefresh={fetchDashboardStats}
          />
        )}

        {(view === 'create' || view === 'edit') && (
          <CampaignEditor
            campaign={view === 'edit' ? selectedCampaign : null}
            initialOffer={view === 'create' ? initialOffer : null}
            onOfferConsumed={onOfferConsumed}
            onSave={() => {
              fetchDashboardStats();
              setView('campaigns');
            }}
            onCancel={() => setView('dashboard')}
          />
        )}
      </div>

      {/* Campaign Details Modal */}
      {view === 'details' && selectedCampaign && (
        <CampaignDetailsModal
          campaign={selectedCampaign}
          stats={selectedCampaignStats}
          loading={detailsLoading}
          onClose={() => setView('dashboard')}
          onAction={handleAction}
        />
      )}
    </div>
  );
};

export default CampaignPortal;
