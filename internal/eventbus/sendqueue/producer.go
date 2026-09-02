package sendqueue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// EnqueueSendCommands produces one Kafka record per command onto
// TopicSendCommands, KEYED by the command's IdempotencyKey bytes. Keying by the
// idempotency key is load-bearing in two ways:
//
//   - Per-key ordering: all records sharing a key route to the same partition,
//     so a key's claim/redelivery ordering is preserved.
//   - Duplicate co-location: any duplicate of a key (a redrive, a retry, a
//     double-enqueue) lands on the SAME partition and is therefore handled by a
//     single consumer instance — combined with the ledger claim, that is the
//     belt-and-suspenders that keeps at most one send per key.
//
// It produces through the eventbus.Producer seam (so it is broker-free testable
// with eventbus.FakeProducer). The producer is the durable, idempotent
// franz-go producer — NOT the fire-and-forget Tap: send commands are work that
// must not be silently dropped on a full buffer, so this is a synchronous,
// error-returning produce, unlike the lossy hot-path Tap.
//
// On the first produce error it returns immediately. Re-running with the same
// commands is safe: the consumer dedups on the ledger, so a partial enqueue that
// is retried in full never double-sends.
func EnqueueSendCommands(ctx context.Context, prod eventbus.Producer, cmds []SendCommand) error {
	if prod == nil {
		return fmt.Errorf("sendqueue: EnqueueSendCommands called with nil producer")
	}
	for i := range cmds {
		cmd := cmds[i]
		if cmd.IdempotencyKey == uuid.Nil {
			return fmt.Errorf("sendqueue: command %d has a zero idempotency_key", i)
		}
		value, err := json.Marshal(cmd)
		if err != nil {
			return fmt.Errorf("sendqueue: marshal command %d (key=%s): %w", i, cmd.IdempotencyKey, err)
		}
		// Key = idempotency key bytes: pins partition for ordering + duplicate
		// co-location.
		key := make([]byte, len(cmd.IdempotencyKey))
		copy(key, cmd.IdempotencyKey[:])
		if err := prod.Produce(ctx, TopicSendCommands, key, value); err != nil {
			return fmt.Errorf("sendqueue: produce command %d (key=%s): %w", i, cmd.IdempotencyKey, err)
		}
	}
	return nil
}

// batchProduceChunk bounds one ProduceBatch call so a very large wave cannot
// hand franz-go more records than its default buffer wants in flight at once.
const batchProduceChunk = 1000

// EnqueueSendCommandsBatch is EnqueueSendCommands for a WHOLE wave: same topic,
// same idempotency-key keying, same marshalling — but when the producer
// implements eventbus.BatchProducer it issues ONE ProduceSync per chunk instead
// of one per command. A 45k-recipient wave is 45 broker round trips instead of
// 45,000 (REQ-089 DoD 4).
//
// Producers without the batch extension (eventbus.FakeProducer in unit tests)
// fall through to EnqueueSendCommands, so behaviour and per-record test
// assertions are unchanged.
func EnqueueSendCommandsBatch(ctx context.Context, prod eventbus.Producer, cmds []SendCommand) error {
	if prod == nil {
		return fmt.Errorf("sendqueue: EnqueueSendCommandsBatch called with nil producer")
	}
	bp, ok := prod.(eventbus.BatchProducer)
	if !ok {
		return EnqueueSendCommands(ctx, prod, cmds)
	}
	for start := 0; start < len(cmds); start += batchProduceChunk {
		end := start + batchProduceChunk
		if end > len(cmds) {
			end = len(cmds)
		}
		keys := make([][]byte, 0, end-start)
		values := make([][]byte, 0, end-start)
		for i := start; i < end; i++ {
			cmd := cmds[i]
			if cmd.IdempotencyKey == uuid.Nil {
				return fmt.Errorf("sendqueue: command %d has a zero idempotency_key", i)
			}
			value, err := json.Marshal(cmd)
			if err != nil {
				return fmt.Errorf("sendqueue: marshal command %d (key=%s): %w", i, cmd.IdempotencyKey, err)
			}
			key := make([]byte, len(cmd.IdempotencyKey))
			copy(key, cmd.IdempotencyKey[:])
			keys = append(keys, key)
			values = append(values, value)
		}
		if err := bp.ProduceBatch(ctx, TopicSendCommands, keys, values); err != nil {
			return fmt.Errorf("sendqueue: produce batch [%d,%d): %w", start, end, err)
		}
	}
	return nil
}
