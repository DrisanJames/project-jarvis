package dataingest

// counters.go is the Redis side of the Data Ingest dashboard: the WRITER the
// consumer calls once per event, the READER the API serves from, and the
// ROLLUP that freezes a finished day into data_ingest_rollup.
//
// WHY REDIS AND NOT A GROUP BY: the operator's requirement is a dashboard that
// "doesn't load the database". partner_clean_queue is ~15M rows; the one
// grouped scan the reservoir endpoint runs measures 8.7s and is already cached
// for 60s. Counting at the WRITER and reading hashes keeps the live numbers at
// O(1) per event and O(#keys) per page.
//
// IDEMPOTENCY: every operation carries an op_id and the writer SETNXs
// di:op:<op_id> (72h) BEFORE incrementing. A Kafka replay, a consumer-group
// reset or a double POST therefore counts once. 72h covers a weekend replay
// and is far longer than any retention-driven redelivery window.
//
// NO SCAN / NO KEYS: the contract's five key shapes cannot be enumerated
// without knowing the datasets and batches that were active. Rather than SCAN a
// SHARED production Redis, the writer maintains four small INDEX keys per day
// (ix:cls, ix:t, ix:ds, ix:batch) plus ix:dsmeta (dataset -> "class|lane").
// Every read and the rollup walk those indexes, so the read path touches only
// keys we know exist.
//
// TTL: 100 days on every day-scoped key (the dashboard's 30-day series plus
// slack for a late reconcile), 72h on the op-id guards. Nothing here is the
// settled truth — data_ingest_rollup is, and the nightly reconcile writes it
// from the TABLES.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// KeyTTL is the retention on every day-scoped counter key.
	KeyTTL = 100 * 24 * time.Hour
	// OpTTL is the retention on the per-operation idempotency guard.
	OpTTL = 72 * time.Hour
	// DayLayout is the Denver calendar day every key is bucketed by.
	DayLayout = "2006-01-02"
)

// ErrNoRedis is returned when a Counters/Reader was built with a nil client.
// Callers must render the affected field as "not_measured" — NEVER as 0, which
// is the failure the dashboard exists to avoid.
var ErrNoRedis = errors.New("dataingest: redis client is nil (counters unavailable)")

