package worker

import (
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// SUPPRESSION IMPORT WORKER - Large File Processing (up to 10GB)
// =============================================================================
// Designed to handle multi-GB suppression files (e.g., 2.97 GB Optizmo dumps)
// with constant ~50 MB memory usage via:
// - Chunked/resumable file uploads (10 MB chunks)
// - Streaming line-by-line parsing (bufio.Scanner)
// - Bulk PostgreSQL inserts using COPY protocol (5000 rows/batch)
// - Background goroutine processing (decoupled from HTTP request)
// - Progress tracking via Redis (preferred) or in-memory fallback
// =============================================================================

const (
	// SuppImport limits
	SuppMaxFileSize      = 10 * 1024 * 1024 * 1024 // 10 GB
	SuppMaxChunkSize     = 50 * 1024 * 1024         // 50 MB
	SuppDefaultChunkSize = 50 * 1024 * 1024         // 50 MB (was 10 MB — larger chunks = fewer HTTP requests)
	SuppMinChunkSize     = 1 * 1024 * 1024          // 1 MB

	// Processing — tuned for 50M+ row suppression files
	SuppBatchSize        = 50000             // Rows per COPY batch (was 5000 — COPY protocol handles large batches efficiently)
	SuppProgressInterval = 50000             // Update progress every N rows (was 2500 — less frequent = less Redis overhead)
	SuppWriterWorkers    = 4                 // Parallel DB writer goroutines for pipelined inserts
	SuppChannelBuffer    = 8                 // Buffered channel depth between reader and writers
	SuppTempDir          = "/tmp/mailing-suppression-imports"
	SuppSessionTTL       = 24 * time.Hour
)

// =============================================================================
// TYPES
// =============================================================================

// SuppImportJob represents a background suppression import job
type SuppImportJob struct {
	ID              string    `json:"id"`
	ListID          string    `json:"list_id"`
	Filename        string    `json:"filename"`
	FileSize        int64     `json:"file_size"`
	TempFilePath    string    `json:"temp_file_path"`
	Status          string    `json:"status"` // pending, uploading, processing, completed, failed
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`

	// Chunked upload tracking
	ChunkSize      int64 `json:"chunk_size"`
	TotalChunks    int   `json:"total_chunks"`
	UploadedChunks []int `json:"uploaded_chunks"`
}

// SuppImportProgress tracks real-time import progress
type SuppImportProgress struct {
	JobID          string     `json:"job_id"`
	Status         string     `json:"status"`
	Phase          string     `json:"phase"` // uploading, processing, completed, failed
	TotalLines     int64      `json:"total_lines"`
	ProcessedRows  int64      `json:"processed_rows"`
	ImportedCount  int64      `json:"imported_count"`
	DuplicateCount int64      `json:"duplicate_count"`
	InvalidCount   int64      `json:"invalid_count"`
	BytesUploaded  int64      `json:"bytes_uploaded"`
	TotalBytes     int64      `json:"total_bytes"`
	RowsPerSecond  float64    `json:"rows_per_second"`
	EstimatedETA   int64      `json:"estimated_eta_seconds"`
	StartedAt      time.Time  `json:"started_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Errors         []string   `json:"errors,omitempty"`
}

// =============================================================================
// SERVICE
// =============================================================================

// SuppressionImportService handles large suppression file imports.
// Works with or without Redis; falls back to in-memory progress tracking.
type SuppressionImportService struct {
	db    *sql.DB
	redis *redis.Client // optional
	dir   string

	// In-memory fallback when Redis is unavailable
	mu       sync.RWMutex
	jobs     map[string]*SuppImportJob
	progress map[string]*SuppImportProgress
	chunks   map[string]map[int]bool
}

// NewSuppressionImportService creates a new suppression import service
func NewSuppressionImportService(db *sql.DB, redisClient *redis.Client) *SuppressionImportService {
	os.MkdirAll(SuppTempDir, 0755)

	svc := &SuppressionImportService{
		db:       db,
		redis:    redisClient,
		dir:      SuppTempDir,
		jobs:     make(map[string]*SuppImportJob),
		progress: make(map[string]*SuppImportProgress),
		chunks:   make(map[string]map[int]bool),
	}

	svc.ensureTable()
	return svc
}

func (s *SuppressionImportService) hasRedis() bool {
	return s.redis != nil
}

// ensureTable creates the import jobs tracking table
func (s *SuppressionImportService) ensureTable() {
	if s.db == nil {
		return
	}
	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS mailing_suppression_import_jobs (
			id UUID PRIMARY KEY,
			list_id VARCHAR(100),
			filename VARCHAR(500),
			file_size BIGINT DEFAULT 0,
			status VARCHAR(50) DEFAULT 'pending',
			total_lines BIGINT DEFAULT 0,
			imported_count BIGINT DEFAULT 0,
			duplicate_count BIGINT DEFAULT 0,
			invalid_count BIGINT DEFAULT 0,
			error_message TEXT,
			started_at TIMESTAMP WITH TIME ZONE,
			completed_at TIMESTAMP WITH TIME ZONE,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		)
	`)
}

// =============================================================================
// STORAGE LAYER (Redis with in-memory fallback)
// =============================================================================

func (s *SuppressionImportService) storeJob(ctx context.Context, job *SuppImportJob) {
	data, _ := json.Marshal(job)
	if s.hasRedis() {
		s.redis.Set(ctx, s.jobKey(job.ID), data, SuppSessionTTL)
	}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()
}

func (s *SuppressionImportService) setProgress(ctx context.Context, jobID string, p *SuppImportProgress) {
	if s.hasRedis() {
		data, _ := json.Marshal(p)
		s.redis.Set(ctx, s.progressKey(jobID), data, SuppSessionTTL)
	}
	s.mu.Lock()
	s.progress[jobID] = p
	s.mu.Unlock()
}

func (s *SuppressionImportService) addChunk(ctx context.Context, jobID string, chunkNumber int) int64 {
	if s.hasRedis() {
		key := s.chunkSetKey(jobID)
		s.redis.SAdd(ctx, key, chunkNumber)
		s.redis.Expire(ctx, key, SuppSessionTTL)
		count, _ := s.redis.SCard(ctx, key).Result()
		return count
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chunks[jobID] == nil {
		s.chunks[jobID] = make(map[int]bool)
	}
	s.chunks[jobID][chunkNumber] = true
	return int64(len(s.chunks[jobID]))
}

func (s *SuppressionImportService) getChunkCount(ctx context.Context, jobID string) int64 {
	if s.hasRedis() {
		count, _ := s.redis.SCard(ctx, s.chunkSetKey(jobID)).Result()
		return count
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int64(len(s.chunks[jobID]))
}

// =============================================================================
// CHUNKED UPLOAD SESSION
// =============================================================================

// InitUpload creates a chunked upload session
func (s *SuppressionImportService) InitUpload(ctx context.Context, listID, filename string, fileSize int64) (*SuppImportJob, error) {
	if fileSize > SuppMaxFileSize {
		return nil, fmt.Errorf("file size %d exceeds maximum %d bytes (10 GB)", fileSize, SuppMaxFileSize)
	}

	chunkSize := int64(SuppDefaultChunkSize)
	totalChunks := int((fileSize + chunkSize - 1) / chunkSize)

	jobID := uuid.New().String()
	job := &SuppImportJob{
		ID:             jobID,
		ListID:         listID,
		Filename:       filename,
		FileSize:       fileSize,
		TempFilePath:   filepath.Join(s.dir, fmt.Sprintf("%s.txt", jobID)),
		Status:         "pending",
		CreatedAt:      time.Now(),
		ExpiresAt:      time.Now().Add(SuppSessionTTL),
		ChunkSize:      chunkSize,
		TotalChunks:    totalChunks,
		UploadedChunks: []int{},
	}

	s.storeJob(ctx, job)

	// Pre-allocate temp file
	f, err := os.Create(job.TempFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	f.Close()
	os.Truncate(job.TempFilePath, fileSize)

	// DB record
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_suppression_import_jobs (id, list_id, filename, file_size, status, created_at)
		VALUES ($1, $2, $3, $4, 'pending', NOW())
	`, jobID, listID, filename, fileSize)

	log.Printf("[SuppImport] Session %s: file=%s, size=%d, chunks=%d", jobID, filename, fileSize, totalChunks)
	return job, nil
}

