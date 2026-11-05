package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ConvictionStore manages the conviction memory for all agents.
// Convictions live in a hot in-memory ring buffer per agent and are
// periodically flushed to S3 as JSONL. Pattern recognition is performed
// at query time across the micro-observations, never at storage time.
type ConvictionStore struct {
	memory *MemoryStore
	db     *sql.DB
	mu     sync.RWMutex

	// Hot buffer: agentKey -> ring of recent convictions
	// agentKey = "{isp}/{agentType}"
	buffers map[string]*convictionRing

	maxPerAgent int

	// SSE subscriber fan-out
	subMu       sync.RWMutex
	subscribers map[string]chan Conviction

	// Velocity tracking: timestamps of recent Record() calls
	velMu      sync.Mutex
	velHistory []time.Time

	// dbPersistDisabled, when true, suppresses INSERTs into
	// mailing_engine_convictions. The hot in-memory ring, S3 archival
	// (via MemoryStore), and SSE fan-out are unaffected — only the
	// redundant DB copy is skipped. Used to relieve RDS write IO when the
	// convictions engine is operationally decommissioned.
	dbPersistDisabled bool
}

// convictionRing is a bounded FIFO of Convictions (oldest first). It is a true
// circular buffer (2026-11-05 CPU audit): the previous shape did
// `convictions = convictions[1:]; append(...)` once full, which reallocated and
// copied all max (2,000) ~500-byte structs on EVERY append, and pre-allocated
// the full capacity per (ISP, agent) ring — 60 MB of mostly-empty heap in the
// prod heap profile. buf grows geometrically until it holds max entries, then
// head rotates. Index i (0 = oldest) lives at buf[(head+i)%len(buf)].
type convictionRing struct {
	buf  []Conviction
	head int
	max  int
}

func newConvictionRing(max int) *convictionRing {
	if max < 1 {
		max = 1
	}
	initial := 64
	if initial > max {
		initial = max
	}
	return &convictionRing{
		buf: make([]Conviction, 0, initial),
		max: max,
	}
}

func (r *convictionRing) append(c Conviction) {
	if len(r.buf) < r.max {
		r.buf = append(r.buf, c)
		return
	}
	r.buf[r.head] = c
	r.head = (r.head + 1) % r.max
}

// len is the number of convictions held (<= max).
func (r *convictionRing) len() int { return len(r.buf) }

// at returns a pointer to the i-th oldest conviction (0 <= i < len()). The
// pointer is only valid while the store lock is held; callers copy what they
// keep. Scanning via at() is how RecallSimilar avoids copying the whole ring.
func (r *convictionRing) at(i int) *Conviction {
	return &r.buf[(r.head+i)%len(r.buf)]
}

func (r *convictionRing) all() []Conviction {
	out := make([]Conviction, len(r.buf))
	for i := range r.buf {
		out[i] = *r.at(i)
	}
	return out
}

func (r *convictionRing) byVerdict(v Verdict) []Conviction {
	var out []Conviction
	for i := range r.buf {
		if c := r.at(i); c.Verdict == v {
			out = append(out, *c)
		}
	}
	return out
}

// NewConvictionStore creates a conviction store backed by the given MemoryStore.
func NewConvictionStore(memory *MemoryStore) *ConvictionStore {
	return &ConvictionStore{
		memory:      memory,
		buffers:     make(map[string]*convictionRing),
		maxPerAgent: 2000,
		subscribers: make(map[string]chan Conviction),
	}
}

// SetDB enables database-backed conviction persistence as a fallback to S3.
func (cs *ConvictionStore) SetDB(db *sql.DB) {
	cs.db = db
}

// DisableDBPersist suppresses DB writes to mailing_engine_convictions while
// leaving the in-memory ring, S3 archival, and SSE fan-out intact. Reversible
// at construction time via the ENGINE_DECISION_PERSIST_DISABLED env var.
func (cs *ConvictionStore) DisableDBPersist() {
	cs.dbPersistDisabled = true
}

