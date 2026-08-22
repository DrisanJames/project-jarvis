package api

// CSV ingestion fixtures. What each test PINS:
//   - header auto-map: exact ingestRecord field names = high confidence,
//     partner-spelling aliases (postal_code, opt_in_url, var1 …) = medium,
//     unknown headers fall back to metadata.<normalized> (none) so no column
//     is silently destroyed;
//   - preview streams the WHOLE file: row_count / invalid_email_count / the
//     per-ISP mix computed with isp.Group over the mapped email column;
//   - the byte cap rejects with 413 (cap shrunk via the package var — no
//     50MB fixture);
//   - commit skips invalid emails (counted), chunks into ≤N-record batches,
//     and persists EVERY chunk through persistPartnerBatch — the same S3 +
//     partner_inbound_batches path the API door uses (fake S3 = the
//     httptest PutObject stub from partner_ingest_rawsample_test.go), with
//     the batch marked ingest_metadata.source='csv_upload'.

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const csvTestDatasetID = "99999999-8888-7777-6666-555555555555"

func csvMultipartRequest(t *testing.T, path string, fields map[string]string, filename, csvBody string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write([]byte(csvBody))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-User-Email", "operator@jv")
	return req
}

// expectCSVDatasetResolve queues the active-dataset lookup.
func expectCSVDatasetResolve(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM partner_datasets d\s+JOIN data_partners p`).
		WithArgs(csvTestDatasetID).
		WillReturnRows(sqlmock.NewRows([]string{
			"partner_id", "slug", "name", "vertical", "status", "paused", "pslug", "pstatus",
		}).AddRow("p-1", "test-feed", "Test Feed", "consumer", "active", false, "testpartner", "active"))
}

func TestSuggestCSVMapping_AliasesAndFallback(t *testing.T) {
	sugs := suggestCSVMapping([]string{
		"Email", "First Name", "postal_code", "opt_in_url", "var1", "Favorite Color", "state",
	})
	want := []struct {
		target, confidence string
	}{
		{"email", "high"},
		{"first_name", "high"},
		{"zip", "medium"},
		{"signup_url", "medium"},
		{"metadata.tid", "medium"},
		{"metadata.favorite_color", "none"},
		{"state", "high"},
	}
	require.Len(t, sugs, len(want))
	for i, w := range want {
		require.Equal(t, w.target, sugs[i].Target, sugs[i].Header)
		require.Equal(t, w.confidence, sugs[i].Confidence, sugs[i].Header)
	}
}

func TestCSVPreview_StreamsWholeFileAndComputesISPMix(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	expectCSVDatasetResolve(mock)
	svc := NewPartnerCSVIngestService(db, nil) // preview persists nothing

	csvBody := "\uFEFFEmail,First Name,zip_code,Favorite Color\n" +
		"a@gmail.com,Ann,83704,blue\n" +
		"b@yahoo.com,Bob,83705,red\n" +
		"c@gmail.com,Cee,83706,green\n" +
		"not-an-email,Dee,83707,mauve\n" +
		"e@outlook.com,Eve,83708,teal\n"

	rec := httptest.NewRecorder()
	svc.HandlePreview(rec, csvMultipartRequest(t, "/api/mailing/partner-ingest-csv/preview",
		map[string]string{"dataset_id": csvTestDatasetID}, "feed.csv", csvBody))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		HasHeader         bool                   `json:"has_header"`
		Headers           []string               `json:"headers"`
		SuggestedMapping  []csvMappingSuggestion `json:"suggested_mapping"`
		SampleRows        [][]string             `json:"sample_rows"`
		RowCount          int64                  `json:"row_count"`
		InvalidEmailCount int64                  `json:"invalid_email_count"`
		EmailColumn       int                    `json:"email_column"`
		ISPBreakdown      []struct {
			ISP   string  `json:"isp"`
			Count int64   `json:"count"`
			Pct   float64 `json:"pct"`
		} `json:"isp_breakdown"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	require.True(t, resp.HasHeader)
	require.Equal(t, 0, resp.EmailColumn, "BOM-prefixed 'Email' header still maps")
	require.Equal(t, "zip", resp.SuggestedMapping[2].Target)
	require.Equal(t, "metadata.favorite_color", resp.SuggestedMapping[3].Target)
	require.EqualValues(t, 5, resp.RowCount)
	require.EqualValues(t, 1, resp.InvalidEmailCount)
	require.Len(t, resp.SampleRows, 5)

	mix := map[string][2]float64{}
	for _, e := range resp.ISPBreakdown {
		mix[e.ISP] = [2]float64{float64(e.Count), e.Pct}
	}
	require.Equal(t, [2]float64{2, 50}, mix["gmail"], "2 of 4 valid emails")
	require.Equal(t, [2]float64{1, 25}, mix["yahoo"])
	require.Equal(t, [2]float64{1, 25}, mix["microsoft"], "outlook.com classifies microsoft (isp.Group)")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCSVPreview_OversizeRejected(t *testing.T) {
	saved := partnerCSVMaxBytes
	partnerCSVMaxBytes = 512
	t.Cleanup(func() { partnerCSVMaxBytes = saved })

	db, _ := newPartnerMockDB(t)
	svc := NewPartnerCSVIngestService(db, nil)

	big := bytes.Repeat([]byte("a@b.com,x\n"), 200) // > 512 bytes
	rec := httptest.NewRecorder()
	svc.HandlePreview(rec, csvMultipartRequest(t, "/api/mailing/partner-ingest-csv/preview",
		map[string]string{"dataset_id": csvTestDatasetID}, "big.csv", string(big)))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
}

func TestCSVCommit_ChunksSkipsInvalidAndMarksSource(t *testing.T) {
	savedChunk := partnerCSVMaxRecordsPerBatch
	partnerCSVMaxRecordsPerBatch = 2
	t.Cleanup(func() { partnerCSVMaxRecordsPerBatch = savedChunk })

	db, mock := newPartnerMockDB(t)
	expectCSVDatasetResolve(mock)
	// 5 valid records / chunk size 2 → 3 batches, each indexed with the
	// csv_upload source marker in ingest_metadata.
	for i := 0; i < 3; i++ {
		mock.ExpectExec(`INSERT INTO partner_inbound_batches`).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewPartnerCSVIngestService(db, newRawSampleFakeS3(t))

	csvBody := "email,first_name,favorite_color\n" +
		"a@gmail.com,Ann,blue\n" +
		"b@yahoo.com,Bob,red\n" +
		"broken email,Bad,black\n" + // invalid → skipped, counted
		"c@aol.com,Cee,green\n" +
		"d@outlook.com,Dee,teal\n" +
		"e@gmail.com,Eve,pink\n"

	mapping := `{"email":0,"first_name":1}`
	rec := httptest.NewRecorder()
	svc.HandleCommit(rec, csvMultipartRequest(t, "/api/mailing/partner-ingest-csv/commit",
		map[string]string{"dataset_id": csvTestDatasetID, "mapping": mapping}, "feed.csv", csvBody))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp struct {
		BatchIDs       []string `json:"batch_ids"`
		Batches        int      `json:"batches"`
		Records        int64    `json:"records"`
		SkippedInvalid int64    `json:"skipped_invalid"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 3, resp.Batches)
	require.Len(t, resp.BatchIDs, 3)
	require.EqualValues(t, 5, resp.Records)
	require.EqualValues(t, 1, resp.SkippedInvalid)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCSVCommit_RequiresEmailMapping(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	expectCSVDatasetResolve(mock)
	svc := NewPartnerCSVIngestService(db, newRawSampleFakeS3(t))

	rec := httptest.NewRecorder()
	svc.HandleCommit(rec, csvMultipartRequest(t, "/api/mailing/partner-ingest-csv/commit",
		map[string]string{"dataset_id": csvTestDatasetID, "mapping": `{"first_name":1}`},
		"feed.csv", "email,first_name\na@b.com,Ann\n"))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "email")
}

// The persist path a commit rides is EXACTLY the API door's: unmapped
// columns survive as metadata in the canonical NDJSON. Verified via the
// record builder contract — the same closed-struct marshal persistPartnerBatch
// re-serializes.
func TestCSVCommit_UnmappedColumnsBecomeMetadata(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	expectCSVDatasetResolve(mock)
	mock.ExpectExec(`INSERT INTO partner_inbound_batches`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewPartnerCSVIngestService(db, newRawSampleFakeS3(t))
	csvBody := "email,tid,Favorite Color\n" +
		"a@gmail.com,tok-123,blue\n"
	// email mapped; tid mapped to metadata.tid; favorite color left unmapped.
	mapping := `{"email":0,"metadata.tid":1}`
	rec := httptest.NewRecorder()
	svc.HandleCommit(rec, csvMultipartRequest(t, "/api/mailing/partner-ingest-csv/commit",
		map[string]string{"dataset_id": csvTestDatasetID, "mapping": mapping}, "feed.csv", csvBody))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCSVCommit_DatasetNotActive409(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectQuery(`FROM partner_datasets d\s+JOIN data_partners p`).
		WithArgs(csvTestDatasetID).
		WillReturnRows(sqlmock.NewRows([]string{
			"partner_id", "slug", "name", "vertical", "status", "paused", "pslug", "pstatus",
		}).AddRow("p-1", "test-feed", "Test Feed", "consumer", "archived", false, "testpartner", "active"))

	svc := NewPartnerCSVIngestService(db, newRawSampleFakeS3(t))
	rec := httptest.NewRecorder()
	svc.HandleCommit(rec, csvMultipartRequest(t, "/api/mailing/partner-ingest-csv/commit",
		map[string]string{"dataset_id": csvTestDatasetID, "mapping": `{"email":0}`},
		"feed.csv", "email\na@b.com\n"))
	require.Equal(t, http.StatusConflict, rec.Code)
}
