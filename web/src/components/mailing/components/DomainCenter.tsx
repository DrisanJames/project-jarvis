import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faGlobe,
  faLink,
  faImage,
  faRocket,
  faShieldAlt,
  faCheck,
  faClock,
  faExclamationTriangle,
  faSpinner,
  faArrowRight,
  faBolt,
  faChartPie,
  faServer,
  faSearch,
  faFilter,
  faTimes,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { SendingProfiles } from './SendingProfiles';
import { TrackingDomainManager } from './TrackingDomainManager';
import { ImageDomainManager } from './ImageDomainManager';
import './DomainCenter.css';

// ============================================================================
// TYPES
// ============================================================================

type ViewMode = 'dashboard' | 'sending' | 'tracking' | 'image-cdn';

interface DomainStats {
  sendingProfiles: number;
  trackingDomains: number;
  trackingActive: number;
  trackingPending: number;
  imageDomains: number;
  imageActive: number;
  imagePending: number;
  totalDomains: number;
  activeCount: number;
  pendingCount: number;
}

interface DomainOverviewItem {
  domain: string;
  type: 'sending' | 'tracking' | 'image';
  status: string;
  verified: boolean;
  profile?: string;
}

const API_BASE = '/api/mailing';

async function orgFetch(url: string, orgId: string, opts?: RequestInit) {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'X-Organization-ID': orgId,
    ...(opts?.headers as Record<string, string> || {}),
  };
  return fetch(url, { ...opts, headers });
}

// ============================================================================
// MAIN COMPONENT
// ============================================================================

