package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/redis/go-redis/v9"
)

// =============================================================================
// LIST UPLOAD HANDLERS
// =============================================================================
// HTTP handlers for the list upload API. Supports:
// - Header detection and validation
// - Field mapping suggestions
// - Chunked uploads for large files (up to 10GB)
// - Direct uploads for smaller files
// - Progress tracking

// ListUploadHandlers provides HTTP handlers for list uploads
type ListUploadHandlers struct {
	service            *worker.ListUploadService
	customFieldService *mailing.CustomFieldService
	db                 *sql.DB
}

// NewListUploadHandlers creates a new handler instance
func NewListUploadHandlers(db *sql.DB, redisClient *redis.Client) *ListUploadHandlers {
	return &ListUploadHandlers{
		service:            worker.NewListUploadService(db, redisClient),
		customFieldService: mailing.NewCustomFieldService(db),
		db:                 db,
	}
}

// RegisterRoutes registers the list upload routes
func (h *ListUploadHandlers) RegisterRoutes(r chi.Router) {
	r.Route("/lists", func(r chi.Router) {
		// Field mapping reference
		r.Get("/upload/fields", h.HandleGetSystemFields)

		// Header detection and validation
		r.Post("/upload/validate", h.HandleValidateHeaders)
		
		// Enhanced validation with custom field detection
		r.Post("/upload/analyze", h.HandleAnalyzeHeaders)

		// Direct upload (smaller files < 100MB)
		r.Post("/{listId}/upload", h.HandleDirectUpload)

		// Chunked upload for large files
		r.Post("/{listId}/upload/init", h.HandleInitChunkedUpload)
		r.Post("/{listId}/upload/{sessionId}/chunk/{chunkNumber}", h.HandleUploadChunk)
		r.Post("/{listId}/upload/{sessionId}/complete", h.HandleCompleteUpload)
		r.Get("/{listId}/upload/{sessionId}/status", h.HandleGetUploadStatus)
		r.Get("/{listId}/upload/{sessionId}/progress", h.HandleGetUploadProgress)
	})
}

// =============================================================================
// FIELD MAPPING ENDPOINTS
// =============================================================================

// HandleGetSystemFields returns the list of system fields available for mapping
// GET /api/mailing/lists/upload/fields
func (h *ListUploadHandlers) HandleGetSystemFields(w http.ResponseWriter, r *http.Request) {
	fields := worker.GetSystemFields()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"system_fields": fields,
		"required_fields": []string{"email"},
		"supported_custom_fields": true,
	})
}

// =============================================================================
// HEADER VALIDATION ENDPOINTS
// =============================================================================

// ValidateHeadersRequest is the request body for header validation
type ValidateHeadersRequest struct {
	// For multipart form upload, use "file" field
	// Or pass CSV content directly in "content" field
	Content string `json:"content,omitempty"`
}

// HandleValidateHeaders validates CSV headers and suggests field mappings
// POST /api/mailing/lists/upload/validate
// Accepts: multipart/form-data with "file" field OR application/json with "content" field
func (h *ListUploadHandlers) HandleValidateHeaders(w http.ResponseWriter, r *http.Request) {
	var reader io.Reader

	// Check content type
	contentType := r.Header.Get("Content-Type")
	if contentType == "application/json" {
		var req ValidateHeadersRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Content == "" {
			writeError(w, "content is required", http.StatusBadRequest)
			return
		}
		reader = stringReader(req.Content)
	} else {
		// Multipart form
		r.ParseMultipartForm(10 << 20) // 10MB for validation
		file, _, err := r.FormFile("file")
		if err != nil {
			writeError(w, "file is required", http.StatusBadRequest)
			return
		}
		defer file.Close()
		reader = file
	}

	// Detect headers
	result, err := h.service.DetectHeaders(reader)
	if err != nil {
		if err == worker.ErrEmptyFile {
			writeError(w, "file is empty", http.StatusBadRequest)
			return
		}
		writeError(w, fmt.Sprintf("failed to analyze file: %v", err), http.StatusBadRequest)
		return
	}

	// Return result
	w.Header().Set("Content-Type", "application/json")

	if !result.HasHeaders {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid":            false,
			"has_headers":      false,
			"rejection_reason": result.RejectionReason,
			"confidence":       result.Confidence,
			"detection_method": result.DetectionMethod,
			"sample_rows":      result.SampleRows,
			"total_columns":    result.TotalColumns,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"valid":              true,
		"has_headers":        true,
		"headers":            result.Headers,
		"suggested_mappings": result.SuggestedMappings,
		"sample_rows":        result.SampleRows,
		"total_columns":      result.TotalColumns,
		"confidence":         result.Confidence,
		"detection_method":   result.DetectionMethod,
	})
}

