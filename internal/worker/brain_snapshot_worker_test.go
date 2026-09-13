package worker

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

type fakeBrainSnapshotStore struct {
	puts map[string][]byte
}

func (f *fakeBrainSnapshotStore) Put(_ context.Context, key string, body []byte, _ string) error {
	if f.puts == nil {
		f.puts = map[string][]byte{}
	}
	f.puts[key] = append([]byte(nil), body...)
	return nil
}

// Every table is exported in FK order under one UTC-day prefix, and the
// manifest carries the row counts — the restore contract.
func TestBrainSnapshotRunOnceWritesAllTablesAndManifest(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, tbl := range brainSnapshotTables {
		rows := sqlmock.NewRows([]string{"row_to_json"})
		for j := 0; j <= i; j++ { // table k has k+1 rows
			rows.AddRow([]byte(`{"id":` + strings.Repeat("1", j+1) + `}`))
		}
		mock.ExpectQuery(regexp.QuoteMeta("SELECT row_to_json(t) FROM " + tbl + " t ORDER BY id")).WillReturnRows(rows)
	}
	store := &fakeBrainSnapshotStore{}
	w := NewBrainSnapshotWorker(db, nil).SetStore(store)
	w.bucket = "test-bucket"
	w.now = func() time.Time { return time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC) }
	m, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if m.Day != "2026-09-13" || len(m.Tables) != len(brainSnapshotTables) {
		t.Fatalf("manifest = %+v", m)
	}
	for i, tbl := range brainSnapshotTables {
		key := "brain-snapshots/2026-09-13/" + tbl + ".jsonl"
		body, ok := store.puts[key]
		if !ok {
			t.Fatalf("missing key %s (have %v)", key, keysOf(store.puts))
		}
		if lines := strings.Count(string(body), "\n"); lines != i+1 || m.Tables[tbl] != int64(i+1) {
			t.Errorf("%s: %d lines, manifest %d, want %d", tbl, lines, m.Tables[tbl], i+1)
		}
	}
	var man BrainSnapshotManifest
	if err := json.Unmarshal(store.puts["brain-snapshots/2026-09-13/manifest.json"], &man); err != nil || man.Tables["jarvis_brain_eval_runs"] != 5 {
		t.Fatalf("manifest.json = %s (%v)", store.puts["brain-snapshots/2026-09-13/manifest.json"], err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Negative path: no bucket ⇒ refused, nothing written, no panic.
func TestBrainSnapshotRefusesWithoutBucket(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	t.Setenv("BRAIN_SNAPSHOT_BUCKET", "")
	t.Setenv("ENGINE_S3_BUCKET", "")
	w := NewBrainSnapshotWorker(db, nil)
	if _, err := w.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "no bucket") {
		t.Fatalf("want no-bucket refusal, got %v", err)
	}
}

// The public CDN bucket must never be the default target.
func TestBrainSnapshotDefaultBucketIsEngineNotCDN(t *testing.T) {
	t.Setenv("BRAIN_SNAPSHOT_BUCKET", "")
	t.Setenv("ENGINE_S3_BUCKET", "ignite-pmta-engine")
	t.Setenv("JARVIS_S3_BUCKET", "jarvis-image-cdn")
	w := NewBrainSnapshotWorker(nil, nil)
	if w.bucket != "ignite-pmta-engine" {
		t.Fatalf("default bucket = %q", w.bucket)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
