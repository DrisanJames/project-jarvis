import React, { useState, useCallback } from 'react';
import { useApi } from '../../hooks/useApi';
import { Loading } from '../common/Loading';
import { useDateFilter } from '../../context/DateFilterContext';
import { useCostOverrides, CostOverride } from '../../context/CostOverrideContext';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faArrowUp, 
  faEdit, 
  faSave,
  faUndo,
  faLightbulb,
  faBullseye,
  faDollarSign,
  faExclamationTriangle,
  faCheckCircle,
  faChevronDown,
  faChevronUp
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
}

interface GrowthScenario {
  name: string;
  description: string;
  monthly_growth: number;
  volume_growth: number;
  ecpm_change: number;
  is_default: boolean;
}

interface MonthlyPL {
  month: string;
  month_name: string;
  gross_revenue: number;
  total_costs: number;
  net_profit: number;
  net_margin: number;
  is_actual: boolean;
  is_forecast: boolean;
}

interface GrowthMetrics {
  starting_mrr: number;
  ending_mrr: number;
  mrr_growth: number;
  months_to_break_even: number;
  profitable_months: number;
  projected_annual_profit: number;
}

interface AnnualSummary {
  total_revenue: number;
  total_costs: number;
  total_profit: number;
  average_margin: number;
  ending_mrr: number;
  revenue_growth: number;
}

interface ScenarioForecast {
  scenario: GrowthScenario;
  months: MonthlyPL[];
  annual_summary: AnnualSummary;
  growth_metrics: GrowthMetrics;
}

interface HistoricalCostPoint {
  month: string;
  cost: number;
}

interface GrowthDrivers {
  current_monthly_revenue: number;
  current_monthly_sent: number;
  current_ecpm: number;
  revenue_per_employee: number;
  esp_volumes: Record<string, number>;
  esp_revenue: Record<string, number>;
  esp_ecpms: Record<string, number>;
  volume_growth_potential: number;
  ecpm_optimization_potential: number;
  historical_costs: HistoricalCostPoint[];
  cost_growth_rate: number;
  current_operating_cost: number;
  target_operating_cost: number;
  cost_reduction_target: number;
  esp_mix_optimization: number;
}

