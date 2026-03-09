-- 046: MPP (Machine Privacy Protection) Open Tracking
-- Adds a boolean flag to distinguish machine opens (within 30s of delivery)
-- from human opens. Supports Apple MPP and bot detection by timing heuristic.

ALTER TABLE mailing_tracking_events
  ADD COLUMN IF NOT EXISTS is_machine_open BOOLEAN DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_mte_machine_open
  ON mailing_tracking_events (campaign_id, is_machine_open)
  WHERE event_type = 'opened' AND is_machine_open = TRUE;
