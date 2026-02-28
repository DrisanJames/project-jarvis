-- Custom Fields System Migration
-- Version: 013
-- Date: 2026-02-05
-- Purpose: Enable custom field definitions for non-standard CSV columns during list uploads

-- ============================================
-- CUSTOM FIELD DEFINITIONS
-- ============================================
-- Stores custom field schemas per organization
-- Enables type validation and UI rendering

CREATE TABLE IF NOT EXISTS mailing_custom_field_definitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name VARCHAR(100) NOT NULL,  -- Internal name (snake_case)
    display_name VARCHAR(255) NOT NULL,  -- Friendly name for UI
    field_type VARCHAR(50) NOT NULL CHECK (field_type IN ('string', 'number', 'boolean', 'date', 'datetime', 'enum')),
    enum_values JSONB,  -- For enum types: ["highly_engaged", "engager", "disengaged", "complainer", "no_data"]
    default_value TEXT,
    is_required BOOLEAN DEFAULT FALSE,
    description TEXT,
    is_active BOOLEAN DEFAULT TRUE,
    sort_order INTEGER DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(organization_id, name)
);

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_custom_field_defs_org ON mailing_custom_field_definitions(organization_id);
CREATE INDEX IF NOT EXISTS idx_custom_field_defs_active ON mailing_custom_field_definitions(organization_id, is_active) WHERE is_active = TRUE;
CREATE INDEX IF NOT EXISTS idx_custom_field_defs_name ON mailing_custom_field_definitions(organization_id, name);

-- ============================================
-- IMPORT JOB CUSTOM FIELD MAPPINGS
-- ============================================
-- Tracks which custom fields were used during an import

CREATE TABLE IF NOT EXISTS mailing_import_custom_field_mappings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    import_job_id UUID NOT NULL,
    custom_field_id UUID NOT NULL REFERENCES mailing_custom_field_definitions(id) ON DELETE CASCADE,
    csv_column_name VARCHAR(255) NOT NULL,
    csv_column_index INTEGER NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_import_custom_field_job ON mailing_import_custom_field_mappings(import_job_id);

-- ============================================
-- CUSTOM FIELD USAGE STATISTICS
-- ============================================
-- Tracks usage of custom fields for analytics and cleanup recommendations

