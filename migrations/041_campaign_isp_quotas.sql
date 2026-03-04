-- ISP volume quotas with optional randomization flag
ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS isp_quotas JSONB DEFAULT '{}';
