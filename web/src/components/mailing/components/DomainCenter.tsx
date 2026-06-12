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
  faStethoscope,
  faSync,
  faCopy,
  faHistory,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { SendingProfiles } from './SendingProfiles';
import { TrackingDomainManager } from './TrackingDomainManager';
import { ImageDomainManager } from './ImageDomainManager';
import { SideShelf } from './shared/SideShelf';
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

// --- DNS Health (live check via /domain-center/dns-health) ---

interface DnsSPF {
  status: string;
  record?: string;
  issues?: string[];
}

interface DnsDMARC {
  status: string;
  record?: string;
  policy?: string;
  issues?: string[];
}

interface DnsDKIM {
  selectors_found: string[];
  status: string;
  note?: string;
}

interface DnsNS {
  servers: string[];
  provider?: string;
}

interface DnsBlocklistEntry {
  list: string;
  target: string;
  status: 'clean' | 'listed' | 'unverifiable';
  detail?: string;
}

interface DnsHealthData {
  domain: string;
  apex: string;
  checked_at: string;
  spf: DnsSPF;
  dmarc: DnsDMARC;
  dkim: DnsDKIM;
  mx: string[];
  ns: DnsNS;
  a: string[];
  blocklists: DnsBlocklistEntry[];
  ip_source?: string;
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
            orgId={orgId}
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
  orgId: string;
}

const DomainDashboard: React.FC<DashboardProps> = ({ stats, recentDomains, onNavigate, animateIn, loading, orgId }) => {
  const [searchQuery, setSearchQuery] = useState('');
  const [typeFilter, setTypeFilter] = useState<'all' | 'sending' | 'tracking' | 'image'>('all');
  const [statusFilter, setStatusFilter] = useState<'all' | 'active' | 'pending' | 'failed' | 'not-provisioned'>('all');
  const [dnsHealthDomain, setDnsHealthDomain] = useState<string | null>(null);

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
      <div className="domain-hero-stats ig-stagger">
        <div className="domain-hero-card primary ig-card-hover" style={{ animationDelay: '0ms' }}>
          <div className="domain-hero-icon"><FontAwesomeIcon icon={faGlobe} /></div>
          <div>
            <div className="domain-hero-value">{stats?.totalDomains || 0}</div>
            <div className="domain-hero-label">Total Domains</div>
            <div className="domain-hero-trend neutral">
              Across all domain types
            </div>
          </div>
        </div>

        <div className="domain-hero-card success ig-card-hover" style={{ animationDelay: '60ms' }}>
          <div className="domain-hero-icon"><FontAwesomeIcon icon={faCheck} /></div>
          <div>
            <div className="domain-hero-value">{stats?.activeCount || 0}</div>
            <div className="domain-hero-label">Active & Verified</div>
            <div className="domain-hero-trend positive">
              <FontAwesomeIcon icon={faShieldAlt} /> Ready for production
            </div>
          </div>
        </div>

        <div className="domain-hero-card warning ig-card-hover" style={{ animationDelay: '120ms' }}>
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

        <div className="domain-hero-card purple ig-card-hover" style={{ animationDelay: '180ms' }}>
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
      <div className="domain-section ig-fade-in" style={{ animationDelay: '200ms' }}>
        <h3><FontAwesomeIcon icon={faBolt} /> Manage Domains</h3>
        <div className="domain-actions-grid">
          <button className="domain-action-card sending ig-btn-glow ig-ripple" onClick={() => onNavigate('sending')}>
            <div className="domain-action-icon"><FontAwesomeIcon icon={faServer} /></div>
            <div className="domain-action-content">
              <strong>Sending Domains</strong>
              <small>ESP profiles, from addresses, sending domains &amp; rate limits</small>
            </div>
            <span className="domain-action-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="domain-action-card tracking ig-btn-glow ig-ripple" onClick={() => onNavigate('tracking')}>
            <div className="domain-action-icon"><FontAwesomeIcon icon={faLink} /></div>
            <div className="domain-action-content">
              <strong>Tracking Domains</strong>
              <small>Branded click tracking for deliverability &amp; reputation</small>
            </div>
            <span className="domain-action-arrow"><FontAwesomeIcon icon={faArrowRight} /></span>
          </button>

          <button className="domain-action-card image ig-btn-glow ig-ripple" onClick={() => onNavigate('image-cdn')}>
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
      <div className="domain-section ig-fade-in" style={{ animationDelay: '300ms' }}>
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
          <div className="domain-summary-grid ig-stagger">
            {filteredDomains.map((item, idx) => (
              <div
                key={`${item.type}-${item.domain}-${idx}`}
                className={`domain-summary-item ig-card-hover ${item.status === 'pending' || item.status === 'provisioning' ? 'ig-breathe-border' : ''}`}
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
                <button
                  onClick={(e) => {
                    e.stopPropagation();
                    setDnsHealthDomain(item.domain);
                  }}
                  title="Run live DNS & blocklist health check"
                  style={{
                    marginTop: 8,
                    display: 'inline-flex',
                    alignItems: 'center',
                    gap: 6,
                    padding: '4px 10px',
                    fontSize: 11,
                    fontWeight: 600,
                    color: '#74b9ff',
                    background: 'rgba(116,185,255,0.10)',
                    border: '1px solid rgba(116,185,255,0.25)',
                    borderRadius: 6,
                    cursor: 'pointer',
                  }}
                >
                  <FontAwesomeIcon icon={faStethoscope} /> DNS Health
                </button>
              </div>
            ))}
          </div>
        )}
      </div>

      {dnsHealthDomain && (
        <DnsHealthShelf
          key={dnsHealthDomain}
          domain={dnsHealthDomain}
          orgId={orgId}
          onClose={() => setDnsHealthDomain(null)}
        />
      )}
    </div>
  );
};

