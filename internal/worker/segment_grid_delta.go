package worker

// SegmentGridWorker — DELTA write path (phase 2, operator-approved
// 2026-08-21: "That's the architecture we need. And it is quick").
//
// Phase 1 rebuilt every grid segment with a full DELETE + COPY swap. Full
// swaps are what exhausted RDS EBSIOBalance to 0% (5 concurrent 300s+
// member inserts, peer-verified in pg_stat_activity). Phase 2 keeps the
// phase-1 full path INTACT (bootstrap, reconcile, and every fallback) and
// adds the delta path around it:
//
//   Athena side (internal/analytics/segment_grid_delta.go): each pass
//   UNLOADs the bucket's (subscriber_id, brand) pairs to a per-day S3
//   snapshot, then the NEXT pass anti-joins today vs the base snapshot and
//   returns only adds + removes.
//
//   PG side (this file): adds/removes ship via COPY into the UNLOGGED
//   staging table below, then ONE set-based transaction under the shared
//   per-segment advisory lock: DELETE the removes, INSERT the adds
//   (ON CONFLICT DO NOTHING against the members PK; materialized_at is
//   stamped on adds only), recount, restamp the cached segment count,
//   truncate the stage. A failed merge rolls the whole thing back — members
//   are never left partially applied.
//
// State model (all additive; a missing column/table degrades to the phase-1
// full path, never breaks the ledger contract):
//   - mailing_segment_grid_snapshots (PG): one row per (event, window, dt)
//     snapshot that COMPLETED its UNLOAD. Written after the UNLOAD returns,
//     so a crash mid-UNLOAD leaves no row and the retry deletes the partial
//     prefix and re-UNLOADs (the S3 files alone are never trusted).
//   - mailing_segment_build_ledger.last_snapshot_dt: the snapshot day the
//     segment's PG membership currently corresponds to — the diff BASE.
//     Stamped on every 'ok' build (cleared when a full build ran without a
//     snapshot, so a stale base can never be diffed against).
//   - mailing_segment_build_ledger.last_full_built_at: the reconcile clock —
//     every segmentGridReconcileEvery a full rebuild is forced to correct
//     the drift the delta path accrues by design (Go-side filters apply to
//     adds only; snapshot/build timing skews by minutes).

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/lib/pq"
)

const (
	// Ledger build_source values (kept in sync with the doc comment in
	// internal/api/segment_ledger.go). Phase 1 wrote 'lake-grid'; phase 2
	// splits it so S3-vs-PG drift analysis can tell which path built a row.
	segmentGridLedgerSourceFull  = "lake-grid-full"
	segmentGridLedgerSourceDelta = "lake-grid-delta"

	// segmentGridReconcileEvery: a segment whose last FULL build is older
	// than this is force-rebuilt via the full path — drift between S3
	// snapshots and PG truth is inevitable, design for it.
	segmentGridReconcileEvery = 7 * 24 * time.Hour

	// segmentGridDeltaMaxBaseAgeDays: a diff base older than this is refused
	// (the accumulated day-over-day churn approaches a full swap anyway and
	// the projection/prune horizon is near) — full path instead.
	segmentGridDeltaMaxBaseAgeDays = 7

	// segmentGridSnapshotPruneAfterDays: snapshot partitions older than this
	// are pruned (S3 objects + PG bookkeeping rows), best-effort.
	segmentGridSnapshotPruneAfterDays = 14

	// Timeouts for the Athena legs of the delta path.
	segmentGridUnloadTimeout = 10 * time.Minute
	segmentGridDiffTimeout   = 5 * time.Minute
)

// ---------------------------------------------------------------------------
// Schema (registered in cmd/server/main.go runStartupMigrations — each const
// its OWN entry, SET/RESET lock_timeout bracketed like the phase-1 slice).
// ---------------------------------------------------------------------------

