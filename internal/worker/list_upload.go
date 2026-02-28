package worker

import (
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// LIST UPLOAD SERVICE - Large File CSV Processing
// =============================================================================
// Handles CSV uploads up to 10GB with:
// - Header detection and validation
// - Field mapping to system fields
// - Streaming processing (never loads entire file into memory)
// - Chunked/resumable uploads
// - Real-time progress tracking via Redis
// - Batch database inserts for performance

var (
	ErrNoHeaders            = errors.New("no headers detected in CSV file")
	ErrEmptyFile            = errors.New("file is empty")
	ErrInvalidCSV           = errors.New("invalid CSV format")
	ErrMissingEmailColumn   = errors.New("email column mapping is required")
	ErrUploadNotFound       = errors.New("upload session not found")
	ErrUploadExpired        = errors.New("upload session has expired")
	ErrChunkMismatch        = errors.New("chunk number mismatch")
	ErrInvalidChunkSize     = errors.New("invalid chunk size")
	ErrUploadAlreadyComplete = errors.New("upload already complete")
)

// =============================================================================
// CONFIGURATION
// =============================================================================

const (
	// Upload limits
	MaxFileSize        = 10 * 1024 * 1024 * 1024 // 10GB
	MaxChunkSize       = 50 * 1024 * 1024        // 50MB chunks
	MinChunkSize       = 1 * 1024 * 1024         // 1MB minimum
	DefaultChunkSize   = 10 * 1024 * 1024        // 10MB default

	// Processing settings
	BatchInsertSize    = 5000                    // Rows per batch insert
	ProgressUpdateFreq = 1000                    // Update progress every N rows
	UploadSessionTTL   = 24 * time.Hour          // Session expiry
	TempUploadDir      = "/tmp/mailing-uploads"  // Temporary upload directory

	// Header detection
	MinHeaderConfidence = 0.6                    // 60% confidence threshold
)

// SystemFields defines the standard fields that can be mapped
var SystemFields = []FieldDefinition{
	{Name: "email", Label: "Email Address", Required: true, Type: "email"},
	{Name: "first_name", Label: "First Name", Required: false, Type: "text"},
	{Name: "last_name", Label: "Last Name", Required: false, Type: "text"},
	{Name: "phone", Label: "Phone Number", Required: false, Type: "phone"},
	{Name: "company", Label: "Company", Required: false, Type: "text"},
	{Name: "job_title", Label: "Job Title", Required: false, Type: "text"},
	{Name: "city", Label: "City", Required: false, Type: "text"},
	{Name: "state", Label: "State/Province", Required: false, Type: "text"},
	{Name: "country", Label: "Country", Required: false, Type: "text"},
	{Name: "postal_code", Label: "Postal Code", Required: false, Type: "text"},
	{Name: "timezone", Label: "Timezone", Required: false, Type: "timezone"},
	{Name: "language", Label: "Language", Required: false, Type: "text"},
	{Name: "industry", Label: "Industry", Required: false, Type: "text"},
	{Name: "source", Label: "Source", Required: false, Type: "text"},
	{Name: "tags", Label: "Tags", Required: false, Type: "text"},
	{Name: "birthdate", Label: "Birth Date", Required: false, Type: "date"},
	{Name: "subscribed_at", Label: "Subscribed Date", Required: false, Type: "datetime"},
}

// Common header aliases for auto-mapping
var headerAliases = map[string][]string{
	"email":       {"email", "email_address", "e-mail", "emailaddress", "mail", "subscriber_email"},
	"first_name":  {"first_name", "firstname", "first", "fname", "given_name", "givenname"},
	"last_name":   {"last_name", "lastname", "last", "lname", "surname", "family_name", "familyname"},
	"phone":       {"phone", "phone_number", "phonenumber", "mobile", "cell", "telephone", "tel"},
	"company":     {"company", "company_name", "companyname", "organization", "org", "business"},
	"job_title":   {"job_title", "jobtitle", "title", "position", "role", "job"},
	"city":        {"city", "town", "locality"},
	"state":       {"state", "state_province", "province", "region"},
	"country":     {"country", "nation", "country_code"},
	"postal_code": {"postal_code", "postalcode", "zip", "zipcode", "zip_code", "postcode"},
	"timezone":    {"timezone", "time_zone", "tz"},
	"language":    {"language", "lang", "locale"},
	"industry":    {"industry", "sector", "vertical"},
	"source":      {"source", "lead_source", "leadsource", "signup_source", "origin"},
	"tags":        {"tags", "labels", "categories"},
	"birthdate":   {"birthdate", "birth_date", "birthday", "dob", "date_of_birth"},
}

// =============================================================================
// TYPES
// =============================================================================

// FieldDefinition describes a system field that can be mapped
type FieldDefinition struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
	Type     string `json:"type"` // email, text, phone, date, datetime, timezone
}

