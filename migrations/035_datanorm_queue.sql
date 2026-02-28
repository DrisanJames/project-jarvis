-- Migration 035: Data Normalizer Queue System
-- Adds columns for proper file queuing with progress tracking.

ALTER TABLE data_import_log ADD COLUMN IF NOT EXISTS file_size BIGINT DEFAULT 0;
ALTER TABLE data_import_log ADD COLUMN IF NOT EXISTS retry_count INTEGER DEFAULT 0;
ALTER TABLE data_import_log ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ;

ALTER TABLE data_import_log DROP CONSTRAINT IF EXISTS data_import_log_status_check;
ALTER TABLE data_import_log ADD CONSTRAINT data_import_log_status_check
  CHECK (status IN ('pending','processing','completed','failed','skipped'));
