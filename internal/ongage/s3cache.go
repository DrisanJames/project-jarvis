package ongage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3VolumeCache provides S3-backed persistence for Contact Activity volume data.
// This allows the once-daily Ongage report to survive server restarts without
// needing to regenerate the 15-30 minute report.
type S3VolumeCache struct {
	client *s3.Client
	bucket string
}

// s3VolumePayload is the JSON structure stored in S3.
type s3VolumePayload struct {
	From        string           `json:"from"`
	To          string           `json:"to"`
	GeneratedAt time.Time        `json:"generated_at"`
	Data        map[string]int64 `json:"data"`
}

// NewS3VolumeCache creates a new S3-backed volume cache.
func NewS3VolumeCache(ctx context.Context, bucket, region string) (*S3VolumeCache, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config for S3 volume cache: %w", err)
	}

	return &S3VolumeCache{
		client: s3.NewFromConfig(cfg),
		bucket: bucket,
	}, nil
}

// s3Key returns the S3 object key for a given date range.
func (sc *S3VolumeCache) s3Key(from, to string) string {
	return fmt.Sprintf("contact-activity/volume/%s_%s.json", from, to)
}

// Load retrieves a cached volume map from S3 for the given date range.
// Returns the data, the generation timestamp, and any error.
// Returns nil data (not an error) if the object does not exist.
func (sc *S3VolumeCache) Load(ctx context.Context, from, to time.Time) (map[string]int64, time.Time, error) {
	key := sc.s3Key(from.Format("2006-01-02"), to.Format("2006-01-02"))

	resp, err := sc.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(sc.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// Check if it's a "not found" error — treat as cache miss, not failure
		// AWS SDK v2 returns errors containing "NoSuchKey" or "NotFound"
		errStr := err.Error()
		if contains404(errStr) {
			return nil, time.Time{}, nil
		}
		return nil, time.Time{}, fmt.Errorf("S3 GetObject %s/%s: %w", sc.bucket, key, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("reading S3 object body: %w", err)
	}

	var payload s3VolumePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, time.Time{}, fmt.Errorf("unmarshaling S3 volume payload: %w", err)
	}

	log.Printf("S3 volume cache: loaded %d data-set entries for %s to %s (generated %s)",
		len(payload.Data), payload.From, payload.To, payload.GeneratedAt.Format(time.RFC3339))

	return payload.Data, payload.GeneratedAt, nil
}

// Save writes a volume map to S3 for the given date range.
func (sc *S3VolumeCache) Save(ctx context.Context, from, to time.Time, data map[string]int64) error {
	key := sc.s3Key(from.Format("2006-01-02"), to.Format("2006-01-02"))

	payload := s3VolumePayload{
		From:        from.Format("2006-01-02"),
		To:          to.Format("2006-01-02"),
		GeneratedAt: time.Now().UTC(),
		Data:        data,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling volume payload: %w", err)
	}

	contentType := "application/json"
	_, err = sc.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(sc.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: &contentType,
	})
	if err != nil {
		return fmt.Errorf("S3 PutObject %s/%s: %w", sc.bucket, key, err)
	}

	log.Printf("S3 volume cache: saved %d data-set entries for %s to %s (%d bytes)",
		len(data), payload.From, payload.To, len(body))

	return nil
}

// contains404 checks if an error string indicates a "not found" condition.
func contains404(s string) bool {
	for _, keyword := range []string{"NoSuchKey", "NotFound", "404", "not found"} {
		if len(s) >= len(keyword) {
			for i := 0; i <= len(s)-len(keyword); i++ {
				match := true
				for j := 0; j < len(keyword); j++ {
					if s[i+j] != keyword[j] {
						match = false
						break
					}
				}
				if match {
					return true
				}
			}
		}
	}
	return false
}