// FieldMapping maps a CSV column to a system field
type FieldMapping struct {
	ColumnIndex int    `json:"column_index"`
	ColumnName  string `json:"column_name"`
	SystemField string `json:"system_field"` // empty means custom field
	CustomField string `json:"custom_field"` // name of custom field if not system
}

// HeaderDetectionResult contains the results of header analysis
type HeaderDetectionResult struct {
	HasHeaders         bool              `json:"has_headers"`
	Confidence         float64           `json:"confidence"`
	Headers            []string          `json:"headers"`
	SuggestedMappings  []FieldMapping    `json:"suggested_mappings"`
	SampleRows         [][]string        `json:"sample_rows"`
	TotalColumns       int               `json:"total_columns"`
	DetectionMethod    string            `json:"detection_method"`
	RejectionReason    string            `json:"rejection_reason,omitempty"`
}

// UploadSession tracks a chunked upload in progress
type UploadSession struct {
	ID              string            `json:"id"`
	OrganizationID  string            `json:"organization_id"`
	ListID          string            `json:"list_id"`
	Filename        string            `json:"filename"`
	FileSize        int64             `json:"file_size"`
	ChunkSize       int64             `json:"chunk_size"`
	TotalChunks     int               `json:"total_chunks"`
	UploadedChunks  []int             `json:"uploaded_chunks"`
	FieldMapping    []FieldMapping    `json:"field_mapping"`
	UpdateExisting  bool              `json:"update_existing"`
	TempFilePath    string            `json:"temp_file_path"`
	CreatedAt       time.Time         `json:"created_at"`
	ExpiresAt       time.Time         `json:"expires_at"`
	Status          string            `json:"status"` // pending, uploading, processing, completed, failed
	Error           string            `json:"error,omitempty"`
}

// UploadProgress tracks processing progress
type UploadProgress struct {
	SessionID       string    `json:"session_id"`
	Status          string    `json:"status"`
	Phase           string    `json:"phase"` // uploading, validating, importing
	TotalRows       int64     `json:"total_rows"`
	ProcessedRows   int64     `json:"processed_rows"`
	ImportedCount   int64     `json:"imported_count"`
	UpdatedCount    int64     `json:"updated_count"`
	SkippedCount    int64     `json:"skipped_count"`
	ErrorCount      int64     `json:"error_count"`
	BytesUploaded   int64     `json:"bytes_uploaded"`
	TotalBytes      int64     `json:"total_bytes"`
	CurrentRate     float64   `json:"current_rate"`     // rows per second
	EstimatedETA    int64     `json:"estimated_eta"`    // seconds remaining
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Errors          []string  `json:"errors,omitempty"` // Sample of errors
}

// ImportResult contains final import statistics
type ImportResult struct {
	JobID           string    `json:"job_id"`
	ListID          string    `json:"list_id"`
	TotalRows       int64     `json:"total_rows"`
	ImportedCount   int64     `json:"imported_count"`
	UpdatedCount    int64     `json:"updated_count"`
	SkippedCount    int64     `json:"skipped_count"`
	ErrorCount      int64     `json:"error_count"`
	DurationSeconds float64   `json:"duration_seconds"`
	RowsPerSecond   float64   `json:"rows_per_second"`
	Errors          []string  `json:"errors,omitempty"`
	CompletedAt     time.Time `json:"completed_at"`
}

// =============================================================================
// LIST UPLOAD SERVICE
// =============================================================================

// ListUploadService handles large CSV file uploads and imports
type ListUploadService struct {
	db    *sql.DB
	redis *redis.Client

	// Temp file management
	uploadDir string
	mu        sync.Mutex

	// Email validation regex
	emailRegex *regexp.Regexp
}

