import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faListUl,
  faUsers,
  faCrosshairs,
  faPlus,
  faUpload,
  faChartBar,
  faArrowRight,
  faArrowUp,
  faSearch,
  faEye,
  faPencilAlt,
  faTrash,
  faCheckCircle,
  faTimesCircle,
  faSpinner,
  faUserPlus,
  faClock,
  faChartLine,
  faLayerGroup,
  faTimes,
  faSave,
  faArrowLeft,
  faBolt,
  faDownload,
  faFileAlt,
  faRocket,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { ChunkedUploader } from '../ChunkedUploader';
import './ListPortal.css';

// ============================================================================
// TYPES
// ============================================================================

interface List {
  id: string;
  name: string;
  description?: string;
  subscriber_count: number;
  active_count: number;
  status: 'active' | 'paused' | 'archived';
  opt_in_type: 'single' | 'double';
  default_from_name?: string;
  default_from_email?: string;
  created_at: string;
  updated_at: string;
}

interface Segment {
  id: string;
  name: string;
  description?: string;
  list_id?: string;
  list_name?: string;
  segment_type: 'dynamic' | 'static';
  subscriber_count: number;
  status: 'active' | 'draft' | 'archived';
  conditions?: SegmentCondition[];
  created_at: string;
  updated_at: string;
}

interface SegmentCondition {
  field: string;
  operator: string;
  value: string;
}

interface Subscriber {
  id: string;
  email: string;
  first_name?: string;
  last_name?: string;
  status: 'confirmed' | 'unconfirmed' | 'unsubscribed' | 'bounced' | 'complained';
  engagement_score: number;
  total_opens: number;
  total_clicks: number;
  subscribed_at: string;
  last_open_at?: string;
  last_click_at?: string;
}

interface DashboardStats {
  total_lists: number;
  total_subscribers: number;
  total_segments: number;
  active_subscribers: number;
  new_subscribers_24h: number;
  new_subscribers_7d: number;
  unsubscribes_7d: number;
  avg_engagement: number;
  lists_by_status: { status: string; count: number }[];
  top_lists: { id: string; name: string; subscriber_count: number; active_count: number }[];
  recent_activity: { action: string; details: string; timestamp: string }[];
}

type ViewMode = 'dashboard' | 'lists' | 'create-list' | 'edit-list' | 'subscribers' | 
                'segments' | 'create-segment' | 'edit-segment' | 'segment-preview' |
                'import' | 'export';

// ============================================================================
// HELPER COMPONENTS
// ============================================================================

const AnimatedNumber: React.FC<{ value: number }> = ({ value }) => {
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

  return <>{displayValue.toLocaleString()}</>;
};

const formatTimeAgo = (dateStr: string): string => {
  if (!dateStr) return 'Never';
  const date = new Date(dateStr);
  const now = new Date();
  const diff = now.getTime() - date.getTime();
  const minutes = Math.floor(diff / 60000);
  const hours = Math.floor(diff / 3600000);
  const days = Math.floor(diff / 86400000);

  if (minutes < 1) return 'Just now';
  if (minutes < 60) return `${minutes}m ago`;
  if (hours < 24) return `${hours}h ago`;
  if (days < 7) return `${days}d ago`;
  return date.toLocaleDateString();
};

// ============================================================================
// MAIN COMPONENT
// ============================================================================

