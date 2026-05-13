package api

// Partner data partner ingestion S3 client.
//
// Mirrors the suppression_s3.go pattern. Stores raw inbound batches from
// data partners (e.g. Attribits) as gzipped NDJSON in jarvis-partner-ingest,
// partitioned by partner slug + dataset slug + UTC date so the slicer worker
// can find FIFO-ordered batches and apply pointer-based slice resumption.
//
// S3 layout:
//   partners/<partner_slug>/<dataset_slug>/<YYYY/MM/DD>/<batch_id>.ndjson.gz
//   partners/<partner_slug>/<dataset_slug>/<YYYY/MM/DD>/<batch_id>.meta.json
//
// The .meta.json file is a small audit footprint that mirrors a row in
// partner_inbound_batches — it survives even if the DB row is later
// archived/deleted.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	defaultPartnerIngestS3Bucket = "jarvis-partner-ingest"
	defaultPartnerIngestS3Region = "us-west-2"
)

type PartnerIngestS3Client struct {
	client *s3.Client
	bucket string
	region string
}

type PartnerBatchMeta struct {
	BatchID      string    `json:"batch_id"`
	PartnerID    string    `json:"partner_id"`
	PartnerSlug  string    `json:"partner_slug"`
	DatasetID    string    `json:"dataset_id"`
	DatasetSlug  string    `json:"dataset_slug"`
	Vertical     string    `json:"vertical"`
	RecordCount  int       `json:"record_count"`
	ReceivedAt   time.Time `json:"received_at"`
	S3Bucket     string    `json:"s3_bucket"`
	S3Key        string    `json:"s3_key"`
	ContentBytes int64     `json:"content_bytes"`
}

func NewPartnerIngestS3Client(client *s3.Client) *PartnerIngestS3Client {
	bucket := os.Getenv("PARTNER_INGEST_S3_BUCKET")
	if bucket == "" {
		bucket = defaultPartnerIngestS3Bucket
	}
	region := os.Getenv("PARTNER_INGEST_S3_REGION")
	if region == "" {
		region = defaultPartnerIngestS3Region
	}
	return &PartnerIngestS3Client{client: client, bucket: bucket, region: region}
}

func (pc *PartnerIngestS3Client) Bucket() string { return pc.bucket }
func (pc *PartnerIngestS3Client) Region() string { return pc.region }

// BatchKey builds the canonical S3 key for a batch payload.
func (pc *PartnerIngestS3Client) BatchKey(partnerSlug, datasetSlug, batchID string, receivedAt time.Time) string {
	return fmt.Sprintf("partners/%s/%s/%s/%s.ndjson.gz",
		sanitizeSlug(partnerSlug),
		sanitizeSlug(datasetSlug),
		receivedAt.UTC().Format("2006/01/02"),
		batchID,
	)
}

// MetaKey is the sibling .meta.json key for a batch.
func (pc *PartnerIngestS3Client) MetaKey(partnerSlug, datasetSlug, batchID string, receivedAt time.Time) string {
	return fmt.Sprintf("partners/%s/%s/%s/%s.meta.json",
		sanitizeSlug(partnerSlug),
		sanitizeSlug(datasetSlug),
		receivedAt.UTC().Format("2006/01/02"),
		batchID,
	)
}

// UploadBatchGzip gzips the records body and uploads it to S3.
// Returns the bytes written (compressed). Caller is responsible for
// providing already-canonical NDJSON (one JSON object per line).
//
// Implementation note: the gzipped payload is buffered in memory before
// upload so that PutObject sees a known Content-Length and can compute
// the request signature in a single pass. Streaming uploads via io.Pipe
// fail against vanilla S3 endpoints with `NotImplemented: A header you
// provided implies functionality that is not implemented` because the
// SDK falls back to STREAMING-AWS4-HMAC-SHA256-PAYLOAD signing, which
// some bucket configurations reject. Batches are bounded to ~25 MiB
// pre-gzip (see maxIngestBytes) so post-gzip is comfortably under 10 MiB.
func (pc *PartnerIngestS3Client) UploadBatchGzip(ctx context.Context, key string, ndjsonBody io.Reader) (int64, error) {
	var compressed bytes.Buffer
	gzWriter := gzip.NewWriter(&compressed)
	if _, err := io.Copy(gzWriter, ndjsonBody); err != nil {
		_ = gzWriter.Close()
		return 0, fmt.Errorf("gzip records: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return 0, fmt.Errorf("gzip flush: %w", err)
	}
	compressedBytes := int64(compressed.Len())

	_, err := pc.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:          aws.String(pc.bucket),
		Key:             aws.String(key),
		Body:            bytes.NewReader(compressed.Bytes()),
		ContentLength:   aws.Int64(compressedBytes),
		ContentType:     aws.String("application/x-ndjson"),
		ContentEncoding: aws.String("gzip"),
		Metadata: map[string]string{
			"x-uploaded-by": "partner-ingest-api",
			"x-uploaded-at": time.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return 0, fmt.Errorf("partner-ingest s3 PutObject %s: %w", key, err)
	}
	return compressedBytes, nil
}

// UploadBatchMeta writes the audit JSON sidecar.
func (pc *PartnerIngestS3Client) UploadBatchMeta(ctx context.Context, key string, meta PartnerBatchMeta) error {
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal batch meta: %w", err)
	}
	_, err = pc.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(pc.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(string(body)),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("partner-ingest s3 PutObject meta %s: %w", key, err)
	}
	return nil
}

// GetBatchGzipReader returns an io.ReadCloser streaming the decompressed
// NDJSON for a batch. The slicer worker uses this to seek through records.
func (pc *PartnerIngestS3Client) GetBatchGzipReader(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := pc.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(pc.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("partner-ingest s3 GetObject %s: %w", key, err)
	}
	gzr, gzErr := gzip.NewReader(out.Body)
	if gzErr != nil {
		out.Body.Close()
		return nil, fmt.Errorf("gzip reader for %s: %w", key, gzErr)
	}
	return &combinedReadCloser{Reader: gzr, closers: []io.Closer{gzr, out.Body}}, nil
}

type combinedReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (c *combinedReadCloser) Close() error {
	var firstErr error
	for _, closer := range c.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// sanitizeSlug normalizes a partner / dataset slug for safe S3 key usage.
// Allowed: lowercase letters, digits, hyphens, underscores. Anything else
// is collapsed to a single hyphen.
func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "unknown"
	}
	out := make([]byte, 0, len(s))
	lastDash := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= '0' && ch <= '9',
			ch == '-', ch == '_':
			out = append(out, ch)
			lastDash = false
		default:
			if !lastDash {
				out = append(out, '-')
				lastDash = true
			}
		}
	}
	r := strings.Trim(string(out), "-_")
	if r == "" {
		return "unknown"
	}
	return r
}
