-- Migration 028: Global Suppression Hub — Single Source of Truth
-- All negative signals (bounces, complaints, unsubscribes, FBL, inactive, manual)
-- converge into this table. Every pre-send check queries this.

CREATE TABLE IF NOT EXISTS mailing_global_suppressions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    email           TEXT NOT NULL,
    md5_hash        VARCHAR(32) NOT NULL,
    reason          VARCHAR(100) NOT NULL,
    source          VARCHAR(100) NOT NULL,
    isp             VARCHAR(50),
    dsn_code        VARCHAR(20),
    dsn_diag        TEXT,
    source_ip       VARCHAR(45),
    campaign_id     VARCHAR(255),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW(),

    CONSTRAINT uq_global_supp_org_hash UNIQUE (organization_id, md5_hash)
);

-- Primary lookup: MD5 hash check (O(1) with hash index)
CREATE INDEX IF NOT EXISTS idx_global_supp_md5 ON mailing_global_suppressions USING hash (md5_hash);

-- Email lookup (for human-readable search)
CREATE INDEX IF NOT EXISTS idx_global_supp_email ON mailing_global_suppressions (email);

-- Org-scoped queries
CREATE INDEX IF NOT EXISTS idx_global_supp_org ON mailing_global_suppressions (organization_id);

-- Analytics: by reason, source, ISP
CREATE INDEX IF NOT EXISTS idx_global_supp_reason ON mailing_global_suppressions (reason);
CREATE INDEX IF NOT EXISTS idx_global_supp_source ON mailing_global_suppressions (source);
CREATE INDEX IF NOT EXISTS idx_global_supp_isp ON mailing_global_suppressions (isp);

-- Time-based queries (recent additions, velocity)
CREATE INDEX IF NOT EXISTS idx_global_supp_created ON mailing_global_suppressions (created_at DESC);

-- Composite for common dashboard queries
CREATE INDEX IF NOT EXISTS idx_global_supp_org_created ON mailing_global_suppressions (organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_global_supp_org_reason ON mailing_global_suppressions (organization_id, reason);