// SegmentGridStageDDL is the UNLOGGED staging table the delta merge COPYs
// into. UNLOGGED: no WAL — the whole point of this phase is IO relief, and
// stage content is transient by definition (truncated at the start and end
// of every merge; loss on crash is free because the merge TX that reads it
// either committed or never happened). Single-writer by construction: only
// the distlock-elected grid leader touches it, sequentially.
const SegmentGridStageDDL = `CREATE UNLOGGED TABLE IF NOT EXISTS mailing_segment_grid_stage (
	op            TEXT NOT NULL,
	subscriber_id UUID NOT NULL,
	email         TEXT NOT NULL DEFAULT ''
)`

// SegmentGridSnapshotsDDL records which bucket-day snapshots COMPLETED their
// UNLOAD. The diff path trusts only rows here, never bare S3 listings.
const SegmentGridSnapshotsDDL = `CREATE TABLE IF NOT EXISTS mailing_segment_grid_snapshots (
	event        TEXT NOT NULL,
	window_days  INT  NOT NULL,
	dt           TEXT NOT NULL,
	pair_count   BIGINT NOT NULL DEFAULT 0,
	completed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (event, window_days, dt)
)`

// SegmentGridLedgerFullBuiltAtDDL / SegmentGridLedgerSnapshotDtDDL extend the
// app-owned build ledger with the delta-state columns. Nullable, no default —
// instant ADD COLUMN under the slice's 2s lock_timeout bracket.
const SegmentGridLedgerFullBuiltAtDDL = `ALTER TABLE mailing_segment_build_ledger ADD COLUMN IF NOT EXISTS last_full_built_at TIMESTAMPTZ`

// SegmentGridLedgerSnapshotDtDDL — see SegmentGridLedgerFullBuiltAtDDL.
const SegmentGridLedgerSnapshotDtDDL = `ALTER TABLE mailing_segment_build_ledger ADD COLUMN IF NOT EXISTS last_snapshot_dt TEXT`

// ---------------------------------------------------------------------------
// Delta state (ledger columns + snapshot bookkeeping)
// ---------------------------------------------------------------------------

// gridDeltaState is the per-segment slice of the ledger the path router
// consults. Read in a SEPARATE query from loadLedger on purpose: if the
// additive columns have not landed yet (migration timed out and is absent
// until next boot), only this read fails — the pass logs it, routes every
// segment down the phase-1 full path, and the core ledger contract is
// untouched.
type gridDeltaState struct {
	fullBuiltAt sql.NullTime
	snapshotDt  string
}

// loadDeltaState returns the delta-state map, or (nil, false) when the read
// failed (missing columns, truncated read) — the caller must then treat the
// whole pass as delta-ineligible.
func (w *SegmentGridWorker) loadDeltaState(ctx context.Context, ids []string) (map[string]gridDeltaState, bool) {
	out := make(map[string]gridDeltaState, len(ids))
	if len(ids) == 0 {
		return out, true
	}
	rows, err := w.db.QueryContext(ctx, `
		SELECT segment_id::text, last_full_built_at, COALESCE(last_snapshot_dt, '')
		FROM mailing_segment_build_ledger
		WHERE segment_id = ANY($1::uuid[])
	`, pq.Array(ids))
	if err != nil {
		log.Printf("[SegmentGrid] delta-state read failed (delta path disabled this pass): %v", err)
		return nil, false
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var st gridDeltaState
		if err := rows.Scan(&id, &st.fullBuiltAt, &st.snapshotDt); err != nil {
			log.Printf("[SegmentGrid] delta-state scan failed (delta path disabled this pass): %v", err)
			return nil, false
		}
		out[id] = st
	}
	if err := rows.Err(); err != nil {
		log.Printf("[SegmentGrid] delta-state read truncated (delta path disabled this pass): %v", err)
		return nil, false
	}
	return out, true
}