// Denver is the operational-day location. Falls back to UTC if the container
// has no tzdata, mirroring internal/api/property_ledger.go:44.
var Denver = func() *time.Location {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// DayOf returns the Denver calendar day of t ("2006-01-02").
func DayOf(t time.Time) string { return t.In(Denver).Format(DayLayout) }

// HourOf returns the Denver hour of t as a zero-padded "00".."23".
func HourOf(t time.Time) string { return t.In(Denver).Format("15") }

// Today returns the current Denver day.
func Today() string { return DayOf(time.Now()) }

// ── key shapes (the contract's, verbatim, plus the index keys) ──────────────

func keyClass(day, class, transition string) string {
	return "di:d:" + day + ":cls:" + class + ":t:" + transition
}
func keyDataset(day, datasetID, transition string) string {
	return "di:d:" + day + ":ds:" + datasetID + ":t:" + transition
}
func keyHour(day, hour, class string) string {
	return "di:d:" + day + ":hr:" + hour + ":cls:" + class
}
func keyBatch(day, batchID, transition string) string {
	return "di:d:" + day + ":batch:" + batchID + ":t:" + transition
}
func keyLastDataset(datasetID string) string { return "di:last:ds:" + datasetID }
func keyOp(opID string) string               { return "di:op:" + opID }

func ixClasses(day string) string     { return "di:d:" + day + ":ix:cls" }
func ixTransitions(day string) string { return "di:d:" + day + ":ix:t" }
func ixDatasets(day string) string    { return "di:d:" + day + ":ix:ds" }
func ixBatches(day string) string     { return "di:d:" + day + ":ix:batch" }
func ixDatasetMeta(day string) string { return "di:d:" + day + ":ix:dsmeta" }

// ── writer ──────────────────────────────────────────────────────────────────

// Counters applies events to Redis. A nil *redis.Client yields a disabled
// Counters whose Apply returns ErrNoRedis — it never panics and never silently
// counts nothing.
type Counters struct {
	rdb *redis.Client
	// hub receives one Delta per APPLIED event so the SSE stream can push
	// without re-reading Redis. nil = no fan-out.
	hub *Hub
}

// NewCounters builds the writer. rdb may be nil (disabled).
func NewCounters(rdb *redis.Client) *Counters { return &Counters{rdb: rdb} }

// WithHub attaches the delta fan-out used by GET /data-ingest/stream.
func (c *Counters) WithHub(h *Hub) *Counters {
	if c != nil {
		c.hub = h
	}
	return c
}

// Available reports whether the counters have a live Redis client.
func (c *Counters) Available() bool { return c != nil && c.rdb != nil }

// Apply increments every counter for one event, idempotently.
//
// Returns applied=false, err=nil when the op_id was already seen — that is a
// SUCCESS (the record may be committed), not a failure. Any other error is
// returned so the consumer retries and, if permanent, DLQs the record rather
// than committing an offset over an uncounted operation.
func (c *Counters) Apply(ctx context.Context, ev Event) (bool, error) {
	if !c.Available() {
		return false, ErrNoRedis
	}
	ev.Normalize()
	if err := ev.Validate(); err != nil {
		return false, err
	}

	// Idempotency FIRST: the guard is set before any HINCRBY, so a crash
	// between the two re-runs the increments (at-least-once, counted once only
	// if the guard survived) rather than dropping them silently.
	ok, err := c.rdb.SetNX(ctx, keyOp(ev.OpID), "1", OpTTL).Result()
	if err != nil {
		return false, fmt.Errorf("dataingest: op guard: %w", err)
	}
	if !ok {
		return false, nil // duplicate — already counted
	}

	day := DayOf(ev.At)
	hour := HourOf(ev.At)
	buckets := ev.Buckets()

	pipe := c.rdb.Pipeline()
	touch := func(key string) { pipe.Expire(ctx, key, KeyTTL) }

	ck := keyClass(day, ev.Class, ev.Transition)
	for isp, n := range buckets {
		if n == 0 {
			continue
		}
		pipe.HIncrBy(ctx, ck, isp, n)
	}
	touch(ck)

	if ev.DatasetID != "" {
		dk := keyDataset(day, ev.DatasetID, ev.Transition)
		for isp, n := range buckets {
			if n == 0 {
				continue
			}
			pipe.HIncrBy(ctx, dk, isp, n)
		}
		touch(dk)
		pipe.SAdd(ctx, ixDatasets(day), ev.DatasetID)
		pipe.HSet(ctx, ixDatasetMeta(day), ev.DatasetID, ev.Class+"|"+ev.Lane)
		touch(ixDatasets(day))
		touch(ixDatasetMeta(day))
		pipe.Set(ctx, keyLastDataset(ev.DatasetID), ev.At.UTC().Format(time.RFC3339), KeyTTL)
	}

	if ev.BatchID != "" {
		bk := keyBatch(day, ev.BatchID, ev.Transition)
		for isp, n := range buckets {
			if n == 0 {
				continue
			}
			pipe.HIncrBy(ctx, bk, isp, n)
		}
		touch(bk)
		pipe.SAdd(ctx, ixBatches(day), ev.BatchID)
		touch(ixBatches(day))
	}

	hk := keyHour(day, hour, ev.Class)
	if ev.N != 0 {
		pipe.HIncrBy(ctx, hk, ev.Transition, ev.N)
	}
	touch(hk)

	pipe.SAdd(ctx, ixClasses(day), ev.Class)
	pipe.SAdd(ctx, ixTransitions(day), ev.Transition)
	touch(ixClasses(day))
	touch(ixTransitions(day))

	if _, err := pipe.Exec(ctx); err != nil {
		// Release the guard so the framework's retry can count this operation:
		// with the guard left set, the retry would read "duplicate" and commit
		// the offset over increments that never happened (2026-09-20 review).
		if derr := c.rdb.Del(context.Background(), keyOp(ev.OpID)).Err(); derr != nil {
			return false, fmt.Errorf("dataingest: counter pipeline: %w (guard %s NOT released: %v — operation lost)", err, ev.OpID, derr)
		}
		return false, fmt.Errorf("dataingest: counter pipeline: %w", err)
	}

	delta := Delta{
		Day:         day,
		SupplyClass: ev.Class,
		Transition:  ev.Transition,
		DatasetID:   ev.DatasetID,
		Lane:        ev.Lane,
		ByISP:       buckets,
		N:           ev.N,
		Origin:      originID,
	}
	if c.hub != nil {
		c.hub.Publish(delta)
	}
	// The hub is per process and the consumer group splits partitions across
	// the service's tasks, so a browser attached to one task would see only
	// that task's share (2026-09-20 review). Every applied delta also goes
	// over Redis pub/sub; StartDeltaRelay on each task republishes the OTHER
	// tasks' deltas into its local hub (Origin filters out its own).
	if b, err := json.Marshal(delta); err == nil {
		if perr := c.rdb.Publish(ctx, DeltaChannel, b).Err(); perr != nil {
			log.Printf("[data-ingest] delta publish failed (local subscribers still served): %v", perr)
		}
	}
	return true, nil
}

// DeltaChannel is the Redis pub/sub channel that carries every applied delta
// to every task of the service.
const DeltaChannel = "di:deltas"

// originID identifies this process on the channel so a task never re-applies
// its own deltas to its own hub.
var originID = uuid.NewString()

// StartDeltaRelay subscribes to DeltaChannel and publishes every delta from
// ANOTHER process into hub. Returns immediately; stops when ctx is done.
// go-redis re-subscribes on connection loss on its own.
func StartDeltaRelay(ctx context.Context, rdb *redis.Client, hub *Hub) {
	if rdb == nil || hub == nil {
		return
	}
	go func() {
		ps := rdb.Subscribe(ctx, DeltaChannel)
		defer ps.Close()
		ch := ps.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var d Delta
				if err := json.Unmarshal([]byte(msg.Payload), &d); err != nil || d.Origin == originID {
					continue
				}
				hub.Publish(d)
			}
		}
	}()
}

