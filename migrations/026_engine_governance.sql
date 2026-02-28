-- Migration 026: PMTA Multi-Agent Traffic Governance Engine
-- Adds tables for ISP-scoped agent configuration, rules, decisions,
-- signal snapshots, agent state, and ISP-scoped email suppression lists.

-- Per-ISP configuration (thresholds, deferral codes, connection limits)
CREATE TABLE IF NOT EXISTS mailing_engine_isp_config (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    isp VARCHAR(50) NOT NULL,
    display_name VARCHAR(100) NOT NULL,
    domain_patterns JSONB NOT NULL DEFAULT '[]',
    mx_patterns JSONB NOT NULL DEFAULT '[]',
    bounce_warn_pct DECIMAL(5,3) NOT NULL DEFAULT 2.0,
    bounce_action_pct DECIMAL(5,3) NOT NULL DEFAULT 5.0,
    complaint_warn_pct DECIMAL(6,4) NOT NULL DEFAULT 0.03,
    complaint_action_pct DECIMAL(6,4) NOT NULL DEFAULT 0.06,
    max_connections INTEGER NOT NULL DEFAULT 20,
    max_msg_rate INTEGER NOT NULL DEFAULT 500,
    deferral_codes JSONB NOT NULL DEFAULT '[]',
    known_behaviors JSONB NOT NULL DEFAULT '{}',
    pool_name VARCHAR(100) NOT NULL,
    warmup_schedule JSONB NOT NULL DEFAULT '[]',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(organization_id, isp)
);

-- Rule definitions scoped to ISP + agent type
CREATE TABLE IF NOT EXISTS mailing_engine_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    isp VARCHAR(50) NOT NULL,
    agent_type VARCHAR(30) NOT NULL
        CHECK (agent_type IN ('reputation','throttle','pool','warmup','emergency','suppression')),
    name VARCHAR(255) NOT NULL,
    description TEXT,
    metric VARCHAR(100) NOT NULL,
    operator VARCHAR(10) NOT NULL CHECK (operator IN ('>','>=','<','<=','==','!=')),
    threshold DECIMAL(10,4) NOT NULL,
    window_seconds INTEGER NOT NULL DEFAULT 3600,
    action VARCHAR(100) NOT NULL,
    action_params JSONB NOT NULL DEFAULT '{}',
    cooldown_seconds INTEGER NOT NULL DEFAULT 300,
    priority INTEGER NOT NULL DEFAULT 50,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Decision audit log
CREATE TABLE IF NOT EXISTS mailing_engine_decisions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    isp VARCHAR(50) NOT NULL,
    agent_type VARCHAR(30) NOT NULL,
    rule_id UUID REFERENCES mailing_engine_rules(id) ON DELETE SET NULL,
    signal_values JSONB NOT NULL DEFAULT '{}',
    action_taken VARCHAR(100) NOT NULL,
    action_params JSONB NOT NULL DEFAULT '{}',
    target_type VARCHAR(30),
    target_value VARCHAR(255),
    result VARCHAR(30) DEFAULT 'pending'
        CHECK (result IN ('pending','applied','failed','reverted','skipped')),
    reverted_at TIMESTAMPTZ,
    revert_reason TEXT,
    s3_decision_key TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Signal snapshots (rolling window aggregations)
CREATE TABLE IF NOT EXISTS mailing_engine_signals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    isp VARCHAR(50) NOT NULL,
    metric_name VARCHAR(100) NOT NULL,
    dimension_type VARCHAR(30) NOT NULL CHECK (dimension_type IN ('ip','domain','pool','global')),
    dimension_value VARCHAR(255) NOT NULL,
    value DECIMAL(12,6) NOT NULL,
    window_seconds INTEGER NOT NULL,
    sample_count INTEGER NOT NULL DEFAULT 0,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Current agent state (one row per ISP+agent_type)
