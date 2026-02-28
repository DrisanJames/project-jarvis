-- Enterprise Segmentation Engine - Database Schema
-- Version: 1.0.0
-- Date: 2026-02-01
-- 
-- This migration adds enterprise-grade segmentation capabilities:
-- - Custom event tracking with arbitrary properties
-- - Flexible contact field definitions
-- - Advanced segment conditions with nested AND/OR logic
-- - Segment snapshots for A/B testing
-- - GDPR/CCPA consent management
-- - Computed fields and aggregates

BEGIN;

-- ============================================
-- CONTACT FIELD DEFINITIONS (Schema Registry)
-- ============================================
-- Allows creating unlimited custom fields with type enforcement

CREATE TABLE IF NOT EXISTS mailing_contact_fields (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Field definition
    field_key VARCHAR(100) NOT NULL,
    field_label VARCHAR(255) NOT NULL,
    field_type VARCHAR(50) NOT NULL CHECK (field_type IN (
        'string', 'number', 'integer', 'decimal', 'boolean', 
        'date', 'datetime', 'array', 'tags'
    )),
    
    -- Metadata
    description TEXT,
    category VARCHAR(100) DEFAULT 'custom',
    is_system BOOLEAN DEFAULT FALSE,
    is_pii BOOLEAN DEFAULT FALSE,
    is_required BOOLEAN DEFAULT FALSE,
    
    -- Validation rules
    validation_rules JSONB DEFAULT '{}',
    -- Example: {"min": 0, "max": 100, "pattern": "^[A-Z]+$", "enum": ["a","b","c"]}
    
    -- Default value
    default_value TEXT,
    
    -- For picklist/enum fields
    allowed_values JSONB DEFAULT NULL,
    
    -- Display settings
    display_order INTEGER DEFAULT 0,
    is_visible BOOLEAN DEFAULT TRUE,
    
    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(organization_id, field_key)
);

CREATE INDEX idx_contact_fields_org ON mailing_contact_fields(organization_id);
CREATE INDEX idx_contact_fields_category ON mailing_contact_fields(category);

-- ============================================
-- CUSTOM EVENTS (Time-Series Data)
-- ============================================
-- Supports arbitrary event types with properties

CREATE TABLE IF NOT EXISTS mailing_custom_events (
    id UUID DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL,
    subscriber_id UUID NOT NULL,
    
    -- Event identification
    event_name VARCHAR(255) NOT NULL,
    event_category VARCHAR(100) DEFAULT 'custom',
    
    -- Event properties (arbitrary JSON)
    properties JSONB DEFAULT '{}',
    
    -- Context
    source VARCHAR(100),  -- 'api', 'web', 'import', 'integration'
    ip_address INET,
    user_agent TEXT,
    device_type VARCHAR(50),
    geo_country VARCHAR(2),
    geo_region VARCHAR(100),
    geo_city VARCHAR(255),
    
    -- Session tracking
    session_id VARCHAR(255),
    
    -- Timestamps
    event_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    PRIMARY KEY (id, event_at)
) PARTITION BY RANGE (event_at);

-- Create partitions for custom events
CREATE TABLE mailing_custom_events_2026_01 PARTITION OF mailing_custom_events
    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE mailing_custom_events_2026_02 PARTITION OF mailing_custom_events
    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
CREATE TABLE mailing_custom_events_2026_03 PARTITION OF mailing_custom_events
    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
CREATE TABLE mailing_custom_events_2026_04 PARTITION OF mailing_custom_events
    FOR VALUES FROM ('2026-04-01') TO ('2026-05-01');
CREATE TABLE mailing_custom_events_2026_05 PARTITION OF mailing_custom_events
    FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
CREATE TABLE mailing_custom_events_2026_06 PARTITION OF mailing_custom_events
    FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');

CREATE INDEX idx_custom_events_org ON mailing_custom_events(organization_id, event_at DESC);
CREATE INDEX idx_custom_events_subscriber ON mailing_custom_events(subscriber_id, event_at DESC);
CREATE INDEX idx_custom_events_name ON mailing_custom_events(event_name, event_at DESC);
CREATE INDEX idx_custom_events_category ON mailing_custom_events(event_category, event_at DESC);
CREATE INDEX idx_custom_events_properties ON mailing_custom_events USING GIN (properties);

