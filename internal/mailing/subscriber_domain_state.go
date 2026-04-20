package mailing

// Master List Migration — Phase 2 (shadow writes)
//
// This file provides shared helpers for writing to
// `mailing_subscriber_domain_state` (the SDS side table). Every legacy
// subscriber-state write path (send dispatch, open tracking, click
// tracking, unsubscribe, bounce, complaint, engagement recompute) also
// calls into these helpers so the new per-domain state stays reconciled
// with the legacy mailing_subscribers row.
//
// During P2 these are ADDITIVE ONLY: the legacy path is still the
// source of truth, selection still reads the legacy list_id path, and
// SDS writes are observed but never read. P3 adds the backfill + the
// first master-selection pilot. Only after the 14-day shadow-write
// observation window do we trust SDS for reads.
//
// Design rules (locked in conversation, do not drift):
//   - Hard bounce + complaint stay GLOBAL on mailing_subscribers. We
//     additionally stamp hard_bounced_at / complained_at in SDS for
//     per-domain reporting, but the global flag is what suppresses the
//     address across every domain.
//   - Unsubscribe is DOMAIN-SCOPED. We write unsubscribed_at in SDS only.
//     The caller is responsible for deciding whether to ALSO write a
//     global suppression row (e.g. 3-part legacy tokens fall back to
//     global for safety).
//   - engagement_score on mailing_subscribers is a global rollup,
//     reporting-only. score_local in SDS is the selection signal.
//   - warmup_status is initialized as 'cold' by the DB default. First
//     send flips it to 'warming' when we observe the first send. The
//     warmup state machine (P5) promotes warming→engaged→dormant on a
//     nightly schedule; we never mutate those transitions from inline
//     send-time writers.

import (
	"context"
	"database/sql"
	"log"
	"strings"

	"github.com/google/uuid"
)

// NormalizeSendingDomain lowercases and trims a domain string. Empty
// sending domains are rejected by the caller (the SDS primary key
// includes sending_domain so empty would collapse unrelated brands).
func NormalizeSendingDomain(d string) string {
	return strings.TrimSpace(strings.ToLower(d))
}

// ResolveSendingDomainForCampaign looks up the campaign's from_email and
// returns the lowercased domain part. Returns empty string (and logs)
// when the campaign has no from_email — callers MUST treat the empty
// result as "skip SDS write", never as a valid row key.
func ResolveSendingDomainForCampaign(ctx context.Context, db *sql.DB, campaignID uuid.UUID) string {
	if db == nil || campaignID == uuid.Nil {
		return ""
	}
	var fromEmail string
	err := db.QueryRowContext(ctx, `SELECT COALESCE(from_email, '') FROM mailing_campaigns WHERE id = $1`, campaignID).Scan(&fromEmail)
	if err != nil {
		return ""
	}
	at := strings.LastIndex(fromEmail, "@")
	if at < 0 {
		return ""
	}
	return NormalizeSendingDomain(fromEmail[at+1:])
}