// ============================================================================
// DNS HEALTH SHELF — live SPF / DKIM / DMARC / NS / blocklist check
// ============================================================================

type ChipTone = 'ok' | 'warn' | 'bad' | 'muted';

const chipToneColors: Record<ChipTone, { fg: string; bg: string; border: string }> = {
  ok: { fg: '#00b894', bg: 'rgba(0,184,148,0.12)', border: 'rgba(0,184,148,0.3)' },
  warn: { fg: '#fdcb6e', bg: 'rgba(253,203,110,0.12)', border: 'rgba(253,203,110,0.3)' },
  bad: { fg: '#e94560', bg: 'rgba(233,69,96,0.12)', border: 'rgba(233,69,96,0.3)' },
  muted: { fg: '#888', bg: 'rgba(255,255,255,0.05)', border: 'rgba(255,255,255,0.12)' },
};

const StatusChip: React.FC<{ tone: ChipTone; children: React.ReactNode }> = ({ tone, children }) => {
  const c = chipToneColors[tone];
  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 5,
        padding: '3px 10px',
        fontSize: 11,
        fontWeight: 600,
        color: c.fg,
        background: c.bg,
        border: `1px solid ${c.border}`,
        borderRadius: 999,
        whiteSpace: 'nowrap',
      }}
    >
      {children}
    </span>
  );
};

const authStatusTone = (status: string): ChipTone => {
  if (status === 'pass' || status === 'found') return 'ok';
  if (status === 'warn' || status === 'unknown') return 'warn';
  if (status === 'fail' || status === 'missing') return 'bad';
  return 'muted';
};

const blocklistTone = (status: string): ChipTone => {
  if (status === 'clean') return 'ok';
  if (status === 'listed') return 'bad';
  return 'warn'; // unverifiable
};

const RecordBlock: React.FC<{ label: string; record: string }> = ({ label, record }) => (
  <details style={{ marginTop: 6 }}>
    <summary style={{ cursor: 'pointer', fontSize: 11, color: '#74b9ff' }}>{label}</summary>
    <code
      style={{
        display: 'block',
        marginTop: 6,
        padding: '8px 10px',
        fontSize: 11,
        lineHeight: 1.5,
        color: '#e0e0e0',
        background: '#0a0f1a',
        border: '1px solid rgba(255,255,255,0.08)',
        borderRadius: 6,
        wordBreak: 'break-all',
        whiteSpace: 'pre-wrap',
      }}
    >
      {record}
    </code>
  </details>
);

