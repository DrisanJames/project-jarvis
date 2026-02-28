import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faRoute, faChartLine, faUsers, faCheckCircle, faBullseye,
  faPlay, faPause, faEye, faEdit, faTrash, faPlus, faSync,
  faSearch, faArrowUp, faArrowDown, faClock,
  faEnvelope, faMousePointer, faExclamationTriangle,
  faSpinner, faPercentage, faTachometerAlt,
  faArrowRight, faTimes,
  faUserPlus, faFileAlt, faChartBar, faChartPie,
  faEnvelopeOpen, faUserMinus, faInbox, faLayerGroup,
  faSitemap, faCodeBranch, faStopwatch, faFlag
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { JourneyBuilder } from './JourneyBuilder';
import './JourneyCenter.css';

// ============================================================================
// TYPES
// ============================================================================

interface Journey {
  id: string;
  name: string;
  description?: string;
  status: 'draft' | 'active' | 'paused' | 'completed' | 'archived';
  trigger_type: 'schedule' | 'event' | 'performance' | 'api';
  created_at: string;
  updated_at: string;
  activated_at?: string;
  node_count: number;
  total_enrolled: number;
  active_enrolled: number;
  completed_count: number;
  conversion_count: number;
  conversion_rate: number;
  email_count: number;
  total_sent: number;
  total_opens: number;
  total_clicks: number;
  open_rate: number;
  click_rate: number;
  drop_off_rate: number;
  list_name?: string;
  segment_name?: string;
}

interface JourneyNode {
  id: string;
  type: 'trigger' | 'email' | 'delay' | 'condition' | 'split' | 'goal';
  name: string;
  order: number;
  entered: number;
  completed: number;
  dropped: number;
  conversion_rate: number;
  metrics?: {
    sent?: number;
    opens?: number;
    clicks?: number;
    bounces?: number;
    open_rate?: number;
    click_rate?: number;
  };
}

interface JourneyEnrollment {
  id: string;
  subscriber_email: string;
  subscriber_name?: string;
  current_node_id: string;
  current_node_name: string;
  status: 'active' | 'completed' | 'exited' | 'paused';
  enrolled_at: string;
  last_activity_at: string;
  emails_received: number;
  emails_opened: number;
  emails_clicked: number;
}

interface JourneyStats {
  total_journeys: number;
  active_journeys: number;
  draft_journeys: number;
  paused_journeys: number;
  total_enrolled: number;
  active_enrolled: number;
  total_completions: number;
  total_conversions: number;
  avg_completion_rate: number;
  avg_conversion_rate: number;
  total_emails_sent: number;
  avg_open_rate: number;
  avg_click_rate: number;
  top_journeys: Journey[];
  recent_activity: ActivityItem[];
}

interface ActivityItem {
  id: string;
  journey_name: string;
  event_type: 'enrolled' | 'completed' | 'converted' | 'email_sent' | 'email_opened' | 'exited';
  subscriber_email: string;
  timestamp: string;
}

type ViewType = 'overview' | 'list' | 'detail' | 'enrollments' | 'performance' | 'builder';
type StatusFilter = 'all' | 'draft' | 'active' | 'paused' | 'completed' | 'archived';
type TimeRange = '7d' | '30d' | '90d' | 'all';

const API_BASE = '/api/mailing';

// Normalize journey data to ensure all numeric fields have defaults
const normalizeJourney = (j: any): Journey => ({
  id: j.id || '',
  name: j.name || 'Untitled',
  description: j.description || '',
  status: j.status || 'draft',
  trigger_type: j.trigger_type || 'schedule',
  created_at: j.created_at || '',
  updated_at: j.updated_at || '',
  activated_at: j.activated_at,
  node_count: j.node_count || 0,
  total_enrolled: j.total_enrolled || 0,
  active_enrolled: j.active_enrolled || 0,
  completed_count: j.completed_count || 0,
  conversion_count: j.conversion_count || 0,
  conversion_rate: j.conversion_rate || 0,
  email_count: j.email_count || 0,
  total_sent: j.total_sent || 0,
  total_opens: j.total_opens || 0,
  total_clicks: j.total_clicks || 0,
  open_rate: j.open_rate || 0,
  click_rate: j.click_rate || 0,
  drop_off_rate: j.drop_off_rate || 0,
  list_name: j.list_name,
  segment_name: j.segment_name,
});

// ============================================================================
// API HELPER WITH ORGANIZATION CONTEXT
// ============================================================================

const getOrgHeaders = (organizationId: string | undefined): HeadersInit => {
  const headers: HeadersInit = {
    'Content-Type': 'application/json',
  };
  if (organizationId) {
    headers['X-Organization-ID'] = organizationId;
  }
  return headers;
};

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

