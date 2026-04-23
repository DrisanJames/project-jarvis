package engine

// =============================================================================
// SIGNAL ARCHIVE — cold-read helper for mailing_engine_signals
// =============================================================================
// The EngineSignalsArchiver worker (internal/worker/engine_signals_archiver.go)
// moves signals older than 14 days to S3 and records a pointer in
// mailing_engine_signals_archive_index.
//
// This file provides the inverse operation: given an (isp, metric, time-range)
// query, fetch matching signals, transparently merging hot rows from Postgres
// with cold rows unpacked from S3.
//
// Intended callers are analytics and forensic endpoints. The hot path
// (rule evaluation, throttle decisions) must NEVER touch this — it always
// has what it needs within the 14-day hot window.

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// SignalArchive is the façade used by analytics to fetch signals across the
// hot/cold boundary. Construct once at server start and share across
// handlers — it is safe for concurrent use.
type SignalArchive struct {
	db  *sql.DB
	s3c *s3.Client

	// maxParallelDownloads bounds concurrency to protect Postgres and S3
	// both. 4 has proven sufficient for month-wide queries; override via
	// NewSignalArchiveWithConcurrency if larger history reads are needed.
	maxParallelDownloads int
}

// NewSignalArchive constructs the helper with sensible defaults.
func NewSignalArchive(db *sql.DB, s3c *s3.Client) *SignalArchive {
	return &SignalArchive{
		db:                   db,
		s3c:                  s3c,
		maxParallelDownloads: 4,
	}
}

// NewSignalArchiveWithConcurrency allows callers to tune download parallelism.
func NewSignalArchiveWithConcurrency(db *sql.DB, s3c *s3.Client, parallel int) *SignalArchive {
	if parallel < 1 {
		parallel = 1
	}
	return &SignalArchive{db: db, s3c: s3c, maxParallelDownloads: parallel}
}

// archivePointer mirrors mailing_engine_signals_archive_index.
type archivePointer struct {
	S3Bucket      string
	S3Key         string
	RowCount      int64
	MinRecordedAt time.Time
	MaxRecordedAt time.Time
	Format        string
}

// FetchSignals returns all signals matching (isp, metricName) whose
// recorded_at falls in [from, to). metricName may be the empty string to
// match all metrics. Hot rows from Postgres are always included; cold rows
// from S3 are included only when the query window extends past the
// 14-day hot boundary.
//
// Results are sorted by RecordedAt ASC. The function is safe for concurrent
// use; each call owns its own goroutines and HTTP connections.
func (a *SignalArchive) FetchSignals(
	ctx context.Context,
	isp ISP,
	metricName string,
	from, to time.Time,
) ([]Signal, error) {
	if a.db == nil {
		return nil, fmt.Errorf("signal archive: db not configured")
	}
	if !to.After(from) {
		return nil, fmt.Errorf("signal archive: invalid range from=%s to=%s", from, to)
	}

	hot, err := a.fetchHot(ctx, isp, metricName, from, to)
	if err != nil {
		return nil, fmt.Errorf("fetch hot: %w", err)
	}

	cold, err := a.fetchCold(ctx, isp, metricName, from, to)
	if err != nil {
		// Cold fetch failure should not hide hot results — log upstream.
		return hot, fmt.Errorf("fetch cold: %w", err)
	}

	merged := append(cold, hot...)
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].RecordedAt.Before(merged[j].RecordedAt)
	})
	return merged, nil
}

// fetchHot reads live rows from mailing_engine_signals.
func (a *SignalArchive) fetchHot(
	ctx context.Context, isp ISP, metric string, from, to time.Time,
) ([]Signal, error) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var (
		rows *sql.Rows
		err  error
	)
	if metric == "" {
		rows, err = a.db.QueryContext(qctx, `
			SELECT id, organization_id, isp, metric_name, dimension_type, dimension_value,
			       value, window_seconds, sample_count, recorded_at
			FROM mailing_engine_signals
			WHERE isp = $1 AND recorded_at >= $2 AND recorded_at < $3
			ORDER BY recorded_at ASC
		`, string(isp), from, to)
	} else {
		rows, err = a.db.QueryContext(qctx, `
			SELECT id, organization_id, isp, metric_name, dimension_type, dimension_value,
			       value, window_seconds, sample_count, recorded_at
			FROM mailing_engine_signals
			WHERE isp = $1 AND metric_name = $2
			  AND recorded_at >= $3 AND recorded_at < $4
			ORDER BY recorded_at ASC
		`, string(isp), metric, from, to)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Signal
	for rows.Next() {
		var s Signal
		var ispStr string
		if err := rows.Scan(
			&s.ID, &s.OrganizationID, &ispStr, &s.MetricName,
			&s.DimensionType, &s.DimensionValue, &s.Value,
			&s.WindowSeconds, &s.SampleCount, &s.RecordedAt,
		); err != nil {
			return nil, err
		}
		s.ISP = ISP(ispStr)
		out = append(out, s)
	}
	return out, rows.Err()
}