// ── reader ──────────────────────────────────────────────────────────────────

// Reader serves the live counters to the API. A nil client yields a Reader
// whose Available() is false; handlers must then mark the field not_measured.
type Reader struct{ rdb *redis.Client }

// NewReader builds the read side. rdb may be nil (disabled).
func NewReader(rdb *redis.Client) *Reader { return &Reader{rdb: rdb} }

// Available reports whether live counters can be read at all.
func (r *Reader) Available() bool { return r != nil && r.rdb != nil }

// GetDay returns class -> transition -> isp -> n for one Denver day, read from
// the CLASS hashes (the authoritative totals). Missing keys are absent from the
// map, never zero-filled — "no key" and "zero" are different answers.
func (r *Reader) GetDay(ctx context.Context, day string) (map[string]map[string]map[string]int64, error) {
	if !r.Available() {
		return nil, ErrNoRedis
	}
	classes, err := r.members(ctx, ixClasses(day))
	if err != nil {
		return nil, err
	}
	trans, err := r.members(ctx, ixTransitions(day))
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]map[string]int64{}
	for _, cls := range classes {
		for _, t := range trans {
			h, err := r.hash(ctx, keyClass(day, cls, t))
			if err != nil {
				return nil, err
			}
			if len(h) == 0 {
				continue
			}
			if out[cls] == nil {
				out[cls] = map[string]map[string]int64{}
			}
			out[cls][t] = h
		}
	}
	return out, nil
}

// HourCount is one Denver hour of one supply class.
type HourCount struct {
	Hour         int              `json:"hour"`
	N            int64            `json:"n"`
	ByTransition map[string]int64 `json:"by_transition"`
}

// GetHours returns the 24 Denver hours of one class for a day. Hours with no
// key come back with N=0 and a nil map — the caller knows the day is measured
// (the class key existed) so a genuine quiet hour IS zero.
func (r *Reader) GetHours(ctx context.Context, day, class string) ([]HourCount, error) {
	if !r.Available() {
		return nil, ErrNoRedis
	}
	out := make([]HourCount, 0, 24)
	for h := 0; h < 24; h++ {
		hh := fmt.Sprintf("%02d", h)
		m, err := r.hash(ctx, keyHour(day, hh, class))
		if err != nil {
			return nil, err
		}
		var total int64
		for _, v := range m {
			total += v
		}
		out = append(out, HourCount{Hour: h, N: total, ByTransition: m})
	}
	return out, nil
}

