// API Response Types

export interface Summary {
  timestamp: string;
  period_start: string;
  period_end: string;
  total_targeted: number;
  total_delivered: number;
  total_opened: number;
  total_clicked: number;
  total_bounced: number;
  total_complaints: number;
  total_unsubscribes: number;
  delivery_rate: number;
  open_rate: number;
  click_rate: number;
  bounce_rate: number;
  complaint_rate: number;
  unsubscribe_rate: number;
  volume_change?: number;
  delivery_change?: number;
  open_rate_change?: number;
  complaint_change?: number;
}

export interface ProcessedMetrics {
  timestamp: string;
  source: string;
  group_by: string;
  group_value: string;
  targeted: number;
  injected: number;
  sent: number;
  delivered: number;
  opened: number;
  unique_opened: number;
  clicked: number;
  unique_clicked: number;
  bounced: number;
  hard_bounced: number;
  soft_bounced: number;
  block_bounced: number;
  complaints: number;
  unsubscribes: number;
  delayed: number;
  rejected: number;
  delivery_rate: number;
  open_rate: number;
  click_rate: number;
  bounce_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  block_rate: number;
  complaint_rate: number;
  unsubscribe_rate: number;
}

export interface MetricTrends {
  volume_direction: 'up' | 'down' | 'stable';
  volume_change_percent: number;
  delivery_trend: 'up' | 'down' | 'stable';
  delivery_change: number;
  complaint_trend: 'up' | 'down' | 'stable';
  complaint_change: number;
}

export interface ISPMetrics {
  provider: string;
  metrics: ProcessedMetrics;
  status: 'healthy' | 'warning' | 'critical';
  status_reason?: string;
  trends?: MetricTrends;
}

export interface IPMetrics {
  ip: string;
  pool: string;
  metrics: ProcessedMetrics;
  status: 'healthy' | 'warning' | 'critical';
  status_reason?: string;
  trends?: MetricTrends;
}

export interface DomainMetrics {
  domain: string;
  metrics: ProcessedMetrics;
  status: 'healthy' | 'warning' | 'critical';
  status_reason?: string;
  trends?: MetricTrends;
}

export interface BounceReason {
  reason: string;
  domain?: string;
  bounce_class_name: string;
  bounce_class_description: string;
  bounce_category_id: number;
  bounce_category_name: string;
  classification_id: number;
  count_inband_bounce: number;
  count_outofband_bounce: number;
  count_bounce: number;
}

export interface DelayReason {
  reason: string;
  domain?: string;
  count_delayed: number;
  count_delayed_first: number;
}

export interface RejectionReason {
  reason: string;
  domain?: string;
  count_rejected: number;
  rejection_category_id: number;
  rejection_type: string;
}

export interface Issue {
  severity: 'warning' | 'critical';
  category: string;
  description: string;
  affected_isp?: string;
  affected_ip?: string;
  count: number;
  recommendation: string;
}

export interface SignalsData {
  timestamp: string;
  bounce_reasons: BounceReason[];
  delay_reasons: DelayReason[];
  rejection_reasons: RejectionReason[];
  top_issues: Issue[];
}

export interface Alert {
  id: string;
  timestamp: string;
  severity: 'info' | 'warning' | 'critical';
  category: string;
  title: string;
  description: string;
  entity_type: string;
  entity_name: string;
  metric_name: string;
  current_value: number;
  baseline_value: number;
  deviation: number;
  recommendation: string;
  acknowledged: boolean;
}

export interface Insight {
  id: string;
  timestamp: string;
  type: 'pattern' | 'correlation' | 'trend' | 'anomaly';
  title: string;
  description: string;
  confidence: number;
  entity_type?: string;
  entity_name?: string;
}

export interface Baseline {
  entity_type: string;
  entity_name: string;
  metrics: Record<string, MetricBaseline>;
  updated_at: string;
  data_points: number;
}

