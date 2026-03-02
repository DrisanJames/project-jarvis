-- Migration 039: Campaign Pipeline Fix
-- Resolves cascading failures that orphan campaigns in 'sending' status:
--   1. Status CHECK constraint blocks 'completed'/'failed'/'preparing' transitions
--   2. Missing mailing_suppressions table causes scheduler SQL errors
--   3. Missing columns on campaign/queue tables

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 1: Fix status CHECK constraint on mailing_campaigns
-- ═══════════════════════════════════════════════════════════════════════════

ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check;
ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_campaign_type_check;

ALTER TABLE mailing_campaigns ADD CONSTRAINT mailing_campaigns_status_check
  CHECK (status IN ('draft','scheduled','preparing','sending','paused','completed',
                    'completed_with_errors','cancelled','failed','deleted','sent'));

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 2: Missing columns on mailing_campaigns
-- ═══════════════════════════════════════════════════════════════════════════

ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS queued_count INTEGER DEFAULT 0;
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS list_ids JSONB DEFAULT '[]';
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS suppression_list_ids JSONB DEFAULT '[]';
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS suppression_segment_ids JSONB DEFAULT '[]';

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 3: Missing columns on mailing_campaign_queue (send_worker v1)
-- ═══════════════════════════════════════════════════════════════════════════

ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS locked_at TIMESTAMPTZ;
ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS worker_id VARCHAR(100);

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 4: Ensure mailing_suppressions table exists
-- ═══════════════════════════════════════════════════════════════════════════

CREATE TABLE IF NOT EXISTS mailing_suppressions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email VARCHAR(255) NOT NULL,
    reason TEXT,
    source VARCHAR(50),
    active BOOLEAN DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    CONSTRAINT mailing_suppressions_email_key UNIQUE (email)
);

CREATE INDEX IF NOT EXISTS idx_suppressions_active_email
    ON mailing_suppressions(email) WHERE active = true;

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 5: Reset orphaned campaigns stuck in 'sending' with no queue items
-- ═══════════════════════════════════════════════════════════════════════════

UPDATE mailing_campaigns
SET status = 'failed', completed_at = NOW(), updated_at = NOW()
WHERE status = 'sending'
  AND NOT EXISTS (
      SELECT 1 FROM mailing_campaign_queue q
      WHERE q.campaign_id = mailing_campaigns.id
        AND q.status IN ('queued', 'sending', 'claimed')
  );