export const DomainCenter: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id || '';
  const [viewMode, setViewMode] = useState<ViewMode>('dashboard');
  const [stats, setStats] = useState<DomainStats | null>(null);
  const [recentDomains, setRecentDomains] = useState<DomainOverviewItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [animateIn, setAnimateIn] = useState(false);

  const fetchStats = useCallback(async () => {
    if (!orgId) return;
    try {
      const [sendingRes, trackingRes, trackingSugRes, imageSugRes, imageRes] = await Promise.all([
        orgFetch(`${API_BASE}/sending-profiles?organization_id=${orgId}`, orgId),
        orgFetch(`${API_BASE}/tracking-domains`, orgId),
        orgFetch(`${API_BASE}/tracking-domains/suggestions`, orgId),
        orgFetch(`${API_BASE}/image-domains/suggestions`, orgId),
        orgFetch(`${API_BASE}/image-domains`, orgId),
      ]);

      const sendingData = await sendingRes.json().catch(() => ({ profiles: [] }));
      const trackingData = await trackingRes.json().catch(() => ([]));
      const trackingSugData = await trackingSugRes.json().catch(() => ({ suggestions: [] }));
      const imageSugData = await imageSugRes.json().catch(() => ({ suggestions: [] }));
      const imageData = await imageRes.json().catch(() => ([]));

      const profiles = sendingData.profiles || sendingData || [];
      const trackingDomains = Array.isArray(trackingData) ? trackingData : trackingData.domains || [];
      const imageDomains = Array.isArray(imageData) ? imageData : imageData.domains || [];
      const trackingSuggestions = trackingSugData.suggestions || [];
      const imageSuggestions = imageSugData.suggestions || [];

      // Count active/pending
      const trackingActive = trackingDomains.filter((d: any) => d.ssl_status === 'active' || d.verified).length;
      const trackingPending = trackingDomains.length - trackingActive;
      const imageActive = imageDomains.filter((d: any) => d.ssl_status === 'active' || d.verified).length;
      const imagePending = imageDomains.length - imageActive;

      setStats({
        sendingProfiles: Array.isArray(profiles) ? profiles.length : 0,
        trackingDomains: trackingDomains.length,
        trackingActive,
        trackingPending,
        imageDomains: imageDomains.length,
        imageActive,
        imagePending,
        totalDomains: (Array.isArray(profiles) ? profiles.length : 0) + trackingDomains.length + imageDomains.length,
        activeCount: (Array.isArray(profiles) ? profiles.filter((p: any) => p.status === 'active').length : 0) + trackingActive + imageActive,
        pendingCount: trackingPending + imagePending,
      });

      // Build overview items
      const items: DomainOverviewItem[] = [];

      // Sending profiles
      if (Array.isArray(profiles)) {
        profiles.forEach((p: any) => {
          if (p.sending_domain) {
            items.push({
              domain: p.sending_domain,
              type: 'sending',
              status: p.status || 'active',
              verified: p.domain_verified || false,
              profile: p.name,
            });
          }
        });
      }

      // Tracking domain suggestions/existing
      trackingSuggestions.forEach((s: any) => {
        items.push({
          domain: s.suggested_tracking_domain,
          type: 'tracking',
          status: s.status,
          verified: s.verified || false,
          profile: s.profile_name,
        });
      });

      // Image domain suggestions/existing
      imageSuggestions.forEach((s: any) => {
        items.push({
          domain: s.suggested_image_domain,
          type: 'image',
          status: s.status,
          verified: s.verified || false,
          profile: s.profile_name,
        });
      });

      setRecentDomains(items);
    } catch (err) {
      console.error('Failed to fetch domain stats:', err);
    } finally {
      setLoading(false);
      setTimeout(() => setAnimateIn(true), 100);
    }
  }, [orgId]);

  useEffect(() => {
    fetchStats();
  }, [fetchStats]);

  const navigateTo = (view: ViewMode) => {
    setAnimateIn(false);
    setTimeout(() => {
      setViewMode(view);
      setTimeout(() => setAnimateIn(true), 50);
    }, 200);
  };

  const renderContent = () => {
    switch (viewMode) {
      case 'dashboard':
        return (
          <DomainDashboard
            stats={stats}
            recentDomains={recentDomains}
            onNavigate={navigateTo}
            animateIn={animateIn}
            loading={loading}
          />
        );
      case 'sending':
        return <SendingProfiles />;
      case 'tracking':
        return <TrackingDomainManager />;
      case 'image-cdn':
        return <ImageDomainManager />;
      default:
        return null;
    }
  };

  return (
    <div className="domain-center">
      {/* Breadcrumb Navigation */}
      <nav className="domain-breadcrumb">
        <button
          className={viewMode === 'dashboard' ? 'active' : ''}
          onClick={() => navigateTo('dashboard')}
        >
          <span className="bc-icon"><FontAwesomeIcon icon={faGlobe} /></span>
          Domain Center
        </button>

        {viewMode !== 'dashboard' && (
          <>
            <span className="bc-separator">&rsaquo;</span>
            <span className="bc-current">
              {viewMode === 'sending' && 'Sending Domains'}
              {viewMode === 'tracking' && 'Tracking Domains'}
              {viewMode === 'image-cdn' && 'Image CDN'}
            </span>
          </>
        )}
      </nav>

      {/* Main Content */}
      <div className="domain-content">
        {renderContent()}
      </div>
    </div>
  );
};

// ============================================================================
// DASHBOARD COMPONENT
// ============================================================================

interface DashboardProps {
  stats: DomainStats | null;
  recentDomains: DomainOverviewItem[];
  onNavigate: (view: ViewMode) => void;
  animateIn: boolean;
  loading: boolean;
}

