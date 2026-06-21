-- ingest_parity.sql — PG-side parity: mailing_tracking_events (LIVE) vs
-- ingest_events_shadow (KAFKA-FED), joined on the deterministic event id.
-- ============================================================================
-- Read-only. Touches no existing table except SELECT.
--
-- Parameters (run_parity.py binds these as named params via psycopg2):
--   %(dt_start)s  inclusive lower bound on event_at (UTC, e.g. '2026-06-20')
--   %(dt_end)s    exclusive upper bound on event_at (UTC, e.g. '2026-06-21')
--
-- The deterministic identity of a tracking row is (id, event_at) — the same
-- key the live path dedups on via ON CONFLICT (id, event_at) DO NOTHING. We
-- compare the two sides on that key.
--
-- Four metrics, one row out:
--   missing_in_shadow  = | live \ shadow |   (completeness: should be 0)
--   excess_in_shadow   = | shadow \ live |   (excess:       should be 0)
--   field_mismatches   = rows in the intersection whose canonical-field hash
--                        differs (field fidelity: should be 0)
--   dedup_parity_fail  = ids whose collapsed-redelivery count differs between
--                        sides (same redeliveries must collapse the same way)
-- ============================================================================

WITH params AS (
    SELECT %(dt_start)s::timestamptz AS dt_start,
           %(dt_end)s::timestamptz   AS dt_end
),

-- Live side, deduped to the (id, event_at) identity exactly as the live path
-- would have it after ON CONFLICT. Canonical fields are the indispensable
-- columns; coalesced so NULL vs '' never reads as a false mismatch.
live AS (
    SELECT
        e.id,
        e.event_at,
        md5(
            coalesce(e.id::text, '')            || '|' ||
            coalesce(e.campaign_id::text, '')   || '|' ||
            coalesce(e.subscriber_id::text, '') || '|' ||
            coalesce(lower(e.email), '')         || '|' ||
            coalesce(e.event_type, '')          || '|' ||
            coalesce(e.event_at::text, '')
        ) AS field_hash
    FROM mailing_tracking_events e, params p
    WHERE e.event_at >= p.dt_start
      AND e.event_at <  p.dt_end
),

-- Shadow side. recipient is the shadow's column name for the live "email".
shadow AS (
    SELECT
        s.id,
        s.event_at,
        md5(
            coalesce(s.id::text, '')            || '|' ||
            coalesce(s.campaign_id::text, '')   || '|' ||
            coalesce(s.subscriber_id::text, '') || '|' ||
            coalesce(lower(s.recipient), '')     || '|' ||
            coalesce(s.event_type, '')          || '|' ||
            coalesce(s.event_at::text, '')
        ) AS field_hash
    FROM ingest_events_shadow s, params p
    WHERE s.event_at >= p.dt_start
      AND s.event_at <  p.dt_end
),

-- Dedup-parity: count physical rows per identity on each side. After the live
-- ON CONFLICT and the shadow UNIQUE (id, event_at), both should be 1 per id; a
-- mismatch means one side failed to collapse a redelivery.
live_counts AS (
    SELECT id, event_at, count(*) AS n FROM live  GROUP BY id, event_at
),
shadow_counts AS (
    SELECT id, event_at, count(*) AS n FROM shadow GROUP BY id, event_at
),

live_keys   AS (SELECT DISTINCT id, event_at FROM live),
shadow_keys AS (SELECT DISTINCT id, event_at FROM shadow)

SELECT
    (SELECT count(*) FROM live)   AS live_rows,
    (SELECT count(*) FROM shadow) AS shadow_rows,

    -- completeness: live keys not present in shadow
    (SELECT count(*) FROM (
        SELECT id, event_at FROM live_keys
        EXCEPT
        SELECT id, event_at FROM shadow_keys
     ) q) AS missing_in_shadow,

    -- excess: shadow keys not present in live
    (SELECT count(*) FROM (
        SELECT id, event_at FROM shadow_keys
        EXCEPT
        SELECT id, event_at FROM live_keys
     ) q) AS excess_in_shadow,

    -- field fidelity over the intersection
    (SELECT count(*) FROM live l
        JOIN shadow s USING (id, event_at)
        WHERE l.field_hash <> s.field_hash
    ) AS field_mismatches,

    -- dedup parity: identities whose collapsed count differs across sides
    (SELECT count(*) FROM live_counts lc
        FULL OUTER JOIN shadow_counts sc USING (id, event_at)
        WHERE coalesce(lc.n, 0) <> coalesce(sc.n, 0)
    ) AS dedup_parity_fail;
