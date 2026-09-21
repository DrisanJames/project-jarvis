package api

// Static upload door — the S3-first path for at-rest data.
//
// Ruling (brain #3589): a file sitting on the operator's Mac is not a
// repository. Static drops go to s3://jarvis-partner-ingest/static/ FIRST,
// and only then become partner_inbound_batches rows. This file is that door:
//
//	POST /api/mailing/data-ingest/static/presign   → multipart upload URLs
//	POST /api/mailing/data-ingest/static/complete  → CompleteMultipartUpload + HeadObject
//	POST /api/mailing/data-ingest/static/register  → object → partner batches
//	GET  /api/mailing/data-ingest/static/objects       (this org, newest first)
//	GET  /api/mailing/data-ingest/static/objects/{id}
//
// register NEVER re-uploads: it streams GetObject through commitCSVReader —
// the EXACT record-building path the operator CSV door uses
// (partner_csv_ingest.go) — so the slicer and EO validator see byte-identical
// batches from both doors. Every batch row is stamped supply_class='at_rest',
// source_path='static_upload', object_sha256=<the object's sha256>, and
// carries the static object's bucket/key/id in ingest_metadata.
//
// Auth: mounted inside the authenticated /api/mailing router (session /
// X-Admin-Key). Org: data_ingest_static_objects IS org-scoped
// (organization_id) — every read and write filters on GetOrgIDFromRequest.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/dataingest"
)

const (
	// staticUploadPartSize is the multipart part size handed to the browser.
	// 64 MiB × the 10,000-part S3 ceiling = a 640 GiB object ceiling.
	staticUploadPartSize int64 = 64 * 1024 * 1024
	staticUploadMaxParts       = 10000
	// staticPresignTTL is how long a part URL stays valid.
	staticPresignTTL = time.Hour
	// staticUploadSourcePath is the batch row's source_path marker, and the
	// value the dashboard's at_rest class counts on.
	staticUploadSourcePath = "static_upload"
	staticUploadSupplyClas = "at_rest"
)

// dataIngestS3API is the narrow slice of S3 this door needs. Declared here
// (not in partner_s3.go — that file's public API is untouched) so tests can
// substitute a fake without an httptest server for HEAD/GET semantics.
type dataIngestS3API interface {
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// partnerIngestS3Adapter adapts the existing PartnerIngestS3Client to
// dataIngestS3API by forwarding to its embedded *s3.Client. A thin adapter
// rather than new methods on PartnerIngestS3Client, so the partner ingest
// surface keeps exactly the API it has today.
type partnerIngestS3Adapter struct{ pc *PartnerIngestS3Client }

func (a partnerIngestS3Adapter) HeadObject(ctx context.Context, in *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return a.pc.client.HeadObject(ctx, in, optFns...)
}

func (a partnerIngestS3Adapter) CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	return a.pc.client.CreateMultipartUpload(ctx, in, optFns...)
}

func (a partnerIngestS3Adapter) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	return a.pc.client.CompleteMultipartUpload(ctx, in, optFns...)
}

func (a partnerIngestS3Adapter) GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return a.pc.client.GetObject(ctx, in, optFns...)
}

// DataIngestStaticService is the static upload door.
type DataIngestStaticService struct {
	db      *sql.DB
	s3      *PartnerIngestS3Client
	presign *s3.PresignClient
	// api is the HEAD/multipart/GET surface — the adapter over s3 in prod,
	// a fake in tests.
	api dataIngestS3API
}

func NewDataIngestStaticService(db *sql.DB, s3c *PartnerIngestS3Client) *DataIngestStaticService {
	svc := &DataIngestStaticService{db: db}
	svc.SetS3Client(s3c)
	return svc
}

// SetS3Client allows late wiring during boot (same idiom as
// PartnerCSVIngestService.SetS3Client).
func (s *DataIngestStaticService) SetS3Client(c *PartnerIngestS3Client) {
	s.s3 = c
	if c == nil {
		s.api = nil
		s.presign = nil
		return
	}
	s.api = partnerIngestS3Adapter{pc: c}
	s.presign = s3.NewPresignClient(c.client)
}

