import React, { useState, useEffect, useCallback } from 'react';
import SuppressionAutoRefresh from './SuppressionAutoRefresh';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faShieldAlt,
  faList,
  faBullseye,
  faBolt,
  faPlus,
  faUpload,
  faSync,
  faChartBar,
  faChartPie,
  faArrowRight,
  faArrowUp,
  faSearch,
  faEnvelope,
  faEye,
  faPencilAlt,
  faTrash,
  faExclamationTriangle,
  faInbox,
  faCog,
  faBook,
  faHeart,
  faArrowLeft,
  faFile,
  faCheckCircle,
  faTimesCircle,
  faSpinner,
  faWrench,
  faGlobe,
  faBan,
  faUserSlash,
  faExclamationCircle,
  faAt,
  faUserTimes,
  faHandPaper,
  faGavel,
  faClock,
} from '@fortawesome/free-solid-svg-icons';
import './SuppressionPortal.css';

// ============================================================================
// TYPES
// ============================================================================

interface SuppressionList {
  id: string;
  name: string;
  description?: string;
  entry_count: number;
  source: string;
  last_sync_at?: string;
  created_at: string;
  status: 'active' | 'syncing' | 'error';
}

interface SuppressionEntry {
  id: string;
  email: string;
  md5_hash: string;
  reason: string;
  source: string;
  list_id: string;
  created_at: string;
}

interface OptizmoList {
  id: string;
  name: string;
  entry_count: number;
  last_delta_at?: string;
  sync_enabled: boolean;
}

interface GlobalSuppressionStats {
  total: number;
  hard_bounces: number;
  spam_complaints: number;
  unsubscribes: number;
  spam_traps: number;
  role_based: number;
  disposable: number;
  known_litigator: number;
  invalid: number;
  manual: number;
  recent_24h: number;
  by_category: Record<string, number>;
}

interface DashboardStats {
  total_suppressed: number;
  total_lists: number;
  avg_suppressed_per_campaign: number;
  recent_additions: number;
  optizmo_synced_today: number;
  last_delta_update: string;
  lists_updated_24h: number;
  new_lists_7d: number;
  suppression_rate: number;
  by_source: { source: string; count: number }[];
  by_reason: { reason: string; count: number }[];
  recent_activity: { action: string; details: string; timestamp: string }[];
  global_suppression: GlobalSuppressionStats;
}

type ViewMode = 'dashboard' | 'lists' | 'create' | 'edit' | 'entries' | 'optizmo' | 'bulk-upload' | 'auto-refresh';

// ============================================================================
// MAIN COMPONENT
// ============================================================================

