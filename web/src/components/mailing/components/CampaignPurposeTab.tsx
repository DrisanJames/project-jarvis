import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBullseye,
  faDollarSign,
  faBolt,
  faChartLine,
  faShieldAlt,
  faMagic,
  faChevronDown,
  faChevronUp,
  faRocket,
  faCog,
  faSpinner,
  faSearch,
  faStar,
  faArrowUp,
  faArrowDown,
  faMinus,
  faCheckCircle,
  faLightbulb,
  faSync,
  faGlobe,
  faUsers,
  faFire,
  faCrosshairs,
  faEnvelope,
  faSnowflake,
  faServer,
} from '@fortawesome/free-solid-svg-icons';
import './CampaignPurposeTab.css';

// ============================================================================
// TYPES
// ============================================================================

export type CampaignPurpose = 'data_activation' | 'offer_revenue';
export type OfferModel = 'cpm' | 'cpl' | 'cpa' | 'hybrid';
export type TargetMetric = 'clicks' | 'conversions' | 'revenue' | 'ecpm';
export type ActivationGoal = 'warm_new_data' | 'reactivate_cold' | 'domain_warmup';
export type RotationStrategy = 'performance' | 'round_robin' | 'weighted' | 'explore';
export type ThroughputSensitivity = 'low' | 'medium' | 'high';
export type PacingStrategy = 'aggressive' | 'even' | 'conservative';

// Everflow Types
export interface EverflowAffiliate {
  id: string;
  name: string;
  description?: string;
}

export interface EverflowOffer {
  offer_id: string;
  offer_name: string;
  offer_type: string;
  advertiser_name?: string;
  clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  conversion_rate: number;
  epc: number;
  today_clicks: number;
  today_conversions: number;
  today_revenue: number;
  revenue_trend: 'up' | 'down' | 'stable';
  trend_percentage: number;
  ai_score: number;
  ai_recommendation: 'highly_recommended' | 'recommended' | 'neutral' | 'caution';
  ai_reason: string;
}

// Network-wide offer intelligence (from background worker)
export interface NetworkOffer {
  offer_id: string;
  offer_name: string;
  offer_type: string;
  advertiser_name?: string;
  network_clicks: number;
  network_conversions: number;
  network_revenue: number;
  network_cvr: number;
  network_epc: number;
  today_clicks: number;
  today_conversions: number;
  today_revenue: number;
  today_epc: number;
  revenue_trend: string;
  trend_percentage: number;
  hourly_velocity: number;
  audience_profile?: AudienceProfile;
  ai_score: number;
  ai_recommendation: string;
  ai_reason: string;
  network_rank: number;
}

export interface AudienceProfile {
  inbox_distribution: Record<string, number>;
  primary_inbox: string;
  primary_inbox_pct: number;
  browser_distribution: Record<string, number>;
  chromium_percentage: number;
  device_distribution: Record<string, number>;
  primary_device: string;
  os_distribution: Record<string, number>;
  top_countries: Array<{ name: string; count: number; percentage: number }>;
  peak_conversion_hours: number[];
  best_day_of_week: string;
  peak_hour_utc: number;
  total_samples: number;
}

export interface AudienceRecommendation {
  offer_id: string;
  offer_name: string;
  offer_type: string;
  target_audience: string;
  target_isp?: string;
  target_segment_hint: string;
  estimated_audience: number;
  match_reasons: string[];
  match_score: number;
  predicted_cvr: number;
  predicted_epc: number;
  predicted_revenue: number;
  confidence_level: number;
  recommended_strategy: string;
  send_time_hint?: string;
  creative_hints: string[];
}

export interface CampaignObjective {
  purpose: CampaignPurpose;
  
  // Data Activation Settings
  activation_goal?: ActivationGoal;
  target_engagement_rate?: number;
  target_clean_rate?: number;
  warmup_daily_increment?: number;
  warmup_max_daily_volume?: number;
  
  // Offer Revenue Settings
  offer_model?: OfferModel;
  ecpm_target?: number;
  budget_limit?: number;
  target_metric?: TargetMetric;
  target_value?: number;
  
  // Everflow Integration
  everflow_offer_ids?: string[];
  everflow_affiliate_id?: string;
  property_code?: string;
  
  // AI Configuration
  ai_optimization_enabled: boolean;
  ai_throughput_optimization: boolean;
  ai_creative_rotation: boolean;
  ai_budget_pacing: boolean;
  esp_signal_monitoring: boolean;
  
  // Thresholds
  pause_on_spam_signal: boolean;
  spam_signal_threshold?: number;
  bounce_threshold?: number;
  throughput_sensitivity: ThroughputSensitivity;
  min_throughput_rate: number;
  max_throughput_rate: number;
  
  // Pacing
  target_completion_hours?: number;
  pacing_strategy: PacingStrategy;
  rotation_strategy: RotationStrategy;
}

// Property Mapping
export const PROPERTY_MAPPING: Record<string, string> = {
  'FTT': 'FinancialTipsToday',
  'DHF': 'dailyhistoryfacts.org',
  'SFT': 'savvyfinancetips.net',
  'EHG': 'everydayhealthguide.net',
  'BPG': 'bestpropertyguides.net',
  'JOTD': 'jokeoftheday.info',
  'SH': 'sportshistory.info',
  'NPY': 'newproductsforyou.com',
  'FNI': 'e.financialsinfo.com',
  'AFI': 'affordinginsurance.com',
  'SBD': 'secretbeautydiscounts.com',
  'OTD': 'theoftheday.com',
  'HRO': 'horoscopeinfo.com',
  'TDIH': 'thisdayinhistory.co',
  'OTDD': 'onthisdaydaily.com',
  'FMO': 'financial-money.com',
  'ALC': 'alcatrazblog.com',
  'MHH': 'myhealthyhabitsblog.net',
  'GHH': 'goodhomehub.org',
  'DIH': 'dayinhistory.org',
  'FTD': 'financialtipsdaily.net',
  'FYF': 'findyourfit.net',
  'IGN': 'ignitemedia.com',
};

