package api

// Segment build ledger — mailing_segment_build_ledger.
//
// App-owned, one-row-per-segment summary table that makes the v2 segments
// list fast: instead of a COUNT(*) GROUP BY over the enormous
// mailing_segment_members rollup on every page load, the list does a single
// indexed read of this ledger. Every member-writing path upserts its row
// here when a build starts and again when it finishes.
//
// Why a companion table: mailing_segments is owned by apex_admin and the app
// user cannot ALTER it (CLAUDE.md §4), so the build-summary columns live in
// this app-owned table keyed by segment_id instead.
//
// Source values: 'materializer' | 'recalculate' | 'lake-builder' |
// 'lake-standard' | 'lake-engaged' | 'backfill' | 'bulk-tag' |
// 'partner-drip' (the partner-drip upsert SQL is inlined in
// internal/worker/partner_drip_orchestrator.go because internal/api already
// imports internal/worker — keep it in sync with UpsertSegmentLedger below)
// Status values: 'running' | 'ok' | 'failed' | 'blocked_delta' | 'unknown'
//
// Best-effort philosophy: the ledger is observability, not control flow.
// Callers MUST log-and-continue on any ledger error and never fail a build
// because of the ledger. All helpers here are nil-DB safe and, on write
// paths, detach from already-cancelled caller contexts so a timed-out build
// can still record its failure.

import (
	"context"
	"database/sql"
	"time"
	"unicode/utf8"
)

// ledgerRunningStaleAfter: a 'running' ledger row whose updated_at is older
// than this is treated as failed — the build crashed (or its task was
// recycled) mid-build and never wrote a terminal status. The list read path
// coerces the DISPLAYED status to 'failed' via coerceLedgerStatus; builders
// must likewise ignore a stale 'running' row (it is NOT an in-flight build
// and must not block a new build from starting).
const ledgerRunningStaleAfter = 30 * time.Minute

// ledgerErrorMaxLen caps last_error so a multi-KB driver error can't bloat
// the hot ledger table.
const ledgerErrorMaxLen = 500

// ledgerWriteTimeout bounds every ledger statement; the table is tiny and a
// PK upsert should be instant — anything slower means the DB is in trouble
// and the build should not wait on observability.
const ledgerWriteTimeout = 10 * time.Second

// truncateLedgerError caps errMsg at ledgerErrorMaxLen bytes, backing off to
// a valid UTF-8 boundary so the truncated string is still insertable into a
// Postgres TEXT column (byte-slicing mid-rune would make the INSERT itself
// fail with "invalid byte sequence").
func truncateLedgerError(msg string) string {
	if len(msg) <= ledgerErrorMaxLen {
		return msg
	}
	cut := msg[:ledgerErrorMaxLen]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// coerceLedgerStatus maps a stored status to the status a reader should
// display: a 'running' row not updated within ledgerRunningStaleAfter is a
// crashed build and is shown as 'failed'. Pure so it is unit-testable.
func coerceLedgerStatus(status string, updatedAt, now time.Time) string {
	if status == "running" && now.Sub(updatedAt) > ledgerRunningStaleAfter {
		return "failed"
	}
	return status
}

// computeLedgerDeltaPct returns the percentage change from prev to cur,
// defined as 0 when there is no meaningful prior count (prev <= 0). Pure so
// it is unit-testable.
func computeLedgerDeltaPct(prev, cur int64) float64 {
	if prev <= 0 {
		return 0
	}
	return float64(cur-prev) / float64(prev) * 100
}

// ledgerWriteCtx returns a bounded context for a ledger write. If the
// caller's context is already cancelled/expired (e.g. a build that just
// timed out and wants to record 'failed'), the write detaches to a fresh
// background context so the terminal status still lands.
func ledgerWriteCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil || ctx.Err() != nil {
		return context.WithTimeout(context.Background(), ledgerWriteTimeout)
	}
	return context.WithTimeout(ctx, ledgerWriteTimeout)
}

