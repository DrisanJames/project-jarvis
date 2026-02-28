-- ============================================
-- SEGMENT CLEANUP & HYGIENE SYSTEM
-- Migration 006: Adds tracking for segment usage and cleanup functionality
-- ============================================

-- Add usage tracking columns to mailing_segments
ALTER TABLE mailing_segments 
ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
ADD COLUMN IF NOT EXISTS last_count_at TIMESTAMP WITH TIME ZONE,
ADD COLUMN IF NOT EXISTS keep_active BOOLEAN DEFAULT FALSE,
ADD COLUMN IF NOT EXISTS cleanup_warned_at TIMESTAMP WITH TIME ZONE,
ADD COLUMN IF NOT EXISTS cleanup_warning_sent BOOLEAN DEFAULT FALSE,
ADD COLUMN IF NOT EXISTS archived_at TIMESTAMP WITH TIME ZONE,
ADD COLUMN IF NOT EXISTS usage_count INTEGER DEFAULT 0;

-- Create index for cleanup queries
CREATE INDEX IF NOT EXISTS idx_segments_last_used ON mailing_segments(last_used_at) 
    WHERE archived_at IS NULL AND keep_active = FALSE;

CREATE INDEX IF NOT EXISTS idx_segments_cleanup_warning ON mailing_segments(cleanup_warned_at) 
    WHERE cleanup_warning_sent = TRUE AND archived_at IS NULL;

