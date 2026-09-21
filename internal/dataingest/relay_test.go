package dataingest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// A delta published by ANOTHER process reaches this process's hub through the
// relay; this process's own deltas (same Origin) are filtered out, so a task
// never double-publishes what its consumer already pushed locally.
func TestDeltaRelay_ForwardsOtherOriginsOnly(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	hub := NewHub()
	ch, cancel := hub.Subscribe()
	defer cancel()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	StartDeltaRelay(ctx, rdb, hub)
	time.Sleep(50 * time.Millisecond) // let the subscription attach

	own, _ := json.Marshal(Delta{Day: "2026-09-20", SupplyClass: "dynamic", Transition: "landed", N: 5, Origin: originID})
	other, _ := json.Marshal(Delta{Day: "2026-09-20", SupplyClass: "at_rest", Transition: "landed", N: 7, Origin: "task-b"})
	if err := rdb.Publish(ctx, DeltaChannel, own).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Publish(ctx, DeltaChannel, other).Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-ch:
		if d.Origin != "task-b" || d.N != 7 {
			t.Fatalf("relayed the wrong delta: %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("other-origin delta never reached the hub")
	}
	select {
	case d := <-ch:
		t.Fatalf("own-origin delta must be filtered, got %+v", d)
	case <-time.After(200 * time.Millisecond):
	}
}

// The dataset's latest stamped batch decides the class; the dataset's
// source_channel is only the fallback. (Reconcile 2026-09-20: the family
// lane's verdicts were labelled at_rest by the dataset rule while every batch
// of that dataset is internal_transfer.)
func TestDatasetSupplyClass_LatestBatchWins(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ResetDatasetClassCache()
	id := "8b3d1c2e-1111-4222-8333-444455556666"
	mock.ExpectQuery(`SELECT d.source_channel`).WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"source_channel", "last_batch_class"}).AddRow("", "internal_transfer"))
	if got := DatasetSupplyClass(context.Background(), db, id); got != ClassInternalTransfer {
		t.Fatalf("got %q, want internal_transfer from the latest batch", got)
	}
	ResetDatasetClassCache()
	mock.ExpectQuery(`SELECT d.source_channel`).WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"source_channel", "last_batch_class"}).AddRow("api_feed", nil))
	if got := DatasetSupplyClass(context.Background(), db, id); got != ClassDynamic {
		t.Fatalf("got %q, want dynamic from source_channel fallback", got)
	}
}
