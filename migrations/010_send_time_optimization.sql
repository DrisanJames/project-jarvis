-- AI Send Time Optimization - Database Schema
-- Version: 1.0.0
-- Date: 2026-02-05

-- ============================================
-- SUBSCRIBER OPTIMAL SEND TIMES
-- ============================================
-- Stores calculated optimal send times per subscriber
-- based on historical engagement patterns

CREATE TABLE IF NOT EXISTS mailing_subscriber_optimal_times (
    subscriber_id UUID PRIMARY KEY REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    optimal_hour INT NOT NULL CHECK (optimal_hour >= 0 AND optimal_hour <= 23),
    optimal_day INT CHECK (optimal_day >= 0 AND optimal_day <= 6), -- 0=Sunday, 6=Saturday
    timezone VARCHAR(50),
    confidence FLOAT DEFAULT 0.5 CHECK (confidence >= 0 AND confidence <= 1),
    sample_size INT DEFAULT 0,
    last_calculated TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    factors JSONB DEFAULT '{}',
    
    -- Additional optimization data
    best_hours_distribution JSONB DEFAULT '[]',
    best_days_distribution JSONB DEFAULT '[]',
    engagement_patterns JSONB DEFAULT '{}',
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Index for bulk queries
CREATE INDEX IF NOT EXISTS idx_optimal_times_confidence ON mailing_subscriber_optimal_times(confidence DESC);
CREATE INDEX IF NOT EXISTS idx_optimal_times_hour ON mailing_subscriber_optimal_times(optimal_hour);
CREATE INDEX IF NOT EXISTS idx_optimal_times_calculated ON mailing_subscriber_optimal_times(last_calculated);

-- ============================================
-- SEND TIME HISTORY
-- ============================================
-- Tracks when emails were sent and engagement outcomes
-- for machine learning optimization

CREATE TABLE IF NOT EXISTS mailing_send_time_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscriber_id UUID REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    campaign_id UUID REFERENCES mailing_campaigns(id) ON DELETE SET NULL,
    
    -- Send timing details
    sent_at TIMESTAMP WITH TIME ZONE NOT NULL,
    sent_hour INT NOT NULL CHECK (sent_hour >= 0 AND sent_hour <= 23),
    sent_day INT NOT NULL CHECK (sent_day >= 0 AND sent_day <= 6),
    sent_local_hour INT CHECK (sent_local_hour >= 0 AND sent_local_hour <= 23),
    timezone VARCHAR(50),
    
    -- Engagement outcomes
    opened BOOLEAN DEFAULT FALSE,
    clicked BOOLEAN DEFAULT FALSE,
    open_delay_seconds INT, -- Time from send to first open
    click_delay_seconds INT, -- Time from send to first click
    
    -- Additional context
    subject_line VARCHAR(500),
    email_client VARCHAR(100),
    device_type VARCHAR(50),
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Indexes for analytics queries
CREATE INDEX IF NOT EXISTS idx_send_time_history_subscriber ON mailing_send_time_history(subscriber_id);
CREATE INDEX IF NOT EXISTS idx_send_time_history_sent_at ON mailing_send_time_history(sent_at DESC);
CREATE INDEX IF NOT EXISTS idx_send_time_history_hour ON mailing_send_time_history(sent_hour);
CREATE INDEX IF NOT EXISTS idx_send_time_history_day ON mailing_send_time_history(sent_day);
CREATE INDEX IF NOT EXISTS idx_send_time_history_opened ON mailing_send_time_history(subscriber_id, opened) WHERE opened = TRUE;
CREATE INDEX IF NOT EXISTS idx_send_time_history_campaign ON mailing_send_time_history(campaign_id);

-- ============================================
-- AUDIENCE OPTIMAL TIMES (List Level)
-- ============================================
-- Caches audience-level optimal time calculations

CREATE TABLE IF NOT EXISTS mailing_audience_optimal_times (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    list_id UUID REFERENCES mailing_lists(id) ON DELETE CASCADE,
    segment_id UUID REFERENCES mailing_segments(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Calculated optimal times
    best_hours JSONB DEFAULT '[]', -- Array of HourDistribution
    best_days JSONB DEFAULT '[]',  -- Array of DayDistribution
    overall_best_hour INT,
    overall_best_day INT,
    overall_best_time TIMESTAMP WITH TIME ZONE,
    
    -- Statistics
    total_sample_size INT DEFAULT 0,
    confidence_score FLOAT DEFAULT 0.5,
    timezone_distribution JSONB DEFAULT '{}',
    
    -- Metadata
    last_calculated TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    calculation_window_days INT DEFAULT 90,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(list_id, segment_id)
);

CREATE INDEX IF NOT EXISTS idx_audience_optimal_list ON mailing_audience_optimal_times(list_id);
CREATE INDEX IF NOT EXISTS idx_audience_optimal_segment ON mailing_audience_optimal_times(segment_id);
CREATE INDEX IF NOT EXISTS idx_audience_optimal_org ON mailing_audience_optimal_times(organization_id);

-- ============================================
-- CAMPAIGN SCHEDULED SEND TIMES
-- ============================================
-- Per-subscriber scheduled times for AI-optimized campaigns

CREATE TABLE IF NOT EXISTS mailing_campaign_scheduled_times (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    
    scheduled_time TIMESTAMP WITH TIME ZONE NOT NULL,
    local_time TIMESTAMP WITH TIME ZONE,
    timezone VARCHAR(50),
    confidence FLOAT DEFAULT 0.5,
    reasoning TEXT,
    
    -- Tracking
    status VARCHAR(20) DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'skipped', 'failed')),
    sent_at TIMESTAMP WITH TIME ZONE,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(campaign_id, subscriber_id)
);

