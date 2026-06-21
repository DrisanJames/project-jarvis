-- suppression_safety.sql — the "suppressed_but_would_mail" safety check.
-- ============================================================================
-- Read-only. THE most important parity query: it is a one-directional safety
-- assertion, not a symmetric diff.
--
-- INVARIANT: the Kafka-fed (shadow) suppression projection must NEVER FAIL to
-- suppress something the LIVE path suppressed. If the live path suppressed
-- (email, brand_root) inside the window, the shadow MUST also carry it.
-- Excess suppression in the shadow is SAFE (it only over-protects); a MISS is
-- the dangerous case — it means cutting over to Kafka would mail someone the
-- live path correctly held back. This query returns exactly those misses.
--
--   MUST RETURN 0 ROWS. Any row = a real "suppressed_but_would_mail" defect.
--
-- Matching semantics, mirroring the live suppression model
-- (mailing_global_suppressions + the in-memory hub):
--   * brand_root '' (empty) in the shadow = a GLOBAL suppression: it covers the
--     email for EVERY brand. So a live brand-scoped suppression is "covered" by
--     the shadow if the shadow has either the same (email, brand_root) OR a
--     global (email, '') row.
--   * email compared case-insensitively (lower()).
--
-- Parameters:
--   %(window_start)s  inclusive lower bound (mailing_global_suppressions.created_at)
--   %(window_end)s    exclusive upper bound
-- ============================================================================

WITH params AS (
    SELECT %(window_start)s::timestamptz AS window_start,
           %(window_end)s::timestamptz   AS window_end
),

-- LIVE suppressions applied in the window. brand_root is derived from the
-- live source/reason model; the live table is org+email keyed, so we treat
-- every live suppression as the authoritative "must be covered" set. We do not
-- have a brand_root column on mailing_global_suppressions, so a live row is a
-- GLOBAL must-cover requirement (brand '') — the strongest assertion: the
-- shadow must suppress this email for at least the global scope.
live_supp AS (
    SELECT DISTINCT lower(g.email) AS email
    FROM mailing_global_suppressions g, params p
    WHERE g.created_at >= p.window_start
      AND g.created_at <  p.window_end
),

-- Everything the shadow would suppress (any brand scope counts as coverage of
-- the email; a global '' row certainly does).
shadow_supp AS (
    SELECT DISTINCT lower(s.email) AS email
    FROM suppression_shadow s
)

-- The defect set: live-suppressed emails the shadow does NOT cover at all.
SELECT ls.email AS suppressed_but_would_mail
FROM live_supp ls
LEFT JOIN shadow_supp ss ON ss.email = ls.email
WHERE ss.email IS NULL
ORDER BY ls.email;