interface ScenarioPlanningData {
  timestamp: string;
  growth_drivers: GrowthDrivers;
  scenarios: ScenarioForecast[];
  cost_breakdown: CostBreakdown;
  recommendations: string[];
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

const formatCurrencyK = (value: number): string => {
  if (value >= 1000000) {
    return `$${(value / 1000000).toFixed(1)}M`;
  }
  if (value >= 1000) {
    return `$${(value / 1000).toFixed(0)}K`;
  }
  return formatCurrency(value);
};

const formatPercent = (value: number): string => {
  return `${value >= 0 ? '+' : ''}${value.toFixed(1)}%`;
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
  return value.toFixed(0);
};

// Scenario comparison card
const ScenarioCard: React.FC<{
  forecast: ScenarioForecast;
  isSelected: boolean;
  onSelect: () => void;
}> = ({ forecast, isSelected, onSelect }) => {
  const { scenario, annual_summary, growth_metrics } = forecast;
  const isProfitable = annual_summary.total_profit >= 0;

  return (
    <div 
      className={`scenario-card ${isSelected ? 'selected' : ''} ${scenario.is_default ? 'default' : ''}`}
      onClick={onSelect}
    >
      {scenario.is_default && <span className="default-badge">Recommended</span>}
      <h4>{scenario.name}</h4>
      <p className="scenario-desc">{scenario.description}</p>
      
      <div className="scenario-metrics">
        <div className="metric">
          <span className="label">Annual Revenue</span>
          <span className="value">{formatCurrencyK(annual_summary.total_revenue)}</span>
        </div>
        <div className="metric">
          <span className="label">Annual Profit</span>
          <span className={`value ${isProfitable ? 'positive' : 'negative'}`}>
            {formatCurrencyK(annual_summary.total_profit)}
          </span>
        </div>
        <div className="metric">
          <span className="label">Ending MRR</span>
          <span className="value">{formatCurrencyK(annual_summary.ending_mrr)}</span>
        </div>
        <div className="metric">
          <span className="label">MRR Growth</span>
          <span className={`value ${growth_metrics.mrr_growth >= 0 ? 'positive' : 'negative'}`}>
            {formatPercent(growth_metrics.mrr_growth)}
          </span>
        </div>
      </div>

      <div className="growth-rates">
        <span className="rate">
          <FontAwesomeIcon icon={faArrowUp} /> Volume: {formatPercent(scenario.volume_growth)}/mo
        </span>
        <span className="rate">
          <FontAwesomeIcon icon={faDollarSign} /> eCPM: {formatPercent(scenario.ecpm_change)}/mo
        </span>
      </div>
    </div>
  );
};

// Editable cost row
const EditableCostRow: React.FC<{
  item: CostItem;
  override?: CostOverride;
  onOverride: (override: CostOverride) => void;
  onReset: (name: string, category: string) => void;
}> = ({ item, override, onOverride, onReset }) => {
  const [isEditing, setIsEditing] = useState(false);
  const [editValue, setEditValue] = useState(item.monthly_cost.toFixed(2));
  
  const hasOverride = override && override.new_cost !== override.original_cost;

  const handleSave = () => {
    const newCost = parseFloat(editValue);
    if (!isNaN(newCost) && newCost >= 0) {
      onOverride({
        category: item.type,
        name: item.name,
        new_cost: newCost,
        original_cost: override?.original_cost ?? item.monthly_cost,
      });
    }
    setIsEditing(false);
  };

  const handleReset = () => {
    onReset(item.name, item.type);
    setEditValue((override?.original_cost ?? item.monthly_cost).toFixed(2));
  };

  return (
    <tr className={hasOverride ? 'modified' : ''}>
      <td className="name">{item.name}</td>
      <td className="category">{item.category}</td>
      <td className="cost">
        {isEditing ? (
          <div className="edit-input">
            <span>$</span>
            <input
              type="number"
              value={editValue}
              onChange={(e) => setEditValue(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && handleSave()}
              autoFocus
            />
            <button className="save-btn" onClick={handleSave}><FontAwesomeIcon icon={faSave} /></button>
          </div>
        ) : (
          <span className={hasOverride ? 'modified-value' : ''}>
            {formatCurrency(override?.new_cost ?? item.monthly_cost)}
            {hasOverride && (
              <span className="original">was {formatCurrency(override!.original_cost)}</span>
            )}
          </span>
        )}
      </td>
      <td className="actions">
        {!isEditing && (
          <>
            <button className="edit-btn" onClick={() => setIsEditing(true)}>
              <FontAwesomeIcon icon={faEdit} />
            </button>
            {hasOverride && (
              <button className="reset-btn" onClick={handleReset}>
                <FontAwesomeIcon icon={faUndo} />
              </button>
            )}
          </>
        )}
      </td>
    </tr>
  );
};

// Cost section with expandable details
const CostSection: React.FC<{
  title: string;
  items: CostItem[];
  overrides: CostOverride[];
  onOverride: (override: CostOverride) => void;
  onReset: (name: string, category: string) => void;
}> = ({ title, items, overrides, onOverride, onReset }) => {
  const [isExpanded, setIsExpanded] = useState(false);
  
  const total = items.reduce((sum, item) => {
    const override = overrides.find(o => o.name === item.name && o.category === item.type);
    return sum + (override?.new_cost ?? item.monthly_cost);
  }, 0);

  const originalTotal = items.reduce((sum, item) => sum + item.monthly_cost, 0);
  const hasChanges = total !== originalTotal;

  return (
    <div className="cost-section">
      <div className="section-header" onClick={() => setIsExpanded(!isExpanded)}>
        <span className="section-title">{title}</span>
        <span className={`section-total ${hasChanges ? 'modified' : ''}`}>
          {formatCurrency(total)}
          {hasChanges && <span className="change">({total > originalTotal ? '+' : ''}{formatCurrency(total - originalTotal)})</span>}
        </span>
        {isExpanded ? <FontAwesomeIcon icon={faChevronUp} /> : <FontAwesomeIcon icon={faChevronDown} />}
      </div>
      
      {isExpanded && (
        <table className="cost-table">
          <tbody>
            {items.map((item, idx) => (
              <EditableCostRow
                key={idx}
                item={item}
                override={overrides.find(o => o.name === item.name && o.category === item.type)}
                onOverride={onOverride}
                onReset={onReset}
              />
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
};

export const ScenarioPlanning: React.FC = () => {
  const [selectedScenario, setSelectedScenario] = useState<number>(1); // Default to Moderate
  const [saveMessage, setSaveMessage] = useState<{ type: 'success' | 'error'; text: string } | null>(null);
  
  // Use global date filter
  const { dateRange } = useDateFilter();
  
  // Use shared cost overrides context
  const {
    costOverrides,
    hasUnsavedChanges,
    isSaving,
    saveError,
    setOverride,
    resetOverride,
    resetAllOverrides,
    saveOverrides,
    getOverride,
  } = useCostOverrides();

  // Build API URL with date range params
  const apiUrl = `/api/financial/scenarios?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
  
  const { data, loading, error } = useApi<ScenarioPlanningData>(
    apiUrl,
    {
      pollingInterval: 60000,
    }
  );

  // Calculate total monthly cost in real-time with overrides (memoized for performance)
  const { currentTotalCost, totalCostChange } = React.useMemo(() => {
    if (!data?.cost_breakdown) {
      return { currentTotalCost: 0, totalCostChange: 0 };
    }
    
    const { esp_costs, vendor_costs, payroll_costs, revenue_share_costs, total_monthly_cost } = data.cost_breakdown;
    
    const calculateSectionTotal = (items: CostItem[]) => {
      return items.reduce((sum, item) => {
        const override = getOverride(item.name, item.type);
        return sum + (override?.new_cost ?? item.monthly_cost);
      }, 0);
    };
    
    const current = calculateSectionTotal(esp_costs) +
                    calculateSectionTotal(vendor_costs) +
                    calculateSectionTotal(payroll_costs) +
                    calculateSectionTotal(revenue_share_costs);
    
    return {
      currentTotalCost: current,
      totalCostChange: current - total_monthly_cost,
    };
  }, [data?.cost_breakdown, costOverrides, getOverride]);

  // Adjust all scenario forecasts based on cost overrides (real-time sync)
  const adjustedScenarios = React.useMemo((): ScenarioForecast[] => {
    if (!data?.scenarios || totalCostChange === 0) {
      return data?.scenarios || [];
    }

    return data.scenarios.map(scenario => {
      // Adjust each month's costs and recalculate profit/margin
      const adjustedMonths = scenario.months.map(month => {
        const adjustedCosts = month.total_costs + totalCostChange;
        const adjustedProfit = month.gross_revenue - adjustedCosts;
        const adjustedMargin = month.gross_revenue > 0 
          ? (adjustedProfit / month.gross_revenue) * 100 
          : 0;
        
        return {
          ...month,
          total_costs: adjustedCosts,
          net_profit: adjustedProfit,
          net_margin: adjustedMargin,
        };
      });

      // Recalculate annual summary
      const totalRevenue = adjustedMonths.reduce((sum, m) => sum + m.gross_revenue, 0);
      const totalCosts = adjustedMonths.reduce((sum, m) => sum + m.total_costs, 0);
      const totalProfit = totalRevenue - totalCosts;
      const avgMargin = adjustedMonths.reduce((sum, m) => sum + m.net_margin, 0) / adjustedMonths.length;
      const endingMrr = adjustedMonths[adjustedMonths.length - 1]?.gross_revenue || 0;
      const startingMrr = adjustedMonths[0]?.gross_revenue || 0;
      const mrrGrowth = startingMrr > 0 ? ((endingMrr - startingMrr) / startingMrr) * 100 : 0;

      return {
        ...scenario,
        months: adjustedMonths,
        annual_summary: {
          ...scenario.annual_summary,
          total_revenue: totalRevenue,
          total_costs: totalCosts,
          total_profit: totalProfit,
          avg_margin: avgMargin,
          ending_mrr: endingMrr,
        },
        growth_metrics: {
          ...scenario.growth_metrics,
          mrr_growth: mrrGrowth,
        },
      };
    });
  }, [data?.scenarios, totalCostChange]);

  // Save cost configurations using the shared context
  const saveConfigsPermanently = useCallback(async () => {
    setSaveMessage(null);
    
    const success = await saveOverrides();
    
    if (success) {
      const changeText = totalCostChange !== 0 
        ? ` (${totalCostChange > 0 ? '+' : ''}${formatCurrency(totalCostChange)} change)`
        : '';
      setSaveMessage({ 
        type: 'success', 
        text: `Monthly costs updated to ${formatCurrency(currentTotalCost)}${changeText}. Changes synced across all views!` 
      });
      setTimeout(() => setSaveMessage(null), 5000);
    } else {
      setSaveMessage({ type: 'error', text: saveError || 'Failed to save configurations' });
    }
  }, [saveOverrides, totalCostChange, currentTotalCost, saveError]);

  // Reset all cost configurations
  const resetAllConfigs = useCallback(async () => {
    try {
      await fetch('/api/financial/config/costs/reset', { method: 'POST', credentials: 'include' });
      resetAllOverrides();
      setSaveMessage({ type: 'success', text: 'Configurations reset to defaults!' });
      setTimeout(() => setSaveMessage(null), 3000);
    } catch (err) {
      console.error('Failed to reset configs:', err);
    }
  }, [resetAllOverrides]);

  // Use context methods for override management
  const handleOverride = useCallback((override: CostOverride) => {
    setOverride(override);
  }, [setOverride]);

  const handleReset = useCallback((name: string, category: string) => {
    resetOverride(name, category);
  }, [resetOverride]);

  const handleResetAll = useCallback(() => {
    resetAllOverrides();
  }, [resetAllOverrides]);

  if (loading && !data) {
    return <Loading message="Loading scenario planning..." />;
  }

  if (error || !data) {
    return (
      <div className="scenario-planning">
        <div className="error-card">
          <FontAwesomeIcon icon={faExclamationTriangle} />
          <p>Failed to load scenario data</p>
        </div>
      </div>
    );
  }

  const { growth_drivers, cost_breakdown, recommendations } = data;
  // Use adjusted scenarios for real-time cost sync
  const scenarios = adjustedScenarios;
  const selectedForecast = scenarios[selectedScenario];
  const hasAdjustments = totalCostChange !== 0;

  return (
    <div className="scenario-planning">
      {/* Growth Drivers Section */}
      <div className="section">
        <h2>Growth Drivers</h2>
        <p className="section-desc">Key metrics driving your revenue growth potential</p>
        
        <div className="drivers-grid">
          <div className="driver-card">
            <div className="driver-label">Current Monthly Revenue</div>
            <div className="driver-value">{formatCurrencyK(growth_drivers.current_monthly_revenue)}</div>
          </div>
          <div className="driver-card">
            <div className="driver-label">Monthly Email Volume</div>
            <div className="driver-value">{formatNumber(growth_drivers.current_monthly_sent)}</div>
          </div>
          <div className="driver-card">
            <div className="driver-label">Current eCPM</div>
            <div className="driver-value">${growth_drivers.current_ecpm.toFixed(2)}</div>
          </div>
          <div className="driver-card">
            <div className="driver-label">Revenue/Employee</div>
            <div className="driver-value">{formatCurrencyK(growth_drivers.revenue_per_employee)}/mo</div>
          </div>
        </div>

        <div className="esp-breakdown">
          <h4>Revenue by ESP</h4>
          <div className="esp-bars">
            {Object.entries(growth_drivers.esp_revenue).map(([esp, revenue]) => {
              const percentage = (revenue / growth_drivers.current_monthly_revenue) * 100;
              const volume = growth_drivers.esp_volumes[esp] || 0;
              const ecpm = growth_drivers.esp_ecpms[esp] || 0;
              return (
                <div key={esp} className="esp-bar-row">
                  <div className="esp-name">{esp}</div>
                  <div className="esp-bar-container">
                    <div className="esp-bar" style={{ width: `${percentage}%` }} />
                    <span className="esp-value">{formatCurrencyK(revenue)} ({percentage.toFixed(0)}%)</span>
                  </div>
                  <div className="esp-details">
                    <span>{formatNumber(volume)} emails</span>
                    <span>${ecpm.toFixed(2)} eCPM</span>
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      </div>

      {/* Historical Cost Analysis */}
      {growth_drivers.historical_costs && growth_drivers.historical_costs.length > 0 && (
        <div className="section">
          <h2>Historical Cost Trend (2025)</h2>
          <p className="section-desc">
            Operational costs grew {growth_drivers.cost_growth_rate?.toFixed(0) || 0}% in 2025. 
            Current: {formatCurrencyK(growth_drivers.current_operating_cost)} → 
            Target: {formatCurrencyK(growth_drivers.target_operating_cost)} 
            (−{formatCurrencyK(growth_drivers.cost_reduction_target)} reduction)
          </p>
          
          <div className="cost-trend-chart">
            <div className="chart-area">
              {growth_drivers.historical_costs.map((point, idx) => {
                const maxCost = Math.max(...growth_drivers.historical_costs.map(p => p.cost));
                const minCost = Math.min(...growth_drivers.historical_costs.map(p => p.cost)) * 0.9;
                const range = maxCost - minCost;
                const height = ((point.cost - minCost) / range) * 100;
                const monthLabel = point.month.split('-')[1]; // Get MM from YYYY-MM
                const monthNames = ['', 'Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
                return (
                  <div key={idx} className="chart-bar-container">
                    <div className="chart-bar" style={{ height: `${height}%` }}>
                      <span className="bar-value">{formatCurrencyK(point.cost)}</span>
                    </div>
                    <span className="bar-label">{monthNames[parseInt(monthLabel)]}</span>
                  </div>
                );
              })}
            </div>
            <div className="chart-legend">
              <div className="legend-item">
                <span className="legend-marker current"></span>
                <span>Current Max: {formatCurrencyK(growth_drivers.current_operating_cost)}</span>
              </div>
              <div className="legend-item">
                <span className="legend-marker target"></span>
                <span>Target: {formatCurrencyK(growth_drivers.target_operating_cost)}</span>
              </div>
            </div>
          </div>
          
          <div className="cost-trajectory">
            <h4>Cost Reduction Plan</h4>
            <div className="trajectory-steps">
              <div className="step">
                <div className="step-month">Jan 2026</div>
                <div className="step-cost">{formatCurrencyK(growth_drivers.current_operating_cost)}</div>
                <div className="step-label">Current</div>
              </div>
              <div className="step-arrow">→</div>
              <div className="step">
                <div className="step-month">Feb 2026</div>
                <div className="step-cost">{formatCurrencyK(growth_drivers.current_operating_cost - (growth_drivers.cost_reduction_target / 3))}</div>
                <div className="step-label">−{formatCurrencyK(growth_drivers.cost_reduction_target / 3)}</div>
              </div>
              <div className="step-arrow">→</div>
              <div className="step">
                <div className="step-month">Mar 2026</div>
                <div className="step-cost">{formatCurrencyK(growth_drivers.current_operating_cost - (growth_drivers.cost_reduction_target * 2 / 3))}</div>
                <div className="step-label">−{formatCurrencyK(growth_drivers.cost_reduction_target / 3)}</div>
              </div>
              <div className="step-arrow">→</div>
              <div className="step target-step">
                <div className="step-month">Apr+ 2026</div>
                <div className="step-cost">{formatCurrencyK(growth_drivers.target_operating_cost)}</div>
                <div className="step-label">Target achieved</div>
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Scenarios Comparison */}
      <div className="section">
        <div className="section-header-row">
          <div>
            <h2>Growth Scenarios</h2>
            <p className="section-desc">Compare different growth projections based on volume and eCPM optimization</p>
          </div>
          {hasAdjustments && (
            <div className="adjustment-indicator">
              <FontAwesomeIcon icon={faBullseye} />
              <span>
                Costs adjusted by <strong>{totalCostChange > 0 ? '+' : ''}{formatCurrency(totalCostChange)}</strong>/mo
              </span>
            </div>
          )}
        </div>
        
        <div className="scenarios-grid">
          {scenarios.map((forecast, idx) => (
            <ScenarioCard
              key={idx}
              forecast={forecast}
              isSelected={selectedScenario === idx}
              onSelect={() => setSelectedScenario(idx)}
            />
          ))}
        </div>
      </div>

      {/* Selected Scenario Forecast */}
      {selectedForecast && (
        <div className="section">
          <div className="section-header-row">
            <h2>{selectedForecast.scenario.name} Scenario - 12 Month Forecast</h2>
            {hasAdjustments && (
              <span className="forecast-adjusted-badge">
                <FontAwesomeIcon icon={faCheckCircle} /> Live-adjusted
              </span>
            )}
          </div>
          <div className="forecast-table-container">
            <table className="forecast-table">
              <thead>
                <tr>
                  <th>Month</th>
                  <th>Revenue</th>
                  <th className={hasAdjustments ? 'adjusted-header' : ''}>
                    Costs {hasAdjustments && <span className="header-note">(adjusted)</span>}
                  </th>
                  <th className={hasAdjustments ? 'adjusted-header' : ''}>Net Profit</th>
                  <th className={hasAdjustments ? 'adjusted-header' : ''}>Margin</th>
                  <th>Status</th>
                </tr>
              </thead>
              <tbody>
                {selectedForecast.months.map((month, idx) => (
                  <tr key={idx} className={month.is_actual ? 'actual' : ''}>
                    <td>
                      {month.month_name.split(' ')[0]}
                      {month.is_actual && <span className="actual-badge">Actual</span>}
                    </td>
                    <td className="revenue">{formatCurrency(month.gross_revenue)}</td>
                    <td className="cost">{formatCurrency(month.total_costs)}</td>
                    <td className={month.net_profit >= 0 ? 'profit positive' : 'profit negative'}>
                      {formatCurrency(month.net_profit)}
                    </td>
                    <td className={month.net_margin >= 0 ? 'positive' : 'negative'}>
                      {month.net_margin.toFixed(1)}%
                    </td>
                    <td>
                      {month.net_profit >= 0 ? (
                        <span className="status profitable"><FontAwesomeIcon icon={faCheckCircle} /></span>
                      ) : (
                        <span className="status loss"><FontAwesomeIcon icon={faExclamationTriangle} /></span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Cost Editor */}
      <div className="section">
        <div className="section-header-with-actions">
          <div>
            <h2>Cost Editor</h2>
            <p className="section-desc">Adjust costs to see how changes affect profitability. Changes are saved permanently to your configuration.</p>
          </div>
          <div className="header-actions">
            {costOverrides.length > 0 && (
              <button className="btn-secondary" onClick={handleResetAll}>
                <FontAwesomeIcon icon={faUndo} /> Undo All Changes
              </button>
            )}
            <button 
              className="btn-save-permanent" 
              onClick={saveConfigsPermanently}
              disabled={isSaving || !hasUnsavedChanges}
            >
              {isSaving ? (
                <>Saving...</>
              ) : (
                <><FontAwesomeIcon icon={faSave} /> Save Changes</>
              )}
            </button>
            <button className="btn-danger" onClick={resetAllConfigs}>
              <FontAwesomeIcon icon={faUndo} /> Reset to Defaults
            </button>
          </div>
        </div>
        
        {saveMessage && (
          <div className={`save-message ${saveMessage.type}`}>
            {saveMessage.type === 'success' ? <FontAwesomeIcon icon={faCheckCircle} /> : <FontAwesomeIcon icon={faExclamationTriangle} />}
            {saveMessage.text}
          </div>
        )}

        <div className="cost-editor">
          <CostSection
            title="ESP Costs"
            items={cost_breakdown.esp_costs}
            overrides={costOverrides}
            onOverride={handleOverride}
            onReset={handleReset}
          />
          <CostSection
            title="Vendor/SaaS"
            items={cost_breakdown.vendor_costs}
            overrides={costOverrides}
            onOverride={handleOverride}
            onReset={handleReset}
          />
          <CostSection
            title="Payroll"
            items={cost_breakdown.payroll_costs}
            overrides={costOverrides}
            onOverride={handleOverride}
            onReset={handleReset}
          />
          {cost_breakdown.revenue_share_costs.length > 0 && (
            <CostSection
              title="Revenue Share"
              items={cost_breakdown.revenue_share_costs}
              overrides={costOverrides}
              onOverride={handleOverride}
              onReset={handleReset}
            />
          )}
        </div>

        <div className="total-cost-summary">
          <div className="total-label">
            <span>Total Monthly Costs</span>
            {hasUnsavedChanges && <span className="unsaved-indicator">Unsaved changes</span>}
          </div>
          <div className="total-values">
            <span className={`total-value ${totalCostChange !== 0 ? 'modified' : ''}`}>
              {formatCurrency(currentTotalCost)}
            </span>
            {totalCostChange !== 0 && (
              <span className={`total-change ${totalCostChange > 0 ? 'increase' : 'decrease'}`}>
                ({totalCostChange > 0 ? '+' : ''}{formatCurrency(totalCostChange)})
              </span>
            )}
          </div>
        </div>
      </div>

      {/* Recommendations */}
      {recommendations && recommendations.length > 0 && (
        <div className="section recommendations">
          <h2><FontAwesomeIcon icon={faLightbulb} /> Data-Driven Recommendations</h2>
          <ul>
            {recommendations.map((rec, idx) => (
              <li key={idx}>
                <FontAwesomeIcon icon={faBullseye} />
                <span>{rec}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      <style>{`
        .scenario-planning {
          padding: 1rem;
          max-width: 1600px;
          margin: 0 auto;
        }

        .section {
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 12px;
          padding: 1.5rem;
          margin-bottom: 1.5rem;
          border: 1px solid var(--border-color, #333);
        }

        .section h2 {
          margin: 0 0 0.25rem 0;
          font-size: 1.125rem;
          display: flex;
          align-items: center;
          gap: 0.5rem;
        }

        .section-desc {
          margin: 0 0 1.25rem 0;
          color: var(--text-muted);
          font-size: 0.875rem;
        }

        .section-header-with-actions {
          display: flex;
          justify-content: space-between;
          align-items: flex-start;
          margin-bottom: 1rem;
        }

        .section-header-row {
          display: flex;
          justify-content: space-between;
          align-items: flex-start;
          margin-bottom: 1rem;
        }

        .section-header-row h2 {
          margin-bottom: 0.25rem;
        }

        .adjustment-indicator {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          padding: 0.5rem 0.75rem;
          background: rgba(250, 204, 21, 0.12);
          border: 1px solid rgba(250, 204, 21, 0.3);
          border-radius: 6px;
          color: var(--accent-yellow, #facc15);
          font-size: 0.8125rem;
        }

        .adjustment-indicator strong {
          color: var(--accent-yellow, #facc15);
        }

        .forecast-adjusted-badge {
          display: flex;
          align-items: center;
          gap: 0.375rem;
          padding: 0.25rem 0.5rem;
          background: rgba(34, 197, 94, 0.12);
          border: 1px solid rgba(34, 197, 94, 0.3);
          border-radius: 4px;
          color: var(--accent-green, #22c55e);
          font-size: 0.75rem;
          font-weight: 500;
        }

        .adjusted-header {
          color: var(--accent-yellow, #facc15) !important;
        }

        .header-note {
          font-size: 0.625rem;
          font-weight: 400;
          opacity: 0.8;
        }

        .header-actions {
          display: flex;
          gap: 0.5rem;
        }

        .btn-primary, .btn-secondary {
          display: flex;
          align-items: center;
          gap: 0.375rem;
          padding: 0.5rem 1rem;
          border-radius: 6px;
          font-size: 0.8125rem;
          cursor: pointer;
          border: none;
        }

        .btn-primary {
          background: var(--accent-blue, #3b82f6);
          color: white;
        }

        .btn-secondary {
          background: var(--bg-tertiary, #2a2a3e);
          color: var(--text-primary);
          border: 1px solid var(--border-color, #333);
        }

        .btn-save-permanent {
          display: flex;
          align-items: center;
          gap: 0.375rem;
          padding: 0.5rem 1rem;
          border-radius: 6px;
          font-size: 0.8125rem;
          cursor: pointer;
          border: none;
          background: var(--accent-green, #22c55e);
          color: white;
        }

        .btn-save-permanent:disabled {
          opacity: 0.5;
          cursor: not-allowed;
        }

        .btn-danger {
          display: flex;
          align-items: center;
          gap: 0.375rem;
          padding: 0.5rem 1rem;
          border-radius: 6px;
          font-size: 0.8125rem;
          cursor: pointer;
          border: none;
          background: rgba(239, 68, 68, 0.15);
          color: var(--accent-red, #ef4444);
          border: 1px solid var(--accent-red, #ef4444);
        }

        .btn-danger:hover {
          background: rgba(239, 68, 68, 0.25);
        }

        .save-message {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          padding: 0.75rem 1rem;
          border-radius: 8px;
          margin-bottom: 1rem;
          font-size: 0.875rem;
        }

        .save-message.success {
          background: rgba(34, 197, 94, 0.15);
          color: var(--accent-green, #22c55e);
          border: 1px solid var(--accent-green, #22c55e);
        }

        .save-message.error {
          background: rgba(239, 68, 68, 0.15);
          color: var(--accent-red, #ef4444);
          border: 1px solid var(--accent-red, #ef4444);
        }

        /* Drivers Grid */
        .drivers-grid {
          display: grid;
          grid-template-columns: repeat(4, 1fr);
          gap: 1rem;
          margin-bottom: 1.5rem;
        }

        .driver-card {
          background: var(--bg-tertiary, #2a2a3e);
          padding: 1rem;
          border-radius: 8px;
          text-align: center;
        }

        .driver-label {
          font-size: 0.75rem;
          color: var(--text-muted);
          margin-bottom: 0.375rem;
        }

        .driver-value {
          font-size: 1.25rem;
          font-weight: 600;
        }

        /* ESP Breakdown */
        .esp-breakdown h4 {
          font-size: 0.875rem;
          margin: 0 0 1rem 0;
          color: var(--text-muted);
        }

        .esp-bar-row {
          display: grid;
          grid-template-columns: 100px 1fr 180px;
          gap: 1rem;
          align-items: center;
          margin-bottom: 0.75rem;
        }

        .esp-name {
          font-weight: 500;
          font-size: 0.875rem;
        }

        .esp-bar-container {
          position: relative;
          height: 24px;
          background: var(--bg-tertiary, #2a2a3e);
          border-radius: 4px;
          overflow: hidden;
        }

        .esp-bar {
          height: 100%;
          background: linear-gradient(90deg, var(--accent-blue, #3b82f6), var(--accent-green, #22c55e));
          border-radius: 4px;
        }

        .esp-value {
          position: absolute;
          right: 0.5rem;
          top: 50%;
          transform: translateY(-50%);
          font-size: 0.75rem;
          font-weight: 500;
        }

        .esp-details {
          display: flex;
          gap: 1rem;
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        /* Scenarios Grid */
        .scenarios-grid {
          display: grid;
          grid-template-columns: repeat(4, 1fr);
          gap: 1rem;
        }

        .scenario-card {
          background: var(--bg-tertiary, #2a2a3e);
          border-radius: 8px;
          padding: 1.25rem;
          cursor: pointer;
          border: 2px solid transparent;
          transition: all 0.2s;
          position: relative;
        }

        .scenario-card:hover {
          border-color: var(--accent-blue, #3b82f6);
        }

        .scenario-card.selected {
          border-color: var(--accent-blue, #3b82f6);
          background: rgba(59, 130, 246, 0.1);
        }

        .scenario-card.default {
          border-color: var(--accent-green, #22c55e);
        }

        .default-badge {
          position: absolute;
          top: -8px;
          right: 12px;
          background: var(--accent-green, #22c55e);
          color: white;
          font-size: 0.625rem;
          padding: 0.125rem 0.5rem;
          border-radius: 4px;
          font-weight: 600;
        }

        .scenario-card h4 {
          margin: 0 0 0.5rem 0;
          font-size: 1rem;
        }

        .scenario-desc {
          font-size: 0.75rem;
          color: var(--text-muted);
          margin: 0 0 1rem 0;
          line-height: 1.4;
        }

        .scenario-metrics {
          display: grid;
          grid-template-columns: 1fr 1fr;
          gap: 0.75rem;
          margin-bottom: 1rem;
        }

        .scenario-metrics .metric {
          display: flex;
          flex-direction: column;
        }

        .scenario-metrics .label {
          font-size: 0.625rem;
          color: var(--text-muted);
          text-transform: uppercase;
        }

        .scenario-metrics .value {
          font-size: 0.9375rem;
          font-weight: 600;
        }

        .scenario-metrics .value.positive {
          color: var(--accent-green, #22c55e);
        }

        .scenario-metrics .value.negative {
          color: var(--accent-red, #ef4444);
        }

        .growth-rates {
          display: flex;
          gap: 0.75rem;
          padding-top: 0.75rem;
          border-top: 1px solid var(--border-color, #333);
        }

        .growth-rates .rate {
          display: flex;
          align-items: center;
          gap: 0.25rem;
          font-size: 0.6875rem;
          color: var(--text-muted);
        }

        /* Forecast Table */
        .forecast-table-container {
          overflow-x: auto;
        }

        .forecast-table {
          width: 100%;
          border-collapse: collapse;
        }

        .forecast-table th,
        .forecast-table td {
          padding: 0.625rem 1rem;
          text-align: right;
          border-bottom: 1px solid var(--border-color, #333);
          font-size: 0.8125rem;
        }

        .forecast-table th:first-child,
        .forecast-table td:first-child {
          text-align: left;
        }

        .forecast-table th {
          font-weight: 600;
          font-size: 0.6875rem;
          text-transform: uppercase;
          color: var(--text-muted);
        }

        .forecast-table tr.actual {
          background: rgba(34, 197, 94, 0.08);
        }

        .actual-badge {
          background: var(--accent-green, #22c55e);
          color: white;
          font-size: 0.5625rem;
          padding: 0.125rem 0.375rem;
          border-radius: 4px;
          margin-left: 0.5rem;
        }

        .forecast-table .revenue {
          color: var(--accent-green, #22c55e);
        }

        .forecast-table .cost {
          color: var(--accent-orange, #f59e0b);
        }

        .forecast-table .positive {
          color: var(--accent-green, #22c55e);
        }

        .forecast-table .negative {
          color: var(--accent-red, #ef4444);
        }

        .status {
          display: inline-flex;
        }

        .status.profitable {
          color: var(--accent-green, #22c55e);
        }

        .status.loss {
          color: var(--accent-red, #ef4444);
        }

        /* Cost Editor */
        .cost-editor {
          display: flex;
          flex-direction: column;
          gap: 0.5rem;
        }

        .cost-section {
          border: 1px solid var(--border-color, #333);
          border-radius: 8px;
          overflow: hidden;
        }

        .cost-section .section-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          padding: 0.75rem 1rem;
          background: var(--bg-tertiary, #2a2a3e);
          cursor: pointer;
        }

        .section-title {
          font-weight: 500;
        }

        .section-total {
          font-weight: 600;
        }

        .section-total.modified {
          color: var(--accent-orange, #f59e0b);
        }

        .section-total .change {
          font-size: 0.75rem;
          margin-left: 0.5rem;
          opacity: 0.8;
        }

        .cost-table {
          width: 100%;
          border-collapse: collapse;
        }

        .cost-table td {
          padding: 0.5rem 1rem;
          border-bottom: 1px solid var(--border-color, #333);
          font-size: 0.8125rem;
        }

        .cost-table tr:last-child td {
          border-bottom: none;
        }

        .cost-table tr.modified {
          background: rgba(249, 115, 22, 0.08);
        }

        .cost-table .name {
          font-weight: 500;
        }

        .cost-table .category {
          color: var(--text-muted);
          font-size: 0.75rem;
        }

        .cost-table .cost {
          text-align: right;
        }

        .cost-table .actions {
          width: 60px;
          text-align: right;
        }

        .modified-value .original {
          font-size: 0.6875rem;
          color: var(--text-muted);
          text-decoration: line-through;
          margin-left: 0.5rem;
        }

        .edit-input {
          display: flex;
          align-items: center;
          gap: 0.25rem;
          justify-content: flex-end;
        }

        .edit-input input {
          width: 100px;
          padding: 0.25rem 0.5rem;
          border: 1px solid var(--border-color, #333);
          border-radius: 4px;
          background: var(--bg-primary, #12121a);
          color: var(--text-primary);
          text-align: right;
        }

        .edit-btn, .save-btn, .reset-btn {
          background: none;
          border: none;
          padding: 0.25rem;
          cursor: pointer;
          color: var(--text-muted);
          border-radius: 4px;
        }

        .edit-btn:hover, .save-btn:hover {
          color: var(--accent-blue, #3b82f6);
          background: rgba(59, 130, 246, 0.1);
        }

        .reset-btn:hover {
          color: var(--accent-orange, #f59e0b);
          background: rgba(249, 115, 22, 0.1);
        }

        .total-cost-summary {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-top: 1rem;
          padding: 1rem;
          border-top: 2px solid var(--border-color, #333);
          font-weight: 600;
          background: var(--bg-tertiary, #2a2a3e);
          border-radius: 0 0 8px 8px;
        }

        .total-label {
          display: flex;
          align-items: center;
          gap: 0.75rem;
        }

        .unsaved-indicator {
          font-size: 0.75rem;
          color: var(--accent-yellow, #facc15);
          font-weight: 500;
          padding: 0.125rem 0.5rem;
          background: rgba(250, 204, 21, 0.15);
          border-radius: 4px;
        }

        .total-values {
          display: flex;
          align-items: baseline;
          gap: 0.5rem;
        }

        .total-value {
          font-size: 1.5rem;
          color: var(--accent-orange, #f59e0b);
          transition: color 0.2s ease;
        }

        .total-value.modified {
          color: var(--accent-yellow, #facc15);
        }

        .total-change {
          font-size: 0.875rem;
          font-weight: 500;
          transition: opacity 0.2s ease;
        }

        .total-change.increase {
          color: var(--accent-red, #ef4444);
        }

        .total-change.decrease {
          color: var(--accent-green, #22c55e);
        }

        /* Smooth transitions for cost values */
        .cost-table .cost,
        .section-total,
        .modified-value {
          transition: color 0.15s ease;
        }

        /* Recommendations */
        .recommendations ul {
          list-style: none;
          padding: 0;
          margin: 0;
        }

        .recommendations li {
          display: flex;
          align-items: flex-start;
          gap: 0.75rem;
          padding: 0.75rem 0;
          border-bottom: 1px solid var(--border-color, #333);
          font-size: 0.875rem;
        }

        .recommendations li:last-child {
          border-bottom: none;
        }

        .recommendations li svg {
          flex-shrink: 0;
          color: var(--accent-blue, #3b82f6);
          margin-top: 0.125rem;
        }

        .error-card {
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 12px;
          padding: 2rem;
          border: 1px solid var(--accent-red, #ef4444);
          text-align: center;
          color: var(--accent-red, #ef4444);
        }

        /* Historical Cost Chart */
        .cost-trend-chart {
          margin-bottom: 1.5rem;
        }

        .chart-area {
          display: flex;
          align-items: flex-end;
          gap: 0.5rem;
          height: 200px;
          padding: 1rem;
          background: var(--bg-tertiary, #2a2a3e);
          border-radius: 8px;
          margin-bottom: 0.75rem;
        }

        .chart-bar-container {
          flex: 1;
          display: flex;
          flex-direction: column;
          align-items: center;
          height: 100%;
        }

        .chart-bar {
          width: 100%;
          max-width: 40px;
          background: linear-gradient(180deg, var(--accent-orange, #f59e0b) 0%, var(--accent-red, #ef4444) 100%);
          border-radius: 4px 4px 0 0;
          display: flex;
          justify-content: center;
          align-items: flex-start;
          position: relative;
          margin-top: auto;
          transition: height 0.3s ease;
        }

        .bar-value {
          position: absolute;
          top: -20px;
          font-size: 0.625rem;
          font-weight: 600;
          white-space: nowrap;
        }

        .bar-label {
          margin-top: 0.5rem;
          font-size: 0.6875rem;
          color: var(--text-muted);
        }

        .chart-legend {
          display: flex;
          gap: 1.5rem;
          justify-content: center;
        }

        .legend-item {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          font-size: 0.75rem;
        }

        .legend-marker {
          width: 12px;
          height: 12px;
          border-radius: 3px;
        }

        .legend-marker.current {
          background: var(--accent-orange, #f59e0b);
        }

        .legend-marker.target {
          background: var(--accent-green, #22c55e);
        }

        /* Cost Trajectory */
        .cost-trajectory h4 {
          margin: 0 0 1rem 0;
          font-size: 0.875rem;
        }

        .trajectory-steps {
          display: flex;
          align-items: center;
          justify-content: space-between;
          gap: 0.5rem;
          padding: 1rem;
          background: var(--bg-tertiary, #2a2a3e);
          border-radius: 8px;
          overflow-x: auto;
        }

        .step {
          text-align: center;
          padding: 0.75rem 1rem;
          background: var(--bg-secondary, #1e1e2e);
          border-radius: 8px;
          min-width: 100px;
        }

        .step.target-step {
          background: rgba(34, 197, 94, 0.15);
          border: 1px solid var(--accent-green, #22c55e);
        }

        .step-month {
          font-size: 0.6875rem;
          color: var(--text-muted);
          margin-bottom: 0.25rem;
        }

        .step-cost {
          font-size: 1rem;
          font-weight: 600;
        }

        .target-step .step-cost {
          color: var(--accent-green, #22c55e);
        }

        .step-label {
          font-size: 0.625rem;
          color: var(--text-muted);
          margin-top: 0.25rem;
        }

        .step-arrow {
          color: var(--text-muted);
          font-size: 1.25rem;
        }

        @media (max-width: 1200px) {
          .scenarios-grid {
            grid-template-columns: repeat(2, 1fr);
          }
          .drivers-grid {
            grid-template-columns: repeat(2, 1fr);
          }
        }

        @media (max-width: 768px) {
          .scenarios-grid {
            grid-template-columns: 1fr;
          }
          .drivers-grid {
            grid-template-columns: 1fr;
          }
          .esp-bar-row {
            grid-template-columns: 1fr;
            gap: 0.5rem;
          }
        }
      `}</style>
    </div>
  );
};

export default ScenarioPlanning;
