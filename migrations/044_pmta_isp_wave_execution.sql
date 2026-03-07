-- Migration 044: PMTA ISP Wave Execution
-- Adds normalized PMTA execution tables for parent campaigns, ISP plans,
-- delivery windows, execution waves, and recipient provenance.

ALTER TABLE mailing_campaigns
    ADD COLUMN IF NOT EXISTS execution_mode TEXT DEFAULT 'standard';

ALTER TABLE mailing_campaigns
    DROP CONSTRAINT IF EXISTS mailing_campaigns_execution_mode_check;

ALTER TABLE mailing_campaigns
    ADD CONSTRAINT mailing_campaigns_execution_mode_check
    CHECK (execution_mode IN ('standard', 'pmta_isp_wave'));

CREATE TABLE IF NOT EXISTS mailing_campaign_isp_plans (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    isp VARCHAR(50) NOT NULL,
    sending_domain VARCHAR(255) NOT NULL,
    sending_profile_id UUID REFERENCES mailing_sending_profiles(id) ON DELETE SET NULL,
    quota INTEGER DEFAULT 0,
    randomize_audience BOOLEAN DEFAULT FALSE,
    throttle_strategy VARCHAR(50) DEFAULT 'auto',
    selection_strategy VARCHAR(50) DEFAULT 'priority_first',
    priority_strategy VARCHAR(50) DEFAULT 'selection_rank',
    timezone VARCHAR(80) DEFAULT 'UTC',
    status VARCHAR(30) DEFAULT 'planned'
        CHECK (status IN ('planned', 'ready', 'running', 'paused', 'completed', 'cancelled', 'failed')),
    audience_estimated_count INTEGER DEFAULT 0,
    audience_selected_count INTEGER DEFAULT 0,
    enqueued_count INTEGER DEFAULT 0,
    sent_count INTEGER DEFAULT 0,
    failed_count INTEGER DEFAULT 0,
    config_snapshot JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_campaign_isp_plans_campaign ON mailing_campaign_isp_plans(campaign_id);
CREATE INDEX IF NOT EXISTS idx_campaign_isp_plans_status ON mailing_campaign_isp_plans(status, isp);

CREATE TABLE IF NOT EXISTS mailing_campaign_isp_time_spans (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    isp_plan_id UUID NOT NULL REFERENCES mailing_campaign_isp_plans(id) ON DELETE CASCADE,
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    span_type VARCHAR(20) DEFAULT 'absolute' CHECK (span_type IN ('absolute', 'weekly')),
    day_of_week VARCHAR(20),
    start_hour INTEGER,
    end_hour INTEGER,
    start_at TIMESTAMPTZ NOT NULL,
    end_at TIMESTAMPTZ NOT NULL,
    timezone VARCHAR(80) DEFAULT 'UTC',
    source VARCHAR(50) DEFAULT 'manual',
    sort_order INTEGER DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_campaign_isp_time_spans_plan ON mailing_campaign_isp_time_spans(isp_plan_id, sort_order);

CREATE TABLE IF NOT EXISTS mailing_campaign_waves (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    isp_plan_id UUID NOT NULL REFERENCES mailing_campaign_isp_plans(id) ON DELETE CASCADE,
    wave_number INTEGER NOT NULL,
    scheduled_at TIMESTAMPTZ NOT NULL,
    window_start_at TIMESTAMPTZ NOT NULL,
    window_end_at TIMESTAMPTZ NOT NULL,
    cadence_minutes INTEGER DEFAULT 0,
    batch_size INTEGER DEFAULT 0,
    planned_recipients INTEGER DEFAULT 0,
    enqueued_recipients INTEGER DEFAULT 0,
    status VARCHAR(30) DEFAULT 'planned'
        CHECK (status IN ('planned', 'due', 'dispatched', 'enqueuing', 'queued', 'sending', 'completed', 'cancelled', 'failed', 'dead_letter')),
    idempotency_key VARCHAR(255) NOT NULL,
    sqs_message_id VARCHAR(255),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (isp_plan_id, wave_number),
    UNIQUE (idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_campaign_waves_due ON mailing_campaign_waves(status, scheduled_at);
CREATE INDEX IF NOT EXISTS idx_campaign_waves_campaign ON mailing_campaign_waves(campaign_id, isp_plan_id);

CREATE TABLE IF NOT EXISTS mailing_campaign_plan_recipients (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    isp_plan_id UUID NOT NULL REFERENCES mailing_campaign_isp_plans(id) ON DELETE CASCADE,
    wave_id UUID REFERENCES mailing_campaign_waves(id) ON DELETE SET NULL,
    subscriber_id UUID NOT NULL REFERENCES mailing_subscribers(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    recipient_isp VARCHAR(50) NOT NULL,
    selection_rank INTEGER NOT NULL,
    audience_source_type VARCHAR(30) NOT NULL,
    audience_source_id UUID,
    status VARCHAR(20) DEFAULT 'selected'
        CHECK (status IN ('selected', 'queued', 'sent', 'failed', 'skipped', 'cancelled')),
    queued_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (isp_plan_id, subscriber_id)
);

CREATE INDEX IF NOT EXISTS idx_campaign_plan_recipients_plan ON mailing_campaign_plan_recipients(isp_plan_id, status, selection_rank);
CREATE INDEX IF NOT EXISTS idx_campaign_plan_recipients_wave ON mailing_campaign_plan_recipients(wave_id);

ALTER TABLE mailing_campaign_queue
    ADD COLUMN IF NOT EXISTS isp_plan_id UUID REFERENCES mailing_campaign_isp_plans(id) ON DELETE SET NULL;

ALTER TABLE mailing_campaign_queue
    ADD COLUMN IF NOT EXISTS wave_id UUID REFERENCES mailing_campaign_waves(id) ON DELETE SET NULL;

ALTER TABLE mailing_campaign_queue
    ADD COLUMN IF NOT EXISTS recipient_isp VARCHAR(50);

ALTER TABLE mailing_campaign_queue
    ADD COLUMN IF NOT EXISTS selection_rank INTEGER;

ALTER TABLE mailing_campaign_queue
    ADD COLUMN IF NOT EXISTS audience_source_type VARCHAR(30);

ALTER TABLE mailing_campaign_queue
    ADD COLUMN IF NOT EXISTS audience_source_id UUID;

CREATE INDEX IF NOT EXISTS idx_queue_wave_id ON mailing_campaign_queue(wave_id);
CREATE INDEX IF NOT EXISTS idx_queue_plan_id ON mailing_campaign_queue(isp_plan_id);
CREATE INDEX IF NOT EXISTS idx_queue_campaign_wave_schedule
    ON mailing_campaign_queue(campaign_id, wave_id, scheduled_at);
