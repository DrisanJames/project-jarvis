-- Migration: 014_aws_tracking_infrastructure.sql
-- Description: Add AWS infrastructure fields for tracking domains and image CDN
-- Date: 2026-02-05

-- ============================================
-- UPDATE TRACKING DOMAINS FOR AWS INFRASTRUCTURE
-- ============================================

-- Add AWS-specific columns to tracking domains
ALTER TABLE mailing_tracking_domains 
ADD COLUMN IF NOT EXISTS ssl_status VARCHAR(50) DEFAULT 'pending',
ADD COLUMN IF NOT EXISTS cloudfront_id VARCHAR(100),
ADD COLUMN IF NOT EXISTS cloudfront_domain VARCHAR(255),
ADD COLUMN IF NOT EXISTS acm_cert_arn TEXT,
ADD COLUMN IF NOT EXISTS route53_record_id VARCHAR(100),
ADD COLUMN IF NOT EXISTS origin_server VARCHAR(255);

-- Comment updates
COMMENT ON COLUMN mailing_tracking_domains.ssl_status IS 'SSL certificate status: pending, validating, active, failed';
COMMENT ON COLUMN mailing_tracking_domains.cloudfront_id IS 'AWS CloudFront distribution ID';
COMMENT ON COLUMN mailing_tracking_domains.cloudfront_domain IS 'CloudFront distribution domain (xxx.cloudfront.net)';
COMMENT ON COLUMN mailing_tracking_domains.acm_cert_arn IS 'AWS ACM certificate ARN';
COMMENT ON COLUMN mailing_tracking_domains.route53_record_id IS 'Route53 record change ID';
COMMENT ON COLUMN mailing_tracking_domains.origin_server IS 'Origin server for CloudFront (API server for tracking)';

-- ============================================
-- UPDATE IMAGE DOMAINS FOR AWS INFRASTRUCTURE
-- ============================================

-- Add AWS-specific columns to image domains
ALTER TABLE mailing_image_domains
ADD COLUMN IF NOT EXISTS s3_bucket VARCHAR(255),
ADD COLUMN IF NOT EXISTS cloudfront_domain VARCHAR(255),
ADD COLUMN IF NOT EXISTS route53_record_id VARCHAR(100),
ADD COLUMN IF NOT EXISTS origin_access_identity_id VARCHAR(100);

-- Update ssl_certificate_arn to acm_cert_arn for consistency (if exists)
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns 
               WHERE table_name = 'mailing_image_domains' 
               AND column_name = 'ssl_certificate_arn') THEN
        ALTER TABLE mailing_image_domains 
        RENAME COLUMN ssl_certificate_arn TO acm_cert_arn;
    ELSE
        ALTER TABLE mailing_image_domains
        ADD COLUMN IF NOT EXISTS acm_cert_arn TEXT;
    END IF;
END $$;

COMMENT ON COLUMN mailing_image_domains.s3_bucket IS 'S3 bucket name for image storage';
COMMENT ON COLUMN mailing_image_domains.cloudfront_domain IS 'CloudFront distribution domain';
COMMENT ON COLUMN mailing_image_domains.route53_record_id IS 'Route53 record change ID';
COMMENT ON COLUMN mailing_image_domains.origin_access_identity_id IS 'CloudFront OAI for S3 access';

-- ============================================
-- AWS INFRASTRUCTURE PROVISIONING LOG
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_aws_provisioning_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_type VARCHAR(50) NOT NULL, -- tracking_domain, image_domain, s3_bucket, cloudfront, acm
    resource_id UUID NOT NULL,
    action VARCHAR(50) NOT NULL, -- create, update, delete, validate
    aws_resource_id VARCHAR(255),
    aws_resource_arn TEXT,
    status VARCHAR(50) NOT NULL, -- pending, in_progress, completed, failed
    request_params JSONB,
    response_data JSONB,
    error_message TEXT,
    started_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    completed_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_aws_provisioning_resource ON mailing_aws_provisioning_log(resource_type, resource_id);
CREATE INDEX IF NOT EXISTS idx_aws_provisioning_status ON mailing_aws_provisioning_log(status);
CREATE INDEX IF NOT EXISTS idx_aws_provisioning_created ON mailing_aws_provisioning_log(started_at DESC);

COMMENT ON TABLE mailing_aws_provisioning_log IS 'Log of AWS infrastructure provisioning operations';

-- ============================================
-- CLOUDFRONT DISTRIBUTIONS TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_cloudfront_distributions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    distribution_type VARCHAR(50) NOT NULL, -- tracking, image_cdn
    domain VARCHAR(255) NOT NULL,
    cloudfront_id VARCHAR(100) NOT NULL UNIQUE,
    cloudfront_domain VARCHAR(255) NOT NULL,
    origin_domain VARCHAR(255) NOT NULL,
    acm_cert_arn TEXT,
    status VARCHAR(50) DEFAULT 'Deployed', -- InProgress, Deployed
    enabled BOOLEAN DEFAULT true,
    price_class VARCHAR(50) DEFAULT 'PriceClass_100',
    config JSONB DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_cloudfront_org ON mailing_cloudfront_distributions(org_id);
