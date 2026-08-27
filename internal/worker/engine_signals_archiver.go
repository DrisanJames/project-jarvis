package worker

// =============================================================================
// ENGINE SIGNALS ARCHIVER — 14d hot in Postgres, cold in S3
// =============================================================================
// mailing_engine_signals grows at ~22 K rows/hour (~16 M rows/month). The
// ThrottleAgent only queries the trailing 3600 s, so anything older is dead
// weight in the primary database. This worker:
//
//   1. Finds (date_bucket, isp) groups older than 14 days that are not yet
//      archived (checked against mailing_engine_signals_archive_index).
//   2. Streams their rows, writes them as gzipped JSONL to
//      s3://$ENGINE_S3_BUCKET/engine-signals/dt=YYYY-MM-DD/isp=<isp>/part-<uuid>.jsonl.gz
//   3. Inserts a pointer row into mailing_engine_signals_archive_index.
//   4. Deletes the now-archived rows from mailing_engine_signals in batches.
//
// Crash safety properties:
//   - S3 PUT is idempotent per (uuid) key; if a crash happens between PUT and
//     index INSERT, the orphan object is cheap and the next cycle re-archives
//     under a new key. Garbage collection of orphan S3 keys is cheap and can
//     be added later if needed.
//   - Index INSERT happens BEFORE any DELETE. If we crash between INSERT and
//     DELETE, the bucket is marked archived; the residual rows in Postgres
//     will be noticed by the DELETE loop on the next cycle (since archive
//     re-detection is keyed off row presence, we re-run the DELETE phase
//     when it sees matching rows plus an existing index row for the same
//     bucket).
//   - All DB operations use bounded context timeouts so a stuck query
//     cannot block graceful shutdown indefinitely.
//
// Written April 2026 as part of Phase 1 storage maintenance.

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

const (
	// archiveHotDays is how many days of signals remain in Postgres.
	// The ThrottleAgent's widest window is 3600 s, so 14 days is ~336×
	// the hot-read requirement — ample headroom for ad-hoc analytics.
	archiveHotDays = 14

	// archiveInterval controls how often the worker runs. 6 h means a
	// single missed cycle still catches up the same day.
	archiveInterval = 6 * time.Hour

	// archiveStartupDelay gives startup migrations and the ingestor time
	// to stabilize before we begin scanning.
	archiveStartupDelay = 2 * time.Minute

	// archiveMaxBucketsPerCycle caps how many (date, isp) groups we handle
	// per cycle so an unexpectedly large backlog cannot monopolize the
	// worker. The Python backfill script handles historical drain.
	archiveMaxBucketsPerCycle = 40

	// archiveDeleteBatch mirrors DataCleanupWorker.cleanupBatchSize so DB
	// lock holding times stay predictable. Lowered from 50000 to 20000 on
	// 2026-06-07: 50k DELETEs (6 indexes to maintain per row) consistently
	// blew the 60s per-batch timeout under concurrent send-day / segment-
	// refresh IO; 20k completes within budget while still draining a full
	// day-bucket in a handful of batches.
	archiveDeleteBatch = 20000
)

// EngineSignalsArchiver moves aged signals to S3 and trims the hot table.
type EngineSignalsArchiver struct {
	db       *sql.DB
	s3c      *s3.Client
	bucket   string
	interval time.Duration
}

// NewEngineSignalsArchiver constructs the worker. If bucket is empty the
// worker refuses to start and logs a diagnostic — this keeps the rest of
// the server healthy when S3 is not configured in a given environment.
func NewEngineSignalsArchiver(db *sql.DB, s3c *s3.Client, bucket string) *EngineSignalsArchiver {
	return &EngineSignalsArchiver{
		db:       db,
		s3c:      s3c,
		bucket:   bucket,
		interval: archiveInterval,
	}
}

