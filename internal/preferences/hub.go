package preferences

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mode governs whether a failed ShouldSend changes a send.
type Mode string

const (
	// ModeShadow (the default): count would-skip by reason, never change a send.
	ModeShadow Mode = "shadow"
	// ModeEnforce: the call site skips the recipient.
	ModeEnforce Mode = "enforce"
)

// ModeFromEnv reads PREFERENCES_MODE per call so a task restart with a new
// value takes effect without a rebuild. Anything but "enforce" is shadow.
func ModeFromEnv() Mode {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("PREFERENCES_MODE")), string(ModeEnforce)) {
		return ModeEnforce
	}
	return ModeShadow
}

// Frequency values stored for a subscriber.
const (
	FrequencyNormal  = "normal"
	FrequencyReduced = "reduced"
	FrequencyWeekly  = "weekly"
)

// ValidFrequency reports whether f is a storable frequency value.
func ValidFrequency(f string) bool {
	return f == FrequencyNormal || f == FrequencyReduced || f == FrequencyWeekly
}

// Preference is one mailing_subscriber_preferences row. BrandRoot "" = all
// brands (the column is NULL).
type Preference struct {
	OrgID       string
	EmailHash   string
	BrandRoot   string
	PausedUntil *time.Time
	Frequency   string
	Topics      map[string]bool // topic -> subscribed; absent = default (subscribed)
	UpdatedAt   time.Time
	Source      string
}

// affectsSend reports whether the row can make ShouldSend return false — only
// these rows are held in memory (bounded memory: frequency alone is stored for
// display, not enforced, so it never needs a hot-path slot).
func (p Preference) affectsSend(now time.Time) bool {
	if p.PausedUntil != nil && now.Before(*p.PausedUntil) {
		return true
	}
	for _, sub := range p.Topics {
		if !sub {
			return true
		}
	}
	return false
}

// DefaultMaxRows bounds the in-memory set. A row costs ~200 bytes; 500k is
// ~100 MB worst case. Past the bound the load is truncated and /health says so.
const DefaultMaxRows = 500_000

// Hub is the in-memory ShouldSend index — the same shape as the global
// suppression hub: loaded from the DB at boot, refreshed on every write this
// task makes, and fully reloaded on an interval so writes made by the sibling
// ECS task converge within that interval. Unlike the suppression hub a reload
// REPLACES the set (a pause can end, a preference can be relaxed).
type Hub struct {
	db      *sql.DB
	orgID   string
	maxRows int
	modeFn  func() Mode

	mu        sync.RWMutex
	rows      map[string][]Preference // email_hash -> rows (<=1 per brand + 1 all-brands)
	loadedAt  time.Time
	loadErr   string
	truncated bool
	loaded    int

	checked   atomic.Int64
	cntMu     sync.Mutex
	wouldSkip map[string]int64 // "<site>.<reason>"
	skipped   map[string]int64
}

// NewHub builds an empty hub; call Load or Start to populate it.
func NewHub(db *sql.DB, orgID string) *Hub {
	return &Hub{
		db:        db,
		orgID:     orgID,
		maxRows:   DefaultMaxRows,
		modeFn:    ModeFromEnv,
		rows:      map[string][]Preference{},
		wouldSkip: map[string]int64{},
		skipped:   map[string]int64{},
	}
}

// SetModeFunc overrides the mode source (tests).
func (h *Hub) SetModeFunc(f func() Mode) { h.modeFn = f }

// SetMaxRows overrides the memory bound (tests).
func (h *Hub) SetMaxRows(n int) { h.maxRows = n }

const loadSQL = `
SELECT email_hash, COALESCE(brand_root, ''), paused_until, COALESCE(topics::text, '{}')
FROM mailing_subscriber_preferences
WHERE organization_id = $1
  AND (paused_until > NOW() OR topics <> '{}'::jsonb)
LIMIT $2`

