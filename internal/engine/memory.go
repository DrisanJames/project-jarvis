package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// MemoryStore provides S3-backed long-term memory for agents.
// Each agent instance has its own S3 key namespace under
// agents/{isp}/{agentType}/.
//
// APPEND STREAMS (convictions/decisions/signals) are written as small DELTA
// OBJECTS, never by rewriting one growing file. The previous implementation's
// appendObject did GET(full file) + concat + PUT(full file) on EVERY append —
// with ~21MB journals and thousands of appends/hour that moved terabytes/day
// through the NAT gateway (the 2026-06 USW2-NatGateway-Bytes incident) and,
// with bucket versioning enabled, stored every rewritten copy (~7TB/day of
// dead versions). Deltas are buffered in memory and flushed every 30s as one
// object per stream:
//
//	agents/{isp}/{agentType}/convictions/dt=2026-06-10/040730.123-000042.jsonl
//
// Keys sort lexically == chronologically. Readers walk day prefixes newest-
// first (plus the legacy single-file key for pre-cutover history) and only
// need the recent tail: the conviction hot buffer is a 2000-entry ring.
type MemoryStore struct {
	client     *s3.Client
	bucket     string
	mu         sync.Mutex
	flushQueue map[string][]byte // whole-object writes (state.json etc.), last-write-wins
	appendQueue map[string][]byte // stream base prefix -> buffered JSONL lines pending delta flush
	flushSeq   uint64            // monotonic suffix so same-second delta keys never collide
	flushTick  *time.Ticker
	stopCh     chan struct{}
}

// Tail-read bounds for append streams. The conviction ring keeps 2000
// entries (conviction.go maxPerAgent), so reading further back is wasted I/O.
const (
	streamReadMaxLines   = 2000
	streamReadMaxObjects = 200
	streamReadLookbackDays = 7
)

// NewMemoryStore creates a new S3-backed memory store.
func NewMemoryStore(client *s3.Client, bucket string) *MemoryStore {
	m := &MemoryStore{
		client:      client,
		bucket:      bucket,
		flushQueue:  make(map[string][]byte),
		appendQueue: make(map[string][]byte),
		stopCh:      make(chan struct{}),
	}
	m.flushTick = time.NewTicker(30 * time.Second)
	go m.flushLoop()
	return m
}

func (m *MemoryStore) flushLoop() {
	for {
		select {
		case <-m.flushTick.C:
			m.Flush(context.Background())
		case <-m.stopCh:
			m.flushTick.Stop()
			return
		}
	}
}

// Stop terminates the background flush loop, draining buffered writes first
// so up to 30s of queued deltas aren't lost on shutdown.
func (m *MemoryStore) Stop() {
	close(m.stopCh)
	m.Flush(context.Background())
}

// Flush writes all pending data to S3: whole-object writes as-is, and each
// append stream's buffered lines as ONE small delta object (never a rewrite
// of the full journal).
func (m *MemoryStore) Flush(ctx context.Context) {
	m.mu.Lock()
	pending := m.flushQueue
	pendingAppends := m.appendQueue
	m.flushQueue = make(map[string][]byte)
	m.appendQueue = make(map[string][]byte)
	m.mu.Unlock()

	for key, data := range pending {
		if err := m.putObject(ctx, key, data); err != nil {
			log.Printf("[memory] flush error key=%s: %v", key, err)
			m.mu.Lock()
			m.flushQueue[key] = data
			m.mu.Unlock()
		}
	}

	for stream, lines := range pendingAppends {
		key := m.deltaKey(stream, time.Now().UTC())
		if err := m.putObject(ctx, key, lines); err != nil {
			log.Printf("[memory] delta flush error stream=%s: %v", stream, err)
			// Re-queue at the FRONT so order is preserved relative to lines
			// appended while we were flushing.
			m.mu.Lock()
			m.appendQueue[stream] = append(lines, m.appendQueue[stream]...)
			m.mu.Unlock()
		}
	}
}

// deltaKey builds a delta-object key whose lexical order matches
// chronological order: <stream>/dt=YYYY-MM-DD/HHMMSS.mmm-SEQ.jsonl.
func (m *MemoryStore) deltaKey(stream string, now time.Time) string {
	seq := atomic.AddUint64(&m.flushSeq, 1)
	return fmt.Sprintf("%s/dt=%s/%s-%06d.jsonl",
		stream, now.Format("2006-01-02"), now.Format("150405.000"), seq)
}

// enqueueLine buffers one JSONL line on an append stream for the next flush.
func (m *MemoryStore) enqueueLine(stream string, line []byte) {
	m.mu.Lock()
	m.appendQueue[stream] = append(m.appendQueue[stream], line...)
	m.mu.Unlock()
}

// FlushImmediate forces an immediate flush (used during emergencies).
func (m *MemoryStore) FlushImmediate(ctx context.Context) {
	m.Flush(ctx)
}

