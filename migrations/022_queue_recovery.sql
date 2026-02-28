-- Migration 022: Queue Recovery & Dead Letter Support
-- Adds dead_letter status to queue tables, recovery indexes, and ensures
-- claimed_at column exists for stuck-item detection.
--
-- Context: If a send worker crashes mid-processing, queue items remain stuck
-- in 'claimed'/'sending' status forever. This migration adds the schema
-- support needed by the QueueRecoveryWorker.

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 1: Ensure required columns exist on mailing_campaign_queue
-- ═══════════════════════════════════════════════════════════════════════════

-- claimed_at is used by CampaignProcessor to timestamp when a worker claims an item
ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;

-- attempts column used by SendWorkerPool v1 (may already exist via app code)
ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS attempts INTEGER DEFAULT 0;

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 2: Update status CHECK constraints to include 'claimed' and 'dead_letter'
-- ═══════════════════════════════════════════════════════════════════════════

-- Drop the old constraint (name from 001_mailing_schema.sql; safe if already dropped)
ALTER TABLE mailing_campaign_queue DROP CONSTRAINT IF EXISTS mailing_campaign_queue_status_check;

-- Add updated constraint with all valid statuses
ALTER TABLE mailing_campaign_queue ADD CONSTRAINT mailing_campaign_queue_status_check
    CHECK (status IN ('queued', 'claimed', 'sending', 'sent', 'failed', 'skipped', 'dead_letter'));

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 3: Recovery indexes for mailing_campaign_queue
-- ═══════════════════════════════════════════════════════════════════════════

-- Index for the recovery worker to quickly find stuck claimed/sending items
CREATE INDEX IF NOT EXISTS idx_queue_stuck_recovery
    ON mailing_campaign_queue(status, claimed_at)
    WHERE status IN ('claimed', 'sending');

-- Index for querying dead-lettered items (monitoring / manual review)
CREATE INDEX IF NOT EXISTS idx_queue_dead_letter
    ON mailing_campaign_queue(status, updated_at)
    WHERE status = 'dead_letter';

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 4: Same changes for mailing_campaign_queue_v2 (if it exists)
-- ═══════════════════════════════════════════════════════════════════════════
-- The v2 queue table is used by SendWorkerPoolV2. These statements are
-- wrapped in a DO block so they silently skip if the table doesn't exist.

DO $$
BEGIN
    -- Ensure claimed_at column exists
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'mailing_campaign_queue_v2') THEN
        -- Add claimed_at if missing
        ALTER TABLE mailing_campaign_queue_v2 ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;

        -- Drop old constraint if present
        ALTER TABLE mailing_campaign_queue_v2 DROP CONSTRAINT IF EXISTS mailing_campaign_queue_v2_status_check;

        -- Add updated constraint
        ALTER TABLE mailing_campaign_queue_v2 ADD CONSTRAINT mailing_campaign_queue_v2_status_check
            CHECK (status IN ('queued', 'claimed', 'sending', 'sent', 'failed', 'skipped', 'dead_letter'));

        -- Recovery index
        CREATE INDEX IF NOT EXISTS idx_queue_v2_stuck_recovery
            ON mailing_campaign_queue_v2(status, claimed_at)
            WHERE status IN ('claimed', 'sending');

        -- Dead letter index
        CREATE INDEX IF NOT EXISTS idx_queue_v2_dead_letter
            ON mailing_campaign_queue_v2(status)
            WHERE status = 'dead_letter';

        RAISE NOTICE 'mailing_campaign_queue_v2: recovery schema applied';
    ELSE
        RAISE NOTICE 'mailing_campaign_queue_v2 does not exist, skipping';
    END IF;
END $$;