// NewListUploadService creates a new upload service
func NewListUploadService(db *sql.DB, redisClient *redis.Client) *ListUploadService {
	// Ensure upload directory exists
	os.MkdirAll(TempUploadDir, 0755)

	return &ListUploadService{
		db:         db,
		redis:      redisClient,
		uploadDir:  TempUploadDir,
		emailRegex: regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`),
	}
}

// =============================================================================
// HEADER DETECTION
// =============================================================================

// DetectHeaders analyzes a CSV file to detect if it has headers
// Returns detailed information including rejection reason if no headers found
func (s *ListUploadService) DetectHeaders(reader io.Reader) (*HeaderDetectionResult, error) {
	// Use a buffered reader to allow re-reading
	bufReader := bufio.NewReader(reader)

	// Read first few lines for analysis
	csvReader := csv.NewReader(bufReader)
	csvReader.FieldsPerRecord = -1 // Allow variable fields
	csvReader.LazyQuotes = true
	csvReader.TrimLeadingSpace = true

	// Read first row (potential header)
	firstRow, err := csvReader.Read()
	if err != nil {
		if err == io.EOF {
			return nil, ErrEmptyFile
		}
		return nil, fmt.Errorf("failed to read CSV: %w", err)
	}

	if len(firstRow) == 0 {
		return nil, ErrEmptyFile
	}

	// Read sample data rows (up to 5)
	var sampleRows [][]string
	for i := 0; i < 5; i++ {
		row, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		sampleRows = append(sampleRows, row)
	}

	// Analyze for header presence
	result := s.analyzeHeaders(firstRow, sampleRows)

	// If no headers detected, prepare rejection
	if !result.HasHeaders {
		result.RejectionReason = fmt.Sprintf(
			"No headers detected (confidence: %.0f%%). "+
				"The first row appears to be data, not column headers. "+
				"CSV files must have a header row with column names.",
			result.Confidence*100,
		)
	}

	return result, nil
}

// analyzeHeaders determines if the first row contains headers
func (s *ListUploadService) analyzeHeaders(firstRow []string, sampleRows [][]string) *HeaderDetectionResult {
	result := &HeaderDetectionResult{
		Headers:      firstRow,
		TotalColumns: len(firstRow),
		SampleRows:   sampleRows,
	}

	// Multiple detection strategies
	var scores []float64
	var methods []string

	// Strategy 1: Check for known header patterns
	knownHeaderScore := s.scoreKnownHeaders(firstRow)
	scores = append(scores, knownHeaderScore)
	methods = append(methods, fmt.Sprintf("known_headers:%.2f", knownHeaderScore))

	// Strategy 2: Check if first row differs from data rows
	if len(sampleRows) > 0 {
		typeConsistencyScore := s.scoreTypeConsistency(firstRow, sampleRows)
		scores = append(scores, typeConsistencyScore)
		methods = append(methods, fmt.Sprintf("type_consistency:%.2f", typeConsistencyScore))
	}

	// Strategy 3: Check for email patterns
	emailPatternScore := s.scoreEmailPattern(firstRow, sampleRows)
	scores = append(scores, emailPatternScore)
	methods = append(methods, fmt.Sprintf("email_pattern:%.2f", emailPatternScore))

	// Strategy 4: Check for numeric patterns (data often has numbers, headers don't)
	numericScore := s.scoreNumericPattern(firstRow, sampleRows)
	scores = append(scores, numericScore)
	methods = append(methods, fmt.Sprintf("numeric:%.2f", numericScore))

	// Calculate weighted average confidence
	var totalScore float64
	weights := []float64{0.4, 0.3, 0.2, 0.1} // Known headers weighted highest
	for i, score := range scores {
		if i < len(weights) {
			totalScore += score * weights[i]
		}
	}

	result.Confidence = totalScore
	result.HasHeaders = totalScore >= MinHeaderConfidence
	result.DetectionMethod = strings.Join(methods, ", ")

	// If headers detected, suggest field mappings
	if result.HasHeaders {
		result.SuggestedMappings = s.suggestMappings(firstRow)
	}

	return result
}

// scoreKnownHeaders checks how many headers match known field names
func (s *ListUploadService) scoreKnownHeaders(headers []string) float64 {
	if len(headers) == 0 {
		return 0
	}

	matched := 0
	for _, header := range headers {
		normalized := normalizeHeader(header)
		for _, aliases := range headerAliases {
			for _, alias := range aliases {
				if normalized == alias {
					matched++
					break
				}
			}
		}
	}

	return float64(matched) / float64(len(headers))
}

// scoreTypeConsistency checks if first row has different data types than data rows
func (s *ListUploadService) scoreTypeConsistency(firstRow []string, dataRows [][]string) float64 {
	if len(dataRows) == 0 {
		return 0.5 // Neutral if no data to compare
	}

	differentColumns := 0
	for colIdx := range firstRow {
		if colIdx >= len(firstRow) {
			continue
		}

		firstValue := firstRow[colIdx]
		isFirstNumeric := isNumericString(firstValue)
		isFirstEmail := strings.Contains(firstValue, "@")

		// Check if data rows are consistently different
		numericCount := 0
		emailCount := 0
		for _, row := range dataRows {
			if colIdx < len(row) {
				if isNumericString(row[colIdx]) {
					numericCount++
				}
				if strings.Contains(row[colIdx], "@") {
					emailCount++
				}
			}
		}

		// If first row is not numeric but data is mostly numeric
		if !isFirstNumeric && numericCount >= len(dataRows)/2 {
			differentColumns++
		}

		// If first row is not email but data contains emails
		if !isFirstEmail && emailCount >= len(dataRows)/2 {
			differentColumns++
		}
	}

	if len(firstRow) == 0 {
		return 0
	}
	return float64(differentColumns) / float64(len(firstRow))
}

// scoreEmailPattern checks if emails appear in data rows but not header
func (s *ListUploadService) scoreEmailPattern(firstRow []string, dataRows [][]string) float64 {
	// Check if any header cell looks like an email
	headerHasEmail := false
	for _, cell := range firstRow {
		if s.emailRegex.MatchString(strings.TrimSpace(cell)) {
			headerHasEmail = true
			break
		}
	}

	// Check if data rows contain emails
	dataHasEmail := false
	for _, row := range dataRows {
		for _, cell := range row {
			if s.emailRegex.MatchString(strings.TrimSpace(cell)) {
				dataHasEmail = true
				break
			}
		}
		if dataHasEmail {
			break
		}
	}

	// If header has email but data also has emails, likely no header
	if headerHasEmail && dataHasEmail {
		return 0.0
	}

	// If header has no email but data has emails, likely has header
	if !headerHasEmail && dataHasEmail {
		return 1.0
	}

	// Neutral
	return 0.5
}

// scoreNumericPattern checks numeric patterns
func (s *ListUploadService) scoreNumericPattern(firstRow []string, dataRows [][]string) float64 {
	headerNumericCount := 0
	for _, cell := range firstRow {
		if isNumericString(cell) {
			headerNumericCount++
		}
	}

	// Headers shouldn't be mostly numeric
	if len(firstRow) > 0 && float64(headerNumericCount)/float64(len(firstRow)) > 0.5 {
		return 0.0 // Probably data, not header
	}

	return 0.7 // Likely header
}

// suggestMappings auto-maps headers to system fields
func (s *ListUploadService) suggestMappings(headers []string) []FieldMapping {
	var mappings []FieldMapping

	for colIdx, header := range headers {
		normalized := normalizeHeader(header)
		mapping := FieldMapping{
			ColumnIndex: colIdx,
			ColumnName:  header,
		}

		// Try to match to system field
		matched := false
		for systemField, aliases := range headerAliases {
			for _, alias := range aliases {
				if normalized == alias {
					mapping.SystemField = systemField
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}

		// If no match, suggest as custom field
		if !matched && normalized != "" {
			mapping.CustomField = normalized
		}

		mappings = append(mappings, mapping)
	}

	return mappings
}

// =============================================================================
// CHUNKED UPLOAD MANAGEMENT
// =============================================================================

// InitUploadSession creates a new upload session for chunked uploads
func (s *ListUploadService) InitUploadSession(ctx context.Context, orgID, listID, filename string, fileSize, chunkSize int64) (*UploadSession, error) {
	// Validate parameters
	if fileSize > MaxFileSize {
		return nil, fmt.Errorf("file size %d exceeds maximum %d bytes", fileSize, MaxFileSize)
	}

	if chunkSize == 0 {
		chunkSize = DefaultChunkSize
	}
	if chunkSize < MinChunkSize {
		chunkSize = MinChunkSize
	}
	if chunkSize > MaxChunkSize {
		chunkSize = MaxChunkSize
	}

	// Calculate total chunks
	totalChunks := int((fileSize + chunkSize - 1) / chunkSize)

	// Create session
	sessionID := uuid.New().String()
	session := &UploadSession{
		ID:             sessionID,
		OrganizationID: orgID,
		ListID:         listID,
		Filename:       filename,
		FileSize:       fileSize,
		ChunkSize:      chunkSize,
		TotalChunks:    totalChunks,
		UploadedChunks: []int{},
		TempFilePath:   filepath.Join(s.uploadDir, fmt.Sprintf("%s.csv", sessionID)),
		CreatedAt:      time.Now(),
		ExpiresAt:      time.Now().Add(UploadSessionTTL),
		Status:         "pending",
	}

	// Store session in Redis
	sessionJSON, _ := json.Marshal(session)
	err := s.redis.Set(ctx, s.sessionKey(sessionID), sessionJSON, UploadSessionTTL).Err()
	if err != nil {
		return nil, fmt.Errorf("failed to store upload session: %w", err)
	}

	// Create temporary file
	f, err := os.Create(session.TempFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	f.Close()

	// Pre-allocate file to expected size (sparse file)
	os.Truncate(session.TempFilePath, fileSize)

	log.Printf("[ListUpload] Created upload session %s: file=%s, size=%d, chunks=%d",
		sessionID, filename, fileSize, totalChunks)

	return session, nil
}

// UploadChunk handles a single chunk upload
func (s *ListUploadService) UploadChunk(ctx context.Context, sessionID string, chunkNumber int, data []byte) error {
	// Get session
	session, err := s.GetUploadSession(ctx, sessionID)
	if err != nil {
		return err
	}

	if session.Status == "completed" {
		return ErrUploadAlreadyComplete
	}

	// Validate chunk number
	if chunkNumber < 0 || chunkNumber >= session.TotalChunks {
		return fmt.Errorf("invalid chunk number %d (total: %d)", chunkNumber, session.TotalChunks)
	}

	// Calculate offset
	offset := int64(chunkNumber) * session.ChunkSize

	// Write chunk to file at correct offset
	f, err := os.OpenFile(session.TempFilePath, os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open temp file: %w", err)
	}
	defer f.Close()

	_, err = f.WriteAt(data, offset)
	if err != nil {
		return fmt.Errorf("failed to write chunk: %w", err)
	}

	// Use Redis SADD for atomic chunk tracking (avoids race conditions)
	chunkSetKey := s.chunkSetKey(sessionID)
	s.redis.SAdd(ctx, chunkSetKey, chunkNumber)
	s.redis.Expire(ctx, chunkSetKey, UploadSessionTTL)

	// Update session status (non-critical, can be stale)
	session.Status = "uploading"
	sessionJSON, _ := json.Marshal(session)
	s.redis.Set(ctx, s.sessionKey(sessionID), sessionJSON, UploadSessionTTL)

	// Get current chunk count for progress
	chunkCount, _ := s.redis.SCard(ctx, chunkSetKey).Result()

	// Update progress
	progress := &UploadProgress{
		SessionID:     sessionID,
		Status:        "uploading",
		Phase:         "uploading",
		BytesUploaded: chunkCount * session.ChunkSize,
		TotalBytes:    session.FileSize,
		UpdatedAt:     time.Now(),
	}
	s.updateProgress(ctx, sessionID, progress)

	log.Printf("[ListUpload] Session %s: chunk %d/%d uploaded", sessionID, chunkNumber+1, session.TotalChunks)

	return nil
}

// GetUploadSession retrieves an upload session
func (s *ListUploadService) GetUploadSession(ctx context.Context, sessionID string) (*UploadSession, error) {
	data, err := s.redis.Get(ctx, s.sessionKey(sessionID)).Bytes()
	if err == redis.Nil {
		return nil, ErrUploadNotFound
	}
	if err != nil {
		return nil, err
	}

	var session UploadSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}

	if time.Now().After(session.ExpiresAt) {
		return nil, ErrUploadExpired
	}

	// Get uploaded chunks from Redis set (atomic tracking)
	chunkSetKey := s.chunkSetKey(sessionID)
	members, _ := s.redis.SMembers(ctx, chunkSetKey).Result()
	session.UploadedChunks = make([]int, 0, len(members))
	for _, m := range members {
		var chunkNum int
		fmt.Sscanf(m, "%d", &chunkNum)
		session.UploadedChunks = append(session.UploadedChunks, chunkNum)
	}

	return &session, nil
}

// IsUploadComplete checks if all chunks have been uploaded
func (s *ListUploadService) IsUploadComplete(ctx context.Context, sessionID string) (bool, error) {
	session, err := s.GetUploadSession(ctx, sessionID)
	if err != nil {
		return false, err
	}

	// Use Redis SCARD for accurate count
	chunkCount, err := s.redis.SCard(ctx, s.chunkSetKey(sessionID)).Result()
	if err != nil {
		return false, err
	}

	return int(chunkCount) == session.TotalChunks, nil
}

// GetProgress retrieves current upload/import progress
func (s *ListUploadService) GetProgress(ctx context.Context, sessionID string) (*UploadProgress, error) {
	data, err := s.redis.Get(ctx, s.progressKey(sessionID)).Bytes()
	if err == redis.Nil {
		return &UploadProgress{SessionID: sessionID, Status: "unknown"}, nil
	}
	if err != nil {
		return nil, err
	}

	var progress UploadProgress
	if err := json.Unmarshal(data, &progress); err != nil {
		return nil, err
	}

	return &progress, nil
}

// =============================================================================
// STREAMING CSV PROCESSING
// =============================================================================

// ProcessUploadedFile validates and imports the uploaded CSV file
func (s *ListUploadService) ProcessUploadedFile(ctx context.Context, sessionID string, mapping []FieldMapping, updateExisting bool) (*ImportResult, error) {
	// Get session
	session, err := s.GetUploadSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// Verify upload is complete
	if len(session.UploadedChunks) != session.TotalChunks {
		return nil, fmt.Errorf("upload incomplete: %d/%d chunks", len(session.UploadedChunks), session.TotalChunks)
	}

	// Store mapping in session
	session.FieldMapping = mapping
	session.UpdateExisting = updateExisting
	session.Status = "processing"
	sessionJSON, _ := json.Marshal(session)
	s.redis.Set(ctx, s.sessionKey(sessionID), sessionJSON, UploadSessionTTL)

	// Open the temp file
	file, err := os.Open(session.TempFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open temp file: %w", err)
	}
	defer file.Close()

	// First pass: validate headers
	headerResult, err := s.DetectHeaders(file)
	if err != nil {
		return nil, err
	}

	if !headerResult.HasHeaders {
		s.updateSessionError(ctx, sessionID, ErrNoHeaders.Error())
		return nil, ErrNoHeaders
	}

	// Validate mapping has email
	hasEmail := false
	for _, m := range mapping {
		if m.SystemField == "email" {
			hasEmail = true
			break
		}
	}
	if !hasEmail {
		return nil, ErrMissingEmailColumn
	}

	// Reset file position
	file.Seek(0, 0)

	// Create import job in database
	jobID := uuid.New()
	listUUID, _ := uuid.Parse(session.ListID)
	orgUUID, _ := uuid.Parse(session.OrganizationID)

	mappingJSON, _ := json.Marshal(mapping)
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_import_jobs (id, organization_id, list_id, filename, field_mapping, status, started_at)
		VALUES ($1, $2, $3, $4, $5, 'processing', NOW())
	`, jobID, orgUUID, listUUID, session.Filename, mappingJSON)

	// Process the file
	result := s.processCSVStreaming(ctx, sessionID, jobID, listUUID, orgUUID, file, mapping, updateExisting)

	// Clean up temp file
	os.Remove(session.TempFilePath)

	// Update session status
	session.Status = "completed"
	sessionJSON, _ = json.Marshal(session)
	s.redis.Set(ctx, s.sessionKey(sessionID), sessionJSON, time.Hour) // Keep for 1 hour after completion

	return result, nil
}

