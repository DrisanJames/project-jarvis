-- Migration: Scale Infrastructure for 8.4M emails/day
-- Version: 2.0.0
-- Date: 2026-02-03

-- ============================================
-- SEGMENT CONDITIONS TABLE (Missing from original schema)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_segment_conditions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    segment_id UUID NOT NULL REFERENCES mailing_segments(id) ON DELETE CASCADE,
    condition_group INTEGER DEFAULT 0,
    field VARCHAR(100) NOT NULL,
    operator VARCHAR(20) NOT NULL CHECK (operator IN ('equals', 'not_equals', 'contains', 'not_contains', 'starts_with', 'ends_with', 'gt', 'lt', 'gte', 'lte', 'is_null', 'is_not_null', 'in', 'not_in')),
    value TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_segment_conditions_segment ON mailing_segment_conditions(segment_id);

-- ============================================
-- JOURNEY EXECUTION TABLES
-- ============================================

-- Add additional columns to journey enrollments for execution tracking
ALTER TABLE mailing_journey_enrollments ADD COLUMN IF NOT EXISTS next_execute_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE mailing_journey_enrollments ADD COLUMN IF NOT EXISTS last_executed_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE mailing_journey_enrollments ADD COLUMN IF NOT EXISTS execution_count INTEGER DEFAULT 0;
ALTER TABLE mailing_journey_enrollments ADD COLUMN IF NOT EXISTS metadata JSONB DEFAULT '{}';

-- Index for finding enrollments ready to execute
CREATE INDEX IF NOT EXISTS idx_journey_enrollments_execute 
    ON mailing_journey_enrollments(next_execute_at) 
    WHERE status = 'active' AND next_execute_at IS NOT NULL;

