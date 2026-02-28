import React, { useState, useMemo } from 'react';
import { useApi } from '../../hooks/useApi';
import { Loading } from '../common/Loading';
import { ScenarioPlanning } from './ScenarioPlanning';
import { useDateFilter } from '../../context/DateFilterContext';
import { useCostOverrides } from '../../context/CostOverrideContext';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faDollarSign, 
  faArrowUp, 
  faArrowDown, 
  faUsers, 
  faChartPie, 
  faBullseye,
  faExclamationTriangle,
  faChartBar,
  faSlidersH,
  faExclamationCircle
} from '@fortawesome/free-solid-svg-icons';

// Types
interface CostItem {
  name: string;
  category: string;
  monthly_cost: number;
  type: string;
}

interface CostBreakdown {
  total_monthly_cost: number;
  esp_costs: CostItem[];
  vendor_costs: CostItem[];
  payroll_costs: CostItem[];
  revenue_share_costs: CostItem[];
  costs_by_category: Record<string, number>;
}

interface MonthlyPL {
  month: string;
  month_name: string;
  gross_revenue: number;
  esp_revenue: Record<string, number>;
  total_costs: number;
  esp_costs: number;
  vendor_costs: number;
  payroll_costs: number;
  revenue_share: number;
  gross_profit: number;
  gross_margin: number;
  net_profit: number;
  net_margin: number;
  is_actual: boolean;
  is_forecast: boolean;
}

interface AnnualForecast {
  fiscal_year: string;
  months: MonthlyPL[];
  total_revenue: number;
  total_costs: number;
  total_esp_costs: number;
  total_vendor_costs: number;
  total_payroll: number;
  total_revenue_share: number;
  annual_profit: number;
  annual_margin: number;
  avg_monthly_revenue: number;
  avg_monthly_profit: number;
  break_even_revenue: number;
}

interface KeyMetrics {
  monthly_burn_rate: number;
  runway_months: number;
  revenue_per_employee: number;
  cost_per_email: number;
  revenue_per_email: number;
  profit_per_email: number;
}

interface FinancialDashboardData {
  timestamp: string;
  current_month: MonthlyPL;
  cost_breakdown: CostBreakdown;
  annual_forecast: AnnualForecast;
  key_metrics: KeyMetrics;
}

// Formatters
const formatCurrency = (value: number): string => {
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 0,
    maximumFractionDigits: 0,
  }).format(value);
};

const formatCurrencyDetailed = (value: number): string => {
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value);
};

const formatPercent = (value: number): string => {
  return `${value.toFixed(1)}%`;
};

const formatNumber = (value: number, decimals: number = 4): string => {
  return `$${value.toFixed(decimals)}`;
};

// Category colors for pie chart
const categoryColors: Record<string, string> = {
  'ESP': '#ff6b35',
  'Platform': '#3b82f6',
  'Deliverability': '#22c55e',
  'Infrastructure': '#8b5cf6',
  'Productivity': '#f59e0b',
  'Data': '#06b6d4',
  'Development': '#ec4899',
  'Employees': '#10b981',
  'Contractors': '#14b8a6',
  'Compliance': '#f43f5e',
  'Creative': '#a855f7',
  'AI/ML': '#6366f1',
  'Marketing': '#84cc16',
  'Revenue Share': '#ef4444',
};

// Components
const MetricCard: React.FC<{
  title: string;
  value: string;
  subtitle?: string;
  icon: React.ReactNode;
  trend?: 'up' | 'down' | 'neutral';
  trendValue?: string;
}> = ({ title, value, subtitle, icon, trend, trendValue }) => (
  <div className="metric-card">
    <div className="metric-header">
      <span className="metric-icon">{icon}</span>
      <span className="metric-title">{title}</span>
    </div>
    <div className={`metric-value ${trend === 'up' ? 'positive' : trend === 'down' ? 'negative' : ''}`}>
      {value}
    </div>
    {subtitle && <div className="metric-subtitle">{subtitle}</div>}
    {trendValue && (
      <div className={`metric-trend ${trend}`}>
        {trend === 'up' ? <FontAwesomeIcon icon={faArrowUp} /> : trend === 'down' ? <FontAwesomeIcon icon={faArrowDown} /> : null}
        <span>{trendValue}</span>
      </div>
    )}
  </div>
);

