package api

// REQ-118 §8.2 test 13 — "Old jobs and sidecars cannot bypass reservation
// (fence test)."
//
// The fresh-broadcast runner holds the last direct `partner_clean_queue ->
// claimed` UPDATE outside the drip orchestrator (fresh_broadcast_runner.go, the
// `queue claim` statement in stage). Under DRIP_SUPPLY_BROADCAST_FENCE=1 it must
// refuse and write NOTHING; with the flag unset it must behave exactly as
// before. Both halves are asserted here, and the negative control is the second
// one: a fence test that only proves the blocked case cannot tell a fence from a
// runner that is broken for everybody.
//
// sqlmock is the right tool for this one: the assertion is "how many statements
// reached the database", which a mock counts exactly and a real Postgres does
// not.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// queueBackedAssignment is one cell holding one QUEUE-sourced row — the shape
// that reaches the direct claim UPDATE.
func queueBackedAssignment() map[freshCell][]freshDrawRow {
	return map[freshCell][]freshDrawRow{
		{Site: "DB", Lane: "GMAIL"}: {{
			QueueID:      uuid.New().String(),
			Email:        "fence@example.com",
			EmailMD5:     "0123456789abcdef0123456789abcdef",
			ISPFamily:    "gmail",
			SubscriberID: uuid.New().String(),
		}},
	}
}

// tagBackedAssignment is the wcm shape: no QueueID, so nothing this batch does
// can ever claim a queue row.
func tagBackedAssignment() map[freshCell][]freshDrawRow {
	return map[freshCell][]freshDrawRow{
		{Site: "WCL", Lane: "GMAIL"}: {{
			Email:        "tagged@example.com",
			EmailMD5:     "abcdef0123456789abcdef0123456789",
			SubscriberID: uuid.New().String(),
		}},
	}
}

func fenceTestConfig() freshStreamConfig {
	return freshStreamConfig{
		StreamKey: "consumer",
		Label:     "Consumer",
		SegPrefix: "CONSUMER",
		DailyCap:  1000,
	}
}

// TestFreshBroadcastFenceBlocksClaim is test 13's positive half: flag armed,
// queue-backed batch, error returned and ZERO statements issued.
func TestFreshBroadcastFenceBlocksClaim(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_BROADCAST_FENCE", "1")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()
	// No ExpectQuery/ExpectExec at all: any statement the runner issues is an
	// unexpected call, which sqlmock reports through the call's error AND
	// through ExpectationsWereMet.

	r := &FreshBroadcastRunner{db: db}
	_, err = r.stage(context.Background(), uuid.New(), fenceTestConfig(),
		queueBackedAssignment(), time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), "S10")

	if !errors.Is(err, errBroadcastFenced) {
		t.Fatalf("stage() error = %v, want errBroadcastFenced", err)
	}
	if !strings.Contains(err.Error(), "REQ-118") {
		t.Errorf("fence error %q does not name the section an operator would grep for", err)
	}
	if merr := mock.ExpectationsWereMet(); merr != nil {
		t.Errorf("fenced stage touched the database: %v", merr)
	}
}

// TestFreshBroadcastFenceOffUnchanged is the NEGATIVE CONTROL: with the flag
// unset the runner must run its old path. It is allowed to fail on the mock —
// what is proven is that it got PAST the fence and issued the first statement,
// so the fence is a flag and not a permanent break.
func TestFreshBroadcastFenceOffUnchanged(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_BROADCAST_FENCE", "")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	// The first thing stage does after the fence is the per-cell segment
	// lookup. Make it fail so the test stops there without needing the whole
	// staging path mocked.
	mock.ExpectQuery(`SELECT id::text FROM mailing_segments`).
		WillReturnError(errors.New("boom"))

	r := &FreshBroadcastRunner{db: db}
	_, err = r.stage(context.Background(), uuid.New(), fenceTestConfig(),
		queueBackedAssignment(), time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), "S10")

	if errors.Is(err, errBroadcastFenced) {
		t.Fatalf("stage() was fenced with DRIP_SUPPLY_BROADCAST_FENCE unset — the flag is not the gate")
	}
	if err == nil || !strings.Contains(err.Error(), "segment lookup") {
		t.Fatalf("stage() error = %v, want the segment-lookup failure that proves the old path ran", err)
	}
	if merr := mock.ExpectationsWereMet(); merr != nil {
		t.Errorf("expected the segment lookup to be issued: %v", merr)
	}
}

// TestFreshBroadcastFenceOnlyArmsForNonOneValues pins the flag's vocabulary:
// only "1" arms it. A truthy-looking value that silently did nothing would be
// the worst outcome of the §7 cutover.
func TestFreshBroadcastFenceFlagVocabulary(t *testing.T) {
	cases := map[string]bool{
		"1":     true,
		" 1 ":   true,
		"":      false,
		"0":     false,
		"true":  false,
		"yes":   false,
		"1,off": false,
	}
	for v, want := range cases {
		t.Setenv("DRIP_SUPPLY_BROADCAST_FENCE", v)
		if got := broadcastFenceEnabled(); got != want {
			t.Errorf("broadcastFenceEnabled() with %q = %v, want %v", v, got, want)
		}
	}
}

// TestFreshBroadcastFenceIgnoresTagSourcedBatch pins the scope of the fence: it
// gates the CLAIM, not the runner. A tag-sourced batch never reaches the queue
// UPDATE, so arming the fence must not break it.
func TestFreshBroadcastFenceIgnoresTagSourcedBatch(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_BROADCAST_FENCE", "1")

	if assignmentClaimsQueueRows(tagBackedAssignment()) {
		t.Fatal("tag-sourced assignment reported as claiming queue rows")
	}
	if !assignmentClaimsQueueRows(queueBackedAssignment()) {
		t.Fatal("queue-sourced assignment reported as claiming nothing")
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT id::text FROM mailing_segments`).
		WillReturnError(errors.New("boom"))

	r := &FreshBroadcastRunner{db: db}
	_, err = r.stage(context.Background(), uuid.New(), fenceTestConfig(),
		tagBackedAssignment(), time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), "S10")
	if errors.Is(err, errBroadcastFenced) {
		t.Fatalf("tag-sourced batch was fenced; the fence must gate the claim, not the runner")
	}
	if err == nil || !strings.Contains(err.Error(), "segment lookup") {
		t.Fatalf("stage() error = %v, want the segment-lookup failure that proves the old path ran", err)
	}
}