// processCSVStreaming processes a large CSV file without loading it into memory
func (s *ListUploadService) processCSVStreaming(
	ctx context.Context,
	sessionID string,
	jobID, listID, orgID uuid.UUID,
	file io.Reader,
	mapping []FieldMapping,
	updateExisting bool,
) *ImportResult {
	startTime := time.Now()

	// Create buffered CSV reader
	csvReader := csv.NewReader(bufio.NewReaderSize(file, 1024*1024)) // 1MB buffer
	csvReader.FieldsPerRecord = -1
	csvReader.LazyQuotes = true
	csvReader.TrimLeadingSpace = true

	// Skip header row
	csvReader.Read()

	// Build field index map
	fieldMap := make(map[string]int)
	for _, m := range mapping {
		if m.SystemField != "" {
			fieldMap[m.SystemField] = m.ColumnIndex
		}
	}

	// Statistics
	var totalRows, imported, updated, skipped, errorCount int64
	var sampleErrors []string
	seenEmails := make(map[string]bool) // For deduplication within file

	// Batch processing
	var batch []subscriberRow
	const batchSize = BatchInsertSize

	// Progress tracking
	progress := &UploadProgress{
		SessionID: sessionID,
		Status:    "processing",
		Phase:     "importing",
		StartedAt: startTime,
	}

	// Process rows
	for {
		record, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			atomic.AddInt64(&errorCount, 1)
			if len(sampleErrors) < 10 {
				sampleErrors = append(sampleErrors, fmt.Sprintf("Row %d: parse error: %v", totalRows+1, err))
			}
			continue
		}

		atomic.AddInt64(&totalRows, 1)

		// Extract email (required)
		emailIdx, ok := fieldMap["email"]
		if !ok || emailIdx >= len(record) {
			atomic.AddInt64(&errorCount, 1)
			continue
		}

		email := strings.ToLower(strings.TrimSpace(record[emailIdx]))
		if email == "" || !s.emailRegex.MatchString(email) {
			atomic.AddInt64(&skipped, 1)
			if len(sampleErrors) < 10 {
				sampleErrors = append(sampleErrors, fmt.Sprintf("Row %d: invalid email '%s'", totalRows, email))
			}
			continue
		}

		// Check for duplicate within file
		if seenEmails[email] {
			atomic.AddInt64(&skipped, 1)
			continue
		}
		seenEmails[email] = true

		// Extract other fields
		row := subscriberRow{
			Email:     email,
			EmailHash: hashEmail(email),
		}

		if idx, ok := fieldMap["first_name"]; ok && idx < len(record) {
			row.FirstName = strings.TrimSpace(record[idx])
		}
		if idx, ok := fieldMap["last_name"]; ok && idx < len(record) {
			row.LastName = strings.TrimSpace(record[idx])
		}
		if idx, ok := fieldMap["phone"]; ok && idx < len(record) {
			row.Phone = strings.TrimSpace(record[idx])
		}
		if idx, ok := fieldMap["company"]; ok && idx < len(record) {
			row.Company = strings.TrimSpace(record[idx])
		}
		if idx, ok := fieldMap["city"]; ok && idx < len(record) {
			row.City = strings.TrimSpace(record[idx])
		}
		if idx, ok := fieldMap["state"]; ok && idx < len(record) {
			row.State = strings.TrimSpace(record[idx])
		}
		if idx, ok := fieldMap["country"]; ok && idx < len(record) {
			row.Country = strings.TrimSpace(record[idx])
		}
		if idx, ok := fieldMap["postal_code"]; ok && idx < len(record) {
			row.PostalCode = strings.TrimSpace(record[idx])
		}
		if idx, ok := fieldMap["timezone"]; ok && idx < len(record) {
			row.Timezone = strings.TrimSpace(record[idx])
		}
		if idx, ok := fieldMap["source"]; ok && idx < len(record) {
			row.Source = strings.TrimSpace(record[idx])
		}

		// Build custom fields
		customFields := make(map[string]interface{})
		for _, m := range mapping {
			if m.CustomField != "" && m.ColumnIndex < len(record) {
				value := strings.TrimSpace(record[m.ColumnIndex])
				if value != "" {
					customFields[m.CustomField] = value
				}
			}
		}
		if len(customFields) > 0 {
			row.CustomFieldsJSON, _ = json.Marshal(customFields)
		}

		batch = append(batch, row)

		// Process batch when full
		if len(batch) >= batchSize {
			importedBatch, updatedBatch := s.insertBatch(ctx, listID, orgID, batch, updateExisting)
			atomic.AddInt64(&imported, int64(importedBatch))
			atomic.AddInt64(&updated, int64(updatedBatch))
			batch = batch[:0]
		}

		// Update progress periodically
		if totalRows%ProgressUpdateFreq == 0 {
			progress.TotalRows = totalRows
			progress.ProcessedRows = totalRows
			progress.ImportedCount = imported
			progress.UpdatedCount = updated
			progress.SkippedCount = skipped
			progress.ErrorCount = errorCount
			progress.UpdatedAt = time.Now()

			elapsed := time.Since(startTime).Seconds()
			if elapsed > 0 {
				progress.CurrentRate = float64(totalRows) / elapsed
			}

			s.updateProgress(ctx, sessionID, progress)

			// Update job in database
			s.db.ExecContext(ctx, `
				UPDATE mailing_import_jobs 
				SET processed_rows = $1, imported_count = $2, updated_count = $3, 
				    skipped_count = $4, error_count = $5
				WHERE id = $6
			`, totalRows, imported, updated, skipped, errorCount, jobID)
		}
	}

	// Process remaining batch
	if len(batch) > 0 {
		importedBatch, updatedBatch := s.insertBatch(ctx, listID, orgID, batch, updateExisting)
		imported += int64(importedBatch)
		updated += int64(updatedBatch)
	}

	// Calculate final statistics
	duration := time.Since(startTime)
	rowsPerSecond := float64(totalRows) / duration.Seconds()

	// Final progress update
	progress.Status = "completed"
	progress.TotalRows = totalRows
	progress.ProcessedRows = totalRows
	progress.ImportedCount = imported
	progress.UpdatedCount = updated
	progress.SkippedCount = skipped
	progress.ErrorCount = errorCount
	progress.CurrentRate = rowsPerSecond
	progress.Errors = sampleErrors
	progress.UpdatedAt = time.Now()
	s.updateProgress(ctx, sessionID, progress)

	// Update job to completed
	s.db.ExecContext(ctx, `
		UPDATE mailing_import_jobs 
		SET status = 'completed', total_rows = $1, processed_rows = $1,
		    imported_count = $2, updated_count = $3, skipped_count = $4, 
		    error_count = $5, completed_at = NOW()
		WHERE id = $6
	`, totalRows, imported, updated, skipped, errorCount, jobID)

	// Update list subscriber count
	s.db.ExecContext(ctx, `
		UPDATE mailing_lists 
		SET subscriber_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1),
		    active_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'),
		    updated_at = NOW()
		WHERE id = $1
	`, listID)

	log.Printf("[ListUpload] Completed: %d rows, %d imported, %d updated, %d skipped, %d errors in %.2fs (%.0f rows/sec)",
		totalRows, imported, updated, skipped, errorCount, duration.Seconds(), rowsPerSecond)

	return &ImportResult{
		JobID:           jobID.String(),
		ListID:          listID.String(),
		TotalRows:       totalRows,
		ImportedCount:   imported,
		UpdatedCount:    updated,
		SkippedCount:    skipped,
		ErrorCount:      errorCount,
		DurationSeconds: duration.Seconds(),
		RowsPerSecond:   rowsPerSecond,
		Errors:          sampleErrors,
		CompletedAt:     time.Now(),
	}
}