export interface MetricBaseline {
  mean: number;
  std_dev: number;
  min: number;
  max: number;
  p50: number;
  p75: number;
  p90: number;
  p95: number;
  p99: number;
}

export interface Correlation {
  entity_type: string;
  entity_name: string;
  trigger_metric: string;
  trigger_threshold: number;
  trigger_operator: string;
  effect_metric: string;
  effect_change: number;
  confidence: number;
  occurrences: number;
  last_observed: string;
}

export interface ChatResponse {
  message: string;
  data?: unknown;
  suggestions?: string[];
}

export interface DashboardData {
  timestamp: string;
  last_fetch: string;
  summary: Summary | null;
  isp_metrics: ISPMetrics[];
  ip_metrics: IPMetrics[];
  domain_metrics: DomainMetrics[];
  signals: SignalsData | null;
  alerts: {
    active_count: number;
    total_count: number;
    items: Alert[];
  };
}

export interface SystemStatus {
  timestamp: string;
  collector: {
    running: boolean;
    last_fetch: string;
  };
  agent: {
    alerts_count: number;
    baselines_count: number;
    correlations_count: number;
  };
  storage: {
    metrics_days: number;
    isp_days: number;
    ip_days: number;
    domain_days: number;
    signals_count: number;
    baselines_count: number;
    correlations: number;
  };
}

// Mailgun-specific types

export interface MailgunSummary {
  timestamp: string;
  period_start: string;
  period_end: string;
  total_targeted: number;
  total_delivered: number;
  total_opened: number;
  total_clicked: number;
  total_bounced: number;
  total_complaints: number;
  total_unsubscribes: number;
  delivery_rate: number;
  open_rate: number;
  click_rate: number;
  bounce_rate: number;
  complaint_rate: number;
  unsubscribe_rate: number;
  volume_change?: number;
  open_rate_change?: number;
  complaint_change?: number;
}

export interface MailgunISPMetrics {
  provider: string;
  metrics: ProcessedMetrics;
  status: 'healthy' | 'warning' | 'critical';
  status_reason?: string;
  trends?: MetricTrends;
}

export interface MailgunDomainMetrics {
  domain: string;
  metrics: ProcessedMetrics;
  status: 'healthy' | 'warning' | 'critical';
  status_reason?: string;
  trends?: MetricTrends;
}

export interface MailgunBounceClassification {
  entity: string;
  classification: string;
  count: number;
  reason?: string;
}

export interface MailgunSignalsData {
  timestamp: string;
  bounce_reasons: MailgunBounceClassification[];
  top_issues: Issue[];
}

export interface MailgunDashboardData {
  timestamp: string;
  last_fetch: string;
  summary: MailgunSummary | null;
  isp_metrics: MailgunISPMetrics[];
  domain_metrics: MailgunDomainMetrics[];
  signals: MailgunSignalsData | null;
}

export interface CombinedDashboardData {
  timestamp: string;
  sparkpost: {
    last_fetch: string;
    summary: Summary | null;
    isp_metrics: ISPMetrics[];
    ip_metrics: IPMetrics[];
    domain_metrics: DomainMetrics[];
    signals: SignalsData | null;
  };
  mailgun?: {
    last_fetch: string;
    summary: MailgunSummary | null;
    isp_metrics: MailgunISPMetrics[];
    domain_metrics: MailgunDomainMetrics[];
    signals: MailgunSignalsData | null;
  };
  ses?: {
    last_fetch: string;
    summary: SESSummary | null;
    isp_metrics: SESISPMetrics[];
    signals: SESSignalsData | null;
  };
  alerts: {
    active_count: number;
    total_count: number;
    items: Alert[];
  };
}

// SES-specific types

export interface SESSummary {
  timestamp: string;
  period_start: string;
  period_end: string;
  total_targeted: number;
  total_delivered: number;
  total_opened: number;
  total_clicked: number;
  total_bounced: number;
  total_complaints: number;
  total_unsubscribes: number;
  delivery_rate: number;
  open_rate: number;
  click_rate: number;
  bounce_rate: number;
  complaint_rate: number;
  unsubscribe_rate: number;
  volume_change?: number;
  open_rate_change?: number;
  complaint_change?: number;
}

