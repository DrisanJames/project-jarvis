-- Migration: 007_tracking_domains.sql
-- Description: Add custom tracking domain support
-- Date: 2026-02-04

-- ============================================
-- TRACKING DOMAINS
-- ============================================

-- Custom tracking domains table for organizations
CREATE TABLE IF NOT EXISTS mailing_tracking_domains (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    domain VARCHAR(255) NOT NULL UNIQUE,
    verified BOOLEAN DEFAULT false,
    ssl_provisioned BOOLEAN DEFAULT false,
    dns_records JSONB DEFAULT '[]',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Index for fast lookup by organization
CREATE INDEX idx_tracking_domains_org ON mailing_tracking_domains(org_id);

-- Index for finding verified domains (used when generating tracking URLs)
CREATE INDEX idx_tracking_domains_verified ON mailing_tracking_domains(org_id, verified) WHERE verified = true;

-- Index for domain uniqueness lookups
CREATE INDEX idx_tracking_domains_domain ON mailing_tracking_domains(domain);

-- ============================================
-- DOMAIN VERIFICATION LOG
-- ============================================

-- Track verification attempts for debugging and audit
CREATE TABLE IF NOT EXISTS mailing_tracking_domain_verifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tracking_domain_id UUID NOT NULL REFERENCES mailing_tracking_domains(id) ON DELETE CASCADE,
    record_type VARCHAR(10) NOT NULL, -- CNAME, TXT
    record_name VARCHAR(255) NOT NULL,
    expected_value TEXT NOT NULL,
    found_value TEXT,
    verified BOOLEAN DEFAULT false,
    verified_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_domain_verifications_domain ON mailing_tracking_domain_verifications(tracking_domain_id);

-- ============================================
-- SSL PROVISIONING LOG
-- ============================================

-- Track SSL certificate provisioning status
CREATE TABLE IF NOT EXISTS mailing_tracking_domain_ssl_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tracking_domain_id UUID NOT NULL REFERENCES mailing_tracking_domains(id) ON DELETE CASCADE,
    status VARCHAR(50) NOT NULL, -- pending, provisioning, completed, failed
    provider VARCHAR(50), -- letsencrypt, cloudflare, custom
    certificate_expires_at TIMESTAMP WITH TIME ZONE,
    error_message TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_ssl_logs_domain ON mailing_tracking_domain_ssl_logs(tracking_domain_id);
CREATE INDEX idx_ssl_logs_expires ON mailing_tracking_domain_ssl_logs(certificate_expires_at);

-- ============================================
-- HELPER FUNCTIONS
-- ============================================

-- Function to update the updated_at timestamp
CREATE OR REPLACE FUNCTION update_tracking_domain_timestamp()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Trigger to auto-update timestamp on tracking domains
DROP TRIGGER IF EXISTS trigger_tracking_domain_updated_at ON mailing_tracking_domains;
CREATE TRIGGER trigger_tracking_domain_updated_at
    BEFORE UPDATE ON mailing_tracking_domains
    FOR EACH ROW
    EXECUTE FUNCTION update_tracking_domain_timestamp();

-- ============================================
-- AUDIT INTEGRATION
-- ============================================

-- Function to log tracking domain changes to audit log
CREATE OR REPLACE FUNCTION audit_tracking_domain_changes()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO mailing_audit_log (organization_id, action, entity_type, entity_id, new_values)
        VALUES (
            NEW.org_id,
            'tracking_domain_created',
            'tracking_domain',
            NEW.id,
            jsonb_build_object(
                'domain', NEW.domain,
                'verified', NEW.verified,
                'ssl_provisioned', NEW.ssl_provisioned
            )
        );
    ELSIF TG_OP = 'UPDATE' THEN
        INSERT INTO mailing_audit_log (organization_id, action, entity_type, entity_id, old_values, new_values)
        VALUES (
            NEW.org_id,
            CASE 
                WHEN OLD.verified = false AND NEW.verified = true THEN 'tracking_domain_verified'
                WHEN OLD.ssl_provisioned = false AND NEW.ssl_provisioned = true THEN 'tracking_domain_ssl_provisioned'
                ELSE 'tracking_domain_updated'
            END,
            'tracking_domain',
            NEW.id,
            jsonb_build_object(
                'domain', OLD.domain,
                'verified', OLD.verified,
                'ssl_provisioned', OLD.ssl_provisioned
            ),
            jsonb_build_object(
                'domain', NEW.domain,
                'verified', NEW.verified,
                'ssl_provisioned', NEW.ssl_provisioned
            )
        );
    ELSIF TG_OP = 'DELETE' THEN
        INSERT INTO mailing_audit_log (organization_id, action, entity_type, entity_id, old_values)
        VALUES (
            OLD.org_id,
            'tracking_domain_deleted',
            'tracking_domain',
            OLD.id,
            jsonb_build_object(
                'domain', OLD.domain,
                'verified', OLD.verified,
                'ssl_provisioned', OLD.ssl_provisioned
            )
        );
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Trigger for audit logging
DROP TRIGGER IF EXISTS trigger_audit_tracking_domain ON mailing_tracking_domains;
CREATE TRIGGER trigger_audit_tracking_domain
    AFTER INSERT OR UPDATE OR DELETE ON mailing_tracking_domains
    FOR EACH ROW
    EXECUTE FUNCTION audit_tracking_domain_changes();

-- ============================================
-- COMMENTS
-- ============================================

COMMENT ON TABLE mailing_tracking_domains IS 'Custom tracking domains for email open/click tracking';
COMMENT ON COLUMN mailing_tracking_domains.domain IS 'Fully qualified domain name, e.g., track.example.com';
COMMENT ON COLUMN mailing_tracking_domains.dns_records IS 'JSON array of required DNS records with verification status';
COMMENT ON COLUMN mailing_tracking_domains.ssl_provisioned IS 'Whether SSL certificate has been provisioned for HTTPS';

COMMIT;
