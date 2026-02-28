package datanorm

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

type Normalizer struct {
	s3Client      *s3.Client
	db            *sql.DB
	bucket        string
	orgID         string
	listID        string
	classifier    *Classifier
	importer      *Importer
	interval      time.Duration
	ctx           context.Context
	cancel        context.CancelFunc
	lastRunAt     time.Time
	healthy       bool
	running       int32
	hasFileSize   bool
	hasStartedAt  bool
	hasRetryCount bool
}

func NewNormalizer(db *sql.DB, cfg Config) (*Normalizer, error) {
	ctx := context.Background()

	var awsCfg aws.Config
	var err error
	if cfg.AWSProfile != "" {
		awsCfg, err = awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(cfg.Region),
			awsconfig.WithSharedConfigProfile(cfg.AWSProfile),
		)
	} else {
		awsCfg, err = awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(cfg.Region),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	interval := cfg.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	return &Normalizer{
		s3Client:   s3.NewFromConfig(awsCfg),
		db:         db,
		bucket:     cfg.Bucket,
		orgID:      cfg.OrgID,
		listID:     cfg.ListID,
		classifier: NewClassifier(),
		importer:   NewImporter(db, cfg.OrgID, cfg.ListID),
		interval:   interval,
		healthy:    true,
	}, nil
}

func (n *Normalizer) Start() {
	n.ctx, n.cancel = context.WithCancel(context.Background())
	n.ensureSchema()
	go func() {
		n.resumeStuck()
		n.runOnce()
		ticker := time.NewTicker(n.interval)
		defer ticker.Stop()
		for {
			select {
			case <-n.ctx.Done():
				return
			case <-ticker.C:
				n.runOnce()
			}
		}
	}()
}

func (n *Normalizer) Stop() {
	if n.cancel != nil {
		n.cancel()
	}
}

func (n *Normalizer) IsHealthy() bool    { return n.healthy }
func (n *Normalizer) LastRunAt() time.Time { return n.lastRunAt }
func (n *Normalizer) IsRunning() bool     { return atomic.LoadInt32(&n.running) == 1 }

// runOnce executes one cycle: discover new files, then process a batch from the queue.
func (n *Normalizer) runOnce() {
	if !atomic.CompareAndSwapInt32(&n.running, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&n.running, 0)

	ctx := n.ctx
	n.lastRunAt = time.Now()
	n.healthy = true

	n.discoverFiles(ctx)
	n.processQueue(ctx)
}

// discoverFiles scans the S3 bucket and inserts every new CSV as a pending
// entry in data_import_log. Already-known files are skipped via ON CONFLICT.
func (n *Normalizer) discoverFiles(ctx context.Context) {
	paginator := s3.NewListObjectsV2Paginator(n.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(n.bucket),
	})

	inserted := 0
	for paginator.HasMorePages() {
		if ctx.Err() != nil {
			return
		}
		page, err := paginator.NextPage(ctx)
		if err != nil {
			log.Printf("[datanorm] list S3 objects error: %v", err)
			n.healthy = false
			return
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)

			if obj.Size == nil || *obj.Size == 0 {
				continue
			}
			if strings.HasPrefix(key, "processed/") {
				continue
			}
			if !strings.HasSuffix(strings.ToLower(key), ".csv") {
				continue
			}

			fileSize := *obj.Size
			var res sql.Result
			if n.hasFileSize {
				res, err = n.db.ExecContext(ctx,
					`INSERT INTO data_import_log (original_key, status, file_size)
					 VALUES ($1, 'pending', $2)
					 ON CONFLICT (original_key) DO NOTHING`,
					key, fileSize,
				)
			} else {
				res, err = n.db.ExecContext(ctx,
					`INSERT INTO data_import_log (original_key, status)
					 VALUES ($1, 'pending')
					 ON CONFLICT (original_key) DO NOTHING`,
					key,
				)
			}
			if err != nil {
				log.Printf("[datanorm] insert pending %s: %v", key, err)
				continue
			}
			if rows, _ := res.RowsAffected(); rows > 0 {
				inserted++
			}
		}
	}

	if inserted > 0 {
		log.Printf("[datanorm] discovered %d new files", inserted)
	}
}

