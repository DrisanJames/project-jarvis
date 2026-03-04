-- Support DISTINCT ON (LOWER(email)) for multi-list deduplication
CREATE INDEX IF NOT EXISTS idx_subscribers_email_lower
  ON mailing_subscribers (LOWER(email), list_id);

-- Prevent duplicate emails in the same campaign queue
CREATE UNIQUE INDEX IF NOT EXISTS idx_campaign_queue_email_dedup
  ON mailing_campaign_queue (campaign_id, subscriber_id);
