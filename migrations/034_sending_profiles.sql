-- Migration 034: Create mailing_sending_profiles table
-- This table was referenced by code and seed data (030) but never created.

CREATE TABLE IF NOT EXISTS mailing_sending_profiles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    name TEXT NOT NULL,
    description TEXT,
    vendor_type TEXT NOT NULL DEFAULT 'ses',

    from_name TEXT NOT NULL DEFAULT '',
    from_email TEXT NOT NULL DEFAULT '',
    reply_email TEXT,

    api_key TEXT,
    api_secret TEXT,
    api_endpoint TEXT,

    smtp_host TEXT,
    smtp_port INTEGER NOT NULL DEFAULT 587,
    smtp_username TEXT,
    smtp_password TEXT,
    smtp_encryption TEXT NOT NULL DEFAULT 'tls',

    sending_domain TEXT,
    bounce_domain TEXT,
    tracking_domain TEXT,

    spf_verified BOOLEAN NOT NULL DEFAULT FALSE,
    dkim_verified BOOLEAN NOT NULL DEFAULT FALSE,
    dmarc_verified BOOLEAN NOT NULL DEFAULT FALSE,
    domain_verified BOOLEAN NOT NULL DEFAULT FALSE,
    credentials_verified BOOLEAN NOT NULL DEFAULT FALSE,
    last_verification_at TIMESTAMPTZ,
    verification_error TEXT,

    hourly_limit INTEGER NOT NULL DEFAULT 10000,
    daily_limit INTEGER NOT NULL DEFAULT 100000,
    current_hourly_count INTEGER NOT NULL DEFAULT 0,
    current_daily_count INTEGER NOT NULL DEFAULT 0,

    ip_pool TEXT,

    status TEXT NOT NULL DEFAULT 'draft',
    is_default BOOLEAN NOT NULL DEFAULT FALSE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by UUID,

    CONSTRAINT fk_sending_profiles_org FOREIGN KEY (organization_id)
        REFERENCES organizations(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_sending_profiles_org ON mailing_sending_profiles(organization_id);
CREATE INDEX IF NOT EXISTS idx_sending_profiles_status ON mailing_sending_profiles(status);
CREATE INDEX IF NOT EXISTS idx_sending_profiles_vendor ON mailing_sending_profiles(vendor_type);
CREATE INDEX IF NOT EXISTS idx_sending_profiles_default ON mailing_sending_profiles(organization_id, is_default) WHERE is_default = TRUE;

-- Also create the list-level defaults table referenced in sending_profiles_handlers.go
CREATE TABLE IF NOT EXISTS mailing_sending_profile_list_defaults (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id UUID NOT NULL REFERENCES mailing_sending_profiles(id) ON DELETE CASCADE,
    list_id UUID NOT NULL,
    from_name_override TEXT,
    from_email_override TEXT,
    reply_email_override TEXT,
    is_default BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(profile_id, list_id)
);

-- Re-run the seed from migration 030 now that the table exists
DO $$
DECLARE
    org_id UUID := '00000000-0000-0000-0000-000000000001';
    profile_id UUID;
BEGIN
    INSERT INTO mailing_sending_profiles (organization_id, name, description, vendor_type,
        from_name, from_email, reply_email,
        smtp_host, smtp_port, smtp_username, smtp_password,
        sending_domain, tracking_domain, ip_pool,
        hourly_limit, daily_limit, status)
    VALUES (org_id, 'PMTA Warmup', 'ProjectJarvis.io via OVH PMTA (warmup phase)', 'pmta',
        'Ignite', 'hello@projectjarvis.io', 'reply@projectjarvis.io',
        '15.204.101.125', 25, 'ignite', 'xK9#mPtA2026!ovh',
        'projectjarvis.io', 'projectjarvis.io', 'warmup-pool',
        3200, 25600, 'active')
    ON CONFLICT DO NOTHING
    RETURNING id INTO profile_id;

    RAISE NOTICE 'Seeded sending profile: %', COALESCE(profile_id::text, 'already exists');
END $$;