// GetDataset returns transition -> isp -> n for one feed on one day.
func (r *Reader) GetDataset(ctx context.Context, day, datasetID string) (map[string]map[string]int64, error) {
	if !r.Available() {
		return nil, ErrNoRedis
	}
	trans, err := r.members(ctx, ixTransitions(day))
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]int64{}
	for _, t := range trans {
		h, err := r.hash(ctx, keyDataset(day, datasetID, t))
		if err != nil {
			return nil, err
		}
		if len(h) > 0 {
			out[t] = h
		}
	}
	return out, nil
}

// GetBatch returns transition -> isp -> n for one load (batch) on one day.
func (r *Reader) GetBatch(ctx context.Context, day, batchID string) (map[string]map[string]int64, error) {
	if !r.Available() {
		return nil, ErrNoRedis
	}
	trans, err := r.members(ctx, ixTransitions(day))
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]int64{}
	for _, t := range trans {
		h, err := r.hash(ctx, keyBatch(day, batchID, t))
		if err != nil {
			return nil, err
		}
		if len(h) > 0 {
			out[t] = h
		}
	}
	return out, nil
}

// LastEvent returns dataset_id -> RFC3339 timestamp of that feed's most recent
// event, for the ids that HAVE one. An id absent from the result has never been
// seen by the counters — the caller renders "not_measured", not "never".
func (r *Reader) LastEvent(ctx context.Context, datasetIDs []string) (map[string]string, error) {
	if !r.Available() {
		return nil, ErrNoRedis
	}
	out := map[string]string{}
	if len(datasetIDs) == 0 {
		return out, nil
	}
	pipe := r.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, 0, len(datasetIDs))
	for _, id := range datasetIDs {
		cmds = append(cmds, pipe.Get(ctx, keyLastDataset(id)))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	for i, cmd := range cmds {
		v, err := cmd.Result()
		if err != nil {
			continue // redis.Nil => never seen
		}
		out[datasetIDs[i]] = v
	}
	return out, nil
}

// Datasets returns the feeds that produced at least one event on a day.
func (r *Reader) Datasets(ctx context.Context, day string) ([]string, error) {
	return r.members(ctx, ixDatasets(day))
}

// Batches returns the loads that produced at least one event on a day.
func (r *Reader) Batches(ctx context.Context, day string) ([]string, error) {
	return r.members(ctx, ixBatches(day))
}

// Measured reports whether ANY counter exists for a day. False means the day is
// not measured (bus dark, flag off, Redis flushed) — the caller must render
// not_measured rather than a confident zero.
func (r *Reader) Measured(ctx context.Context, day string) (bool, error) {
	if !r.Available() {
		return false, ErrNoRedis
	}
	n, err := r.rdb.SCard(ctx, ixClasses(day)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, err
	}
	return n > 0, nil
}

func (r *Reader) members(ctx context.Context, key string) ([]string, error) {
	if !r.Available() {
		return nil, ErrNoRedis
	}
	vals, err := r.rdb.SMembers(ctx, key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	sort.Strings(vals)
	return vals, nil
}

func (r *Reader) hash(ctx context.Context, key string) (map[string]int64, error) {
	raw, err := r.rdb.HGetAll(ctx, key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]int64, len(raw))
	for k, v := range raw {
		n, convErr := strconv.ParseInt(v, 10, 64)
		if convErr != nil {
			continue
		}
		out[k] = n
	}
	return out, nil
}

// ── rollup ──────────────────────────────────────────────────────────────────

// RollupRow is one data_ingest_rollup row.
type RollupRow struct {
	Day         string `json:"day"`
	SupplyClass string `json:"supply_class"`
	DatasetID   string `json:"dataset_id,omitempty"` // "" => SQL NULL
	Lane        string `json:"lane,omitempty"`
	Transition  string `json:"transition"`
	ISP         string `json:"isp"`
	N           int64  `json:"n"`
}

