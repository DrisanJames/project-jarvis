package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	suppressionS3Bucket = "jarvis-offer-suppressions"
	suppressionS3Region = "us-west-2"
)

// S3 key layout:
//   offers/{offerID}/raw/{timestamp}.zip        — original Optizmo ZIP
//   offers/{offerID}/hashes/latest.txt.gz       — processed MD5 hashes (one per line, gzipped)
//   offers/{offerID}/bloom/latest.bloom          — serialized Bloom filter
//   offers/{offerID}/meta.json                   — metadata (hash count, last sync, bloom FP rate)

type SuppressionS3Client struct {
	client *s3.Client
	bucket string
}

type SuppressionMeta struct {
	OfferID       string    `json:"offer_id"`
	HashCount     int       `json:"hash_count"`
	BloomSizeKB   int       `json:"bloom_size_kb"`
	BloomFPRate   float64   `json:"bloom_fp_rate"`
	LastSyncAt    time.Time `json:"last_sync_at"`
	S3HashKey     string    `json:"s3_hash_key"`
	S3BloomKey    string    `json:"s3_bloom_key"`
	S3RawKey      string    `json:"s3_raw_key"`
}

func NewSuppressionS3Client(client *s3.Client) *SuppressionS3Client {
	return &SuppressionS3Client{
		client: client,
		bucket: suppressionS3Bucket,
	}
}

func (sc *SuppressionS3Client) rawKey(offerID string) string {
	return fmt.Sprintf("offers/%s/raw/%s.zip", offerID, time.Now().UTC().Format("20060102T150405"))
}

func (sc *SuppressionS3Client) hashKey(offerID string) string {
	return fmt.Sprintf("offers/%s/hashes/latest.txt.gz", offerID)
}

func (sc *SuppressionS3Client) bloomKey(offerID string) string {
	return fmt.Sprintf("offers/%s/bloom/latest.bloom", offerID)
}

func (sc *SuppressionS3Client) metaKey(offerID string) string {
	return fmt.Sprintf("offers/%s/meta.json", offerID)
}

// UploadRawZIP stores the original Optizmo ZIP in S3 for archival.
func (sc *SuppressionS3Client) UploadRawZIP(ctx context.Context, offerID string, data io.Reader) (string, error) {
	key := sc.rawKey(offerID)
	_, err := sc.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(sc.bucket),
		Key:         aws.String(key),
		Body:        data,
		ContentType: aws.String("application/zip"),
	})
	if err != nil {
		return "", fmt.Errorf("upload raw ZIP to s3://%s/%s: %w", sc.bucket, key, err)
	}
	log.Printf("[SuppressionS3] uploaded raw ZIP → s3://%s/%s", sc.bucket, key)
	return key, nil
}

// UploadHashFile uploads a gzipped hash file to S3.
// The caller passes the uncompressed hash data; this function gzips it.
func (sc *SuppressionS3Client) UploadHashFile(ctx context.Context, offerID string, hashData io.Reader) (string, int, error) {
	key := sc.hashKey(offerID)

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	n, err := io.Copy(gw, hashData)
	if err != nil {
		return "", 0, fmt.Errorf("gzip hashes: %w", err)
	}
	gw.Close()

	_, err = sc.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:          aws.String(sc.bucket),
		Key:             aws.String(key),
		Body:            bytes.NewReader(buf.Bytes()),
		ContentType:     aws.String("application/gzip"),
		ContentEncoding: aws.String("gzip"),
	})
	if err != nil {
		return "", 0, fmt.Errorf("upload hashes to s3://%s/%s: %w", sc.bucket, key, err)
	}
	log.Printf("[SuppressionS3] uploaded %d bytes of hashes (gzipped: %d bytes) → s3://%s/%s",
		n, buf.Len(), sc.bucket, key)
	return key, int(n), nil
}

// DownloadHashFile downloads and decompresses the hash file for an offer.
// Returns the hashes as a reader (one MD5 per line, uncompressed).
func (sc *SuppressionS3Client) DownloadHashFile(ctx context.Context, offerID string) (io.ReadCloser, error) {
	key := sc.hashKey(offerID)
	out, err := sc.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(sc.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("download s3://%s/%s: %w", sc.bucket, key, err)
	}
	gr, err := gzip.NewReader(out.Body)
	if err != nil {
		out.Body.Close()
		return nil, fmt.Errorf("decompress s3://%s/%s: %w", sc.bucket, key, err)
	}
	return &gzipReadCloser{gr: gr, underlying: out.Body}, nil
}

// HashFileExists checks if a hash file already exists for this offer.
func (sc *SuppressionS3Client) HashFileExists(ctx context.Context, offerID string) (bool, error) {
	key := sc.hashKey(offerID)
	_, err := sc.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(sc.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *s3types.NotFound
		if ok := strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404"); ok {
			return false, nil
		}
		_ = nsk
		return false, err
	}
	return true, nil
}

// UploadBloom stores the serialized Bloom filter.
func (sc *SuppressionS3Client) UploadBloom(ctx context.Context, offerID string, data []byte) (string, error) {
	key := sc.bloomKey(offerID)
	_, err := sc.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(sc.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return "", fmt.Errorf("upload bloom to s3://%s/%s: %w", sc.bucket, key, err)
	}
	log.Printf("[SuppressionS3] uploaded Bloom filter (%d KB) → s3://%s/%s", len(data)/1024, sc.bucket, key)
	return key, nil
}

// DownloadBloom retrieves the serialized Bloom filter for an offer.
func (sc *SuppressionS3Client) DownloadBloom(ctx context.Context, offerID string) ([]byte, error) {
	key := sc.bloomKey(offerID)
	out, err := sc.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(sc.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("download s3://%s/%s: %w", sc.bucket, key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// SaveMeta writes the metadata JSON for an offer's suppression state.
func (sc *SuppressionS3Client) SaveMeta(ctx context.Context, meta SuppressionMeta) error {
	key := sc.metaKey(meta.OfferID)
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = sc.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(sc.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

// LoadMeta reads the metadata JSON for an offer.
func (sc *SuppressionS3Client) LoadMeta(ctx context.Context, offerID string) (*SuppressionMeta, error) {
	key := sc.metaKey(offerID)
	out, err := sc.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(sc.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	var meta SuppressionMeta
	if err := json.NewDecoder(out.Body).Decode(&meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

type gzipReadCloser struct {
	gr         *gzip.Reader
	underlying io.ReadCloser
}

func (g *gzipReadCloser) Read(p []byte) (int, error) {
	return g.gr.Read(p)
}

func (g *gzipReadCloser) Close() error {
	g.gr.Close()
	return g.underlying.Close()
}