-- ============================================
-- SEGMENT CLEANUP NOTIFICATIONS TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_segment_cleanup_notifications (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    segment_id UUID NOT NULL REFERENCES mailing_segments(id) ON DELETE CASCADE,
    segment_name VARCHAR(255) NOT NULL,
    subscriber_count INTEGER DEFAULT 0,
    last_used_at TIMESTAMP WITH TIME ZONE,
    warning_sent_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    grace_period_ends_at TIMESTAMP WITH TIME ZONE NOT NULL,
    action_taken VARCHAR(20), -- 'kept', 'archived', 'deleted', NULL (pending)
    action_taken_at TIMESTAMP WITH TIME ZONE,
    action_taken_by UUID REFERENCES users(id) ON DELETE SET NULL,
    notification_email_id VARCHAR(255), -- Message ID from email provider
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_cleanup_notifications_org ON mailing_segment_cleanup_notifications(organization_id);
CREATE INDEX IF NOT EXISTS idx_cleanup_notifications_segment ON mailing_segment_cleanup_notifications(segment_id);
CREATE INDEX IF NOT EXISTS idx_cleanup_notifications_pending ON mailing_segment_cleanup_notifications(grace_period_ends_at) 
    WHERE action_taken IS NULL;

-- ============================================
-- SEGMENT USAGE LOG TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_segment_usage_log (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    segment_id UUID NOT NULL REFERENCES mailing_segments(id) ON DELETE CASCADE,
    usage_type VARCHAR(50) NOT NULL, -- 'campaign_target', 'count_query', 'preview', 'export', 'automation_trigger'
    campaign_id UUID REFERENCES mailing_campaigns(id) ON DELETE SET NULL,
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    subscriber_count INTEGER,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_segment_usage_segment ON mailing_segment_usage_log(segment_id);
CREATE INDEX IF NOT EXISTS idx_segment_usage_created ON mailing_segment_usage_log(created_at);

-- ============================================
-- CLEANUP SETTINGS TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_segment_cleanup_settings (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organization_id UUID NOT NULL UNIQUE REFERENCES organizations(id) ON DELETE CASCADE,
    enabled BOOLEAN DEFAULT TRUE,
    inactive_days_threshold INTEGER DEFAULT 30, -- Days before warning
    grace_period_days INTEGER DEFAULT 7, -- Days after warning before action
    auto_archive BOOLEAN DEFAULT TRUE, -- Archive instead of delete
    auto_delete BOOLEAN DEFAULT FALSE, -- Hard delete after archive period
    archive_retention_days INTEGER DEFAULT 90, -- Days to keep archived segments
    notify_admins BOOLEAN DEFAULT TRUE,
    admin_emails TEXT[], -- Additional admin emails to notify
    min_segment_age_days INTEGER DEFAULT 14, -- Don't warn about recently created segments
    exclude_patterns TEXT[], -- Segment name patterns to exclude from cleanup (e.g., 'system_%')
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Insert default settings for existing organizations
INSERT INTO mailing_segment_cleanup_settings (organization_id)
SELECT id FROM organizations
ON CONFLICT (organization_id) DO NOTHING;

-- ============================================
-- FUNCTIONS
-- ============================================

-- Function to update segment last_used_at when used in a campaign
CREATE OR REPLACE FUNCTION update_segment_usage()
RETURNS TRIGGER AS $$
BEGIN
    -- Update the segment's last_used_at when it's used in a campaign
    IF NEW.segment_ids IS NOT NULL AND array_length(NEW.segment_ids, 1) > 0 THEN
        UPDATE mailing_segments 
        SET 
            last_used_at = NOW(),
            usage_count = usage_count + 1,
            -- Reset cleanup warning if segment is being used
            cleanup_warning_sent = CASE WHEN cleanup_warning_sent THEN FALSE ELSE cleanup_warning_sent END,
            cleanup_warned_at = CASE WHEN cleanup_warning_sent THEN NULL ELSE cleanup_warned_at END
        WHERE id = ANY(NEW.segment_ids);
        
        -- Log the usage
        INSERT INTO mailing_segment_usage_log (segment_id, usage_type, campaign_id, subscriber_count)
        SELECT unnest(NEW.segment_ids), 'campaign_target', NEW.id, NULL;
    END IF;
    
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Create trigger on campaigns table (if segment_ids column exists)
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns 
        WHERE table_name = 'mailing_campaigns' AND column_name = 'segment_ids'
    ) THEN
        DROP TRIGGER IF EXISTS trg_update_segment_usage ON mailing_campaigns;
        CREATE TRIGGER trg_update_segment_usage
            AFTER INSERT OR UPDATE OF segment_ids ON mailing_campaigns
            FOR EACH ROW
            EXECUTE FUNCTION update_segment_usage();
    END IF;
END $$;

-- Function to count segment subscribers and update tracking
CREATE OR REPLACE FUNCTION count_segment_subscribers(
    p_segment_id UUID,
    p_update_usage BOOLEAN DEFAULT TRUE
) RETURNS INTEGER AS $$
DECLARE
    v_count INTEGER;
    v_org_id UUID;
BEGIN
    -- Get organization ID
    SELECT organization_id INTO v_org_id FROM mailing_segments WHERE id = p_segment_id;
    
    -- This is a placeholder - actual count would use the segment conditions
    -- For now, just return the cached count
    SELECT subscriber_count INTO v_count FROM mailing_segments WHERE id = p_segment_id;
    
    -- Update usage tracking
    IF p_update_usage THEN
        UPDATE mailing_segments 
        SET 
            last_used_at = NOW(),
            last_count_at = NOW(),
            usage_count = usage_count + 1,
            -- Reset cleanup warning when actively used
            cleanup_warning_sent = FALSE,
            cleanup_warned_at = NULL
        WHERE id = p_segment_id;
        
        -- Log the usage
        INSERT INTO mailing_segment_usage_log (segment_id, usage_type, subscriber_count)
        VALUES (p_segment_id, 'count_query', v_count);
    END IF;
    
    RETURN COALESCE(v_count, 0);
END;
$$ LANGUAGE plpgsql;

-- Function to get segments due for cleanup warning
CREATE OR REPLACE FUNCTION get_segments_for_cleanup_warning(
    p_organization_id UUID
) RETURNS TABLE (
    segment_id UUID,
    segment_name VARCHAR(255),
    subscriber_count INTEGER,
    last_used_at TIMESTAMP WITH TIME ZONE,
    days_inactive INTEGER,
    created_at TIMESTAMP WITH TIME ZONE
) AS $$
DECLARE
    v_settings RECORD;
BEGIN
    -- Get cleanup settings
    SELECT * INTO v_settings 
    FROM mailing_segment_cleanup_settings 
    WHERE organization_id = p_organization_id;
    
    -- If no settings or disabled, return empty
    IF NOT FOUND OR NOT v_settings.enabled THEN
        RETURN;
    END IF;
    
    RETURN QUERY
    SELECT 
        s.id as segment_id,
        s.name as segment_name,
        s.subscriber_count,
        s.last_used_at,
        EXTRACT(DAY FROM NOW() - COALESCE(s.last_used_at, s.created_at))::INTEGER as days_inactive,
        s.created_at
    FROM mailing_segments s
    WHERE s.organization_id = p_organization_id
        AND s.archived_at IS NULL
        AND s.keep_active = FALSE
        AND s.cleanup_warning_sent = FALSE
        -- Segment is older than min age
        AND s.created_at < NOW() - (v_settings.min_segment_age_days || ' days')::INTERVAL
        -- Segment hasn't been used in threshold days
        AND COALESCE(s.last_used_at, s.created_at) < NOW() - (v_settings.inactive_days_threshold || ' days')::INTERVAL
        -- Exclude system patterns
        AND NOT EXISTS (
            SELECT 1 FROM unnest(v_settings.exclude_patterns) AS pattern
            WHERE s.name ILIKE pattern
        )
    ORDER BY s.last_used_at ASC NULLS FIRST;
END;
$$ LANGUAGE plpgsql;

-- Function to get segments ready for cleanup (grace period expired)
CREATE OR REPLACE FUNCTION get_segments_for_cleanup_action(
    p_organization_id UUID
) RETURNS TABLE (
    segment_id UUID,
    segment_name VARCHAR(255),
    subscriber_count INTEGER,
    warned_at TIMESTAMP WITH TIME ZONE,
    grace_period_ends_at TIMESTAMP WITH TIME ZONE
) AS $$
DECLARE
    v_settings RECORD;
BEGIN
    SELECT * INTO v_settings 
    FROM mailing_segment_cleanup_settings 
    WHERE organization_id = p_organization_id;
    
    IF NOT FOUND OR NOT v_settings.enabled THEN
        RETURN;
    END IF;
    
    RETURN QUERY
    SELECT 
        n.segment_id,
        n.segment_name,
        n.subscriber_count,
        n.warning_sent_at as warned_at,
        n.grace_period_ends_at
    FROM mailing_segment_cleanup_notifications n
    JOIN mailing_segments s ON s.id = n.segment_id
    WHERE n.organization_id = p_organization_id
        AND n.action_taken IS NULL
        AND n.grace_period_ends_at < NOW()
        AND s.archived_at IS NULL
        AND s.keep_active = FALSE
    ORDER BY n.grace_period_ends_at ASC;
END;
$$ LANGUAGE plpgsql;

-- ============================================
-- COMMENTS
-- ============================================

COMMENT ON TABLE mailing_segment_cleanup_notifications IS 'Tracks cleanup warnings sent to admins for unused segments';
COMMENT ON TABLE mailing_segment_usage_log IS 'Audit log of segment usage for analytics and cleanup decisions';
COMMENT ON TABLE mailing_segment_cleanup_settings IS 'Per-organization settings for segment cleanup automation';
COMMENT ON COLUMN mailing_segments.last_used_at IS 'Last time segment was used in a campaign, export, or count query';
COMMENT ON COLUMN mailing_segments.keep_active IS 'If true, segment will never be auto-archived regardless of usage';
COMMENT ON COLUMN mailing_segments.cleanup_warning_sent IS 'True if a cleanup warning has been sent for this segment';
COMMENT ON COLUMN mailing_segments.archived_at IS 'When segment was archived (soft delete)';
