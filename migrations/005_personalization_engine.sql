-- Personalization Engine - Database Schema
-- Version: 1.0.0
-- Date: 2026-02-01
-- 
-- This migration adds support for dynamic data injection (merge tags)
-- including custom merge tag definitions and template validation.

BEGIN;

-- ============================================
-- MERGE TAG DEFINITIONS (Schema Registry)
-- ============================================
-- Allows organizations to define custom merge tags beyond the defaults
-- Useful for integration with external data sources

CREATE TABLE IF NOT EXISTS mailing_merge_tags (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- Tag definition
    tag_key VARCHAR(100) NOT NULL,
    tag_label VARCHAR(255) NOT NULL,
    tag_category VARCHAR(50) NOT NULL DEFAULT 'custom',
    
    -- Data source mapping
    source_type VARCHAR(50) NOT NULL DEFAULT 'json_field',
    -- Types: 'column' (direct DB column), 'json_field' (custom_fields JSONB), 
    --        'computed' (mailing_subscriber_computed), 'function' (dynamic calculation)
    source_path TEXT NOT NULL,
    -- Examples: "first_name" (column), "custom_fields->>'company'" (json), "computed.ltv" (computed)
    
    -- Metadata
    data_type VARCHAR(50) DEFAULT 'string',
    -- Types: string, number, integer, decimal, boolean, date, datetime, array
    description TEXT,
    
    -- Default value when data is missing
    default_value TEXT,
    
    -- Sample value for preview
    sample_value TEXT,
    
    -- Display settings
    display_order INTEGER DEFAULT 0,
    is_visible BOOLEAN DEFAULT TRUE,
    is_system BOOLEAN DEFAULT FALSE,
    
    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(organization_id, tag_key)
);

CREATE INDEX idx_merge_tags_org ON mailing_merge_tags(organization_id);
CREATE INDEX idx_merge_tags_category ON mailing_merge_tags(tag_category);

-- ============================================
-- TEMPLATE VALIDATION CACHE
-- ============================================
-- Caches parsed template syntax errors to avoid re-validation

CREATE TABLE IF NOT EXISTS mailing_template_validations (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    
    -- What was validated
    content_hash VARCHAR(64) NOT NULL,  -- SHA-256 of template content
    content_type VARCHAR(50) NOT NULL,  -- 'subject', 'html', 'text'
    
    -- Validation results
    is_valid BOOLEAN NOT NULL,
    syntax_errors JSONB DEFAULT '[]',
    -- Example: [{"line": 5, "message": "Unclosed tag", "tag": "{% if %}"}]
    
    undefined_variables JSONB DEFAULT '[]',
    -- Example: [{"variable": "custom.xyz", "message": "May not exist for all subscribers"}]
    
    -- Timestamps
    validated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    
    UNIQUE(organization_id, content_hash, content_type)
);

CREATE INDEX idx_template_validations_hash ON mailing_template_validations(content_hash);

-- ============================================
-- PERSONALIZATION ANALYTICS
-- ============================================
-- Tracks which merge tags are being used in campaigns

