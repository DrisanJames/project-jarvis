// ENGAGEMENT-GRID DELTA SNAPSHOTS (phase 2 of the segment system,
// operator-approved 2026-08-21: "That's the architecture we need. And it is
// quick"). Full delete+insert member swaps exhausted RDS EBSIOBalance to 0%
// (5 concurrent 300s+ member inserts, peer-verified in pg_stat_activity), so
// segment builds become DELTA writes:
//
//  1. Each daily pass UNLOADs every needed bucket's distinct
//     (subscriber_id, brand) pairs — the exact set the phase-1 bucket query
//     returns — to a deterministic per-bucket/per-day prefix:
//     s3://<lake-bucket>/segment_snapshots/event=<e>/window_days=<n>/dt=<d>/
//     (Hive-style keys, the lake's events/ convention; Parquet, tiny).
//  2. The next pass diffs today vs the stored base IN ATHENA (EXCEPT
//     anti-joins over the snapshot table) and returns ONLY adds + removes,
//     which the worker merges set-based in ONE Postgres transaction.
//
// The snapshot table (ignite_analytics.segment_grid_snapshots) is an external
// table with PARTITION PROJECTION (event enum / window_days enum / dt date) —
// same no-crawler pattern as email_events. It is created lazily by
// EnsureGridSnapshotTable (idempotent DDL).
//
// This file is the ONE sanctioned S3-write path in the otherwise read-only
// analytics package: UNLOAD output, the pre-UNLOAD prefix cleanup (Athena
// UNLOAD refuses a non-empty location, so a crashed run's partial files must
// be deleted before retry), and aged-snapshot pruning. It still never touches
// Postgres, and every interpolated value is whitelist/format validated.
//
// Degradation contract: when the reader is disabled, the bucket cannot be
// parsed, or any call here fails, callers (worker.SegmentGridWorker) fall
// back to the phase-1 full-swap path — snapshots are an optimization, never
// a correctness dependency. PG membership truth is reconciled by a forced
// full rebuild every 7 days regardless (drift between S3 snapshots and PG is
// inevitable by design: Go-side filters — seeds, created_at,
// exclude_never_clickers — apply to adds only, and snapshot/build timing
// skews by minutes).
package analytics

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	// gridSnapshotTable is the Athena external table over the snapshot
	// prefix. Unqualified — created/queried in the reader's database
	// (ignite_analytics).
	gridSnapshotTable = "segment_grid_snapshots"

	// gridSnapshotPrefix is the S3 key prefix under the lake bucket.
	gridSnapshotPrefix = "segment_snapshots"

	// gridSnapshotDeleteMax bounds the pre-UNLOAD cleanup / prune listing so
	// a pathological prefix can never grind pagination forever. A day's
	// bucket snapshot is a handful of Parquet files; thousands means
	// something upstream is wrong.
	gridSnapshotDeleteMax = 10000
)

// gridSnapshotWindows is the closed set of window partitions the projection
// enum declares. A window outside this set cannot be snapshotted (the
// partition would be invisible to the projection) — callers must use the
// full path for it.
var gridSnapshotWindows = map[int]bool{7: true, 14: true, 30: true, 60: true}

// GridSnapshotWindowOK reports whether windowDays has a snapshot partition.
func GridSnapshotWindowOK(windowDays int) bool { return gridSnapshotWindows[windowDays] }

// GridDelta is one membership change from DiffGridSnapshots.
// Op is "add" (in today, not in base) or "del" (in base, not in today).
type GridDelta struct {
	Op           string
	SubscriberID string
	BrandApex    string
}

// GridSnapshotsSupported reports whether the snapshot layer can operate:
// reader configured AND the lake bucket was parseable from the Athena output
// location (which is where UNLOAD output and the S3 cleanup go).
func GridSnapshotsSupported() bool {
	r := getReader()
	return r != nil && r.s3c != nil && r.bucket != ""
}