// Start runs the worker loop until ctx is cancelled.
func (w *EngineSignalsArchiver) Start(ctx context.Context) {
	if w.bucket == "" || w.s3c == nil || w.db == nil {
		log.Println("[SignalsArchiver] disabled (missing bucket, s3 client, or db)")
		return
	}
	log.Printf("[SignalsArchiver] Starting (hot=%dd, interval=%s, bucket=%s)",
		archiveHotDays, w.interval, w.bucket)

	// Pause briefly on boot so runStartupMigrations() has time to create
	// the index table before we try to write to it.
	select {
	case <-ctx.Done():
		return
	case <-time.After(archiveStartupDelay):
	}
	w.runOnce(ctx)

	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[SignalsArchiver] Stopping")
			return
		case <-t.C:
			w.runOnce(ctx)
		}
	}
}

func (w *EngineSignalsArchiver) runOnce(ctx context.Context) {
	start := time.Now()
	cutoff := time.Now().UTC().Add(-archiveHotDays * 24 * time.Hour)

	// Heartbeat at end of every cycle (including healthy no-op cycles) so a
	// real stall is distinguishable from "nothing to archive right now".
	hbStatus, hbErr := "ok", ""
	defer func() {
		EmitHeartbeat(ctx, w.db, "engine_signals_archiver", int(w.interval.Seconds()), hbStatus, hbErr)
	}()

	buckets, err := w.findUnarchivedBuckets(ctx, cutoff)
	if err != nil {
		if isTableNotExistsError(err) {
			// Startup migrations haven't created the index yet — silent retry.
			return
		}
		hbStatus, hbErr = "error", err.Error()
		log.Printf("[SignalsArchiver] enumerate buckets: %v", err)
		return
	}
	if len(buckets) == 0 {
		return
	}

	log.Printf("[SignalsArchiver] cycle start: %d bucket(s) to archive (cutoff=%s)",
		len(buckets), cutoff.Format("2006-01-02"))

	var (
		ok        int
		skipped   int
		rowsMoved int64
	)
	for _, b := range buckets {
		if ctx.Err() != nil {
			return
		}
		var (
			n   int64
			err error
		)
		if b.DeleteOnly {
			// Rows already in S3 from an interrupted prior cycle (index row
			// present, Phase-4 DELETE never finished). Skip re-archival and
			// just drain them from the hot table. This is what un-wedges a
			// backlog whose oldest day is fully archived but never deleted.
			n, err = w.deleteBucketRows(ctx, b)
			if err == nil && n > 0 {
				log.Printf("[SignalsArchiver] %s/%s drained %d orphan rows (already archived)",
					b.Date.Format("2006-01-02"), b.ISP, n)
			}
		} else {
			n, err = w.archiveBucket(ctx, b)
		}
		if err != nil {
			log.Printf("[SignalsArchiver] %s/%s FAILED: %v",
				b.Date.Format("2006-01-02"), b.ISP, err)
			skipped++
			continue
		}
		ok++
		rowsMoved += n
	}
	log.Printf("[SignalsArchiver] cycle done: ok=%d skipped=%d rows=%d elapsed=%s",
		ok, skipped, rowsMoved, time.Since(start).Round(time.Second))
}

type signalBucket struct {
	Date time.Time
	ISP  string
	// DeleteOnly marks a bucket whose rows are already present in
	// mailing_engine_signals_archive_index (a prior cycle archived to S3 but
	// its Phase-4 DELETE was interrupted, leaving the rows in the hot table).
	// For these we skip S3 archival and only run the delete loop. This is the
	// fix for the 2026-04-06..08 wedge: ~2.8M rows were archived but never
	// deleted, and the old anti-join enumerate timed out re-scanning them every
	// cycle, freezing the archive index for ~2 months.
	DeleteOnly bool
}