CREATE INDEX IF NOT EXISTS idx_campaign_scheduled_campaign ON mailing_campaign_scheduled_times(campaign_id);
CREATE INDEX IF NOT EXISTS idx_campaign_scheduled_subscriber ON mailing_campaign_scheduled_times(subscriber_id);
CREATE INDEX IF NOT EXISTS idx_campaign_scheduled_time ON mailing_campaign_scheduled_times(scheduled_time);
CREATE INDEX IF NOT EXISTS idx_campaign_scheduled_status ON mailing_campaign_scheduled_times(status) WHERE status = 'pending';

-- ============================================
-- IP GEOLOCATION CACHE
-- ============================================
-- Cache for IP to timezone lookups

CREATE TABLE IF NOT EXISTS mailing_ip_geolocation_cache (
    ip_address INET PRIMARY KEY,
    timezone VARCHAR(50),
    country_code VARCHAR(2),
    region VARCHAR(100),
    city VARCHAR(100),
    latitude DECIMAL(9,6),
    longitude DECIMAL(9,6),
    cached_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    expires_at TIMESTAMP WITH TIME ZONE DEFAULT NOW() + INTERVAL '30 days'
);

CREATE INDEX IF NOT EXISTS idx_ip_geo_expires ON mailing_ip_geolocation_cache(expires_at);

-- ============================================
-- INDUSTRY DEFAULT SEND TIMES
-- ============================================
-- Default optimal times by industry/category for fallback

CREATE TABLE IF NOT EXISTS mailing_industry_default_times (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    industry VARCHAR(100) NOT NULL,
    category VARCHAR(100),
    
    -- Optimal times (UTC)
    best_hour INT NOT NULL,
    best_day INT,
    
    -- Distribution data
    hours_distribution JSONB DEFAULT '[]',
    days_distribution JSONB DEFAULT '[]',
    
    -- Metadata
    sample_size INT DEFAULT 0,
    source VARCHAR(100) DEFAULT 'system',
    notes TEXT,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(industry, category)
);

-- Insert default industry times based on research
INSERT INTO mailing_industry_default_times (industry, category, best_hour, best_day, hours_distribution, days_distribution) VALUES
('general', NULL, 10, 2, 
 '[{"hour":9,"open_rate":15.2},{"hour":10,"open_rate":17.8},{"hour":11,"open_rate":16.4},{"hour":14,"open_rate":14.9}]'::jsonb,
 '[{"day":1,"open_rate":16.2},{"day":2,"open_rate":17.5},{"day":3,"open_rate":16.8},{"day":4,"open_rate":15.9}]'::jsonb),
