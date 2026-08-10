package api

// Partner ingestion HTTP handlers.
//
// POST /api/partner-ingest/v1/records
//   - Authenticated via X-Partner-Key header (PartnerKeyAuth middleware).
//   - Accepts JSON ({"records":[...]}) OR NDJSON (Content-Type:
//     application/x-ndjson | text/plain). Each record MUST contain "email";
//     other fields are preserved as opaque metadata.
//   - Streams gzip-compressed canonical NDJSON to S3.
//   - Inserts a partner_inbound_batches row.
//   - Returns 202 {batch_id, record_count} immediately. The slicer worker
//     picks up the batch asynchronously.
//
// GET /api/partner-ingest/v1/schema
//   - Public (no auth). Returns example payload + JSON Schema.
//
// GET /api/partner-ingest/v1/batches/{id}
//   - Authenticated. Returns batch status (partner-scoped).

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// PartnerIngestHandler bundles the dependencies needed by the inbound API.
type PartnerIngestHandler struct {
	db       *sql.DB
	s3Client *PartnerIngestS3Client
}

// NewPartnerIngestHandler constructs the handler with required deps. db must
// be non-nil; s3Client may be nil during early boot but record-ingest will
// 503 until it becomes available.
func NewPartnerIngestHandler(db *sql.DB, s3Client *PartnerIngestS3Client) *PartnerIngestHandler {
	return &PartnerIngestHandler{db: db, s3Client: s3Client}
}

// SetS3Client allows late wiring during boot.
func (h *PartnerIngestHandler) SetS3Client(c *PartnerIngestS3Client) {
	h.s3Client = c
}

// ============ Request limits ============

const (
	maxIngestBytes      = 25 * 1024 * 1024 // 25 MiB JSON / NDJSON body
	maxRecordsPerBatch  = 50_000
	maxEmailLength      = 254
	defaultBatchTimeout = 60 * time.Second
)

// ============ Wire types ============

// ingestRecord is a CLOSED struct: json.Unmarshal silently discards any posted
// field with no home here, and the canonical NDJSON written to S3 is a re-marshal
// of THIS struct — so a field missing below is destroyed at the door and can
// never be recovered downstream, no matter what the drip or the send path do.
//
// That is not hypothetical: a partner posting the documented shape
// {"city":"Boise","postal_code":"83669",...} landed in partner_clean_queue as
// {"zip":"","state":"ID",...} with the city gone and the ZIP empty — the struct
// accepted only "zip" (2026-08-09). Personalized creatives rendered their
// location fields blank and the cause looked like a templating bug three layers
// away. Adding a field here is cheap; add it rather than dropping partner data.
type ingestRecord struct {
	Email     string                 `json:"email"`
	FirstName string                 `json:"first_name,omitempty"`
	LastName  string                 `json:"last_name,omitempty"`
	City      string                 `json:"city,omitempty"`
	Zip       string                 `json:"zip,omitempty"`
	State     string                 `json:"state,omitempty"`
	IPAddress string                 `json:"ip_address,omitempty"`
	OptInDate string                 `json:"opt_in_date,omitempty"`
	Source    string                 `json:"source,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// PostalCode is the spelling most partners send; Zip is the legacy field this
// struct already had. UnmarshalJSON accepts EITHER and normalizes onto Zip so
// exactly one representation reaches S3 and every downstream reader keeps
// working unchanged.
func (r *ingestRecord) UnmarshalJSON(b []byte) error {
	type raw ingestRecord // alias: no recursion back into this method
	var v struct {
		raw
		PostalCode string `json:"postal_code"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*r = ingestRecord(v.raw)
	if strings.TrimSpace(r.Zip) == "" {
		r.Zip = v.PostalCode
	}
	return nil
}

type ingestRequest struct {
	Records []ingestRecord `json:"records"`
}

type ingestResponse struct {
	BatchID     string `json:"batch_id"`
	RecordCount int    `json:"record_count"`
	Accepted    bool   `json:"accepted"`
	Message     string `json:"message,omitempty"`
	S3Key       string `json:"s3_key,omitempty"`
	ReceivedAt  string `json:"received_at"`
}

// ============ POST /api/partner-ingest/v1/records ============

