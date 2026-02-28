-- Migration 029: Affiliate Mail Readiness
-- Adds missing columns and tables required for full campaign sending pipeline.

-- =====================================================================
-- 1. mailing_campaigns: missing columns for scheduler + processor
-- =====================================================================
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS scheduled_at TIMESTAMPTZ;
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS esp_quotas JSONB DEFAULT '[]';
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS throttle_speed VARCHAR(30) DEFAULT 'gentle';
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS max_recipients INTEGER;
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS tracking_domain VARCHAR(255);

CREATE INDEX IF NOT EXISTS idx_campaigns_scheduled
    ON mailing_campaigns (scheduled_at)
    WHERE status IN ('scheduled', 'preparing');

-- =====================================================================
-- 2. mailing_campaign_queue_v2: normalized queue (content not duplicated)
-- =====================================================================
CREATE TABLE IF NOT EXISTS mailing_campaign_queue_v2 (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id       UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    subscriber_id     UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    email             TEXT NOT NULL,
    substitution_data JSONB DEFAULT '{}',
    status            VARCHAR(20) DEFAULT 'queued'
                      CHECK (status IN ('queued','claimed','sending','sent','failed','skipped','dead_letter')),
    priority          INTEGER DEFAULT 5,
    retry_count       INTEGER DEFAULT 0,
    max_retries       INTEGER DEFAULT 3,
    scheduled_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claimed_at        TIMESTAMPTZ,
    sent_at           TIMESTAMPTZ,
    created_at        TIMESTAMPTZ DEFAULT NOW(),
    worker_id         VARCHAR(100),
    batch_id          UUID,
    message_id        VARCHAR(255),
    error_code        VARCHAR(50)
);

CREATE INDEX IF NOT EXISTS idx_queue_v2_claim
    ON mailing_campaign_queue_v2 (status, scheduled_at, priority DESC)
    WHERE status = 'queued';

CREATE INDEX IF NOT EXISTS idx_queue_v2_campaign
    ON mailing_campaign_queue_v2 (campaign_id);

CREATE INDEX IF NOT EXISTS idx_queue_v2_subscriber
    ON mailing_campaign_queue_v2 (subscriber_id);

CREATE INDEX IF NOT EXISTS idx_queue_v2_stuck
    ON mailing_campaign_queue_v2 (status, claimed_at)
    WHERE status = 'sending' AND claimed_at IS NOT NULL;

-- =====================================================================
-- 3. Tracking events table (for open/click/unsubscribe events from PMTA)
-- =====================================================================
CREATE TABLE IF NOT EXISTS mailing_tracking_events (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id   UUID NOT NULL,
    subscriber_id UUID,
    email         TEXT NOT NULL,
    event_type    VARCHAR(30) NOT NULL CHECK (event_type IN ('open','click','unsubscribe','complaint','bounce')),
    metadata      JSONB DEFAULT '{}',
    ip_address    VARCHAR(45),
    user_agent    TEXT,
    url           TEXT,
    is_unique     BOOLEAN DEFAULT false,
    created_at    TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_tracking_campaign ON mailing_tracking_events (campaign_id);
CREATE INDEX IF NOT EXISTS idx_tracking_email ON mailing_tracking_events (email);
CREATE INDEX IF NOT EXISTS idx_tracking_type ON mailing_tracking_events (event_type);
CREATE INDEX IF NOT EXISTS idx_tracking_created ON mailing_tracking_events (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tracking_campaign_type ON mailing_tracking_events (campaign_id, event_type);

-- =====================================================================
-- 4. Sending domains table (multi-domain DKIM rotation)
-- =====================================================================
CREATE TABLE IF NOT EXISTS mailing_domains (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    domain          VARCHAR(255) NOT NULL UNIQUE,
    dkim_selector   VARCHAR(100),
    dkim_key_path   TEXT,
    dkim_public_key TEXT,
    spf_status      VARCHAR(20) DEFAULT 'pending',
    dkim_status     VARCHAR(20) DEFAULT 'pending',
    dmarc_status    VARCHAR(20) DEFAULT 'pending',
    tracking_domain VARCHAR(255),
    is_active       BOOLEAN DEFAULT true,
    warmup_stage    INTEGER DEFAULT 0,
    daily_limit     INTEGER DEFAULT 1000,
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_domains_org ON mailing_domains (organization_id);
CREATE INDEX IF NOT EXISTS idx_domains_active ON mailing_domains (is_active) WHERE is_active = true;

-- =====================================================================
-- 5. Unsubscribe tokens (secure one-click unsubscribe)
-- =====================================================================
CREATE TABLE IF NOT EXISTS mailing_unsubscribe_tokens (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token         VARCHAR(64) NOT NULL UNIQUE,
    campaign_id   UUID NOT NULL,
    subscriber_id UUID,
    email         TEXT NOT NULL,
    list_id       UUID,
    used_at       TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '90 days',
    created_at    TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_unsub_token ON mailing_unsubscribe_tokens (token);
CREATE INDEX IF NOT EXISTS idx_unsub_email ON mailing_unsubscribe_tokens (email);
