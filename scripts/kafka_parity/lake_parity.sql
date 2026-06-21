-- lake_parity.sql — Event-Lake parity: LIVE lake partition vs KAFKA-SHADOW
-- partition, for a given dt, keyed on the deterministic event_uid.
-- ============================================================================
-- Read-only.
--
-- WHERE THE LIVE LAKE ACTUALLY LIVES
-- The production Event Lake is the Glue/Athena table
-- `ignite_analytics.email_events` (internal/analytics/lake_emitter.go), fed by
-- Firehose, partitioned by (source, dt), keyed by event_uid. The CANONICAL
-- engine for this query is Athena. Because the repo's parity runner connects to
-- Postgres via IGNITE_DSN, this file is written to run against two PG-resident
-- projections of the same two partitions:
--
--   live_lake_events_<dt>     -- the live Firehose-fed lake rows for this dt
--   kafka_lake_events_<dt>    -- the kafka-shadow consumer's lake projection
--
-- run_parity.py resolves the table names from %(live_lake_table)s /
-- %(kafka_lake_table)s (defaulting to ingest_events_shadow-style projections in
-- dev). Both projections expose at minimum: event_uid, campaign_id, email,
-- event_type, event_at, isp_group, route_type, dt. The ATHENA-native form of
-- this same query is in the trailing comment block.
--
-- Parameters:
--   %(dt)s                 partition date, e.g. '2026-06-20'
--   %(live_lake_table)s    identifier of the live lake projection  (psycopg2.sql.Identifier)
--   %(kafka_lake_table)s   identifier of the kafka-shadow lake projection
--
-- Three metrics (event_uid is already deduped per the lake's DISTINCT-event_uid
-- contract — lake_emitter.go:15):
--   missing_in_kafka  = | live_uids \ kafka_uids |   completeness (should be 0)
--   excess_in_kafka   = | kafka_uids \ live_uids |   excess        (should be 0)
--   field_mismatches  = canonical-field hash differs on the intersection
-- ============================================================================

WITH live AS (
    SELECT
        l.event_uid,
        md5(
            coalesce(l.event_uid, '')         || '|' ||
            coalesce(l.campaign_id, '')       || '|' ||
            coalesce(lower(l.email), '')        || '|' ||
            coalesce(l.event_type, '')        || '|' ||
            coalesce(l.isp_group, '')         || '|' ||
            coalesce(l.route_type, '')        || '|' ||
            coalesce(l.event_at, '')
        ) AS field_hash
    FROM {live_lake_table} l
    WHERE l.dt = %(dt)s
),
kafka AS (
    SELECT
        k.event_uid,
        md5(
            coalesce(k.event_uid, '')         || '|' ||
            coalesce(k.campaign_id, '')       || '|' ||
            coalesce(lower(k.email), '')        || '|' ||
            coalesce(k.event_type, '')        || '|' ||
            coalesce(k.isp_group, '')         || '|' ||
            coalesce(k.route_type, '')        || '|' ||
            coalesce(k.event_at, '')
        ) AS field_hash
    FROM {kafka_lake_table} k
    WHERE k.dt = %(dt)s
),
live_u  AS (SELECT DISTINCT event_uid FROM live),
kafka_u AS (SELECT DISTINCT event_uid FROM kafka)

SELECT
    (SELECT count(DISTINCT event_uid) FROM live)  AS live_uids,
    (SELECT count(DISTINCT event_uid) FROM kafka) AS kafka_uids,

    -- completeness: live event_uids absent from the kafka-shadow partition
    (SELECT count(*) FROM (
        SELECT event_uid FROM live_u
        EXCEPT
        SELECT event_uid FROM kafka_u
     ) q) AS missing_in_kafka,

    -- excess: kafka event_uids absent from the live partition
    (SELECT count(*) FROM (
        SELECT event_uid FROM kafka_u
        EXCEPT
        SELECT event_uid FROM live_u
     ) q) AS excess_in_kafka,

    -- field fidelity: dedup each side to one hash per event_uid (lake contract
    -- is one logical event per uid) then compare on the intersection.
    (SELECT count(*) FROM
        (SELECT DISTINCT event_uid, field_hash FROM live)  l
        JOIN
        (SELECT DISTINCT event_uid, field_hash FROM kafka) k
          USING (event_uid)
        WHERE l.field_hash <> k.field_hash
    ) AS field_mismatches;

-- ============================================================================
-- ATHENA-NATIVE EQUIVALENT (for when parity is run against the real lake):
--
--   WITH live AS (
--     SELECT event_uid, lower(email) email, campaign_id, event_type,
--            isp_group, route_type, event_at
--     FROM ignite_analytics.email_events
--     WHERE dt = ? AND source IN ('ses','pmta')      -- live producers
--   ),
--   kafka AS (
--     SELECT event_uid, lower(email) email, campaign_id, event_type,
--            isp_group, route_type, event_at
--     FROM ignite_analytics.email_events
--     WHERE dt = ? AND source IN ('ses_kafka','pmta_kafka')  -- shadow producers
--   )
--   SELECT
--     (SELECT count(DISTINCT event_uid) FROM live)  live_uids,
--     (SELECT count(DISTINCT event_uid) FROM kafka) kafka_uids,
--     cardinality(array_except(
--        (SELECT array_agg(DISTINCT event_uid) FROM live),
--        (SELECT array_agg(DISTINCT event_uid) FROM kafka))) missing_in_kafka,
--     cardinality(array_except(
--        (SELECT array_agg(DISTINCT event_uid) FROM kafka),
--        (SELECT array_agg(DISTINCT event_uid) FROM live)))  excess_in_kafka;
-- ============================================================================