// subscriberRow holds data for batch insert
type subscriberRow struct {
	Email            string
	EmailHash        string
	FirstName        string
	LastName         string
	Phone            string
	Company          string
	City             string
	State            string
	Country          string
	PostalCode       string
	Timezone         string
	Source           string
	CustomFieldsJSON []byte
}

// insertBatch performs a batch upsert of subscribers
func (s *ListUploadService) insertBatch(ctx context.Context, listID, orgID uuid.UUID, batch []subscriberRow, updateExisting bool) (imported, updated int) {
	if len(batch) == 0 {
		return 0, 0
	}

	// Build batch insert query with ON CONFLICT
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0
	}
	defer tx.Rollback()

	for _, row := range batch {
		customFields := "{}"
		if len(row.CustomFieldsJSON) > 0 {
			customFields = string(row.CustomFieldsJSON)
		}

		var result sql.Result
		if updateExisting {
			// Upsert: insert or update
			result, err = tx.ExecContext(ctx, `
				INSERT INTO mailing_subscribers (
					id, organization_id, list_id, email, email_hash,
					first_name, last_name, timezone, source, custom_fields,
					status, created_at, updated_at
				) VALUES (
					$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'confirmed', NOW(), NOW()
				)
				ON CONFLICT (list_id, email) DO UPDATE SET
					first_name = COALESCE(NULLIF(EXCLUDED.first_name, ''), mailing_subscribers.first_name),
					last_name = COALESCE(NULLIF(EXCLUDED.last_name, ''), mailing_subscribers.last_name),
					timezone = COALESCE(NULLIF(EXCLUDED.timezone, ''), mailing_subscribers.timezone),
					source = COALESCE(NULLIF(EXCLUDED.source, ''), mailing_subscribers.source),
					custom_fields = mailing_subscribers.custom_fields || EXCLUDED.custom_fields,
					updated_at = NOW()
			`, uuid.New(), orgID, listID, row.Email, row.EmailHash,
				row.FirstName, row.LastName, row.Timezone, row.Source, customFields)
		} else {
			// Insert only, skip existing
			result, err = tx.ExecContext(ctx, `
				INSERT INTO mailing_subscribers (
					id, organization_id, list_id, email, email_hash,
					first_name, last_name, timezone, source, custom_fields,
					status, created_at, updated_at
				) VALUES (
					$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'confirmed', NOW(), NOW()
				)
				ON CONFLICT (list_id, email) DO NOTHING
			`, uuid.New(), orgID, listID, row.Email, row.EmailHash,
				row.FirstName, row.LastName, row.Timezone, row.Source, customFields)
		}

		if err == nil {
			rowsAffected, _ := result.RowsAffected()
			if rowsAffected > 0 {
				imported++
			}
		}
	}

	tx.Commit()
	return imported, updated
}

