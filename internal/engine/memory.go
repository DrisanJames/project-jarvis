package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// MemoryStore provides S3-backed long-term memory for agents.
// Each agent instance has its own S3 key namespace under
// agents/{isp}/{agentType}/.
type MemoryStore struct {
	client     *s3.Client
	bucket     string
	mu         sync.Mutex
	flushQueue map[string][]byte
	flushTick  *time.Ticker
	stopCh     chan struct{}
}

// NewMemoryStore creates a new S3-backed memory store.
func NewMemoryStore(client *s3.Client, bucket string) *MemoryStore {
	m := &MemoryStore{
		client:     client,
		bucket:     bucket,
		flushQueue: make(map[string][]byte),
		stopCh:     make(chan struct{}),
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

// Stop terminates the background flush loop.
func (m *MemoryStore) Stop() {
	close(m.stopCh)
}

// Flush writes all pending data to S3.
func (m *MemoryStore) Flush(ctx context.Context) {
	m.mu.Lock()
	pending := m.flushQueue
	m.flushQueue = make(map[string][]byte)
	m.mu.Unlock()

	for key, data := range pending {
		if err := m.putObject(ctx, key, data); err != nil {
			log.Printf("[memory] flush error key=%s: %v", key, err)
			m.mu.Lock()
			m.flushQueue[key] = data
			m.mu.Unlock()
		}
	}
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

// AppendDecision appends a decision entry to the JSONL log.
func (m *MemoryStore) AppendDecision(ctx context.Context, isp ISP, agentType AgentType, decision interface{}) error {
	key := m.agentPrefix(isp, agentType) + "/decisions.jsonl"
	line, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	return m.appendObject(ctx, key, line)
}

// AppendSignal appends a signal snapshot to the JSONL log.
func (m *MemoryStore) AppendSignal(ctx context.Context, isp ISP, agentType AgentType, signal interface{}) error {
	key := m.agentPrefix(isp, agentType) + "/signals.jsonl"
	line, err := json.Marshal(signal)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	return m.appendObject(ctx, key, line)
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

// AppendConviction appends a single conviction to the agent's JSONL log in S3.
func (m *MemoryStore) AppendConviction(ctx context.Context, isp ISP, agentType AgentType, conviction interface{}) error {
	key := m.agentPrefix(isp, agentType) + "/convictions.jsonl"
	line, err := json.Marshal(conviction)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	return m.appendObject(ctx, key, line)
}

// ReadConvictions loads all convictions for an agent from S3.
func (m *MemoryStore) ReadConvictions(ctx context.Context, isp ISP, agentType AgentType) ([]Conviction, error) {
	key := m.agentPrefix(isp, agentType) + "/convictions.jsonl"
	data, err := m.getObject(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	var convictions []Conviction
	for _, line := range splitJSONL(data) {
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

func (m *MemoryStore) appendObject(ctx context.Context, key string, data []byte) error {
	if m.client == nil {
		return nil
	}
	existing, _ := m.getObject(ctx, key)
	combined := append(existing, data...)
	return m.putObject(ctx, key, combined)
}
