import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faChartLine, faEnvelope, faEye, faMousePointer,
  faExclamationTriangle, faBan, faDollarSign,
  faShieldAlt,
  faSyncAlt, faSpinner,
  faDatabase,
  faTrophy,
} from '@fortawesome/free-solid-svg-icons';
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid,
  Tooltip as RechartsTooltip, Legend, ResponsiveContainer,
} from 'recharts';
import { useAuth } from '../../../contexts/AuthContext';
import { useDateFilter } from '../../../context/DateFilterContext';
import { AnimatedCounter } from '../shared/AnimatedCounter';
import './AnalyticsCenter.css';

// ─── Types ───────────────────────────────────────────────────────────────────

interface OverviewData {
  totals: {
    sent: number; delivered: number; opens: number; clicks: number;
    hard_bounces: number; soft_bounces: number;
    complaints: number; revenue: number;
  };
  rates: { open_rate: number; click_rate: number; hard_bounce_rate: number; soft_bounce_rate: number; complaint_rate: number; delivery_rate?: number; unsubscribe_rate?: number; deferral_rate?: number; bounce_rate?: number };
  daily_trend: { date: string; sent: number; delivered: number; opens: number; clicks: number; hard_bounces: number; soft_bounces: number; complaints: number; deferred: number; unsubscribes: number }[];
  granularity: string;
  range: { start: string; end: string };
  sending_domains?: string[];
  api_version?: string;
}

interface EngagementData {
  distribution: { high: number; medium: number; low: number; none: number };
  top_subscribers: { email: string; score: number; opens: number; clicks: number; last_open: string }[];
  engagement_trend: { date: string; engaged: number }[];
}

interface DeliverabilityData {
  totals: { sent: number; delivered: number; bounced?: number; hard_bounces?: number; soft_bounces?: number; hard_bounced?: number; soft_bounced?: number; complaints: number };
  rates: { delivery_rate: number; bounce_rate?: number; hard_bounce_rate: number; soft_bounce_rate: number; complaint_rate: number };
  bounce_breakdown: { type: string; count: number }[];
  global_suppressions: number;
  exclude_mpp?: boolean;
  api_version?: string;
}

interface RevenueData {
  period_days: number;
  total_revenue: number;
  campaigns_with_revenue: number;
  total_sent: number;
  revenue_per_email: number;
  top_revenue_campaigns: { name: string; sent: number; revenue: number; revenue_per_email: number }[];
  daily_trend: { date: string; revenue: number }[];
}

interface CampaignData {
  campaigns: {
    id: string; name: string; status: string;
    sent: number; opens: number; clicks: number; hard_bounces: number; soft_bounces: number;
    revenue: number; open_rate: number; click_rate: number; hard_bounce_rate: number; soft_bounce_rate: number;
    created_at: string;
  }[];
}

interface ProfileStats {
  total_profiles: number;
  recently_active: number;
  avg_engagement: number;
  avg_open_rate: number;
  new_this_week: number;
  total_sends: number; total_opens: number; total_clicks: number;
  tier_distribution: { high: number; medium: number; low: number; inactive: number };
  isp_distribution: Record<string, number>;
}

interface ISPAgentSummary {
  total_agents: number;
  active_agents: number;
  total_profiles: number;
  total_data_points: number;
  last_system_learning: string;
}

interface ISPAgent {
  isp: string; isp_key: string; domain: string; status: string;
  profiles_count: number; data_points_total: number; avg_engagement: number;
  learning_days: number; last_learning_at: string;
  knowledge: { optimal_send_hour: number; optimal_send_day: number; insights: string[] };
}

interface OptimalSend {
  optimal_hour: number; optimal_day: number; optimal_day_name: string;
  confidence: number; reasoning: string[];
}

interface InfraRow {
  entity: string;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  complaints: number;
  deferred: number;
  open_rate: number;
  click_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  complaint_rate: number;
  deferral_rate: number;
  parent_sent?: number;
  parent_delivered?: number;
}

interface ISPData {
  isp: string; label: string;
  sent: number; delivered: number; opens: number; clicks: number;
  hard_bounces: number; soft_bounces: number; complaints: number;
  open_rate: number; click_rate: number; hard_bounce_rate: number; soft_bounce_rate: number; complaint_rate: number;
}

const ISP_LABELS: Record<string, string> = {
  gmail: 'Gmail', yahoo: 'Yahoo', microsoft: 'Microsoft',
  apple: 'Apple iCloud', comcast: 'Comcast', att: 'AT&T',
  cox: 'Cox', charter: 'Charter/Spectrum', other: 'Other',
};

const ISP_COLORS: Record<string, string> = {
  gmail: '#EA4335', yahoo: '#6001D2', microsoft: '#00A4EF',
  apple: '#A2AAAD', comcast: '#ED1C24', att: '#009FDB',
  cox: '#0070C0', charter: '#0078D4', other: '#6B7280',
};

type TimeRange = '1h' | '24h' | 'today' | '7' | '14' | '30' | '90';

const PAGE_VERSION = '2.0';

