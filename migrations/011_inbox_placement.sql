-- ============================================
-- INBOX PLACEMENT & DELIVERABILITY SYSTEM
-- Migration 011: Intelligent inbox placement monitoring
-- ============================================

-- ============================================
-- SEED LISTS TABLE
-- Stores collections of seed email addresses for inbox testing
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_seed_lists (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    name VARCHAR(255) NOT NULL,
    seeds JSONB NOT NULL DEFAULT '[]',
    provider VARCHAR(50) NOT NULL DEFAULT 'internal', -- internal, emailonacid, litmus, glockapps
    is_active BOOLEAN DEFAULT true,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Indexes for seed lists
CREATE INDEX IF NOT EXISTS idx_seed_lists_org ON mailing_seed_lists(org_id);
CREATE INDEX IF NOT EXISTS idx_seed_lists_active ON mailing_seed_lists(org_id, is_active) WHERE is_active = true;
CREATE INDEX IF NOT EXISTS idx_seed_lists_provider ON mailing_seed_lists(provider);

-- ============================================
-- INBOX TEST RESULTS TABLE
-- Stores results from inbox placement tests
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_inbox_test_results (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL,
    seed_list_id UUID NOT NULL REFERENCES mailing_seed_lists(id) ON DELETE CASCADE,
    test_date TIMESTAMP WITH TIME ZONE NOT NULL,
    overall_score NUMERIC(5,2) DEFAULT 0, -- 0-100
    inbox_rate NUMERIC(5,2) DEFAULT 0,    -- percentage
    spam_rate NUMERIC(5,2) DEFAULT 0,     -- percentage
    missing_rate NUMERIC(5,2) DEFAULT 0,  -- percentage
    isp_results JSONB NOT NULL DEFAULT '[]',
    status VARCHAR(50) NOT NULL DEFAULT 'pending', -- pending, running, completed, failed
    error_message TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    completed_at TIMESTAMP WITH TIME ZONE
);

-- Indexes for test results
CREATE INDEX IF NOT EXISTS idx_inbox_test_campaign ON mailing_inbox_test_results(campaign_id);
CREATE INDEX IF NOT EXISTS idx_inbox_test_seed_list ON mailing_inbox_test_results(seed_list_id);
CREATE INDEX IF NOT EXISTS idx_inbox_test_date ON mailing_inbox_test_results(test_date DESC);
CREATE INDEX IF NOT EXISTS idx_inbox_test_status ON mailing_inbox_test_results(status);

-- ============================================
-- IP WARMUP PLANS TABLE
-- Stores IP warmup schedules and progress
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_ip_warmup_plans (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    ip_address VARCHAR(45) NOT NULL, -- Supports IPv4 and IPv6
    plan_type VARCHAR(50) NOT NULL DEFAULT 'conservative', -- conservative, aggressive, custom
    start_date TIMESTAMP WITH TIME ZONE NOT NULL,
    current_day INT DEFAULT 1,
    total_days INT NOT NULL,
    daily_schedule JSONB NOT NULL DEFAULT '[]',
    status VARCHAR(50) NOT NULL DEFAULT 'active', -- active, paused, completed, cancelled
    notes TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Indexes for warmup plans
CREATE INDEX IF NOT EXISTS idx_warmup_plans_org ON mailing_ip_warmup_plans(org_id);
CREATE INDEX IF NOT EXISTS idx_warmup_plans_ip ON mailing_ip_warmup_plans(ip_address);
CREATE INDEX IF NOT EXISTS idx_warmup_plans_status ON mailing_ip_warmup_plans(status) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_warmup_plans_start ON mailing_ip_warmup_plans(start_date);

-- ============================================
-- REPUTATION SNAPSHOTS TABLE
-- Stores periodic snapshots of sending reputation
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_reputation_snapshots (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    sending_domain VARCHAR(255) NOT NULL,
    ip_address VARCHAR(45),
    overall_score NUMERIC(5,2) NOT NULL,
    bounce_rate NUMERIC(5,4) NOT NULL,
    complaint_rate NUMERIC(5,4) NOT NULL,
    engagement_rate NUMERIC(5,2) NOT NULL,
    spam_trap_hits INT DEFAULT 0,
    blacklist_count INT DEFAULT 0,
    blacklist_details JSONB DEFAULT '[]',
    trend VARCHAR(20) DEFAULT 'stable', -- improving, stable, declining
    snapshot_date TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Indexes for reputation snapshots
CREATE INDEX IF NOT EXISTS idx_reputation_org ON mailing_reputation_snapshots(org_id);
CREATE INDEX IF NOT EXISTS idx_reputation_domain ON mailing_reputation_snapshots(sending_domain);
CREATE INDEX IF NOT EXISTS idx_reputation_date ON mailing_reputation_snapshots(snapshot_date DESC);
CREATE INDEX IF NOT EXISTS idx_reputation_org_date ON mailing_reputation_snapshots(org_id, snapshot_date DESC);

-- ============================================
-- BLACKLIST CHECK RESULTS TABLE
-- Caches blacklist lookup results
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_blacklist_checks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID,
    ip_or_domain VARCHAR(255) NOT NULL,
    check_type VARCHAR(20) NOT NULL, -- ip, domain
    blacklist_name VARCHAR(100) NOT NULL,
    is_listed BOOLEAN DEFAULT false,
    check_url TEXT,
    checked_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    expires_at TIMESTAMP WITH TIME ZONE DEFAULT NOW() + INTERVAL '1 hour'
);

-- Indexes for blacklist checks
CREATE INDEX IF NOT EXISTS idx_blacklist_target ON mailing_blacklist_checks(ip_or_domain);
CREATE INDEX IF NOT EXISTS idx_blacklist_expires ON mailing_blacklist_checks(expires_at);
CREATE INDEX IF NOT EXISTS idx_blacklist_listed ON mailing_blacklist_checks(is_listed) WHERE is_listed = true;

-- ============================================
-- ISP DELIVERY METRICS TABLE
-- Tracks delivery metrics by ISP over time
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_isp_metrics (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    isp VARCHAR(50) NOT NULL, -- gmail, outlook, yahoo, aol, other
    metric_date DATE NOT NULL,
    total_sent INT DEFAULT 0,
    delivered INT DEFAULT 0,
    bounced INT DEFAULT 0,
    complained INT DEFAULT 0,
    opened INT DEFAULT 0,
    clicked INT DEFAULT 0,
    inbox_count INT DEFAULT 0,    -- From seed tests
    spam_count INT DEFAULT 0,     -- From seed tests
    missing_count INT DEFAULT 0,  -- From seed tests
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(org_id, isp, metric_date)
);

-- Indexes for ISP metrics
CREATE INDEX IF NOT EXISTS idx_isp_metrics_org ON mailing_isp_metrics(org_id);
CREATE INDEX IF NOT EXISTS idx_isp_metrics_isp ON mailing_isp_metrics(isp);
CREATE INDEX IF NOT EXISTS idx_isp_metrics_date ON mailing_isp_metrics(metric_date DESC);
CREATE INDEX IF NOT EXISTS idx_isp_metrics_org_date ON mailing_isp_metrics(org_id, metric_date DESC);

-- ============================================
-- FUNCTIONS
-- ============================================

-- Function to update updated_at timestamp
CREATE OR REPLACE FUNCTION update_inbox_placement_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Triggers for updated_at
DROP TRIGGER IF EXISTS trg_seed_lists_updated ON mailing_seed_lists;
CREATE TRIGGER trg_seed_lists_updated
    BEFORE UPDATE ON mailing_seed_lists
    FOR EACH ROW
    EXECUTE FUNCTION update_inbox_placement_updated_at();

DROP TRIGGER IF EXISTS trg_warmup_plans_updated ON mailing_ip_warmup_plans;
CREATE TRIGGER trg_warmup_plans_updated
    BEFORE UPDATE ON mailing_ip_warmup_plans
    FOR EACH ROW
    EXECUTE FUNCTION update_inbox_placement_updated_at();

DROP TRIGGER IF EXISTS trg_isp_metrics_updated ON mailing_isp_metrics;
CREATE TRIGGER trg_isp_metrics_updated
    BEFORE UPDATE ON mailing_isp_metrics
    FOR EACH ROW
    EXECUTE FUNCTION update_inbox_placement_updated_at();

-- Function to get inbox placement summary for an organization
CREATE OR REPLACE FUNCTION get_inbox_placement_summary(p_org_id UUID)
RETURNS TABLE (
    total_tests BIGINT,
    avg_inbox_rate NUMERIC,
    avg_spam_rate NUMERIC,
    avg_missing_rate NUMERIC,
    best_isp VARCHAR,
    worst_isp VARCHAR,
    last_test_date TIMESTAMP WITH TIME ZONE
) AS $$
BEGIN
    RETURN QUERY
    WITH recent_tests AS (
        SELECT tr.*
        FROM mailing_inbox_test_results tr
        JOIN mailing_seed_lists sl ON tr.seed_list_id = sl.id
        WHERE sl.org_id = p_org_id 
        AND tr.status = 'completed'
        AND tr.test_date > NOW() - INTERVAL '30 days'
    ),
    isp_performance AS (
        SELECT 
            isp->>'isp' as isp_name,
            AVG((isp->>'inbox_rate')::NUMERIC) as avg_rate
        FROM recent_tests,
        LATERAL jsonb_array_elements(isp_results) as isp
        GROUP BY isp->>'isp'
    )
    SELECT 
        COUNT(rt.*)::BIGINT as total_tests,
        ROUND(AVG(rt.inbox_rate), 2) as avg_inbox_rate,
        ROUND(AVG(rt.spam_rate), 2) as avg_spam_rate,
        ROUND(AVG(rt.missing_rate), 2) as avg_missing_rate,
        (SELECT isp_name FROM isp_performance ORDER BY avg_rate DESC LIMIT 1)::VARCHAR as best_isp,
        (SELECT isp_name FROM isp_performance ORDER BY avg_rate ASC LIMIT 1)::VARCHAR as worst_isp,
        MAX(rt.test_date) as last_test_date
    FROM recent_tests rt;
END;
$$ LANGUAGE plpgsql;

-- Function to get warmup plan daily recommendations
CREATE OR REPLACE FUNCTION get_warmup_daily_target(p_plan_id UUID)
RETURNS TABLE (
    day_number INT,
    target_volume INT,
    actual_volume INT,
    pct_complete NUMERIC,
    status VARCHAR
) AS $$
BEGIN
    RETURN QUERY
    WITH plan_data AS (
        SELECT daily_schedule, current_day
        FROM mailing_ip_warmup_plans
        WHERE id = p_plan_id
    ),
    schedule AS (
        SELECT 
            (day->>'day')::INT as day_num,
            (day->>'target_volume')::INT as target,
            (day->>'actual_volume')::INT as actual,
            (day->>'completed')::BOOLEAN as is_complete
        FROM plan_data,
        LATERAL jsonb_array_elements(daily_schedule) as day
    )
    SELECT 
        s.day_num as day_number,
        s.target as target_volume,
        s.actual as actual_volume,
        CASE WHEN s.target > 0 
            THEN ROUND((s.actual::NUMERIC / s.target) * 100, 1)
            ELSE 0 
        END as pct_complete,
        CASE 
            WHEN s.is_complete THEN 'completed'::VARCHAR
            WHEN s.day_num = (SELECT current_day FROM plan_data) THEN 'current'::VARCHAR
            ELSE 'pending'::VARCHAR
        END as status
    FROM schedule s
    ORDER BY s.day_num;
END;
$$ LANGUAGE plpgsql;

-- Function to clean up expired blacklist cache entries
CREATE OR REPLACE FUNCTION cleanup_expired_blacklist_cache()
RETURNS void AS $$
BEGIN
    DELETE FROM mailing_blacklist_checks WHERE expires_at < NOW();
END;
$$ LANGUAGE plpgsql;

-- Function to record ISP metrics from campaign data
CREATE OR REPLACE FUNCTION record_isp_metrics(
    p_org_id UUID,
    p_isp VARCHAR,
    p_sent INT DEFAULT 0,
    p_delivered INT DEFAULT 0,
    p_bounced INT DEFAULT 0,
    p_complained INT DEFAULT 0,
    p_opened INT DEFAULT 0,
    p_clicked INT DEFAULT 0
)
RETURNS void AS $$
BEGIN
    INSERT INTO mailing_isp_metrics (org_id, isp, metric_date, total_sent, delivered, bounced, complained, opened, clicked)
    VALUES (p_org_id, p_isp, CURRENT_DATE, p_sent, p_delivered, p_bounced, p_complained, p_opened, p_clicked)
    ON CONFLICT (org_id, isp, metric_date) 
    DO UPDATE SET
        total_sent = mailing_isp_metrics.total_sent + EXCLUDED.total_sent,
        delivered = mailing_isp_metrics.delivered + EXCLUDED.delivered,
        bounced = mailing_isp_metrics.bounced + EXCLUDED.bounced,
        complained = mailing_isp_metrics.complained + EXCLUDED.complained,
        opened = mailing_isp_metrics.opened + EXCLUDED.opened,
        clicked = mailing_isp_metrics.clicked + EXCLUDED.clicked,
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;

-- ============================================
-- VIEWS
-- ============================================

-- View for organization reputation overview
CREATE OR REPLACE VIEW vw_org_reputation_overview AS
SELECT 
    rs.org_id,
    rs.sending_domain,
    rs.overall_score,
    rs.bounce_rate,
    rs.complaint_rate,
    rs.engagement_rate,
    rs.blacklist_count,
    rs.trend,
    rs.snapshot_date,
    (SELECT COUNT(*) FROM mailing_ip_warmup_plans wp 
     WHERE wp.org_id = rs.org_id AND wp.status = 'active') as active_warmup_plans,
    (SELECT AVG(inbox_rate) FROM mailing_inbox_test_results tr 
     JOIN mailing_seed_lists sl ON tr.seed_list_id = sl.id 
     WHERE sl.org_id = rs.org_id 
     AND tr.test_date > NOW() - INTERVAL '7 days'
     AND tr.status = 'completed') as recent_avg_inbox_rate
FROM mailing_reputation_snapshots rs
WHERE rs.snapshot_date = (
    SELECT MAX(snapshot_date) 
    FROM mailing_reputation_snapshots 
    WHERE org_id = rs.org_id
);

-- View for ISP performance comparison
CREATE OR REPLACE VIEW vw_isp_performance AS
SELECT 
    org_id,
    isp,
    SUM(total_sent) as total_sent,
    SUM(delivered) as total_delivered,
    SUM(bounced) as total_bounced,
    SUM(complained) as total_complained,
    SUM(opened) as total_opened,
    SUM(clicked) as total_clicked,
    CASE WHEN SUM(total_sent) > 0 
        THEN ROUND(SUM(delivered)::NUMERIC / SUM(total_sent) * 100, 2) 
        ELSE 0 END as delivery_rate,
    CASE WHEN SUM(total_sent) > 0 
        THEN ROUND(SUM(bounced)::NUMERIC / SUM(total_sent) * 100, 2) 
        ELSE 0 END as bounce_rate,
    CASE WHEN SUM(total_sent) > 0 
        THEN ROUND(SUM(complained)::NUMERIC / SUM(total_sent) * 100, 4) 
        ELSE 0 END as complaint_rate,
    CASE WHEN SUM(delivered) > 0 
        THEN ROUND(SUM(opened)::NUMERIC / SUM(delivered) * 100, 2) 
        ELSE 0 END as open_rate,
    CASE WHEN SUM(opened) > 0 
        THEN ROUND(SUM(clicked)::NUMERIC / SUM(opened) * 100, 2) 
        ELSE 0 END as click_to_open_rate
FROM mailing_isp_metrics
WHERE metric_date > CURRENT_DATE - INTERVAL '30 days'
GROUP BY org_id, isp;

-- ============================================
-- COMMENTS
-- ============================================

COMMENT ON TABLE mailing_seed_lists IS 'Seed email lists for inbox placement testing';
COMMENT ON TABLE mailing_inbox_test_results IS 'Results from inbox placement seed tests';
COMMENT ON TABLE mailing_ip_warmup_plans IS 'IP warming schedules and progress tracking';
COMMENT ON TABLE mailing_reputation_snapshots IS 'Historical snapshots of sending reputation';
COMMENT ON TABLE mailing_blacklist_checks IS 'Cached blacklist lookup results';
COMMENT ON TABLE mailing_isp_metrics IS 'Daily delivery metrics by ISP';

COMMENT ON COLUMN mailing_seed_lists.provider IS 'Source of seed list: internal, emailonacid, litmus, glockapps';
COMMENT ON COLUMN mailing_inbox_test_results.overall_score IS 'Weighted score 0-100 based on inbox/spam/missing rates';
COMMENT ON COLUMN mailing_ip_warmup_plans.plan_type IS 'conservative (30 days), aggressive (15 days), or custom';
COMMENT ON COLUMN mailing_reputation_snapshots.trend IS 'Direction of reputation change: improving, stable, declining';
