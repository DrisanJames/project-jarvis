import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBrain, faSearch, faChevronLeft, faChevronRight, faSort,
  faSortUp, faSortDown, faEnvelope, faEye, faMousePointer,
  faExclamationTriangle, faClock, faCalendarAlt, faShieldAlt,
  faBullseye, faChartLine, faArrowUp, faArrowDown, faMinus,
  faRobot, faCheck, faTimes, faSpinner, faSyncAlt,
  faUserSecret, faFingerprint, faNetworkWired, faFire,
  faStar, faDatabase
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import './InboxProfiles.css';

const orgFetch = async (url: string, orgId?: string, options?: RequestInit) => {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (orgId) headers['X-Organization-ID'] = orgId;
  return fetch(url, { ...options, headers: { ...headers, ...(options?.headers || {}) } });
};

// ─── Types ───────────────────────────────────────────────────────────────────

interface InboxProfile {
  email: string;
  domain: string;
  isp: string;
  total_sent: number;
  total_opens: number;
  total_clicks: number;
  total_bounces: number;
  total_complaints: number;
  engagement_score: number;
  engagement_tier: string;
  engagement_trend: string;
  best_send_hour: number;
  best_send_day: number;
  open_rate: number;
  click_rate: number;
  last_sent_at?: string;
  last_open_at?: string;
  last_click_at?: string;
  first_seen_at?: string;
  updated_at?: string;
}

interface ProfileDetail {
  email: string;
  domain: string;
  engagement_tier: string;
  engagement_score: number;
  metrics: {
    total_sent: number;
    total_opens: number;
    total_clicks: number;
    total_bounces: number;
    total_complaints: number;
    open_rate: number;
    click_rate: number;
    click_to_open_rate: number;
    avg_open_delay_mins: number;
  };
  optimal_send: {
    hour_utc: number;
    day: number;
    day_name: string;
    formatted: string;
  };
  recency: {
    days_since_open: number;
    days_since_click: number;
    last_sent_at?: string;
    last_open_at?: string;
    last_click_at?: string;
  };
  engagement_history: Array<{
    event: string;
    time: string;
    campaign: string;
  }>;
  recommendations: string[];
  risk_assessment: {
    bounce_risk: boolean;
    complaint_risk: boolean;
    inactivity_risk: boolean;
  };
}

interface SendDecision {
  email: string;
  should_send: boolean;
  optimal_hour: number;
  optimal_day: number;
  optimal_time: string;
  confidence: number;
  reasoning: string[];
  risk_factors: string[];
}

interface ProfileStats {
  total_profiles: number;
  recently_active: number;
  avg_engagement: number;
  avg_open_rate: number;
  new_this_week: number;
  total_sends: number;
  total_opens: number;
  total_clicks: number;
  tier_distribution: { high: number; medium: number; low: number; inactive: number };
  isp_distribution: Record<string, number>;
}

type SortField = 'engagement' | 'sent' | 'opens' | 'clicks' | 'recent';
type SortOrder = 'asc' | 'desc';
type TierFilter = '' | 'high' | 'medium' | 'low' | 'inactive';

// ─── Helpers ─────────────────────────────────────────────────────────────────

const getEngagementColor = (score: number): string => {
  if (score >= 70) return '#10b981';
  if (score >= 40) return '#f59e0b';
  if (score > 0) return '#ef4444';
  return '#6b7280';
};

const getTierLabel = (tier: string): string => {
  switch (tier) {
    case 'high': return 'High';
    case 'medium': return 'Medium';
    case 'low': return 'Low';
    case 'inactive': return 'Inactive';
    default: return 'Unknown';
  }
};

const getTierIcon = (tier: string) => {
  switch (tier) {
    case 'high': return faStar;
    case 'medium': return faChartLine;
    case 'low': return faArrowDown;
    case 'inactive': return faMinus;
    default: return faMinus;
  }
};

const getTrendIcon = (trend: string) => {
  switch (trend) {
    case 'rising': return faArrowUp;
    case 'falling': return faArrowDown;
    default: return faMinus;
  }
};

const getTrendColor = (trend: string): string => {
  switch (trend) {
    case 'rising': return '#10b981';
    case 'falling': return '#ef4444';
    default: return '#6b7280';
  }
};

