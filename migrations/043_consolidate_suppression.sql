-- Migration 043: Consolidate suppression into single system
-- mailing_global_suppressions is the single source of truth.
-- This migration merges global suppression entries from the legacy
-- mailing_suppression_entries table (where is_global = TRUE) into
-- mailing_global_suppressions, then removes the redundant rows.

-- Step 1: Merge global entries that don't already exist in the hub
INSERT INTO mailing_global_suppressions (
    id, organization_id, email, md5_hash, reason, source, created_at
)
SELECT
    gen_random_uuid(),
    COALESCE(
        (SELECT organization_id FROM mailing_suppression_lists WHERE id = e.list_id LIMIT 1),
        (SELECT id FROM organizations LIMIT 1)
    ),
    LOWER(TRIM(e.email)),
    e.md5_hash,
    COALESCE(e.reason, e.category, 'manual'),
    COALESCE(e.source, 'legacy_migration'),
    COALESCE(e.created_at, NOW())
FROM mailing_suppression_entries e
WHERE e.is_global = TRUE
  AND e.md5_hash IS NOT NULL
  AND e.md5_hash != ''
  AND NOT EXISTS (
      SELECT 1 FROM mailing_global_suppressions g
      WHERE g.md5_hash = e.md5_hash
  )
ON CONFLICT DO NOTHING;

-- Step 2: Also merge entries from the mailing_suppressions legacy table
INSERT INTO mailing_global_suppressions (
    id, organization_id, email, md5_hash, reason, source, created_at
)
SELECT
    gen_random_uuid(),
    (SELECT id FROM organizations LIMIT 1),
    LOWER(TRIM(s.email)),
    MD5(LOWER(TRIM(s.email))),
    COALESCE(s.reason, 'manual'),
    COALESCE(s.source, 'legacy_migration'),
    COALESCE(s.created_at, NOW())
FROM mailing_suppressions s
WHERE s.active = TRUE
  AND NOT EXISTS (
      SELECT 1 FROM mailing_global_suppressions g
      WHERE g.md5_hash = MD5(LOWER(TRIM(s.email)))
  )
ON CONFLICT DO NOTHING;

-- Step 3: Remove global entries from the legacy table (they now live in the hub)
DELETE FROM mailing_suppression_entries WHERE is_global = TRUE;