// RegisterRoutes mounts the door. Mount with:
//
//	r.Route("/data-ingest/static", staticSvc.RegisterRoutes)
func (s *DataIngestStaticService) RegisterRoutes(r chi.Router) {
	r.Post("/presign", s.HandlePresign)
	r.Post("/complete", s.HandleComplete)
	r.Post("/register", s.HandleRegister)
	r.Get("/objects", s.HandleListObjects)
	r.Get("/objects/{id}", s.HandleGetObject)
}

func (s *DataIngestStaticService) ready(w http.ResponseWriter) bool {
	if s.db == nil || s.s3 == nil || s.api == nil {
		respondError(w, http.StatusServiceUnavailable, "ingest pipeline is initialising — retry in a moment")
		return false
	}
	return true
}

// ── key building ────────────────────────────────────────────────────────────

// staticObjectKey is the canonical layout: static/<source-slug>/<YYYY-MM-DD>/<file>.
// The date is the DENVER date, not UTC: the dashboard buckets ingest by
// Denver day, and an evening drop keyed to UTC would file itself under
// tomorrow — the operator looking for "the file I uploaded yesterday" would
// not find it under yesterday's prefix.
func staticObjectKey(source, filename string, at time.Time) string {
	return fmt.Sprintf("static/%s/%s/%s",
		sanitizeSlug(source),
		at.In(denverLocation()).Format("2006-01-02"),
		sanitizeStaticFilename(filename),
	)
}

// denverLocation degrades to UTC if the tzdata is unavailable in the image.
func denverLocation() *time.Location {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return time.UTC
	}
	return loc
}

// sanitizeStaticFilename keeps the operator's filename recognisable (dots and
// case survive — sanitizeSlug would eat the extension) while refusing path
// traversal and anything that would need URL escaping in a key.
func sanitizeStaticFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name) // strips any directory component, incl. ../
	if name == "." || name == "/" || name == ".." {
		name = ""
	}
	out := make([]byte, 0, len(name))
	lastDash := false
	for i := 0; i < len(name); i++ {
		ch := name[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '.', ch == '-', ch == '_':
			out = append(out, ch)
			lastDash = false
		default:
			if !lastDash {
				out = append(out, '-')
				lastDash = true
			}
		}
	}
	r := strings.Trim(string(out), "-.")
	if len(r) > 200 {
		r = r[:200]
	}
	if r == "" {
		return "upload.csv"
	}
	return r
}

// ── the row ─────────────────────────────────────────────────────────────────

type staticObjectRow struct {
	ID           string                 `json:"id"`
	OrgID        string                 `json:"organization_id"`
	Bucket       string                 `json:"s3_bucket"`
	Key          string                 `json:"s3_key"`
	SHA256       string                 `json:"sha256,omitempty"`
	Bytes        int64                  `json:"bytes"`
	ContentType  string                 `json:"content_type,omitempty"`
	Source       string                 `json:"source"`
	DatasetID    string                 `json:"dataset_id,omitempty"`
	Declared     map[string]interface{} `json:"declared,omitempty"`
	UploadedBy   string                 `json:"uploaded_by,omitempty"`
	UploadedAt   time.Time              `json:"uploaded_at"`
	RegisteredAt *time.Time             `json:"registered_at,omitempty"`
	BatchID      string                 `json:"batch_id,omitempty"`
	Status       string                 `json:"status"`
}

const staticObjectSelect = `
	SELECT id, organization_id, s3_bucket, s3_key, COALESCE(sha256,''), COALESCE(bytes,0),
	       COALESCE(content_type,''), source, COALESCE(dataset_id::text,''),
	       COALESCE(declared::text,'{}'), COALESCE(uploaded_by,''), uploaded_at,
	       registered_at, COALESCE(batch_id::text,''), status
	FROM data_ingest_static_objects`