// Budget Options
export const BUDGET_OPTIONS = [
  { value: 1000, label: '$1,000' },
  { value: 2000, label: '$2,000' },
  { value: 3000, label: '$3,000' },
  { value: 4000, label: '$4,000' },
  { value: 5000, label: '$5,000' },
  { value: 10000, label: '$10,000' },
];

// Default objective
export const DEFAULT_OBJECTIVE: CampaignObjective = {
  purpose: 'data_activation',
  ai_optimization_enabled: true,
  ai_throughput_optimization: true,
  ai_creative_rotation: true,
  ai_budget_pacing: true,
  esp_signal_monitoring: true,
  pause_on_spam_signal: true,
  spam_signal_threshold: 0.001,
  bounce_threshold: 0.05,
  throughput_sensitivity: 'medium',
  min_throughput_rate: 1000,
  max_throughput_rate: 100000,
  pacing_strategy: 'even',
  rotation_strategy: 'performance',
  target_clean_rate: 0.001,
};

// ============================================================================
// COMPONENT
// ============================================================================

interface CampaignPurposeTabProps {
  objective: CampaignObjective;
  onChange: (objective: CampaignObjective) => void;
}

export const CampaignPurposeTab: React.FC<CampaignPurposeTabProps> = ({
  objective,
  onChange,
}) => {
  const [showAdvanced, setShowAdvanced] = useState(false);

  const updateField = <K extends keyof CampaignObjective>(
    field: K,
    value: CampaignObjective[K]
  ) => {
    onChange({ ...objective, [field]: value });
  };

  const setPurpose = (purpose: CampaignPurpose) => {
    const updates: CampaignObjective = {
      ...objective,
      purpose,
    };

    if (purpose === 'data_activation') {
      updates.offer_model = undefined;
      updates.ecpm_target = undefined;
      updates.budget_limit = undefined;
      updates.target_metric = undefined;
      updates.target_value = undefined;
      updates.everflow_offer_ids = undefined;
      updates.everflow_affiliate_id = undefined;
    } else {
      updates.activation_goal = undefined;
      updates.warmup_daily_increment = undefined;
      updates.warmup_max_daily_volume = undefined;
    }

    onChange(updates);
  };

  return (
    <div className="purpose-tab">
      {/* Purpose Selection Header */}
      <div className="purpose-header">
        <h2>What is the purpose of this campaign?</h2>
        <p>This helps the AI understand how to optimize your campaign for maximum results</p>
      </div>

      {/* Purpose Cards */}
      <div className="purpose-cards">
        {/* Data Activation Card */}
        <button
          type="button"
          onClick={() => setPurpose('data_activation')}
          className={`purpose-card ${objective.purpose === 'data_activation' ? 'active' : ''}`}
        >
          <div className="purpose-card-icon activation">
            <FontAwesomeIcon icon={faBolt} />
          </div>
          <div className="purpose-card-content">
            <h3>Data Activation</h3>
            <p>
              Warm up new data, build sender reputation, and activate email
              addresses for future campaigns.
            </p>
            <div className="purpose-tags">
              <span>New Data Warmup</span>
              <span>Domain Reputation</span>
              <span>List Hygiene</span>
            </div>
          </div>
        </button>

        {/* Offer Revenue Card */}
        <button
          type="button"
          onClick={() => setPurpose('offer_revenue')}
          className={`purpose-card ${objective.purpose === 'offer_revenue' ? 'active revenue' : ''}`}
        >
          <div className="purpose-card-icon revenue">
            <FontAwesomeIcon icon={faDollarSign} />
          </div>
          <div className="purpose-card-content">
            <h3>Offer Revenue</h3>
            <p>
              Drive CPM/CPL conversions and maximize revenue with
              performance-based optimization.
            </p>
            <div className="purpose-tags">
              <span>CPM Offers</span>
              <span>CPL Offers</span>
              <span>Revenue Goals</span>
            </div>
          </div>
        </button>
      </div>

      {/* Purpose-Specific Settings */}
      {objective.purpose === 'data_activation' && (
        <DataActivationSettings objective={objective} updateField={updateField} />
      )}

      {objective.purpose === 'offer_revenue' && (
        <OfferRevenueSettings objective={objective} onChange={onChange} updateField={updateField} />
      )}

      {/* AI Optimization Section */}
      <div className="ai-section">
        <div className="ai-section-header">
          <div className="ai-section-icon">
            <FontAwesomeIcon icon={faMagic} />
          </div>
          <div>
            <h3>AI Optimization</h3>
            <p>Let the AI automatically optimize your campaign based on real-time signals</p>
          </div>
        </div>

        <div className="ai-toggles">
          <AIToggle
            label="Auto-adjust throughput"
            description="Increase or decrease send rate based on ISP signals"
            checked={objective.ai_throughput_optimization}
            onChange={(v) => updateField('ai_throughput_optimization', v)}
          />
          <AIToggle
            label="Rotate creatives"
            description="Automatically rotate subject lines and content"
            checked={objective.ai_creative_rotation}
            onChange={(v) => updateField('ai_creative_rotation', v)}
          />
          <AIToggle
            label="Monitor ESP signals"
            description="Watch SparkPost/SES events for spam signals"
            checked={objective.esp_signal_monitoring}
            onChange={(v) => updateField('esp_signal_monitoring', v)}
          />
          <AIToggle
            label="Pause on high complaints"
            description="Auto-pause if complaint rate exceeds threshold"
            checked={objective.pause_on_spam_signal}
            onChange={(v) => updateField('pause_on_spam_signal', v)}
          />
        </div>
      </div>

      {/* Advanced Settings */}
      <div className="advanced-section">
        <button
          type="button"
          onClick={() => setShowAdvanced(!showAdvanced)}
          className="advanced-toggle"
        >
          <span>Advanced Settings</span>
          <FontAwesomeIcon icon={showAdvanced ? faChevronUp : faChevronDown} />
        </button>

        {showAdvanced && (
          <AdvancedSettings objective={objective} updateField={updateField} />
        )}
      </div>
    </div>
  );
};

