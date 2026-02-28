-- A/B Split Testing - Enterprise Grade
-- Version: 1.0.0
-- Date: 2026-02-01
--
-- This migration creates a robust A/B split testing system integrated with campaigns:
-- - Multiple test types (subject, content, from_name, send_time, full variants)
-- - Statistical significance calculation
-- - Automatic winner selection
-- - Multi-variant support (A/B/C/D/n)
-- - Integration with segments for targeted testing

BEGIN;

-- ============================================
-- DROP OLD TABLES (if rebuilding)
-- ============================================
DROP TABLE IF EXISTS mailing_ab_test_results CASCADE;
DROP TABLE IF EXISTS mailing_ab_variants CASCADE;
DROP TABLE IF EXISTS mailing_ab_tests CASCADE;

-- ============================================
-- A/B TESTS (Parent table - linked to campaigns)
-- ============================================

CREATE TABLE mailing_ab_tests (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Can be standalone or linked to a campaign
    campaign_id UUID REFERENCES mailing_campaigns(id) ON DELETE SET NULL,
    
    -- Test identification
    name VARCHAR(255) NOT NULL,
    description TEXT,
    
    -- Test type
    test_type VARCHAR(50) NOT NULL CHECK (test_type IN (
        'subject_line',      -- Test different subject lines
        'from_name',         -- Test different from names
        'content',           -- Test different email content
        'send_time',         -- Test different send times
        'full_variant',      -- Test completely different emails
        'preheader',         -- Test preview text
        'cta'                -- Test call-to-action variations
    )),
    
    -- Audience configuration
    list_id UUID REFERENCES mailing_lists(id) ON DELETE SET NULL,
    segment_id UUID REFERENCES mailing_segments(id) ON DELETE SET NULL,
    segment_ids JSONB DEFAULT '[]',  -- Multiple segments
    
    -- Split configuration
    split_type VARCHAR(20) DEFAULT 'percentage' CHECK (split_type IN (
        'percentage',        -- Split by percentage
        'count',             -- Split by absolute count
        'auto'               -- Auto-optimize (MAB algorithm)
    )),
    
    -- For percentage split
    test_sample_percent INTEGER DEFAULT 20 CHECK (test_sample_percent BETWEEN 1 AND 100),
    
    -- For count split
    test_sample_count INTEGER,
    
    -- Winner selection
    winner_metric VARCHAR(50) DEFAULT 'open_rate' CHECK (winner_metric IN (
        'open_rate',
        'click_rate', 
        'click_to_open_rate',
        'conversion_rate',
        'revenue',
        'unsubscribe_rate'   -- Lower is better
    )),
    
    -- Winner selection timing
    winner_wait_hours INTEGER DEFAULT 4,
    winner_auto_select BOOLEAN DEFAULT TRUE,
    winner_confidence_threshold DECIMAL(5,4) DEFAULT 0.95,  -- 95% confidence
    winner_min_sample_size INTEGER DEFAULT 100,  -- Minimum samples before declaring winner
    
    -- Sending configuration (copied to winner campaign)
    sending_profile_id UUID,
    from_email VARCHAR(255),
    reply_email VARCHAR(255),
    
    -- Throttling for test sends
    throttle_speed VARCHAR(20) DEFAULT 'gentle',
    
    -- Test schedule
    test_start_at TIMESTAMP WITH TIME ZONE,
    test_end_at TIMESTAMP WITH TIME ZONE,
    winner_send_at TIMESTAMP WITH TIME ZONE,
    
    -- Status tracking
    status VARCHAR(20) DEFAULT 'draft' CHECK (status IN (
        'draft',
        'scheduled',
        'testing',          -- Variants being sent to test sample
        'waiting',          -- Waiting for results
        'analyzing',        -- Computing winner
        'winner_selected',  -- Winner chosen, ready to send
        'sending_winner',   -- Sending winner to remaining audience
        'completed',
        'cancelled',
        'failed'
    )),
    
    -- Results
    total_audience_size INTEGER DEFAULT 0,
    test_sample_size INTEGER DEFAULT 0,
    remaining_audience_size INTEGER DEFAULT 0,
    winner_variant_id UUID,
    
    -- Metadata
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_ab_tests_org ON mailing_ab_tests(organization_id);
CREATE INDEX idx_ab_tests_campaign ON mailing_ab_tests(campaign_id);
CREATE INDEX idx_ab_tests_status ON mailing_ab_tests(status);

-- ============================================
-- A/B TEST VARIANTS
-- ============================================

CREATE TABLE mailing_ab_variants (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id UUID NOT NULL REFERENCES mailing_ab_tests(id) ON DELETE CASCADE,
    
    -- Variant identification
    variant_name VARCHAR(50) NOT NULL,  -- 'A', 'B', 'C', 'Control', etc.
    variant_label VARCHAR(255),          -- Human-readable label
    
    -- Content variations (only filled based on test_type)
    subject VARCHAR(500),
    from_name VARCHAR(255),
    preheader VARCHAR(255),
    html_content TEXT,
    text_content TEXT,
    
    -- For send_time tests
    send_hour INTEGER CHECK (send_hour BETWEEN 0 AND 23),
    send_day_of_week INTEGER CHECK (send_day_of_week BETWEEN 0 AND 6),
    
    -- CTA variations
    cta_text VARCHAR(255),
    cta_url TEXT,
    cta_color VARCHAR(20),
    
    -- Split allocation
    split_percent INTEGER DEFAULT 50 CHECK (split_percent BETWEEN 0 AND 100),
    split_count INTEGER,
    
    -- Performance metrics
    sent_count INTEGER DEFAULT 0,
    delivered_count INTEGER DEFAULT 0,
    open_count INTEGER DEFAULT 0,
    unique_open_count INTEGER DEFAULT 0,
    click_count INTEGER DEFAULT 0,
    unique_click_count INTEGER DEFAULT 0,
    bounce_count INTEGER DEFAULT 0,
    complaint_count INTEGER DEFAULT 0,
    unsubscribe_count INTEGER DEFAULT 0,
    conversion_count INTEGER DEFAULT 0,
    revenue DECIMAL(12,2) DEFAULT 0,
    
    -- Calculated rates (updated after each batch)
    open_rate DECIMAL(8,4) DEFAULT 0,
    click_rate DECIMAL(8,4) DEFAULT 0,
    click_to_open_rate DECIMAL(8,4) DEFAULT 0,
    bounce_rate DECIMAL(8,4) DEFAULT 0,
    unsubscribe_rate DECIMAL(8,4) DEFAULT 0,
    conversion_rate DECIMAL(8,4) DEFAULT 0,
    revenue_per_send DECIMAL(10,4) DEFAULT 0,
    
    -- Statistical analysis
    confidence_score DECIMAL(5,4) DEFAULT 0,    -- Confidence this is the winner
    lift_vs_control DECIMAL(8,4) DEFAULT 0,     -- % improvement over control
    statistical_significance BOOLEAN DEFAULT FALSE,
    
    -- Winner flag
    is_control BOOLEAN DEFAULT FALSE,
    is_winner BOOLEAN DEFAULT FALSE,
    
    -- Timestamps
    first_sent_at TIMESTAMP WITH TIME ZONE,
    last_sent_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(test_id, variant_name)
);

CREATE INDEX idx_ab_variants_test ON mailing_ab_variants(test_id);
CREATE INDEX idx_ab_variants_winner ON mailing_ab_variants(is_winner) WHERE is_winner = TRUE;

-- ============================================
-- A/B TEST SUBSCRIBER ASSIGNMENTS
-- ============================================
-- Tracks which subscribers are assigned to which variant

CREATE TABLE mailing_ab_assignments (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id UUID NOT NULL REFERENCES mailing_ab_tests(id) ON DELETE CASCADE,
    variant_id UUID NOT NULL REFERENCES mailing_ab_variants(id) ON DELETE CASCADE,
    subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    
    -- Assignment tracking
    assignment_type VARCHAR(20) DEFAULT 'test' CHECK (assignment_type IN (
        'test',              -- Part of test sample
        'winner'             -- Receiving winner variant
    )),
    
    -- Send status
    status VARCHAR(20) DEFAULT 'assigned' CHECK (status IN (
        'assigned',
        'queued',
        'sent',
        'failed',
        'skipped'
    )),
    
    -- Tracking
    sent_at TIMESTAMP WITH TIME ZONE,
    message_id VARCHAR(255),
    email_id UUID,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(test_id, subscriber_id)
);

CREATE INDEX idx_ab_assignments_test ON mailing_ab_assignments(test_id);
CREATE INDEX idx_ab_assignments_variant ON mailing_ab_assignments(variant_id);
CREATE INDEX idx_ab_assignments_subscriber ON mailing_ab_assignments(subscriber_id);
CREATE INDEX idx_ab_assignments_status ON mailing_ab_assignments(status);

-- ============================================
-- A/B TEST EVENTS (Detailed tracking)
-- ============================================

CREATE TABLE mailing_ab_events (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id UUID NOT NULL,
    variant_id UUID NOT NULL,
    assignment_id UUID,
    subscriber_id UUID,
    
    event_type VARCHAR(50) NOT NULL,
    event_data JSONB DEFAULT '{}',
    
    -- Revenue tracking
    revenue_amount DECIMAL(12,2),
    conversion_id VARCHAR(255),
    
    event_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_ab_events_test ON mailing_ab_events(test_id, event_at);
CREATE INDEX idx_ab_events_variant ON mailing_ab_events(variant_id, event_at);

-- ============================================
-- A/B TEST RESULTS SNAPSHOTS
-- ============================================
-- Periodic snapshots for trend analysis

CREATE TABLE mailing_ab_result_snapshots (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id UUID NOT NULL REFERENCES mailing_ab_tests(id) ON DELETE CASCADE,
    variant_id UUID NOT NULL REFERENCES mailing_ab_variants(id) ON DELETE CASCADE,
    
    -- Metrics at snapshot time
    sent_count INTEGER,
    open_count INTEGER,
    click_count INTEGER,
    open_rate DECIMAL(8,4),
    click_rate DECIMAL(8,4),
    confidence_score DECIMAL(5,4),
    
    snapshot_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_ab_snapshots_test ON mailing_ab_result_snapshots(test_id, snapshot_at);

-- ============================================
-- FUNCTIONS
-- ============================================

-- Function to update variant rates
CREATE OR REPLACE FUNCTION update_ab_variant_rates(p_variant_id UUID) RETURNS VOID AS $$
BEGIN
    UPDATE mailing_ab_variants
    SET 
        open_rate = CASE WHEN sent_count > 0 THEN (unique_open_count::DECIMAL / sent_count) ELSE 0 END,
        click_rate = CASE WHEN sent_count > 0 THEN (unique_click_count::DECIMAL / sent_count) ELSE 0 END,
        click_to_open_rate = CASE WHEN unique_open_count > 0 THEN (unique_click_count::DECIMAL / unique_open_count) ELSE 0 END,
        bounce_rate = CASE WHEN sent_count > 0 THEN (bounce_count::DECIMAL / sent_count) ELSE 0 END,
        unsubscribe_rate = CASE WHEN sent_count > 0 THEN (unsubscribe_count::DECIMAL / sent_count) ELSE 0 END,
        conversion_rate = CASE WHEN sent_count > 0 THEN (conversion_count::DECIMAL / sent_count) ELSE 0 END,
        revenue_per_send = CASE WHEN sent_count > 0 THEN (revenue / sent_count) ELSE 0 END
    WHERE id = p_variant_id;
END;
$$ LANGUAGE plpgsql;

-- Function to calculate statistical significance (Z-test approximation)
CREATE OR REPLACE FUNCTION calculate_ab_significance(
    p_control_conversions INTEGER,
    p_control_samples INTEGER,
    p_variant_conversions INTEGER,
    p_variant_samples INTEGER
) RETURNS DECIMAL AS $$
DECLARE
    p1 DECIMAL;
    p2 DECIMAL;
    p_pooled DECIMAL;
    se DECIMAL;
    z_score DECIMAL;
    confidence DECIMAL;
BEGIN
    -- Handle edge cases
    IF p_control_samples < 30 OR p_variant_samples < 30 THEN
        RETURN 0;
    END IF;
    
    -- Calculate proportions
    p1 := p_control_conversions::DECIMAL / p_control_samples;
    p2 := p_variant_conversions::DECIMAL / p_variant_samples;
    
    -- Pooled proportion
    p_pooled := (p_control_conversions + p_variant_conversions)::DECIMAL / (p_control_samples + p_variant_samples);
    
    -- Standard error
    se := SQRT(p_pooled * (1 - p_pooled) * (1.0/p_control_samples + 1.0/p_variant_samples));
    
    IF se = 0 THEN
        RETURN 0;
    END IF;
    
    -- Z-score
    z_score := ABS(p2 - p1) / se;
    
    -- Approximate confidence from z-score
    -- z=1.645 -> 90%, z=1.96 -> 95%, z=2.576 -> 99%
    IF z_score >= 2.576 THEN
        confidence := 0.99;
    ELSIF z_score >= 1.96 THEN
        confidence := 0.95;
    ELSIF z_score >= 1.645 THEN
        confidence := 0.90;
    ELSIF z_score >= 1.28 THEN
        confidence := 0.80;
    ELSE
        confidence := z_score / 2.576;  -- Linear approximation below 80%
    END IF;
    
    RETURN confidence;
END;
$$ LANGUAGE plpgsql;

-- Function to automatically select winner
CREATE OR REPLACE FUNCTION auto_select_ab_winner(p_test_id UUID) RETURNS UUID AS $$
DECLARE
    v_winner_metric VARCHAR(50);
    v_min_samples INTEGER;
    v_confidence_threshold DECIMAL;
    v_winner_id UUID;
    v_control_id UUID;
BEGIN
    -- Get test configuration
    SELECT winner_metric, winner_min_sample_size, winner_confidence_threshold
    INTO v_winner_metric, v_min_samples, v_confidence_threshold
    FROM mailing_ab_tests WHERE id = p_test_id;
    
    -- Get control variant
    SELECT id INTO v_control_id
    FROM mailing_ab_variants
    WHERE test_id = p_test_id AND is_control = TRUE;
    
    -- Find winner based on metric
    CASE v_winner_metric
        WHEN 'open_rate' THEN
            SELECT id INTO v_winner_id
            FROM mailing_ab_variants
            WHERE test_id = p_test_id
            AND sent_count >= v_min_samples
            ORDER BY open_rate DESC
            LIMIT 1;
        WHEN 'click_rate' THEN
            SELECT id INTO v_winner_id
            FROM mailing_ab_variants
            WHERE test_id = p_test_id
            AND sent_count >= v_min_samples
            ORDER BY click_rate DESC
            LIMIT 1;
        WHEN 'click_to_open_rate' THEN
            SELECT id INTO v_winner_id
            FROM mailing_ab_variants
            WHERE test_id = p_test_id
            AND sent_count >= v_min_samples
            ORDER BY click_to_open_rate DESC
            LIMIT 1;
        WHEN 'conversion_rate' THEN
            SELECT id INTO v_winner_id
            FROM mailing_ab_variants
            WHERE test_id = p_test_id
            AND sent_count >= v_min_samples
            ORDER BY conversion_rate DESC
            LIMIT 1;
        WHEN 'revenue' THEN
            SELECT id INTO v_winner_id
            FROM mailing_ab_variants
            WHERE test_id = p_test_id
            AND sent_count >= v_min_samples
            ORDER BY revenue_per_send DESC
            LIMIT 1;
        WHEN 'unsubscribe_rate' THEN
            SELECT id INTO v_winner_id
            FROM mailing_ab_variants
            WHERE test_id = p_test_id
            AND sent_count >= v_min_samples
            ORDER BY unsubscribe_rate ASC  -- Lower is better
            LIMIT 1;
        ELSE
            SELECT id INTO v_winner_id
            FROM mailing_ab_variants
            WHERE test_id = p_test_id
            AND sent_count >= v_min_samples
            ORDER BY open_rate DESC
            LIMIT 1;
    END CASE;
    
    -- Mark winner
    IF v_winner_id IS NOT NULL THEN
        UPDATE mailing_ab_variants SET is_winner = FALSE WHERE test_id = p_test_id;
        UPDATE mailing_ab_variants SET is_winner = TRUE WHERE id = v_winner_id;
        UPDATE mailing_ab_tests SET winner_variant_id = v_winner_id, status = 'winner_selected' WHERE id = p_test_id;
    END IF;
    
    RETURN v_winner_id;
END;
$$ LANGUAGE plpgsql;

-- ============================================
-- ADD A/B TEST SUPPORT TO CAMPAIGNS
-- ============================================

ALTER TABLE mailing_campaigns
ADD COLUMN IF NOT EXISTS is_ab_test BOOLEAN DEFAULT FALSE,
ADD COLUMN IF NOT EXISTS ab_test_id UUID REFERENCES mailing_ab_tests(id) ON DELETE SET NULL,
ADD COLUMN IF NOT EXISTS ab_variant_id UUID REFERENCES mailing_ab_variants(id) ON DELETE SET NULL;

COMMIT;