// findUnarchivedBuckets returns (date, isp) groups where at least one row
// exists in mailing_engine_signals with recorded_at < cutoff AND no entry
// exists in the archive index for that (date, isp). Bounded result set so
// runaway backlogs don't block the cycle.
//
// IMPLEMENTATION NOTE (rewritten 2026-06-04 after a ~2-month stall):
// The previous implementation ran a single `SELECT DISTINCT date_trunc(...),
// isp ... WHERE recorded_at < cutoff ... ORDER BY ... LIMIT 40` over the whole
// table. Because `recorded_at < cutoff` matches the overwhelming majority of
// rows once a backlog forms (45+ unarchived days × ~22K rows/hour), Postgres
// had to hash-aggregate tens of millions of rows BEFORE the LIMIT could apply,
// blowing past the 90s timeout every cycle. The worker logged "enumerate
// buckets" and made zero progress for ~2 months (archive index frozen at
// 2026-04-08; mailing_engine_signals grew to 54.7M rows / 13 GB).
//
// The fix walks forward one UTC day at a time starting from the oldest row
// still in the hot table. Each per-day `DISTINCT isp` is bounded by the
// recorded_at range index (idx_engine_signals_recorded) and scans ~one day of
// rows instead of the entire table, so it returns in well under the per-day
// timeout even under send-day IO pressure. We advance `day` unconditionally so
// a crash-gap day (index row present but rows not yet deleted) can never wedge
// the walk — later days still get processed.
func (w *EngineSignalsArchiver) findUnarchivedBuckets(
	ctx context.Context, cutoff time.Time,
) ([]signalBucket, error) {
	// Oldest row still in the hot table. min() on a btree(recorded_at) index
	// is a cheap backward index scan, fast even on a 50M+ row table.
	minCtx, minCancel := context.WithTimeout(ctx, 30*time.Second)
	var oldest sql.NullTime
	err := w.db.QueryRowContext(minCtx,
		`SELECT min(recorded_at) FROM mailing_engine_signals`).Scan(&oldest)
	minCancel()
	if err != nil {
		return nil, fmt.Errorf("min recorded_at: %w", err)
	}
	if !oldest.Valid {
		return nil, nil // empty table
	}

	day := oldest.Time.UTC().Truncate(24 * time.Hour)
	cutoffDay := cutoff.UTC().Truncate(24 * time.Hour)

	var out []signalBucket
	for day.Before(cutoffDay) && len(out) < archiveMaxBucketsPerCycle {
		if ctx.Err() != nil {
			return out, nil
		}
		next := day.Add(24 * time.Hour)

		// Which isps for this day are already in the archive index. The index
		// table is tiny (one row per (day, isp)) so this is a cheap lookup. We
		// use it to classify each hot-table isp below as either DeleteOnly (an
		// interrupted-Phase-4 orphan) or archive-and-delete (genuinely new).
		// This REPLACES the previous `NOT EXISTS` correlated anti-join, which
		// had to heap-scan the entire day to evaluate the subquery per row and
		// timed out for ~2 months once a backlog formed (archive index frozen
		// at 2026-04-08; ~862K-974K orphan rows/day wedged the walk on the
		// oldest day, which the walk re-derives from min() every cycle).
		archived := map[string]bool{}
		actx, acancel := context.WithTimeout(ctx, 20*time.Second)
		arows, aerr := w.db.QueryContext(actx, `
			SELECT isp FROM mailing_engine_signals_archive_index WHERE date_bucket = $1::date
		`, day)
		if aerr != nil {
			acancel()
			return out, fmt.Errorf("archived isps for %s: %w", day.Format("2006-01-02"), aerr)
		}
		for arows.Next() {
			var isp string
			if arows.Scan(&isp) == nil {
				archived[isp] = true
			}
		}
		rerrA := arows.Err()
		arows.Close()
		acancel()
		if rerrA != nil {
			return out, rerrA
		}

		// Distinct isps that still have rows in the hot table for this day.
		// With idx_engine_signals_recorded_isp (recorded_at, isp) this is an
		// index-only scan of a single day's slice — fast even on a 50M+ row
		// table and under send-day IO, so it can no longer wedge the walk. We
		// advance `day` unconditionally after this, so even a day that returns
		// only DeleteOnly buckets keeps the walk moving.
		dctx, dcancel := context.WithTimeout(ctx, 90*time.Second)
		rows, qerr := w.db.QueryContext(dctx, `
			SELECT DISTINCT isp
			FROM mailing_engine_signals
			WHERE recorded_at >= $1 AND recorded_at < $2
			ORDER BY isp ASC
		`, day, next)
		if qerr != nil {
			dcancel()
			// Surface the day so the operator can see exactly where it choked,
			// then return what we have so far rather than failing the cycle.
			return out, fmt.Errorf("enumerate day %s: %w", day.Format("2006-01-02"), qerr)
		}
		for rows.Next() {
			var isp string
			if err := rows.Scan(&isp); err != nil {
				rows.Close()
				dcancel()
				return out, err
			}
			out = append(out, signalBucket{Date: day, ISP: isp, DeleteOnly: archived[isp]})
			if len(out) >= archiveMaxBucketsPerCycle {
				break
			}
		}
		rerr := rows.Err()
		rows.Close()
		dcancel()
		if rerr != nil {
			return out, rerr
		}
		day = next
	}
	return out, nil
}

