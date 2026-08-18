package api

// Tests for the raw-sample capture in HandlePostRecords.
//
// The canonical NDJSON persisted to S3 is a re-marshal through the CLOSED
// ingestRecord struct, so a field the struct doesn't carry is destroyed at
// the door (the 2026-08-09 city/postal_code loss). captureRawSample keeps the
// FIRST record's ORIGINAL bytes in partner_ingest_raw_samples for contract
// review. These tests pin the contract of the capture itself:
//   - inserts on a fresh dataset (JSON envelope first element, NDJSON first line)
//   - dataset-level 10-minute throttle lives in the SQL (WHERE NOT EXISTS)
//   - a capture failure NEVER fails the partner-facing POST (still 202)
//   - the 16 KiB sample cap is enforced

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

// rawSampleInsertRe pins the capture SQL shape, including the per-dataset
// 10-minute NOT EXISTS throttle — if the guard clause is ever dropped, every
// capture test fails to match.
const rawSampleInsertRe = `(?s)INSERT INTO partner_ingest_raw_samples\s*\(batch_id, dataset_id, sample, content_type\).*SELECT \$1, \$2, \$3, \$4.*WHERE NOT EXISTS.*dataset_id = \$2.*captured_at > NOW\(\) - INTERVAL '10 minutes'`

// newRawSampleFakeS3 returns a PartnerIngestS3Client wired to a local
// httptest server that 200s every PutObject, so the FULL HandlePostRecords
// happy path (raw-sample capture → S3 upload → batch index → 202) can run
// in-process. Anonymous credentials skip sigv4 signing.
func newRawSampleFakeS3(t *testing.T) *PartnerIngestS3Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(ts.URL),
		Region:       "us-west-2",
		UsePathStyle: true,
		Credentials:  aws.AnonymousCredentials{},
	})
	return &PartnerIngestS3Client{client: client, bucket: "test-bucket", region: "us-west-2"}
}

// newRawSampleAuthedRequest builds a POST with the partner auth context the
// middleware would attach (same idiom as TestHandlePostRecords_S3Unavailable).
func newRawSampleAuthedRequest(body, contentType string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/partner-ingest/v1/records", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxKeyPartnerID, "p-id")
	ctx = context.WithValue(ctx, ctxKeyPartnerSlug, "attribits")
	ctx = context.WithValue(ctx, ctxKeyPartnerName, "Attribits")
	ctx = context.WithValue(ctx, ctxKeyDatasetID, "d-id")
	ctx = context.WithValue(ctx, ctxKeyDatasetSlug, "attribits-heloc")
	ctx = context.WithValue(ctx, ctxKeyDatasetName, "Attribits-HELOC")
	ctx = context.WithValue(ctx, ctxKeyVertical, "refi_heloc")
	ctx = context.WithValue(ctx, ctxKeyAPIKeyID, "k-id")
	ctx = context.WithValue(ctx, ctxKeyKeyPrefix, "dpk_xxxx")
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", contentType)
	return req
}

// A fresh dataset + JSON envelope: the capture inserts the FIRST element's
// raw bytes — unknown fields ("extra_field") intact, exactly as posted.
func TestRawSampleCapture_JSONFirstElement(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	body := `{"records":[{"email":"a@b.com","extra_field":"survives","city":"Boise"},{"email":"b@b.com"}]}`
	wantSample := `{"email":"a@b.com","extra_field":"survives","city":"Boise"}`

	mock.ExpectExec(rawSampleInsertRe).
		WithArgs(sqlmock.AnyArg(), "d-id", wantSample, "application/json").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_inbound_batches`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPartnerIngestHandler(db, newRawSampleFakeS3(t))
	rec := httptest.NewRecorder()
	h.HandlePostRecords(rec, newRawSampleAuthedRequest(body, "application/json"))

	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"accepted":true`)
	require.NoError(t, mock.ExpectationsWereMet())
}