// parseS3Bucket extracts the bucket from an "s3://bucket/prefix..." URL.
// Returns "" when the value is not an s3 URL.
func parseS3Bucket(loc string) string {
	rest, ok := strings.CutPrefix(loc, "s3://")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// validateGridSnapshotKey validates the (event, windowDays, dt) partition
// coordinates before any of them reach SQL or an S3 key.
func validateGridSnapshotKey(event string, windowDays int, dt string) error {
	switch event {
	case SegmentEventOpen, SegmentEventClick:
	default:
		return fmt.Errorf("invalid event %q", event)
	}
	if !gridSnapshotWindows[windowDays] {
		return fmt.Errorf("window %d has no snapshot partition (enum 7,14,30,60)", windowDays)
	}
	if !dtRe.MatchString(dt) {
		return fmt.Errorf("invalid dt %q: must be YYYY-MM-DD", dt)
	}
	return nil
}

// gridSnapshotKeyPrefix is the S3 key prefix (no bucket, no scheme) for one
// bucket-day partition. Trailing slash included.
func gridSnapshotKeyPrefix(event string, windowDays int, dt string) string {
	return fmt.Sprintf("%s/event=%s/window_days=%d/dt=%s/", gridSnapshotPrefix, event, windowDays, dt)
}

// buildGridSnapshotTableDDL renders the idempotent external-table DDL.
// Factored out so the shape is unit-testable without Athena.
func buildGridSnapshotTableDDL(bucket string) string {
	loc := "s3://" + bucket + "/" + gridSnapshotPrefix + "/"
	return "CREATE EXTERNAL TABLE IF NOT EXISTS " + gridSnapshotTable +
		" (subscriber_id string, brand string)" +
		" PARTITIONED BY (event string, window_days string, dt string)" +
		" STORED AS PARQUET" +
		" LOCATION '" + loc + "'" +
		" TBLPROPERTIES (" +
		"'projection.enabled'='true'," +
		"'projection.event.type'='enum','projection.event.values'='open,click'," +
		"'projection.window_days.type'='enum','projection.window_days.values'='7,14,30,60'," +
		"'projection.dt.type'='date','projection.dt.range'='2026-08-01,NOW','projection.dt.format'='yyyy-MM-dd'," +
		"'storage.location.template'='" + loc + "event=${event}/window_days=${window_days}/dt=${dt}/'" +
		")"
}

// EnsureGridSnapshotTable creates the snapshot external table if it does not
// exist. Idempotent; requires glue:CreateTable on the task role — on
// permission failure the caller treats snapshots as unsupported this pass.
func (r *Reader) EnsureGridSnapshotTable(ctx context.Context) error {
	_, _, err := r.runQuery(ctx, buildGridSnapshotTableDDL(r.bucket))
	return err
}

// deleteGridSnapshotObjects removes every object under one bucket-day
// partition prefix (pre-UNLOAD cleanup of a crashed run's partial files, and
// the prune path). Bounded by gridSnapshotDeleteMax.
func (r *Reader) deleteGridSnapshotObjects(ctx context.Context, event string, windowDays int, dt string) error {
	if err := validateGridSnapshotKey(event, windowDays, dt); err != nil {
		return err
	}
	prefix := gridSnapshotKeyPrefix(event, windowDays, dt)
	deleted := 0
	var token *string
	for {
		out, err := r.s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(r.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		if len(out.Contents) == 0 {
			return nil
		}
		ids := make([]s3types.ObjectIdentifier, 0, len(out.Contents))
		for _, o := range out.Contents {
			ids = append(ids, s3types.ObjectIdentifier{Key: o.Key})
		}
		if _, err := r.s3c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(r.bucket),
			Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		}); err != nil {
			return fmt.Errorf("delete under %s: %w", prefix, err)
		}
		deleted += len(ids)
		if deleted > gridSnapshotDeleteMax {
			return fmt.Errorf("prefix %s holds more than %d objects — refusing to keep deleting", prefix, gridSnapshotDeleteMax)
		}
		if out.NextContinuationToken == nil {
			return nil
		}
		token = out.NextContinuationToken
	}
}

// buildGridSnapshotUnloadSQL renders the UNLOAD for one bucket-day.
func buildGridSnapshotUnloadSQL(bucket, event string, windowDays int, dt, fromDt string, fromEpochMs int64) (string, error) {
	if err := validateGridSnapshotKey(event, windowDays, dt); err != nil {
		return "", err
	}
	inner, err := buildGridPairsSelect(event, fromDt, dt, fromEpochMs)
	if err != nil {
		return "", err
	}
	return "UNLOAD (" + inner + ") TO 's3://" + bucket + "/" + gridSnapshotKeyPrefix(event, windowDays, dt) +
		"' WITH (format = 'PARQUET')", nil
}

// buildGridSnapshotPartSQL renders the SELECT over one snapshot partition
// (shared by the count and the diff arms).
func buildGridSnapshotPartSQL(event string, windowDays int, dt string) string {
	return "SELECT subscriber_id, brand FROM " + gridSnapshotTable +
		" WHERE event = " + sqlStr(event) +
		" AND window_days = " + sqlStr(strconv.Itoa(windowDays)) +
		" AND dt = " + sqlStr(dt)
}

// buildGridSnapshotDiffSQL renders the two anti-joins (adds + removes) as one
// query. EXCEPT is set semantics (distinct), matching the GROUP BY'd
// snapshot content.
func buildGridSnapshotDiffSQL(event string, windowDays int, baseDt, todayDt string) (string, error) {
	if err := validateGridSnapshotKey(event, windowDays, baseDt); err != nil {
		return "", fmt.Errorf("base: %w", err)
	}
	if err := validateGridSnapshotKey(event, windowDays, todayDt); err != nil {
		return "", fmt.Errorf("today: %w", err)
	}
	if baseDt >= todayDt {
		return "", fmt.Errorf("invalid diff range: base %s is not before today %s", baseDt, todayDt)
	}
	today := buildGridSnapshotPartSQL(event, windowDays, todayDt)
	base := buildGridSnapshotPartSQL(event, windowDays, baseDt)
	return "SELECT 'add' AS op, subscriber_id, brand FROM (" + today + " EXCEPT " + base + ") t" +
		" UNION ALL" +
		" SELECT 'del' AS op, subscriber_id, brand FROM (" + base + " EXCEPT " + today + ") b" +
		" LIMIT " + strconv.Itoa(gridBucketMaxPairs+1), nil
}