// archiveBucket drains one (date, isp) group end-to-end. Returns the number
// of rows archived. Caller handles error logging.
func (w *EngineSignalsArchiver) archiveBucket(
	ctx context.Context, b signalBucket,
) (int64, error) {
	startAt := b.Date                   // inclusive
	endAt := b.Date.Add(24 * time.Hour) // exclusive

	// Phase 1 — stream rows out of Postgres into a gzipped JSONL buffer.
	// We hold the buffer in memory; measured 200K rows compresses to
	// ~5–8 MB so peak memory per bucket stays under 50 MB.
	qctx, qcancel := context.WithTimeout(ctx, 10*time.Minute)
	defer qcancel()

	rows, err := w.db.QueryContext(qctx, `
		SELECT id, organization_id, isp, metric_name, dimension_type, dimension_value,
		       value, window_seconds, sample_count, recorded_at
		FROM mailing_engine_signals
		WHERE recorded_at >= $1 AND recorded_at < $2 AND isp = $3
		ORDER BY recorded_at ASC
	`, startAt, endAt, b.ISP)
	if err != nil {
		return 0, fmt.Errorf("query rows: %w", err)
	}
	defer rows.Close()

	var buf bytes.Buffer
	gz, gerr := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if gerr != nil {
		return 0, fmt.Errorf("gzip writer: %w", gerr)
	}
	enc := json.NewEncoder(gz)

	type signalRow struct {
		ID             string    `json:"id"`
		OrganizationID string    `json:"organization_id"`
		ISP            string    `json:"isp"`
		MetricName     string    `json:"metric_name"`
		DimensionType  string    `json:"dimension_type"`
		DimensionValue string    `json:"dimension_value"`
		Value          float64   `json:"value"`
		WindowSeconds  int       `json:"window_seconds"`
		SampleCount    int       `json:"sample_count"`
		RecordedAt     time.Time `json:"recorded_at"`
	}

	var (
		rowCount     int64
		minAt, maxAt time.Time
	)
	for rows.Next() {
		var r signalRow
		if err := rows.Scan(
			&r.ID, &r.OrganizationID, &r.ISP, &r.MetricName,
			&r.DimensionType, &r.DimensionValue, &r.Value,
			&r.WindowSeconds, &r.SampleCount, &r.RecordedAt,
		); err != nil {
			return 0, fmt.Errorf("scan: %w", err)
		}
		if err := enc.Encode(&r); err != nil {
			return 0, fmt.Errorf("encode: %w", err)
		}
		if rowCount == 0 {
			minAt = r.RecordedAt
		}
		maxAt = r.RecordedAt
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rows err: %w", err)
	}
	if err := gz.Close(); err != nil {
		return 0, fmt.Errorf("gzip close: %w", err)
	}

	// Nothing to archive for this bucket — write a zero-row marker so the
	// bucket becomes ineligible on subsequent cycles and we don't waste a
	// SELECT on every run.
	if rowCount == 0 {
		markerKey := fmt.Sprintf("engine-signals/dt=%s/isp=%s/EMPTY",
			startAt.Format("2006-01-02"), b.ISP)
		_, err := w.db.ExecContext(ctx, `
			INSERT INTO mailing_engine_signals_archive_index
			  (date_bucket, isp, s3_bucket, s3_key, row_count,
			   min_recorded_at, max_recorded_at, compressed_bytes, format)
			VALUES ($1,$2,$3,$4,0,$5,$5,0,'empty')
			ON CONFLICT (s3_key) DO NOTHING
		`, startAt, b.ISP, w.bucket, markerKey, startAt)
		if err != nil {
			return 0, fmt.Errorf("insert empty marker: %w", err)
		}
		return 0, nil
	}

	// Phase 2 — upload to S3. Key includes a uuid so retries never clobber
	// a prior successful upload referenced by the index.
	key := fmt.Sprintf("engine-signals/dt=%s/isp=%s/part-%s.jsonl.gz",
		startAt.Format("2006-01-02"), b.ISP, uuid.NewString())

	uctx, ucancel := context.WithTimeout(ctx, 5*time.Minute)
	defer ucancel()

	_, err = w.s3c.PutObject(uctx, &s3.PutObjectInput{
		Bucket:          aws.String(w.bucket),
		Key:             aws.String(key),
		Body:            bytes.NewReader(buf.Bytes()),
		ContentType:     aws.String("application/x-ndjson"),
		ContentEncoding: aws.String("gzip"),
		Metadata: map[string]string{
			"row-count":    fmt.Sprintf("%d", rowCount),
			"date-bucket":  startAt.Format("2006-01-02"),
			"isp":          b.ISP,
			"min-recorded": minAt.UTC().Format(time.RFC3339),
			"max-recorded": maxAt.UTC().Format(time.RFC3339),
			"source":       "engine_signals_archiver",
		},
	})
	if err != nil {
		return 0, fmt.Errorf("s3 put %s: %w", key, err)
	}

	// Phase 3 — index row. If this fails, the S3 object is orphaned; the
	// next cycle re-archives under a new key. Operationally harmless.
	if _, err := w.db.ExecContext(ctx, `
		INSERT INTO mailing_engine_signals_archive_index
		  (date_bucket, isp, s3_bucket, s3_key, row_count,
		   min_recorded_at, max_recorded_at, compressed_bytes, format)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'jsonl.gz')
		ON CONFLICT (s3_key) DO NOTHING
	`, startAt, b.ISP, w.bucket, key, rowCount, minAt, maxAt, int64(buf.Len())); err != nil {
		return 0, fmt.Errorf("insert index (orphan s3://%s/%s): %w", w.bucket, key, err)
	}

	// Phase 4 — delete archived rows in batches.
	deleted, derr := w.deleteBucketRows(ctx, b)
	if derr != nil {
		return deleted, derr
	}

	log.Printf("[SignalsArchiver] %s/%s rows=%d deleted=%d bytes=%d key=%s",
		startAt.Format("2006-01-02"), b.ISP, rowCount, deleted, buf.Len(), key)
	return rowCount, nil
}

