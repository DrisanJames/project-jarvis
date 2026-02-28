-- Migration: 014_ai_smart_sender.sql
-- Description: AI-Powered Smart Sender with Real-Time Optimization
-- Date: 2026-02-05

-- ============================================================================
-- CAMPAIGN AI OPTIMIZATION SETTINGS
-- ============================================================================
-- Stores AI optimization settings for each campaign

CREATE TABLE IF NOT EXISTS mailing_campaign_ai_settings (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    campaign_id UUID NOT NULL UNIQUE,
    enable_smart_sending BOOLEAN DEFAULT true,
    enable_throttle_optimization BOOLEAN DEFAULT true,
    enable_send_time_optimization BOOLEAN DEFAULT true,
    enable_ab_auto_winner BOOLEAN DEFAULT true,
    target_metric VARCHAR(50) DEFAULT 'opens', -- opens, clicks, conversions, revenue
    min_throttle_rate INTEGER DEFAULT 1000, -- Minimum sends per hour
    max_throttle_rate INTEGER DEFAULT 50000, -- Maximum sends per hour
    current_throttle_rate INTEGER DEFAULT 10000, -- Current sends per hour
    learning_period_minutes INTEGER DEFAULT 60,
    ab_confidence_threshold DECIMAL(3,2) DEFAULT 0.95, -- 95% confidence for winner
    ab_min_sample_size INTEGER DEFAULT 1000, -- Minimum sample per variant
    pause_on_high_complaints BOOLEAN DEFAULT true,
    complaint_threshold DECIMAL(5,4) DEFAULT 0.001, -- 0.1%
    bounce_threshold DECIMAL(5,4) DEFAULT 0.05, -- 5%
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Index for campaign lookups
CREATE INDEX IF NOT EXISTS idx_campaign_ai_settings_campaign 
ON mailing_campaign_ai_settings(campaign_id);

-- ============================================================================
-- REAL-TIME CAMPAIGN METRICS
-- ============================================================================
-- Updated every minute during active send, used for AI decisions

CREATE TABLE IF NOT EXISTS mailing_campaign_realtime_metrics (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    campaign_id UUID NOT NULL,
    timestamp TIMESTAMPTZ DEFAULT NOW(),
    interval_start TIMESTAMPTZ NOT NULL,
    interval_end TIMESTAMPTZ NOT NULL,
    sent_count INTEGER DEFAULT 0,
    delivered_count INTEGER DEFAULT 0,
    open_count INTEGER DEFAULT 0,
    unique_open_count INTEGER DEFAULT 0,
    click_count INTEGER DEFAULT 0,
    unique_click_count INTEGER DEFAULT 0,
    bounce_count INTEGER DEFAULT 0,
    hard_bounce_count INTEGER DEFAULT 0,
    soft_bounce_count INTEGER DEFAULT 0,
    complaint_count INTEGER DEFAULT 0,
    unsubscribe_count INTEGER DEFAULT 0,
    -- Calculated rates (as of this interval)
    open_rate DECIMAL(5,4),
    click_rate DECIMAL(5,4),
    bounce_rate DECIMAL(5,4),
    complaint_rate DECIMAL(6,5),
    -- Cumulative totals
    cumulative_sent INTEGER DEFAULT 0,
    cumulative_opens INTEGER DEFAULT 0,
    cumulative_clicks INTEGER DEFAULT 0,
    cumulative_bounces INTEGER DEFAULT 0,
    cumulative_complaints INTEGER DEFAULT 0,
    -- Throttle info
    current_throttle_rate INTEGER,
    throttle_utilization DECIMAL(5,4),
    -- AI recommendation
    ai_recommendation TEXT,
    ai_confidence DECIMAL(3,2),
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Index for campaign and time-based queries
CREATE INDEX IF NOT EXISTS idx_realtime_metrics_campaign_ts 
ON mailing_campaign_realtime_metrics(campaign_id, timestamp DESC);

-- Index for recent metrics lookup
CREATE INDEX IF NOT EXISTS idx_realtime_metrics_recent 
ON mailing_campaign_realtime_metrics(campaign_id, interval_start DESC)
WHERE interval_start >= NOW() - INTERVAL '1 hour';

-- Partition by month for large-scale deployments (optional)
-- CREATE INDEX IF NOT EXISTS idx_realtime_metrics_partition 
-- ON mailing_campaign_realtime_metrics(timestamp);

-- ============================================================================
-- SUBSCRIBER INBOX PROFILES
-- ============================================================================
-- Stores learned behavior patterns for subscribers by email hash

CREATE TABLE IF NOT EXISTS mailing_inbox_profiles (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    email_hash VARCHAR(64) NOT NULL UNIQUE, -- SHA256 of email (lowercase)
    domain VARCHAR(255) NOT NULL, -- yahoo.com, gmail.com, etc.
    isp VARCHAR(50), -- yahoo, gmail, microsoft, etc.
    -- Optimal send timing
    optimal_send_hour INTEGER, -- 0-23 UTC
    optimal_send_day INTEGER, -- 0-6 (Sunday-Saturday)
    optimal_send_hour_confidence DECIMAL(3,2) DEFAULT 0.5,
    -- Engagement patterns
    avg_open_delay_minutes INTEGER, -- How long after send they typically open
    avg_click_delay_minutes INTEGER, -- How long after open they typically click
    engagement_score DECIMAL(3,2) DEFAULT 0.5, -- 0.00 to 1.00
    engagement_trend VARCHAR(20) DEFAULT 'stable', -- improving, declining, stable
    -- Behavior stats
    last_open_at TIMESTAMPTZ,
    last_click_at TIMESTAMPTZ,
    last_send_at TIMESTAMPTZ,
    total_sends INTEGER DEFAULT 0,
    total_opens INTEGER DEFAULT 0,
    total_clicks INTEGER DEFAULT 0,
    total_bounces INTEGER DEFAULT 0,
    total_complaints INTEGER DEFAULT 0,
    -- Hourly engagement histogram (JSON array of 24 values)
    hourly_open_histogram JSONB DEFAULT '[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]'::jsonb,
    daily_open_histogram JSONB DEFAULT '[0,0,0,0,0,0,0]'::jsonb, -- Sun-Sat
    -- Metadata
    first_seen_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Index for domain lookups
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_domain 
ON mailing_inbox_profiles(domain);

-- Index for ISP lookups
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_isp 
ON mailing_inbox_profiles(isp);

-- Index for email hash lookups (primary use case)
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_hash 
ON mailing_inbox_profiles(email_hash);

-- Index for engagement score queries
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_engagement 
ON mailing_inbox_profiles(engagement_score DESC);

-- Index for optimal send time queries
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_optimal_hour 
ON mailing_inbox_profiles(optimal_send_hour, domain);

-- ============================================================================
-- AI DECISION LOG
-- ============================================================================
-- Audit trail of all AI decisions for transparency and learning

CREATE TABLE IF NOT EXISTS mailing_ai_decisions (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    campaign_id UUID NOT NULL,
    organization_id UUID,
    decision_type VARCHAR(50) NOT NULL, -- throttle_increase, throttle_decrease, pause, resume, variant_winner, alert
    decision_reason TEXT NOT NULL,
    old_value TEXT,
    new_value TEXT,
    metrics_snapshot JSONB NOT NULL, -- Full metrics at time of decision
    ai_model VARCHAR(50) DEFAULT 'rules-based', -- rules-based, claude, etc.
    confidence DECIMAL(3,2),
    applied BOOLEAN DEFAULT true,
    applied_at TIMESTAMPTZ,
    reverted BOOLEAN DEFAULT false,
    reverted_at TIMESTAMPTZ,
    revert_reason TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Index for campaign decision history
CREATE INDEX IF NOT EXISTS idx_ai_decisions_campaign 
ON mailing_ai_decisions(campaign_id, created_at DESC);

-- Index for decision type analysis
CREATE INDEX IF NOT EXISTS idx_ai_decisions_type 
ON mailing_ai_decisions(decision_type, created_at DESC);

-- Index for organization-wide decision history
CREATE INDEX IF NOT EXISTS idx_ai_decisions_org 
ON mailing_ai_decisions(organization_id, created_at DESC);

-- ============================================================================
-- CAMPAIGN A/B TEST VARIANTS
-- ============================================================================
-- Tracks A/B test variants and their performance

CREATE TABLE IF NOT EXISTS mailing_campaign_ab_variants (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    campaign_id UUID NOT NULL,
    variant_name VARCHAR(50) NOT NULL, -- A, B, C, etc.
    variant_type VARCHAR(50) NOT NULL, -- subject, content, from_name, send_time
    variant_value TEXT NOT NULL, -- The actual variant content
    traffic_percentage INTEGER DEFAULT 50, -- % of traffic to this variant
    -- Performance metrics
    sent_count INTEGER DEFAULT 0,
    delivered_count INTEGER DEFAULT 0,
    open_count INTEGER DEFAULT 0,
    unique_open_count INTEGER DEFAULT 0,
    click_count INTEGER DEFAULT 0,
    unique_click_count INTEGER DEFAULT 0,
    conversion_count INTEGER DEFAULT 0,
    revenue DECIMAL(12,2) DEFAULT 0,
    -- Calculated metrics
    open_rate DECIMAL(5,4),
    click_rate DECIMAL(5,4),
    conversion_rate DECIMAL(5,4),
    revenue_per_send DECIMAL(8,4),
    -- Statistical analysis
    z_score DECIMAL(6,4),
    p_value DECIMAL(6,5),
    confidence_level DECIMAL(3,2),
    -- Status
    is_winner BOOLEAN DEFAULT false,
    is_control BOOLEAN DEFAULT false,
    status VARCHAR(20) DEFAULT 'active', -- active, winner, loser, stopped
    declared_winner_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(campaign_id, variant_name)
);

-- Index for campaign variants
CREATE INDEX IF NOT EXISTS idx_ab_variants_campaign 
ON mailing_campaign_ab_variants(campaign_id);

-- Index for finding winners
CREATE INDEX IF NOT EXISTS idx_ab_variants_winner 
ON mailing_campaign_ab_variants(campaign_id, is_winner) 
WHERE is_winner = true;

-- ============================================================================
-- DOMAIN OPTIMAL SEND TIMES
-- ============================================================================
-- Aggregated optimal send times by domain (learned from inbox profiles)

CREATE TABLE IF NOT EXISTS mailing_domain_send_times (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    domain VARCHAR(255) NOT NULL UNIQUE,
    isp VARCHAR(50),
    -- Weekday optimal hours (JSON array of best hours)
    weekday_optimal_hours JSONB DEFAULT '[9, 10, 11, 14, 15]'::jsonb,
    weekend_optimal_hours JSONB DEFAULT '[10, 11, 12, 19, 20]'::jsonb,
    -- Hour-by-hour engagement scores (0-100)
    hourly_engagement_scores JSONB DEFAULT '[]'::jsonb,
    -- Stats
    sample_size INTEGER DEFAULT 0,
    avg_open_rate DECIMAL(5,4),
    avg_click_rate DECIMAL(5,4),
    last_calculated_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Index for domain lookups
CREATE INDEX IF NOT EXISTS idx_domain_send_times_domain 
ON mailing_domain_send_times(domain);

-- Index for ISP lookups
CREATE INDEX IF NOT EXISTS idx_domain_send_times_isp 
ON mailing_domain_send_times(isp);

-- ============================================================================
-- CAMPAIGN ANOMALY ALERTS
-- ============================================================================
-- Stores alerts triggered by anomaly detection

CREATE TABLE IF NOT EXISTS mailing_campaign_alerts (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    campaign_id UUID NOT NULL,
    organization_id UUID,
    alert_type VARCHAR(50) NOT NULL, -- high_bounce, high_complaint, delivery_drop, engagement_drop
    severity VARCHAR(20) NOT NULL, -- info, warning, critical
    title VARCHAR(255) NOT NULL,
    message TEXT NOT NULL,
    metrics_snapshot JSONB,
    threshold_value DECIMAL(10,4),
    actual_value DECIMAL(10,4),
    acknowledged BOOLEAN DEFAULT false,
    acknowledged_by UUID,
    acknowledged_at TIMESTAMPTZ,
    auto_action_taken VARCHAR(100), -- paused, throttled, etc.
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Index for campaign alerts
CREATE INDEX IF NOT EXISTS idx_campaign_alerts_campaign 
ON mailing_campaign_alerts(campaign_id, created_at DESC);

-- Index for unacknowledged alerts
CREATE INDEX IF NOT EXISTS idx_campaign_alerts_unacked 
ON mailing_campaign_alerts(organization_id, acknowledged, created_at DESC)
WHERE acknowledged = false;

-- ============================================================================
-- FUNCTIONS
-- ============================================================================

-- Function to calculate engagement score from open/click history
CREATE OR REPLACE FUNCTION calculate_engagement_score(
    p_total_sends INTEGER,
    p_total_opens INTEGER,
    p_total_clicks INTEGER,
    p_last_open_at TIMESTAMPTZ
) RETURNS DECIMAL(3,2) AS $$
DECLARE
    v_open_rate DECIMAL(5,4);
    v_click_rate DECIMAL(5,4);
    v_recency_score DECIMAL(3,2);
    v_score DECIMAL(3,2);
BEGIN
    -- Base case: new subscriber
    IF p_total_sends = 0 THEN
        RETURN 0.50;
    END IF;
    
    -- Calculate open rate component (0-40 points)
    v_open_rate := LEAST(p_total_opens::DECIMAL / p_total_sends, 1.0);
    
    -- Calculate click rate component (0-30 points)
    IF p_total_opens > 0 THEN
        v_click_rate := LEAST(p_total_clicks::DECIMAL / p_total_opens, 1.0);
    ELSE
        v_click_rate := 0;
    END IF;
    
    -- Calculate recency component (0-30 points)
    IF p_last_open_at IS NOT NULL THEN
        v_recency_score := GREATEST(0, 1 - EXTRACT(EPOCH FROM (NOW() - p_last_open_at)) / (90 * 24 * 3600));
    ELSE
        v_recency_score := 0;
    END IF;
    
    -- Combined score
    v_score := (v_open_rate * 0.40) + (v_click_rate * 0.30) + (v_recency_score * 0.30);
    
    RETURN LEAST(1.00, GREATEST(0.00, v_score));
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- Function to find optimal send hour from histogram
CREATE OR REPLACE FUNCTION find_optimal_send_hour(p_histogram JSONB) RETURNS INTEGER AS $$
DECLARE
    v_max_value INTEGER := 0;
    v_optimal_hour INTEGER := 10; -- Default to 10 AM
    v_hour INTEGER;
    v_value INTEGER;
BEGIN
    FOR v_hour IN 0..23 LOOP
        v_value := COALESCE((p_histogram->>v_hour)::INTEGER, 0);
        IF v_value > v_max_value THEN
            v_max_value := v_value;
            v_optimal_hour := v_hour;
        END IF;
    END LOOP;
    
    RETURN v_optimal_hour;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- Function to update inbox profile from tracking event
CREATE OR REPLACE FUNCTION update_inbox_profile(
    p_email_hash VARCHAR(64),
    p_domain VARCHAR(255),
    p_event_type VARCHAR(50),
    p_event_hour INTEGER,
    p_event_day INTEGER
) RETURNS VOID AS $$
DECLARE
    v_histogram JSONB;
    v_day_histogram JSONB;
BEGIN
    -- Upsert the profile
    INSERT INTO mailing_inbox_profiles (email_hash, domain, total_sends)
    VALUES (p_email_hash, p_domain, 0)
    ON CONFLICT (email_hash) DO NOTHING;
    
    -- Update based on event type
    IF p_event_type = 'sent' THEN
        UPDATE mailing_inbox_profiles
        SET total_sends = total_sends + 1,
            last_send_at = NOW(),
            updated_at = NOW()
        WHERE email_hash = p_email_hash;
        
    ELSIF p_event_type = 'open' OR p_event_type = 'opened' THEN
        -- Get current histogram
        SELECT hourly_open_histogram, daily_open_histogram INTO v_histogram, v_day_histogram
        FROM mailing_inbox_profiles WHERE email_hash = p_email_hash;
        
        -- Update hourly histogram
        v_histogram := jsonb_set(
            COALESCE(v_histogram, '[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]'::jsonb),
            ARRAY[p_event_hour::TEXT],
            to_jsonb(COALESCE((v_histogram->>p_event_hour)::INTEGER, 0) + 1)
        );
        
        -- Update daily histogram
        v_day_histogram := jsonb_set(
            COALESCE(v_day_histogram, '[0,0,0,0,0,0,0]'::jsonb),
            ARRAY[p_event_day::TEXT],
            to_jsonb(COALESCE((v_day_histogram->>p_event_day)::INTEGER, 0) + 1)
        );
        
        UPDATE mailing_inbox_profiles
        SET total_opens = total_opens + 1,
            last_open_at = NOW(),
            hourly_open_histogram = v_histogram,
            daily_open_histogram = v_day_histogram,
            optimal_send_hour = find_optimal_send_hour(v_histogram),
            optimal_send_day = find_optimal_send_hour(v_day_histogram),
            engagement_score = calculate_engagement_score(total_sends, total_opens + 1, total_clicks, NOW()),
            updated_at = NOW()
        WHERE email_hash = p_email_hash;
        
    ELSIF p_event_type = 'click' OR p_event_type = 'clicked' THEN
        UPDATE mailing_inbox_profiles
        SET total_clicks = total_clicks + 1,
            last_click_at = NOW(),
            engagement_score = calculate_engagement_score(total_sends, total_opens, total_clicks + 1, last_open_at),
            updated_at = NOW()
        WHERE email_hash = p_email_hash;
        
    ELSIF p_event_type = 'bounce' OR p_event_type = 'bounced' THEN
        UPDATE mailing_inbox_profiles
        SET total_bounces = total_bounces + 1,
            engagement_score = GREATEST(0, engagement_score - 0.2),
            updated_at = NOW()
        WHERE email_hash = p_email_hash;
        
    ELSIF p_event_type = 'complaint' OR p_event_type = 'complained' THEN
        UPDATE mailing_inbox_profiles
        SET total_complaints = total_complaints + 1,
            engagement_score = 0,
            updated_at = NOW()
        WHERE email_hash = p_email_hash;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- Function to calculate Z-score for A/B test comparison
CREATE OR REPLACE FUNCTION calculate_ab_zscore(
    p_control_conversions INTEGER,
    p_control_total INTEGER,
    p_variant_conversions INTEGER,
    p_variant_total INTEGER
) RETURNS DECIMAL(6,4) AS $$
DECLARE
    v_p1 DECIMAL(10,8);
    v_p2 DECIMAL(10,8);
    v_p_pooled DECIMAL(10,8);
    v_se DECIMAL(10,8);
    v_z DECIMAL(6,4);
BEGIN
    -- Handle edge cases
    IF p_control_total = 0 OR p_variant_total = 0 THEN
        RETURN 0;
    END IF;
    
    -- Conversion rates
    v_p1 := p_control_conversions::DECIMAL / p_control_total;
    v_p2 := p_variant_conversions::DECIMAL / p_variant_total;
    
    -- Pooled proportion
    v_p_pooled := (p_control_conversions + p_variant_conversions)::DECIMAL / (p_control_total + p_variant_total);
    
    -- Standard error
    v_se := SQRT(v_p_pooled * (1 - v_p_pooled) * (1.0/p_control_total + 1.0/p_variant_total));
    
    -- Handle zero SE
    IF v_se = 0 THEN
        RETURN 0;
    END IF;
    
    -- Z-score
    v_z := (v_p2 - v_p1) / v_se;
    
    RETURN v_z;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- Function to get P-value from Z-score (approximation)
CREATE OR REPLACE FUNCTION zscore_to_pvalue(p_z DECIMAL(6,4)) RETURNS DECIMAL(6,5) AS $$
DECLARE
    v_z DECIMAL(10,6);
    v_p DECIMAL(10,8);
BEGIN
    v_z := ABS(p_z);
    
    -- Approximation using the error function
    -- For |z| > 3.5, p-value is essentially 0
    IF v_z > 3.5 THEN
        RETURN 0.00001;
    END IF;
    
    -- Simple approximation for common ranges
    IF v_z >= 2.576 THEN RETURN 0.01;    -- 99% confidence
    ELSIF v_z >= 1.96 THEN RETURN 0.05;  -- 95% confidence
    ELSIF v_z >= 1.645 THEN RETURN 0.10; -- 90% confidence
    ELSIF v_z >= 1.28 THEN RETURN 0.20;  -- 80% confidence
    ELSE RETURN 0.50;                     -- Not significant
    END IF;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- Function to record real-time metrics
CREATE OR REPLACE FUNCTION record_realtime_metrics(
    p_campaign_id UUID,
    p_interval_minutes INTEGER DEFAULT 1
) RETURNS UUID AS $$
DECLARE
    v_id UUID;
    v_interval_start TIMESTAMPTZ;
    v_interval_end TIMESTAMPTZ;
    v_prev_metrics RECORD;
    v_current_metrics RECORD;
BEGIN
    v_interval_end := NOW();
    v_interval_start := v_interval_end - (p_interval_minutes || ' minutes')::INTERVAL;
    
    -- Get previous cumulative totals
    SELECT cumulative_sent, cumulative_opens, cumulative_clicks, cumulative_bounces, cumulative_complaints
    INTO v_prev_metrics
    FROM mailing_campaign_realtime_metrics
    WHERE campaign_id = p_campaign_id
    ORDER BY timestamp DESC
    LIMIT 1;
    
    -- Get current campaign stats
    SELECT sent_count, open_count, click_count, bounce_count, complaint_count
    INTO v_current_metrics
    FROM mailing_campaigns
    WHERE id = p_campaign_id;
    
    -- Insert new metrics record
    INSERT INTO mailing_campaign_realtime_metrics (
        campaign_id, interval_start, interval_end,
        sent_count, open_count, click_count, bounce_count, complaint_count,
        cumulative_sent, cumulative_opens, cumulative_clicks, cumulative_bounces, cumulative_complaints,
        open_rate, click_rate, bounce_rate, complaint_rate
    ) VALUES (
        p_campaign_id, v_interval_start, v_interval_end,
        COALESCE(v_current_metrics.sent_count, 0) - COALESCE(v_prev_metrics.cumulative_sent, 0),
        COALESCE(v_current_metrics.open_count, 0) - COALESCE(v_prev_metrics.cumulative_opens, 0),
        COALESCE(v_current_metrics.click_count, 0) - COALESCE(v_prev_metrics.cumulative_clicks, 0),
        COALESCE(v_current_metrics.bounce_count, 0) - COALESCE(v_prev_metrics.cumulative_bounces, 0),
        COALESCE(v_current_metrics.complaint_count, 0) - COALESCE(v_prev_metrics.cumulative_complaints, 0),
        COALESCE(v_current_metrics.sent_count, 0),
        COALESCE(v_current_metrics.open_count, 0),
        COALESCE(v_current_metrics.click_count, 0),
        COALESCE(v_current_metrics.bounce_count, 0),
        COALESCE(v_current_metrics.complaint_count, 0),
        CASE WHEN v_current_metrics.sent_count > 0 
            THEN v_current_metrics.open_count::DECIMAL / v_current_metrics.sent_count 
            ELSE 0 END,
        CASE WHEN v_current_metrics.sent_count > 0 
            THEN v_current_metrics.click_count::DECIMAL / v_current_metrics.sent_count 
            ELSE 0 END,
        CASE WHEN v_current_metrics.sent_count > 0 
            THEN v_current_metrics.bounce_count::DECIMAL / v_current_metrics.sent_count 
            ELSE 0 END,
        CASE WHEN v_current_metrics.sent_count > 0 
            THEN v_current_metrics.complaint_count::DECIMAL / v_current_metrics.sent_count 
            ELSE 0 END
    ) RETURNING id INTO v_id;
    
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- ============================================================================
-- DEFAULT DATA
-- ============================================================================

-- Insert default domain send times for major ISPs
INSERT INTO mailing_domain_send_times (domain, isp, weekday_optimal_hours, weekend_optimal_hours, sample_size)
VALUES 
    ('gmail.com', 'gmail', '[9, 10, 11, 14, 15]'::jsonb, '[10, 11, 19, 20]'::jsonb, 10000),
    ('yahoo.com', 'yahoo', '[8, 9, 10, 14, 15]'::jsonb, '[10, 11, 18, 19]'::jsonb, 10000),
    ('outlook.com', 'microsoft', '[9, 10, 11, 14, 15, 16]'::jsonb, '[10, 11, 12, 19]'::jsonb, 10000),
    ('hotmail.com', 'microsoft', '[9, 10, 11, 14, 15, 16]'::jsonb, '[10, 11, 12, 19]'::jsonb, 10000),
    ('aol.com', 'aol', '[8, 9, 10, 11, 14]'::jsonb, '[9, 10, 11, 18]'::jsonb, 5000),
    ('icloud.com', 'apple', '[9, 10, 11, 14, 15]'::jsonb, '[10, 11, 19, 20]'::jsonb, 5000)
ON CONFLICT (domain) DO NOTHING;

-- ============================================================================
-- COMMENTS
-- ============================================================================

COMMENT ON TABLE mailing_campaign_ai_settings IS 'AI optimization settings per campaign for smart sending';
COMMENT ON TABLE mailing_campaign_realtime_metrics IS 'Real-time metrics snapshots updated every minute during active sends';
COMMENT ON TABLE mailing_inbox_profiles IS 'Learned subscriber behavior patterns for send time optimization';
COMMENT ON TABLE mailing_ai_decisions IS 'Audit log of all AI-driven decisions for transparency';
COMMENT ON TABLE mailing_campaign_ab_variants IS 'A/B test variant configuration and performance tracking';
COMMENT ON TABLE mailing_domain_send_times IS 'Aggregated optimal send times by domain';
COMMENT ON TABLE mailing_campaign_alerts IS 'Anomaly alerts triggered during campaign sending';

COMMENT ON FUNCTION calculate_engagement_score IS 'Calculates engagement score (0-1) from subscriber activity';
COMMENT ON FUNCTION find_optimal_send_hour IS 'Finds the best hour to send from an engagement histogram';
COMMENT ON FUNCTION update_inbox_profile IS 'Updates inbox profile when tracking events occur';
COMMENT ON FUNCTION calculate_ab_zscore IS 'Calculates Z-score for A/B test statistical significance';
COMMENT ON FUNCTION zscore_to_pvalue IS 'Converts Z-score to approximate P-value';
COMMENT ON FUNCTION record_realtime_metrics IS 'Records a snapshot of real-time campaign metrics';
