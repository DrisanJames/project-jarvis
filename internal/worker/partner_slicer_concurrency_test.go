package worker

import (
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// Regression guard for the 2026-08-09 ingest-backlog incident.
//
// PartnerSlicerConfig.Concurrency was declared and defaulted but NEVER READ:
// Start() spawned exactly one goroutine regardless of the value, so the knob
// documented parallelism that did not exist. Because a batch costs a serialized
// S3 GetObject plus a claim transaction (~183ms), and partners post ONE record
// per HTTP call, throughput was bound by API-CALL count rather than data volume
// — 37,958 single-record batches backed up ~1.9h in front of every other lane
// (92,579 batches / ~15h on 2026-07-31).
//
// The assertion is deterministic: on boot each worker runs processOnce once,
// claims nothing, and parks on its (1 hour) ticker. So exactly Concurrency
// claim cycles must occur. Reverting Start() to a single `go ps.run()` leaves
// the other cycles unmet and fails the test.
func TestPartnerSlicer_ConcurrencyIsHonored(t *testing.T) {
	const workers = 4

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	// Exactly one claim cycle per worker. Empty rows => claimNextBatch returns
	// (nil, nil), processOnce returns, the worker parks.
	for i := 0; i < workers; i++ {
		mock.ExpectBegin()
		mock.ExpectQuery("FROM partner_inbound_batches").
			WillReturnRows(sqlmock.NewRows([]string{
				"id", "dataset_id", "partner_id", "vertical",
				"s3_bucket", "s3_key", "record_count", "next_record_offset",
			}))
		mock.ExpectRollback()
	}

	ps := NewPartnerSlicer(db, nil, "bucket", nil, PartnerSlicerConfig{
		Concurrency:  workers,
		PollInterval: time.Hour, // only the boot pass should run
	})
	if ps.cfg.Concurrency != workers {
		t.Fatalf("configured concurrency not retained: got %d want %d", ps.cfg.Concurrency, workers)
	}

	ps.Start()
	defer ps.Stop()

	// Workers stagger 250ms apart; wait for the last one plus slack.
	deadline := time.Now().Add(time.Duration(workers)*250*time.Millisecond + 3*time.Second)
	for time.Now().Before(deadline) {
		if mock.ExpectationsWereMet() == nil {
			return // all `workers` claim cycles happened — contract satisfied
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("only some workers claimed — Concurrency is not honored: %v", mock.ExpectationsWereMet())
}
