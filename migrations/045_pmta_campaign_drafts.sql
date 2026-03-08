-- Migration 045: PMTA campaign draft snapshots
-- Stores the PMTA wizard payload directly on the parent campaign row so drafts
-- can be resumed and later promoted to a scheduled PMTA campaign in place.

ALTER TABLE mailing_campaigns
    ADD COLUMN IF NOT EXISTS pmta_config JSONB DEFAULT '{}'::jsonb;

CREATE INDEX IF NOT EXISTS idx_mailing_campaigns_pmta_drafts
    ON mailing_campaigns (organization_id, updated_at DESC)
    WHERE status = 'draft' AND execution_mode = 'pmta_isp_wave';