// UploadChunk writes a chunk to the temp file at the correct offset
func (s *SuppressionImportService) UploadChunk(ctx context.Context, jobID string, chunkNumber int, data []byte) error {
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return err
	}

	if chunkNumber < 0 || chunkNumber >= job.TotalChunks {
		return fmt.Errorf("invalid chunk %d (total: %d)", chunkNumber, job.TotalChunks)
	}

	// Write at offset
	offset := int64(chunkNumber) * job.ChunkSize
	f, err := os.OpenFile(job.TempFilePath, os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open temp file: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteAt(data, offset); err != nil {
		return fmt.Errorf("failed to write chunk: %w", err)
	}

	// Track chunk
	chunkCount := s.addChunk(ctx, jobID, chunkNumber)

	// Update progress
	s.setProgress(ctx, jobID, &SuppImportProgress{
		JobID:         jobID,
		Status:        "uploading",
		Phase:         "uploading",
		BytesUploaded: chunkCount * job.ChunkSize,
		TotalBytes:    job.FileSize,
		UpdatedAt:     time.Now(),
	})

	return nil
}

// IsUploadComplete checks if all chunks are uploaded
func (s *SuppressionImportService) IsUploadComplete(ctx context.Context, jobID string) (bool, error) {
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	count := s.getChunkCount(ctx, jobID)
	return int(count) == job.TotalChunks, nil
}

