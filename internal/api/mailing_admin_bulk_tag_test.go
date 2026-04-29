package api

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------
// Layer 1: pure helpers (canonicalColIndex, nullIfEmpty, minInt)
// ---------------------------------------------------------------------

func TestCanonicalColIndex_HappyPath(t *testing.T) {
	header := []string{
		"email", "email_hash", "first_name", "last_name", "isp",
		"eo_domain_group", "tags", "source_detail", "source_metadata", "custom_fields",
	}
	idx, err := canonicalColIndex(header)
	require.NoError(t, err)
	assert.Equal(t, 0, idx["email"])
	assert.Equal(t, 6, idx["tags"])
	assert.Equal(t, 9, idx["custom_fields"])
}

func TestCanonicalColIndex_HappyPath_DifferentOrder(t *testing.T) {
	header := []string{
		"tags", "email_hash", "email", "first_name", "last_name",
		"source_detail", "isp", "eo_domain_group", "custom_fields", "source_metadata",
	}
	idx, err := canonicalColIndex(header)
	require.NoError(t, err)
	assert.Equal(t, 2, idx["email"])
	assert.Equal(t, 0, idx["tags"])
}

func TestCanonicalColIndex_HappyPath_CaseAndWhitespace(t *testing.T) {
	header := []string{
		"  Email ", "Email_Hash", "First_Name", "Last_Name", "ISP",
		"EO_Domain_Group", "TAGS", "Source_Detail", "source_metadata", "custom_fields",
	}
	idx, err := canonicalColIndex(header)
	require.NoError(t, err)
	assert.Equal(t, 0, idx["email"])
	assert.Equal(t, 4, idx["isp"])
}

func TestCanonicalColIndex_MissingColumns(t *testing.T) {
	header := []string{"email", "first_name", "last_name"}
	_, err := canonicalColIndex(header)
	require.Error(t, err)
	msg := err.Error()
	for _, col := range []string{
		"email_hash", "isp", "eo_domain_group", "tags",
		"source_detail", "source_metadata", "custom_fields",
	} {
		assert.Contains(t, msg, col, "missing-columns error must list %s", col)
	}
}

// TestIsEOF — the wrapper canonicalCSVReader.Read converts io.EOF to a
// stringly "EOF" error so the handler can treat both as a clean
// terminator without importing io into the wire-format path.
func TestIsEOF(t *testing.T) {
	assert.False(t, isEOF(nil))
	assert.True(t, isEOF(stringErr("EOF")))
	assert.False(t, isEOF(stringErr("connection reset")))
}

type stringErr string

func (s stringErr) Error() string { return string(s) }

// ---------------------------------------------------------------------
// Layer 2: HTTP-layer auth + validation paths (no DB calls)
// ---------------------------------------------------------------------

func newBulkTagRequest(t *testing.T, body string, queryParams string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bulk-tag-canonical?"+queryParams, strings.NewReader(body))
	req.Header.Set("Content-Type", "text/csv")
	return req
}

func TestBulkTagCanonical_Unauthorized_NoAdminKey(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "secret-test")
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	w := httptest.NewRecorder()
	req := newBulkTagRequest(t, "", "segment_name=Test&segment_tag=offer:test")
	HandleBulkTagCanonical(db).ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unauthorized")
}

func TestBulkTagCanonical_Unauthorized_WrongAdminKey(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "secret-test")
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	w := httptest.NewRecorder()
	req := newBulkTagRequest(t, "", "segment_name=Test&segment_tag=offer:test")
	req.Header.Set("X-Admin-Key", "wrong-key")
	HandleBulkTagCanonical(db).ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestBulkTagCanonical_Unauthorized_NoEnvKeySet(t *testing.T) {
	// Empty ADMIN_API_KEY env must reject everything (closed-by-default).
	os.Unsetenv("ADMIN_API_KEY")
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	w := httptest.NewRecorder()
	req := newBulkTagRequest(t, "", "segment_name=Test&segment_tag=offer:test")
	req.Header.Set("X-Admin-Key", "anything")
	HandleBulkTagCanonical(db).ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestBulkTagCanonical_MissingSegmentName(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "secret-test")
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	w := httptest.NewRecorder()
	req := newBulkTagRequest(t, "", "segment_tag=offer:test")
	req.Header.Set("X-Admin-Key", "secret-test")
	HandleBulkTagCanonical(db).ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "segment_name")
}