CREATE TABLE IF NOT EXISTS mailing_personalization_usage (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL,
    campaign_id UUID,
    
    -- Usage tracking
    tag_key VARCHAR(100) NOT NULL,
    usage_context VARCHAR(50) NOT NULL,  -- 'subject', 'html', 'text', 'preheader'
    
    -- Stats
    total_renders INTEGER DEFAULT 0,
    successful_renders INTEGER DEFAULT 0,
    fallback_renders INTEGER DEFAULT 0,  -- Used default value
    
    -- Sample data
    sample_rendered_value TEXT,
    
    -- Timestamps
    first_used_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    last_used_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_personalization_usage_org ON mailing_personalization_usage(organization_id);
CREATE INDEX idx_personalization_usage_campaign ON mailing_personalization_usage(campaign_id);
CREATE INDEX idx_personalization_usage_tag ON mailing_personalization_usage(tag_key);

-- ============================================
-- INSERT DEFAULT MERGE TAGS
-- ============================================
-- System-defined merge tags available to all organizations

INSERT INTO mailing_merge_tags (
    organization_id, tag_key, tag_label, tag_category, source_type, source_path, 
    data_type, description, sample_value, is_system, display_order
)
SELECT 
    '00000000-0000-0000-0000-000000000001',
    tag_key,
    tag_label,
    tag_category,
    source_type,
    source_path,
    data_type,
    description,
    sample_value,
    true,
    display_order
FROM (VALUES
    -- Profile tags
    ('first_name', 'First Name', 'profile', 'column', 'first_name', 'string', 'Subscriber first name', 'John', 10),
    ('last_name', 'Last Name', 'profile', 'column', 'last_name', 'string', 'Subscriber last name', 'Doe', 20),
    ('email', 'Email Address', 'profile', 'column', 'email', 'string', 'Subscriber email', 'john@example.com', 30),
    ('full_name', 'Full Name', 'profile', 'computed', 'first_name || '' '' || last_name', 'string', 'First and last name combined', 'John Doe', 40),
    
    -- Engagement tags
    ('engagement.score', 'Engagement Score', 'engagement', 'column', 'engagement_score', 'number', 'Subscriber engagement score (0-100)', '85', 100),
    ('engagement.total_opens', 'Total Opens', 'engagement', 'column', 'total_opens', 'integer', 'Total email opens', '42', 110),
    ('engagement.total_clicks', 'Total Clicks', 'engagement', 'column', 'total_clicks', 'integer', 'Total link clicks', '15', 120),
    
    -- System tags
    ('system.current_date', 'Current Date', 'system', 'function', 'NOW()', 'date', 'Today''s date', 'February 1, 2026', 200),
    ('system.current_year', 'Current Year', 'system', 'function', 'EXTRACT(YEAR FROM NOW())', 'integer', 'Current year', '2026', 210),
    ('system.unsubscribe_url', 'Unsubscribe Link', 'system', 'function', 'generate_unsubscribe_url()', 'string', 'One-click unsubscribe URL', 'https://...', 220),
    ('system.preferences_url', 'Preferences Link', 'system', 'function', 'generate_preferences_url()', 'string', 'Email preferences URL', 'https://...', 230)
) AS v(tag_key, tag_label, tag_category, source_type, source_path, data_type, description, sample_value, display_order)
ON CONFLICT (organization_id, tag_key) DO NOTHING;

-- ============================================
-- HELPER FUNCTION: Check for Liquid syntax
-- ============================================
-- Simple function to detect if content contains Liquid tags

CREATE OR REPLACE FUNCTION has_liquid_syntax(content TEXT)
RETURNS BOOLEAN AS $$
BEGIN
    RETURN content ~ '\{\{.*\}\}' OR content ~ '\{%.*%\}';
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- ============================================
-- TRIGGER: Update timestamps
-- ============================================

CREATE OR REPLACE FUNCTION update_merge_tag_timestamp()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_merge_tag_timestamp ON mailing_merge_tags;
CREATE TRIGGER trigger_merge_tag_timestamp
    BEFORE UPDATE ON mailing_merge_tags
    FOR EACH ROW EXECUTE FUNCTION update_merge_tag_timestamp();

COMMIT;

-- ============================================
-- DOCUMENTATION
-- ============================================
-- 
-- MERGE TAG SYNTAX (Liquid):
-- 
-- Basic variable:
--   {{ first_name }}
--   {{ custom.company }}
-- 
-- With default value:
--   {{ first_name | default: "Friend" }}
-- 
-- With filter:
--   {{ price | currency }}
--   {{ signup_date | date: "%B %d, %Y" }}
-- 
-- Conditional:
--   {% if custom.is_vip %}
--     VIP content here
--   {% else %}
--     Standard content
--   {% endif %}
-- 
-- Loop:
--   {% for item in custom.interests %}
--     {{ item }}
--   {% endfor %}
-- 
-- AVAILABLE FILTERS:
--   default       - Fallback value if empty
--   capitalize    - First letter uppercase
--   titlecase     - Title Case All Words
--   uppercase     - ALL UPPERCASE
--   lowercase     - all lowercase
--   truncate      - Truncate with ellipsis
--   currency      - Format as $XX.XX
--   date          - Format date
--   relative_time - "3 days ago"
--   gravatar      - Generate Gravatar URL
--   urlencode     - URL encode
--   escape        - HTML escape