// ledgerStampDeltaState records, after an 'ok' ledger upsert, which snapshot
// day the segment's membership now corresponds to and (for full builds) that
// a full rebuild happened. SEPARATE best-effort statement from the phase-1
// ledgerUpsert so a missing column can never break the core ledger writes.
// snapshotDt == "" CLEARS the base (a full build without a snapshot must not
// leave a stale base a later diff could be applied against).
func (w *SegmentGridWorker) ledgerStampDeltaState(ctx context.Context, segmentID, source, snapshotDt string) {
	wctx, cancel := gridLedgerCtx(ctx)
	defer cancel()
	if _, err := w.db.ExecContext(wctx, `
		UPDATE mailing_segment_build_ledger SET
			last_full_built_at = CASE WHEN $2::text = 'lake-grid-full' THEN NOW() ELSE last_full_built_at END,
			last_snapshot_dt   = NULLIF($3, '')
		WHERE segment_id = $1::uuid
	`, segmentID, source, snapshotDt); err != nil {
		log.Printf("[SegmentGrid] delta-state stamp failed for %s: %v", segmentID, err)
	}
}

// snapshotRecorded reports whether (event, window, dt) completed its UNLOAD.
// Errors read as false (delta-ineligible), never as recorded.
func (w *SegmentGridWorker) snapshotRecorded(ctx context.Context, event string, windowDays int, dt string) bool {
	var one int
	err := w.db.QueryRowContext(ctx, `
		SELECT 1 FROM mailing_segment_grid_snapshots
		WHERE event = $1 AND window_days = $2 AND dt = $3
	`, event, windowDays, dt).Scan(&one)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[SegmentGrid] snapshot-record read failed (%s/%dd/%s): %v", event, windowDays, dt, err)
		}
		return false
	}
	return true
}

// recordSnapshot marks a bucket-day snapshot complete. Best-effort — but a
// failed record means the snapshot will be re-UNLOADed next pass (safe,
// costs one Athena scan).
func (w *SegmentGridWorker) recordSnapshot(ctx context.Context, event string, windowDays int, dt string, pairs int64) {
	wctx, cancel := gridLedgerCtx(ctx)
	defer cancel()
	if _, err := w.db.ExecContext(wctx, `
		INSERT INTO mailing_segment_grid_snapshots (event, window_days, dt, pair_count, completed_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (event, window_days, dt) DO UPDATE SET
			pair_count = EXCLUDED.pair_count, completed_at = NOW()
	`, event, windowDays, dt, pairs); err != nil {
		log.Printf("[SegmentGrid] snapshot record failed (%s/%dd/%s): %v", event, windowDays, dt, err)
	}
}