// Subscribe registers an SSE client. Returns a read channel and a unique ID.
func (cs *ConvictionStore) Subscribe(id string) <-chan Conviction {
	ch := make(chan Conviction, 100)
	cs.subMu.Lock()
	cs.subscribers[id] = ch
	cs.subMu.Unlock()
	log.Printf("[conviction] subscriber %s connected (%d active)", id, len(cs.subscribers))
	return ch
}

// Unsubscribe removes an SSE client and closes its channel.
func (cs *ConvictionStore) Unsubscribe(id string) {
	cs.subMu.Lock()
	if ch, ok := cs.subscribers[id]; ok {
		close(ch)
		delete(cs.subscribers, id)
	}
	cs.subMu.Unlock()
	log.Printf("[conviction] subscriber %s disconnected (%d active)", id, len(cs.subscribers))
}

func (cs *ConvictionStore) fanOut(c Conviction) {
	cs.subMu.RLock()
	defer cs.subMu.RUnlock()
	for _, ch := range cs.subscribers {
		select {
		case ch <- c:
		default:
			// Drop oldest if buffer full, then push
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- c:
			default:
			}
		}
	}
}

func (cs *ConvictionStore) trackVelocity() {
	cs.velMu.Lock()
	defer cs.velMu.Unlock()
	now := time.Now()
	cs.velHistory = append(cs.velHistory, now)
	// Prune entries older than 5 minutes
	cutoff := now.Add(-5 * time.Minute)
	start := 0
	for start < len(cs.velHistory) && cs.velHistory[start].Before(cutoff) {
		start++
	}
	if start > 0 {
		cs.velHistory = cs.velHistory[start:]
	}
}

// VelocityStats holds conviction throughput metrics.
type VelocityStats struct {
	PerMinute1m float64 `json:"per_minute_1m"`
	PerMinute5m float64 `json:"per_minute_5m"`
	Total1m     int     `json:"total_1m"`
	Total5m     int     `json:"total_5m"`
}

// Velocity computes conviction throughput for the last 1m and 5m.
func (cs *ConvictionStore) Velocity() VelocityStats {
	cs.velMu.Lock()
	defer cs.velMu.Unlock()
	now := time.Now()
	cutoff1m := now.Add(-1 * time.Minute)
	cutoff5m := now.Add(-5 * time.Minute)
	count1m, count5m := 0, 0
	for _, t := range cs.velHistory {
		if t.After(cutoff5m) {
			count5m++
		}
		if t.After(cutoff1m) {
			count1m++
		}
	}
	return VelocityStats{
		PerMinute1m: float64(count1m),
		PerMinute5m: float64(count5m) / 5.0,
		Total1m:     count1m,
		Total5m:     count5m,
	}
}

func agentKey(isp ISP, at AgentType) string {
	return string(isp) + "/" + string(at)
}

func (cs *ConvictionStore) ring(isp ISP, at AgentType) *convictionRing {
	key := agentKey(isp, at)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	r, ok := cs.buffers[key]
	if !ok {
		r = newConvictionRing(cs.maxPerAgent)
		cs.buffers[key] = r
	}
	return r
}

// Record stores a conviction in the hot buffer and queues it for S3 persistence.
func (cs *ConvictionStore) Record(ctx context.Context, c Conviction) {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	if c.LastSeenAt.IsZero() {
		c.LastSeenAt = c.CreatedAt
	}
	if c.Confidence == 0 {
		c.Confidence = 1.0
	}
	if c.Corroborations == 0 {
		c.Corroborations = 1
	}

	r := cs.ring(c.ISP, c.AgentType)
	cs.mu.Lock()
	r.append(c)
	cs.mu.Unlock()

	if cs.memory != nil {
		cs.memory.AppendConviction(ctx, c.ISP, c.AgentType, c)
	}

	if cs.db != nil && !cs.dbPersistDisabled {
		cs.persistConvictionToDB(ctx, c)
	}

	// Fan out to SSE subscribers
	cs.fanOut(c)

	// Track velocity
	cs.trackVelocity()

	log.Printf("[conviction/%s/%s] %s: %s", c.ISP, c.AgentType, strings.ToUpper(string(c.Verdict)), c.Statement)
}