function computeDateRange(range: TimeRange): { startDate: string; endDate: string } {
  const now = new Date();
  const endDate = now.toISOString();
  let start: Date;
  switch (range) {
    case '1h':
      start = new Date(now.getTime() - 60 * 60 * 1000);
      return { startDate: start.toISOString(), endDate };
    case '24h':
      start = new Date(now.getTime() - 24 * 60 * 60 * 1000);
      return { startDate: start.toISOString(), endDate };
    case 'today':
      start = new Date(); start.setHours(0, 0, 0, 0);
      return { startDate: start.toISOString(), endDate };
    case '7':
    case '14':
    case '30':
    case '90': {
      const days = Number(range);
      start = new Date(); start.setDate(start.getDate() - days); start.setHours(0, 0, 0, 0);
      return { startDate: start.toISOString(), endDate };
    }
    default:
      start = new Date(); start.setDate(start.getDate() - 30); start.setHours(0, 0, 0, 0);
      return { startDate: start.toISOString(), endDate };
  }
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

const orgFetch = async (url: string, orgId?: string) => {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (orgId) headers['X-Organization-ID'] = orgId;
  return fetch(url, { headers });
};

const fmt = (n: number): string => {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
};

const fmtCurrency = (n: number): string => {
  if (n >= 1_000_000) return '$' + (n / 1_000_000).toFixed(2) + 'M';
  if (n >= 1_000) return '$' + (n / 1_000).toFixed(1) + 'K';
  return '$' + n.toFixed(2);
};

const pct = (n: number | undefined | null): string => (n == null || isNaN(n)) ? '0.0%' : n.toFixed(1) + '%';

// ─── Component ───────────────────────────────────────────────────────────────

export const AnalyticsCenter: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id || '';
  const { dateRange } = useDateFilter();

  // Map global date filter to closest local range option
  const rangeMap: Record<string, TimeRange> = {
    today: 'today', last7: '7', last14: '14', mtd: '30', last30: '30',
    last60: '90', last90: '90', lastMonth: '30', ytd: '90', custom: '30',
  };
  const globalRangeHint: TimeRange = rangeMap[dateRange.type] || 'today';
  const [range, setRange] = useState<TimeRange>(globalRangeHint);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (rangeMap[dateRange.type]) {
      setRange(rangeMap[dateRange.type]);
    }
  }, [dateRange.type]);

  // Data
  const [overview, setOverview] = useState<OverviewData | null>(null);
  const [_engagement, setEngagement] = useState<EngagementData | null>(null);
  const [deliverability, setDeliverability] = useState<DeliverabilityData | null>(null);
  const [revenue, setRevenue] = useState<RevenueData | null>(null);
  const [campaigns, setCampaigns] = useState<CampaignData | null>(null);
  const [_profileStats, setProfileStats] = useState<ProfileStats | null>(null);
  const [_agentSummary, setAgentSummary] = useState<ISPAgentSummary | null>(null);
  const [_agents, setAgents] = useState<ISPAgent[]>([]);
  const [_optimalSend, setOptimalSend] = useState<OptimalSend | null>(null);
  const [_dashData, setDashData] = useState<any>(null);

  // Infrastructure Breakdown state
  const [infraData, setInfraData] = useState<InfraRow[]>([]);
  const [infraLoading, setInfraLoading] = useState(false);
  const [selectedDomain, setSelectedDomain] = useState<string | null>(null);
  const [drilldownType, setDrilldownType] = useState<'ip' | 'isp'>('ip');
  const [selectedCampaign, setSelectedCampaign] = useState<{id: string, name: string} | null>(null);

  // Chart domain filter — drives chart-specific trend data (UI selector TBD)
  const [chartDomain, _setChartDomain] = useState<string>('');
  const [_chartTrend, setChartTrend] = useState<OverviewData['daily_trend']>([]);
  const [_chartLoading, setChartLoading] = useState(false);

  // ISP Performance
  const [ispCards, setIspCards] = useState<ISPData[]>([]);
  const [selectedISP, setSelectedISP] = useState<string | null>(null);
  const [ispTrend, setIspTrend] = useState<OverviewData['daily_trend']>([]);
  const [ispGranularity, setIspGranularity] = useState<string>('day');
  const [ispTrendLoading, setIspTrendLoading] = useState(false);

  // MPP filtering toggle
  const [excludeMPP, setExcludeMPP] = useState(false);

  // Deployment verification: track API versions from responses
  const [apiVersions, setApiVersions] = useState<Record<string, string>>({});

  const fetchAll = useCallback(async () => {
    setLoading(true);
    try {
      const { startDate, endDate } = computeDateRange(range);
      const daysMap: Record<TimeRange, string> = {
        '1h': '1', '24h': '1', 'today': '1', '7': '7', '14': '14', '30': '30', '90': '90',
      };
      const mppParam = excludeMPP ? '&exclude_mpp=true' : '';
      const qp = `?start_date=${encodeURIComponent(startDate)}&end_date=${encodeURIComponent(endDate)}&range_type=${range}&days=${daysMap[range]}${mppParam}`;
      const [ovRes, engRes, delRes, revRes, campRes, profRes, agentRes, optRes, dashRes, ispRes] = await Promise.all([
        orgFetch(`/api/mailing/analytics/overview${qp}`, orgId),
        orgFetch(`/api/mailing/reports/engagement${qp}`, orgId),
        orgFetch(`/api/mailing/reports/deliverability${qp}`, orgId),
        orgFetch(`/api/mailing/reports/revenue${qp}`, orgId),
        orgFetch(`/api/mailing/reports/campaigns${qp}`, orgId),
        orgFetch(`/api/mailing/profiles/stats${qp}`, orgId),
        orgFetch(`/api/mailing/isp-agents${qp}`, orgId),
        orgFetch(`/api/mailing/analytics/optimal-send${qp}`, orgId),
        orgFetch(`/api/mailing/dashboard${qp}`, orgId),
        orgFetch(`/api/mailing/analytics/isp-performance${qp}`, orgId),
      ]);
      const [ov, eng, del, rev, camp, prof, ag, opt, dash, ispPerf] = await Promise.all([
        ovRes.json().catch(() => null),
        engRes.json().catch(() => null),
        delRes.json().catch(() => null),
        revRes.json().catch(() => null),
        campRes.json().catch(() => null),
        profRes.json().catch(() => null),
        agentRes.json().catch(() => null),
        optRes.json().catch(() => null),
        dashRes.json().catch(() => null),
        ispRes.json().catch(() => null),
      ]);
      setOverview(ov);
      setEngagement(eng);
      setDeliverability(del);
      setRevenue(rev);
      setCampaigns(camp);
      setProfileStats(prof);
      setAgentSummary(ag?.summary || null);
      setAgents(ag?.agents || []);
      setOptimalSend(opt);
      setDashData(dash);
      setIspCards(ispPerf?.isps || []);
      setSelectedISP(null);
      setIspTrend([]);
      setApiVersions(prev => ({
        ...prev,
        overview: ov?.api_version || '?',
        engagement: eng?.api_version || '?',
        deliverability: del?.api_version || '?',
        revenue: rev?.api_version || '?',
        campaigns: camp?.api_version || '?',
        isp_performance: ispPerf?.api_version || '?',
      }));
    } catch (err) {
      console.error('Analytics load error:', err);
    } finally {
      setLoading(false);
    }
  }, [range, orgId, dateRange.startDate, dateRange.endDate, dateRange.type, excludeMPP]);

  useEffect(() => { fetchAll(); }, [fetchAll]);

  useEffect(() => {
    if (!selectedISP) { setIspTrend([]); return; }
    let cancelled = false;
    const load = async () => {
      setIspTrendLoading(true);
      try {
        const { startDate, endDate } = computeDateRange(range);
        const mppP = excludeMPP ? '&exclude_mpp=true' : '';
        const qp = `?start_date=${encodeURIComponent(startDate)}&end_date=${encodeURIComponent(endDate)}&range_type=${range}&isp=${selectedISP}${mppP}`;
        const res = await orgFetch(`/api/mailing/analytics/isp-performance${qp}`, orgId);
        const data = await res.json();
        if (!cancelled) {
          setIspTrend(data?.trend || []);
          setIspGranularity(data?.granularity || 'day');
        }
      } catch (err) {
        console.error('ISP trend load error:', err);
        if (!cancelled) setIspTrend([]);
      } finally {
        if (!cancelled) setIspTrendLoading(false);
      }
    };
    load();
    return () => { cancelled = true; };
  }, [selectedISP, range, orgId, dateRange.startDate, dateRange.endDate]);

  const fetchInfrastructure = useCallback(async (domain: string | null, type: 'ip' | 'isp', campaignId?: string) => {
    setInfraLoading(true);
    try {
      const { startDate, endDate } = computeDateRange(range);
      let qp = `?start_date=${encodeURIComponent(startDate)}&end_date=${encodeURIComponent(endDate)}`;
      if (domain) qp += `&domain=${encodeURIComponent(domain)}&drilldown=${type}`;
      if (campaignId) qp += `&campaign_id=${encodeURIComponent(campaignId)}`;

      const res = await orgFetch(`/api/mailing/reports/infrastructure${qp}`, orgId);
      const result = await res.json();
      setInfraData(result.data || []);
      setApiVersions(prev => ({ ...prev, infrastructure: result.api_version || '?' }));
    } catch (err) {
      console.error('Infra load error:', err);
    } finally {
      setInfraLoading(false);
    }
  }, [range, orgId]);

  useEffect(() => {
    fetchInfrastructure(selectedDomain, drilldownType, selectedCampaign?.id);
  }, [fetchInfrastructure, selectedDomain, drilldownType, range, selectedCampaign?.id]);

  // Fetch chart-specific trend when domain filter changes
  useEffect(() => {
    if (!chartDomain) {
      setChartTrend(overview?.daily_trend || []);
      return;
    }
    let cancelled = false;
    (async () => {
      setChartLoading(true);
      try {
        const { startDate, endDate } = computeDateRange(range);
        const qp = `?start_date=${encodeURIComponent(startDate)}&end_date=${encodeURIComponent(endDate)}&trend_domain=${encodeURIComponent(chartDomain)}`;
        const res = await orgFetch(`/api/mailing/analytics/overview${qp}`, orgId);
        const data = await res.json();
        if (!cancelled) setChartTrend(data.daily_trend || []);
      } catch { /* noop */ }
      finally { if (!cancelled) setChartLoading(false); }
    })();
    return () => { cancelled = true; };
  }, [chartDomain, overview, range, orgId]);

  // Sync chart trend from overview when no domain filter
  useEffect(() => {
    if (!chartDomain) setChartTrend(overview?.daily_trend || []);
  }, [overview, chartDomain]);

  // ─── Derived Values ────────────────────────────────────────────────────────
  const totals = overview?.totals || { sent: 0, opens: 0, clicks: 0, hard_bounces: 0, soft_bounces: 0, complaints: 0, revenue: 0 };
  const rates = overview?.rates || { open_rate: 0, click_rate: 0, hard_bounce_rate: 0, soft_bounce_rate: 0, complaint_rate: 0 };
  const trend = overview?.daily_trend || [];
  const granularity = overview?.granularity || 'day';

  return (
    <div className="ac-container ig-scan-line">
      {/* ─── Header ─────────────────────────────────────────────────────── */}
      <div className="ac-header">
        <div className="ac-header-left">
          <div className="ac-header-icon"><FontAwesomeIcon icon={faChartLine} /></div>
          <div>
            <h1>Analytics Center</h1>
            <p>Comprehensive mail performance &amp; AI intelligence metrics</p>
          </div>
        </div>
        <div className="ac-header-right">
          <div className="ac-range-selector">
            {([
              { key: '1h' as TimeRange, label: '1h' },
              { key: '24h' as TimeRange, label: '24h' },
              { key: 'today' as TimeRange, label: 'Today' },
              { key: '7' as TimeRange, label: '7d' },
              { key: '14' as TimeRange, label: '14d' },
              { key: '30' as TimeRange, label: '30d' },
              { key: '90' as TimeRange, label: '90d' },
            ]).map(r => (
              <button key={r.key} className={range === r.key ? 'active' : ''} onClick={() => setRange(r.key)}>
                {r.label}
              </button>
            ))}
          </div>
          <button
            className={`ac-mpp-toggle ${excludeMPP ? 'active' : ''}`}
            onClick={() => setExcludeMPP(prev => !prev)}
            title="Exclude Apple Mail Privacy Protection (MPP) opens from metrics"
          >
            <FontAwesomeIcon icon={faShieldAlt} />
            {excludeMPP ? ' MPP Excluded' : ' Include MPP'}
          </button>
          <button className="ac-refresh-btn ig-btn-glow ig-ripple" onClick={fetchAll} disabled={loading}>
            <FontAwesomeIcon icon={faSyncAlt} spin={loading} /> Refresh
          </button>
        </div>
      </div>

      {loading && !overview ? (
        <div className="ac-loading">
          <FontAwesomeIcon icon={faSpinner} spin size="2x" />
          <p>Loading comprehensive analytics...</p>
        </div>
      ) : (
        <>
          {/* ─── KPI Hero Cards ─────────────────────────────────────────── */}
          <div className="ac-kpi-grid ig-stagger">
            <div className="ac-kpi sent ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faEnvelope} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={totals.sent} formatFn={fmt} /></span>
                <span className="ac-kpi-label">Emails Sent</span>
              </div>
            </div>
            <div className="ac-kpi opens ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faEye} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={rates.open_rate} decimals={1} suffix="%" /></span>
                <span className="ac-kpi-label">Open Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.opens)} opens</span>
              </div>
            </div>
            <div className="ac-kpi clicks ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faMousePointer} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={rates.click_rate} decimals={1} suffix="%" /></span>
                <span className="ac-kpi-label">Click Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.clicks)} clicks</span>
              </div>
            </div>
            <div className="ac-kpi revenue ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faDollarSign} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={totals.revenue} formatFn={fmtCurrency} /></span>
                <span className="ac-kpi-label">Revenue</span>
                <span className="ac-kpi-sub">{fmtCurrency(revenue?.revenue_per_email || 0)}/email</span>
              </div>
            </div>
            <div className="ac-kpi bounces ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faExclamationTriangle} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value" style={{ color: '#ef4444' }}><AnimatedCounter value={rates.hard_bounce_rate} decimals={1} suffix="%" /></span>
                <span className="ac-kpi-label">Hard Bounce Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.hard_bounces)} hard bounced</span>
              </div>
            </div>
            <div className="ac-kpi bounces ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faExclamationTriangle} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value" style={{ color: '#f59e0b' }}><AnimatedCounter value={rates.soft_bounce_rate} decimals={1} suffix="%" /></span>
                <span className="ac-kpi-label">Soft Bounce Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.soft_bounces)} soft bounced</span>
              </div>
            </div>
            <div className="ac-kpi complaints ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faBan} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={rates.complaint_rate} decimals={2} suffix="%" /></span>
                <span className="ac-kpi-label">Complaint Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.complaints)} complaints</span>
              </div>
            </div>
          </div>

          {/* ─── ISP Performance Cards ─────────────────────────────────── */}
          <div className="ac-card ig-card-hover" style={{ gridColumn: '1 / -1' }}>
            <h3><FontAwesomeIcon icon={faChartLine} /> Performance by ISP</h3>
            {ispCards.length === 0 ? (
              <div className="ac-empty-mini">No ISP data available for this period.</div>
            ) : (
              <>
                <div className="ac-isp-grid">
                  {ispCards.filter(c => c.isp !== 'other').map(card => {
                    const score = Math.max(0, Math.min(100, Math.round((card.open_rate || 0) - ((card.hard_bounce_rate || 0) + (card.soft_bounce_rate || 0)) * 2 - (card.complaint_rate || 0) * 10)));
                    const scoreColor = score >= 60 ? '#22c55e' : score >= 30 ? '#f59e0b' : '#ef4444';
                    const isSelected = selectedISP === card.isp;
                    return (
                      <div
                        key={card.isp}
                        className={`ac-isp-card${isSelected ? ' selected' : ''}`}
                        onClick={() => setSelectedISP(isSelected ? null : card.isp)}
                        style={{ borderColor: isSelected ? ISP_COLORS[card.isp] || '#6366f1' : undefined }}
                      >
                        <div className="ac-isp-card-header">
                          <span className="ac-isp-dot" style={{ background: ISP_COLORS[card.isp] || '#6B7280' }} />
                          <span className="ac-isp-name">{ISP_LABELS[card.isp] || card.label}</span>
                          <span className="ac-isp-score" style={{ color: scoreColor }}>{score}</span>
                        </div>
                        <div className="ac-isp-card-metrics">
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val">{fmt(card.sent)}</span>
                            <span className="ac-isp-metric-lbl">Sent</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#22c55e' }}>{card.open_rate}%</span>
                            <span className="ac-isp-metric-lbl">Opens</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#3b82f6' }}>{card.click_rate}%</span>
                            <span className="ac-isp-metric-lbl">Clicks</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#ef4444' }}>{card.hard_bounce_rate ?? 0}%</span>
                            <span className="ac-isp-metric-lbl">Hard Bnc</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#f59e0b' }}>{card.soft_bounce_rate ?? 0}%</span>
                            <span className="ac-isp-metric-lbl">Soft Bnc</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#ef4444' }}>{card.complaint_rate}%</span>
                            <span className="ac-isp-metric-lbl">Complaints</span>
                          </div>
                        </div>
                      </div>
                    );
                  })}
                </div>

                {selectedISP && (
                  <div className="ac-isp-detail">
                    <div className="ac-isp-detail-header">
                      <h4 style={{ color: ISP_COLORS[selectedISP] || '#a5b4fc' }}>
                        {ISP_LABELS[selectedISP] || selectedISP} — Detailed Metrics
                      </h4>
                      <button className="ac-isp-close" onClick={() => setSelectedISP(null)}>&times;</button>
                    </div>
                    {ispTrendLoading ? (
                      <div className="ac-empty-mini"><FontAwesomeIcon icon={faSpinner} spin /> Loading trend…</div>
                    ) : ispTrend.length === 0 ? (
                      <div className="ac-empty-mini">No trend data for this ISP.</div>
                    ) : (
                      <div className="ac-trend-chart">
                        <ResponsiveContainer width="100%" height={300}>
                          <LineChart data={ispTrend} margin={{ top: 10, right: 10, left: 0, bottom: 0 }}>
                            <CartesianGrid strokeDasharray="3 3" stroke="#1e293b" vertical={false} />
                            <XAxis
                              dataKey="date"
                              stroke="#334155"
                              tick={{ fill: '#64748b', fontSize: 11 }}
                              tickLine={false}
                              axisLine={{ stroke: '#1e293b' }}
                              minTickGap={30}
                              tickFormatter={(value: string) => {
                                if (ispGranularity === '10min' || ispGranularity === 'hour') {
                                  const t = value.includes('T') ? value.split('T')[1] : value;
                                  if (range === '1h') return t.slice(0, 5);
                                  const h = parseInt(t.split(':')[0], 10);
                                  const ampm = h >= 12 ? 'PM' : 'AM';
                                  return `${h === 0 ? 12 : h > 12 ? h - 12 : h} ${ampm}`;
                                }
                                return value.slice(5);
                              }}
                            />
                            <YAxis
                              yAxisId="left"
                              stroke="#334155"
                              tick={{ fill: '#64748b', fontSize: 11 }}
                              tickLine={false}
                              axisLine={false}
                              tickFormatter={(v: number) => v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(v)}
                            />
                            <YAxis
                              yAxisId="right"
                              orientation="right"
                              stroke="#334155"
                              tick={{ fill: '#64748b', fontSize: 11 }}
                              tickLine={false}
                              axisLine={false}
                              tickFormatter={(v: number) => v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(v)}
                            />
                            <RechartsTooltip
                              contentStyle={{ background: '#1e293b', border: '1px solid #334155', borderRadius: 8, fontSize: 12 }}
                              labelStyle={{ color: '#94a3b8' }}
                              formatter={(value: number, name: string) => [fmt(value), name.charAt(0).toUpperCase() + name.slice(1)]}
                            />
                            <Legend wrapperStyle={{ fontSize: 11, color: '#94a3b8' }} />
                            <Line yAxisId="left" type="monotone" dataKey="sent" stroke={ISP_COLORS[selectedISP] || '#a5b4fc'} strokeWidth={2} dot={false} name="Sent" />
                            <Line yAxisId="right" type="monotone" dataKey="opens" stroke="#22c55e" strokeWidth={1.5} dot={false} name="Opens" />
                            <Line yAxisId="right" type="monotone" dataKey="clicks" stroke="#3b82f6" strokeWidth={1.5} dot={false} name="Clicks" />
                            <Line yAxisId="right" type="monotone" dataKey="hard_bounces" stroke="#ef4444" strokeWidth={1.5} dot={false} name="Hard Bounces" />
                            <Line yAxisId="right" type="monotone" dataKey="soft_bounces" stroke="#f59e0b" strokeWidth={1.5} dot={false} name="Soft Bounces" />
                            <Line yAxisId="right" type="monotone" dataKey="complaints" stroke="#a855f7" strokeWidth={1.5} dot={false} name="Complaints" />
                          </LineChart>
                        </ResponsiveContainer>
                      </div>
                    )}
                  </div>
                )}
              </>
            )}
          </div>

          {/* ─── Main Content (full-width) ────────────────────────────── */}
          <div className="ac-main-content ig-fade-in">

              {/* Daily Trend Chart */}
              <div className="ac-card ig-card-hover">
                <h3><FontAwesomeIcon icon={faChartLine} /> Send Volume &amp; Engagement</h3>
                {trend.length === 0 ? (
                  <div className="ac-empty-mini">No trend data available for this period.</div>
                ) : (
                  <div className="ac-trend-chart">
                    <ResponsiveContainer width="100%" height={300}>
                      <LineChart data={trend} margin={{ top: 10, right: 10, left: 0, bottom: 0 }}>
                        <CartesianGrid strokeDasharray="3 3" stroke="#1e293b" vertical={false} />
                        <XAxis
                          dataKey="date"
                          stroke="#334155"
                          tick={{ fill: '#64748b', fontSize: 11 }}
                          tickLine={false}
                          axisLine={{ stroke: '#1e293b' }}
                          minTickGap={30}
                          tickFormatter={(value: string) => {
                            if (granularity === '10min' || granularity === 'hour') {
                              const t = value.includes('T') ? value.split('T')[1] : value;
                              if (range === '1h') return t.slice(0, 5);
                              const h = parseInt(t.split(':')[0], 10);
                              const ampm = h >= 12 ? 'PM' : 'AM';
                              return `${h === 0 ? 12 : h > 12 ? h - 12 : h} ${ampm}`;
                            }
                            return value.slice(5);
                          }}
                        />
                        <YAxis
                          yAxisId="sent"
                          stroke="#334155"
                          tick={{ fill: '#64748b', fontSize: 11 }}
                          tickLine={false}
                          axisLine={false}
                          width={55}
                          tickFormatter={(v: number) => {
                            if (v >= 1_000_000) return (v / 1_000_000).toFixed(1) + 'M';
                            if (v >= 1_000) return (v / 1_000).toFixed(0) + 'K';
                            return String(v);
                          }}
                        />
                        <YAxis
                          yAxisId="engagement"
                          orientation="right"
                          stroke="#334155"
                          tick={{ fill: '#475569', fontSize: 10 }}
                          tickLine={false}
                          axisLine={false}
                          width={45}
                          tickFormatter={(v: number) => {
                            if (v >= 1_000_000) return (v / 1_000_000).toFixed(1) + 'M';
                            if (v >= 1_000) return (v / 1_000).toFixed(0) + 'K';
                            return String(v);
                          }}
                        />
                        <RechartsTooltip
                          contentStyle={{ background: '#0f172a', border: '1px solid #1e293b', borderRadius: 8, fontSize: 12 }}
                          labelStyle={{ color: '#94a3b8', marginBottom: 4 }}
                          itemStyle={{ padding: '1px 0' }}
                          labelFormatter={(label: string) => {
                            if (granularity === '10min' || granularity === 'hour') {
                              const t = String(label).includes('T') ? String(label).split('T')[1] : String(label);
                              return t.slice(0, 5);
                            }
                            return label;
                          }}
                          formatter={(value: number) => fmt(value)}
                        />
                        <Legend
                          verticalAlign="top"
                          height={30}
                          wrapperStyle={{ fontSize: 11, color: '#94a3b8' }}
                        />
                        <Line yAxisId="sent" type="monotone" dataKey="sent" stroke="#00e5ff" strokeWidth={2} dot={false} name="Sent" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="opens" stroke="#10b981" strokeWidth={2} dot={false} name="Opens" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="clicks" stroke="#f59e0b" strokeWidth={2} dot={false} name="Clicks" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="hard_bounces" stroke="#ef4444" strokeWidth={1.5} dot={false} name="Hard Bounces" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="soft_bounces" stroke="#f59e0b" strokeWidth={1.5} dot={false} name="Soft Bounces" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="complaints" stroke="#a855f7" strokeWidth={1.5} dot={false} name="Complaints" animationDuration={800} />
                      </LineChart>
                    </ResponsiveContainer>
                  </div>
                )}
              </div>

              {/* Campaign Performance Table */}
              <div className="ac-card ig-card-hover">
                <h3><FontAwesomeIcon icon={faTrophy} /> Campaign Performance</h3>
                {!campaigns?.campaigns?.length ? (
                  <div className="ac-empty-mini">No campaign data yet.</div>
                ) : (
                  <div className="ac-table-wrap">
                    <table className="ac-table">
                      <thead>
                        <tr>
                          <th>Campaign</th>
                          <th>Sent</th>
                          <th>Open %</th>
                          <th>Click %</th>
                          <th>Hard %</th>
                          <th>Soft %</th>
                          <th>Revenue</th>
                          <th>Action</th>
                        </tr>
                      </thead>
                      <tbody>
                        {campaigns.campaigns.slice(0, 10).map(c => (
                          <tr key={c.id}>
                            <td className="ac-camp-name">
                              <span className={`ac-camp-status ${c.status}`} />
                              {c.name}
                            </td>
                            <td>{fmt(c.sent)}</td>
                            <td className={c.open_rate > 20 ? 'ac-good' : c.open_rate > 10 ? 'ac-ok' : 'ac-bad'}>{pct(c.open_rate)}</td>
                            <td className={c.click_rate > 3 ? 'ac-good' : c.click_rate > 1 ? 'ac-ok' : 'ac-bad'}>{pct(c.click_rate)}</td>
                            <td className={c.hard_bounce_rate < 2 ? 'ac-good' : c.hard_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(c.hard_bounce_rate)}</td>
                            <td className={c.soft_bounce_rate < 2 ? 'ac-good' : c.soft_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(c.soft_bounce_rate)}</td>
                            <td>{fmtCurrency(c.revenue)}</td>
                            <td>
                              <button
                                className="ig-btn-glow"
                                style={{ padding: '2px 8px', fontSize: '0.75em', cursor: 'pointer', background: 'transparent', border: '1px solid rgba(255,255,255,0.2)' }}
                                onClick={() => {
                                  setSelectedCampaign({ id: c.id, name: c.name });
                                  setSelectedDomain(null);
                                  document.getElementById('infra-breakdown-section')?.scrollIntoView({ behavior: 'smooth' });
                                }}
                              >
                                View Infra
                              </button>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>

              {/* Deliverability Health */}
              <div className="ac-card ig-card-hover">
                <h3><FontAwesomeIcon icon={faShieldAlt} /> Deliverability Health</h3>
                <div className="ac-deliver-grid">
                  <div className="ac-deliver-metric">
                    <div className="ac-deliver-ring" style={{
                      '--ring-pct': `${deliverability?.rates?.delivery_rate || 0}%`,
                      '--ring-color': (deliverability?.rates?.delivery_rate || 0) > 98 ? '#10b981' : '#f59e0b'
                    } as React.CSSProperties}>
                      <span>{(deliverability?.rates?.delivery_rate || 0).toFixed(1)}%</span>
                    </div>
                    <span className="ac-deliver-label">Delivery Rate</span>
                  </div>
                  <div className="ac-deliver-details">
                    <div className="ac-dd-row">
                      <span>Total Sent</span>
                      <strong>{fmt(deliverability?.totals?.sent || 0)}</strong>
                    </div>
                    <div className="ac-dd-row">
                      <span>Delivered</span>
                      <strong className="ac-good">{fmt(deliverability?.totals?.delivered || 0)}</strong>
                    </div>
                    <div className="ac-dd-row">
                      <span>Hard Bounced</span>
                      <strong className="ac-bad" style={{ color: '#ef4444' }}>{fmt(deliverability?.totals?.hard_bounced || 0)}</strong>
                    </div>
                    <div className="ac-dd-row">
                      <span>Soft Bounced</span>
                      <strong className="ac-bad" style={{ color: '#f59e0b' }}>{fmt(deliverability?.totals?.soft_bounced || 0)}</strong>
                    </div>
                    <div className="ac-dd-row">
                      <span>Complaints</span>
                      <strong className="ac-bad">{fmt(deliverability?.totals?.complaints || 0)}</strong>
                    </div>
                    <div className="ac-dd-row">
                      <span>Total Suppressed</span>
                      <strong>{fmt(deliverability?.global_suppressions || 0)}</strong>
                    </div>
                  </div>
                </div>
                {deliverability?.bounce_breakdown && deliverability.bounce_breakdown.length > 0 && (
                  <div className="ac-bounce-breakdown">
                    <h4>Bounce Breakdown</h4>
                    <div className="ac-bounce-tags">
                      {deliverability.bounce_breakdown.map((b, i) => (
                        <span key={i} className="ac-bounce-tag">{b.type}: {b.count}</span>
                      ))}
                    </div>
                  </div>
                )}
              </div>

              {/* ─── Infrastructure Breakdown ──────────────────────────────────── */}
              <div id="infra-breakdown-section" className="ac-card ig-card-hover">
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '15px', flexWrap: 'wrap', gap: '10px' }}>
                  <h3 style={{ margin: 0, display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '8px' }}>
                    <FontAwesomeIcon icon={faDatabase} /> Infrastructure Breakdown
                    {selectedCampaign && (
                      <span style={{ background: 'rgba(59, 130, 246, 0.2)', color: '#60a5fa', padding: '2px 8px', borderRadius: '12px', fontSize: '0.75em', border: '1px solid rgba(59, 130, 246, 0.3)' }}>
                        Campaign: {selectedCampaign.name}
                      </span>
                    )}
                    {selectedDomain && (
                      <span style={{ color: '#94a3b8', fontSize: '0.85em' }}>
                        / Domain: {selectedDomain}
                      </span>
                    )}
                  </h3>

                  <div className="ac-range-selector">
                    {selectedDomain && (
                      <>
                        <button className={drilldownType === 'ip' ? 'active' : ''} onClick={() => setDrilldownType('ip')}>By IP</button>
                        <button className={drilldownType === 'isp' ? 'active' : ''} onClick={() => setDrilldownType('isp')}>By Target ISP</button>
                        <button style={{ marginLeft: '10px', background: '#334155' }} onClick={() => { setSelectedDomain(null); setDrilldownType('ip'); }}>&larr; Back to Domains</button>
                      </>
                    )}
                    {selectedCampaign && (
                      <button
                        style={{ marginLeft: selectedDomain ? '10px' : '0', background: 'rgba(239, 68, 68, 0.2)', color: '#f87171', border: '1px solid rgba(239, 68, 68, 0.3)' }}
                        onClick={() => { setSelectedCampaign(null); setSelectedDomain(null); }}
                      >
                        Clear Campaign Filter &times;
                      </button>
                    )}
                  </div>
                </div>

                {infraLoading ? (
                  <div className="ac-empty-mini"><FontAwesomeIcon icon={faSpinner} spin /> Loading infrastructure data...</div>
                ) : infraData.length === 0 ? (
                  <div className="ac-empty-mini">No infrastructure data found for this period.</div>
                ) : (
                  <div className="ac-table-wrap">
                    {selectedDomain && infraData[0]?.parent_sent != null && (
                      <div style={{ display: 'flex', gap: '20px', padding: '8px 12px', marginBottom: '10px', background: 'rgba(99,102,241,0.1)', borderRadius: '8px', fontSize: '0.85em', color: '#a5b4fc' }}>
                        <span>Domain Total Delivered: <strong style={{ color: '#e0e7ff' }}>{fmt(infraData[0].parent_delivered ?? 0)}</strong></span>
                        <span style={{ color: '#94a3b8' }}>Rates below are calculated against domain totals</span>
                      </div>
                    )}
                    <table className="ac-table">
                      <thead>
                        <tr>
                          <th>{selectedDomain ? (drilldownType === 'ip' ? 'Sending IP' : 'Target ISP') : 'Sending Domain'}</th>
                          <th>Delivered</th>
                          <th>Deferred</th>
                          <th>Opens</th>
                          <th>Clicks</th>
                          <th>Open %</th>
                          <th>Click %</th>
                          <th>Hard %</th>
                          <th>Soft %</th>
                          <th>Deferral %</th>
                          <th>Complaint %</th>
                          {!selectedDomain && <th>Action</th>}
                        </tr>
                      </thead>
                      <tbody>
                        {infraData.map((row, i) => (
                          <tr key={i}>
                            <td style={{ fontWeight: 500 }}>{row.entity}</td>
                            <td>{fmt(row.delivered)}</td>
                            <td>{fmt(row.deferred)}</td>
                            <td>{fmt(row.opens)}</td>
                            <td>{fmt(row.clicks)}</td>
                            <td className={row.open_rate > 20 ? 'ac-good' : row.open_rate > 10 ? 'ac-ok' : 'ac-bad'}>{pct(row.open_rate)}</td>
                            <td className={row.click_rate > 3 ? 'ac-good' : row.click_rate > 1 ? 'ac-ok' : 'ac-bad'}>{pct(row.click_rate)}</td>
                            <td className={row.hard_bounce_rate < 2 ? 'ac-good' : row.hard_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(row.hard_bounce_rate)}</td>
                            <td className={row.soft_bounce_rate < 2 ? 'ac-good' : row.soft_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(row.soft_bounce_rate)}</td>
                            <td className={row.deferral_rate < 1 ? 'ac-good' : row.deferral_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(row.deferral_rate)}</td>
                            <td className={row.complaint_rate < 0.1 ? 'ac-good' : 'ac-bad'}>{row.complaint_rate.toFixed(2)}%</td>
                            {!selectedDomain && (
                              <td>
                                <button
                                  className="ig-btn-glow"
                                  style={{ padding: '4px 12px', fontSize: '0.85em', cursor: 'pointer', background: 'transparent', border: '1px solid #ffffff', color: '#ffffff' }}
                                  onClick={() => setSelectedDomain(row.entity)}
                                >
                                  Drilldown
                                </button>
                              </td>
                            )}
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>
          </div>
        </>
      )}

      {/* Deployment verification footer */}
      <div style={{
        marginTop: 32, padding: '8px 16px',
        fontSize: '0.7em', color: '#475569',
        borderTop: '1px solid rgba(255,255,255,0.05)',
        display: 'flex', gap: 16, flexWrap: 'wrap',
      }}>
        <span>Page: Analytics Center v{PAGE_VERSION}</span>
        {Object.entries(apiVersions).map(([api, ver]) => (
          <span key={api}>{api}: v{ver}</span>
        ))}
      </div>
    </div>
  );
};

export default AnalyticsCenter;