// =============================================================================
// BACKGROUND PROCESSING
// =============================================================================

// StartProcessing kicks off background processing of the uploaded file.
// Returns immediately; caller polls GetProgress for status.
func (s *SuppressionImportService) StartProcessing(ctx context.Context, jobID string) error {
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		return err
	}

	complete, err := s.IsUploadComplete(ctx, jobID)
	if err != nil {
		return err
	}
	if !complete {
		count := s.getChunkCount(ctx, jobID)
		return fmt.Errorf("upload incomplete: %d/%d chunks", count, job.TotalChunks)
	}

	job.Status = "processing"
	s.storeJob(ctx, job)

	s.db.ExecContext(ctx, `
		UPDATE mailing_suppression_import_jobs SET status = 'processing', started_at = NOW() WHERE id = $1
	`, jobID)

	// Launch background goroutine
	go s.processFile(context.Background(), job)

	log.Printf("[SuppImport] Job %s: background processing started for %s", jobID, job.Filename)
	return nil
}

// processFile is the background worker that streams the file and bulk-inserts.
// Architecture: pipelined reader → N parallel DB writer goroutines for maximum throughput.
// The reader goroutine parses lines and fills batches; writer goroutines consume batches
// and perform COPY inserts concurrently. This keeps the disk I/O and DB I/O overlapped.
func (s *SuppressionImportService) processFile(ctx context.Context, job *SuppImportJob) {
	startTime := time.Now()
	jobID := job.ID

	file, err := os.Open(job.TempFilePath)
	if err != nil {
		s.failJob(ctx, jobID, fmt.Sprintf("failed to open file: %v", err))
		return
	}
	defer file.Close()
	defer os.Remove(job.TempFilePath)

	s.setProgress(ctx, jobID, &SuppImportProgress{
		JobID:     jobID,
		Status:    "processing",
		Phase:     "processing",
		StartedAt: startTime,
		UpdatedAt: startTime,
	})

	// Disable autovacuum and synchronous_commit for this session to speed up bulk inserts
	s.db.ExecContext(ctx, `SET synchronous_commit = OFF`)
	defer s.db.ExecContext(ctx, `SET synchronous_commit = ON`)

	// --- Pipeline: reader goroutine → batch channel → N writer goroutines ---
	type batchWork struct {
		rows []suppRow
	}
	batchCh := make(chan batchWork, SuppChannelBuffer)

	var (
		totalLines   int64
		imported     int64
		duplicates   int64
		invalid      int64
		sampleErrors []string
		sampleMu     sync.Mutex
	)

	// Start writer goroutines
	var writerWg sync.WaitGroup
	for w := 0; w < SuppWriterWorkers; w++ {
		writerWg.Add(1)
		go func() {
			defer writerWg.Done()
			for work := range batchCh {
				imp, dup := s.insertBatchCopy(ctx, job.ListID, work.rows)
				atomic.AddInt64(&imported, int64(imp))
				atomic.AddInt64(&duplicates, int64(dup))
			}
		}()
	}

	// Reader goroutine: parse file and dispatch batches
	go func() {
		defer close(batchCh)

		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024) // 2MB line buffer

		var (
			batch       = make([]suppRow, 0, SuppBatchSize)
			isFirstLine = true
		)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			lineNum := atomic.AddInt64(&totalLines, 1)

			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			// --- CSV-aware pre-processing ---
			// Skip CSV header row (e.g. "Email,Time" or "email,timestamp,reason")
			if isFirstLine {
				isFirstLine = false
				if strings.Contains(line, ",") && strings.Contains(strings.ToLower(line), "email") {
					continue // header row, skip
				}
			}

			// Extract first column from CSV lines (e.g. "md5hash","2024-11-19 01:06:12")
			if strings.Contains(line, ",") {
				line = strings.TrimSpace(strings.SplitN(line, ",", 2)[0])
			}

			// Strip surrounding double-quotes (e.g. "0000031e..." → 0000031e...)
			if len(line) >= 2 && line[0] == '"' && line[len(line)-1] == '"' {
				line = strings.TrimSpace(line[1 : len(line)-1])
			}

			// Parse: email or MD5 hash
			var email, md5Hash string
			if strings.Contains(line, "@") {
				email = strings.ToLower(line)
				hash := md5.Sum([]byte(email))
				md5Hash = hex.EncodeToString(hash[:])
			} else if len(line) == 32 && isHexString(line) {
				md5Hash = strings.ToLower(line)
			} else {
				atomic.AddInt64(&invalid, 1)
				sampleMu.Lock()
				if len(sampleErrors) < 20 {
					sampleErrors = append(sampleErrors, fmt.Sprintf("line %d: unrecognized format: %.60s", lineNum, line))
				}
				sampleMu.Unlock()
				continue
			}

			batch = append(batch, suppRow{email: email, md5Hash: md5Hash})

			if len(batch) >= SuppBatchSize {
				// Send batch to writers — copy slice to avoid data race
				outBatch := make([]suppRow, len(batch))
				copy(outBatch, batch)
				batchCh <- batchWork{rows: outBatch}
				batch = batch[:0]
			}

			if lineNum%int64(SuppProgressInterval) == 0 {
				elapsed := time.Since(startTime).Seconds()
				rate := float64(lineNum) / elapsed
				var eta int64
				if rate > 0 && job.FileSize > 0 {
					processedFraction := float64(lineNum) / (float64(job.FileSize) / 35.0)
					if processedFraction > 0 && processedFraction < 1.0 {
						eta = int64((1.0 - processedFraction) / processedFraction * elapsed)
					}
				}

				s.setProgress(ctx, jobID, &SuppImportProgress{
					JobID:          jobID,
					Status:         "processing",
					Phase:          "processing",
					TotalLines:     lineNum,
					ProcessedRows:  lineNum,
					ImportedCount:  atomic.LoadInt64(&imported),
					DuplicateCount: atomic.LoadInt64(&duplicates),
					InvalidCount:   atomic.LoadInt64(&invalid),
					RowsPerSecond:  rate,
					EstimatedETA:   eta,
					StartedAt:      startTime,
					UpdatedAt:      time.Now(),
				})
			}
		}

		// Flush remaining
		if len(batch) > 0 {
			batchCh <- batchWork{rows: batch}
		}

		if err := scanner.Err(); err != nil {
			sampleMu.Lock()
			if len(sampleErrors) < 20 {
				sampleErrors = append(sampleErrors, fmt.Sprintf("scanner error: %v", err))
			}
			sampleMu.Unlock()
		}
	}()

	// Wait for all writers to finish
	writerWg.Wait()

	// Re-enable synchronous_commit before final updates
	s.db.ExecContext(ctx, `SET synchronous_commit = ON`)

	// Update suppression list entry count
	s.db.ExecContext(ctx, `
		UPDATE mailing_suppression_lists
		SET entry_count = (SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = $1),
		    updated_at = NOW()
		WHERE id = $1
	`, job.ListID)

	finalLines := atomic.LoadInt64(&totalLines)
	finalImported := atomic.LoadInt64(&imported)
	finalDuplicates := atomic.LoadInt64(&duplicates)
	finalInvalid := atomic.LoadInt64(&invalid)

	duration := time.Since(startTime)
	rowsPerSec := 0.0
	if duration.Seconds() > 0 {
		rowsPerSec = float64(finalLines) / duration.Seconds()
	}
	now := time.Now()

	sampleMu.Lock()
	finalErrors := make([]string, len(sampleErrors))
	copy(finalErrors, sampleErrors)
	sampleMu.Unlock()

	s.setProgress(ctx, jobID, &SuppImportProgress{
		JobID:          jobID,
		Status:         "completed",
		Phase:          "completed",
		TotalLines:     finalLines,
		ProcessedRows:  finalLines,
		ImportedCount:  finalImported,
		DuplicateCount: finalDuplicates,
		InvalidCount:   finalInvalid,
		RowsPerSecond:  rowsPerSec,
		EstimatedETA:   0,
		StartedAt:      startTime,
		UpdatedAt:      now,
		CompletedAt:    &now,
		Errors:         finalErrors,
	})

	s.db.ExecContext(ctx, `
		UPDATE mailing_suppression_import_jobs
		SET status = 'completed', total_lines = $1, imported_count = $2,
		    duplicate_count = $3, invalid_count = $4, completed_at = NOW()
		WHERE id = $5
	`, finalLines, finalImported, finalDuplicates, finalInvalid, jobID)

	job.Status = "completed"
	s.storeJob(ctx, job)

	log.Printf("[SuppImport] Job %s COMPLETE: %d lines, %d imported, %d dups, %d invalid in %.1fs (%.0f rows/sec)",
		jobID, finalLines, finalImported, finalDuplicates, finalInvalid, duration.Seconds(), rowsPerSec)
}