// pruneSnapshots best-effort removes aged bucket-day snapshots: the S3
// objects for the exact aged-out days (run daily, so deleting a trailing
// 3-day strip keeps up and self-heals one-day gaps) and the PG bookkeeping
// rows past the horizon.
func (w *SegmentGridWorker) pruneSnapshotsDefault(ctx context.Context, event string, windowDays int, todayDt string) {
	today, err := time.Parse("2006-01-02", todayDt)
	if err != nil {
		return
	}
	for back := segmentGridSnapshotPruneAfterDays; back < segmentGridSnapshotPruneAfterDays+3; back++ {
		dt := today.AddDate(0, 0, -back).Format("2006-01-02")
		if err := w.deleteSnapshotFn(ctx, event, windowDays, dt); err != nil {
			log.Printf("[SegmentGrid] snapshot prune %s/%dd/%s: %v", event, windowDays, dt, err)
			break // best-effort; don't hammer a failing S3 path
		}
	}
	horizon := today.AddDate(0, 0, -segmentGridSnapshotPruneAfterDays).Format("2006-01-02")
	wctx, cancel := gridLedgerCtx(ctx)
	defer cancel()
	if _, err := w.db.ExecContext(wctx, `
		DELETE FROM mailing_segment_grid_snapshots WHERE dt < $1
	`, horizon); err != nil {
		log.Printf("[SegmentGrid] snapshot bookkeeping prune: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The merge — ONE transaction, phase 2's entire reason to exist
// ---------------------------------------------------------------------------

// mergeSegmentDelta applies one segment's diff in a single transaction and
// returns the post-merge member count. Order inside the TX:
//
//	advisory xact lock (same key as every other member writer)
//	→ TRUNCATE stage (clears a crashed run's leftovers; transactional)
//	→ COPY removes ('del') + adds ('add') into the stage
//	→ DELETE members matching removes (set-based, PK join)
//	→ INSERT adds ON CONFLICT (segment_id, subscriber_id) DO NOTHING
//	  (materialized_at stamped on the adds ONLY — surviving members keep
//	  their stamps, which is what makes this IO-light)
//	→ recount + restamp mailing_segments cached count
//	→ TRUNCATE stage → COMMIT
//
// A failure ANYWHERE rolls the whole thing back: members are never left
// between states, and the stage's leftover rows are wiped by the next
// merge's opening TRUNCATE.
func (w *SegmentGridWorker) mergeSegmentDelta(ctx context.Context, segmentID string, adds []gridMember, removes []string) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(segmentGridSwapTimeoutMs)*time.Millisecond+time.Minute)
	defer cancel()

	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit

	if _, err := tx.ExecContext(cctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", segmentGridSwapTimeoutMs)); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(cctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, segmentID); err != nil {
		return 0, fmt.Errorf("advisory lock: %w", err)
	}
	if _, err := tx.ExecContext(cctx, `TRUNCATE mailing_segment_grid_stage`); err != nil {
		return 0, fmt.Errorf("truncate stage: %w", err)
	}

	stmt, err := tx.PrepareContext(cctx, pq.CopyIn("mailing_segment_grid_stage", "op", "subscriber_id", "email"))
	if err != nil {
		return 0, fmt.Errorf("prepare copy: %w", err)
	}
	seen := make(map[string]bool, len(adds)+len(removes))
	for _, id := range removes {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := stmt.ExecContext(cctx, "del", id, ""); err != nil {
			_ = stmt.Close()
			return 0, fmt.Errorf("copy remove: %w", err)
		}
	}
	for _, m := range adds {
		if m.SubscriberID == "" || m.Email == "" || seen[m.SubscriberID] {
			continue
		}
		seen[m.SubscriberID] = true
		if _, err := stmt.ExecContext(cctx, "add", m.SubscriberID, m.Email); err != nil {
			_ = stmt.Close()
			return 0, fmt.Errorf("copy add: %w", err)
		}
	}
	if _, err := stmt.ExecContext(cctx); err != nil { // flush
		_ = stmt.Close()
		return 0, fmt.Errorf("flush copy: %w", err)
	}
	if err := stmt.Close(); err != nil {
		return 0, fmt.Errorf("close copy: %w", err)
	}

	if _, err := tx.ExecContext(cctx, `
		DELETE FROM mailing_segment_members m
		USING mailing_segment_grid_stage st
		WHERE st.op = 'del' AND m.segment_id = $1::uuid AND m.subscriber_id = st.subscriber_id
	`, segmentID); err != nil {
		return 0, fmt.Errorf("delete removes: %w", err)
	}
	if _, err := tx.ExecContext(cctx, `
		INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at)
		SELECT $1::uuid, st.subscriber_id, st.email, NOW()
		FROM mailing_segment_grid_stage st
		WHERE st.op = 'add'
		ON CONFLICT (segment_id, subscriber_id) DO NOTHING
	`, segmentID); err != nil {
		return 0, fmt.Errorf("insert adds: %w", err)
	}

	var count int64
	if err := tx.QueryRowContext(cctx, `
		SELECT COUNT(*) FROM mailing_segment_members WHERE segment_id = $1::uuid
	`, segmentID).Scan(&count); err != nil {
		return 0, fmt.Errorf("recount: %w", err)
	}
	if _, err := tx.ExecContext(cctx, `
		UPDATE mailing_segments
		   SET subscriber_count = $1, last_calculated_at = NOW(), updated_at = NOW()
		 WHERE id = $2::uuid
	`, count, segmentID); err != nil {
		return 0, fmt.Errorf("update cached count: %w", err)
	}
	if _, err := tx.ExecContext(cctx, `TRUNCATE mailing_segment_grid_stage`); err != nil {
		return 0, fmt.Errorf("truncate stage (post): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