export interface SESISPMetrics {
  provider: string;
  metrics: ProcessedMetrics;
  status: 'healthy' | 'warning' | 'critical';
  status_reason?: string;
  trends?: MetricTrends;
}

export interface SESIssue {
  severity: 'warning' | 'critical';
  category: string;
  description: string;
  affected_isp?: string;
  count: number;
  recommendation: string;
}

export interface SESSignalsData {
  timestamp: string;
  top_issues: SESIssue[];
  recommendations: string[];
}

export interface SESDashboardData {
  timestamp: string;
  last_fetch: string;
  summary: SESSummary | null;
  isp_metrics: SESISPMetrics[];
  signals: SESSignalsData | null;
}

// Unified ISP Performance types (combines all providers)

export interface UnifiedISPMetric {
  provider: 'sparkpost' | 'mailgun' | 'ses';
  isp: string;
  volume: number;
  delivered: number;
  delivery_rate: number;
  opens: number;
  open_rate: number;
  clicks: number;
  click_rate: number;
  bounces: number;
  bounce_rate: number;
  complaints: number;
  complaint_rate: number;
  status: 'healthy' | 'warning' | 'critical';
}

export interface UnifiedISPResponse {
  timestamp: string;
  metrics: UnifiedISPMetric[];
  providers: string[];
}

// Unified IP Performance types (combines all providers)

export interface UnifiedIPMetric {
  provider: 'sparkpost' | 'mailgun' | 'ses';
  ip: string;
  pool: string;
  pool_type: 'dedicated' | 'shared' | 'unknown';
  pool_description: string;
  volume: number;
  delivered: number;
  delivery_rate: number;
  opens: number;
  open_rate: number;
  clicks: number;
  click_rate: number;
  bounces: number;
  bounce_rate: number;
  complaints: number;
  complaint_rate: number;
  status: 'healthy' | 'warning' | 'critical';
  status_reason: string;
}

export interface UnifiedIPResponse {
  timestamp: string;
  metrics: UnifiedIPMetric[];
  providers: string[];
}

// Unified Domain Performance types (combines all providers)

export interface UnifiedDomainMetric {
  provider: 'sparkpost' | 'mailgun' | 'ses';
  domain: string;
  volume: number;
  delivered: number;
  delivery_rate: number;
  opens: number;
  open_rate: number;
  clicks: number;
  click_rate: number;
  bounces: number;
  bounce_rate: number;
  complaints: number;
  complaint_rate: number;
  status: 'healthy' | 'warning' | 'critical';
  status_reason: string;
}

export interface UnifiedDomainResponse {
  timestamp: string;
  metrics: UnifiedDomainMetric[];
  providers: string[];
}

// ========== Ongage Types ==========

export interface OngageCampaign {
  id: string;
  name: string;
  subject: string;
  status: string;
  status_desc: string;
  schedule_time: string;
  send_start_time?: string;
  send_end_time?: string;
  esp: string;
  esp_connection_id: string;
  segments: string[];
  targeted: number;
  sent: number;
  delivered: number;
  delivery_rate: number;
  opens: number;
  unique_opens: number;
  open_rate: number;
  clicks: number;
  unique_clicks: number;
  click_rate: number;
  ctr: number;
  unsubscribes: number;
  unsubscribe_rate: number;
  complaints: number;
  complaint_rate: number;
  bounces: number;
  bounce_rate: number;
  is_test: boolean;
}

export interface OngageESPConnection {
  id: string;
  esp_id: string;
  name: string;
  title?: string;
  active?: string;
}

export interface OngageSubjectAnalysis {
  subject: string;
  campaign_count: number;
  total_sent: number;
  avg_open_rate: number;
  avg_click_rate: number;
  avg_ctr: number;
  length: number;
  has_emoji: boolean;
  has_number: boolean;
  has_question: boolean;
  has_urgency: boolean;
  esps: string[];
  performance: 'high' | 'medium' | 'low';
}