const CostBreakdownChart: React.FC<{ breakdown: CostBreakdown }> = ({ breakdown }) => {
  const categories = Object.entries(breakdown.costs_by_category)
    .sort(([, a], [, b]) => b - a);
  
  const total = breakdown.total_monthly_cost;

  return (
    <div className="cost-breakdown-chart">
      <h3><FontAwesomeIcon icon={faChartPie} /> Cost Breakdown by Category</h3>
      <div className="chart-container">
        <div className="pie-visual">
          {categories.map(([category, amount], index) => {
            const percentage = (amount / total) * 100;
            const rotation = categories
              .slice(0, index)
              .reduce((sum, [, a]) => sum + (a / total) * 360, 0);
            return (
              <div
                key={category}
                className="pie-segment"
                style={{
                  '--rotation': `${rotation}deg`,
                  '--percentage': percentage,
                  '--color': categoryColors[category] || '#666',
                } as React.CSSProperties}
              />
            );
          })}
        </div>
        <div className="legend">
          {categories.map(([category, amount]) => (
            <div key={category} className="legend-item">
              <span 
                className="legend-color" 
                style={{ backgroundColor: categoryColors[category] || '#666' }}
              />
              <span className="legend-label">{category}</span>
              <span className="legend-value">{formatCurrency(amount)}</span>
              <span className="legend-percent">{formatPercent((amount / total) * 100)}</span>
            </div>
          ))}
        </div>
      </div>
      <div className="total-row">
        <span>Total Monthly Costs</span>
        <span className="total-value">{formatCurrency(total)}</span>
      </div>
    </div>
  );
};

const CostDetailTable: React.FC<{ 
  title: string; 
  items: CostItem[]; 
  icon: React.ReactNode;
}> = ({ title, items, icon }) => {
  const total = items.reduce((sum, item) => sum + item.monthly_cost, 0);
  
  return (
    <div className="cost-detail-table">
      <h4>{icon} {title}</h4>
      <table>
        <tbody>
          {items.map((item, index) => (
            <tr key={index}>
              <td className="item-name">{item.name}</td>
              <td className="item-category">{item.category}</td>
              <td className="item-cost">{formatCurrencyDetailed(item.monthly_cost)}</td>
            </tr>
          ))}
        </tbody>
        <tfoot>
          <tr>
            <td colSpan={2}><strong>Subtotal</strong></td>
            <td className="item-cost"><strong>{formatCurrencyDetailed(total)}</strong></td>
          </tr>
        </tfoot>
      </table>
    </div>
  );
};

type TabView = 'overview' | 'scenarios';

const tabStyles = `
  .tab-nav {
    display: flex;
    gap: 0.5rem;
  }
  
  .tab-btn {
    display: flex;
    align-items: center;
    gap: 0.375rem;
    padding: 0.5rem 1rem;
    border: none;
    background: var(--bg-tertiary, #2a2a3e);
    color: var(--text-muted);
    border-radius: 6px;
    cursor: pointer;
    font-size: 0.875rem;
    transition: all 0.2s;
  }
  
  .tab-btn:hover {
    background: var(--bg-secondary, #1e1e2e);
    color: var(--text-primary);
  }
  
  .tab-btn.active {
    background: var(--accent-blue, #3b82f6);
    color: white;
  }
`;