const getISPColor = (isp: string): string => {
  switch (isp) {
    case 'Gmail': return '#ea4335';
    case 'Yahoo': return '#7b1fa2';
    case 'Microsoft': return '#0078d4';
    case 'AOL': return '#ff6600';
    case 'Apple': return '#a2aaad';
    case 'Comcast': return '#e60000';
    case 'Proton': return '#6d4aff';
    default: return '#64748b';
  }
};

const timeAgo = (dateStr?: string): string => {
  if (!dateStr) return 'Never';
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  const months = Math.floor(days / 30);
  return `${months}mo ago`;
};

const formatNumber = (n: number): string => {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
  return n.toLocaleString();
};

// ─── Main Component ──────────────────────────────────────────────────────────

export const InboxProfiles: React.FC = () => {
  const { organization } = useAuth();

  // Data state
  const [profiles, setProfiles] = useState<InboxProfile[]>([]);
  const [stats, setStats] = useState<ProfileStats | null>(null);
  const [selectedDetail, setSelectedDetail] = useState<ProfileDetail | null>(null);
  const [selectedDecision, setSelectedDecision] = useState<SendDecision | null>(null);
  const [loading, setLoading] = useState(true);
  const [detailLoading, setDetailLoading] = useState(false);
  const [managedAgentDomains, setManagedAgentDomains] = useState<Set<string>>(new Set());

  // Filter state
  const [search, setSearch] = useState('');
  const [ispFilter, setIspFilter] = useState('');
  const [tierFilter, setTierFilter] = useState<TierFilter>('');
  const [sortField, setSortField] = useState<SortField>('recent');
  const [sortOrder, setSortOrder] = useState<SortOrder>('desc');
  const [page, setPage] = useState(1);
  const [totalPages, setTotalPages] = useState(1);
  const [totalProfiles, setTotalProfiles] = useState(0);

  const searchTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // ─── Fetch Stats ─────────────────────────────────────────────────────────
  const fetchStats = useCallback(async () => {
    try {
      const res = await orgFetch('/api/mailing/profiles/stats', organization?.id);
      const data = await res.json();
      setStats(data);
    } catch (err) {
      console.error('Failed to load profile stats:', err);
      setStats({
        total_profiles: 0, recently_active: 0, avg_engagement: 0, avg_open_rate: 0,
        new_this_week: 0, total_sends: 0, total_opens: 0, total_clicks: 0,
        tier_distribution: { high: 0, medium: 0, low: 0, inactive: 0 },
        isp_distribution: {},
      });
    }
  }, [organization]);

  // ─── Fetch Profiles ──────────────────────────────────────────────────────
  const fetchProfiles = useCallback(async () => {
    setLoading(true);
    try {
      const params = new URLSearchParams();
      if (search) params.set('search', search);
      if (ispFilter) params.set('isp', ispFilter);
      if (tierFilter) params.set('tier', tierFilter);
      params.set('sort', sortField);
      params.set('order', sortOrder);
      params.set('page', String(page));
      params.set('limit', '50');

      const res = await orgFetch(`/api/mailing/profiles?${params}`, organization?.id);
      const data = await res.json();
      setProfiles(data.profiles || []);
      setTotalPages(data.total_pages || 1);
      setTotalProfiles(data.total || 0);
    } catch (err) {
      console.error('Failed to load profiles:', err);
      setProfiles([]);
    } finally {
      setLoading(false);
    }
  }, [organization, search, ispFilter, tierFilter, sortField, sortOrder, page]);

  // ─── Fetch Profile Detail ────────────────────────────────────────────────
  const fetchDetail = async (email: string) => {
    setDetailLoading(true);
    try {
      const [profileRes, decisionRes] = await Promise.all([
        orgFetch(`/api/mailing/profiles/${encodeURIComponent(email)}`, organization?.id),
        orgFetch(`/api/mailing/analytics/decision/${encodeURIComponent(email)}`, organization?.id),
      ]);
      const profile = await profileRes.json();
      const decision = await decisionRes.json();

      // Ensure nested objects have defaults to prevent render crashes
      const safeProfile: ProfileDetail = {
        email: profile.email || email,
        domain: profile.domain || '',
        engagement_tier: profile.engagement_tier || 'inactive',
        engagement_score: profile.engagement_score || 0,
        metrics: {
          total_sent: 0, total_opens: 0, total_clicks: 0,
          total_bounces: 0, total_complaints: 0,
          open_rate: 0, click_rate: 0, click_to_open_rate: 0,
          avg_open_delay_mins: 0,
          ...(profile.metrics || {}),
        },
        optimal_send: {
          hour_utc: 0, day: 0, day_name: 'N/A', formatted: 'N/A',
          ...(profile.optimal_send || {}),
        },
        recency: {
          days_since_open: -1, days_since_click: -1,
          ...(profile.recency || {}),
        },
        engagement_history: profile.engagement_history || [],
        recommendations: profile.recommendations || [],
        risk_assessment: {
          bounce_risk: false, complaint_risk: false, inactivity_risk: false,
          ...(profile.risk_assessment || {}),
        },
      };

      const safeDecision: SendDecision = {
        email: decision.email || email,
        should_send: decision.should_send ?? true,
        optimal_hour: decision.optimal_hour || 0,
        optimal_day: decision.optimal_day || 0,
        optimal_time: decision.optimal_time || '',
        confidence: decision.confidence || 0,
        reasoning: decision.reasoning || [],
        risk_factors: decision.risk_factors || [],
      };

      setSelectedDetail(safeProfile);
      setSelectedDecision(safeDecision);
    } catch (err) {
      console.error('Failed to load profile detail:', err);
    } finally {
      setDetailLoading(false);
    }
  };

  // ─── Fetch Managed Agent Domains ──────────────────────────────────────
  const fetchManagedAgents = useCallback(async () => {
    try {
      const res = await orgFetch('/api/mailing/isp-agents/managed', organization?.id);
      const data = await res.json();
      const domains = new Set<string>((data.agents || data || []).map((a: { domain: string }) => a.domain));
      setManagedAgentDomains(domains);
    } catch {
      // Silently ignore — badge just won't show
    }
  }, [organization]);

  // ─── Effects ─────────────────────────────────────────────────────────────
  useEffect(() => {
    fetchStats();
    fetchManagedAgents();
  }, [fetchStats, fetchManagedAgents]);

  useEffect(() => {
    fetchProfiles();
  }, [fetchProfiles]);

  // Debounced search
  const handleSearchChange = (value: string) => {
    setSearch(value);
    if (searchTimeoutRef.current) clearTimeout(searchTimeoutRef.current);
    searchTimeoutRef.current = setTimeout(() => {
      setPage(1);
    }, 300);
  };

  // ─── Sort handler ────────────────────────────────────────────────────────
  const handleSort = (field: SortField) => {
    if (sortField === field) {
      setSortOrder(prev => prev === 'desc' ? 'asc' : 'desc');
    } else {
      setSortField(field);
      setSortOrder('desc');
    }
    setPage(1);
  };

  const getSortIcon = (field: SortField) => {
    if (sortField !== field) return faSort;
    return sortOrder === 'desc' ? faSortDown : faSortUp;
  };

  // ─── Render ──────────────────────────────────────────────────────────────
  return (
    <div className="ii-container">
      {/* ─── Header ─────────────────────────────────────────────────────── */}
      <div className="ii-header">
        <div className="ii-header-left">
          <div className="ii-header-icon">
            <FontAwesomeIcon icon={faBrain} />
            <span className="ii-pulse" />
          </div>
          <div>
            <h1>Inbox Intel</h1>
            <p>AI-powered inbox profiling &middot; Learning from every send, open &amp; click</p>
          </div>
        </div>
        <button className="ii-refresh-btn" onClick={() => { fetchStats(); fetchProfiles(); }} disabled={loading}>
          <FontAwesomeIcon icon={faSyncAlt} spin={loading} /> Refresh
        </button>
      </div>

      {/* ─── Stats Bar ──────────────────────────────────────────────────── */}
      <div className="ii-stats-bar">
        <div className="ii-stat-card">
          <div className="ii-stat-icon" style={{ background: 'rgba(99, 102, 241, 0.15)', color: '#818cf8' }}>
            <FontAwesomeIcon icon={faFingerprint} />
          </div>
          <div className="ii-stat-body">
            <span className="ii-stat-value">{formatNumber(stats?.total_profiles || 0)}</span>
            <span className="ii-stat-label">Profiles Built</span>
          </div>
        </div>
        <div className="ii-stat-card">
          <div className="ii-stat-icon" style={{ background: 'rgba(16, 185, 129, 0.15)', color: '#10b981' }}>
            <FontAwesomeIcon icon={faFire} />
          </div>
          <div className="ii-stat-body">
            <span className="ii-stat-value">{formatNumber(stats?.recently_active || 0)}</span>
            <span className="ii-stat-label">Active (30d)</span>
          </div>
        </div>
        <div className="ii-stat-card">
          <div className="ii-stat-icon" style={{ background: 'rgba(245, 158, 11, 0.15)', color: '#f59e0b' }}>
            <FontAwesomeIcon icon={faChartLine} />
          </div>
          <div className="ii-stat-body">
            <span className="ii-stat-value">{(stats?.avg_engagement || 0).toFixed(1)}%</span>
            <span className="ii-stat-label">Avg Engagement</span>
          </div>
        </div>
        <div className="ii-stat-card">
          <div className="ii-stat-icon" style={{ background: 'rgba(59, 130, 246, 0.15)', color: '#3b82f6' }}>
            <FontAwesomeIcon icon={faEye} />
          </div>
          <div className="ii-stat-body">
            <span className="ii-stat-value">{(stats?.avg_open_rate || 0).toFixed(1)}%</span>
            <span className="ii-stat-label">Avg Open Rate</span>
          </div>
        </div>
        <div className="ii-stat-card">
          <div className="ii-stat-icon" style={{ background: 'rgba(236, 72, 153, 0.15)', color: '#ec4899' }}>
            <FontAwesomeIcon icon={faDatabase} />
          </div>
          <div className="ii-stat-body">
            <span className="ii-stat-value">{formatNumber(stats?.new_this_week || 0)}</span>
            <span className="ii-stat-label">New This Week</span>
          </div>
        </div>
      </div>

      {/* ─── Tier Distribution ──────────────────────────────────────────── */}
      {stats && (
        <div className="ii-tier-bar">
          <span className="ii-tier-label">AI Classification:</span>
          <div className="ii-tier-chips">
            <button
              className={`ii-tier-chip ii-tier-high ${tierFilter === 'high' ? 'active' : ''}`}
              onClick={() => { setTierFilter(tierFilter === 'high' ? '' : 'high'); setPage(1); }}
            >
              <FontAwesomeIcon icon={faStar} /> High <span>{stats.tier_distribution.high}</span>
            </button>
            <button
              className={`ii-tier-chip ii-tier-medium ${tierFilter === 'medium' ? 'active' : ''}`}
              onClick={() => { setTierFilter(tierFilter === 'medium' ? '' : 'medium'); setPage(1); }}
            >
              <FontAwesomeIcon icon={faChartLine} /> Medium <span>{stats.tier_distribution.medium}</span>
            </button>
            <button
              className={`ii-tier-chip ii-tier-low ${tierFilter === 'low' ? 'active' : ''}`}
              onClick={() => { setTierFilter(tierFilter === 'low' ? '' : 'low'); setPage(1); }}
            >
              <FontAwesomeIcon icon={faArrowDown} /> Low <span>{stats.tier_distribution.low}</span>
            </button>
            <button
              className={`ii-tier-chip ii-tier-inactive ${tierFilter === 'inactive' ? 'active' : ''}`}
              onClick={() => { setTierFilter(tierFilter === 'inactive' ? '' : 'inactive'); setPage(1); }}
            >
              <FontAwesomeIcon icon={faMinus} /> Inactive <span>{stats.tier_distribution.inactive}</span>
            </button>
          </div>
        </div>
      )}

      {/* ─── Filter Bar ─────────────────────────────────────────────────── */}
      <div className="ii-filter-bar">
        <div className="ii-search-wrap">
          <FontAwesomeIcon icon={faSearch} className="ii-search-icon" />
          <input
            type="text"
            placeholder="Search by email address..."
            value={search}
            onChange={(e) => handleSearchChange(e.target.value)}
            className="ii-search-input"
          />
        </div>
        <select
          value={ispFilter}
          onChange={(e) => { setIspFilter(e.target.value); setPage(1); }}
          className="ii-filter-select"
        >
          <option value="">All ISPs</option>
          <option value="gmail">Gmail</option>
          <option value="yahoo">Yahoo</option>
          <option value="microsoft">Microsoft</option>
          <option value="aol">AOL</option>
          <option value="apple">Apple</option>
          <option value="comcast">Comcast</option>
        </select>
        {(search || ispFilter || tierFilter) && (
          <button className="ii-clear-btn" onClick={() => { setSearch(''); setIspFilter(''); setTierFilter(''); setPage(1); }}>
            Clear Filters
          </button>
        )}
        <div className="ii-result-count">
          {totalProfiles.toLocaleString()} profile{totalProfiles !== 1 ? 's' : ''}
        </div>
      </div>

      {/* ─── Main Content ───────────────────────────────────────────────── */}
      <div className="ii-main">
        {/* ─── Profile Table ────────────────────────────────────────────── */}
        <div className="ii-table-wrap">
          {loading && profiles.length === 0 ? (
            <div className="ii-loading">
              <FontAwesomeIcon icon={faSpinner} spin size="2x" />
              <p>Loading inbox profiles...</p>
            </div>
          ) : profiles.length === 0 ? (
            <div className="ii-empty">
              <FontAwesomeIcon icon={faUserSecret} size="3x" />
              <h3>No Profiles Found</h3>
              <p>
                {search || ispFilter || tierFilter
                  ? 'Try adjusting your filters or search term.'
                  : 'AI will build profiles as emails are sent and engagement is tracked.'}
              </p>
            </div>
          ) : (
            <>
              <table className="ii-table">
                <thead>
                  <tr>
                    <th className="ii-th-email">Inbox</th>
                    <th className="ii-th-isp">ISP</th>
                    <th className="ii-th-score" onClick={() => handleSort('engagement')}>
                      Score <FontAwesomeIcon icon={getSortIcon('engagement')} className="ii-sort-icon" />
                    </th>
                    <th className="ii-th-trend">Trend</th>
                    <th className="ii-th-num" onClick={() => handleSort('sent')}>
                      Sent <FontAwesomeIcon icon={getSortIcon('sent')} className="ii-sort-icon" />
                    </th>
                    <th className="ii-th-num" onClick={() => handleSort('opens')}>
                      Opens <FontAwesomeIcon icon={getSortIcon('opens')} className="ii-sort-icon" />
                    </th>
                    <th className="ii-th-num" onClick={() => handleSort('clicks')}>
                      Clicks <FontAwesomeIcon icon={getSortIcon('clicks')} className="ii-sort-icon" />
                    </th>
                    <th className="ii-th-rate">Open %</th>
                    <th className="ii-th-time" onClick={() => handleSort('recent')}>
                      Last Activity <FontAwesomeIcon icon={getSortIcon('recent')} className="ii-sort-icon" />
                    </th>
                    <th className="ii-th-time">First Seen</th>
                  </tr>
                </thead>
                <tbody>
                  {profiles.map((p) => (
                    <tr
                      key={p.email}
                      role="button"
                      tabIndex={0}
                      className={`ii-row ${selectedDetail?.email === p.email ? 'ii-row-active' : ''}`}
                      onClick={() => fetchDetail(p.email)}
                      onKeyDown={(e) => { if (e.key === 'Enter') fetchDetail(p.email); }}
                    >
                      <td className="ii-td-email">
                        <div className="ii-email-cell">
                          <span className={`ii-activity-dot ii-dot-${p.engagement_tier}`} />
                          <div>
                            <div className="ii-email-text">{p.email}</div>
                            <div className="ii-domain-text">
                              {p.domain}
                              {managedAgentDomains.has(p.domain) && (
                                <span className="ii-agent-badge" title="Managed ISP agent active for this domain">🤖 Agent Active</span>
                              )}
                            </div>
                          </div>
                        </div>
                      </td>
                      <td>
                        <span className="ii-isp-badge" style={{ background: getISPColor(p.isp) + '22', color: getISPColor(p.isp), borderColor: getISPColor(p.isp) + '44' }}>
                          {p.isp || 'Other'}
                        </span>
                      </td>
                      <td>
                        <div className="ii-score-cell">
                          <div className="ii-score-ring" style={{ '--score-color': getEngagementColor(p.engagement_score), '--score-pct': `${p.engagement_score}%` } as React.CSSProperties}>
                            <span>{Math.round(p.engagement_score)}</span>
                          </div>
                        </div>
                      </td>
                      <td>
                        <FontAwesomeIcon
                          icon={getTrendIcon(p.engagement_trend)}
                          style={{ color: getTrendColor(p.engagement_trend) }}
                          title={p.engagement_trend}
                        />
                      </td>
                      <td className="ii-td-num">{formatNumber(p.total_sent)}</td>
                      <td className="ii-td-num">{formatNumber(p.total_opens)}</td>
                      <td className="ii-td-num">{formatNumber(p.total_clicks)}</td>
                      <td className="ii-td-num">
                        <span style={{ color: p.open_rate >= 20 ? '#10b981' : p.open_rate >= 10 ? '#f59e0b' : '#ef4444' }}>
                          {p.open_rate.toFixed(1)}%
                        </span>
                      </td>
                      <td className="ii-td-time">{timeAgo(p.last_open_at || p.last_sent_at || p.updated_at)}</td>
                      <td className="ii-td-time">{timeAgo(p.first_seen_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>

              {/* Pagination */}
              <div className="ii-pagination">
                <button
                  disabled={page <= 1}
                  onClick={() => setPage(p => Math.max(1, p - 1))}
                  className="ii-page-btn"
                >
                  <FontAwesomeIcon icon={faChevronLeft} />
                </button>
                <span className="ii-page-info">
                  Page {page} of {totalPages}
                </span>
                <button
                  disabled={page >= totalPages}
                  onClick={() => setPage(p => p + 1)}
                  className="ii-page-btn"
                >
                  <FontAwesomeIcon icon={faChevronRight} />
                </button>
              </div>
            </>
          )}
        </div>

        {/* ─── Detail Panel ─────────────────────────────────────────────── */}
        {selectedDetail && (
          <div className="ii-detail-panel">
            <button className="ii-detail-close" onClick={() => { setSelectedDetail(null); setSelectedDecision(null); }}>
              <FontAwesomeIcon icon={faTimes} />
            </button>

            {detailLoading ? (
              <div className="ii-detail-loading">
                <FontAwesomeIcon icon={faSpinner} spin size="2x" />
                <p>Loading AI profile...</p>
              </div>
            ) : (
              <>
                {/* Profile Header */}
                <div className="ii-detail-header">
                  <div className="ii-detail-score-ring" style={{ '--score-color': getEngagementColor(selectedDetail.engagement_score), '--score-pct': `${selectedDetail.engagement_score}%` } as React.CSSProperties}>
                    <span className="ii-detail-score-num">{Math.round(selectedDetail.engagement_score)}</span>
                    <span className="ii-detail-score-label">Score</span>
                  </div>
                  <div className="ii-detail-identity">
                    <h3>{selectedDetail.email}</h3>
                    <div className="ii-detail-badges">
                      <span className="ii-isp-badge" style={{ background: getISPColor(detectISPFrontend(selectedDetail.domain)) + '22', color: getISPColor(detectISPFrontend(selectedDetail.domain)), borderColor: getISPColor(detectISPFrontend(selectedDetail.domain)) + '44' }}>
                        {detectISPFrontend(selectedDetail.domain)}
                      </span>
                      <span className={`ii-tier-badge ii-tier-${selectedDetail.engagement_tier}`}>
                        <FontAwesomeIcon icon={getTierIcon(selectedDetail.engagement_tier)} /> {getTierLabel(selectedDetail.engagement_tier)}
                      </span>
                    </div>
                  </div>
                </div>

                {/* Key Metrics */}
                <div className="ii-detail-metrics">
                  <div className="ii-metric">
                    <FontAwesomeIcon icon={faEnvelope} />
                    <span className="ii-metric-val">{selectedDetail.metrics.total_sent}</span>
                    <span className="ii-metric-lbl">Sent</span>
                  </div>
                  <div className="ii-metric">
                    <FontAwesomeIcon icon={faEye} />
                    <span className="ii-metric-val">{selectedDetail.metrics.total_opens}</span>
                    <span className="ii-metric-lbl">Opens</span>
                  </div>
                  <div className="ii-metric">
                    <FontAwesomeIcon icon={faMousePointer} />
                    <span className="ii-metric-val">{selectedDetail.metrics.total_clicks}</span>
                    <span className="ii-metric-lbl">Clicks</span>
                  </div>
                  <div className="ii-metric">
                    <FontAwesomeIcon icon={faExclamationTriangle} />
                    <span className="ii-metric-val">{selectedDetail.metrics.total_bounces}</span>
                    <span className="ii-metric-lbl">Bounces</span>
                  </div>
                </div>

                {/* Rates */}
                <div className="ii-detail-rates">
                  <div className="ii-rate-bar">
                    <div className="ii-rate-header">
                      <span>Open Rate</span>
                      <span className="ii-rate-pct">{selectedDetail.metrics.open_rate.toFixed(1)}%</span>
                    </div>
                    <div className="ii-bar-bg">
                      <div className="ii-bar-fill ii-bar-opens" style={{ width: `${Math.min(selectedDetail.metrics.open_rate, 100)}%` }} />
                    </div>
                  </div>
                  <div className="ii-rate-bar">
                    <div className="ii-rate-header">
                      <span>Click Rate</span>
                      <span className="ii-rate-pct">{selectedDetail.metrics.click_rate.toFixed(1)}%</span>
                    </div>
                    <div className="ii-bar-bg">
                      <div className="ii-bar-fill ii-bar-clicks" style={{ width: `${Math.min(selectedDetail.metrics.click_rate * 2, 100)}%` }} />
                    </div>
                  </div>
                  <div className="ii-rate-bar">
                    <div className="ii-rate-header">
                      <span>Click-to-Open</span>
                      <span className="ii-rate-pct">{selectedDetail.metrics.click_to_open_rate.toFixed(1)}%</span>
                    </div>
                    <div className="ii-bar-bg">
                      <div className="ii-bar-fill ii-bar-cto" style={{ width: `${Math.min(selectedDetail.metrics.click_to_open_rate, 100)}%` }} />
                    </div>
                  </div>
                </div>

                {/* AI Optimal Send Time */}
                <div className="ii-detail-section ii-optimal-send">
                  <h4><FontAwesomeIcon icon={faBullseye} /> AI Optimal Send Time</h4>
                  <div className="ii-optimal-display">
                    <div className="ii-optimal-day">
                      <FontAwesomeIcon icon={faCalendarAlt} />
                      <span>{selectedDetail.optimal_send.day_name}</span>
                    </div>
                    <div className="ii-optimal-hour">
                      <FontAwesomeIcon icon={faClock} />
                      <span>{selectedDetail.optimal_send.hour_utc}:00 UTC</span>
                    </div>
                  </div>
                  {selectedDetail.metrics.avg_open_delay_mins > 0 && (
                    <div className="ii-avg-delay">
                      Avg. opens {selectedDetail.metrics.avg_open_delay_mins} min after send
                    </div>
                  )}
                </div>

                {/* Recency / Timeline */}
                <div className="ii-detail-section">
                  <h4><FontAwesomeIcon icon={faClock} /> Activity Timeline</h4>
                  <div className="ii-timeline">
                    {selectedDetail.recency.last_sent_at && (
                      <div className="ii-timeline-item">
                        <span className="ii-tl-dot ii-tl-sent" />
                        <div className="ii-tl-content">
                          <span className="ii-tl-label">Last Mailed</span>
                          <span className="ii-tl-time">{new Date(selectedDetail.recency.last_sent_at).toLocaleString()}</span>
                        </div>
                      </div>
                    )}
                    {selectedDetail.recency.last_open_at && (
                      <div className="ii-timeline-item">
                        <span className="ii-tl-dot ii-tl-open" />
                        <div className="ii-tl-content">
                          <span className="ii-tl-label">Last Opened</span>
                          <span className="ii-tl-time">{new Date(selectedDetail.recency.last_open_at).toLocaleString()}</span>
                          {selectedDetail.recency.days_since_open >= 0 && (
                            <span className="ii-tl-ago">{selectedDetail.recency.days_since_open}d ago</span>
                          )}
                        </div>
                      </div>
                    )}
                    {selectedDetail.recency.last_click_at && (
                      <div className="ii-timeline-item">
                        <span className="ii-tl-dot ii-tl-click" />
                        <div className="ii-tl-content">
                          <span className="ii-tl-label">Last Clicked</span>
                          <span className="ii-tl-time">{new Date(selectedDetail.recency.last_click_at).toLocaleString()}</span>
                          {selectedDetail.recency.days_since_click >= 0 && (
                            <span className="ii-tl-ago">{selectedDetail.recency.days_since_click}d ago</span>
                          )}
                        </div>
                      </div>
                    )}
                  </div>
                </div>

                {/* Engagement History */}
                {selectedDetail.engagement_history && selectedDetail.engagement_history.length > 0 && (
                  <div className="ii-detail-section">
                    <h4><FontAwesomeIcon icon={faNetworkWired} /> Engagement History</h4>
                    <div className="ii-history-list">
                      {selectedDetail.engagement_history.map((evt, idx) => (
                        <div key={idx} className="ii-history-item">
                          <span className={`ii-history-dot ii-evt-${evt.event}`} />
                          <div className="ii-history-body">
                            <span className="ii-history-event">{(evt.event || 'event').replace(/_/g, ' ')}</span>
                            {evt.campaign && <span className="ii-history-campaign">{evt.campaign}</span>}
                          </div>
                          <span className="ii-history-time">{evt.time ? timeAgo(evt.time) : ''}</span>
                        </div>
                      ))}
                    </div>
                  </div>
                )}

                {/* Risk Assessment */}
                <div className="ii-detail-section">
                  <h4><FontAwesomeIcon icon={faShieldAlt} /> Risk Assessment</h4>
                  <div className="ii-risk-grid">
                    <div className={`ii-risk-item ${selectedDetail.risk_assessment.bounce_risk ? 'ii-risk-warn' : 'ii-risk-ok'}`}>
                      <FontAwesomeIcon icon={selectedDetail.risk_assessment.bounce_risk ? faExclamationTriangle : faCheck} />
                      <span>Bounce Risk</span>
                    </div>
                    <div className={`ii-risk-item ${selectedDetail.risk_assessment.complaint_risk ? 'ii-risk-warn' : 'ii-risk-ok'}`}>
                      <FontAwesomeIcon icon={selectedDetail.risk_assessment.complaint_risk ? faExclamationTriangle : faCheck} />
                      <span>Complaint Risk</span>
                    </div>
                    <div className={`ii-risk-item ${selectedDetail.risk_assessment.inactivity_risk ? 'ii-risk-warn' : 'ii-risk-ok'}`}>
                      <FontAwesomeIcon icon={selectedDetail.risk_assessment.inactivity_risk ? faExclamationTriangle : faCheck} />
                      <span>Inactivity Risk</span>
                    </div>
                  </div>
                </div>

                {/* AI Recommendations */}
                {selectedDetail.recommendations && selectedDetail.recommendations.length > 0 && (
                  <div className="ii-detail-section ii-recs-section">
                    <h4><FontAwesomeIcon icon={faRobot} /> AI Recommendations</h4>
                    <ul className="ii-recs-list">
                      {selectedDetail.recommendations.map((rec, idx) => (
                        <li key={idx}>{rec}</li>
                      ))}
                    </ul>
                  </div>
                )}

                {/* AI Send Decision */}
                {selectedDecision && (
                  <div className={`ii-decision-card ${selectedDecision.should_send ? 'ii-decision-go' : 'ii-decision-stop'}`}>
                    <div className="ii-decision-header">
                      <FontAwesomeIcon icon={faRobot} />
                      <h4>AI Send Decision</h4>
                    </div>
                    <div className="ii-decision-verdict">
                      <FontAwesomeIcon icon={selectedDecision.should_send ? faCheck : faTimes} />
                      <span>{selectedDecision.should_send ? 'SHOULD SEND' : 'DO NOT SEND'}</span>
                    </div>
                    <div className="ii-decision-confidence">
                      <span>Confidence: {(selectedDecision.confidence * 100).toFixed(0)}%</span>
                      <div className="ii-bar-bg">
                        <div className="ii-bar-fill ii-bar-confidence" style={{ width: `${selectedDecision.confidence * 100}%` }} />
                      </div>
                    </div>
                    {selectedDecision.reasoning && selectedDecision.reasoning.length > 0 && (
                      <div className="ii-decision-reasons">
                        <strong>Reasoning:</strong>
                        <ul>
                          {selectedDecision.reasoning.map((r, i) => <li key={i}>{r}</li>)}
                        </ul>
                      </div>
                    )}
                    {selectedDecision.risk_factors && selectedDecision.risk_factors.length > 0 && (
                      <div className="ii-decision-risks">
                        <strong>Risk Factors:</strong>
                        <ul>
                          {selectedDecision.risk_factors.map((r, i) => <li key={i}>{r}</li>)}
                        </ul>
                      </div>
                    )}
                  </div>
                )}
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
};

// Frontend ISP detection helper
function detectISPFrontend(domain: string): string {
  const d = (domain || '').toLowerCase();
  if (d === 'gmail.com') return 'Gmail';
  if (d === 'yahoo.com' || d === 'ymail.com' || d.startsWith('yahoo.')) return 'Yahoo';
  if (d === 'outlook.com' || d === 'hotmail.com' || d === 'live.com' || d === 'msn.com') return 'Microsoft';
  if (d === 'aol.com') return 'AOL';
  if (d === 'icloud.com' || d === 'me.com' || d === 'mac.com') return 'Apple';
  if (d === 'comcast.net' || d === 'xfinity.com') return 'Comcast';
  if (d === 'protonmail.com' || d === 'proton.me') return 'Proton';
  return 'Other';
}

export default InboxProfiles;