func (m *MemoryStore) agentPrefix(isp ISP, agentType AgentType) string {
	return fmt.Sprintf("agents/%s/%s", isp, agentType)
}

// ReadState loads an agent's current state from S3.
func (m *MemoryStore) ReadState(ctx context.Context, isp ISP, agentType AgentType) (json.RawMessage, error) {
	key := m.agentPrefix(isp, agentType) + "/state.json"
	return m.getObject(ctx, key)
}

// WriteState persists an agent's current state (batched).
func (m *MemoryStore) WriteState(isp ISP, agentType AgentType, state interface{}) error {
	key := m.agentPrefix(isp, agentType) + "/state.json"
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.flushQueue[key] = data
	m.mu.Unlock()
	return nil
}

// AppendDecision buffers a decision entry; the flush loop writes it as part
// of a small delta object (write-only audit stream, nothing reads it back).
func (m *MemoryStore) AppendDecision(ctx context.Context, isp ISP, agentType AgentType, decision interface{}) error {
	_ = ctx // buffered; no S3 I/O on the hot path
	line, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	m.enqueueLine(m.agentPrefix(isp, agentType)+"/decisions", append(line, '\n'))
	return nil
}

// AppendSignal buffers a signal snapshot; flushed as a delta object.
func (m *MemoryStore) AppendSignal(ctx context.Context, isp ISP, agentType AgentType, signal interface{}) error {
	_ = ctx // buffered; no S3 I/O on the hot path
	line, err := json.Marshal(signal)
	if err != nil {
		return err
	}
	m.enqueueLine(m.agentPrefix(isp, agentType)+"/signals", append(line, '\n'))
	return nil
}

// ReadPatterns loads learned behavior patterns from S3.
func (m *MemoryStore) ReadPatterns(ctx context.Context, isp ISP, agentType AgentType) (json.RawMessage, error) {
	key := m.agentPrefix(isp, agentType) + "/patterns.json"
	return m.getObject(ctx, key)
}

// WritePatterns persists learned behavior patterns (batched).
func (m *MemoryStore) WritePatterns(isp ISP, agentType AgentType, patterns interface{}) error {
	key := m.agentPrefix(isp, agentType) + "/patterns.json"
	data, err := json.Marshal(patterns)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.flushQueue[key] = data
	m.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Conviction Memory — Binary Verdict Storage
// ---------------------------------------------------------------------------

// convictionS3Enabled reports whether conviction memory is persisted to / read
// from S3. DISABLED by default (operator 2026-07-16): convictions are not
// actively used, and their per-append S3 delta stream drove a ~$2.5k/mo GET
// storm — every ReadConvictions fetched up to streamReadMaxObjects (200) tiny
// delta objects (+ a legacy-journal GET) across a 7-day window, multiplied by
// the engine's read loops, and the writes grew the bucket to 47M objects.
// Convictions now live in the in-memory ring ONLY (conviction.go still calls
// ring.append). Set ENGINE_CONVICTION_S3_ENABLED=true to restore S3 I/O.
func convictionS3Enabled() bool {
	return os.Getenv("ENGINE_CONVICTION_S3_ENABLED") == "true"
}

// AppendConviction buffers a single conviction; flushed as a delta object.
// Dead call unless conviction S3 is explicitly re-enabled (see convictionS3Enabled).
func (m *MemoryStore) AppendConviction(ctx context.Context, isp ISP, agentType AgentType, conviction interface{}) error {
	if !convictionS3Enabled() {
		return nil // dead call — no S3 write (operator 2026-07-16)
	}
	_ = ctx // buffered; no S3 I/O on the hot path
	line, err := json.Marshal(conviction)
	if err != nil {
		return err
	}
	m.enqueueLine(m.agentPrefix(isp, agentType)+"/convictions", append(line, '\n'))
	return nil
}

// ReadConvictions loads the recent conviction tail for an agent from S3:
// delta objects from the last few day-prefixes (newest first, bounded), plus
// the legacy single-file journal for pre-cutover history when the deltas
// alone don't fill the window. Returned oldest-first (the ring hydrator
// appends sequentially and keeps the last 2000).
func (m *MemoryStore) ReadConvictions(ctx context.Context, isp ISP, agentType AgentType) ([]Conviction, error) {
	if !convictionS3Enabled() {
		return nil, nil // dead call — no S3 read (operator 2026-07-16); ring hydrates empty
	}
	stream := m.agentPrefix(isp, agentType) + "/convictions"

	lines, err := m.readStreamTail(ctx, stream)
	if err != nil {
		return nil, err
	}

	// Legacy pre-cutover journal (the old full-file key). Only consulted when
	// the delta window is short — its lines are strictly OLDER than any delta.
	if len(lines) < streamReadMaxLines {
		legacy, _ := m.getObject(ctx, stream+".jsonl")
		if len(legacy) > 0 {
			legacyLines := splitJSONL(legacy)
			if keep := streamReadMaxLines - len(lines); len(legacyLines) > keep {
				legacyLines = legacyLines[len(legacyLines)-keep:]
			}
			lines = append(legacyLines, lines...)
		}
	}

	var convictions []Conviction
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var c Conviction
		if err := json.Unmarshal(line, &c); err != nil {
			continue
		}
		convictions = append(convictions, c)
	}
	return convictions, nil
}

