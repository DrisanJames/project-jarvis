BEGIN;

CREATE TABLE IF NOT EXISTS automation_flows (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    trigger_event   VARCHAR(50) NOT NULL,
    trigger_config  JSONB DEFAULT '{}',
    steps           JSONB NOT NULL DEFAULT '[]',
    status          VARCHAR(20) DEFAULT 'active' CHECK (status IN ('active', 'paused', 'archived')),
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_auto_flows_org ON automation_flows (organization_id);
CREATE INDEX idx_auto_flows_trigger ON automation_flows (trigger_event, status);

CREATE TABLE IF NOT EXISTS automation_executions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    flow_id         UUID NOT NULL REFERENCES automation_flows(id) ON DELETE CASCADE,
    subscriber_id   UUID NOT NULL,
    email           VARCHAR(255) NOT NULL,
    current_step    INTEGER DEFAULT 0,
    status          VARCHAR(20) DEFAULT 'running' CHECK (status IN ('running', 'paused', 'completed', 'failed', 'cancelled')),
    step_results    JSONB DEFAULT '[]',
    next_run_at     TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_auto_exec_flow ON automation_executions (flow_id, status);
CREATE INDEX idx_auto_exec_next ON automation_executions (next_run_at) WHERE status = 'running';
CREATE INDEX idx_auto_exec_sub ON automation_executions (subscriber_id);

-- Seed the default welcome series flow
INSERT INTO automation_flows (organization_id, name, description, trigger_event, steps) VALUES (
    '00000000-0000-0000-0000-000000000001',
    'DiscountBlog Welcome Series',
    'Automated welcome series for new discountblog.com subscribers',
    'subscriber_created',
    '[
        {"type": "send_email", "template": "welcome_verify", "delay_hours": 0},
        {"type": "wait", "delay_hours": 1},
        {"type": "condition", "check": "email_verified", "on_false": "skip_to_end"},
        {"type": "send_email", "template": "welcome_intro", "delay_hours": 0},
        {"type": "wait", "delay_hours": 24},
        {"type": "send_email", "template": "top_deals_week", "delay_hours": 0},
        {"type": "wait", "delay_hours": 48},
        {"type": "send_email", "template": "set_preferences", "delay_hours": 0},
        {"type": "wait", "delay_hours": 96},
        {"type": "send_email", "template": "weekly_digest_first", "delay_hours": 0}
    ]'::jsonb
) ON CONFLICT DO NOTHING;

COMMIT;
