package worker

// BrainSnapshotWorker (governance, operator 2026-09-13: the brain is the
// company's institutional memory and apex-postgres backup retention is 3 days
// by a deliberate cost decision — the brain therefore owns its OWN durability).
//
// Once a day, under a distlock lease, every jarvis_brain_* table is exported
// as JSONL to S3 under <prefix>/<UTC day>/<table>.jsonl plus a manifest.json
// (row counts, git sha, exported_at). The bucket (default ENGINE_S3_BUCKET =
// ignite-pmta-engine: private, versioning ENABLED — verified 2026-09-13) keeps
// every version; this worker NEVER deletes. The public jarvis-image-cdn bucket
// (JARVIS_S3_BUCKET) is deliberately NOT the default.
//
// Restore (one table, one day):
//   aws s3 cp s3://<bucket>/<prefix>/<day>/jarvis_brain_claims.jsonl - \
//     | psql "$DATABASE_URL" -c "\copy jarvis_brain_claims_restore FROM STDIN" (after
//     CREATE TABLE jarvis_brain_claims_restore (doc jsonb); then INSERT ... SELECT
//     from the json). Row order in the files is by id so FK targets precede users.
//
// Kill switch: BRAIN_SNAPSHOT_DISABLED. Heartbeat: mailing_worker_heartbeats
// 'brain_snapshot'. Env: BRAIN_SNAPSHOT_BUCKET (default ENGINE_S3_BUCKET),
// BRAIN_SNAPSHOT_PREFIX (default brain-snapshots), BRAIN_SNAPSHOT_REGION
// (default JARVIS_S3_REGION, then us-west-2).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/buildinfo"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

const (
	brainSnapshotWorkerName    = "brain_snapshot"
	brainSnapshotLockKey       = "brain_snapshot"
	DefaultBrainSnapshotPeriod = 24 * time.Hour
	brainSnapshotDefaultPrefix = "brain-snapshots"
	brainSnapshotMaxRowsPerTbl = 2_000_000
)

// brainSnapshotTables is the closed list, in FK order (claims before evidence
// and evals; evals before runs) so a restore can replay them in file order.
var brainSnapshotTables = []string{
	"jarvis_brain_claims",
	"jarvis_brain_evidence",
	"jarvis_brain_capabilities",
	"jarvis_brain_evals",
	"jarvis_brain_eval_runs",
}

// BrainSnapshotStore is the seam over S3 (tests inject a fake).
type BrainSnapshotStore interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
}

type s3BrainSnapshotStore struct {
	client *s3.Client
	bucket string
}

func (s *s3BrainSnapshotStore) Put(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
	})
	return err
}

func brainSnapshotDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BRAIN_SNAPSHOT_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

type BrainSnapshotWorker struct {
	db       *sql.DB
	redis    *redis.Client
	interval time.Duration
	bucket   string
	prefix   string
	region   string
	store    BrainSnapshotStore
	now      func() time.Time
}

func NewBrainSnapshotWorker(db *sql.DB, redisClient *redis.Client) *BrainSnapshotWorker {
	bucket := strings.TrimSpace(os.Getenv("BRAIN_SNAPSHOT_BUCKET"))
	if bucket == "" {
		bucket = strings.TrimSpace(os.Getenv("ENGINE_S3_BUCKET"))
	}
	prefix := strings.Trim(strings.TrimSpace(os.Getenv("BRAIN_SNAPSHOT_PREFIX")), "/")
	if prefix == "" {
		prefix = brainSnapshotDefaultPrefix
	}
	region := strings.TrimSpace(os.Getenv("BRAIN_SNAPSHOT_REGION"))
	if region == "" {
		region = strings.TrimSpace(os.Getenv("JARVIS_S3_REGION"))
	}
	if region == "" {
		region = "us-west-2"
	}
	return &BrainSnapshotWorker{db: db, redis: redisClient, interval: DefaultBrainSnapshotPeriod,
		bucket: bucket, prefix: prefix, region: region, now: time.Now}
}

// SetStore injects a store (tests). Call before Start.
func (w *BrainSnapshotWorker) SetStore(s BrainSnapshotStore) *BrainSnapshotWorker {
	w.store = s
	return w
}

func (w *BrainSnapshotWorker) WithInterval(d time.Duration) *BrainSnapshotWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