// NDJSON: the capture stores the FIRST non-empty line verbatim.
func TestRawSampleCapture_NDJSONFirstLine(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	firstLine := `{"email":"a@b.com","root_extra":"kept"}`
	body := firstLine + "\n" + `{"email":"b@b.com"}`

	mock.ExpectExec(rawSampleInsertRe).
		WithArgs(sqlmock.AnyArg(), "d-id", firstLine, "application/x-ndjson").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_inbound_batches`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPartnerIngestHandler(db, newRawSampleFakeS3(t))
	rec := httptest.NewRecorder()
	h.HandlePostRecords(rec, newRawSampleAuthedRequest(body, "application/x-ndjson"))

	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

// A capture INSERT failure must be INVISIBLE to the partner: the POST still
// completes 202 with a batch_id. This is the load-bearing guarantee — the
// capture is diagnostics, never a gate on ingest.
func TestRawSampleCapture_FailureDoesNotFailPOST(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectExec(rawSampleInsertRe).
		WillReturnError(errors.New("relation partner_ingest_raw_samples does not exist"))
	mock.ExpectExec(`INSERT INTO partner_inbound_batches`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPartnerIngestHandler(db, newRawSampleFakeS3(t))
	rec := httptest.NewRecorder()
	h.HandlePostRecords(rec, newRawSampleAuthedRequest(`{"records":[{"email":"a@b.com"}]}`, "application/json"))

	require.Equal(t, http.StatusAccepted, rec.Code, "capture failure must not fail the POST; body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"batch_id"`)
	require.Contains(t, rec.Body.String(), `"accepted":true`)
	require.NoError(t, mock.ExpectationsWereMet())
}

// When a recent sample already exists for the dataset, the server-side
// WHERE NOT EXISTS suppresses the insert (RowsAffected 0). The request is
// unaffected — still 202. (The throttle clause itself is pinned by
// rawSampleInsertRe on every capture expectation.)
func TestRawSampleCapture_ThrottledWhenRecentSampleExists(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectExec(rawSampleInsertRe).
		WillReturnResult(sqlmock.NewResult(0, 0)) // NOT EXISTS guard tripped: no row written
	mock.ExpectExec(`INSERT INTO partner_inbound_batches`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPartnerIngestHandler(db, newRawSampleFakeS3(t))
	rec := httptest.NewRecorder()
	h.HandlePostRecords(rec, newRawSampleAuthedRequest(`{"records":[{"email":"a@b.com"}]}`, "application/json"))

	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

// The stored sample is capped at rawSampleMaxBytes (16 KiB).
func TestCaptureRawSample_16KBCap(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	blob := strings.Repeat("x", 20*1024)
	record := `{"email":"a@b.com","blob":"` + blob + `"}`
	body := `{"records":[` + record + `]}`
	require.Greater(t, len(record), rawSampleMaxBytes)
	wantSample := record[:rawSampleMaxBytes]

	mock.ExpectExec(rawSampleInsertRe).
		WithArgs("batch-1", "d-id", wantSample, "application/json").
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPartnerIngestHandler(db, nil) // capture needs no S3
	h.captureRawSample(context.Background(), "batch-1", "d-id", []byte(body), "application/json")

	require.NoError(t, mock.ExpectationsWereMet())
}

// extractFirstRawRecord shape coverage beyond the handler tests: JSON array
// bodies, gzip-wrapped envelopes, and no-record inputs.
func TestExtractFirstRawRecord_Shapes(t *testing.T) {
	t.Run("json array first element", func(t *testing.T) {
		got := extractFirstRawRecord([]byte(`[{"email":"a@b.com","x":1},{"email":"b@b.com"}]`), "application/x-ndjson")
		require.Equal(t, `{"email":"a@b.com","x":1}`, string(got))
	})
	t.Run("gzip envelope", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, err := gz.Write([]byte(`{"records":[{"email":"a@b.com","zipped":"yes"}]}`))
		require.NoError(t, err)
		require.NoError(t, gz.Close())
		got := extractFirstRawRecord(buf.Bytes(), "application/json")
		require.Equal(t, `{"email":"a@b.com","zipped":"yes"}`, string(got))
	})
	t.Run("bare single object is its own first record", func(t *testing.T) {
		got := extractFirstRawRecord([]byte(`{"email":"a@b.com","solo":true}`), "application/json")
		require.Equal(t, `{"email":"a@b.com","solo":true}`, string(got))
	})
	t.Run("empty body yields nil", func(t *testing.T) {
		require.Nil(t, extractFirstRawRecord(nil, "application/json"))
		require.Nil(t, extractFirstRawRecord([]byte("   \n  "), "application/x-ndjson"))
	})
}
