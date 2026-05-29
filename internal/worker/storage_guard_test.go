package worker

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestStorageGuard_NoReplicaExpectsZeroSlots(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM pg_replication_slots`).WillReturnRows(
		sqlmock.NewRows([]string{"slot_name", "active", "retained_bytes"}),
	)
	mock.ExpectQuery(`status = 'accepted'`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0),
	)
	mock.ExpectQuery(`operationally-terminal|dead_letter_strict`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0),
	)
	mock.ExpectQuery(`pmta_acct_raw WHERE processed = FALSE`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(100),
	)

	g := NewStorageGuard(db, false)
	g.runOnce(context.Background())

	snap := g.Snapshot()
	if snap.ReplicationSlotCount != 0 {
		t.Fatalf("expected 0 slots, got %d", snap.ReplicationSlotCount)
	}
	if len(snap.Alerts) != 0 {
		t.Fatalf("unexpected alerts: %v", snap.Alerts)
	}
}

func TestStorageGuard_UnexpectedSlotWhenNoReplicaConfigured(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM pg_replication_slots`).WillReturnRows(
		sqlmock.NewRows([]string{"slot_name", "active", "retained_bytes"}).
			AddRow("apex_postgres_read", false, int64(15*1024*1024*1024)),
	)
	mock.ExpectQuery(`status = 'accepted'`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0),
	)
	mock.ExpectQuery(`dead_letter_strict`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0),
	)
	mock.ExpectQuery(`pmta_acct_raw WHERE processed = FALSE`).WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0),
	)

	g := NewStorageGuard(db, false)
	g.runOnce(context.Background())

	snap := g.Snapshot()
	if snap.ReplicationSlotCount != 1 {
		t.Fatalf("expected 1 slot, got %d", snap.ReplicationSlotCount)
	}
	if len(snap.Alerts) < 2 {
		t.Fatalf("expected unexpected-slot and inactive-slot alerts, got %v", snap.Alerts)
	}
}