const DomainDashboard: React.FC<DashboardProps> = ({ stats, recentDomains, onNavigate, animateIn, loading }) => {
  const [searchQuery, setSearchQuery] = useState('');
  const [typeFilter, setTypeFilter] = useState<'all' | 'sending' | 'tracking' | 'image'>('all');
  const [statusFilter, setStatusFilter] = useState<'all' | 'active' | 'pending' | 'failed' | 'not-provisioned'>('all');

  if (loading) {
    return (
      <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', minHeight: 300, gap: 16, color: '#888' }}>
        <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: 28 }} />
        <div>Loading domain inventory...</div>
      </div>
    );
  }

  const getStatusIcon = (status: string) => {
    if (status === 'active') return <FontAwesomeIcon icon={faCheck} style={{ color: '#00b894', fontSize: 11 }} />;
    if (status === 'pending' || status === 'provisioning') return <FontAwesomeIcon icon={faClock} style={{ color: '#fdcb6e', fontSize: 11 }} />;
    if (status === 'failed') return <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#e94560', fontSize: 11 }} />;
    return <FontAwesomeIcon icon={faClock} style={{ color: '#888', fontSize: 11 }} />;
  };

  const getStatusClass = (status: string) => {
    if (status === 'active') return 'active';
    if (status === 'pending' || status === 'provisioning') return 'pending';
    if (status === 'failed') return 'failed';
    return 'not-provisioned';
  };

  const typeLabels: Record<string, string> = {
    sending: 'Sending',
    tracking: 'Tracking',
    image: 'Image CDN',
  };

  // Filter domains
  const filteredDomains = recentDomains.filter(item => {
    // Search query filter
    if (searchQuery) {
      const q = searchQuery.toLowerCase();
      const matchesDomain = item.domain.toLowerCase().includes(q);
      const matchesProfile = (item.profile || '').toLowerCase().includes(q);
      const matchesType = typeLabels[item.type]?.toLowerCase().includes(q);
      const matchesStatus = item.status.toLowerCase().includes(q);
      if (!matchesDomain && !matchesProfile && !matchesType && !matchesStatus) return false;
    }
    // Type filter
    if (typeFilter !== 'all' && item.type !== typeFilter) return false;
    // Status filter
    if (statusFilter !== 'all') {
      const sc = getStatusClass(item.status);
      if (sc !== statusFilter) return false;
    }
    return true;
  });

  const hasActiveFilters = searchQuery !== '' || typeFilter !== 'all' || statusFilter !== 'all';

  const clearFilters = () => {
    setSearchQuery('');
    setTypeFilter('all');
    setStatusFilter('all');
  };

  // Count by type for filter badges
  const typeCounts = { sending: 0, tracking: 0, image: 0 };
  const statusCounts = { active: 0, pending: 0, failed: 0, 'not-provisioned': 0 };
  recentDomains.forEach(item => {
    typeCounts[item.type] = (typeCounts[item.type] || 0) + 1;
    const sc = getStatusClass(item.status) as keyof typeof statusCounts;
    statusCounts[sc] = (statusCounts[sc] || 0) + 1;
  });

  return (
    <div className={`domain-dashboard ${animateIn ? 'animate-in' : ''}`}>
      {/* Hero Stats */}
      <div className="domain-hero-stats">
        <div className="domain-hero-card primary" style={{ animationDelay: '0ms' }}>
          <div className="domain-hero-icon"><FontAwesomeIcon icon={faGlobe} /></div>
          <div>
            <div className="domain-hero-value">{stats?.totalDomains || 0}</div>
            <div className="domain-hero-label">Total Domains</div>
            <div className="domain-hero-trend neutral">
              Across all domain types
            </div>
          </div>
        </div>

        <div className="domain-hero-card success" style={{ animationDelay: '60ms' }}>
          <div className="domain-hero-icon"><FontAwesomeIcon icon={faCheck} /></div>
          <div>
            <div className="domain-hero-value">{stats?.activeCount || 0}</div>
            <div className="domain-hero-label">Active & Verified</div>
            <div className="domain-hero-trend positive">
              <FontAwesomeIcon icon={faShieldAlt} /> Ready for production
            </div>
          </div>
        </div>

        <div className="domain-hero-card warning" style={{ animationDelay: '120ms' }}>
          <div className="domain-hero-icon"><FontAwesomeIcon icon={faClock} /></div>
          <div>
            <div className="domain-hero-value">{stats?.pendingCount || 0}</div>
            <div className="domain-hero-label">Pending / Provisioning</div>
            {(stats?.pendingCount || 0) > 0 && (
              <div className="domain-hero-trend warning">
                <FontAwesomeIcon icon={faExclamationTriangle} /> Needs attention
              </div>
            )}
          </div>
        </div>

        <div className="domain-hero-card purple" style={{ animationDelay: '180ms' }}>
          <div className="domain-hero-icon"><FontAwesomeIcon icon={faRocket} /></div>
          <div>
            <div className="domain-hero-value">{stats?.sendingProfiles || 0}</div>
            <div className="domain-hero-label">ESP Profiles</div>
            <div className="domain-hero-trend neutral">
              SparkPost, SES, Mailgun
            </div>
          </div>
        </div>
      </div>

      {/* Quick Actions */}
      <div className="domain-section" style={{ animationDelay: '200ms' }}>
        <h3><FontAwesomeIcon icon={faBolt} /> Manage Domains</h3>
        <div className="domain-actions-grid">
          <button className="domain-action-card sending" onClick={() => onNavigate('sending')}>
            <div className="domain-action-icon"><FontAwesomeIcon icon={faServer} /></div>
            <div className="domain-action-content">
              <strong>Sending Domains</strong>
              <small>ESP profiles, from addresses, sending domains &amp; rate limits</small>
            </div>
            <span className="domain-action-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="domain-action-card tracking" onClick={() => onNavigate('tracking')}>
            <div className="domain-action-icon"><FontAwesomeIcon icon={faLink} /></div>
            <div className="domain-action-content">
              <strong>Tracking Domains</strong>
              <small>Branded click tracking for deliverability &amp; reputation</small>
            </div>
            <span className="domain-action-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="domain-action-card image" onClick={() => onNavigate('image-cdn')}>
            <div className="domain-action-icon"><FontAwesomeIcon icon={faImage} /></div>
            <div className="domain-action-content">
              <strong>Image CDN</strong>
              <small>Custom image hosting domains with S3 &amp; CloudFront</small>
            </div>
            <span className="domain-action-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>
        </div>
      </div>

      {/* Domain Inventory with Search */}
      <div className="domain-section" style={{ animationDelay: '300ms' }}>
        <h3><FontAwesomeIcon icon={faChartPie} /> Domain Inventory</h3>

        {/* Search & Filters */}
        <div className="domain-search-bar">
          <div className="domain-search-input-wrap">
            <FontAwesomeIcon icon={faSearch} className="domain-search-icon" />
            <input
              type="text"
              className="domain-search-input"
              placeholder="Search domains, profiles, status..."
              value={searchQuery}
              onChange={e => setSearchQuery(e.target.value)}
            />
            {searchQuery && (
              <button className="domain-search-clear" onClick={() => setSearchQuery('')}>
                <FontAwesomeIcon icon={faTimes} />
              </button>
            )}
          </div>

          <div className="domain-filter-row">
            <span className="domain-filter-label">
              <FontAwesomeIcon icon={faFilter} /> Type:
            </span>
            <button
              className={`domain-filter-chip ${typeFilter === 'all' ? 'active' : ''}`}
              onClick={() => setTypeFilter('all')}
            >
              All ({recentDomains.length})
            </button>
            <button
              className={`domain-filter-chip chip-sending ${typeFilter === 'sending' ? 'active' : ''}`}
              onClick={() => setTypeFilter(typeFilter === 'sending' ? 'all' : 'sending')}
            >
              <FontAwesomeIcon icon={faServer} /> Sending ({typeCounts.sending})
            </button>
            <button
              className={`domain-filter-chip chip-tracking ${typeFilter === 'tracking' ? 'active' : ''}`}
              onClick={() => setTypeFilter(typeFilter === 'tracking' ? 'all' : 'tracking')}
            >
              <FontAwesomeIcon icon={faLink} /> Tracking ({typeCounts.tracking})
            </button>
            <button
              className={`domain-filter-chip chip-image ${typeFilter === 'image' ? 'active' : ''}`}
              onClick={() => setTypeFilter(typeFilter === 'image' ? 'all' : 'image')}
            >
              <FontAwesomeIcon icon={faImage} /> Image CDN ({typeCounts.image})
            </button>

            <span className="domain-filter-divider" />

            <span className="domain-filter-label">Status:</span>
            <button
              className={`domain-filter-chip chip-active ${statusFilter === 'active' ? 'active' : ''}`}
              onClick={() => setStatusFilter(statusFilter === 'active' ? 'all' : 'active')}
            >
              <FontAwesomeIcon icon={faCheck} /> Active ({statusCounts.active})
            </button>
            <button
              className={`domain-filter-chip chip-pending ${statusFilter === 'pending' ? 'active' : ''}`}
              onClick={() => setStatusFilter(statusFilter === 'pending' ? 'all' : 'pending')}
            >
              <FontAwesomeIcon icon={faClock} /> Pending ({statusCounts.pending})
            </button>
            {statusCounts.failed > 0 && (
              <button
                className={`domain-filter-chip chip-failed ${statusFilter === 'failed' ? 'active' : ''}`}
                onClick={() => setStatusFilter(statusFilter === 'failed' ? 'all' : 'failed')}
              >
                <FontAwesomeIcon icon={faExclamationTriangle} /> Failed ({statusCounts.failed})
              </button>
            )}
            {statusCounts['not-provisioned'] > 0 && (
              <button
                className={`domain-filter-chip chip-notprov ${statusFilter === 'not-provisioned' ? 'active' : ''}`}
                onClick={() => setStatusFilter(statusFilter === 'not-provisioned' ? 'all' : 'not-provisioned')}
              >
                Not Provisioned ({statusCounts['not-provisioned']})
              </button>
            )}
          </div>

          {hasActiveFilters && (
            <div className="domain-search-results-info">
              <span>Showing {filteredDomains.length} of {recentDomains.length} domains</span>
              <button className="domain-clear-filters-btn" onClick={clearFilters}>
                <FontAwesomeIcon icon={faTimes} /> Clear filters
              </button>
            </div>
          )}
        </div>

        {/* Domain Grid */}
        {recentDomains.length === 0 ? (
          <div style={{ textAlign: 'center', padding: 40, color: '#888', fontSize: 14 }}>
            No domains configured yet. Start by adding sending profiles or provisioning tracking/image domains.
          </div>
        ) : filteredDomains.length === 0 ? (
          <div className="domain-empty-search">
            <FontAwesomeIcon icon={faSearch} />
            <h4>No domains match your search</h4>
            <p>Try adjusting your search terms or filters.</p>
            <button className="domain-clear-filters-btn" onClick={clearFilters}>Clear all filters</button>
          </div>
        ) : (
          <div className="domain-summary-grid">
            {filteredDomains.map((item, idx) => (
              <div
                key={`${item.type}-${item.domain}-${idx}`}
                className="domain-summary-item"
                onClick={() => {
                  if (item.type === 'sending') onNavigate('sending');
                  else if (item.type === 'tracking') onNavigate('tracking');
                  else onNavigate('image-cdn');
                }}
                style={{ cursor: 'pointer' }}
              >
                <div className="domain-summary-name">{item.domain}</div>
                <div>
                  <span className={`domain-summary-type ${item.type}`}>
                    {typeLabels[item.type]}
                  </span>
                  <span className={`domain-summary-status ${getStatusClass(item.status)}`}>
                    {getStatusIcon(item.status)} {item.status.replace('_', ' ')}
                  </span>
                </div>
                {item.profile && (
                  <div className="domain-summary-meta">
                    Profile: {item.profile}
                  </div>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
};
