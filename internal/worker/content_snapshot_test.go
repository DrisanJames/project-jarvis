package worker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

const snapTestHTML = `<html><body><table><tr><td style="color: red; padding: 4px;">save big deals</td></tr></table><p>hello</p></body></html>`

func TestComputeContentHash_StableAndDistinct(t *testing.T) {
	h1 := computeContentHash(snapTestHTML, "plain", false)
	h2 := computeContentHash(snapTestHTML, "plain", false)
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %s vs %s", h1, h2)
	}
	if computeContentHash(snapTestHTML, "plain", true) == h1 {
		t.Fatal("content_locked flag must participate in the hash")
	}
	if computeContentHash(snapTestHTML, "other-plain", false) == h1 {
		t.Fatal("plain content must participate in the hash")
	}
	if computeContentHash(snapTestHTML+"x", "plain", false) == h1 {
		t.Fatal("html content must participate in the hash")
	}
	// Concatenation ambiguity guard: ("ab","c") must differ from ("a","bc").
	if computeContentHash("ab", "c", false) == computeContentHash("a", "bc", false) {
		t.Fatal("hash must not be ambiguous across the html/plain boundary")
	}
}

// TestRenderSnapshotBody_MatchesLegacyEnqueueOrder is the byte-for-byte
// equivalence contract for the snapshot cutover: send-time rendering must
// reproduce exactly what enqueueWaveRowAtATime stores per row — the per-
// recipient hash mutation, with mutation bypassed for content-locked
// campaigns. (The honeypot bot-trap link was removed 2026-07-22.)
func TestRenderSnapshotBody_MatchesLegacyEnqueueOrder(t *testing.T) {
	subscriberID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	waveID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Legacy enqueue path, unlocked: mutate only.
	seed := computeMutationSeed(subscriberID, waveID)
	legacy := mutateHTMLHash(snapTestHTML, seed)

	snap := &ContentSnapshot{HTMLContent: snapTestHTML, ContentLocked: false}
	got := renderSnapshotBody(snap, subscriberID, waveID)
	if got != legacy {
		t.Fatalf("unlocked snapshot render diverges from legacy enqueue output\nlegacy: %q\ngot:    %q", legacy, got)
	}

	// Locked: mutation bypassed — the stored HTML is sent verbatim.
	snapLocked := &ContentSnapshot{HTMLContent: snapTestHTML, ContentLocked: true}
	if got := renderSnapshotBody(snapLocked, subscriberID, waveID); got != snapTestHTML {
		t.Fatal("locked snapshot render must send the stored HTML verbatim")
	}
	// And a locked render must never carry the removed honeypot link.
	if strings.Contains(renderSnapshotBody(snapLocked, subscriberID, waveID), "api/mailing/bt/") {
		t.Fatal("bot-trap honeypot link must not be present in rendered body")
	}

	// Different recipients in one wave must produce different fingerprints.
	other := renderSnapshotBody(snap, uuid.MustParse("99999999-8888-7777-6666-555555555555"), waveID)
	if other == got {
		t.Fatal("two subscribers produced identical fingerprinted bodies")
	}
}

func TestEnsureContentSnapshot_ReusesExistingByHash(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	existing := uuid.New()
	mock.ExpectQuery(`SELECT id FROM mailing_content_snapshots WHERE content_hash`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(existing.String()))

	got, err := ensureContentSnapshot(context.Background(), db, uuid.New(), uuid.New().String(), snapTestHTML, "", false)
	if err != nil {
		t.Fatalf("ensureContentSnapshot: %v", err)
	}
	if got != existing {
		t.Fatalf("expected existing snapshot %s, got %s", existing, got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureContentSnapshot_InsertsWhenMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	inserted := uuid.New()
	mock.ExpectQuery(`SELECT id FROM mailing_content_snapshots WHERE content_hash`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO mailing_content_snapshots`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(inserted.String()))

	got, err := ensureContentSnapshot(context.Background(), db, uuid.New(), uuid.New().String(), snapTestHTML, "plain", true)
	if err != nil {
		t.Fatalf("ensureContentSnapshot: %v", err)
	}
	if got != inserted {
		t.Fatalf("expected inserted snapshot %s, got %s", inserted, got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureContentSnapshot_LosesInsertRaceAndRereads(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	winner := uuid.New()
	mock.ExpectQuery(`SELECT id FROM mailing_content_snapshots WHERE content_hash`).
		WillReturnError(sql.ErrNoRows)
	// ON CONFLICT DO NOTHING + RETURNING yields zero rows when we lose the race.
	mock.ExpectQuery(`INSERT INTO mailing_content_snapshots`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`SELECT id FROM mailing_content_snapshots WHERE content_hash`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(winner.String()))

	got, err := ensureContentSnapshot(context.Background(), db, uuid.New(), uuid.New().String(), snapTestHTML, "", false)
	if err != nil {
		t.Fatalf("ensureContentSnapshot: %v", err)
	}
	if got != winner {
		t.Fatalf("expected race winner's snapshot %s, got %s", winner, got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotCache_SingleflightLoadsOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id := uuid.New()
	// Exactly ONE load expected, no matter how many concurrent getters.
	mock.ExpectQuery(`FROM mailing_content_snapshots WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"html_content", "plain_content", "content_locked"}).
			AddRow(snapTestHTML, "plain", false))

	cache := NewSnapshotCache(8)
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := cache.Get(context.Background(), db, id)
			if err != nil {
				errs <- err
				return
			}
			if snap.HTMLContent != snapTestHTML {
				errs <- fmt.Errorf("wrong body: %q", snap.HTMLContent)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected exactly one snapshot load: %v", err)
	}

	// Subsequent Get is a pure cache hit (no further expectations registered,
	// so a DB round trip here would fail the mock).
	if _, err := cache.Get(context.Background(), db, id); err != nil {
		t.Fatalf("cache hit failed: %v", err)
	}
}

func TestSnapshotCache_EvictsLRU(t *testing.T) {
	cache := NewSnapshotCache(2)
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	cache.put(a, &ContentSnapshot{ID: a, HTMLContent: "a"})
	cache.put(b, &ContentSnapshot{ID: b, HTMLContent: "b"})
	cache.put(c, &ContentSnapshot{ID: c, HTMLContent: "c"}) // evicts a

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if _, ok := cache.entries[a]; ok {
		t.Fatal("oldest entry should have been evicted")
	}
	if len(cache.entries) != 2 {
		t.Fatalf("expected 2 resident entries, got %d", len(cache.entries))
	}
	if _, ok := cache.entries[b]; !ok {
		t.Fatal("entry b should be resident")
	}
	if _, ok := cache.entries[c]; !ok {
		t.Fatal("entry c should be resident")
	}
}

func TestLoadContentSnapshot_RejectsEmptyBody(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_content_snapshots WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"html_content", "plain_content", "content_locked"}).
			AddRow("   ", "", false))

	if _, err := loadContentSnapshot(context.Background(), db, uuid.New()); err == nil {
		t.Fatal("empty html_content must be an error — an empty body must never reach an ISP")
	}
}
