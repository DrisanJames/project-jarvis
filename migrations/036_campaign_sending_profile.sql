-- Migration 036: Add sending_profile_id and send controls to mailing_campaigns

ALTER TABLE mailing_campaigns
    ADD COLUMN IF NOT EXISTS sending_profile_id UUID REFERENCES mailing_sending_profiles(id) ON DELETE SET NULL;

ALTER TABLE mailing_campaigns
    ADD COLUMN IF NOT EXISTS send_type VARCHAR(20) DEFAULT 'blast'
    CHECK (send_type IN ('blast', 'drip', 'test'));

ALTER TABLE mailing_campaigns
    ADD COLUMN IF NOT EXISTS max_recipients INTEGER DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_campaigns_sending_profile
    ON mailing_campaigns(sending_profile_id);