// UnloadGridSnapshot writes today's snapshot for one (event, window) bucket
// and returns the pair count it holds. Any pre-existing objects under the
// day prefix (a crashed run's partial UNLOAD) are deleted first — UNLOAD
// refuses a non-empty location, and a partial snapshot silently under-counts,
// which would read as mass removes downstream.
func (r *Reader) UnloadGridSnapshot(ctx context.Context, event string, windowDays int, dt string) (int64, error) {
	windowDays = ClampSegmentWindowDays(windowDays)
	if err := validateGridSnapshotKey(event, windowDays, dt); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -windowDays)
	sql, err := buildGridSnapshotUnloadSQL(r.bucket, event, windowDays, dt, from.Format("2006-01-02"), from.UnixMilli())
	if err != nil {
		return 0, err
	}
	if err := r.deleteGridSnapshotObjects(ctx, event, windowDays, dt); err != nil {
		return 0, fmt.Errorf("pre-unload cleanup: %w", err)
	}
	if _, _, err := r.runQuery(ctx, sql); err != nil {
		return 0, fmt.Errorf("unload: %w", err)
	}
	// Count what actually landed (tiny Parquet scan). This is the snapshot's
	// truth for the PG bookkeeping row; 0 pairs on an active estate is an
	// upstream fault the worker's guards must see.
	countSQL := "SELECT COUNT(*) FROM (" + buildGridSnapshotPartSQL(event, windowDays, dt) + ") s"
	_, rows, err := r.runQuery(ctx, countSQL)
	if err != nil {
		return 0, fmt.Errorf("post-unload count: %w", err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return 0, fmt.Errorf("post-unload count returned no rows")
	}
	n, err := strconv.ParseInt(rows[0][0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("post-unload count parse: %w", err)
	}
	return n, nil
}

// DiffGridSnapshots returns the membership changes between the base and
// today snapshots of one bucket: adds (in today only) and removes (in base
// only), capped at gridBucketMaxPairs.
func (r *Reader) DiffGridSnapshots(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]GridDelta, error) {
	sql, err := buildGridSnapshotDiffSQL(event, windowDays, baseDt, todayDt)
	if err != nil {
		return nil, err
	}
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	if len(rows) > gridBucketMaxPairs {
		return nil, fmt.Errorf("grid diff %s/%dd %s→%s exceeds %d rows — refusing to page an unbounded result",
			event, windowDays, baseDt, todayDt, gridBucketMaxPairs)
	}
	out := make([]GridDelta, 0, len(rows))
	for _, row := range rows {
		if len(row) < 3 || row[1] == "" {
			continue
		}
		out = append(out, GridDelta{Op: row[0], SubscriberID: row[1], BrandApex: row[2]})
	}
	return out, nil
}

// DeleteGridSnapshot removes one bucket-day snapshot's objects (prune path).
func (r *Reader) DeleteGridSnapshot(ctx context.Context, event string, windowDays int, dt string) error {
	return r.deleteGridSnapshotObjects(ctx, event, windowDays, dt)
}

// --- package-level wrappers (errDisabled when the reader is off) ---

// EnsureGridSnapshotTable runs against the global reader.
func EnsureGridSnapshotTable(ctx context.Context) error {
	r := getReader()
	if r == nil || r.s3c == nil || r.bucket == "" {
		return errDisabled
	}
	return r.EnsureGridSnapshotTable(ctx)
}

// UnloadGridSnapshot runs against the global reader.
func UnloadGridSnapshot(ctx context.Context, event string, windowDays int, dt string) (int64, error) {
	r := getReader()
	if r == nil || r.s3c == nil || r.bucket == "" {
		return 0, errDisabled
	}
	return r.UnloadGridSnapshot(ctx, event, windowDays, dt)
}

// DiffGridSnapshots runs against the global reader.
func DiffGridSnapshots(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]GridDelta, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.DiffGridSnapshots(ctx, event, windowDays, baseDt, todayDt)
}

// DeleteGridSnapshot runs against the global reader.
func DeleteGridSnapshot(ctx context.Context, event string, windowDays int, dt string) error {
	r := getReader()
	if r == nil || r.s3c == nil || r.bucket == "" {
		return errDisabled
	}
	return r.deleteGridSnapshotObjects(ctx, event, windowDays, dt)
}