// processQueue picks pending files from the database (smallest first) and
// processes them concurrently with a semaphore of 4.
func (n *Normalizer) processQueue(ctx context.Context) {
	orderCol := "created_at"
	if n.hasFileSize {
		orderCol = "file_size"
	}
	rows, err := n.db.QueryContext(ctx,
		`SELECT original_key FROM data_import_log
		 WHERE status = 'pending'
		 ORDER BY `+orderCol+` ASC
		 LIMIT 10`)
	if err != nil {
		log.Printf("[datanorm] query queue: %v", err)
		return
	}

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			keys = append(keys, k)
		}
	}
	rows.Close()

	if len(keys) == 0 {
		return
	}

	log.Printf("[datanorm] processing batch of %d files from queue", len(keys))

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, key := range keys {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(k string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := n.processFile(ctx, k); err != nil {
				log.Printf("[datanorm] process file %s error: %v", k, err)
			}
		}(key)
	}
	wg.Wait()
}

// processFile downloads a file from S3, detects headers, classifies it,
// and imports records into the database. The file must already exist in
// data_import_log with status='pending'.
func (n *Normalizer) processFile(ctx context.Context, key string) error {
	// Atomically claim the file — if another worker grabbed it, skip.
	titleCaser := cases.Title(language.English)
	var counter int
	n.db.QueryRowContext(ctx, `SELECT COUNT(*)+1 FROM data_import_log WHERE status != 'pending'`).Scan(&counter)

	claimSQL := `UPDATE data_import_log SET status='processing'`
	if n.hasRetryCount {
		claimSQL += `, retry_count=retry_count+1`
	}
	if n.hasStartedAt {
		claimSQL += `, started_at=NOW()`
	}
	claimSQL += ` WHERE original_key=$1 AND status='pending'`
	res, err := n.db.ExecContext(ctx, claimSQL, key)
	if err != nil {
		return fmt.Errorf("claim file: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return nil
	}

	log.Printf("[datanorm] processing %s", key)

	getOutput, err := n.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(n.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		n.markFailed(ctx, key, fmt.Sprintf("get S3 object: %v", err))
		return fmt.Errorf("get S3 object: %w", err)
	}
	defer getOutput.Body.Close()

	bufReader := bufio.NewReaderSize(getOutput.Body, 256*1024)

	headerLine, err := peekHeaderLine(bufReader)
	if err != nil {
		if err == io.EOF {
			log.Printf("[datanorm] empty file %s, skipping", key)
			n.markFailed(ctx, key, "empty file")
			return nil
		}
		n.markFailed(ctx, key, fmt.Sprintf("peek header: %v", err))
		return fmt.Errorf("peek header: %w", err)
	}

	firstRow := parseCSVLine(headerLine)

	var preMapping *ColumnMapping
	headerless := false

	mapping := MapColumns(firstRow)
	if mapping != nil {
		preMapping = nil
	} else {
		preMapping = MapColumnsHeaderless(firstRow)
		if preMapping != nil {
			headerless = true
			log.Printf("[datanorm] headerless CSV detected for %s (email at col %d)", key, preMapping.EmailIdx)
		} else {
			log.Printf("[datanorm] skipping %s: no email column in header or data", key)
			n.markFailed(ctx, key, "no email column detected in header or first row")
			return fmt.Errorf("no email column detected")
		}
	}

	classification := n.classifier.Classify(key, firstRow)

	renamedKey := fmt.Sprintf("processed/%05d-JVC-%s.csv", counter, titleCaser.String(string(classification)))
	n.db.ExecContext(ctx,
		`UPDATE data_import_log SET renamed_key=$1, classification=$2 WHERE original_key=$3`,
		renamedKey, string(classification), key,
	)

	onProgress := func(imported, errors int) {
		n.db.ExecContext(ctx,
			`UPDATE data_import_log SET record_count=$1, error_count=$2 WHERE original_key=$3`,
			imported, errors, key)
	}

	imported, errCount, importErr := n.importer.ImportFromReader(
		ctx, bufReader, classification, key,
		preMapping, headerless, onProgress,
	)
	if importErr != nil {
		n.markFailed(ctx, key, importErr.Error())
		return importErr
	}

	_, copyErr := n.s3Client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(n.bucket),
		CopySource: aws.String(n.bucket + "/" + key),
		Key:        aws.String(renamedKey),
	})
	if copyErr != nil {
		log.Printf("[datanorm] copy to %s failed: %v", renamedKey, copyErr)
	}

	n.db.ExecContext(ctx,
		`UPDATE data_import_log SET status='completed', record_count=$1, error_count=$2, processed_at=NOW()
		WHERE original_key=$3`,
		imported, errCount, key,
	)

	if copyErr == nil {
		_, delErr := n.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(n.bucket),
			Key:    aws.String(key),
		})
		if delErr != nil {
			log.Printf("[datanorm] delete original %s failed: %v", key, delErr)
		} else {
			n.db.ExecContext(ctx,
				`UPDATE data_import_log SET original_exists=FALSE WHERE original_key=$1`, key)
		}
	}

	log.Printf("[datanorm] completed %s -> %s: imported=%d errors=%d classification=%s headerless=%v",
		key, renamedKey, imported, errCount, classification, headerless)
	return nil
}