// ============================================================================
// DATA ACTIVATION SETTINGS
// ============================================================================

const DataActivationSettings: React.FC<{
  objective: CampaignObjective;
  updateField: <K extends keyof CampaignObjective>(field: K, value: CampaignObjective[K]) => void;
}> = ({ objective, updateField }) => {
  const [activationData, setActivationData] = useState<any>(null);
  const [activationLoading, setActivationLoading] = useState(true);
  const [expandedRec, setExpandedRec] = useState<string | null>(null);

  useEffect(() => {
    fetchActivationIntelligence();
  }, []);

  const fetchActivationIntelligence = async () => {
    setActivationLoading(true);
    try {
      const response = await fetch('/api/activation/intelligence');
      if (response.ok) {
        const data = await response.json();
        setActivationData(data);
      }
    } catch (error) {
      console.error('Failed to fetch activation intelligence:', error);
    } finally {
      setActivationLoading(false);
    }
  };

  return (
    <div className="settings-section activation-settings">
      <div className="settings-header">
        <FontAwesomeIcon icon={faBullseye} />
        <h4>Data Activation Settings</h4>
      </div>

      {/* Activation Goal */}
      <div className="activation-goals">
        {[
          {
            value: 'warm_new_data',
            label: 'Warm New Data',
            desc: 'Gradually introduce new email addresses to build reputation',
            icon: faFire,
            color: 'warm',
          },
          {
            value: 'reactivate_cold',
            label: 'Reactivate Cold Data',
            desc: 'Re-engage subscribers who haven\'t opened recently',
            icon: faSnowflake,
            color: 'cold',
          },
          {
            value: 'domain_warmup',
            label: 'Domain Warmup',
            desc: 'Build reputation for a new sending domain',
            icon: faServer,
            color: 'domain',
          },
        ].map((goal) => (
          <button
            key={goal.value}
            type="button"
            onClick={() => updateField('activation_goal', goal.value as ActivationGoal)}
            className={`goal-card ${goal.color} ${objective.activation_goal === goal.value ? 'active' : ''}`}
          >
            <div className={`goal-icon ${goal.color}`}>
              <FontAwesomeIcon icon={goal.icon} />
            </div>
            <div className="goal-content">
              <div className="goal-label">{goal.label}</div>
              <div className="goal-desc">{goal.desc}</div>
            </div>
            {objective.activation_goal === goal.value && (
              <div className="goal-check">
                <FontAwesomeIcon icon={faCheckCircle} />
              </div>
            )}
          </button>
        ))}
      </div>

      {/* Engagement Targets */}
      <div className="form-row">
        <div className="form-group">
          <label>Target Engagement Rate</label>
          <div className="input-with-suffix">
            <input
              type="number"
              step="0.1"
              min="0"
              max="100"
              value={objective.target_engagement_rate ?? ''}
              onChange={(e) =>
                updateField('target_engagement_rate', parseFloat(e.target.value) || undefined)
              }
              placeholder="e.g., 15"
            />
            <span className="suffix">%</span>
          </div>
          <span className="help-text">Expected open rate for activation sends</span>
        </div>

        <div className="form-group">
          <label>Max Complaint Rate</label>
          <div className="input-with-suffix">
            <input
              type="number"
              step="0.01"
              min="0"
              max="1"
              value={objective.target_clean_rate ? (objective.target_clean_rate * 100).toFixed(2) : ''}
              onChange={(e) =>
                updateField('target_clean_rate', parseFloat(e.target.value) / 100 || undefined)
              }
              placeholder="e.g., 0.1"
            />
            <span className="suffix">%</span>
          </div>
          <span className="help-text">Pause if complaints exceed this rate</span>
        </div>
      </div>

      {/* Domain Warmup Settings */}
      {objective.activation_goal === 'domain_warmup' && (
        <div className="form-row warmup-settings">
          <div className="form-group">
            <label>Daily Increment</label>
            <input
              type="number"
              min="100"
              max="10000"
              value={objective.warmup_daily_increment ?? 1000}
              onChange={(e) =>
                updateField('warmup_daily_increment', parseInt(e.target.value) || 1000)
              }
            />
            <span className="help-text">Increase volume by this amount each day</span>
          </div>
          <div className="form-group">
            <label>Max Daily Volume</label>
            <input
              type="number"
              min="1000"
              max="500000"
              value={objective.warmup_max_daily_volume ?? 50000}
              onChange={(e) =>
                updateField('warmup_max_daily_volume', parseInt(e.target.value) || 50000)
              }
            />
            <span className="help-text">Maximum emails per day during warmup</span>
          </div>
        </div>
      )}

      {/* ============================================================ */}
      {/* DATA ACTIVATION INTELLIGENCE */}
      {/* ============================================================ */}
      {activationLoading ? (
        <div className="activation-intel-loading">
          <FontAwesomeIcon icon={faSpinner} spin />
          <span>Analyzing your ecosystem health...</span>
        </div>
      ) : activationData && (
        <div className="activation-intelligence">
          {/* Ecosystem Health Overview */}
          <div className="ecosystem-health-banner">
            <div className="ecosystem-health-header">
              <h5><FontAwesomeIcon icon={faShieldAlt} /> Ecosystem Health Intelligence</h5>
              <span className={`risk-badge risk-${activationData.overall_risk}`}>
                {activationData.overall_risk?.toUpperCase()} RISK
              </span>
            </div>

            <div className="health-score-bar">
              <div className="score-label">
                <span>Overall Health Score</span>
                <strong>{activationData.overall_health}/100</strong>
              </div>
              <div className="score-track">
                <div className={`score-fill risk-${activationData.overall_risk}`}
                  style={{ width: `${activationData.overall_health}%` }} />
              </div>
            </div>

            <p className="ecosystem-summary">{activationData.summary}</p>

            {/* Per-ISP Health Cards */}
            <div className="isp-health-grid">
              {Object.entries(activationData.health_scores || {}).map(([isp, score]: [string, any]) => (
                <div key={isp} className={`isp-health-card risk-${score.risk_level}`}>
                  <div className="isp-card-header">
                    <FontAwesomeIcon icon={faEnvelope} />
                    <span>{score.isp_display_name}</span>
                  </div>
                  <div className="isp-card-score">{score.overall_score.toFixed(0)}</div>
                  <div className={`isp-card-risk risk-${score.risk_level}`}>{score.risk_level}</div>
                  <div className="isp-card-metrics">
                    <div>Bounce: {score.bounce_rate.toFixed(1)}%</div>
                    <div>Complaints: {score.complaint_rate.toFixed(3)}%</div>
                    <div>Engagement: {score.engagement_rate.toFixed(1)}%</div>
                  </div>
                </div>
              ))}
            </div>
          </div>

          {/* ISP-Specific Recommendations */}
          <div className="activation-recommendations">
            <h5>
              <FontAwesomeIcon icon={faMagic} />
              AI Activation Recommendations — Strategic Campaigns by ISP
            </h5>

            <div className="recommendation-list">
              {(activationData.recommendations || []).map((rec: any) => {
                const isExpanded = expandedRec === rec.id;
                return (
                  <div key={rec.id} className={`recommendation-card priority-${rec.priority}`}>
                    <button className="rec-header" onClick={() => setExpandedRec(isExpanded ? null : rec.id)}>
                      <div className="rec-header-left">
                        <FontAwesomeIcon icon={faEnvelope} className={`isp-icon isp-${rec.isp}`} />
                        <div>
                          <div className="rec-title">{rec.title}</div>
                          <div className="rec-subtitle">{rec.impact_estimate} | {rec.timeline_estimate}</div>
                        </div>
                      </div>
                      <div className="rec-header-right">
                        <div className="rec-score">{rec.health_score.overall_score.toFixed(0)}</div>
                        <span className={`priority-badge priority-${rec.priority}`}>{rec.priority.toUpperCase()}</span>
                        <FontAwesomeIcon icon={isExpanded ? faChevronUp : faChevronDown} />
                      </div>
                    </button>

                    {isExpanded && (
                      <div className="rec-expanded">
                        <div className="rec-strategy">
                          <h6>Strategy</h6>
                          <p>{rec.description}</p>
                        </div>

                        <div className="rec-actions">
                          <h6>Key Actions</h6>
                          <ol>
                            {rec.strategy.key_actions.map((action: string, i: number) => (
                              <li key={i}>{action}</li>
                            ))}
                          </ol>
                        </div>

                        {rec.health_score.diagnostics.length > 0 && (
                          <div className="rec-diagnostics">
                            <h6>Diagnostics</h6>
                            <ul>
                              {rec.health_score.diagnostics.map((diag: string, i: number) => (
                                <li key={i}>{diag}</li>
                              ))}
                            </ul>
                          </div>
                        )}

                        <div className="rec-campaign-suggestion">
                          <h6><FontAwesomeIcon icon={faRocket} /> Ready-to-Launch Campaign</h6>
                          <div className="campaign-grid">
                            <div><span>Campaign:</span> <strong>{rec.campaign_suggestion.campaign_name}</strong></div>
                            <div><span>Target:</span> <strong>{rec.campaign_suggestion.target_segment}</strong></div>
                            <div><span>Schedule:</span> {rec.campaign_suggestion.send_schedule}</div>
                            <div><span>ESP:</span> {rec.campaign_suggestion.esp_recommended}</div>
                            <div><span>Volume:</span> {rec.campaign_suggestion.volume}</div>
                          </div>
                          <div className="subject-lines">
                            <span>Subject Line Ideas:</span>
                            {rec.campaign_suggestion.subject_lines.map((sl: string, i: number) => (
                              <span key={i} className="subject-chip">"{sl}"</span>
                            ))}
                          </div>
                          <p className="campaign-notes">{rec.campaign_suggestion.notes}</p>
                        </div>

                        <div className="rec-metrics-risks">
                          <div>
                            <h6>Success Metrics</h6>
                            <ul>
                              {rec.strategy.success_metrics.map((m: string, i: number) => (
                                <li key={i} className="success-metric">✓ {m}</li>
                              ))}
                            </ul>
                          </div>
                          <div>
                            <h6>Risks</h6>
                            <ul>
                              {rec.strategy.risks.map((r: string, i: number) => (
                                <li key={i} className="risk-item">⚠ {r}</li>
                              ))}
                            </ul>
                          </div>
                        </div>
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// ============================================================================
// OFFER REVENUE SETTINGS WITH NETWORK INTELLIGENCE
// ============================================================================

const OfferRevenueSettings: React.FC<{
  objective: CampaignObjective;
  onChange: (objective: CampaignObjective) => void;
  updateField: <K extends keyof CampaignObjective>(field: K, value: CampaignObjective[K]) => void;
}> = ({ objective, onChange, updateField }) => {
  // Network intelligence state (shown immediately)
  const [networkOffers, setNetworkOffers] = useState<NetworkOffer[]>([]);
  const [audienceRecs, setAudienceRecs] = useState<AudienceRecommendation[]>([]);
  const [networkLoading, setNetworkLoading] = useState(true);
  const [networkStats, setNetworkStats] = useState<{
    total_clicks: number; total_conversions: number; total_revenue: number;
    avg_cvr: number; avg_epc: number;
  } | null>(null);

  // Affiliate drill-down state (secondary view)
  const [showAffiliateDrilldown, setShowAffiliateDrilldown] = useState(false);
  const [affiliates, setAffiliates] = useState<EverflowAffiliate[]>([]);
  const [affiliateOffers, setAffiliateOffers] = useState<EverflowOffer[]>([]);
  const [affiliateLoading, setAffiliateLoading] = useState(false);
  const [selectedAffiliateId, setSelectedAffiliateId] = useState<string>(objective.everflow_affiliate_id || '');

  // Common state
  const [selectedOffers, setSelectedOffers] = useState<string[]>(objective.everflow_offer_ids || []);
  const [searchTerm, setSearchTerm] = useState('');
  const [offerTypeFilter, setOfferTypeFilter] = useState<string>('');

  // Fetch network-wide top offers immediately on mount
  useEffect(() => {
    fetchNetworkTopOffers();
  }, []);

  // Fetch affiliates for drill-down
  useEffect(() => {
    if (showAffiliateDrilldown) {
      fetchAffiliates();
    }
  }, [showAffiliateDrilldown]);

  // Fetch affiliate offers when drill-down affiliate changes
  useEffect(() => {
    if (selectedAffiliateId && showAffiliateDrilldown) {
      fetchAffiliateOffers(selectedAffiliateId);
    }
  }, [selectedAffiliateId, showAffiliateDrilldown]);

  const fetchNetworkTopOffers = async () => {
    setNetworkLoading(true);
    try {
      const response = await fetch('/api/everflow/network-top-offers');
      if (response.ok) {
        const data = await response.json();
        setNetworkOffers(data.top_offers || []);
        setAudienceRecs(data.audience_recommendations || []);
        setNetworkStats({
          total_clicks: data.network_total_clicks || 0,
          total_conversions: data.network_total_conversions || 0,
          total_revenue: data.network_total_revenue || 0,
          avg_cvr: data.network_avg_cvr || 0,
          avg_epc: data.network_avg_epc || 0,
        });
      }
    } catch (error) {
      console.error('Failed to fetch network top offers:', error);
    } finally {
      setNetworkLoading(false);
    }
  };

  const fetchAffiliates = async () => {
    try {
      const response = await fetch('/api/everflow/affiliates');
      if (response.ok) {
        const data = await response.json();
        setAffiliates(data.affiliates || []);
        if (!selectedAffiliateId && data.affiliates?.length > 0) {
          setSelectedAffiliateId(data.affiliates[0].id);
        }
      }
    } catch (error) {
      console.error('Failed to fetch affiliates:', error);
    }
  };

  const fetchAffiliateOffers = useCallback(async (affiliateId: string) => {
    setAffiliateLoading(true);
    try {
      const params = new URLSearchParams({ affiliate_id: affiliateId, lookback_days: '7' });
      if (offerTypeFilter) params.append('offer_type', offerTypeFilter);
      
      const response = await fetch(`/api/everflow/campaign-offers?${params}`);
      if (response.ok) {
        const data = await response.json();
        setAffiliateOffers(data.offers || []);
      }
    } catch (error) {
      console.error('Failed to fetch affiliate offers:', error);
    } finally {
      setAffiliateLoading(false);
    }
  }, [offerTypeFilter]);

  const toggleOfferSelection = (offerId: string) => {
    const newSelection = selectedOffers.includes(offerId)
      ? selectedOffers.filter(id => id !== offerId)
      : [...selectedOffers, offerId];
    
    setSelectedOffers(newSelection);
    onChange({ ...objective, everflow_offer_ids: newSelection });
  };

  const selectRecommendedOffer = (offerId: string) => {
    if (!selectedOffers.includes(offerId)) {
      const newSelection = [...selectedOffers, offerId];
      setSelectedOffers(newSelection);
      onChange({ ...objective, everflow_offer_ids: newSelection });
    }
  };

  // Filter network offers
  const filteredNetworkOffers = networkOffers.filter(offer => {
    const matchesSearch = !searchTerm ||
      offer.offer_name.toLowerCase().includes(searchTerm.toLowerCase()) ||
      offer.offer_id.includes(searchTerm);
    const matchesType = !offerTypeFilter || offer.offer_type === offerTypeFilter;
    return matchesSearch && matchesType;
  });

  // Get selected offer details from network data
  const selectedOfferDetails = networkOffers.filter(o => selectedOffers.includes(o.offer_id));

  return (
    <div className="settings-section revenue-settings">
      <div className="settings-header">
        <FontAwesomeIcon icon={faChartLine} />
        <h4>Offer Revenue Settings</h4>
      </div>

      {/* ============================================================ */}
      {/* NETWORK-WIDE TOP OFFERS (Shown Immediately) */}
      {/* ============================================================ */}

      {/* Network Stats Banner */}
      {networkStats && !networkLoading && (
        <div className="network-stats-banner">
          <div className="network-stat">
            <FontAwesomeIcon icon={faGlobe} />
            <div>
              <span className="stat-value">${networkStats.total_revenue.toLocaleString(undefined, { maximumFractionDigits: 0 })}</span>
              <span className="stat-label">Network Revenue (7d)</span>
            </div>
          </div>
          <div className="network-stat">
            <FontAwesomeIcon icon={faUsers} />
            <div>
              <span className="stat-value">{networkStats.total_clicks.toLocaleString()}</span>
              <span className="stat-label">Network Clicks</span>
            </div>
          </div>
          <div className="network-stat">
            <FontAwesomeIcon icon={faCrosshairs} />
            <div>
              <span className="stat-value">{networkStats.avg_cvr.toFixed(2)}%</span>
              <span className="stat-label">Avg CVR</span>
            </div>
          </div>
          <div className="network-stat">
            <FontAwesomeIcon icon={faDollarSign} />
            <div>
              <span className="stat-value">${networkStats.avg_epc.toFixed(3)}</span>
              <span className="stat-label">Avg EPC</span>
            </div>
          </div>
        </div>
      )}

      {/* AI Audience Match Recommendations */}
      {audienceRecs.length > 0 && (
        <div className="ai-audience-recommendations">
          <div className="ai-recommendations-header">
            <div className="ai-rec-title">
              <FontAwesomeIcon icon={faLightbulb} />
              <h5>AI Recommendations - What to Send Today</h5>
            </div>
            <button
              type="button"
              className="refresh-btn"
              onClick={fetchNetworkTopOffers}
              disabled={networkLoading}
            >
              <FontAwesomeIcon icon={faSync} spin={networkLoading} />
            </button>
          </div>
          <div className="audience-rec-cards">
            {audienceRecs.slice(0, 4).map(rec => (
              <div
                key={rec.offer_id}
                className={`audience-rec-card ${selectedOffers.includes(rec.offer_id) ? 'selected' : ''}`}
                onClick={() => selectRecommendedOffer(rec.offer_id)}
              >
                <div className="arc-header">
                  <div className="arc-score-badge">
                    <span className="arc-score">{Math.round(rec.match_score)}</span>
                  </div>
                  <div className="arc-offer-info">
                    <span className="arc-offer-name">{rec.offer_name}</span>
                    <span className={`arc-type type-${rec.offer_type.toLowerCase()}`}>{rec.offer_type}</span>
                  </div>
                  {selectedOffers.includes(rec.offer_id) && (
                    <FontAwesomeIcon icon={faCheckCircle} className="arc-selected-icon" />
                  )}
                </div>

                <div className="arc-target">
                  <FontAwesomeIcon icon={faEnvelope} />
                  <span className="arc-target-audience">{rec.target_audience}</span>
                </div>

                <div className="arc-metrics">
                  <span className={`arc-strategy strategy-${rec.recommended_strategy}`}>
                    {rec.recommended_strategy}
                  </span>
                  <span className="arc-confidence">
                    {Math.round(rec.confidence_level * 100)}% confidence
                  </span>
                  {rec.predicted_cvr > 0 && (
                    <span className="arc-prediction">~{rec.predicted_cvr.toFixed(2)}% CVR</span>
                  )}
                </div>

                <ul className="arc-reasons">
                  {rec.match_reasons.slice(0, 2).map((reason, i) => (
                    <li key={i}>{reason}</li>
                  ))}
                </ul>

                {rec.send_time_hint && (
                  <div className="arc-timing">
                    <FontAwesomeIcon icon={faCog} />
                    <span>{rec.send_time_hint}</span>
                  </div>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Selected Offers Summary */}
      {selectedOfferDetails.length > 0 && (
        <div className="selected-offers-summary">
          <h5>Selected Offers ({selectedOfferDetails.length})</h5>
          <div className="selected-offer-chips">
            {selectedOfferDetails.map(offer => (
              <div key={offer.offer_id} className="offer-chip">
                <span>{offer.offer_name}</span>
                <span className="chip-revenue">${offer.today_revenue.toFixed(2)} today</span>
                <button type="button" onClick={() => toggleOfferSelection(offer.offer_id)}>×</button>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Search & Filter Controls */}
      <div className="offer-controls">
        <div className="search-box">
          <FontAwesomeIcon icon={faSearch} />
          <input
            type="text"
            placeholder="Search offers across the network..."
            value={searchTerm}
            onChange={(e) => setSearchTerm(e.target.value)}
          />
        </div>
        <div className="type-filter">
          <select
            value={offerTypeFilter}
            onChange={(e) => setOfferTypeFilter(e.target.value)}
          >
            <option value="">All Types</option>
            <option value="CPM">CPM</option>
            <option value="CPL">CPL</option>
            <option value="CPA">CPA</option>
            <option value="CPS">CPS</option>
          </select>
        </div>
      </div>

      {/* Network Top Offers Table */}
      <div className="offers-list network-offers-list">
        <div className="offers-section-header">
          <FontAwesomeIcon icon={faFire} />
          <h5>Top Performing Offers Across Network Today</h5>
        </div>

        {networkLoading ? (
          <div className="offers-loading">
            <FontAwesomeIcon icon={faSpinner} spin />
            <span>Analyzing network performance...</span>
          </div>
        ) : filteredNetworkOffers.length === 0 ? (
          <div className="offers-empty">
            <p>No offers found. Try adjusting your filters.</p>
          </div>
        ) : (
          <div className="offers-table network-table">
            <div className="offers-table-header network-header">
              <span className="col-rank">#</span>
              <span className="col-offer">Offer</span>
              <span className="col-type">Type</span>
              <span className="col-today-rev">Today Revenue</span>
              <span className="col-today-conv">Today Conv</span>
              <span className="col-epc">EPC</span>
              <span className="col-trend">Trend</span>
              <span className="col-audience">Audience Signal</span>
              <span className="col-score">AI Score</span>
            </div>
            {filteredNetworkOffers.slice(0, 30).map(offer => (
              <div
                key={offer.offer_id}
                className={`offer-row network-row ${selectedOffers.includes(offer.offer_id) ? 'selected' : ''} ${offer.ai_recommendation}`}
                onClick={() => toggleOfferSelection(offer.offer_id)}
              >
                <span className="col-rank rank-badge">
                  {offer.network_rank <= 3 ? (
                    <span className={`rank-top rank-${offer.network_rank}`}>{offer.network_rank}</span>
                  ) : (
                    offer.network_rank
                  )}
                </span>
                <div className="offer-info col-offer">
                  <span className="offer-name">{offer.offer_name}</span>
                  <span className="offer-id">#{offer.offer_id}</span>
                </div>
                <span className={`offer-type type-${offer.offer_type.toLowerCase()} col-type`}>
                  {offer.offer_type}
                </span>
                <span className="offer-today col-today-rev">
                  ${offer.today_revenue.toFixed(2)}
                  <small>{offer.today_clicks.toLocaleString()} clicks</small>
                </span>
                <span className="col-today-conv">
                  {offer.today_conversions}
                </span>
                <span className="offer-epc col-epc">
                  ${offer.today_epc > 0 ? offer.today_epc.toFixed(3) : offer.network_epc.toFixed(3)}
                </span>
                <span className={`offer-trend col-trend trend-${offer.revenue_trend === 'accelerating' ? 'up' : offer.revenue_trend === 'decelerating' ? 'down' : 'stable'}`}>
                  <FontAwesomeIcon
                    icon={offer.revenue_trend === 'accelerating' ? faArrowUp : offer.revenue_trend === 'decelerating' ? faArrowDown : faMinus}
                  />
                  {offer.trend_percentage > 0 ? '+' : ''}{offer.trend_percentage.toFixed(0)}%
                </span>
                <span className="col-audience">
                  {offer.audience_profile ? (
                    <span className="audience-signal" title={`${offer.audience_profile.primary_inbox_pct.toFixed(0)}% ${offer.audience_profile.primary_inbox} | ${offer.audience_profile.chromium_percentage.toFixed(0)}% Chromium | ${offer.audience_profile.primary_device}`}>
                      <span className={`inbox-badge inbox-${offer.audience_profile.primary_inbox}`}>
                        {offer.audience_profile.primary_inbox}
                      </span>
                      <small>{offer.audience_profile.primary_inbox_pct.toFixed(0)}%</small>
                    </span>
                  ) : (
                    <span className="no-signal">--</span>
                  )}
                </span>
                <span className={`offer-score col-score score-${offer.ai_recommendation}`}>
                  {offer.ai_score.toFixed(0)}
                  {offer.ai_recommendation === 'highly_recommended' && (
                    <FontAwesomeIcon icon={faStar} className="star" />
                  )}
                </span>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Affiliate Drill-Down (Secondary View) */}
      <div className="affiliate-drilldown-section">
        <button
          type="button"
          className="drilldown-toggle"
          onClick={() => setShowAffiliateDrilldown(!showAffiliateDrilldown)}
        >
          <FontAwesomeIcon icon={showAffiliateDrilldown ? faChevronUp : faChevronDown} />
          <span>View My Affiliate Performance</span>
          <small>Filter by your affiliate IDs (9533 / 9572)</small>
        </button>

        {showAffiliateDrilldown && (
          <div className="affiliate-drilldown-content">
            <div className="affiliate-buttons">
              {affiliates.map(affiliate => (
                <button
                  key={affiliate.id}
                  type="button"
                  onClick={() => {
                    setSelectedAffiliateId(affiliate.id);
                    onChange({ ...objective, everflow_affiliate_id: affiliate.id });
                  }}
                  className={`affiliate-btn ${selectedAffiliateId === affiliate.id ? 'active' : ''}`}
                >
                  <span className="affiliate-id">#{affiliate.id}</span>
                  <span className="affiliate-name">{affiliate.name}</span>
                </button>
              ))}
            </div>

            {affiliateLoading ? (
              <div className="offers-loading">
                <FontAwesomeIcon icon={faSpinner} spin />
                <span>Loading affiliate offers...</span>
              </div>
            ) : affiliateOffers.length > 0 && (
              <div className="offers-table affiliate-table">
                <div className="offers-table-header">
                  <span>Offer</span>
                  <span>Type</span>
                  <span>Today</span>
                  <span>7-Day Rev</span>
                  <span>EPC</span>
                  <span>Trend</span>
                  <span>AI Score</span>
                </div>
                {affiliateOffers.map(offer => (
                  <div
                    key={offer.offer_id}
                    className={`offer-row ${selectedOffers.includes(offer.offer_id) ? 'selected' : ''} ${offer.ai_recommendation}`}
                    onClick={() => toggleOfferSelection(offer.offer_id)}
                  >
                    <div className="offer-info">
                      <span className="offer-name">{offer.offer_name}</span>
                      <span className="offer-id">#{offer.offer_id}</span>
                    </div>
                    <span className={`offer-type type-${offer.offer_type.toLowerCase()}`}>
                      {offer.offer_type}
                    </span>
                    <span className="offer-today">
                      ${offer.today_revenue.toFixed(2)}
                      <small>{offer.today_clicks} clicks</small>
                    </span>
                    <span className="offer-revenue">${offer.revenue.toFixed(2)}</span>
                    <span className="offer-epc">${offer.epc.toFixed(3)}</span>
                    <span className={`offer-trend trend-${offer.revenue_trend}`}>
                      <FontAwesomeIcon
                        icon={offer.revenue_trend === 'up' ? faArrowUp : offer.revenue_trend === 'down' ? faArrowDown : faMinus}
                      />
                      {offer.trend_percentage > 0 ? '+' : ''}{offer.trend_percentage.toFixed(0)}%
                    </span>
                    <span className={`offer-score score-${offer.ai_recommendation}`}>
                      {offer.ai_score.toFixed(0)}
                      {offer.ai_recommendation === 'highly_recommended' && (
                        <FontAwesomeIcon icon={faStar} className="star" />
                      )}
                    </span>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>

      {/* Budget & Targets */}
      <div className="form-row three-col">
        <div className="form-group">
          <label>ECM Target (from contract)</label>
          <div className="input-with-prefix">
            <span className="prefix">$</span>
            <input
              type="number"
              step="0.01"
              min="0"
              value={objective.ecpm_target ?? ''}
              onChange={(e) =>
                updateField('ecpm_target', parseFloat(e.target.value) || undefined)
              }
              placeholder="e.g., 2.50"
            />
          </div>
          <span className="help-text">Target eCPM per 1000 sends</span>
        </div>

        <div className="form-group">
          <label>Campaign Budget</label>
          <select
            value={objective.budget_limit ?? ''}
            onChange={(e) =>
              updateField('budget_limit', parseFloat(e.target.value) || undefined)
            }
          >
            <option value="">Select budget...</option>
            {BUDGET_OPTIONS.map((opt) => (
              <option key={opt.value} value={opt.value}>
                {opt.label}
              </option>
            ))}
          </select>
        </div>

        <div className="form-group">
          <label>Property Code</label>
          <select
            value={objective.property_code ?? ''}
            onChange={(e) => updateField('property_code', e.target.value || undefined)}
          >
            <option value="">Select property...</option>
            {Object.entries(PROPERTY_MAPPING).map(([code, name]) => (
              <option key={code} value={code}>
                {code} - {name}
              </option>
            ))}
          </select>
        </div>
      </div>

      {/* Target Metric & Value */}
      <div className="form-row">
        <div className="form-group">
          <label>Target Metric</label>
          <select
            value={objective.target_metric ?? ''}
            onChange={(e) =>
              updateField('target_metric', (e.target.value as TargetMetric) || undefined)
            }
          >
            <option value="">Select metric...</option>
            <option value="clicks">Clicks</option>
            <option value="conversions">Conversions</option>
            <option value="revenue">Revenue ($)</option>
            <option value="ecpm">eCPM ($)</option>
          </select>
        </div>

        <div className="form-group">
          <label>Target Value</label>
          <input
            type="number"
            min="0"
            value={objective.target_value ?? ''}
            onChange={(e) =>
              updateField('target_value', parseInt(e.target.value) || undefined)
            }
            placeholder={
              objective.target_metric === 'clicks'
                ? 'e.g., 5000 clicks'
                : objective.target_metric === 'conversions'
                ? 'e.g., 500 conversions'
                : 'Enter target...'
            }
          />
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// ADVANCED SETTINGS
// ============================================================================

const AdvancedSettings: React.FC<{
  objective: CampaignObjective;
  updateField: <K extends keyof CampaignObjective>(field: K, value: CampaignObjective[K]) => void;
}> = ({ objective, updateField }) => {
  return (
    <div className="advanced-content">
      {/* Throughput Settings */}
      <div className="settings-group">
        <h5><FontAwesomeIcon icon={faShieldAlt} /> Throughput Control</h5>
        <div className="form-row three-col">
          <div className="form-group">
            <label>Sensitivity</label>
            <select
              value={objective.throughput_sensitivity}
              onChange={(e) =>
                updateField('throughput_sensitivity', e.target.value as ThroughputSensitivity)
              }
            >
              <option value="low">Low - Slow adjustments</option>
              <option value="medium">Medium - Balanced</option>
              <option value="high">High - Fast response</option>
            </select>
          </div>
          <div className="form-group">
            <label>Min Throughput (per hour)</label>
            <input
              type="number"
              min="100"
              max="100000"
              value={objective.min_throughput_rate}
              onChange={(e) =>
                updateField('min_throughput_rate', parseInt(e.target.value) || 1000)
              }
            />
          </div>
          <div className="form-group">
            <label>Max Throughput (per hour)</label>
            <input
              type="number"
              min="1000"
              max="500000"
              value={objective.max_throughput_rate}
              onChange={(e) =>
                updateField('max_throughput_rate', parseInt(e.target.value) || 100000)
              }
            />
          </div>
        </div>
      </div>

      {/* Safety Thresholds */}
      <div className="settings-group">
        <h5>Safety Thresholds</h5>
        <div className="form-row">
          <div className="form-group">
            <label>Spam Signal Threshold</label>
            <div className="input-with-suffix">
              <input
                type="number"
                step="0.01"
                min="0"
                max="1"
                value={objective.spam_signal_threshold ? (objective.spam_signal_threshold * 100).toFixed(2) : '0.10'}
                onChange={(e) =>
                  updateField('spam_signal_threshold', parseFloat(e.target.value) / 100 || 0.001)
                }
              />
              <span className="suffix">%</span>
            </div>
          </div>
          <div className="form-group">
            <label>Bounce Rate Threshold</label>
            <div className="input-with-suffix">
              <input
                type="number"
                step="0.1"
                min="0"
                max="20"
                value={objective.bounce_threshold ? (objective.bounce_threshold * 100).toFixed(1) : '5.0'}
                onChange={(e) =>
                  updateField('bounce_threshold', parseFloat(e.target.value) / 100 || 0.05)
                }
              />
              <span className="suffix">%</span>
            </div>
          </div>
        </div>
      </div>

      {/* Pacing Strategy */}
      <div className="settings-group">
        <h5><FontAwesomeIcon icon={faRocket} /> Pacing Strategy</h5>
        <div className="pacing-options">
          {[
            { value: 'aggressive', label: 'Aggressive', desc: 'Front-load sending' },
            { value: 'even', label: 'Even', desc: 'Spread evenly' },
            { value: 'conservative', label: 'Conservative', desc: 'Slow start, ramp up' },
          ].map((strategy) => (
            <button
              key={strategy.value}
              type="button"
              onClick={() => updateField('pacing_strategy', strategy.value as PacingStrategy)}
              className={`pacing-option ${objective.pacing_strategy === strategy.value ? 'active' : ''}`}
            >
              <div className="pacing-label">{strategy.label}</div>
              <div className="pacing-desc">{strategy.desc}</div>
            </button>
          ))}
        </div>
      </div>

      {/* Creative Rotation */}
      <div className="settings-group">
        <h5><FontAwesomeIcon icon={faCog} /> Creative Rotation</h5>
        <div className="form-group">
          <select
            value={objective.rotation_strategy}
            onChange={(e) => updateField('rotation_strategy', e.target.value as RotationStrategy)}
          >
            <option value="performance">Performance - Best performing creative wins</option>
            <option value="weighted">Weighted - Proportional to performance</option>
            <option value="round_robin">Round Robin - Equal rotation</option>
            <option value="explore">Explore - Prioritize testing new creatives</option>
          </select>
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// AI TOGGLE COMPONENT
// ============================================================================

const AIToggle: React.FC<{
  label: string;
  description: string;
  checked: boolean;
  onChange: (checked: boolean) => void;
}> = ({ label, description, checked, onChange }) => {
  return (
    <label className="ai-toggle">
      <div className="toggle-switch">
        <input
          type="checkbox"
          checked={checked}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span className="toggle-slider" />
      </div>
      <div className="toggle-content">
        <div className="toggle-label">{label}</div>
        <div className="toggle-desc">{description}</div>
      </div>
    </label>
  );
};

export default CampaignPurposeTab;
