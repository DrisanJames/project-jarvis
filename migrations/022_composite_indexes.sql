-- Migration 022: Composite Indexes for Common Query Patterns
-- Date: 2026-02-07
--
-- Adds composite indexes to optimize the hot-path queries identified in:
--   - internal/api/campaign_builder.go   (campaign listing, filtering)
--   - internal/api/mailing_handlers.go   (list/subscriber queries)
--   - internal/worker/campaign_processor.go (queue claiming, batch processing)
--   - internal/worker/send_worker_v2.go  (send queue, suppression checks)
--   - internal/api/isp_agent_manager.go  (agent listing, decision feed)
--
-- All indexes use IF NOT EXISTS for idempotent execution.

-- ═══════════════════════════════════════════════════════════════════════════
-- 1. CAMPAIGN LISTING
-- Optimizes: HandleListCampaigns — WHERE organization_id = $1 AND status = $2
--            ORDER BY created_at DESC
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_campaigns_org_status_created
ON mailing_campaigns(organization_id, status, created_at DESC);

-- ═══════════════════════════════════════════════════════════════════════════
-- 2. SUBSCRIBER LISTING
-- Optimizes: getSubscribers / getAudienceCount —
--            WHERE list_id = $1 AND status = 'confirmed'
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_subscribers_list_status
ON mailing_subscribers(list_id, status);

-- ═══════════════════════════════════════════════════════════════════════════
-- 3. QUEUE PROCESSING: Worker batch claim
-- Optimizes: claimBatch() in campaign_processor.go —
--            WHERE status = 'queued' AND scheduled_at <= NOW()
--            ORDER BY priority DESC, scheduled_at ASC
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_queue_campaign_status_scheduled
ON mailing_campaign_queue(campaign_id, status, scheduled_at)
WHERE status = 'queued';

-- ═══════════════════════════════════════════════════════════════════════════
-- 4. QUEUE RECOVERY: Find stuck items claimed by workers that crashed
-- Optimizes: recovery queries for items in claimed/sending status
--            that have been locked too long
-- Note: locked_at column added in migration 002_scale_infrastructure.sql
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_queue_stuck_recovery
ON mailing_campaign_queue(status, locked_at)
WHERE status IN ('sending') AND locked_at IS NOT NULL;

-- ═══════════════════════════════════════════════════════════════════════════
-- 5. AGENT LISTING: Filtered by org + status
-- Optimizes: HandleListAgents — filters by status and isp, ordered by
--            last_active_at DESC
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_agents_org_status
ON mailing_isp_agents(organization_id, status);

-- ═══════════════════════════════════════════════════════════════════════════
-- 6. AGENT SEND DECISIONS: Lookup by agent + campaign
-- Optimizes: HandleAgentFeed / HandleAgentActivity —
--            WHERE agent_id = $1 AND campaign_id = $2
--            ORDER BY created_at DESC
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_agent_decisions_agent_campaign
ON mailing_agent_send_decisions(agent_id, campaign_id, created_at DESC);

-- ═══════════════════════════════════════════════════════════════════════════
-- 7. AGENT SEND DECISIONS: Webhook result updates by email hash
-- Optimizes: send_worker_v2 post-send update —
--            WHERE campaign_id = $1 AND email_hash = $2 AND executed = false
--            Also used for finding executed decisions awaiting webhook results
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_agent_decisions_email_executed
ON mailing_agent_send_decisions(email_hash, executed)
WHERE executed = true AND result IS NULL;

-- ═══════════════════════════════════════════════════════════════════════════
-- 8. INBOX PROFILES: Lookup by ISP for agent processing
-- Optimizes: HandleAgentLearn — queries profiles by domain/isp
--            Agent preprocessor filters by isp + inbox_health
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_inbox_profiles_isp_health
ON mailing_inbox_profiles(isp, inbox_health);

-- ═══════════════════════════════════════════════════════════════════════════
-- 9. SUPPRESSIONS: Fast email lookup for active suppressions
-- Optimizes: campaign_builder / send_worker suppression checks —
--            WHERE LOWER(email) = LOWER($1) AND active = true
--            WHERE email = $1 AND active = true
-- Note: Uses functional index on LOWER(email) to match case-insensitive
--       lookups used across the codebase
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_suppressions_active_email
ON mailing_suppressions(LOWER(email))
WHERE active = true;

-- ═══════════════════════════════════════════════════════════════════════════
-- 10. TRACKING EVENTS: Time-range queries per campaign
-- Optimizes: HandleCampaignTimeline — GROUP BY DATE_TRUNC('hour', ...)
--            WHERE campaign_id = $1
-- Note: Existing idx_tracking_campaign covers (campaign_id, event_at DESC).
--       This index adds created_at coverage for insertion-time queries.
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_tracking_events_campaign_time
ON mailing_tracking_events(campaign_id, created_at DESC);

-- ═══════════════════════════════════════════════════════════════════════════
-- 11. HOSTED IMAGES: Org listing ordered by upload time
-- Optimizes: image listing queries — WHERE org_id = $1 ORDER BY created_at DESC
-- Note: Existing idx_hosted_images_org covers (org_id) alone;
--       this composite avoids a sort step.
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_hosted_images_org_created
ON mailing_hosted_images(org_id, created_at DESC);

-- ═══════════════════════════════════════════════════════════════════════════
-- 12. LISTS: Org listing filtered by status
-- Optimizes: GetMailingLists — WHERE organization_id = $1 AND status = 'active'
--            ORDER BY created_at DESC
-- Note: Existing single-column indexes idx_lists_org and idx_lists_status
--       require an index intersection; this composite eliminates that.
-- ═══════════════════════════════════════════════════════════════════════════

CREATE INDEX IF NOT EXISTS idx_lists_org_status
ON mailing_lists(organization_id, status);
