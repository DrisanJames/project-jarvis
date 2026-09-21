package api

// Static upload door fixtures. What each test PINS:
//   - register is GATED on the object being in the bucket: a HeadObject miss
//     is a 409 "object not in bucket", and NOTHING is written (no dataset
//     lookup, no batch, no status flip);
//   - a registered object rides the SAME commit core the CSV door uses and
//     produces batch rows stamped supply_class='at_rest',
//     source_path='static_upload', object_sha256=<the object's sha>, with the
//     static object's real bucket/key on every row's ingest_metadata;
//   - presign refuses a key a live row already claims (409) BEFORE it touches
//     S3 — re-presigning would overwrite an object a batch already points at;
//   - every read is org-scoped: another org's object is a 404, and the org in
//     the WHERE clause is the REQUESTING org.

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

const (
	staticTestOrgA    = "11111111-1111-1111-1111-111111111111"
	staticTestOrgB    = "22222222-2222-2222-2222-222222222222"
	staticTestObjectA = "33333333-3333-3333-3333-333333333333"
	staticTestKey     = "static/attribits/2026-09-20/drop.csv"
)

// ── the fake S3 surface ─────────────────────────────────────────────────────

// fakeStaticS3 implements dataIngestS3API. HeadObject/GetObject are the two
// the door's behaviour hangs on; the multipart pair is present for the
// interface and counted so a test can prove S3 was NOT touched.
type fakeStaticS3 struct {
	headErr   error
	headBytes int64
	body      string
	getCalls  int
	headCalls int
	cmuCalls  int
}

func (f *fakeStaticS3) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.headCalls++
	if f.headErr != nil {
		return nil, f.headErr
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(f.headBytes), ETag: aws.String(`"etag-1"`)}, nil
}

func (f *fakeStaticS3) CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.cmuCalls++
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-1")}, nil
}

func (f *fakeStaticS3) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	return &s3.CompleteMultipartUploadOutput{ETag: aws.String(`"etag-1"`)}, nil
}

func (f *fakeStaticS3) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.getCalls++
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

// staticObjectRows builds the SELECT result for one object row.
func staticObjectRows(orgID, status, sha string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "organization_id", "s3_bucket", "s3_key", "sha256", "bytes",
		"content_type", "source", "dataset_id", "declared", "uploaded_by",
		"uploaded_at", "registered_at", "batch_id", "status",
	}).AddRow(staticTestObjectA, orgID, "test-bucket", staticTestKey, sha, int64(1234),
		"text/csv", "attribits", "", `{"filename":"drop.csv","upload_id":"upload-1"}`,
		"operator@jv", time.Now().UTC(), nil, "", status)
}

func staticJSONRequest(t *testing.T, method, path, org string, body interface{}) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", org)
	req.Header.Set("X-User-Email", "operator@jv")
	return req
}

// staticRouter mounts the door exactly as the overseer will:
//
//	r.Route("/data-ingest/static", staticSvc.RegisterRoutes)
func staticRouter(svc *DataIngestStaticService) chi.Router {
	r := chi.NewRouter()
	r.Route("/data-ingest/static", svc.RegisterRoutes)
	return r
}

// prefixMatch asserts a string driver arg starts with a prefix.
type prefixMatch string

func (p prefixMatch) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && strings.HasPrefix(s, string(p))
}

// ── 1. register refuses an object that is not in the bucket ─────────────────