const IssueList: React.FC<{ issues?: string[] }> = ({ issues }) => {
  if (!issues || issues.length === 0) return null;
  return (
    <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>
      {issues.map((iss, i) => (
        <li
          key={i}
          style={{
            fontSize: 11,
            lineHeight: 1.5,
            color: iss.startsWith('CRITICAL') ? '#e94560' : '#fdcb6e',
          }}
        >
          {iss}
        </li>
      ))}
    </ul>
  );
};

const dnsSectionStyle: React.CSSProperties = {
  padding: '12px 14px',
  background: 'rgba(255,255,255,0.02)',
  border: '1px solid rgba(255,255,255,0.06)',
  borderRadius: 8,
};

const dnsSectionTitleStyle: React.CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: 8,
  margin: 0,
  fontSize: 12,
  fontWeight: 700,
  color: '#e0e0e0',
  textTransform: 'uppercase',
  letterSpacing: 0.5,
};

interface DnsHealthShelfProps {
  domain: string;
  orgId: string;
  onClose: () => void;
}

interface DnsHealthRun {
  ts: string; // ISO timestamp of when the run completed (client-side)
  data: DnsHealthData;
}

const MAX_DNS_RUNS = 5;

// Session-scoped run history per domain — survives shelf close/reopen within
// the page session (module-level, client-side only).
const dnsRunHistory = new Map<string, DnsHealthRun[]>();

function formatDnsReport(data: DnsHealthData): string {
  const lines: string[] = [];
  lines.push(`DNS Health Report — ${data.domain}`);
  lines.push(`Apex: ${data.apex}`);
  lines.push(`Checked: ${new Date(data.checked_at).toLocaleString()}`);
  lines.push('');
  lines.push(`SPF: ${data.spf.status}`);
  if (data.spf.record) lines.push(`  Record: ${data.spf.record}`);
  (data.spf.issues || []).forEach(iss => lines.push(`  Issue: ${iss}`));
  lines.push('');
  lines.push(`DKIM: ${data.dkim.status}`);
  if (data.dkim.selectors_found.length > 0) {
    lines.push(`  Selectors: ${data.dkim.selectors_found.map(s => `${s}._domainkey`).join(', ')}`);
  }
  if (data.dkim.note) lines.push(`  Note: ${data.dkim.note}`);
  lines.push('');
  lines.push(`DMARC (_dmarc.${data.apex}): ${data.dmarc.status}${data.dmarc.policy ? ` · p=${data.dmarc.policy}` : ''}`);
  if (data.dmarc.record) lines.push(`  Record: ${data.dmarc.record}`);
  (data.dmarc.issues || []).forEach(iss => lines.push(`  Issue: ${iss}`));
  lines.push('');
  lines.push(`NS: ${data.ns.provider || (data.ns.servers.length > 0 ? 'Unrecognized provider' : 'No NS records found')}`);
  data.ns.servers.forEach(s => lines.push(`  ${s}`));
  lines.push('');
  lines.push(`MX records (${data.mx.length})${data.mx.length === 0 ? ': none' : ':'}`);
  data.mx.forEach(m => lines.push(`  ${m}`));
  lines.push('');
  lines.push(`A records (${data.a.length})${data.a.length === 0 ? ': none' : ':'}`);
  data.a.forEach(a => lines.push(`  ${a}`));
  lines.push('');
  lines.push(`Blocklists${data.ip_source ? ` (IP source: ${data.ip_source})` : ''}:`);
  if (data.blocklists.length === 0) {
    lines.push('  No blocklist targets resolved.');
  } else {
    data.blocklists.forEach(b =>
      lines.push(`  ${b.target} [${b.list}]: ${b.status}${b.detail ? ` — ${b.detail}` : ''}`),
    );
  }
  return lines.join('\n');
}

const dnsToolbarBtnStyle = (disabled: boolean): React.CSSProperties => ({
  display: 'inline-flex',
  alignItems: 'center',
  gap: 6,
  padding: '5px 12px',
  fontSize: 11,
  fontWeight: 600,
  color: '#74b9ff',
  background: 'rgba(116,185,255,0.10)',
  border: '1px solid rgba(116,185,255,0.25)',
  borderRadius: 6,
  cursor: disabled ? 'not-allowed' : 'pointer',
  opacity: disabled ? 0.5 : 1,
});