func scanStaticObject(sc interface{ Scan(...interface{}) error }) (staticObjectRow, error) {
	var (
		row      staticObjectRow
		declared string
		regAt    sql.NullTime
	)
	err := sc.Scan(&row.ID, &row.OrgID, &row.Bucket, &row.Key, &row.SHA256, &row.Bytes,
		&row.ContentType, &row.Source, &row.DatasetID, &declared, &row.UploadedBy,
		&row.UploadedAt, &regAt, &row.BatchID, &row.Status)
	if err != nil {
		return row, err
	}
	if regAt.Valid {
		t := regAt.Time
		row.RegisteredAt = &t
	}
	if declared != "" {
		_ = json.Unmarshal([]byte(declared), &row.Declared)
	}
	return row, nil
}

// loadStaticObject fetches one object BY ORG — a different org's object is a
// 404, never a read.
func (s *DataIngestStaticService) loadStaticObject(ctx context.Context, orgID, id, key string) (staticObjectRow, error) {
	var (
		q    string
		args []interface{}
	)
	if id != "" {
		q, args = staticObjectSelect+` WHERE id = $1 AND organization_id = $2`, []interface{}{id, orgID}
	} else {
		q, args = staticObjectSelect+` WHERE s3_key = $1 AND organization_id = $2`, []interface{}{key, orgID}
	}
	return scanStaticObject(s.db.QueryRowContext(ctx, q, args...))
}

// ── POST /presign ───────────────────────────────────────────────────────────

type staticPresignRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
	Source      string `json:"source"`
	DatasetID   string `json:"dataset_id"`
}