// RecallAll returns all cached convictions for an agent.
func (cs *ConvictionStore) RecallAll(isp ISP, at AgentType) []Conviction {
	r := cs.ring(isp, at)
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return r.all()
}

// RecallByVerdict returns all WILL or all WONT convictions for an agent.
func (cs *ConvictionStore) RecallByVerdict(isp ISP, at AgentType, v Verdict) []Conviction {
	r := cs.ring(isp, at)
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return r.byVerdict(v)
}

// RecallSimilar finds convictions with similar temporal and infrastructure context.
// This is the throttle agent's primary recall mechanism: "have I been in this
// situation before, and what did I decide?"
//
// Scores IN PLACE under the read lock and copies only the returned entries.
// The previous shape copied the whole ring twice (RecallAll + range-by-value)
// and swapped full ScoredConviction structs in its sort: on prod it was 14% of
// the server's CPU at idle (pprof 2026-11-05, 84% of that in runtime.duffcopy),
// called by ThrottleAgent.Evaluate for every ISP on every signal snapshot.
// Ordering is unchanged: similarity descending, ties keep ring order (oldest
// first) — the old insertion sort was stable in exactly that way.
func (cs *ConvictionStore) RecallSimilar(isp ISP, at AgentType, query MicroContext, limit int) []ScoredConviction {
	r := cs.ring(isp, at)
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	n := r.len()
	if n == 0 {
		return nil
	}
	type hit struct {
		i   int
		sim float64
	}
	var hits []hit
	for i := 0; i < n; i++ {
		if sim := contextSimilarity(query, r.at(i).Context); sim > 0.2 {
			hits = append(hits, hit{i: i, sim: sim})
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].sim > hits[b].sim })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]ScoredConviction, len(hits))
	for k, h := range hits {
		out[k] = ScoredConviction{Conviction: *r.at(h.i), Similarity: h.sim}
	}
	return out
}

// ScoredConviction pairs a conviction with its similarity score to a query.
type ScoredConviction struct {
	Conviction Conviction `json:"conviction"`
	Similarity float64    `json:"similarity"`
}

// RecallByIP returns convictions involving a specific IP address.
func (cs *ConvictionStore) RecallByIP(isp ISP, at AgentType, ip string) []Conviction {
	all := cs.RecallAll(isp, at)
	var out []Conviction
	for _, c := range all {
		if c.Context.IP == ip {
			out = append(out, c)
		}
	}
	return out
}

// RecallRecent returns the N most recent convictions for an agent.
func (cs *ConvictionStore) RecallRecent(isp ISP, at AgentType, n int) []Conviction {
	r := cs.ring(isp, at)
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	total := r.len()
	if n < 0 {
		n = 0
	}
	if n > total {
		n = total
	}
	// Copy only the n newest (oldest-first order preserved), not the whole ring.
	out := make([]Conviction, n)
	for k := 0; k < n; k++ {
		out[k] = *r.at(total - n + k)
	}
	return out
}

// Stats returns conviction counts per verdict for an agent.
func (cs *ConvictionStore) Stats(isp ISP, at AgentType) (willCount, wontCount int) {
	r := cs.ring(isp, at)
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	for i := 0; i < r.len(); i++ {
		switch r.at(i).Verdict {
		case VerdictWill:
			willCount++
		case VerdictWont:
			wontCount++
		}
	}
	return
}

func (cs *ConvictionStore) persistConvictionToDB(ctx context.Context, c Conviction) {
	ctxJSON, _ := json.Marshal(c.Context)
	var outcomeStr *string
	if c.Outcome != nil {
		b, _ := json.Marshal(c.Outcome)
		s := string(b)
		outcomeStr = &s
	}
	_, err := cs.db.ExecContext(ctx,
		`INSERT INTO mailing_engine_convictions (id, isp, agent_type, agent_id, verdict, statement, effective_rate, micro_context, outcome, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT DO NOTHING`,
		c.ID, string(c.ISP), string(c.AgentType), c.AgentID,
		string(c.Verdict), c.Statement, c.Context.AttemptedRate,
		string(ctxJSON), outcomeStr, c.CreatedAt,
	)
	if err != nil {
		log.Printf("[conviction] DB persist error: %v", err)
	}
}

