import React, { useState } from 'react';
import { useApi } from '../../hooks/useApi';
import { useDateFilter } from '../../context/DateFilterContext';
import { Loading, ErrorDisplay } from '../common/Loading';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBrain,
  faLightbulb,
  faClock,
  faBullseye,
  faBolt,
  faChartBar,
  faSync,
  faChevronRight,
  faTrophy,
  faExclamationCircle,
  faCheckCircle,
  faTimesCircle,
  faArrowUp,
  faArrowDown,
  faMinus,
} from '@fortawesome/free-solid-svg-icons';

// Types
interface Recommendation {
  id: string;
  category: string;
  title: string;
  description: string;
  impact_type: string;
  estimated_impact: number;
  confidence: number;
  priority: number;
  difficulty: string;
  time_to_implement: string;
  status: string;
  created_at: string;
}

interface PropertyOfferInsight {
  id: string;
  property_code: string;
  property_name: string;
  offer_id: string;
  offer_name: string;
  total_revenue: number;
  total_sent: number;
  conversions: number;
  ecpm: number;
  conversion_rate: number;
  trend: string;
  sample_size: number;
  confidence: number;
  recommendation: string;
}

interface TimingPattern {
  id: string;
  property_code: string;
  day_of_week: number;
  hour_of_day: number;
  avg_open_rate: number;
  avg_click_rate: number;
  avg_ecpm: number;
  campaign_count: number;
  performance_tier: string;
  recommendation: string;
  confidence: number;
}

interface ESPISPPerformance {
  esp: string;
  isp: string;
  delivery_rate: number;
  bounce_rate: number;
  open_rate: number;
  click_rate: number;
  score: number;
  is_recommended: boolean;
  confidence: number;
}

interface StrategyInsight {
  id: string;
  category: string;
  title: string;
  description: string;
  impact: string;
  impact_value: number;
  action_required: boolean;
  suggested_action: string;
  priority: number;
  confidence: number;
}

interface IntelligenceMemory {
  version: string;
  last_updated: string;
  total_cycles: number;
  data_points_processed: number;
  recommendations: Recommendation[];
  property_offer_insights: PropertyOfferInsight[];
  best_property_offer_pairs: any[];
  timing_patterns: TimingPattern[];
  optimal_send_times: Record<string, any[]>;
  esp_isp_matrix: ESPISPPerformance[];
  esp_recommendations: Record<string, string>;
  strategy_insights: StrategyInsight[];
  confidence_scores: Record<string, number>;
}

// Sub-tabs
type IntelligenceTab = 'overview' | 'property-offer' | 'timing' | 'esp-isp' | 'strategy';

const formatCurrency = (value: number): string => {
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value);
};

const formatPercent = (value: number, decimals: number = 1): string => {
  return `${(value * 100).toFixed(decimals)}%`;
};

const formatNumber = (value: number): string => {
  return new Intl.NumberFormat('en-US').format(value);
};

const getDayName = (day: number): string => {
  const days = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  return days[day] || 'Unknown';
};

const getPriorityBadge = (priority: number) => {
  if (priority === 1) return <span className="badge badge-danger">Critical</span>;
  if (priority === 2) return <span className="badge badge-warning">High</span>;
  if (priority === 3) return <span className="badge badge-info">Medium</span>;
  return <span className="badge badge-secondary">Low</span>;
};

const getConfidenceBadge = (confidence: number) => {
  if (confidence >= 0.8) return <span className="badge badge-success">{formatPercent(confidence, 0)} confident</span>;
  if (confidence >= 0.5) return <span className="badge badge-warning">{formatPercent(confidence, 0)} confident</span>;
  return <span className="badge badge-secondary">{formatPercent(confidence, 0)} confident</span>;
};

const getTrendIcon = (trend: string) => {
  if (trend === 'improving') return <FontAwesomeIcon icon={faArrowUp} className="text-success" style={{ fontSize: '14px' }} />;
  if (trend === 'declining') return <FontAwesomeIcon icon={faArrowDown} className="text-danger" style={{ fontSize: '14px' }} />;
  return <FontAwesomeIcon icon={faMinus} className="text-muted" style={{ fontSize: '14px' }} />;
};