('ecommerce', NULL, 10, 1, 
 '[{"hour":10,"open_rate":18.5},{"hour":11,"open_rate":17.2},{"hour":20,"open_rate":15.8}]'::jsonb,
 '[{"day":1,"open_rate":17.8},{"day":4,"open_rate":16.5},{"day":5,"open_rate":15.2}]'::jsonb),
('b2b', NULL, 9, 2,
 '[{"hour":9,"open_rate":19.2},{"hour":10,"open_rate":18.5},{"hour":14,"open_rate":16.8}]'::jsonb,
 '[{"day":1,"open_rate":18.2},{"day":2,"open_rate":19.5},{"day":3,"open_rate":17.8}]'::jsonb),
('newsletter', NULL, 8, 0,
 '[{"hour":8,"open_rate":20.1},{"hour":9,"open_rate":18.8},{"hour":20,"open_rate":16.2}]'::jsonb,
 '[{"day":0,"open_rate":18.5},{"day":6,"open_rate":17.2}]'::jsonb)
ON CONFLICT (industry, category) DO NOTHING;

-- ============================================
-- HELPER FUNCTION: Update optimal times after engagement
-- ============================================

CREATE OR REPLACE FUNCTION update_subscriber_optimal_time_on_open()
RETURNS TRIGGER AS $$
DECLARE
    v_subscriber_id UUID;
    v_sent_hour INT;
    v_sent_day INT;
    v_total_opens INT;
    v_weighted_avg_hour FLOAT;
BEGIN
    -- Only process open events
    IF NEW.event_type != 'opened' THEN
        RETURN NEW;
    END IF;
    
    v_subscriber_id := NEW.subscriber_id;
    
    -- Skip if no subscriber
    IF v_subscriber_id IS NULL THEN
        RETURN NEW;
    END IF;
    
    -- Get the send hour from the queue/campaign
    SELECT 
        EXTRACT(HOUR FROM cq.scheduled_at)::INT,
        EXTRACT(DOW FROM cq.scheduled_at)::INT
    INTO v_sent_hour, v_sent_day
    FROM mailing_campaign_queue cq
    WHERE cq.campaign_id = NEW.campaign_id 
      AND cq.subscriber_id = v_subscriber_id
    LIMIT 1;
    
    IF v_sent_hour IS NULL THEN
        RETURN NEW;
    END IF;
    
    -- Record in send time history
    INSERT INTO mailing_send_time_history (
        subscriber_id, campaign_id, sent_at, sent_hour, sent_day, opened
    ) VALUES (
        v_subscriber_id, NEW.campaign_id, NEW.event_at, v_sent_hour, v_sent_day, TRUE
    ) ON CONFLICT DO NOTHING;
    
    -- Update optimal times if we have enough data
    SELECT COUNT(*), 
           SUM(sent_hour * POWER(0.9, EXTRACT(DAY FROM NOW() - sent_at))) / 
           NULLIF(SUM(POWER(0.9, EXTRACT(DAY FROM NOW() - sent_at))), 0)
    INTO v_total_opens, v_weighted_avg_hour
    FROM mailing_send_time_history
    WHERE subscriber_id = v_subscriber_id AND opened = TRUE;
    
    IF v_total_opens >= 3 THEN
        INSERT INTO mailing_subscriber_optimal_times (
            subscriber_id, optimal_hour, sample_size, confidence, last_calculated
        ) VALUES (
            v_subscriber_id, 
            ROUND(v_weighted_avg_hour)::INT,
            v_total_opens,
            LEAST(0.9, 0.3 + (v_total_opens * 0.06)),
            NOW()
        )
        ON CONFLICT (subscriber_id) DO UPDATE SET
            optimal_hour = ROUND(v_weighted_avg_hour)::INT,
            sample_size = v_total_opens,
            confidence = LEAST(0.9, 0.3 + (v_total_opens * 0.06)),
            last_calculated = NOW(),
            updated_at = NOW();
    END IF;
    
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Create trigger for automatic optimal time updates
DROP TRIGGER IF EXISTS trigger_update_optimal_time_on_open ON mailing_tracking_events;
CREATE TRIGGER trigger_update_optimal_time_on_open
    AFTER INSERT ON mailing_tracking_events
    FOR EACH ROW
    EXECUTE FUNCTION update_subscriber_optimal_time_on_open();

COMMIT;
