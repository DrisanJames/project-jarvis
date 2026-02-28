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
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

type Normalizer struct {
	s3Client   *s3.Client
	db         *sql.DB
	bucket     string
	orgID      string
	listID     string
	classifier *Classifier
	importer   *Importer
	interval   time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
	lastRunAt  time.Time
	healthy    bool
	running    int32 // atomic flag: 1 = run in progress
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

func (n *Normalizer) IsHealthy() bool  { return n.healthy }
func (n *Normalizer) LastRunAt() time.Time { return n.lastRunAt }
func (n *Normalizer) IsRunning() bool   { return atomic.LoadInt32(&n.running) == 1 }

func (n *Normalizer) runOnce() {
	if !atomic.CompareAndSwapInt32(&n.running, 0, 1) {
		return // already running
	}
	defer atomic.StoreInt32(&n.running, 0)

	ctx := n.ctx
	n.lastRunAt = time.Now()
	n.healthy = true

	keys, err := n.listUnprocessed(ctx)
	if err != nil {
		log.Printf("[datanorm] list unprocessed error: %v", err)
		n.healthy = false
		return
	}

	if len(keys) == 0 {
		log.Printf("[datanorm] no new files to process")
		return
	}

	log.Printf("[datanorm] found %d unprocessed files", len(keys))

	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		if err := n.processFile(ctx, key); err != nil {
			log.Printf("[datanorm] process file %s error: %v", key, err)
		}
	}
}

// listUnprocessed uses a paginator to list ALL objects in the bucket,
// skipping processed/ prefix, 0-byte directory placeholders, and
// keys already tracked in data_import_log.
func (n *Normalizer) listUnprocessed(ctx context.Context) ([]string, error) {
	knownKeys := make(map[string]bool)
	rows, err := n.db.QueryContext(ctx, `SELECT original_key FROM data_import_log`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			rows.Scan(&k)
			knownKeys[k] = true
		}
	}

	var unprocessed []string
	paginator := s3.NewListObjectsV2Paginator(n.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(n.bucket),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list S3 objects: %w", err)
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)

			if obj.Size == nil || *obj.Size == 0 {
				continue // directory placeholder
			}
			if strings.HasPrefix(key, "processed/") {
				continue
			}
			if knownKeys[key] {
				continue
			}
			if !strings.HasSuffix(strings.ToLower(key), ".csv") {
				continue
			}
			unprocessed = append(unprocessed, key)
		}
	}
	return unprocessed, nil
}

// processFile downloads once with a buffered reader: peeks the header for
// classification, then continues reading the same stream for the full import.
func (n *Normalizer) processFile(ctx context.Context, key string) error {
	log.Printf("[datanorm] processing %s", key)

	getOutput, err := n.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(n.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("get S3 object: %w", err)
	}
	defer getOutput.Body.Close()

	// Buffer the stream so we can peek the header and then replay it
	bufReader := bufio.NewReaderSize(getOutput.Body, 4096)

	headerLine, err := peekHeaderLine(bufReader)
	if err != nil {
		if err == io.EOF {
			log.Printf("[datanorm] empty file %s, skipping", key)
			return nil
		}
		return fmt.Errorf("peek header: %w", err)
	}

	header := parseCSVLine(headerLine)
	classification := n.classifier.Classify(key, header)

	titleCaser := cases.Title(language.English)
	var counter int
	n.db.QueryRowContext(ctx, `SELECT COUNT(*)+1 FROM data_import_log`).Scan(&counter)
	renamedKey := fmt.Sprintf("processed/%05d-JVC-%s.csv", counter, titleCaser.String(string(classification)))

	_, err = n.db.ExecContext(ctx,
		`INSERT INTO data_import_log (original_key, renamed_key, classification, status)
		VALUES ($1, $2, $3, 'processing')
		ON CONFLICT (original_key) DO NOTHING`,
		key, renamedKey, string(classification),
	)
	if err != nil {
		return fmt.Errorf("insert import log: %w", err)
	}

	// The bufReader still contains the full stream (header + body)
	// since peekHeaderLine only used Peek, not Read.
	imported, errCount, importErr := n.importer.ImportFromReader(ctx, bufReader, classification, key)
	if importErr != nil {
		n.markFailed(ctx, key, importErr.Error())
		return importErr
	}

	// Copy to processed/ location
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

	log.Printf("[datanorm] completed %s -> %s: imported=%d errors=%d classification=%s",
		key, renamedKey, imported, errCount, classification)
	return nil
}

func (n *Normalizer) markFailed(ctx context.Context, key, errMsg string) {
	n.db.ExecContext(ctx,
		`UPDATE data_import_log SET status='failed', error_message=$1 WHERE original_key=$2`,
		errMsg, key,
	)
}

func (n *Normalizer) resumeStuck() {
	ctx := n.ctx
	rows, err := n.db.QueryContext(ctx,
		`SELECT original_key FROM data_import_log WHERE status = 'processing'`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var origKey string
		if err := rows.Scan(&origKey); err != nil {
			continue
		}
		log.Printf("[datanorm] resuming stuck import: %s", origKey)
		// Reset status so processFile can re-insert
		n.db.ExecContext(ctx, `DELETE FROM data_import_log WHERE original_key = $1`, origKey)
		if err := n.processFile(ctx, origKey); err != nil {
			log.Printf("[datanorm] resume failed for %s: %v", origKey, err)
		}
	}
}

// ManualTrigger runs a single import cycle immediately.
func (n *Normalizer) ManualTrigger() {
	go n.runOnce()
}

// peekHeaderLine reads the first line from a bufio.Reader using Peek
// without consuming the bytes, so the full stream can be re-read.
func peekHeaderLine(br *bufio.Reader) (string, error) {
	// Peek progressively larger chunks to find the newline
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
	// Header line is very long; return what we have
	peeked, _ := br.Peek(64 * 1024)
	return strings.TrimRight(string(peeked), "\r\n"), nil
}

// parseCSVLine splits a single CSV header line into fields.
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
