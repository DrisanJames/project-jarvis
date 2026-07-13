package worker

// REQ-034 negative-path hardening — complements send_worker_dataset_stamp_test.go
// (same package; reuses its newDatasetStampTestPool / resolveQueryRe /
// probeQueryRe / membershipRows helpers).
//
// Those tests prove the guarded paths DO run; these prove the guards actually
// SUPPRESS the hot-path queries. Technique: "poisoned" sqlmock expectations —
// an expectation whose consumption would visibly flip an assertion. This
// matters because resolveDatasetStamps fails OPEN by design: a leaked query in
// a zero-expectation test surfaces only as a swallowed "call was not expected"
// error, ExpectationsWereMet stays green, and the leak goes undetected.
// Here a leak either stamps a poison dataset id or satisfies an expectation
// that must remain unmet — absence of the query is asserted positively.

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveDatasetStamps_KillSwitchLeakDetector: with
// DISABLE_MESSAGE_DATASET_STAMP=true, the batch resolver must not touch the
// DB even when the pool is fully latched ready — a leaked query would consume
// the poisoned row and stamp the item.
func TestResolveDatasetStamps_KillSwitchLeakDetector(t *testing.T) {
	t.Setenv("DISABLE_MESSAGE_DATASET_STAMP", "true")
	pool, mock := newDatasetStampTestPool(t)
	pool.datasetResolveReady = true

	sub := uuid.New()
	poison := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	mock.ExpectQuery(resolveQueryRe).WillReturnRows(membershipRows([]membershipRow{
		{sub, poison, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
	}))

	items := pool.resolveDatasetStamps(context.Background(), []QueueItem{{SubscriberID: sub}})
	require.Len(t, items, 1)
	assert.Equal(t, uuid.Nil, items[0].PartnerDatasetID,
		"kill switch leaked: the pcq lookup ran and stamped the poison dataset")
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("kill switch leaked: the poisoned pcq expectation was consumed")
	}
}

// TestDatasetResolveIndexReady_BackoffSuppressesReprobe: after a not-ready
// probe, the pool must not hit pg_index again inside datasetResolveProbeTTL.
// The poisoned second probe returns valid=true, so a broken backoff is caught
// twice over — the second call would return ready AND consume the expectation.
func TestDatasetResolveIndexReady_BackoffSuppressesReprobe(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)

	mock.ExpectQuery(probeQueryRe).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(false))
	// Poisoned: a re-probe inside the TTL would see valid=true and flip the
	// pool to ready.
	mock.ExpectQuery(probeQueryRe).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(true))

	if pool.datasetResolveIndexReady(context.Background()) {
		t.Fatal("invalid index must report not-ready")
	}
	if pool.datasetResolveIndexReady(context.Background()) {
		t.Fatal("backoff violated: re-probed inside datasetResolveProbeTTL and saw the poisoned valid=true")
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("backoff violated: the poisoned second probe was consumed")
	}
}

// TestResolveDatasetStamps_ClosedGateSuppressesResolve: while the index gate
// is closed and inside the probe-backoff window, a batch must pass through
// with its campaign-level stamps intact and issue ZERO queries — neither the
// probe nor the poisoned resolve.
func TestResolveDatasetStamps_ClosedGateSuppressesResolve(t *testing.T) {
	pool, mock := newDatasetStampTestPool(t)
	pool.datasetResolveNextProbe = time.Now().Add(time.Hour) // dormant, mid-backoff

	sub := uuid.New()
	campaignDS := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	poison := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	mock.ExpectQuery(resolveQueryRe).WillReturnRows(membershipRows([]membershipRow{
		{sub, poison, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
	}))

	items := pool.resolveDatasetStamps(context.Background(), []QueueItem{
		{SubscriberID: sub, PartnerDatasetID: campaignDS},
	})
	require.Len(t, items, 1)
	assert.Equal(t, campaignDS, items[0].PartnerDatasetID,
		"closed gate leaked: the resolver overrode the campaign stamp with the poison dataset")
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("closed gate leaked: the poisoned resolve expectation was consumed")
	}
}