func (h *PartnerIngestHandler) HandlePostRecords(w http.ResponseWriter, r *http.Request) {
	authCtx, ok := PartnerAuthFromContext(r.Context())
	if !ok {
		partnerErrorResponse(w, http.StatusUnauthorized, "auth_missing", "Authentication required")
		return
	}
	if h.s3Client == nil {
		partnerErrorResponse(w, http.StatusServiceUnavailable, "ingest_unavailable", "Ingest pipeline is initialising. Retry in a moment.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultBatchTimeout)
	defer cancel()

	r.Body = http.MaxBytesReader(w, r.Body, maxIngestBytes)
	defer r.Body.Close()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			partnerErrorResponse(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("Payload exceeds %d MiB. Split into smaller batches.", maxIngestBytes/1024/1024))
			return
		}
		partnerErrorResponse(w, http.StatusBadRequest, "body_read_failed", "Unable to read request body")
		return
	}

	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	parsed, parseErr := parseIngestPayload(bodyBytes, contentType)
	if parseErr != nil {
		partnerErrorResponse(w, http.StatusBadRequest, "invalid_payload", parseErr.Error())
		return
	}
	if len(parsed) == 0 {
		partnerErrorResponse(w, http.StatusBadRequest, "no_records", "Payload contained zero valid records")
		return
	}
	if len(parsed) > maxRecordsPerBatch {
		partnerErrorResponse(w, http.StatusRequestEntityTooLarge, "too_many_records",
			fmt.Sprintf("Batch contains %d records, max is %d. Split into smaller batches.", len(parsed), maxRecordsPerBatch))
		return
	}

	batchID := uuid.New().String()
	receivedAt := time.Now().UTC()

	// Reduce parsed records to canonical NDJSON — one JSON object per line in
	// FIFO order. The slicer reads this NDJSON, applies suppression checks,
	// then enqueues survivors into partner_clean_queue.
	var canonicalNDJSON bytes.Buffer
	for _, rec := range parsed {
		line, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		canonicalNDJSON.Write(line)
		canonicalNDJSON.WriteByte('\n')
	}

	s3Key := h.s3Client.BatchKey(authCtx.PartnerSlug, authCtx.DatasetSlug, batchID, receivedAt)
	bytesWritten, uploadErr := h.s3Client.UploadBatchGzip(ctx, s3Key, &canonicalNDJSON)
	if uploadErr != nil {
		log.Printf("[partner-ingest] s3 upload failed bucket=%s key=%s partner=%s dataset=%s err=%v",
			h.s3Client.Bucket(), s3Key, authCtx.PartnerSlug, authCtx.DatasetSlug, uploadErr)
		partnerErrorResponse(w, http.StatusBadGateway, "s3_upload_failed", "Unable to persist batch right now. Retry in a moment.")
		return
	}

	meta := PartnerBatchMeta{
		BatchID:      batchID,
		PartnerID:    authCtx.PartnerID,
		PartnerSlug:  authCtx.PartnerSlug,
		DatasetID:    authCtx.DatasetID,
		DatasetSlug:  authCtx.DatasetSlug,
		Vertical:     authCtx.Vertical,
		RecordCount:  len(parsed),
		ReceivedAt:   receivedAt,
		S3Bucket:     h.s3Client.Bucket(),
		S3Key:        s3Key,
		ContentBytes: bytesWritten,
	}
	_ = h.s3Client.UploadBatchMeta(ctx, h.s3Client.MetaKey(authCtx.PartnerSlug, authCtx.DatasetSlug, batchID, receivedAt), meta)

	_, dbErr := h.db.ExecContext(ctx, `
		INSERT INTO partner_inbound_batches
		    (id, dataset_id, partner_id, s3_bucket, s3_key, record_count, status,
		     emergency_stopped, next_record_offset, received_at, ingest_metadata)
		VALUES ($1, $2, $3, $4, $5, $6, 'received', false, 0, $7, $8::jsonb)
	`,
		batchID, authCtx.DatasetID, authCtx.PartnerID,
		h.s3Client.Bucket(), s3Key, len(parsed),
		receivedAt,
		mustJSONString(map[string]interface{}{
			"api_key_prefix": authCtx.KeyPrefix,
			"content_type":   contentType,
			"bytes_written":  bytesWritten,
			"remote_ip":      readClientIP(r),
			"user_agent":     r.UserAgent(),
		}),
	)
	if dbErr != nil {
		// The S3 object is already persisted — operators can still recover
		// it via the slicer's S3-discovery fallback. Return a soft error so
		// the partner is told the upload completed but the indexing failed.
		partnerErrorResponse(w, http.StatusAccepted, "batch_indexing_deferred",
			"Records persisted to S3 but indexing is deferred. Batch will be picked up automatically.")
		return
	}

	respondJSON(w, http.StatusAccepted, ingestResponse{
		BatchID:     batchID,
		RecordCount: len(parsed),
		Accepted:    true,
		Message:     "Batch accepted; processing asynchronously.",
		S3Key:       s3Key,
		ReceivedAt:  receivedAt.Format(time.RFC3339),
	})
}

// ============ GET /api/partner-ingest/v1/batches/{id} ============

