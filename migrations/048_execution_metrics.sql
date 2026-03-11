-- Phase 5B: Add execution_metrics column to agent_campaign_recommendations
-- for recording campaign outcomes back into the EDITH learning loop.

ALTER TABLE agent_campaign_recommendations
    ADD COLUMN IF NOT EXISTS execution_metrics JSONB;

COMMENT ON COLUMN agent_campaign_recommendations.execution_metrics IS
    'Final campaign metrics recorded after completion: sent, delivered, bounced, opens, clicks, complaints';