// =============================================================================
// BULK INSERT via PostgreSQL COPY
// =============================================================================

type suppRow struct {
	email   string
	md5Hash string
}

// insertBatchCopy uses PostgreSQL COPY protocol for maximum throughput.
// Uses UNLOGGED temp table and work_mem boost for faster bulk operations.
// Falls back to multi-row INSERT if COPY fails.
func (s *SuppressionImportService) insertBatchCopy(ctx context.Context, listID string, batch []suppRow) (imported, duplicates int) {
	if len(batch) == 0 {
		return 0, 0
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("[SuppImport] BeginTx error: %v", err)
		return s.insertBatchFallback(ctx, listID, batch)
	}
	defer tx.Rollback()

	// Boost work_mem for this transaction's sort/hash operations
	tx.ExecContext(ctx, `SET LOCAL work_mem = '256MB'`)

	// Create UNLOGGED session-scoped temp table (no WAL = faster writes)
	_, err = tx.ExecContext(ctx, `
		CREATE TEMP TABLE _supp_import_batch (
			email VARCHAR(255),
			md5_hash VARCHAR(64)
		) ON COMMIT DROP
	`)
	if err != nil {
		log.Printf("[SuppImport] Create temp table error: %v", err)
		return s.insertBatchFallback(ctx, listID, batch)
	}

	// COPY into temp table
	stmt, err := tx.Prepare(pq.CopyIn("_supp_import_batch", "email", "md5_hash"))
	if err != nil {
		log.Printf("[SuppImport] COPY prepare error: %v", err)
		return s.insertBatchFallback(ctx, listID, batch)
	}

	for _, row := range batch {
		if _, err := stmt.Exec(row.email, row.md5Hash); err != nil {
			log.Printf("[SuppImport] COPY exec error: %v", err)
			stmt.Close()
			return s.insertBatchFallback(ctx, listID, batch)
		}
	}

	if _, err := stmt.Exec(); err != nil {
		log.Printf("[SuppImport] COPY flush error: %v", err)
		stmt.Close()
		return s.insertBatchFallback(ctx, listID, batch)
	}
	stmt.Close()

	// Merge from temp table into real table
	result, err := tx.ExecContext(ctx, `
		INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source, created_at)
		SELECT gen_random_uuid()::text, $1, b.email, b.md5_hash, 'Import', 'bulk_import', NOW()
		FROM _supp_import_batch b
		WHERE b.md5_hash IS NOT NULL
		ON CONFLICT (list_id, md5_hash) DO NOTHING
	`, listID)
	if err != nil {
		log.Printf("[SuppImport] Merge INSERT error: %v", err)
		return s.insertBatchFallback(ctx, listID, batch)
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[SuppImport] Commit error: %v", err)
		return s.insertBatchFallback(ctx, listID, batch)
	}

	rowsAffected, _ := result.RowsAffected()
	imported = int(rowsAffected)
	duplicates = len(batch) - imported
	return imported, duplicates
}

