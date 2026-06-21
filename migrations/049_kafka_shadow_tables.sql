-- Migration 049: Kafka event-backbone — SHADOW tables (dark passenger)
-- ============================================================================
-- ADDITIVE ONLY. These tables are NEW. This migration touches NO existing
-- table (no ALTER on mailing_tracking_events, mailing_global_suppressions, the
-- Event Lake / Glue table, or anything else). It ships dark: nothing writes to
-- these tables until the Wave-2 shadow consumers are wired behind their flags.
--
-- Purpose: the Kafka migration runs Kafka as a "dark passenger" alongside the
-- live ingestion/suppression path. Producer taps publish a copy of every event
-- to Kafka; shadow CONSUMERS project those events into these tables. Parity is
-- proved by comparing the shadow projections against the live path on the
-- DETERMINISTIC event IDs that already exist:
--   * SES   event_uid = "ses:"  + eventID                (handlers_ses_events.go:451)
--   * PMTA  event_uid = "pmta:" + jobid:recipient:type:time (engine/ingest.go:392)
-- The live lake/tracking dedup is `ON CONFLICT (id, event_at) DO NOTHING`; the
-- shadow tables mirror that idempotency so reprocessing a Kafka partition (e.g.
-- a consumer-group reset) is a no-op and can't inflate parity counts.
--
-- ----------------------------------------------------------------------------
-- WIRING (do later — DO NOT do it in this file):
-- Production schema changes go through runStartupMigrations() in
-- cmd/server/main.go, NOT raw SQL against apex-postgres. When integrating,
-- paste the following idempotent Go snippet into runStartupMigrations(). It is
-- the exact, table-touching-nothing-existing equivalent of this migration:
--
--   // ---- Kafka event-backbone shadow tables (migration 049, ships dark) ----
--   // Additive: new tables only. No existing table is altered. Safe to run on
--   // every boot; CREATE ... IF NOT EXISTS is idempotent.
--   if _, err := db.ExecContext(ctx, `
--   CREATE TABLE IF NOT EXISTS ingest_events_shadow (
--       id                 UUID        NOT NULL,
--       campaign_id        UUID,
--       subscriber_id      UUID,
--       recipient          TEXT,
--       event_type         VARCHAR(20) NOT NULL,
--       isp                VARCHAR(50),
--       event_at           TIMESTAMPTZ NOT NULL,
--       kafka_event_uid    TEXT,
--       kafka_offset       BIGINT,
--       kafka_partition    INT,
--       shadow_ingested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
--       CONSTRAINT uq_ingest_events_shadow_id_event_at UNIQUE (id, event_at)
--   )`); err != nil {
--       log.Printf("startup-migration 049 ingest_events_shadow: %v", err)
--   }
--   for _, stmt := range []string{
--       `CREATE INDEX IF NOT EXISTS idx_ies_kafka_uid ON ingest_events_shadow (kafka_event_uid)`,
--       `CREATE INDEX IF NOT EXISTS idx_ies_event_at  ON ingest_events_shadow (event_at)`,
--       `CREATE INDEX IF NOT EXISTS idx_ies_campaign  ON ingest_events_shadow (campaign_id, event_at DESC)`,
--   } {
--       if _, err := db.ExecContext(ctx, stmt); err != nil {
--           log.Printf("startup-migration 049 ingest_events_shadow idx: %v", err)
--       }
--   }
--   if _, err := db.ExecContext(ctx, `
--   CREATE TABLE IF NOT EXISTS suppression_shadow (
--       email           TEXT NOT NULL,
--       reason          TEXT,
--       brand_root      TEXT NOT NULL DEFAULT '',
--       source          TEXT,
--       kafka_event_uid TEXT,
--       applied_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
--       CONSTRAINT uq_suppression_shadow_email_brand UNIQUE (email, brand_root)
--   )`); err != nil {
--       log.Printf("startup-migration 049 suppression_shadow: %v", err)
--   }
--   for _, stmt := range []string{
--       `CREATE INDEX IF NOT EXISTS idx_supp_shadow_email      ON suppression_shadow (email)`,
--       `CREATE INDEX IF NOT EXISTS idx_supp_shadow_applied_at ON suppression_shadow (applied_at DESC)`,
--       `CREATE INDEX IF NOT EXISTS idx_supp_shadow_kafka_uid  ON suppression_shadow (kafka_event_uid)`,
--   } {
--       if _, err := db.ExecContext(ctx, stmt); err != nil {
--           log.Printf("startup-migration 049 suppression_shadow idx: %v", err)
--       }
--   }
--   // ---- end Kafka shadow tables ----
--
-- NOTE on UNIQUE (email, brand_root): NULL is not equal to NULL under a UNIQUE
-- constraint, so brand_root carries DEFAULT '' (not NULL) — a global (brandless)
-- suppression collapses to the same key on reprocessing instead of duplicating.
-- ============================================================================


