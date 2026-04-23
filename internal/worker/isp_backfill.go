package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// =============================================================================
// ISP BACKFILL WORKER — Populates mailing_subscribers.isp for the entire table
// =============================================================================
// The PMTA campaign planner's cold-fallback selector stripes per-ISP when
// filling quotas for a new brand. It relies on mailing_subscribers.isp being
// accurate — previously this column was only populated lazily (async, after
// a subscriber was selected for the first time), which left the cold pool
// largely unclassified on the fast path.
//
// This worker eagerly classifies every subscriber whose isp column is empty
// or NULL, using the canonical isp.SQLCaseFromEmail expression so the SQL
// classifier cannot drift from the Go GroupFromDomain classifier. It runs
// once on startup, then hourly so that newly imported subscribers are
// classified within one cycle.
//
// Design notes:
//   - Batch size 50 000 keeps each UPDATE's lock footprint small. On a
//     10 M row table, the initial full backfill completes in ~200 batches.
//   - SKIP LOCKED is not available on UPDATE; instead we use a CTE that
//     selects a bounded set of IDs first, so concurrent writers on other
//     subscribers are not blocked by the backfill.
//   - Idempotent by construction: WHERE isp IS NULL OR isp = '' means a
//     second pass over already-classified rows is a 0-row update.
//   - The backfill is SAFE to run while the planner is selecting audiences.
//     UPDATE sets isp atomically; a concurrent SELECT on the same row will
//     see either the old empty value or the new classification, never a
//     torn read.

const (
	// ispBackfillBatchSize caps each UPDATE's row count. Keeps the lock
	// footprint small enough that concurrent writes to mailing_subscribers
	// (new imports, engagement-score updates) are not blocked for long.
	// Initial value 50k hit the platform-wide Postgres statement_timeout
	// (30s) on first run against a ~M-row unclassified backlog; 10k fits
	// comfortably even when the WHERE isp IS NULL predicate forces a
	// partial-index scan on cold rows.
	ispBackfillBatchSize = 10000

	// ispBackfillInterval is how often the worker re-scans for unclassified
	// rows. Hourly is enough to absorb list imports within one cycle; more
	// frequent would add noise to the query log with no functional benefit.
	ispBackfillInterval = 1 * time.Hour

	// ispBackfillBatchTimeout bounds a single batch UPDATE at the Go layer.
	// The per-statement Postgres timeout is also raised inside runBatch
	// (see SET statement_timeout below) so this context timeout is the
	// outer safety net — if the DB somehow hangs past 5 minutes, the Go
	// context forces the connection closed.
	ispBackfillBatchTimeout = 5 * time.Minute

	// ispBackfillStatementTimeout overrides the platform-wide Postgres
	// statement_timeout (typically 30s for analytics queries) for the
	// scope of the UPDATE only. Set per-connection so no other query is
	// affected. 4 minutes leaves comfortable headroom against the 10k
	// batch typical runtime of a few seconds.
	ispBackfillStatementTimeout = "4min"
)

// ISPBackfillWorker eagerly classifies mailing_subscribers.isp for rows
// that have an empty or NULL isp column, using the canonical SQL CASE
// expression generated from the Go classifier.
type ISPBackfillWorker struct {
	db       *sql.DB
	interval time.Duration
}

// NewISPBackfillWorker constructs the worker with production defaults.
func NewISPBackfillWorker(db *sql.DB) *ISPBackfillWorker {
	return &ISPBackfillWorker{
		db:       db,
		interval: ispBackfillInterval,
	}
}

// Start runs the backfill once immediately, then on a fixed interval until
// ctx is cancelled. Designed to be invoked as a background goroutine from
// server startup.
func (w *ISPBackfillWorker) Start(ctx context.Context) {
	log.Printf("[ISPBackfill] starting (interval=%s, batch=%d)", w.interval, ispBackfillBatchSize)
	if err := w.RunOnce(ctx); err != nil {
		log.Printf("[ISPBackfill] initial pass error: %v", err)
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[ISPBackfill] context cancelled, stopping")
			return
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil {
				log.Printf("[ISPBackfill] pass error: %v", err)
			}
		}
	}
}

// RunOnce executes a complete backfill pass: repeatedly applies one batch
// UPDATE until no more rows are affected, then returns. Exposed for tests
// and for manual invocation during admin/ops work.
//
// Returns the total number of rows updated in this pass.
func (w *ISPBackfillWorker) RunOnce(ctx context.Context) error {
	totalUpdated := 0
	passStart := time.Now()
	batches := 0
	for {
		if ctx.Err() != nil {
			log.Printf("[ISPBackfill] pass cancelled after %d batches / %d rows", batches, totalUpdated)
			return ctx.Err()
		}
		n, err := w.runBatch(ctx)
		if err != nil {
			return fmt.Errorf("isp backfill batch %d: %w", batches, err)
		}
		batches++
		if n == 0 {
			break
		}
		totalUpdated += n
	}
	if totalUpdated > 0 {
		log.Printf("[ISPBackfill] pass complete: %d rows classified in %d batches (%s)",
			totalUpdated, batches, time.Since(passStart))
	}
	return nil
}

// runBatch classifies up to ispBackfillBatchSize subscribers whose isp
// column is empty or NULL. Returns the number of rows affected so the
// caller knows whether another batch is needed.
//
// The UPDATE uses a CTE that selects a bounded set of IDs first, then
// updates only those rows. This keeps the row lock count bounded and lets
// concurrent writes to other mailing_subscribers rows proceed without
// waiting on the backfill.
func (w *ISPBackfillWorker) runBatch(parentCtx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(parentCtx, ispBackfillBatchTimeout)
	defer cancel()

	// isp.SQLCaseFromEmail("email") emits the canonical CASE expression
	// keyed on the mailing_subscribers.email column. It is trusted SQL
	// generated from the Go classifier — never user input.
	caseExpr := isp.SQLCaseFromEmail("email")

	query := fmt.Sprintf(`
		WITH batch AS (
			SELECT id FROM mailing_subscribers
			WHERE isp IS NULL OR isp = ''
			LIMIT %d
			FOR UPDATE SKIP LOCKED
		)
		UPDATE mailing_subscribers s
		SET isp = %s,
		    updated_at = NOW()
		FROM batch
		WHERE s.id = batch.id
		  AND (s.isp IS NULL OR s.isp = '')
	`, ispBackfillBatchSize, caseExpr)

	// Pin a dedicated connection so the statement_timeout override lives
	// only for this UPDATE. The platform-wide default (typically 30s for
	// analytics queries) is too tight for the cold-start case where the
	// CTE has to scan many rows to find batchsize candidates.
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = '%s'", ispBackfillStatementTimeout)); err != nil {
		return 0, fmt.Errorf("set statement_timeout: %w", err)
	}

	res, err := conn.ExecContext(ctx, query)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