func (s *DataIngestStaticService) HandlePresign(w http.ResponseWriter, r *http.Request) {
	if !s.ready(w) {
		return
	}
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context is required")
		return
	}
	var req staticPresignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Filename) == "" {
		respondError(w, http.StatusBadRequest, "filename is required")
		return
	}
	if strings.TrimSpace(req.Source) == "" {
		respondError(w, http.StatusBadRequest, "source is required — it is the key's second segment (static/<source>/<date>/<file>)")
		return
	}
	if req.Bytes <= 0 {
		respondError(w, http.StatusBadRequest, "bytes must be > 0 — the part count is derived from it")
		return
	}
	if req.DatasetID != "" && !isValidUUID(req.DatasetID) {
		respondError(w, http.StatusBadRequest, "dataset_id must be a valid dataset UUID")
		return
	}
	parts := int((req.Bytes + staticUploadPartSize - 1) / staticUploadPartSize)
	if parts < 1 {
		parts = 1
	}
	if parts > staticUploadMaxParts {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("object needs %d parts — the S3 ceiling is %d at a %d MiB part size; split the file",
			parts, staticUploadMaxParts, staticUploadPartSize/1024/1024))
		return
	}

	key := staticObjectKey(req.Source, req.Filename, time.Now())

	// A key already claimed by a live row is refused: re-presigning it would
	// overwrite an object another batch already points at. A 'failed' row is
	// the retry state and does NOT block.
	var existingID, existingStatus string
	err = s.db.QueryRowContext(r.Context(), `
		SELECT id::text, status FROM data_ingest_static_objects
		WHERE s3_key = $1 AND status <> 'failed'
		LIMIT 1
	`, key).Scan(&existingID, &existingStatus)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusInternalServerError, "static object lookup failed")
		return
	}
	if err == nil {
		respondJSON(w, http.StatusConflict, map[string]interface{}{
			"error":     "key already exists — rename the file or use a different source",
			"s3_key":    key,
			"object_id": existingID,
			"status":    existingStatus,
		})
		return
	}

	actor := actorFromRequest(r)
	objectID := uuid.New().String()
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" {
		contentType = "text/csv"
	}

	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO data_ingest_static_objects
		    (id, organization_id, s3_bucket, s3_key, bytes, content_type, source,
		     dataset_id, declared, uploaded_by, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8,'')::uuid, $9::jsonb, $10, 'uploading')
	`, objectID, orgID.String(), s.s3.Bucket(), key, req.Bytes, contentType,
		strings.TrimSpace(req.Source), req.DatasetID,
		mustJSONString(map[string]interface{}{
			"filename":       req.Filename,
			"declared_bytes": req.Bytes,
		}), actor); err != nil {
		respondError(w, http.StatusInternalServerError, "could not record the static object")
		return
	}

	cmu, err := s.api.CreateMultipartUpload(r.Context(), &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.s3.Bucket()),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		s.markStatus(r.Context(), objectID, "failed", map[string]interface{}{"error": err.Error()})
		respondError(w, http.StatusBadGateway, "CreateMultipartUpload failed: "+err.Error())
		return
	}
	uploadID := aws.ToString(cmu.UploadId)
	s.mergeDeclared(r.Context(), objectID, map[string]interface{}{"upload_id": uploadID})

	if s.presign == nil {
		respondError(w, http.StatusServiceUnavailable, "presigner is not wired")
		return
	}
	type partURL struct {
		PartNumber int32  `json:"part_number"`
		URL        string `json:"url"`
	}
	urls := make([]partURL, 0, parts)
	for n := 1; n <= parts; n++ {
		// ContentLength is deliberately NOT set: the browser's PUT length is
		// unknown at signing time, and binding it would make every part
		// signature length-specific. The SDK signs S3 presigned requests with
		// UNSIGNED-PAYLOAD, so the body is not part of the signature either.
		pr, perr := s.presign.PresignUploadPart(r.Context(), &s3.UploadPartInput{
			Bucket:     aws.String(s.s3.Bucket()),
			Key:        aws.String(key),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(int32(n)),
		}, s3.WithPresignExpires(staticPresignTTL))
		if perr != nil {
			s.markStatus(r.Context(), objectID, "failed", map[string]interface{}{"error": perr.Error()})
			respondError(w, http.StatusBadGateway, "presign UploadPart failed: "+perr.Error())
			return
		}
		urls = append(urls, partURL{PartNumber: int32(n), URL: pr.URL})
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"organization_id": orgID.String(),
		"object_id":       objectID,
		"s3_bucket":       s.s3.Bucket(),
		"s3_key":          key,
		"upload_id":       uploadID,
		"part_size":       staticUploadPartSize,
		"part_count":      parts,
		"parts":           urls,
		"expires_in":      int(staticPresignTTL.Seconds()),
		"method":          http.MethodPut,
		"message":         "PUT each part to its URL, collect the ETag response header, then POST /complete",
	})
}

// ── POST /complete ──────────────────────────────────────────────────────────

type staticCompleteRequest struct {
	ObjectID string `json:"object_id"`
	UploadID string `json:"upload_id"`
	SHA256   string `json:"sha256"`
	Parts    []struct {
		PartNumber int32  `json:"part_number"`
		N          int32  `json:"n"`
		ETag       string `json:"etag"`
	} `json:"parts"`
}

func (s *DataIngestStaticService) HandleComplete(w http.ResponseWriter, r *http.Request) {
	if !s.ready(w) {
		return
	}
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context is required")
		return
	}
	var req staticCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !isValidUUID(req.ObjectID) {
		respondError(w, http.StatusBadRequest, "object_id must be a valid UUID")
		return
	}
	if len(req.Parts) == 0 {
		respondError(w, http.StatusBadRequest, "parts[] is required — {part_number, etag} per uploaded part")
		return
	}
	obj, err := s.loadStaticObject(r.Context(), orgID.String(), req.ObjectID, "")
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusNotFound, "static object not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "static object lookup failed")
		return
	}
	if obj.Status != "uploading" {
		respondError(w, http.StatusConflict, "static object is '"+obj.Status+"' — only an 'uploading' object can be completed")
		return
	}
	uploadID := strings.TrimSpace(req.UploadID)
	if uploadID == "" {
		if v, ok := obj.Declared["upload_id"].(string); ok {
			uploadID = v
		}
	}
	if uploadID == "" {
		respondError(w, http.StatusBadRequest, "upload_id is required (none recorded at presign)")
		return
	}

	completed := make([]types.CompletedPart, 0, len(req.Parts))
	for _, p := range req.Parts {
		n := p.PartNumber
		if n == 0 {
			n = p.N
		}
		if n < 1 || strings.TrimSpace(p.ETag) == "" {
			respondError(w, http.StatusBadRequest, "every part needs a part_number >= 1 and its ETag")
			return
		}
		completed = append(completed, types.CompletedPart{
			ETag:       aws.String(p.ETag),
			PartNumber: aws.Int32(n),
		})
	}
	sort.Slice(completed, func(i, j int) bool {
		return aws.ToInt32(completed[i].PartNumber) < aws.ToInt32(completed[j].PartNumber)
	})

	if _, err := s.api.CompleteMultipartUpload(r.Context(), &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(obj.Bucket),
		Key:             aws.String(obj.Key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		s.markStatus(r.Context(), obj.ID, "failed", map[string]interface{}{"error": err.Error()})
		respondError(w, http.StatusBadGateway, "CompleteMultipartUpload failed: "+err.Error())
		return
	}

	// HeadObject is the confirmation: the object EXISTS, with these bytes and
	// this server-side etag. Never trust the client's word for it.
	head, err := s.api.HeadObject(r.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(obj.Bucket),
		Key:    aws.String(obj.Key),
	})
	if err != nil {
		s.markStatus(r.Context(), obj.ID, "failed", map[string]interface{}{"error": "head after complete: " + err.Error()})
		respondError(w, http.StatusBadGateway, "object did not confirm after complete: "+err.Error())
		return
	}
	bytesWritten := aws.ToInt64(head.ContentLength)
	etag := strings.Trim(aws.ToString(head.ETag), `"`)

	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE data_ingest_static_objects
		SET status = 'object', bytes = $2, sha256 = NULLIF($3,''),
		    declared = COALESCE(declared,'{}'::jsonb) || $4::jsonb
		WHERE id = $1 AND organization_id = $5
	`, obj.ID, bytesWritten, strings.TrimSpace(req.SHA256),
		mustJSONString(map[string]interface{}{"etag": etag, "parts": len(completed)}),
		orgID.String()); err != nil {
		respondError(w, http.StatusInternalServerError, "could not record the completed object")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"organization_id": orgID.String(),
		"object_id":       obj.ID,
		"s3_bucket":       obj.Bucket,
		"s3_key":          obj.Key,
		"bytes":           bytesWritten,
		"etag":            etag,
		"sha256":          strings.TrimSpace(req.SHA256),
		"status":          "object",
	})
}

// ── POST /register ──────────────────────────────────────────────────────────

type staticRegisterRequest struct {
	ObjectID  string         `json:"object_id"`
	Key       string         `json:"key"`
	DatasetID string         `json:"dataset_id"`
	Mapping   map[string]int `json:"mapping"`
	Declared  struct {
		CleanedUpstream bool   `json:"cleaned_upstream"`
		LandingStatus   string `json:"landing_status"`
		Dedupe          string `json:"dedupe"`
		Note            string `json:"note"`
	} `json:"declared"`
}

// resolveLandingStatus: the operator's declaration wins; otherwise a drop
// declared clean upstream lands 'ready' and everything else lands 'held'.
// HELD IS THE DEFAULT — an unvetted at-rest drop never mails on arrival.
func resolveLandingStatus(declared string, cleanedUpstream bool) (string, error) {
	switch strings.TrimSpace(strings.ToLower(declared)) {
	case "held", "pending_eo", "ready":
		return strings.TrimSpace(strings.ToLower(declared)), nil
	case "":
		if cleanedUpstream {
			return "ready", nil
		}
		return "held", nil
	default:
		return "", fmt.Errorf("landing_status must be held | pending_eo | ready")
	}
}

func (s *DataIngestStaticService) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.ready(w) {
		return
	}
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context is required")
		return
	}
	var req staticRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ObjectID == "" && strings.TrimSpace(req.Key) == "" {
		respondError(w, http.StatusBadRequest, "object_id or key is required")
		return
	}
	if req.ObjectID != "" && !isValidUUID(req.ObjectID) {
		respondError(w, http.StatusBadRequest, "object_id must be a valid UUID")
		return
	}
	if !isValidUUID(req.DatasetID) {
		respondError(w, http.StatusBadRequest, "dataset_id must be a valid dataset UUID")
		return
	}
	landing, lerr := resolveLandingStatus(req.Declared.LandingStatus, req.Declared.CleanedUpstream)
	if lerr != nil {
		respondError(w, http.StatusBadRequest, lerr.Error())
		return
	}

	obj, err := s.loadStaticObject(r.Context(), orgID.String(), req.ObjectID, strings.TrimSpace(req.Key))
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusNotFound, "static object not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "static object lookup failed")
		return
	}
	if obj.Status == "loaded" {
		respondError(w, http.StatusConflict, "static object is already loaded (batch "+obj.BatchID+")")
		return
	}

	// THE GATE: the object must actually be in the bucket. A row without an
	// object is the red tile on the dashboard, never an ingest.
	head, herr := s.api.HeadObject(r.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(obj.Bucket),
		Key:    aws.String(obj.Key),
	})
	if herr != nil {
		log.Printf("[data-ingest-static] register refused bucket=%s key=%s err=%v", obj.Bucket, obj.Key, herr)
		respondError(w, http.StatusConflict, "object not in bucket")
		return
	}

	ident, ok := NewPartnerCSVIngestService(s.db, s.s3).resolveDataset(w, r, req.DatasetID)
	if !ok {
		return // resolveDataset already wrote 404 / 409 / 500
	}

	mapping := req.Mapping
	if len(mapping) == 0 {
		mapping, err = s.deriveMappingFromObject(r.Context(), obj)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	actor := actorFromRequest(r)
	sha := obj.SHA256
	// registered: the object is confirmed and the load has started. 'loaded'
	// follows only once the batches exist.
	s.markStatus(r.Context(), obj.ID, "registered", nil)

	body, gerr := s.api.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(obj.Bucket),
		Key:    aws.String(obj.Key),
	})
	if gerr != nil {
		s.markStatus(r.Context(), obj.ID, "failed", map[string]interface{}{"error": gerr.Error()})
		respondError(w, http.StatusBadGateway, "GetObject failed: "+gerr.Error())
		return
	}
	defer body.Body.Close()

	ingestMeta := map[string]interface{}{
		"source":           staticUploadSourcePath,
		"source_path":      staticUploadSourcePath,
		"supply_class":     staticUploadSupplyClas,
		"uploaded_by":      obj.UploadedBy,
		"registered_by":    actor,
		"filename":         staticDeclaredString(obj.Declared, "filename"),
		"static_object_id": obj.ID,
		"object_s3_bucket": obj.Bucket,
		"object_s3_key":    obj.Key,
		"object_sha256":    sha,
		"object_bytes":     aws.ToInt64(head.ContentLength),
		"landing_status":   landing,
		"cleaned_upstream": req.Declared.CleanedUpstream,
	}
	for k, v := range map[string]string{
		"dedupe": req.Declared.Dedupe,
		"note":   req.Declared.Note,
	} {
		if strings.TrimSpace(v) != "" {
			ingestMeta[k] = v
		}
	}

	res, cerr := NewPartnerCSVIngestService(s.db, s.s3).commitCSVReader(r.Context(), body.Body, csvCommitTarget{
		DatasetID:   req.DatasetID,
		Ident:       ident,
		ContentType: obj.ContentType,
		ReceivedAt:  time.Now().UTC(),
	}, mapping, ingestMeta)
	if cerr != nil {
		s.markStatus(r.Context(), obj.ID, "failed", map[string]interface{}{
			"error":     cerr.Error(),
			"batch_ids": res.BatchIDs,
		})
		writeCSVCommitError(w, cerr)
		return
	}

	// Stamp the at-rest provenance onto every batch row this object produced.
	// object_sha256 is what ties a batch back to the exact bytes in S3.
	stampDeferred := false
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE partner_inbound_batches
		SET supply_class = $2, source_path = $3, object_sha256 = NULLIF($4,'')
		WHERE id = ANY($1::uuid[])
	`, pgTextArray(res.BatchIDs), staticUploadSupplyClas, staticUploadSourcePath, sha); err != nil {
		// The batches are already persisted and slicer-visible; a missing
		// stamp is a dashboard-classification gap, not a data loss. Report it
		// rather than failing a completed load (same philosophy as the API
		// door's index_deferred).
		stampDeferred = true
		log.Printf("[data-ingest-static] supply_class stamp failed object=%s batches=%d err=%v", obj.ID, len(res.BatchIDs), err)
	}

	firstBatch := ""
	if len(res.BatchIDs) > 0 {
		firstBatch = res.BatchIDs[0]
	}
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE data_ingest_static_objects
		SET status = 'loaded', registered_at = NOW(),
		    dataset_id = $2::uuid, batch_id = NULLIF($3,'')::uuid,
		    declared = COALESCE(declared,'{}'::jsonb) || $4::jsonb
		WHERE id = $1 AND organization_id = $5
	`, obj.ID, req.DatasetID, firstBatch, mustJSONString(map[string]interface{}{
		"batch_ids":        res.BatchIDs,
		"records":          res.Records,
		"skipped_invalid":  res.SkippedInvalid,
		"landing_status":   landing,
		"cleaned_upstream": req.Declared.CleanedUpstream,
		"dedupe":           req.Declared.Dedupe,
		"note":             req.Declared.Note,
	}), orgID.String()); err != nil {
		log.Printf("[data-ingest-static] loaded-status update failed object=%s err=%v", obj.ID, err)
	}

	writeAuditLog(r.Context(), s.db, actor, "static_upload_register", "data_ingest_static_object", obj.ID, nil, map[string]interface{}{
		"s3_bucket": obj.Bucket, "s3_key": obj.Key, "sha256": sha,
		"dataset_id": req.DatasetID, "records": res.Records,
		"skipped_invalid": res.SkippedInvalid, "batches": len(res.BatchIDs),
		"batch_ids": res.BatchIDs, "landing_status": landing,
	})

	// One aggregate event for the dashboard's at_rest "load" tile. Dark-safe
	// and non-blocking: a dead bus or an OFF flag makes this a no-op, and the
	// nightly reconcile from the tables remains the settled truth.
	dataingest.Emit(r.Context(), dataingest.Event{
		Class:      dataingest.ClassAtRest,
		Source:     dataingest.SourceStaticUpload,
		PartnerID:  ident.PartnerID,
		DatasetID:  req.DatasetID,
		BatchID:    firstBatch,
		Lane:       ident.Vertical,
		Transition: dataingest.TransitionLoadRegistered,
		N:          res.Records,
	})

	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"organization_id": orgID.String(),
		"object_id":       obj.ID,
		"s3_bucket":       obj.Bucket,
		"s3_key":          obj.Key,
		"object_sha256":   sha,
		"dataset_id":      req.DatasetID,
		"dataset_name":    ident.DatasetName,
		"supply_class":    staticUploadSupplyClas,
		"source_path":     staticUploadSourcePath,
		"landing_status":  landing,
		"batch_ids":       res.BatchIDs,
		"batches":         len(res.BatchIDs),
		"records":         res.Records,
		"skipped_invalid": res.SkippedInvalid,
		"index_deferred":  res.IndexDeferred,
		"stamp_deferred":  stampDeferred,
		"status":          "loaded",
		"message":         "object registered; the slicer + EO validator process the batches asynchronously (same path as the CSV door)",
	})
}

// deriveMappingFromObject reads ONLY the header row of the object and derives
// the mapping the preview door would have suggested. Used when the caller
// (e.g. agents/jobs/static_register.py) sends no explicit mapping.
func (s *DataIngestStaticService) deriveMappingFromObject(ctx context.Context, obj staticObjectRow) (map[string]int, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(obj.Bucket),
		Key:    aws.String(obj.Key),
	})
	if err != nil {
		return nil, fmt.Errorf("could not read the object to derive a mapping: %w", err)
	}
	defer out.Body.Close()
	first, err := newPartnerCSVReader(out.Body).Read()
	if err != nil {
		return nil, fmt.Errorf("object is empty or not parseable CSV")
	}
	if !csvLooksLikeHeader(first) {
		return nil, fmt.Errorf("object has no header row — send an explicit mapping {target: column_index}")
	}
	mapping := map[string]int{}
	for _, sug := range suggestCSVMapping(first) {
		if sug.Target == "" {
			continue
		}
		if _, taken := mapping[sug.Target]; taken {
			continue // first column wins, same as a preview-confirmed mapping
		}
		mapping[sug.Target] = sug.ColumnIndex
	}
	if _, ok := mapping["email"]; !ok {
		return nil, fmt.Errorf("no email column detected in the object header — send an explicit mapping {target: column_index}")
	}
	return mapping, nil
}

// ── GET /objects, GET /objects/{id} ─────────────────────────────────────────

func (s *DataIngestStaticService) HandleListObjects(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "ingest pipeline is initialising — retry in a moment")
		return
	}
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context is required")
		return
	}
	limit := 100
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	rows, err := s.db.QueryContext(r.Context(), staticObjectSelect+`
		WHERE organization_id = $1
		  AND ($2 = '' OR status = $2)
		ORDER BY uploaded_at DESC
		LIMIT $3`, orgID.String(), status, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "static object list failed")
		return
	}
	defer rows.Close()
	objects := []staticObjectRow{}
	for rows.Next() {
		obj, scanErr := scanStaticObject(rows)
		if scanErr != nil {
			respondError(w, http.StatusInternalServerError, "static object scan failed")
			return
		}
		objects = append(objects, obj)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"organization_id": orgID.String(),
		"objects":         objects,
		"count":           len(objects),
		"as_of":           time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *DataIngestStaticService) HandleGetObject(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "ingest pipeline is initialising — retry in a moment")
		return
	}
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization context is required")
		return
	}
	id := chi.URLParam(r, "id")
	if !isValidUUID(id) {
		respondError(w, http.StatusBadRequest, "id must be a valid UUID")
		return
	}
	obj, err := s.loadStaticObject(r.Context(), orgID.String(), id, "")
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusNotFound, "static object not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "static object lookup failed")
		return
	}
	respondJSON(w, http.StatusOK, obj)
}

// ── small helpers ───────────────────────────────────────────────────────────

// markStatus flips the object's status, optionally merging context into
// declared. Best-effort: a status write must never mask the real error.
func (s *DataIngestStaticService) markStatus(ctx context.Context, id, status string, merge map[string]interface{}) {
	if s.db == nil {
		return
	}
	if merge == nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE data_ingest_static_objects SET status = $2 WHERE id = $1`, id, status); err != nil {
			log.Printf("[data-ingest-static] status=%s write failed object=%s err=%v", status, id, err)
		}
		return
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE data_ingest_static_objects
		SET status = $2, declared = COALESCE(declared,'{}'::jsonb) || $3::jsonb
		WHERE id = $1`, id, status, mustJSONString(merge)); err != nil {
		log.Printf("[data-ingest-static] status=%s write failed object=%s err=%v", status, id, err)
	}
}

func (s *DataIngestStaticService) mergeDeclared(ctx context.Context, id string, merge map[string]interface{}) {
	if s.db == nil || len(merge) == 0 {
		return
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE data_ingest_static_objects
		SET declared = COALESCE(declared,'{}'::jsonb) || $2::jsonb
		WHERE id = $1`, id, mustJSONString(merge)); err != nil {
		log.Printf("[data-ingest-static] declared merge failed object=%s err=%v", id, err)
	}
}

func staticDeclaredString(declared map[string]interface{}, key string) string {
	if declared == nil {
		return ""
	}
	if v, ok := declared[key].(string); ok {
		return v
	}
	return ""
}

// pgTextArray renders a []string as a Postgres array literal for
// `= ANY($1::uuid[])`. The ids are server-generated UUIDs (uuid.New), so no
// quoting hazard exists; non-UUID input is dropped rather than interpolated.
func pgTextArray(ids []string) string {
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		if isValidUUID(id) {
			clean = append(clean, id)
		}
	}
	return "{" + strings.Join(clean, ",") + "}"
}
