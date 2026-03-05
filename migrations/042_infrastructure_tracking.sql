-- Infrastructure tracking: denormalize sending_domain and sending_ip onto tracking events
-- for high-performance analytics without heavy joins.

ALTER TABLE mailing_tracking_events
ADD COLUMN IF NOT EXISTS sending_domain VARCHAR(255),
ADD COLUMN IF NOT EXISTS sending_ip VARCHAR(45);

CREATE INDEX IF NOT EXISTS idx_tracking_sending_domain ON mailing_tracking_events(sending_domain);
CREATE INDEX IF NOT EXISTS idx_tracking_sending_ip ON mailing_tracking_events(sending_ip);
