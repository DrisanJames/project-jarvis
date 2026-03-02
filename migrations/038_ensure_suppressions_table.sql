-- Migration 038: Ensure mailing_suppressions table exists
-- This legacy table is referenced across multiple handlers and must exist.
-- The global_suppressions table (028) is the primary source of truth,
-- but this table is checked at send-time in multiple paths.

CREATE TABLE IF NOT EXISTS mailing_suppressions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT NOT NULL,
    reason      VARCHAR(200) NOT NULL DEFAULT 'unsubscribe',
    source      VARCHAR(100) NOT NULL DEFAULT 'manual',
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW(),
    CONSTRAINT uq_suppressions_email UNIQUE (email)
);

CREATE INDEX IF NOT EXISTS idx_suppressions_active_email
ON mailing_suppressions(LOWER(email))
WHERE active = true;

-- Ensure mailing_ab_variants has status column (used by campaign_scheduler)
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'mailing_ab_variants' AND column_name = 'status'
    ) THEN
        ALTER TABLE mailing_ab_variants ADD COLUMN status VARCHAR(20) DEFAULT 'active';
    END IF;
END$$;
