-- Everflow Creative Integration + Tracking Links
-- Version: 019
-- Date: 2026-02-06
-- Description: Adds Everflow creative selection, tracking link builder,
--              affiliate/offer encoding mappings, DATA_SET custom field,
--              and manual link offer configuration

-- ============================================================================
-- AFFILIATE ENCODING LOOKUP
-- Maps affiliate IDs to encoded tracking URL segments
-- e.g., 9533 -> JFR89NB, 9572 -> JHJSFL9
-- ============================================================================
CREATE TABLE IF NOT EXISTS mailing_affiliate_encodings (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    organization_id UUID NOT NULL REFERENCES organizations(id),
    affiliate_id VARCHAR(20) NOT NULL,
    encoded_value VARCHAR(50) NOT NULL,
    affiliate_name VARCHAR(255),
    is_default BOOLEAN DEFAULT false,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(organization_id, affiliate_id)
);

-- ============================================================================
-- OFFER ENCODING LOOKUP
-- Maps offer IDs to encoded tracking URL segments
-- e.g., 529 -> X7LBB6 (Sam's Club)
-- ============================================================================
CREATE TABLE IF NOT EXISTS mailing_offer_encodings (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    organization_id UUID NOT NULL REFERENCES organizations(id),
    offer_id VARCHAR(20) NOT NULL,
    encoded_value VARCHAR(50) NOT NULL,
    offer_name VARCHAR(255),
    tracking_domain VARCHAR(255) DEFAULT 'https://www.si3p4trk.com',
    requires_manual_link BOOLEAN DEFAULT false,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(organization_id, offer_id)
);

-- ============================================================================
-- MANUAL LINK OFFERS
-- Offers that require creative-specific tracking links (manual prompt)
-- e.g., Lifelock, Norton, Farmers, 3 Day Blinds
-- ============================================================================
CREATE TABLE IF NOT EXISTS mailing_manual_link_offers (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    organization_id UUID NOT NULL REFERENCES organizations(id),
    offer_name_pattern VARCHAR(255) NOT NULL,
    description TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- ============================================================================
-- DATA_SET CUSTOM FIELD ON SUBSCRIBERS
-- 3-letter code identifying data source. Default IGN for non-suppressed.
-- ============================================================================
ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS data_set VARCHAR(10) DEFAULT 'IGN';
CREATE INDEX IF NOT EXISTS idx_subscribers_data_set ON mailing_subscribers(data_set);

-- ============================================================================
-- CAMPAIGN EVERFLOW FIELDS
-- Store selected creative and tracking link template on campaigns
-- ============================================================================
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS everflow_creative_id INTEGER;
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS everflow_offer_id INTEGER;
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS tracking_link_template TEXT;

-- ============================================================================
-- SEED DATA
-- ============================================================================

-- Known affiliate encodings
INSERT INTO mailing_affiliate_encodings (organization_id, affiliate_id, encoded_value, affiliate_name, is_default)
VALUES 
    ('00000000-0000-0000-0000-000000000001', '9533', 'JFR89NB', 'Ignite Media Internal Email', true),
    ('00000000-0000-0000-0000-000000000001', '9572', 'JHJSFL9', 'Ignite Media Internal Email 4', false)
ON CONFLICT (organization_id, affiliate_id) DO NOTHING;

-- Known offer encodings
INSERT INTO mailing_offer_encodings (organization_id, offer_id, encoded_value, offer_name, requires_manual_link)
VALUES 
    ('00000000-0000-0000-0000-000000000001', '529', 'X7LBB6', 'Sam''s Club CPS - Club Membership', false),
    ('00000000-0000-0000-0000-000000000001', '420', 'X7LBB6', 'Sam''s Club CPM - NEW - EXCLUSIVE', false)
ON CONFLICT (organization_id, offer_id) DO NOTHING;

-- Manual link offer patterns
INSERT INTO mailing_manual_link_offers (organization_id, offer_name_pattern, description)
VALUES 
    ('00000000-0000-0000-0000-000000000001', 'Lifelock', 'Creative-specific tracking links required'),
    ('00000000-0000-0000-0000-000000000001', 'Norton', 'Creative-specific tracking links required'),
    ('00000000-0000-0000-0000-000000000001', 'Farmers', 'Creative-specific tracking links required'),
    ('00000000-0000-0000-0000-000000000001', '3 Day Blinds', 'Creative-specific tracking links required')
ON CONFLICT DO NOTHING;
