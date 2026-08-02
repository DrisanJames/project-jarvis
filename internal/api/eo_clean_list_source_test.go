package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// REQ-070 / ruling R1 — the `list` source resolves membership from
// mailing_subscribers.list_id, the platform's ONE membership vehicle.
//
// It used to read the legacy list-to-subscriber join table, which has no DDL
// anywhere in the repo and which nothing writes (the importer records
// membership as mailing_subscribers.list_id, mailing_import.go:493-500). So an
// imported list resolved to 0 emails here — or errored outright if the table is
// absent in prod — while the picker on the same screen quoted a nonzero
// mailing_lists.subscriber_count. Two sources for one quantity on one screen.
//
// The regexps below are the load-bearing part of these tests: they require
// `FROM mailing_subscribers s ... WHERE s.list_id`, which the old two-table
// join cannot satisfy.
const (
	eoCleanListCountRe  = `SELECT COUNT\(DISTINCT lower\(s\.email\)\)\s+FROM mailing_subscribers s\s+WHERE s\.list_id = \$1::uuid`
	eoCleanListInsertRe = `INSERT INTO mailing_eo_clean_items[\s\S]*FROM mailing_subscribers s\s+WHERE s\.list_id = \$2::uuid[\s\S]*mailing_eo_validation[\s\S]*'Verified'`
)

const eoCleanTestListID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func eoCleanListRequest() *http.Request {
	return eoCleanRequest("POST", "/eo-clean/jobs", map[string]interface{}{
		"source_type": "list",
		"source_ref":  eoCleanTestListID,
		"label":       "imported list",
	})
}

// TestEOCleanListSource pins the whole list-source contract:
//   - count and item-insert read the SAME table and the SAME predicate, so the
//     job's total_count is exactly the number the picker quotes for that list
//     (mailing_lists.subscriber_count is COUNT(*) over list_id with no status
//     filter — mailing_import.go:533 — hence no status predicate here either);
//   - the already-Verified skip still applies (NEVER PAY TWICE);
//   - the eoCleanMaxJobSize bound still applies.
func TestEOCleanListSource(t *testing.T) {
	// ── happy path: quote and job agree, already-Verified are skipped ────────
	t.Run("count and insert share one predicate; already-Verified skipped", func(t *testing.T) {
		r, mock, closeFn := newEOCleanTestRouter(t)
		defer closeFn()

		jobID := "22222222-3333-4444-5555-666666666666"
		// The number mailing_lists.subscriber_count would quote for this list.
		const pickerCount = int64(120000)
		const queued = int64(118000)

		mock.ExpectBegin()
		mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`SELECT name FROM mailing_lists WHERE id = \$1::uuid AND organization_id = \$2`).
			WithArgs(eoCleanTestListID, sqlmock.AnyArg()).
			WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Imported List"))
		mock.ExpectQuery(`INSERT INTO mailing_eo_clean_jobs`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(jobID))
		mock.ExpectQuery(eoCleanListCountRe).
			WithArgs(eoCleanTestListID).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(pickerCount))
		mock.ExpectExec(eoCleanListInsertRe).
			WithArgs(jobID, eoCleanTestListID).
			WillReturnResult(sqlmock.NewResult(0, queued))
		mock.ExpectQuery(`UPDATE mailing_eo_clean_jobs SET[\s\S]*total_count = \$2`).
			WithArgs(jobID, pickerCount, pickerCount-queued, queued, "queued").
			WillReturnRows(sqlmock.NewRows(eoCleanTestJobCols).AddRow(
				jobID, "list", eoCleanTestListID, "imported list", "queued",
				pickerCount, pickerCount-queued, queued, 0, 0, 0, 0, 0, 0, 0, "", eoCleanTestNow, nil))
		mock.ExpectCommit()

		w := httptest.NewRecorder()
		r.ServeHTTP(w, eoCleanListRequest())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var resp struct {
			Job eoCleanJobJSON `json:"job"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		// The screen's quote and the screen's job must be the same number.
		if resp.Job.TotalCount != pickerCount {
			t.Errorf("total_count = %d, want %d (the number the picker quotes for this list)",
				resp.Job.TotalCount, pickerCount)
		}
		if resp.Job.QueuedCount != queued || resp.Job.AlreadyCleanCount != pickerCount-queued {
			t.Errorf("queued/already_clean = %d/%d, want %d/%d",
				resp.Job.QueuedCount, resp.Job.AlreadyCleanCount, queued, pickerCount-queued)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations (the list source is not reading mailing_subscribers.list_id): %v", err)
		}
	})

	// ── the per-job bound still applies on the new path ─────────────────────
	t.Run("over eoCleanMaxJobSize is rejected 400 with no items inserted", func(t *testing.T) {
		r, mock, closeFn := newEOCleanTestRouter(t)
		defer closeFn()

		jobID := "22222222-3333-4444-5555-777777777777"
		mock.ExpectBegin()
		mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`SELECT name FROM mailing_lists`).
			WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Huge List"))
		mock.ExpectQuery(`INSERT INTO mailing_eo_clean_jobs`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(jobID))
		mock.ExpectQuery(eoCleanListCountRe).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(eoCleanMaxJobSize + 1)))
		// No item INSERT is expected — the bound short-circuits before it and
		// the deferred Rollback discards the job row.
		mock.ExpectRollback()

		w := httptest.NewRecorder()
		r.ServeHTTP(w, eoCleanListRequest())
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	// ── NEVER PAY TWICE: an all-Verified list costs nothing ─────────────────
	t.Run("all-Verified list yields queued 0 and status done", func(t *testing.T) {
		r, mock, closeFn := newEOCleanTestRouter(t)
		defer closeFn()

		jobID := "22222222-3333-4444-5555-888888888888"
		const pickerCount = int64(500)

		mock.ExpectBegin()
		mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`SELECT name FROM mailing_lists`).
			WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Clean List"))
		mock.ExpectQuery(`INSERT INTO mailing_eo_clean_jobs`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(jobID))
		mock.ExpectQuery(eoCleanListCountRe).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(pickerCount))
		mock.ExpectExec(eoCleanListInsertRe).
			WillReturnResult(sqlmock.NewResult(0, 0)) // anti-join skipped them all
		mock.ExpectQuery(`UPDATE mailing_eo_clean_jobs SET[\s\S]*finished_at = NOW\(\)`).
			WithArgs(jobID, pickerCount, pickerCount, int64(0), "done").
			WillReturnRows(sqlmock.NewRows(eoCleanTestJobCols).AddRow(
				jobID, "list", eoCleanTestListID, "imported list", "done",
				pickerCount, pickerCount, 0, 0, 0, 0, 0, 0, 0, 0, "", eoCleanTestNow, eoCleanTestNow))
		mock.ExpectCommit()

		w := httptest.NewRecorder()
		r.ServeHTTP(w, eoCleanListRequest())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var resp struct {
			Job eoCleanJobJSON `json:"job"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Job.QueuedCount != 0 || resp.Job.Status != "done" || resp.Job.FinishedAt == nil {
			t.Fatalf("job = %+v, want queued 0 / status done / finished_at set", resp.Job)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}
