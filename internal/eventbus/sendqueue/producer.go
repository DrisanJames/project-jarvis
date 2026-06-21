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
