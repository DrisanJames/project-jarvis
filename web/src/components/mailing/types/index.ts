export interface List {
  id: string;
  organization_id: string;
  name: string;
  description: string;
  default_from_name: string;
  default_from_email: string;
  default_reply_to: string;
  subscriber_count: number;
  active_count: number;
  opt_in_type: string;
  status: string;
  created_at: string;
  updated_at: string;
}

export interface Subscriber {
  id: string;
  organization_id: string;
  list_id: string;
  email: string;
  first_name: string;
  last_name: string;
  status: 'confirmed' | 'unconfirmed' | 'unsubscribed' | 'bounced' | 'complained';
  source: string;
  engagement_score: number;
  total_emails_received: number;
  total_opens: number;
  total_clicks: number;
  last_open_at: string | null;
  last_click_at: string | null;
  subscribed_at: string;
  created_at: string;
}

export interface Campaign {
  id: string;
  organization_id: string;
  list_id: string | null;
  template_id: string | null;
  segment_id: string | null;
  name: string;
  campaign_type: string;
  subject: string;
  from_name: string;
  from_email: string;
  reply_to: string;
  html_content: string;
  plain_content: string;
  preview_text: string;
  delivery_server_id: string | null;
  send_at: string | null;
  timezone: string;
  ai_send_time_optimization: boolean;
  ai_content_optimization: boolean;
  ai_audience_optimization: boolean;
  status: 'draft' | 'queued' | 'sending' | 'sent' | 'paused' | 'failed';
  total_recipients: number;
  sent_count: number;
  delivered_count: number;
  open_count: number;
  unique_open_count: number;
  click_count: number;
  unique_click_count: number;
  bounce_count: number;
  complaint_count: number;
  unsubscribe_count: number;
  revenue: number;
  started_at: string | null;
  completed_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface CampaignStats {
  open_rate: number;
  click_rate: number;
  bounce_rate: number;
  complaint_rate: number;
  unsubscribe_rate: number;
  ctr: number;
  revenue_per_send: number;
}

export interface DeliveryServer {
  id: string;
  organization_id: string;
  name: string;
  server_type: 'sparkpost' | 'ses' | 'mailgun' | 'smtp';
  region: string;
  hourly_quota: number;
  daily_quota: number;
  monthly_quota: number;
  used_hourly: number;
  used_daily: number;
  used_monthly: number;
  probability: number;
  priority: number;
  warmup_enabled: boolean;
  warmup_stage: number;
  status: 'active' | 'inactive' | 'warmup';
  reputation_score: number;
  created_at: string;
}

export interface SendingPlan {
  time_period: 'morning' | 'first_half' | 'full_day';
  name: string;
  description: string;
  recommended_volume: number;
  time_slots: TimeSlotPlan[];
  audience_breakdown: AudienceSegment[];
  offer_recommendations: OfferRecommendation[];
  predictions: PlanPredictions;
  confidence_score: number;
  ai_explanation: string;
  warnings: string[];
  recommendations: string[];
}

export interface TimeSlotPlan {
  start_time: string;
  end_time: string;
  volume: number;
  priority: string;
  target_audience: string;
}

export interface AudienceSegment {
  name: string;
  count: number;
  engagement_level: string;
  predicted_open_rate: number;
  predicted_click_rate: number;
  recommended_action: string;
}

export interface OfferRecommendation {
  offer_id: string;
  offer_name: string;
  match_score: number;
  predicted_epc: number;
  recommended_volume: number;
  reason: string;
}

export interface PlanPredictions {
  estimated_opens: number;
  estimated_clicks: number;
  estimated_revenue: number;
  estimated_bounce_rate: number;
  estimated_complaint_rate: number;
  revenue_range: [number, number];
  confidence_interval: number;
}

export interface DashboardData {
  overview: {
    total_subscribers: number;
    total_lists: number;
    total_campaigns: number;
    daily_capacity: number;
    daily_used: number;
  };
  performance: {
    total_sent: number;
    total_opens: number;
    total_clicks: number;
    total_revenue: number;
    open_rate: number;
    click_rate: number;
  };
  recent_campaigns: Campaign[];
}