func TestStaticRegister_RefusesMissingObject409(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	api := &fakeStaticS3{headErr: fmt.Errorf("NotFound: 404")}
	svc := NewDataIngestStaticService(db, newRawSampleFakeS3(t))
	svc.api = api

	mock.ExpectQuery(`SELECT id, organization_id, s3_bucket`).
		WithArgs(staticTestObjectA, staticTestOrgA).
		WillReturnRows(staticObjectRows(staticTestOrgA, "object", "sha-abc"))

	rec := httptest.NewRecorder()
	staticRouter(svc).ServeHTTP(rec, staticJSONRequest(t, http.MethodPost,
		"/data-ingest/static/register", staticTestOrgA, map[string]interface{}{
			"object_id":  staticTestObjectA,
			"dataset_id": csvTestDatasetID,
		}))

	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"error":"object not in bucket"}`, rec.Body.String())
	require.Equal(t, 1, api.headCalls)
	// The gate is BEFORE any load work: no object was read, and the dataset
	// lookup / batch INSERT never ran (unfulfilled expectations would fail).
	require.Equal(t, 0, api.getCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ── 2. the happy path: object → batches, stamped at_rest ────────────────────

func TestStaticRegister_CreatesAtRestBatchFromObject(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	api := &fakeStaticS3{
		headBytes: 64,
		body:      "email,first_name\na@gmail.com,Ann\nb@yahoo.com,Bob\nc@aol.com,Cid\n",
	}
	svc := NewDataIngestStaticService(db, newRawSampleFakeS3(t))
	svc.api = api

	mock.ExpectQuery(`SELECT id, organization_id, s3_bucket`).
		WithArgs(staticTestObjectA, staticTestOrgA).
		WillReturnRows(staticObjectRows(staticTestOrgA, "object", "sha-abc"))
	expectCSVDatasetResolve(mock)
	// registered → (batches) → loaded
	mock.ExpectExec(`UPDATE data_ingest_static_objects SET status = \$2 WHERE id = \$1`).
		WithArgs(staticTestObjectA, "registered").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// The batch row: REAL bucket, a real key in it, 3 records, and the static
	// object's identity + sha on ingest_metadata.
	mock.ExpectExec(`INSERT INTO partner_inbound_batches`).
		WithArgs(
			sqlmock.AnyArg(), csvTestDatasetID, sqlmock.AnyArg(),
			"test-bucket", prefixMatch("partners/"), 3,
			sqlmock.AnyArg(),
			csvMetaMatch{want: []string{
				`"source":"static_upload"`,
				`"source_path":"static_upload"`,
				`"supply_class":"at_rest"`,
				`"object_s3_bucket":"test-bucket"`,
				`"object_s3_key":"` + staticTestKey + `"`,
				`"object_sha256":"sha-abc"`,
				`"static_object_id":"` + staticTestObjectA + `"`,
				`"landing_status":"held"`,
			}},
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// …and the columns the dashboard classifies on.
	mock.ExpectExec(`UPDATE partner_inbound_batches\s+SET supply_class`).
		WithArgs(prefixMatch("{"), "at_rest", "static_upload", "sha-abc").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec(`UPDATE data_ingest_static_objects\s+SET status = 'loaded'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := httptest.NewRecorder()
	staticRouter(svc).ServeHTTP(rec, staticJSONRequest(t, http.MethodPost,
		"/data-ingest/static/register", staticTestOrgA, map[string]interface{}{
			"object_id":  staticTestObjectA,
			"dataset_id": csvTestDatasetID,
			"declared":   map[string]interface{}{"cleaned_upstream": false, "note": "wcl drop"},
		}))

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "at_rest", resp["supply_class"])
	require.Equal(t, "static_upload", resp["source_path"])
	require.Equal(t, "sha-abc", resp["object_sha256"])
	require.Equal(t, "test-bucket", resp["s3_bucket"])
	require.Equal(t, staticTestKey, resp["s3_key"])
	require.Equal(t, "held", resp["landing_status"]) // held is the DEFAULT
	require.Equal(t, float64(3), resp["records"])
	require.Equal(t, float64(1), resp["batches"])
	require.Equal(t, false, resp["stamp_deferred"])
	require.NoError(t, mock.ExpectationsWereMet())
}

// cleaned_upstream=true is the ONLY thing that turns an undeclared landing
// status into 'ready' — the slicer reads this off ingest_metadata.
func TestStaticRegister_CleanedUpstreamLandsReady(t *testing.T) {
	require.Equal(t, "held", mustLanding(t, "", false))
	require.Equal(t, "ready", mustLanding(t, "", true))
	require.Equal(t, "held", mustLanding(t, "held", true)) // explicit wins
	require.Equal(t, "pending_eo", mustLanding(t, "pending_eo", false))
	_, err := resolveLandingStatus("mail-it-now", false)
	require.Error(t, err)
}