export const FinancialDashboard: React.FC = () => {
  const [activeTab, setActiveTab] = useState<TabView>('overview');
  
  // Use global date filter
  const { dateRange } = useDateFilter();
  
  // Use shared cost overrides context
  const { costOverrides, hasUnsavedChanges, getCostWithOverride } = useCostOverrides();
  
  // Build API URL with date range params
  const apiUrl = `/api/financial/dashboard?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
  
  const { data, loading, error } = useApi<FinancialDashboardData>(apiUrl, {
    pollingInterval: 60000,
  });

  // Calculate adjusted costs with overrides applied (real-time sync)
  const adjustedCosts = useMemo(() => {
    if (!data?.cost_breakdown) return null;
    
    const { esp_costs, vendor_costs, payroll_costs, revenue_share_costs } = data.cost_breakdown;
    
    const calculateSectionTotal = (items: CostItem[], type: string) => {
      return items.reduce((sum, item) => {
        return sum + getCostWithOverride(item.name, type, item.monthly_cost);
      }, 0);
    };
    
    const espTotal = calculateSectionTotal(esp_costs, 'esp');
    const vendorTotal = calculateSectionTotal(vendor_costs, 'vendor');
    const payrollTotal = calculateSectionTotal(payroll_costs, 'payroll');
    const revenueShareTotal = calculateSectionTotal(revenue_share_costs, 'revenue_share');
    
    const totalCosts = espTotal + vendorTotal + payrollTotal + revenueShareTotal;
    const originalTotal = data.cost_breakdown.total_monthly_cost;
    const costChange = totalCosts - originalTotal;
    
    return {
      espCosts: espTotal,
      vendorCosts: vendorTotal,
      payrollCosts: payrollTotal,
      revenueShare: revenueShareTotal,
      totalCosts,
      originalTotal,
      costChange,
      hasChanges: Math.abs(costChange) > 0.01,
    };
  }, [data?.cost_breakdown, costOverrides, getCostWithOverride]);

  // Render scenarios tab
  if (activeTab === 'scenarios') {
    return (
      <div className="financial-dashboard">
        <div className="dashboard-header">
          <h1>Financial Dashboard</h1>
          <div className="tab-nav">
            <button 
              className="tab-btn"
              onClick={() => setActiveTab('overview')}
            >
              <FontAwesomeIcon icon={faChartBar} /> P&L Overview
            </button>
            <button 
              className="tab-btn active"
              onClick={() => setActiveTab('scenarios')}
            >
              <FontAwesomeIcon icon={faSlidersH} /> Growth Scenarios
            </button>
          </div>
        </div>
        <ScenarioPlanning />
        <style>{tabStyles}</style>
      </div>
    );
  }

  if (loading && !data) {
    return <Loading message="Loading financial data..." />;
  }

  if (error) {
    return (
      <div className="financial-dashboard">
        <div className="error-card">
          <FontAwesomeIcon icon={faExclamationTriangle} />
          <p>Failed to load financial data: {String(error)}</p>
        </div>
      </div>
    );
  }

  if (!data) {
    return (
      <div className="financial-dashboard">
        <div className="error-card">
          <p>No financial data available. Make sure the revenue model is configured in config.yaml.</p>
        </div>
      </div>
    );
  }

  const { current_month, cost_breakdown, annual_forecast, key_metrics } = data;
  const isProfitable = current_month.net_profit >= 0;

  return (
    <div className="financial-dashboard">
      {/* Header with Tabs */}
      <div className="dashboard-header">
        <h1>Financial Dashboard</h1>
        <div className="tab-nav">
          <button 
            className="tab-btn active"
            onClick={() => setActiveTab('overview')}
          >
            <FontAwesomeIcon icon={faChartBar} /> P&L Overview
          </button>
          <button 
            className="tab-btn"
            onClick={() => setActiveTab('scenarios')}
          >
            <FontAwesomeIcon icon={faSlidersH} /> Growth Scenarios
          </button>
        </div>
      </div>
      
      <div className="period-label">{current_month.month_name} ({dateRange.label})</div>

      {/* Key Metrics Row */}
      <div className="metrics-grid">
        <MetricCard
          title="Monthly Revenue"
          value={formatCurrency(current_month.gross_revenue)}
          subtitle="From all ESPs"
          icon={<FontAwesomeIcon icon={faDollarSign} />}
          trend="up"
        />
        <MetricCard
          title="Monthly Costs"
          value={formatCurrency(adjustedCosts?.totalCosts ?? current_month.total_costs)}
          subtitle={adjustedCosts?.hasChanges ? `Adjusted (${adjustedCosts.costChange > 0 ? '+' : ''}${formatCurrency(adjustedCosts.costChange)})` : "All operating costs"}
          icon={<FontAwesomeIcon icon={faArrowDown} />}
        />
        <MetricCard
          title="Net Profit"
          value={formatCurrency(current_month.gross_revenue - (adjustedCosts?.totalCosts ?? current_month.total_costs))}
          subtitle={`${formatPercent(((current_month.gross_revenue - (adjustedCosts?.totalCosts ?? current_month.total_costs)) / current_month.gross_revenue) * 100)} margin`}
          icon={isProfitable ? <FontAwesomeIcon icon={faArrowUp} /> : <FontAwesomeIcon icon={faArrowDown} />}
          trend={(current_month.gross_revenue - (adjustedCosts?.totalCosts ?? current_month.total_costs)) >= 0 ? 'up' : 'down'}
        />
        <MetricCard
          title="Break-Even"
          value={formatCurrency(annual_forecast.break_even_revenue)}
          subtitle="Monthly revenue needed"
          icon={<FontAwesomeIcon icon={faBullseye} />}
        />
      </div>

      {/* Secondary Metrics */}
      <div className="metrics-grid secondary">
        <MetricCard
          title="Revenue per Employee"
          value={formatCurrency(key_metrics.revenue_per_employee)}
          subtitle="Monthly"
          icon={<FontAwesomeIcon icon={faUsers} />}
        />
        <MetricCard
          title="Annual Profit Forecast"
          value={formatCurrency(annual_forecast.annual_profit)}
          subtitle={`${formatPercent(annual_forecast.annual_margin)} margin`}
          icon={<FontAwesomeIcon icon={faChartBar} />}
          trend={annual_forecast.annual_profit >= 0 ? 'up' : 'down'}
        />
        <MetricCard
          title="Cost per 1K Emails"
          value={formatNumber(key_metrics.cost_per_email * 1000, 2)}
          subtitle="Operating cost"
          icon={<FontAwesomeIcon icon={faChartPie} />}
        />
        <MetricCard
          title="Profit per 1K Emails"
          value={formatNumber(key_metrics.profit_per_email * 1000, 2)}
          subtitle="Net margin per email"
          icon={<FontAwesomeIcon icon={faArrowUp} />}
          trend={key_metrics.profit_per_email >= 0 ? 'up' : 'down'}
        />
      </div>

      {/* Main Content Grid */}
      <div className="content-grid">
        {/* Left Column: P&L Summary */}
        <div className="pl-summary">
          <div className="pl-header-row">
            <h3>Current Month P&L</h3>
            {hasUnsavedChanges && (
              <span className="sync-badge">
                <FontAwesomeIcon icon={faExclamationCircle} />
                Unsaved edits
              </span>
            )}
          </div>
          <div className="pl-section">
            <div className="pl-header">Revenue</div>
            {Object.entries(current_month.esp_revenue).map(([esp, revenue]) => (
              <div key={esp} className="pl-row">
                <span>{esp}</span>
                <span className="revenue">{formatCurrencyDetailed(revenue)}</span>
              </div>
            ))}
            <div className="pl-row total">
              <span>Gross Revenue</span>
              <span className="revenue">{formatCurrencyDetailed(current_month.gross_revenue)}</span>
            </div>
          </div>
          
          <div className="pl-section">
            <div className="pl-header">Costs {adjustedCosts?.hasChanges && <span className="adjusted-label">(adjusted)</span>}</div>
            <div className="pl-row">
              <span>ESP Costs</span>
              <span className={`cost ${adjustedCosts?.espCosts !== current_month.esp_costs ? 'modified' : ''}`}>
                ({formatCurrencyDetailed(adjustedCosts?.espCosts ?? current_month.esp_costs)})
              </span>
            </div>
            <div className="pl-row">
              <span>Vendor/SaaS</span>
              <span className={`cost ${adjustedCosts?.vendorCosts !== current_month.vendor_costs ? 'modified' : ''}`}>
                ({formatCurrencyDetailed(adjustedCosts?.vendorCosts ?? current_month.vendor_costs)})
              </span>
            </div>
            <div className="pl-row">
              <span>Payroll</span>
              <span className={`cost ${adjustedCosts?.payrollCosts !== current_month.payroll_costs ? 'modified' : ''}`}>
                ({formatCurrencyDetailed(adjustedCosts?.payrollCosts ?? current_month.payroll_costs)})
              </span>
            </div>
            <div className="pl-row">
              <span>Revenue Share</span>
              <span className={`cost ${adjustedCosts?.revenueShare !== current_month.revenue_share ? 'modified' : ''}`}>
                ({formatCurrencyDetailed(adjustedCosts?.revenueShare ?? current_month.revenue_share)})
              </span>
            </div>
            <div className="pl-row total">
              <span>Total Costs</span>
              <span className={`cost ${adjustedCosts?.hasChanges ? 'modified' : ''}`}>
                ({formatCurrencyDetailed(adjustedCosts?.totalCosts ?? current_month.total_costs)})
                {adjustedCosts?.hasChanges && (
                  <span className="cost-change">
                    {adjustedCosts.costChange > 0 ? '+' : ''}{formatCurrency(adjustedCosts.costChange)}
                  </span>
                )}
              </span>
            </div>
          </div>

          <div className="pl-section profit">
            <div className="pl-row gross">
              <span>Gross Profit</span>
              <span className={(current_month.gross_revenue - (adjustedCosts?.espCosts ?? current_month.esp_costs)) >= 0 ? 'positive' : 'negative'}>
                {formatCurrencyDetailed(current_month.gross_revenue - (adjustedCosts?.espCosts ?? current_month.esp_costs))}
              </span>
            </div>
            <div className="pl-row net">
              <span>Net Profit</span>
              <span className={(current_month.gross_revenue - (adjustedCosts?.totalCosts ?? current_month.total_costs)) >= 0 ? 'positive' : 'negative'}>
                {formatCurrencyDetailed(current_month.gross_revenue - (adjustedCosts?.totalCosts ?? current_month.total_costs))}
              </span>
            </div>
            <div className="pl-row margin">
              <span>Net Margin</span>
              <span className={(current_month.gross_revenue - (adjustedCosts?.totalCosts ?? current_month.total_costs)) >= 0 ? 'positive' : 'negative'}>
                {formatPercent(((current_month.gross_revenue - (adjustedCosts?.totalCosts ?? current_month.total_costs)) / current_month.gross_revenue) * 100)}
              </span>
            </div>
          </div>
        </div>

        {/* Right Column: Cost Breakdown */}
        <CostBreakdownChart breakdown={cost_breakdown} />
      </div>

      {/* Cost Details */}
      <div className="cost-details">
        <h3>Cost Details (Monthly)</h3>
        <div className="cost-details-grid">
          <CostDetailTable 
            title="ESP Costs" 
            items={cost_breakdown.esp_costs} 
            icon={<FontAwesomeIcon icon={faDollarSign} />}
          />
          <CostDetailTable 
            title="Vendor/SaaS" 
            items={cost_breakdown.vendor_costs} 
            icon={<FontAwesomeIcon icon={faChartPie} />}
          />
          <CostDetailTable 
            title="Payroll" 
            items={cost_breakdown.payroll_costs} 
            icon={<FontAwesomeIcon icon={faUsers} />}
          />
          {cost_breakdown.revenue_share_costs.length > 0 && (
            <CostDetailTable 
              title="Revenue Share" 
              items={cost_breakdown.revenue_share_costs} 
              icon={<FontAwesomeIcon icon={faArrowDown} />}
            />
          )}
        </div>
      </div>

      <style>{`
        ${tabStyles}
        
        .financial-dashboard {
          padding: 1rem;
          max-width: 1600px;
          margin: 0 auto;
        }

        .dashboard-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 0.5rem;
        }

        .dashboard-header h1 {
          margin: 0;
          font-size: 1.5rem;
        }

        .period-label {
          color: var(--text-muted);
          font-size: 0.875rem;
          margin-bottom: 1.5rem;
        }

        .metrics-grid {
          display: grid;
          grid-template-columns: repeat(4, 1fr);
          gap: 1rem;
          margin-bottom: 1.5rem;
        }

        .metrics-grid.secondary {
          margin-bottom: 2rem;
        }

        .metric-card {
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 12px;
          padding: 1.25rem;
          border: 1px solid var(--border-color, #333);
        }

        .metric-header {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          color: var(--text-muted);
          margin-bottom: 0.75rem;
        }

        .metric-title {
          font-size: 0.875rem;
        }

        .metric-value {
          font-size: 1.5rem;
          font-weight: 700;
          margin-bottom: 0.25rem;
        }

        .metric-value.positive {
          color: var(--accent-green, #22c55e);
        }

        .metric-value.negative {
          color: var(--accent-red, #ef4444);
        }

        .metric-subtitle {
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        .metric-trend {
          display: flex;
          align-items: center;
          gap: 0.25rem;
          font-size: 0.75rem;
          margin-top: 0.5rem;
        }

        .metric-trend.up {
          color: var(--accent-green, #22c55e);
        }

        .metric-trend.down {
          color: var(--accent-red, #ef4444);
        }

        .content-grid {
          display: grid;
          grid-template-columns: 1fr 1fr;
          gap: 1.5rem;
          margin-bottom: 2rem;
        }

        .pl-summary {
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 12px;
          padding: 1.5rem;
          border: 1px solid var(--border-color, #333);
        }

        .pl-summary h3 {
          margin: 0;
          font-size: 1rem;
        }

        .pl-header-row {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 1rem;
        }

        .sync-badge {
          display: flex;
          align-items: center;
          gap: 0.25rem;
          font-size: 0.6875rem;
          font-weight: 500;
          padding: 0.25rem 0.5rem;
          background: rgba(250, 204, 21, 0.15);
          color: var(--accent-yellow, #facc15);
          border-radius: 4px;
        }

        .adjusted-label {
          font-size: 0.625rem;
          font-weight: 400;
          color: var(--accent-yellow, #facc15);
          margin-left: 0.375rem;
        }

        .cost.modified {
          color: var(--accent-yellow, #facc15) !important;
          transition: color 0.2s ease;
        }

        .cost-change {
          font-size: 0.6875rem;
          margin-left: 0.5rem;
          color: var(--text-muted);
        }

        .pl-section {
          margin-bottom: 1rem;
          padding-bottom: 1rem;
          border-bottom: 1px solid var(--border-color, #333);
        }

        .pl-section:last-child {
          border-bottom: none;
          margin-bottom: 0;
          padding-bottom: 0;
        }

        .pl-header {
          font-size: 0.75rem;
          font-weight: 600;
          text-transform: uppercase;
          color: var(--text-muted);
          margin-bottom: 0.5rem;
        }

        .pl-row {
          display: flex;
          justify-content: space-between;
          padding: 0.375rem 0;
          font-size: 0.875rem;
        }

        .pl-row.total {
          font-weight: 600;
          border-top: 1px solid var(--border-color, #333);
          margin-top: 0.5rem;
          padding-top: 0.75rem;
        }

        .pl-row .revenue {
          color: var(--accent-green, #22c55e);
        }

        .pl-row .cost {
          color: var(--accent-orange, #f59e0b);
        }

        .pl-section.profit .pl-row {
          padding: 0.5rem 0;
        }

        .pl-row.gross {
          font-size: 1rem;
        }

        .pl-row.net {
          font-size: 1.25rem;
          font-weight: 700;
        }

        .pl-row .positive {
          color: var(--accent-green, #22c55e);
        }

        .pl-row .negative {
          color: var(--accent-red, #ef4444);
        }

        .cost-breakdown-chart {
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 12px;
          padding: 1.5rem;
          border: 1px solid var(--border-color, #333);
        }

        .cost-breakdown-chart h3 {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          margin: 0 0 1rem 0;
          font-size: 1rem;
        }

        .chart-container {
          display: flex;
          gap: 2rem;
        }

        .pie-visual {
          width: 200px;
          height: 200px;
          border-radius: 50%;
          background: conic-gradient(
            from 0deg,
            #ff6b35 0deg 90deg,
            #3b82f6 90deg 180deg,
            #22c55e 180deg 270deg,
            #8b5cf6 270deg 360deg
          );
          position: relative;
        }

        .legend {
          flex: 1;
        }

        .legend-item {
          display: grid;
          grid-template-columns: 12px 1fr auto auto;
          gap: 0.75rem;
          align-items: center;
          padding: 0.375rem 0;
          font-size: 0.8125rem;
        }

        .legend-color {
          width: 12px;
          height: 12px;
          border-radius: 3px;
        }

        .legend-value {
          font-weight: 500;
          text-align: right;
        }

        .legend-percent {
          color: var(--text-muted);
          text-align: right;
          min-width: 45px;
        }

        .total-row {
          display: flex;
          justify-content: space-between;
          margin-top: 1rem;
          padding-top: 1rem;
          border-top: 1px solid var(--border-color, #333);
          font-weight: 600;
        }

        .total-value {
          color: var(--accent-orange, #f59e0b);
        }

        .cost-details {
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 12px;
          padding: 1.5rem;
          border: 1px solid var(--border-color, #333);
        }

        .cost-details h3 {
          margin: 0 0 1rem 0;
          font-size: 1rem;
        }

        .cost-details-grid {
          display: grid;
          grid-template-columns: repeat(auto-fit, minmax(350px, 1fr));
          gap: 1.5rem;
        }

        .cost-detail-table h4 {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          margin: 0 0 0.75rem 0;
          font-size: 0.875rem;
          color: var(--text-muted);
        }

        .cost-detail-table table {
          width: 100%;
          border-collapse: collapse;
        }

        .cost-detail-table td {
          padding: 0.5rem 0;
          font-size: 0.8125rem;
          border-bottom: 1px solid var(--border-color, #333);
        }

        .cost-detail-table .item-name {
          font-weight: 500;
        }

        .cost-detail-table .item-category {
          color: var(--text-muted);
          font-size: 0.75rem;
        }

        .cost-detail-table .item-cost {
          text-align: right;
          color: var(--accent-orange, #f59e0b);
        }

        .cost-detail-table tfoot td {
          border-top: 2px solid var(--border-color, #333);
          border-bottom: none;
          padding-top: 0.75rem;
        }

        .error-card {
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 12px;
          padding: 2rem;
          border: 1px solid var(--accent-red, #ef4444);
          text-align: center;
          color: var(--accent-red, #ef4444);
        }

        .error-card svg {
          margin-bottom: 1rem;
        }

        @media (max-width: 1200px) {
          .metrics-grid {
            grid-template-columns: repeat(2, 1fr);
          }
          .content-grid {
            grid-template-columns: 1fr;
          }
        }

        @media (max-width: 768px) {
          .metrics-grid {
            grid-template-columns: 1fr;
          }
          .cost-details-grid {
            grid-template-columns: 1fr;
          }
        }
      `}</style>
    </div>
  );
};

export default FinancialDashboard;