// fetchCold looks up S3 pointers whose [min,max] range overlaps [from, to),
// downloads them in parallel, and returns matching signals.
func (a *SignalArchive) fetchCold(
	ctx context.Context, isp ISP, metric string, from, to time.Time,
) ([]Signal, error) {
	if a.s3c == nil {
		return nil, nil // cold disabled — pretend there's nothing
	}

	pointers, err := a.lookupPointers(ctx, isp, from, to)
	if err != nil {
		return nil, err
	}
	if len(pointers) == 0 {
		return nil, nil
	}

	var (
		mu      sync.Mutex
		results []Signal
		wg      sync.WaitGroup
		errCh   = make(chan error, len(pointers))
		sem     = make(chan struct{}, a.maxParallelDownloads)
	)

	for _, p := range pointers {
		if p.Format == "empty" || p.RowCount == 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ptr archivePointer) {
			defer wg.Done()
			defer func() { <-sem }()

			signals, err := a.downloadAndFilter(ctx, ptr, metric, from, to)
			if err != nil {
				errCh <- fmt.Errorf("s3://%s/%s: %w", ptr.S3Bucket, ptr.S3Key, err)
				return
			}
			mu.Lock()
			results = append(results, signals...)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	close(errCh)

	// Surface the first error but still return whatever we successfully
	// collected — callers choosing to fail closed can check len(results).
	for e := range errCh {
		return results, e
	}
	return results, nil
}

// lookupPointers queries the index for archive files that could contain
// rows in [from, to) for the given isp. We use the simple rule:
// "an archive object is relevant if its [min,max] range intersects [from,to)".
func (a *SignalArchive) lookupPointers(
	ctx context.Context, isp ISP, from, to time.Time,
) ([]archivePointer, error) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := a.db.QueryContext(qctx, `
		SELECT s3_bucket, s3_key, row_count,
		       min_recorded_at, max_recorded_at, format
		FROM mailing_engine_signals_archive_index
		WHERE isp = $1
		  AND max_recorded_at >= $2
		  AND min_recorded_at <  $3
		ORDER BY min_recorded_at ASC
	`, string(isp), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []archivePointer
	for rows.Next() {
		var p archivePointer
		if err := rows.Scan(
			&p.S3Bucket, &p.S3Key, &p.RowCount,
			&p.MinRecordedAt, &p.MaxRecordedAt, &p.Format,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// downloadAndFilter pulls one S3 object, decompresses it, and returns
// signals that match the metric filter and fall in [from, to).
func (a *SignalArchive) downloadAndFilter(
	ctx context.Context, p archivePointer,
	metric string, from, to time.Time,
) ([]Signal, error) {
	getCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	obj, err := a.s3c.GetObject(getCtx, &s3.GetObjectInput{
		Bucket: aws.String(p.S3Bucket),
		Key:    aws.String(p.S3Key),
	})
	if err != nil {
		return nil, err
	}
	defer obj.Body.Close()

	var reader io.Reader = obj.Body
	if p.Format == "jsonl.gz" {
		gz, err := gzip.NewReader(obj.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		reader = gz
	}

	dec := json.NewDecoder(reader)
	var out []Signal
	for {
		var s Signal
		var raw struct {
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
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return out, fmt.Errorf("decode: %w", err)
		}
		// Metric and time filters are cheap; apply before appending to
		// keep the result slice tight.
		if metric != "" && raw.MetricName != metric {
			continue
		}
		if raw.RecordedAt.Before(from) || !raw.RecordedAt.Before(to) {
			continue
		}
		s.ID = raw.ID
		s.OrganizationID = raw.OrganizationID
		s.ISP = ISP(raw.ISP)
		s.MetricName = raw.MetricName
		s.DimensionType = raw.DimensionType
		s.DimensionValue = raw.DimensionValue
		s.Value = raw.Value
		s.WindowSeconds = raw.WindowSeconds
		s.SampleCount = raw.SampleCount
		s.RecordedAt = raw.RecordedAt
		out = append(out, s)
	}
	return out, nil
}