// Rollup reads one Denver day's counters into rows for data_ingest_rollup.
//
// SHAPE (read this before summing the table): per (class, transition, isp) it
// emits one row PER DATASET plus, when the class total exceeds the datasets'
// sum, ONE residual row with dataset_id NULL for the events that carried no
// dataset (subscriber imports, site events, transfers). A plain
// SUM(n) GROUP BY day, supply_class, transition is therefore exact, and the
// per-feed breakdown is the same rows filtered to dataset_id IS NOT NULL. The
// two never double-count.
func (r *Reader) Rollup(ctx context.Context, day string) ([]RollupRow, error) {
	if !r.Available() {
		return nil, ErrNoRedis
	}
	classes, err := r.members(ctx, ixClasses(day))
	if err != nil {
		return nil, err
	}
	trans, err := r.members(ctx, ixTransitions(day))
	if err != nil {
		return nil, err
	}
	datasets, err := r.members(ctx, ixDatasets(day))
	if err != nil {
		return nil, err
	}
	meta, err := r.rdb.HGetAll(ctx, ixDatasetMeta(day)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	dsClass := map[string]string{}
	dsLane := map[string]string{}
	for id, v := range meta {
		parts := strings.SplitN(v, "|", 2)
		dsClass[id] = parts[0]
		if len(parts) > 1 {
			dsLane[id] = parts[1]
		}
	}

	rows := make([]RollupRow, 0, 256)
	for _, cls := range classes {
		for _, t := range trans {
			total, err := r.hash(ctx, keyClass(day, cls, t))
			if err != nil {
				return nil, err
			}
			if len(total) == 0 {
				continue
			}
			attributed := map[string]int64{}
			for _, ds := range datasets {
				if dsClass[ds] != cls {
					continue
				}
				h, err := r.hash(ctx, keyDataset(day, ds, t))
				if err != nil {
					return nil, err
				}
				for isp, n := range h {
					if n == 0 {
						continue
					}
					attributed[isp] += n
					rows = append(rows, RollupRow{
						Day: day, SupplyClass: cls, DatasetID: ds, Lane: dsLane[ds],
						Transition: t, ISP: isp, N: n,
					})
				}
			}
			for isp, n := range total {
				if rem := n - attributed[isp]; rem > 0 {
					rows = append(rows, RollupRow{
						Day: day, SupplyClass: cls, Transition: t, ISP: isp, N: rem,
					})
				}
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.SupplyClass != b.SupplyClass {
			return a.SupplyClass < b.SupplyClass
		}
		if a.Transition != b.Transition {
			return a.Transition < b.Transition
		}
		if a.DatasetID != b.DatasetID {
			return a.DatasetID < b.DatasetID
		}
		return a.ISP < b.ISP
	})
	return rows, nil
}

// ── SSE fan-out ─────────────────────────────────────────────────────────────

// Delta is one applied operation, pushed to every open /stream subscriber.
type Delta struct {
	Day         string           `json:"day"`
	SupplyClass string           `json:"supply_class"`
	Transition  string           `json:"transition"`
	DatasetID   string           `json:"dataset_id,omitempty"`
	Lane        string           `json:"lane,omitempty"`
	ByISP       map[string]int64 `json:"by_isp,omitempty"`
	N           int64            `json:"n"`
	// Origin is the producing process (see StartDeltaRelay); not for clients.
	Origin string `json:"origin,omitempty"`
}

// Hub is the in-process delta fan-out between the Kafka consumer and the SSE
// handler. Bounded and DROP-ON-FULL by design: a slow browser must never stall
// the consumer that is counting production traffic.
type Hub struct {
	mu   sync.RWMutex
	subs map[chan Delta]struct{}
}

// NewHub builds an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan Delta]struct{}{}} }

// defaultHub lets the consumer (internal/eventbus/consumers) and the API
// (internal/api) share one fan-out without either importing the other.
var defaultHub = NewHub()

// DefaultHub is the process-wide delta hub.
func DefaultHub() *Hub { return defaultHub }

// Subscribe returns a receive channel and its cancel func. The caller MUST call
// cancel (defer) or the subscription leaks.
func (h *Hub) Subscribe() (<-chan Delta, func()) {
	ch := make(chan Delta, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// Publish fans one delta out, dropping for any subscriber whose buffer is full.
func (h *Hub) Publish(d Delta) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs {
		select {
		case ch <- d:
		default: // slow client: drop, never block the consumer
		}
	}
}

// Subscribers reports the number of open subscriptions (for /state liveness).
func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}