// deleteBucketRows removes every hot-table row for one (date, isp) bucket in
// bounded batches of archiveDeleteBatch, sleeping 100 ms between batches to
// match DataCleanupWorker pacing and avoid replication-lag pressure. It is used
// both as archiveBucket's Phase 4 and standalone for DeleteOnly buckets whose
// rows are already safely in S3 (interrupted prior cycle). Returns the number
// of rows deleted.
func (w *EngineSignalsArchiver) deleteBucketRows(ctx context.Context, b signalBucket) (int64, error) {
	startAt := b.Date                   // inclusive
	endAt := b.Date.Add(24 * time.Hour) // exclusive

	var deleted int64
	for {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		dctx, dcancel := context.WithTimeout(ctx, 60*time.Second)
		res, err := w.db.ExecContext(dctx, `
			DELETE FROM mailing_engine_signals
			WHERE id IN (
			  SELECT id FROM mailing_engine_signals
			  WHERE recorded_at >= $1 AND recorded_at < $2 AND isp = $3
			  LIMIT $4
			)
		`, startAt, endAt, b.ISP, archiveDeleteBatch)
		dcancel()
		if err != nil {
			return deleted, fmt.Errorf("delete batch: %w", err)
		}
		n, _ := res.RowsAffected()
		deleted += n
		if n == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return deleted, nil
}
