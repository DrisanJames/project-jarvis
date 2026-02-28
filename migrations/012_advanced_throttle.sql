-- Migration: 012_advanced_throttle.sql
-- Description: Advanced throttling system for per-domain and per-ISP rate limiting
-- Date: 2026-02-05

-- ============================================================================
-- THROTTLE CONFIGURATION TABLE
-- ============================================================================
-- Stores per-organization throttle configuration including domain and ISP rules

CREATE TABLE IF NOT EXISTS mailing_throttle_configs (
    org_id VARCHAR(100) PRIMARY KEY,
    global_hourly_limit INTEGER DEFAULT 50000,
    global_daily_limit INTEGER DEFAULT 500000,
    domain_rules JSONB DEFAULT '[]'::jsonb,
    isp_rules JSONB DEFAULT '[]'::jsonb,
    auto_adjust BOOLEAN DEFAULT true,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Index for quick config lookups
CREATE INDEX IF NOT EXISTS idx_throttle_configs_updated 
ON mailing_throttle_configs(updated_at);

-- ============================================================================
-- THROTTLE BACKPRESSURE TABLE
-- ============================================================================
-- Stores temporary backpressure (pauses) applied to specific domains

CREATE TABLE IF NOT EXISTS mailing_throttle_backpressure (
    id UUID DEFAULT uuid_generate_v4() PRIMARY KEY,
    org_id VARCHAR(100) NOT NULL,
    domain VARCHAR(255) NOT NULL,
    backoff_until TIMESTAMP WITH TIME ZONE NOT NULL,
    reason VARCHAR(255) DEFAULT 'manual',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(org_id, domain)
);

-- Index for checking active backpressure
CREATE INDEX IF NOT EXISTS idx_throttle_backpressure_active 
ON mailing_throttle_backpressure(org_id, domain, backoff_until) 
WHERE backoff_until > NOW();

-- Index for cleanup of expired backpressure
CREATE INDEX IF NOT EXISTS idx_throttle_backpressure_expired 
ON mailing_throttle_backpressure(backoff_until) 
WHERE backoff_until < NOW();

-- ============================================================================
-- THROTTLE DAILY STATS TABLE
-- ============================================================================
-- Stores daily aggregated statistics for auto-adjustment analysis

CREATE TABLE IF NOT EXISTS mailing_throttle_daily_stats (
    id UUID DEFAULT uuid_generate_v4() PRIMARY KEY,
    org_id VARCHAR(100) NOT NULL,
    domain VARCHAR(255) NOT NULL,
    recorded_at DATE NOT NULL,
    sent_count INTEGER DEFAULT 0,
    bounce_count INTEGER DEFAULT 0,
    complaint_count INTEGER DEFAULT 0,
    soft_bounce_count INTEGER DEFAULT 0,
    open_count INTEGER DEFAULT 0,
    click_count INTEGER DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(org_id, domain, recorded_at)
);

-- Index for querying stats by org and date range
CREATE INDEX IF NOT EXISTS idx_throttle_daily_stats_org_date 
ON mailing_throttle_daily_stats(org_id, recorded_at DESC);

-- Index for querying stats by domain
CREATE INDEX IF NOT EXISTS idx_throttle_daily_stats_domain 
ON mailing_throttle_daily_stats(domain, recorded_at DESC);

-- Index for auto-adjustment queries (7-day lookback)
CREATE INDEX IF NOT EXISTS idx_throttle_daily_stats_recent 
ON mailing_throttle_daily_stats(org_id, domain, recorded_at) 
WHERE recorded_at >= CURRENT_DATE - INTERVAL '7 days';

-- ============================================================================
-- ISP DAILY STATS TABLE
-- ============================================================================
-- Stores daily aggregated statistics at ISP level

CREATE TABLE IF NOT EXISTS mailing_throttle_isp_stats (
    id UUID DEFAULT uuid_generate_v4() PRIMARY KEY,
    org_id VARCHAR(100) NOT NULL,
    isp VARCHAR(50) NOT NULL,
    recorded_at DATE NOT NULL,
    sent_count INTEGER DEFAULT 0,
    bounce_count INTEGER DEFAULT 0,
    complaint_count INTEGER DEFAULT 0,
    defer_count INTEGER DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(org_id, isp, recorded_at)
);

-- Index for ISP stats queries
CREATE INDEX IF NOT EXISTS idx_throttle_isp_stats_org_date 
ON mailing_throttle_isp_stats(org_id, recorded_at DESC);

-- ============================================================================
-- THROTTLE ADJUSTMENT LOG TABLE
-- ============================================================================
-- Audit log of all throttle adjustments (manual and automatic)

CREATE TABLE IF NOT EXISTS mailing_throttle_adjustment_log (
    id UUID DEFAULT uuid_generate_v4() PRIMARY KEY,
    org_id VARCHAR(100) NOT NULL,
    target_type VARCHAR(20) NOT NULL, -- 'domain', 'isp', 'global'
    target_name VARCHAR(255) NOT NULL,
    adjustment_type VARCHAR(50) NOT NULL, -- 'increase', 'decrease', 'backpressure', 'clear_backpressure'
    old_hourly_limit INTEGER,
    new_hourly_limit INTEGER,
    old_daily_limit INTEGER,
    new_daily_limit INTEGER,
    reason VARCHAR(500),
    triggered_by VARCHAR(50) NOT NULL, -- 'auto', 'manual', 'webhook'
    bounce_rate DECIMAL(5,2),
    complaint_rate DECIMAL(5,3),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Index for audit queries
CREATE INDEX IF NOT EXISTS idx_throttle_adjustment_log_org 
ON mailing_throttle_adjustment_log(org_id, created_at DESC);

-- Index for analyzing adjustment patterns
CREATE INDEX IF NOT EXISTS idx_throttle_adjustment_log_target 
ON mailing_throttle_adjustment_log(target_type, target_name, created_at DESC);

-- ============================================================================
-- DOMAIN ISP MAPPING TABLE (for custom domains)
-- ============================================================================
-- Allows mapping custom domains to ISPs (e.g., company.com -> gmail MX)

CREATE TABLE IF NOT EXISTS mailing_domain_isp_mapping (
    domain VARCHAR(255) PRIMARY KEY,
    isp VARCHAR(50) NOT NULL,
    detected_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    mx_records TEXT[],
    verified BOOLEAN DEFAULT false
);

-- Index for ISP lookups
CREATE INDEX IF NOT EXISTS idx_domain_isp_mapping_isp 
ON mailing_domain_isp_mapping(isp);

-- ============================================================================
-- FUNCTIONS
-- ============================================================================

-- Function to log throttle adjustments
CREATE OR REPLACE FUNCTION log_throttle_adjustment(
    p_org_id VARCHAR(100),
    p_target_type VARCHAR(20),
    p_target_name VARCHAR(255),
    p_adjustment_type VARCHAR(50),
    p_old_hourly INTEGER,
    p_new_hourly INTEGER,
    p_old_daily INTEGER,
    p_new_daily INTEGER,
    p_reason VARCHAR(500),
    p_triggered_by VARCHAR(50),
    p_bounce_rate DECIMAL(5,2) DEFAULT NULL,
    p_complaint_rate DECIMAL(5,3) DEFAULT NULL
) RETURNS UUID AS $$
DECLARE
    v_id UUID;
BEGIN
    INSERT INTO mailing_throttle_adjustment_log (
        org_id, target_type, target_name, adjustment_type,
        old_hourly_limit, new_hourly_limit, old_daily_limit, new_daily_limit,
        reason, triggered_by, bounce_rate, complaint_rate
    ) VALUES (
        p_org_id, p_target_type, p_target_name, p_adjustment_type,
        p_old_hourly, p_new_hourly, p_old_daily, p_new_daily,
        p_reason, p_triggered_by, p_bounce_rate, p_complaint_rate
    ) RETURNING id INTO v_id;
    
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- Function to get current throttle status for a domain
CREATE OR REPLACE FUNCTION get_domain_throttle_status(
    p_org_id VARCHAR(100),
    p_domain VARCHAR(255)
) RETURNS TABLE (
    domain VARCHAR(255),
    is_backpressured BOOLEAN,
    backoff_until TIMESTAMP WITH TIME ZONE,
    hourly_limit INTEGER,
    daily_limit INTEGER,
    sent_today INTEGER,
    bounce_rate DECIMAL(5,2),
    complaint_rate DECIMAL(5,3)
) AS $$
BEGIN
    RETURN QUERY
    SELECT 
        p_domain as domain,
        COALESCE(bp.backoff_until > NOW(), false) as is_backpressured,
        bp.backoff_until,
        COALESCE((tc.domain_rules->0->>'hourly_limit')::INTEGER, 5000) as hourly_limit,
        COALESCE((tc.domain_rules->0->>'daily_limit')::INTEGER, 50000) as daily_limit,
        COALESCE(ds.sent_count, 0) as sent_today,
        COALESCE(
            CASE WHEN ds.sent_count > 0 
                THEN (ds.bounce_count::DECIMAL / ds.sent_count * 100)
                ELSE 0 
            END, 0
        ) as bounce_rate,
        COALESCE(
            CASE WHEN ds.sent_count > 0 
                THEN (ds.complaint_count::DECIMAL / ds.sent_count * 100)
                ELSE 0 
            END, 0
        ) as complaint_rate
    FROM (SELECT 1) as dummy
    LEFT JOIN mailing_throttle_backpressure bp 
        ON bp.org_id = p_org_id AND bp.domain = p_domain
    LEFT JOIN mailing_throttle_configs tc 
        ON tc.org_id = p_org_id
    LEFT JOIN mailing_throttle_daily_stats ds 
        ON ds.org_id = p_org_id 
        AND ds.domain = p_domain 
        AND ds.recorded_at = CURRENT_DATE;
END;
$$ LANGUAGE plpgsql;

-- Function to clean up expired backpressure entries
CREATE OR REPLACE FUNCTION cleanup_expired_backpressure() RETURNS INTEGER AS $$
DECLARE
    v_count INTEGER;
BEGIN
    DELETE FROM mailing_throttle_backpressure
    WHERE backoff_until < NOW() - INTERVAL '1 day';
    
    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END;
$$ LANGUAGE plpgsql;

-- Function to aggregate daily stats from hourly data
CREATE OR REPLACE FUNCTION aggregate_throttle_stats(p_org_id VARCHAR(100)) RETURNS VOID AS $$
BEGIN
    -- This function would be called by a scheduled job to aggregate stats
    -- Implementation depends on the source of hourly stats (usually Redis)
    -- For now, this is a placeholder for the aggregation logic
    NULL;
END;
$$ LANGUAGE plpgsql;

-- ============================================================================
-- DEFAULT DATA
-- ============================================================================

-- Insert default configuration for 'default' org
INSERT INTO mailing_throttle_configs (org_id, global_hourly_limit, global_daily_limit, domain_rules, isp_rules, auto_adjust)
VALUES (
    'default',
    50000,
    500000,
    '[]'::jsonb,
    '[
        {"isp": "gmail", "domains": ["gmail.com", "googlemail.com"], "hourly_limit": 10000, "daily_limit": 100000, "burst_limit": 500},
        {"isp": "yahoo", "domains": ["yahoo.com", "yahoo.co.uk", "ymail.com", "rocketmail.com"], "hourly_limit": 8000, "daily_limit": 80000, "burst_limit": 400},
        {"isp": "microsoft", "domains": ["outlook.com", "hotmail.com", "live.com", "msn.com"], "hourly_limit": 10000, "daily_limit": 100000, "burst_limit": 500},
        {"isp": "aol", "domains": ["aol.com", "aim.com"], "hourly_limit": 5000, "daily_limit": 50000, "burst_limit": 250},
        {"isp": "apple", "domains": ["icloud.com", "me.com", "mac.com"], "hourly_limit": 8000, "daily_limit": 80000, "burst_limit": 400}
    ]'::jsonb,
    true
)
ON CONFLICT (org_id) DO NOTHING;

-- ============================================================================
-- COMMENTS
-- ============================================================================

COMMENT ON TABLE mailing_throttle_configs IS 'Stores throttle configuration per organization including domain and ISP rules';
COMMENT ON TABLE mailing_throttle_backpressure IS 'Stores temporary backpressure (pauses) applied to specific domains';
COMMENT ON TABLE mailing_throttle_daily_stats IS 'Daily aggregated statistics for auto-adjustment analysis';
COMMENT ON TABLE mailing_throttle_isp_stats IS 'Daily aggregated statistics at ISP level';
COMMENT ON TABLE mailing_throttle_adjustment_log IS 'Audit log of all throttle adjustments';
COMMENT ON TABLE mailing_domain_isp_mapping IS 'Maps custom domains to ISPs based on MX records';

COMMENT ON FUNCTION log_throttle_adjustment IS 'Logs throttle adjustments for auditing';
COMMENT ON FUNCTION get_domain_throttle_status IS 'Returns current throttle status for a domain';
COMMENT ON FUNCTION cleanup_expired_backpressure IS 'Removes expired backpressure entries';