// UpsertSDSSend records a send for (subscriber, sending_domain):
//   - bumps total_sent
//   - stamps last_mailed_at = NOW()
//   - advances warmup_status from 'cold' to 'warming' on first send
//     (warmup_status_changed_at updated for audit). Does NOT touch
//     'warming', 'engaged', or 'dormant' rows — those transitions are
//     owned by the nightly warmup state-machine job.
func UpsertSDSSend(ctx context.Context, db *sql.DB, subscriberID uuid.UUID, sendingDomain string) {
	if db == nil || subscriberID == uuid.Nil {
		return
	}
	d := NormalizeSendingDomain(sendingDomain)
	if d == "" {
		return
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO mailing_subscriber_domain_state
			(subscriber_id, sending_domain, total_sent, last_mailed_at, warmup_status, warmup_status_changed_at, created_at, updated_at)
		VALUES ($1, $2, 1, NOW(), 'warming', NOW(), NOW(), NOW())
		ON CONFLICT (subscriber_id, sending_domain) DO UPDATE SET
			total_sent = mailing_subscriber_domain_state.total_sent + 1,
			last_mailed_at = NOW(),
			warmup_status = CASE
				WHEN mailing_subscriber_domain_state.warmup_status = 'cold' THEN 'warming'
				ELSE mailing_subscriber_domain_state.warmup_status
			END,
			warmup_status_changed_at = CASE
				WHEN mailing_subscriber_domain_state.warmup_status = 'cold' THEN NOW()
				ELSE mailing_subscriber_domain_state.warmup_status_changed_at
			END,
			updated_at = NOW()
	`, subscriberID, d); err != nil {
		log.Printf("[SDS] UpsertSDSSend failed sub=%s domain=%s: %v", subscriberID, d, err)
	}
}

// UpsertSDSOpen records an open. If the SDS row does not exist yet
// (edge case: tracking event arrives before the send row is written),
// we insert a minimal row so counters are not lost. warmup_status left
// at the default 'cold'; next send will flip to 'warming'.
func UpsertSDSOpen(ctx context.Context, db *sql.DB, subscriberID uuid.UUID, sendingDomain string) {
	if db == nil || subscriberID == uuid.Nil {
		return
	}
	d := NormalizeSendingDomain(sendingDomain)
	if d == "" {
		return
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO mailing_subscriber_domain_state
			(subscriber_id, sending_domain, total_opens, last_open_at, created_at, updated_at)
		VALUES ($1, $2, 1, NOW(), NOW(), NOW())
		ON CONFLICT (subscriber_id, sending_domain) DO UPDATE SET
			total_opens = mailing_subscriber_domain_state.total_opens + 1,
			last_open_at = NOW(),
			updated_at = NOW()
	`, subscriberID, d); err != nil {
		log.Printf("[SDS] UpsertSDSOpen failed sub=%s domain=%s: %v", subscriberID, d, err)
	}
}

// UpsertSDSClick records a click.
func UpsertSDSClick(ctx context.Context, db *sql.DB, subscriberID uuid.UUID, sendingDomain string) {
	if db == nil || subscriberID == uuid.Nil {
		return
	}
	d := NormalizeSendingDomain(sendingDomain)
	if d == "" {
		return
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO mailing_subscriber_domain_state
			(subscriber_id, sending_domain, total_clicks, last_click_at, created_at, updated_at)
		VALUES ($1, $2, 1, NOW(), NOW(), NOW())
		ON CONFLICT (subscriber_id, sending_domain) DO UPDATE SET
			total_clicks = mailing_subscriber_domain_state.total_clicks + 1,
			last_click_at = NOW(),
			updated_at = NOW()
	`, subscriberID, d); err != nil {
		log.Printf("[SDS] UpsertSDSClick failed sub=%s domain=%s: %v", subscriberID, d, err)
	}
}

// UpsertSDSUnsub stamps a domain-scoped unsubscribe. Idempotent: does
// not overwrite an earlier unsubscribed_at. Callers still decide
// independently whether to write a global suppression row (3-part
// legacy tokens do, 4-part brand-scoped tokens don't).
func UpsertSDSUnsub(ctx context.Context, db *sql.DB, subscriberID uuid.UUID, sendingDomain string) {
	if db == nil || subscriberID == uuid.Nil {
		return
	}
	d := NormalizeSendingDomain(sendingDomain)
	if d == "" {
		return
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO mailing_subscriber_domain_state
			(subscriber_id, sending_domain, unsubscribed_at, created_at, updated_at)
		VALUES ($1, $2, NOW(), NOW(), NOW())
		ON CONFLICT (subscriber_id, sending_domain) DO UPDATE SET
			unsubscribed_at = COALESCE(mailing_subscriber_domain_state.unsubscribed_at, NOW()),
			updated_at = NOW()
	`, subscriberID, d); err != nil {
		log.Printf("[SDS] UpsertSDSUnsub failed sub=%s domain=%s: %v", subscriberID, d, err)
	}
}

