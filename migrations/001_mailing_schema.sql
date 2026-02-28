-- Ignite Mailing Platform - Database Schema
-- Version: 1.0.0
-- Date: 2026-02-01

-- Enable required extensions
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";

-- ============================================
-- ORGANIZATIONS & USERS
-- ============================================

CREATE TABLE IF NOT EXISTS organizations (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(100) UNIQUE NOT NULL,
    status VARCHAR(20) DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'cancelled')),
    plan VARCHAR(50) DEFAULT 'starter',
    daily_email_limit INTEGER DEFAULT 10000,
    monthly_email_limit INTEGER DEFAULT 300000,
    settings JSONB DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    name VARCHAR(255),
    role VARCHAR(50) DEFAULT 'user' CHECK (role IN ('owner', 'admin', 'user', 'viewer')),
    google_id VARCHAR(255),
    avatar_url TEXT,
    last_login_at TIMESTAMP WITH TIME ZONE,
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(organization_id, email)
);

CREATE TABLE IF NOT EXISTS api_keys (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    name VARCHAR(255) NOT NULL,
    key_hash VARCHAR(64) NOT NULL UNIQUE,
    key_prefix VARCHAR(10) NOT NULL,
    permissions JSONB DEFAULT '["send"]',
    last_used_at TIMESTAMP WITH TIME ZONE,
    expires_at TIMESTAMP WITH TIME ZONE,
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- ============================================
-- MAILING LISTS
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_lists (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    default_from_name VARCHAR(255),
    default_from_email VARCHAR(255),
    default_reply_to VARCHAR(255),
    subscriber_count INTEGER DEFAULT 0,
    active_count INTEGER DEFAULT 0,
    opt_in_type VARCHAR(20) DEFAULT 'single' CHECK (opt_in_type IN ('single', 'double')),
    status VARCHAR(20) DEFAULT 'active' CHECK (status IN ('active', 'archived')),
    settings JSONB DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_lists_org ON mailing_lists(organization_id);
CREATE INDEX idx_lists_status ON mailing_lists(status);

-- ============================================
-- SUBSCRIBERS
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_subscribers (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    list_id UUID NOT NULL REFERENCES mailing_lists(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    email_hash VARCHAR(64) NOT NULL,
    first_name VARCHAR(100),
    last_name VARCHAR(100),
    status VARCHAR(20) DEFAULT 'confirmed' CHECK (status IN ('pending', 'confirmed', 'unsubscribed', 'complained', 'bounced', 'blacklisted')),
    source VARCHAR(50),
    ip_address INET,
    custom_fields JSONB DEFAULT '{}',
    
    -- Engagement tracking
    engagement_score DECIMAL(5,2) DEFAULT 50.0,
    total_emails_received INTEGER DEFAULT 0,
    total_opens INTEGER DEFAULT 0,
    total_clicks INTEGER DEFAULT 0,
    last_open_at TIMESTAMP WITH TIME ZONE,
    last_click_at TIMESTAMP WITH TIME ZONE,
    last_email_at TIMESTAMP WITH TIME ZONE,
    
    -- AI Intelligence
    optimal_send_hour_utc SMALLINT,
    timezone VARCHAR(50),
    churn_risk_score DECIMAL(5,4),
    predicted_ltv DECIMAL(10,2),
    
    -- Timestamps
    subscribed_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    unsubscribed_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(list_id, email)
);

CREATE INDEX idx_subscribers_org ON mailing_subscribers(organization_id);
CREATE INDEX idx_subscribers_list ON mailing_subscribers(list_id);
CREATE INDEX idx_subscribers_email_hash ON mailing_subscribers(email_hash);
CREATE INDEX idx_subscribers_status ON mailing_subscribers(status);
CREATE INDEX idx_subscribers_engagement ON mailing_subscribers(engagement_score DESC);
CREATE INDEX idx_subscribers_optimal_hour ON mailing_subscribers(optimal_send_hour_utc);

-- ============================================
-- SUBSCRIBER INTELLIGENCE (AI Learning)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_subscriber_intelligence (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Learned profiles (JSONB for flexibility)
    engagement_profile JSONB DEFAULT '{}',
    temporal_profile JSONB DEFAULT '{}',
    content_preferences JSONB DEFAULT '{}',
    delivery_profile JSONB DEFAULT '{}',
    risk_profile JSONB DEFAULT '{}',
    predictive_scores JSONB DEFAULT '{}',
    profile_maturity JSONB DEFAULT '{}',
    
    -- Quick access fields
    profile_stage VARCHAR(20) DEFAULT 'new',
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    last_prediction_at TIMESTAMP WITH TIME ZONE,
    
    UNIQUE(subscriber_id)
);

CREATE INDEX idx_intelligence_org ON mailing_subscriber_intelligence(organization_id);

-- ============================================
-- SEGMENTS
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_segments (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    list_id UUID REFERENCES mailing_lists(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    segment_type VARCHAR(20) DEFAULT 'dynamic' CHECK (segment_type IN ('dynamic', 'static')),
    conditions JSONB NOT NULL DEFAULT '[]',
    subscriber_count INTEGER DEFAULT 0,
    last_calculated_at TIMESTAMP WITH TIME ZONE,
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_segments_org ON mailing_segments(organization_id);
CREATE INDEX idx_segments_list ON mailing_segments(list_id);

-- ============================================
-- TEMPLATES
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_template_categories (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name VARCHAR(100) NOT NULL,
    description TEXT,
    sort_order INTEGER DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS mailing_templates (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    category_id UUID REFERENCES mailing_template_categories(id) ON DELETE SET NULL,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    subject VARCHAR(500),
    from_name VARCHAR(255),
    from_email VARCHAR(255),
    reply_to VARCHAR(255),
    html_content TEXT,
    plain_content TEXT,
    preview_text VARCHAR(255),
    thumbnail_url TEXT,
    
    -- Personalization
    personalization_tags JSONB DEFAULT '[]',
    dynamic_content_blocks JSONB DEFAULT '[]',
    
    status VARCHAR(20) DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'archived')),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_templates_org ON mailing_templates(organization_id);
CREATE INDEX idx_templates_category ON mailing_templates(category_id);
CREATE INDEX idx_templates_status ON mailing_templates(status);

-- ============================================
-- DELIVERY SERVERS (ESP Configuration)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_delivery_servers (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    server_type VARCHAR(50) NOT NULL CHECK (server_type IN ('sparkpost', 'ses', 'smtp', 'mailgun')),
    
    -- Connection settings (encrypted)
    credentials_encrypted BYTEA,
    api_key_encrypted BYTEA,
    region VARCHAR(50),
    
    -- Quotas
    hourly_quota INTEGER DEFAULT 1000,
    daily_quota INTEGER DEFAULT 50000,
    monthly_quota INTEGER DEFAULT 1000000,
    used_hourly INTEGER DEFAULT 0,
    used_daily INTEGER DEFAULT 0,
    used_monthly INTEGER DEFAULT 0,
    
    -- Routing
    probability INTEGER DEFAULT 100 CHECK (probability BETWEEN 0 AND 100),
    priority INTEGER DEFAULT 1,
    
    -- Warmup
    warmup_enabled BOOLEAN DEFAULT FALSE,
    warmup_stage VARCHAR(20) DEFAULT 'established',
    warmup_current_limit INTEGER,
    warmup_started_at TIMESTAMP WITH TIME ZONE,
    
    -- Health
    status VARCHAR(20) DEFAULT 'active' CHECK (status IN ('active', 'inactive', 'warmup', 'throttled', 'error')),
    last_error TEXT,
    last_error_at TIMESTAMP WITH TIME ZONE,
    reputation_score DECIMAL(5,2) DEFAULT 100.0,
    
    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_delivery_servers_org ON mailing_delivery_servers(organization_id);
CREATE INDEX idx_delivery_servers_status ON mailing_delivery_servers(status);
CREATE INDEX idx_delivery_servers_type ON mailing_delivery_servers(server_type);

-- ============================================
-- SENDING DOMAINS
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_sending_domains (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    domain VARCHAR(255) NOT NULL,
    
    -- DNS verification
    dkim_verified BOOLEAN DEFAULT FALSE,
    spf_verified BOOLEAN DEFAULT FALSE,
    dmarc_verified BOOLEAN DEFAULT FALSE,
    tracking_domain VARCHAR(255),
    tracking_verified BOOLEAN DEFAULT FALSE,
    
    -- Warmup
    warmup_stage VARCHAR(20) DEFAULT 'cold',
    warmup_daily_limit INTEGER DEFAULT 500,
    warmup_started_at TIMESTAMP WITH TIME ZONE,
    
    -- Health
    reputation_score DECIMAL(5,2) DEFAULT 50.0,
    status VARCHAR(20) DEFAULT 'pending' CHECK (status IN ('pending', 'verified', 'failed', 'suspended')),
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(organization_id, domain)
);

CREATE INDEX idx_sending_domains_org ON mailing_sending_domains(organization_id);

-- ============================================
-- CAMPAIGNS
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_campaigns (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    list_id UUID REFERENCES mailing_lists(id) ON DELETE SET NULL,
    template_id UUID REFERENCES mailing_templates(id) ON DELETE SET NULL,
    segment_id UUID REFERENCES mailing_segments(id) ON DELETE SET NULL,
    
    -- Basic info
    name VARCHAR(255) NOT NULL,
    campaign_type VARCHAR(20) DEFAULT 'regular' CHECK (campaign_type IN ('regular', 'autoresponder', 'transactional', 'ab_test')),
    
    -- Content
    subject VARCHAR(500) NOT NULL,
    from_name VARCHAR(255) NOT NULL,
    from_email VARCHAR(255) NOT NULL,
    reply_to VARCHAR(255),
    html_content TEXT,
    plain_content TEXT,
    preview_text VARCHAR(255),
    
    -- Sending options
    delivery_server_id UUID REFERENCES mailing_delivery_servers(id),
    send_at TIMESTAMP WITH TIME ZONE,
    timezone VARCHAR(50) DEFAULT 'UTC',
    
    -- AI Options
    ai_send_time_optimization BOOLEAN DEFAULT FALSE,
    ai_content_optimization BOOLEAN DEFAULT FALSE,
    ai_audience_optimization BOOLEAN DEFAULT FALSE,
    
    -- Status
    status VARCHAR(20) DEFAULT 'draft' CHECK (status IN (
        'draft', 'scheduled', 'sending', 'paused', 'sent', 'cancelled'
    )),
    
    -- Stats (denormalized for performance)
    total_recipients INTEGER DEFAULT 0,
    sent_count INTEGER DEFAULT 0,
    delivered_count INTEGER DEFAULT 0,
    open_count INTEGER DEFAULT 0,
    unique_open_count INTEGER DEFAULT 0,
    click_count INTEGER DEFAULT 0,
    unique_click_count INTEGER DEFAULT 0,
    bounce_count INTEGER DEFAULT 0,
    complaint_count INTEGER DEFAULT 0,
    unsubscribe_count INTEGER DEFAULT 0,
    revenue DECIMAL(12,2) DEFAULT 0,
    
    -- Timestamps
    started_at TIMESTAMP WITH TIME ZONE,
    completed_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_campaigns_org ON mailing_campaigns(organization_id);
CREATE INDEX idx_campaigns_list ON mailing_campaigns(list_id);
CREATE INDEX idx_campaigns_status ON mailing_campaigns(status);
CREATE INDEX idx_campaigns_send_at ON mailing_campaigns(send_at);

-- ============================================
-- CAMPAIGN QUEUE (Individual sends)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_campaign_queue (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    
    -- Personalized content
    subject VARCHAR(500),
    html_content TEXT,
    plain_content TEXT,
    
    -- Send scheduling
    scheduled_at TIMESTAMP WITH TIME ZONE NOT NULL,
    priority INTEGER DEFAULT 5,
    
    -- AI predictions at queue time
    predicted_open_prob DECIMAL(5,4),
    predicted_click_prob DECIMAL(5,4),
    predicted_revenue DECIMAL(10,4),
    
    -- Status
    status VARCHAR(20) DEFAULT 'queued' CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'skipped')),
    attempts INTEGER DEFAULT 0,
    last_attempt_at TIMESTAMP WITH TIME ZONE,
    error_message TEXT,
    
    -- Result
    message_id VARCHAR(255),
    delivery_server_id UUID,
    sent_at TIMESTAMP WITH TIME ZONE,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_queue_campaign ON mailing_campaign_queue(campaign_id);
CREATE INDEX idx_queue_status ON mailing_campaign_queue(status);
CREATE INDEX idx_queue_scheduled ON mailing_campaign_queue(scheduled_at) WHERE status = 'queued';
CREATE INDEX idx_queue_subscriber ON mailing_campaign_queue(subscriber_id);

-- ============================================
-- TRACKING EVENTS
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_tracking_events (
    id UUID DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL,
    campaign_id UUID,
    subscriber_id UUID,
    email_id UUID,
    
    event_type VARCHAR(20) NOT NULL CHECK (event_type IN (
        'sent', 'delivered', 'deferred', 'bounced', 'opened', 'clicked', 
        'unsubscribed', 'complained', 'rejected'
    )),
    
    -- Event details
    ip_address INET,
    user_agent TEXT,
    device_type VARCHAR(20),
    email_client VARCHAR(50),
    link_url TEXT,
    link_id UUID,
    bounce_type VARCHAR(20),
    bounce_reason TEXT,
    
    -- Timestamps
    event_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    PRIMARY KEY (id, event_at)
) PARTITION BY RANGE (event_at);

-- Create monthly partitions
CREATE TABLE mailing_tracking_events_2026_01 PARTITION OF mailing_tracking_events
    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE mailing_tracking_events_2026_02 PARTITION OF mailing_tracking_events
    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
CREATE TABLE mailing_tracking_events_2026_03 PARTITION OF mailing_tracking_events
    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
CREATE TABLE mailing_tracking_events_2026_04 PARTITION OF mailing_tracking_events
    FOR VALUES FROM ('2026-04-01') TO ('2026-05-01');
CREATE TABLE mailing_tracking_events_2026_05 PARTITION OF mailing_tracking_events
    FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
CREATE TABLE mailing_tracking_events_2026_06 PARTITION OF mailing_tracking_events
    FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');

CREATE INDEX idx_tracking_org ON mailing_tracking_events(organization_id, event_at DESC);
CREATE INDEX idx_tracking_campaign ON mailing_tracking_events(campaign_id, event_at DESC);
CREATE INDEX idx_tracking_subscriber ON mailing_tracking_events(subscriber_id, event_at DESC);

-- ============================================
-- BOUNCES
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_bounces (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    subscriber_id UUID REFERENCES mailing_subscribers(id) ON DELETE SET NULL,
    campaign_id UUID REFERENCES mailing_campaigns(id) ON DELETE SET NULL,
    email VARCHAR(255) NOT NULL,
    
    bounce_type VARCHAR(20) NOT NULL CHECK (bounce_type IN ('hard', 'soft', 'block')),
    bounce_category VARCHAR(50),
    bounce_code VARCHAR(20),
    bounce_message TEXT,
    
    delivery_server_id UUID,
    message_id VARCHAR(255),
    
    processed BOOLEAN DEFAULT FALSE,
    processed_at TIMESTAMP WITH TIME ZONE,
    
    bounced_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_bounces_org ON mailing_bounces(organization_id);
CREATE INDEX idx_bounces_subscriber ON mailing_bounces(subscriber_id);
CREATE INDEX idx_bounces_email ON mailing_bounces(email);
CREATE INDEX idx_bounces_unprocessed ON mailing_bounces(processed) WHERE processed = FALSE;

-- ============================================
-- COMPLAINTS (FBL)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_complaints (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    subscriber_id UUID REFERENCES mailing_subscribers(id) ON DELETE SET NULL,
    campaign_id UUID REFERENCES mailing_campaigns(id) ON DELETE SET NULL,
    email VARCHAR(255) NOT NULL,
    
    complaint_type VARCHAR(50) DEFAULT 'abuse',
    feedback_id VARCHAR(255),
    
    delivery_server_id UUID,
    message_id VARCHAR(255),
    
    processed BOOLEAN DEFAULT FALSE,
    processed_at TIMESTAMP WITH TIME ZONE,
    
    complained_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_complaints_org ON mailing_complaints(organization_id);
CREATE INDEX idx_complaints_subscriber ON mailing_complaints(subscriber_id);
CREATE INDEX idx_complaints_email ON mailing_complaints(email);

-- ============================================
-- AUTORESPONDERS
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_autoresponders (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    list_id UUID NOT NULL REFERENCES mailing_lists(id) ON DELETE CASCADE,
    template_id UUID REFERENCES mailing_templates(id) ON DELETE SET NULL,
    
    name VARCHAR(255) NOT NULL,
    trigger_type VARCHAR(50) NOT NULL CHECK (trigger_type IN ('subscribe', 'date_field', 'profile_update', 'custom')),
    trigger_config JSONB DEFAULT '{}',
    
    -- Delay settings
    delay_value INTEGER DEFAULT 0,
    delay_unit VARCHAR(20) DEFAULT 'hours' CHECK (delay_unit IN ('minutes', 'hours', 'days', 'weeks')),
    
    -- Content
    subject VARCHAR(500) NOT NULL,
    from_name VARCHAR(255),
    from_email VARCHAR(255),
    html_content TEXT,
    plain_content TEXT,
    
    -- Status
    status VARCHAR(20) DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'paused')),
    sent_count INTEGER DEFAULT 0,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_autoresponders_org ON mailing_autoresponders(organization_id);
CREATE INDEX idx_autoresponders_list ON mailing_autoresponders(list_id);
CREATE INDEX idx_autoresponders_status ON mailing_autoresponders(status);

-- ============================================
-- SENDING PLANS (AI Generated)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_sending_plans (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    plan_date DATE NOT NULL,
    status VARCHAR(20) DEFAULT 'draft' CHECK (status IN ('draft', 'approved', 'executing', 'completed', 'cancelled')),
    
    -- Input analysis
    input_summary JSONB NOT NULL,
    
    -- Plan options
    morning_plan JSONB,
    first_half_plan JSONB,
    full_day_plan JSONB,
    
    -- Selected/approved plan
    approved_period VARCHAR(20),
    approved_at TIMESTAMP WITH TIME ZONE,
    approved_by UUID REFERENCES users(id),
    
    -- Execution tracking
    execution_started_at TIMESTAMP WITH TIME ZONE,
    execution_completed_at TIMESTAMP WITH TIME ZONE,
    
    -- Results
    planned_volume INTEGER,
    actual_volume INTEGER,
    planned_revenue DECIMAL(12,2),
    actual_revenue DECIMAL(12,2),
    
    -- AI explanation
    ai_explanation TEXT,
    key_insights JSONB,
    recommendations JSONB,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(organization_id, plan_date)
);

CREATE INDEX idx_sending_plans_org ON mailing_sending_plans(organization_id);
CREATE INDEX idx_sending_plans_date ON mailing_sending_plans(plan_date);
CREATE INDEX idx_sending_plans_status ON mailing_sending_plans(status);

-- ============================================
-- OFFERS (For Revenue Tracking)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_offers (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    external_id VARCHAR(255),
    
    name VARCHAR(255) NOT NULL,
    description TEXT,
    category VARCHAR(100),
    advertiser VARCHAR(255),
    
    payout DECIMAL(10,2) DEFAULT 0,
    payout_type VARCHAR(20) DEFAULT 'cpa',
    
    -- Performance metrics (updated periodically)
    total_sends INTEGER DEFAULT 0,
    total_clicks INTEGER DEFAULT 0,
    total_conversions INTEGER DEFAULT 0,
    total_revenue DECIMAL(12,2) DEFAULT 0,
    epc DECIMAL(8,4) DEFAULT 0,
    conversion_rate DECIMAL(5,4) DEFAULT 0,
    
    -- Fatigue tracking
    fatigue_score DECIMAL(5,4) DEFAULT 0,
    last_sent_at TIMESTAMP WITH TIME ZONE,
    
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_offers_org ON mailing_offers(organization_id);
CREATE INDEX idx_offers_external ON mailing_offers(external_id);
CREATE INDEX idx_offers_category ON mailing_offers(category);

-- ============================================
-- REVENUE ATTRIBUTIONS
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_revenue_attributions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Conversion details
    conversion_id VARCHAR(255) NOT NULL,
    revenue DECIMAL(12,2) NOT NULL,
    offer_id UUID REFERENCES mailing_offers(id),
    
    -- Attribution
    campaign_id UUID REFERENCES mailing_campaigns(id),
    subscriber_id UUID REFERENCES mailing_subscribers(id),
    email_id UUID,
    
    attribution_model VARCHAR(50) DEFAULT 'last_click',
    attribution_weight DECIMAL(5,4) DEFAULT 1.0,
    attributed_revenue DECIMAL(12,2),
    
    -- Context
    click_id VARCHAR(255),
    time_to_conversion INTERVAL,
    touch_points INTEGER DEFAULT 1,
    
    converted_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_attributions_org ON mailing_revenue_attributions(organization_id);
CREATE INDEX idx_attributions_campaign ON mailing_revenue_attributions(campaign_id);
CREATE INDEX idx_attributions_subscriber ON mailing_revenue_attributions(subscriber_id);
CREATE INDEX idx_attributions_converted ON mailing_revenue_attributions(converted_at);

-- ============================================
-- AI MODEL PREDICTIONS (Cache)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_ai_predictions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL,
    subscriber_id UUID NOT NULL,
    
    -- Predictions
    open_probability DECIMAL(5,4),
    click_probability DECIMAL(5,4),
    conversion_probability DECIMAL(5,4),
    expected_revenue DECIMAL(10,4),
    churn_risk DECIMAL(5,4),
    optimal_send_hour SMALLINT,
    
    -- Model versions
    model_version VARCHAR(50),
    
    -- Validity
    valid_until TIMESTAMP WITH TIME ZONE,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_predictions_subscriber ON mailing_ai_predictions(subscriber_id);
CREATE INDEX idx_predictions_valid ON mailing_ai_predictions(valid_until);

-- ============================================
-- AUDIT LOG
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_audit_log (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL,
    user_id UUID,
    
    action VARCHAR(100) NOT NULL,
    entity_type VARCHAR(50) NOT NULL,
    entity_id UUID,
    
    old_values JSONB,
    new_values JSONB,
    
    ip_address INET,
    user_agent TEXT,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_audit_org ON mailing_audit_log(organization_id, created_at DESC);
CREATE INDEX idx_audit_entity ON mailing_audit_log(entity_type, entity_id);

-- ============================================
-- HELPER FUNCTIONS
-- ============================================

-- Function to update subscriber engagement score
CREATE OR REPLACE FUNCTION update_subscriber_engagement() RETURNS TRIGGER AS $$
DECLARE
    v_score DECIMAL(5,2);
    v_days_since_open INTEGER;
    v_open_rate DECIMAL;
    v_click_rate DECIMAL;
BEGIN
    -- Calculate days since last open
    IF NEW.last_open_at IS NOT NULL THEN
        v_days_since_open := EXTRACT(DAY FROM (NOW() - NEW.last_open_at));
    ELSE
        v_days_since_open := 365;
    END IF;
    
    -- Calculate rates
    IF NEW.total_emails_received > 0 THEN
        v_open_rate := NEW.total_opens::DECIMAL / NEW.total_emails_received;
        v_click_rate := NEW.total_clicks::DECIMAL / NEW.total_emails_received;
    ELSE
        v_open_rate := 0;
        v_click_rate := 0;
    END IF;
    
    -- Recency (40%) + Frequency (30%) + Depth (30%)
    v_score := (
        (100.0 * POWER(0.5, v_days_since_open / 14.0) * 0.4) +
        (LEAST(100, v_open_rate * 200) * 0.3) +
        (LEAST(100, v_click_rate * 500) * 0.3)
    );
    
    NEW.engagement_score := GREATEST(0, LEAST(100, v_score));
    NEW.updated_at := NOW();
    
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_update_engagement
    BEFORE UPDATE OF total_opens, total_clicks, last_open_at, last_click_at ON mailing_subscribers
    FOR EACH ROW
    EXECUTE FUNCTION update_subscriber_engagement();

-- Function to update list counts
CREATE OR REPLACE FUNCTION update_list_counts() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' OR TG_OP = 'UPDATE' THEN
        UPDATE mailing_lists SET
            subscriber_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = NEW.list_id),
            active_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = NEW.list_id AND status = 'confirmed'),
            updated_at = NOW()
        WHERE id = NEW.list_id;
    END IF;
    
    IF TG_OP = 'DELETE' THEN
        UPDATE mailing_lists SET
            subscriber_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = OLD.list_id),
            active_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = OLD.list_id AND status = 'confirmed'),
            updated_at = NOW()
        WHERE id = OLD.list_id;
    END IF;
    
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_update_list_counts
    AFTER INSERT OR UPDATE OR DELETE ON mailing_subscribers
    FOR EACH ROW
    EXECUTE FUNCTION update_list_counts();

-- ============================================
-- DEFAULT DATA
-- ============================================

-- Insert default organization for development
INSERT INTO organizations (id, name, slug, plan, daily_email_limit, monthly_email_limit)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    'Ignite Media Group',
    'ignite',
    'enterprise',
    1000000,
    30000000
) ON CONFLICT (slug) DO NOTHING;

COMMIT;