export interface OngageScheduleAnalysis {
  hour: number;
  day_of_week: number;
  day_name: string;
  campaign_count: number;
  total_sent: number;
  avg_open_rate: number;
  avg_click_rate: number;
  avg_delivery_rate: number;
  performance: 'optimal' | 'good' | 'average' | 'poor';
}

export interface OngageESPPerformance {
  esp_id: string;
  esp_name: string;
  connection_id: string;
  connection_title: string;
  campaign_count: number;
  total_sent: number;
  total_delivered: number;
  delivery_rate: number;
  open_rate: number;
  click_rate: number;
  bounce_rate: number;
  complaint_rate: number;
}

export interface OngageAudienceAnalysis {
  segment_id: string;
  segment_name: string;
  campaign_count: number;
  total_targeted: number;
  total_sent: number;
  avg_open_rate: number;
  avg_click_rate: number;
  avg_bounce_rate: number;
  engagement: 'high' | 'medium' | 'low';
}

export interface OngagePipelineMetrics {
  date: string;
  imports_count: number;
  records_imported: number;
  campaigns_sent: number;
  total_targeted: number;
  total_sent: number;
  total_delivered: number;
  delivery_rate: number;
  total_opens: number;
  open_rate: number;
  total_clicks: number;
  click_rate: number;
}

export interface OngageDashboardResponse {
  timestamp: string;
  start_date: string;
  end_date: string;
  range_type: string;
  total_campaigns: number;
  active_campaigns: number;
  last_fetch: string;
  campaigns: OngageCampaign[];
  esp_connections: OngageESPConnection[];
  subject_analysis: OngageSubjectAnalysis[];
  schedule_analysis: OngageScheduleAnalysis[];
  esp_performance: OngageESPPerformance[];
  audience_analysis: OngageAudienceAnalysis[];
  pipeline_metrics: OngagePipelineMetrics[];
  today_imports: number;
  today_targeted: number;
}

export interface OngageCampaignsResponse {
  timestamp: string;
  count: number;
  campaigns: OngageCampaign[];
}

export interface OngageSubjectAnalysisResponse {
  timestamp: string;
  count: number;
  subject_analysis: OngageSubjectAnalysis[];
}

export interface OngageScheduleAnalysisResponse {
  timestamp: string;
  count: number;
  analysis: OngageScheduleAnalysis[];
  optimal_times: OngageScheduleAnalysis[];
  optimal_count: number;
}

export interface OngageESPPerformanceResponse {
  timestamp: string;
  count: number;
  performance: OngageESPPerformance[];
}

export interface OngageAudienceAnalysisResponse {
  timestamp: string;
  count: number;
  audience_analysis: OngageAudienceAnalysis[];
  engagement_counts: {
    high: number;
    medium: number;
    low: number;
  };
}

export interface OngagePipelineResponse {
  timestamp: string;
  days: number;
  pipeline_metrics: OngagePipelineMetrics[];
  totals: {
    campaigns_sent: number;
    total_sent: number;
    total_delivered: number;
    total_opens: number;
    total_clicks: number;
    avg_delivery_rate: number;
    avg_open_rate: number;
    avg_click_rate: number;
  };
}

export interface OngageHealthResponse {
  status: 'healthy' | 'degraded' | 'initializing' | 'disabled';
  timestamp: string;
  last_fetch?: string;
  total_campaigns?: number;
  active_campaigns?: number;
  message?: string;
}

// ========== Everflow Types ==========

export interface EverflowDailyPerformance {
  date: string;
  clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  conversion_rate: number;
  epc: number; // Earnings per click
}

export interface EverflowOfferPerformance {
  offer_id: string;
  offer_name: string;
  clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  conversion_rate: number;
  epc: number;
}