-- ----------------------------------------------------------------------------
-- ingest_events_shadow
-- Mirrors the indispensable columns the ingestion path persists to
-- mailing_tracking_events (id, campaign_id, subscriber_id, recipient,
-- event_type, isp, event_at) PLUS Kafka provenance. recipient is the shadow's
-- name for the live "email" field; isp is the shadow's name for the resolved
-- ISP group. The UNIQUE (id, event_at) mirrors the live PRIMARY KEY (id,
-- event_at) dedup so Kafka redeliveries / consumer replays are idempotent.
--
-- NOT partitioned: the shadow is a parity ledger, not a hot read path. It is
-- compared per-dt by parity SQL and truncated between parity runs, so the live
-- table's monthly RANGE partitioning is unnecessary here.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ingest_events_shadow (
    id                 UUID        NOT NULL,
    campaign_id        UUID,
    subscriber_id      UUID,
    recipient          TEXT,
    event_type         VARCHAR(20) NOT NULL,
    isp                VARCHAR(50),
    event_at           TIMESTAMPTZ NOT NULL,

    -- Kafka provenance (so a parity failure can be traced to an exact offset)
    kafka_event_uid    TEXT,
    kafka_offset       BIGINT,
    kafka_partition    INT,
    shadow_ingested_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Mirror of live dedup: PRIMARY KEY (id, event_at) on mailing_tracking_events
    CONSTRAINT uq_ingest_events_shadow_id_event_at UNIQUE (id, event_at)
);

-- Parity joins key on the deterministic event_uid and on the dt window.
CREATE INDEX IF NOT EXISTS idx_ies_kafka_uid ON ingest_events_shadow (kafka_event_uid);
CREATE INDEX IF NOT EXISTS idx_ies_event_at  ON ingest_events_shadow (event_at);
CREATE INDEX IF NOT EXISTS idx_ies_campaign  ON ingest_events_shadow (campaign_id, event_at DESC);


-- ----------------------------------------------------------------------------
-- suppression_shadow
-- The suppression projector's "would-apply" ledger: every suppression the
-- Kafka-fed path WOULD have written to mailing_global_suppressions + the
-- in-memory hub. Used by suppression_safety.sql to prove the shadow never FAILS
-- to suppress something the live path suppressed (suppressed_but_would_mail = 0).
--
-- UNIQUE (email, brand_root): one row per (email, brand). brand_root DEFAULT ''
-- represents a global/brandless suppression and collapses on reprocessing.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS suppression_shadow (
    email           TEXT NOT NULL,
    reason          TEXT,
    brand_root      TEXT NOT NULL DEFAULT '',
    source          TEXT,
    kafka_event_uid TEXT,
    applied_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_suppression_shadow_email_brand UNIQUE (email, brand_root)
);

CREATE INDEX IF NOT EXISTS idx_supp_shadow_email      ON suppression_shadow (email);
CREATE INDEX IF NOT EXISTS idx_supp_shadow_applied_at ON suppression_shadow (applied_at DESC);
CREATE INDEX IF NOT EXISTS idx_supp_shadow_kafka_uid  ON suppression_shadow (kafka_event_uid);