CREATE TABLE IF NOT EXISTS mailing_engine_agent_state (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    isp VARCHAR(50) NOT NULL,
    agent_type VARCHAR(30) NOT NULL
        CHECK (agent_type IN ('reputation','throttle','pool','warmup','emergency','suppression')),
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active','paused','firing','error','cooldown')),
    last_eval_at TIMESTAMPTZ,
    decisions_count INTEGER NOT NULL DEFAULT 0,
    current_actions JSONB NOT NULL DEFAULT '[]',
    error_message TEXT,
    s3_state_key TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(organization_id, isp, agent_type)
);

-- ISP-scoped email suppression list ("Highway Patrol")
CREATE TABLE IF NOT EXISTS mailing_engine_suppressions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id),
    email TEXT NOT NULL,
    isp VARCHAR(50) NOT NULL,
    reason VARCHAR(100) NOT NULL,
    dsn_code VARCHAR(50),
    dsn_diagnostic TEXT,
    source_ip INET,
    source_vmta VARCHAR(255),
    campaign_id VARCHAR(255),
    suppressed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Unique: one email suppressed once per ISP per org
ALTER TABLE mailing_engine_suppressions
    ADD CONSTRAINT uq_engine_suppression_email_isp
    UNIQUE (organization_id, email, isp);

-- === INDEXES ===

-- ISP config
CREATE INDEX IF NOT EXISTS idx_engine_isp_config_org ON mailing_engine_isp_config(organization_id);
CREATE INDEX IF NOT EXISTS idx_engine_isp_config_isp ON mailing_engine_isp_config(isp);

-- Rules
CREATE INDEX IF NOT EXISTS idx_engine_rules_org ON mailing_engine_rules(organization_id);
CREATE INDEX IF NOT EXISTS idx_engine_rules_isp_agent ON mailing_engine_rules(isp, agent_type);
CREATE INDEX IF NOT EXISTS idx_engine_rules_enabled ON mailing_engine_rules(enabled) WHERE enabled = TRUE;

-- Decisions
CREATE INDEX IF NOT EXISTS idx_engine_decisions_org ON mailing_engine_decisions(organization_id);
CREATE INDEX IF NOT EXISTS idx_engine_decisions_isp ON mailing_engine_decisions(isp);
CREATE INDEX IF NOT EXISTS idx_engine_decisions_isp_agent ON mailing_engine_decisions(isp, agent_type);
CREATE INDEX IF NOT EXISTS idx_engine_decisions_created ON mailing_engine_decisions(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_engine_decisions_isp_created ON mailing_engine_decisions(isp, created_at DESC);

-- Signals
CREATE INDEX IF NOT EXISTS idx_engine_signals_org ON mailing_engine_signals(organization_id);
CREATE INDEX IF NOT EXISTS idx_engine_signals_isp ON mailing_engine_signals(isp);
CREATE INDEX IF NOT EXISTS idx_engine_signals_isp_metric ON mailing_engine_signals(isp, metric_name, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_engine_signals_recorded ON mailing_engine_signals(recorded_at DESC);

-- Agent state
CREATE INDEX IF NOT EXISTS idx_engine_agent_state_org ON mailing_engine_agent_state(organization_id);
CREATE INDEX IF NOT EXISTS idx_engine_agent_state_isp ON mailing_engine_agent_state(isp);

-- Suppressions (critical for performance)
CREATE INDEX IF NOT EXISTS idx_engine_suppressions_isp_email ON mailing_engine_suppressions(isp, email);
CREATE INDEX IF NOT EXISTS idx_engine_suppressions_email ON mailing_engine_suppressions(email);
CREATE INDEX IF NOT EXISTS idx_engine_suppressions_isp_at ON mailing_engine_suppressions(isp, suppressed_at DESC);
CREATE INDEX IF NOT EXISTS idx_engine_suppressions_org ON mailing_engine_suppressions(organization_id);
CREATE INDEX IF NOT EXISTS idx_engine_suppressions_reason ON mailing_engine_suppressions(isp, reason);
CREATE INDEX IF NOT EXISTS idx_engine_suppressions_campaign ON mailing_engine_suppressions(campaign_id) WHERE campaign_id IS NOT NULL;

-- Time-based partition hint for signals (large table)
-- Consider partitioning mailing_engine_signals by recorded_at in production.