const DnsHealthShelf: React.FC<DnsHealthShelfProps> = ({ domain, orgId, onClose }) => {
  const [history, setHistory] = useState<DnsHealthRun[]>(() => dnsRunHistory.get(domain) || []);
  const [selectedTs, setSelectedTs] = useState<string | null>(null);
  const [checking, setChecking] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  const runCheck = useCallback(async () => {
    setChecking(true);
    setError(null);
    try {
      const res = await orgFetch(
        `${API_BASE}/domain-center/dns-health?domain=${encodeURIComponent(domain)}`,
        orgId,
      );
      const body = await res.json().catch(() => null);
      if (!res.ok) {
        throw new Error(body?.error || `DNS health check failed (HTTP ${res.status})`);
      }
      const run: DnsHealthRun = { ts: new Date().toISOString(), data: body as DnsHealthData };
      setHistory(prev => {
        const next = [run, ...prev].slice(0, MAX_DNS_RUNS);
        dnsRunHistory.set(domain, next);
        return next;
      });
      setSelectedTs(null); // newest run becomes the displayed one
    } catch (err) {
      setError(err instanceof Error ? err.message : 'DNS health check failed');
    } finally {
      setChecking(false);
    }
  }, [domain, orgId]);

  useEffect(() => {
    runCheck();
  }, [runCheck]);

  const currentRun = (selectedTs ? history.find(h => h.ts === selectedTs) : undefined) || history[0] || null;
  const data = currentRun?.data ?? null;

  const copyReport = async () => {
    if (!data) return;
    try {
      await navigator.clipboard.writeText(formatDnsReport(data));
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard unavailable — ignore
    }
  };

  const listedCount = data?.blocklists?.filter(b => b.status === 'listed').length ?? 0;
  const unverifiableCount = data?.blocklists?.filter(b => b.status === 'unverifiable').length ?? 0;

  return (
    <SideShelf
      title={(
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 10, minWidth: 0, maxWidth: '100%' }}>
          <FontAwesomeIcon icon={faStethoscope} style={{ color: '#74b9ff', flexShrink: 0 }} />
          <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            DNS Health — {domain}
          </span>
        </span>
      )}
      onClose={onClose}
    >
      <div style={{ padding: '14px 20px 20px', color: '#e0e0e0' }}>
        {/* Toolbar: re-run, copy report, checked-at */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 12 }}>
          <button
            onClick={runCheck}
            disabled={checking}
            title="Re-run check (live DNS)"
            style={dnsToolbarBtnStyle(checking)}
          >
            <FontAwesomeIcon icon={faSync} spin={checking} /> Re-run
          </button>
          <button
            onClick={copyReport}
            disabled={!data}
            title="Copy this report as readable text"
            style={dnsToolbarBtnStyle(!data)}
          >
            <FontAwesomeIcon icon={copied ? faCheck : faCopy} /> {copied ? 'Copied' : 'Copy report'}
          </button>
          {data && (
            <span style={{ marginLeft: 'auto', fontSize: 11, color: '#888' }}>
              Apex: {data.apex} · Checked {new Date(data.checked_at).toLocaleTimeString()}
            </span>
          )}
        </div>

        {/* Check history — last 5 runs of this session, selectable */}
        {history.length > 0 && (
          <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap', marginBottom: 14 }}>
            <span style={{ fontSize: 10, fontWeight: 700, color: '#888', textTransform: 'uppercase', letterSpacing: 0.5 }}>
              <FontAwesomeIcon icon={faHistory} /> Runs
            </span>
            {history.map((run, i) => {
              const isCurrent = currentRun?.ts === run.ts;
              return (
                <button
                  key={run.ts}
                  onClick={() => setSelectedTs(run.ts)}
                  title={new Date(run.ts).toLocaleString()}
                  style={{
                    padding: '3px 9px',
                    fontSize: 10,
                    fontWeight: 600,
                    borderRadius: 999,
                    cursor: 'pointer',
                    color: isCurrent ? '#74b9ff' : '#888',
                    background: isCurrent ? 'rgba(116,185,255,0.12)' : 'rgba(255,255,255,0.04)',
                    border: `1px solid ${isCurrent ? 'rgba(116,185,255,0.35)' : 'rgba(255,255,255,0.10)'}`,
                  }}
                >
                  {new Date(run.ts).toLocaleTimeString()}{i === 0 ? ' · latest' : ''}
                </button>
              );
            })}
          </div>
        )}

        {/* Loading */}
        {checking && !data && (
          <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 12, padding: '40px 0', color: '#888' }}>
            <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: 24 }} />
            <div style={{ fontSize: 13 }}>Running live DNS lookups (SPF, DKIM, DMARC, NS, blocklists)...</div>
          </div>
        )}

        {/* Error */}
        {error && (
          <div
            style={{
              padding: '12px 14px',
              fontSize: 13,
              color: '#e94560',
              background: 'rgba(233,69,96,0.08)',
              border: '1px solid rgba(233,69,96,0.25)',
              borderRadius: 8,
            }}
          >
            <FontAwesomeIcon icon={faExclamationTriangle} /> {error}
          </div>
        )}

        {data && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 14, opacity: checking ? 0.5 : 1 }}>
            {/* Summary chips */}
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
              <StatusChip tone={authStatusTone(data.spf.status)}>
                SPF {data.spf.status === 'pass' ? '✓' : data.spf.status === 'warn' ? '!' : '✗'}
              </StatusChip>
              <StatusChip tone={authStatusTone(data.dkim.status)}>
                DKIM {data.dkim.status === 'found' ? `✓ (${data.dkim.selectors_found.join(', ')})` : '?'}
              </StatusChip>
              <StatusChip tone={authStatusTone(data.dmarc.status)}>
                DMARC {data.dmarc.policy ? `p=${data.dmarc.policy}` : data.dmarc.status === 'missing' ? '✗ missing' : data.dmarc.status}
              </StatusChip>
              <StatusChip tone={data.mx.length > 0 ? 'ok' : 'warn'}>MX: {data.mx.length}</StatusChip>
              <StatusChip tone={data.ns.servers.length > 0 ? 'ok' : 'bad'}>
                NS: {data.ns.provider || (data.ns.servers.length > 0 ? `${data.ns.servers.length} servers` : 'none found')}
              </StatusChip>
              <StatusChip tone={listedCount > 0 ? 'bad' : unverifiableCount > 0 ? 'warn' : 'ok'}>
                Blocklists:{' '}
                {listedCount > 0
                  ? `${listedCount} LISTED`
                  : unverifiableCount > 0
                    ? `clean (${unverifiableCount} unverifiable)`
                    : 'all clean'}
              </StatusChip>
            </div>

            {/* SPF */}
            <div style={dnsSectionStyle}>
              <h4 style={dnsSectionTitleStyle}>
                <FontAwesomeIcon icon={faShieldAlt} style={{ color: '#74b9ff' }} /> SPF
                <StatusChip tone={authStatusTone(data.spf.status)}>{data.spf.status}</StatusChip>
              </h4>
              {data.spf.record && <RecordBlock label="Show record" record={data.spf.record} />}
              <IssueList issues={data.spf.issues} />
            </div>

            {/* DKIM */}
            <div style={dnsSectionStyle}>
              <h4 style={dnsSectionTitleStyle}>
                <FontAwesomeIcon icon={faShieldAlt} style={{ color: '#a29bfe' }} /> DKIM
                <StatusChip tone={authStatusTone(data.dkim.status)}>{data.dkim.status}</StatusChip>
              </h4>
              {data.dkim.selectors_found.length > 0 ? (
                <div style={{ marginTop: 6, fontSize: 12, color: '#e0e0e0' }}>
                  Selectors found:{' '}
                  {data.dkim.selectors_found.map(sel => (
                    <code
                      key={sel}
                      style={{
                        margin: '0 4px 0 0',
                        padding: '2px 6px',
                        fontSize: 11,
                        background: '#0a0f1a',
                        border: '1px solid rgba(255,255,255,0.08)',
                        borderRadius: 4,
                      }}
                    >
                      {sel}._domainkey
                    </code>
                  ))}
                </div>
              ) : (
                <div style={{ marginTop: 6, fontSize: 12, color: '#fdcb6e' }}>{data.dkim.note}</div>
              )}
            </div>

            {/* DMARC */}
            <div style={dnsSectionStyle}>
              <h4 style={dnsSectionTitleStyle}>
                <FontAwesomeIcon icon={faShieldAlt} style={{ color: '#00b894' }} /> DMARC ({'_dmarc.'}{data.apex})
                <StatusChip tone={authStatusTone(data.dmarc.status)}>
                  {data.dmarc.status}{data.dmarc.policy ? ` · p=${data.dmarc.policy}` : ''}
                </StatusChip>
              </h4>
              {data.dmarc.record && <RecordBlock label="Show record" record={data.dmarc.record} />}
              <IssueList issues={data.dmarc.issues} />
            </div>

            {/* Infrastructure: NS / MX / A */}
            <div style={dnsSectionStyle}>
              <h4 style={dnsSectionTitleStyle}>
                <FontAwesomeIcon icon={faServer} style={{ color: '#74b9ff' }} /> Infrastructure
              </h4>
              <div style={{ marginTop: 8, display: 'flex', flexDirection: 'column', gap: 6, fontSize: 12 }}>
                <div>
                  <span style={{ color: '#888' }}>NS host: </span>
                  <span style={{ color: '#fff', fontWeight: 600 }}>
                    {data.ns.provider || (data.ns.servers.length > 0 ? 'Unrecognized provider' : 'No NS records found')}
                  </span>
                </div>
                {data.ns.servers.length > 0 && (
                  <RecordBlock label={`Nameservers (${data.ns.servers.length})`} record={data.ns.servers.join('\n')} />
                )}
                {data.mx.length > 0 ? (
                  <RecordBlock label={`MX records (${data.mx.length})`} record={data.mx.join('\n')} />
                ) : (
                  <div style={{ color: '#fdcb6e' }}>No MX records on {data.domain}</div>
                )}
                {data.a.length > 0 ? (
                  <RecordBlock label={`A records (${data.a.length})`} record={data.a.join('\n')} />
                ) : (
                  <div style={{ color: '#888' }}>No A record on {data.domain}</div>
                )}
              </div>
            </div>

            {/* Blocklists */}
            <div style={dnsSectionStyle}>
              <h4 style={dnsSectionTitleStyle}>
                <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#fdcb6e' }} /> Blocklists (Spamhaus DBL/ZEN, SpamCop, Barracuda)
              </h4>
              {data.ip_source && (
                <div style={{ marginTop: 4, fontSize: 11, color: '#888' }}>
                  IP source: {data.ip_source === 'db-pool' ? 'sending profile IP pool' : data.ip_source === 'a-record' ? 'domain A record (no pool found)' : data.ip_source}
                </div>
              )}
              {data.blocklists.length === 0 ? (
                <div style={{ marginTop: 6, fontSize: 12, color: '#888' }}>No blocklist targets resolved.</div>
              ) : (
                <table style={{ width: '100%', marginTop: 8, borderCollapse: 'collapse', fontSize: 11 }}>
                  <thead>
                    <tr style={{ color: '#888', textAlign: 'left' }}>
                      <th style={{ padding: '4px 8px 4px 0', fontWeight: 600 }}>Target</th>
                      <th style={{ padding: '4px 8px 4px 0', fontWeight: 600 }}>List</th>
                      <th style={{ padding: '4px 8px 4px 0', fontWeight: 600 }}>Status</th>
                      <th style={{ padding: '4px 0', fontWeight: 600 }}>Detail</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.blocklists.map((b, i) => (
                      <tr key={`${b.target}-${b.list}-${i}`} style={{ borderTop: '1px solid rgba(255,255,255,0.05)' }}>
                        <td style={{ padding: '5px 8px 5px 0', color: '#e0e0e0', fontFamily: 'monospace' }}>{b.target}</td>
                        <td style={{ padding: '5px 8px 5px 0', color: '#888' }}>{b.list}</td>
                        <td style={{ padding: '5px 8px 5px 0' }}>
                          <StatusChip tone={blocklistTone(b.status)}>{b.status}</StatusChip>
                        </td>
                        <td style={{ padding: '5px 0', color: '#888', lineHeight: 1.4 }}>{b.detail || '—'}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {unverifiableCount > 0 && (
                <div style={{ marginTop: 8, fontSize: 11, color: '#fdcb6e' }}>
                  "Unverifiable" usually means Spamhaus rejected the query because it came from a
                  public/open resolver — it does NOT mean listed. Verify via a dedicated resolver if needed.
                </div>
              )}
            </div>
          </div>
        )}
      </div>
    </SideShelf>
  );
};
