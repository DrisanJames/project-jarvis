package worker

import (
	"container/list"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// ContentSnapshot is one immutable, hash-addressed base creative shared by all
// queue rows of a wave. The body stored here is the campaign content AFTER
// sanitizeVariantURLsAtDispatch but BEFORE per-recipient fingerprint mutation —
// the mutation layer (mutateHTMLHash) is recomputed at
// send time from computeMutationSeed(subscriber_id, wave_id), which is
// deterministic, so the bytes on the wire are identical to the legacy
// copy-per-row path. See docs/CAMPAIGN_QUEUE_STORAGE_REDESIGN.md §5.
type ContentSnapshot struct {
	ID            uuid.UUID
	HTMLContent   string
	PlainContent  string
	ContentLocked bool
}

// computeContentHash addresses a snapshot by its full send-relevant identity.
// content_locked participates in the hash because it changes send-time
// behavior (mutation bypass): two campaigns with identical bodies but
// different lock flags must not collapse onto one snapshot row.
func computeContentHash(html, plain string, locked bool) string {
	h := sha256.New()
	h.Write([]byte(html))
	h.Write([]byte{0})
	h.Write([]byte(plain))
	h.Write([]byte{0})
	if locked {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ensureContentSnapshot returns the snapshot id for the given base content,
// inserting it if no row with the same content_hash exists yet.
//
// Runs on db (autocommit), NOT inside the wave-enqueue transaction, by design:
//   - the snapshot row is committed before any queue row referencing it,
//     preserving the crash-safety invariant even mid-wave;
//   - SELECT-first + ON CONFLICT DO NOTHING + re-SELECT never holds a row lock
//     on a shared snapshot, so concurrent waves reusing one creative don't
//     serialize on it (a DO UPDATE upsert would pin the row until commit).
//
// A snapshot orphaned by a wave-tx rollback is harmless: rows are immutable,
// deduped by hash, and reclaimed by future retention of unreferenced rows.
func ensureContentSnapshot(ctx context.Context, db *sql.DB, campaignID uuid.UUID, waveID string, html, plain string, locked bool) (uuid.UUID, error) {
	hash := computeContentHash(html, plain, locked)

	var id uuid.UUID
	err := db.QueryRowContext(ctx,
		`SELECT id FROM mailing_content_snapshots WHERE content_hash = $1`, hash,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return uuid.Nil, fmt.Errorf("snapshot lookup: %w", err)
	}

	var waveUUID interface{}
	if parsed, parseErr := uuid.Parse(waveID); parseErr == nil {
		waveUUID = parsed
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO mailing_content_snapshots
			(id, content_hash, campaign_id, wave_id, html_content, plain_content, content_locked)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (content_hash) DO NOTHING
		RETURNING id
	`, uuid.New(), hash, campaignID, waveUUID, html, plain, locked).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return uuid.Nil, fmt.Errorf("snapshot insert: %w", err)
	}

	// Lost a concurrent-insert race: the other writer's row is committed by
	// the time the unique-index wait resolves, so this re-read must find it.
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM mailing_content_snapshots WHERE content_hash = $1`, hash,
	).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("snapshot re-lookup after conflict: %w", err)
	}
	return id, nil
}

// renderSnapshotBody reproduces, at send time, exactly what the legacy enqueue
// path stored per-row: the per-recipient hash mutation, with mutation bypassed
// for content-locked campaigns. Any change to this ordering breaks byte-for-byte
// equivalence with rows enqueued before the snapshot cutover.
func renderSnapshotBody(snap *ContentSnapshot, subscriberID uuid.UUID, waveID string) string {
	html := snap.HTMLContent
	if !snap.ContentLocked {
		html = mutateHTMLHash(html, computeMutationSeed(subscriberID, waveID))
	}
	return html
}

// ---------------------------------------------------------------------------
// In-process snapshot cache (LRU + singleflight)
// ---------------------------------------------------------------------------

type snapshotCall struct {
	done chan struct{}
	snap *ContentSnapshot
	err  error
}

type snapshotCacheEntry struct {
	id   uuid.UUID
	snap *ContentSnapshot
}

// SnapshotCache caches loaded snapshots so N send workers claiming the same
// wave don't each fetch the (potentially large) body from Postgres. Loads are
// singleflighted per snapshot id: at wave start, the first claimer performs
// the one DB read and every concurrent claimer waits on it.
type SnapshotCache struct {
	mu       sync.Mutex
	max      int
	entries  map[uuid.UUID]*list.Element
	lru      *list.List // front = most recently used
	inflight map[uuid.UUID]*snapshotCall
}

// defaultSnapshotCacheSize bounds resident bodies. Snapshots are ~15KB-200KB;
// 128 entries ≈ a few MB worst case, far more distinct creatives than are
// ever in flight in one day.
const defaultSnapshotCacheSize = 128

func NewSnapshotCache(max int) *SnapshotCache {
	if max <= 0 {
		max = defaultSnapshotCacheSize
	}
	return &SnapshotCache{
		max:      max,
		entries:  map[uuid.UUID]*list.Element{},
		lru:      list.New(),
		inflight: map[uuid.UUID]*snapshotCall{},
	}
}

// Get returns the snapshot, loading it from the DB at most once concurrently.
// Errors are returned but never cached, so a transient DB failure doesn't
// poison the id.
func (c *SnapshotCache) Get(ctx context.Context, db *sql.DB, id uuid.UUID) (*ContentSnapshot, error) {
	c.mu.Lock()
	if el, ok := c.entries[id]; ok {
		c.lru.MoveToFront(el)
		snap := el.Value.(*snapshotCacheEntry).snap
		c.mu.Unlock()
		return snap, nil
	}
	if call, ok := c.inflight[id]; ok {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.snap, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &snapshotCall{done: make(chan struct{})}
	c.inflight[id] = call
	c.mu.Unlock()

	snap, err := loadContentSnapshot(ctx, db, id)
	call.snap, call.err = snap, err
	close(call.done)

	c.mu.Lock()
	delete(c.inflight, id)
	if err == nil {
		el := c.lru.PushFront(&snapshotCacheEntry{id: id, snap: snap})
		c.entries[id] = el
		for c.lru.Len() > c.max {
			oldest := c.lru.Back()
			c.lru.Remove(oldest)
			delete(c.entries, oldest.Value.(*snapshotCacheEntry).id)
		}
	}
	c.mu.Unlock()
	return snap, err
}

// put pre-populates the cache; used by tests and available for warm-on-enqueue.
func (c *SnapshotCache) put(id uuid.UUID, snap *ContentSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[id]; ok {
		return
	}
	el := c.lru.PushFront(&snapshotCacheEntry{id: id, snap: snap})
	c.entries[id] = el
	for c.lru.Len() > c.max {
		oldest := c.lru.Back()
		c.lru.Remove(oldest)
		delete(c.entries, oldest.Value.(*snapshotCacheEntry).id)
	}
}

func loadContentSnapshot(ctx context.Context, db *sql.DB, id uuid.UUID) (*ContentSnapshot, error) {
	snap := &ContentSnapshot{ID: id}
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(html_content, ''), COALESCE(plain_content, ''), COALESCE(content_locked, FALSE)
		FROM mailing_content_snapshots WHERE id = $1
	`, id).Scan(&snap.HTMLContent, &snap.PlainContent, &snap.ContentLocked)
	if err != nil {
		return nil, fmt.Errorf("load snapshot %s: %w", id, err)
	}
	if strings.TrimSpace(snap.HTMLContent) == "" {
		return nil, fmt.Errorf("snapshot %s has empty html_content", id)
	}
	return snap, nil
}
