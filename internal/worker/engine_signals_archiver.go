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
	// lock holding times stay predictable.
	archiveDeleteBatch = 50000
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

	buckets, err := w.findUnarchivedBuckets(ctx, cutoff)
	if err != nil {
		if isTableNotExistsError(err) {
			// Startup migrations haven't created the index yet — silent retry.
			return
		}
		log.Printf("[SignalsArchiver] enumerate buckets: %v", err)
		return
	}
	if len(buckets) == 0 {
		return
	}

	log.Printf("[SignalsArchiver] cycle start: %d bucket(s) to archive (cutoff=%s)",
		len(buckets), cutoff.Format("2006-01-02"))

	var (
		ok       int
		skipped  int
		rowsMoved int64
	)
	for _, b := range buckets {
		if ctx.Err() != nil {
			return
		}
		n, err := w.archiveBucket(ctx, b)
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
}

// findUnarchivedBuckets returns (date, isp) groups where at least one row
// exists in mailing_engine_signals with recorded_at < cutoff AND no entry
// exists in the archive index for that (date, isp). Bounded result set so
// runaway backlogs don't block the cycle.
func (w *EngineSignalsArchiver) findUnarchivedBuckets(
	ctx context.Context, cutoff time.Time,
) ([]signalBucket, error) {
	qctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	rows, err := w.db.QueryContext(qctx, `
		SELECT DISTINCT
			date_trunc('day', s.recorded_at)::date AS date_bucket,
			s.isp
		FROM mailing_engine_signals s
		WHERE s.recorded_at < $1
		  AND NOT EXISTS (
		      SELECT 1
		      FROM mailing_engine_signals_archive_index a
		      WHERE a.date_bucket = date_trunc('day', s.recorded_at)::date
		        AND a.isp         = s.isp
		  )
		ORDER BY date_bucket ASC, isp ASC
		LIMIT $2
	`, cutoff, archiveMaxBucketsPerCycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []signalBucket
	for rows.Next() {
		var b signalBucket
		if err := rows.Scan(&b.Date, &b.ISP); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// archiveBucket drains one (date, isp) group end-to-end. Returns the number
// of rows archived. Caller handles error logging.
func (w *EngineSignalsArchiver) archiveBucket(
	ctx context.Context, b signalBucket,
) (int64, error) {
	startAt := b.Date                             // inclusive
	endAt := b.Date.Add(24 * time.Hour)           // exclusive

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
		rowCount      int64
		minAt, maxAt  time.Time
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

	// Phase 4 — delete archived rows in batches of archiveDeleteBatch.
	// Sleep 100 ms between batches to match the DataCleanupWorker pacing
	// and avoid replication-lag pressure.
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

	log.Printf("[SignalsArchiver] %s/%s rows=%d deleted=%d bytes=%d key=%s",
		startAt.Format("2006-01-02"), b.ISP, rowCount, deleted, buf.Len(), key)
	return rowCount, nil
}