// insertBatchFallback uses multi-row INSERT as a fallback
func (s *SuppressionImportService) insertBatchFallback(ctx context.Context, listID string, batch []suppRow) (imported, duplicates int) {
	if len(batch) == 0 {
		return 0, 0
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0
	}
	defer tx.Rollback()

	for _, row := range batch {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source, created_at)
			VALUES (gen_random_uuid()::text, $1, $2, $3, 'Import', 'bulk_import', NOW())
			ON CONFLICT (list_id, md5_hash) DO NOTHING
		`, listID, nullableString(row.email), row.md5Hash)
		if err == nil {
			rows, _ := result.RowsAffected()
			if rows > 0 {
				imported++
			} else {
				duplicates++
			}
		}
	}

	tx.Commit()
	return imported, duplicates
}

// =============================================================================
// DIRECT UPLOAD (small files, no chunking needed)
// =============================================================================

// ProcessDirectUpload handles files uploaded in a single request.
// Streams to disk, then processes in a background goroutine.
func (s *SuppressionImportService) ProcessDirectUpload(ctx context.Context, listID string, reader io.Reader, filename string, fileSize int64) (string, error) {
	jobID := uuid.New().String()

	tempPath := filepath.Join(s.dir, fmt.Sprintf("%s.txt", jobID))
	f, err := os.Create(tempPath)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	written, err := io.Copy(f, reader)
	f.Close()
	if err != nil {
		os.Remove(tempPath)
		return "", fmt.Errorf("failed to write upload: %w", err)
	}

	job := &SuppImportJob{
		ID:             jobID,
		ListID:         listID,
		Filename:       filename,
		FileSize:       written,
		TempFilePath:   tempPath,
		Status:         "processing",
		CreatedAt:      time.Now(),
		ExpiresAt:      time.Now().Add(SuppSessionTTL),
		TotalChunks:    1,
		UploadedChunks: []int{0},
	}

	s.storeJob(ctx, job)

	s.db.ExecContext(ctx, `
		INSERT INTO mailing_suppression_import_jobs (id, list_id, filename, file_size, status, started_at, created_at)
		VALUES ($1, $2, $3, $4, 'processing', NOW(), NOW())
	`, jobID, listID, filename, written)

	go s.processFile(context.Background(), job)

	log.Printf("[SuppImport] Direct upload job %s: %s (%d bytes)", jobID, filename, written)
	return jobID, nil
}

// =============================================================================
// JOB & PROGRESS QUERIES
// =============================================================================

// GetJob retrieves a job from Redis or in-memory store
func (s *SuppressionImportService) GetJob(ctx context.Context, jobID string) (*SuppImportJob, error) {
	// Try Redis first
	if s.hasRedis() {
		data, err := s.redis.Get(ctx, s.jobKey(jobID)).Bytes()
		if err == nil {
			var job SuppImportJob
			if err := json.Unmarshal(data, &job); err == nil {
				return &job, nil
			}
		}
	}

	// Fallback to in-memory
	s.mu.RLock()
	job, ok := s.jobs[jobID]
	s.mu.RUnlock()
	if ok {
		return job, nil
	}

	return nil, fmt.Errorf("import job not found: %s", jobID)
}

// GetProgress retrieves the current progress of an import job
func (s *SuppressionImportService) GetProgress(ctx context.Context, jobID string) (*SuppImportProgress, error) {
	// Try Redis first
	if s.hasRedis() {
		data, err := s.redis.Get(ctx, s.progressKey(jobID)).Bytes()
		if err == nil {
			var p SuppImportProgress
			if err := json.Unmarshal(data, &p); err == nil {
				return &p, nil
			}
		}
	}

	// Fallback to in-memory
	s.mu.RLock()
	p, ok := s.progress[jobID]
	s.mu.RUnlock()
	if ok {
		return p, nil
	}

	// Fallback to DB
	var status string
	var totalLines, importedCount, duplicateCount, invalidCount int64
	err := s.db.QueryRowContext(ctx, `
		SELECT status, COALESCE(total_lines, 0), COALESCE(imported_count, 0),
		       COALESCE(duplicate_count, 0), COALESCE(invalid_count, 0)
		FROM mailing_suppression_import_jobs WHERE id = $1
	`, jobID).Scan(&status, &totalLines, &importedCount, &duplicateCount, &invalidCount)
	if err != nil {
		return &SuppImportProgress{JobID: jobID, Status: "unknown"}, nil
	}
	return &SuppImportProgress{
		JobID:          jobID,
		Status:         status,
		Phase:          status,
		TotalLines:     totalLines,
		ImportedCount:  importedCount,
		DuplicateCount: duplicateCount,
		InvalidCount:   invalidCount,
	}, nil
}

// =============================================================================
// HELPERS
// =============================================================================

func (s *SuppressionImportService) jobKey(jobID string) string {
	return fmt.Sprintf("supp_import:job:%s", jobID)
}

func (s *SuppressionImportService) progressKey(jobID string) string {
	return fmt.Sprintf("supp_import:progress:%s", jobID)
}

func (s *SuppressionImportService) chunkSetKey(jobID string) string {
	return fmt.Sprintf("supp_import:chunks:%s", jobID)
}

func (s *SuppressionImportService) failJob(ctx context.Context, jobID, errMsg string) {
	log.Printf("[SuppImport] Job %s FAILED: %s", jobID, errMsg)

	s.setProgress(ctx, jobID, &SuppImportProgress{
		JobID:     jobID,
		Status:    "failed",
		Phase:     "failed",
		Errors:    []string{errMsg},
		UpdatedAt: time.Now(),
	})

	s.db.ExecContext(ctx, `
		UPDATE mailing_suppression_import_jobs SET status = 'failed', error_message = $1, completed_at = NOW() WHERE id = $2
	`, errMsg, jobID)
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