CREATE INDEX IF NOT EXISTS idx_cloudfront_type ON mailing_cloudfront_distributions(distribution_type);
CREATE INDEX IF NOT EXISTS idx_cloudfront_domain ON mailing_cloudfront_distributions(domain);

COMMENT ON TABLE mailing_cloudfront_distributions IS 'CloudFront distributions for tracking and image CDN';

-- ============================================
-- ACM CERTIFICATES TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_acm_certificates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    domain VARCHAR(255) NOT NULL,
    certificate_arn TEXT NOT NULL UNIQUE,
    status VARCHAR(50) DEFAULT 'PENDING_VALIDATION', -- PENDING_VALIDATION, ISSUED, INACTIVE, EXPIRED, VALIDATION_TIMED_OUT, REVOKED, FAILED
    validation_method VARCHAR(50) DEFAULT 'DNS', -- DNS, EMAIL
    validation_records JSONB DEFAULT '[]',
    issued_at TIMESTAMP WITH TIME ZONE,
    expires_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_acm_certs_org ON mailing_acm_certificates(org_id);
CREATE INDEX IF NOT EXISTS idx_acm_certs_domain ON mailing_acm_certificates(domain);
CREATE INDEX IF NOT EXISTS idx_acm_certs_status ON mailing_acm_certificates(status);

COMMENT ON TABLE mailing_acm_certificates IS 'ACM SSL certificates for custom domains';

-- ============================================
-- S3 BUCKETS TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_s3_buckets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    bucket_name VARCHAR(255) NOT NULL UNIQUE,
    bucket_type VARCHAR(50) NOT NULL, -- image_cdn, email_templates, exports
    region VARCHAR(50) DEFAULT 'us-east-1',
    public_access_blocked BOOLEAN DEFAULT true,
    versioning_enabled BOOLEAN DEFAULT false,
    lifecycle_rules JSONB DEFAULT '[]',
    cors_config JSONB DEFAULT '[]',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_s3_buckets_org ON mailing_s3_buckets(org_id);
CREATE INDEX IF NOT EXISTS idx_s3_buckets_type ON mailing_s3_buckets(bucket_type);

COMMENT ON TABLE mailing_s3_buckets IS 'S3 buckets managed by the platform';

-- ============================================
-- ROUTE53 RECORDS TABLE
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_route53_records (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    hosted_zone_id VARCHAR(100),
    record_name VARCHAR(255) NOT NULL,
    record_type VARCHAR(10) NOT NULL, -- A, AAAA, CNAME, TXT, MX
    record_value TEXT NOT NULL,
    ttl INT DEFAULT 300,
    change_id VARCHAR(100),
    status VARCHAR(50) DEFAULT 'PENDING', -- PENDING, INSYNC
    resource_type VARCHAR(50), -- tracking_domain, image_domain, acm_validation
    resource_id UUID,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_route53_org ON mailing_route53_records(org_id);
CREATE INDEX IF NOT EXISTS idx_route53_zone ON mailing_route53_records(hosted_zone_id);
CREATE INDEX IF NOT EXISTS idx_route53_resource ON mailing_route53_records(resource_type, resource_id);

COMMENT ON TABLE mailing_route53_records IS 'Route53 DNS records managed by the platform';

-- ============================================
-- TRIGGERS FOR UPDATED_AT
-- ============================================

-- Provisioning log doesn't need updated_at trigger

CREATE OR REPLACE FUNCTION update_aws_resource_timestamp()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_cloudfront_updated ON mailing_cloudfront_distributions;
CREATE TRIGGER trg_cloudfront_updated
    BEFORE UPDATE ON mailing_cloudfront_distributions
    FOR EACH ROW
    EXECUTE FUNCTION update_aws_resource_timestamp();

DROP TRIGGER IF EXISTS trg_acm_updated ON mailing_acm_certificates;
CREATE TRIGGER trg_acm_updated
    BEFORE UPDATE ON mailing_acm_certificates
    FOR EACH ROW
    EXECUTE FUNCTION update_aws_resource_timestamp();

DROP TRIGGER IF EXISTS trg_s3_buckets_updated ON mailing_s3_buckets;
CREATE TRIGGER trg_s3_buckets_updated
    BEFORE UPDATE ON mailing_s3_buckets
    FOR EACH ROW
    EXECUTE FUNCTION update_aws_resource_timestamp();

DROP TRIGGER IF EXISTS trg_route53_updated ON mailing_route53_records;
CREATE TRIGGER trg_route53_updated
    BEFORE UPDATE ON mailing_route53_records
    FOR EACH ROW
    EXECUTE FUNCTION update_aws_resource_timestamp();

COMMIT;
