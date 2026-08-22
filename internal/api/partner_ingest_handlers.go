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
	Address1  string                 `json:"address_1,omitempty"`
	IPAddress string                 `json:"ip_address,omitempty"`
	OptInDate string                 `json:"opt_in_date,omitempty"`
	SignupURL string                 `json:"signup_url,omitempty"`
	SignupAt  string                 `json:"signup_date,omitempty"`
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
		PostalCode string                 `json:"postal_code"`
		Data       map[string]interface{} `json:"data"`
		SignupIP   string                 `json:"signup_ip"`
		OptInIP    string                 `json:"opt_in_ip"`
		OptInURL   string                 `json:"opt_in_url"`
		TID        json.RawMessage        `json:"tid"`

		// Attribits gmail_v1/v2 spellings (2026-08-21). This feed posts a
		// 34-field record and only three names lined up with ours, so ZIP,
		// state, the per-user token and the whole vehicle block were being
		// destroyed at this door while the send looked healthy — the exact
		// failure class as the 2026-08-09 city loss and the 2026-08-13 tid loss.
		ZipCode      string          `json:"zip_code"`
		StateUpper   string          `json:"state_upper"`
		LongState    string          `json:"long_state"`
		County       string          `json:"county"`
		Var1         json.RawMessage `json:"var1"`
		VehicleMake  string          `json:"vehicle_make__title"`
		VehicleModel string          `json:"vehicle_model__title"`
		VehicleYear  json.RawMessage `json:"vehicle_year"`
		DOB          string          `json:"dob"`
		Gender       string          `json:"gender"`
		HomeOwner    json.RawMessage `json:"home_owner"`
		PartnerUUID  string          `json:"uuid"`
		EMD5         string          `json:"emd5"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*r = ingestRecord(v.raw)
	if strings.TrimSpace(r.Zip) == "" {
		r.Zip = v.PostalCode
	}
	if strings.TrimSpace(r.Zip) == "" {
		r.Zip = v.ZipCode
	}
	// Prefer the 2-letter form; fall back to the spelled-out state.
	if strings.TrimSpace(r.State) == "" {
		r.State = v.StateUpper
	}
	if strings.TrimSpace(r.State) == "" {
		r.State = v.LongState
	}
	if strings.TrimSpace(r.IPAddress) == "" {
		r.IPAddress = v.SignupIP
	}
	if strings.TrimSpace(r.IPAddress) == "" {
		r.IPAddress = v.OptInIP
	}
	if strings.TrimSpace(r.SignupURL) == "" {
		r.SignupURL = v.OptInURL
	}
	// "data" is the partner's spelling of "metadata" and it carries `tid` — the
	// PER-USER money-link token. Dropping it silently ships every recipient the
	// same (or an empty) tokenid, which breaks attribution for the whole feed
	// while looking like a working send. Merge rather than replace so a payload
	// carrying both keys loses neither.
	if len(v.Data) > 0 {
		if r.Metadata == nil {
			r.Metadata = map[string]interface{}{}
		}
		for k, val := range v.Data {
			if _, exists := r.Metadata[k]; !exists {
				r.Metadata[k] = val
			}
		}
	}
	// Partners also send `tid` at the ROOT of the record (2026-08-13: the
	// ratesavings feed did exactly that after being told metadata was
	// optional, and the token died at this door). Fold a root-level tid into
	// Metadata — that is the only spot extraMetadataJSON carries through to
	// partner_clean_queue and the only spot personaFieldsFromExtra reads it
	// back out. A nested metadata/data tid wins over the root spelling.
	if tid := scalarToString(v.TID); tid != "" {
		if r.Metadata == nil {
			r.Metadata = map[string]interface{}{}
		}
		if _, exists := r.Metadata["tid"]; !exists {
			r.Metadata["tid"] = tid
		}
	}
	// `var1` is the Attribits gmail_v1/v2 spelling of the per-user money-link
	// token — same hex-encoded 16-byte shape as the tid the other auto feeds
	// send. Lower precedence than an explicit tid so a payload carrying both
	// keeps the canonical one.
	if tid := scalarToString(v.Var1); tid != "" {
		if r.Metadata == nil {
			r.Metadata = map[string]interface{}{}
		}
		if _, exists := r.Metadata["tid"]; !exists {
			r.Metadata["tid"] = tid
		}
	}
	// Everything else this feed sends that has no column of its own. Metadata
	// is the ONLY map that survives partnerRawRecord and extraMetadataJSON
	// intact, so folding here needs no change to the other two closed lists.
	// `vehicle` is composed because that is the form a creative actually uses
	// ("2004 Toyota Camry"); the parts are kept alongside it.
	putMeta := func(k, val string) {
		if strings.TrimSpace(val) == "" {
			return
		}
		if r.Metadata == nil {
			r.Metadata = map[string]interface{}{}
		}
		if _, exists := r.Metadata[k]; !exists {
			r.Metadata[k] = val
		}
	}
	year := scalarToString(v.VehicleYear)
	vehicle := strings.TrimSpace(strings.Join([]string{
		strings.TrimSpace(year), strings.TrimSpace(v.VehicleMake), strings.TrimSpace(v.VehicleModel),
	}, " "))
	putMeta("vehicle", strings.Join(strings.Fields(vehicle), " "))
	putMeta("vehicle_make", v.VehicleMake)
	putMeta("vehicle_model", v.VehicleModel)
	putMeta("vehicle_year", year)
	putMeta("county", v.County)
	putMeta("dob", v.DOB)
	putMeta("gender", v.Gender)
	putMeta("home_owner", scalarToString(v.HomeOwner))
	putMeta("partner_uuid", v.PartnerUUID)
	// The SimpleInsure wall wants s11 = the record's emd5. It equals
	// md5(lower(email)) (verified against 3 live rows), so it is derivable —
	// but carry the partner's own value so the token we send is byte-identical
	// to the one they issued rather than one we recomputed.
	putMeta("emd5", v.EMD5)
	return nil
}

// scalarToString decodes a JSON scalar (string or number) leniently. A partner
// posting tid as a bare number must not fail the whole record's unmarshal —
// one bad field type would otherwise drop the record entirely.
func scalarToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
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

	// Raw-sample capture (contract review): the canonical NDJSON below is a
	// re-marshal through the CLOSED ingestRecord struct, which silently
	// destroys any posted field the struct doesn't carry (see the struct
	// comment — the 2026-08-09 city/postal_code loss). Persist the FIRST
	// record's ORIGINAL bytes so the partner's actual payload survives for
	// schema-drift review. Best-effort and per-dataset throttled — it must
	// never fail or visibly slow the partner-facing request.
	h.captureRawSample(ctx, batchID, authCtx.DatasetID, bodyBytes, contentType)

	s3Key, _, indexDeferred, persistErr := persistPartnerBatch(ctx, h.db, h.s3Client, partnerBatchPersistInput{
		BatchID:     batchID,
		PartnerID:   authCtx.PartnerID,
		PartnerSlug: authCtx.PartnerSlug,
		DatasetID:   authCtx.DatasetID,
		DatasetSlug: authCtx.DatasetSlug,
		Vertical:    authCtx.Vertical,
		ReceivedAt:  receivedAt,
		ContentType: contentType,
		Records:     parsed,
		IngestMeta: map[string]interface{}{
			"api_key_prefix": authCtx.KeyPrefix,
			"remote_ip":      readClientIP(r),
			"user_agent":     r.UserAgent(),
		},
	})
	if persistErr != nil {
		partnerErrorResponse(w, http.StatusBadGateway, "s3_upload_failed", "Unable to persist batch right now. Retry in a moment.")
		return
	}
	if indexDeferred {
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

// ============ shared batch persistence ============

// partnerBatchPersistInput carries one batch through the shared persistence
// path. IngestMeta is caller-specific context merged into the batch row's
// ingest_metadata (content_type and bytes_written are always stamped).
type partnerBatchPersistInput struct {
	BatchID     string
	PartnerID   string
	PartnerSlug string
	DatasetID   string
	DatasetSlug string
	Vertical    string
	ReceivedAt  time.Time
	ContentType string
	Records     []ingestRecord
	IngestMeta  map[string]interface{}
}

// persistPartnerBatch is the ONE batch persistence path: canonical NDJSON
// (one re-marshaled ingestRecord per line, FIFO) → gzip to S3 (+ .meta.json
// audit sidecar) → INSERT partner_inbound_batches status='received'. Shared
// by the partner API door (HandlePostRecords) and the operator CSV upload
// (partner_csv_ingest.go) so the slicer sees byte-identical batches from
// both. Returns:
//   - err != nil            — the S3 upload failed; nothing was persisted.
//   - indexDeferred == true — S3 succeeded but the DB index insert failed;
//     the slicer's S3-discovery fallback will pick the batch up.
func persistPartnerBatch(ctx context.Context, db *sql.DB, s3c *PartnerIngestS3Client, in partnerBatchPersistInput) (s3Key string, bytesWritten int64, indexDeferred bool, err error) {
	var canonicalNDJSON bytes.Buffer
	for _, rec := range in.Records {
		line, mErr := json.Marshal(rec)
		if mErr != nil {
			continue
		}
		canonicalNDJSON.Write(line)
		canonicalNDJSON.WriteByte('\n')
	}

	s3Key = s3c.BatchKey(in.PartnerSlug, in.DatasetSlug, in.BatchID, in.ReceivedAt)
	bytesWritten, uploadErr := s3c.UploadBatchGzip(ctx, s3Key, &canonicalNDJSON)
	if uploadErr != nil {
		log.Printf("[partner-ingest] s3 upload failed bucket=%s key=%s partner=%s dataset=%s err=%v",
			s3c.Bucket(), s3Key, in.PartnerSlug, in.DatasetSlug, uploadErr)
		return s3Key, 0, false, uploadErr
	}

	meta := PartnerBatchMeta{
		BatchID:      in.BatchID,
		PartnerID:    in.PartnerID,
		PartnerSlug:  in.PartnerSlug,
		DatasetID:    in.DatasetID,
		DatasetSlug:  in.DatasetSlug,
		Vertical:     in.Vertical,
		RecordCount:  len(in.Records),
		ReceivedAt:   in.ReceivedAt,
		S3Bucket:     s3c.Bucket(),
		S3Key:        s3Key,
		ContentBytes: bytesWritten,
	}
	_ = s3c.UploadBatchMeta(ctx, s3c.MetaKey(in.PartnerSlug, in.DatasetSlug, in.BatchID, in.ReceivedAt), meta)

	ingestMeta := map[string]interface{}{
		"content_type":  in.ContentType,
		"bytes_written": bytesWritten,
	}
	for k, v := range in.IngestMeta {
		ingestMeta[k] = v
	}

	_, dbErr := db.ExecContext(ctx, `
		INSERT INTO partner_inbound_batches
		    (id, dataset_id, partner_id, s3_bucket, s3_key, record_count, status,
		     emergency_stopped, next_record_offset, received_at, ingest_metadata)
		VALUES ($1, $2, $3, $4, $5, $6, 'received', false, 0, $7, $8::jsonb)
	`,
		in.BatchID, in.DatasetID, in.PartnerID,
		s3c.Bucket(), s3Key, len(in.Records),
		in.ReceivedAt,
		mustJSONString(ingestMeta),
	)
	if dbErr != nil {
		return s3Key, bytesWritten, true, nil
	}
	return s3Key, bytesWritten, false, nil
}

// ============ raw-sample capture ============

// rawSampleMaxBytes caps the stored sample: 16 KiB comfortably holds any
// single partner record while bounding row size under the observed
// 1-record-batch flood pattern (~70k POSTs/day on one dataset).
const rawSampleMaxBytes = 16 * 1024

// rawSampleInsertSQL writes at most one sample row per dataset per 10
// minutes (WHERE NOT EXISTS on a recent row for the SAME dataset). Contract
// review needs per-dataset freshness, not per-batch coverage — an unguarded
// capture under the 1-record-batch flood would write ~70k rows/day.
const rawSampleInsertSQL = `
	INSERT INTO partner_ingest_raw_samples (batch_id, dataset_id, sample, content_type)
	SELECT $1, $2, $3, $4
	WHERE NOT EXISTS (
		SELECT 1 FROM partner_ingest_raw_samples
		WHERE dataset_id = $2
		  AND captured_at > NOW() - INTERVAL '10 minutes'
	)`

// captureRawSample persists the first posted record's raw bytes so the
// original payload survives the closed-struct re-marshal for contract
// review. Best-effort by design: every failure is logged and swallowed —
// the capture must be invisible to the partner.
func (h *PartnerIngestHandler) captureRawSample(ctx context.Context, batchID, datasetID string, body []byte, contentType string) {
	sample := extractFirstRawRecord(body, contentType)
	if len(sample) == 0 {
		return
	}
	if len(sample) > rawSampleMaxBytes {
		sample = sample[:rawSampleMaxBytes]
	}
	// Postgres TEXT rejects NUL bytes and invalid UTF-8 (which the cap
	// truncation above can create mid-rune); sanitize so a weird payload
	// degrades to a mangled sample instead of a failed capture.
	text := strings.ToValidUTF8(strings.ReplaceAll(string(sample), "\x00", ""), "�")
	if _, err := h.db.ExecContext(ctx, rawSampleInsertSQL, batchID, datasetID, text, contentType); err != nil {
		log.Printf("[partner-ingest] raw-sample capture failed batch=%s dataset=%s err=%v", batchID, datasetID, err)
	}
}

// extractFirstRawRecord returns the raw bytes of the FIRST record in the
// posted body WITHOUT decoding through ingestRecord, mirroring
// parseIngestPayload's branching (gzip → content-type → shape). Returns nil
// when no record can be isolated; the caller skips capture in that case.
func extractFirstRawRecord(body []byte, contentType string) []byte {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	if isGzipBody(body) {
		gzr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil
		}
		decoded, err := io.ReadAll(io.LimitReader(gzr, maxIngestBytes*8))
		gzr.Close()
		if err != nil {
			return nil
		}
		body = bytes.TrimSpace(decoded)
		if len(body) == 0 {
			return nil
		}
	}
	// JSON envelope {"records":[...]}: the first element's raw bytes.
	if body[0] == '{' && !strings.Contains(contentType, "ndjson") {
		var env struct {
			Records []json.RawMessage `json:"records"`
		}
		if err := json.Unmarshal(body, &env); err == nil && len(env.Records) > 0 {
			return bytes.TrimSpace(env.Records[0])
		}
		// Bare single object: the body IS the first record.
		return body
	}
	// JSON array (partners post one to the NDJSON path — see parseNDJSON).
	if body[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
			return bytes.TrimSpace(arr[0])
		}
		return nil
	}
	// NDJSON: the first non-empty line.
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) > 0 {
			out := make([]byte, len(line))
			copy(out, line)
			return out
		}
	}
	return nil
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
		// Sourced from PartnerVerticals (partner_admin_handlers.go) — the ONE
		// Go source; the DB CHECK constraint (cmd/server/main.go) enforces it.
		"verticals_supported": append([]string{}, PartnerVerticals...),
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
		"required_fields": []string{"email"},
		"optional_fields": []string{
			"first_name", "last_name", "city", "zip (alias: postal_code)", "state", "address_1",
			"ip_address (aliases: signup_ip, opt_in_ip)", "opt_in_date", "signup_url (alias: opt_in_url)",
			"signup_date", "source", "tid (root level, or nested under metadata/data)",
			"metadata (alias: data — arbitrary key/value object, preserved verbatim)",
		},
		"field_notes": map[string]string{
			"tid":      "Per-user tracking token used in money links. Accepted at the record root or inside metadata/data; if both are present the nested value wins.",
			"metadata": "The metadata object is OPTIONAL. Documented fields may sit at the root. Undocumented fields are only preserved if placed inside metadata (or data).",
		},
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
