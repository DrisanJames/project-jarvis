import { useMemo } from 'react';
import { EverflowDailyPerformance } from '../../types';

interface RevenueChartProps {
  data: EverflowDailyPerformance[];
  height?: number;
}

export const RevenueChart: React.FC<RevenueChartProps> = ({
  data,
  height = 200,
}) => {
  // Sort data by date ascending for chart display
  const sortedData = useMemo(() => {
    return [...data].sort((a, b) => a.date.localeCompare(b.date));
  }, [data]);

  const chartData = useMemo(() => {
    if (sortedData.length === 0) return null;

    const maxRevenue = Math.max(...sortedData.map(d => d.revenue), 1);
    const maxClicks = Math.max(...sortedData.map(d => d.clicks), 1);

    return {
      maxRevenue,
      maxClicks,
      bars: sortedData.map((day, index) => ({
        date: day.date,
        revenue: day.revenue,
        clicks: day.clicks,
        conversions: day.conversions,
        conversionRate: day.conversion_rate,
        revenuePercent: (day.revenue / maxRevenue) * 100,
        clicksPercent: (day.clicks / maxClicks) * 100,
        index,
      })),
    };
  }, [sortedData]);

  const formatCurrency = (value: number): string => {
    if (value >= 1000) {
      return `$${(value / 1000).toFixed(1)}K`;
    }
    return `$${value.toFixed(0)}`;
  };

  const formatDate = (dateStr: string): string => {
    const date = new Date(dateStr);
    return date.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
  };

  if (!chartData || sortedData.length === 0) {
    return (
      <div className="revenue-chart-empty" style={{ height }}>
        No revenue data available
      </div>
    );
  }

  const barWidth = Math.max(8, Math.min(40, (100 / chartData.bars.length) - 1));

  return (
    <div className="revenue-chart" style={{ height }}>
      <div className="chart-container">
        <div className="chart-y-axis">
          <span>{formatCurrency(chartData.maxRevenue)}</span>
          <span>{formatCurrency(chartData.maxRevenue / 2)}</span>
          <span>$0</span>
        </div>
        <div className="chart-bars">
          {chartData.bars.map((bar) => (
            <div 
              key={bar.date} 
              className="chart-bar-container"
              style={{ width: `${barWidth}%` }}
            >
              <div 
                className="chart-bar revenue-bar"
                style={{ height: `${bar.revenuePercent}%` }}
                title={`${formatDate(bar.date)}: ${formatCurrency(bar.revenue)} (${bar.conversions} conv.)`}
              >
                <span className="bar-tooltip">
                  <strong>{formatDate(bar.date)}</strong>
                  <br />
                  Revenue: {formatCurrency(bar.revenue)}
                  <br />
                  Conversions: {bar.conversions}
                  <br />
                  Clicks: {bar.clicks.toLocaleString()}
                </span>
              </div>
            </div>
          ))}
        </div>
      </div>
      <div className="chart-x-axis">
        {chartData.bars.length <= 15 ? (
          chartData.bars.map((bar) => (
            <span key={bar.date} style={{ width: `${barWidth}%` }}>
              {formatDate(bar.date)}
            </span>
          ))
        ) : (
          <>
            <span>{formatDate(chartData.bars[0].date)}</span>
            <span>{formatDate(chartData.bars[Math.floor(chartData.bars.length / 2)].date)}</span>
            <span>{formatDate(chartData.bars[chartData.bars.length - 1].date)}</span>
          </>
        )}
      </div>
      <style>{`
        .revenue-chart {
          width: 100%;
          display: flex;
          flex-direction: column;
        }
        .revenue-chart-empty {
          display: flex;
          align-items: center;
          justify-content: center;
          color: var(--text-muted, #666);
        }
        .chart-container {
          flex: 1;
          display: flex;
          gap: 8px;
        }
        .chart-y-axis {
          display: flex;
          flex-direction: column;
          justify-content: space-between;
          font-size: 0.75rem;
          color: var(--text-muted, #666);
          padding-right: 4px;
          min-width: 45px;
          text-align: right;
        }
        .chart-bars {
          flex: 1;
          display: flex;
          align-items: flex-end;
          gap: 2px;
          border-bottom: 1px solid var(--border-color, #e5e7eb);
          border-left: 1px solid var(--border-color, #e5e7eb);
          padding: 0 4px;
        }
        .chart-bar-container {
          display: flex;
          flex-direction: column;
          align-items: center;
          justify-content: flex-end;
          height: 100%;
        }
        .chart-bar {
          width: 80%;
          min-height: 2px;
          border-radius: 2px 2px 0 0;
          transition: all 0.2s ease;
          position: relative;
        }
        .chart-bar:hover {
          opacity: 0.8;
          transform: scaleX(1.1);
        }
        .revenue-bar {
          background: linear-gradient(to top, var(--accent-green, #22c55e), #4ade80);
        }
        .bar-tooltip {
          display: none;
          position: absolute;
          bottom: 100%;
          left: 50%;
          transform: translateX(-50%);
          background: var(--card-bg, #1a1a2e);
          color: var(--text-primary, #fff);
          padding: 8px;
          border-radius: 4px;
          font-size: 0.75rem;
          white-space: nowrap;
          z-index: 10;
          box-shadow: 0 2px 8px rgba(0,0,0,0.2);
        }
        .chart-bar:hover .bar-tooltip {
          display: block;
        }
        .chart-x-axis {
          display: flex;
          justify-content: space-between;
          font-size: 0.7rem;
          color: var(--text-muted, #666);
          padding-top: 4px;
          padding-left: 53px;
        }
        .chart-x-axis span {
          text-align: center;
        }
      `}</style>
    </div>
  );
};

export default RevenueChart;