-- ============================================
-- EVENT TYPE DEFINITIONS
-- ============================================
-- Registry of allowed event types per organization

CREATE TABLE IF NOT EXISTS mailing_event_definitions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    event_name VARCHAR(255) NOT NULL,
    event_category VARCHAR(100) DEFAULT 'custom',
    description TEXT,
    
    -- Property schema (JSON Schema format)
    property_schema JSONB DEFAULT '{}',
    
    -- Examples for documentation
    example_payload JSONB,
    
    -- Usage stats
    total_events BIGINT DEFAULT 0,
    last_event_at TIMESTAMP WITH TIME ZONE,
    
    is_system BOOLEAN DEFAULT FALSE,
    status VARCHAR(20) DEFAULT 'active',
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(organization_id, event_name)
);

CREATE INDEX idx_event_definitions_org ON mailing_event_definitions(organization_id);

-- ============================================
-- ENHANCED SEGMENT CONDITIONS
-- ============================================
-- Supports nested AND/OR groups

-- Drop old conditions table if exists and recreate with better structure
DROP TABLE IF EXISTS mailing_segment_conditions CASCADE;

CREATE TABLE mailing_segment_condition_groups (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    segment_id UUID NOT NULL REFERENCES mailing_segments(id) ON DELETE CASCADE,
    parent_group_id UUID REFERENCES mailing_segment_condition_groups(id) ON DELETE CASCADE,
    
    -- Logic operator for this group
    logic_operator VARCHAR(10) DEFAULT 'AND' CHECK (logic_operator IN ('AND', 'OR')),
    
    -- For ordering within parent
    sort_order INTEGER DEFAULT 0,
    
    -- Is this a negation group (NOT)?
    is_negated BOOLEAN DEFAULT FALSE,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_condition_groups_segment ON mailing_segment_condition_groups(segment_id);
CREATE INDEX idx_condition_groups_parent ON mailing_segment_condition_groups(parent_group_id);

CREATE TABLE mailing_segment_conditions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    segment_id UUID NOT NULL REFERENCES mailing_segments(id) ON DELETE CASCADE,
    group_id UUID NOT NULL REFERENCES mailing_segment_condition_groups(id) ON DELETE CASCADE,
    
    -- Condition type
    condition_type VARCHAR(50) NOT NULL CHECK (condition_type IN (
        'profile',      -- Contact attributes
        'custom_field', -- Custom fields in JSONB
        'event',        -- Behavioral events
        'computed',     -- Computed fields
        'tag'           -- Array/tag matching
    )),
    
    -- Field specification
    field VARCHAR(255) NOT NULL,
    field_type VARCHAR(50),  -- For type-aware operators
    
    -- Operator (comprehensive list)
    operator VARCHAR(50) NOT NULL CHECK (operator IN (
        -- String operators
        'equals', 'not_equals', 'contains', 'not_contains',
        'starts_with', 'ends_with', 'is_empty', 'is_not_empty',
        'matches_regex',
        
        -- Numeric operators
        'gt', 'gte', 'lt', 'lte', 'between', 'not_between',
        
        -- Date operators
        'date_equals', 'date_before', 'date_after', 'date_between',
        'in_last_days', 'in_next_days', 'more_than_days_ago',
        'anniversary_month', 'anniversary_day',
        
        -- Array/List operators
        'contains_any', 'contains_all', 'not_contains_any',
        'array_is_empty', 'array_is_not_empty',
        
        -- Boolean
        'is_true', 'is_false',
        
        -- NULL checks
        'is_null', 'is_not_null',
        
        -- Event-specific operators
        'event_count_gte', 'event_count_lte', 'event_count_between',
        'event_in_last_days', 'event_not_in_last_days',
        'event_property_equals', 'event_property_contains'
    )),
    
    -- Value(s) for comparison
    value TEXT,
    value_secondary TEXT,  -- For 'between' operators
    values_array JSONB,    -- For 'contains_any', 'contains_all'
    
    -- Event-specific settings
    event_name VARCHAR(255),
    event_time_window_days INTEGER,
    event_min_count INTEGER,
    event_max_count INTEGER,
    event_property_path VARCHAR(255),  -- For drilling into event properties
    
    -- Sorting
    sort_order INTEGER DEFAULT 0,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_conditions_segment ON mailing_segment_conditions(segment_id);
CREATE INDEX idx_conditions_group ON mailing_segment_conditions(group_id);
CREATE INDEX idx_conditions_type ON mailing_segment_conditions(condition_type);

-- ============================================
-- SEGMENT SNAPSHOTS
-- ============================================
-- Freeze segments at a point in time for A/B tests and audits

CREATE TABLE IF NOT EXISTS mailing_segment_snapshots (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    segment_id UUID NOT NULL REFERENCES mailing_segments(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Snapshot metadata
    name VARCHAR(255),
    description TEXT,
    
    -- Frozen conditions (copy at snapshot time)
    conditions_snapshot JSONB NOT NULL,
    
    -- Stats at snapshot time
    subscriber_count INTEGER NOT NULL,
    
    -- Frozen subscriber list (for small segments) or query hash
    subscriber_ids JSONB,  -- Array of UUIDs if < 10000
    query_hash VARCHAR(64),
    
    -- Usage tracking
    purpose VARCHAR(50) DEFAULT 'manual',  -- 'manual', 'ab_test', 'campaign', 'audit'
    campaign_id UUID REFERENCES mailing_campaigns(id) ON DELETE SET NULL,
    
    -- Creator
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    
    -- Timestamps
    snapshot_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    expires_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_snapshots_segment ON mailing_segment_snapshots(segment_id);
CREATE INDEX idx_snapshots_org ON mailing_segment_snapshots(organization_id);
CREATE INDEX idx_snapshots_purpose ON mailing_segment_snapshots(purpose);

-- ============================================
-- CONSENT & SUPPRESSION MANAGEMENT
-- ============================================
-- GDPR/CCPA compliance

CREATE TABLE IF NOT EXISTS mailing_consent_records (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    subscriber_id UUID REFERENCES mailing_subscribers(id) ON DELETE SET NULL,
    email VARCHAR(255) NOT NULL,
    email_hash VARCHAR(64) NOT NULL,
    
    -- Consent type
    consent_type VARCHAR(50) NOT NULL CHECK (consent_type IN (
        'marketing_email', 'transactional_email', 'tracking', 
        'profiling', 'data_processing', 'third_party_sharing'
    )),
    
    -- Consent status
    status VARCHAR(20) NOT NULL CHECK (status IN ('granted', 'denied', 'withdrawn')),
    
    -- Legal basis
    legal_basis VARCHAR(50),  -- 'consent', 'legitimate_interest', 'contract', 'legal_obligation'
    
    -- Consent details
    consent_text TEXT,  -- What they agreed to
    consent_version VARCHAR(50),
    
    -- Collection context
    source VARCHAR(100),  -- 'signup_form', 'preference_center', 'api', 'import'
    ip_address INET,
    user_agent TEXT,
    
    -- Timestamps
    consented_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    expires_at TIMESTAMP WITH TIME ZONE,
    withdrawn_at TIMESTAMP WITH TIME ZONE,
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_consent_org ON mailing_consent_records(organization_id);
CREATE INDEX idx_consent_subscriber ON mailing_consent_records(subscriber_id);
CREATE INDEX idx_consent_email_hash ON mailing_consent_records(email_hash);
CREATE INDEX idx_consent_type ON mailing_consent_records(consent_type, status);

-- Global suppression list (across all lists)
CREATE TABLE IF NOT EXISTS mailing_suppression_list (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    email VARCHAR(255) NOT NULL,
    email_hash VARCHAR(64) NOT NULL,
    
    -- Suppression reason
    reason VARCHAR(50) NOT NULL CHECK (reason IN (
        'unsubscribe', 'complaint', 'hard_bounce', 'manual',
        'gdpr_request', 'ccpa_request', 'legal', 'competitor'
    )),
    
    -- Scope
    scope VARCHAR(20) DEFAULT 'all' CHECK (scope IN ('all', 'marketing', 'transactional')),
    
    -- Details
    notes TEXT,
    
    -- Origin
    source VARCHAR(100),
    original_list_id UUID,
    
    -- Timestamps
    suppressed_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    expires_at TIMESTAMP WITH TIME ZONE,  -- NULL = permanent
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(organization_id, email_hash, scope)
);

CREATE INDEX idx_suppression_org ON mailing_suppression_list(organization_id);
CREATE INDEX idx_suppression_email_hash ON mailing_suppression_list(email_hash);
CREATE INDEX idx_suppression_reason ON mailing_suppression_list(reason);

-- ============================================
-- COMPUTED FIELDS TRACKING
-- ============================================
-- Store aggregated/computed values for fast querying

CREATE TABLE IF NOT EXISTS mailing_subscriber_computed (
    subscriber_id UUID PRIMARY KEY REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Lifetime Value
    total_purchases INTEGER DEFAULT 0,
    total_revenue DECIMAL(12,2) DEFAULT 0,
    average_order_value DECIMAL(10,2) DEFAULT 0,
    
    -- Activity timestamps
    first_email_at TIMESTAMP WITH TIME ZONE,
    last_active_at TIMESTAMP WITH TIME ZONE,  -- Max of open/click/login/purchase
    last_purchase_at TIMESTAMP WITH TIME ZONE,
    last_login_at TIMESTAMP WITH TIME ZONE,
    
    -- Engagement metrics (rolling windows)
    opens_7d INTEGER DEFAULT 0,
    opens_30d INTEGER DEFAULT 0,
    opens_90d INTEGER DEFAULT 0,
    clicks_7d INTEGER DEFAULT 0,
    clicks_30d INTEGER DEFAULT 0,
    clicks_90d INTEGER DEFAULT 0,
    
    -- Velocity (engagement trend)
    engagement_velocity DECIMAL(5,2) DEFAULT 0,  -- Positive = increasing, negative = decreasing
    
    -- Predictive scores
    propensity_to_buy DECIMAL(5,4),
    next_purchase_days INTEGER,  -- Predicted days until next purchase
    
    -- Calculated at
    calculated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_computed_org ON mailing_subscriber_computed(organization_id);
CREATE INDEX idx_computed_last_active ON mailing_subscriber_computed(last_active_at DESC);
CREATE INDEX idx_computed_revenue ON mailing_subscriber_computed(total_revenue DESC);

-- ============================================
-- SEGMENT CALCULATION JOBS
-- ============================================
-- Track segment calculation for large segments

CREATE TABLE IF NOT EXISTS mailing_segment_jobs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    segment_id UUID NOT NULL REFERENCES mailing_segments(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Job type
    job_type VARCHAR(50) NOT NULL CHECK (job_type IN (
        'preview', 'full_calculation', 'snapshot', 'export'
    )),
    
    -- Status
    status VARCHAR(20) DEFAULT 'pending' CHECK (status IN (
        'pending', 'running', 'completed', 'failed', 'cancelled'
    )),
    
    -- Results
    estimated_count INTEGER,
    actual_count INTEGER,
    sample_subscribers JSONB,  -- Sample of 10 subscribers for preview
    
    -- Performance
    started_at TIMESTAMP WITH TIME ZONE,
    completed_at TIMESTAMP WITH TIME ZONE,
    duration_ms INTEGER,
    
    -- Error handling
    error_message TEXT,
    retry_count INTEGER DEFAULT 0,
    
    -- Idempotency
    query_hash VARCHAR(64),  -- Hash of conditions for cache invalidation
    
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_segment_jobs_segment ON mailing_segment_jobs(segment_id);
CREATE INDEX idx_segment_jobs_status ON mailing_segment_jobs(status);

-- ============================================
-- ALTER EXISTING TABLES
-- ============================================

-- Add columns to mailing_segments for enhanced features
ALTER TABLE mailing_segments 
ADD COLUMN IF NOT EXISTS last_calculated_at TIMESTAMP WITH TIME ZONE,
ADD COLUMN IF NOT EXISTS calculation_mode VARCHAR(20) DEFAULT 'batch' 
    CHECK (calculation_mode IN ('realtime', 'batch', 'hybrid')),
ADD COLUMN IF NOT EXISTS refresh_interval_minutes INTEGER DEFAULT 60,
ADD COLUMN IF NOT EXISTS include_suppressed BOOLEAN DEFAULT FALSE,
ADD COLUMN IF NOT EXISTS global_exclusion_rules JSONB DEFAULT '[]',
ADD COLUMN IF NOT EXISTS created_by UUID REFERENCES users(id) ON DELETE SET NULL,
ADD COLUMN IF NOT EXISTS last_edited_by UUID REFERENCES users(id) ON DELETE SET NULL,
ADD COLUMN IF NOT EXISTS last_edited_at TIMESTAMP WITH TIME ZONE;

-- Add tags array to subscribers for array operators
ALTER TABLE mailing_subscribers
ADD COLUMN IF NOT EXISTS tags TEXT[] DEFAULT '{}';

CREATE INDEX IF NOT EXISTS idx_subscribers_tags ON mailing_subscribers USING GIN (tags);

-- ============================================
-- FUNCTIONS FOR SEGMENT CALCULATION
-- ============================================

-- Function to get approximate segment count quickly
CREATE OR REPLACE FUNCTION estimate_segment_count(
    p_segment_id UUID
) RETURNS INTEGER AS $$
DECLARE
    v_count INTEGER;
    v_list_id UUID;
BEGIN
    -- Get list_id from segment
    SELECT list_id INTO v_list_id FROM mailing_segments WHERE id = p_segment_id;
    
    -- Use table statistics for quick estimate
    SELECT (reltuples * 0.5)::INTEGER INTO v_count
    FROM pg_class WHERE relname = 'mailing_subscribers';
    
    -- This is a placeholder - actual implementation would parse conditions
    -- and use statistics for estimation
    RETURN v_count;
END;
$$ LANGUAGE plpgsql;

-- Function to update computed fields for a subscriber
CREATE OR REPLACE FUNCTION update_subscriber_computed(
    p_subscriber_id UUID
) RETURNS VOID AS $$
DECLARE
    v_org_id UUID;
    v_opens_7d INTEGER;
    v_opens_30d INTEGER;
    v_opens_90d INTEGER;
    v_clicks_7d INTEGER;
    v_clicks_30d INTEGER;
    v_clicks_90d INTEGER;
    v_last_active TIMESTAMP WITH TIME ZONE;
    v_total_revenue DECIMAL(12,2);
BEGIN
    -- Get organization
    SELECT organization_id INTO v_org_id FROM mailing_subscribers WHERE id = p_subscriber_id;
    
    -- Calculate rolling windows from tracking events
    SELECT 
        COALESCE(SUM(CASE WHEN event_at > NOW() - INTERVAL '7 days' AND event_type = 'opened' THEN 1 ELSE 0 END), 0),
        COALESCE(SUM(CASE WHEN event_at > NOW() - INTERVAL '30 days' AND event_type = 'opened' THEN 1 ELSE 0 END), 0),
        COALESCE(SUM(CASE WHEN event_at > NOW() - INTERVAL '90 days' AND event_type = 'opened' THEN 1 ELSE 0 END), 0),
        COALESCE(SUM(CASE WHEN event_at > NOW() - INTERVAL '7 days' AND event_type = 'clicked' THEN 1 ELSE 0 END), 0),
        COALESCE(SUM(CASE WHEN event_at > NOW() - INTERVAL '30 days' AND event_type = 'clicked' THEN 1 ELSE 0 END), 0),
        COALESCE(SUM(CASE WHEN event_at > NOW() - INTERVAL '90 days' AND event_type = 'clicked' THEN 1 ELSE 0 END), 0),
        MAX(event_at)
    INTO v_opens_7d, v_opens_30d, v_opens_90d, v_clicks_7d, v_clicks_30d, v_clicks_90d, v_last_active
    FROM mailing_tracking_events
    WHERE subscriber_id = p_subscriber_id
    AND event_at > NOW() - INTERVAL '90 days';
    
    -- Calculate total revenue from attributions
    SELECT COALESCE(SUM(attributed_revenue), 0) INTO v_total_revenue
    FROM mailing_revenue_attributions
    WHERE subscriber_id = p_subscriber_id;
    
    -- Upsert computed record
    INSERT INTO mailing_subscriber_computed (
        subscriber_id, organization_id,
        opens_7d, opens_30d, opens_90d,
        clicks_7d, clicks_30d, clicks_90d,
        last_active_at, total_revenue,
        calculated_at, updated_at
    ) VALUES (
        p_subscriber_id, v_org_id,
        v_opens_7d, v_opens_30d, v_opens_90d,
        v_clicks_7d, v_clicks_30d, v_clicks_90d,
        v_last_active, v_total_revenue,
        NOW(), NOW()
    )
    ON CONFLICT (subscriber_id) DO UPDATE SET
        opens_7d = EXCLUDED.opens_7d,
        opens_30d = EXCLUDED.opens_30d,
        opens_90d = EXCLUDED.opens_90d,
        clicks_7d = EXCLUDED.clicks_7d,
        clicks_30d = EXCLUDED.clicks_30d,
        clicks_90d = EXCLUDED.clicks_90d,
        last_active_at = EXCLUDED.last_active_at,
        total_revenue = EXCLUDED.total_revenue,
        calculated_at = NOW(),
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;

-- ============================================
-- SEED SYSTEM CONTACT FIELDS
-- ============================================

INSERT INTO mailing_contact_fields (organization_id, field_key, field_label, field_type, category, is_system, description)
SELECT 
    '00000000-0000-0000-0000-000000000001',
    field_key,
    field_label,
    field_type,
    'system',
    true,
    description
FROM (VALUES
    ('email', 'Email', 'string', 'Primary email address'),
    ('first_name', 'First Name', 'string', 'First name'),
    ('last_name', 'Last Name', 'string', 'Last name'),
    ('status', 'Status', 'string', 'Subscription status'),
    ('source', 'Source', 'string', 'Acquisition source'),
    ('engagement_score', 'Engagement Score', 'decimal', 'Calculated engagement score 0-100'),
    ('total_opens', 'Total Opens', 'integer', 'Total email opens'),
    ('total_clicks', 'Total Clicks', 'integer', 'Total email clicks'),
    ('optimal_send_hour_utc', 'Best Send Hour', 'integer', 'AI-predicted optimal send hour'),
    ('churn_risk_score', 'Churn Risk', 'decimal', 'AI-predicted churn probability'),
    ('predicted_ltv', 'Predicted LTV', 'decimal', 'AI-predicted lifetime value'),
    ('subscribed_at', 'Subscribed At', 'datetime', 'Date of subscription'),
    ('last_open_at', 'Last Open', 'datetime', 'Last email open date'),
    ('last_click_at', 'Last Click', 'datetime', 'Last email click date'),
    ('tags', 'Tags', 'tags', 'Contact tags array')
) AS t(field_key, field_label, field_type, description)
ON CONFLICT DO NOTHING;

-- ============================================
-- SEED SYSTEM EVENT DEFINITIONS
-- ============================================

INSERT INTO mailing_event_definitions (organization_id, event_name, event_category, description, is_system, property_schema)
SELECT 
    '00000000-0000-0000-0000-000000000001',
    event_name,
    event_category,
    description,
    true,
    property_schema::jsonb
FROM (VALUES
    ('email_opened', 'email', 'Email was opened', '{"campaign_id": "uuid", "subject": "string"}'),
    ('email_clicked', 'email', 'Link in email was clicked', '{"campaign_id": "uuid", "link_url": "string"}'),
    ('email_bounced', 'email', 'Email bounced', '{"bounce_type": "string", "bounce_reason": "string"}'),
    ('email_unsubscribed', 'email', 'User unsubscribed', '{"campaign_id": "uuid", "reason": "string"}'),
    ('email_complained', 'email', 'Spam complaint', '{"campaign_id": "uuid"}'),
    ('page_view', 'web', 'Web page viewed', '{"url": "string", "title": "string", "referrer": "string"}'),
    ('form_submit', 'web', 'Form submitted', '{"form_id": "string", "form_name": "string"}'),
    ('add_to_cart', 'ecommerce', 'Product added to cart', '{"product_id": "string", "sku": "string", "price": "number", "quantity": "integer"}'),
    ('purchase', 'ecommerce', 'Purchase completed', '{"order_id": "string", "total": "number", "items": "array"}'),
    ('login', 'account', 'User logged in', '{"method": "string"}')
) AS t(event_name, event_category, description, property_schema)
ON CONFLICT DO NOTHING;

COMMIT;