// Load replaces the in-memory set from the DB. A missing table (first boot,
// migration not yet landed) leaves the hub empty and is not an error.
func (h *Hub) Load(ctx context.Context) error {
	if h == nil || h.db == nil {
		return nil
	}
	now := time.Now()
	rows, err := h.db.QueryContext(ctx, loadSQL, h.orgID, h.maxRows+1)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			h.setLoadResult(map[string][]Preference{}, 0, false, "table absent (startup migration not yet applied)")
			return nil
		}
		h.setLoadErr(err.Error())
		return fmt.Errorf("load preferences: %w", err)
	}
	defer rows.Close()

	next := map[string][]Preference{}
	n := 0
	truncated := false
	for rows.Next() {
		if n >= h.maxRows {
			truncated = true
			break
		}
		var p Preference
		var paused sql.NullTime
		var topics string
		if err := rows.Scan(&p.EmailHash, &p.BrandRoot, &paused, &topics); err != nil {
			continue
		}
		if paused.Valid {
			t := paused.Time
			p.PausedUntil = &t
		}
		p.Topics = parseTopics(topics)
		if !p.affectsSend(now) {
			continue
		}
		next[p.EmailHash] = append(next[p.EmailHash], p)
		n++
	}
	if err := rows.Err(); err != nil {
		h.setLoadErr(err.Error())
		return fmt.Errorf("load preferences: %w", err)
	}
	if truncated {
		log.Printf("[preferences] ERROR: load truncated at %d rows — preferences beyond the bound are NOT evaluated", h.maxRows)
	}
	h.setLoadResult(next, n, truncated, "")
	return nil
}

func (h *Hub) setLoadResult(next map[string][]Preference, n int, truncated bool, note string) {
	h.mu.Lock()
	h.rows = next
	h.loaded = n
	h.truncated = truncated
	h.loadedAt = time.Now()
	h.loadErr = note
	h.mu.Unlock()
}

func (h *Hub) setLoadErr(e string) {
	h.mu.Lock()
	h.loadErr = e
	h.mu.Unlock()
}

// Start loads now and then every interval until ctx is cancelled.
func (h *Hub) Start(ctx context.Context, interval time.Duration) {
	if h == nil {
		return
	}
	go func() {
		load := func() {
			lctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			if err := h.Load(lctx); err != nil {
				log.Printf("[preferences] reload failed: %v", err)
			}
		}
		load()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				load()
			}
		}
	}()
}

// Apply refreshes one row in memory after this task wrote it (refresh on write).
func (h *Hub) Apply(p Preference) {
	if h == nil {
		return
	}
	p.BrandRoot = strings.ToLower(strings.TrimSpace(p.BrandRoot))
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.rows[p.EmailHash]
	out := cur[:0:0]
	for _, r := range cur {
		if r.BrandRoot != p.BrandRoot {
			out = append(out, r)
		}
	}
	if p.affectsSend(time.Now()) {
		out = append(out, p)
	}
	if len(out) == 0 {
		delete(h.rows, p.EmailHash)
	} else {
		h.rows[p.EmailHash] = out
	}
}

// ShouldSend reports whether email may be mailed for brandRoot/topic at now,
// and the reason when it may not. Pure read — no counters.
func (h *Hub) ShouldSend(email, brandRoot, topic string, now time.Time) (bool, string) {
	if h == nil {
		return true, ""
	}
	hash := EmailHash(email)
	brandRoot = strings.ToLower(strings.TrimSpace(brandRoot))
	h.mu.RLock()
	rows := h.rows[hash]
	h.mu.RUnlock()
	for _, p := range rows {
		if p.BrandRoot != "" && p.BrandRoot != brandRoot {
			continue
		}
		if p.PausedUntil != nil && now.Before(*p.PausedUntil) {
			return false, "paused"
		}
		if topic != "" {
			if sub, ok := p.Topics[topic]; ok && !sub {
				return false, "topic_opt_out"
			}
		}
	}
	return true, ""
}

// Gate is the call-site entry point. It evaluates ShouldSend, counts a
// would-skip under "<site>.<reason>", and returns skip=true ONLY in enforce
// mode. In shadow it never changes the send. Nil-safe.
func (h *Hub) Gate(site, email, brandRoot, topic string, now time.Time) (skip bool, reason string) {
	if h == nil {
		return false, ""
	}
	h.checked.Add(1)
	ok, reason := h.ShouldSend(email, brandRoot, topic, now)
	if ok {
		return false, ""
	}
	key := site + "." + reason
	enforce := h.modeFn() == ModeEnforce
	h.cntMu.Lock()
	h.wouldSkip[key]++
	if enforce {
		h.skipped[key]++
	}
	h.cntMu.Unlock()
	return enforce, reason
}

// Status is the /health "preferences" block.
type Status struct {
	Wired             bool             `json:"wired"`
	Mode              string           `json:"mode"`
	LoadedRows        int              `json:"loaded_rows"`
	LoadedAt          *time.Time       `json:"loaded_at"`
	LoadNote          string           `json:"load_note,omitempty"`
	Truncated         bool             `json:"truncated"`
	Checked           int64            `json:"checked"`
	WouldSkip         map[string]int64 `json:"would_skip"`
	Skipped           map[string]int64 `json:"skipped"`
	FrequencyEnforced bool             `json:"frequency_enforced"`
	TopicsEnforced    bool             `json:"topics_enforced"`
}