// =============================================================================
// DIRECT FILE UPLOAD (NON-CHUNKED)
// =============================================================================

// ProcessDirectUpload handles a direct file upload (for smaller files)
func (s *ListUploadService) ProcessDirectUpload(ctx context.Context, orgID, listID string, reader io.Reader, filename string, mapping []FieldMapping, updateExisting bool) (*ImportResult, error) {
	// First validate headers
	buf := new(strings.Builder)
	tee := io.TeeReader(reader, buf)

	headerResult, err := s.DetectHeaders(tee)
	if err != nil {
		return nil, err
	}

	if !headerResult.HasHeaders {
		return nil, ErrNoHeaders
	}

	// Validate mapping has email
	hasEmail := false
	for _, m := range mapping {
		if m.SystemField == "email" {
			hasEmail = true
			break
		}
	}
	if !hasEmail {
		return nil, ErrMissingEmailColumn
	}

	// Create temp file from reader
	sessionID := uuid.New().String()
	tempPath := filepath.Join(s.uploadDir, fmt.Sprintf("%s.csv", sessionID))

	tempFile, err := os.Create(tempPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tempPath)

	// Write remaining data after header detection
	tempFile.WriteString(buf.String())
	io.Copy(tempFile, reader)
	tempFile.Close()

	// Reopen for processing
	file, _ := os.Open(tempPath)
	defer file.Close()

	// Create job
	jobID := uuid.New()
	listUUID, _ := uuid.Parse(listID)
	orgUUID, _ := uuid.Parse(orgID)

	mappingJSON, _ := json.Marshal(mapping)
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_import_jobs (id, organization_id, list_id, filename, field_mapping, status, started_at)
		VALUES ($1, $2, $3, $4, $5, 'processing', NOW())
	`, jobID, orgUUID, listUUID, filename, mappingJSON)

	// Process
	return s.processCSVStreaming(ctx, sessionID, jobID, listUUID, orgUUID, file, mapping, updateExisting), nil
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

func (s *ListUploadService) sessionKey(sessionID string) string {
	return fmt.Sprintf("upload:session:%s", sessionID)
}

func (s *ListUploadService) progressKey(sessionID string) string {
	return fmt.Sprintf("upload:progress:%s", sessionID)
}

func (s *ListUploadService) updateProgress(ctx context.Context, sessionID string, progress *UploadProgress) {
	data, _ := json.Marshal(progress)
	s.redis.Set(ctx, s.progressKey(sessionID), data, UploadSessionTTL)
}

func (s *ListUploadService) updateSessionError(ctx context.Context, sessionID, errorMsg string) {
	session, err := s.GetUploadSession(ctx, sessionID)
	if err != nil {
		return
	}
	session.Status = "failed"
	session.Error = errorMsg
	data, _ := json.Marshal(session)
	s.redis.Set(ctx, s.sessionKey(sessionID), data, UploadSessionTTL)
}

func (s *ListUploadService) chunkSetKey(sessionID string) string {
	return fmt.Sprintf("upload:chunks:%s", sessionID)
}

func normalizeHeader(header string) string {
	// Convert to lowercase, trim spaces, replace common separators
	normalized := strings.ToLower(strings.TrimSpace(header))
	normalized = strings.ReplaceAll(normalized, " ", "_")
	normalized = strings.ReplaceAll(normalized, "-", "_")
	return normalized
}

func isNumericString(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && c != '.' && c != '-' && c != '+' {
			return false
		}
	}
	return true
}

func hashEmail(email string) string {
	hash := md5.Sum([]byte(strings.ToLower(email)))
	return hex.EncodeToString(hash[:])
}

func contains(slice []int, item int) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}

// GetSystemFields returns the list of system fields for mapping UI
func GetSystemFields() []FieldDefinition {
	return SystemFields
}
