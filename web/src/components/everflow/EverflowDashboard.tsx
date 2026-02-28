import { useState, useEffect, useCallback, useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSync, faDollarSign, faMousePointer, faBullseye, faArrowUp, faArrowDown, faServer, faChartLine, faMinus } from '@fortawesome/free-solid-svg-icons';
import { MetricCard } from '../common/MetricCard';
import { OfferPerformanceTable } from './OfferPerformanceTable';
import { PropertyPerformanceTable } from './PropertyPerformanceTable';
import { CampaignRevenueTable } from './CampaignRevenueTable';
import { CampaignDetailsModal } from './CampaignDetailsModal';
import { ESPRevenueTable } from './ESPRevenueTable';
import { RevenueChart } from './RevenueChart';
import { useDateFilter } from '../../context/DateFilterContext';
import { 
  EverflowDashboardResponse,
  EverflowDailyPerformance,
  EverflowOfferPerformance,
  EverflowPropertyPerformance,
  EverflowCampaignRevenue,
  EverflowRevenueBreakdown,
  EverflowESPRevenue,
  EverflowESPRevenueResponse,
} from '../../types';

type TabId = 'overview' | 'offers' | 'properties' | 'campaigns' | 'esp' | 'trends';

interface Tab {
  id: TabId;
  label: string;
}

const TABS: Tab[] = [
  { id: 'overview', label: 'Overview' },
  { id: 'offers', label: 'Offers' },
  { id: 'properties', label: 'Properties' },
  { id: 'campaigns', label: 'Campaigns' },
  { id: 'esp', label: 'By ESP' },
  { id: 'trends', label: 'Trends' },
];