// Status snapshots the hub. Nil-safe (wired=false).
func (h *Hub) Status() Status {
	if h == nil {
		return Status{Mode: string(ModeFromEnv()), WouldSkip: map[string]int64{}, Skipped: map[string]int64{}}
	}
	st := Status{Wired: true, Mode: string(h.modeFn()), Checked: h.checked.Load()}
	h.mu.RLock()
	st.LoadedRows, st.Truncated, st.LoadNote = h.loaded, h.truncated, h.loadErr
	if !h.loadedAt.IsZero() {
		t := h.loadedAt
		st.LoadedAt = &t
	}
	h.mu.RUnlock()
	h.cntMu.Lock()
	st.WouldSkip = copyCounts(h.wouldSkip)
	st.Skipped = copyCounts(h.skipped)
	h.cntMu.Unlock()
	return st
}

func copyCounts(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var current atomic.Pointer[Hub]

// SetCurrent publishes the process hub for call sites that are not handed it
// directly (the planner, /health, the preference page).
func SetCurrent(h *Hub) { current.Store(h) }

// Current returns the process hub or nil before boot wiring.
func Current() *Hub { return current.Load() }

// CurrentStatus is Current().Status() (nil-safe).
func CurrentStatus() Status { return Current().Status() }

func parseTopics(s string) map[string]bool {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]bool
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// topicsJSON renders topics deterministically for storage.
func topicsJSON(m map[string]bool) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]bool, len(m))
	for _, k := range keys {
		ordered[k] = m[k]
	}
	b, _ := json.Marshal(ordered)
	return string(b)
}

// ── store ───────────────────────────────────────────────────────────────────

// UpsertSQL writes one row keyed on (org, email_hash, COALESCE(brand_root,”)),
// matching the unique expression index created by runStartupMigrations.
const UpsertSQL = `
INSERT INTO mailing_subscriber_preferences
	(organization_id, email_hash, brand_root, paused_until, frequency, topics, updated_at, source)
VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6::jsonb, NOW(), $7)
ON CONFLICT (organization_id, email_hash, (COALESCE(brand_root, ''))) DO UPDATE SET
	paused_until = EXCLUDED.paused_until,
	frequency    = EXCLUDED.frequency,
	topics       = EXCLUDED.topics,
	updated_at   = NOW(),
	source       = EXCLUDED.source`

// Upsert writes p to the DB and, on success, refreshes this task's hub.
func Upsert(ctx context.Context, db *sql.DB, h *Hub, p Preference) error {
	if !ValidFrequency(p.Frequency) {
		p.Frequency = FrequencyNormal
	}
	var paused interface{}
	if p.PausedUntil != nil {
		paused = *p.PausedUntil
	}
	if _, err := db.ExecContext(ctx, UpsertSQL,
		p.OrgID, p.EmailHash, strings.ToLower(strings.TrimSpace(p.BrandRoot)),
		paused, p.Frequency, topicsJSON(p.Topics), p.Source); err != nil {
		return fmt.Errorf("upsert preference: %w", err)
	}
	h.Apply(p)
	return nil
}

// GetSQL reads every row for one address (all-brands row + per-brand rows).
const GetSQL = `
SELECT COALESCE(brand_root, ''), paused_until, frequency, COALESCE(topics::text, '{}'), updated_at, COALESCE(source, '')
FROM mailing_subscriber_preferences
WHERE organization_id = $1 AND email_hash = $2`

// Get returns the stored rows for an address, authoritative from the DB.
func Get(ctx context.Context, db *sql.DB, orgID, emailHash string) ([]Preference, error) {
	rows, err := db.QueryContext(ctx, GetSQL, orgID, emailHash)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []Preference
	for rows.Next() {
		p := Preference{OrgID: orgID, EmailHash: emailHash}
		var paused sql.NullTime
		var topics string
		if err := rows.Scan(&p.BrandRoot, &paused, &p.Frequency, &topics, &p.UpdatedAt, &p.Source); err != nil {
			return nil, err
		}
		if paused.Valid {
			t := paused.Time
			p.PausedUntil = &t
		}
		p.Topics = parseTopics(topics)
		out = append(out, p)
	}
	return out, rows.Err()
}
