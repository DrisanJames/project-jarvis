-- Migration 021: Enriched Inbox Profiles + Agent Pipeline Support
-- Adds columns for content preferences, inbox health, seasonal patterns,
-- revenue tracking, and agent classification to mailing_inbox_profiles.
-- Also adds agent decision logging table for real-time pipeline.

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 1: Enrich mailing_inbox_profiles
-- ═══════════════════════════════════════════════════════════════════════════

-- Content preferences (0.0 to 1.0 scores learned from engagement)
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS pref_text_score FLOAT DEFAULT 0.5;
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS pref_image_score FLOAT DEFAULT 0.5;
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS pref_personalized_score FLOAT DEFAULT 0.5;

-- Inbox health signals
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS inbox_health VARCHAR(20) DEFAULT 'unknown';
  -- Values: healthy, degraded, full, unknown
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS last_bounce_type VARCHAR(20);
  -- Values: soft, hard, inbox_full, throttle, null
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS last_bounce_at TIMESTAMPTZ;
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS consecutive_bounces INT DEFAULT 0;

-- Send suspension
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS send_suspended_until TIMESTAMPTZ;
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS suspension_reason VARCHAR(50);

-- Seasonal engagement pattern (monthly send/open counts for pattern detection)
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS monthly_engagement JSONB DEFAULT '{}';
  -- Format: {"2026-01": {"sent": 5, "opened": 3, "clicked": 1}, "2026-02": {...}}

-- Revenue attribution
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS revenue_total FLOAT DEFAULT 0;
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS revenue_last_90d FLOAT DEFAULT 0;
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS total_conversions INT DEFAULT 0;

-- Agent classification (set by agent pre-processor before send)
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS agent_classification VARCHAR(20);
  -- Values: send_now, send_later, defer, suppress, null
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS agent_content_strategy VARCHAR(30);
  -- Values: text_personalized, text_generic, image_personalized, image_generic, null
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS agent_classified_at TIMESTAMPTZ;
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS agent_classified_by UUID;
  -- References mailing_isp_agents.id

-- Data set tracking (3-digit code for data origin)
ALTER TABLE mailing_inbox_profiles ADD COLUMN IF NOT EXISTS data_set_code VARCHAR(10) DEFAULT 'IGN';

-- Indexes for agent pre-processing queries
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_inbox_health ON mailing_inbox_profiles(inbox_health);
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_suspended ON mailing_inbox_profiles(send_suspended_until) WHERE send_suspended_until IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_agent_class ON mailing_inbox_profiles(agent_classification) WHERE agent_classification IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_inbox_profiles_domain_engagement ON mailing_inbox_profiles(domain, engagement_score DESC);

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 2: Agent Send Decisions Table (per-campaign, per-recipient decisions)
-- ═══════════════════════════════════════════════════════════════════════════

