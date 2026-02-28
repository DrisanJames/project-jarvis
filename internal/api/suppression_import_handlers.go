package api

import (
	"encoding/json"
	"database/sql"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/redis/go-redis/v9"
)

// SuppressionImportAPI handles HTTP endpoints for bulk suppression imports.
// Supports both chunked (multi-GB) and direct (single-request) uploads.
type SuppressionImportAPI struct {
	svc *worker.SuppressionImportService
}

// NewSuppressionImportAPI creates the API handler with a backing service
func NewSuppressionImportAPI(db *sql.DB, redisClient *redis.Client) *SuppressionImportAPI {
	return &SuppressionImportAPI{
		svc: worker.NewSuppressionImportService(db, redisClient),
	}
}

// HandleInitUpload creates a chunked upload session for a large suppression file.
// POST /api/mailing/suppression-import/init
// Body: { "list_id": "...", "filename": "...", "file_size": 3190000000 }
func (a *SuppressionImportAPI) HandleInitUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ListID   string `json:"list_id"`
		Filename string `json:"filename"`
		FileSize int64  `json:"file_size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.ListID == "" || req.Filename == "" || req.FileSize <= 0 {
		respondError(w, http.StatusBadRequest, "list_id, filename, and file_size are required")
		return
	}

	job, err := a.svc.InitUpload(r.Context(), req.ListID, req.Filename, req.FileSize)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, job)
}

// HandleUploadChunk receives a single chunk of the file upload.
// POST /api/mailing/suppression-import/{jobId}/chunk
// Query: ?chunk=0
// Body: raw chunk bytes
func (a *SuppressionImportAPI) HandleUploadChunk(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")

	chunkStr := r.URL.Query().Get("chunk")
	chunkNumber, err := strconv.Atoi(chunkStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "chunk query parameter required (integer)")
		return
	}

	// Read chunk body (limited to 50MB)
	data, err := io.ReadAll(io.LimitReader(r.Body, 50*1024*1024+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to read chunk data")
		return
	}
	if len(data) > 50*1024*1024 {
		respondError(w, http.StatusRequestEntityTooLarge, "chunk exceeds 50MB limit")
		return
	}

	if err := a.svc.UploadChunk(r.Context(), jobID, chunkNumber, data); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Check if upload is now complete
	complete, _ := a.svc.IsUploadComplete(r.Context(), jobID)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"chunk":    chunkNumber,
		"complete": complete,
	})
}

// HandleStartProcessing triggers background processing of the uploaded file.
// POST /api/mailing/suppression-import/{jobId}/process
func (a *SuppressionImportAPI) HandleStartProcessing(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")

	if err := a.svc.StartProcessing(r.Context(), jobID); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"job_id":  jobID,
		"message": "Processing started in background. Poll progress endpoint for status.",
	})
}

// HandleGetProgress returns the current import progress.
// GET /api/mailing/suppression-import/{jobId}/progress
func (a *SuppressionImportAPI) HandleGetProgress(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobId")

	progress, err := a.svc.GetProgress(r.Context(), jobID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, progress)
}

// HandleDirectUpload handles single-request uploads for files under ~500 MB.
// Streams the file to disk (no memory buffering) and processes in background.
// POST /api/mailing/suppression-import/direct
// Multipart form: file + list_id
func (a *SuppressionImportAPI) HandleDirectUpload(w http.ResponseWriter, r *http.Request) {
	// Allow up to 512 MB for direct upload (32MB in memory, rest on disk)
	r.Body = http.MaxBytesReader(w, r.Body, 512*1024*1024)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		respondError(w, http.StatusRequestEntityTooLarge, "file too large for direct upload (max 512MB). Use chunked upload for larger files.")
		return
	}

	listID := r.FormValue("list_id")
	if listID == "" {
		respondError(w, http.StatusBadRequest, "list_id is required")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		respondError(w, http.StatusBadRequest, "no file provided")
		return
	}
	defer file.Close()

	jobID, err := a.svc.ProcessDirectUpload(r.Context(), listID, file, header.Filename, header.Size)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"success": true,
		"job_id":  jobID,
		"message": "File received. Processing in background.",
	})
}

// respondJSON and respondError are defined in handlers.go
