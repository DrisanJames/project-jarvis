-- Phantom-bounce data cleanup (one-shot; NOT part of startup migrations).
--
-- Context: prior to the PMTA-HTTP-bridge transport-error fix, every time
-- the bridge returned a 5xx or "out of connection slots", send_worker
-- wrote a "bounced" tracking event with bounce_reason LIKE 'PMTA API error%'.
-- PMTA then retried and delivered the message normally, leaving a phantom
-- bounce row behind. This inflated soft_bounce_count on campaigns and
-- produced misleading analytics (reported ~42% "soft bounce" rate when
-- the actual ISP-reported soft-bounce rate was <1%).
--
-- Run in two explicit phases so the operator can review counts before
-- committing the DELETE.

\set ON_ERROR_STOP on
SET statement_timeout = 600000; -- 10 min

\echo === 1. Phantom bounce row counts by sending_domain (review only) ===
SELECT COALESCE(sending_domain, '(null)') AS sending_domain,
       COUNT(*) AS phantom_rows
FROM mailing_tracking_events
WHERE event_at >= NOW() - INTERVAL '7 days'
  AND event_type = 'bounced'
  AND bounce_reason LIKE 'PMTA API error%'
GROUP BY 1 ORDER BY 2 DESC;

\echo === 2. Per-campaign phantom bounce counts (top 30) ===
SELECT campaign_id, COUNT(*) AS phantom_rows
FROM mailing_tracking_events
WHERE event_at >= NOW() - INTERVAL '7 days'
  AND event_type = 'bounced'
  AND bounce_reason LIKE 'PMTA API error%'
GROUP BY campaign_id ORDER BY 2 DESC LIMIT 30;

\echo === 3. Double-check: how many of these phantoms ALSO have a delivered event ===
WITH phantoms AS (
  SELECT DISTINCT campaign_id, subscriber_id
  FROM mailing_tracking_events
  WHERE event_at >= NOW() - INTERVAL '7 days'
    AND event_type = 'bounced'
    AND bounce_reason LIKE 'PMTA API error%'
    AND subscriber_id IS NOT NULL
)
SELECT
  (SELECT COUNT(*) FROM phantoms) AS total_phantom_pairs,
  (SELECT COUNT(*) FROM phantoms p
     WHERE EXISTS(
       SELECT 1 FROM mailing_tracking_events t
       WHERE t.event_type='delivered'
         AND t.campaign_id=p.campaign_id
         AND t.subscriber_id=p.subscriber_id
         AND t.event_at >= NOW() - INTERVAL '7 days'
     )
  ) AS also_delivered;

-- ============================================================
-- PHASE 2 — actually delete phantom rows and decrement counters.
-- Uncomment the block below after reviewing phase-1 output.
-- ============================================================

-- BEGIN;
--
-- -- Per-campaign phantom counts (used to decrement aggregate counters).
-- CREATE TEMP TABLE _phantom_campaign_counts AS
-- SELECT campaign_id, COUNT(*) AS n
-- FROM mailing_tracking_events
-- WHERE event_at >= NOW() - INTERVAL '7 days'
--   AND event_type = 'bounced'
--   AND bounce_reason LIKE 'PMTA API error%'
-- GROUP BY campaign_id;
--
-- SELECT COUNT(*) AS campaigns_to_fix, SUM(n) AS phantom_rows_to_delete
-- FROM _phantom_campaign_counts;
--
-- -- Delete the phantom tracking rows.
-- DELETE FROM mailing_tracking_events
-- WHERE event_at >= NOW() - INTERVAL '7 days'
--   AND event_type = 'bounced'
--   AND bounce_reason LIKE 'PMTA API error%';
--
-- -- Decrement campaign counters.
-- UPDATE mailing_campaigns c
-- SET bounce_count       = GREATEST(COALESCE(c.bounce_count, 0)       - pc.n, 0),
--     soft_bounce_count  = GREATEST(COALESCE(c.soft_bounce_count, 0)  - pc.n, 0),
--     updated_at         = NOW()
-- FROM _phantom_campaign_counts pc
-- WHERE c.id = pc.campaign_id;
--
-- COMMIT;