export const SuppressionPortal: React.FC = () => {
  const [viewMode, setViewMode] = useState<ViewMode>('dashboard');
  const [selectedList, setSelectedList] = useState<SuppressionList | null>(null);
  const [stats, setStats] = useState<DashboardStats | null>(null);
  const [lists, setLists] = useState<SuppressionList[]>([]);
  const [loading, setLoading] = useState(true);
  const [animateIn, setAnimateIn] = useState(false);

  // Fetch ONLY dashboard stats (lightweight - safe for polling)
  const fetchStats = useCallback(async () => {
    try {
      const statsRes = await fetch('/api/mailing/suppressions/dashboard');
      const statsData = await statsRes.json().catch(() => ({}));
      
      const globalSupp = statsData.global_suppression || {};
      setStats({
        total_suppressed: statsData.total_suppressed || 0,
        total_lists: statsData.total_lists || 0,
        avg_suppressed_per_campaign: statsData.avg_suppressed_per_campaign || 0,
        recent_additions: statsData.recent_additions || 0,
        optizmo_synced_today: statsData.optizmo_synced_today || 0,
        last_delta_update: statsData.last_delta_update || '',
        lists_updated_24h: statsData.lists_updated_24h || 0,
        new_lists_7d: statsData.new_lists_7d || 0,
        suppression_rate: statsData.suppression_rate || 0,
        by_source: statsData.by_source || [],
        by_reason: statsData.by_reason || [],
        recent_activity: statsData.recent_activity || [],
        global_suppression: {
          total: globalSupp.total || 0,
          hard_bounces: globalSupp.hard_bounces || 0,
          spam_complaints: globalSupp.spam_complaints || 0,
          unsubscribes: globalSupp.unsubscribes || 0,
          spam_traps: globalSupp.spam_traps || 0,
          role_based: globalSupp.role_based || 0,
          disposable: globalSupp.disposable || 0,
          known_litigator: globalSupp.known_litigator || 0,
          invalid: globalSupp.invalid || 0,
          manual: globalSupp.manual || 0,
          recent_24h: globalSupp.recent_24h || 0,
          by_category: globalSupp.by_category || {},
        },
      });
    } catch (err) {
      console.error('Failed to fetch dashboard stats:', err);
    }
  }, []);

  // Fetch suppression lists (heavier - only on demand)
  const fetchLists = useCallback(async () => {
    try {
      const listsRes = await fetch('/api/mailing/suppression-lists');
      const listsData = await listsRes.json().catch(() => ({ lists: [] }));
      setLists(listsData.lists || []);
    } catch (err) {
      console.error('Failed to fetch suppression lists:', err);
    }
  }, []);

  // Combined refresh for CRUD operations (used by child components' onRefresh)
  const fetchDashboard = useCallback(async () => {
    await fetchStats();
    fetchLists(); // refresh lists in background after CRUD
  }, [fetchStats, fetchLists]);

  // Initial load: fetch stats immediately, lists in background
  useEffect(() => {
    const init = async () => {
      await fetchStats();
      setLoading(false);
      setTimeout(() => setAnimateIn(true), 100);
      // Load lists in background (non-blocking) for dashboard preview
      fetchLists();
    };
    init();
    // Poll only stats every 60s - lists don't need constant refreshing
    const interval = setInterval(fetchStats, 60000);
    return () => clearInterval(interval);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Navigate to a view
  const navigateTo = (view: ViewMode, list?: SuppressionList) => {
    setAnimateIn(false);
    setTimeout(() => {
      setViewMode(view);
      if (list) setSelectedList(list);
      // Refresh lists when navigating to views that need them
      if (view === 'lists' || view === 'bulk-upload') {
        fetchLists();
      }
      setTimeout(() => setAnimateIn(true), 50);
    }, 200);
  };

  // Render based on view mode
  const renderContent = () => {
    switch (viewMode) {
      case 'dashboard':
        return (
          <SuppressionDashboard
            stats={stats}
            lists={lists}
            onNavigate={navigateTo}
            animateIn={animateIn}
          />
        );
      case 'lists':
        return (
          <SuppressionListsManager
            lists={lists}
            onNavigate={navigateTo}
            onRefresh={fetchDashboard}
            animateIn={animateIn}
          />
        );
      case 'create':
        return (
          <CreateSuppressionList
            onCancel={() => navigateTo('lists')}
            onSuccess={() => {
              fetchDashboard();
              navigateTo('lists');
            }}
            animateIn={animateIn}
          />
        );
      case 'edit':
        return selectedList ? (
          <EditSuppressionList
            list={selectedList}
            onCancel={() => navigateTo('lists')}
            onSuccess={() => {
              fetchDashboard();
              navigateTo('lists');
            }}
            animateIn={animateIn}
          />
        ) : null;
      case 'entries':
        return selectedList ? (
          <SuppressionEntries
            list={selectedList}
            onBack={() => navigateTo('lists')}
            animateIn={animateIn}
          />
        ) : null;
      case 'optizmo':
        return (
          <OptizmoManager
            onBack={() => navigateTo('dashboard')}
            onRefresh={fetchDashboard}
            animateIn={animateIn}
          />
        );
      case 'bulk-upload':
        return (
          <BulkUpload
            lists={lists}
            onCancel={() => navigateTo('lists')}
            onSuccess={() => {
              fetchDashboard();
              navigateTo('lists');
            }}
            animateIn={animateIn}
          />
        );
      case 'auto-refresh':
        return (
          <SuppressionAutoRefresh
            onBack={() => navigateTo('dashboard')}
            animateIn={animateIn}
          />
        );
      default:
        return null;
    }
  };

  if (loading) {
    return (
      <div className="suppression-portal">
        <div className="loading-container">
          <div className="loading-spinner"></div>
          <p>Loading suppression data...</p>
        </div>
      </div>
    );
  }

  return (
    <div className="suppression-portal">
      {/* Breadcrumb Navigation */}
      <nav className="portal-breadcrumb">
        <button
          className={viewMode === 'dashboard' ? 'active' : ''}
          onClick={() => navigateTo('dashboard')}
        >
          <span className="breadcrumb-icon"><FontAwesomeIcon icon={faChartBar} /></span>
          Dashboard
        </button>
        {viewMode !== 'dashboard' && (
          <>
            <span className="breadcrumb-separator">›</span>
            <button
              className={viewMode === 'lists' ? 'active' : ''}
              onClick={() => navigateTo('lists')}
            >
              <span className="breadcrumb-icon"><FontAwesomeIcon icon={faList} /></span>
              Manage Lists
            </button>
          </>
        )}
        {(viewMode === 'create' || viewMode === 'edit' || viewMode === 'entries' || viewMode === 'bulk-upload') && (
          <>
            <span className="breadcrumb-separator">›</span>
            <span className="breadcrumb-current">
              {viewMode === 'create' && 'Create New'}
              {viewMode === 'edit' && `Edit: ${selectedList?.name}`}
              {viewMode === 'entries' && `Entries: ${selectedList?.name}`}
              {viewMode === 'bulk-upload' && 'Bulk Upload'}
            </span>
          </>
        )}
        {viewMode === 'optizmo' && (
          <>
            <span className="breadcrumb-separator">›</span>
            <span className="breadcrumb-current">Optizmo Integration</span>
          </>
        )}
        {viewMode === 'auto-refresh' && (
          <>
            <span className="breadcrumb-separator">›</span>
            <span className="breadcrumb-current">
              <FontAwesomeIcon icon={faSync} style={{ marginRight: 6 }} />
              Auto-Refresh Manager
            </span>
          </>
        )}
      </nav>

      {/* Main Content */}
      <div className="portal-content">
        {renderContent()}
      </div>
    </div>
  );
};

// ============================================================================
// DASHBOARD COMPONENT
// ============================================================================

interface DashboardProps {
  stats: DashboardStats | null;
  lists: SuppressionList[];
  onNavigate: (view: ViewMode, list?: SuppressionList) => void;
  animateIn: boolean;
}

const SuppressionDashboard: React.FC<DashboardProps> = ({ stats, lists, onNavigate, animateIn }) => {
  if (!stats) return null;

  return (
    <div className={`suppression-dashboard ${animateIn ? 'animate-in' : ''}`}>
      {/* Hero Stats */}
      <div className="hero-stats">
        <div className="hero-stat-card primary" style={{ animationDelay: '0ms' }}>
          <div className="hero-stat-icon"><FontAwesomeIcon icon={faShieldAlt} /></div>
          <div className="hero-stat-content">
            <div className="hero-stat-value">
              <AnimatedNumber value={stats.total_suppressed} />
            </div>
            <div className="hero-stat-label">Total Suppressed Emails</div>
            {stats.total_suppressed > 0 && (
              <div className="hero-stat-trend positive">
                <FontAwesomeIcon icon={faArrowUp} /> Protected from invalid sends
              </div>
            )}
          </div>
        </div>

        <div className="hero-stat-card" style={{ animationDelay: '50ms' }}>
          <div className="hero-stat-icon"><FontAwesomeIcon icon={faList} /></div>
          <div className="hero-stat-content">
            <div className="hero-stat-value">{stats.total_lists}</div>
            <div className="hero-stat-label">Suppression Lists</div>
            {stats.new_lists_7d > 0 && (
              <div className="hero-stat-trend">+{stats.new_lists_7d} this week</div>
            )}
          </div>
        </div>

        <div className="hero-stat-card" style={{ animationDelay: '100ms' }}>
          <div className="hero-stat-icon"><FontAwesomeIcon icon={faBullseye} /></div>
          <div className="hero-stat-content">
            <div className="hero-stat-value">
              <AnimatedNumber value={stats.avg_suppressed_per_campaign} />
            </div>
            <div className="hero-stat-label">Avg. Suppressed/Campaign</div>
            {stats.avg_suppressed_per_campaign > 0 && (
              <div className="hero-stat-trend">Protecting your reputation</div>
            )}
          </div>
        </div>

        <div className="hero-stat-card optizmo" style={{ animationDelay: '150ms' }}>
          <div className="hero-stat-icon"><FontAwesomeIcon icon={faBolt} /></div>
          <div className="hero-stat-content">
            <div className="hero-stat-value">
              <AnimatedNumber value={stats.optizmo_synced_today} />
            </div>
            <div className="hero-stat-label">Optizmo Synced Today</div>
            {stats.last_delta_update && (
              <div className="hero-stat-trend">
                Last delta: {formatTimeAgo(stats.last_delta_update)}
              </div>
            )}
          </div>
        </div>
      </div>

      {/* Quick Actions */}
      <div className="dashboard-section" style={{ animationDelay: '200ms' }}>
        <h3><FontAwesomeIcon icon={faBolt} /> Quick Actions</h3>
        <div className="quick-actions-grid">
          <button className="quick-action-btn primary" onClick={() => onNavigate('create')}>
            <span className="qa-icon"><FontAwesomeIcon icon={faPlus} /></span>
            <div className="qa-content">
              <strong>Create New List</strong>
              <small>Add a new suppression list</small>
            </div>
            <span className="qa-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="quick-action-btn" onClick={() => onNavigate('lists')}>
            <span className="qa-icon"><FontAwesomeIcon icon={faList} /></span>
            <div className="qa-content">
              <strong>Manage Lists</strong>
              <small>View, edit, or delete lists</small>
            </div>
            <span className="qa-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="quick-action-btn" onClick={() => onNavigate('bulk-upload')}>
            <span className="qa-icon"><FontAwesomeIcon icon={faUpload} /></span>
            <div className="qa-content">
              <strong>Bulk Upload</strong>
              <small>Import emails from CSV/file</small>
            </div>
            <span className="qa-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="quick-action-btn optizmo" onClick={() => onNavigate('optizmo')}>
            <span className="qa-icon"><FontAwesomeIcon icon={faSync} /></span>
            <div className="qa-content">
              <strong>Optizmo Sync</strong>
              <small>Configure daily delta sync</small>
            </div>
            <span className="qa-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="quick-action-btn" onClick={() => onNavigate('auto-refresh')} style={{ borderLeft: '3px solid #4fc3f7' }}>
            <span className="qa-icon" style={{ background: 'rgba(79,195,247,0.15)', color: '#4fc3f7' }}><FontAwesomeIcon icon={faBolt} /></span>
            <div className="qa-content">
              <strong>Auto-Refresh Manager</strong>
              <small>Daily advertiser suppression refresh (12PM-12AM MST)</small>
            </div>
            <span className="qa-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>
        </div>
      </div>

      {/* Global Suppression List - Industry Standard */}
      <div className="global-suppression-section" style={{ animationDelay: '250ms' }}>
        <div className="global-suppression-header">
          <div className="global-header-title">
            <FontAwesomeIcon icon={faGlobe} className="global-icon" />
            <div>
              <h3>Global Suppression List</h3>
              <p>Industry-standard protection applied to all campaigns automatically</p>
            </div>
          </div>
          <div className="global-header-stats">
            <div className="global-total">
              <span className="global-total-value">
                <AnimatedNumber value={stats.global_suppression.total} />
              </span>
              <span className="global-total-label">Total Protected</span>
            </div>
            {stats.global_suppression.recent_24h > 0 && (
              <div className="global-recent">
                <FontAwesomeIcon icon={faClock} />
                +{stats.global_suppression.recent_24h.toLocaleString()} in 24h
              </div>
            )}
          </div>
        </div>

        <div className="global-categories-grid">
          <div className="global-category-card hard-bounce">
            <div className="category-icon"><FontAwesomeIcon icon={faBan} /></div>
            <div className="category-info">
              <span className="category-value">{stats.global_suppression.hard_bounces.toLocaleString()}</span>
              <span className="category-label">Hard Bounces</span>
            </div>
            <span className="category-badge critical">Critical</span>
          </div>

          <div className="global-category-card spam-complaint">
            <div className="category-icon"><FontAwesomeIcon icon={faExclamationCircle} /></div>
            <div className="category-info">
              <span className="category-value">{stats.global_suppression.spam_complaints.toLocaleString()}</span>
              <span className="category-label">Spam Complaints</span>
            </div>
            <span className="category-badge critical">Critical</span>
          </div>

          <div className="global-category-card unsubscribe">
            <div className="category-icon"><FontAwesomeIcon icon={faUserSlash} /></div>
            <div className="category-info">
              <span className="category-value">{stats.global_suppression.unsubscribes.toLocaleString()}</span>
              <span className="category-label">Unsubscribes</span>
            </div>
            <span className="category-badge warning">CAN-SPAM</span>
          </div>

          <div className="global-category-card spam-trap">
            <div className="category-icon"><FontAwesomeIcon icon={faUserTimes} /></div>
            <div className="category-info">
              <span className="category-value">{stats.global_suppression.spam_traps.toLocaleString()}</span>
              <span className="category-label">Spam Traps</span>
            </div>
            <span className="category-badge critical">Critical</span>
          </div>

          <div className="global-category-card role-based">
            <div className="category-icon"><FontAwesomeIcon icon={faAt} /></div>
            <div className="category-info">
              <span className="category-value">{stats.global_suppression.role_based.toLocaleString()}</span>
              <span className="category-label">Role-Based</span>
            </div>
            <span className="category-badge info">Best Practice</span>
          </div>

          <div className="global-category-card known-litigator">
            <div className="category-icon"><FontAwesomeIcon icon={faGavel} /></div>
            <div className="category-info">
              <span className="category-value">{stats.global_suppression.known_litigator.toLocaleString()}</span>
              <span className="category-label">Known Litigators</span>
            </div>
            <span className="category-badge critical">Legal Risk</span>
          </div>

          <div className="global-category-card disposable">
            <div className="category-icon"><FontAwesomeIcon icon={faHandPaper} /></div>
            <div className="category-info">
              <span className="category-value">{stats.global_suppression.disposable.toLocaleString()}</span>
              <span className="category-label">Disposable</span>
            </div>
            <span className="category-badge warning">Low Quality</span>
          </div>

          <div className="global-category-card manual">
            <div className="category-icon"><FontAwesomeIcon icon={faPencilAlt} /></div>
            <div className="category-info">
              <span className="category-value">{stats.global_suppression.manual.toLocaleString()}</span>
              <span className="category-label">Manual</span>
            </div>
            <span className="category-badge info">User Added</span>
          </div>
        </div>

        {stats.global_suppression.total === 0 && (
          <div className="global-empty-state">
            <FontAwesomeIcon icon={faShieldAlt} />
            <p>Your Global Suppression List will populate automatically as bounces, complaints, and unsubscribes are processed.</p>
          </div>
        )}
      </div>

      {/* Charts Row */}
      <div className="charts-row" style={{ animationDelay: '300ms' }}>
        {/* By Source Chart */}
        <div className="chart-card">
          <h4><FontAwesomeIcon icon={faChartBar} /> Suppressions by Source</h4>
          {stats.by_source.length > 0 ? (
            <div className="bar-chart">
              {stats.by_source.map((item, i) => {
                const maxCount = Math.max(...stats.by_source.map(s => s.count));
                const percentage = maxCount > 0 ? (item.count / maxCount) * 100 : 0;
                return (
                  <div key={item.source} className="bar-row" style={{ animationDelay: `${400 + i * 50}ms` }}>
                    <span className="bar-label">{item.source}</span>
                    <div className="bar-track">
                      <div
                        className="bar-fill"
                        style={{ width: `${percentage}%` }}
                        data-source={item.source.toLowerCase()}
                      />
                    </div>
                    <span className="bar-value">{item.count.toLocaleString()}</span>
                  </div>
                );
              })}
            </div>
          ) : (
            <div className="empty-chart">
              <span className="empty-chart-icon"><FontAwesomeIcon icon={faChartBar} /></span>
              <p>No suppression data yet</p>
              <small>Data will appear as suppressions are added</small>
            </div>
          )}
        </div>

        {/* By Reason Chart */}
        <div className="chart-card">
          <h4><FontAwesomeIcon icon={faChartPie} /> Suppressions by Reason</h4>
          {stats.by_reason.length > 0 ? (
            <div className="donut-chart-container">
              <DonutChart data={stats.by_reason} />
              <div className="donut-legend">
                {stats.by_reason.map((item, i) => (
                  <div key={item.reason} className="legend-item" style={{ animationDelay: `${450 + i * 50}ms` }}>
                    <span className={`legend-color reason-${i}`} />
                    <span className="legend-label">{item.reason}</span>
                    <span className="legend-value">{item.count.toLocaleString()}</span>
                  </div>
                ))}
              </div>
            </div>
          ) : (
            <div className="empty-chart">
              <span className="empty-chart-icon"><FontAwesomeIcon icon={faChartPie} /></span>
              <p>No reason breakdown yet</p>
              <small>Data will appear as suppressions are added</small>
            </div>
          )}
        </div>
      </div>

      {/* Recent Lists & Activity */}
      <div className="info-row" style={{ animationDelay: '400ms' }}>
        {/* Recent Lists */}
        <div className="info-card">
          <div className="info-card-header">
            <h4><FontAwesomeIcon icon={faList} /> Recent Suppression Lists</h4>
            <button className="view-all-btn" onClick={() => onNavigate('lists')}>
              View All <FontAwesomeIcon icon={faArrowRight} />
            </button>
          </div>
          <div className="lists-preview">
            {lists.slice(0, 5).map((list, i) => (
              <div
                key={list.id}
                className="list-preview-item"
                style={{ animationDelay: `${500 + i * 50}ms` }}
                onClick={() => {
                  onNavigate('entries', list);
                }}
              >
                <div className="list-preview-info">
                  <strong>{list.name}</strong>
                  <span className="list-meta">
                    {(list.entry_count || 0).toLocaleString()} entries • {list.source}
                  </span>
                </div>
                <div className="list-preview-status">
                  <span className={`status-indicator ${list.status}`} />
                </div>
              </div>
            ))}
            {lists.length === 0 && (
              <div className="empty-state-mini">
                <p>No suppression lists yet</p>
                <button onClick={() => onNavigate('create')}>Create your first list</button>
              </div>
            )}
          </div>
        </div>

        {/* System Health */}
        <div className="info-card health-card">
          <h4><FontAwesomeIcon icon={faHeart} /> System Health</h4>
          <div className="health-metrics">
            <div className="health-metric">
              <div className="health-metric-header">
                <span>Bloom Filter Memory</span>
                <span className="health-value good">Optimal</span>
              </div>
              <div className="health-bar">
                <div className="health-bar-fill" style={{ width: '35%' }} />
              </div>
              <span className="health-detail">~280 MB for 10M records</span>
            </div>

            <div className="health-metric">
              <div className="health-metric-header">
                <span>Lookup Latency</span>
                <span className="health-value good">&lt;1ms</span>
              </div>
              <div className="health-bar">
                <div className="health-bar-fill excellent" style={{ width: '15%' }} />
              </div>
              <span className="health-detail">O(1) Bloom + O(log n) verify</span>
            </div>

            <div className="health-metric">
              <div className="health-metric-header">
                <span>Optizmo Sync</span>
                <span className="health-value good">Connected</span>
              </div>
              <div className="health-bar">
                <div className="health-bar-fill syncing" style={{ width: '100%' }} />
              </div>
              <span className="health-detail">Daily delta at 2:00 AM UTC</span>
            </div>

            <div className="health-metric">
              <div className="health-metric-header">
                <span>Lists Updated (24h)</span>
                <span className="health-value">{stats.lists_updated_24h}</span>
              </div>
            </div>
          </div>
        </div>
      </div>

      {/* How It Works */}
      <div className="how-it-works" style={{ animationDelay: '500ms' }}>
        <h4><FontAwesomeIcon icon={faWrench} /> How Suppression Works</h4>
        <div className="how-steps">
          <div className="how-step">
            <div className="how-step-number">1</div>
            <div className="how-step-content">
              <strong>Email Check</strong>
              <p>Before sending, each recipient is checked against all active suppression lists</p>
            </div>
          </div>
          <div className="how-step-arrow"><FontAwesomeIcon icon={faArrowRight} /></div>
          <div className="how-step">
            <div className="how-step-number">2</div>
            <div className="how-step-content">
              <strong>Bloom Filter</strong>
              <p>O(1) probabilistic check eliminates 99% of non-matches instantly</p>
            </div>
          </div>
          <div className="how-step-arrow"><FontAwesomeIcon icon={faArrowRight} /></div>
          <div className="how-step">
            <div className="how-step-number">3</div>
            <div className="how-step-content">
              <strong>Verification</strong>
              <p>Bloom positives verified against sorted MD5 hashes for 100% accuracy</p>
            </div>
          </div>
          <div className="how-step-arrow"><FontAwesomeIcon icon={faArrowRight} /></div>
          <div className="how-step">
            <div className="how-step-number">4</div>
            <div className="how-step-content">
              <strong>Protection</strong>
              <p>Suppressed emails are excluded, protecting your sender reputation</p>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// LISTS MANAGER
// ============================================================================

interface ListsManagerProps {
  lists: SuppressionList[];
  onNavigate: (view: ViewMode, list?: SuppressionList) => void;
  onRefresh: () => void;
  animateIn: boolean;
}

const SuppressionListsManager: React.FC<ListsManagerProps> = ({ lists, onNavigate, onRefresh, animateIn }) => {
  const [searchQuery, setSearchQuery] = useState('');
  const [sortBy, setSortBy] = useState<'name' | 'count' | 'date'>('date');
  const [deleteConfirm, setDeleteConfirm] = useState<string | null>(null);

  const filteredLists = lists
    .filter(l => l.name.toLowerCase().includes(searchQuery.toLowerCase()))
    .sort((a, b) => {
      if (sortBy === 'name') return a.name.localeCompare(b.name);
      if (sortBy === 'count') return (b.entry_count || 0) - (a.entry_count || 0);
      return new Date(b.created_at).getTime() - new Date(a.created_at).getTime();
    });

  const handleDelete = async (id: string) => {
    try {
      await fetch(`/api/mailing/suppression-lists/${id}`, { method: 'DELETE' });
      setDeleteConfirm(null);
      onRefresh();
    } catch (err) {
      console.error('Delete failed:', err);
    }
  };

  return (
    <div className={`lists-manager ${animateIn ? 'animate-in' : ''}`}>
      <div className="manager-header">
        <div className="manager-title">
          <h2><FontAwesomeIcon icon={faList} /> Suppression Lists</h2>
          <p>Create, edit, and manage your suppression lists</p>
        </div>
        <div className="manager-actions">
          <button className="btn-secondary" onClick={() => onNavigate('bulk-upload')}>
            <FontAwesomeIcon icon={faUpload} /> Bulk Upload
          </button>
          <button className="btn-primary" onClick={() => onNavigate('create')}>
            <FontAwesomeIcon icon={faPlus} /> Create New List
          </button>
        </div>
      </div>

      {/* Filters */}
      <div className="filters-bar">
        <div className="search-box">
          <span className="search-icon"><FontAwesomeIcon icon={faSearch} /></span>
          <input
            type="text"
            placeholder="Search lists..."
            value={searchQuery}
            onChange={e => setSearchQuery(e.target.value)}
          />
        </div>
        <div className="sort-options">
          <label>Sort by:</label>
          <select value={sortBy} onChange={e => setSortBy(e.target.value as any)}>
            <option value="date">Recently Updated</option>
            <option value="name">Name</option>
            <option value="count">Entry Count</option>
          </select>
        </div>
      </div>

      {/* Lists Grid */}
      <div className="lists-grid">
        {filteredLists.map((list, i) => (
          <div
            key={list.id}
            className="list-card"
            style={{ animationDelay: `${i * 50}ms` }}
          >
            <div className="list-card-header">
              <div className="list-icon">
                <FontAwesomeIcon icon={list.source === 'optizmo' ? faBolt : faList} />
              </div>
              <div className="list-info">
                <h3>{list.name}</h3>
                <span className="list-source">{list.source}</span>
              </div>
              <span className={`list-status-badge ${list.status}`}>
                {list.status}
              </span>
            </div>

            {list.description && (
              <p className="list-description">{list.description}</p>
            )}

            <div className="list-stats">
              <div className="list-stat">
                <span className="stat-number">{(list.entry_count || 0).toLocaleString()}</span>
                <span className="stat-label">Entries</span>
              </div>
              <div className="list-stat">
                <span className="stat-number">{formatTimeAgo(list.created_at)}</span>
                <span className="stat-label">Created</span>
              </div>
              {list.last_sync_at && (
                <div className="list-stat">
                  <span className="stat-number">{formatTimeAgo(list.last_sync_at)}</span>
                  <span className="stat-label">Last Sync</span>
                </div>
              )}
            </div>

            <div className="list-card-actions">
              <button
                className="action-btn view"
                onClick={() => onNavigate('entries', list)}
              >
                <FontAwesomeIcon icon={faEye} /> View Entries
              </button>
              <button
                className="action-btn edit"
                onClick={() => onNavigate('edit', list)}
              >
                <FontAwesomeIcon icon={faPencilAlt} /> Edit
              </button>
              <button
                className="action-btn delete"
                onClick={() => setDeleteConfirm(list.id)}
              >
                <FontAwesomeIcon icon={faTrash} /> Delete
              </button>
            </div>
          </div>
        ))}
      </div>

      {filteredLists.length === 0 && (
        <div className="empty-state">
          <span className="empty-icon"><FontAwesomeIcon icon={faInbox} /></span>
          <h3>No suppression lists found</h3>
          <p>
            {searchQuery
              ? 'Try a different search term'
              : 'Create your first suppression list to protect your sender reputation'}
          </p>
          {!searchQuery && (
            <button className="btn-primary" onClick={() => onNavigate('create')}>
              Create First List
            </button>
          )}
        </div>
      )}

      {/* Delete Confirmation Modal */}
      {deleteConfirm && (
        <div className="modal-overlay" onClick={() => setDeleteConfirm(null)}>
          <div className="modal delete-modal" onClick={e => e.stopPropagation()}>
            <div className="modal-icon danger"><FontAwesomeIcon icon={faExclamationTriangle} /></div>
            <h3>Delete Suppression List?</h3>
            <p>
              This will permanently delete the list and all {lists.find(l => l.id === deleteConfirm)?.entry_count?.toLocaleString() || 0} entries.
              This action cannot be undone.
            </p>
            <div className="modal-actions">
              <button className="btn-secondary" onClick={() => setDeleteConfirm(null)}>
                Cancel
              </button>
              <button className="btn-danger" onClick={() => handleDelete(deleteConfirm)}>
                Delete List
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// ============================================================================
// CREATE LIST
// ============================================================================

interface CreateListProps {
  onCancel: () => void;
  onSuccess: () => void;
  animateIn: boolean;
}

const CreateSuppressionList: React.FC<CreateListProps> = ({ onCancel, onSuccess, animateIn }) => {
  const [formData, setFormData] = useState({
    name: '',
    description: '',
    source: 'manual',
  });
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formData.name.trim()) {
      setError('Name is required');
      return;
    }

    setSaving(true);
    setError(null);

    try {
      const res = await fetch('/api/mailing/suppression-lists', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(formData),
      });

      if (res.ok) {
        onSuccess();
      } else {
        const data = await res.json();
        setError(data.error || 'Failed to create list');
      }
    } catch (err) {
      setError('Network error. Please try again.');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className={`create-list-form ${animateIn ? 'animate-in' : ''}`}>
      <div className="form-header">
        <h2><FontAwesomeIcon icon={faPlus} /> Create New Suppression List</h2>
        <p>Add a new list to manage suppressed email addresses</p>
      </div>

      <form onSubmit={handleSubmit}>
        {error && <div className="form-error">{error}</div>}

        <div className="form-group">
          <label>List Name *</label>
          <input
            type="text"
            placeholder="e.g., Optizmo Master List"
            value={formData.name}
            onChange={e => setFormData(p => ({ ...p, name: e.target.value }))}
            autoFocus
          />
        </div>

        <div className="form-group">
          <label>Description</label>
          <textarea
            placeholder="What is this list for?"
            value={formData.description}
            onChange={e => setFormData(p => ({ ...p, description: e.target.value }))}
            rows={3}
          />
        </div>

        <div className="form-group">
          <label>Source</label>
          <select
            value={formData.source}
            onChange={e => setFormData(p => ({ ...p, source: e.target.value }))}
          >
            <option value="manual">Manual</option>
            <option value="optizmo">Optizmo</option>
            <option value="import">Import</option>
            <option value="webhook">Webhook</option>
            <option value="bounce">Bounce Handler</option>
            <option value="complaint">Complaint Handler</option>
          </select>
        </div>

        <div className="form-actions">
          <button type="button" className="btn-secondary" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="btn-primary" disabled={saving}>
            {saving ? 'Creating...' : 'Create List'}
          </button>
        </div>
      </form>
    </div>
  );
};

// ============================================================================
// EDIT LIST
// ============================================================================

interface EditListProps {
  list: SuppressionList;
  onCancel: () => void;
  onSuccess: () => void;
  animateIn: boolean;
}

const EditSuppressionList: React.FC<EditListProps> = ({ list, onCancel, onSuccess, animateIn }) => {
  const [formData, setFormData] = useState({
    name: list.name,
    description: list.description || '',
    source: list.source,
  });
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formData.name.trim()) {
      setError('Name is required');
      return;
    }

    setSaving(true);
    setError(null);

    try {
      const res = await fetch(`/api/mailing/suppression-lists/${list.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(formData),
      });

      if (res.ok) {
        onSuccess();
      } else {
        const data = await res.json();
        setError(data.error || 'Failed to update list');
      }
    } catch (err) {
      setError('Network error. Please try again.');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className={`create-list-form ${animateIn ? 'animate-in' : ''}`}>
      <div className="form-header">
        <h2><FontAwesomeIcon icon={faPencilAlt} /> Edit Suppression List</h2>
        <p>Update list details for "{list.name}"</p>
      </div>

      <form onSubmit={handleSubmit}>
        {error && <div className="form-error">{error}</div>}

        <div className="form-group">
          <label>List Name *</label>
          <input
            type="text"
            value={formData.name}
            onChange={e => setFormData(p => ({ ...p, name: e.target.value }))}
          />
        </div>

        <div className="form-group">
          <label>Description</label>
          <textarea
            value={formData.description}
            onChange={e => setFormData(p => ({ ...p, description: e.target.value }))}
            rows={3}
          />
        </div>

        <div className="form-group">
          <label>Source</label>
          <select
            value={formData.source}
            onChange={e => setFormData(p => ({ ...p, source: e.target.value }))}
          >
            <option value="manual">Manual</option>
            <option value="optizmo">Optizmo</option>
            <option value="import">Import</option>
            <option value="webhook">Webhook</option>
            <option value="bounce">Bounce Handler</option>
            <option value="complaint">Complaint Handler</option>
          </select>
        </div>

        <div className="form-info">
          <p><strong>Created:</strong> {new Date(list.created_at).toLocaleString()}</p>
          <p><strong>Entries:</strong> {(list.entry_count || 0).toLocaleString()}</p>
          {list.last_sync_at && (
            <p><strong>Last Sync:</strong> {new Date(list.last_sync_at).toLocaleString()}</p>
          )}
        </div>

        <div className="form-actions">
          <button type="button" className="btn-secondary" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="btn-primary" disabled={saving}>
            {saving ? 'Saving...' : 'Save Changes'}
          </button>
        </div>
      </form>
    </div>
  );
};

// ============================================================================
// ENTRIES VIEW
// ============================================================================

interface EntriesProps {
  list: SuppressionList;
  onBack: () => void;
  animateIn: boolean;
}

const SuppressionEntries: React.FC<EntriesProps> = ({ list, onBack, animateIn }) => {
  const [entries, setEntries] = useState<SuppressionEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [searchQuery, setSearchQuery] = useState('');
  const [addEmail, setAddEmail] = useState('');
  const [addReason, setAddReason] = useState('Manual addition');
  const [showAddForm, setShowAddForm] = useState(false);

  useEffect(() => {
    fetchEntries();
  }, [list.id]);

  const fetchEntries = async () => {
    try {
      const res = await fetch(`/api/mailing/suppression-lists/${list.id}/entries`);
      const data = await res.json();
      setEntries(data.entries || []);
    } catch (err) {
      console.error('Failed to fetch entries:', err);
    } finally {
      setLoading(false);
    }
  };

  const handleAddEntry = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!addEmail.trim()) return;

    try {
      const res = await fetch(`/api/mailing/suppression-lists/${list.id}/entries`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: addEmail, reason: addReason }),
      });

      if (res.ok) {
        setAddEmail('');
        setShowAddForm(false);
        fetchEntries();
      }
    } catch (err) {
      console.error('Failed to add entry:', err);
    }
  };

  const handleRemoveEntry = async (entryId: string) => {
    try {
      await fetch(`/api/mailing/suppression-lists/${list.id}/entries/${entryId}`, {
        method: 'DELETE',
      });
      fetchEntries();
    } catch (err) {
      console.error('Failed to remove entry:', err);
    }
  };

  const filteredEntries = entries.filter(e =>
    e.email.toLowerCase().includes(searchQuery.toLowerCase())
  );

  return (
    <div className={`entries-view ${animateIn ? 'animate-in' : ''}`}>
      <div className="entries-header">
        <button className="back-btn" onClick={onBack}>
          <FontAwesomeIcon icon={faArrowLeft} /> Back to Lists
        </button>
        <div className="entries-title">
          <h2><FontAwesomeIcon icon={faEnvelope} /> {list.name}</h2>
          <p>{(list.entry_count || 0).toLocaleString()} suppressed emails</p>
        </div>
        <button className="btn-primary" onClick={() => setShowAddForm(true)}>
          <FontAwesomeIcon icon={faPlus} /> Add Entry
        </button>
      </div>

      {showAddForm && (
        <form className="add-entry-form" onSubmit={handleAddEntry}>
          <input
            type="email"
            placeholder="Email address to suppress"
            value={addEmail}
            onChange={e => setAddEmail(e.target.value)}
            autoFocus
          />
          <select value={addReason} onChange={e => setAddReason(e.target.value)}>
            <option value="Manual addition">Manual addition</option>
            <option value="User request">User request</option>
            <option value="Complaint">Complaint</option>
            <option value="Hard bounce">Hard bounce</option>
            <option value="Spam trap">Spam trap</option>
          </select>
          <button type="submit" className="btn-primary">Add</button>
          <button type="button" className="btn-secondary" onClick={() => setShowAddForm(false)}>
            Cancel
          </button>
        </form>
      )}

      <div className="entries-search">
        <span className="search-icon"><FontAwesomeIcon icon={faSearch} /></span>
        <input
          type="text"
          placeholder="Search emails..."
          value={searchQuery}
          onChange={e => setSearchQuery(e.target.value)}
        />
        <span className="search-count">
          {filteredEntries.length.toLocaleString()} / {entries.length.toLocaleString()}
        </span>
      </div>

      {loading ? (
        <div className="loading-container">
          <div className="loading-spinner"></div>
          <p>Loading entries...</p>
        </div>
      ) : (
        <div className="entries-table">
          <table>
            <thead>
              <tr>
                <th>Email</th>
                <th>MD5 Hash</th>
                <th>Reason</th>
                <th>Added</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {filteredEntries.slice(0, 100).map(entry => (
                <tr key={entry.id}>
                  <td className="email-cell">{entry.email}</td>
                  <td className="hash-cell">{entry.md5_hash?.substring(0, 12)}...</td>
                  <td>
                    <span className={`reason-badge ${entry.reason.toLowerCase().replace(/\s+/g, '-')}`}>
                      {entry.reason}
                    </span>
                  </td>
                  <td>{formatTimeAgo(entry.created_at)}</td>
                  <td>
                    <button
                      className="btn-danger btn-sm"
                      onClick={() => handleRemoveEntry(entry.id)}
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {filteredEntries.length > 100 && (
            <div className="table-footer">
              Showing 100 of {filteredEntries.length.toLocaleString()} entries.
              Use search to narrow down results.
            </div>
          )}
          {filteredEntries.length === 0 && (
            <div className="empty-state-mini">
              {searchQuery ? 'No matching emails found' : 'No entries in this list yet'}
            </div>
          )}
        </div>
      )}
    </div>
  );
};

// ============================================================================
// OPTIZMO MANAGER
// ============================================================================

interface OptizmoManagerProps {
  onBack: () => void;
  onRefresh: () => void;
  animateIn: boolean;
}

const OptizmoManager: React.FC<OptizmoManagerProps> = ({ onBack, onRefresh, animateIn }) => {
  const [config, setConfig] = useState({
    api_key: '',
    sync_enabled: true,
    sync_time: '02:00',
    lists: [] as OptizmoList[],
  });
  const [status, setStatus] = useState<any>(null);
  const [syncing, setSyncing] = useState(false);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    fetchOptizmoData();
  }, []);

  const fetchOptizmoData = async () => {
    try {
      const [configRes, statusRes, listsRes] = await Promise.all([
        fetch('/api/mailing/optizmo/config'),
        fetch('/api/mailing/optizmo/status'),
        fetch('/api/mailing/optizmo/lists'),
      ]);

      const configData = await configRes.json().catch(() => ({}));
      const statusData = await statusRes.json().catch(() => ({}));
      const listsData = await listsRes.json().catch(() => ({ lists: [] }));

      setConfig(prev => ({
        ...prev,
        ...configData,
        lists: listsData.lists || [],
      }));
      setStatus(statusData);
    } catch (err) {
      console.error('Failed to fetch Optizmo data:', err);
    } finally {
      setLoading(false);
    }
  };

  const handleSync = async () => {
    setSyncing(true);
    try {
      await fetch('/api/mailing/optizmo/sync', { method: 'POST' });
      await fetchOptizmoData();
      onRefresh();
    } catch (err) {
      console.error('Sync failed:', err);
    } finally {
      setSyncing(false);
    }
  };

  const handleSaveConfig = async () => {
    try {
      await fetch('/api/mailing/optizmo/config', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          sync_enabled: config.sync_enabled,
          sync_time: config.sync_time,
        }),
      });
      alert('Configuration saved!');
    } catch (err) {
      console.error('Save failed:', err);
    }
  };

  return (
    <div className={`optizmo-manager ${animateIn ? 'animate-in' : ''}`}>
      <div className="optizmo-header">
        <button className="back-btn" onClick={onBack}>
          <FontAwesomeIcon icon={faArrowLeft} /> Back to Dashboard
        </button>
        <div className="optizmo-title">
          <h2><FontAwesomeIcon icon={faSync} /> Optizmo Integration</h2>
          <p>Configure daily delta sync with Optizmo suppression API</p>
        </div>
      </div>

      {loading ? (
        <div className="loading-container">
          <div className="loading-spinner"></div>
          <p>Loading Optizmo configuration...</p>
        </div>
      ) : (
        <>
          {/* Status Card */}
          <div className="optizmo-status-card">
            <div className="status-header">
              <span className={`status-indicator ${status?.connected ? 'connected' : 'disconnected'}`} />
              <span className="status-text">
                {status?.connected ? 'Connected to Optizmo API' : 'Not Connected'}
              </span>
            </div>
            {status?.last_sync && (
              <div className="status-detail">
                <strong>Last Sync:</strong> {new Date(status.last_sync).toLocaleString()}
              </div>
            )}
            {status?.records_synced !== undefined && (
              <div className="status-detail">
                <strong>Records Synced:</strong> {status.records_synced.toLocaleString()}
              </div>
            )}
            <button
              className="btn-primary sync-btn"
              onClick={handleSync}
              disabled={syncing}
            >
              {syncing ? (
                <>
                  <FontAwesomeIcon icon={faSpinner} spin /> Syncing...
                </>
              ) : (
                <><FontAwesomeIcon icon={faSync} /> Sync Now</>
              )}
            </button>
          </div>

          {/* Configuration */}
          <div className="optizmo-config">
            <h3><FontAwesomeIcon icon={faCog} /> Sync Configuration</h3>
            <div className="config-form">
              <div className="config-row">
                <label>
                  <input
                    type="checkbox"
                    checked={config.sync_enabled}
                    onChange={e => setConfig(p => ({ ...p, sync_enabled: e.target.checked }))}
                  />
                  Enable automatic daily sync
                </label>
              </div>
              <div className="config-row">
                <label>Sync Time (UTC)</label>
                <input
                  type="time"
                  value={config.sync_time}
                  onChange={e => setConfig(p => ({ ...p, sync_time: e.target.value }))}
                />
              </div>
              <button className="btn-secondary" onClick={handleSaveConfig}>
                Save Configuration
              </button>
            </div>
          </div>

          {/* Optizmo Lists */}
          <div className="optizmo-lists">
            <h3><FontAwesomeIcon icon={faList} /> Synced Optizmo Lists</h3>
            {config.lists.length > 0 ? (
              <div className="optizmo-lists-grid">
                {config.lists.map(list => (
                  <div key={list.id} className="optizmo-list-card">
                    <div className="optizmo-list-header">
                      <strong>{list.name}</strong>
                      <span className={`sync-badge ${list.sync_enabled ? 'enabled' : 'disabled'}`}>
                        {list.sync_enabled ? 'Sync On' : 'Sync Off'}
                      </span>
                    </div>
                    <div className="optizmo-list-stats">
                      <span>{list.entry_count?.toLocaleString() || 0} entries</span>
                      {list.last_delta_at && (
                        <span>Last delta: {formatTimeAgo(list.last_delta_at)}</span>
                      )}
                    </div>
                  </div>
                ))}
              </div>
            ) : (
              <div className="empty-state-mini">
                <p>No Optizmo lists configured yet</p>
                <p className="hint">Lists will appear here after your first sync</p>
              </div>
            )}
          </div>

          {/* How Delta Sync Works */}
          <div className="how-delta-works">
            <h3><FontAwesomeIcon icon={faBook} /> How Delta Sync Works</h3>
            <ol>
              <li>
                <strong>Daily Check:</strong> At the configured time, the system calls Optizmo's
                <code>prepare-download</code> API
              </li>
              <li>
                <strong>Delta Download:</strong> Only new/changed suppressions since last sync are downloaded
              </li>
              <li>
                <strong>S3 Storage:</strong> Files are stored in S3 with logical naming for fast lookup
              </li>
              <li>
                <strong>Bloom Update:</strong> New MD5 hashes are added to the Bloom filter for O(1) lookups
              </li>
            </ol>
          </div>
        </>
      )}
    </div>
  );
};

// ============================================================================
// BULK UPLOAD
// ============================================================================

interface BulkUploadProps {
  lists: SuppressionList[];
  onCancel: () => void;
  onSuccess: () => void;
  animateIn: boolean;
}

const CHUNK_SIZE = 50 * 1024 * 1024; // 50 MB chunks (was 10 MB — fewer HTTP requests = faster upload)
const DIRECT_UPLOAD_THRESHOLD = 500 * 1024 * 1024; // 500 MB - use direct upload below this
const PARALLEL_CHUNK_UPLOADS = 4; // Upload 4 chunks concurrently for faster throughput

function formatFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${Math.round(seconds % 60)}s`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
}

interface ScheduledCampaign {
  id: string;
  name: string;
  status: string;
  scheduled_at?: string;
  total_recipients: number;
  sent_count: number;
}

const BulkUpload: React.FC<BulkUploadProps> = ({ lists, onCancel, onSuccess, animateIn }) => {
  const [selectedList, setSelectedList] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [uploading, setUploading] = useState(false);
  const [phase, setPhase] = useState<'idle' | 'uploading' | 'processing' | 'completed' | 'failed'>('idle');
  const [uploadProgress, setUploadProgress] = useState(0); // 0-100 for upload phase
  const [jobId, setJobId] = useState<string | null>(null);
  const [importProgress, setImportProgress] = useState<any>(null);
  const [error, setError] = useState<string | null>(null);
  const [scheduledCampaigns, setScheduledCampaigns] = useState<ScheduledCampaign[]>([]);
  const [showCampaignWarning, setShowCampaignWarning] = useState(false);
  const [cancellingCampaigns, setCancellingCampaigns] = useState(false);

  // Check for scheduled campaigns on mount
  useEffect(() => {
    const checkScheduled = async () => {
      try {
        const res = await fetch('/api/mailing/campaigns/scheduled');
        const data = await res.json();
        if (data.count > 0) {
          setScheduledCampaigns(data.campaigns || []);
          setShowCampaignWarning(true);
        }
      } catch {
        // Non-critical — proceed without warning
      }
    };
    checkScheduled();
  }, []);

  const handleCancelAllScheduled = async () => {
    setCancellingCampaigns(true);
    try {
      const res = await fetch('/api/mailing/campaigns/cancel-all-scheduled', { method: 'POST' });
      const data = await res.json();
      setScheduledCampaigns([]);
      setShowCampaignWarning(false);
      console.log('Cancelled campaigns:', data);
    } catch (err) {
      console.error('Failed to cancel campaigns:', err);
    } finally {
      setCancellingCampaigns(false);
    }
  };

  // Poll for processing progress
  useEffect(() => {
    if (!jobId || phase !== 'processing') return;

    const interval = setInterval(async () => {
      try {
        const res = await fetch(`/api/mailing/suppression-import/${jobId}/progress`);
        const data = await res.json();
        setImportProgress(data);

        if (data.status === 'completed') {
          setPhase('completed');
          clearInterval(interval);
          setTimeout(() => onSuccess(), 3000);
        } else if (data.status === 'failed') {
          setPhase('failed');
          setError(data.errors?.[0] || 'Processing failed');
          clearInterval(interval);
        }
      } catch {
        // Keep polling on transient errors
      }
    }, 1500);

    return () => clearInterval(interval);
  }, [jobId, phase, onSuccess]);

  const handleUpload = async () => {
    if (!file || !selectedList) return;

    setUploading(true);
    setError(null);
    setImportProgress(null);

    try {
      if (file.size <= DIRECT_UPLOAD_THRESHOLD) {
        // === DIRECT UPLOAD (small files, single request) ===
        setPhase('uploading');
        setUploadProgress(0);

        const formData = new FormData();
        formData.append('file', file);
        formData.append('list_id', selectedList);

        // Use XMLHttpRequest for upload progress
        const xhr = new XMLHttpRequest();
        const uploadPromise = new Promise<any>((resolve, reject) => {
          xhr.upload.onprogress = (e) => {
            if (e.lengthComputable) {
              setUploadProgress(Math.round((e.loaded / e.total) * 100));
            }
          };
          xhr.onload = () => {
            try {
              resolve(JSON.parse(xhr.responseText));
            } catch {
              reject(new Error('Invalid response'));
            }
          };
          xhr.onerror = () => reject(new Error('Upload failed'));
        });

        xhr.open('POST', '/api/mailing/suppression-import/direct');
        xhr.send(formData);

        const data = await uploadPromise;
        if (data.error) {
          throw new Error(data.error);
        }

        setJobId(data.job_id);
        setPhase('processing');
      } else {
        // === CHUNKED UPLOAD (large files, multi-request) ===
        setPhase('uploading');
        setUploadProgress(0);

        // Step 1: Init upload session
        const initRes = await fetch('/api/mailing/suppression-import/init', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            list_id: selectedList,
            filename: file.name,
            file_size: file.size,
          }),
        });
        const session = await initRes.json();
        if (session.error) throw new Error(session.error);

        const currentJobId = session.id;
        setJobId(currentJobId);

        // Step 2: Upload chunks in parallel batches for maximum throughput
        const totalChunks = session.total_chunks;
        let completedChunks = 0;

        // Upload chunks in parallel windows of PARALLEL_CHUNK_UPLOADS
        for (let windowStart = 0; windowStart < totalChunks; windowStart += PARALLEL_CHUNK_UPLOADS) {
          const windowEnd = Math.min(windowStart + PARALLEL_CHUNK_UPLOADS, totalChunks);
          const chunkPromises: Promise<void>[] = [];

          for (let i = windowStart; i < windowEnd; i++) {
            const start = i * CHUNK_SIZE;
            const end = Math.min(start + CHUNK_SIZE, file.size);
            const chunk = file.slice(start, end);

            chunkPromises.push(
              fetch(
                `/api/mailing/suppression-import/${currentJobId}/chunk?chunk=${i}`,
                { method: 'POST', body: chunk }
              ).then(async (res) => {
                const chunkData = await res.json();
                if (chunkData.error) throw new Error(chunkData.error);
                completedChunks++;
                setUploadProgress(Math.round((completedChunks / totalChunks) * 100));
              })
            );
          }

          // Wait for all chunks in this parallel window to complete
          await Promise.all(chunkPromises);
        }

        // Step 3: Trigger processing
        const procRes = await fetch(
          `/api/mailing/suppression-import/${currentJobId}/process`,
          { method: 'POST' }
        );
        const procData = await procRes.json();
        if (procData.error) throw new Error(procData.error);

        setPhase('processing');
      }
    } catch (err: any) {
      setPhase('failed');
      setError(err.message || 'Upload failed. Please try again.');
    } finally {
      setUploading(false);
    }
  };

  const handleReset = () => {
    setPhase('idle');
    setJobId(null);
    setImportProgress(null);
    setUploadProgress(0);
    setError(null);
    setFile(null);
    setUploading(false);
  };

  // Calculate overall progress percentage
  const overallProgress = (() => {
    if (phase === 'uploading') return Math.round(uploadProgress * 0.3); // Upload is 0-30%
    if (phase === 'processing' && importProgress?.total_lines > 0) {
      return 30 + Math.round((importProgress.processed_rows / importProgress.total_lines) * 70);
    }
    if (phase === 'completed') return 100;
    return 0;
  })();

  return (
    <div className={`bulk-upload ${animateIn ? 'animate-in' : ''}`}>
      <div className="form-header">
        <h2><FontAwesomeIcon icon={faUpload} /> Bulk Upload Suppressions</h2>
        <p>Import email addresses from a CSV or text file (supports files up to 10 GB)</p>
      </div>

      {/* Scheduled Campaign Safety Gate */}
      {showCampaignWarning && scheduledCampaigns.length > 0 && (
        <div className="form-error" style={{ 
          background: '#fef3c7', border: '1px solid #f59e0b', color: '#92400e',
          borderRadius: '8px', padding: '16px', marginBottom: '16px'
        }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '8px' }}>
            <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#f59e0b' }} />
            <strong>{scheduledCampaigns.length} Campaign{scheduledCampaigns.length > 1 ? 's' : ''} Currently Scheduled</strong>
          </div>
          <p style={{ margin: '0 0 8px 0', fontSize: '13px' }}>
            Uploading a suppression list while campaigns are scheduled could affect sending. 
            It's recommended to cancel all scheduled campaigns before importing.
          </p>
          <div style={{ fontSize: '12px', marginBottom: '12px' }}>
            {scheduledCampaigns.map(c => (
              <div key={c.id} style={{ padding: '4px 0', borderBottom: '1px solid #fde68a' }}>
                <strong>{c.name}</strong> — {c.status}
                {c.scheduled_at && <> (scheduled: {new Date(c.scheduled_at).toLocaleString()})</>}
              </div>
            ))}
          </div>
          <div style={{ display: 'flex', gap: '8px' }}>
            <button
              className="btn-danger"
              style={{ fontSize: '13px', padding: '6px 16px' }}
              onClick={handleCancelAllScheduled}
              disabled={cancellingCampaigns}
            >
              {cancellingCampaigns ? (
                <><FontAwesomeIcon icon={faSpinner} spin /> Cancelling...</>
              ) : (
                <><FontAwesomeIcon icon={faBan} /> Cancel All Scheduled Campaigns</>
              )}
            </button>
            <button
              className="btn-secondary"
              style={{ fontSize: '13px', padding: '6px 16px' }}
              onClick={() => setShowCampaignWarning(false)}
            >
              Dismiss — I'll proceed anyway
            </button>
          </div>
        </div>
      )}

      {/* Confirmation: No scheduled campaigns */}
      {!showCampaignWarning && scheduledCampaigns.length === 0 && phase === 'idle' && (
        <div style={{
          background: '#ecfdf5', border: '1px solid #10b981', color: '#065f46',
          borderRadius: '8px', padding: '12px 16px', marginBottom: '16px',
          display: 'flex', alignItems: 'center', gap: '8px', fontSize: '13px'
        }}>
          <FontAwesomeIcon icon={faCheckCircle} style={{ color: '#10b981' }} />
          <strong>No campaigns are scheduled.</strong> Safe to upload suppression list.
        </div>
      )}

      {phase === 'idle' && (
        <div className="upload-form">
          <div className="form-group">
            <label>Select Suppression List *</label>
            <select
              value={selectedList}
              onChange={e => setSelectedList(e.target.value)}
            >
              <option value="">Choose a list...</option>
              {lists.map(list => (
                <option key={list.id} value={list.id}>
                  {list.name} ({(list.entry_count || 0).toLocaleString()} entries)
                </option>
              ))}
            </select>
          </div>

          <div className="form-group">
            <label>Upload File *</label>
            <div className="file-upload-zone">
              <input
                type="file"
                accept=".csv,.txt,.gz"
                onChange={e => setFile(e.target.files?.[0] || null)}
                id="file-input"
              />
              <label htmlFor="file-input" className="file-upload-label">
                {file ? (
                  <>
                    <span className="file-icon"><FontAwesomeIcon icon={faFile} /></span>
                    <span className="file-name">{file.name}</span>
                    <span className="file-size">({formatFileSize(file.size)})</span>
                  </>
                ) : (
                  <>
                    <span className="upload-icon"><FontAwesomeIcon icon={faUpload} /></span>
                    <span>Click to select file or drag and drop</span>
                    <span className="file-hint">CSV or TXT file with one email per line (up to 10 GB)</span>
                  </>
                )}
              </label>
            </div>
            {file && file.size > DIRECT_UPLOAD_THRESHOLD && (
              <div style={{ marginTop: '8px', color: 'var(--text-muted)', fontSize: '12px' }}>
                <FontAwesomeIcon icon={faBolt} style={{ color: '#f59e0b' }} /> Large file detected — will use parallel chunked upload ({Math.ceil(file.size / CHUNK_SIZE)} chunks, {PARALLEL_CHUNK_UPLOADS} concurrent)
              </div>
            )}
          </div>

          <div className="form-actions">
            <button type="button" className="btn-secondary" onClick={onCancel}>
              Cancel
            </button>
            <button
              type="button"
              className="btn-primary"
              onClick={handleUpload}
              disabled={!file || !selectedList || uploading}
            >
              Upload & Import
            </button>
          </div>

          <div className="upload-hints">
            <h4><FontAwesomeIcon icon={faFile} /> File Format</h4>
            <ul>
              <li>One email address per line</li>
              <li>MD5 hashes (32 characters) are also supported</li>
              <li>Lines starting with # are treated as comments</li>
              <li>Duplicates are automatically removed</li>
              <li>Files up to <strong>10 GB</strong> supported with chunked upload</li>
            </ul>
          </div>
        </div>
      )}

      {(phase === 'uploading' || phase === 'processing') && (
        <div className="upload-progress-panel" style={{ padding: '24px 0' }}>
          <div style={{ textAlign: 'center', marginBottom: '20px' }}>
            <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: '32px', color: '#3b82f6', marginBottom: '12px' }} />
            <h3 style={{ margin: '0 0 4px 0' }}>
              {phase === 'uploading' ? 'Uploading File...' : 'Processing Suppressions...'}
            </h3>
            {file && phase === 'uploading' && (
              <p style={{ color: 'var(--text-muted)', margin: 0 }}>
                {formatFileSize(file.size)} — {uploadProgress}% uploaded
              </p>
            )}
            {phase === 'processing' && importProgress && (
              <p style={{ color: 'var(--text-muted)', margin: 0 }}>
                {(importProgress.processed_rows || 0).toLocaleString()} rows processed
                {importProgress.rows_per_second > 0 && (
                  <> — {Math.round(importProgress.rows_per_second).toLocaleString()} rows/sec</>
                )}
                {importProgress.estimated_eta_seconds > 0 && (
                  <> — ETA: {formatDuration(importProgress.estimated_eta_seconds)}</>
                )}
              </p>
            )}
          </div>

          <div className="upload-progress">
            <div className="progress-bar">
              <div
                className="progress-fill"
                style={{
                  width: `${overallProgress}%`,
                  transition: 'width 0.5s ease',
                  background: 'linear-gradient(90deg, #3b82f6, #10b981)',
                }}
              />
            </div>
            <span className="progress-text">{overallProgress}%</span>
          </div>

          {phase === 'processing' && importProgress && (
            <div className="result-stats" style={{ marginTop: '20px' }}>
              <div className="result-stat">
                <span className="stat-value">{(importProgress.imported_count || 0).toLocaleString()}</span>
                <span className="stat-label">Imported</span>
              </div>
              <div className="result-stat">
                <span className="stat-value">{(importProgress.duplicate_count || 0).toLocaleString()}</span>
                <span className="stat-label">Duplicates</span>
              </div>
              <div className="result-stat">
                <span className="stat-value">{(importProgress.invalid_count || 0).toLocaleString()}</span>
                <span className="stat-label">Invalid</span>
              </div>
            </div>
          )}
        </div>
      )}

      {phase === 'completed' && importProgress && (
        <div className="upload-result">
          <div className="result-success">
            <span className="result-icon"><FontAwesomeIcon icon={faCheckCircle} /></span>
            <h3>Import Complete!</h3>
            <div className="result-stats">
              <div className="result-stat">
                <span className="stat-value">{(importProgress.imported_count || 0).toLocaleString()}</span>
                <span className="stat-label">Imported</span>
              </div>
              <div className="result-stat">
                <span className="stat-value">{(importProgress.duplicate_count || 0).toLocaleString()}</span>
                <span className="stat-label">Duplicates Skipped</span>
              </div>
              <div className="result-stat">
                <span className="stat-value">{(importProgress.invalid_count || 0).toLocaleString()}</span>
                <span className="stat-label">Invalid</span>
              </div>
            </div>
            {importProgress.rows_per_second > 0 && (
              <p style={{ color: 'var(--text-muted)', marginTop: '8px' }}>
                {(importProgress.total_lines || 0).toLocaleString()} lines processed at{' '}
                {Math.round(importProgress.rows_per_second).toLocaleString()} rows/sec
              </p>
            )}
            <p>Redirecting to lists...</p>
          </div>
        </div>
      )}

      {phase === 'failed' && (
        <div className="upload-result">
          <div className="result-error">
            <span className="result-icon"><FontAwesomeIcon icon={faTimesCircle} /></span>
            <h3>Import Failed</h3>
            <p>{error || 'An unexpected error occurred.'}</p>
            <button className="btn-primary" onClick={handleReset}>
              Try Again
            </button>
          </div>
        </div>
      )}
    </div>
  );
};

// ============================================================================
// HELPER COMPONENTS
// ============================================================================

interface AnimatedNumberProps {
  value: number;
}

const AnimatedNumber: React.FC<AnimatedNumberProps> = ({ value }) => {
  const [displayValue, setDisplayValue] = useState(0);

  useEffect(() => {
    const duration = 1000;
    const steps = 30;
    const increment = value / steps;
    let current = 0;

    const timer = setInterval(() => {
      current += increment;
      if (current >= value) {
        setDisplayValue(value);
        clearInterval(timer);
      } else {
        setDisplayValue(Math.floor(current));
      }
    }, duration / steps);

    return () => clearInterval(timer);
  }, [value]);

  return <span>{displayValue.toLocaleString()}</span>;
};

interface DonutChartProps {
  data: { reason: string; count: number }[];
}

const DonutChart: React.FC<DonutChartProps> = ({ data }) => {
  const total = data.reduce((acc, item) => acc + item.count, 0);
  
  if (total === 0) {
    return (
      <div className="donut-chart empty">
        <div className="donut-center">
          <span className="donut-total">0</span>
          <span className="donut-label">Total</span>
        </div>
      </div>
    );
  }

  let cumulativePercent = 0;
  const colors = ['#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6'];

  const segments = data.map((item, i) => {
    const percent = (item.count / total) * 100;
    const startPercent = cumulativePercent;
    cumulativePercent += percent;

    return {
      color: colors[i % colors.length],
      offset: startPercent,
      percent,
    };
  });

  return (
    <div className="donut-chart">
      <svg viewBox="0 0 36 36" className="donut-svg">
        {segments.map((segment, i) => (
          <circle
            key={i}
            cx="18"
            cy="18"
            r="15.9155"
            fill="none"
            stroke={segment.color}
            strokeWidth="3"
            strokeDasharray={`${segment.percent} ${100 - segment.percent}`}
            strokeDashoffset={`${25 - segment.offset}`}
            className="donut-segment"
            style={{ animationDelay: `${i * 100}ms` }}
          />
        ))}
      </svg>
      <div className="donut-center">
        <span className="donut-total">{total.toLocaleString()}</span>
        <span className="donut-label">Total</span>
      </div>
    </div>
  );
};

// ============================================================================
// HELPER FUNCTIONS
// ============================================================================

function formatTimeAgo(dateString: string): string {
  if (!dateString) return 'Never';
  
  const date = new Date(dateString);
  const now = new Date();
  const seconds = Math.floor((now.getTime() - date.getTime()) / 1000);

  if (seconds < 60) return 'Just now';
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  if (seconds < 604800) return `${Math.floor(seconds / 86400)}d ago`;
  return date.toLocaleDateString();
}

export default SuppressionPortal;
