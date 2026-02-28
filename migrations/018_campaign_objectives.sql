-- Campaign Objectives & Business Goals
-- Version: 018
-- Date: 2026-02-05
-- Description: Adds campaign purpose/objective tracking for AI-driven optimization
--              Supports Data Activation vs Offer Revenue campaign types

-- ============================================================================
-- CAMPAIGN OBJECTIVES
-- ============================================================================
-- Stores the business purpose and goals for each campaign
-- This informs the AI agent how to optimize the campaign

CREATE TABLE IF NOT EXISTS mailing_campaign_objectives (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    campaign_id UUID NOT NULL UNIQUE REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Purpose Classification
    -- 'data_activation': Warm up new data, build reputation, activate emails
    -- 'offer_revenue': Drive CPM/CPL conversions and revenue
    purpose VARCHAR(30) NOT NULL CHECK (purpose IN ('data_activation', 'offer_revenue')),
    
    -- ============================================
    -- Data Activation Settings
    -- ============================================
    -- Goal: warm_new_data, reactivate_cold, domain_warmup
    activation_goal VARCHAR(50),
    -- Target engagement rate (open rate) for activation success
    target_engagement_rate DECIMAL(5,4),
    -- Maximum acceptable complaint rate before pausing
    target_clean_rate DECIMAL(5,4) DEFAULT 0.0010,
    -- Domain warmup specific settings
    warmup_daily_increment INTEGER DEFAULT 1000,
    warmup_max_daily_volume INTEGER DEFAULT 50000,
    
    -- ============================================
    -- Offer Revenue Settings (CPM/CPL/CPA)
    -- ============================================
    offer_model VARCHAR(10) CHECK (offer_model IN ('cpm', 'cpl', 'cpa', 'hybrid')),
    -- ECM (Effective Cost per Mille) target from ESP contracts
    ecpm_target DECIMAL(10,2),
    -- Budget limit for this campaign ($2000, $3000, $4000, etc.)
    budget_limit DECIMAL(10,2),
    budget_spent DECIMAL(10,2) DEFAULT 0,
    -- Target metric to optimize for
    target_metric VARCHAR(20) CHECK (target_metric IN ('clicks', 'conversions', 'revenue', 'ecpm')),
    -- Target value (number of clicks/conversions targeted)
    target_value INTEGER,
    target_achieved INTEGER DEFAULT 0,
    
    -- ============================================
    -- Everflow Integration
    -- ============================================
    -- Linked Everflow offer IDs for attribution
    everflow_offer_ids JSONB DEFAULT '[]',
    -- Custom sub ID mappings for tracking
    everflow_sub_id_template VARCHAR(255),
    -- Property code for this campaign (FTT, DHF, etc.)
    property_code VARCHAR(20),
    
    -- ============================================
    -- Creative Rotation Settings
    -- ============================================
    -- Array of approved creatives: [{subject, preheader, template_id, approved_at}]
    approved_creatives JSONB DEFAULT '[]',
    -- Rotation strategy: performance (best wins), round_robin, weighted, explore
    rotation_strategy VARCHAR(30) DEFAULT 'performance' CHECK (rotation_strategy IN ('performance', 'round_robin', 'weighted', 'explore')),
    -- Current creative index for round robin
    current_creative_index INTEGER DEFAULT 0,
    
    -- ============================================
    -- AI Behavior Configuration
    -- ============================================
    -- Master switch for AI optimization
    ai_optimization_enabled BOOLEAN DEFAULT true,
    -- Auto-adjust throughput based on ISP signals
    ai_throughput_optimization BOOLEAN DEFAULT true,
    -- Auto-rotate creatives based on performance
    ai_creative_rotation BOOLEAN DEFAULT true,
    -- Auto-pace budget spend over campaign duration
    ai_budget_pacing BOOLEAN DEFAULT true,
    -- Monitor SparkPost/SES events for deliverability signals
    esp_signal_monitoring BOOLEAN DEFAULT true,
    
    -- ============================================
    -- Thresholds for AI Actions
    -- ============================================
    -- Pause sending if spam signals exceed this rate
    pause_on_spam_signal BOOLEAN DEFAULT true,
    spam_signal_threshold DECIMAL(5,4) DEFAULT 0.0010,
    -- Bounce rate threshold for action
    bounce_threshold DECIMAL(5,4) DEFAULT 0.0500,
    -- How aggressively to adjust throughput: low, medium, high
    throughput_sensitivity VARCHAR(20) DEFAULT 'medium' CHECK (throughput_sensitivity IN ('low', 'medium', 'high')),
    -- Minimum sends per hour (floor)
    min_throughput_rate INTEGER DEFAULT 1000,
    -- Maximum sends per hour (ceiling)
    max_throughput_rate INTEGER DEFAULT 100000,
    
    -- ============================================
    -- Campaign Pacing
    -- ============================================
    -- Target completion time (for budget/volume pacing)
    target_completion_hours INTEGER,
    -- Pacing strategy: aggressive (front-load), even (spread), conservative (slow start)
    pacing_strategy VARCHAR(20) DEFAULT 'even' CHECK (pacing_strategy IN ('aggressive', 'even', 'conservative')),
    
    -- Timestamps
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Indexes for efficient lookups
CREATE INDEX IF NOT EXISTS idx_campaign_objectives_campaign 
ON mailing_campaign_objectives(campaign_id);

CREATE INDEX IF NOT EXISTS idx_campaign_objectives_org 
ON mailing_campaign_objectives(organization_id);

CREATE INDEX IF NOT EXISTS idx_campaign_objectives_purpose 
ON mailing_campaign_objectives(purpose);

CREATE INDEX IF NOT EXISTS idx_campaign_objectives_property 
ON mailing_campaign_objectives(property_code);

-- ============================================================================
-- ESP SIGNAL MONITORING
-- ============================================================================
-- Tracks deliverability signals from SparkPost, SES, Mailgun
-- Used by AI to determine ISP behavior and adjust strategy

CREATE TABLE IF NOT EXISTS mailing_esp_signals (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- ESP identification
    esp_type VARCHAR(20) NOT NULL CHECK (esp_type IN ('sparkpost', 'ses', 'mailgun')),
    
    -- Signal classification
    signal_type VARCHAR(50) NOT NULL CHECK (signal_type IN (
        'delivered',           -- Successfully delivered
        'bounced_hard',        -- Permanent delivery failure
        'bounced_soft',        -- Temporary delivery failure
        'spam_complaint',      -- Recipient marked as spam
        'policy_rejection',    -- Rejected by ISP policy
        'throttled',           -- ISP is throttling
        'deferred',            -- Temporarily deferred
        'blocked',             -- Blocked by ISP
        'out_of_band_bounce'   -- Delayed bounce
    )),
    
    -- ISP identification
    isp VARCHAR(50), -- yahoo, gmail, microsoft, aol, comcast, etc.
    receiving_domain VARCHAR(255), -- Full domain (yahoo.com, gmail.com)
    
    -- Signal details
    signal_count INTEGER DEFAULT 1,
    -- Sample message IDs for debugging
    sample_message_ids JSONB DEFAULT '[]',
    -- Raw bounce/error codes
    bounce_class VARCHAR(50),
    error_code VARCHAR(50),
    error_message TEXT,
    
    -- Time window this signal covers
    interval_start TIMESTAMPTZ NOT NULL,
    interval_end TIMESTAMPTZ NOT NULL,
    
    -- AI interpretation
    ai_interpretation TEXT, -- "Yahoo is treating mail as spam", "Gmail accepting normally"
    ai_severity VARCHAR(20) CHECK (ai_severity IN ('info', 'warning', 'critical')),
    recommended_action VARCHAR(50), -- pause, reduce_throttle, rotate_creative, continue
    action_taken BOOLEAN DEFAULT false,
    action_taken_at TIMESTAMPTZ,
    
    -- Timestamp
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Indexes for signal analysis
CREATE INDEX IF NOT EXISTS idx_esp_signals_campaign 
ON mailing_esp_signals(campaign_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_esp_signals_isp 
ON mailing_esp_signals(isp, signal_type, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_esp_signals_type 
ON mailing_esp_signals(signal_type, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_esp_signals_interval 
ON mailing_esp_signals(campaign_id, interval_start DESC);

-- Partial index for unactioned critical signals
CREATE INDEX IF NOT EXISTS idx_esp_signals_critical_unactioned
ON mailing_esp_signals(campaign_id, created_at DESC)
WHERE ai_severity = 'critical' AND action_taken = false;

-- ============================================================================
-- ISP BEHAVIOR ANALYSIS
-- ============================================================================
-- Aggregated ISP behavior patterns learned over time

CREATE TABLE IF NOT EXISTS mailing_isp_behavior (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- ISP identification
    isp VARCHAR(50) NOT NULL,
    receiving_domain VARCHAR(255),
    
    -- Current behavior assessment
    behavior_status VARCHAR(30) DEFAULT 'normal' CHECK (behavior_status IN (
        'normal',           -- Accepting mail normally
        'throttling',       -- Rate limiting our mail
        'suspicious',       -- Showing signs of concern
        'blocking',         -- Actively blocking
        'spam_filtering'    -- Putting mail in spam folder
    )),
    
    -- Metrics (rolling 24h window)
    delivery_rate DECIMAL(5,4),
    bounce_rate DECIMAL(5,4),
    spam_complaint_rate DECIMAL(6,5),
    open_rate DECIMAL(5,4),
    
    -- Optimal sending parameters for this ISP
    recommended_hourly_volume INTEGER,
    recommended_send_hours JSONB DEFAULT '[]', -- Best hours to send (UTC)
    
    -- Learning metadata
    sample_size INTEGER DEFAULT 0,
    confidence_score DECIMAL(3,2) DEFAULT 0.50,
    last_analyzed_at TIMESTAMPTZ,
    
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    
    UNIQUE(organization_id, isp)
);

CREATE INDEX IF NOT EXISTS idx_isp_behavior_org 
ON mailing_isp_behavior(organization_id);

CREATE INDEX IF NOT EXISTS idx_isp_behavior_status 
ON mailing_isp_behavior(behavior_status);

-- ============================================================================
-- CREATIVE PERFORMANCE TRACKING
-- ============================================================================
-- Tracks performance of each creative variant for rotation decisions

CREATE TABLE IF NOT EXISTS mailing_creative_performance (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Creative identification
    creative_index INTEGER NOT NULL, -- Index in approved_creatives array
    subject_line VARCHAR(500),
    preheader VARCHAR(255),
    template_id UUID,
    
    -- Performance metrics
    sent_count INTEGER DEFAULT 0,
    delivered_count INTEGER DEFAULT 0,
    open_count INTEGER DEFAULT 0,
    unique_open_count INTEGER DEFAULT 0,
    click_count INTEGER DEFAULT 0,
    unique_click_count INTEGER DEFAULT 0,
    conversion_count INTEGER DEFAULT 0,
    revenue DECIMAL(12,2) DEFAULT 0,
    
    -- Calculated rates
    open_rate DECIMAL(5,4),
    click_rate DECIMAL(5,4),
    click_to_open_rate DECIMAL(5,4),
    conversion_rate DECIMAL(5,4),
    revenue_per_send DECIMAL(8,4),
    ecpm DECIMAL(10,2),
    
    -- Statistical confidence
    z_score DECIMAL(6,4),
    confidence_level DECIMAL(3,2),
    is_winner BOOLEAN DEFAULT false,
    
    -- Status
    status VARCHAR(20) DEFAULT 'active' CHECK (status IN ('active', 'winner', 'underperforming', 'paused')),
    
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    
    UNIQUE(campaign_id, creative_index)
);

CREATE INDEX IF NOT EXISTS idx_creative_perf_campaign 
ON mailing_creative_performance(campaign_id);

CREATE INDEX IF NOT EXISTS idx_creative_perf_winner 
ON mailing_creative_performance(campaign_id, is_winner)
WHERE is_winner = true;

-- ============================================================================
-- CAMPAIGN OPTIMIZATION LOG
-- ============================================================================
-- Detailed log of all AI optimization actions for audit and learning

CREATE TABLE IF NOT EXISTS mailing_campaign_optimization_log (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Optimization context
    optimization_type VARCHAR(50) NOT NULL CHECK (optimization_type IN (
        'throughput_increase',
        'throughput_decrease',
        'creative_rotation',
        'creative_winner',
        'pause_campaign',
        'resume_campaign',
        'budget_pace_adjust',
        'isp_throttle_response',
        'spam_signal_response',
        'target_achieved'
    )),
    
    -- What triggered this optimization
    trigger_reason TEXT NOT NULL,
    trigger_metrics JSONB NOT NULL, -- Snapshot of metrics at decision time
    
    -- The decision made
    old_value TEXT,
    new_value TEXT,
    
    -- AI reasoning
    ai_reasoning TEXT,
    ai_confidence DECIMAL(3,2),
    
    -- Outcome tracking
    applied BOOLEAN DEFAULT true,
    applied_at TIMESTAMPTZ,
    outcome_measured BOOLEAN DEFAULT false,
    outcome_positive BOOLEAN,
    outcome_notes TEXT,
    
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_optimization_log_campaign 
ON mailing_campaign_optimization_log(campaign_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_optimization_log_type 
ON mailing_campaign_optimization_log(optimization_type, created_at DESC);

-- ============================================================================
-- FUNCTIONS
-- ============================================================================

-- Function to get current campaign objective status
CREATE OR REPLACE FUNCTION get_campaign_objective_status(p_campaign_id UUID)
RETURNS JSONB AS $$
DECLARE
    v_result JSONB;
BEGIN
    SELECT jsonb_build_object(
        'purpose', co.purpose,
        'budget_progress', CASE 
            WHEN co.budget_limit > 0 THEN (co.budget_spent / co.budget_limit)
            ELSE 0 
        END,
        'target_progress', CASE 
            WHEN co.target_value > 0 THEN (co.target_achieved::decimal / co.target_value)
            ELSE 0 
        END,
        'current_ecpm', CASE 
            WHEN c.sent_count > 0 THEN (c.revenue / c.sent_count) * 1000
            ELSE 0 
        END,
        'ecpm_vs_target', CASE 
            WHEN co.ecpm_target > 0 AND c.sent_count > 0 
            THEN ((c.revenue / c.sent_count) * 1000) / co.ecpm_target
            ELSE 0 
        END
    ) INTO v_result
    FROM mailing_campaign_objectives co
    JOIN mailing_campaigns c ON c.id = co.campaign_id
    WHERE co.campaign_id = p_campaign_id;
    
    RETURN COALESCE(v_result, '{}'::jsonb);
END;
$$ LANGUAGE plpgsql;

-- Function to determine if campaign should pause based on signals
CREATE OR REPLACE FUNCTION should_pause_campaign(p_campaign_id UUID)
RETURNS BOOLEAN AS $$
DECLARE
    v_spam_rate DECIMAL(6,5);
    v_threshold DECIMAL(5,4);
    v_pause_enabled BOOLEAN;
BEGIN
    -- Get campaign objective settings
    SELECT spam_signal_threshold, pause_on_spam_signal
    INTO v_threshold, v_pause_enabled
    FROM mailing_campaign_objectives
    WHERE campaign_id = p_campaign_id;
    
    IF NOT v_pause_enabled THEN
        RETURN FALSE;
    END IF;
    
    -- Calculate recent spam rate from signals (last 15 minutes)
    SELECT COALESCE(
        SUM(CASE WHEN signal_type = 'spam_complaint' THEN signal_count ELSE 0 END)::decimal /
        NULLIF(SUM(CASE WHEN signal_type = 'delivered' THEN signal_count ELSE 0 END), 0),
        0
    ) INTO v_spam_rate
    FROM mailing_esp_signals
    WHERE campaign_id = p_campaign_id
      AND created_at >= NOW() - INTERVAL '15 minutes';
    
    RETURN v_spam_rate > v_threshold;
END;
$$ LANGUAGE plpgsql;

-- ============================================================================
-- TRIGGERS
-- ============================================================================

-- Trigger to update updated_at timestamp
CREATE OR REPLACE FUNCTION update_campaign_objectives_timestamp()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_campaign_objectives_updated
BEFORE UPDATE ON mailing_campaign_objectives
FOR EACH ROW EXECUTE FUNCTION update_campaign_objectives_timestamp();

CREATE TRIGGER trigger_isp_behavior_updated
BEFORE UPDATE ON mailing_isp_behavior
FOR EACH ROW EXECUTE FUNCTION update_campaign_objectives_timestamp();

CREATE TRIGGER trigger_creative_performance_updated
BEFORE UPDATE ON mailing_creative_performance
FOR EACH ROW EXECUTE FUNCTION update_campaign_objectives_timestamp();

-- ============================================================================
-- VIEWS
-- ============================================================================

-- View for campaign objective dashboard
CREATE OR REPLACE VIEW v_campaign_objective_status AS
SELECT 
    co.id,
    co.campaign_id,
    co.organization_id,
    c.name as campaign_name,
    c.status as campaign_status,
    co.purpose,
    co.offer_model,
    co.budget_limit,
    co.budget_spent,
    CASE WHEN co.budget_limit > 0 
         THEN ROUND((co.budget_spent / co.budget_limit) * 100, 2) 
         ELSE 0 
    END as budget_progress_pct,
    co.target_metric,
    co.target_value,
    co.target_achieved,
    CASE WHEN co.target_value > 0 
         THEN ROUND((co.target_achieved::decimal / co.target_value) * 100, 2) 
         ELSE 0 
    END as target_progress_pct,
    co.ecpm_target,
    CASE WHEN c.sent_count > 0 
         THEN ROUND((c.revenue / c.sent_count) * 1000, 2) 
         ELSE 0 
    END as current_ecpm,
    co.rotation_strategy,
    co.ai_optimization_enabled,
    co.created_at,
    co.updated_at
FROM mailing_campaign_objectives co
JOIN mailing_campaigns c ON c.id = co.campaign_id;

-- View for ISP health dashboard
CREATE OR REPLACE VIEW v_isp_health_status AS
SELECT 
    isp,
    behavior_status,
    delivery_rate,
    bounce_rate,
    spam_complaint_rate,
    recommended_hourly_volume,
    confidence_score,
    last_analyzed_at,
    CASE 
        WHEN behavior_status = 'normal' THEN 'green'
        WHEN behavior_status IN ('throttling', 'suspicious') THEN 'yellow'
        ELSE 'red'
    END as health_color
FROM mailing_isp_behavior
WHERE last_analyzed_at >= NOW() - INTERVAL '24 hours';