func (cs *ConvictionStore) loadFromDB(ctx context.Context, isp ISP, at AgentType, limit int) ([]Conviction, error) {
	if cs.db == nil {
		return nil, nil
	}
	rows, err := cs.db.QueryContext(ctx,
		`SELECT id, isp, agent_type, agent_id, verdict, statement, effective_rate, micro_context, outcome, created_at
		 FROM mailing_engine_convictions
		 WHERE isp = $1 AND agent_type = $2
		 ORDER BY created_at DESC LIMIT $3`,
		string(isp), string(at), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query convictions: %w", err)
	}
	defer rows.Close()

	var convictions []Conviction
	for rows.Next() {
		var (
			c           Conviction
			ispStr      string
			atStr       string
			effRate     sql.NullFloat64
			ctxJSON     []byte
			outcomeJSON []byte
		)
		if err := rows.Scan(&c.ID, &ispStr, &atStr, &c.AgentID, &c.Verdict, &c.Statement, &effRate, &ctxJSON, &outcomeJSON, &c.CreatedAt); err != nil {
			log.Printf("[conviction] DB scan error: %v", err)
			continue
		}
		c.ISP = ISP(ispStr)
		c.AgentType = AgentType(atStr)
		c.LastSeenAt = c.CreatedAt
		c.Confidence = 1.0
		c.Corroborations = 1
		if len(ctxJSON) > 0 {
			_ = json.Unmarshal(ctxJSON, &c.Context)
		}
		if len(outcomeJSON) > 0 {
			var outcome ConvictionOutcome
			if json.Unmarshal(outcomeJSON, &outcome) == nil {
				c.Outcome = &outcome
			}
		}
		convictions = append(convictions, c)
	}
	// Reverse so oldest is first (ring expects chronological order)
	for i, j := 0, len(convictions)-1; i < j; i, j = i+1, j-1 {
		convictions[i], convictions[j] = convictions[j], convictions[i]
	}
	return convictions, nil
}

// LoadFromS3 hydrates the hot buffer from S3 for a given agent.
func (cs *ConvictionStore) LoadFromS3(ctx context.Context, isp ISP, at AgentType) error {
	if cs.memory == nil {
		return nil
	}
	convictions, err := cs.memory.ReadConvictions(ctx, isp, at)
	if err != nil {
		return fmt.Errorf("load convictions %s/%s: %w", isp, at, err)
	}

	r := cs.ring(isp, at)
	cs.mu.Lock()
	for _, c := range convictions {
		r.append(c)
	}
	cs.mu.Unlock()

	if len(convictions) > 0 {
		log.Printf("[conviction] loaded %d convictions for %s/%s from S3", len(convictions), isp, at)
	}
	return nil
}

// LoadAll hydrates convictions for all ISP/agent combinations.
// Tries S3 first; falls back to the DB if S3 is empty or unavailable.
func (cs *ConvictionStore) LoadAll(ctx context.Context) {
	totalRestored := 0
	for _, isp := range AllISPs() {
		for _, at := range AllAgentTypes() {
			if err := cs.LoadFromS3(ctx, isp, at); err != nil {
				log.Printf("[conviction] S3 load error %s/%s: %v", isp, at, err)
			}

			// If S3 didn't produce convictions, try DB fallback
			r := cs.ring(isp, at)
			cs.mu.RLock()
			count := r.len()
			cs.mu.RUnlock()

			if count == 0 && cs.db != nil {
				dbConvictions, err := cs.loadFromDB(ctx, isp, at, cs.maxPerAgent)
				if err != nil {
					log.Printf("[conviction] DB load error %s/%s: %v", isp, at, err)
					continue
				}
				if len(dbConvictions) > 0 {
					cs.mu.Lock()
					for _, c := range dbConvictions {
						r.append(c)
					}
					cs.mu.Unlock()
					totalRestored += len(dbConvictions)
					log.Printf("[conviction] restored %d convictions for %s/%s from DB", len(dbConvictions), isp, at)
				}
			}
		}
	}
	if totalRestored > 0 {
		log.Printf("[conviction] total restored from DB: %d convictions", totalRestored)
	}
}

