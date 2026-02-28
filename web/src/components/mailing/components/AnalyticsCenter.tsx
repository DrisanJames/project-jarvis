import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faChartLine, faEnvelope, faEye, faMousePointer,
  faExclamationTriangle, faBan, faDollarSign, faRobot,
  faBrain,   faShieldAlt, faCalendarAlt, faClock,
  faSyncAlt, faSpinner, faArrowUp, faArrowDown,
  faDatabase, faBolt,
  faChartBar, faTrophy, faUsers, faChartPie
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { useDateFilter } from '../../../context/DateFilterContext';
import './AnalyticsCenter.css';

// ─── Types ───────────────────────────────────────────────────────────────────

interface OverviewData {
  period_days: number;
  totals: { sent: number; opens: number; clicks: number; bounces: number; complaints: number; revenue: number };
  rates: { open_rate: number; click_rate: number; bounce_rate: number; complaint_rate: number };
  daily_trend: { date: string; sent: number; opens: number; clicks: number; bounces: number }[];
}

interface EngagementData {
  distribution: { high: number; medium: number; low: number; none: number };
  top_subscribers: { email: string; score: number; opens: number; clicks: number; last_open: string }[];
  engagement_trend: { date: string; engaged: number }[];
}

interface DeliverabilityData {
  totals: { sent: number; delivered: number; bounced: number; complaints: number };
  rates: { delivery_rate: number; bounce_rate: number; complaint_rate: number };
  bounce_breakdown: { type: string; count: number }[];
  suppressions: { total: number; bounces: number; complaints: number };
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
    sent: number; opens: number; clicks: number; bounces: number;
    revenue: number; open_rate: number; click_rate: number; bounce_rate: number;
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

type TimeRange = '7' | '14' | '30' | '90';

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

const pct = (n: number): string => n.toFixed(1) + '%';

const timeAgo = (s?: string): string => {
  if (!s) return 'Never';
  const d = Date.now() - new Date(s).getTime();
  const m = Math.floor(d / 60000);
  if (m < 1) return 'Just now';
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
};

// ─── Component ───────────────────────────────────────────────────────────────

export const AnalyticsCenter: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id || '';
  const { dateRange } = useDateFilter();

  // Map global date filter to closest local range option
  const rangeMap: Record<string, TimeRange> = {
    last7: '7', last14: '14', mtd: '30', last30: '30',
    last60: '90', last90: '90', lastMonth: '30', ytd: '90', custom: '30',
  };
  const globalRangeHint: TimeRange = rangeMap[dateRange.type] || '30';
  const [range, setRange] = useState<TimeRange>(globalRangeHint);
  const [loading, setLoading] = useState(true);

  // Data
  const [overview, setOverview] = useState<OverviewData | null>(null);
  const [engagement, setEngagement] = useState<EngagementData | null>(null);
  const [deliverability, setDeliverability] = useState<DeliverabilityData | null>(null);
  const [revenue, setRevenue] = useState<RevenueData | null>(null);
  const [campaigns, setCampaigns] = useState<CampaignData | null>(null);
  const [profileStats, setProfileStats] = useState<ProfileStats | null>(null);
  const [agentSummary, setAgentSummary] = useState<ISPAgentSummary | null>(null);
  const [agents, setAgents] = useState<ISPAgent[]>([]);
  const [optimalSend, setOptimalSend] = useState<OptimalSend | null>(null);
  const [dashData, setDashData] = useState<any>(null);