const StatusBadge: React.FC<{ status: Journey['status'] }> = ({ status }) => {
  const statusConfig: Record<string, { icon: any; label: string; className: string }> = {
    draft: { icon: faFileAlt, label: 'Draft', className: 'jc-status-draft' },
    active: { icon: faPlay, label: 'Active', className: 'jc-status-active' },
    paused: { icon: faPause, label: 'Paused', className: 'jc-status-paused' },
    completed: { icon: faCheckCircle, label: 'Completed', className: 'jc-status-completed' },
    archived: { icon: faInbox, label: 'Archived', className: 'jc-status-archived' },
  };

  const config = statusConfig[status] || statusConfig.draft;

  return (
    <span className={`jc-status-badge ${config.className}`}>
      <FontAwesomeIcon icon={config.icon} />
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
  trendValue?: string;
  color?: string;
}> = ({ icon, label, value, subValue, trend, trendValue, color = 'primary' }) => (
  <div className={`jc-metric-card jc-metric-${color}`}>
    <div className="jc-metric-icon">
      <FontAwesomeIcon icon={icon} />
    </div>
    <div className="jc-metric-content">
      <div className="jc-metric-value">
        {typeof value === 'number' ? value.toLocaleString() : value}
        {trend && (
          <span className={`jc-metric-trend jc-trend-${trend}`}>
            <FontAwesomeIcon icon={trend === 'up' ? faArrowUp : trend === 'down' ? faArrowDown : faArrowRight} />
            {trendValue}
          </span>
        )}
      </div>
      <div className="jc-metric-label">{label}</div>
      {subValue && <div className="jc-metric-sub">{subValue}</div>}
    </div>
  </div>
);

const ProgressBar: React.FC<{ value: number; max: number; color?: string; showLabel?: boolean }> = ({ 
  value, max, color = 'primary', showLabel = false 
}) => {
  const percentage = max > 0 ? (value / max) * 100 : 0;
  return (
    <div className="jc-progress-container">
      <div className="jc-progress-bar">
        <div 
          className={`jc-progress-fill jc-progress-${color}`} 
          style={{ width: `${Math.min(percentage, 100)}%` }} 
        />
      </div>
      {showLabel && (
        <span className="jc-progress-label">{percentage.toFixed(1)}%</span>
      )}
    </div>
  );
};

const FunnelNode: React.FC<{
  node: JourneyNode;
  isFirst: boolean;
  isLast: boolean;
  maxEntered: number;
}> = ({ node, isFirst, maxEntered }) => {
  const nodeTypeIcons: Record<string, any> = {
    trigger: faFlag,
    email: faEnvelope,
    delay: faStopwatch,
    condition: faCodeBranch,
    split: faSitemap,
    goal: faBullseye,
  };

  const nodeTypeColors: Record<string, string> = {
    trigger: '#10b981',
    email: '#3b82f6',
    delay: '#f59e0b',
    condition: '#8b5cf6',
    split: '#ec4899',
    goal: '#14b8a6',
  };

  const widthPercentage = maxEntered > 0 ? (node.entered / maxEntered) * 100 : 100;

  return (
    <div className="jc-funnel-node">
      {!isFirst && <div className="jc-funnel-connector" />}
      <div 
        className="jc-funnel-bar"
        style={{ 
          width: `${Math.max(widthPercentage, 20)}%`,
          background: `linear-gradient(135deg, ${nodeTypeColors[node.type]}dd, ${nodeTypeColors[node.type]})`
        }}
      >
        <div className="jc-funnel-node-content">
          <span className="jc-funnel-icon">
            <FontAwesomeIcon icon={nodeTypeIcons[node.type] || faLayerGroup} />
          </span>
          <span className="jc-funnel-name">{node.name}</span>
          <span className="jc-funnel-count">{node.entered.toLocaleString()}</span>
        </div>
      </div>
      <div className="jc-funnel-stats">
        <span className="jc-funnel-stat">
          <FontAwesomeIcon icon={faUsers} /> {node.completed.toLocaleString()} completed
        </span>
        {node.dropped > 0 && (
          <span className="jc-funnel-stat jc-funnel-drop">
            <FontAwesomeIcon icon={faArrowDown} /> {node.dropped.toLocaleString()} dropped
          </span>
        )}
        {node.metrics?.open_rate !== undefined && (
          <span className="jc-funnel-stat jc-funnel-rate">
            <FontAwesomeIcon icon={faEnvelopeOpen} /> {(node.metrics.open_rate * 100).toFixed(1)}% open rate
          </span>
        )}
      </div>
    </div>
  );
};

// ============================================================================
// JOURNEY CENTER OVERVIEW
// ============================================================================

const JourneyCenterOverview: React.FC<{
  stats: JourneyStats | null;
  loading: boolean;
  onViewJourney: (id: string) => void;
  onViewAll: () => void;
  timeRange: TimeRange;
  onTimeRangeChange: (range: TimeRange) => void;
}> = ({ stats, loading, onViewJourney, onViewAll, timeRange, onTimeRangeChange }) => {
  if (loading) {
    return (
      <div className="jc-loading-state">
        <FontAwesomeIcon icon={faSpinner} spin size="2x" />
        <p>Loading journey analytics...</p>
      </div>
    );
  }

  if (!stats) {
    return (
      <div className="jc-empty-state">
        <FontAwesomeIcon icon={faRoute} size="3x" />
        <h3>No Journey Data</h3>
        <p>Create your first journey to see analytics here</p>
      </div>
    );
  }

  return (
    <div className="jc-overview">
      {/* Time Range Selector */}
      <div className="jc-time-selector">
        <span className="jc-time-label">Show data for:</span>
        <div className="jc-time-buttons">
          {(['7d', '30d', '90d', 'all'] as TimeRange[]).map((range) => (
            <button
              key={range}
              className={`jc-time-btn ${timeRange === range ? 'active' : ''}`}
              onClick={() => onTimeRangeChange(range)}
            >
              {range === 'all' ? 'All Time' : range === '7d' ? '7 Days' : range === '30d' ? '30 Days' : '90 Days'}
            </button>
          ))}
        </div>
      </div>

      {/* Hero Stats */}
      <div className="jc-hero-stats">
        <MetricCard
          icon={faRoute}
          label="Total Journeys"
          value={stats.total_journeys ?? 0}
          subValue={`${stats.active_journeys ?? 0} active`}
          color="primary"
        />
        <MetricCard
          icon={faUsers}
          label="Total Enrolled"
          value={stats.total_enrolled ?? 0}
          subValue={`${stats.active_enrolled ?? 0} currently active`}
          color="blue"
        />
        <MetricCard
          icon={faCheckCircle}
          label="Completions"
          value={stats.total_completions ?? 0}
          subValue={`${((stats.avg_completion_rate ?? 0) * 100).toFixed(1)}% avg rate`}
          color="green"
        />
        <MetricCard
          icon={faBullseye}
          label="Conversions"
          value={stats.total_conversions ?? 0}
          subValue={`${((stats.avg_conversion_rate ?? 0) * 100).toFixed(1)}% avg rate`}
          color="purple"
        />
      </div>

      {/* Journey Status Overview */}
      <div className="jc-status-overview">
        <div className="jc-section-header">
          <h3><FontAwesomeIcon icon={faChartPie} /> Journey Status</h3>
        </div>
        <div className="jc-status-grid">
          <div className="jc-status-item jc-status-draft" onClick={onViewAll}>
            <div className="jc-status-count">{stats.draft_journeys ?? 0}</div>
            <div className="jc-status-label">Drafts</div>
          </div>
          <div className="jc-status-item jc-status-active" onClick={onViewAll}>
            <div className="jc-status-count">{stats.active_journeys ?? 0}</div>
            <div className="jc-status-label">Active</div>
          </div>
          <div className="jc-status-item jc-status-paused" onClick={onViewAll}>
            <div className="jc-status-count">{stats.paused_journeys ?? 0}</div>
            <div className="jc-status-label">Paused</div>
          </div>
          <div className="jc-status-item jc-status-enrolled">
            <div className="jc-status-count">{stats.active_enrolled ?? 0}</div>
            <div className="jc-status-label">Active Enrollments</div>
          </div>
        </div>
      </div>

      {/* Performance Rates */}
      <div className="jc-performance-section">
        <div className="jc-section-header">
          <h3><FontAwesomeIcon icon={faPercentage} /> Email Performance</h3>
        </div>
        <div className="jc-rates-grid">
          <div className="jc-rate-display jc-rate-blue">
            <FontAwesomeIcon icon={faEnvelope} className="jc-rate-icon" />
            <span className="jc-rate-value">{(stats.total_emails_sent ?? 0).toLocaleString()}</span>
            <span className="jc-rate-label">Emails Sent</span>
          </div>
          <div className="jc-rate-display jc-rate-green">
            <FontAwesomeIcon icon={faEnvelopeOpen} className="jc-rate-icon" />
            <span className="jc-rate-value">{((stats.avg_open_rate ?? 0) * 100).toFixed(1)}%</span>
            <span className="jc-rate-label">Avg Open Rate</span>
          </div>
          <div className="jc-rate-display jc-rate-purple">
            <FontAwesomeIcon icon={faMousePointer} className="jc-rate-icon" />
            <span className="jc-rate-value">{((stats.avg_click_rate ?? 0) * 100).toFixed(1)}%</span>
            <span className="jc-rate-label">Avg Click Rate</span>
          </div>
          <div className="jc-rate-display jc-rate-teal">
            <FontAwesomeIcon icon={faCheckCircle} className="jc-rate-icon" />
            <span className="jc-rate-value">{((stats.avg_completion_rate ?? 0) * 100).toFixed(1)}%</span>
            <span className="jc-rate-label">Avg Completion</span>
          </div>
        </div>
      </div>

      {/* Two Column Layout */}
      <div className="jc-dashboard-columns">
        {/* Top Performing Journeys */}
        <div className="jc-dashboard-section">
          <div className="jc-section-header">
            <h3><FontAwesomeIcon icon={faChartLine} /> Top Performers</h3>
            <button className="jc-section-action" onClick={onViewAll}>
              View All <FontAwesomeIcon icon={faArrowRight} />
            </button>
          </div>
          <div className="jc-journey-list-mini">
            {(!stats.top_journeys || stats.top_journeys.length === 0) ? (
              <div className="jc-empty-list">
                <FontAwesomeIcon icon={faRoute} />
                <span>No active journeys yet</span>
              </div>
            ) : (
              stats.top_journeys.slice(0, 5).map((journey, index) => (
                <div 
                  key={journey.id} 
                  className="jc-journey-mini-card" 
                  onClick={() => onViewJourney(journey.id)}
                >
                  <div className="jc-rank-badge">#{index + 1}</div>
                  <div className="jc-mini-card-info">
                    <div className="jc-mini-card-name">{journey.name}</div>
                    <div className="jc-mini-card-stats">
                      <span className="jc-stat-enrolled">
                        <FontAwesomeIcon icon={faUsers} />
                        {(journey.total_enrolled || 0).toLocaleString()}
                      </span>
                      <span className="jc-stat-conversion">
                        <FontAwesomeIcon icon={faBullseye} />
                        {((journey.conversion_rate || 0) * 100).toFixed(1)}%
                      </span>
                    </div>
                  </div>
                  <StatusBadge status={journey.status} />
                </div>
              ))
            )}
          </div>
        </div>

        {/* Recent Activity */}
        <div className="jc-dashboard-section">
          <div className="jc-section-header">
            <h3><FontAwesomeIcon icon={faClock} /> Recent Activity</h3>
          </div>
          <div className="jc-activity-feed">
            {(!stats.recent_activity || stats.recent_activity.length === 0) ? (
              <div className="jc-empty-list">
                <FontAwesomeIcon icon={faClock} />
                <span>No recent activity</span>
              </div>
            ) : (
              stats.recent_activity.slice(0, 8).map((activity) => (
                <div key={activity.id} className="jc-activity-item">
                  <span className={`jc-activity-icon jc-activity-${activity.event_type || 'enrolled'}`}>
                    <FontAwesomeIcon 
                      icon={
                        activity.event_type === 'enrolled' ? faUserPlus :
                        activity.event_type === 'completed' ? faCheckCircle :
                        activity.event_type === 'converted' ? faBullseye :
                        activity.event_type === 'email_sent' ? faEnvelope :
                        activity.event_type === 'email_opened' ? faEnvelopeOpen :
                        faUserMinus
                      } 
                    />
                  </span>
                  <div className="jc-activity-content">
                    <span className="jc-activity-text">
                      <strong>{activity.subscriber_email || 'Unknown'}</strong> {(activity.event_type || 'event').replace('_', ' ')}
                    </span>
                    <span className="jc-activity-journey">{activity.journey_name || ''}</span>
                  </div>
                  <span className="jc-activity-time">
                    {activity.timestamp ? new Date(activity.timestamp).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : ''}
                  </span>
                </div>
              ))
            )}
          </div>
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// JOURNEY LIST VIEW
// ============================================================================

const JourneyList: React.FC<{
  journeys: Journey[];
  loading: boolean;
  filter: StatusFilter;
  search: string;
  onFilterChange: (filter: StatusFilter) => void;
  onSearchChange: (search: string) => void;
  onViewJourney: (id: string) => void;
  onAction: (id: string, action: string) => void;
  onRefresh: () => void;
  onCreateJourney: () => void;
}> = ({ 
  journeys, loading, filter, search, onFilterChange, onSearchChange, 
  onViewJourney, onAction, onRefresh, onCreateJourney 
}) => {
  const filteredJourneys = journeys.filter(j => {
    const matchesFilter = filter === 'all' || j.status === filter;
    const matchesSearch = search === '' || 
      j.name.toLowerCase().includes(search.toLowerCase()) ||
      j.description?.toLowerCase().includes(search.toLowerCase());
    return matchesFilter && matchesSearch;
  });

  return (
    <div className="jc-list-view">
      {/* Filters and Search */}
      <div className="jc-list-controls">
        <div className="jc-filter-tabs">
          {(['all', 'draft', 'active', 'paused', 'completed', 'archived'] as StatusFilter[]).map(f => (
            <button
              key={f}
              className={`jc-filter-tab ${filter === f ? 'active' : ''}`}
              onClick={() => onFilterChange(f)}
            >
              {f === 'all' ? 'All' : f.charAt(0).toUpperCase() + f.slice(1)}
              {f !== 'all' && (
                <span className="jc-filter-count">
                  {journeys.filter(j => j.status === f).length}
                </span>
              )}
            </button>
          ))}
        </div>
        <div className="jc-search-controls">
          <div className="jc-search-input-wrapper">
            <FontAwesomeIcon icon={faSearch} className="jc-search-icon" />
            <input
              type="text"
              placeholder="Search journeys..."
              value={search}
              onChange={(e) => onSearchChange(e.target.value)}
            />
          </div>
          <button className="jc-refresh-btn" onClick={onRefresh}>
            <FontAwesomeIcon icon={faSync} spin={loading} />
          </button>
          <button className="jc-create-btn" onClick={onCreateJourney}>
            <FontAwesomeIcon icon={faPlus} /> New Journey
          </button>
        </div>
      </div>

      {/* Journey Cards */}
      {loading ? (
        <div className="jc-loading-state">
          <FontAwesomeIcon icon={faSpinner} spin size="2x" />
          <p>Loading journeys...</p>
        </div>
      ) : filteredJourneys.length === 0 ? (
        <div className="jc-empty-state">
          <FontAwesomeIcon icon={faRoute} size="3x" />
          <h3>No Journeys Found</h3>
          <p>{filter !== 'all' ? `No ${filter} journeys` : 'Create your first journey to get started'}</p>
          <button className="jc-create-btn-large" onClick={onCreateJourney}>
            <FontAwesomeIcon icon={faPlus} /> Create Journey
          </button>
        </div>
      ) : (
        <div className="jc-journeys-grid">
          {filteredJourneys.map(journey => (
            <div key={journey.id} className="jc-journey-card">
              <div className="jc-card-header">
                <StatusBadge status={journey.status} />
                <div className="jc-card-actions">
                  <button onClick={() => onViewJourney(journey.id)} title="View Details">
                    <FontAwesomeIcon icon={faEye} />
                  </button>
                  <button onClick={() => onAction(journey.id, 'edit')} title="Edit Journey">
                    <FontAwesomeIcon icon={faEdit} />
                  </button>
                  {journey.status === 'active' && (
                    <button onClick={() => onAction(journey.id, 'pause')} title="Pause">
                      <FontAwesomeIcon icon={faPause} />
                    </button>
                  )}
                  {journey.status === 'paused' && (
                    <button onClick={() => onAction(journey.id, 'activate')} title="Activate">
                      <FontAwesomeIcon icon={faPlay} />
                    </button>
                  )}
                  {journey.status === 'draft' && (
                    <button onClick={() => onAction(journey.id, 'activate')} title="Activate" className="activate">
                      <FontAwesomeIcon icon={faPlay} />
                    </button>
                  )}
                  <button onClick={() => onAction(journey.id, 'delete')} title="Delete" className="danger">
                    <FontAwesomeIcon icon={faTrash} />
                  </button>
                </div>
              </div>

              <div className="jc-card-body" onClick={() => onViewJourney(journey.id)}>
                <h4 className="jc-journey-name">{journey.name}</h4>
                {journey.description && (
                  <p className="jc-journey-description">{journey.description}</p>
                )}

                <div className="jc-journey-meta">
                  <span className="jc-meta-item">
                    <FontAwesomeIcon icon={faLayerGroup} /> {journey.node_count} steps
                  </span>
                  <span className="jc-meta-item">
                    <FontAwesomeIcon icon={faEnvelope} /> {journey.email_count} emails
                  </span>
                  {journey.list_name && (
                    <span className="jc-meta-item">
                      <FontAwesomeIcon icon={faUsers} /> {journey.list_name}
                    </span>
                  )}
                </div>
              </div>

              <div className="jc-card-stats">
                <div className="jc-stat">
                  <span className="jc-stat-value">{journey.total_enrolled.toLocaleString()}</span>
                  <span className="jc-stat-label">Enrolled</span>
                </div>
                <div className="jc-stat">
                  <span className="jc-stat-value">{journey.active_enrolled.toLocaleString()}</span>
                  <span className="jc-stat-label">Active</span>
                </div>
                <div className="jc-stat">
                  <span className="jc-stat-value">{journey.completed_count.toLocaleString()}</span>
                  <span className="jc-stat-label">Completed</span>
                </div>
                <div className="jc-stat">
                  <span className="jc-stat-value">{(journey.conversion_rate * 100).toFixed(1)}%</span>
                  <span className="jc-stat-label">Conversion</span>
                </div>
              </div>

              {journey.status === 'active' && journey.active_enrolled > 0 && (
                <div className="jc-progress-section">
                  <div className="jc-progress-info">
                    <span>Active Progress</span>
                    <span>{journey.active_enrolled} / {journey.total_enrolled}</span>
                  </div>
                  <ProgressBar 
                    value={journey.completed_count} 
                    max={journey.total_enrolled} 
                    color="green" 
                  />
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
// JOURNEY DETAIL VIEW
// ============================================================================

const JourneyDetail: React.FC<{
  journey: Journey | null;
  nodes: JourneyNode[];
  loading: boolean;
  onClose: () => void;
  onAction: (id: string, action: string) => void;
  onViewEnrollments: () => void;
}> = ({ journey, nodes, loading, onClose, onAction, onViewEnrollments }) => {
  const [activeTab, setActiveTab] = useState<'funnel' | 'metrics' | 'emails'>('funnel');

  if (!journey) return null;

  const maxEntered = nodes.length > 0 ? Math.max(...nodes.map(n => n.entered)) : 0;

  return (
    <div className="jc-modal-overlay" onClick={onClose}>
      <div className="jc-detail-modal" onClick={e => e.stopPropagation()}>
        <div className="jc-modal-header">
          <div className="jc-modal-title">
            <h2>{journey.name}</h2>
            <StatusBadge status={journey.status} />
          </div>
          <button className="jc-close-btn" onClick={onClose}>
            <FontAwesomeIcon icon={faTimes} />
          </button>
        </div>

        <div className="jc-modal-tabs">
          <button 
            className={`jc-tab ${activeTab === 'funnel' ? 'active' : ''}`}
            onClick={() => setActiveTab('funnel')}
          >
            <FontAwesomeIcon icon={faChartBar} /> Funnel View
          </button>
          <button 
            className={`jc-tab ${activeTab === 'metrics' ? 'active' : ''}`}
            onClick={() => setActiveTab('metrics')}
          >
            <FontAwesomeIcon icon={faChartLine} /> Metrics
          </button>
          <button 
            className={`jc-tab ${activeTab === 'emails' ? 'active' : ''}`}
            onClick={() => setActiveTab('emails')}
          >
            <FontAwesomeIcon icon={faEnvelope} /> Email Performance
          </button>
        </div>

        <div className="jc-modal-body">
          {loading ? (
            <div className="jc-loading-state">
              <FontAwesomeIcon icon={faSpinner} spin size="2x" />
              <p>Loading journey details...</p>
            </div>
          ) : (
            <>
              {/* Quick Stats Row */}
              <div className="jc-quick-stats">
                <div className="jc-quick-stat">
                  <FontAwesomeIcon icon={faUsers} />
                  <div>
                    <span className="jc-quick-value">{journey.total_enrolled.toLocaleString()}</span>
                    <span className="jc-quick-label">Total Enrolled</span>
                  </div>
                </div>
                <div className="jc-quick-stat">
                  <FontAwesomeIcon icon={faPlay} />
                  <div>
                    <span className="jc-quick-value">{journey.active_enrolled.toLocaleString()}</span>
                    <span className="jc-quick-label">Currently Active</span>
                  </div>
                </div>
                <div className="jc-quick-stat">
                  <FontAwesomeIcon icon={faCheckCircle} />
                  <div>
                    <span className="jc-quick-value">{journey.completed_count.toLocaleString()}</span>
                    <span className="jc-quick-label">Completed</span>
                  </div>
                </div>
                <div className="jc-quick-stat">
                  <FontAwesomeIcon icon={faBullseye} />
                  <div>
                    <span className="jc-quick-value">{(journey.conversion_rate * 100).toFixed(1)}%</span>
                    <span className="jc-quick-label">Conversion Rate</span>
                  </div>
                </div>
              </div>

              {/* Funnel View */}
              {activeTab === 'funnel' && (
                <div className="jc-funnel-section">
                  <h3>Journey Funnel</h3>
                  <div className="jc-funnel">
                    {nodes.length === 0 ? (
                      <div className="jc-empty-funnel">
                        <FontAwesomeIcon icon={faLayerGroup} />
                        <p>No nodes in this journey yet</p>
                      </div>
                    ) : (
                      nodes.map((node, index) => (
                        <FunnelNode
                          key={node.id}
                          node={node}
                          isFirst={index === 0}
                          isLast={index === nodes.length - 1}
                          maxEntered={maxEntered}
                        />
                      ))
                    )}
                  </div>
                  {journey.drop_off_rate > 0 && (
                    <div className="jc-drop-off-summary">
                      <FontAwesomeIcon icon={faExclamationTriangle} />
                      <span>Overall drop-off rate: <strong>{(journey.drop_off_rate * 100).toFixed(1)}%</strong></span>
                    </div>
                  )}
                </div>
              )}

              {/* Metrics Tab */}
              {activeTab === 'metrics' && (
                <div className="jc-metrics-section">
                  <div className="jc-metrics-grid">
                    <div className="jc-metric-box">
                      <FontAwesomeIcon icon={faEnvelope} className="jc-metric-box-icon blue" />
                      <div className="jc-metric-box-value">{journey.total_sent.toLocaleString()}</div>
                      <div className="jc-metric-box-label">Emails Sent</div>
                    </div>
                    <div className="jc-metric-box">
                      <FontAwesomeIcon icon={faEnvelopeOpen} className="jc-metric-box-icon green" />
                      <div className="jc-metric-box-value">{journey.total_opens.toLocaleString()}</div>
                      <div className="jc-metric-box-label">Opens ({(journey.open_rate * 100).toFixed(1)}%)</div>
                    </div>
                    <div className="jc-metric-box">
                      <FontAwesomeIcon icon={faMousePointer} className="jc-metric-box-icon purple" />
                      <div className="jc-metric-box-value">{journey.total_clicks.toLocaleString()}</div>
                      <div className="jc-metric-box-label">Clicks ({(journey.click_rate * 100).toFixed(1)}%)</div>
                    </div>
                    <div className="jc-metric-box">
                      <FontAwesomeIcon icon={faBullseye} className="jc-metric-box-icon teal" />
                      <div className="jc-metric-box-value">{journey.conversion_count.toLocaleString()}</div>
                      <div className="jc-metric-box-label">Conversions</div>
                    </div>
                  </div>

                  <div className="jc-journey-info">
                    <h4>Journey Information</h4>
                    <div className="jc-info-grid">
                      <div className="jc-info-item">
                        <span className="jc-info-label">Created</span>
                        <span className="jc-info-value">{new Date(journey.created_at).toLocaleDateString()}</span>
                      </div>
                      {journey.activated_at && (
                        <div className="jc-info-item">
                          <span className="jc-info-label">Activated</span>
                          <span className="jc-info-value">{new Date(journey.activated_at).toLocaleDateString()}</span>
                        </div>
                      )}
                      <div className="jc-info-item">
                        <span className="jc-info-label">Trigger Type</span>
                        <span className="jc-info-value">{journey.trigger_type}</span>
                      </div>
                      <div className="jc-info-item">
                        <span className="jc-info-label">Steps</span>
                        <span className="jc-info-value">{journey.node_count}</span>
                      </div>
                    </div>
                  </div>
                </div>
              )}

              {/* Email Performance Tab */}
              {activeTab === 'emails' && (
                <div className="jc-emails-section">
                  <h4>Email Node Performance</h4>
                  <div className="jc-email-nodes">
                    {nodes.filter(n => n.type === 'email').map((node) => (
                      <div key={node.id} className="jc-email-node-card">
                        <div className="jc-email-node-header">
                          <FontAwesomeIcon icon={faEnvelope} />
                          <span>{node.name}</span>
                        </div>
                        <div className="jc-email-node-stats">
                          <div className="jc-email-stat">
                            <span className="jc-email-stat-value">{node.metrics?.sent?.toLocaleString() || 0}</span>
                            <span className="jc-email-stat-label">Sent</span>
                          </div>
                          <div className="jc-email-stat">
                            <span className="jc-email-stat-value">{node.metrics?.opens?.toLocaleString() || 0}</span>
                            <span className="jc-email-stat-label">Opens</span>
                          </div>
                          <div className="jc-email-stat">
                            <span className="jc-email-stat-value">{node.metrics?.clicks?.toLocaleString() || 0}</span>
                            <span className="jc-email-stat-label">Clicks</span>
                          </div>
                          <div className="jc-email-stat">
                            <span className="jc-email-stat-value">
                              {((node.metrics?.open_rate || 0) * 100).toFixed(1)}%
                            </span>
                            <span className="jc-email-stat-label">Open Rate</span>
                          </div>
                        </div>
                      </div>
                    ))}
                    {nodes.filter(n => n.type === 'email').length === 0 && (
                      <div className="jc-empty-emails">
                        <FontAwesomeIcon icon={faEnvelope} />
                        <p>No email nodes in this journey</p>
                      </div>
                    )}
                  </div>
                </div>
              )}
            </>
          )}
        </div>

        <div className="jc-modal-footer">
          <button className="jc-action-btn jc-btn-secondary" onClick={onViewEnrollments}>
            <FontAwesomeIcon icon={faUsers} /> View Enrollments
          </button>
          <button className="jc-action-btn jc-btn-secondary" onClick={() => onAction(journey.id, 'edit')}>
            <FontAwesomeIcon icon={faEdit} /> Edit Journey
          </button>
          {journey.status === 'draft' && (
            <button className="jc-action-btn jc-btn-success" onClick={() => onAction(journey.id, 'activate')}>
              <FontAwesomeIcon icon={faPlay} /> Activate
            </button>
          )}
          {journey.status === 'active' && (
            <button className="jc-action-btn jc-btn-warning" onClick={() => onAction(journey.id, 'pause')}>
              <FontAwesomeIcon icon={faPause} /> Pause
            </button>
          )}
          {journey.status === 'paused' && (
            <button className="jc-action-btn jc-btn-success" onClick={() => onAction(journey.id, 'activate')}>
              <FontAwesomeIcon icon={faPlay} /> Resume
            </button>
          )}
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// ENROLLMENT MANAGEMENT VIEW
// ============================================================================

const JourneyEnrollments: React.FC<{
  journey: Journey | null;
  enrollments: JourneyEnrollment[];
  loading: boolean;
  onClose: () => void;
  onEnrollManually: () => void;
  onRemoveEnrollment: (id: string) => void;
  onPauseEnrollment: (id: string) => void;
}> = ({ journey, enrollments, loading, onClose, onEnrollManually, onRemoveEnrollment, onPauseEnrollment }) => {
  const [search, setSearch] = useState('');
  const [statusFilter, setStatusFilter] = useState<'all' | 'active' | 'completed' | 'exited' | 'paused'>('all');

  if (!journey) return null;

  const filteredEnrollments = enrollments.filter(e => {
    const matchesSearch = search === '' || 
      e.subscriber_email.toLowerCase().includes(search.toLowerCase()) ||
      e.subscriber_name?.toLowerCase().includes(search.toLowerCase());
    const matchesStatus = statusFilter === 'all' || e.status === statusFilter;
    return matchesSearch && matchesStatus;
  });

  return (
    <div className="jc-modal-overlay" onClick={onClose}>
      <div className="jc-enrollments-modal" onClick={e => e.stopPropagation()}>
        <div className="jc-modal-header">
          <div className="jc-modal-title">
            <h2>Enrollments: {journey.name}</h2>
            <span className="jc-enrollment-count">{enrollments.length} total</span>
          </div>
          <button className="jc-close-btn" onClick={onClose}>
            <FontAwesomeIcon icon={faTimes} />
          </button>
        </div>

        <div className="jc-enrollments-controls">
          <div className="jc-search-input-wrapper">
            <FontAwesomeIcon icon={faSearch} className="jc-search-icon" />
            <input
              type="text"
              placeholder="Search by email or name..."
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
          </div>
          <select 
            value={statusFilter} 
            onChange={(e) => setStatusFilter(e.target.value as any)}
            className="jc-status-select"
          >
            <option value="all">All Status</option>
            <option value="active">Active</option>
            <option value="completed">Completed</option>
            <option value="exited">Exited</option>
            <option value="paused">Paused</option>
          </select>
          <button className="jc-enroll-btn" onClick={onEnrollManually}>
            <FontAwesomeIcon icon={faUserPlus} /> Enroll Manually
          </button>
        </div>

        <div className="jc-enrollments-body">
          {loading ? (
            <div className="jc-loading-state">
              <FontAwesomeIcon icon={faSpinner} spin size="2x" />
              <p>Loading enrollments...</p>
            </div>
          ) : filteredEnrollments.length === 0 ? (
            <div className="jc-empty-state">
              <FontAwesomeIcon icon={faUsers} size="3x" />
              <h3>No Enrollments Found</h3>
              <p>{search ? 'No matches for your search' : 'No one has been enrolled in this journey yet'}</p>
            </div>
          ) : (
            <div className="jc-enrollments-table">
              <table>
                <thead>
                  <tr>
                    <th>Subscriber</th>
                    <th>Current Step</th>
                    <th>Status</th>
                    <th>Enrolled</th>
                    <th>Emails</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {filteredEnrollments.map((enrollment) => (
                    <tr key={enrollment.id}>
                      <td>
                        <div className="jc-subscriber-cell">
                          <span className="jc-subscriber-email">{enrollment.subscriber_email}</span>
                          {enrollment.subscriber_name && (
                            <span className="jc-subscriber-name">{enrollment.subscriber_name}</span>
                          )}
                        </div>
                      </td>
                      <td>
                        <span className="jc-current-step">{enrollment.current_node_name}</span>
                      </td>
                      <td>
                        <span className={`jc-enrollment-status jc-status-${enrollment.status}`}>
                          {enrollment.status}
                        </span>
                      </td>
                      <td>
                        <span className="jc-date">{new Date(enrollment.enrolled_at).toLocaleDateString()}</span>
                      </td>
                      <td>
                        <div className="jc-email-stats-cell">
                          <span title="Received">{enrollment.emails_received}</span>
                          <span title="Opened" className="jc-opened">{enrollment.emails_opened}</span>
                          <span title="Clicked" className="jc-clicked">{enrollment.emails_clicked}</span>
                        </div>
                      </td>
                      <td>
                        <div className="jc-enrollment-actions">
                          {enrollment.status === 'active' && (
                            <button 
                              onClick={() => onPauseEnrollment(enrollment.id)}
                              title="Pause"
                            >
                              <FontAwesomeIcon icon={faPause} />
                            </button>
                          )}
                          <button 
                            onClick={() => onRemoveEnrollment(enrollment.id)}
                            title="Remove"
                            className="danger"
                          >
                            <FontAwesomeIcon icon={faTrash} />
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// PERFORMANCE ANALYTICS VIEW
// ============================================================================

const JourneyPerformance: React.FC<{
  journeys: Journey[];
  loading: boolean;
  timeRange: TimeRange;
  onTimeRangeChange: (range: TimeRange) => void;
  onViewJourney: (id: string) => void;
}> = ({ journeys, loading, timeRange, onTimeRangeChange, onViewJourney }) => {
  const activeJourneys = journeys.filter(j => j.status === 'active' || j.status === 'completed');

  // Sort by different metrics
  const byConversion = [...activeJourneys].sort((a, b) => b.conversion_rate - a.conversion_rate);
  const byEnrollment = [...activeJourneys].sort((a, b) => b.total_enrolled - a.total_enrolled);
  const byOpenRate = [...activeJourneys].sort((a, b) => b.open_rate - a.open_rate);

  if (loading) {
    return (
      <div className="jc-loading-state">
        <FontAwesomeIcon icon={faSpinner} spin size="2x" />
        <p>Loading performance data...</p>
      </div>
    );
  }

  return (
    <div className="jc-performance-view">
      {/* Time Range Selector */}
      <div className="jc-time-selector">
        <span className="jc-time-label">Analyze:</span>
        <div className="jc-time-buttons">
          {(['7d', '30d', '90d', 'all'] as TimeRange[]).map((range) => (
            <button
              key={range}
              className={`jc-time-btn ${timeRange === range ? 'active' : ''}`}
              onClick={() => onTimeRangeChange(range)}
            >
              {range === 'all' ? 'All Time' : range === '7d' ? '7 Days' : range === '30d' ? '30 Days' : '90 Days'}
            </button>
          ))}
        </div>
      </div>

      {activeJourneys.length === 0 ? (
        <div className="jc-empty-state">
          <FontAwesomeIcon icon={faChartLine} size="3x" />
          <h3>No Performance Data</h3>
          <p>Activate some journeys to see performance analytics</p>
        </div>
      ) : (
        <>
          {/* Comparison Charts */}
          <div className="jc-comparison-section">
            <h3><FontAwesomeIcon icon={faChartBar} /> Journey Comparison</h3>
            
            <div className="jc-comparison-grid">
              {/* By Conversion Rate */}
              <div className="jc-comparison-card">
                <h4>By Conversion Rate</h4>
                <div className="jc-comparison-list">
                  {byConversion.slice(0, 5).map((journey, index) => (
                    <div 
                      key={journey.id} 
                      className="jc-comparison-item"
                      onClick={() => onViewJourney(journey.id)}
                    >
                      <span className="jc-comparison-rank">#{index + 1}</span>
                      <span className="jc-comparison-name">{journey.name}</span>
                      <div className="jc-comparison-bar-wrapper">
                        <div 
                          className="jc-comparison-bar jc-bar-purple"
                          style={{ width: `${Math.min(journey.conversion_rate * 100 * 2, 100)}%` }}
                        />
                      </div>
                      <span className="jc-comparison-value">{(journey.conversion_rate * 100).toFixed(1)}%</span>
                    </div>
                  ))}
                </div>
              </div>

              {/* By Enrollment */}
              <div className="jc-comparison-card">
                <h4>By Total Enrolled</h4>
                <div className="jc-comparison-list">
                  {byEnrollment.slice(0, 5).map((journey, index) => (
                    <div 
                      key={journey.id} 
                      className="jc-comparison-item"
                      onClick={() => onViewJourney(journey.id)}
                    >
                      <span className="jc-comparison-rank">#{index + 1}</span>
                      <span className="jc-comparison-name">{journey.name}</span>
                      <div className="jc-comparison-bar-wrapper">
                        <div 
                          className="jc-comparison-bar jc-bar-blue"
                          style={{ 
                            width: `${byEnrollment[0]?.total_enrolled > 0 
                              ? (journey.total_enrolled / byEnrollment[0].total_enrolled) * 100 
                              : 0}%` 
                          }}
                        />
                      </div>
                      <span className="jc-comparison-value">{journey.total_enrolled.toLocaleString()}</span>
                    </div>
                  ))}
                </div>
              </div>

              {/* By Open Rate */}
              <div className="jc-comparison-card">
                <h4>By Open Rate</h4>
                <div className="jc-comparison-list">
                  {byOpenRate.slice(0, 5).map((journey, index) => (
                    <div 
                      key={journey.id} 
                      className="jc-comparison-item"
                      onClick={() => onViewJourney(journey.id)}
                    >
                      <span className="jc-comparison-rank">#{index + 1}</span>
                      <span className="jc-comparison-name">{journey.name}</span>
                      <div className="jc-comparison-bar-wrapper">
                        <div 
                          className="jc-comparison-bar jc-bar-green"
                          style={{ width: `${Math.min(journey.open_rate * 100 * 2, 100)}%` }}
                        />
                      </div>
                      <span className="jc-comparison-value">{(journey.open_rate * 100).toFixed(1)}%</span>
                    </div>
                  ))}
                </div>
              </div>
            </div>
          </div>

          {/* Drop-off Analysis */}
          <div className="jc-dropoff-section">
            <h3><FontAwesomeIcon icon={faExclamationTriangle} /> Drop-off Analysis</h3>
            <div className="jc-dropoff-list">
              {activeJourneys
                .filter(j => j.drop_off_rate > 0)
                .sort((a, b) => b.drop_off_rate - a.drop_off_rate)
                .slice(0, 5)
                .map((journey) => (
                  <div 
                    key={journey.id} 
                    className="jc-dropoff-item"
                    onClick={() => onViewJourney(journey.id)}
                  >
                    <div className="jc-dropoff-info">
                      <span className="jc-dropoff-name">{journey.name}</span>
                      <span className="jc-dropoff-stats">
                        {journey.total_enrolled - journey.completed_count} dropped of {journey.total_enrolled}
                      </span>
                    </div>
                    <div className="jc-dropoff-rate">
                      <span className={`jc-dropoff-value ${journey.drop_off_rate > 0.5 ? 'high' : ''}`}>
                        {(journey.drop_off_rate * 100).toFixed(1)}%
                      </span>
                      <ProgressBar 
                        value={journey.drop_off_rate * 100} 
                        max={100} 
                        color={journey.drop_off_rate > 0.5 ? 'red' : 'orange'} 
                      />
                    </div>
                  </div>
                ))}
              {activeJourneys.filter(j => j.drop_off_rate > 0).length === 0 && (
                <div className="jc-no-dropoff">
                  <FontAwesomeIcon icon={faCheckCircle} />
                  <span>No significant drop-offs detected</span>
                </div>
              )}
            </div>
          </div>
        </>
      )}
    </div>
  );
};

// ============================================================================
// MAIN JOURNEY CENTER COMPONENT
// ============================================================================

export const JourneyCenter: React.FC = () => {
  const { organization } = useAuth();
  const [view, setView] = useState<ViewType>('overview');
  const [journeys, setJourneys] = useState<Journey[]>([]);
  const [stats, setStats] = useState<JourneyStats | null>(null);
  const [selectedJourney, setSelectedJourney] = useState<Journey | null>(null);
  const [journeyNodes, setJourneyNodes] = useState<JourneyNode[]>([]);
  const [enrollments, setEnrollments] = useState<JourneyEnrollment[]>([]);
  const [loading, setLoading] = useState(true);
  const [detailsLoading, setDetailsLoading] = useState(false);
  const [filter, setFilter] = useState<StatusFilter>('all');
  const [search, setSearch] = useState('');
  const [timeRange, setTimeRange] = useState<TimeRange>('30d');

  // Fetch overview stats
  const fetchStats = useCallback(async () => {
    setLoading(true);
    try {
      const [overviewRes, journeysRes] = await Promise.all([
        orgFetch(`${API_BASE}/journey-center/overview?range=${timeRange}`, organization?.id),
        orgFetch(`${API_BASE}/journey-center/journeys`, organization?.id),
      ]);

      const overviewData = await overviewRes.json();
      const journeysData = await journeysRes.json();

      // Normalize stats top_journeys
      if (overviewData.top_journeys) {
        overviewData.top_journeys = overviewData.top_journeys.map(normalizeJourney);
      }
      setStats(overviewData);
      setJourneys((journeysData.journeys || []).map(normalizeJourney));
    } catch (err) {
      console.error('Failed to fetch journey stats:', err);
      // Set empty fallback stats on error
      setStats({
        total_journeys: 0,
        active_journeys: 0,
        draft_journeys: 0,
        paused_journeys: 0,
        total_enrolled: 0,
        active_enrolled: 0,
        total_completions: 0,
        total_conversions: 0,
        avg_completion_rate: 0,
        avg_conversion_rate: 0,
        total_emails_sent: 0,
        avg_open_rate: 0,
        avg_click_rate: 0,
        top_journeys: [],
        recent_activity: [],
      });
    } finally {
      setLoading(false);
    }
  }, [organization, timeRange]);

  // Fetch journey details
  const fetchJourneyDetails = useCallback(async (id: string) => {
    setDetailsLoading(true);
    try {
      const [journeyRes, funnelRes] = await Promise.all([
        orgFetch(`${API_BASE}/journey-center/journeys/${id}/metrics`, organization?.id),
        orgFetch(`${API_BASE}/journey-center/journeys/${id}/funnel`, organization?.id),
      ]);

      const journey = await journeyRes.json();
      const funnel = await funnelRes.json();

      setSelectedJourney(normalizeJourney(journey));
      setJourneyNodes(funnel.nodes || []);
    } catch (err) {
      console.error('Failed to fetch journey details:', err);
      // Find journey from existing list
      const journey = journeys.find(j => j.id === id);
      if (journey) {
        setSelectedJourney(journey);
        setJourneyNodes([]);
      }
    } finally {
      setDetailsLoading(false);
    }
  }, [organization, journeys]);

  // Fetch enrollments
  const fetchEnrollments = useCallback(async (id: string) => {
    setDetailsLoading(true);
    try {
      const res = await orgFetch(`${API_BASE}/journey-center/journeys/${id}/enrollments`, organization?.id);
      const data = await res.json();
      setEnrollments(data.enrollments || []);
    } catch (err) {
      console.error('Failed to fetch enrollments:', err);
      setEnrollments([]);
    } finally {
      setDetailsLoading(false);
    }
  }, [organization]);

  // Handle journey actions
  const handleAction = useCallback(async (id: string, action: string) => {
    try {
      if (action === 'edit') {
        // Navigate to journey builder
        window.location.hash = `#journeys/${id}/edit`;
        return;
      }

      if (action === 'activate') {
        await orgFetch(`${API_BASE}/journeys/${id}/activate`, organization?.id, { method: 'POST' });
      } else if (action === 'pause') {
        await orgFetch(`${API_BASE}/journeys/${id}/pause`, organization?.id, { method: 'POST' });
      } else if (action === 'delete') {
        if (window.confirm('Are you sure you want to delete this journey?')) {
          await orgFetch(`${API_BASE}/journeys/${id}`, organization?.id, { method: 'DELETE' });
        } else {
          return;
        }
      }

      // Refresh data
      fetchStats();
    } catch (err) {
      console.error(`Failed to ${action} journey:`, err);
    }
  }, [organization, fetchStats]);

  // View journey
  const handleViewJourney = (id: string) => {
    fetchJourneyDetails(id);
    setView('detail');
  };

  // View enrollments
  const handleViewEnrollments = () => {
    if (selectedJourney) {
      fetchEnrollments(selectedJourney.id);
      setView('enrollments');
    }
  };

  // Create new journey - navigate to builder tab
  const handleCreateJourney = () => {
    setView('builder');
  };

  // Initial load
  useEffect(() => {
    fetchStats();
  }, [fetchStats]);

  // Auto-refresh for active journeys using a ref to avoid re-subscription loops
  const fetchStatsRef = useRef(fetchStats);
  fetchStatsRef.current = fetchStats;

  useEffect(() => {
    const hasActiveJourneys = journeys.some(j => j.status === 'active');
    if (!hasActiveJourneys) return;

    const interval = setInterval(() => {
      fetchStatsRef.current();
    }, 30000);

    return () => clearInterval(interval);
  }, [journeys.length]);

  return (
    <div className="journey-center">
      {/* Header */}
      <div className="jc-header">
        <div className="jc-header-title">
          <FontAwesomeIcon icon={faRoute} className="jc-header-icon" />
          <div>
            <h1>Journey Center</h1>
            <p>Monitor and manage your automated email journeys</p>
          </div>
        </div>
        <div className="jc-header-actions">
          <button className="jc-create-btn" onClick={handleCreateJourney}>
            <FontAwesomeIcon icon={faPlus} /> New Journey
          </button>
          <button className="jc-refresh-btn" onClick={fetchStats} disabled={loading}>
            <FontAwesomeIcon icon={faSync} spin={loading} /> Refresh
          </button>
        </div>
      </div>

      {/* Navigation Tabs */}
      <div className="jc-nav">
        <button
          className={`jc-nav-tab ${view === 'overview' ? 'active' : ''}`}
          onClick={() => setView('overview')}
        >
          <FontAwesomeIcon icon={faTachometerAlt} /> Overview
        </button>
        <button
          className={`jc-nav-tab ${view === 'list' ? 'active' : ''}`}
          onClick={() => setView('list')}
        >
          <FontAwesomeIcon icon={faRoute} /> All Journeys
          <span className="jc-nav-count">{journeys.length}</span>
        </button>
        <button
          className={`jc-nav-tab ${view === 'performance' ? 'active' : ''}`}
          onClick={() => setView('performance')}
        >
          <FontAwesomeIcon icon={faChartLine} /> Performance
        </button>
        <button
          className={`jc-nav-tab ${view === 'builder' ? 'active' : ''}`}
          onClick={() => setView('builder')}
        >
          <FontAwesomeIcon icon={faCodeBranch} /> Journey Builder
        </button>
      </div>

      {/* Main Content */}
      <div className="jc-content">
        {view === 'overview' && (
          <JourneyCenterOverview
            stats={stats}
            loading={loading}
            onViewJourney={handleViewJourney}
            onViewAll={() => setView('list')}
            timeRange={timeRange}
            onTimeRangeChange={setTimeRange}
          />
        )}

        {view === 'list' && (
          <JourneyList
            journeys={journeys}
            loading={loading}
            filter={filter}
            search={search}
            onFilterChange={setFilter}
            onSearchChange={setSearch}
            onViewJourney={handleViewJourney}
            onAction={handleAction}
            onRefresh={fetchStats}
            onCreateJourney={handleCreateJourney}
          />
        )}

        {view === 'performance' && (
          <JourneyPerformance
            journeys={journeys}
            loading={loading}
            timeRange={timeRange}
            onTimeRangeChange={setTimeRange}
            onViewJourney={handleViewJourney}
          />
        )}

        {view === 'builder' && (
          <JourneyBuilder />
        )}
      </div>

      {/* Journey Detail Modal */}
      {view === 'detail' && selectedJourney && (
        <JourneyDetail
          journey={selectedJourney}
          nodes={journeyNodes}
          loading={detailsLoading}
          onClose={() => setView('list')}
          onAction={handleAction}
          onViewEnrollments={handleViewEnrollments}
        />
      )}

      {/* Enrollments Modal */}
      {view === 'enrollments' && selectedJourney && (
        <JourneyEnrollments
          journey={selectedJourney}
          enrollments={enrollments}
          loading={detailsLoading}
          onClose={() => setView('detail')}
          onEnrollManually={() => alert('Manual enrollment coming soon')}
          onRemoveEnrollment={(id) => console.log('Remove enrollment:', id)}
          onPauseEnrollment={(id) => console.log('Pause enrollment:', id)}
        />
      )}
    </div>
  );
};

export default JourneyCenter;