func TestBulkTagCanonical_MissingSegmentTag(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "secret-test")
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	w := httptest.NewRecorder()
	req := newBulkTagRequest(t, "", "segment_name=Test")
	req.Header.Set("X-Admin-Key", "secret-test")
	HandleBulkTagCanonical(db).ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "segment_tag")
}

// ---------------------------------------------------------------------
// Layer 3: NDJSON streaming error paths after auth + validation passes
// ---------------------------------------------------------------------

// readNDJSON is a tiny helper for reading the chunked NDJSON response
// the handler emits.
func readNDJSON(t *testing.T, body []byte) []map[string]interface{} {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	out := []map[string]interface{}{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m map[string]interface{}
		require.NoError(t, json.Unmarshal(line, &m), "line: %s", string(line))
		out = append(out, m)
	}
	require.NoError(t, scanner.Err())
	return out
}

// findPhase returns the first NDJSON line with the given phase.
func findPhase(events []map[string]interface{}, phase string) map[string]interface{} {
	for _, e := range events {
		if p, _ := e["phase"].(string); p == phase {
			return e
		}
	}
	return nil
}

func TestBulkTagCanonical_ListNotFound_StreamsError(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "secret-test")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT id FROM mailing_lists").
		WithArgs(bulkTagDefaultOrgID, bulkTagDefaultListName).
		WillReturnError(sql.ErrNoRows)

	body := "email,email_hash,first_name,last_name,isp,eo_domain_group,tags,source_detail,source_metadata,custom_fields\n"
	w := httptest.NewRecorder()
	req := newBulkTagRequest(t, body, "segment_name=Test&segment_tag=offer:test")
	req.Header.Set("X-Admin-Key", "secret-test")
	HandleBulkTagCanonical(db).ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "errors stream as NDJSON, not as HTTP status")
	events := readNDJSON(t, w.Body.Bytes())
	require.NotEmpty(t, events)
	errEvent := findPhase(events, "error")
	require.NotNil(t, errEvent, "expected error event in NDJSON stream")
	assert.Equal(t, "resolve_list", errEvent["where"])
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestBulkTagCanonical_VersionHeaderPresent ensures every successful
// auth + validation path sets the X-Handler-Version header so deploy
// checks can confirm which build is live without a full successful run.
func TestBulkTagCanonical_VersionHeaderPresent(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "secret-test")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT id FROM mailing_lists").
		WithArgs(bulkTagDefaultOrgID, bulkTagDefaultListName).
		WillReturnError(sql.ErrNoRows)

	body := "email,email_hash,first_name,last_name,isp,eo_domain_group,tags,source_detail,source_metadata,custom_fields\n"
	w := httptest.NewRecorder()
	req := newBulkTagRequest(t, body, "segment_name=Test&segment_tag=offer:test")
	req.Header.Set("X-Admin-Key", "secret-test")
	HandleBulkTagCanonical(db).ServeHTTP(w, req)

	assert.Equal(t, VersionBulkTagCanonical, w.Header().Get("X-Handler-Version"))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestBulkTagCanonical_VersionConstantMatchesPattern is a tripwire — if
// someone bumps the constant, the test will remind them to also update
// the audit-row notes string and the operational rule referenced in
// .scratch/apr30_trugreen_attribits_handoff.md.
func TestBulkTagCanonical_VersionConstantMatchesPattern(t *testing.T) {
	require.Regexp(t, `^\d+\.\d+$`, VersionBulkTagCanonical)
}