  const fetchAll = useCallback(async () => {
    setLoading(true);
    try {
      const dateSuffix = `&start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
      const [ovRes, engRes, delRes, revRes, campRes, profRes, agentRes, optRes, dashRes] = await Promise.all([
        orgFetch(`/api/mailing/analytics/overview?days=${range}${dateSuffix}`, orgId),
        orgFetch(`/api/mailing/reports/engagement?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}`, orgId),
        orgFetch(`/api/mailing/reports/deliverability?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}`, orgId),
        orgFetch(`/api/mailing/reports/revenue?days=${range}${dateSuffix}`, orgId),
        orgFetch(`/api/mailing/reports/campaigns${dateSuffix}`, orgId),
        orgFetch(`/api/mailing/profiles/stats${dateSuffix}`, orgId),
        orgFetch(`/api/mailing/isp-agents${dateSuffix}`, orgId),
        orgFetch(`/api/mailing/analytics/optimal-send${dateSuffix}`, orgId),
        orgFetch(`/api/mailing/dashboard${dateSuffix}`, orgId),
      ]);
      const [ov, eng, del, rev, camp, prof, ag, opt, dash] = await Promise.all([
        ovRes.json().catch(() => null),
        engRes.json().catch(() => null),
        delRes.json().catch(() => null),
        revRes.json().catch(() => null),
        campRes.json().catch(() => null),
        profRes.json().catch(() => null),
        agentRes.json().catch(() => null),
        optRes.json().catch(() => null),
        dashRes.json().catch(() => null),
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
    } catch (err) {
      console.error('Analytics load error:', err);
    } finally {
      setLoading(false);
    }
  }, [range, orgId, dateRange.startDate, dateRange.endDate, dateRange.type]);

  useEffect(() => { fetchAll(); }, [fetchAll]);

  // ─── Derived Values ────────────────────────────────────────────────────────
  const totals = overview?.totals || { sent: 0, opens: 0, clicks: 0, bounces: 0, complaints: 0, revenue: 0 };
  const rates = overview?.rates || { open_rate: 0, click_rate: 0, bounce_rate: 0, complaint_rate: 0 };
  const trend = overview?.daily_trend || [];
  const maxTrendSent = Math.max(...trend.map(d => d.sent), 1);

  // AI knowledge depth across all agents
  const totalKnowledge = agents.reduce((sum, a) => {
    let score = 0;
    if (a.data_points_total > 100) score += 25;
    else if (a.data_points_total > 50) score += 20;
    else if (a.data_points_total > 10) score += 15;
    else if (a.data_points_total > 0) score += 5;
    if (a.status === 'active') score += 20;
    else if (a.status === 'idle') score += 10;
    if (a.learning_days > 30) score += 20;
    else if (a.learning_days > 7) score += 15;
    else if (a.learning_days > 1) score += 10;
    return sum + Math.min(100, score);
  }, 0);
  const avgKnowledge = agents.length > 0 ? Math.round(totalKnowledge / agents.length) : 0;

  return (
    <div className="ac-container">
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
            {(['7', '14', '30', '90'] as TimeRange[]).map(r => (
              <button key={r} className={range === r ? 'active' : ''} onClick={() => setRange(r)}>
                {r}d
              </button>
            ))}
          </div>
          <button className="ac-refresh-btn" onClick={fetchAll} disabled={loading}>
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
          <div className="ac-kpi-grid">
            <div className="ac-kpi sent">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faEnvelope} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value">{fmt(totals.sent)}</span>
                <span className="ac-kpi-label">Emails Sent</span>
              </div>
            </div>
            <div className="ac-kpi opens">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faEye} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value">{pct(rates.open_rate)}</span>
                <span className="ac-kpi-label">Open Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.opens)} opens</span>
              </div>
            </div>
            <div className="ac-kpi clicks">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faMousePointer} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value">{pct(rates.click_rate)}</span>
                <span className="ac-kpi-label">Click Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.clicks)} clicks</span>
              </div>
            </div>
            <div className="ac-kpi revenue">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faDollarSign} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value">{fmtCurrency(totals.revenue)}</span>
                <span className="ac-kpi-label">Revenue</span>
                <span className="ac-kpi-sub">{fmtCurrency(revenue?.revenue_per_email || 0)}/email</span>
              </div>
            </div>
            <div className="ac-kpi bounces">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faExclamationTriangle} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value">{pct(rates.bounce_rate)}</span>
                <span className="ac-kpi-label">Bounce Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.bounces)} bounced</span>
              </div>
            </div>
            <div className="ac-kpi complaints">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faBan} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value">{pct(rates.complaint_rate)}</span>
                <span className="ac-kpi-label">Complaint Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.complaints)} complaints</span>
              </div>
            </div>
          </div>

          {/* ─── Statistical Intelligence Bar ──────────────────────────── */}
          {trend.length >= 3 && (() => {
            const sentArr = trend.map(d => d.sent);
            // openArr/clickArr used implicitly via trend data in rate calculations
            const mean = (a: number[]) => a.length ? a.reduce((x, y) => x + y, 0) / a.length : 0;
            const stdDev = (a: number[]) => { const m = mean(a); return Math.sqrt(a.reduce((s, v) => s + (v - m) ** 2, 0) / a.length); };
            const pctChg = (a: number, b: number) => b === 0 ? 0 : ((a - b) / b) * 100;

            const sentMean = mean(sentArr);
            const sentStdDev = stdDev(sentArr);
            const sentCV = sentMean > 0 ? (sentStdDev / sentMean) * 100 : 0;

            // Momentum: last 3 vs prior 3
            const last3Sent = sentArr.slice(-3);
            const prior3Sent = sentArr.slice(-6, -3);
            const momentum = prior3Sent.length >= 3 ? pctChg(mean(last3Sent), mean(prior3Sent)) : 0;
            const mSignal = momentum > 10 ? 'bullish' : momentum < -10 ? 'bearish' : 'neutral';

            // Streak
            let upStreak = 0, downStreak = 0;
            for (let i = sentArr.length - 1; i > 0; i--) {
              if (sentArr[i] >= sentArr[i - 1]) { if (downStreak === 0) upStreak++; else break; }
              else { if (upStreak === 0) downStreak++; else break; }
            }

            // Open rate trend
            const orArr = trend.map(d => d.sent > 0 ? (d.opens / d.sent) * 100 : 0);
            const orMomentum = orArr.length >= 6 ? pctChg(mean(orArr.slice(-3)), mean(orArr.slice(-6, -3))) : 0;
            const avgOpenRate = mean(orArr);

            // Click rate trend
            const crArr = trend.map(d => d.opens > 0 ? (d.clicks / d.opens) * 100 : 0);
            const avgCTR = mean(crArr);

            // Volatility label
            const vol = sentCV > 50 ? 'HIGH' : sentCV > 25 ? 'MODERATE' : 'LOW';
            const volColor = sentCV > 50 ? '#ef4444' : sentCV > 25 ? '#f59e0b' : '#22c55e';

            return (
              <div className="ac-intel-bar">
                <div className="ac-intel-item">
                  <span className="ac-intel-label">SEND MOMENTUM</span>
                  <span className="ac-intel-value" style={{ color: mSignal === 'bullish' ? '#22c55e' : mSignal === 'bearish' ? '#ef4444' : '#94a3b8' }}>
                    {mSignal === 'bullish' ? '▲' : mSignal === 'bearish' ? '▼' : '●'} {mSignal.toUpperCase()}
                  </span>
                  <span className="ac-intel-sub" style={{ color: momentum >= 0 ? '#22c55e' : '#ef4444' }}>{momentum >= 0 ? '+' : ''}{momentum.toFixed(1)}%</span>
                </div>
                <div className="ac-intel-item">
                  <span className="ac-intel-label">VOLATILITY</span>
                  <span className="ac-intel-value" style={{ color: volColor }}>{vol}</span>
                  <span className="ac-intel-sub">CV: {sentCV.toFixed(1)}%</span>
                </div>
                <div className="ac-intel-item">
                  <span className="ac-intel-label">AVG DAILY SEND</span>
                  <span className="ac-intel-value">{fmt(Math.round(sentMean))}</span>
                  <span className="ac-intel-sub">&sigma; {fmt(Math.round(sentStdDev))}</span>
                </div>
                <div className="ac-intel-item">
                  <span className="ac-intel-label">AVG OPEN RATE</span>
                  <span className="ac-intel-value">{avgOpenRate.toFixed(1)}%</span>
                  <span className="ac-intel-sub" style={{ color: orMomentum >= 0 ? '#22c55e' : '#ef4444' }}>
                    {orMomentum >= 0 ? '▲' : '▼'} {Math.abs(orMomentum).toFixed(1)}%
                  </span>
                </div>
                <div className="ac-intel-item">
                  <span className="ac-intel-label">AVG CTR</span>
                  <span className="ac-intel-value">{avgCTR.toFixed(2)}%</span>
                </div>
                <div className="ac-intel-item">
                  <span className="ac-intel-label">STREAK</span>
                  <span className="ac-intel-value" style={{ color: upStreak > 0 ? '#22c55e' : downStreak > 0 ? '#ef4444' : '#94a3b8' }}>
                    {upStreak > 0 ? `${upStreak}d ▲` : downStreak > 0 ? `${downStreak}d ▼` : 'FLAT'}
                  </span>
                </div>
                <div className="ac-intel-item">
                  <span className="ac-intel-label">BEST DAY</span>
                  <span className="ac-intel-value">{fmt(Math.max(...sentArr))}</span>
                  <span className="ac-intel-sub">{trend[sentArr.indexOf(Math.max(...sentArr))]?.date?.slice(5) || '–'}</span>
                </div>
              </div>
            );
          })()}

          {/* ─── Two-Column Layout ──────────────────────────────────────── */}
          <div className="ac-two-col">
            {/* LEFT COLUMN */}
            <div className="ac-col-left">

              {/* Daily Trend Chart */}
              <div className="ac-card">
                <h3><FontAwesomeIcon icon={faChartBar} /> Daily Send Volume &amp; Engagement</h3>
                {trend.length === 0 ? (
                  <div className="ac-empty-mini">No trend data available for this period.</div>
                ) : (
                  <div className="ac-trend-chart">
                    <div className="ac-trend-bars">
                      {trend.slice(-Math.min(trend.length, 30)).map((d, i) => {
                        const h = Math.max((d.sent / maxTrendSent) * 100, 2);
                        const openPct = d.sent > 0 ? (d.opens / d.sent) * 100 : 0;
                        return (
                          <div key={i} className="ac-trend-col" title={`${d.date}\nSent: ${d.sent}\nOpens: ${d.opens}\nClicks: ${d.clicks}`}>
                            <div className="ac-bar-wrap">
                              <div className="ac-bar ac-bar-sent" style={{ height: `${h}%` }} />
                              <div className="ac-bar ac-bar-opens" style={{ height: `${Math.max(openPct, 1)}%` }} />
                            </div>
                            <span className="ac-trend-label">{d.date.slice(5)}</span>
                          </div>
                        );
                      })}
                    </div>
                    <div className="ac-trend-legend">
                      <span><span className="ac-legend-dot ac-dot-sent" /> Sent</span>
                      <span><span className="ac-legend-dot ac-dot-opens" /> Opens</span>
                    </div>
                  </div>
                )}
              </div>

              {/* Campaign Performance Table */}
              <div className="ac-card">
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
                          <th>Bounce %</th>
                          <th>Revenue</th>
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
                            <td className={c.bounce_rate < 2 ? 'ac-good' : c.bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(c.bounce_rate)}</td>
                            <td>{fmtCurrency(c.revenue)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>

              {/* Deliverability Health */}
              <div className="ac-card">
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
                      <span>Bounced</span>
                      <strong className="ac-bad">{fmt(deliverability?.totals?.bounced || 0)}</strong>
                    </div>
                    <div className="ac-dd-row">
                      <span>Complaints</span>
                      <strong className="ac-bad">{fmt(deliverability?.totals?.complaints || 0)}</strong>
                    </div>
                    <div className="ac-dd-row">
                      <span>Total Suppressed</span>
                      <strong>{fmt(deliverability?.suppressions?.total || 0)}</strong>
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
            </div>

            {/* RIGHT COLUMN */}
            <div className="ac-col-right">

              {/* AI Intelligence Overview */}
              <div className="ac-card ac-ai-card">
                <h3><FontAwesomeIcon icon={faBrain} /> AI Intelligence</h3>
                <div className="ac-ai-hero">
                  <div className="ac-ai-ring" style={{ '--ai-pct': `${avgKnowledge}%`, '--ai-color': avgKnowledge >= 60 ? '#10b981' : avgKnowledge >= 30 ? '#f59e0b' : '#ef4444' } as React.CSSProperties}>
                    <span className="ac-ai-ring-val">{avgKnowledge}</span>
                    <span className="ac-ai-ring-lbl">Avg Knowledge</span>
                  </div>
                  <div className="ac-ai-stats">
                    <div className="ac-ai-stat">
                      <FontAwesomeIcon icon={faRobot} />
                      <span className="ac-ai-sv">{agentSummary?.total_agents || 0}</span>
                      <span className="ac-ai-sl">ISP Agents</span>
                    </div>
                    <div className="ac-ai-stat">
                      <FontAwesomeIcon icon={faBolt} />
                      <span className="ac-ai-sv">{agentSummary?.active_agents || 0}</span>
                      <span className="ac-ai-sl">Active</span>
                    </div>
                    <div className="ac-ai-stat">
                      <FontAwesomeIcon icon={faBrain} />
                      <span className="ac-ai-sv">{fmt(profileStats?.total_profiles || 0)}</span>
                      <span className="ac-ai-sl">Profiles</span>
                    </div>
                    <div className="ac-ai-stat">
                      <FontAwesomeIcon icon={faDatabase} />
                      <span className="ac-ai-sv">{fmt(agentSummary?.total_data_points || 0)}</span>
                      <span className="ac-ai-sl">Data Pts</span>
                    </div>
                  </div>
                </div>
                <div className="ac-ai-last-learn">
                  <FontAwesomeIcon icon={faClock} /> Last system learning: <strong>{timeAgo(agentSummary?.last_system_learning)}</strong>
                </div>

                {/* ISP Agent Mini Cards */}
                {agents.length > 0 && (
                  <div className="ac-agent-list">
                    {agents.slice(0, 5).map(a => (
                      <div key={a.domain} className="ac-agent-mini">
                        <div className="ac-agent-mini-header">
                          <span className="ac-agent-mini-name">{a.isp}</span>
                          <span className={`ac-agent-mini-status ac-st-${a.status}`}>{a.status}</span>
                        </div>
                        <div className="ac-agent-mini-stats">
                          <span>{a.profiles_count} profiles</span>
                          <span>{fmt(a.data_points_total)} pts</span>
                          <span>{a.learning_days}d learning</span>
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </div>

              {/* Engagement Distribution */}
              <div className="ac-card">
                <h3><FontAwesomeIcon icon={faUsers} /> Engagement Distribution</h3>
                {(() => {
                  const dist = engagement?.distribution || profileStats?.tier_distribution || { high: 0, medium: 0, low: 0, none: 0 };
                  // Normalize: profileStats uses 'inactive', engagement uses 'none'
                  const h = dist.high || 0;
                  const m = dist.medium || 0;
                  const l = dist.low || 0;
                  const n = (dist as any).none || (dist as any).inactive || 0;
                  const total = h + m + l + n || 1;
                  return (
                    <div className="ac-engage-dist">
                      <div className="ac-engage-bar-full">
                        <div className="ac-engage-seg ac-seg-high" style={{ width: `${(h / total) * 100}%` }} title={`High: ${h}`} />
                        <div className="ac-engage-seg ac-seg-med" style={{ width: `${(m / total) * 100}%` }} title={`Medium: ${m}`} />
                        <div className="ac-engage-seg ac-seg-low" style={{ width: `${(l / total) * 100}%` }} title={`Low: ${l}`} />
                        <div className="ac-engage-seg ac-seg-none" style={{ width: `${(n / total) * 100}%` }} title={`Inactive: ${n}`} />
                      </div>
                      <div className="ac-engage-legend">
                        <span><span className="ac-legend-dot ac-seg-high" /> High ({h})</span>
                        <span><span className="ac-legend-dot ac-seg-med" /> Medium ({m})</span>
                        <span><span className="ac-legend-dot ac-seg-low" /> Low ({l})</span>
                        <span><span className="ac-legend-dot ac-seg-none" /> Inactive ({n})</span>
                      </div>
                      <div className="ac-engage-avg">
                        Avg Engagement Score: <strong>{(profileStats?.avg_engagement || 0).toFixed(1)}</strong>
                      </div>
                    </div>
                  );
                })()}
              </div>

              {/* SparkPost Daily Cap */}
              <div className="ac-card ac-cap-card">
                <h3><FontAwesomeIcon icon={faEnvelope} /> SparkPost Daily Cap</h3>
                {(() => {
                  const cap = dashData?.daily_capacity || 0;
                  const used = dashData?.daily_used || 0;
                  const util = dashData?.daily_utilization || 0;
                  const remaining = dashData?.daily_remaining || 0;
                  const status = util > 90 ? 'critical' : util > 70 ? 'warning' : 'healthy';
                  return (
                    <div className="ac-cap-content">
                      <div className="ac-cap-hero">
                        <div className="ac-cap-ring" style={{
                          '--cap-pct': `${Math.min(util, 100)}%`,
                          '--cap-color': status === 'critical' ? '#ef4444' : status === 'warning' ? '#f59e0b' : '#10b981'
                        } as React.CSSProperties}>
                          <span className="ac-cap-ring-val">{util.toFixed(0)}%</span>
                          <span className="ac-cap-ring-lbl">Used</span>
                        </div>
                        <div className="ac-cap-stats">
                          <div className="ac-cap-stat">
                            <span className="ac-cap-sv">{fmt(used)}</span>
                            <span className="ac-cap-sl">Sent Today</span>
                          </div>
                          <div className="ac-cap-stat">
                            <span className="ac-cap-sv">{fmt(cap)}</span>
                            <span className="ac-cap-sl">Daily Cap</span>
                          </div>
                          <div className="ac-cap-stat">
                            <span className="ac-cap-sv ac-cap-remaining">{fmt(remaining)}</span>
                            <span className="ac-cap-sl">Remaining</span>
                          </div>
                        </div>
                      </div>
                      <div className="ac-cap-bar-wrap">
                        <div className="ac-cap-bar">
                          <div className={`ac-cap-fill ac-cap-${status}`} style={{ width: `${Math.min(util, 100)}%` }} />
                        </div>
                        <div className="ac-cap-labels">
                          <span>0</span>
                          <span>{fmt(cap)}</span>
                        </div>
                      </div>
                      {util < 50 && cap > 0 && (
                        <div className="ac-cap-alert ac-cap-under">
                          SparkPost is underutilized. You have capacity for {fmt(remaining)} more emails today.
                        </div>
                      )}
                      {util > 90 && (
                        <div className="ac-cap-alert ac-cap-near">
                          Approaching daily cap. Consider throttling or scheduling remaining sends for tomorrow.
                        </div>
                      )}
                    </div>
                  );
                })()}
              </div>

              {/* ISP Agent Performance */}
              <div className="ac-card ac-agent-perf-card">
                <h3><FontAwesomeIcon icon={faRobot} /> ISP Agent Performance</h3>
                <div className="ac-agent-perf-stats">
                  <div className="ac-agent-perf-stat">
                    <span className="ac-agent-perf-val">{agentSummary?.total_agents || agents.length}</span>
                    <span className="ac-agent-perf-lbl">Total Agents</span>
                  </div>
                  <div className="ac-agent-perf-stat">
                    <span className="ac-agent-perf-val">{agentSummary?.active_agents || agents.filter(a => a.status === 'active').length}</span>
                    <span className="ac-agent-perf-lbl">Active</span>
                  </div>
                  <div className="ac-agent-perf-stat">
                    <span className="ac-agent-perf-val">{fmt(agents.reduce((s, a) => s + a.data_points_total, 0))}</span>
                    <span className="ac-agent-perf-lbl">Total Sends</span>
                  </div>
                  <div className="ac-agent-perf-stat">
                    <span className="ac-agent-perf-val">{agents.length > 0 ? (agents.reduce((s, a) => s + a.avg_engagement, 0) / agents.length).toFixed(1) : '0'}%</span>
                    <span className="ac-agent-perf-lbl">Avg Engagement</span>
                  </div>
                </div>
                {agents.length > 0 && (
                  <div className="ac-agent-perf-chart">
                    <h4>Per-ISP Send Volume</h4>
                    {(() => {
                      const maxPts = Math.max(...agents.map(a => a.data_points_total), 1);
                      return agents.slice(0, 8).map(a => (
                        <div key={a.isp_key} className="ac-agent-perf-bar-row">
                          <span className="ac-agent-perf-bar-label">{a.isp}</span>
                          <div className="ac-agent-perf-bar-track">
                            <div
                              className={`ac-agent-perf-bar-fill ac-st-${a.status}`}
                              style={{ width: `${(a.data_points_total / maxPts) * 100}%` }}
                            />
                          </div>
                          <span className="ac-agent-perf-bar-val">{fmt(a.data_points_total)}</span>
                        </div>
                      ));
                    })()}
                  </div>
                )}
                {agents.length === 0 && (
                  <div className="ac-empty-mini">No ISP agent data available yet.</div>
                )}
              </div>

              {/* Optimal Send Time */}
              <div className="ac-card ac-optimal-card">
                <h3><FontAwesomeIcon icon={faCalendarAlt} /> AI Optimal Send Time</h3>
                {optimalSend ? (
                  <div className="ac-optimal">
                    <div className="ac-optimal-time">
                      <div className="ac-optimal-day">{optimalSend.optimal_day_name}</div>
                      <div className="ac-optimal-hour">{optimalSend.optimal_hour}:00</div>
                    </div>
                    <div className="ac-optimal-conf">
                      <span>Confidence</span>
                      <div className="ac-conf-bar">
                        <div style={{ width: `${(optimalSend.confidence || 0) * 100}%` }} />
                      </div>
                      <span className="ac-conf-pct">{((optimalSend.confidence || 0) * 100).toFixed(0)}%</span>
                    </div>
                    {optimalSend.reasoning && optimalSend.reasoning.length > 0 && (
                      <ul className="ac-reasoning">
                        {optimalSend.reasoning.map((r, i) => <li key={i}>{r}</li>)}
                      </ul>
                    )}
                  </div>
                ) : (
                  <div className="ac-empty-mini">Not enough data to determine optimal send time.</div>
                )}
              </div>

              {/* Revenue by Campaign */}
              <div className="ac-card">
                <h3><FontAwesomeIcon icon={faDollarSign} /> Top Revenue Campaigns</h3>
                {!revenue?.top_revenue_campaigns?.length ? (
                  <div className="ac-empty-mini">No revenue data yet.</div>
                ) : (
                  <div className="ac-rev-list">
                    {revenue.top_revenue_campaigns.slice(0, 5).map((c, i) => (
                      <div key={i} className="ac-rev-item">
                        <span className="ac-rev-rank">#{i + 1}</span>
                        <div className="ac-rev-body">
                          <span className="ac-rev-name">{c.name}</span>
                          <span className="ac-rev-meta">{fmt(c.sent)} sent &middot; {fmtCurrency(c.revenue_per_email)}/email</span>
                        </div>
                        <span className="ac-rev-amount">{fmtCurrency(c.revenue)}</span>
                      </div>
                    ))}
                  </div>
                )}
              </div>

              {/* ISP Distribution */}
              {profileStats?.isp_distribution && Object.keys(profileStats.isp_distribution).length > 0 && (
                <div className="ac-card">
                  <h3><FontAwesomeIcon icon={faChartPie} /> ISP Distribution</h3>
                  <div className="ac-isp-dist">
                    {Object.entries(profileStats.isp_distribution)
                      .sort(([, a], [, b]) => (b as number) - (a as number))
                      .slice(0, 8)
                      .map(([domain, count]) => {
                        const maxCount = Math.max(...Object.values(profileStats.isp_distribution));
                        return (
                          <div key={domain} className="ac-isp-row">
                            <span className="ac-isp-name">{domain}</span>
                            <div className="ac-isp-bar-bg">
                              <div className="ac-isp-bar-fill" style={{ width: `${((count as number) / maxCount) * 100}%` }} />
                            </div>
                            <span className="ac-isp-count">{count as number}</span>
                          </div>
                        );
                      })}
                  </div>
                </div>
              )}

              {/* Industry Benchmarks */}
              <div className="ac-card ac-bench-card">
                <h3><FontAwesomeIcon icon={faChartLine} /> Your Performance vs Industry</h3>
                <div className="ac-bench-grid">
                  {[
                    { label: 'Open Rate', yours: rates.open_rate, bench: 20, unit: '%' },
                    { label: 'Click Rate', yours: rates.click_rate, bench: 3, unit: '%' },
                    { label: 'Bounce Rate', yours: rates.bounce_rate, bench: 2, unit: '%', inverse: true },
                    { label: 'Complaint Rate', yours: rates.complaint_rate, bench: 0.1, unit: '%', inverse: true },
                  ].map((b, i) => {
                    const better = b.inverse ? b.yours < b.bench : b.yours > b.bench;
                    return (
                      <div key={i} className="ac-bench-item">
                        <span className="ac-bench-label">{b.label}</span>
                        <div className="ac-bench-compare">
                          <span className={`ac-bench-yours ${better ? 'ac-good' : 'ac-bad'}`}>
                            {b.yours.toFixed(b.unit === '%' && b.yours < 1 ? 2 : 1)}{b.unit}
                            {better ? <FontAwesomeIcon icon={faArrowUp} /> : <FontAwesomeIcon icon={faArrowDown} />}
                          </span>
                          <span className="ac-bench-vs">vs</span>
                          <span className="ac-bench-industry">{b.bench}{b.unit}</span>
                        </div>
                      </div>
                    );
                  })}
                </div>
              </div>
            </div>
          </div>
        </>
      )}
    </div>
  );
};

export default AnalyticsCenter;
