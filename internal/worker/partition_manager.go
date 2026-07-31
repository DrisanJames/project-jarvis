package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// PartitionManager keeps monthly RANGE partitions provisioned AHEAD of now for
// the time-partitioned event tables.
//
// Why this exists (2026-07-31 near-miss): mailing_tracking_events is
// PARTITION BY RANGE (event_at) with NO default partition, and its newest
// partition ended at 2026-08-01. Postgres rejects an insert with no matching
// partition ("no partition of relation found for row"), so at 2026-08-01
// 00:00 UTC every open, click, delivery, bounce, unsub and complaint insert
// would have failed estate-wide. It was caught with ~7h to spare, and only
// because an unrelated investigation happened to list the partitions.
//
// The previous version of this file called create_subscriber_events_partition()
// — a function for a DIFFERENT table — and, more importantly, was NEVER
// STARTED: there were zero callers outside this file. It was dead code
// impersonating a safety net, which is worse than no safety net.
//
// Design notes:
//   - Explicit table list, not "every partitioned parent". The other
//     partitioned tables here (mailing_smart_link_hits,
//     mailing_campaign_plan_recipients_p) carry DEFAULT partitions and cannot
//     suffer this failure; auto-discovery would invent DDL for tables whose
//     conventions have not been verified.
//   - Existence is checked by NAME against the established convention
//     <parent>_YYYY_MM, which every partition already in prod follows.
//   - Runs at boot (synchronously, before workers) AND daily, because a
//     boot-only ensure silently decays on a server that is not redeployed for
//     a few months — the exact way the original gap opened.
type PartitionManager struct {
	db        *sql.DB
	ctx       context.Context
	cancel    context.CancelFunc
	lastRunAt time.Time
	healthy   bool
}

// partitionedEventTables are the RANGE-partitioned parents with no DEFAULT
// partition, where a missing partition is an outage rather than a nuisance.
var partitionedEventTables = []string{
	"mailing_tracking_events",
	"mailing_custom_events",
}

// monthsAhead is how many future months to keep provisioned beyond the
// current one. Three gives 90+ days of runway, so a missed daily tick plus a
// long deploy freeze still cannot reach the edge.
const monthsAhead = 3

func NewPartitionManager(db *sql.DB) *PartitionManager {
	return &PartitionManager{db: db, healthy: true}
}

func (pm *PartitionManager) Start() {
	pm.ctx, pm.cancel = context.WithCancel(context.Background())
	go func() {
		log.Println("[PartitionManager] Starting partition maintenance worker")
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-pm.ctx.Done():
				log.Println("[PartitionManager] Stopped")
				return
			case <-ticker.C:
				pm.runOnce(pm.ctx)
			}
		}
	}()
}

func (pm *PartitionManager) Stop() {
	if pm.cancel != nil {
		pm.cancel()
	}
}

func (pm *PartitionManager) IsHealthy() bool      { return pm.healthy }
func (pm *PartitionManager) LastRunAt() time.Time { return pm.lastRunAt }

// EnsurePartitions provisions the current month plus monthsAhead for every
// table in partitionedEventTables. Idempotent: months that already have a
// partition are skipped, so a steady-state run is a handful of catalog reads
// and no DDL at all. Safe to call from boot and from the daily ticker.
//
// Returns the number of partitions actually created and the first error, if
// any. A partial failure does not stop the remaining tables — one table's
// problem must not deny another table its partition.
func EnsurePartitions(ctx context.Context, db *sql.DB) (int, error) {
	created := 0
	var firstErr error

	// Anchor on UTC: every partition bound in this schema is UTC-based, and a
	// local-time anchor near month end would compute the wrong month.
	base := time.Now().UTC()

	for _, table := range partitionedEventTables {
		for i := 0; i <= monthsAhead; i++ {
			start := time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, i, 0)
			end := start.AddDate(0, 1, 0)
			child := fmt.Sprintf("%s_%04d_%02d", table, start.Year(), int(start.Month()))

			exists, err := partitionExists(ctx, db, table, child)
			if err != nil {
				log.Printf("[PartitionManager] %s: existence check failed: %v", child, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if exists {
				continue
			}

			// Identifiers come from a compile-time table list and integer date
			// parts, so there is no injectable surface; Postgres does not
			// accept parameters for DDL identifiers.
			stmt := fmt.Sprintf(
				`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				child, table,
				start.Format("2006-01-02 15:04:05-07"),
				end.Format("2006-01-02 15:04:05-07"),
			)
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				log.Printf("[PartitionManager] CRITICAL: could not create %s: %v", child, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			created++
			log.Printf("[PartitionManager] created %s [%s, %s)", child,
				start.Format("2006-01-02"), end.Format("2006-01-02"))
		}
	}
	return created, firstErr
}

// partitionExists reports whether child is already attached to parent. The
// lookup is scoped through pg_inherits so an unrelated table that merely
// shares the name cannot mask a genuinely missing partition.
func partitionExists(ctx context.Context, db *sql.DB, parent, child string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT count(*)
		  FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		 WHERE i.inhparent = $1::regclass
		   AND c.relname = $2`, parent, child).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (pm *PartitionManager) runOnce(ctx context.Context) {
	pm.lastRunAt = time.Now()
	created, err := EnsurePartitions(ctx, pm.db)
	if err != nil {
		pm.healthy = false
		log.Printf("[PartitionManager] maintenance completed WITH ERRORS (created %d): %v", created, err)
		return
	}
	pm.healthy = true
	log.Printf("[PartitionManager] maintenance completed (created %d)", created)
}
