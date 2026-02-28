-- ============================================================================
-- Migration 023: ISP Agent Learning Engine — Long-Term Memory & Web Research
-- Provides persistent storage for agent research sessions, learned facts,
-- and source quality scores to enable intelligent web research.
-- ============================================================================

-- Learning sessions log (each hourly research session)
CREATE TABLE IF NOT EXISTS mailing_isp_agent_research (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id UUID REFERENCES mailing_isp_agents(id) ON DELETE CASCADE,
    session_id VARCHAR(64) NOT NULL,
    isp VARCHAR(50) NOT NULL,
    domain VARCHAR(255),
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    duration_sec INT DEFAULT 0,
    sources_scraped INT DEFAULT 0,
    facts_found INT DEFAULT 0,
    sources JSONB DEFAULT '[]',
    facts JSONB DEFAULT '[]',
    status VARCHAR(20) DEFAULT 'running',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_research_agent ON mailing_isp_agent_research(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_research_isp ON mailing_isp_agent_research(isp);
CREATE INDEX IF NOT EXISTS idx_agent_research_session ON mailing_isp_agent_research(session_id);

-- Long-term memory facts (individual facts learned by agents)
CREATE TABLE IF NOT EXISTS mailing_isp_agent_ltm (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id UUID REFERENCES mailing_isp_agents(id) ON DELETE CASCADE,
    isp VARCHAR(50) NOT NULL,
    category VARCHAR(30) NOT NULL,          -- policy, threshold, best_practice, news, guideline, change
    fact TEXT NOT NULL,
    source_url TEXT,
    source_domain VARCHAR(255),
    confidence FLOAT DEFAULT 0.5,
    session_id VARCHAR(64),
    supersedes_id UUID,
    is_active BOOLEAN DEFAULT true,
    learned_at TIMESTAMPTZ DEFAULT NOW(),
    expires_at TIMESTAMPTZ,                 -- auto-expire news/temporary facts
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_ltm_agent ON mailing_isp_agent_ltm(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_ltm_isp ON mailing_isp_agent_ltm(isp);
CREATE INDEX IF NOT EXISTS idx_agent_ltm_category ON mailing_isp_agent_ltm(category);
CREATE INDEX IF NOT EXISTS idx_agent_ltm_active ON mailing_isp_agent_ltm(is_active) WHERE is_active = true;

-- Source quality scores (track which web sources yield useful info)
CREATE TABLE IF NOT EXISTS mailing_isp_source_scores (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_domain VARCHAR(255) NOT NULL UNIQUE,
    isp_relevance JSONB DEFAULT '{}',       -- per-ISP relevance scores
    times_scraped INT DEFAULT 0,
    total_facts INT DEFAULT 0,
    avg_relevance FLOAT DEFAULT 0,
    rating VARCHAR(20) DEFAULT 'unknown',   -- important, useful, waste, unknown
    last_scraped_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_source_scores_domain ON mailing_isp_source_scores(source_domain);
CREATE INDEX IF NOT EXISTS idx_source_scores_rating ON mailing_isp_source_scores(rating);