export const EverflowDashboard: React.FC = () => {
  const [activeTab, setActiveTab] = useState<TabId>('overview');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastFetch, setLastFetch] = useState<Date | null>(null);
  
  // Use global date filter
  const { dateRange } = useDateFilter();
  
  // Data state
  const [todayMetrics, setTodayMetrics] = useState({
    clicks: 0,
    conversions: 0,
    revenue: 0,
    payout: 0,
  });
  const [dailyPerformance, setDailyPerformance] = useState<EverflowDailyPerformance[]>([]);
  const [offerPerformance, setOfferPerformance] = useState<EverflowOfferPerformance[]>([]);
  const [propertyPerformance, setPropertyPerformance] = useState<EverflowPropertyPerformance[]>([]);
  const [campaignRevenue, setCampaignRevenue] = useState<EverflowCampaignRevenue[]>([]);
  const [filteredCampaigns, setFilteredCampaigns] = useState<EverflowCampaignRevenue[]>([]);
  const [campaignsLoading, setCampaignsLoading] = useState(false);
  const [revenueBreakdown, setRevenueBreakdown] = useState<EverflowRevenueBreakdown | null>(null);
  const [espRevenue, setEspRevenue] = useState<EverflowESPRevenue[]>([]);
  const [espLoading, setEspLoading] = useState(false);
  const [selectedCampaignId, setSelectedCampaignId] = useState<string | null>(null);
  
  // Minimum audience size filter (10,000)
  const MIN_AUDIENCE_SIZE = 10000;

  // Daily performance is already filtered by the backend based on date range
  const filteredDailyPerformance = dailyPerformance;

  const fetchDashboard = useCallback(async () => {
    try {
      setLoading(true);
      setError(null);
      
      // Build URL with date range params from global filter
      const params = new URLSearchParams({
        start_date: dateRange.startDate,
        end_date: dateRange.endDate,
        range_type: dateRange.type,
      });
      
      const response = await fetch(`/api/everflow/dashboard?${params}`);
      if (!response.ok) {
        const errorData = await response.json().catch(() => ({}));
        throw new Error(errorData.error || `HTTP ${response.status}`);
      }
      
      const data: EverflowDashboardResponse = await response.json();
      
      setTodayMetrics({
        clicks: data.today_clicks || 0,
        conversions: data.today_conversions || 0,
        revenue: data.today_revenue || 0,
        payout: data.today_payout || 0,
      });
      setDailyPerformance(data.daily_performance || []);
      setOfferPerformance(data.offer_performance || []);
      setPropertyPerformance(data.property_performance || []);
      setCampaignRevenue(data.campaign_revenue || []);
      setRevenueBreakdown(data.revenue_breakdown || null);
      
      // Handle zero/invalid last_fetch date
      if (data.last_fetch && !data.last_fetch.startsWith('0001-01-01')) {
        setLastFetch(new Date(data.last_fetch));
      } else {
        setLastFetch(null);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch Everflow data');
    } finally {
      setLoading(false);
    }
  }, [dateRange]); // Refetch when date range changes

  // Fetch campaigns with audience filter
  // Note: Filtering is instant - data is pre-enriched in background by the collector
  const fetchFilteredCampaigns = useCallback(async () => {
    try {
      setCampaignsLoading(true);
      const response = await fetch(`/api/everflow/campaigns?min_audience=${MIN_AUDIENCE_SIZE}`);
      if (!response.ok) {
        // Fall back to unfiltered campaigns on error
        setFilteredCampaigns(campaignRevenue);
        return;
      }
      const data = await response.json();
      setFilteredCampaigns(data.campaigns || []);
    } catch {
      // Fall back to unfiltered campaigns on error
      setFilteredCampaigns(campaignRevenue);
    } finally {
      setCampaignsLoading(false);
    }
  }, [campaignRevenue]);

  useEffect(() => {
    fetchDashboard();
    // Refresh every 5 minutes
    const interval = setInterval(fetchDashboard, 5 * 60 * 1000);
    return () => clearInterval(interval);
  }, [fetchDashboard]);

  // Fetch filtered campaigns when switching to campaigns tab
  useEffect(() => {
    if (activeTab === 'campaigns' && filteredCampaigns.length === 0 && !campaignsLoading) {
      fetchFilteredCampaigns();
    }
  }, [activeTab, filteredCampaigns.length, campaignsLoading, fetchFilteredCampaigns]);

  // Fetch ESP revenue when switching to ESP tab
  const fetchESPRevenue = useCallback(async () => {
    try {
      setEspLoading(true);
      const response = await fetch(`/api/everflow/esp-revenue?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`);
      if (!response.ok) {
        return;
      }
      const data: EverflowESPRevenueResponse = await response.json();
      setEspRevenue(data.esp_revenue || []);
    } catch {
      // Ignore errors - ESP data is optional
    } finally {
      setEspLoading(false);
    }
  }, [dateRange]);

  useEffect(() => {
    if (activeTab === 'esp' && espRevenue.length === 0 && !espLoading) {
      fetchESPRevenue();
    }
  }, [activeTab, espRevenue.length, espLoading, fetchESPRevenue]);

  // Calculate totals based on filtered data
  const totalRevenue = filteredDailyPerformance.reduce((sum, d) => sum + d.revenue, 0);
  const totalClicks = filteredDailyPerformance.reduce((sum, d) => sum + d.clicks, 0);
  const totalConversions = filteredDailyPerformance.reduce((sum, d) => sum + d.conversions, 0);
  const overallConversionRate = totalClicks > 0 ? totalConversions / totalClicks : 0;
  const overallEPC = totalClicks > 0 ? totalRevenue / totalClicks : 0;
  
  // Get current date range label from global filter
  const currentRangeLabel = dateRange.label;

  const formatCurrency = (value: number): string => {
    return new Intl.NumberFormat('en-US', {
      style: 'currency',
      currency: 'USD',
      minimumFractionDigits: 2,
    }).format(value);
  };

  const formatNumber = (value: number): string => {
    if (value >= 1000000) {
      return `${(value / 1000000).toFixed(1)}M`;
    }
    if (value >= 1000) {
      return `${(value / 1000).toFixed(1)}K`;
    }
    return new Intl.NumberFormat('en-US').format(value);
  };

  // ═══════════════════════════════════════════════════════════════════════
  // STATISTICAL ENGINE — Computes trend analytics, moving averages,
  // momentum, volatility, and period-over-period deltas.
  // ═══════════════════════════════════════════════════════════════════════

  const trendStats = useMemo(() => {
    const dp = filteredDailyPerformance;
    if (dp.length < 2) return null;

    const revenues = dp.map(d => d.revenue);
    const clicks = dp.map(d => d.clicks);
    const conversions = dp.map(d => d.conversions);
    const epcs = dp.map(d => d.epc);

    const mean = (arr: number[]) => arr.length ? arr.reduce((a, b) => a + b, 0) / arr.length : 0;
    const median = (arr: number[]) => {
      const s = [...arr].sort((a, b) => a - b);
      const m = Math.floor(s.length / 2);
      return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
    };
    const stdDev = (arr: number[]) => {
      const m = mean(arr);
      return Math.sqrt(arr.reduce((s, v) => s + (v - m) ** 2, 0) / arr.length);
    };
    const pctChange = (curr: number, prev: number) => prev === 0 ? 0 : ((curr - prev) / prev) * 100;

    // SMA helpers
    const sma = (arr: number[], window: number) => {
      if (arr.length < window) return arr.map(() => mean(arr));
      return arr.map((_, i) => {
        if (i < window - 1) return mean(arr.slice(0, i + 1));
        return mean(arr.slice(i - window + 1, i + 1));
      });
    };

    // Day-over-day deltas
    const revDeltas = revenues.map((r, i) => i > 0 ? pctChange(r, revenues[i - 1]) : 0);

    // Streaks
    let upStreak = 0, downStreak = 0;
    for (let i = revenues.length - 1; i > 0; i--) {
      if (revenues[i] >= revenues[i - 1]) { if (downStreak === 0) upStreak++; else break; }
      else { if (upStreak === 0) downStreak++; else break; }
    }

    // Momentum: compare last 3 days avg to prior 3 days avg
    const last3 = revenues.slice(-3);
    const prior3 = revenues.slice(-6, -3);
    const momentum = prior3.length >= 3 ? pctChange(mean(last3), mean(prior3)) : 0;
    const momentumSignal = momentum > 5 ? 'bullish' : momentum < -5 ? 'bearish' : 'neutral';

    // Volatility
    const revenueStdDev = stdDev(revenues);
    const revenueMean = mean(revenues);
    const cv = revenueMean > 0 ? (revenueStdDev / revenueMean) * 100 : 0; // Coefficient of variation
    const volatility = cv > 50 ? 'high' : cv > 25 ? 'moderate' : 'low';

    // Period halves comparison
    const halfIdx = Math.floor(dp.length / 2);
    const firstHalfRev = revenues.slice(0, halfIdx);
    const secondHalfRev = revenues.slice(halfIdx);
    const periodTrend = pctChange(mean(secondHalfRev), mean(firstHalfRev));

    // Best/worst days
    const bestDayIdx = revenues.indexOf(Math.max(...revenues));
    const worstDayIdx = revenues.indexOf(Math.min(...revenues));

    return {
      revenue: { mean: mean(revenues), median: median(revenues), stdDev: revenueStdDev, min: Math.min(...revenues), max: Math.max(...revenues), total: totalRevenue },
      clicks: { mean: mean(clicks), median: median(clicks), stdDev: stdDev(clicks), min: Math.min(...clicks), max: Math.max(...clicks), total: totalClicks },
      conversions: { mean: mean(conversions), median: median(conversions), stdDev: stdDev(conversions), min: Math.min(...conversions), max: Math.max(...conversions), total: totalConversions },
      epc: { mean: mean(epcs), median: median(epcs), stdDev: stdDev(epcs), min: Math.min(...epcs), max: Math.max(...epcs) },
      sma3Revenue: sma(revenues, 3),
      sma7Revenue: sma(revenues, 7),
      revDeltas,
      upStreak,
      downStreak,
      momentum,
      momentumSignal,
      cv,
      volatility,
      periodTrend,
      bestDay: dp[bestDayIdx]?.date,
      worstDay: dp[worstDayIdx]?.date,
    };
  }, [filteredDailyPerformance, totalRevenue, totalClicks, totalConversions]);

  // Trend direction helper
  const TrendArrow: React.FC<{ value: number; suffix?: string; invert?: boolean }> = ({ value, suffix = '%', invert = false }) => {
    const isPositive = invert ? value < 0 : value > 0;
    const isNegative = invert ? value > 0 : value < 0;
    const color = isPositive ? '#22c55e' : isNegative ? '#ef4444' : '#6b7280';
    const icon = value > 0 ? faArrowUp : value < 0 ? faArrowDown : faMinus;
    return (
      <span style={{ color, fontWeight: 600, fontSize: '0.8rem', fontFamily: 'monospace', display: 'inline-flex', alignItems: 'center', gap: '2px' }}>
        <FontAwesomeIcon icon={icon} style={{ fontSize: '0.65rem' }} />
        {Math.abs(value).toFixed(1)}{suffix}
      </span>
    );
  };

  // Sparkline mini chart
  const Sparkline: React.FC<{ data: number[]; width?: number; height?: number; color?: string }> = ({ data, width = 80, height = 24, color = '#22c55e' }) => {
    if (data.length < 2) return null;
    const max = Math.max(...data);
    const min = Math.min(...data);
    const range = max - min || 1;
    const points = data.map((v, i) => `${(i / (data.length - 1)) * width},${height - ((v - min) / range) * (height - 2) - 1}`).join(' ');
    return (
      <svg width={width} height={height} style={{ display: 'inline-block', verticalAlign: 'middle' }}>
        <polyline points={points} fill="none" stroke={color} strokeWidth="1.5" strokeLinejoin="round" />
      </svg>
    );
  };

  const renderOverview = () => (
    <div className="everflow-overview">
      {/* Today's Metrics */}
      <div className="section-header">
        <h3>Today's Performance</h3>
      </div>
      <div className="metric-grid">
        <MetricCard
          label="Today's Revenue"
          value={formatCurrency(todayMetrics.revenue)}
          status={todayMetrics.revenue > 0 ? 'healthy' : undefined}
        />
        <MetricCard
          label="Today's Conversions"
          value={formatNumber(todayMetrics.conversions)}
        />
        <MetricCard
          label="Today's Clicks"
          value={formatNumber(todayMetrics.clicks)}
        />
        <MetricCard
          label="Today's Payout"
          value={formatCurrency(todayMetrics.payout)}
        />
      </div>

      {/* Period Totals */}
      <div className="section-header">
        <h3>Period Totals ({currentRangeLabel} - {filteredDailyPerformance.length} days)</h3>
      </div>
      <div className="metric-grid">
        <MetricCard
          label="Total Revenue"
          value={formatCurrency(totalRevenue)}
          status="healthy"
        />
        <MetricCard
          label="Total Conversions"
          value={formatNumber(totalConversions)}
        />
        <MetricCard
          label="Total Clicks"
          value={formatNumber(totalClicks)}
        />
        <MetricCard
          label="Avg. EPC"
          value={formatCurrency(overallEPC)}
        />
      </div>

      {/* Statistical Intelligence Panel */}
      {trendStats && (
        <>
          <div className="section-header">
            <h3><FontAwesomeIcon icon={faChartLine} style={{ marginRight: '0.4rem' }} />Market Intelligence</h3>
            <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center' }}>
              <span className={`signal-badge signal-${trendStats.momentumSignal}`}>
                {trendStats.momentumSignal === 'bullish' ? '▲' : trendStats.momentumSignal === 'bearish' ? '▼' : '●'} {trendStats.momentumSignal.toUpperCase()}
              </span>
              <span className={`signal-badge signal-vol-${trendStats.volatility}`}>
                VOL: {trendStats.volatility.toUpperCase()}
              </span>
            </div>
          </div>

          <div className="stats-ticker-row">
            <div className="stats-ticker-item">
              <span className="ticker-label">AVG REV/DAY</span>
              <span className="ticker-value">{formatCurrency(trendStats.revenue.mean)}</span>
              <Sparkline data={filteredDailyPerformance.map(d => d.revenue)} color="#22c55e" />
            </div>
            <div className="stats-ticker-item">
              <span className="ticker-label">MEDIAN REV</span>
              <span className="ticker-value">{formatCurrency(trendStats.revenue.median)}</span>
            </div>
            <div className="stats-ticker-item">
              <span className="ticker-label">STD DEV</span>
              <span className="ticker-value">{formatCurrency(trendStats.revenue.stdDev)}</span>
            </div>
            <div className="stats-ticker-item">
              <span className="ticker-label">AVG EPC</span>
              <span className="ticker-value">{formatCurrency(trendStats.epc.mean)}</span>
              <Sparkline data={filteredDailyPerformance.map(d => d.epc)} color="#3b82f6" />
            </div>
            <div className="stats-ticker-item">
              <span className="ticker-label">AVG CVR</span>
              <span className="ticker-value">{(overallConversionRate * 100).toFixed(2)}%</span>
            </div>
            <div className="stats-ticker-item">
              <span className="ticker-label">MOMENTUM</span>
              <TrendArrow value={trendStats.momentum} />
            </div>
            <div className="stats-ticker-item">
              <span className="ticker-label">PERIOD TREND</span>
              <TrendArrow value={trendStats.periodTrend} />
            </div>
            <div className="stats-ticker-item">
              <span className="ticker-label">STREAK</span>
              <span className="ticker-value" style={{ color: trendStats.upStreak > 0 ? '#22c55e' : trendStats.downStreak > 0 ? '#ef4444' : '#6b7280' }}>
                {trendStats.upStreak > 0 ? `${trendStats.upStreak}d ▲` : trendStats.downStreak > 0 ? `${trendStats.downStreak}d ▼` : 'FLAT'}
              </span>
            </div>
          </div>

          <div className="stats-range-row">
            <div className="stats-range-item">
              <span className="range-label">Revenue Range</span>
              <div className="range-bar-track">
                <div className="range-bar-fill" style={{ 
                  left: `${trendStats.revenue.max > 0 ? (trendStats.revenue.min / trendStats.revenue.max) * 100 : 0}%`,
                  width: `${trendStats.revenue.max > 0 ? ((trendStats.revenue.max - trendStats.revenue.min) / trendStats.revenue.max) * 100 : 100}%`
                }} />
                <div className="range-bar-mean" style={{
                  left: `${trendStats.revenue.max > 0 ? (trendStats.revenue.mean / trendStats.revenue.max) * 100 : 50}%`
                }} />
              </div>
              <div className="range-labels">
                <span>{formatCurrency(trendStats.revenue.min)}</span>
                <span style={{ color: '#f59e0b' }}>μ {formatCurrency(trendStats.revenue.mean)}</span>
                <span>{formatCurrency(trendStats.revenue.max)}</span>
              </div>
            </div>
            <div className="stats-range-item">
              <span className="range-label">Best Day: {trendStats.bestDay ? new Date(trendStats.bestDay).toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric' }) : '–'}</span>
              <span className="range-label">Worst Day: {trendStats.worstDay ? new Date(trendStats.worstDay).toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric' }) : '–'}</span>
              <span className="range-label">CV: {trendStats.cv.toFixed(1)}%</span>
            </div>
          </div>
        </>
      )}

      {/* CPM vs Non-CPM Revenue Breakdown */}
      {revenueBreakdown && (
        <>
          <div className="section-header">
            <h3>Revenue Breakdown: CPM vs Non-CPM</h3>
          </div>
          <div className="revenue-breakdown-grid">
            <div className="breakdown-card cpm-card">
              <div className="breakdown-header">
                <span className="breakdown-label">CPM Revenue</span>
                <span className="breakdown-percentage">{revenueBreakdown.cpm.percentage.toFixed(1)}%</span>
              </div>
              <div className="breakdown-value">{formatCurrency(revenueBreakdown.cpm.revenue)}</div>
              <div className="breakdown-details">
                <span>{revenueBreakdown.cpm.offer_count} offers</span>
                <span>{formatNumber(revenueBreakdown.cpm.clicks)} clicks</span>
                <span>{revenueBreakdown.cpm.conversions} conv</span>
              </div>
            </div>
            <div className="breakdown-card non-cpm-card">
              <div className="breakdown-header">
                <span className="breakdown-label">Non-CPM Revenue</span>
                <span className="breakdown-percentage">{revenueBreakdown.non_cpm.percentage.toFixed(1)}%</span>
              </div>
              <div className="breakdown-value">{formatCurrency(revenueBreakdown.non_cpm.revenue)}</div>
              <div className="breakdown-details">
                <span>{revenueBreakdown.non_cpm.offer_count} offers</span>
                <span>{formatNumber(revenueBreakdown.non_cpm.clicks)} clicks</span>
                <span>{revenueBreakdown.non_cpm.conversions} conv</span>
              </div>
            </div>
          </div>
          <div className="breakdown-bar">
            <div 
              className="breakdown-bar-cpm" 
              style={{ width: `${revenueBreakdown.cpm.percentage}%` }}
              title={`CPM: ${formatCurrency(revenueBreakdown.cpm.revenue)} (${revenueBreakdown.cpm.percentage.toFixed(1)}%)`}
            />
            <div 
              className="breakdown-bar-non-cpm" 
              style={{ width: `${revenueBreakdown.non_cpm.percentage}%` }}
              title={`Non-CPM: ${formatCurrency(revenueBreakdown.non_cpm.revenue)} (${revenueBreakdown.non_cpm.percentage.toFixed(1)}%)`}
            />
          </div>
        </>
      )}

      {/* Revenue Chart */}
      <div className="section-header">
        <h3>Daily Revenue Trend ({currentRangeLabel})</h3>
      </div>
      <div className="chart-card card">
        <RevenueChart data={filteredDailyPerformance} height={250} />
      </div>

      {/* Top Performers */}
      <div className="top-performers-grid">
        <div className="card">
          <h4>Top Offers by Revenue</h4>
          <OfferPerformanceTable 
            data={offerPerformance.slice(0, 5)} 
            maxHeight="300px"
          />
        </div>
        <div className="card">
          <h4>Top Properties by Revenue</h4>
          <PropertyPerformanceTable 
            data={propertyPerformance.slice(0, 5)} 
            maxHeight="300px"
          />
        </div>
      </div>
    </div>
  );

  const renderOffers = () => (
    <div className="everflow-offers">
      <div className="section-header">
        <h3>Offer Performance ({offerPerformance.length} offers)</h3>
        <span className="total-badge">
          Total: {formatCurrency(offerPerformance.reduce((s, o) => s + o.revenue, 0))}
        </span>
      </div>
      <div className="card">
        <OfferPerformanceTable 
          data={offerPerformance} 
          maxHeight="calc(100vh - 300px)"
        />
      </div>
    </div>
  );

  const renderProperties = () => (
    <div className="everflow-properties">
      <div className="section-header">
        <h3>Property Performance ({propertyPerformance.length} properties)</h3>
        <span className="total-badge">
          Total: {formatCurrency(propertyPerformance.reduce((s, p) => s + p.revenue, 0))}
        </span>
      </div>
      <div className="card">
        <PropertyPerformanceTable 
          data={propertyPerformance} 
          maxHeight="calc(100vh - 300px)"
        />
      </div>
    </div>
  );

  const renderCampaigns = () => {
    const displayCampaigns = filteredCampaigns.length > 0 ? filteredCampaigns : campaignRevenue;
    
    return (
      <div className="everflow-campaigns">
        <div className="section-header">
          <h3>
            Campaign Revenue ({displayCampaigns.length} campaigns)
            {filteredCampaigns.length > 0 && (
              <span className="filter-badge">Filtered: {MIN_AUDIENCE_SIZE.toLocaleString()}+ audience</span>
            )}
          </h3>
          <span className="total-badge">
            Total: {formatCurrency(displayCampaigns.reduce((s, c) => s + c.revenue, 0))}
          </span>
        </div>
        <div className="card">
          {campaignsLoading ? (
            <div className="loading-state">Loading filtered campaigns...</div>
          ) : (
            <CampaignRevenueTable 
              data={displayCampaigns}
              onCampaignClick={(campaign) => setSelectedCampaignId(campaign.mailing_id)}
              maxHeight="calc(100vh - 300px)"
            />
          )}
        </div>
      </div>
    );
  };

  const renderESP = () => {
    const espTotal = espRevenue.reduce((sum, esp) => sum + esp.revenue, 0);
    
    return (
      <div className="everflow-esp">
        <div className="section-header">
          <h3>
            <FontAwesomeIcon icon={faServer} style={{ marginRight: '0.5rem', verticalAlign: 'middle' }} />
            Revenue by ESP ({espRevenue.length} providers)
          </h3>
          <span className="total-badge">
            Total: {formatCurrency(espTotal)}
          </span>
        </div>
        
        {espLoading ? (
          <div className="loading-state">Loading ESP revenue data...</div>
        ) : espRevenue.length === 0 ? (
          <div className="info-state card">
            <p>ESP revenue data is being collected.</p>
            <p className="text-muted">This requires campaign enrichment from Ongage to determine which ESP was used for each campaign.</p>
          </div>
        ) : (
          <>
            {/* ESP Summary Cards */}
            <div className="esp-summary-grid">
              {espRevenue.slice(0, 3).map((esp) => (
                <div key={esp.esp_name} className="esp-summary-card card">
                  <div className="esp-header">
                    <span className="esp-name">{esp.esp_name}</span>
                    <span className="esp-percentage">{esp.percentage.toFixed(1)}%</span>
                  </div>
                  <div className="esp-revenue">{formatCurrency(esp.revenue)}</div>
                  <div className="esp-stats">
                    <span>{esp.campaign_count} campaigns</span>
                    <span>{formatNumber(esp.total_delivered)} delivered</span>
                    <span>eCPM: {formatCurrency(esp.avg_ecpm)}</span>
                  </div>
                </div>
              ))}
            </div>
            
            {/* ESP Details Table */}
            <div className="card" style={{ marginTop: '1rem' }}>
              <ESPRevenueTable 
                data={espRevenue} 
                maxHeight="calc(100vh - 450px)"
              />
            </div>
          </>
        )}
        
        <style>{`
          .everflow-esp .esp-summary-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(250px, 1fr));
            gap: 1rem;
            margin-bottom: 1rem;
          }
          .everflow-esp .esp-summary-card {
            padding: 1rem;
          }
          .everflow-esp .esp-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 0.5rem;
          }
          .everflow-esp .esp-name {
            font-weight: 600;
            font-size: 1.1rem;
          }
          .everflow-esp .esp-percentage {
            font-size: 0.9rem;
            color: var(--accent-blue, #3b82f6);
            font-weight: 500;
          }
          .everflow-esp .esp-revenue {
            font-size: 1.5rem;
            font-weight: 700;
            color: var(--accent-green, #22c55e);
            margin-bottom: 0.5rem;
          }
          .everflow-esp .esp-stats {
            display: flex;
            gap: 1rem;
            font-size: 0.85rem;
            color: var(--text-muted, #666);
          }
          .everflow-esp .info-state {
            padding: 2rem;
            text-align: center;
          }
          .everflow-esp .text-muted {
            color: var(--text-muted, #666);
            font-size: 0.9rem;
            margin-top: 0.5rem;
          }
        `}</style>
      </div>
    );
  };

  const renderTrends = () => (
    <div className="everflow-trends">
      {/* Statistical Summary Bar */}
      {trendStats && (
        <div className="trends-stat-grid">
          <div className="trends-stat-card">
            <div className="tsc-header">Revenue</div>
            <div className="tsc-grid">
              <div><span className="tsc-label">Mean</span><span className="tsc-val">{formatCurrency(trendStats.revenue.mean)}</span></div>
              <div><span className="tsc-label">Median</span><span className="tsc-val">{formatCurrency(trendStats.revenue.median)}</span></div>
              <div><span className="tsc-label">Std Dev</span><span className="tsc-val">{formatCurrency(trendStats.revenue.stdDev)}</span></div>
              <div><span className="tsc-label">Min</span><span className="tsc-val">{formatCurrency(trendStats.revenue.min)}</span></div>
              <div><span className="tsc-label">Max</span><span className="tsc-val">{formatCurrency(trendStats.revenue.max)}</span></div>
              <div><span className="tsc-label">CV</span><span className="tsc-val">{trendStats.cv.toFixed(1)}%</span></div>
            </div>
          </div>
          <div className="trends-stat-card">
            <div className="tsc-header">Clicks</div>
            <div className="tsc-grid">
              <div><span className="tsc-label">Mean</span><span className="tsc-val">{formatNumber(trendStats.clicks.mean)}</span></div>
              <div><span className="tsc-label">Median</span><span className="tsc-val">{formatNumber(trendStats.clicks.median)}</span></div>
              <div><span className="tsc-label">Std Dev</span><span className="tsc-val">{formatNumber(trendStats.clicks.stdDev)}</span></div>
              <div><span className="tsc-label">Min</span><span className="tsc-val">{formatNumber(trendStats.clicks.min)}</span></div>
              <div><span className="tsc-label">Max</span><span className="tsc-val">{formatNumber(trendStats.clicks.max)}</span></div>
              <div><span className="tsc-label">Sparkline</span><Sparkline data={filteredDailyPerformance.map(d => d.clicks)} color="#3b82f6" /></div>
            </div>
          </div>
          <div className="trends-stat-card">
            <div className="tsc-header">Conversions</div>
            <div className="tsc-grid">
              <div><span className="tsc-label">Mean</span><span className="tsc-val">{trendStats.conversions.mean.toFixed(1)}</span></div>
              <div><span className="tsc-label">Median</span><span className="tsc-val">{trendStats.conversions.median.toFixed(0)}</span></div>
              <div><span className="tsc-label">Std Dev</span><span className="tsc-val">{trendStats.conversions.stdDev.toFixed(1)}</span></div>
              <div><span className="tsc-label">Min</span><span className="tsc-val">{trendStats.conversions.min}</span></div>
              <div><span className="tsc-label">Max</span><span className="tsc-val">{trendStats.conversions.max}</span></div>
              <div><span className="tsc-label">Sparkline</span><Sparkline data={filteredDailyPerformance.map(d => d.conversions)} color="#f59e0b" /></div>
            </div>
          </div>
          <div className="trends-stat-card">
            <div className="tsc-header">EPC</div>
            <div className="tsc-grid">
              <div><span className="tsc-label">Mean</span><span className="tsc-val">{formatCurrency(trendStats.epc.mean)}</span></div>
              <div><span className="tsc-label">Median</span><span className="tsc-val">{formatCurrency(trendStats.epc.median)}</span></div>
              <div><span className="tsc-label">Std Dev</span><span className="tsc-val">{formatCurrency(trendStats.epc.stdDev)}</span></div>
              <div><span className="tsc-label">Min</span><span className="tsc-val">{formatCurrency(trendStats.epc.min)}</span></div>
              <div><span className="tsc-label">Max</span><span className="tsc-val">{formatCurrency(trendStats.epc.max)}</span></div>
              <div><span className="tsc-label">Sparkline</span><Sparkline data={filteredDailyPerformance.map(d => d.epc)} color="#8b5cf6" /></div>
            </div>
          </div>
        </div>
      )}

      <div className="section-header">
        <h3>Daily Performance Ledger ({currentRangeLabel})</h3>
        <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center', fontSize: '0.75rem', color: 'var(--text-muted)' }}>
          <span style={{ display: 'inline-block', width: 12, height: 2, background: '#f59e0b' }} /> SMA-3
          <span style={{ display: 'inline-block', width: 12, height: 2, background: '#8b5cf6' }} /> SMA-7
        </div>
      </div>
      <div className="card">
        <div className="daily-table-container">
          <table className="table trends-ledger">
            <thead>
              <tr>
                <th>Date</th>
                <th style={{ textAlign: 'right' }}>Clicks</th>
                <th style={{ textAlign: 'right' }}>Conv.</th>
                <th style={{ textAlign: 'right' }}>Revenue</th>
                <th style={{ textAlign: 'right' }}>DoD Δ</th>
                <th style={{ textAlign: 'right' }}>SMA-3</th>
                <th style={{ textAlign: 'right' }}>SMA-7</th>
                <th style={{ textAlign: 'right' }}>EPC</th>
                <th style={{ textAlign: 'right' }}>CVR</th>
                <th style={{ textAlign: 'right' }}>Profit</th>
              </tr>
            </thead>
            <tbody>
              {filteredDailyPerformance.map((day, idx) => {
                const prevRev = idx > 0 ? filteredDailyPerformance[idx - 1].revenue : day.revenue;
                const dodDelta = prevRev > 0 ? ((day.revenue - prevRev) / prevRev) * 100 : 0;
                const sma3 = trendStats?.sma3Revenue[idx] ?? day.revenue;
                const sma7 = trendStats?.sma7Revenue[idx] ?? day.revenue;
                const profit = day.revenue - day.payout;
                const isAboveSma7 = day.revenue > sma7;

                return (
                  <tr key={day.date} className={isAboveSma7 ? 'above-sma' : 'below-sma'}>
                    <td>{new Date(day.date).toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric' })}</td>
                    <td style={{ textAlign: 'right', fontFamily: 'monospace' }}>{formatNumber(day.clicks)}</td>
                    <td style={{ textAlign: 'right', fontFamily: 'monospace' }}>{formatNumber(day.conversions)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600, color: 'var(--accent-green)', fontFamily: 'monospace' }}>
                      {formatCurrency(day.revenue)}
                    </td>
                    <td style={{ textAlign: 'right' }}>
                      {idx > 0 ? <TrendArrow value={dodDelta} /> : <span style={{ color: '#6b7280', fontSize: '0.8rem' }}>–</span>}
                    </td>
                    <td style={{ textAlign: 'right', fontFamily: 'monospace', fontSize: '0.8rem', color: '#f59e0b' }}>{formatCurrency(sma3)}</td>
                    <td style={{ textAlign: 'right', fontFamily: 'monospace', fontSize: '0.8rem', color: '#8b5cf6' }}>{formatCurrency(sma7)}</td>
                    <td style={{ textAlign: 'right', fontFamily: 'monospace', fontSize: '0.85rem' }}>{formatCurrency(day.epc)}</td>
                    <td style={{ textAlign: 'right', fontFamily: 'monospace', fontSize: '0.85rem' }}>{(day.conversion_rate * 100).toFixed(2)}%</td>
                    <td style={{ textAlign: 'right', fontFamily: 'monospace', fontWeight: 600, color: profit >= 0 ? '#22c55e' : '#ef4444' }}>{formatCurrency(profit)}</td>
                  </tr>
                );
              })}
            </tbody>
            <tfoot>
              <tr style={{ fontWeight: 600 }}>
                <td>TOTAL</td>
                <td style={{ textAlign: 'right', fontFamily: 'monospace' }}>{formatNumber(totalClicks)}</td>
                <td style={{ textAlign: 'right', fontFamily: 'monospace' }}>{formatNumber(totalConversions)}</td>
                <td style={{ textAlign: 'right', color: 'var(--accent-green)', fontFamily: 'monospace' }}>{formatCurrency(totalRevenue)}</td>
                <td style={{ textAlign: 'right' }}>–</td>
                <td style={{ textAlign: 'right' }}>–</td>
                <td style={{ textAlign: 'right' }}>–</td>
                <td style={{ textAlign: 'right', fontFamily: 'monospace' }}>{formatCurrency(overallEPC)}</td>
                <td style={{ textAlign: 'right', fontFamily: 'monospace' }}>{(overallConversionRate * 100).toFixed(2)}%</td>
                <td style={{ textAlign: 'right', fontFamily: 'monospace', color: (totalRevenue - filteredDailyPerformance.reduce((s, d) => s + d.payout, 0)) >= 0 ? '#22c55e' : '#ef4444' }}>
                  {formatCurrency(totalRevenue - filteredDailyPerformance.reduce((s, d) => s + d.payout, 0))}
                </td>
              </tr>
            </tfoot>
          </table>
        </div>
      </div>
    </div>
  );

  const renderContent = () => {
    switch (activeTab) {
      case 'overview':
        return renderOverview();
      case 'offers':
        return renderOffers();
      case 'properties':
        return renderProperties();
      case 'campaigns':
        return renderCampaigns();
      case 'esp':
        return renderESP();
      case 'trends':
        return renderTrends();
      default:
        return renderOverview();
    }
  };

  if (error && !dailyPerformance.length) {
    return (
      <div className="everflow-dashboard">
        <div className="dashboard-header">
          <div className="header-title">
            <FontAwesomeIcon icon={faDollarSign} style={{ fontSize: '24px' }} />
            <h2>Everflow Revenue</h2>
          </div>
        </div>
        <div className="error-state card">
          <p>Error loading Everflow data: {error}</p>
          <button onClick={fetchDashboard} className="btn btn-primary">
            <FontAwesomeIcon icon={faSync} /> Retry
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="everflow-dashboard">
      <div className="dashboard-header">
        <div className="header-title">
          <FontAwesomeIcon icon={faDollarSign} style={{ fontSize: '24px' }} />
          <h2>Everflow Revenue</h2>
          <span className="date-range-badge">{currentRangeLabel}</span>
        </div>
        <div className="header-actions">
          {lastFetch && (
            <span className="last-updated">
              Updated: {lastFetch.toLocaleTimeString()}
            </span>
          )}
          <button 
            onClick={fetchDashboard} 
            disabled={loading}
            className="btn btn-icon"
            title="Refresh data"
          >
            <FontAwesomeIcon icon={faSync} spin={loading} />
          </button>
        </div>
      </div>

      <div className="tab-bar">
        {TABS.map((tab) => (
          <button
            key={tab.id}
            className={`tab ${activeTab === tab.id ? 'active' : ''}`}
            onClick={() => setActiveTab(tab.id)}
          >
            {tab.id === 'overview' && <FontAwesomeIcon icon={faArrowUp} />}
            {tab.id === 'offers' && <FontAwesomeIcon icon={faBullseye} />}
            {tab.id === 'properties' && <FontAwesomeIcon icon={faDollarSign} />}
            {tab.id === 'campaigns' && <FontAwesomeIcon icon={faMousePointer} />}
            {tab.id === 'esp' && <FontAwesomeIcon icon={faServer} />}
            {tab.id === 'trends' && <FontAwesomeIcon icon={faArrowUp} />}
            {tab.label}
          </button>
        ))}
      </div>

      <div className="dashboard-content">
        {loading && !dailyPerformance.length ? (
          <div className="loading-state">
            <FontAwesomeIcon icon={faSync} spin style={{ fontSize: '32px' }} />
            <p>Loading Everflow data...</p>
          </div>
        ) : !lastFetch && !dailyPerformance.length ? (
          <div className="loading-state">
            <FontAwesomeIcon icon={faSync} spin style={{ fontSize: '32px' }} />
            <p>Everflow data is being collected... This may take a few minutes on first load.</p>
          </div>
        ) : (
          renderContent()
        )}
      </div>

      <style>{`
        .everflow-dashboard {
          padding: 1rem;
        }
        .dashboard-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 1rem;
        }
        .header-title {
          display: flex;
          align-items: center;
          gap: 0.5rem;
        }
        .header-title h2 {
          margin: 0;
          font-size: 1.5rem;
        }
        .header-actions {
          display: flex;
          align-items: center;
          gap: 1rem;
          flex-wrap: wrap;
        }
        .date-range-selector {
          display: flex;
          align-items: center;
          gap: 4px;
          padding: 4px;
          background: var(--bg-secondary, #252538);
          border-radius: 8px;
        }
        .date-range-selector svg {
          margin-left: 4px;
          margin-right: 4px;
          color: var(--text-muted, #666);
        }
        .date-range-btn {
          padding: 6px 10px;
          font-size: 0.75rem;
          border: none;
          border-radius: 6px;
          cursor: pointer;
          background: transparent;
          color: var(--text-muted, #666);
          transition: all 0.2s ease;
          white-space: nowrap;
        }
        .date-range-btn:hover {
          background: var(--bg-tertiary, #333);
          color: var(--text-primary, #fff);
        }
        .date-range-btn.active {
          background: var(--accent-green, #22c55e);
          color: white;
          font-weight: 500;
        }
        .last-updated {
          font-size: 0.85rem;
          color: var(--text-muted, #666);
        }
        .btn-icon {
          padding: 0.5rem;
          background: transparent;
          border: 1px solid var(--border-color, #e5e7eb);
          border-radius: 6px;
          cursor: pointer;
          display: flex;
          align-items: center;
          justify-content: center;
        }
        .btn-icon:hover {
          background: var(--hover-bg, #f5f5f5);
        }
        .spinning {
          animation: spin 1s linear infinite;
        }
        @keyframes spin {
          from { transform: rotate(0deg); }
          to { transform: rotate(360deg); }
        }
        .tab-bar {
          display: flex;
          gap: 0.25rem;
          border-bottom: 1px solid var(--border-color, #e5e7eb);
          margin-bottom: 1rem;
          overflow-x: auto;
        }
        .tab {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          padding: 0.75rem 1rem;
          border: none;
          background: transparent;
          cursor: pointer;
          font-size: 0.9rem;
          color: var(--text-muted, #666);
          border-bottom: 2px solid transparent;
          transition: all 0.2s ease;
          white-space: nowrap;
        }
        .tab:hover {
          color: var(--text-primary, #fff);
        }
        .tab.active {
          color: var(--accent-green, #22c55e);
          border-bottom-color: var(--accent-green, #22c55e);
        }
        .metric-grid {
          display: grid;
          grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
          gap: 1rem;
          margin-bottom: 1.5rem;
        }
        .section-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 0.75rem;
        }
        .section-header h3 {
          margin: 0;
          font-size: 1.1rem;
        }
        .total-badge {
          background: var(--accent-green, #22c55e);
          color: white;
          padding: 0.25rem 0.75rem;
          border-radius: 12px;
          font-size: 0.85rem;
          font-weight: 600;
        }
        .chart-card {
          padding: 1rem;
          margin-bottom: 1.5rem;
        }
        .top-performers-grid {
          display: grid;
          grid-template-columns: repeat(auto-fit, minmax(400px, 1fr));
          gap: 1rem;
        }
        .top-performers-grid .card {
          padding: 1rem;
        }
        .top-performers-grid h4 {
          margin: 0 0 1rem 0;
          font-size: 1rem;
        }
        .loading-state {
          display: flex;
          flex-direction: column;
          align-items: center;
          justify-content: center;
          padding: 4rem;
          color: var(--text-muted, #666);
        }
        .loading-state p {
          margin-top: 1rem;
        }
        .error-state {
          padding: 2rem;
          text-align: center;
        }
        .error-state p {
          margin-bottom: 1rem;
          color: var(--accent-red, #ef4444);
        }
        .daily-table-container {
          max-height: calc(100vh - 350px);
          overflow-y: auto;
        }
        .daily-table-container table {
          width: 100%;
        }
        .daily-table-container tfoot {
          position: sticky;
          bottom: 0;
          background: var(--card-bg, #1a1a2e);
        }

        /* ── Statistical Intelligence Panel ── */
        .stats-ticker-row {
          display: flex;
          gap: 0;
          overflow-x: auto;
          margin-bottom: 1.25rem;
          border: 1px solid var(--border-color, #333);
          border-radius: 8px;
          background: var(--bg-secondary, #1e1e30);
        }
        .stats-ticker-item {
          flex: 1;
          min-width: 100px;
          padding: 0.6rem 0.75rem;
          border-right: 1px solid var(--border-color, #333);
          display: flex;
          flex-direction: column;
          align-items: center;
          gap: 2px;
        }
        .stats-ticker-item:last-child { border-right: none; }
        .ticker-label {
          font-size: 0.6rem;
          letter-spacing: 0.05em;
          color: var(--text-muted, #888);
          text-transform: uppercase;
          font-weight: 600;
        }
        .ticker-value {
          font-family: 'SF Mono', 'Fira Code', monospace;
          font-size: 0.85rem;
          font-weight: 700;
          color: var(--text-primary, #fff);
        }
        .signal-badge {
          padding: 0.15rem 0.5rem;
          border-radius: 4px;
          font-size: 0.7rem;
          font-weight: 700;
          letter-spacing: 0.05em;
          font-family: monospace;
        }
        .signal-bullish { background: rgba(34, 197, 94, 0.15); color: #22c55e; }
        .signal-bearish { background: rgba(239, 68, 68, 0.15); color: #ef4444; }
        .signal-neutral { background: rgba(107, 114, 128, 0.15); color: #9ca3af; }
        .signal-vol-low { background: rgba(34, 197, 94, 0.12); color: #22c55e; }
        .signal-vol-moderate { background: rgba(245, 158, 11, 0.12); color: #f59e0b; }
        .signal-vol-high { background: rgba(239, 68, 68, 0.12); color: #ef4444; }

        .stats-range-row {
          display: grid;
          grid-template-columns: 2fr 1fr;
          gap: 1rem;
          margin-bottom: 1.5rem;
          padding: 0.75rem 1rem;
          background: var(--bg-secondary, #1e1e30);
          border-radius: 8px;
          border: 1px solid var(--border-color, #333);
        }
        .stats-range-item { display: flex; flex-direction: column; gap: 4px; }
        .range-label { font-size: 0.75rem; color: var(--text-muted, #888); }
        .range-bar-track {
          position: relative;
          height: 6px;
          background: var(--bg-tertiary, #333);
          border-radius: 3px;
          margin: 4px 0;
        }
        .range-bar-fill {
          position: absolute;
          height: 100%;
          background: linear-gradient(90deg, #22c55e, #3b82f6);
          border-radius: 3px;
          opacity: 0.5;
        }
        .range-bar-mean {
          position: absolute;
          top: -3px;
          width: 2px;
          height: 12px;
          background: #f59e0b;
          border-radius: 1px;
        }
        .range-labels {
          display: flex;
          justify-content: space-between;
          font-size: 0.7rem;
          font-family: monospace;
          color: var(--text-muted, #888);
        }

        /* ── Trends Stat Cards ── */
        .trends-stat-grid {
          display: grid;
          grid-template-columns: repeat(4, 1fr);
          gap: 0.75rem;
          margin-bottom: 1.25rem;
        }
        .trends-stat-card {
          background: var(--bg-secondary, #1e1e30);
          border: 1px solid var(--border-color, #333);
          border-radius: 8px;
          padding: 0.75rem;
        }
        .tsc-header {
          font-size: 0.75rem;
          font-weight: 700;
          text-transform: uppercase;
          letter-spacing: 0.05em;
          color: var(--text-muted, #aaa);
          margin-bottom: 0.5rem;
          padding-bottom: 0.3rem;
          border-bottom: 1px solid var(--border-color, #333);
        }
        .tsc-grid {
          display: grid;
          grid-template-columns: 1fr 1fr;
          gap: 4px 8px;
        }
        .tsc-grid > div {
          display: flex;
          justify-content: space-between;
          align-items: center;
          padding: 2px 0;
        }
        .tsc-label {
          font-size: 0.65rem;
          color: var(--text-muted, #888);
          text-transform: uppercase;
          letter-spacing: 0.03em;
        }
        .tsc-val {
          font-family: 'SF Mono', 'Fira Code', monospace;
          font-size: 0.75rem;
          font-weight: 600;
          color: var(--text-primary, #fff);
        }

        /* ── Trends Ledger Styling ── */
        .trends-ledger th {
          font-size: 0.7rem;
          letter-spacing: 0.04em;
          text-transform: uppercase;
          color: var(--text-muted, #888);
          padding: 0.5rem 0.6rem;
          border-bottom: 2px solid var(--border-color, #444);
        }
        .trends-ledger td {
          padding: 0.4rem 0.6rem;
          font-size: 0.82rem;
          border-bottom: 1px solid var(--border-color, rgba(255,255,255,0.05));
        }
        .trends-ledger tr.above-sma {
          border-left: 3px solid rgba(34, 197, 94, 0.4);
        }
        .trends-ledger tr.below-sma {
          border-left: 3px solid rgba(239, 68, 68, 0.2);
        }
        .trends-ledger tfoot td {
          font-size: 0.82rem;
          border-top: 2px solid var(--border-color, #444);
          padding-top: 0.6rem;
        }

        /* ── Date Range Badge ── */
        .date-range-badge {
          padding: 0.2rem 0.6rem;
          background: var(--primary-color, #6366f1);
          color: white;
          border-radius: 1rem;
          font-size: 0.7rem;
          font-weight: 500;
        }
        .filter-badge {
          font-size: 0.75rem;
          margin-left: 0.5rem;
          padding: 0.15rem 0.5rem;
          background: rgba(59, 130, 246, 0.15);
          color: var(--accent-blue, #3b82f6);
          border-radius: 4px;
        }
      `}</style>

      {/* Campaign Details Modal */}
      {selectedCampaignId && (
        <CampaignDetailsModal
          mailingId={selectedCampaignId}
          onClose={() => setSelectedCampaignId(null)}
        />
      )}
    </div>
  );
};

export default EverflowDashboard;
