import React, { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faChartLine,
  faArrowUp,
  faArrowDown,
  faMinus,
  faSyncAlt,
  faExclamationTriangle,
  faChartArea,
  faCrown,
  faSkullCrossbones,
  faBolt,
  faDollarSign,
  faSort,
  faSortUp,
  faSortDown,
  faCalendarWeek,
  faGlobe,
  faSignal,
} from '@fortawesome/free-solid-svg-icons';
import {
  ComposedChart,
  Area,
  Bar,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  Legend,
  ResponsiveContainer,
  Brush,
  ReferenceLine,
  LineChart,
  AreaChart,
} from 'recharts';
import { useDateFilter } from '../../context/DateFilterContext';
import './InjectionAnalytics.css';

/* ============================================================================
   Types
   ============================================================================ */

interface InjectionAnalyticsData {
  period: {
    start: string;
    end: string;
    days: number;
    label: string;
  };
  summary: {
    total_injected: number;
    total_delivered: number;
    total_bounced: number;
    total_opened: number;
    total_clicked: number;
    total_complained: number;
    delivery_rate: number;
    open_rate: number;
    click_rate: number;
    bounce_rate: number;
    complaint_rate: number;
    avg_daily_volume: number;
    peak_daily_volume: number;
    peak_date: string;
    low_daily_volume: number;
    low_date: string;
    active_campaigns: number;
    total_revenue: number;
  };
  comparison: {
    prev_total_injected: number;
    injected_change_pct: number;
    prev_delivery_rate: number;
    delivery_rate_change: number;
    prev_open_rate: number;
    open_rate_change: number;
    prev_click_rate: number;
    click_rate_change: number;
    prev_bounce_rate: number;
    bounce_rate_change: number;
    prev_complaint_rate: number;
    complaint_rate_change: number;
    prev_avg_daily_volume: number;
    avg_daily_volume_change_pct: number;
    trend: string;
  };
  daily_series: Array<{
    date: string;
    injected: number;
    delivered: number;
    bounced: number;
    opened: number;
    clicked: number;
    complained: number;
    delivery_rate: number;
    open_rate: number;
    click_rate: number;
    bounce_rate: number;
    complaint_rate: number;
    cumulative_injected: number;
    revenue: number;
    ma7_injected: number | null;
    ma14_injected: number | null;
    campaigns_active: number;
  }>;
  isp_breakdown: Array<{
    isp: string;
    total_sent: number;
    delivered: number;
    delivery_rate: number;
    open_rate: number;
    click_rate: number;
    bounce_rate: number;
    complaint_rate: number;
    volume_share_pct: number;
    trend: string;
    trend_pct: number;
  }>;
  moving_averages: {
    ma7: Array<{ date: string; value: number }>;
    ma14: Array<{ date: string; value: number }>;
    ma30: Array<{ date: string; value: number }>;
  };
  weekly_aggregates: Array<{
    week_start: string;
    week_end: string;
    week_label: string;
    total_injected: number;
    avg_daily: number;
    delivery_rate: number;
    open_rate: number;
    change_from_prev_week_pct: number | null;
  }>;
  volatility: {
    daily_std_dev: number;
    coefficient_of_variation: number;
    max_daily_swing: number;
    max_swing_date: string;
    stability_score: string;
  };
}

/* ============================================================================
   Formatting helpers
   ============================================================================ */

const fmtNum = (n: number): string => {
  if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(1) + 'B';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
};

const fmtPct = (n: number): string => n.toFixed(2) + '%';

const fmtCurrency = (n: number): string =>
  '$' +
  n.toLocaleString(undefined, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });

const fmtDate = (d: string): string =>
  new Date(d + 'T00:00:00').toLocaleDateString('en-US', {
    month: 'short',
    day: 'numeric',
  });

const fmtFullDate = (d: string): string =>
  new Date(d + 'T00:00:00').toLocaleDateString('en-US', {
    month: 'short',
    day: 'numeric',
    year: 'numeric',
  });

/* ============================================================================
   Range options
   ============================================================================ */

const RANGES = [
  { value: '7d', label: '7D' },
  { value: '14d', label: '14D' },
  { value: '30d', label: '30D' },
  { value: '60d', label: '60D' },
  { value: '90d', label: '90D' },
  { value: '180d', label: '180D' },
  { value: '1y', label: '1Y' },
];

/* ============================================================================
   Color helpers for rates
   ============================================================================ */

const rateClass = (
  value: number,
  thresholds: { good: number; warn: number },
  invert = false,
): string => {
  if (invert) {
    if (value <= thresholds.good) return 'good';
    if (value <= thresholds.warn) return 'warning';
    return 'bad';
  }
  if (value >= thresholds.good) return 'good';
  if (value >= thresholds.warn) return 'warning';
  return 'bad';
};

/* ============================================================================
   Custom Tooltip for the Big Board
   ============================================================================ */

