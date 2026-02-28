package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Storage handles persistent storage of knowledge base data in S3
type S3Storage struct {
	client        *s3.Client
	bucket        string
	prefix        string
	encryptionKey []byte // 32-byte AES-256 key (optional)
	compress      bool
	region        string
}

// S3StorageConfig contains configuration for S3 storage
type S3StorageConfig struct {
	Bucket        string
	Prefix        string // e.g., "ignite/knowledge/"
	Region        string
	EncryptionKey string // Base64-encoded 32-byte key for AES-256 (optional)
	Compress      bool   // Enable gzip compression
}

// NewS3Storage creates a new S3 storage handler using the default AWS profile
func NewS3Storage(cfg S3StorageConfig) (*S3Storage, error) {
	ctx := context.Background()

	// Load AWS config using default profile
	region := cfg.Region
	if region == "" {
		region = os.Getenv("AWS_REGION")
		if region == "" {
			region = "us-east-1"
		}
	}

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg)

	storage := &S3Storage{
		client:   client,
		bucket:   cfg.Bucket,
		prefix:   cfg.Prefix,
		compress: cfg.Compress,
		region:   region,
	}

	// Parse encryption key if provided
	if cfg.EncryptionKey != "" {
		key, err := base64.StdEncoding.DecodeString(cfg.EncryptionKey)
		if err != nil {
			return nil, fmt.Errorf("invalid encryption key: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("encryption key must be 32 bytes (AES-256)")
		}
		storage.encryptionKey = key
	}

	// Verify bucket access
	_, err = client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(cfg.Bucket),
	})
	if err != nil {
		log.Printf("S3Storage: Warning - bucket access check failed: %v", err)
		// Don't fail - bucket might need to be created
	}

	log.Printf("S3Storage: Initialized with bucket=%s, prefix=%s, region=%s, encrypted=%v, compressed=%v",
		cfg.Bucket, cfg.Prefix, region, cfg.EncryptionKey != "", cfg.Compress)

	return storage, nil
}

// SaveKnowledgeBase saves the knowledge base to S3
func (s *S3Storage) SaveKnowledgeBase(ctx context.Context, kb *KnowledgeBase) error {
	kb.mu.RLock()
	defer kb.mu.RUnlock()

	// Serialize to JSON
	data, err := json.MarshalIndent(kb, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize knowledge base: %w", err)
	}

	// Compress if enabled
	if s.compress {
		data, err = s.gzipCompress(data)
		if err != nil {
			return fmt.Errorf("failed to compress data: %w", err)
		}
	}

	// Encrypt if key is set
	if s.encryptionKey != nil {
		data, err = s.encrypt(data)
		if err != nil {
			return fmt.Errorf("failed to encrypt data: %w", err)
		}
	}

	// Upload to S3
	key := s.prefix + "knowledge_base.json"
	if s.compress {
		key += ".gz"
	}
	if s.encryptionKey != nil {
		key += ".enc"
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
		Metadata: map[string]string{
			"compressed": fmt.Sprintf("%v", s.compress),
			"encrypted":  fmt.Sprintf("%v", s.encryptionKey != nil),
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	log.Printf("S3Storage: Saved knowledge base to s3://%s/%s (%d bytes)", s.bucket, key, len(data))
	return nil
}

// LoadKnowledgeBase loads the knowledge base from S3
func (s *S3Storage) LoadKnowledgeBase(ctx context.Context) (*KnowledgeBase, error) {
	// Determine the key based on configuration
	key := s.prefix + "knowledge_base.json"
	if s.compress {
		key += ".gz"
	}
	if s.encryptionKey != nil {
		key += ".enc"
	}

	// Download from S3
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download from S3: %w", err)
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read S3 object: %w", err)
	}

	// Decrypt if encrypted
	if s.encryptionKey != nil {
		data, err = s.decrypt(data)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt data: %w", err)
		}
	}

	// Decompress if compressed
	if s.compress {
		data, err = s.gzipDecompress(data)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress data: %w", err)
		}
	}

	// Deserialize
	kb := &KnowledgeBase{}
	if err := json.Unmarshal(data, kb); err != nil {
		return nil, fmt.Errorf("failed to deserialize knowledge base: %w", err)
	}

	log.Printf("S3Storage: Loaded knowledge base from s3://%s/%s", s.bucket, key)
	return kb, nil
}

// SaveSnapshot saves a timestamped snapshot of performance data
func (s *S3Storage) SaveSnapshot(ctx context.Context, snapshot PerformanceSnapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}

	key := fmt.Sprintf("%ssnapshots/%s.json", s.prefix, snapshot.Timestamp.Format("2006/01/02/15-04-05"))

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("failed to save snapshot: %w", err)
	}

	return nil
}

// SaveLearnedPatterns saves learned patterns separately for analysis
func (s *S3Storage) SaveLearnedPatterns(ctx context.Context, patterns []LearnedPattern) error {
	data, err := json.MarshalIndent(patterns, "", "  ")
	if err != nil {
		return err
	}

	key := s.prefix + "learned_patterns.json"

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("failed to save patterns: %w", err)
	}

	return nil
}

// SaveAgenticState saves the agentic loop state
func (s *S3Storage) SaveAgenticState(ctx context.Context, state map[string]interface{}) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	key := s.prefix + "agentic_state.json"

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("failed to save agentic state: %w", err)
	}

	log.Printf("S3Storage: Saved agentic state to s3://%s/%s", s.bucket, key)
	return nil
}

// LoadAgenticState loads the agentic loop state
func (s *S3Storage) LoadAgenticState(ctx context.Context) (map[string]interface{}, error) {
	key := s.prefix + "agentic_state.json"

	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, err
	}

	var state map[string]interface{}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	return state, nil
}

// ListSnapshots lists available snapshots in a date range
func (s *S3Storage) ListSnapshots(ctx context.Context, startDate, endDate time.Time) ([]string, error) {
	prefix := s.prefix + "snapshots/"

	var snapshots []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, obj := range page.Contents {
			snapshots = append(snapshots, *obj.Key)
		}
	}

	return snapshots, nil
}

// gzipCompress compresses data using gzip
func (s *S3Storage) gzipCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gzipDecompress decompresses gzip data
func (s *S3Storage) gzipDecompress(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return io.ReadAll(gz)
}

// encrypt encrypts data using AES-256-GCM
func (s *S3Storage) encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// decrypt decrypts data using AES-256-GCM
func (s *S3Storage) decrypt(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.encryptionKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}

// CreateBucket creates the S3 bucket if it doesn't exist
func (s *S3Storage) CreateBucket(ctx context.Context) error {
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		// Ignore if bucket already exists
		log.Printf("S3Storage: CreateBucket result: %v", err)
	}
	return nil
}

// GetBucket returns the bucket name
func (s *S3Storage) GetBucket() string {
	return s.bucket
}

// GetPrefix returns the key prefix
func (s *S3Storage) GetPrefix() string {
	return s.prefix
}
