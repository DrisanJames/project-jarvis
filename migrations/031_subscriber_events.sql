BEGIN;

-- 1. Partitioned event fact table (star schema fact)
CREATE TABLE IF NOT EXISTS subscriber_events (
    id              BIGSERIAL,
    email_hash      VARCHAR(64) NOT NULL,
    event_type      VARCHAR(30) NOT NULL,
    campaign_id     UUID,
    variant_id      UUID,
    source          VARCHAR(30) NOT NULL DEFAULT 'system',
    metadata        JSONB DEFAULT '{}',
    event_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, event_at)
) PARTITION BY RANGE (event_at);

-- Monthly partitions (March-August 2026)
CREATE TABLE subscriber_events_2026_03 PARTITION OF subscriber_events
    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
CREATE TABLE subscriber_events_2026_04 PARTITION OF subscriber_events
    FOR VALUES FROM ('2026-04-01') TO ('2026-05-01');
CREATE TABLE subscriber_events_2026_05 PARTITION OF subscriber_events
    FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
CREATE TABLE subscriber_events_2026_06 PARTITION OF subscriber_events
    FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');
CREATE TABLE subscriber_events_2026_07 PARTITION OF subscriber_events
    FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');
CREATE TABLE subscriber_events_2026_08 PARTITION OF subscriber_events
    FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');

-- Indexes on parent (auto-applied to partitions)
CREATE INDEX idx_subevt_hash_time ON subscriber_events (email_hash, event_at DESC);
CREATE INDEX idx_subevt_campaign ON subscriber_events (campaign_id, event_at DESC) WHERE campaign_id IS NOT NULL;
CREATE INDEX idx_subevt_type ON subscriber_events (event_type, event_at DESC);
CREATE INDEX idx_subevt_variant ON subscriber_events (variant_id) WHERE variant_id IS NOT NULL;

-- H2: Auto-partition function — creates partitions 2 months ahead.
-- Called by internal/worker/partition_manager.go daily.
CREATE OR REPLACE FUNCTION create_subscriber_events_partition() RETURNS void AS $$
DECLARE
    next_month DATE := date_trunc('month', NOW()) + INTERVAL '2 months';
    partition_name TEXT;
    start_date TEXT;
    end_date TEXT;
BEGIN
    partition_name := 'subscriber_events_' || to_char(next_month, 'YYYY_MM');
    start_date := to_char(next_month, 'YYYY-MM-DD');
    end_date := to_char(next_month + INTERVAL '1 month', 'YYYY-MM-DD');
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF subscriber_events FOR VALUES FROM (%L) TO (%L)',
        partition_name, start_date, end_date
    );
END;
$$ LANGUAGE plpgsql;

-- 2. Data quality score + source on existing subscribers table
ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS data_quality_score DECIMAL(3,2) DEFAULT 0.00;
ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS data_source VARCHAR(50);
ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS verification_status VARCHAR(20);
ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_sub_quality ON mailing_subscribers (data_quality_score) WHERE data_quality_score < 1.00;
CREATE INDEX IF NOT EXISTS idx_sub_source ON mailing_subscribers (data_source) WHERE data_source IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sub_verification ON mailing_subscribers (verification_status) WHERE verification_status IS NOT NULL;

-- 3. Import tracking log
CREATE TABLE IF NOT EXISTS data_import_log (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    original_key    TEXT NOT NULL,
    renamed_key     TEXT NOT NULL DEFAULT '',
    classification  VARCHAR(20) NOT NULL CHECK (classification IN ('mailable', 'suppression', 'warmup')),
    record_count    INTEGER DEFAULT 0,
    error_count     INTEGER DEFAULT 0,
    status          VARCHAR(20) DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    error_message   TEXT,
    original_exists BOOLEAN DEFAULT TRUE,
    processed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(original_key)
);

COMMIT;