const getPerformanceTierClass = (tier: string): string => {
  switch (tier) {
    case 'excellent': return 'tier-excellent';
    case 'good': return 'tier-good';
    case 'average': return 'tier-average';
    default: return 'tier-poor';
  }
};

// Components
const RecommendationCard: React.FC<{ rec: Recommendation; onStatusChange?: (id: string, status: string) => void }> = ({ rec, onStatusChange }) => {
  const getCategoryIcon = () => {
    switch (rec.category) {
      case 'property_offer': return <FontAwesomeIcon icon={faBullseye} style={{ fontSize: '16px' }} />;
      case 'timing': return <FontAwesomeIcon icon={faClock} style={{ fontSize: '16px' }} />;
      case 'audience': return <FontAwesomeIcon icon={faChartBar} style={{ fontSize: '16px' }} />;
      case 'esp': return <FontAwesomeIcon icon={faBolt} style={{ fontSize: '16px' }} />;
      default: return <FontAwesomeIcon icon={faLightbulb} style={{ fontSize: '16px' }} />;
    }
  };

  return (
    <div className={`recommendation-card priority-${rec.priority}`}>
      <div className="rec-header">
        <div className="rec-category">
          {getCategoryIcon()}
          <span>{rec.category.replace('_', ' ')}</span>
        </div>
        {getPriorityBadge(rec.priority)}
      </div>
      <h4 className="rec-title">{rec.title}</h4>
      <p className="rec-description">{rec.description}</p>
      <div className="rec-meta">
        <span className="rec-impact">
          Est. Impact: <strong>{rec.impact_type === 'revenue' ? formatCurrency(rec.estimated_impact) : `${rec.estimated_impact}%`}</strong>
        </span>
        {getConfidenceBadge(rec.confidence)}
      </div>
      <div className="rec-footer">
        <span className="rec-difficulty">{rec.difficulty} • {rec.time_to_implement}</span>
        {onStatusChange && rec.status === 'new' && (
          <div className="rec-actions">
            <button 
              className="btn-small btn-success"
              onClick={() => onStatusChange(rec.id, 'acknowledged')}
            >
              <FontAwesomeIcon icon={faCheckCircle} style={{ fontSize: '12px' }} /> Acknowledge
            </button>
            <button 
              className="btn-small btn-secondary"
              onClick={() => onStatusChange(rec.id, 'dismissed')}
            >
              <FontAwesomeIcon icon={faTimesCircle} style={{ fontSize: '12px' }} /> Dismiss
            </button>
          </div>
        )}
      </div>
    </div>
  );
};