export const ListPortal: React.FC = () => {
  const { organization } = useAuth();
  const [viewMode, setViewMode] = useState<ViewMode>('dashboard');
  const [selectedList, setSelectedList] = useState<List | null>(null);
  const [selectedSegment, setSelectedSegment] = useState<Segment | null>(null);
  const [stats, setStats] = useState<DashboardStats | null>(null);
  const [lists, setLists] = useState<List[]>([]);
  const [segments, setSegments] = useState<Segment[]>([]);
  const [loading, setLoading] = useState(true);
  const [animateIn, setAnimateIn] = useState(false);

  // API helper with organization context
  const orgFetch = useCallback((url: string, options: RequestInit = {}) => {
    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
      ...(options.headers as Record<string, string> || {}),
    };
    if (organization?.id) {
      headers['X-Organization-ID'] = organization.id;
    }
    return fetch(url, { ...options, headers, credentials: 'include' });
  }, [organization]);

  // Fetch dashboard data
  const fetchDashboard = useCallback(async () => {
    try {
      const [listsRes, segmentsRes, activityRes] = await Promise.all([
        orgFetch('/api/mailing/lists'),
        orgFetch('/api/mailing/segments'),
        orgFetch('/api/mailing/lists/activity').catch(() => null),
      ]);

      const listsData = await listsRes.json().catch(() => ({ lists: [] }));
      const segmentsData = await segmentsRes.json().catch(() => ({ segments: [] }));
      const activityData = activityRes ? await activityRes.json().catch(() => ({})) : {};

      const allLists: List[] = listsData.lists || [];
      const allSegments: Segment[] = segmentsData.segments || [];

      setLists(allLists);
      setSegments(allSegments);

      // Calculate dashboard stats
      const totalSubscribers = allLists.reduce((sum, l) => sum + (l.subscriber_count || 0), 0);
      const activeSubscribers = allLists.reduce((sum, l) => sum + (l.active_count || 0), 0);

      setStats({
        total_lists: allLists.length,
        total_subscribers: totalSubscribers,
        total_segments: allSegments.length,
        active_subscribers: activeSubscribers,
        new_subscribers_24h: activityData?.new_subscribers_24h || 0,
        new_subscribers_7d: activityData?.new_subscribers_7d || 0,
        unsubscribes_7d: activityData?.unsubscribes_7d || 0,
        avg_engagement: totalSubscribers > 0 ? Math.round((activeSubscribers / totalSubscribers) * 100) : 0,
        lists_by_status: [
          { status: 'active', count: allLists.filter(l => l.status === 'active').length },
          { status: 'paused', count: allLists.filter(l => l.status === 'paused').length },
          { status: 'archived', count: allLists.filter(l => l.status === 'archived').length },
        ],
        top_lists: allLists
          .sort((a, b) => (b.subscriber_count || 0) - (a.subscriber_count || 0))
          .slice(0, 5),
        recent_activity: activityData?.recent_activity || [],
      });
    } catch (err) {
      console.error('Failed to fetch dashboard:', err);
    } finally {
      setLoading(false);
      setTimeout(() => setAnimateIn(true), 100);
    }
  }, [orgFetch]);

  useEffect(() => {
    fetchDashboard();
    const interval = setInterval(fetchDashboard, 60000);
    return () => clearInterval(interval);
  }, [fetchDashboard]);

  // Navigate to a view
  const navigateTo = (view: ViewMode, list?: List, segment?: Segment) => {
    setAnimateIn(false);
    setTimeout(() => {
      setViewMode(view);
      if (list) setSelectedList(list);
      if (segment) setSelectedSegment(segment);
      setTimeout(() => setAnimateIn(true), 50);
    }, 200);
  };

  // Render based on view mode
  const renderContent = () => {
    switch (viewMode) {
      case 'dashboard':
        return (
          <ListDashboard
            stats={stats}
            lists={lists}
            segments={segments}
            onNavigate={navigateTo}
            animateIn={animateIn}
          />
        );
      case 'lists':
        return (
          <ListsManager
            lists={lists}
            onNavigate={navigateTo}
            onRefresh={fetchDashboard}
            orgFetch={orgFetch}
            animateIn={animateIn}
          />
        );
      case 'create-list':
        return (
          <CreateList
            onCancel={() => navigateTo('lists')}
            onSuccess={() => {
              fetchDashboard();
              navigateTo('lists');
            }}
            orgFetch={orgFetch}
            animateIn={animateIn}
          />
        );
      case 'edit-list':
        return selectedList ? (
          <EditList
            list={selectedList}
            onCancel={() => navigateTo('lists')}
            onSuccess={() => {
              fetchDashboard();
              navigateTo('lists');
            }}
            orgFetch={orgFetch}
            animateIn={animateIn}
          />
        ) : null;
      case 'subscribers':
        return selectedList ? (
          <SubscribersView
            list={selectedList}
            onBack={() => navigateTo('lists')}
            orgFetch={orgFetch}
            animateIn={animateIn}
          />
        ) : null;
      case 'segments':
        return (
          <SegmentsManager
            segments={segments}
            lists={lists}
            onNavigate={navigateTo}
            onRefresh={fetchDashboard}
            orgFetch={orgFetch}
            animateIn={animateIn}
          />
        );
      case 'create-segment':
        return (
          <CreateSegment
            lists={lists}
            onCancel={() => navigateTo('segments')}
            onSuccess={() => {
              fetchDashboard();
              navigateTo('segments');
            }}
            orgFetch={orgFetch}
            animateIn={animateIn}
          />
        );
      case 'edit-segment':
        return selectedSegment ? (
          <EditSegment
            segment={selectedSegment}
            lists={lists}
            onCancel={() => navigateTo('segments')}
            onSuccess={() => {
              fetchDashboard();
              navigateTo('segments');
            }}
            orgFetch={orgFetch}
            animateIn={animateIn}
          />
        ) : null;
      case 'import':
        return (
          <ImportSubscribers
            lists={lists}
            onCancel={() => navigateTo('lists')}
            onSuccess={() => {
              fetchDashboard();
              navigateTo('lists');
            }}
            orgFetch={orgFetch}
            animateIn={animateIn}
          />
        );
      default:
        return null;
    }
  };

  if (loading) {
    return (
      <div className="list-portal">
        <div className="loading-container">
          <div className="loading-spinner"></div>
          <p>Loading list data...</p>
        </div>
      </div>
    );
  }

  return (
    <div className="list-portal">
      {/* Breadcrumb Navigation */}
      <nav className="portal-breadcrumb">
        <button
          className={viewMode === 'dashboard' ? 'active' : ''}
          onClick={() => navigateTo('dashboard')}
        >
          <span className="breadcrumb-icon"><FontAwesomeIcon icon={faChartBar} /></span>
          Dashboard
        </button>
        {(viewMode.includes('list') || viewMode === 'subscribers' || viewMode === 'import') && viewMode !== 'dashboard' && (
          <>
            <span className="breadcrumb-separator">›</span>
            <button
              className={viewMode === 'lists' ? 'active' : ''}
              onClick={() => navigateTo('lists')}
            >
              <span className="breadcrumb-icon"><FontAwesomeIcon icon={faListUl} /></span>
              Lists
            </button>
          </>
        )}
        {viewMode.includes('segment') && (
          <>
            <span className="breadcrumb-separator">›</span>
            <button
              className={viewMode === 'segments' ? 'active' : ''}
              onClick={() => navigateTo('segments')}
            >
              <span className="breadcrumb-icon"><FontAwesomeIcon icon={faCrosshairs} /></span>
              Segments
            </button>
          </>
        )}
        {(viewMode === 'create-list' || viewMode === 'edit-list' || viewMode === 'subscribers' || viewMode === 'import') && (
          <>
            <span className="breadcrumb-separator">›</span>
            <span className="breadcrumb-current">
              {viewMode === 'create-list' && 'Create New List'}
              {viewMode === 'edit-list' && `Edit: ${selectedList?.name}`}
              {viewMode === 'subscribers' && `${selectedList?.name} Subscribers`}
              {viewMode === 'import' && 'Import Subscribers'}
            </span>
          </>
        )}
        {(viewMode === 'create-segment' || viewMode === 'edit-segment') && (
          <>
            <span className="breadcrumb-separator">›</span>
            <span className="breadcrumb-current">
              {viewMode === 'create-segment' && 'Create New Segment'}
              {viewMode === 'edit-segment' && `Edit: ${selectedSegment?.name}`}
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
  lists: List[];
  segments: Segment[];
  onNavigate: (view: ViewMode, list?: List, segment?: Segment) => void;
  animateIn: boolean;
}

const ListDashboard: React.FC<DashboardProps> = ({ stats, lists, segments, onNavigate, animateIn }) => {
  if (!stats) return null;

  return (
    <div className={`list-dashboard ${animateIn ? 'animate-in' : ''}`}>
      {/* Hero Stats */}
      <div className="hero-stats">
        <div className="hero-stat-card primary" style={{ animationDelay: '0ms' }}>
          <div className="hero-stat-icon"><FontAwesomeIcon icon={faUsers} /></div>
          <div className="hero-stat-content">
            <div className="hero-stat-value">
              <AnimatedNumber value={stats.total_subscribers} />
            </div>
            <div className="hero-stat-label">Total Subscribers</div>
            {stats.new_subscribers_7d > 0 && (
              <div className="hero-stat-trend positive">
                <FontAwesomeIcon icon={faArrowUp} /> +{stats.new_subscribers_7d} this week
              </div>
            )}
          </div>
        </div>

        <div className="hero-stat-card" style={{ animationDelay: '50ms' }}>
          <div className="hero-stat-icon"><FontAwesomeIcon icon={faListUl} /></div>
          <div className="hero-stat-content">
            <div className="hero-stat-value">{stats.total_lists}</div>
            <div className="hero-stat-label">Mailing Lists</div>
            <div className="hero-stat-trend">{stats.lists_by_status.find(s => s.status === 'active')?.count || 0} active</div>
          </div>
        </div>

        <div className="hero-stat-card" style={{ animationDelay: '100ms' }}>
          <div className="hero-stat-icon"><FontAwesomeIcon icon={faCrosshairs} /></div>
          <div className="hero-stat-content">
            <div className="hero-stat-value">{stats.total_segments}</div>
            <div className="hero-stat-label">Segments</div>
            <div className="hero-stat-trend">Target specific groups</div>
          </div>
        </div>

        <div className="hero-stat-card engagement" style={{ animationDelay: '150ms' }}>
          <div className="hero-stat-icon"><FontAwesomeIcon icon={faChartLine} /></div>
          <div className="hero-stat-content">
            <div className="hero-stat-value">{stats.avg_engagement}%</div>
            <div className="hero-stat-label">Active Rate</div>
            <div className="hero-stat-trend">
              {stats.active_subscribers.toLocaleString()} engaged subscribers
            </div>
          </div>
        </div>
      </div>

      {/* Quick Actions */}
      <div className="dashboard-section" style={{ animationDelay: '200ms' }}>
        <h3><FontAwesomeIcon icon={faBolt} /> Quick Actions</h3>
        <div className="quick-actions-grid">
          <button className="quick-action-btn primary" onClick={() => onNavigate('create-list')}>
            <span className="qa-icon"><FontAwesomeIcon icon={faPlus} /></span>
            <div className="qa-content">
              <strong>Create New List</strong>
              <small>Add a new mailing list</small>
            </div>
            <span className="qa-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="quick-action-btn" onClick={() => onNavigate('import')}>
            <span className="qa-icon"><FontAwesomeIcon icon={faUpload} /></span>
            <div className="qa-content">
              <strong>Import Subscribers</strong>
              <small>Upload from CSV file</small>
            </div>
            <span className="qa-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="quick-action-btn segment" onClick={() => onNavigate('create-segment')}>
            <span className="qa-icon"><FontAwesomeIcon icon={faCrosshairs} /></span>
            <div className="qa-content">
              <strong>Create Segment</strong>
              <small>Target specific subscribers</small>
            </div>
            <span className="qa-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="quick-action-btn" onClick={() => onNavigate('lists')}>
            <span className="qa-icon"><FontAwesomeIcon icon={faListUl} /></span>
            <div className="qa-content">
              <strong>Manage Lists</strong>
              <small>View and edit all lists</small>
            </div>
            <span className="qa-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>
        </div>
      </div>

      {/* Two Column Layout */}
      <div className="dashboard-grid">
        {/* Top Lists */}
        <div className="dashboard-card" style={{ animationDelay: '250ms' }}>
          <div className="card-header">
            <h3><FontAwesomeIcon icon={faListUl} /> Top Lists</h3>
            <button className="view-all-btn" onClick={() => onNavigate('lists')}>
              View All <FontAwesomeIcon icon={faArrowRight} />
            </button>
          </div>
          <div className="top-lists">
            {stats.top_lists.length === 0 ? (
              <div className="empty-state">
                <FontAwesomeIcon icon={faListUl} />
                <p>No lists yet</p>
                <button onClick={() => onNavigate('create-list')}>Create Your First List</button>
              </div>
            ) : (
              stats.top_lists.map((list, idx) => (
                <div 
                  key={list.id} 
                  className="top-list-item"
                  onClick={() => {
                    const fullList = lists.find(l => l.id === list.id);
                    if (fullList) onNavigate('subscribers', fullList);
                  }}
                >
                  <div className="list-rank">#{idx + 1}</div>
                  <div className="list-info">
                    <div className="list-name">{list.name}</div>
                    <div className="list-meta">
                      <span><FontAwesomeIcon icon={faUsers} /> {list.subscriber_count?.toLocaleString() || 0}</span>
                      <span><FontAwesomeIcon icon={faCheckCircle} /> {list.active_count?.toLocaleString() || 0} active</span>
                    </div>
                  </div>
                  <FontAwesomeIcon icon={faArrowRight} className="list-arrow" />
                </div>
              ))
            )}
          </div>
        </div>

        {/* Segments Overview */}
        <div className="dashboard-card" style={{ animationDelay: '300ms' }}>
          <div className="card-header">
            <h3><FontAwesomeIcon icon={faCrosshairs} /> Segments</h3>
            <button className="view-all-btn" onClick={() => onNavigate('segments')}>
              View All <FontAwesomeIcon icon={faArrowRight} />
            </button>
          </div>
          <div className="segments-overview">
            {segments.length === 0 ? (
              <div className="empty-state">
                <FontAwesomeIcon icon={faCrosshairs} />
                <p>No segments yet</p>
                <button onClick={() => onNavigate('create-segment')}>Create Your First Segment</button>
              </div>
            ) : (
              segments.slice(0, 5).map(segment => (
                <div 
                  key={segment.id} 
                  className="segment-item"
                  onClick={() => onNavigate('edit-segment', undefined, segment)}
                >
                  <div className={`segment-type-badge ${segment.segment_type}`}>
                    {segment.segment_type === 'dynamic' ? <FontAwesomeIcon icon={faBolt} /> : <FontAwesomeIcon icon={faLayerGroup} />}
                  </div>
                  <div className="segment-info">
                    <div className="segment-name">{segment.name}</div>
                    <div className="segment-meta">
                      <span><FontAwesomeIcon icon={faUsers} /> {segment.subscriber_count?.toLocaleString() || 0}</span>
                      {segment.list_name && <span>{segment.list_name}</span>}
                    </div>
                  </div>
                  <span className={`segment-status status-${segment.status}`}>{segment.status}</span>
                </div>
              ))
            )}
          </div>
        </div>
      </div>

      {/* How It Works */}
      <div className="how-it-works" style={{ animationDelay: '350ms' }}>
        <h3>How Lists & Segments Work</h3>
        <div className="how-it-works-grid">
          <div className="how-step">
            <div className="step-number">1</div>
            <h4>Create Lists</h4>
            <p>Organize subscribers into lists based on source, topic, or any criteria you choose.</p>
          </div>
          <div className="how-step">
            <div className="step-number">2</div>
            <h4>Import Subscribers</h4>
            <p>Upload subscribers via CSV or add them individually. Data is validated automatically.</p>
          </div>
          <div className="how-step">
            <div className="step-number">3</div>
            <h4>Create Segments</h4>
            <p>Build dynamic or static segments to target specific groups across all your lists.</p>
          </div>
          <div className="how-step">
            <div className="step-number">4</div>
            <h4>Send Campaigns</h4>
            <p>Use segments in your campaigns to reach the right audience at the right time.</p>
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
  lists: List[];
  onNavigate: (view: ViewMode, list?: List) => void;
  onRefresh: () => void;
  orgFetch: (url: string, options?: RequestInit) => Promise<Response>;
  animateIn: boolean;
}

const ListsManager: React.FC<ListsManagerProps> = ({ lists, onNavigate, onRefresh, orgFetch, animateIn }) => {
  const [search, setSearch] = useState('');
  const [statusFilter, setStatusFilter] = useState<string>('all');
  const [deleting, setDeleting] = useState<string | null>(null);

  const filteredLists = lists.filter(list => {
    const matchesSearch = list.name.toLowerCase().includes(search.toLowerCase()) ||
                         (list.description || '').toLowerCase().includes(search.toLowerCase());
    const matchesStatus = statusFilter === 'all' || list.status === statusFilter;
    return matchesSearch && matchesStatus;
  });

  const handleDelete = async (list: List) => {
    if (!confirm(`Are you sure you want to delete "${list.name}"? This will also remove all subscribers.`)) return;
    
    setDeleting(list.id);
    try {
      await orgFetch(`/api/mailing/lists/${list.id}`, { method: 'DELETE' });
      onRefresh();
    } catch (err) {
      alert('Failed to delete list');
    } finally {
      setDeleting(null);
    }
  };

  return (
    <div className={`lists-manager ${animateIn ? 'animate-in' : ''}`}>
      <div className="manager-header">
        <div className="header-left">
          <h2><FontAwesomeIcon icon={faListUl} /> Mailing Lists</h2>
          <p>{lists.length} total lists</p>
        </div>
        <div className="header-actions">
          <button className="btn btn-secondary" onClick={() => onNavigate('import')}>
            <FontAwesomeIcon icon={faUpload} /> Import
          </button>
          <button className="btn btn-primary" onClick={() => onNavigate('create-list')}>
            <FontAwesomeIcon icon={faPlus} /> Create List
          </button>
        </div>
      </div>

      <div className="manager-toolbar">
        <div className="search-box">
          <FontAwesomeIcon icon={faSearch} />
          <input
            type="text"
            placeholder="Search lists..."
            value={search}
            onChange={e => setSearch(e.target.value)}
          />
        </div>
        <div className="filter-group">
          <select value={statusFilter} onChange={e => setStatusFilter(e.target.value)}>
            <option value="all">All Status</option>
            <option value="active">Active</option>
            <option value="paused">Paused</option>
            <option value="archived">Archived</option>
          </select>
        </div>
      </div>

      <div className="lists-grid">
        {filteredLists.length === 0 ? (
          <div className="empty-state-large">
            <FontAwesomeIcon icon={faListUl} />
            <h3>{search || statusFilter !== 'all' ? 'No lists found' : 'No lists yet'}</h3>
            <p>{search || statusFilter !== 'all' ? 'Try adjusting your filters' : 'Create your first mailing list to get started'}</p>
            {!search && statusFilter === 'all' && (
              <button className="btn btn-primary" onClick={() => onNavigate('create-list')}>
                <FontAwesomeIcon icon={faPlus} /> Create Your First List
              </button>
            )}
          </div>
        ) : (
          filteredLists.map((list, idx) => (
            <div 
              key={list.id} 
              className="list-card"
              style={{ animationDelay: `${idx * 50}ms` }}
            >
              <div className="list-card-header">
                <h3>{list.name}</h3>
                <span className={`status-badge status-${list.status}`}>{list.status}</span>
              </div>
              <p className="list-description">{list.description || 'No description'}</p>
              
              <div className="list-stats">
                <div className="stat">
                  <span className="stat-value">{(list.subscriber_count || 0).toLocaleString()}</span>
                  <span className="stat-label">Subscribers</span>
                </div>
                <div className="stat">
                  <span className="stat-value">{(list.active_count || 0).toLocaleString()}</span>
                  <span className="stat-label">Active</span>
                </div>
                <div className="stat">
                  <span className="stat-value">
                    {list.subscriber_count > 0 
                      ? Math.round((list.active_count / list.subscriber_count) * 100) 
                      : 0}%
                  </span>
                  <span className="stat-label">Rate</span>
                </div>
              </div>

              <div className="list-card-footer">
                <span className="list-date">
                  <FontAwesomeIcon icon={faClock} /> Created {formatTimeAgo(list.created_at)}
                </span>
                <div className="list-actions">
                  <button 
                    className="action-btn"
                    onClick={() => onNavigate('subscribers', list)}
                    title="View Subscribers"
                  >
                    <FontAwesomeIcon icon={faUsers} />
                  </button>
                  <button 
                    className="action-btn"
                    onClick={() => onNavigate('edit-list', list)}
                    title="Edit List"
                  >
                    <FontAwesomeIcon icon={faPencilAlt} />
                  </button>
                  <button 
                    className="action-btn danger"
                    onClick={() => handleDelete(list)}
                    disabled={deleting === list.id}
                    title="Delete List"
                  >
                    <FontAwesomeIcon icon={deleting === list.id ? faSpinner : faTrash} spin={deleting === list.id} />
                  </button>
                </div>
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  );
};

// ============================================================================
// CREATE LIST
// ============================================================================

interface CreateListProps {
  onCancel: () => void;
  onSuccess: () => void;
  orgFetch: (url: string, options?: RequestInit) => Promise<Response>;
  animateIn: boolean;
}

const CreateList: React.FC<CreateListProps> = ({ onCancel, onSuccess, orgFetch, animateIn }) => {
  const [formData, setFormData] = useState({
    name: '',
    description: '',
    default_from_name: '',
    default_from_email: '',
    default_reply_to: '',
    opt_in_type: 'single',
  });
  const [saving, setSaving] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formData.name) return;

    setSaving(true);
    try {
      const res = await orgFetch('/api/mailing/lists', {
        method: 'POST',
        body: JSON.stringify(formData),
      });
      
      if (res.ok) {
        onSuccess();
      } else {
        const error = await res.json();
        alert(`Failed to create list: ${error.error || 'Unknown error'}`);
      }
    } catch (err) {
      alert('Failed to create list');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className={`create-form-container ${animateIn ? 'animate-in' : ''}`}>
      <div className="form-header">
        <h2><FontAwesomeIcon icon={faPlus} /> Create New List</h2>
        <p>Set up a new mailing list for your subscribers</p>
      </div>

      <form onSubmit={handleSubmit} className="create-form">
        <div className="form-section">
          <h3>Basic Information</h3>
          <div className="form-group">
            <label>List Name *</label>
            <input
              type="text"
              value={formData.name}
              onChange={e => setFormData(prev => ({ ...prev, name: e.target.value }))}
              placeholder="e.g., Newsletter Subscribers"
              required
            />
          </div>
          <div className="form-group">
            <label>Description</label>
            <textarea
              value={formData.description}
              onChange={e => setFormData(prev => ({ ...prev, description: e.target.value }))}
              placeholder="Brief description of this list's purpose..."
              rows={3}
            />
          </div>
        </div>

        <div className="form-section">
          <h3>Default Sender Information</h3>
          <p className="section-hint">These defaults will be used when sending to this list</p>
          <div className="form-row">
            <div className="form-group">
              <label>From Name</label>
              <input
                type="text"
                value={formData.default_from_name}
                onChange={e => setFormData(prev => ({ ...prev, default_from_name: e.target.value }))}
                placeholder="Your Company"
              />
            </div>
            <div className="form-group">
              <label>From Email</label>
              <input
                type="email"
                value={formData.default_from_email}
                onChange={e => setFormData(prev => ({ ...prev, default_from_email: e.target.value }))}
                placeholder="news@example.com"
              />
            </div>
          </div>
          <div className="form-group">
            <label>Reply-To Email</label>
            <input
              type="email"
              value={formData.default_reply_to}
              onChange={e => setFormData(prev => ({ ...prev, default_reply_to: e.target.value }))}
              placeholder="reply@example.com"
            />
          </div>
        </div>

        <div className="form-section">
          <h3>Subscription Settings</h3>
          <div className="form-group">
            <label>Opt-in Type</label>
            <div className="radio-group">
              <label className="radio-option">
                <input
                  type="radio"
                  name="opt_in"
                  value="single"
                  checked={formData.opt_in_type === 'single'}
                  onChange={() => setFormData(prev => ({ ...prev, opt_in_type: 'single' }))}
                />
                <div className="radio-content">
                  <strong>Single Opt-in</strong>
                  <small>Subscribers are added immediately</small>
                </div>
              </label>
              <label className="radio-option">
                <input
                  type="radio"
                  name="opt_in"
                  value="double"
                  checked={formData.opt_in_type === 'double'}
                  onChange={() => setFormData(prev => ({ ...prev, opt_in_type: 'double' }))}
                />
                <div className="radio-content">
                  <strong>Double Opt-in</strong>
                  <small>Subscribers must confirm via email</small>
                </div>
              </label>
            </div>
          </div>
        </div>

        <div className="form-actions">
          <button type="button" className="btn btn-secondary" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={saving || !formData.name}>
            <FontAwesomeIcon icon={saving ? faSpinner : faSave} spin={saving} />
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
  list: List;
  onCancel: () => void;
  onSuccess: () => void;
  orgFetch: (url: string, options?: RequestInit) => Promise<Response>;
  animateIn: boolean;
}

const EditList: React.FC<EditListProps> = ({ list, onCancel, onSuccess, orgFetch, animateIn }) => {
  const [formData, setFormData] = useState({
    name: list.name,
    description: list.description || '',
    default_from_name: list.default_from_name || '',
    default_from_email: list.default_from_email || '',
    default_reply_to: '',
    opt_in_type: list.opt_in_type || 'single',
    status: list.status,
  });
  const [saving, setSaving] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formData.name) return;

    setSaving(true);
    try {
      const res = await orgFetch(`/api/mailing/lists/${list.id}`, {
        method: 'PUT',
        body: JSON.stringify(formData),
      });
      
      if (res.ok) {
        onSuccess();
      } else {
        const error = await res.json();
        alert(`Failed to update list: ${error.error || 'Unknown error'}`);
      }
    } catch (err) {
      alert('Failed to update list');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className={`create-form-container ${animateIn ? 'animate-in' : ''}`}>
      <div className="form-header">
        <h2><FontAwesomeIcon icon={faPencilAlt} /> Edit List</h2>
        <p>Update settings for "{list.name}"</p>
      </div>

      <form onSubmit={handleSubmit} className="create-form">
        <div className="form-section">
          <h3>Basic Information</h3>
          <div className="form-group">
            <label>List Name *</label>
            <input
              type="text"
              value={formData.name}
              onChange={e => setFormData(prev => ({ ...prev, name: e.target.value }))}
              required
            />
          </div>
          <div className="form-group">
            <label>Description</label>
            <textarea
              value={formData.description}
              onChange={e => setFormData(prev => ({ ...prev, description: e.target.value }))}
              rows={3}
            />
          </div>
          <div className="form-group">
            <label>Status</label>
            <select
              value={formData.status}
              onChange={e => setFormData(prev => ({ ...prev, status: e.target.value as any }))}
            >
              <option value="active">Active</option>
              <option value="paused">Paused</option>
              <option value="archived">Archived</option>
            </select>
          </div>
        </div>

        <div className="form-section">
          <h3>Default Sender Information</h3>
          <div className="form-row">
            <div className="form-group">
              <label>From Name</label>
              <input
                type="text"
                value={formData.default_from_name}
                onChange={e => setFormData(prev => ({ ...prev, default_from_name: e.target.value }))}
              />
            </div>
            <div className="form-group">
              <label>From Email</label>
              <input
                type="email"
                value={formData.default_from_email}
                onChange={e => setFormData(prev => ({ ...prev, default_from_email: e.target.value }))}
              />
            </div>
          </div>
        </div>

        <div className="form-actions">
          <button type="button" className="btn btn-secondary" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={saving || !formData.name}>
            <FontAwesomeIcon icon={saving ? faSpinner : faSave} spin={saving} />
            {saving ? 'Saving...' : 'Save Changes'}
          </button>
        </div>
      </form>
    </div>
  );
};

// ============================================================================
// SUBSCRIBERS VIEW
// ============================================================================

interface SubscribersViewProps {
  list: List;
  onBack: () => void;
  orgFetch: (url: string, options?: RequestInit) => Promise<Response>;
  animateIn: boolean;
}

const SubscribersView: React.FC<SubscribersViewProps> = ({ list, onBack, orgFetch, animateIn }) => {
  const [subscribers, setSubscribers] = useState<Subscriber[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [page, setPage] = useState(0);
  const [search, setSearch] = useState('');
  const [showAddModal, setShowAddModal] = useState(false);
  const [newSubscriber, setNewSubscriber] = useState({ email: '', first_name: '', last_name: '' });
  const [adding, setAdding] = useState(false);
  const limit = 50;

  const fetchSubscribers = useCallback(async () => {
    setLoading(true);
    try {
      const res = await orgFetch(`/api/mailing/lists/${list.id}/subscribers?limit=${limit}&offset=${page * limit}`);
      const data = await res.json();
      setSubscribers(data.subscribers || []);
      setTotal(data.total || 0);
    } catch (err) {
      console.error('Failed to fetch subscribers:', err);
    } finally {
      setLoading(false);
    }
  }, [list.id, page, orgFetch]);

  useEffect(() => {
    fetchSubscribers();
  }, [fetchSubscribers]);

  const handleAddSubscriber = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newSubscriber.email) return;

    setAdding(true);
    try {
      const res = await orgFetch(`/api/mailing/lists/${list.id}/subscribers`, {
        method: 'POST',
        body: JSON.stringify(newSubscriber),
      });
      
      if (res.ok) {
        setShowAddModal(false);
        setNewSubscriber({ email: '', first_name: '', last_name: '' });
        fetchSubscribers();
      } else {
        const error = await res.json();
        alert(error.error || 'Failed to add subscriber');
      }
    } catch (err) {
      alert('Failed to add subscriber');
    } finally {
      setAdding(false);
    }
  };

  const getStatusColor = (status: string) => {
    const colors: Record<string, string> = {
      confirmed: '#22c55e',
      unconfirmed: '#f59e0b',
      unsubscribed: '#6b7280',
      bounced: '#ef4444',
      complained: '#dc2626',
    };
    return colors[status] || '#6b7280';
  };

  return (
    <div className={`subscribers-view ${animateIn ? 'animate-in' : ''}`}>
      <div className="subscribers-header">
        <button className="back-btn" onClick={onBack}>
          <FontAwesomeIcon icon={faArrowLeft} /> Back to Lists
        </button>
        <div className="list-info">
          <h2>{list.name}</h2>
          <p>{total.toLocaleString()} subscribers</p>
        </div>
        <button className="btn btn-primary" onClick={() => setShowAddModal(true)}>
          <FontAwesomeIcon icon={faUserPlus} /> Add Subscriber
        </button>
      </div>

      <div className="subscribers-toolbar">
        <div className="search-box">
          <FontAwesomeIcon icon={faSearch} />
          <input
            type="text"
            placeholder="Search by email..."
            value={search}
            onChange={e => setSearch(e.target.value)}
          />
        </div>
      </div>

      {loading ? (
        <div className="loading-container">
          <div className="loading-spinner"></div>
          <p>Loading subscribers...</p>
        </div>
      ) : subscribers.length === 0 ? (
        <div className="empty-state-large">
          <FontAwesomeIcon icon={faUsers} />
          <h3>No subscribers yet</h3>
          <p>Add subscribers to this list to get started</p>
          <button className="btn btn-primary" onClick={() => setShowAddModal(true)}>
            <FontAwesomeIcon icon={faUserPlus} /> Add First Subscriber
          </button>
        </div>
      ) : (
        <>
          <table className="subscribers-table">
            <thead>
              <tr>
                <th>Email</th>
                <th>Name</th>
                <th>Status</th>
                <th>Engagement</th>
                <th>Opens</th>
                <th>Clicks</th>
                <th>Subscribed</th>
              </tr>
            </thead>
            <tbody>
              {subscribers.map(sub => (
                <tr key={sub.id}>
                  <td className="email-cell">{sub.email}</td>
                  <td>{[sub.first_name, sub.last_name].filter(Boolean).join(' ') || '-'}</td>
                  <td>
                    <span className="status-dot" style={{ backgroundColor: getStatusColor(sub.status) }} />
                    {sub.status}
                  </td>
                  <td>
                    <div className="engagement-bar">
                      <div className="engagement-fill" style={{ width: `${sub.engagement_score}%` }} />
                    </div>
                    <span className="engagement-score">{sub.engagement_score?.toFixed(0) || 0}</span>
                  </td>
                  <td>{sub.total_opens || 0}</td>
                  <td>{sub.total_clicks || 0}</td>
                  <td>{sub.subscribed_at ? new Date(sub.subscribed_at).toLocaleDateString() : '-'}</td>
                </tr>
              ))}
            </tbody>
          </table>

          <div className="pagination">
            <button disabled={page === 0} onClick={() => setPage(p => p - 1)}>
              Previous
            </button>
            <span>Page {page + 1} of {Math.ceil(total / limit) || 1}</span>
            <button disabled={(page + 1) * limit >= total} onClick={() => setPage(p => p + 1)}>
              Next
            </button>
          </div>
        </>
      )}

      {/* Add Subscriber Modal */}
      {showAddModal && (
        <div className="modal-overlay" onClick={() => setShowAddModal(false)}>
          <div className="modal-content" onClick={e => e.stopPropagation()}>
            <div className="modal-header">
              <h3><FontAwesomeIcon icon={faUserPlus} /> Add Subscriber</h3>
              <button className="modal-close" onClick={() => setShowAddModal(false)}>
                <FontAwesomeIcon icon={faTimes} />
              </button>
            </div>
            <form onSubmit={handleAddSubscriber}>
              <div className="form-group">
                <label>Email Address *</label>
                <input
                  type="email"
                  value={newSubscriber.email}
                  onChange={e => setNewSubscriber(prev => ({ ...prev, email: e.target.value }))}
                  required
                  placeholder="subscriber@example.com"
                />
              </div>
              <div className="form-row">
                <div className="form-group">
                  <label>First Name</label>
                  <input
                    type="text"
                    value={newSubscriber.first_name}
                    onChange={e => setNewSubscriber(prev => ({ ...prev, first_name: e.target.value }))}
                    placeholder="John"
                  />
                </div>
                <div className="form-group">
                  <label>Last Name</label>
                  <input
                    type="text"
                    value={newSubscriber.last_name}
                    onChange={e => setNewSubscriber(prev => ({ ...prev, last_name: e.target.value }))}
                    placeholder="Doe"
                  />
                </div>
              </div>
              <div className="modal-actions">
                <button type="button" className="btn btn-secondary" onClick={() => setShowAddModal(false)}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary" disabled={adding}>
                  <FontAwesomeIcon icon={adding ? faSpinner : faUserPlus} spin={adding} />
                  {adding ? 'Adding...' : 'Add Subscriber'}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  );
};

// ============================================================================
// SEGMENTS MANAGER
// ============================================================================

interface SegmentsManagerProps {
  segments: Segment[];
  lists: List[];
  onNavigate: (view: ViewMode, list?: List, segment?: Segment) => void;
  onRefresh: () => void;
  orgFetch: (url: string, options?: RequestInit) => Promise<Response>;
  animateIn: boolean;
}

const SegmentsManager: React.FC<SegmentsManagerProps> = ({ segments, onNavigate, onRefresh, orgFetch, animateIn }) => {
  const [search, setSearch] = useState('');
  const [typeFilter, setTypeFilter] = useState<string>('all');
  const [deleting, setDeleting] = useState<string | null>(null);

  const filteredSegments = segments.filter(segment => {
    const matchesSearch = segment.name.toLowerCase().includes(search.toLowerCase()) ||
                         (segment.description || '').toLowerCase().includes(search.toLowerCase());
    const matchesType = typeFilter === 'all' || segment.segment_type === typeFilter;
    return matchesSearch && matchesType;
  });

  const handleDelete = async (segment: Segment) => {
    if (!confirm(`Are you sure you want to delete "${segment.name}"?`)) return;
    
    setDeleting(segment.id);
    try {
      await orgFetch(`/api/mailing/segments/${segment.id}`, { method: 'DELETE' });
      onRefresh();
    } catch (err) {
      alert('Failed to delete segment');
    } finally {
      setDeleting(null);
    }
  };

  return (
    <div className={`segments-manager ${animateIn ? 'animate-in' : ''}`}>
      <div className="manager-header">
        <div className="header-left">
          <h2><FontAwesomeIcon icon={faCrosshairs} /> Segments</h2>
          <p>{segments.length} total segments</p>
        </div>
        <div className="header-actions">
          <button className="btn btn-primary" onClick={() => onNavigate('create-segment')}>
            <FontAwesomeIcon icon={faPlus} /> Create Segment
          </button>
        </div>
      </div>

      <div className="manager-toolbar">
        <div className="search-box">
          <FontAwesomeIcon icon={faSearch} />
          <input
            type="text"
            placeholder="Search segments..."
            value={search}
            onChange={e => setSearch(e.target.value)}
          />
        </div>
        <div className="filter-group">
          <select value={typeFilter} onChange={e => setTypeFilter(e.target.value)}>
            <option value="all">All Types</option>
            <option value="dynamic">Dynamic</option>
            <option value="static">Static</option>
          </select>
        </div>
      </div>

      <div className="segments-grid">
        {filteredSegments.length === 0 ? (
          <div className="empty-state-large">
            <FontAwesomeIcon icon={faCrosshairs} />
            <h3>{search || typeFilter !== 'all' ? 'No segments found' : 'No segments yet'}</h3>
            <p>{search || typeFilter !== 'all' ? 'Try adjusting your filters' : 'Create segments to target specific subscriber groups'}</p>
            {!search && typeFilter === 'all' && (
              <button className="btn btn-primary" onClick={() => onNavigate('create-segment')}>
                <FontAwesomeIcon icon={faPlus} /> Create Your First Segment
              </button>
            )}
          </div>
        ) : (
          filteredSegments.map((segment, idx) => (
            <div 
              key={segment.id} 
              className="segment-card"
              style={{ animationDelay: `${idx * 50}ms` }}
            >
              <div className="segment-card-header">
                <div className={`segment-type-icon ${segment.segment_type}`}>
                  <FontAwesomeIcon icon={segment.segment_type === 'dynamic' ? faBolt : faLayerGroup} />
                </div>
                <div className="segment-title">
                  <h3>{segment.name}</h3>
                  <span className={`segment-type-badge ${segment.segment_type}`}>
                    {segment.segment_type}
                  </span>
                </div>
              </div>
              
              <p className="segment-description">{segment.description || 'No description'}</p>
              
              <div className="segment-stats">
                <div className="stat">
                  <FontAwesomeIcon icon={faUsers} />
                  <span>{(segment.subscriber_count || 0).toLocaleString()}</span>
                  <label>subscribers</label>
                </div>
                {segment.list_name && (
                  <div className="stat">
                    <FontAwesomeIcon icon={faListUl} />
                    <span>{segment.list_name}</span>
                    <label>list</label>
                  </div>
                )}
              </div>

              <div className="segment-card-footer">
                <span className={`status-badge status-${segment.status}`}>{segment.status}</span>
                <div className="segment-actions">
                  <button 
                    className="action-btn"
                    onClick={() => onNavigate('edit-segment', undefined, segment)}
                    title="Edit Segment"
                  >
                    <FontAwesomeIcon icon={faPencilAlt} />
                  </button>
                  <button 
                    className="action-btn"
                    onClick={() => {/* Preview */}}
                    title="Preview Subscribers"
                  >
                    <FontAwesomeIcon icon={faEye} />
                  </button>
                  <button 
                    className="action-btn danger"
                    onClick={() => handleDelete(segment)}
                    disabled={deleting === segment.id}
                    title="Delete Segment"
                  >
                    <FontAwesomeIcon icon={deleting === segment.id ? faSpinner : faTrash} spin={deleting === segment.id} />
                  </button>
                </div>
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  );
};

// ============================================================================
// CREATE SEGMENT
// ============================================================================

interface CreateSegmentProps {
  lists: List[];
  onCancel: () => void;
  onSuccess: () => void;
  orgFetch: (url: string, options?: RequestInit) => Promise<Response>;
  animateIn: boolean;
}

const CreateSegment: React.FC<CreateSegmentProps> = ({ lists, onCancel, onSuccess, orgFetch, animateIn }) => {
  const [formData, setFormData] = useState({
    name: '',
    description: '',
    list_id: '',
    segment_type: 'dynamic',
  });
  const [conditions, setConditions] = useState<SegmentCondition[]>([
    { field: 'engagement_score', operator: 'greater_than', value: '50' }
  ]);
  const [saving, setSaving] = useState(false);

  const fieldOptions = [
    // Contact Profile
    { value: 'email', label: '👤 Email Address', group: 'Contact Profile' },
    { value: 'first_name', label: '👤 First Name', group: 'Contact Profile' },
    { value: 'last_name', label: '👤 Last Name', group: 'Contact Profile' },
    { value: 'phone', label: '👤 Phone', group: 'Contact Profile' },
    { value: 'city', label: '👤 City', group: 'Contact Profile' },
    { value: 'country', label: '👤 Country', group: 'Contact Profile' },
    { value: 'timezone', label: '👤 Timezone', group: 'Contact Profile' },
    // Engagement
    { value: 'engagement_score', label: '📊 Engagement Score', group: 'Engagement' },
    { value: 'total_opens', label: '📊 Total Opens', group: 'Engagement' },
    { value: 'total_clicks', label: '📊 Total Clicks', group: 'Engagement' },
    { value: 'status', label: '📊 Status', group: 'Engagement' },
    { value: 'subscribed_at', label: '📊 Subscribed Date', group: 'Engagement' },
    { value: 'last_open_at', label: '📊 Last Open Date', group: 'Engagement' },
    { value: 'last_click_at', label: '📊 Last Click Date', group: 'Engagement' },
  ];

  const operatorOptions = [
    // String operators
    { value: 'equals', label: 'Equals' },
    { value: 'not_equals', label: 'Does not equal' },
    { value: 'contains', label: 'Contains' },
    { value: 'not_contains', label: 'Does not contain' },
    { value: 'starts_with', label: 'Starts with' },
    { value: 'ends_with', label: 'Ends with' },
    { value: 'is_empty', label: 'Is empty' },
    { value: 'is_not_empty', label: 'Is not empty' },
    // Numeric operators
    { value: 'greater_than', label: 'Greater than' },
    { value: 'less_than', label: 'Less than' },
    { value: 'greater_than_or_equal', label: 'Greater than or equal' },
    { value: 'less_than_or_equal', label: 'Less than or equal' },
    // Date operators
    { value: 'in_last_days', label: 'In the last X days' },
    { value: 'more_than_days_ago', label: 'More than X days ago' },
  ];

  const addCondition = () => {
    setConditions(prev => [...prev, { field: 'engagement_score', operator: 'greater_than', value: '' }]);
  };

  const removeCondition = (index: number) => {
    setConditions(prev => prev.filter((_, i) => i !== index));
  };

  const updateCondition = (index: number, field: keyof SegmentCondition, value: string) => {
    setConditions(prev => prev.map((c, i) => i === index ? { ...c, [field]: value } : c));
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formData.name) return;

    setSaving(true);
    try {
      const res = await orgFetch('/api/mailing/segments', {
        method: 'POST',
        body: JSON.stringify({
          ...formData,
          conditions: formData.segment_type === 'dynamic' ? conditions : undefined,
        }),
      });
      
      if (res.ok) {
        onSuccess();
      } else {
        const error = await res.json();
        alert(`Failed to create segment: ${error.error || 'Unknown error'}`);
      }
    } catch (err) {
      alert('Failed to create segment');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className={`create-form-container ${animateIn ? 'animate-in' : ''}`}>
      <div className="form-header">
        <h2><FontAwesomeIcon icon={faCrosshairs} /> Create New Segment</h2>
        <p>Define criteria to target specific subscribers</p>
      </div>

      <form onSubmit={handleSubmit} className="create-form">
        <div className="form-section">
          <h3>Basic Information</h3>
          <div className="form-group">
            <label>Segment Name *</label>
            <input
              type="text"
              value={formData.name}
              onChange={e => setFormData(prev => ({ ...prev, name: e.target.value }))}
              placeholder="e.g., Highly Engaged Users"
              required
            />
          </div>
          <div className="form-group">
            <label>Description</label>
            <textarea
              value={formData.description}
              onChange={e => setFormData(prev => ({ ...prev, description: e.target.value }))}
              placeholder="Describe what this segment targets..."
              rows={2}
            />
          </div>
          <div className="form-group">
            <label>Based on List (optional)</label>
            <select
              value={formData.list_id}
              onChange={e => setFormData(prev => ({ ...prev, list_id: e.target.value }))}
            >
              <option value="">All Lists</option>
              {lists.map(list => (
                <option key={list.id} value={list.id}>{list.name}</option>
              ))}
            </select>
          </div>
        </div>

        <div className="form-section">
          <h3>Segment Type</h3>
          <div className="radio-group horizontal">
            <label className="radio-option">
              <input
                type="radio"
                name="segment_type"
                value="dynamic"
                checked={formData.segment_type === 'dynamic'}
                onChange={() => setFormData(prev => ({ ...prev, segment_type: 'dynamic' }))}
              />
              <div className="radio-content">
                <FontAwesomeIcon icon={faBolt} />
                <div>
                  <strong>Dynamic</strong>
                  <small>Auto-updates based on conditions</small>
                </div>
              </div>
            </label>
            <label className="radio-option">
              <input
                type="radio"
                name="segment_type"
                value="static"
                checked={formData.segment_type === 'static'}
                onChange={() => setFormData(prev => ({ ...prev, segment_type: 'static' }))}
              />
              <div className="radio-content">
                <FontAwesomeIcon icon={faLayerGroup} />
                <div>
                  <strong>Static</strong>
                  <small>Fixed list of subscribers</small>
                </div>
              </div>
            </label>
          </div>
        </div>

        {formData.segment_type === 'dynamic' && (
          <div className="form-section">
            <div className="section-header">
              <h3>Conditions</h3>
              <button type="button" className="btn btn-small" onClick={addCondition}>
                <FontAwesomeIcon icon={faPlus} /> Add Condition
              </button>
            </div>
            <p className="section-hint">Subscribers matching ALL conditions will be included</p>
            
            <div className="conditions-list">
              {conditions.map((condition, idx) => {
                // Determine placeholder based on operator
                const getPlaceholder = (op: string) => {
                  switch (op) {
                    case 'contains':
                    case 'not_contains':
                      return 'e.g., gmail.com';
                    case 'starts_with':
                      return 'e.g., john';
                    case 'ends_with':
                      return 'e.g., .com';
                    case 'is_empty':
                    case 'is_not_empty':
                      return '(no value needed)';
                    case 'in_last_days':
                    case 'more_than_days_ago':
                      return 'days (e.g., 30)';
                    default:
                      return 'Value';
                  }
                };
                const noValueNeeded = ['is_empty', 'is_not_empty'].includes(condition.operator);
                
                return (
                  <div key={idx} className="condition-row">
                    <select
                      value={condition.field}
                      onChange={e => updateCondition(idx, 'field', e.target.value)}
                    >
                      {fieldOptions.map(opt => (
                        <option key={opt.value} value={opt.value}>{opt.label}</option>
                      ))}
                    </select>
                    <select
                      value={condition.operator}
                      onChange={e => updateCondition(idx, 'operator', e.target.value)}
                    >
                      {operatorOptions.map(opt => (
                        <option key={opt.value} value={opt.value}>{opt.label}</option>
                      ))}
                    </select>
                    <input
                      type="text"
                      value={noValueNeeded ? '' : condition.value}
                      onChange={e => updateCondition(idx, 'value', e.target.value)}
                      placeholder={getPlaceholder(condition.operator)}
                      disabled={noValueNeeded}
                      style={noValueNeeded ? { opacity: 0.5, backgroundColor: '#f5f5f5' } : undefined}
                    />
                    {conditions.length > 1 && (
                      <button 
                        type="button" 
                        className="condition-remove"
                        onClick={() => removeCondition(idx)}
                      >
                        <FontAwesomeIcon icon={faTimes} />
                      </button>
                    )}
                  </div>
                );
              })}
            </div>
          </div>
        )}

        <div className="form-actions">
          <button type="button" className="btn btn-secondary" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={saving || !formData.name}>
            <FontAwesomeIcon icon={saving ? faSpinner : faSave} spin={saving} />
            {saving ? 'Creating...' : 'Create Segment'}
          </button>
        </div>
      </form>
    </div>
  );
};

// ============================================================================
// EDIT SEGMENT
// ============================================================================

interface EditSegmentProps {
  segment: Segment;
  lists: List[];
  onCancel: () => void;
  onSuccess: () => void;
  orgFetch: (url: string, options?: RequestInit) => Promise<Response>;
  animateIn: boolean;
}

const EditSegment: React.FC<EditSegmentProps> = ({ segment, lists: availableLists, onCancel, onSuccess, orgFetch, animateIn }) => {
  const [formData, setFormData] = useState({
    name: segment.name,
    description: segment.description || '',
    list_id: segment.list_id || '',
    status: segment.status,
  });
  const [saving, setSaving] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!formData.name) return;

    setSaving(true);
    try {
      const res = await orgFetch(`/api/mailing/segments/${segment.id}`, {
        method: 'PUT',
        body: JSON.stringify(formData),
      });
      
      if (res.ok) {
        onSuccess();
      } else {
        const error = await res.json();
        alert(`Failed to update segment: ${error.error || 'Unknown error'}`);
      }
    } catch (err) {
      alert('Failed to update segment');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className={`create-form-container ${animateIn ? 'animate-in' : ''}`}>
      <div className="form-header">
        <h2><FontAwesomeIcon icon={faPencilAlt} /> Edit Segment</h2>
        <p>Update settings for "{segment.name}"</p>
      </div>

      <form onSubmit={handleSubmit} className="create-form">
        <div className="form-section">
          <h3>Basic Information</h3>
          <div className="form-group">
            <label>Segment Name *</label>
            <input
              type="text"
              value={formData.name}
              onChange={e => setFormData(prev => ({ ...prev, name: e.target.value }))}
              required
            />
          </div>
          <div className="form-group">
            <label>Description</label>
            <textarea
              value={formData.description}
              onChange={e => setFormData(prev => ({ ...prev, description: e.target.value }))}
              rows={2}
            />
          </div>
          <div className="form-row">
            <div className="form-group">
              <label>Based on List</label>
              <select
                value={formData.list_id}
                onChange={e => setFormData(prev => ({ ...prev, list_id: e.target.value }))}
              >
                <option value="">All Lists</option>
                {availableLists.map(list => (
                  <option key={list.id} value={list.id}>{list.name}</option>
                ))}
              </select>
            </div>
            <div className="form-group">
              <label>Status</label>
              <select
                value={formData.status}
                onChange={e => setFormData(prev => ({ ...prev, status: e.target.value as any }))}
              >
                <option value="active">Active</option>
                <option value="draft">Draft</option>
                <option value="archived">Archived</option>
              </select>
            </div>
          </div>
        </div>

        <div className="form-section">
          <h3>Segment Info</h3>
          <div className="info-grid">
            <div className="info-item">
              <label>Type</label>
              <span className={`segment-type-badge ${segment.segment_type}`}>
                <FontAwesomeIcon icon={segment.segment_type === 'dynamic' ? faBolt : faLayerGroup} />
                {segment.segment_type}
              </span>
            </div>
            <div className="info-item">
              <label>Subscribers</label>
              <span>{(segment.subscriber_count || 0).toLocaleString()}</span>
            </div>
            <div className="info-item">
              <label>Created</label>
              <span>{new Date(segment.created_at).toLocaleDateString()}</span>
            </div>
          </div>
        </div>

        <div className="form-actions">
          <button type="button" className="btn btn-secondary" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={saving || !formData.name}>
            <FontAwesomeIcon icon={saving ? faSpinner : faSave} spin={saving} />
            {saving ? 'Saving...' : 'Save Changes'}
          </button>
        </div>
      </form>
    </div>
  );
};

// ============================================================================
// IMPORT SUBSCRIBERS
// ============================================================================

interface ImportSubscribersProps {
  lists: List[];
  onCancel: () => void;
  onSuccess: () => void;
  orgFetch?: (url: string, options?: RequestInit) => Promise<Response>;
  animateIn: boolean;
}

interface ParsedRecord {
  email: string;
  valid: boolean;
  error?: string;
  data: Record<string, string>;
}

interface ImportAnalysis {
  totalRows: number;
  validEmails: number;
  invalidEmails: number;
  duplicatesInFile: number;
  headers: string[];
  preview: ParsedRecord[];
  invalidRecords: ParsedRecord[];
}

interface ImportResult {
  success: boolean;
  job_id?: string;
  status?: string;
  total_rows?: number;
  processed_rows?: number;
  imported_count?: number;
  new_count?: number;
  updated_count?: number;
  skipped_count?: number;
  error_count?: number;
  duplicate_count?: number;
  errors?: string[];
}

type ImportStep = 'setup' | 'analyze' | 'importing' | 'complete';

const ImportSubscribers: React.FC<ImportSubscribersProps> = ({ lists, onCancel, onSuccess, animateIn }) => {
  const [step, setStep] = useState<ImportStep>('setup');
  const [selectedList, setSelectedList] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [analysis, setAnalysis] = useState<ImportAnalysis | null>(null);
  const [analyzing, setAnalyzing] = useState(false);
  const [result, setResult] = useState<ImportResult | null>(null);
  const [showFieldsRef, setShowFieldsRef] = useState(false);
  const [updateExisting, setUpdateExisting] = useState(true);
  const [progress, setProgress] = useState(0);
  const [progressMessage, setProgressMessage] = useState('');
  const [startTime, setStartTime] = useState<number | null>(null);
  const [estimatedTimeRemaining, setEstimatedTimeRemaining] = useState<string>('');
  const [useAdvancedUploader, setUseAdvancedUploader] = useState(false);

  // If advanced uploader is selected and a list is chosen, render ChunkedUploader
  if (useAdvancedUploader && selectedList) {
    const selectedListData = lists.find(l => l.id === selectedList);
    return (
      <div className={`import-container ${animateIn ? 'animate-in' : ''}`}>
        <ChunkedUploader
          listId={selectedList}
          listName={selectedListData?.name}
          onComplete={() => onSuccess()}
          onCancel={() => setUseAdvancedUploader(false)}
        />
      </div>
    );
  }

  // Email validation regex
  const emailRegex = /^[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$/;

  const validateEmail = (email: string): boolean => {
    if (!email || typeof email !== 'string') return false;
    const trimmed = email.trim().toLowerCase();
    if (trimmed.length < 5 || trimmed.length > 254) return false;
    return emailRegex.test(trimmed);
  };

  // Parse CSV file and analyze
  const analyzeFile = async (selectedFile: File) => {
    setAnalyzing(true);
    setFile(selectedFile);

    try {
      const text = await selectedFile.text();
      const lines = text.split(/\r?\n/).filter(line => line.trim());
      
      if (lines.length < 2) {
        alert('File must have a header row and at least one data row');
        setAnalyzing(false);
        return;
      }

      // Parse headers
      const headers = parseCSVLine(lines[0]);
      const emailIndex = headers.findIndex(h => 
        h.toLowerCase().trim() === 'email' || 
        h.toLowerCase().trim() === 'email_address' ||
        h.toLowerCase().trim() === 'e-mail'
      );

      if (emailIndex === -1) {
        alert('Could not find an "email" column in your file. Please ensure your CSV has an "email" header.');
        setAnalyzing(false);
        return;
      }

      // Parse all records
      const records: ParsedRecord[] = [];
      const emailsSeen = new Set<string>();
      let duplicatesInFile = 0;
      let invalidEmails = 0;

      for (let i = 1; i < lines.length; i++) {
        const values = parseCSVLine(lines[i]);
        const email = values[emailIndex]?.trim().toLowerCase() || '';
        
        const data: Record<string, string> = {};
        headers.forEach((h, idx) => {
          data[h.toLowerCase().trim()] = values[idx]?.trim() || '';
        });

        let valid = true;
        let error: string | undefined;

        if (!email) {
          valid = false;
          error = 'Empty email';
          invalidEmails++;
        } else if (!validateEmail(email)) {
          valid = false;
          error = 'Invalid email format';
          invalidEmails++;
        } else if (emailsSeen.has(email)) {
          valid = false;
          error = 'Duplicate in file';
          duplicatesInFile++;
        } else {
          emailsSeen.add(email);
        }

        records.push({ email, valid, error, data });
      }

      const validRecords = records.filter(r => r.valid);
      const invalidRecords = records.filter(r => !r.valid);

      setAnalysis({
        totalRows: records.length,
        validEmails: validRecords.length,
        invalidEmails,
        duplicatesInFile,
        headers,
        preview: validRecords.slice(0, 5),
        invalidRecords: invalidRecords.slice(0, 10),
      });

      setStep('analyze');
    } catch (err) {
      console.error('Error parsing file:', err);
      alert('Error parsing file. Please ensure it is a valid CSV.');
    }

    setAnalyzing(false);
  };

  // Simple CSV line parser
  const parseCSVLine = (line: string): string[] => {
    const result: string[] = [];
    let current = '';
    let inQuotes = false;

    for (let i = 0; i < line.length; i++) {
      const char = line[i];
      if (char === '"') {
        inQuotes = !inQuotes;
      } else if (char === ',' && !inQuotes) {
        result.push(current);
        current = '';
      } else {
        current += char;
      }
    }
    result.push(current);
    return result.map(s => s.replace(/^"|"$/g, '').trim());
  };

  const handleFileChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    if (e.target.files && e.target.files[0]) {
      analyzeFile(e.target.files[0]);
    }
  };

  const handleUpload = async () => {
    if (!file || !selectedList || !analysis) return;

    setStep('importing');
    setProgress(0);
    setStartTime(Date.now());
    setProgressMessage('Uploading file...');

    try {
      const formData = new FormData();
      formData.append('file', file);
      formData.append('list_id', selectedList);
      formData.append('update_existing', updateExisting ? 'true' : 'false');

      const res = await fetch(`/api/mailing/lists/${selectedList}/import`, {
        method: 'POST',
        body: formData,
        credentials: 'include',
      });

      const data = await res.json();
      
      if (data.job_id) {
        // Poll for progress
        pollJobStatus(data.job_id);
      } else {
        // Direct response (small file)
        setResult({
          success: true,
          ...data,
          new_count: data.imported_count || 0,
          updated_count: data.updated_count || 0,
        });
        setStep('complete');
      }
    } catch (err) {
      setResult({ 
        success: false, 
        errors: ['Upload failed. Please try again.'] 
      });
      setStep('complete');
    }
  };

  const pollJobStatus = async (jobId: string) => {
    const poll = async () => {
      try {
        const res = await fetch(`/api/mailing/import-jobs/${jobId}`);
        if (res.ok) {
          const job = await res.json();
          
          // Update progress
          if (job.total_rows && job.processed_rows) {
            const pct = Math.round((job.processed_rows / job.total_rows) * 100);
            setProgress(pct);
            setProgressMessage(`Processing ${job.processed_rows.toLocaleString()} of ${job.total_rows.toLocaleString()} records...`);
            
            // Estimate time remaining
            if (startTime && job.processed_rows > 0) {
              const elapsed = Date.now() - startTime;
              const rate = job.processed_rows / elapsed;
              const remaining = (job.total_rows - job.processed_rows) / rate;
              setEstimatedTimeRemaining(formatTimeRemaining(remaining));
            }
          }

          if (job.status === 'completed' || job.status === 'failed') {
            setResult({
              success: job.status === 'completed',
              ...job,
              new_count: job.imported_count || 0,
              updated_count: job.updated_count || 0,
            });
            setStep('complete');
          } else {
            setTimeout(poll, 500);
          }
        }
      } catch (err) {
        console.error('Error polling job status');
        setTimeout(poll, 1000);
      }
    };

    setTimeout(poll, 500);
  };

  const formatTimeRemaining = (ms: number): string => {
    const seconds = Math.ceil(ms / 1000);
    if (seconds < 60) return `${seconds}s remaining`;
    const minutes = Math.floor(seconds / 60);
    const secs = seconds % 60;
    return `${minutes}m ${secs}s remaining`;
  };

  const resetImport = () => {
    setStep('setup');
    setFile(null);
    setAnalysis(null);
    setResult(null);
    setProgress(0);
  };

  // Available fields for reference
  const availableFields = [
    { key: 'email', label: 'Email Address', required: true, example: 'john@example.com' },
    { key: 'first_name', label: 'First Name', required: false, example: 'John' },
    { key: 'last_name', label: 'Last Name', required: false, example: 'Smith' },
    { key: 'phone', label: 'Phone Number', required: false, example: '+1-555-123-4567' },
    { key: 'city', label: 'City', required: false, example: 'New York' },
    { key: 'state', label: 'State/Province', required: false, example: 'NY' },
    { key: 'country', label: 'Country', required: false, example: 'US' },
    { key: 'postal_code', label: 'Postal/ZIP Code', required: false, example: '10001' },
    { key: 'company', label: 'Company', required: false, example: 'Acme Corp' },
    { key: 'job_title', label: 'Job Title', required: false, example: 'Marketing Manager' },
    { key: 'tags', label: 'Tags', required: false, example: 'vip,newsletter' },
    { key: 'source', label: 'Source', required: false, example: 'website_signup' },
  ];

  return (
    <div className={`import-container ${animateIn ? 'animate-in' : ''}`}>
      <div className="form-header">
        <h2><FontAwesomeIcon icon={faUpload} /> Import Subscribers</h2>
        <p>Upload a CSV file to add subscribers to a list</p>
      </div>

      <div className="import-form">
        {/* STEP: Setup */}
        {step === 'setup' && (
          <>
            {/* Download Template */}
            <div className="form-section template-section">
              <h3>1. Download Template</h3>
              <p className="section-desc">Start with a template to ensure your data is formatted correctly</p>
              
              <div className="template-buttons">
                <a href="/api/mailing/import/templates/basic" download="subscriber_import_basic.csv" className="template-btn">
                  <FontAwesomeIcon icon={faFileAlt} />
                  <div className="template-info">
                    <strong>Basic Template</strong>
                    <small>email, first_name, last_name</small>
                  </div>
                  <FontAwesomeIcon icon={faDownload} className="download-icon" />
                </a>
                
                <a href="/api/mailing/import/templates/full" download="subscriber_import_full.csv" className="template-btn">
                  <FontAwesomeIcon icon={faFileAlt} />
                  <div className="template-info">
                    <strong>Full Template</strong>
                    <small>All 17 standard fields</small>
                  </div>
                  <FontAwesomeIcon icon={faDownload} className="download-icon" />
                </a>
                
                <a href="/api/mailing/import/templates/custom" download="subscriber_import_custom.csv" className="template-btn">
                  <FontAwesomeIcon icon={faFileAlt} />
                  <div className="template-info">
                    <strong>Custom Template</strong>
                    <small>Includes custom fields</small>
                  </div>
                  <FontAwesomeIcon icon={faDownload} className="download-icon" />
                </a>
              </div>
              
              <button type="button" className="fields-toggle" onClick={() => setShowFieldsRef(!showFieldsRef)}>
                {showFieldsRef ? 'Hide' : 'Show'} Available Fields Reference
              </button>
              
              {showFieldsRef && (
                <div className="fields-reference">
                  <table>
                    <thead>
                      <tr><th>Column Name</th><th>Description</th><th>Example</th></tr>
                    </thead>
                    <tbody>
                      {availableFields.map(field => (
                        <tr key={field.key}>
                          <td><code>{field.key}</code>{field.required && <span className="required-badge">Required</span>}</td>
                          <td>{field.label}</td>
                          <td className="example-cell">{field.example}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                  <p className="fields-note"><strong>Note:</strong> Any column not in this list will be imported as a custom field.</p>
                </div>
              )}
            </div>

            {/* Select List */}
            <div className="form-section">
              <h3>2. Select List</h3>
              <div className="form-group">
                <label>Target List *</label>
                <select value={selectedList} onChange={e => setSelectedList(e.target.value)} required>
                  <option value="">Choose a list...</option>
                  {lists.map(list => (
                    <option key={list.id} value={list.id}>{list.name}</option>
                  ))}
                </select>
              </div>
            </div>

            {/* Upload File */}
            <div className="form-section">
              <h3>3. Upload CSV File</h3>
              <div className="file-upload-area">
                <input type="file" accept=".csv" onChange={handleFileChange} id="csv-upload" disabled={analyzing} />
                <label htmlFor="csv-upload" className="file-upload-label">
                  {analyzing ? (
                    <>
                      <FontAwesomeIcon icon={faSpinner} spin />
                      <span>Analyzing file...</span>
                    </>
                  ) : (
                    <>
                      <FontAwesomeIcon icon={faUpload} />
                      <span>Click to select CSV file</span>
                      <small>We'll validate emails before importing</small>
                    </>
                  )}
                </label>
              </div>
              
              {/* Large File Option */}
              <div className="large-file-option">
                <div className="large-file-info">
                  <FontAwesomeIcon icon={faRocket} />
                  <div>
                    <strong>Uploading a large file?</strong>
                    <small>Use our advanced uploader for files up to 10GB with chunked uploads and progress tracking.</small>
                  </div>
                </div>
                <button 
                  type="button" 
                  className="btn btn-outline"
                  onClick={() => setUseAdvancedUploader(true)}
                  disabled={!selectedList}
                >
                  Use Advanced Uploader
                </button>
                {!selectedList && <small className="select-list-hint">Select a list first</small>}
              </div>
            </div>

            <div className="form-actions">
              <button type="button" className="btn btn-secondary" onClick={onCancel}>Cancel</button>
            </div>
          </>
        )}

        {/* STEP: Analyze */}
        {step === 'analyze' && analysis && (
          <>
            <div className="form-section analysis-section">
              <h3><FontAwesomeIcon icon={faCheckCircle} style={{color: '#10b981'}} /> File Analyzed</h3>
              
              <div className="analysis-stats">
                <div className="stat-card">
                  <span className="stat-value">{analysis.totalRows.toLocaleString()}</span>
                  <span className="stat-label">Total Rows</span>
                </div>
                <div className="stat-card success">
                  <span className="stat-value">{analysis.validEmails.toLocaleString()}</span>
                  <span className="stat-label">Valid Emails</span>
                </div>
                <div className="stat-card warning">
                  <span className="stat-value">{analysis.invalidEmails}</span>
                  <span className="stat-label">Invalid Emails</span>
                </div>
                <div className="stat-card info">
                  <span className="stat-value">{analysis.duplicatesInFile}</span>
                  <span className="stat-label">Duplicates in File</span>
                </div>
              </div>

              {file && (
                <div className="file-info">
                  <FontAwesomeIcon icon={faFileAlt} />
                  <span>{file.name}</span>
                  <span className="file-size">({(file.size / 1024).toFixed(1)} KB)</span>
                  <button type="button" onClick={resetImport}>Change File</button>
                </div>
              )}
            </div>

            {/* Detected Columns */}
            <div className="form-section">
              <h4>Detected Columns ({analysis.headers.length})</h4>
              <div className="detected-columns">
                {analysis.headers.map((h, i) => (
                  <span key={i} className="column-tag">{h}</span>
                ))}
              </div>
            </div>

            {/* Preview */}
            {analysis.preview.length > 0 && (
              <div className="form-section">
                <h4>Preview (first {analysis.preview.length} valid records)</h4>
                <div className="preview-table-wrapper">
                  <table className="preview-table">
                    <thead>
                      <tr>
                        <th>Email</th>
                        <th>First Name</th>
                        <th>Last Name</th>
                      </tr>
                    </thead>
                    <tbody>
                      {analysis.preview.map((r, i) => (
                        <tr key={i}>
                          <td>{r.email}</td>
                          <td>{r.data.first_name || r.data.firstname || '—'}</td>
                          <td>{r.data.last_name || r.data.lastname || '—'}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            )}

            {/* Invalid Records */}
            {analysis.invalidRecords.length > 0 && (
              <div className="form-section warning-section">
                <h4><FontAwesomeIcon icon={faTimesCircle} style={{color: '#f59e0b'}} /> Invalid Records (showing first 10)</h4>
                <div className="preview-table-wrapper">
                  <table className="preview-table">
                    <thead>
                      <tr><th>Email</th><th>Issue</th></tr>
                    </thead>
                    <tbody>
                      {analysis.invalidRecords.map((r, i) => (
                        <tr key={i}>
                          <td>{r.email || '(empty)'}</td>
                          <td className="error-text">{r.error}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <p className="warning-note">These records will be skipped during import.</p>
              </div>
            )}

            {/* Import Options */}
            <div className="form-section options-section">
              <h4>Import Options</h4>
              <label className="checkbox-option">
                <input 
                  type="checkbox" 
                  checked={updateExisting} 
                  onChange={e => setUpdateExisting(e.target.checked)} 
                />
                <span>
                  <strong>Update existing records</strong>
                  <small>If an email already exists in this list, update their information</small>
                </span>
              </label>
            </div>

            {/* Target List Reminder */}
            <div className="form-section">
              <div className="target-list-reminder">
                <strong>Target List:</strong> {lists.find(l => l.id === selectedList)?.name || 'None selected'}
                {!selectedList && <span className="error-text"> (Please select a list)</span>}
              </div>
            </div>

            <div className="form-actions">
              <button type="button" className="btn btn-secondary" onClick={resetImport}>Back</button>
              <button 
                type="button" 
                className="btn btn-primary" 
                onClick={handleUpload}
                disabled={!selectedList || analysis.validEmails === 0}
              >
                <FontAwesomeIcon icon={faUpload} />
                Import {analysis.validEmails.toLocaleString()} Valid Records
              </button>
            </div>
          </>
        )}

        {/* STEP: Importing */}
        {step === 'importing' && (
          <div className="importing-section">
            <div className="progress-container">
              <div className="progress-icon">
                <FontAwesomeIcon icon={faSpinner} spin size="3x" />
              </div>
              <h3>Importing Subscribers...</h3>
              <p className="progress-message">{progressMessage}</p>
              
              <div className="progress-bar-container">
                <div className="progress-bar" style={{ width: `${progress}%` }}></div>
              </div>
              <div className="progress-info">
                <span>{progress}% complete</span>
                {estimatedTimeRemaining && <span>{estimatedTimeRemaining}</span>}
              </div>
            </div>
          </div>
        )}

        {/* STEP: Complete */}
        {step === 'complete' && result && (
          <div className="complete-section">
            <div className={`complete-icon ${result.success ? 'success' : 'error'}`}>
              <FontAwesomeIcon icon={result.success ? faCheckCircle : faTimesCircle} size="3x" />
            </div>
            <h3>{result.success ? 'Import Complete!' : 'Import Failed'}</h3>
            
            {result.success && (
              <>
                <div className="result-stats">
                  <div className="result-stat">
                    <span className="result-value">{(result.total_rows || 0).toLocaleString()}</span>
                    <span className="result-label">Total Processed</span>
                  </div>
                  <div className="result-stat success">
                    <span className="result-value">{(result.new_count || result.imported_count || 0).toLocaleString()}</span>
                    <span className="result-label">New Records</span>
                  </div>
                  <div className="result-stat info">
                    <span className="result-value">{(result.updated_count || 0).toLocaleString()}</span>
                    <span className="result-label">Updated</span>
                  </div>
                  <div className="result-stat warning">
                    <span className="result-value">{(result.skipped_count || 0).toLocaleString()}</span>
                    <span className="result-label">Skipped</span>
                  </div>
                  <div className="result-stat error">
                    <span className="result-value">{(result.error_count || 0).toLocaleString()}</span>
                    <span className="result-label">Errors</span>
                  </div>
                </div>

                {(result.skipped_count || 0) > 0 && (
                  <p className="skip-note">
                    Skipped records may include suppressed emails, invalid formats, or duplicates.
                  </p>
                )}
              </>
            )}

            {result.errors && result.errors.length > 0 && (
              <div className="error-list-container">
                <h4>Errors</h4>
                <ul className="error-list">
                  {result.errors.map((err, idx) => <li key={idx}>{err}</li>)}
                </ul>
              </div>
            )}

            <div className="form-actions">
              <button type="button" className="btn btn-secondary" onClick={resetImport}>
                Import More
              </button>
              <button type="button" className="btn btn-primary" onClick={onSuccess}>
                Done
              </button>
            </div>
          </div>
        )}
      </div>
      
      <style>{`
        .template-section { background: #f8fafc; border-radius: 8px; padding: 20px; margin-bottom: 20px; }
        .section-desc { color: #64748b; font-size: 14px; margin-bottom: 16px; }
        .template-buttons { display: flex; gap: 12px; flex-wrap: wrap; margin-bottom: 16px; }
        .template-btn { display: flex; align-items: center; gap: 12px; padding: 12px 16px; background: white; border: 1px solid #e2e8f0; border-radius: 8px; text-decoration: none; color: inherit; transition: all 0.2s; flex: 1; min-width: 200px; }
        .template-btn:hover { border-color: #3b82f6; background: #eff6ff; }
        .template-btn > svg:first-child { color: #3b82f6; font-size: 24px; }
        .template-info { flex: 1; }
        .template-info strong { display: block; font-size: 14px; margin-bottom: 2px; }
        .template-info small { color: #64748b; font-size: 12px; }
        .download-icon { color: #94a3b8; }
        .template-btn:hover .download-icon { color: #3b82f6; }
        .fields-toggle { background: none; border: none; color: #3b82f6; cursor: pointer; font-size: 13px; padding: 0; text-decoration: underline; }
        .fields-reference { margin-top: 16px; background: white; border: 1px solid #e2e8f0; border-radius: 8px; overflow: hidden; }
        .fields-reference table { width: 100%; border-collapse: collapse; font-size: 13px; }
        .fields-reference th, .fields-reference td { padding: 10px 12px; text-align: left; border-bottom: 1px solid #e2e8f0; }
        .fields-reference th { background: #f8fafc; font-weight: 600; color: #475569; font-size: 12px; text-transform: uppercase; }
        .fields-reference code { background: #f1f5f9; padding: 2px 6px; border-radius: 4px; font-size: 12px; }
        .required-badge { display: inline-block; margin-left: 8px; padding: 2px 6px; background: #fee2e2; color: #dc2626; border-radius: 4px; font-size: 10px; font-weight: 600; }
        .example-cell { color: #64748b; font-family: monospace; font-size: 12px; }
        .fields-note { padding: 12px; background: #fffbeb; color: #92400e; font-size: 13px; margin: 0; }

        /* Analysis */
        .analysis-section { background: #f0fdf4; border: 1px solid #bbf7d0; border-radius: 8px; padding: 20px; }
        .analysis-stats { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; margin: 16px 0; }
        .stat-card { background: white; padding: 16px; border-radius: 8px; text-align: center; border: 1px solid #e2e8f0; }
        .stat-card.success { border-color: #86efac; background: #f0fdf4; }
        .stat-card.warning { border-color: #fcd34d; background: #fffbeb; }
        .stat-card.info { border-color: #93c5fd; background: #eff6ff; }
        .stat-value { display: block; font-size: 24px; font-weight: 700; color: #1e293b; }
        .stat-card.success .stat-value { color: #16a34a; }
        .stat-card.warning .stat-value { color: #d97706; }
        .stat-card.info .stat-value { color: #2563eb; }
        .stat-label { font-size: 12px; color: #64748b; }

        .file-info { display: flex; align-items: center; gap: 8px; padding: 12px; background: white; border-radius: 6px; font-size: 14px; }
        .file-info svg { color: #3b82f6; }
        .file-size { color: #94a3b8; }
        .file-info button { margin-left: auto; background: none; border: none; color: #3b82f6; cursor: pointer; font-size: 13px; }

        .detected-columns { display: flex; flex-wrap: wrap; gap: 8px; }
        .column-tag { padding: 4px 10px; background: #e0e7ff; color: #4338ca; border-radius: 4px; font-size: 12px; font-weight: 500; }

        .preview-table-wrapper { overflow-x: auto; border: 1px solid #e2e8f0; border-radius: 8px; }
        .preview-table { width: 100%; border-collapse: collapse; font-size: 13px; }
        .preview-table th, .preview-table td { padding: 10px 12px; text-align: left; border-bottom: 1px solid #e2e8f0; }
        .preview-table th { background: #f8fafc; font-weight: 600; }

        .warning-section { background: #fffbeb; border: 1px solid #fcd34d; border-radius: 8px; padding: 16px; }
        .warning-note { margin: 12px 0 0; font-size: 13px; color: #92400e; }
        .error-text { color: #dc2626; }

        .options-section { background: #f8fafc; border-radius: 8px; padding: 16px; }
        .checkbox-option { display: flex; align-items: flex-start; gap: 12px; cursor: pointer; }
        .checkbox-option input { margin-top: 4px; width: 18px; height: 18px; }
        .checkbox-option span strong { display: block; margin-bottom: 2px; }
        .checkbox-option span small { color: #64748b; font-size: 13px; }

        .target-list-reminder { padding: 12px; background: #eff6ff; border-radius: 6px; font-size: 14px; }

        /* Importing */
        .importing-section { padding: 48px 24px; text-align: center; }
        .progress-container { max-width: 400px; margin: 0 auto; }
        .progress-icon { color: #3b82f6; margin-bottom: 20px; }
        .progress-message { color: #64748b; margin: 8px 0 24px; }
        .progress-bar-container { height: 8px; background: #e2e8f0; border-radius: 4px; overflow: hidden; }
        .progress-bar { height: 100%; background: linear-gradient(90deg, #3b82f6, #8b5cf6); transition: width 0.3s; }
        .progress-info { display: flex; justify-content: space-between; margin-top: 8px; font-size: 13px; color: #64748b; }

        /* Complete */
        .complete-section { padding: 32px 24px; text-align: center; }
        .complete-icon { margin-bottom: 16px; }
        .complete-icon.success { color: #10b981; }
        .complete-icon.error { color: #ef4444; }
        .result-stats { display: grid; grid-template-columns: repeat(5, 1fr); gap: 12px; margin: 24px 0; }
        .result-stat { padding: 16px 8px; background: #f8fafc; border-radius: 8px; }
        .result-stat.success { background: #f0fdf4; }
        .result-stat.info { background: #eff6ff; }
        .result-stat.warning { background: #fffbeb; }
        .result-stat.error { background: #fef2f2; }
        .result-value { display: block; font-size: 20px; font-weight: 700; }
        .result-stat.success .result-value { color: #16a34a; }
        .result-stat.info .result-value { color: #2563eb; }
        .result-stat.warning .result-value { color: #d97706; }
        .result-stat.error .result-value { color: #dc2626; }
        .result-label { font-size: 11px; color: #64748b; text-transform: uppercase; }
        .skip-note { color: #64748b; font-size: 13px; margin-top: 16px; }
        .error-list-container { text-align: left; background: #fef2f2; border-radius: 8px; padding: 16px; margin-top: 16px; }
        .error-list-container h4 { margin: 0 0 8px; color: #dc2626; }
        .error-list { margin: 0; padding-left: 20px; color: #b91c1c; font-size: 13px; }
      `}</style>
    </div>
  );
};

export default ListPortal;