// contextSimilarity computes a 0-1 similarity score between two MicroContexts.
// Weighted by factors most relevant to decision-making.
func contextSimilarity(a, b MicroContext) float64 {
	score := 0.0
	maxScore := 0.0

	// Same ISP domain
	if a.Domain != "" && b.Domain != "" {
		maxScore += 2.0
		if a.Domain == b.Domain {
			score += 2.0
		}
	}

	// Day of week match
	if a.DayOfWeek != "" && b.DayOfWeek != "" {
		maxScore += 1.5
		if a.DayOfWeek == b.DayOfWeek {
			score += 1.5
		}
	}

	// Hour proximity (within 2 hours = full match, within 4 = partial)
	maxScore += 2.0
	hourDiff := math.Abs(float64(a.HourUTC - b.HourUTC))
	if hourDiff > 12 {
		hourDiff = 24 - hourDiff
	}
	if hourDiff <= 1 {
		score += 2.0
	} else if hourDiff <= 3 {
		score += 1.0
	} else if hourDiff <= 5 {
		score += 0.5
	}

	// Holiday match (same holiday = strong signal)
	if a.IsHoliday && b.IsHoliday {
		maxScore += 3.0
		if a.HolidayName == b.HolidayName {
			score += 3.0
		} else {
			score += 1.5
		}
	} else if a.IsHoliday == b.IsHoliday {
		maxScore += 1.0
		score += 1.0
	}

	// Same IP
	if a.IP != "" && b.IP != "" {
		maxScore += 1.5
		if a.IP == b.IP {
			score += 1.5
		}
	}

	// Rate proximity (within 20% = match)
	if a.AttemptedRate > 0 && b.AttemptedRate > 0 {
		maxScore += 2.0
		ratio := float64(a.AttemptedRate) / float64(b.AttemptedRate)
		if ratio > 1 {
			ratio = 1 / ratio
		}
		if ratio > 0.8 {
			score += 2.0
		} else if ratio > 0.5 {
			score += 1.0
		}
	}

	// Volume proximity
	if a.AttemptedVolume > 0 && b.AttemptedVolume > 0 {
		maxScore += 1.0
		ratio := float64(a.AttemptedVolume) / float64(b.AttemptedVolume)
		if ratio > 1 {
			ratio = 1 / ratio
		}
		if ratio > 0.7 {
			score += 1.0
		} else if ratio > 0.4 {
			score += 0.5
		}
	}

	// Same DSN code family
	if len(a.DSNCodes) > 0 && len(b.DSNCodes) > 0 {
		maxScore += 1.5
		if dsnFamilyOverlap(a.DSNCodes, b.DSNCodes) {
			score += 1.5
		}
	}

	if maxScore == 0 {
		return 0
	}
	return score / maxScore
}

func dsnFamilyOverlap(a, b []string) bool {
	for _, ac := range a {
		for _, bc := range b {
			if dsnFamily(ac) == dsnFamily(bc) && dsnFamily(ac) != "" {
				return true
			}
		}
	}
	return false
}

// dsnFamily extracts the first digit class (4xx vs 5xx) and enhanced status
// code prefix, e.g. "421-4.7.28" -> "4.7", "550 5.1.1" -> "5.1"
func dsnFamily(code string) string {
	parts := strings.Fields(code)
	for _, p := range parts {
		segs := strings.SplitN(p, ".", 3)
		if len(segs) >= 2 {
			return segs[0] + "." + segs[1]
		}
	}
	if len(code) >= 1 {
		return string(code[0])
	}
	return ""
}
