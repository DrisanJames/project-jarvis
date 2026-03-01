-- Migration 037: Reconcile mailing_tracking_events schema
-- The code uses columns (email, event_time, device_type, link_url) that may
-- not exist depending on which CREATE TABLE ran first (001 vs 029).
-- This migration ensures ALL columns needed by the tracking handlers exist.

-- Add email column (code writes email TEXT; 001 schema only has email_id UUID)
ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS email TEXT;

-- Add event_time column (code writes event_time; 001 schema only has event_at)
ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS event_time TIMESTAMPTZ DEFAULT NOW();

-- Add organization_id (exists in 001 as NOT NULL, not in 029). Make nullable
-- since the "sent" event INSERT doesn't always include it.
ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS organization_id UUID;
DO $$ BEGIN
  ALTER TABLE mailing_tracking_events ALTER COLUMN organization_id DROP NOT NULL;
EXCEPTION WHEN OTHERS THEN NULL;
END $$;

-- Add device_type (exists in 001, not in 029)
ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS device_type VARCHAR(20);

-- Add link_url (exists in 001 as link_url, 029 as url)
ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS link_url TEXT;

-- Add metadata (exists in 029, not in 001)
ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS metadata JSONB DEFAULT '{}';

-- Add is_unique (exists in 029, not in 001)
ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS is_unique BOOLEAN DEFAULT false;

-- Drop restrictive event_type CHECK constraints.
-- 001 allows: sent, delivered, deferred, bounced, opened, clicked, unsubscribed, complained, rejected
-- 029 allows: open, click, unsubscribe, complaint, bounce
-- We need both forms accepted.
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT con.conname
        FROM pg_constraint con
        JOIN pg_class rel ON rel.oid = con.conrelid
        WHERE rel.relname = 'mailing_tracking_events'
          AND con.contype = 'c'
          AND pg_get_constraintdef(con.oid) ILIKE '%event_type%'
    LOOP
        EXECUTE 'ALTER TABLE mailing_tracking_events DROP CONSTRAINT IF EXISTS ' || quote_ident(r.conname);
    END LOOP;
END $$;

-- Also check partitions for inherited constraints
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT con.conname, rel.relname
        FROM pg_constraint con
        JOIN pg_class rel ON rel.oid = con.conrelid
        WHERE rel.relname LIKE 'mailing_tracking_events_%'
          AND con.contype = 'c'
          AND pg_get_constraintdef(con.oid) ILIKE '%event_type%'
    LOOP
        EXECUTE 'ALTER TABLE ' || quote_ident(r.relname) || ' DROP CONSTRAINT IF EXISTS ' || quote_ident(r.conname);
    END LOOP;
END $$;

-- Seed PMTA delivery server (for the Servers page)
INSERT INTO mailing_delivery_servers (id, organization_id, name, server_type, region, hourly_quota, daily_quota, status, priority)
VALUES (
    'a0000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000001',
    'PMTA Primary (OVH)',
    'pmta',
    'us-east',
    50000,
    500000,
    'active',
    1
) ON CONFLICT (id) DO NOTHING;

-- Create partition for March 2026 (tracking events)
CREATE TABLE IF NOT EXISTS mailing_tracking_events_2026_03 PARTITION OF mailing_tracking_events
    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');

-- Auto-populate mailing_sending_domains from PMTA sending profiles
INSERT INTO mailing_sending_domains (id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
SELECT
    gen_random_uuid(),
    sp.organization_id,
    sp.sending_domain,
    true, true, true,
    'verified',
    NOW(),
    NOW()
FROM mailing_sending_profiles sp
WHERE sp.vendor_type = 'pmta'
  AND sp.sending_domain IS NOT NULL
  AND sp.sending_domain != ''
ON CONFLICT (organization_id, domain) DO NOTHING;

-- Also extract domains from from_email addresses on PMTA profiles
INSERT INTO mailing_sending_domains (id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
SELECT
    gen_random_uuid(),
    sp.organization_id,
    SUBSTRING(sp.from_email FROM POSITION('@' IN sp.from_email) + 1),
    true, true, true,
    'verified',
    NOW(),
    NOW()
FROM mailing_sending_profiles sp
WHERE sp.vendor_type = 'pmta'
  AND sp.from_email LIKE '%@%'
ON CONFLICT (organization_id, domain) DO NOTHING;

-- Add email column to inbox_profiles for direct lookups (existing schema uses email_hash)
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS email TEXT;
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_email_text ON mailing_inbox_profiles(email);
