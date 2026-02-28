BEGIN;

CREATE TABLE IF NOT EXISTS content_learnings (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    campaign_id     UUID NOT NULL,
    ab_test_id      UUID REFERENCES mailing_ab_tests(id) ON DELETE SET NULL,
    variant_id      UUID NOT NULL,
    subject_style   VARCHAR(50),
    layout_style    VARCHAR(50),
    cta_style       VARCHAR(50),
    tone            VARCHAR(50),
    deal_count      INTEGER,
    sample_size     INTEGER NOT NULL DEFAULT 0,
    open_rate       DECIMAL(5,4) DEFAULT 0,
    click_rate      DECIMAL(5,4) DEFAULT 0,
    site_dwell_avg_ms INTEGER DEFAULT 0,
    conversion_rate DECIMAL(5,4) DEFAULT 0,
    is_winner       BOOLEAN DEFAULT FALSE,
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_content_learnings_org ON content_learnings (organization_id, created_at DESC);
CREATE INDEX idx_content_learnings_winner ON content_learnings (organization_id, is_winner) WHERE is_winner = TRUE;

ALTER TABLE mailing_send_queue ADD COLUMN IF NOT EXISTS variant_id UUID;

COMMIT;
