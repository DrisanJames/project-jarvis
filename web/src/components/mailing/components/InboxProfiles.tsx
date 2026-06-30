import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import type { IconDefinition } from '@fortawesome/fontawesome-svg-core';
import {
  faBrain, faSearch, faChevronLeft, faChevronRight, faSort,
  faSortUp, faSortDown, faEnvelope, faEye, faMousePointer,
  faExclamationTriangle, faClock, faCalendarAlt, faShieldAlt,
  faBullseye, faChartLine, faArrowUp, faArrowDown, faMinus,
  faRobot, faCheck, faTimes, faSpinner, faSyncAlt,
  faUserSecret, faFingerprint, faNetworkWired, faStar,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { AnimatedCounter } from '../shared/AnimatedCounter';
import { usePolling } from '../shared/usePolling';
import {
  Panel, SectionHeader, Stat, Pill, EmptyState, LivePill, PortalKeyframes,
} from '../shared/ui';
import {
  colors, alpha, pageStyle, panelStyle, btnStyle,
  thStyle, numTh, tdStyle, numTd, tableStyle,
} from '../shared/theme';
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

const STATS_POLL_MS = 30_000;

// ─── Engagement scoring helpers ────────────────────────────────────────────────
//
// SCALE NOTE: the underlying mailing_inbox_profiles.engagement_score column is
// DECIMAL(3,2) on a 0–1 scale (DB thresholds 0.70 / 0.40). The API layer
// (HandleGetProfiles / HandleGetProfile / HandleGetProfileStats) already returns
// it pre-scaled to 0–100 via round2(score * 100), and avg_engagement the same
// way. So every score value arriving here is on a 0–100 band — render it as such
// and DO NOT divide again. Tier cutoffs below mirror the DB cutoffs ×100.

const tierFromScore = (score: number): TierFilter =>
  score >= 70 ? 'high' : score >= 40 ? 'medium' : score > 0 ? 'low' : 'inactive';

interface TierMeta { key: Exclude<TierFilter, ''>; label: string; color: string; icon: IconDefinition }

// Indigo → amber → slate band scale.
const TIER_META: Record<Exclude<TierFilter, ''>, TierMeta> = {
  high: { key: 'high', label: 'High', color: colors.indigo400, icon: faStar },
  medium: { key: 'medium', label: 'Medium', color: colors.warning, icon: faChartLine },
  low: { key: 'low', label: 'Low', color: colors.idle, icon: faArrowDown },
  inactive: { key: 'inactive', label: 'Inactive', color: colors.textFaint, icon: faMinus },
};

const tierMeta = (tier: string): TierMeta =>
  (TIER_META as Record<string, TierMeta>)[tier] ?? TIER_META.inactive;

// A labeled engagement band: tier word + the 0–100 numeric, color-coded.
const EngagementBand: React.FC<{ score: number; tier?: string; showValue?: boolean }> = ({
  score, tier, showValue = true,
}) => {
  const meta = tierMeta(tier || tierFromScore(score));
  return (
    <Pill color={meta.color}>
      <FontAwesomeIcon icon={meta.icon} />
      {meta.label}
      {showValue && (
        <span style={{ fontVariantNumeric: 'tabular-nums', opacity: 0.85 }}>{Math.round(score)}</span>
      )}
    </Pill>
  );
};

const scoreColor = (score: number): string => tierMeta(tierFromScore(score)).color;

const getTrendIcon = (trend: string): IconDefinition =>
  trend === 'rising' ? faArrowUp : trend === 'falling' ? faArrowDown : faMinus;

const getTrendColor = (trend: string): string =>
  trend === 'rising' ? colors.success : trend === 'falling' ? colors.danger : colors.textMuted;

const openRateColor = (r: number): string =>
  r >= 20 ? colors.successText : r >= 10 ? colors.warningText : colors.dangerText;

// ISP brand colors (kept: brand identity is meaningful here, not chrome).
const getISPColor = (isp: string): string => {
  switch (isp) {
    case 'Gmail': return '#ea4335';
    case 'Yahoo': return '#7b1fa2';
    case 'Microsoft': return '#0078d4';
    case 'AOL': return '#ff6600';
    case 'Apple': return '#a2aaad';
    case 'Comcast': return '#e60000';
    case 'Proton': return '#6d4aff';
    default: return colors.textFaint;
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
  return Math.round(n).toLocaleString();
};

// ─── Main Component ──────────────────────────────────────────────────────────

export const InboxProfiles: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id;

  // Data state
  const [profiles, setProfiles] = useState<InboxProfile[]>([]);
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

  // ─── Stats (anti-jank polling) ─────────────────────────────────────────────
  // The KPI hero refreshes on an interval without ever blanking the screen:
  // usePolling replaces stats atomically and only on success, keeping the last
  // good values mounted if a refresh fails (surfaced as a stale banner).
  const statsPoll = usePolling<ProfileStats>(
    async (signal) => {
      const res = await orgFetch('/api/mailing/profiles/stats', orgId, { signal });
      if (!res.ok) throw new Error(`stats HTTP ${res.status}`);
      return (await res.json()) as ProfileStats;
    },
    STATS_POLL_MS,
    [orgId],
  );
  const stats = statsPoll.data;

  // ─── Fetch Profiles ──────────────────────────────────────────────────────
  // Driven by user-controlled filters/sort/page (not an interval), so it keeps
  // the prior rows mounted while loading and only shows a spinner on first paint.
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

      const res = await orgFetch(`/api/mailing/profiles?${params}`, orgId);
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
  }, [orgId, search, ispFilter, tierFilter, sortField, sortOrder, page]);

  // ─── Fetch Profile Detail ────────────────────────────────────────────────
  const fetchDetail = async (email: string) => {
    setDetailLoading(true);
    try {
      const [profileRes, decisionRes] = await Promise.all([
        orgFetch(`/api/mailing/profiles/${encodeURIComponent(email)}`, orgId),
        orgFetch(`/api/mailing/analytics/decision/${encodeURIComponent(email)}`, orgId),
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
      const res = await orgFetch('/api/mailing/isp-agents/managed', orgId);
      const data = await res.json();
      const domains = new Set<string>((data.agents || data || []).map((a: { domain: string }) => a.domain));
      setManagedAgentDomains(domains);
    } catch {
      // Silently ignore — badge just won't show
    }
  }, [orgId]);

  // ─── Effects ─────────────────────────────────────────────────────────────
  useEffect(() => {
    fetchManagedAgents();
  }, [fetchManagedAgents]);

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

  const getSortIcon = (field: SortField): IconDefinition => {
    if (sortField !== field) return faSort;
    return sortOrder === 'desc' ? faSortDown : faSortUp;
  };

  // "Top scored" — surface the highest-engagement profiles using params the
  // /profiles endpoint already supports (sort=engagement&order=desc). This is the
  // first lightweight step toward the CDP converter-signal vision.
  //
  // FUTURE ENHANCEMENT (converter signal): once a backend cohort endpoint exists,
  // contrast how the system scored eventual CONVERTERS *prior* to converting vs.
  // non-converters, to derive a leading signal. Do not fabricate that here — it
  // needs real labeled conversion data the current endpoints don't expose.
  const handleTopScored = () => {
    setTierFilter('');
    setSortField('engagement');
    setSortOrder('desc');
    setPage(1);
  };

  const isTopScored = sortField === 'engagement' && sortOrder === 'desc';
  const hasFilters = !!(search || ispFilter || tierFilter);

  // ─── Render ──────────────────────────────────────────────────────────────
  const tierOrder: Exclude<TierFilter, ''>[] = ['high', 'medium', 'low', 'inactive'];

  return (
    <div style={pageStyle}>
      <PortalKeyframes />

      {/* ─── Header ─────────────────────────────────────────────────────── */}
      <header style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: 12, marginBottom: 16 }}>
        <div>
          <h1 style={{ margin: 0, fontSize: 22, color: colors.heading, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faBrain} style={{ color: colors.indigo400 }} />
            Inbox Intel
          </h1>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>
            Per-recipient engagement profiling · learning from every send, open &amp; click · stats refresh every {STATS_POLL_MS / 1000}s
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 14 }}>
          <LivePill live={statsPoll.live} agoSeconds={statsPoll.secondsSinceUpdate} />
          <button
            onClick={() => { statsPoll.refresh(); fetchProfiles(); }}
            disabled={loading}
            style={{ ...btnStyle, display: 'flex', alignItems: 'center', gap: 6, cursor: loading ? 'not-allowed' : 'pointer' }}
          >
            <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} /> Refresh
          </button>
        </div>
      </header>

      {statsPoll.error && (
        <div style={{ background: alpha(colors.danger, '22'), border: `1px solid ${alpha(colors.danger, '66')}`, color: colors.dangerFaint, padding: '10px 14px', borderRadius: 8, marginBottom: 14, fontSize: 13 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          Could not refresh profiling stats: {statsPoll.error} — showing last known values.
        </div>
      )}

      {/* ─── KPI Hero ───────────────────────────────────────────────────── */}
      <Panel accent={colors.indigo500} style={{ marginBottom: 14 }}>
        <SectionHeader title="Audience Intelligence" icon={faFingerprint} />
        <div style={{ display: 'flex', gap: 36, flexWrap: 'wrap', rowGap: 16 }}>
          <Stat
            label="Profiles Built"
            value={<AnimatedCounter value={stats?.total_profiles ?? 0} formatFn={formatNumber} />}
            color={colors.indigo200}
          />
          <Stat
            label="Active (30d)"
            value={<AnimatedCounter value={stats?.recently_active ?? 0} formatFn={formatNumber} />}
            sub="opened in last 30 days"
            color={colors.successText}
          />
          <Stat
            label="Avg Engagement"
            value={<AnimatedCounter value={stats?.avg_engagement ?? 0} decimals={1} />}
            sub="score · 0–100 band"
            color={scoreColor(stats?.avg_engagement ?? 0)}
            title="Mean engagement score across all profiles, 0–100 (High ≥70 · Medium ≥40)"
          />
          <Stat
            label="Avg Open Rate"
            value={<AnimatedCounter value={stats?.avg_open_rate ?? 0} decimals={1} suffix="%" />}
            color={colors.indigo300}
          />
          <Stat
            label="New This Week"
            value={<AnimatedCounter value={stats?.new_this_week ?? 0} formatFn={formatNumber} />}
            sub="last 7 days"
            color={colors.warningText}
          />
        </div>
      </Panel>

      {/* ─── Engagement Scoring distribution (clickable band filters) ──────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="Engagement Scoring"
          icon={faChartLine}
          right={
            <button
              onClick={handleTopScored}
              title="Sort by highest engagement score"
              style={{
                ...btnStyle,
                display: 'flex', alignItems: 'center', gap: 6,
                ...(isTopScored ? { background: alpha(colors.indigo500, '33'), borderColor: alpha(colors.indigo500, '66') } : {}),
              }}
            >
              <FontAwesomeIcon icon={faStar} /> Top scored
            </button>
          }
        />
        <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', alignItems: 'center' }}>
          {tierOrder.map((key) => {
            const meta = TIER_META[key];
            const count = stats?.tier_distribution?.[key] ?? 0;
            const active = tierFilter === key;
            return (
              <button
                key={key}
                onClick={() => { setTierFilter(active ? '' : key); setPage(1); }}
                style={{ background: 'none', border: 'none', padding: 0, cursor: 'pointer' }}
                title={`Filter to ${meta.label} engagement profiles`}
              >
                <Pill
                  color={meta.color}
                  style={{
                    opacity: !tierFilter || active ? 1 : 0.4,
                    boxShadow: active ? `0 0 0 1px ${meta.color}` : 'none',
                  }}
                >
                  <FontAwesomeIcon icon={meta.icon} />
                  {meta.label}
                  <span style={{ fontVariantNumeric: 'tabular-nums' }}>{count.toLocaleString()}</span>
                </Pill>
              </button>
            );
          })}
        </div>
        <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 10 }}>
          Bands on a 0–100 engagement score: High ≥70 · Medium 40–69 · Low 1–39 · Inactive 0
        </div>
      </Panel>

      {/* ─── Filter Bar ─────────────────────────────────────────────────── */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', marginBottom: 14 }}>
        <div style={{ position: 'relative', flex: '1 1 280px', minWidth: 220 }}>
          <FontAwesomeIcon icon={faSearch} style={{ position: 'absolute', left: 12, top: '50%', transform: 'translateY(-50%)', color: colors.textFaint, fontSize: 12 }} />
          <input
            type="text"
            placeholder="Search by email address…"
            value={search}
            onChange={(e) => handleSearchChange(e.target.value)}
            style={{
              width: '100%', boxSizing: 'border-box', padding: '8px 12px 8px 32px',
              background: colors.panelBg, border: `1px solid ${colors.panelBorder}`,
              borderRadius: 8, color: colors.text, fontSize: 13, outline: 'none',
            }}
          />
        </div>
        <select
          value={ispFilter}
          onChange={(e) => { setIspFilter(e.target.value); setPage(1); }}
          style={{
            padding: '8px 12px', background: colors.panelBg, border: `1px solid ${colors.panelBorder}`,
            borderRadius: 8, color: colors.text, fontSize: 13, cursor: 'pointer',
          }}
        >
          <option value="">All Providers</option>
          <option value="gmail">Gmail</option>
          <option value="yahoo">Yahoo</option>
          <option value="microsoft">Microsoft</option>
          <option value="aol">AOL</option>
          <option value="apple">Apple</option>
          <option value="comcast">Comcast</option>
        </select>
        {hasFilters && (
          <button
            onClick={() => { setSearch(''); setIspFilter(''); setTierFilter(''); setPage(1); }}
            style={{ ...btnStyle, color: colors.textMuted, background: 'none', borderColor: colors.panelBorder }}
          >
            Clear Filters
          </button>
        )}
        <div style={{ marginLeft: 'auto', fontSize: 12, color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}>
          {totalProfiles.toLocaleString()} profile{totalProfiles !== 1 ? 's' : ''}
        </div>
      </div>

      {/* ─── Main Content ───────────────────────────────────────────────── */}
      <div className="ii-main">
        {/* ─── Profile Table ────────────────────────────────────────────── */}
        <div style={{ ...panelStyle, padding: 0, overflow: 'hidden' }}>
          {loading && profiles.length === 0 ? (
            <div style={{ textAlign: 'center', padding: 60, color: colors.textMuted }}>
              <FontAwesomeIcon icon={faSpinner} spin size="2x" />
              <p style={{ marginTop: 12, fontSize: 13 }}>Loading inbox profiles…</p>
            </div>
          ) : profiles.length === 0 ? (
            <EmptyState
              icon={faUserSecret}
              title="No Profiles Found"
              hint={hasFilters
                ? 'Try adjusting your filters or search term.'
                : 'Profiles build automatically as emails are sent and engagement is tracked.'}
            />
          ) : (
            <>
              <div style={{ overflowX: 'auto' }}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Inbox</th>
                      <th style={thStyle}>Provider</th>
                      <th style={{ ...thStyle, cursor: 'pointer' }} onClick={() => handleSort('engagement')} title="0–100 engagement score">
                        Engagement <FontAwesomeIcon icon={getSortIcon('engagement')} style={{ fontSize: 10, opacity: 0.7 }} />
                      </th>
                      <th style={thStyle}>Trend</th>
                      <th style={{ ...numTh, cursor: 'pointer' }} onClick={() => handleSort('sent')}>
                        Sent <FontAwesomeIcon icon={getSortIcon('sent')} style={{ fontSize: 10, opacity: 0.7 }} />
                      </th>
                      <th style={{ ...numTh, cursor: 'pointer' }} onClick={() => handleSort('opens')}>
                        Opens <FontAwesomeIcon icon={getSortIcon('opens')} style={{ fontSize: 10, opacity: 0.7 }} />
                      </th>
                      <th style={{ ...numTh, cursor: 'pointer' }} onClick={() => handleSort('clicks')}>
                        Clicks <FontAwesomeIcon icon={getSortIcon('clicks')} style={{ fontSize: 10, opacity: 0.7 }} />
                      </th>
                      <th style={numTh}>Open %</th>
                      <th style={{ ...thStyle, cursor: 'pointer' }} onClick={() => handleSort('recent')}>
                        Last Activity <FontAwesomeIcon icon={getSortIcon('recent')} style={{ fontSize: 10, opacity: 0.7 }} />
                      </th>
                      <th style={thStyle}>First Seen</th>
                    </tr>
                  </thead>
                  <tbody>
                    {profiles.map((p) => {
                      const isActive = selectedDetail?.email === p.email;
                      const ispColor = getISPColor(p.isp);
                      return (
                        <tr
                          key={p.email}
                          role="button"
                          tabIndex={0}
                          onClick={() => fetchDetail(p.email)}
                          onKeyDown={(e) => { if (e.key === 'Enter') fetchDetail(p.email); }}
                          style={{ cursor: 'pointer', background: isActive ? colors.hover : undefined }}
                        >
                          <td style={tdStyle}>
                            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                              <span style={{ width: 8, height: 8, borderRadius: '50%', background: scoreColor(p.engagement_score), flexShrink: 0 }} />
                              <div style={{ minWidth: 0 }}>
                                <div style={{ color: colors.text, fontWeight: 500, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: 240 }}>{p.email}</div>
                                <div style={{ fontSize: 11, color: colors.textFaint, display: 'flex', alignItems: 'center', gap: 6 }}>
                                  {p.domain}
                                  {managedAgentDomains.has(p.domain) && (
                                    <span style={{ color: colors.indigo300, fontSize: 10 }} title="Managed mailbox provider agent active for this domain">
                                      <FontAwesomeIcon icon={faRobot} /> Agent
                                    </span>
                                  )}
                                </div>
                              </div>
                            </div>
                          </td>
                          <td style={tdStyle}>
                            <Pill color={ispColor}>{p.isp || 'Other'}</Pill>
                          </td>
                          <td style={tdStyle}>
                            <EngagementBand score={p.engagement_score} tier={p.engagement_tier} />
                          </td>
                          <td style={tdStyle}>
                            <FontAwesomeIcon icon={getTrendIcon(p.engagement_trend)} style={{ color: getTrendColor(p.engagement_trend) }} title={p.engagement_trend} />
                          </td>
                          <td style={numTd}>{formatNumber(p.total_sent)}</td>
                          <td style={numTd}>{formatNumber(p.total_opens)}</td>
                          <td style={numTd}>{formatNumber(p.total_clicks)}</td>
                          <td style={{ ...numTd, color: openRateColor(p.open_rate) }}>{p.open_rate.toFixed(1)}%</td>
                          <td style={{ ...tdStyle, color: colors.textMuted, whiteSpace: 'nowrap' }}>{timeAgo(p.last_open_at || p.last_sent_at || p.updated_at)}</td>
                          <td style={{ ...tdStyle, color: colors.textMuted, whiteSpace: 'nowrap' }}>{timeAgo(p.first_seen_at)}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>

              {/* Pagination */}
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 14, padding: '12px 16px', borderTop: `1px solid ${colors.hairline}` }}>
                <button
                  disabled={page <= 1}
                  onClick={() => setPage(p => Math.max(1, p - 1))}
                  style={{ ...btnStyle, opacity: page <= 1 ? 0.4 : 1, cursor: page <= 1 ? 'not-allowed' : 'pointer' }}
                >
                  <FontAwesomeIcon icon={faChevronLeft} />
                </button>
                <span style={{ fontSize: 12, color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}>
                  Page {page} of {totalPages}
                </span>
                <button
                  disabled={page >= totalPages}
                  onClick={() => setPage(p => p + 1)}
                  style={{ ...btnStyle, opacity: page >= totalPages ? 0.4 : 1, cursor: page >= totalPages ? 'not-allowed' : 'pointer' }}
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
                <p>Loading profile…</p>
              </div>
            ) : (
              <>
                {/* Profile Header */}
                <div className="ii-detail-header">
                  <div className="ii-detail-score-ring" style={{ '--score-color': scoreColor(selectedDetail.engagement_score), '--score-pct': `${selectedDetail.engagement_score}%` } as React.CSSProperties}>
                    <span className="ii-detail-score-num">{Math.round(selectedDetail.engagement_score)}</span>
                    <span className="ii-detail-score-label">Score</span>
                  </div>
                  <div className="ii-detail-identity">
                    <h3>{selectedDetail.email}</h3>
                    <div className="ii-detail-badges" style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                      <Pill color={getISPColor(detectISPFrontend(selectedDetail.domain))}>
                        {detectISPFrontend(selectedDetail.domain)}
                      </Pill>
                      <EngagementBand score={selectedDetail.engagement_score} tier={selectedDetail.engagement_tier} showValue={false} />
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
                    <div className="ii-decision-verdict" style={{ marginBottom: 10 }}>
                      <Pill color={selectedDecision.should_send ? colors.success : colors.danger}>
                        <FontAwesomeIcon icon={selectedDecision.should_send ? faCheck : faTimes} />
                        {selectedDecision.should_send ? 'Should Send' : 'Do Not Send'}
                      </Pill>
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