interface BigBoardTooltipProps {
  active?: boolean;
  payload?: Array<{ name: string; value: number; color: string; dataKey: string; payload?: Record<string, unknown> }>;
  label?: string;
}

const BigBoardTooltip: React.FC<BigBoardTooltipProps> = ({ active, payload, label }) => {
  if (!active || !payload || !payload.length) return null;

  const dataPoint = payload[0]?.payload as Record<string, unknown> | undefined;
  if (!dataPoint) return null;

  return (
    <div className="ia-tooltip">
      <div className="ia-tooltip-date">{label ? fmtDate(String(label)) : ''}</div>
      <div className="ia-tooltip-row">
        <span className="ia-tooltip-label">
          <span className="ia-tooltip-dot" style={{ background: '#3b82f6' }} /> Injected
        </span>
        <span className="ia-tooltip-value">{fmtNum(Number(dataPoint.injected ?? 0))}</span>
      </div>
      <div className="ia-tooltip-row">
        <span className="ia-tooltip-label">
          <span className="ia-tooltip-dot" style={{ background: '#10b981' }} /> Delivered
        </span>
        <span className="ia-tooltip-value">{fmtNum(Number(dataPoint.delivered ?? 0))}</span>
      </div>
      <div className="ia-tooltip-row">
        <span className="ia-tooltip-label">
          <span className="ia-tooltip-dot" style={{ background: '#ef4444' }} /> Bounced
        </span>
        <span className="ia-tooltip-value">{fmtNum(Number(dataPoint.bounced ?? 0))}</span>
      </div>
      <div className="ia-tooltip-row">
        <span className="ia-tooltip-label">
          <span className="ia-tooltip-dot" style={{ background: '#8b5cf6' }} /> Opened
        </span>
        <span className="ia-tooltip-value">{fmtNum(Number(dataPoint.opened ?? 0))}</span>
      </div>
      <div className="ia-tooltip-row">
        <span className="ia-tooltip-label">
          <span className="ia-tooltip-dot" style={{ background: '#f59e0b' }} /> Clicked
        </span>
        <span className="ia-tooltip-value">{fmtNum(Number(dataPoint.clicked ?? 0))}</span>
      </div>
      <div className="ia-tooltip-row">
        <span className="ia-tooltip-label">Delivery Rate</span>
        <span className="ia-tooltip-value">{fmtPct(Number(dataPoint.delivery_rate ?? 0))}</span>
      </div>
      <div className="ia-tooltip-row">
        <span className="ia-tooltip-label">Open Rate</span>
        <span className="ia-tooltip-value">{fmtPct(Number(dataPoint.open_rate ?? 0))}</span>
      </div>
      <div className="ia-tooltip-row">
        <span className="ia-tooltip-label">Revenue</span>
        <span className="ia-tooltip-value">{fmtCurrency(Number(dataPoint.revenue ?? 0))}</span>
      </div>
    </div>
  );
};

/* ============================================================================
   Custom Tooltip for the secondary rate chart
   ============================================================================ */

interface RateTooltipProps {
  active?: boolean;
  payload?: Array<{ name: string; value: number; color: string }>;
  label?: string;
}

const RateTooltip: React.FC<RateTooltipProps> = ({ active, payload, label }) => {
  if (!active || !payload || !payload.length) return null;
  return (
    <div className="ia-tooltip">
      <div className="ia-tooltip-date">{label ? fmtDate(String(label)) : ''}</div>
      {payload.map((entry, i) => (
        <div className="ia-tooltip-row" key={i}>
          <span className="ia-tooltip-label">
            <span className="ia-tooltip-dot" style={{ background: entry.color }} />
            {entry.name}
          </span>
          <span className="ia-tooltip-value">{fmtPct(entry.value)}</span>
        </div>
      ))}
    </div>
  );
};

/* ============================================================================
   Custom Tooltip for cumulative chart
   ============================================================================ */

interface CumulativeTooltipProps {
  active?: boolean;
  payload?: Array<{ value: number }>;
  label?: string;
}

const CumulativeTooltip: React.FC<CumulativeTooltipProps> = ({ active, payload, label }) => {
  if (!active || !payload || !payload.length) return null;
  return (
    <div className="ia-tooltip">
      <div className="ia-tooltip-date">{label ? fmtDate(String(label)) : ''}</div>
      <div className="ia-tooltip-row">
        <span className="ia-tooltip-label">
          <span className="ia-tooltip-dot" style={{ background: '#8b5cf6' }} />
          Cumulative Volume
        </span>
        <span className="ia-tooltip-value">{fmtNum(payload[0].value)}</span>
      </div>
    </div>
  );
};

/* ============================================================================
   Skeleton Loader
   ============================================================================ */