// =============================================================================
// ENHANCED ANALYSIS ENDPOINT (WITH CUSTOM FIELDS)
// =============================================================================

// HandleAnalyzeHeaders performs comprehensive CSV header analysis including custom field detection
// POST /api/mailing/lists/upload/analyze
// Accepts: multipart/form-data with "file" field OR application/json with "content" field
// This endpoint combines standard header detection with custom field suggestions
func (h *ListUploadHandlers) HandleAnalyzeHeaders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var reader io.Reader

	// Check content type
	contentType := r.Header.Get("Content-Type")
	if contentType == "application/json" {
		var req ValidateHeadersRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Content == "" {
			writeError(w, "content is required", http.StatusBadRequest)
			return
		}
		reader = stringReader(req.Content)
	} else {
		// Multipart form
		r.ParseMultipartForm(10 << 20) // 10MB for validation
		file, _, err := r.FormFile("file")
		if err != nil {
			writeError(w, "file is required", http.StatusBadRequest)
			return
		}
		defer file.Close()
		reader = file
	}

	// Detect headers using worker service
	result, err := h.service.DetectHeaders(reader)
	if err != nil {
		if err == worker.ErrEmptyFile {
			writeError(w, "file is empty", http.StatusBadRequest)
			return
		}
		writeError(w, fmt.Sprintf("failed to analyze file: %v", err), http.StatusBadRequest)
		return
	}

	// Return result
	w.Header().Set("Content-Type", "application/json")

	if !result.HasHeaders {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid":            false,
			"has_headers":      false,
			"rejection_reason": result.RejectionReason,
			"confidence":       result.Confidence,
			"detection_method": result.DetectionMethod,
			"sample_rows":      result.SampleRows,
			"total_columns":    result.TotalColumns,
		})
		return
	}

	// Get organization ID for custom field detection
	orgID := getOrganizationUUID(r)

	// Use custom field service to detect non-standard columns
	customFieldAnalysis, err := h.customFieldService.DetectNonStandardColumns(ctx, orgID, result.Headers, result.SampleRows)
	if err != nil {
		// Fall back to basic analysis if custom field detection fails
		json.NewEncoder(w).Encode(map[string]interface{}{
			"valid":              true,
			"has_headers":        true,
			"headers":            result.Headers,
			"suggested_mappings": result.SuggestedMappings,
			"sample_rows":        result.SampleRows,
			"total_columns":      result.TotalColumns,
			"confidence":         result.Confidence,
			"detection_method":   result.DetectionMethod,
		})
		return
	}

	// Build enhanced response with custom field suggestions
	response := map[string]interface{}{
		"valid":                true,
		"has_headers":          true,
		"headers":              result.Headers,
		"sample_rows":          result.SampleRows,
		"total_columns":        result.TotalColumns,
		"confidence":           result.Confidence,
		"detection_method":     result.DetectionMethod,

		// Standard field mappings
		"standard_mappings":    customFieldAnalysis.StandardMappings,
		"standard_count":       len(customFieldAnalysis.StandardMappings),

		// Non-standard columns that need custom fields
		"non_standard_columns": customFieldAnalysis.NonStandardColumns,
		"non_standard_count":   len(customFieldAnalysis.NonStandardColumns),

		// Existing custom fields that can be mapped to
		"existing_custom_fields": customFieldAnalysis.ExistingCustomFields,
	}

	// Build field mapping suggestions combining standard and custom
	var enhancedMappings []map[string]interface{}
	for _, m := range result.SuggestedMappings {
		mapping := map[string]interface{}{
			"column_index": m.ColumnIndex,
			"column_name":  m.ColumnName,
			"mapping_type": "skip", // default
		}

		// Check if it's a standard field
		if stdField, ok := customFieldAnalysis.StandardMappings[m.ColumnName]; ok {
			mapping["mapping_type"] = "standard"
			mapping["system_field"] = stdField
		} else if m.SystemField != "" {
			mapping["mapping_type"] = "standard"
			mapping["system_field"] = m.SystemField
		} else {
			// Check if it matches a non-standard column
			for _, ns := range customFieldAnalysis.NonStandardColumns {
				if ns.ColumnIndex == m.ColumnIndex {
					if ns.ExistingFieldID != nil {
						mapping["mapping_type"] = "existing_custom"
						mapping["custom_field_id"] = ns.ExistingFieldID
						mapping["custom_field_name"] = ns.SuggestedName
					} else {
						mapping["mapping_type"] = "new_custom"
						mapping["suggested_name"] = ns.SuggestedName
						mapping["suggested_type"] = ns.SuggestedType
						mapping["sample_values"] = ns.SampleValues
						if len(ns.UniqueValues) > 0 && len(ns.UniqueValues) <= 10 {
							mapping["suggested_enum_values"] = ns.UniqueValues
						}
					}
					break
				}
			}
		}

		enhancedMappings = append(enhancedMappings, mapping)
	}
	response["enhanced_mappings"] = enhancedMappings

	json.NewEncoder(w).Encode(response)
}

