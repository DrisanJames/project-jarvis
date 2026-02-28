BEGIN;
DROP TABLE IF EXISTS content_learnings;
ALTER TABLE mailing_send_queue DROP COLUMN IF EXISTS variant_id;
COMMIT;
