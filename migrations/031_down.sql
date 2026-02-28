BEGIN;

DROP TABLE IF EXISTS data_import_log;
DROP TABLE IF EXISTS subscriber_events CASCADE;
DROP FUNCTION IF EXISTS create_subscriber_events_partition();
ALTER TABLE mailing_subscribers DROP COLUMN IF EXISTS data_quality_score;
ALTER TABLE mailing_subscribers DROP COLUMN IF EXISTS data_source;
ALTER TABLE mailing_subscribers DROP COLUMN IF EXISTS verification_status;
ALTER TABLE mailing_subscribers DROP COLUMN IF EXISTS verified_at;

COMMIT;