func (w *BrainSnapshotWorker) ensureStore(ctx context.Context) (BrainSnapshotStore, error) {
	if w.store != nil {
		return w.store, nil
	}
	if w.bucket == "" {
		return nil, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(w.region))
	if err != nil {
		return nil, fmt.Errorf("aws config (region=%s): %w", w.region, err)
	}
	w.store = &s3BrainSnapshotStore{client: s3.NewFromConfig(cfg), bucket: w.bucket}
	return w.store, nil
}

func (w *BrainSnapshotWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[BrainSnapshot] disabled (db missing)")
		return
	}
	go func() {
		log.Printf("Brain snapshot worker started (interval=%s, bucket=%q, prefix=%s)", w.interval, w.bucket, w.prefix)
		w.tick(ctx)
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
			}
		}
	}()
}

func (w *BrainSnapshotWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if brainSnapshotDisabled() {
		EmitHeartbeat(ctx, w.db, brainSnapshotWorkerName, int(w.interval.Seconds()), "disabled", "BRAIN_SNAPSHOT_DISABLED set")
		return
	}
	lock := distlock.NewLock(w.redis, w.db, brainSnapshotLockKey, 30*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[BrainSnapshot] lock acquire error: %v", err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[BrainSnapshot] lock release error: %v", err)
		}
	}()
	if _, err := w.RunOnce(ctx); err != nil {
		log.Printf("[BrainSnapshot] %v", err)
		EmitHeartbeat(ctx, w.db, brainSnapshotWorkerName, int(w.interval.Seconds()), "error", err.Error())
		return
	}
	EmitHeartbeat(ctx, w.db, brainSnapshotWorkerName, int(w.interval.Seconds()), "ok", "")
}

// BrainSnapshotManifest is what manifest.json carries.
type BrainSnapshotManifest struct {
	Day        string           `json:"day"`
	ExportedAt string           `json:"exported_at"`
	GitSHA     string           `json:"git_sha"`
	Bucket     string           `json:"bucket"`
	Prefix     string           `json:"prefix"`
	Tables     map[string]int64 `json:"tables"`
}

// RunOnce exports every table for today's UTC day and writes the manifest.
// Idempotent: a second run the same day overwrites the same keys (the bucket
// keeps versions). Exported for tests and the API; the caller owns locking.
func (w *BrainSnapshotWorker) RunOnce(ctx context.Context) (BrainSnapshotManifest, error) {
	m := BrainSnapshotManifest{Day: w.now().UTC().Format("2006-01-02"), ExportedAt: w.now().UTC().Format(time.RFC3339),
		GitSHA: buildinfo.Current().GitSHA, Bucket: w.bucket, Prefix: w.prefix, Tables: map[string]int64{}}
	store, err := w.ensureStore(ctx)
	if err != nil {
		return m, err
	}
	if store == nil {
		return m, fmt.Errorf("no bucket configured (BRAIN_SNAPSHOT_BUCKET / ENGINE_S3_BUCKET)")
	}
	for _, tbl := range brainSnapshotTables {
		body, n, err := w.dumpTable(ctx, tbl)
		if err != nil {
			return m, fmt.Errorf("dump %s: %w", tbl, err)
		}
		key := fmt.Sprintf("%s/%s/%s.jsonl", w.prefix, m.Day, tbl)
		if err := store.Put(ctx, key, body, "application/x-ndjson"); err != nil {
			return m, fmt.Errorf("put %s: %w", key, err)
		}
		m.Tables[tbl] = n
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := store.Put(ctx, fmt.Sprintf("%s/%s/manifest.json", w.prefix, m.Day), mb, "application/json"); err != nil {
		return m, fmt.Errorf("put manifest: %w", err)
	}
	log.Printf("[BrainSnapshot] %s: %v", m.Day, m.Tables)
	return m, nil
}

// dumpTable streams `SELECT row_to_json(t) FROM <tbl> t ORDER BY id` into
// JSONL. tbl comes from the closed list above — never from input.
func (w *BrainSnapshotWorker) dumpTable(ctx context.Context, tbl string) ([]byte, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rows, err := w.db.QueryContext(ctx, "SELECT row_to_json(t) FROM "+tbl+" t ORDER BY id")
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var buf bytes.Buffer
	var n int64
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, 0, err
		}
		buf.Write(doc)
		buf.WriteByte('\n')
		n++
		if n > brainSnapshotMaxRowsPerTbl {
			return nil, 0, fmt.Errorf("%s exceeds %d rows — snapshot refused, raise the cap deliberately", tbl, brainSnapshotMaxRowsPerTbl)
		}
	}
	return buf.Bytes(), n, rows.Err()
}
