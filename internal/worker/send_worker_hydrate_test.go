package worker

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newHydrateTestPool(t *testing.T) (*SendWorkerPool, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	pool := NewSendWorkerPool(db, 1)
	return pool, mock
}

// TestHydrateSnapshots_PassesThroughInlineRows is the dual-read contract:
// rows carrying html_content inline (legacy enqueue, pre-cutover backlog)
// must flow through completely untouched.
func TestHydrateSnapshots_PassesThroughInlineRows(t *testing.T) {
	pool, mock := newHydrateTestPool(t)

	items := []QueueItem{
		{ID: uuid.New(), HTMLContent: "<html>inline legacy body</html>", TextContent: "txt"},
		{ID: uuid.New(), HTMLContent: "<html>inline with snapshot id too</html>", ContentSnapshotID: uuid.New()},
	}
	out := pool.hydrateSnapshots(context.Background(), items)
	if len(out) != 2 {
		t.Fatalf("expected 2 items, got %d", len(out))
	}
	if out[0].HTMLContent != "<html>inline legacy body</html>" || out[1].HTMLContent != "<html>inline with snapshot id too</html>" {
		t.Fatal("inline rows must pass through unmodified")
	}
	// No DB activity expected at all.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestHydrateSnapshots_RendersDeterministicBody verifies the snapshot path
// produces exactly the legacy per-row bytes: mutateHTMLHash seeded by
// (subscriber, wave), and plain content filled from the snapshot when the
// row has none. (The honeypot bot-trap link was removed 2026-07-22.)
func TestHydrateSnapshots_RendersDeterministicBody(t *testing.T) {
	pool, mock := newHydrateTestPool(t)

	snapID := uuid.New()
	subscriberID := uuid.New()
	waveID := uuid.New()
	snap := &ContentSnapshot{ID: snapID, HTMLContent: snapTestHTML, PlainContent: "snapshot plain"}
	pool.snapshotCache.put(snapID, snap)

	items := []QueueItem{{
		ID:                uuid.New(),
		SubscriberID:      subscriberID,
		WaveID:            waveID,
		ContentSnapshotID: snapID,
	}}
	out := pool.hydrateSnapshots(context.Background(), items)
	if len(out) != 1 {
		t.Fatalf("expected 1 item, got %d", len(out))
	}

	want := mutateHTMLHash(snapTestHTML, computeMutationSeed(subscriberID, waveID.String()))
	if out[0].HTMLContent != want {
		t.Fatal("hydrated body diverges from the legacy enqueue-time bytes")
	}
	if out[0].TextContent != "snapshot plain" {
		t.Fatalf("plain content not filled from snapshot: %q", out[0].TextContent)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestHydrateSnapshots_DropsItemOnLoadFailure: a row whose snapshot can't be
// loaded must be marked failed and dropped — an empty body must never
// continue toward an ISP.
func TestHydrateSnapshots_DropsItemOnLoadFailure(t *testing.T) {
	pool, mock := newHydrateTestPool(t)

	missingSnap := uuid.New()
	itemID := uuid.New()

	// Snapshot load (cache miss → DB) fails.
	mock.ExpectQuery(`FROM mailing_content_snapshots WHERE id`).
		WillReturnError(sql.ErrNoRows)
	// markFailed (legacy outbox mode, non-transient error): attempts probe
	// then UPDATE ... status='failed'.
	mock.ExpectQuery(`SELECT COALESCE\(attempts, 0\) FROM mailing_campaign_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"attempts"}).AddRow(0))
	mock.ExpectExec(`UPDATE mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	items := []QueueItem{
		{ID: itemID, SubscriberID: uuid.New(), WaveID: uuid.New(), ContentSnapshotID: missingSnap},
		{ID: uuid.New(), HTMLContent: "<html>survivor</html>"},
	}
	out := pool.hydrateSnapshots(context.Background(), items)
	if len(out) != 1 {
		t.Fatalf("expected only the inline survivor, got %d items", len(out))
	}
	if out[0].HTMLContent != "<html>survivor</html>" {
		t.Fatal("wrong item survived")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
