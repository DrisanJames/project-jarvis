-- 027: Consciousness + Campaign Event Tracking
-- Adds tables for the AI consciousness layer and campaign-level event aggregation

-- Campaign event snapshots persisted to DB (S3 is primary, this is a queryable mirror)
CREATE TABLE IF NOT EXISTS mailing_campaign_event_snapshots (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    campaign_id     VARCHAR(255) NOT NULL,
    snapshot_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    sent            INTEGER NOT NULL DEFAULT 0,
    delivered       INTEGER NOT NULL DEFAULT 0,
    soft_bounce     INTEGER NOT NULL DEFAULT 0,
    hard_bounce     INTEGER NOT NULL DEFAULT 0,
    complaints      INTEGER NOT NULL DEFAULT 0,
    unsubscribes    INTEGER NOT NULL DEFAULT 0,
    opens           INTEGER NOT NULL DEFAULT 0,
    unique_opens    INTEGER NOT NULL DEFAULT 0,
    clicks          INTEGER NOT NULL DEFAULT 0,
    unique_clicks   INTEGER NOT NULL DEFAULT 0,
    inactive        INTEGER NOT NULL DEFAULT 0,

    delivery_rate   DECIMAL(5,2) DEFAULT 0,
    open_rate       DECIMAL(5,2) DEFAULT 0,
    click_rate      DECIMAL(5,2) DEFAULT 0,
    bounce_rate     DECIMAL(5,2) DEFAULT 0,
    complaint_rate  DECIMAL(5,4) DEFAULT 0,

    isp_breakdown   JSONB DEFAULT '{}',
    variant_breakdown JSONB DEFAULT '{}',

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_campaign_event_snapshots_campaign ON mailing_campaign_event_snapshots(campaign_id, snapshot_at DESC);
CREATE INDEX IF NOT EXISTS idx_campaign_event_snapshots_org ON mailing_campaign_event_snapshots(organization_id, snapshot_at DESC);

-- Inactive email tracking (no engagement after N sends)
CREATE TABLE IF NOT EXISTS mailing_inactive_emails (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    email           VARCHAR(320) NOT NULL,
    campaign_id     VARCHAR(255),
    send_count      INTEGER NOT NULL DEFAULT 0,
    last_sent_at    TIMESTAMPTZ,
    detected_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    suppressed      BOOLEAN NOT NULL DEFAULT FALSE,
    suppressed_at   TIMESTAMPTZ,

    UNIQUE(organization_id, email)
);
CREATE INDEX IF NOT EXISTS idx_inactive_emails_org ON mailing_inactive_emails(organization_id, detected_at DESC);

-- Consciousness philosophies persisted to DB
CREATE TABLE IF NOT EXISTS mailing_consciousness_philosophies (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    isp             VARCHAR(50) NOT NULL,
    domain          VARCHAR(100) NOT NULL,
    belief          TEXT NOT NULL,
    explanation     TEXT,
    confidence      DECIMAL(3,2) NOT NULL DEFAULT 0,
    evidence_count  INTEGER NOT NULL DEFAULT 0,
    category        VARCHAR(100),
    sentiment       VARCHAR(50),
    strength        DECIMAL(3,2) NOT NULL DEFAULT 0,
    challenges      INTEGER NOT NULL DEFAULT 0,
    tags            JSONB DEFAULT '[]',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE(organization_id, isp, domain)
);
CREATE INDEX IF NOT EXISTS idx_philosophies_org ON mailing_consciousness_philosophies(organization_id, isp);

-- Consciousness thought log
CREATE TABLE IF NOT EXISTS mailing_consciousness_thoughts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    isp             VARCHAR(50),
    agent_type      VARCHAR(50),
    thought_type    VARCHAR(50) NOT NULL,
    content         TEXT NOT NULL,
    reasoning       TEXT,
    confidence      DECIMAL(3,2),
    severity        VARCHAR(20),
    related_ids     JSONB DEFAULT '[]',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_thoughts_org ON mailing_consciousness_thoughts(organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_thoughts_isp ON mailing_consciousness_thoughts(isp, created_at DESC);