// UpsertSDSHardBounce stamps a per-domain hard-bounce timestamp AND
// flips the global hard_bounced_at on mailing_subscribers. Hard bounces
// suppress the address across every domain (the agreed design).
func UpsertSDSHardBounce(ctx context.Context, db *sql.DB, subscriberID uuid.UUID, sendingDomain string) {
	if db == nil || subscriberID == uuid.Nil {
		return
	}
	d := NormalizeSendingDomain(sendingDomain)
	if d != "" {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO mailing_subscriber_domain_state
				(subscriber_id, sending_domain, hard_bounced_at, created_at, updated_at)
			VALUES ($1, $2, NOW(), NOW(), NOW())
			ON CONFLICT (subscriber_id, sending_domain) DO UPDATE SET
				hard_bounced_at = COALESCE(mailing_subscriber_domain_state.hard_bounced_at, NOW()),
				updated_at = NOW()
		`, subscriberID, d); err != nil {
			log.Printf("[SDS] UpsertSDSHardBounce SDS write failed sub=%s domain=%s: %v", subscriberID, d, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE mailing_subscribers
		SET hard_bounced_at = COALESCE(hard_bounced_at, NOW()), updated_at = NOW()
		WHERE id = $1
	`, subscriberID); err != nil {
		log.Printf("[SDS] UpsertSDSHardBounce global stamp failed sub=%s: %v", subscriberID, err)
	}
}

// UpsertSDSComplaint stamps a per-domain complaint AND the global
// complained_at on mailing_subscribers. Complaints are reputation
// poison; we suppress globally.
func UpsertSDSComplaint(ctx context.Context, db *sql.DB, subscriberID uuid.UUID, sendingDomain string) {
	if db == nil || subscriberID == uuid.Nil {
		return
	}
	d := NormalizeSendingDomain(sendingDomain)
	if d != "" {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO mailing_subscriber_domain_state
				(subscriber_id, sending_domain, complained_at, created_at, updated_at)
			VALUES ($1, $2, NOW(), NOW(), NOW())
			ON CONFLICT (subscriber_id, sending_domain) DO UPDATE SET
				complained_at = COALESCE(mailing_subscriber_domain_state.complained_at, NOW()),
				updated_at = NOW()
		`, subscriberID, d); err != nil {
			log.Printf("[SDS] UpsertSDSComplaint SDS write failed sub=%s domain=%s: %v", subscriberID, d, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE mailing_subscribers
		SET complained_at = COALESCE(complained_at, NOW()), updated_at = NOW()
		WHERE id = $1
	`, subscriberID); err != nil {
		log.Printf("[SDS] UpsertSDSComplaint global stamp failed sub=%s: %v", subscriberID, err)
	}
}

// RecomputeSDSScoreLocal recalculates score_local for (subscriber,
// sending_domain) from that row's own counters. Mirrors the global
// engagement_score formula in mailing_tracking.go updateEngagementScore
// but scoped per-domain.
//
// Formula: 0.4 * open_rate + 0.6 * click_rate, with a recency bonus.
// Stored as NUMERIC(5,4) — rescaled to 0..1.
func RecomputeSDSScoreLocal(ctx context.Context, db *sql.DB, subscriberID uuid.UUID, sendingDomain string) {
	if db == nil || subscriberID == uuid.Nil {
		return
	}
	d := NormalizeSendingDomain(sendingDomain)
	if d == "" {
		return
	}
	// Single-statement recompute so the function stays cheap on the
	// tracking hot path. We treat total_sent=0 as score=0 to avoid
	// division-by-zero.
	if _, err := db.ExecContext(ctx, `
		UPDATE mailing_subscriber_domain_state
		SET score_local = LEAST(1.0::numeric,
			CASE WHEN COALESCE(total_sent, 0) = 0 THEN 0::numeric
			ELSE (
				(COALESCE(total_opens, 0)::numeric / GREATEST(total_sent, 1)) * 0.4
				+ (COALESCE(total_clicks, 0)::numeric / GREATEST(total_sent, 1)) * 0.6
				+ CASE
					WHEN last_open_at IS NOT NULL AND last_open_at > NOW() - INTERVAL '7 days' THEN 0.20
					WHEN last_open_at IS NOT NULL AND last_open_at > NOW() - INTERVAL '30 days' THEN 0.10
					ELSE 0
				END
			)
			END
		),
		updated_at = NOW()
		WHERE subscriber_id = $1 AND sending_domain = $2
	`, subscriberID, d); err != nil {
		log.Printf("[SDS] RecomputeSDSScoreLocal failed sub=%s domain=%s: %v", subscriberID, d, err)
	}
}