CREATE TABLE IF NOT EXISTS mailing_custom_field_stats (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    custom_field_id UUID NOT NULL REFERENCES mailing_custom_field_definitions(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    subscribers_with_value INTEGER DEFAULT 0,
    total_subscribers INTEGER DEFAULT 0,
    unique_values_count INTEGER DEFAULT 0,
    most_common_values JSONB,  -- Top 10 values and their counts
    last_calculated_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(custom_field_id)
);

CREATE INDEX IF NOT EXISTS idx_custom_field_stats_org ON mailing_custom_field_stats(organization_id);

-- ============================================
-- STANDARD FIELDS CONFIGURATION
-- ============================================
-- Defines the standard/built-in fields that are handled specially

CREATE TABLE IF NOT EXISTS mailing_standard_field_definitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(100) NOT NULL UNIQUE,
    display_name VARCHAR(255) NOT NULL,
    field_type VARCHAR(50) NOT NULL,
    column_aliases JSONB NOT NULL DEFAULT '[]',  -- List of CSV column names that map to this field
    is_required BOOLEAN DEFAULT FALSE,
    database_column VARCHAR(100),  -- The actual column in mailing_subscribers table
    description TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Insert standard field definitions
INSERT INTO mailing_standard_field_definitions (name, display_name, field_type, column_aliases, is_required, database_column, description) VALUES
    ('email', 'Email Address', 'email', '["email", "email_address", "e-mail", "emailaddress", "mail", "subscriber_email", "address"]', TRUE, 'email', 'Primary email address (required)'),
    ('first_name', 'First Name', 'string', '["first_name", "firstname", "first", "fname", "given_name", "givenname"]', FALSE, 'first_name', 'Subscriber first name'),
    ('last_name', 'Last Name', 'string', '["last_name", "lastname", "last", "lname", "surname", "family_name", "familyname"]', FALSE, 'last_name', 'Subscriber last name'),
    ('status', 'Status', 'enum', '["status", "subscription_status", "sub_status"]', FALSE, 'status', 'Subscription status')
ON CONFLICT (name) DO NOTHING;

-- ============================================
-- HELPER FUNCTION: Get Standard Field Names
-- ============================================

CREATE OR REPLACE FUNCTION get_standard_field_names()
RETURNS TABLE(name VARCHAR, aliases JSONB) AS $$
BEGIN
    RETURN QUERY
    SELECT 
        sf.name::VARCHAR,
        sf.column_aliases
    FROM mailing_standard_field_definitions sf;
END;
$$ LANGUAGE plpgsql STABLE;

-- ============================================
-- HELPER FUNCTION: Detect Non-Standard Columns
-- ============================================
-- Given a list of column headers, returns those that don't match standard fields

CREATE OR REPLACE FUNCTION detect_non_standard_columns(headers TEXT[])
RETURNS TABLE(column_name TEXT, suggested_type VARCHAR) AS $$
DECLARE
    header TEXT;
    normalized_header TEXT;
    is_standard BOOLEAN;
    std_name VARCHAR;
    std_aliases JSONB;
    alias TEXT;
BEGIN
    FOREACH header IN ARRAY headers
    LOOP
        normalized_header := lower(trim(regexp_replace(header, '[\s\-]+', '_', 'g')));
        is_standard := FALSE;
        
        -- Check against standard fields and their aliases
        FOR std_name, std_aliases IN SELECT sf.name, sf.column_aliases FROM mailing_standard_field_definitions sf
        LOOP
            -- Check if header matches the standard field name
            IF normalized_header = std_name THEN
                is_standard := TRUE;
                EXIT;
            END IF;
            
            -- Check aliases
            FOR alias IN SELECT jsonb_array_elements_text(std_aliases)
            LOOP
                IF normalized_header = alias THEN
                    is_standard := TRUE;
                    EXIT;
                END IF;
            END LOOP;
            
            IF is_standard THEN
                EXIT;
            END IF;
        END LOOP;
        
        -- If not standard, return it with suggested type
        IF NOT is_standard THEN
            -- Suggest type based on column name patterns
            suggested_type := CASE
                WHEN normalized_header ~* '(is_|has_|can_|should_|enabled|active|verified|valid)' THEN 'boolean'
                WHEN normalized_header ~* '(date|_at$|_on$|created|updated|timestamp)' THEN 'datetime'
                WHEN normalized_header ~* '(count|amount|score|number|qty|quantity|total|sum|age)' THEN 'number'
                ELSE 'string'
            END;
            
            RETURN QUERY SELECT header, suggested_type;
        END IF;
    END LOOP;
END;
$$ LANGUAGE plpgsql STABLE;

-- ============================================
-- TRIGGER: Update timestamps
-- ============================================

CREATE OR REPLACE FUNCTION update_custom_field_timestamp()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_custom_field_updated ON mailing_custom_field_definitions;
CREATE TRIGGER trigger_custom_field_updated
    BEFORE UPDATE ON mailing_custom_field_definitions
    FOR EACH ROW
    EXECUTE FUNCTION update_custom_field_timestamp();

-- ============================================
-- COMMENTS
-- ============================================

COMMENT ON TABLE mailing_custom_field_definitions IS 'Stores custom field schemas for non-standard CSV columns';
COMMENT ON COLUMN mailing_custom_field_definitions.name IS 'Internal identifier (snake_case, unique per org)';
COMMENT ON COLUMN mailing_custom_field_definitions.display_name IS 'User-friendly label shown in UI';
COMMENT ON COLUMN mailing_custom_field_definitions.field_type IS 'Data type: string, number, boolean, date, datetime, enum';
COMMENT ON COLUMN mailing_custom_field_definitions.enum_values IS 'For enum fields: array of allowed values';
COMMENT ON TABLE mailing_standard_field_definitions IS 'System-defined standard fields with their CSV column aliases';

COMMIT;