// readStreamTail returns the most recent JSONL lines of an append stream by
// walking day prefixes newest-first and fetching delta objects from the tail,
// bounded by streamReadMaxLines / streamReadMaxObjects. Lines are returned
// oldest-first.
func (m *MemoryStore) readStreamTail(ctx context.Context, stream string) ([][]byte, error) {
	if m.client == nil {
		return nil, nil
	}

	// Collect candidate delta keys, newest day first. Keys within a day sort
	// lexically == chronologically by construction (deltaKey).
	var keysNewestFirst []string
	now := time.Now().UTC()
	for d := 0; d < streamReadLookbackDays && len(keysNewestFirst) < streamReadMaxObjects; d++ {
		day := now.AddDate(0, 0, -d).Format("2006-01-02")
		prefix := fmt.Sprintf("%s/dt=%s/", stream, day)
		var dayKeys []string
		p := s3.NewListObjectsV2Paginator(m.client, &s3.ListObjectsV2Input{
			Bucket: aws.String(m.bucket),
			Prefix: aws.String(prefix),
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("list %s: %w", prefix, err)
			}
			for _, obj := range page.Contents {
				if obj.Key != nil {
					dayKeys = append(dayKeys, *obj.Key)
				}
			}
		}
		sort.Strings(dayKeys)
		// Newest first within the day, appended after newer days.
		for i := len(dayKeys) - 1; i >= 0 && len(keysNewestFirst) < streamReadMaxObjects; i-- {
			keysNewestFirst = append(keysNewestFirst, dayKeys[i])
		}
	}

	// Fetch newest-first until the line budget is met, then restore
	// chronological (oldest-first) order.
	var chunksNewestFirst [][][]byte
	total := 0
	for _, key := range keysNewestFirst {
		if total >= streamReadMaxLines {
			break
		}
		data, err := m.getObject(ctx, key)
		if err != nil || len(data) == 0 {
			continue
		}
		chunk := splitJSONL(data)
		chunksNewestFirst = append(chunksNewestFirst, chunk)
		total += len(chunk)
	}
	var lines [][]byte
	for i := len(chunksNewestFirst) - 1; i >= 0; i-- {
		lines = append(lines, chunksNewestFirst[i]...)
	}
	return lines, nil
}

func splitJSONL(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// ReadIncidents loads emergency incident history from S3.
func (m *MemoryStore) ReadIncidents(ctx context.Context, isp ISP) (json.RawMessage, error) {
	key := m.agentPrefix(isp, AgentEmergency) + "/incidents.json"
	return m.getObject(ctx, key)
}

// AppendIncident appends an incident report.
func (m *MemoryStore) AppendIncident(ctx context.Context, isp ISP, incident interface{}) error {
	key := m.agentPrefix(isp, AgentEmergency) + "/incidents.json"
	existing, _ := m.getObject(ctx, key)
	var incidents []json.RawMessage
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &incidents)
	}
	newEntry, err := json.Marshal(incident)
	if err != nil {
		return err
	}
	incidents = append(incidents, newEntry)
	data, err := json.Marshal(incidents)
	if err != nil {
		return err
	}
	return m.putObject(ctx, key, data)
}

// WriteGlobalState persists global orchestrator state.
func (m *MemoryStore) WriteGlobalState(state interface{}) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.flushQueue["global/orchestrator/state.json"] = data
	m.mu.Unlock()
	return nil
}

// ReadGlobalState loads global orchestrator state.
func (m *MemoryStore) ReadGlobalState(ctx context.Context) (json.RawMessage, error) {
	return m.getObject(ctx, "global/orchestrator/state.json")
}

func (m *MemoryStore) getObject(ctx context.Context, key string) (json.RawMessage, error) {
	if m.client == nil {
		return nil, nil
	}
	out, err := m.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(m.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, nil // Treat missing objects as empty
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (m *MemoryStore) putObject(ctx context.Context, key string, data []byte) error {
	if m.client == nil {
		return nil
	}
	_, err := m.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(m.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

// NOTE: the old appendObject (GET full file + concat + PUT full file on every
// append) is intentionally gone — it was the root cause of the 2026-06 NAT /
// S3-versioning cost incident. Append streams go through enqueueLine + delta
// flush instead. AppendIncident below keeps its read-modify-write because
// incidents are rare (a handful per month) and consumers read one JSON array.