func (h *PartnerIngestHandler) HandleGetBatch(w http.ResponseWriter, r *http.Request) {
	authCtx, ok := PartnerAuthFromContext(r.Context())
	if !ok {
		partnerErrorResponse(w, http.StatusUnauthorized, "auth_missing", "Authentication required")
		return
	}
	batchID := chi.URLParam(r, "id")
	if batchID == "" {
		partnerErrorResponse(w, http.StatusBadRequest, "missing_batch_id", "Batch id is required")
		return
	}

	row := h.db.QueryRowContext(r.Context(), `
		SELECT b.id, b.status, b.record_count, b.next_record_offset, b.received_at,
		       b.completed_at, b.last_error, b.emergency_stopped,
		       (SELECT COUNT(*) FROM partner_clean_queue WHERE batch_id = b.id) AS sliced,
		       (SELECT COUNT(*) FROM partner_clean_queue WHERE batch_id = b.id AND status = 'suppressed_global') AS suppressed_global,
		       (SELECT COUNT(*) FROM partner_clean_queue WHERE batch_id = b.id AND status = 'suppressed_eo') AS suppressed_eo,
		       (SELECT COUNT(*) FROM partner_clean_queue WHERE batch_id = b.id AND status = 'ready') AS ready,
		       (SELECT COUNT(*) FROM partner_clean_queue WHERE batch_id = b.id AND status = 'mailed') AS mailed
		FROM partner_inbound_batches b
		WHERE b.id = $1 AND b.partner_id = $2
	`, batchID, authCtx.PartnerID)

	var (
		id, status                                string
		recordCount, nextOffset                   int
		receivedAt                                time.Time
		completedAt                               sql.NullTime
		lastError                                 sql.NullString
		emergencyStopped                          bool
		sliced, suppGlobal, suppEO, ready, mailed int
	)
	if err := row.Scan(&id, &status, &recordCount, &nextOffset, &receivedAt, &completedAt, &lastError, &emergencyStopped,
		&sliced, &suppGlobal, &suppEO, &ready, &mailed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			partnerErrorResponse(w, http.StatusNotFound, "batch_not_found", "No batch found with that id under your account")
			return
		}
		partnerErrorResponse(w, http.StatusInternalServerError, "lookup_failed", "Unable to look up batch right now")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"batch_id":           id,
		"status":             status,
		"record_count":       recordCount,
		"next_record_offset": nextOffset,
		"received_at":        receivedAt.Format(time.RFC3339),
		"completed_at":       formatNullTime(completedAt),
		"last_error":         nullStringValue(lastError),
		"emergency_stopped":  emergencyStopped,
		"counters": map[string]int{
			"sliced":            sliced,
			"suppressed_global": suppGlobal,
			"suppressed_eo":     suppEO,
			"ready":             ready,
			"mailed":            mailed,
		},
	})
}

// ============ GET /api/partner-ingest/v1/schema ============

// HandleGetSchema returns an example payload, JSON Schema, and the supported
// verticals so partners can self-serve onboarding without operator round-trip.
// No auth required — this is purely documentation.
func (h *PartnerIngestHandler) HandleGetSchema(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"endpoint": "POST /api/partner-ingest/v1/records",
		"auth": map[string]interface{}{
			"header":  partnerKeyHeader,
			"format":  "dpk_<32 chars>",
			"example": "dpk_abcd1234abcd1234abcd1234abcd1234",
		},
		"content_types_accepted": []string{
			"application/json",
			"application/x-ndjson",
			"text/plain (will be parsed as NDJSON)",
		},
		"limits": map[string]interface{}{
			"max_records_per_batch": maxRecordsPerBatch,
			"max_payload_bytes":     maxIngestBytes,
		},
		"verticals_supported": []string{
			"refi_heloc",
			"personal_loans",
			"tax_relief",
			"remodel",
		},
		"example_json": map[string]interface{}{
			"records": []map[string]interface{}{
				{
					"email":       "first.last@example.com",
					"first_name":  "First",
					"last_name":   "Last",
					"zip":         "83854",
					"state":       "ID",
					"ip_address":  "203.0.113.42",
					"opt_in_date": "2026-05-01T14:22:00Z",
					"source":      "form-12-homepage",
				},
			},
		},
		"example_ndjson_line": `{"email":"first.last@example.com","first_name":"First","zip":"83854","opt_in_date":"2026-05-01T14:22:00Z"}`,
		"required_fields":     []string{"email"},
		"optional_fields":     []string{"first_name", "last_name", "zip", "state", "ip_address", "opt_in_date", "source", "metadata"},
		"async_pipeline": map[string]string{
			"step_1": "POST persists records to S3 and returns 202 immediately",
			"step_2": "Slicer worker drains in FIFO chunks and applies global suppression",
			"step_3": "EmailOversight validator verifies survivors",
			"step_4": "15-minute drip sends Verified records across our 4 mature brands",
		},
	})
}

