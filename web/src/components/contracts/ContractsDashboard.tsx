import React from 'react';
import { useApi } from '../../hooks/useApi';
import { useDateFilter } from '../../context/DateFilterContext';
import { EverflowESPRevenue } from '../../types';
import { Loading } from '../common/Loading';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faDollarSign, faArrowUp, faArrowDown, faEnvelope, faExclamationTriangle, faCheckCircle } from '@fortawesome/free-solid-svg-icons';

interface ESPRevenueResponse {
  timestamp: string;
  esp_revenue: EverflowESPRevenue[];
  total_revenue: number;
  total_payout: number;
  total_sent: number;
  total_delivered: number;
  total_clicks: number;
  total_conversions: number;
}

const formatCurrency = (value: number): string => {
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value);
};

const formatNumber = (value: number): string => {
  if (value >= 1000000000) {
    return `${(value / 1000000000).toFixed(1)}B`;
  }
  if (value >= 1000000) {
    return `${(value / 1000000).toFixed(1)}M`;
  }
  if (value >= 1000) {
    return `${(value / 1000).toFixed(1)}K`;
  }
  return new Intl.NumberFormat('en-US').format(value);
};

const formatPercent = (value: number): string => {
  return `${value.toFixed(1)}%`;
};

// Contract card component for each ESP
const ContractCard: React.FC<{ esp: EverflowESPRevenue }> = ({ esp }) => {
  const costMetrics = esp.cost_metrics;
  const hasContract = !!costMetrics;

  return (
    <div className="contract-card">
      <div className="contract-header">
        <div className="esp-info">
          <span className="esp-indicator" style={{ backgroundColor: getESPColor(esp.esp_name) }} />
          <h3>{esp.esp_name}</h3>
        </div>
        {hasContract ? (
          <span className="contract-badge active">
            <FontAwesomeIcon icon={faCheckCircle} />
            Contract Active
          </span>
        ) : (
          <span className="contract-badge inactive">
            <FontAwesomeIcon icon={faExclamationTriangle} />
            No Contract
          </span>
        )}
      </div>

      {hasContract && costMetrics ? (
        <>
          {/* Contract Details */}
          <div className="contract-section">
            <h4>Contract Terms</h4>
            <div className="contract-terms">
              <div className="term">
                <span className="term-label">Monthly Fee</span>
                <span className="term-value">{formatCurrency(costMetrics.monthly_fee)}</span>
              </div>
              <div className="term">
                <span className="term-label">Included Emails</span>
                <span className="term-value">{formatNumber(costMetrics.monthly_included)}/mo</span>
              </div>
              <div className="term">
                <span className="term-label">Overage Rate</span>
                <span className="term-value">{formatCurrency(costMetrics.overage_rate_per_1000)}/1K</span>
              </div>
            </div>
          </div>

          {/* Usage & Costs */}
          <div className="contract-section">
            <h4>Period Usage & Costs</h4>
            <div className="usage-grid">
              <div className="usage-item">
                <FontAwesomeIcon icon={faEnvelope} />
                <div className="usage-content">
                  <span className="usage-label">Emails Sent</span>
                  <span className="usage-value">{formatNumber(costMetrics.emails_sent)}</span>
                </div>
              </div>
              <div className="usage-item">
                <span className={`overage-indicator ${costMetrics.emails_over_included > 0 ? 'over' : 'under'}`}>
                  {costMetrics.emails_over_included > 0 ? <FontAwesomeIcon icon={faArrowUp} /> : <FontAwesomeIcon icon={faArrowDown} />}
                </span>
                <div className="usage-content">
                  <span className="usage-label">Over Included</span>
                  <span className={`usage-value ${costMetrics.emails_over_included > 0 ? 'warning' : 'good'}`}>
                    {costMetrics.emails_over_included > 0 ? formatNumber(costMetrics.emails_over_included) : 'None'}
                  </span>
                </div>
              </div>
            </div>

            <div className="cost-breakdown">
              <div className="cost-row">
                <span>Base Cost (Pro-rated)</span>
                <span>{formatCurrency(costMetrics.pro_rated_base_cost)}</span>
              </div>
              <div className="cost-row">
                <span>Overage Cost</span>
                <span className={costMetrics.overage_cost > 0 ? 'warning' : ''}>
                  {formatCurrency(costMetrics.overage_cost)}
                </span>
              </div>
              <div className="cost-row total">
                <span>Total Cost</span>
                <span>{formatCurrency(costMetrics.total_cost)}</span>
              </div>
            </div>
          </div>

          {/* eCPM Comparison */}
          <div className="contract-section">
            <h4>eCPM Analysis</h4>
            <div className="ecpm-comparison">
              <div className="ecpm-item cost">
                <span className="ecpm-label">Cost eCPM</span>
                <span className="ecpm-value">{formatCurrency(costMetrics.cost_ecpm)}</span>
                <span className="ecpm-desc">per 1,000 emails</span>
              </div>
              <div className="ecpm-item revenue">
                <span className="ecpm-label">Revenue eCPM</span>
                <span className="ecpm-value">{formatCurrency(costMetrics.revenue_ecpm)}</span>
                <span className="ecpm-desc">per 1,000 emails</span>
              </div>
              <div className="ecpm-item net">
                <span className="ecpm-label">Net eCPM</span>
                <span className={`ecpm-value ${costMetrics.revenue_ecpm - costMetrics.cost_ecpm > 0 ? 'positive' : 'negative'}`}>
                  {formatCurrency(costMetrics.revenue_ecpm - costMetrics.cost_ecpm)}
                </span>
                <span className="ecpm-desc">profit per 1,000</span>
              </div>
            </div>
          </div>

          {/* Profitability */}
          <div className="contract-section profitability">
            <h4>Profitability</h4>
            <div className="profit-grid">
              <div className="profit-item main">
                <span className="profit-label">Gross Profit</span>
                <span className={`profit-value ${costMetrics.gross_profit >= 0 ? 'positive' : 'negative'}`}>
                  {formatCurrency(costMetrics.gross_profit)}
                </span>
              </div>
              <div className="profit-item">
                <span className="profit-label">Gross Margin</span>
                <span className={`profit-value ${costMetrics.gross_margin >= 0 ? 'positive' : 'negative'}`}>
                  {formatPercent(costMetrics.gross_margin)}
                </span>
              </div>
              <div className="profit-item">
                <span className="profit-label">ROI</span>
                <span className={`profit-value ${costMetrics.roi >= 0 ? 'positive' : 'negative'}`}>
                  {formatPercent(costMetrics.roi)}
                </span>
              </div>
              <div className="profit-item">
                <span className="profit-label">Net Rev/Email</span>
                <span className={`profit-value ${costMetrics.net_revenue_per_email >= 0 ? 'positive' : 'negative'}`}>
                  ${(costMetrics.net_revenue_per_email * 1000).toFixed(4)}/1K
                </span>
              </div>
            </div>
          </div>

          {/* Revenue Summary */}
          <div className="contract-section revenue-summary">
            <div className="revenue-row">
              <span>Total Revenue</span>
              <span className="revenue">{formatCurrency(esp.revenue)}</span>
            </div>
            <div className="revenue-row">
              <span>Total Cost</span>
              <span className="cost">- {formatCurrency(costMetrics.total_cost)}</span>
            </div>
            <div className="revenue-row net-profit">
              <span>Net Profit</span>
              <span className={costMetrics.gross_profit >= 0 ? 'positive' : 'negative'}>
                {formatCurrency(costMetrics.gross_profit)}
              </span>
            </div>
          </div>
        </>
      ) : (
        <div className="no-contract">
          <p>No contract configured for this ESP.</p>
          <p className="hint">Add contract details to config.yaml to see cost analysis.</p>
          <div className="basic-stats">
            <div className="stat">
              <span className="stat-label">Campaigns</span>
              <span className="stat-value">{esp.campaign_count}</span>
            </div>
            <div className="stat">
              <span className="stat-label">Emails Sent</span>
              <span className="stat-value">{formatNumber(esp.total_sent)}</span>
            </div>
            <div className="stat">
              <span className="stat-label">Revenue</span>
              <span className="stat-value">{formatCurrency(esp.revenue)}</span>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// Map ESP names to colors
const getESPColor = (espName: string): string => {
  const colors: Record<string, string> = {
    'SparkPost': '#ff6b35',
    'Mailgun': '#e74c3c',
    'Amazon SES': '#ff9900',
    'SES': '#ff9900',
    'SendGrid': '#1a82e2',
    'Postmark': '#ffde00',
    'Unknown': '#666666',
  };
  return colors[espName] || colors['Unknown'];
};

export const ContractsDashboard: React.FC = () => {
  // Use global date filter
  const { dateRange } = useDateFilter();
  
  // Build API URL with date range params
  const apiUrl = `/api/everflow/esp-revenue?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
  
  const { data, loading, error } = useApi<ESPRevenueResponse>(apiUrl, {
    pollingInterval: 60000,
  });

  if (loading && !data) {
    return <Loading message="Loading ESP contract data..." />;
  }

  if (error) {
    return (
      <div className="contracts-dashboard">
        <div className="summary-card">
          <div className="card-header">
            <span className="card-title">Error</span>
          </div>
          <p className="error-message">Failed to load ESP contract data: {String(error)}</p>
        </div>
      </div>
    );
  }

  const espData = data?.esp_revenue || [];
  const espsWithContracts = espData.filter(esp => esp.cost_metrics);
  const espsWithoutContracts = espData.filter(esp => !esp.cost_metrics);

  // Calculate totals for ESPs with contracts
  const totalCost = espsWithContracts.reduce((sum, esp) => sum + (esp.cost_metrics?.total_cost || 0), 0);
  const totalRevenue = espsWithContracts.reduce((sum, esp) => sum + esp.revenue, 0);
  const totalProfit = totalRevenue - totalCost;
  const totalEmailsSent = espsWithContracts.reduce((sum, esp) => sum + (esp.cost_metrics?.emails_sent || 0), 0);

  return (
    <div className="contracts-dashboard">
      {/* Dashboard Header */}
      <div style={{ 
        marginBottom: '1.5rem',
        display: 'flex',
        alignItems: 'center',
        gap: '0.75rem',
      }}>
        <FontAwesomeIcon icon={faDollarSign} style={{ fontSize: '1.5rem', color: 'var(--accent-green)' }} />
        <h2 style={{ margin: 0 }}>ESP Contract Performance</h2>
        <span style={{
          padding: '0.25rem 0.75rem',
          backgroundColor: 'var(--primary-color)',
          color: 'white',
          borderRadius: '1rem',
          fontSize: '0.75rem',
          fontWeight: 500,
        }}>
          {dateRange.label}
        </span>
      </div>
      
      {/* Summary Cards */}
      <div className="summary-cards">
        <div className="summary-card">
          <div className="card-header">
            <FontAwesomeIcon icon={faDollarSign} />
            <span className="card-title">Total ESP Costs</span>
          </div>
          <div className="summary-value cost">{formatCurrency(totalCost)}</div>
          <div className="summary-label">{dateRange.label}</div>
        </div>
        <div className="summary-card">
          <div className="card-header">
            <FontAwesomeIcon icon={faArrowUp} />
            <span className="card-title">Total Revenue</span>
          </div>
          <div className="summary-value revenue">{formatCurrency(totalRevenue)}</div>
          <div className="summary-label">From contracted ESPs</div>
        </div>
        <div className="summary-card">
          <div className="card-header">
            {totalProfit >= 0 ? <FontAwesomeIcon icon={faArrowUp} /> : <FontAwesomeIcon icon={faArrowDown} />}
            <span className="card-title">Net Profit</span>
          </div>
          <div className={`summary-value ${totalProfit >= 0 ? 'positive' : 'negative'}`}>
            {formatCurrency(totalProfit)}
          </div>
          <div className="summary-label">
            {totalRevenue > 0 ? `${formatPercent((totalProfit / totalRevenue) * 100)} margin` : 'No revenue'}
          </div>
        </div>
        <div className="summary-card">
          <div className="card-header">
            <FontAwesomeIcon icon={faEnvelope} />
            <span className="card-title">Total Volume</span>
          </div>
          <div className="summary-value">{formatNumber(totalEmailsSent)}</div>
          <div className="summary-label">Emails sent</div>
        </div>
      </div>

      {/* ESP Contract Cards */}
      <div className="contracts-section">
        <h2>ESP Contracts</h2>
        {espData.length === 0 ? (
          <div className="summary-card">
            <p className="no-data">No ESP data available. Make sure Everflow is configured and campaigns are linked to Ongage.</p>
          </div>
        ) : (
          <div className="contracts-grid">
            {espData.map(esp => (
              <ContractCard key={esp.esp_name} esp={esp} />
            ))}
          </div>
        )}
      </div>

      {espsWithoutContracts.length > 0 && espsWithContracts.length > 0 && (
        <div className="info-banner">
          <FontAwesomeIcon icon={faExclamationTriangle} />
          <span>
            {espsWithoutContracts.length} ESP(s) without contracts: {espsWithoutContracts.map(e => e.esp_name).join(', ')}. 
            Add contract details to config.yaml for cost analysis.
          </span>
        </div>
      )}

      <style>{`
        .contracts-dashboard {
          padding: 1rem;
        }

        .summary-cards {
          display: grid;
          grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
          gap: 1rem;
          margin-bottom: 2rem;
        }

        .summary-card {
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 12px;
          padding: 1.25rem;
          border: 1px solid var(--border-color, #333);
        }

        .summary-card .card-header {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          color: var(--text-muted);
          margin-bottom: 0.5rem;
        }

        .summary-card .card-title {
          font-size: 0.875rem;
          font-weight: 500;
        }

        .summary-value {
          font-size: 1.75rem;
          font-weight: 700;
          margin: 0.5rem 0;
        }

        .summary-value.cost {
          color: var(--accent-orange, #f59e0b);
        }

        .summary-value.revenue {
          color: var(--accent-green, #22c55e);
        }

        .summary-value.positive {
          color: var(--accent-green, #22c55e);
        }

        .summary-value.negative {
          color: var(--accent-red, #ef4444);
        }

        .summary-label {
          color: var(--text-muted);
          font-size: 0.875rem;
        }

        .contracts-section h2 {
          margin-bottom: 1rem;
          font-size: 1.25rem;
          font-weight: 600;
        }

        .contracts-grid {
          display: grid;
          grid-template-columns: repeat(auto-fit, minmax(400px, 1fr));
          gap: 1.5rem;
        }

        .contract-card {
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 12px;
          padding: 1.5rem;
          border: 1px solid var(--border-color, #333);
        }

        .contract-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 1.5rem;
          padding-bottom: 1rem;
          border-bottom: 1px solid var(--border-color, #333);
        }

        .esp-info {
          display: flex;
          align-items: center;
          gap: 0.75rem;
        }

        .esp-indicator {
          width: 16px;
          height: 16px;
          border-radius: 4px;
        }

        .esp-info h3 {
          margin: 0;
          font-size: 1.25rem;
          font-weight: 600;
        }

        .contract-badge {
          display: flex;
          align-items: center;
          gap: 0.375rem;
          padding: 0.375rem 0.75rem;
          border-radius: 20px;
          font-size: 0.75rem;
          font-weight: 600;
        }

        .contract-badge.active {
          background: rgba(34, 197, 94, 0.15);
          color: var(--accent-green, #22c55e);
        }

        .contract-badge.inactive {
          background: rgba(239, 68, 68, 0.15);
          color: var(--accent-red, #ef4444);
        }

        .contract-section {
          margin-bottom: 1.25rem;
        }

        .contract-section h4 {
          margin: 0 0 0.75rem 0;
          font-size: 0.875rem;
          font-weight: 600;
          color: var(--text-muted);
          text-transform: uppercase;
          letter-spacing: 0.5px;
        }

        .contract-terms {
          display: grid;
          grid-template-columns: repeat(3, 1fr);
          gap: 1rem;
        }

        .term {
          text-align: center;
        }

        .term-label {
          display: block;
          font-size: 0.75rem;
          color: var(--text-muted);
          margin-bottom: 0.25rem;
        }

        .term-value {
          font-size: 1rem;
          font-weight: 600;
        }

        .usage-grid {
          display: grid;
          grid-template-columns: repeat(2, 1fr);
          gap: 1rem;
          margin-bottom: 1rem;
        }

        .usage-item {
          display: flex;
          align-items: center;
          gap: 0.75rem;
          padding: 0.75rem;
          background: var(--bg-primary, #12121a);
          border-radius: 8px;
        }

        .usage-content {
          display: flex;
          flex-direction: column;
        }

        .usage-label {
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        .usage-value {
          font-weight: 600;
        }

        .usage-value.warning {
          color: var(--accent-orange, #f59e0b);
        }

        .usage-value.good {
          color: var(--accent-green, #22c55e);
        }

        .overage-indicator.over {
          color: var(--accent-orange, #f59e0b);
        }

        .overage-indicator.under {
          color: var(--accent-green, #22c55e);
        }

        .cost-breakdown {
          background: var(--bg-primary, #12121a);
          border-radius: 8px;
          padding: 0.75rem;
        }

        .cost-row {
          display: flex;
          justify-content: space-between;
          padding: 0.375rem 0;
          font-size: 0.875rem;
        }

        .cost-row.total {
          border-top: 1px solid var(--border-color, #333);
          margin-top: 0.5rem;
          padding-top: 0.75rem;
          font-weight: 600;
        }

        .cost-row .warning {
          color: var(--accent-orange, #f59e0b);
        }

        .ecpm-comparison {
          display: grid;
          grid-template-columns: repeat(3, 1fr);
          gap: 1rem;
        }

        .ecpm-item {
          text-align: center;
          padding: 1rem;
          background: var(--bg-primary, #12121a);
          border-radius: 8px;
        }

        .ecpm-label {
          display: block;
          font-size: 0.75rem;
          color: var(--text-muted);
          margin-bottom: 0.25rem;
        }

        .ecpm-value {
          display: block;
          font-size: 1.125rem;
          font-weight: 700;
        }

        .ecpm-item.cost .ecpm-value {
          color: var(--accent-orange, #f59e0b);
        }

        .ecpm-item.revenue .ecpm-value {
          color: var(--accent-green, #22c55e);
        }

        .ecpm-value.positive {
          color: var(--accent-green, #22c55e);
        }

        .ecpm-value.negative {
          color: var(--accent-red, #ef4444);
        }

        .ecpm-desc {
          display: block;
          font-size: 0.625rem;
          color: var(--text-muted);
          margin-top: 0.25rem;
        }

        .profit-grid {
          display: grid;
          grid-template-columns: repeat(2, 1fr);
          gap: 1rem;
        }

        .profit-item {
          padding: 1rem;
          background: var(--bg-primary, #12121a);
          border-radius: 8px;
          text-align: center;
        }

        .profit-item.main {
          grid-column: span 2;
        }

        .profit-label {
          display: block;
          font-size: 0.75rem;
          color: var(--text-muted);
          margin-bottom: 0.25rem;
        }

        .profit-value {
          font-size: 1.25rem;
          font-weight: 700;
        }

        .profit-item.main .profit-value {
          font-size: 1.75rem;
        }

        .profit-value.positive {
          color: var(--accent-green, #22c55e);
        }

        .profit-value.negative {
          color: var(--accent-red, #ef4444);
        }

        .revenue-summary {
          background: var(--bg-primary, #12121a);
          border-radius: 8px;
          padding: 1rem;
        }

        .revenue-row {
          display: flex;
          justify-content: space-between;
          padding: 0.5rem 0;
          font-size: 0.9375rem;
        }

        .revenue-row .revenue {
          color: var(--accent-green, #22c55e);
          font-weight: 600;
        }

        .revenue-row .cost {
          color: var(--accent-orange, #f59e0b);
          font-weight: 600;
        }

        .revenue-row.net-profit {
          border-top: 1px solid var(--border-color, #333);
          margin-top: 0.5rem;
          padding-top: 1rem;
          font-size: 1rem;
          font-weight: 600;
        }

        .no-contract {
          text-align: center;
          padding: 2rem;
          color: var(--text-muted);
        }

        .no-contract .hint {
          font-size: 0.875rem;
          margin-top: 0.5rem;
        }

        .no-contract .basic-stats {
          display: flex;
          justify-content: center;
          gap: 2rem;
          margin-top: 1.5rem;
        }

        .no-contract .stat {
          text-align: center;
        }

        .no-contract .stat-label {
          display: block;
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        .no-contract .stat-value {
          font-size: 1.125rem;
          font-weight: 600;
          color: var(--text-primary);
        }

        .info-banner {
          display: flex;
          align-items: center;
          gap: 0.75rem;
          padding: 1rem;
          background: rgba(245, 158, 11, 0.1);
          border: 1px solid rgba(245, 158, 11, 0.3);
          border-radius: 8px;
          color: var(--accent-orange, #f59e0b);
          font-size: 0.875rem;
          margin-top: 1.5rem;
        }

        .error-message {
          color: var(--accent-red, #ef4444);
        }

        .no-data {
          text-align: center;
          color: var(--text-muted);
          padding: 2rem;
        }

        @media (max-width: 768px) {
          .contracts-grid {
            grid-template-columns: 1fr;
          }

          .contract-terms,
          .ecpm-comparison {
            grid-template-columns: 1fr;
          }

          .profit-grid {
            grid-template-columns: 1fr;
          }

          .profit-item.main {
            grid-column: span 1;
          }
        }
      `}</style>
    </div>
  );
};

export default ContractsDashboard;