// =============================================================================
// DIRECT UPLOAD ENDPOINT
// =============================================================================

// HandleDirectUpload handles direct file uploads (for files < 100MB)
// POST /api/mailing/lists/{listId}/upload
// Content-Type: multipart/form-data
// Fields:
//   - file: CSV file (required)
//   - field_mapping: JSON array of FieldMapping (required)
//   - update_existing: "true" or "false" (optional, default: true)
func (h *ListUploadHandlers) HandleDirectUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "listId")

	// Parse multipart form (100MB max for direct upload)
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		writeError(w, "file too large for direct upload, use chunked upload", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, "file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Get field mapping
	mappingJSON := r.FormValue("field_mapping")
	if mappingJSON == "" {
		writeError(w, "field_mapping is required", http.StatusBadRequest)
		return
	}

	var mapping []worker.FieldMapping
	if err := json.Unmarshal([]byte(mappingJSON), &mapping); err != nil {
		writeError(w, "invalid field_mapping format", http.StatusBadRequest)
		return
	}

	// Get update_existing flag
	updateExisting := r.FormValue("update_existing") != "false"

	// Get organization ID (from auth context in real implementation)
	orgID := getOrgIDFromRequest(r)

	// Process upload
	result, err := h.service.ProcessDirectUpload(ctx, orgID, listID, file, header.Filename, mapping, updateExisting)
	if err != nil {
		switch err {
		case worker.ErrNoHeaders:
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":   "no_headers_detected",
				"message": "CSV file must have a header row with column names",
			})
			return
		case worker.ErrMissingEmailColumn:
			writeError(w, "email column mapping is required", http.StatusBadRequest)
			return
		default:
			writeError(w, fmt.Sprintf("upload failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// =============================================================================
// CHUNKED UPLOAD ENDPOINTS
// =============================================================================

// InitChunkedUploadRequest is the request body for initializing a chunked upload
type InitChunkedUploadRequest struct {
	Filename  string `json:"filename"`
	FileSize  int64  `json:"file_size"`
	ChunkSize int64  `json:"chunk_size,omitempty"` // Optional, defaults to 10MB
}

// HandleInitChunkedUpload initializes a new chunked upload session
// POST /api/mailing/lists/{listId}/upload/init
func (h *ListUploadHandlers) HandleInitChunkedUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "listId")

	var req InitChunkedUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Filename == "" {
		writeError(w, "filename is required", http.StatusBadRequest)
		return
	}

	if req.FileSize <= 0 {
		writeError(w, "file_size must be positive", http.StatusBadRequest)
		return
	}

	// Get organization ID
	orgID := getOrgIDFromRequest(r)

	// Initialize session
	session, err := h.service.InitUploadSession(ctx, orgID, listID, req.Filename, req.FileSize, req.ChunkSize)
	if err != nil {
		writeError(w, fmt.Sprintf("failed to initialize upload: %v", err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"session_id":    session.ID,
		"chunk_size":    session.ChunkSize,
		"total_chunks":  session.TotalChunks,
		"expires_at":    session.ExpiresAt,
		"temp_file":     session.TempFilePath,
		"upload_url":    fmt.Sprintf("/api/mailing/lists/%s/upload/%s/chunk/{chunk_number}", listID, session.ID),
		"complete_url":  fmt.Sprintf("/api/mailing/lists/%s/upload/%s/complete", listID, session.ID),
		"progress_url":  fmt.Sprintf("/api/mailing/lists/%s/upload/%s/progress", listID, session.ID),
	})
}

// HandleUploadChunk handles a single chunk upload
// POST /api/mailing/lists/{listId}/upload/{sessionId}/chunk/{chunkNumber}
// Content-Type: application/octet-stream
func (h *ListUploadHandlers) HandleUploadChunk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := chi.URLParam(r, "sessionId")
	chunkNumberStr := chi.URLParam(r, "chunkNumber")

	chunkNumber, err := strconv.Atoi(chunkNumberStr)
	if err != nil {
		writeError(w, "invalid chunk number", http.StatusBadRequest)
		return
	}

	// Read chunk data
	data, err := io.ReadAll(io.LimitReader(r.Body, worker.MaxChunkSize+1))
	if err != nil {
		writeError(w, "failed to read chunk data", http.StatusBadRequest)
		return
	}

	if int64(len(data)) > worker.MaxChunkSize {
		writeError(w, fmt.Sprintf("chunk exceeds maximum size of %d bytes", worker.MaxChunkSize), http.StatusBadRequest)
		return
	}

	// Upload chunk
	err = h.service.UploadChunk(ctx, sessionID, chunkNumber, data)
	if err != nil {
		switch err {
		case worker.ErrUploadNotFound:
			writeError(w, "upload session not found", http.StatusNotFound)
		case worker.ErrUploadExpired:
			writeError(w, "upload session has expired", http.StatusGone)
		case worker.ErrUploadAlreadyComplete:
			writeError(w, "upload already completed", http.StatusConflict)
		default:
			writeError(w, fmt.Sprintf("failed to upload chunk: %v", err), http.StatusInternalServerError)
		}
		return
	}

	// Check if upload is now complete
	complete, _ := h.service.IsUploadComplete(ctx, sessionID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"chunk":    chunkNumber,
		"received": len(data),
		"complete": complete,
	})
}

