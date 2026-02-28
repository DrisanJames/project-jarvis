-- Migration 020: Offer Center & AI Agent System
-- Adds tables for: persistent ISP agents, agent-campaign links, creative library, AI suggestions

-- Persistent ISP agents (replaces computed aggregations)
CREATE TABLE IF NOT EXISTS mailing_isp_agents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    isp VARCHAR(50) NOT NULL,
    domain VARCHAR(255) NOT NULL,
    status VARCHAR(20) DEFAULT 'dormant',
    config JSONB DEFAULT '{}',
    knowledge JSONB DEFAULT '{}',
    total_campaigns INT DEFAULT 0,
    total_sends BIGINT DEFAULT 0,
    total_opens BIGINT DEFAULT 0,
    total_clicks BIGINT DEFAULT 0,
    total_bounces BIGINT DEFAULT 0,
    total_complaints BIGINT DEFAULT 0,
    avg_engagement FLOAT DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    last_active_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_isp_agents_org ON mailing_isp_agents(organization_id);
CREATE INDEX IF NOT EXISTS idx_isp_agents_isp ON mailing_isp_agents(isp);
CREATE INDEX IF NOT EXISTS idx_isp_agents_status ON mailing_isp_agents(status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_isp_agents_org_domain ON mailing_isp_agents(organization_id, domain);

-- Agent-to-campaign assignment
CREATE TABLE IF NOT EXISTS mailing_agent_campaigns (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id UUID REFERENCES mailing_isp_agents(id) ON DELETE CASCADE,
    campaign_id UUID REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    recipient_count INT DEFAULT 0,
    status VARCHAR(20) DEFAULT 'pending',
    send_window JSONB DEFAULT '{}',
    performance JSONB DEFAULT '{}',
    decisions JSONB DEFAULT '[]',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_agent_campaigns_agent ON mailing_agent_campaigns(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_campaigns_campaign ON mailing_agent_campaigns(campaign_id);
CREATE INDEX IF NOT EXISTS idx_agent_campaigns_status ON mailing_agent_campaigns(status);

-- Local creative library
CREATE TABLE IF NOT EXISTS mailing_creative_library (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    offer_id VARCHAR(50),
    offer_name VARCHAR(255),
    creative_name VARCHAR(255),
    source VARCHAR(20) DEFAULT 'everflow',
    html_content TEXT,
    text_content TEXT,
    thumbnail_url TEXT,
    ai_optimized BOOLEAN DEFAULT false,
    variant_of UUID REFERENCES mailing_creative_library(id) ON DELETE SET NULL,
    everflow_creative_id VARCHAR(50),
    tracking_link_template TEXT,
    metadata JSONB DEFAULT '{}',
    tags TEXT[] DEFAULT '{}',
    use_count INT DEFAULT 0,
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_creative_lib_org ON mailing_creative_library(organization_id);
CREATE INDEX IF NOT EXISTS idx_creative_lib_offer ON mailing_creative_library(offer_id);
CREATE INDEX IF NOT EXISTS idx_creative_lib_status ON mailing_creative_library(status);

-- AI mailing suggestions
CREATE TABLE IF NOT EXISTS mailing_ai_suggestions_v2 (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL,
    offer_id VARCHAR(50),
    offer_name VARCHAR(255),
    suggestion_type VARCHAR(50),
    reasoning TEXT,
    score FLOAT DEFAULT 0,
    metadata JSONB DEFAULT '{}',
    acted_on BOOLEAN DEFAULT false,
    generated_at TIMESTAMPTZ DEFAULT NOW(),
    expires_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_ai_suggestions_v2_org ON mailing_ai_suggestions_v2(organization_id);
CREATE INDEX IF NOT EXISTS idx_ai_suggestions_v2_type ON mailing_ai_suggestions_v2(suggestion_type);
CREATE INDEX IF NOT EXISTS idx_ai_suggestions_v2_score ON mailing_ai_suggestions_v2(score DESC);