export interface EverflowPropertyPerformance {
  property_code: string;
  property_name: string;
  clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  conversion_rate: number;
  epc: number;
  unique_offers: number;
  // For unattributed revenue categorization
  is_unattributed?: boolean;
  unattrib_reason?: string; // Tooltip explanation
}

export interface EverflowCampaignRevenue {
  mailing_id: string;
  campaign_name?: string;
  property_code: string;
  property_name: string;
  offer_id: string;
  offer_name: string;
  clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  conversion_rate: number;
  epc: number;
  // From Ongage (pre-enriched in background)
  audience_size: number;
  sent: number;
  delivered: number;
  opens: number;
  unique_opens: number;
  email_clicks: number;
  sending_domain: string;
  ongage_linked: boolean;
  // Calculated
  rpm: number;   // Revenue per 1000 sent
  ecpm: number;  // Revenue per 1000 targeted
  revenue_per_open: number;
}

export interface EverflowPeriodPerformance {
  period: string;
  period_type: 'daily' | 'weekly' | 'monthly';
  start_date?: string;
  end_date?: string;
  total_clicks: number;
  total_conversions: number;
  total_revenue: number;
  total_payout: number;
  conversion_rate: number;
  epc: number;
  by_offer?: EverflowOfferPerformance[];
  by_property?: EverflowPropertyPerformance[];
  by_campaign?: EverflowCampaignRevenue[];
}

export interface EverflowClick {
  click_id: string;
  transaction_id: string;
  affiliate_id: string;
  affiliate_name: string;
  offer_id: string;
  offer_name: string;
  sub1: string;
  sub2: string;
  sub3: string;
  timestamp: string;
  ip_address: string;
  device: string;
  browser: string;
  country: string;
  region: string;
  city: string;
  is_failed: boolean;
  property_code: string;
  property_name: string;
  mailing_id: string;
  parsed_offer_id: string;
}

export interface EverflowConversion {
  conversion_id: string;
  transaction_id: string;
  click_id: string;
  affiliate_id: string;
  affiliate_name: string;
  offer_id: string;
  offer_name: string;
  status: string;
  event_name: string;
  revenue: number;
  payout: number;
  revenue_type: string;
  payout_type: string;
  currency: string;
  sub1: string;
  sub2: string;
  sub3: string;
  conversion_time: string;
  click_time: string;
  ip_address: string;
  device: string;
  browser: string;
  country: string;
  region: string;
  city: string;
  property_code: string;
  property_name: string;
  mailing_id: string;
  parsed_offer_id: string;
}

// ========== Everflow API Response Types ==========

export interface EverflowRevenueCategory {
  offer_count: number;
  clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  percentage: number;
}

export interface EverflowDailyBreakdown {
  date: string;
  cpm_revenue: number;
  non_cpm_revenue: number;
  cpm_clicks: number;
  non_cpm_clicks: number;
}

export interface EverflowRevenueBreakdown {
  cpm: EverflowRevenueCategory;
  non_cpm: EverflowRevenueCategory;
  daily_trend: EverflowDailyBreakdown[];
}

// ESP Cost Metrics (from contract calculations)
export interface ESPCostMetrics {
  // Contract details
  monthly_included: number;
  monthly_fee: number;
  overage_rate_per_1000: number;
  
  // Usage for period
  emails_sent: number;
  emails_over_included: number;
  
  // Cost calculations
  pro_rated_base_cost: number;
  overage_cost: number;
  total_cost: number;
  
  // eCPM calculations (cost per 1000 emails)
  cost_ecpm: number;
  revenue_ecpm: number;
  
  // Profitability
  gross_profit: number;
  gross_margin: number;        // As percentage
  net_revenue_per_email: number;
  roi: number;                 // As percentage
}

// ESP Revenue types (SparkPost, Mailgun, SES breakdown)
export interface EverflowESPRevenue {
  esp_name: string;
  campaign_count: number;
  total_sent: number;
  total_delivered: number;
  total_opens: number;
  clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  percentage: number;
  avg_ecpm: number;
  conversion_rate: number;
  epc: number;
  cost_metrics?: ESPCostMetrics;
}

