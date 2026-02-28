-- RSS Feed Campaigns - Automated campaign generation from RSS feeds
-- Version: 1.0.0
-- Date: 2026-02-04

-- ============================================
-- RSS CAMPAIGNS
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_rss_campaigns (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    feed_url TEXT NOT NULL,
    template_id UUID REFERENCES mailing_templates(id) ON DELETE SET NULL,
    list_id UUID REFERENCES mailing_lists(id) ON DELETE SET NULL,
    segment_id UUID REFERENCES mailing_segments(id) ON DELETE SET NULL,
    sending_profile_id UUID,
    
    -- Polling configuration
    poll_interval VARCHAR(20) DEFAULT 'daily' CHECK (poll_interval IN ('hourly', 'daily', 'weekly')),
    last_polled_at TIMESTAMP WITH TIME ZONE,
    last_item_guid TEXT,
    
    -- Campaign generation settings
    auto_send BOOLEAN DEFAULT true,
    max_items_per_poll INTEGER DEFAULT 5,
    subject_template VARCHAR(500) DEFAULT '{{rss.title}}',
    
    -- From details (fallback if template doesn't specify)
    from_name VARCHAR(255),
    from_email VARCHAR(255),
    reply_to VARCHAR(255),
    
    -- Status
    active BOOLEAN DEFAULT true,
    error_count INTEGER DEFAULT 0,
    last_error TEXT,
    last_error_at TIMESTAMP WITH TIME ZONE,
    
    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_rss_campaigns_org ON mailing_rss_campaigns(org_id);
CREATE INDEX idx_rss_campaigns_active ON mailing_rss_campaigns(active) WHERE active = true;
CREATE INDEX idx_rss_campaigns_poll ON mailing_rss_campaigns(poll_interval, last_polled_at) WHERE active = true;

-- ============================================
-- RSS SENT ITEMS (Track which items have been sent)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_rss_sent_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rss_campaign_id UUID NOT NULL REFERENCES mailing_rss_campaigns(id) ON DELETE CASCADE,
    item_guid TEXT NOT NULL,
    item_title VARCHAR(500),
    item_link TEXT,
    item_pub_date TIMESTAMP WITH TIME ZONE,
    
    -- Generated campaign reference
    campaign_id UUID REFERENCES mailing_campaigns(id) ON DELETE SET NULL,
    
    -- Status
    status VARCHAR(20) DEFAULT 'pending' CHECK (status IN ('pending', 'generated', 'sent', 'failed', 'skipped')),
    error_message TEXT,
    
    -- Timestamps
    discovered_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    sent_at TIMESTAMP WITH TIME ZONE,
    
    UNIQUE(rss_campaign_id, item_guid)
);

CREATE INDEX idx_rss_sent_items_campaign ON mailing_rss_sent_items(rss_campaign_id);
CREATE INDEX idx_rss_sent_items_guid ON mailing_rss_sent_items(rss_campaign_id, item_guid);
CREATE INDEX idx_rss_sent_items_status ON mailing_rss_sent_items(status) WHERE status = 'pending';

-- ============================================
-- RSS POLL LOG (Audit trail of polling activity)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_rss_poll_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rss_campaign_id UUID NOT NULL REFERENCES mailing_rss_campaigns(id) ON DELETE CASCADE,
    
    -- Poll results
    items_found INTEGER DEFAULT 0,
    new_items INTEGER DEFAULT 0,
    campaigns_generated INTEGER DEFAULT 0,
    
    -- Status
    status VARCHAR(20) DEFAULT 'success' CHECK (status IN ('success', 'failed', 'partial')),
    error_message TEXT,
    duration_ms INTEGER,
    
    polled_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_rss_poll_log_campaign ON mailing_rss_poll_log(rss_campaign_id, polled_at DESC);

-- ============================================
-- HELPER FUNCTION: Update RSS campaign timestamps
-- ============================================

CREATE OR REPLACE FUNCTION update_rss_campaign_timestamp() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_rss_campaign_updated
    BEFORE UPDATE ON mailing_rss_campaigns
    FOR EACH ROW
    EXECUTE FUNCTION update_rss_campaign_timestamp();

COMMIT;