const SkeletonLoader: React.FC = () => (
  <div className="ia-container">
    <div className="ia-skeleton ia-skeleton-header" />
    <div className="ia-skeleton ia-skeleton-ticker" />
    <div className="ia-skeleton ia-skeleton-chart" />
    <div className="ia-skeleton-row">
      <div className="ia-skeleton ia-skeleton-card" />
      <div className="ia-skeleton ia-skeleton-card" />
    </div>
    <div className="ia-skeleton-stats">
      <div className="ia-skeleton ia-skeleton-stat" />
      <div className="ia-skeleton ia-skeleton-stat" />
      <div className="ia-skeleton ia-skeleton-stat" />
      <div className="ia-skeleton ia-skeleton-stat" />
    </div>
    <div className="ia-skeleton ia-skeleton-table" />
    <div className="ia-skeleton ia-skeleton-table" />
  </div>
);

/* ============================================================================
   Main Component
   ============================================================================ */

export const InjectionAnalytics: React.FC = () => {
  /* ── Global Date Filter Integration ── */
  const { dateRange } = useDateFilter();

  // Map global date filter type to closest local range option
  const globalRangeHint = useMemo(() => {
    const map: Record<string, string> = {
      last7: '7d', last14: '14d', mtd: '30d', last30: '30d',
      last60: '60d', last90: '90d', lastMonth: '30d', ytd: '1y', custom: '30d',
    };
    return map[dateRange.type] || '30d';
  }, [dateRange.type]);

  /* ── State ── */
  const [data, setData] = useState<InjectionAnalyticsData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [range, setRange] = useState<string>(globalRangeHint);
  const [autoRefresh, setAutoRefresh] = useState(false);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const [sortField, setSortField] = useState<string>('total_sent');
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc');
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  /* ── Data Fetching (includes global date params for API alignment) ── */
  const fetchData = useCallback(async () => {
    try {
      const res = await fetch(
        `/api/mailing/injection-analytics?range=${range}&compare=true&start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`,
        { credentials: 'include' },
      );
      if (!res.ok) throw new Error('Failed to fetch');
      const json: InjectionAnalyticsData = await res.json();
      setData(json);
      setLastUpdated(new Date());
      setError(null);
    } catch {
      setError('Failed to load injection analytics');
    } finally {
      setLoading(false);
    }
  }, [range, dateRange.startDate, dateRange.endDate, dateRange.type]);

  /* Fetch on mount + range change */
  useEffect(() => {
    setLoading(true);
    fetchData();
  }, [fetchData]);

  /* Auto-refresh interval */
  useEffect(() => {
    if (autoRefresh) {
      intervalRef.current = setInterval(fetchData, 60_000);
    }
    return () => {
      if (intervalRef.current) clearInterval(intervalRef.current);
    };
  }, [autoRefresh, fetchData]);

  /* ── Sorted ISP Data ── */
  const sortedISPs = useMemo(() => {
    if (!data?.isp_breakdown) return [];
    const sorted = [...data.isp_breakdown];
    sorted.sort((a, b) => {
      const aVal = (a as unknown as Record<string, number>)[sortField] ?? 0;
      const bVal = (b as unknown as Record<string, number>)[sortField] ?? 0;
      return sortDir === 'desc' ? bVal - aVal : aVal - bVal;
    });
    return sorted;
  }, [data?.isp_breakdown, sortField, sortDir]);

  /* ── Sort handler ── */
  const handleSort = (field: string) => {
    if (sortField === field) {
      setSortDir((d) => (d === 'desc' ? 'asc' : 'desc'));
    } else {
      setSortField(field);
      setSortDir('desc');
    }
  };

  const sortIcon = (field: string) => {
    if (sortField !== field) return faSort;
    return sortDir === 'desc' ? faSortDown : faSortUp;
  };

  /* ── Max ISP volume (for proportional bars) ── */
  const maxISPVolume = useMemo(() => {
    if (!data?.isp_breakdown?.length) return 1;
    return Math.max(...data.isp_breakdown.map((i) => i.total_sent));
  }, [data?.isp_breakdown]);

  /* ── Loading state ── */
  if (loading && !data) {
    return <SkeletonLoader />;
  }

  /* ── Error state ── */
  if (error && !data) {
    return (
      <div className="ia-container">
        <div className="ia-error">
          <FontAwesomeIcon icon={faExclamationTriangle} className="ia-error-icon" />
          <div className="ia-error-message">{error}</div>
          <button className="ia-error-retry" onClick={fetchData}>
            <FontAwesomeIcon icon={faSyncAlt} style={{ marginRight: '0.4rem' }} />
            Retry
          </button>
        </div>
      </div>
    );
  }

  /* ── Empty state ── */
  if (!data) {
    return (
      <div className="ia-container">
        <div className="ia-empty">
          <FontAwesomeIcon icon={faChartArea} className="ia-empty-icon" />
          <div>No injection data available for this period.</div>
        </div>
      </div>
    );
  }

  const { summary, comparison, daily_series, volatility, weekly_aggregates } = data;

  /* ── Ticker items ── */
  const tickerItems = [
    {
      label: 'Total Injected',
      value: fmtNum(summary.total_injected),
      change: comparison.injected_change_pct,
      invert: false,
    },
    {
      label: 'Delivery Rate',
      value: fmtPct(summary.delivery_rate),
      change: comparison.delivery_rate_change,
      invert: false,
    },
    {
      label: 'Open Rate',
      value: fmtPct(summary.open_rate),
      change: comparison.open_rate_change,
      invert: false,
    },
    {
      label: 'Click Rate',
      value: fmtPct(summary.click_rate),
      change: comparison.click_rate_change,
      invert: false,
    },
    {
      label: 'Bounce Rate',
      value: fmtPct(summary.bounce_rate),
      change: comparison.bounce_rate_change,
      invert: true,
    },
    {
      label: 'Complaint Rate',
      value: fmtPct(summary.complaint_rate),
      change: comparison.complaint_rate_change,
      invert: true,
    },
  ];

  /* Determine direction class for a change value, accounting for inverted metrics */
  const changeClass = (change: number, invert: boolean): string => {
    if (change === 0) return 'neutral';
    const isPositive = change > 0;
    if (invert) return isPositive ? 'negative' : 'positive';
    return isPositive ? 'positive' : 'negative';
  };

  const changeArrow = (change: number, invert: boolean) => {
    if (change === 0) return faMinus;
    const isPositive = change > 0;
    if (invert) return isPositive ? faArrowUp : faArrowDown;
    return isPositive ? faArrowUp : faArrowDown;
  };

  /* ── Stability badge class ── */
  const stabilityClass = (score: string): string => {
    const s = score.toLowerCase().replace(/[\s-]/g, '-');
    if (s.includes('stable') || s.includes('low')) return 'stable';
    if (s.includes('moderate')) return 'moderate';
    if (s.includes('highly') || s.includes('extreme')) return 'highly-volatile';
    if (s.includes('volatile') || s.includes('high')) return 'volatile';
    return 'moderate';
  };

  /* ── Y-axis tick formatter ── */
  const yAxisTickFormatter = (value: number) => fmtNum(value);
  const pctAxisTickFormatter = (value: number) => value.toFixed(0) + '%';

  return (
    <div className="ia-container">
      {/* ================================================================
          1. Header Bar
          ================================================================ */}
      <div className="ia-header">
        <div className="ia-header-left">
          <h1 className="ia-header-title">
            <FontAwesomeIcon icon={faChartLine} />
            Injection Analytics
          </h1>
          <p className="ia-header-subtitle">
            Email injection pipeline performance &mdash; stock-market style trend analysis
            {data.period.label && (
              <span style={{ marginLeft: '0.75rem', color: '#3b82f6' }}>
                [{data.period.label}]
              </span>
            )}
          </p>
        </div>

        <div className="ia-header-right">
          {/* Range pills */}
          <div className="ia-range-selector">
            {RANGES.map((r) => (
              <button
                key={r.value}
                className={`ia-range-pill ${range === r.value ? 'active' : ''}`}
                onClick={() => setRange(r.value)}
              >
                {r.label}
              </button>
            ))}
          </div>

          {/* Auto-refresh toggle */}
          <label className="ia-auto-refresh">
            <div
              className={`ia-toggle ${autoRefresh ? 'on' : ''}`}
              onClick={() => setAutoRefresh((v) => !v)}
            >
              <div className="ia-toggle-knob" />
            </div>
            <span>Auto</span>
          </label>

          {/* Last updated */}
          {lastUpdated && (
            <span className="ia-last-updated">
              {lastUpdated.toLocaleTimeString('en-US', {
                hour: '2-digit',
                minute: '2-digit',
                second: '2-digit',
              })}
            </span>
          )}
        </div>
      </div>

      {/* ================================================================
          2. Market Ticker Strip
          ================================================================ */}
      <div className="ia-ticker-strip">
        {tickerItems.map((item) => {
          const cls = changeClass(item.change, item.invert);
          return (
            <div className="ia-ticker-item" key={item.label}>
              <span className="ia-ticker-label">{item.label}</span>
              <span className="ia-ticker-value">{item.value}</span>
              <span className={`ia-ticker-change ${cls}`}>
                <FontAwesomeIcon icon={changeArrow(item.change, item.invert)} />
                {Math.abs(item.change).toFixed(2)}%
              </span>
            </div>
          );
        })}
      </div>

      {/* ================================================================
          3. Main Chart — "The Big Board"
          ================================================================ */}
      <div className="ia-chart-section">
        <div className="ia-big-board">
          <div className="ia-big-board-header">
            <div className="ia-big-board-title">
              <FontAwesomeIcon icon={faChartArea} />
              Daily Injection Volume &amp; Trends
            </div>
            <div className="ia-chart-legend-custom">
              <div className="ia-chart-legend-item">
                <span className="ia-chart-legend-dot" style={{ background: '#3b82f6' }} />
                Volume
              </div>
              <div className="ia-chart-legend-item">
                <span
                  className="ia-chart-legend-line dashed"
                  style={{ color: '#f59e0b' }}
                />
                MA7
              </div>
              <div className="ia-chart-legend-item">
                <span
                  className="ia-chart-legend-line dashed"
                  style={{ color: '#8b5cf6' }}
                />
                MA14
              </div>
              <div className="ia-chart-legend-item">
                <span className="ia-chart-legend-dot" style={{ background: '#475569' }} />
                Campaigns
              </div>
            </div>
          </div>

          <ResponsiveContainer width="100%" height={420}>
            <ComposedChart
              data={daily_series}
              margin={{ top: 10, right: 10, left: 0, bottom: 0 }}
            >
              <defs>
                <linearGradient id="iaVolumeGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor="#3b82f6" stopOpacity={0.3} />
                  <stop offset="95%" stopColor="#3b82f6" stopOpacity={0.02} />
                </linearGradient>
              </defs>
              <CartesianGrid
                strokeDasharray="3 3"
                stroke="#1e293b"
                vertical={false}
              />
              <XAxis
                dataKey="date"
                tickFormatter={fmtDate}
                stroke="#334155"
                tick={{ fill: '#64748b', fontSize: 11 }}
                tickLine={false}
                axisLine={{ stroke: '#1e293b' }}
                minTickGap={30}
              />
              <YAxis
                yAxisId="volume"
                tickFormatter={yAxisTickFormatter}
                stroke="#334155"
                tick={{ fill: '#64748b', fontSize: 11 }}
                tickLine={false}
                axisLine={false}
                width={55}
              />
              <YAxis
                yAxisId="campaigns"
                orientation="right"
                tickFormatter={(v: number) => String(v)}
                stroke="#334155"
                tick={{ fill: '#475569', fontSize: 10 }}
                tickLine={false}
                axisLine={false}
                width={30}
                hide
              />
              <Tooltip content={<BigBoardTooltip />} />
              <Legend
                verticalAlign="top"
                height={0}
                wrapperStyle={{ display: 'none' }}
              />
              <ReferenceLine
                yAxisId="volume"
                y={summary.avg_daily_volume}
                stroke="#475569"
                strokeDasharray="6 4"
                strokeWidth={1}
                label={{
                  value: `Avg: ${fmtNum(summary.avg_daily_volume)}`,
                  position: 'insideTopRight',
                  fill: '#475569',
                  fontSize: 10,
                  fontFamily: "'SF Mono', 'Fira Code', monospace",
                }}
              />
              <Area
                yAxisId="volume"
                type="monotone"
                dataKey="injected"
                stroke="#3b82f6"
                strokeWidth={2}
                fill="url(#iaVolumeGrad)"
                name="Injected"
                animationDuration={800}
              />
              <Line
                yAxisId="volume"
                type="monotone"
                dataKey="ma7_injected"
                stroke="#f59e0b"
                strokeWidth={1.5}
                strokeDasharray="6 3"
                dot={false}
                name="MA7"
                connectNulls
                animationDuration={1000}
              />
              <Line
                yAxisId="volume"
                type="monotone"
                dataKey="ma14_injected"
                stroke="#8b5cf6"
                strokeWidth={1.5}
                strokeDasharray="6 3"
                dot={false}
                name="MA14"
                connectNulls
                animationDuration={1200}
              />
              <Bar
                yAxisId="campaigns"
                dataKey="campaigns_active"
                fill="#334155"
                opacity={0.5}
                barSize={4}
                name="Campaigns"
                animationDuration={600}
              />
              <Brush
                dataKey="date"
                height={28}
                stroke="#3b82f6"
                fill="#0f1520"
                tickFormatter={fmtDate}
                travellerWidth={8}
              />
            </ComposedChart>
          </ResponsiveContainer>
        </div>
      </div>

      {/* ================================================================
          4. Secondary Charts Row
          ================================================================ */}
      <div className="ia-secondary-charts">
        {/* Left: Delivery Funnel Rates */}
        <div className="ia-chart-card">
          <div className="ia-chart-card-title">
            <FontAwesomeIcon icon={faSignal} style={{ color: '#10b981' }} />
            Delivery Funnel Rates
          </div>
          <ResponsiveContainer width="100%" height={280}>
            <LineChart
              data={daily_series}
              margin={{ top: 5, right: 10, left: 0, bottom: 0 }}
            >
              <CartesianGrid
                strokeDasharray="3 3"
                stroke="#1e293b"
                vertical={false}
              />
              <XAxis
                dataKey="date"
                tickFormatter={fmtDate}
                stroke="#334155"
                tick={{ fill: '#64748b', fontSize: 10 }}
                tickLine={false}
                axisLine={{ stroke: '#1e293b' }}
                minTickGap={40}
              />
              <YAxis
                tickFormatter={pctAxisTickFormatter}
                domain={[0, 100]}
                stroke="#334155"
                tick={{ fill: '#64748b', fontSize: 10 }}
                tickLine={false}
                axisLine={false}
                width={40}
              />
              <Tooltip content={<RateTooltip />} />
              <Legend
                wrapperStyle={{ fontSize: '0.7rem', color: '#94a3b8' }}
                iconSize={8}
              />
              <Line
                type="monotone"
                dataKey="delivery_rate"
                stroke="#10b981"
                strokeWidth={2}
                dot={false}
                name="Delivery Rate"
                animationDuration={800}
              />
              <Line
                type="monotone"
                dataKey="open_rate"
                stroke="#3b82f6"
                strokeWidth={2}
                dot={false}
                name="Open Rate"
                animationDuration={1000}
              />
              <Line
                type="monotone"
                dataKey="click_rate"
                stroke="#f59e0b"
                strokeWidth={2}
                dot={false}
                name="Click Rate"
                animationDuration={1200}
              />
            </LineChart>
          </ResponsiveContainer>
        </div>

        {/* Right: Cumulative Volume */}
        <div className="ia-chart-card">
          <div className="ia-chart-card-title">
            <FontAwesomeIcon icon={faChartArea} style={{ color: '#8b5cf6' }} />
            Cumulative Injection Volume
          </div>
          <ResponsiveContainer width="100%" height={280}>
            <AreaChart
              data={daily_series}
              margin={{ top: 5, right: 10, left: 0, bottom: 0 }}
            >
              <defs>
                <linearGradient id="iaCumulativeGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor="#8b5cf6" stopOpacity={0.35} />
                  <stop offset="95%" stopColor="#8b5cf6" stopOpacity={0.02} />
                </linearGradient>
              </defs>
              <CartesianGrid
                strokeDasharray="3 3"
                stroke="#1e293b"
                vertical={false}
              />
              <XAxis
                dataKey="date"
                tickFormatter={fmtDate}
                stroke="#334155"
                tick={{ fill: '#64748b', fontSize: 10 }}
                tickLine={false}
                axisLine={{ stroke: '#1e293b' }}
                minTickGap={40}
              />
              <YAxis
                tickFormatter={yAxisTickFormatter}
                stroke="#334155"
                tick={{ fill: '#64748b', fontSize: 10 }}
                tickLine={false}
                axisLine={false}
                width={55}
              />
              <Tooltip content={<CumulativeTooltip />} />
              <Area
                type="monotone"
                dataKey="cumulative_injected"
                stroke="#8b5cf6"
                strokeWidth={2}
                fill="url(#iaCumulativeGrad)"
                name="Cumulative"
                animationDuration={1000}
              />
            </AreaChart>
          </ResponsiveContainer>
        </div>
      </div>

      {/* ================================================================
          5. Statistics Cards Row
          ================================================================ */}
      <div className="ia-section-label">Key Statistics</div>
      <div className="ia-stats-row">
        {/* Peak Volume */}
        <div className="ia-stat-card peak">
          <span className="ia-stat-card-label">
            <FontAwesomeIcon icon={faCrown} style={{ color: '#10b981' }} />
            Peak Volume (High)
          </span>
          <span className="ia-stat-card-value" style={{ color: '#10b981' }}>
            {fmtNum(summary.peak_daily_volume)}
          </span>
          <span className="ia-stat-card-sub">{fmtFullDate(summary.peak_date)}</span>
        </div>

        {/* Low Volume */}
        <div className="ia-stat-card low">
          <span className="ia-stat-card-label">
            <FontAwesomeIcon icon={faSkullCrossbones} style={{ color: '#ef4444' }} />
            Low Volume (Low)
          </span>
          <span className="ia-stat-card-value" style={{ color: '#ef4444' }}>
            {fmtNum(summary.low_daily_volume)}
          </span>
          <span className="ia-stat-card-sub">{fmtFullDate(summary.low_date)}</span>
        </div>

        {/* Volatility */}
        <div className="ia-stat-card volatility">
          <span className="ia-stat-card-label">
            <FontAwesomeIcon icon={faBolt} style={{ color: '#f59e0b' }} />
            Volatility (CoV)
          </span>
          <span className="ia-stat-card-value" style={{ color: '#f59e0b' }}>
            {(volatility.coefficient_of_variation * 100).toFixed(1)}%
          </span>
          <span className={`ia-stat-badge ${stabilityClass(volatility.stability_score)}`}>
            {volatility.stability_score.toUpperCase()}
          </span>
          <span className="ia-stat-card-sub">
            Max swing: {fmtNum(volatility.max_daily_swing)} on{' '}
            {fmtDate(volatility.max_swing_date)}
          </span>
        </div>

        {/* Revenue */}
        <div className="ia-stat-card revenue">
          <span className="ia-stat-card-label">
            <FontAwesomeIcon icon={faDollarSign} style={{ color: '#8b5cf6' }} />
            Total Revenue
          </span>
          <span className="ia-stat-card-value" style={{ color: '#8b5cf6' }}>
            {fmtCurrency(summary.total_revenue)}
          </span>
          <span className="ia-stat-card-sub">
            {summary.total_injected > 0
              ? fmtCurrency((summary.total_revenue / summary.total_injected) * 1000) +
                ' / 1K sends'
              : 'N/A'}
          </span>
        </div>
      </div>

      {/* ================================================================
          6. ISP Breakdown Table
          ================================================================ */}
      <div className="ia-table-section">
        <div className="ia-table-container">
          <div className="ia-table-header">
            <div className="ia-table-title">
              <FontAwesomeIcon icon={faGlobe} />
              ISP Breakdown
            </div>
            <span style={{ fontSize: '0.7rem', color: '#475569' }}>
              {sortedISPs.length} providers
            </span>
          </div>
          <div className="ia-table-scroll">
            <table className="ia-table">
              <thead>
                <tr>
                  <th
                    className={sortField === 'isp' ? 'sorted' : ''}
                    onClick={() => handleSort('isp')}
                  >
                    ISP
                    <FontAwesomeIcon icon={sortIcon('isp')} className="ia-sort-icon" />
                  </th>
                  <th
                    className={sortField === 'total_sent' ? 'sorted' : ''}
                    onClick={() => handleSort('total_sent')}
                  >
                    Volume
                    <FontAwesomeIcon
                      icon={sortIcon('total_sent')}
                      className="ia-sort-icon"
                    />
                  </th>
                  <th
                    className={sortField === 'volume_share_pct' ? 'sorted' : ''}
                    onClick={() => handleSort('volume_share_pct')}
                  >
                    Share %
                    <FontAwesomeIcon
                      icon={sortIcon('volume_share_pct')}
                      className="ia-sort-icon"
                    />
                  </th>
                  <th
                    className={sortField === 'delivery_rate' ? 'sorted' : ''}
                    onClick={() => handleSort('delivery_rate')}
                  >
                    Delivery
                    <FontAwesomeIcon
                      icon={sortIcon('delivery_rate')}
                      className="ia-sort-icon"
                    />
                  </th>
                  <th
                    className={sortField === 'open_rate' ? 'sorted' : ''}
                    onClick={() => handleSort('open_rate')}
                  >
                    Open
                    <FontAwesomeIcon
                      icon={sortIcon('open_rate')}
                      className="ia-sort-icon"
                    />
                  </th>
                  <th
                    className={sortField === 'click_rate' ? 'sorted' : ''}
                    onClick={() => handleSort('click_rate')}
                  >
                    Click
                    <FontAwesomeIcon
                      icon={sortIcon('click_rate')}
                      className="ia-sort-icon"
                    />
                  </th>
                  <th
                    className={sortField === 'bounce_rate' ? 'sorted' : ''}
                    onClick={() => handleSort('bounce_rate')}
                  >
                    Bounce
                    <FontAwesomeIcon
                      icon={sortIcon('bounce_rate')}
                      className="ia-sort-icon"
                    />
                  </th>
                  <th
                    className={sortField === 'complaint_rate' ? 'sorted' : ''}
                    onClick={() => handleSort('complaint_rate')}
                  >
                    Complaint
                    <FontAwesomeIcon
                      icon={sortIcon('complaint_rate')}
                      className="ia-sort-icon"
                    />
                  </th>
                  <th
                    className={sortField === 'trend_pct' ? 'sorted' : ''}
                    onClick={() => handleSort('trend_pct')}
                  >
                    Trend
                    <FontAwesomeIcon
                      icon={sortIcon('trend_pct')}
                      className="ia-sort-icon"
                    />
                  </th>
                </tr>
              </thead>
              <tbody>
                {sortedISPs.map((isp) => (
                  <tr key={isp.isp}>
                    <td className="ia-text-cell">{isp.isp}</td>
                    <td>
                      <div className="ia-volume-bar-wrapper">
                        <span>{fmtNum(isp.total_sent)}</span>
                        <div className="ia-volume-bar">
                          <div
                            className="ia-volume-bar-fill"
                            style={{
                              width: `${(isp.total_sent / maxISPVolume) * 100}%`,
                            }}
                          />
                        </div>
                      </div>
                    </td>
                    <td>{fmtPct(isp.volume_share_pct)}</td>
                    <td>
                      <span
                        className={`ia-rate ${rateClass(isp.delivery_rate, { good: 95, warn: 90 })}`}
                      >
                        {fmtPct(isp.delivery_rate)}
                      </span>
                    </td>
                    <td>
                      <span
                        className={`ia-rate ${rateClass(isp.open_rate, { good: 20, warn: 10 })}`}
                      >
                        {fmtPct(isp.open_rate)}
                      </span>
                    </td>
                    <td>
                      <span
                        className={`ia-rate ${rateClass(isp.click_rate, { good: 3, warn: 1 })}`}
                      >
                        {fmtPct(isp.click_rate)}
                      </span>
                    </td>
                    <td>
                      <span
                        className={`ia-rate ${rateClass(isp.bounce_rate, { good: 2, warn: 5 }, true)}`}
                      >
                        {fmtPct(isp.bounce_rate)}
                      </span>
                    </td>
                    <td>
                      <span
                        className={`ia-rate ${rateClass(isp.complaint_rate, { good: 0.1, warn: 0.3 }, true)}`}
                      >
                        {fmtPct(isp.complaint_rate)}
                      </span>
                    </td>
                    <td>
                      <span
                        className={`ia-trend-indicator ${
                          isp.trend === 'up'
                            ? 'up'
                            : isp.trend === 'down'
                              ? 'down'
                              : 'flat'
                        }`}
                      >
                        <FontAwesomeIcon
                          icon={
                            isp.trend === 'up'
                              ? faArrowUp
                              : isp.trend === 'down'
                                ? faArrowDown
                                : faMinus
                          }
                        />
                        {Math.abs(isp.trend_pct).toFixed(1)}%
                      </span>
                    </td>
                  </tr>
                ))}
                {sortedISPs.length === 0 && (
                  <tr>
                    <td colSpan={9} style={{ textAlign: 'center', color: '#475569', padding: '2rem' }}>
                      No ISP data available
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </div>
      </div>

      {/* ================================================================
          7. Weekly Aggregates Table
          ================================================================ */}
      <div className="ia-table-section">
        <div className="ia-table-container">
          <div className="ia-table-header">
            <div className="ia-table-title">
              <FontAwesomeIcon icon={faCalendarWeek} />
              Weekly Aggregates
            </div>
            <span style={{ fontSize: '0.7rem', color: '#475569' }}>
              {weekly_aggregates.length} weeks
            </span>
          </div>
          <div className="ia-table-scroll">
            <table className="ia-table">
              <thead>
                <tr>
                  <th>Week</th>
                  <th>Total Injected</th>
                  <th>Avg Daily</th>
                  <th>Delivery Rate</th>
                  <th>Open Rate</th>
                  <th>WoW Change</th>
                </tr>
              </thead>
              <tbody>
                {weekly_aggregates.map((week) => {
                  const wowChange = week.change_from_prev_week_pct;
                  const wowClass =
                    wowChange === null
                      ? 'flat'
                      : wowChange > 0
                        ? 'up'
                        : wowChange < 0
                          ? 'down'
                          : 'flat';
                  return (
                    <tr key={week.week_start}>
                      <td className="ia-text-cell">
                        {week.week_label}
                        <span
                          style={{
                            display: 'block',
                            fontSize: '0.65rem',
                            color: '#475569',
                            fontFamily: "'SF Mono', 'Fira Code', monospace",
                          }}
                        >
                          {fmtDate(week.week_start)} &ndash; {fmtDate(week.week_end)}
                        </span>
                      </td>
                      <td>{fmtNum(week.total_injected)}</td>
                      <td>{fmtNum(week.avg_daily)}</td>
                      <td>
                        <span
                          className={`ia-rate ${rateClass(week.delivery_rate, { good: 95, warn: 90 })}`}
                        >
                          {fmtPct(week.delivery_rate)}
                        </span>
                      </td>
                      <td>
                        <span
                          className={`ia-rate ${rateClass(week.open_rate, { good: 20, warn: 10 })}`}
                        >
                          {fmtPct(week.open_rate)}
                        </span>
                      </td>
                      <td>
                        {wowChange !== null ? (
                          <span className={`ia-trend-indicator ${wowClass}`}>
                            <FontAwesomeIcon
                              icon={
                                wowChange > 0
                                  ? faArrowUp
                                  : wowChange < 0
                                    ? faArrowDown
                                    : faMinus
                              }
                            />
                            {Math.abs(wowChange).toFixed(1)}%
                          </span>
                        ) : (
                          <span className="ia-trend-indicator flat">
                            <FontAwesomeIcon icon={faMinus} /> &mdash;
                          </span>
                        )}
                      </td>
                    </tr>
                  );
                })}
                {weekly_aggregates.length === 0 && (
                  <tr>
                    <td colSpan={6} style={{ textAlign: 'center', color: '#475569', padding: '2rem' }}>
                      No weekly data available
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </div>
      </div>

      {/* ── Footer ── */}
      <div
        style={{
          textAlign: 'center',
          fontSize: '0.7rem',
          color: '#334155',
          padding: '1rem 0 0.5rem',
          fontFamily: "'SF Mono', 'Fira Code', monospace",
        }}
      >
        {data.period.start && data.period.end && (
          <span>
            {fmtFullDate(data.period.start)} &ndash; {fmtFullDate(data.period.end)}
            {' · '}
            {data.period.days} days
          </span>
        )}
        {lastUpdated && (
          <span>
            {' · '}Last updated {lastUpdated.toLocaleTimeString()}
          </span>
        )}
        {autoRefresh && <span> · Auto-refresh ON (60s)</span>}
      </div>
    </div>
  );
};

export default InjectionAnalytics;