// CompleteUploadRequest is the request body for completing a chunked upload
type CompleteUploadRequest struct {
	FieldMapping   []worker.FieldMapping `json:"field_mapping"`
	UpdateExisting bool                   `json:"update_existing"`
}

// HandleCompleteUpload completes a chunked upload and starts processing
// POST /api/mailing/lists/{listId}/upload/{sessionId}/complete
func (h *ListUploadHandlers) HandleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := chi.URLParam(r, "sessionId")

	var req CompleteUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.FieldMapping) == 0 {
		writeError(w, "field_mapping is required", http.StatusBadRequest)
		return
	}

	// Check if upload is complete
	complete, err := h.service.IsUploadComplete(ctx, sessionID)
	if err != nil {
		switch err {
		case worker.ErrUploadNotFound:
			writeError(w, "upload session not found", http.StatusNotFound)
		case worker.ErrUploadExpired:
			writeError(w, "upload session has expired", http.StatusGone)
		default:
			writeError(w, fmt.Sprintf("failed to check upload status: %v", err), http.StatusInternalServerError)
		}
		return
	}

	if !complete {
		session, _ := h.service.GetUploadSession(ctx, sessionID)
		writeError(w, fmt.Sprintf("upload incomplete: %d/%d chunks uploaded",
			len(session.UploadedChunks), session.TotalChunks), http.StatusBadRequest)
		return
	}

	// Process the uploaded file (async)
	go func() {
		h.service.ProcessUploadedFile(ctx, sessionID, req.FieldMapping, req.UpdateExisting)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "processing",
		"session_id":   sessionID,
		"progress_url": fmt.Sprintf("/api/mailing/lists/%s/upload/%s/progress", chi.URLParam(r, "listId"), sessionID),
	})
}

// HandleGetUploadStatus returns the status of an upload session
// GET /api/mailing/lists/{listId}/upload/{sessionId}/status
func (h *ListUploadHandlers) HandleGetUploadStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := chi.URLParam(r, "sessionId")

	session, err := h.service.GetUploadSession(ctx, sessionID)
	if err != nil {
		switch err {
		case worker.ErrUploadNotFound:
			writeError(w, "upload session not found", http.StatusNotFound)
		case worker.ErrUploadExpired:
			writeError(w, "upload session has expired", http.StatusGone)
		default:
			writeError(w, fmt.Sprintf("failed to get session: %v", err), http.StatusInternalServerError)
		}
		return
	}

	complete := len(session.UploadedChunks) == session.TotalChunks

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"session_id":      session.ID,
		"status":          session.Status,
		"filename":        session.Filename,
		"file_size":       session.FileSize,
		"chunk_size":      session.ChunkSize,
		"total_chunks":    session.TotalChunks,
		"uploaded_chunks": session.UploadedChunks,
		"upload_complete": complete,
		"created_at":      session.CreatedAt,
		"expires_at":      session.ExpiresAt,
		"error":           session.Error,
	})
}

// HandleGetUploadProgress returns the processing progress of an upload
// GET /api/mailing/lists/{listId}/upload/{sessionId}/progress
func (h *ListUploadHandlers) HandleGetUploadProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessionID := chi.URLParam(r, "sessionId")

	progress, err := h.service.GetProgress(ctx, sessionID)
	if err != nil {
		writeError(w, fmt.Sprintf("failed to get progress: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(progress)
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

func writeError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func stringReader(s string) io.Reader {
	return &stringReaderWrapper{s: s, i: 0}
}

type stringReaderWrapper struct {
	s string
	i int
}

func (r *stringReaderWrapper) Read(p []byte) (n int, err error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n = copy(p, r.s[r.i:])
	r.i += n
	return
}

// Note: getOrgIDFromRequest is defined in mailing_advanced_handlers.go