func mustLanding(t *testing.T, declared string, cleaned bool) string {
	t.Helper()
	v, err := resolveLandingStatus(declared, cleaned)
	require.NoError(t, err)
	return v
}

// ── 3. presign refuses a duplicate key ──────────────────────────────────────

func TestStaticPresign_RefusesDuplicateKey409(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	api := &fakeStaticS3{}
	svc := NewDataIngestStaticService(db, newRawSampleFakeS3(t))
	svc.api = api

	mock.ExpectQuery(`SELECT id::text, status FROM data_ingest_static_objects`).
		WithArgs(staticObjectKey("attribits", "drop.csv", time.Now())).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(staticTestObjectA, "loaded"))

	rec := httptest.NewRecorder()
	staticRouter(svc).ServeHTTP(rec, staticJSONRequest(t, http.MethodPost,
		"/data-ingest/static/presign", staticTestOrgA, map[string]interface{}{
			"filename": "drop.csv",
			"source":   "attribits",
			"bytes":    1234,
		}))

	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "key already exists")
	// Refused BEFORE S3: no multipart upload was created, so nothing can
	// overwrite the object the existing row points at.
	require.Equal(t, 0, api.cmuCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

// The key layout is the contract: static/<source-slug>/<YYYY-MM-DD>/<file>,
// with path traversal stripped and the extension preserved.
func TestStaticObjectKey_Layout(t *testing.T) {
	// 2026-09-21 00:04Z is still 2026-09-20 in Denver — the key follows the
	// operating day, not UTC.
	at, err := time.Parse(time.RFC3339, "2026-09-21T00:04:05Z")
	require.NoError(t, err)
	require.Equal(t, "static/attribits/2026-09-20/drop.csv",
		staticObjectKey("Attribits", "drop.csv", at))
	require.Equal(t, "static/wcl-heloc/2026-09-20/sept_drop-2.csv",
		staticObjectKey("WCL HELOC", "../../sept_drop 2.csv", at))
}

// ── 4. org scoping ──────────────────────────────────────────────────────────

func TestStaticObjects_OrgScoped(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	svc := NewDataIngestStaticService(db, newRawSampleFakeS3(t))
	svc.api = &fakeStaticS3{}

	// Org B asks for org A's object: the WHERE carries B's org, so the row is
	// not found — a 404, never another org's data.
	mock.ExpectQuery(`SELECT id, organization_id, s3_bucket`).
		WithArgs(staticTestObjectA, staticTestOrgB).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "organization_id", "s3_bucket", "s3_key", "sha256", "bytes",
			"content_type", "source", "dataset_id", "declared", "uploaded_by",
			"uploaded_at", "registered_at", "batch_id", "status",
		}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/data-ingest/static/objects/"+staticTestObjectA, nil)
	req.Header.Set("X-Organization-ID", staticTestOrgB)
	staticRouter(svc).ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestStaticListObjects_FiltersByRequestingOrg(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	svc := NewDataIngestStaticService(db, newRawSampleFakeS3(t))
	svc.api = &fakeStaticS3{}

	mock.ExpectQuery(`SELECT id, organization_id, s3_bucket`).
		WithArgs(staticTestOrgB, "", 100).
		WillReturnRows(staticObjectRows(staticTestOrgB, "loaded", "sha-b"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/data-ingest/static/objects", nil)
	req.Header.Set("X-Organization-ID", staticTestOrgB)
	staticRouter(svc).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		OrgID   string            `json:"organization_id"`
		Objects []staticObjectRow `json:"objects"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, staticTestOrgB, resp.OrgID)
	require.Len(t, resp.Objects, 1)
	require.Equal(t, staticTestOrgB, resp.Objects[0].OrgID)
	require.NoError(t, mock.ExpectationsWereMet())
}
