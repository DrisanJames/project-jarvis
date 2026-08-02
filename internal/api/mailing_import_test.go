package api

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const importTestOrg = "00000000-0000-0000-0000-000000000001"
const importTestList = "00000000-0000-0000-0000-0000000000aa"

func newImportTestRouter(t *testing.T) (*chi.Mux, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewAdvancedMailingService(db)
	r := chi.NewRouter()
	r.Post("/lists/{listId}/import", svc.HandleImportSubscribers)
	return r, mock, func() { db.Close() }
}

// importMultipartRequest builds a POST to the import route carrying one file
// part of the given bytes, plus any extra form fields.
func importMultipartRequest(t *testing.T, body []byte, extra map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "subscribers.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	for k, v := range extra {
		_ = mw.WriteField(k, v)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/lists/"+importTestList+"/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Organization-ID", importTestOrg)
	return req
}

// ── REQ-068: field mapping ──────────────────────────────────────────────────

// TestImportFieldMapping pins the mapping CONTRACT, not its implementation:
// an operator-supplied mapping always wins; an absent one is derived from the
// header row by name/alias; and there is NO positional fallback, so a column
// the file does not carry stays unmapped rather than being read off column 1/2.
//
// The deleted default was `{"email":0,"first_name":1,"last_name":2}`, which was
// non-empty and therefore shadowed the auto-mapper for every upload — the
// reason a `email,zip,state` CSV wrote '90210' into first_name and 'CA' into
// last_name, values the send path then personalizes mail with.
func TestImportFieldMapping(t *testing.T) {
	cases := []struct {
		name     string
		provided map[string]int
		headers  []string
		want     map[string]int
		wantGone []string // fields that must NOT be mapped at all
	}{
		{
			// (a) no mapping + aliased headers → auto-mapped by name/alias.
			name:     "no mapping, aliased headers auto-map",
			provided: nil,
			headers:  []string{"first_name", "email", "zip"},
			want:     map[string]int{"first_name": 0, "email": 1, "postal_code": 2},
		},
		{
			// (a') the alias table is doing real work: mixed case, spaces and
			// hyphens all fold before lookup.
			name:     "no mapping, messy header spellings auto-map",
			provided: nil,
			headers:  []string{"E-Mail", "First Name", "Surname", "ZIP Code"},
			want: map[string]int{
				"email": 0, "first_name": 1, "last_name": 2, "postal_code": 3,
			},
		},
		{
			// (b) explicit mapping wins — the header row is ignored entirely,
			// including its aliases.
			name:     "explicit mapping wins over headers",
			provided: map[string]int{"email": 2, "first_name": 0},
			headers:  []string{"first_name", "email", "zip"},
			want:     map[string]int{"email": 2, "first_name": 0},
			wantGone: []string{"postal_code"},
		},
		{
			// (c) THE REGRESSION GUARD. Under the old positional default this
			// file mapped first_name→col 1 ('90210') and last_name→col 2 ('CA').
			// Neither field may be mapped at all now.
			name:     "no name columns in the file leaves names unmapped",
			provided: nil,
			headers:  []string{"email", "zip", "state"},
			want:     map[string]int{"email": 0, "postal_code": 1, "state": 2},
			wantGone: []string{"first_name", "last_name"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveImportMapping(tc.provided, tc.headers)
			for field, idx := range tc.want {
				if got[field] != idx {
					t.Errorf("mapping[%q] = %d (present=%v), want %d", field, got[field], mapHas(got, field), idx)
				}
			}
			for _, field := range tc.wantGone {
				if _, ok := got[field]; ok {
					t.Errorf("mapping[%q] = %d, want ABSENT (no positional fallback)", field, got[field])
				}
			}
		})
	}

	// End-to-end proof of case (c) against the real row loop: the INSERT must
	// carry EMPTY first_name/last_name, never the ZIP and state values.
	t.Run("row loop writes empty names, not zip/state", func(t *testing.T) {
		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		svc := NewAdvancedMailingService(db)

		mock.ExpectExec(`UPDATE mailing_import_jobs SET total_rows`).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM mailing_suppressions`).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectQuery(`SELECT id FROM mailing_subscribers WHERE list_id`).
			WillReturnRows(sqlmock.NewRows([]string{"id"})) // no existing row
		mock.ExpectExec(`INSERT INTO mailing_subscribers`).
			WithArgs(
				sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
				"bob@gmail.com", sqlmock.AnyArg(),
				"", "", // first_name, last_name — the whole point
				sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(`UPDATE mailing_lists SET subscriber_count`).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`UPDATE mailing_import_jobs SET[\s\S]*status = 'completed'`).
			WillReturnResult(sqlmock.NewResult(0, 1))

		csv := "email,zip,state\nbob@gmail.com,90210,CA\n"
		svc.processCSVImportEnhanced(uuid.New(), uuid.New(), uuid.New(),
			strings.NewReader(csv), nil, true)

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

func mapHas(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

// TestImportStandardFieldsMatchesTemplateVocabulary guards the ruling-R2
// consolidation. importStandardFields() now derives the importer's field
// vocabulary from GetStandardFields() instead of re-declaring it. The set must
// stay key-for-key identical to the literal it replaced: any key that silently
// leaves the set starts landing in custom_fields instead of its own column,
// and any key that joins it silently stops landing in custom_fields.
func TestImportStandardFieldsMatchesTemplateVocabulary(t *testing.T) {
	// The exact literal deleted from processCSVImportEnhanced.
	want := map[string]bool{
		"email": true, "first_name": true, "last_name": true, "phone": true,
		"city": true, "state": true, "country": true, "postal_code": true,
		"timezone": true, "company": true, "job_title": true, "industry": true,
		"language": true, "source": true, "tags": true, "birthdate": true,
		"subscribed_at": true,
	}
	got := importStandardFields()
	for k := range want {
		if !got[k] {
			t.Errorf("%q dropped from the importer's standard fields — it will now land in custom_fields", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("%q added to the importer's standard fields — it will no longer land in custom_fields", k)
		}
	}
}

// TestImportNoEmailColumnFails: a CSV whose header row resolves no email column
// must terminalize the job status='failed'. Before REQ-068 it fell through —
// every row missed the mapping["email"] lookup, and the job still finished
// status='completed' with 0 imported, i.e. a total no-op that read as success.
func TestImportNoEmailColumnFails(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := NewAdvancedMailingService(db)

	jobID := uuid.New()
	mock.ExpectExec(`UPDATE mailing_import_jobs SET[\s\S]*status = 'failed'`).
		WithArgs(jobID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	csv := "full_name,city,zip\nBob Smith,Denver,80202\n"
	svc.processCSVImportEnhanced(jobID, uuid.New(), uuid.New(),
		strings.NewReader(csv), nil, true)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("job did not terminalize as 'failed': %v", err)
	}
}

// TestImportNoEmailColumnFailsNeverCompletes is the other half of the same
// contract: the terminal 'completed' UPDATE must NOT run for such a file.
// The expectation is deliberately asserted UNMET.
func TestImportNoEmailColumnFailsNeverCompletes(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)
	svc := NewAdvancedMailingService(db)

	mock.ExpectExec(`UPDATE mailing_import_jobs SET[\s\S]*status = 'completed'`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	csv := "full_name,city,zip\nBob Smith,Denver,80202\n"
	svc.processCSVImportEnhanced(uuid.New(), uuid.New(), uuid.New(),
		strings.NewReader(csv), nil, true)

	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("job was marked 'completed' despite having no email column")
	}
}

// ── REQ-069: oversize rejection ─────────────────────────────────────────────

// TestImportRejectsOversizeFile: a file over MaxImportFileBytes is REJECTED
// with a 4xx naming the limit, BEFORE any mailing_import_jobs row is created.
// io.LimitReader used to drop the tail and return EOF (not an error), so a
// 400k-row CSV imported partially and the job still said 'completed'.
func TestImportRejectsOversizeFile(t *testing.T) {
	t.Run("over the limit is rejected and creates no job", func(t *testing.T) {
		r, mock, closeFn := newImportTestRouter(t)
		defer closeFn()

		// Expected-but-must-NOT-happen: ExpectationsWereMet returning an error
		// is the assertion that the INSERT never ran.
		mock.ExpectExec(`INSERT INTO mailing_import_jobs`).
			WillReturnResult(sqlmock.NewResult(0, 1))

		oversize := make([]byte, MaxImportFileBytes+1)
		for i := range oversize {
			oversize[i] = 'a'
		}
		req := importMultipartRequest(t, oversize, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code < 400 || w.Code > 499 {
			t.Fatalf("status = %d, want a 4xx; body = %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, fmt.Sprintf("%d MB", MaxImportFileMB)) {
			t.Fatalf("error body %q does not name the %d MB limit", body, MaxImportFileMB)
		}
		if strings.Contains(body, "job_id") {
			t.Fatalf("a rejected upload returned a job id: %s", body)
		}
		if err := mock.ExpectationsWereMet(); err == nil {
			t.Fatal("mailing_import_jobs INSERT ran for an oversize file — no job row may exist")
		}
	})

	t.Run("at or under the limit is unaffected", func(t *testing.T) {
		r, mock, closeFn := newImportTestRouter(t)
		defer closeFn()
		mock.MatchExpectationsInOrder(false)

		mock.ExpectExec(`INSERT INTO mailing_import_jobs`).
			WillReturnResult(sqlmock.NewResult(0, 1))

		req := importMultipartRequest(t, []byte("email\nbob@gmail.com\n"), nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "job_id") {
			t.Fatalf("accepted upload returned no job id: %s", w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("under-limit upload did not create a job row: %v", err)
		}
	})
}

// TestImportRejectsOversizeFileCeilingIsPinned: the ceiling is UI-visible copy
// (REQ-069 DoD 3 puts "32 MB" next to the file picker), so a change to the
// constant must be a deliberate, paired edit — not a silent drift that leaves
// the portal advertising a number the server no longer enforces.
func TestImportRejectsOversizeFileCeilingIsPinned(t *testing.T) {
	if MaxImportFileBytes != 32*1024*1024 || MaxImportFileMB != 32 {
		t.Fatalf("import ceiling changed: %d bytes / %d MB — update the UI copy and this test together",
			MaxImportFileBytes, MaxImportFileMB)
	}
}

// TestImportHandlerNeverPersistsPositionalMapping — the REGRESSION GUARD for
// REQ-068 at the HTTP boundary.
//
// Why this exists as a separate test: TestImportFieldMapping unit-tests
// resolveImportMapping() directly, so it stays green even if the HANDLER is
// changed back to defaulting field_mapping to the positional literal. The lead
// proved that in review by re-introducing the old fallback — every other test
// still passed. This test posts a real multipart upload with NO field_mapping
// and asserts on the value actually persisted to mailing_import_jobs, which is
// the only place the handler's choice is observable.
//
// DoD REQ-068 item 3 says "with no field_mapping POSTED" — this is the posted
// path; the unit test is the resolver path. Both are required.
//
// NOTE for future editors: the matcher below only CAPTURES. Never call
// t.Fatalf from inside a sqlmock Argument matcher — Fatalf runs
// runtime.Goexit() and, off the test goroutine, hangs the run instead of
// failing it. Assert in the test body.
func TestImportHandlerNeverPersistsPositionalMapping(t *testing.T) {
	r, mock, closeFn := newImportTestRouter(t)
	defer closeFn()
	mock.MatchExpectationsInOrder(false)

	// arg 5 of the INSERT is field_mapping (id, org, list, filename, mapping).
	capture := &capturedArg{}
	mock.ExpectExec(`INSERT INTO mailing_import_jobs`).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			capture,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Deliberately NOT email,first,last order — the exact shape the old
	// positional default corrupted (zip -> first_name, state -> last_name).
	csv := []byte("first_name,email,zip\nBob,bob@gmail.com,90210\n")
	req := importMultipartRequest(t, csv, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected job INSERT did not run as specified: %v", err)
	}

	got, ok := capture.value.(string)
	if !ok {
		t.Fatalf("field_mapping persisted as %T (%v), want string", capture.value, capture.value)
	}
	if strings.Contains(got, `"email":0`) && strings.Contains(got, `"first_name":1`) {
		t.Fatalf("handler persisted the POSITIONAL default %q — REQ-068 regressed; "+
			"an absent field_mapping must fall through to the header auto-mapper", got)
	}
	// Must still be valid JSON: field_mapping is JSONB, '' would fail the insert.
	var probe map[string]int
	if err := json.Unmarshal([]byte(got), &probe); err != nil {
		t.Fatalf("field_mapping %q is not valid JSON — JSONB insert would fail: %v", got, err)
	}
}

// capturedArg records the driver value it was matched against and always
// matches, so assertions happen on the test goroutine.
type capturedArg struct{ value driver.Value }

func (c *capturedArg) Match(v driver.Value) bool { c.value = v; return true }