-- Journey execution log for audit trail
CREATE TABLE IF NOT EXISTS mailing_journey_execution_log (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    enrollment_id VARCHAR(100) NOT NULL,
    journey_id VARCHAR(100) NOT NULL,
    node_id VARCHAR(100) NOT NULL,
    node_type VARCHAR(50) NOT NULL,
    action VARCHAR(50) NOT NULL,
    result VARCHAR(50),
    metadata JSONB DEFAULT '{}',
    error_message TEXT,
    executed_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_journey_execution_log_enrollment ON mailing_journey_execution_log(enrollment_id);
CREATE INDEX idx_journey_execution_log_journey ON mailing_journey_execution_log(journey_id);
CREATE INDEX idx_journey_execution_log_time ON mailing_journey_execution_log(executed_at);

-- ============================================
-- SCALABLE SEND QUEUE (Worker-Based)
-- ============================================

-- Add priority and batch columns to campaign queue
ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS batch_id UUID;
ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS worker_id VARCHAR(100);
ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS locked_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS retry_count INTEGER DEFAULT 0;
ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS max_retries INTEGER DEFAULT 3;

-- Index for worker batch claiming
CREATE INDEX IF NOT EXISTS idx_queue_batch_claim 
    ON mailing_campaign_queue(status, scheduled_at, priority DESC) 
    WHERE status = 'queued' AND locked_at IS NULL;

-- Worker heartbeat and status tracking
CREATE TABLE IF NOT EXISTS mailing_workers (
    id VARCHAR(100) PRIMARY KEY,
    worker_type VARCHAR(50) NOT NULL, -- 'campaign_sender', 'journey_executor', 'scheduler'
    hostname VARCHAR(255),
    status VARCHAR(20) DEFAULT 'starting' CHECK (status IN ('starting', 'running', 'paused', 'stopping', 'stopped', 'error')),
    
    -- Capacity
    max_concurrent INTEGER DEFAULT 100,
    current_jobs INTEGER DEFAULT 0,
    
    -- Performance
    total_processed INTEGER DEFAULT 0,
    total_errors INTEGER DEFAULT 0,
    avg_process_time_ms DECIMAL(10,2),
    
    -- Health
    last_heartbeat_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    started_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    error_message TEXT,
    
    metadata JSONB DEFAULT '{}'
);

CREATE INDEX idx_workers_type ON mailing_workers(worker_type);
CREATE INDEX idx_workers_status ON mailing_workers(status);
CREATE INDEX idx_workers_heartbeat ON mailing_workers(last_heartbeat_at);

-- ============================================
-- SEND BATCHES (For orchestrating large campaigns)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_send_batches (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    batch_number INTEGER NOT NULL,
    
    -- Batch details
    total_recipients INTEGER NOT NULL,
    queued_count INTEGER DEFAULT 0,
    sent_count INTEGER DEFAULT 0,
    failed_count INTEGER DEFAULT 0,
    suppressed_count INTEGER DEFAULT 0,
    
    -- Status
    status VARCHAR(20) DEFAULT 'pending' CHECK (status IN ('pending', 'queuing', 'queued', 'sending', 'completed', 'failed')),
    
    -- Timing
    scheduled_at TIMESTAMP WITH TIME ZONE,
    started_at TIMESTAMP WITH TIME ZONE,
    completed_at TIMESTAMP WITH TIME ZONE,
    
    -- ESP assignment
    sending_profile_id UUID,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_batches_campaign ON mailing_send_batches(campaign_id);
CREATE INDEX idx_batches_status ON mailing_send_batches(status);

-- ============================================
-- RATE LIMITING
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_rate_limits (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    resource_type VARCHAR(50) NOT NULL, -- 'esp', 'domain', 'ip', 'organization'
    resource_id VARCHAR(100) NOT NULL,
    
    -- Limits
    limit_per_second INTEGER,
    limit_per_minute INTEGER,
    limit_per_hour INTEGER,
    limit_per_day INTEGER,
    
    -- Current usage (reset by worker)
    current_second INTEGER DEFAULT 0,
    current_minute INTEGER DEFAULT 0,
    current_hour INTEGER DEFAULT 0,
    current_day INTEGER DEFAULT 0,
    
    -- Reset timestamps
    second_reset_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    minute_reset_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    hour_reset_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    day_reset_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(resource_type, resource_id)
);

CREATE INDEX idx_rate_limits_resource ON mailing_rate_limits(resource_type, resource_id);

-- ============================================
-- CAMPAIGN-WEBHOOK LINKING (Fix missing links)
-- ============================================

-- Add message tracking table for correlating webhooks to campaigns
CREATE TABLE IF NOT EXISTS mailing_message_log (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    message_id VARCHAR(255) NOT NULL,
    organization_id UUID NOT NULL,
    campaign_id UUID REFERENCES mailing_campaigns(id) ON DELETE SET NULL,
    subscriber_id UUID REFERENCES mailing_subscribers(id) ON DELETE SET NULL,
    email VARCHAR(255) NOT NULL,
    
    -- ESP info
    sending_profile_id UUID,
    esp_type VARCHAR(50),
    
    -- Status
    status VARCHAR(20) DEFAULT 'sent',
    
    -- Timestamps
    sent_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    delivered_at TIMESTAMP WITH TIME ZONE,
    
    -- For quick lookups
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_message_log_message_id ON mailing_message_log(message_id);
CREATE INDEX idx_message_log_campaign ON mailing_message_log(campaign_id);
CREATE INDEX idx_message_log_email ON mailing_message_log(email);
CREATE INDEX idx_message_log_sent ON mailing_message_log(sent_at);

-- ============================================
-- JOURNEYS TABLE (Standard schema)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_journeys (
    id VARCHAR(100) PRIMARY KEY,
    organization_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    status VARCHAR(50) DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'paused', 'completed', 'archived')),
    
    -- Visual builder data
    nodes JSONB DEFAULT '[]',
    connections JSONB DEFAULT '[]',
    
    -- Trigger settings
    trigger_type VARCHAR(50), -- 'list_subscription', 'segment_entry', 'manual', 'api', 'date_field'
    trigger_config JSONB DEFAULT '{}',
    
    -- List/Segment association
    list_id UUID REFERENCES mailing_lists(id) ON DELETE SET NULL,
    segment_id UUID REFERENCES mailing_segments(id) ON DELETE SET NULL,
    
    -- Stats
    total_entered INTEGER DEFAULT 0,
    total_completed INTEGER DEFAULT 0,
    total_converted INTEGER DEFAULT 0,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    activated_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_journeys_org ON mailing_journeys(organization_id);
CREATE INDEX idx_journeys_status ON mailing_journeys(status);

-- ============================================
-- FUNCTIONS FOR SCALE
-- ============================================

-- Function to claim a batch of queue items for a worker
CREATE OR REPLACE FUNCTION claim_send_batch(
    p_worker_id VARCHAR(100),
    p_batch_size INTEGER,
    p_lock_duration INTERVAL DEFAULT '5 minutes'
) RETURNS TABLE (
    queue_id UUID,
    campaign_id UUID,
    subscriber_id UUID,
    email VARCHAR(255),
    subject VARCHAR(500),
    html_content TEXT
) AS $$
BEGIN
    RETURN QUERY
    WITH claimed AS (
        UPDATE mailing_campaign_queue
        SET 
            status = 'sending',
            worker_id = p_worker_id,
            locked_at = NOW()
        WHERE id IN (
            SELECT q.id FROM mailing_campaign_queue q
            WHERE q.status = 'queued'
              AND q.scheduled_at <= NOW()
              AND (q.locked_at IS NULL OR q.locked_at < NOW() - p_lock_duration)
            ORDER BY q.priority DESC, q.scheduled_at ASC
            LIMIT p_batch_size
            FOR UPDATE SKIP LOCKED
        )
        RETURNING id, campaign_id, subscriber_id
    )
    SELECT 
        c.id,
        c.campaign_id,
        c.subscriber_id,
        s.email,
        c.subject,
        c.html_content
    FROM claimed c
    JOIN mailing_subscribers s ON s.id = c.subscriber_id;
END;
$$ LANGUAGE plpgsql;

-- Function to update campaign stats atomically
CREATE OR REPLACE FUNCTION update_campaign_stat(
    p_campaign_id UUID,
    p_stat_name VARCHAR(50),
    p_increment INTEGER DEFAULT 1
) RETURNS VOID AS $$
BEGIN
    EXECUTE format(
        'UPDATE mailing_campaigns SET %I = COALESCE(%I, 0) + $1, updated_at = NOW() WHERE id = $2',
        p_stat_name, p_stat_name
    ) USING p_increment, p_campaign_id;
END;
$$ LANGUAGE plpgsql;

-- Function to check rate limits
CREATE OR REPLACE FUNCTION check_rate_limit(
    p_resource_type VARCHAR(50),
    p_resource_id VARCHAR(100),
    p_increment INTEGER DEFAULT 1
) RETURNS BOOLEAN AS $$
DECLARE
    v_limit_per_minute INTEGER;
    v_current_minute INTEGER;
    v_can_send BOOLEAN := TRUE;
BEGIN
    -- Upsert rate limit record
    INSERT INTO mailing_rate_limits (resource_type, resource_id, current_minute, minute_reset_at)
    VALUES (p_resource_type, p_resource_id, 0, NOW())
    ON CONFLICT (resource_type, resource_id) DO NOTHING;
    
    -- Check and update
    UPDATE mailing_rate_limits
    SET 
        current_minute = CASE 
            WHEN minute_reset_at < date_trunc('minute', NOW()) THEN p_increment
            ELSE current_minute + p_increment
        END,
        minute_reset_at = CASE 
            WHEN minute_reset_at < date_trunc('minute', NOW()) THEN NOW()
            ELSE minute_reset_at
        END
    WHERE resource_type = p_resource_type AND resource_id = p_resource_id
    RETURNING 
        limit_per_minute,
        current_minute
    INTO v_limit_per_minute, v_current_minute;
    
    -- Check if over limit
    IF v_limit_per_minute IS NOT NULL AND v_current_minute > v_limit_per_minute THEN
        v_can_send := FALSE;
    END IF;
    
    RETURN v_can_send;
END;
$$ LANGUAGE plpgsql;

-- ============================================
-- DEFAULT RATE LIMITS
-- ============================================

INSERT INTO mailing_rate_limits (resource_type, resource_id, limit_per_second, limit_per_minute, limit_per_hour, limit_per_day)
VALUES 
    ('organization', '00000000-0000-0000-0000-000000000001', 500, 10000, 400000, 8500000),
    ('esp', 'sparkpost', 100, 5000, 200000, 4000000),
    ('esp', 'ses', 50, 3000, 100000, 2000000),
    ('esp', 'mailgun', 80, 4000, 150000, 3000000)
ON CONFLICT (resource_type, resource_id) DO UPDATE SET
    limit_per_second = EXCLUDED.limit_per_second,
    limit_per_minute = EXCLUDED.limit_per_minute,
    limit_per_hour = EXCLUDED.limit_per_hour,
    limit_per_day = EXCLUDED.limit_per_day;

COMMIT;