export interface EverflowESPRevenueResponse {
  timestamp: string;
  esp_revenue: EverflowESPRevenue[];
  total_revenue: number;
  total_payout: number;
  total_sent: number;
  total_delivered: number;
  total_clicks: number;
  total_conversions: number;
}

export interface EverflowDashboardResponse {
  timestamp: string;
  last_fetch: string;
  today_clicks: number;
  today_conversions: number;
  today_revenue: number;
  today_payout: number;
  total_revenue: number;
  total_conversions: number;
  daily_performance: EverflowDailyPerformance[];
  offer_performance: EverflowOfferPerformance[];
  property_performance: EverflowPropertyPerformance[];
  campaign_revenue: EverflowCampaignRevenue[];
  revenue_breakdown: EverflowRevenueBreakdown | null;
}

export interface EverflowDailyResponse {
  timestamp: string;
  days: number;
  daily: EverflowDailyPerformance[];
  totals: {
    clicks: number;
    conversions: number;
    revenue: number;
    payout: number;
  };
}

export interface EverflowWeeklyResponse {
  timestamp: string;
  weeks: number;
  weekly: EverflowPeriodPerformance[];
}

export interface EverflowMonthlyResponse {
  timestamp: string;
  months: number;
  monthly: EverflowPeriodPerformance[];
}

export interface EverflowOfferResponse {
  timestamp: string;
  count: number;
  offers: EverflowOfferPerformance[];
  total_revenue: number;
}

export interface EverflowPropertyResponse {
  timestamp: string;
  count: number;
  properties: EverflowPropertyPerformance[];
  total_revenue: number;
}

export interface EverflowCampaignResponse {
  timestamp: string;
  count: number;
  campaigns: EverflowCampaignRevenue[];
  total_revenue: number;
}

export interface EverflowConversionsResponse {
  timestamp: string;
  count: number;
  conversions: EverflowConversion[];
}

export interface EverflowClicksResponse {
  timestamp: string;
  count: number;
  clicks: EverflowClick[];
}

export interface EverflowHealthResponse {
  status: 'healthy' | 'degraded' | 'initializing' | 'disabled';
  timestamp: string;
  last_fetch?: string;
  today_revenue?: number;
  total_revenue?: number;
  message?: string;
}

// Enriched Campaign Details (cross-referenced with Ongage)
export interface SegmentInfo {
  segment_id: string;
  name: string;
  count: number;
  is_suppression: boolean;
}

export interface EnrichedCampaignDetails {
  // Identifiers
  mailing_id: string;
  campaign_name: string;
  
  // From Everflow
  property_code: string;
  property_name: string;
  offer_id: string;
  offer_name: string;
  clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  
  // From Ongage
  subject: string;
  sending_domain: string;
  esp_name: string;
  esp_connection_id: string;
  audience_size: number;
  sent: number;
  delivered: number;
  opens: number;
  unique_opens: number;
  email_clicks: number;         // Unique clickers
  total_email_clicks: number;   // Total clicks (including repeats)
  unique_email_clicks: number;
  bounces: number;              // Total bounces (hard + soft)
  hard_bounces: number;         // Permanent delivery failures
  soft_bounces: number;         // Temporary delivery failures
  failed: number;               // Non-bounce failures
  unsubscribes: number;
  complaints: number;
  schedule_date: string;
  sending_start_date: string;
  sending_end_date: string;
  status: string;
  status_desc: string;
  sending_segments: SegmentInfo[];
  suppression_segments: SegmentInfo[];
  
  // Calculated Metrics
  ecpm: number;
  revenue_per_click: number;
  conversion_rate: number;
  delivery_rate: number;
  open_rate: number;
  click_to_open_rate: number;
  
  // Link status
  ongage_linked: boolean;
  link_error?: string;
}

export interface EnrichedCampaignResponse {
  timestamp: string;
  campaign: EnrichedCampaignDetails;
}
