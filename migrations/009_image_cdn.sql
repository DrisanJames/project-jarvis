-- ============================================
-- IMAGE CDN & HOSTING SYSTEM
-- Migration 009: Image hosting with CDN support for email campaigns
-- ============================================

-- ============================================
-- HOSTED IMAGES TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_hosted_images (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    filename VARCHAR(255) NOT NULL,
    original_filename VARCHAR(255) NOT NULL,
    content_type VARCHAR(100) NOT NULL,
    size BIGINT NOT NULL,
    width INT,
    height INT,
    s3_key TEXT NOT NULL,
    s3_key_thumbnail TEXT,
    s3_key_medium TEXT,
    s3_key_large TEXT,
    cdn_url TEXT NOT NULL,
    cdn_url_thumbnail TEXT,
    cdn_url_medium TEXT,
    cdn_url_large TEXT,
    checksum VARCHAR(64),
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Indexes for hosted images
CREATE INDEX IF NOT EXISTS idx_hosted_images_org ON mailing_hosted_images(org_id);
CREATE INDEX IF NOT EXISTS idx_hosted_images_created ON mailing_hosted_images(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_hosted_images_filename ON mailing_hosted_images(org_id, filename);
CREATE INDEX IF NOT EXISTS idx_hosted_images_content_type ON mailing_hosted_images(content_type);

-- ============================================
-- IMAGE DOMAINS TABLE (Custom CDN Domains)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_image_domains (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    domain VARCHAR(255) NOT NULL UNIQUE,
    verified BOOLEAN DEFAULT false,
    verification_token VARCHAR(255),
    verification_method VARCHAR(50) DEFAULT 'dns_txt', -- dns_txt, dns_cname, file
    ssl_status VARCHAR(50) DEFAULT 'pending', -- pending, active, failed
    ssl_certificate_arn TEXT,
    cloudfront_distribution_id TEXT,
    last_verified_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Indexes for image domains
CREATE INDEX IF NOT EXISTS idx_image_domains_org ON mailing_image_domains(org_id);
CREATE INDEX IF NOT EXISTS idx_image_domains_domain ON mailing_image_domains(domain);
CREATE INDEX IF NOT EXISTS idx_image_domains_verified ON mailing_image_domains(verified) WHERE verified = true;

-- ============================================
-- IMAGE USAGE TRACKING TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_image_usage (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    image_id UUID NOT NULL REFERENCES mailing_hosted_images(id) ON DELETE CASCADE,
    campaign_id UUID,
    template_id UUID,
    usage_type VARCHAR(50) NOT NULL, -- campaign, template, signature, preview
    used_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_image_usage_image ON mailing_image_usage(image_id);
CREATE INDEX IF NOT EXISTS idx_image_usage_campaign ON mailing_image_usage(campaign_id);

-- ============================================
-- IMAGE CDN SETTINGS TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_image_cdn_settings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL UNIQUE,
    default_quality INT DEFAULT 85, -- JPEG quality 1-100
    max_file_size_mb INT DEFAULT 10,
    allowed_types TEXT[] DEFAULT ARRAY['image/jpeg', 'image/png', 'image/gif', 'image/webp'],
    auto_optimize BOOLEAN DEFAULT true,
    generate_thumbnails BOOLEAN DEFAULT true,
    thumbnail_width INT DEFAULT 150,
    medium_width INT DEFAULT 600,
    large_width INT DEFAULT 1200,
    strip_metadata BOOLEAN DEFAULT true, -- Remove EXIF data for privacy
    default_cache_ttl INT DEFAULT 31536000, -- 1 year in seconds
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Insert default settings for existing organizations
INSERT INTO mailing_image_cdn_settings (org_id)
SELECT id FROM organizations
ON CONFLICT (org_id) DO NOTHING;

-- ============================================
-- FUNCTIONS
-- ============================================

-- Function to update updated_at timestamp
CREATE OR REPLACE FUNCTION update_image_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Triggers for updated_at
DROP TRIGGER IF EXISTS trg_hosted_images_updated ON mailing_hosted_images;
CREATE TRIGGER trg_hosted_images_updated
    BEFORE UPDATE ON mailing_hosted_images
    FOR EACH ROW
    EXECUTE FUNCTION update_image_updated_at();

DROP TRIGGER IF EXISTS trg_image_domains_updated ON mailing_image_domains;
CREATE TRIGGER trg_image_domains_updated
    BEFORE UPDATE ON mailing_image_domains
    FOR EACH ROW
    EXECUTE FUNCTION update_image_updated_at();

DROP TRIGGER IF EXISTS trg_image_cdn_settings_updated ON mailing_image_cdn_settings;
CREATE TRIGGER trg_image_cdn_settings_updated
    BEFORE UPDATE ON mailing_image_cdn_settings
    FOR EACH ROW
    EXECUTE FUNCTION update_image_updated_at();

-- Function to get image storage statistics per organization
CREATE OR REPLACE FUNCTION get_image_storage_stats(p_org_id UUID)
RETURNS TABLE (
    total_images BIGINT,
    total_size_bytes BIGINT,
    total_size_mb NUMERIC,
    images_this_month BIGINT,
    size_this_month_bytes BIGINT
) AS $$
BEGIN
    RETURN QUERY
    SELECT 
        COUNT(*)::BIGINT as total_images,
        COALESCE(SUM(size), 0)::BIGINT as total_size_bytes,
        ROUND(COALESCE(SUM(size), 0) / 1048576.0, 2) as total_size_mb,
        COUNT(*) FILTER (WHERE created_at >= date_trunc('month', NOW()))::BIGINT as images_this_month,
        COALESCE(SUM(size) FILTER (WHERE created_at >= date_trunc('month', NOW())), 0)::BIGINT as size_this_month_bytes
    FROM mailing_hosted_images
    WHERE org_id = p_org_id;
END;
$$ LANGUAGE plpgsql;

-- Function to clean up orphaned images (not used in any campaign/template for X days)
CREATE OR REPLACE FUNCTION get_orphaned_images(
    p_org_id UUID,
    p_days_unused INT DEFAULT 90
)
RETURNS TABLE (
    image_id UUID,
    filename VARCHAR(255),
    size BIGINT,
    created_at TIMESTAMP WITH TIME ZONE,
    last_used_at TIMESTAMP WITH TIME ZONE
) AS $$
BEGIN
    RETURN QUERY
    SELECT 
        i.id as image_id,
        i.filename,
        i.size,
        i.created_at,
        MAX(u.used_at) as last_used_at
    FROM mailing_hosted_images i
    LEFT JOIN mailing_image_usage u ON u.image_id = i.id
    WHERE i.org_id = p_org_id
    GROUP BY i.id, i.filename, i.size, i.created_at
    HAVING MAX(u.used_at) IS NULL 
        OR MAX(u.used_at) < NOW() - (p_days_unused || ' days')::INTERVAL
    ORDER BY i.created_at ASC;
END;
$$ LANGUAGE plpgsql;

-- ============================================
-- COMMENTS
-- ============================================

COMMENT ON TABLE mailing_hosted_images IS 'Images uploaded and hosted for use in email campaigns';
COMMENT ON TABLE mailing_image_domains IS 'Custom CDN domains for image hosting';
COMMENT ON TABLE mailing_image_usage IS 'Tracks which images are used in campaigns and templates';
COMMENT ON TABLE mailing_image_cdn_settings IS 'Per-organization CDN and image processing settings';
COMMENT ON COLUMN mailing_hosted_images.s3_key IS 'S3 object key for original image';
COMMENT ON COLUMN mailing_hosted_images.cdn_url IS 'Public CDN URL for the original image';
COMMENT ON COLUMN mailing_hosted_images.checksum IS 'SHA-256 checksum for deduplication';
COMMENT ON COLUMN mailing_image_domains.verification_token IS 'Token for domain ownership verification';
COMMENT ON COLUMN mailing_image_cdn_settings.strip_metadata IS 'Remove EXIF/metadata from images for privacy';