CREATE TABLE IF NOT EXISTS mailing_agent_send_decisions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id UUID NOT NULL REFERENCES mailing_isp_agents(id) ON DELETE CASCADE,
    campaign_id UUID NOT NULL REFERENCES mailing_campaigns(id) ON DELETE CASCADE,
    email_hash VARCHAR(64) NOT NULL,
    classification VARCHAR(20) NOT NULL,      -- send_now, send_later, defer, suppress
    content_strategy VARCHAR(30),             -- text_personalized, image_personalized, etc.
    optimal_send_at TIMESTAMPTZ,              -- when to send this specific email
    priority INT DEFAULT 50,                  -- 0-100, higher = send first
    reasoning JSONB DEFAULT '{}',             -- why the agent made this decision
    executed BOOLEAN DEFAULT false,
    executed_at TIMESTAMPTZ,
    result VARCHAR(20),                       -- sent, bounced, opened, clicked, null
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_decisions_campaign ON mailing_agent_send_decisions(campaign_id, classification);
CREATE INDEX IF NOT EXISTS idx_agent_decisions_agent ON mailing_agent_send_decisions(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_decisions_email ON mailing_agent_send_decisions(email_hash);
CREATE INDEX IF NOT EXISTS idx_agent_decisions_pending ON mailing_agent_send_decisions(campaign_id, classification, priority DESC) WHERE executed = false;

-- ═══════════════════════════════════════════════════════════════════════════
-- PART 3: Update record_inbox_event function for enriched fields
-- ═══════════════════════════════════════════════════════════════════════════

CREATE OR REPLACE FUNCTION record_inbox_event_v2(
    p_email_hash VARCHAR,
    p_event_type VARCHAR,
    p_bounce_type VARCHAR DEFAULT NULL,
    p_content_type VARCHAR DEFAULT NULL  -- 'text' or 'image'
) RETURNS void AS $$
DECLARE
    v_profile_id UUID;
    v_month_key VARCHAR;
BEGIN
    v_month_key := to_char(NOW(), 'YYYY-MM');

    -- Upsert the basic profile
    INSERT INTO mailing_inbox_profiles (email_hash, domain, total_sends, updated_at)
    VALUES (
        p_email_hash,
        split_part(p_email_hash, '@', 2),
        CASE WHEN p_event_type = 'send' THEN 1 ELSE 0 END,
        NOW()
    )
    ON CONFLICT (email_hash) DO UPDATE SET updated_at = NOW()
    RETURNING id INTO v_profile_id;

    -- Update based on event type
    CASE p_event_type
        WHEN 'send' THEN
            UPDATE mailing_inbox_profiles SET
                total_sends = total_sends + 1,
                last_send_at = NOW(),
                monthly_engagement = jsonb_set(
                    COALESCE(monthly_engagement, '{}'::jsonb),
                    ARRAY[v_month_key, 'sent'],
                    to_jsonb(COALESCE((monthly_engagement->v_month_key->>'sent')::int, 0) + 1)
                )
            WHERE id = v_profile_id;

        WHEN 'open' THEN
            UPDATE mailing_inbox_profiles SET
                total_opens = total_opens + 1,
                last_open_at = NOW(),
                engagement_score = LEAST(1.0, engagement_score + 0.05),
                inbox_health = 'healthy',
                consecutive_bounces = 0,
                monthly_engagement = jsonb_set(
                    COALESCE(monthly_engagement, '{}'::jsonb),
                    ARRAY[v_month_key, 'opened'],
                    to_jsonb(COALESCE((monthly_engagement->v_month_key->>'opened')::int, 0) + 1)
                ),
                -- Update content preference based on what they opened
                pref_text_score = CASE
                    WHEN p_content_type = 'text' THEN LEAST(1.0, pref_text_score + 0.03)
                    WHEN p_content_type = 'image' THEN GREATEST(0.0, pref_text_score - 0.01)
                    ELSE pref_text_score
                END,
                pref_image_score = CASE
                    WHEN p_content_type = 'image' THEN LEAST(1.0, pref_image_score + 0.03)
                    WHEN p_content_type = 'text' THEN GREATEST(0.0, pref_image_score - 0.01)
                    ELSE pref_image_score
                END
            WHERE id = v_profile_id;

        WHEN 'click' THEN
            UPDATE mailing_inbox_profiles SET
                total_clicks = total_clicks + 1,
                last_click_at = NOW(),
                engagement_score = LEAST(1.0, engagement_score + 0.1),
                inbox_health = 'healthy',
                monthly_engagement = jsonb_set(
                    COALESCE(monthly_engagement, '{}'::jsonb),
                    ARRAY[v_month_key, 'clicked'],
                    to_jsonb(COALESCE((monthly_engagement->v_month_key->>'clicked')::int, 0) + 1)
                )
            WHERE id = v_profile_id;

        WHEN 'bounce' THEN
            UPDATE mailing_inbox_profiles SET
                total_bounces = total_bounces + 1,
                engagement_score = GREATEST(0.0, engagement_score - 0.2),
                last_bounce_type = COALESCE(p_bounce_type, 'soft'),
                last_bounce_at = NOW(),
                consecutive_bounces = consecutive_bounces + 1,
                inbox_health = CASE
                    WHEN p_bounce_type = 'inbox_full' THEN 'full'
                    WHEN consecutive_bounces >= 2 THEN 'degraded'
                    ELSE inbox_health
                END,
                -- Suspend if inbox full (7 days) or 3+ consecutive bounces (30 days)
                send_suspended_until = CASE
                    WHEN p_bounce_type = 'inbox_full' THEN NOW() + INTERVAL '7 days'
                    WHEN consecutive_bounces >= 3 THEN NOW() + INTERVAL '30 days'
                    ELSE send_suspended_until
                END,
                suspension_reason = CASE
                    WHEN p_bounce_type = 'inbox_full' THEN 'inbox_full'
                    WHEN consecutive_bounces >= 3 THEN 'consecutive_bounces'
                    ELSE suspension_reason
                END
            WHERE id = v_profile_id;

        WHEN 'complaint' THEN
            UPDATE mailing_inbox_profiles SET
                total_complaints = total_complaints + 1,
                engagement_score = GREATEST(0.0, engagement_score - 0.5),
                send_suspended_until = NOW() + INTERVAL '90 days',
                suspension_reason = 'complaint'
            WHERE id = v_profile_id;

        ELSE
            -- Unknown event type, just update timestamp
            NULL;
    END CASE;
END;
$$ LANGUAGE plpgsql;