// UpsertSegmentLedger records a finished (or blocked) build: one INSERT ...
// ON CONFLICT (segment_id) DO UPDATE covering every summary field.
// errMsg is truncated to ledgerErrorMaxLen and stored as NULL when empty.
//
// last_built_at and subscriber_count only advance when status='ok': a
// failed/blocked build must neither claim "freshly built" nor overwrite the
// last good audience count. The status/error/duration/delta fields always
// reflect the most recent attempt.
//
// Best-effort: callers log-and-continue on error.
func UpsertSegmentLedger(ctx context.Context, db *sql.DB, segmentID string, count int64, source, status string, durMs int, deltaPct float64, errMsg string) error {
	if db == nil {
		return nil
	}
	wctx, cancel := ledgerWriteCtx(ctx)
	defer cancel()
	_, err := db.ExecContext(wctx, `
		INSERT INTO mailing_segment_build_ledger
			(segment_id, subscriber_count, last_built_at, build_source,
			 last_build_status, last_build_ms, last_delta_pct, last_error, updated_at)
		VALUES ($1::uuid, $2,
		        CASE WHEN $4::text = 'ok' THEN NOW() END,
		        $3, $4, $5, $6, NULLIF($7, ''), NOW())
		ON CONFLICT (segment_id) DO UPDATE SET
			subscriber_count  = CASE WHEN EXCLUDED.last_build_status = 'ok'
			                         THEN EXCLUDED.subscriber_count
			                         ELSE mailing_segment_build_ledger.subscriber_count END,
			last_built_at     = CASE WHEN EXCLUDED.last_build_status = 'ok'
			                         THEN NOW()
			                         ELSE mailing_segment_build_ledger.last_built_at END,
			build_source      = EXCLUDED.build_source,
			last_build_status = EXCLUDED.last_build_status,
			last_build_ms     = EXCLUDED.last_build_ms,
			last_delta_pct    = EXCLUDED.last_delta_pct,
			last_error        = EXCLUDED.last_error,
			updated_at        = NOW()
	`, segmentID, count, source, status, durMs, deltaPct, truncateLedgerError(errMsg))
	return err
}

// MarkSegmentBuildRunning flags a build as in-flight WITHOUT touching the
// last good subscriber_count / last_built_at, so readers keep showing the
// previous successful build while a new one runs. updated_at = NOW() is the
// staleness clock for ledgerRunningStaleAfter. Best-effort: callers
// log-and-continue on error.
func MarkSegmentBuildRunning(ctx context.Context, db *sql.DB, segmentID, source string) error {
	if db == nil {
		return nil
	}
	wctx, cancel := ledgerWriteCtx(ctx)
	defer cancel()
	_, err := db.ExecContext(wctx, `
		INSERT INTO mailing_segment_build_ledger
			(segment_id, build_source, last_build_status, updated_at)
		VALUES ($1::uuid, $2, 'running', NOW())
		ON CONFLICT (segment_id) DO UPDATE SET
			build_source      = EXCLUDED.build_source,
			last_build_status = 'running',
			last_error        = NULL,
			updated_at        = NOW()
	`, segmentID, source)
	return err
}

// ledgerPriorCount returns the segment's last successfully-built
// subscriber_count, used to compute last_delta_pct for the next build.
// ok is false when there is no prior COMPLETED build (no row, row created
// only by MarkSegmentBuildRunning, or read error) — callers report delta 0
// in that case. Best-effort read: errors degrade to (0, false).
func ledgerPriorCount(ctx context.Context, db *sql.DB, segmentID string) (count int64, ok bool) {
	if db == nil {
		return 0, false
	}
	rctx, cancel := ledgerWriteCtx(ctx)
	defer cancel()
	var n int64
	var builtAt sql.NullTime
	err := db.QueryRowContext(rctx, `
		SELECT subscriber_count, last_built_at
		  FROM mailing_segment_build_ledger
		 WHERE segment_id = $1::uuid
	`, segmentID).Scan(&n, &builtAt)
	if err != nil {
		return 0, false
	}
	return n, builtAt.Valid
}
