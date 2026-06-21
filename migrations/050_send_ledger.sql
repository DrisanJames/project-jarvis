-- Migration 050: Kafka send-queue — SEND LEDGER (the serialization point)
-- ============================================================================
-- ADDITIVE ONLY. This table is NEW. This migration touches NO existing table
-- (no ALTER on mailing_campaign_queue, mailing_campaigns, the outbox state
-- machine, or anything else). It ships dark: nothing writes to this table until
-- the KafkaSendConsumer is wired behind its flag, and even then the default
-- SendSink is a NoOpSink that sends NO real email.
--
-- Purpose: mailing_send_ledger is the LOAD-BEARING serialization point that lets
-- Kafka be the primary send queue WITHOUT double-sending. The send pipeline is
-- at-least-once on the consumer side (Kafka redelivers on rebalance / crash /
-- offset-not-committed). To make that effectively-once at the SEND boundary, the
-- ledger's PRIMARY KEY (idempotency_key) is CLAIMED via an INSERT ... ON CONFLICT
-- DO NOTHING BEFORE the send. The unique-constraint conflict is the cross-worker
-- serialization primitive: under redelivery, rebalance, concurrent consumers, or
-- crash, at most ONE claim wins per key, so at most one SEND reaches PMTA.
--
-- INVARIANTS this table enforces (the whole point):
--   * No double-send: the PRIMARY KEY on idempotency_key + claim-before-send means
--     at most one worker transitions a key into 'sent'. The PMTA X-Idempotency
--     header (carried in SendCommand) is a backstop, not the primary guard.
--   * No drop: every idempotency_key eventually reaches status='sent'. The Kafka
--     offset is committed ONLY after status='sent' (or a definitive skip) is
--     recorded. A crash before RECORD leaves the row 'claimed'; the
--     LedgerReconciler (mirroring worker/outbox_reconciler.go) resolves it:
--       message_id present -> 'sent'   (commit the crash-window send)
--       message_id absent  -> 'failed' (re-claimable; PMTA idempotency makes the
--                                       re-send safe).
--
-- idempotency_key is the SAME deterministic UUIDv5 the outbox path already uses
-- (worker.OutboxIdempotencyKey over campaign+subscriber+wave) — see
-- internal/worker/outbox_idempotency.go — so the ledger and the existing queue
-- agree on key identity during a side-by-side cutover.
--
-- ----------------------------------------------------------------------------
-- WIRING (do later — DO NOT do it in this file, and DO NOT edit main.go now):
-- Production schema changes go through runStartupMigrations() in
-- cmd/server/main.go, NOT raw SQL against apex-postgres. When integrating, paste
-- the following idempotent Go snippet into runStartupMigrations(). It is the
-- exact, touches-nothing-existing equivalent of this migration. The CREATE TABLE
-- belongs in criticalSendPathDDL (it sits on the send path), NOT a 5s-budget
-- migration slice — schema must land before any KafkaSendConsumer claims a key.
--
--   // ---- Kafka send-queue ledger (migration 050, ships dark) ----
--   // Additive: new table only. No existing table is altered. Safe to run on
--   // every boot; CREATE ... IF NOT EXISTS is idempotent. Goes in
--   // criticalSendPathDDL so the claim INSERT can never reference a missing table.
--   if _, err := db.ExecContext(ctx, `
--   CREATE TABLE IF NOT EXISTS mailing_send_ledger (
--       idempotency_key UUID PRIMARY KEY,
--       campaign_id     UUID,
--       subscriber_id   UUID,
--       wave_id         UUID,
--       status          TEXT NOT NULL DEFAULT 'claimed',
--       message_id      TEXT,
--       worker_id       TEXT,
--       attempts        INT  NOT NULL DEFAULT 0,
--       claimed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
--       sent_at         TIMESTAMPTZ
--   )`); err != nil {
--       log.Printf("startup-migration 050 mailing_send_ledger: %v", err)
--   }
--   if _, err := db.ExecContext(ctx, `
--   CREATE INDEX IF NOT EXISTS idx_send_ledger_claimed
--       ON mailing_send_ledger (status, claimed_at) WHERE status='claimed'`); err != nil {
--       log.Printf("startup-migration 050 idx_send_ledger_claimed: %v", err)
--   }
--   // ---- end Kafka send-queue ledger ----
-- ============================================================================

CREATE TABLE IF NOT EXISTS mailing_send_ledger (
    idempotency_key UUID PRIMARY KEY,
    campaign_id     UUID,
    subscriber_id   UUID,
    wave_id         UUID,
    status          TEXT NOT NULL DEFAULT 'claimed',   -- claimed | sent | failed | requeued | dead
    message_id      TEXT,
    worker_id       TEXT,
    attempts        INT  NOT NULL DEFAULT 0,
    claimed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at         TIMESTAMPTZ
);

-- ADDITIVE (GAP 1 — guaranteed re-drive): store the marshaled SendCommand on the
-- ledger row at CLAIM time so the row is SELF-SUFFICIENT. The LedgerReconciler
-- re-PRODUCES this command back onto the send-commands topic to re-drive stranded
-- work, instead of leaving an orphaned terminal 'failed' row that relies on Kafka
-- redelivery (not guaranteed once the offset was committed via a skip path).
-- ADD COLUMN IF NOT EXISTS is idempotent and safe to run on every boot.
ALTER TABLE mailing_send_ledger ADD COLUMN IF NOT EXISTS command JSONB;

-- Partial index drives the LedgerReconciler scan: only rows still 'claimed' past
-- the grace window are candidates for crash-window resolution. WHERE status=
-- 'claimed' keeps the index tiny (it does not index 'sent'/'failed' rows).
CREATE INDEX IF NOT EXISTS idx_send_ledger_claimed
    ON mailing_send_ledger (status, claimed_at) WHERE status='claimed';

-- Re-drive scan index (GAP 1): the reconciler also re-drives stranded 'claimed'
-- (NULL message_id) and any 'failed' rows. Keep that scan cheap.
CREATE INDEX IF NOT EXISTS idx_send_ledger_redrive
    ON mailing_send_ledger (status, claimed_at)
    WHERE status IN ('claimed','failed','requeued');