func (n *Normalizer) markFailed(ctx context.Context, key, errMsg string) {
	n.db.ExecContext(ctx,
		`UPDATE data_import_log SET status='failed', error_message=$1 WHERE original_key=$2`,
		errMsg, key,
	)
}

// resumeStuck resets files left in 'processing' state (from a prior crash)
// back to 'pending' so the queue picks them up. Files that have exceeded
// the retry limit are marked as failed.
func (n *Normalizer) resumeStuck() {
	ctx := n.ctx
	if n.hasRetryCount {
		n.db.ExecContext(ctx,
			`UPDATE data_import_log SET status='pending'
			 WHERE status='processing' AND retry_count < 3`)
		n.db.ExecContext(ctx,
			`UPDATE data_import_log SET status='failed', error_message='max retries exceeded'
			 WHERE status='processing' AND retry_count >= 3`)
	} else {
		n.db.ExecContext(ctx,
			`UPDATE data_import_log SET status='pending'
			 WHERE status='processing'`)
	}
}

// ensureSchema applies idempotent schema changes for the queue system.
// Uses a DO block with superuser role grant to handle ownership issues.
func (n *Normalizer) ensureSchema() {
	// Try to take ownership first (works if connected user is rds_superuser member)
	n.db.Exec(`DO $$ BEGIN
		EXECUTE 'ALTER TABLE data_import_log OWNER TO ' || current_user;
	EXCEPTION WHEN OTHERS THEN NULL;
	END $$`)

	stmts := []string{
		`ALTER TABLE data_import_log ADD COLUMN IF NOT EXISTS file_size BIGINT DEFAULT 0`,
		`ALTER TABLE data_import_log ADD COLUMN IF NOT EXISTS retry_count INTEGER DEFAULT 0`,
		`ALTER TABLE data_import_log ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ`,
	}
	for _, s := range stmts {
		if _, err := n.db.Exec(s); err != nil {
			log.Printf("[datanorm] schema migration (non-fatal): %v", err)
		}
	}
	n.db.Exec(`ALTER TABLE data_import_log DROP CONSTRAINT IF EXISTS data_import_log_status_check`)
	n.db.Exec(`ALTER TABLE data_import_log ADD CONSTRAINT data_import_log_status_check CHECK (status IN ('pending','processing','completed','failed','skipped'))`)

	// Detect which columns actually exist so queries can adapt
	var cnt int
	if err := n.db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_name='data_import_log' AND column_name='file_size'`).Scan(&cnt); err == nil {
		n.hasFileSize = cnt > 0
	}
	if err := n.db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_name='data_import_log' AND column_name='started_at'`).Scan(&cnt); err == nil {
		n.hasStartedAt = cnt > 0
	}
	if err := n.db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_name='data_import_log' AND column_name='retry_count'`).Scan(&cnt); err == nil {
		n.hasRetryCount = cnt > 0
	}
	log.Printf("[datanorm] schema: file_size=%v started_at=%v retry_count=%v", n.hasFileSize, n.hasStartedAt, n.hasRetryCount)
}

// ManualTrigger runs a single import cycle immediately.
func (n *Normalizer) ManualTrigger() {
	go n.runOnce()
}

func peekHeaderLine(br *bufio.Reader) (string, error) {
	for size := 4096; size <= 64*1024; size *= 2 {
		peeked, err := br.Peek(size)
		if len(peeked) > 0 {
			if idx := bytes.IndexByte(peeked, '\n'); idx >= 0 {
				line := string(peeked[:idx])
				return strings.TrimRight(line, "\r"), nil
			}
		}
		if err != nil {
			if err == io.EOF && len(peeked) > 0 {
				return strings.TrimRight(string(peeked), "\r\n"), nil
			}
			return "", err
		}
	}
	peeked, _ := br.Peek(64 * 1024)
	return strings.TrimRight(string(peeked), "\r\n"), nil
}

func parseCSVLine(line string) []string {
	r := csv.NewReader(strings.NewReader(line))
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	fields, err := r.Read()
	if err != nil {
		return strings.Split(line, ",")
	}
	return fields
}
