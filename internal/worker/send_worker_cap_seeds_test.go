package worker

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/redis/go-redis/v9"
)

func TestIsInboxScoreSeedRecipient(t *testing.T) {
	for email, want := range map[string]bool{
		"inbox@seed-microsoft.projectjarvis.io": true,
		"INBOX@Seed-Gmail.projectjarvis.io ":    true,
		"inbox@seed-cables.projectjarvis.io":    true,
		"someone@outlook.com":                   false,
		"seed-microsoft.projectjarvis.io":       false, // no local part
		"a@":                                    false,
		"":                                      false,
	} {
		if got := isInboxScoreSeedRecipient(email); got != want {
			t.Fatalf("%q: got %v want %v", email, got, want)
		}
	}
}

// Regression for 2026-10-27: the cross-brand daily cap (30/person/day) skipped the Inbox
// Score seeds like any subscriber, so the Microsoft seed saw 30 evening cells and then
// 1,008 'cross_brand_cap_exceeded' skips — zero test-inbox verdicts for the whole day.
// Seeds must pass the cap untouched; real subscribers must still be capped.
func TestApplyCrossBrandCap_SeedsAreExempt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	pool := &SendWorkerPool{db: db, ctx: context.Background(), orgID: ""} // orgID "" → defaultCap, no org lookup
	pool.capChecker = mailing.NewCapChecker(db, rdb, 1)                  // cap of ONE send per person per day

	seed := uuid.New()
	person := uuid.New()
	skippedID := uuid.New()
	items := []QueueItem{
		{ID: uuid.New(), SubscriberID: seed, Email: "inbox@seed-microsoft.projectjarvis.io"},
		{ID: uuid.New(), SubscriberID: seed, Email: "inbox@seed-microsoft.projectjarvis.io"},
		{ID: uuid.New(), SubscriberID: seed, Email: "inbox@seed-microsoft.projectjarvis.io"},
		{ID: uuid.New(), SubscriberID: person, Email: "someone@outlook.com"},
		{ID: skippedID, SubscriberID: person, Email: "someone@outlook.com"},
	}
	// exactly one markSkipped: the person's second item
	mock.ExpectExec(`UPDATE mailing_campaign_queue`).
		WithArgs(skippedID, "cross_brand_cap_exceeded").
		WillReturnResult(sqlmock.NewResult(0, 1))

	kept := pool.applyCrossBrandCap(items)
	if len(kept) != 4 {
		t.Fatalf("want 4 kept (3 seed + 1 person), got %d", len(kept))
	}
	for _, it := range kept[:3] {
		if it.SubscriberID != seed {
			t.Fatalf("seed item dropped: %+v", kept)
		}
	}
	if kept[3].SubscriberID != person || kept[3].ID == skippedID {
		t.Fatalf("wrong person item kept: %+v", kept[3])
	}
	if pool.totalSkippedCap != 1 {
		t.Fatalf("totalSkippedCap=%d want 1", pool.totalSkippedCap)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// Negative control: the seed never reserved a slot, so no cap key exists for it.
	if n := len(mr.Keys()); n != 1 {
		t.Fatalf("want exactly 1 cap key (the person), got %d: %v", n, mr.Keys())
	}
}