const PropertyOfferTable: React.FC<{ insights: PropertyOfferInsight[] }> = ({ insights }) => {
  const topInsights = insights.slice(0, 20);

  return (
    <div className="table-container">
      <table className="table intelligence-table">
        <thead>
          <tr>
            <th>Property</th>
            <th>Offer</th>
            <th>Revenue</th>
            <th>eCPM</th>
            <th>Conv Rate</th>
            <th>Trend</th>
            <th>Confidence</th>
            <th>Recommendation</th>
          </tr>
        </thead>
        <tbody>
          {topInsights.map((insight) => (
            <tr key={insight.id}>
              <td>
                <strong>{insight.property_code}</strong>
                <br />
                <span className="text-muted">{insight.property_name}</span>
              </td>
              <td>
                <span>{insight.offer_name || insight.offer_id}</span>
              </td>
              <td className="text-right">{formatCurrency(insight.total_revenue)}</td>
              <td className="text-right">{formatCurrency(insight.ecpm)}</td>
              <td className="text-right">{formatPercent(insight.conversion_rate / 100)}</td>
              <td>{getTrendIcon(insight.trend)} {insight.trend}</td>
              <td>{getConfidenceBadge(insight.confidence)}</td>
              <td className="rec-cell">{insight.recommendation}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
};

const TimingHeatmap: React.FC<{ patterns: TimingPattern[] }> = ({ patterns }) => {
  // Build heatmap data: day x hour
  const heatmapData: Record<string, Record<number, TimingPattern>> = {};
  
  patterns.forEach((pattern) => {
    const day = getDayName(pattern.day_of_week);
    if (!heatmapData[day]) {
      heatmapData[day] = {};
    }
    heatmapData[day][pattern.hour_of_day] = pattern;
  });

  const days = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
  const hours = Array.from({ length: 24 }, (_, i) => i);

  return (
    <div className="timing-heatmap">
      <div className="heatmap-header">
        <div className="heatmap-label"></div>
        {hours.map((hour) => (
          <div key={hour} className="heatmap-hour">{hour}</div>
        ))}
      </div>
      {days.map((day) => (
        <div key={day} className="heatmap-row">
          <div className="heatmap-label">{day}</div>
          {hours.map((hour) => {
            const pattern = heatmapData[day]?.[hour];
            const tier = pattern?.performance_tier || 'none';
            return (
              <div 
                key={`${day}-${hour}`} 
                className={`heatmap-cell ${getPerformanceTierClass(tier)}`}
                title={pattern ? `${formatPercent(pattern.avg_open_rate)} open rate, ${pattern.campaign_count} campaigns` : 'No data'}
              />
            );
          })}
        </div>
      ))}
      <div className="heatmap-legend">
        <span className="legend-item"><span className="tier-excellent" /> Excellent</span>
        <span className="legend-item"><span className="tier-good" /> Good</span>
        <span className="legend-item"><span className="tier-average" /> Average</span>
        <span className="legend-item"><span className="tier-poor" /> Poor</span>
      </div>
    </div>
  );
};

const ESPISPMatrix: React.FC<{ matrix: ESPISPPerformance[]; recommendations: Record<string, string> }> = ({ matrix }) => {
  return (
    <div className="esp-isp-section">
      <div className="table-container">
        <table className="table intelligence-table">
          <thead>
            <tr>
              <th>ESP</th>
              <th>Delivery Rate</th>
              <th>Open Rate</th>
              <th>Click Rate</th>
              <th>Bounce Rate</th>
              <th>Score</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            {matrix.map((perf, idx) => (
              <tr key={idx} className={perf.is_recommended ? 'row-recommended' : ''}>
                <td>
                  <strong>{perf.esp}</strong>
                  {perf.is_recommended && <FontAwesomeIcon icon={faTrophy} className="recommended-badge" style={{ fontSize: '14px' }} />}
                </td>
                <td className="text-right">{formatPercent(perf.delivery_rate)}</td>
                <td className="text-right">{formatPercent(perf.open_rate)}</td>
                <td className="text-right">{formatPercent(perf.click_rate)}</td>
                <td className="text-right">{formatPercent(perf.bounce_rate)}</td>
                <td className="text-right">{perf.score.toFixed(1)}</td>
                <td>{perf.is_recommended ? <span className="badge badge-success">Recommended</span> : ''}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
};

const StrategyInsightCard: React.FC<{ insight: StrategyInsight }> = ({ insight }) => {
  return (
    <div className={`strategy-card impact-${insight.impact}`}>
      <div className="strategy-header">
        <span className="strategy-category">{insight.category}</span>
        {insight.action_required && <FontAwesomeIcon icon={faExclamationCircle} className="action-required" style={{ fontSize: '16px' }} />}
      </div>
      <h4>{insight.title}</h4>
      <p>{insight.description}</p>
      {insight.suggested_action && (
        <div className="strategy-action">
          <FontAwesomeIcon icon={faChevronRight} style={{ fontSize: '14px' }} />
          <span>{insight.suggested_action}</span>
        </div>
      )}
      <div className="strategy-meta">
        <span>Impact: <strong>{insight.impact}</strong></span>
        {insight.impact_value > 0 && <span>Est. Value: {formatCurrency(insight.impact_value)}</span>}
        {getConfidenceBadge(insight.confidence)}
      </div>
    </div>
  );
};

// Main Component
export const IntelligenceDashboard: React.FC = () => {
  const [activeTab, setActiveTab] = useState<IntelligenceTab>('overview');
  const [isLearning, setIsLearning] = useState(false);
  const { dateRange } = useDateFilter();

  const { data: memory, loading, error, refetch } = useApi<IntelligenceMemory>(`/api/intelligence/dashboard?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`, {
    pollingInterval: 60000, // Refresh every minute
  });

  const triggerLearning = async () => {
    setIsLearning(true);
    try {
      await fetch('/api/intelligence/learn', { method: 'POST' });
      setTimeout(() => {
        refetch();
        setIsLearning(false);
      }, 5000);
    } catch (err) {
      setIsLearning(false);
    }
  };

  const updateRecommendationStatus = async (id: string, status: string) => {
    try {
      await fetch(`/api/intelligence/recommendations/${id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status }),
      });
      refetch();
    } catch (err) {
      console.error('Failed to update recommendation:', err);
    }
  };

  if (loading && !memory) {
    return <Loading message="Loading intelligence data..." />;
  }

  if (error) {
    return <ErrorDisplay message={error} onRetry={refetch} />;
  }

  if (!memory) {
    return <ErrorDisplay message="No intelligence data available" />;
  }

  const recommendations = memory.recommendations || [];
  const propertyInsights = memory.property_offer_insights || [];
  const timingPatterns = memory.timing_patterns || [];
  const espMatrix = memory.esp_isp_matrix || [];
  const strategyInsights = memory.strategy_insights || [];

  const tabs: { id: IntelligenceTab; label: string; icon: React.ReactNode }[] = [
    { id: 'overview', label: 'Overview', icon: <FontAwesomeIcon icon={faBrain} style={{ fontSize: '14px' }} /> },
    { id: 'property-offer', label: 'Property-Offer', icon: <FontAwesomeIcon icon={faBullseye} style={{ fontSize: '14px' }} /> },
    { id: 'timing', label: 'Send Time', icon: <FontAwesomeIcon icon={faClock} style={{ fontSize: '14px' }} /> },
    { id: 'esp-isp', label: 'ESP Analysis', icon: <FontAwesomeIcon icon={faBolt} style={{ fontSize: '14px' }} /> },
    { id: 'strategy', label: 'Strategy', icon: <FontAwesomeIcon icon={faLightbulb} style={{ fontSize: '14px' }} /> },
  ];

  return (
    <div className="intelligence-dashboard">
      {/* Header */}
      <div className="dashboard-header">
        <div className="header-left">
          <h2><FontAwesomeIcon icon={faBrain} style={{ fontSize: '24px' }} /> Intelligence Hub</h2>
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginTop: '0.25rem' }}>
            <span className="subtitle">AI-powered learning and recommendations</span>
            <span className="badge badge-info">{dateRange.label}</span>
          </div>
        </div>
        <div className="header-right">
          <div className="learning-stats">
            <span>Cycles: <strong>{memory.total_cycles}</strong></span>
            <span>Data Points: <strong>{formatNumber(memory.data_points_processed)}</strong></span>
            <span>Last Updated: <strong>{new Date(memory.last_updated).toLocaleTimeString()}</strong></span>
          </div>
          <button 
            className={`btn-learn ${isLearning ? 'learning' : ''}`}
            onClick={triggerLearning}
            disabled={isLearning}
          >
            <FontAwesomeIcon icon={faSync} className={isLearning ? 'spinning' : ''} style={{ fontSize: '16px' }} />
            {isLearning ? 'Learning...' : 'Trigger Learning'}
          </button>
        </div>
      </div>

      {/* Tabs */}
      <div className="intelligence-tabs">
        {tabs.map((tab) => (
          <button
            key={tab.id}
            className={`tab-button ${activeTab === tab.id ? 'active' : ''}`}
            onClick={() => setActiveTab(tab.id)}
          >
            {tab.icon}
            {tab.label}
          </button>
        ))}
      </div>

      {/* Content */}
      <div className="intelligence-content">
        {activeTab === 'overview' && (
          <div className="overview-section">
            {/* Confidence Scores */}
            <div className="confidence-grid">
              {Object.entries(memory.confidence_scores || {}).map(([key, value]) => (
                <div key={key} className="confidence-card">
                  <span className="confidence-label">{key.replace('_', ' ')}</span>
                  <div className="confidence-bar">
                    <div 
                      className="confidence-fill" 
                      style={{ width: `${value * 100}%` }}
                    />
                  </div>
                  <span className="confidence-value">{formatPercent(value, 0)}</span>
                </div>
              ))}
            </div>

            {/* Top Recommendations */}
            <div className="section-header">
              <h3><FontAwesomeIcon icon={faLightbulb} style={{ fontSize: '18px' }} /> Top Recommendations</h3>
              <span className="count">{recommendations.length} active</span>
            </div>
            <div className="recommendations-grid">
              {recommendations.slice(0, 6).map((rec) => (
                <RecommendationCard 
                  key={rec.id} 
                  rec={rec} 
                  onStatusChange={updateRecommendationStatus}
                />
              ))}
            </div>

            {/* Quick Stats */}
            <div className="quick-stats">
              <div className="stat-card">
                <FontAwesomeIcon icon={faBullseye} style={{ fontSize: '20px' }} />
                <div className="stat-info">
                  <span className="stat-value">{propertyInsights.length}</span>
                  <span className="stat-label">Property-Offer Insights</span>
                </div>
              </div>
              <div className="stat-card">
                <FontAwesomeIcon icon={faClock} style={{ fontSize: '20px' }} />
                <div className="stat-info">
                  <span className="stat-value">{timingPatterns.length}</span>
                  <span className="stat-label">Timing Patterns</span>
                </div>
              </div>
              <div className="stat-card">
                <FontAwesomeIcon icon={faBolt} style={{ fontSize: '20px' }} />
                <div className="stat-info">
                  <span className="stat-value">{espMatrix.length}</span>
                  <span className="stat-label">ESP Analyzed</span>
                </div>
              </div>
              <div className="stat-card">
                <FontAwesomeIcon icon={faLightbulb} style={{ fontSize: '20px' }} />
                <div className="stat-info">
                  <span className="stat-value">{strategyInsights.length}</span>
                  <span className="stat-label">Strategy Insights</span>
                </div>
              </div>
            </div>
          </div>
        )}

        {activeTab === 'property-offer' && (
          <div className="property-offer-section">
            <div className="section-header">
              <h3><FontAwesomeIcon icon={faBullseye} style={{ fontSize: '18px' }} /> Property-Offer Performance Analysis</h3>
              <span className="subtitle">Which properties work best for which offers</span>
            </div>
            {propertyInsights.length > 0 ? (
              <PropertyOfferTable insights={propertyInsights} />
            ) : (
              <div className="empty-state">
                <FontAwesomeIcon icon={faBullseye} style={{ fontSize: '48px' }} />
                <p>No property-offer insights available yet. Run a learning cycle to generate insights.</p>
              </div>
            )}
          </div>
        )}

        {activeTab === 'timing' && (
          <div className="timing-section">
            <div className="section-header">
              <h3><FontAwesomeIcon icon={faClock} style={{ fontSize: '18px' }} /> Send Time Optimization</h3>
              <span className="subtitle">When to send for best engagement</span>
            </div>
            {timingPatterns.length > 0 ? (
              <>
                <TimingHeatmap patterns={timingPatterns} />
                <div className="timing-recommendations">
                  <h4>Optimal Send Windows</h4>
                  {Object.entries(memory.optimal_send_times || {}).slice(0, 5).map(([property, times]) => (
                    <div key={property} className="optimal-time-row">
                      <strong>{property}</strong>
                      <div className="time-badges">
                        {(times as any[]).map((time, idx) => (
                          <span key={idx} className="time-badge">
                            {time.day_of_week} {time.hour_range}
                          </span>
                        ))}
                      </div>
                    </div>
                  ))}
                </div>
              </>
            ) : (
              <div className="empty-state">
                <FontAwesomeIcon icon={faClock} style={{ fontSize: '48px' }} />
                <p>No timing patterns available yet. Run a learning cycle to analyze send time performance.</p>
              </div>
            )}
          </div>
        )}

        {activeTab === 'esp-isp' && (
          <div className="esp-section">
            <div className="section-header">
              <h3><FontAwesomeIcon icon={faBolt} style={{ fontSize: '18px' }} /> ESP Performance Analysis</h3>
              <span className="subtitle">Which ESP performs best for your sends</span>
            </div>
            {espMatrix.length > 0 ? (
              <>
                <ESPISPMatrix matrix={espMatrix} recommendations={memory.esp_recommendations || {}} />
                <div className="esp-recommendations">
                  <h4>ESP Recommendations</h4>
                  <div className="rec-list">
                    {Object.entries(memory.esp_recommendations || {}).map(([isp, esp]) => (
                      <div key={isp} className="rec-item">
                        <span className="isp-name">{isp}</span>
                        <FontAwesomeIcon icon={faChevronRight} style={{ fontSize: '14px' }} />
                        <span className="esp-name">{esp}</span>
                        <FontAwesomeIcon icon={faTrophy} className="recommended-icon" style={{ fontSize: '14px' }} />
                      </div>
                    ))}
                  </div>
                </div>
              </>
            ) : (
              <div className="empty-state">
                <FontAwesomeIcon icon={faBolt} style={{ fontSize: '48px' }} />
                <p>No ESP analysis available yet. Run a learning cycle to analyze ESP performance.</p>
              </div>
            )}
          </div>
        )}

        {activeTab === 'strategy' && (
          <div className="strategy-section">
            <div className="section-header">
              <h3><FontAwesomeIcon icon={faLightbulb} style={{ fontSize: '18px' }} /> Strategic Insights</h3>
              <span className="subtitle">High-level marketing strategy recommendations</span>
            </div>
            {strategyInsights.length > 0 ? (
              <div className="strategy-grid">
                {strategyInsights.map((insight) => (
                  <StrategyInsightCard key={insight.id} insight={insight} />
                ))}
              </div>
            ) : (
              <div className="empty-state">
                <FontAwesomeIcon icon={faLightbulb} style={{ fontSize: '48px' }} />
                <p>No strategic insights available yet. Run a learning cycle to generate insights.</p>
              </div>
            )}
          </div>
        )}
      </div>

      <style>{`
        .intelligence-dashboard {
          padding: 1.5rem;
        }

        .dashboard-header {
          display: flex;
          justify-content: space-between;
          align-items: flex-start;
          margin-bottom: 1.5rem;
          flex-wrap: wrap;
          gap: 1rem;
        }

        .header-left h2 {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          margin: 0;
          font-size: 1.5rem;
          color: var(--text-primary);
        }

        .header-left .subtitle {
          display: block;
          color: var(--text-muted);
          font-size: 0.875rem;
          margin-top: 0.25rem;
        }

        .header-right {
          display: flex;
          align-items: center;
          gap: 1rem;
        }

        .learning-stats {
          display: flex;
          gap: 1rem;
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        .learning-stats strong {
          color: var(--text-primary);
        }

        .btn-learn {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          padding: 0.5rem 1rem;
          background: var(--accent-blue);
          color: white;
          border: none;
          border-radius: 6px;
          cursor: pointer;
          font-size: 0.875rem;
          transition: all 0.2s;
        }

        .btn-learn:hover:not(:disabled) {
          background: #2563eb;
        }

        .btn-learn:disabled {
          opacity: 0.7;
          cursor: not-allowed;
        }

        .btn-learn.learning {
          background: var(--accent-green);
        }

        .spinning {
          animation: spin 1s linear infinite;
        }

        @keyframes spin {
          from { transform: rotate(0deg); }
          to { transform: rotate(360deg); }
        }

        .intelligence-tabs {
          display: flex;
          gap: 0.5rem;
          margin-bottom: 1.5rem;
          border-bottom: 1px solid var(--border-color);
          padding-bottom: 0.5rem;
          overflow-x: auto;
        }

        .tab-button {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          padding: 0.5rem 1rem;
          background: transparent;
          border: none;
          color: var(--text-muted);
          cursor: pointer;
          font-size: 0.875rem;
          border-radius: 6px;
          transition: all 0.2s;
          white-space: nowrap;
        }

        .tab-button:hover {
          background: var(--bg-tertiary);
          color: var(--text-primary);
        }

        .tab-button.active {
          background: var(--accent-blue);
          color: white;
        }

        .section-header {
          display: flex;
          align-items: center;
          justify-content: space-between;
          margin-bottom: 1rem;
        }

        .section-header h3 {
          display: flex;
          align-items: center;
          gap: 0.5rem;
          margin: 0;
          font-size: 1.125rem;
          color: var(--text-primary);
        }

        .section-header .subtitle {
          color: var(--text-muted);
          font-size: 0.75rem;
        }

        .section-header .count {
          color: var(--text-muted);
          font-size: 0.75rem;
        }

        /* Confidence Grid */
        .confidence-grid {
          display: grid;
          grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
          gap: 1rem;
          margin-bottom: 2rem;
        }

        .confidence-card {
          background: var(--bg-secondary);
          border: 1px solid var(--border-color);
          border-radius: 8px;
          padding: 1rem;
        }

        .confidence-label {
          display: block;
          font-size: 0.75rem;
          color: var(--text-muted);
          text-transform: capitalize;
          margin-bottom: 0.5rem;
        }

        .confidence-bar {
          height: 6px;
          background: var(--bg-tertiary);
          border-radius: 3px;
          overflow: hidden;
          margin-bottom: 0.25rem;
        }

        .confidence-fill {
          height: 100%;
          background: var(--accent-green);
          border-radius: 3px;
          transition: width 0.3s ease;
        }

        .confidence-value {
          font-size: 0.875rem;
          font-weight: 600;
          color: var(--text-primary);
        }

        /* Recommendations */
        .recommendations-grid {
          display: grid;
          grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
          gap: 1rem;
          margin-bottom: 2rem;
        }

        .recommendation-card {
          background: var(--bg-secondary);
          border: 1px solid var(--border-color);
          border-radius: 8px;
          padding: 1rem;
          border-left: 4px solid var(--accent-blue);
        }

        .recommendation-card.priority-1 {
          border-left-color: var(--accent-red);
        }

        .recommendation-card.priority-2 {
          border-left-color: var(--accent-yellow);
        }

        .rec-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 0.75rem;
        }

        .rec-category {
          display: flex;
          align-items: center;
          gap: 0.375rem;
          font-size: 0.75rem;
          color: var(--text-muted);
          text-transform: capitalize;
        }

        .rec-title {
          margin: 0 0 0.5rem 0;
          font-size: 1rem;
          color: var(--text-primary);
        }

        .rec-description {
          margin: 0 0 0.75rem 0;
          font-size: 0.875rem;
          color: var(--text-secondary);
          line-height: 1.4;
        }

        .rec-meta {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 0.75rem;
        }

        .rec-impact {
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        .rec-footer {
          display: flex;
          justify-content: space-between;
          align-items: center;
          padding-top: 0.75rem;
          border-top: 1px solid var(--border-color);
        }

        .rec-difficulty {
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        .rec-actions {
          display: flex;
          gap: 0.5rem;
        }

        .btn-small {
          display: flex;
          align-items: center;
          gap: 0.25rem;
          padding: 0.25rem 0.5rem;
          font-size: 0.75rem;
          border: none;
          border-radius: 4px;
          cursor: pointer;
        }

        .btn-small.btn-success {
          background: var(--accent-green);
          color: white;
        }

        .btn-small.btn-secondary {
          background: var(--bg-tertiary);
          color: var(--text-muted);
        }

        /* Quick Stats */
        .quick-stats {
          display: grid;
          grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
          gap: 1rem;
        }

        .stat-card {
          display: flex;
          align-items: center;
          gap: 1rem;
          background: var(--bg-secondary);
          border: 1px solid var(--border-color);
          border-radius: 8px;
          padding: 1rem;
        }

        .stat-card svg {
          color: var(--accent-blue);
        }

        .stat-info {
          display: flex;
          flex-direction: column;
        }

        .stat-value {
          font-size: 1.25rem;
          font-weight: 600;
          color: var(--text-primary);
        }

        .stat-label {
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        /* Badges */
        .badge {
          display: inline-flex;
          align-items: center;
          padding: 0.25rem 0.5rem;
          font-size: 0.625rem;
          font-weight: 600;
          border-radius: 4px;
          text-transform: uppercase;
        }

        .badge-danger {
          background: rgba(239, 68, 68, 0.2);
          color: var(--accent-red);
        }

        .badge-warning {
          background: rgba(245, 158, 11, 0.2);
          color: var(--accent-yellow);
        }

        .badge-info {
          background: rgba(59, 130, 246, 0.2);
          color: var(--accent-blue);
        }

        .badge-success {
          background: rgba(34, 197, 94, 0.2);
          color: var(--accent-green);
        }

        .badge-secondary {
          background: var(--bg-tertiary);
          color: var(--text-muted);
        }

        /* Timing Heatmap */
        .timing-heatmap {
          margin-bottom: 2rem;
        }

        .heatmap-header, .heatmap-row {
          display: flex;
          gap: 2px;
        }

        .heatmap-label {
          width: 40px;
          font-size: 0.625rem;
          color: var(--text-muted);
          display: flex;
          align-items: center;
        }

        .heatmap-hour {
          width: 20px;
          text-align: center;
          font-size: 0.5rem;
          color: var(--text-muted);
        }

        .heatmap-cell {
          width: 20px;
          height: 20px;
          border-radius: 2px;
          background: var(--bg-tertiary);
        }

        .tier-excellent { background: var(--accent-green); }
        .tier-good { background: #22c55e80; }
        .tier-average { background: var(--accent-yellow); }
        .tier-poor { background: var(--accent-red); opacity: 0.6; }

        .heatmap-legend {
          display: flex;
          gap: 1rem;
          margin-top: 1rem;
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        .legend-item {
          display: flex;
          align-items: center;
          gap: 0.375rem;
        }

        .legend-item span:first-child {
          width: 12px;
          height: 12px;
          border-radius: 2px;
        }

        /* Tables */
        .intelligence-table {
          font-size: 0.875rem;
        }

        .intelligence-table .rec-cell {
          max-width: 200px;
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        .text-right {
          text-align: right;
        }

        .text-success { color: var(--accent-green); }
        .text-danger { color: var(--accent-red); }
        .text-muted { color: var(--text-muted); }

        .row-recommended {
          background: rgba(34, 197, 94, 0.1);
        }

        .recommended-badge {
          margin-left: 0.5rem;
          color: var(--accent-green);
        }

        /* Strategy Cards */
        .strategy-grid {
          display: grid;
          grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
          gap: 1rem;
        }

        .strategy-card {
          background: var(--bg-secondary);
          border: 1px solid var(--border-color);
          border-radius: 8px;
          padding: 1rem;
          border-left: 4px solid var(--accent-blue);
        }

        .strategy-card.impact-high {
          border-left-color: var(--accent-red);
        }

        .strategy-card.impact-medium {
          border-left-color: var(--accent-yellow);
        }

        .strategy-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 0.5rem;
        }

        .strategy-category {
          font-size: 0.625rem;
          color: var(--text-muted);
          text-transform: uppercase;
        }

        .action-required {
          color: var(--accent-yellow);
        }

        .strategy-card h4 {
          margin: 0 0 0.5rem 0;
          font-size: 1rem;
          color: var(--text-primary);
        }

        .strategy-card p {
          margin: 0 0 0.75rem 0;
          font-size: 0.875rem;
          color: var(--text-secondary);
        }

        .strategy-action {
          display: flex;
          align-items: center;
          gap: 0.375rem;
          padding: 0.5rem;
          background: var(--bg-tertiary);
          border-radius: 4px;
          font-size: 0.75rem;
          color: var(--accent-blue);
          margin-bottom: 0.75rem;
        }

        .strategy-meta {
          display: flex;
          gap: 1rem;
          font-size: 0.75rem;
          color: var(--text-muted);
        }

        /* Empty States */
        .empty-state {
          display: flex;
          flex-direction: column;
          align-items: center;
          justify-content: center;
          padding: 3rem;
          color: var(--text-muted);
          text-align: center;
        }

        .empty-state svg {
          margin-bottom: 1rem;
          opacity: 0.5;
        }

        .empty-state p {
          max-width: 300px;
        }

        /* Responsive */
        @media (max-width: 768px) {
          .dashboard-header {
            flex-direction: column;
          }

          .header-right {
            flex-direction: column;
            align-items: flex-start;
          }

          .recommendations-grid {
            grid-template-columns: 1fr;
          }
        }
      `}</style>
    </div>
  );
};

export default IntelligenceDashboard;
