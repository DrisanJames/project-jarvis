-- Email Marketing Agent: persistent conversations, memory, domain strategies, campaign recommendations
-- Migration 047

-- 1. Agent Conversations — persistent chat sessions
CREATE TABLE IF NOT EXISTS agent_conversations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    title TEXT,
    summary TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_conversations_org ON agent_conversations(organization_id);
CREATE INDEX IF NOT EXISTS idx_agent_conversations_updated ON agent_conversations(updated_at DESC);

-- 2. Agent Messages — individual messages within conversations
CREATE TABLE IF NOT EXISTS agent_messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
    role VARCHAR(20) NOT NULL,
    content TEXT,
    tool_calls JSONB,
    tool_call_id VARCHAR(100),
    tokens_used INTEGER DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_messages_convo ON agent_messages(conversation_id, created_at);

-- 3. Agent Memory — extracted facts that persist across all conversations
CREATE TABLE IF NOT EXISTS agent_memory (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    category VARCHAR(50) NOT NULL,
    fact TEXT NOT NULL,
    source_conversation_id UUID REFERENCES agent_conversations(id) ON DELETE SET NULL,
    confidence DECIMAL(3,2) DEFAULT 0.80,
    active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_memory_org ON agent_memory(organization_id, active);

-- 4. Agent Domain Strategies — per-domain sending strategy config
CREATE TABLE IF NOT EXISTS agent_domain_strategies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    sending_domain VARCHAR(255) NOT NULL,
    strategy VARCHAR(20) NOT NULL DEFAULT 'warmup',
    params JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(organization_id, sending_domain)
);

CREATE INDEX IF NOT EXISTS idx_agent_strategies_org ON agent_domain_strategies(organization_id);

-- 5. Agent Campaign Recommendations — AI-generated campaign plans
CREATE TABLE IF NOT EXISTS agent_campaign_recommendations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    conversation_id UUID REFERENCES agent_conversations(id) ON DELETE SET NULL,
    sending_domain VARCHAR(255) NOT NULL,
    scheduled_date DATE NOT NULL,
    scheduled_time TIME,
    campaign_name TEXT,
    campaign_config JSONB NOT NULL DEFAULT '{}',
    reasoning TEXT,
    strategy VARCHAR(20),
    projected_volume INTEGER DEFAULT 0,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    approved_at TIMESTAMPTZ,
    approved_by TEXT,
    executed_campaign_id UUID,
    execution_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_recs_org_date ON agent_campaign_recommendations(organization_id, scheduled_date);
CREATE INDEX IF NOT EXISTS idx_agent_recs_status ON agent_campaign_recommendations(status);
CREATE INDEX IF NOT EXISTS idx_agent_recs_domain ON agent_campaign_recommendations(sending_domain);