// ============ payload parsing ============

func parseIngestPayload(body []byte, contentType string) ([]ingestRecord, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errors.New("payload is empty")
	}

	// Handle gzip-encoded uploads transparently.
	if isGzipBody(body) {
		gzr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("gzip decode: %w", err)
		}
		decoded, err := io.ReadAll(io.LimitReader(gzr, maxIngestBytes*8))
		gzr.Close()
		if err != nil {
			return nil, fmt.Errorf("gzip read: %w", err)
		}
		body = bytes.TrimSpace(decoded)
	}

	switch {
	case strings.Contains(contentType, "ndjson"):
		return parseNDJSON(body)
	case strings.Contains(contentType, "json") && body[0] == '{':
		return parseJSONObject(body)
	case body[0] == '{':
		// Heuristic: JSON object with records array.
		return parseJSONObject(body)
	default:
		return parseNDJSON(body)
	}
}

func parseJSONObject(body []byte) ([]ingestRecord, error) {
	var req ingestRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("expected object with `records` array: %w", err)
	}
	cleaned := make([]ingestRecord, 0, len(req.Records))
	for _, rec := range req.Records {
		if normalized, ok := normalizeRecord(rec); ok {
			cleaned = append(cleaned, normalized)
		}
	}
	return cleaned, nil
}

func parseNDJSON(body []byte) ([]ingestRecord, error) {
	trimmedBody := bytes.TrimSpace(body)

	// A JSON ARRAY is never valid NDJSON, but partners send one anyway. It has
	// to be handled BEFORE the line scan: scanning an array yields a SILENT
	// SUBSET — element lines that end in a comma fail Unmarshal and are
	// skipped, while the last element (no trailing comma) parses. That returns
	// a partial batch with no error, which is worse than rejecting it.
	if len(trimmedBody) > 0 && trimmedBody[0] == '[' {
		var arr []ingestRecord
		if err := json.Unmarshal(trimmedBody, &arr); err == nil {
			out := make([]ingestRecord, 0, len(arr))
			for _, rec := range arr {
				if normalized, ok := normalizeRecord(rec); ok {
					out = append(out, normalized)
				}
			}
			return out, nil
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)
	cleaned := make([]ingestRecord, 0, 1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec ingestRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if normalized, ok := normalizeRecord(rec); ok {
			cleaned = append(cleaned, normalized)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan ndjson: %w", err)
	}

	// Pretty-printed fallback (2026-08-09). A partner posting ONE
	// pretty-printed JSON object with an ndjson content-type loses every
	// record: each physical line ("{", `"email": "...",`) fails Unmarshal and
	// hits the `continue` above, so cleaned ends empty and the handler answers
	// 400 no_records — with no log line and the api-key's last_used_at still
	// stamped, which makes it look like the feed is connected when nothing is
	// landing. The Internal Car Insurance feed sat that way from 2026-07-28
	// (key creation) to 2026-08-09: 0 batches, 0 S3 objects, 0 queue rows.
	// Formatting is not a reason to discard a well-formed record, so when
	// line-parsing yields nothing, retry the whole body as a single object or
	// a {"records":[...]} envelope before giving up.
	if len(cleaned) == 0 && len(trimmedBody) > 0 && trimmedBody[0] == '{' {
		var rec ingestRecord
		if err := json.Unmarshal(trimmedBody, &rec); err == nil {
			if normalized, ok := normalizeRecord(rec); ok {
				return []ingestRecord{normalized}, nil
			}
		}
		if recs, err := parseJSONObject(trimmedBody); err == nil && len(recs) > 0 {
			return recs, nil
		}
	}
	return cleaned, nil
}

func normalizeRecord(rec ingestRecord) (ingestRecord, bool) {
	rec.Email = strings.ToLower(strings.TrimSpace(rec.Email))
	if rec.Email == "" || len(rec.Email) > maxEmailLength {
		return ingestRecord{}, false
	}
	if !strings.Contains(rec.Email, "@") || strings.Contains(rec.Email, " ") {
		return ingestRecord{}, false
	}
	return rec, true
}

func isGzipBody(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}

// ============ misc helpers ============

func mustJSONString(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func formatNullTime(nt sql.NullTime) interface{} {
	if !nt.Valid {
		return nil
	}
	return nt.Time.Format(time.RFC3339)
}

func nullStringValue(ns sql.NullString) interface{} {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

func readClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if comma := strings.Index(xff, ","); comma > 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return strings.TrimSpace(xff)
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return strings.TrimSpace(real)
	}
	return strings.TrimSpace(r.RemoteAddr)
}
