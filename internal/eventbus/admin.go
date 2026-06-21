package eventbus

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// TopicSpec describes a topic to ensure-exists. ReplicationFactor -1 means "use
// the cluster default" (required for MSK Serverless, which manages replication).
// Configs may carry e.g. cleanup.policy — note MSK Serverless does NOT support
// compaction, so pass delete-policy there.
type TopicSpec struct {
	Name              string
	Partitions        int32
	ReplicationFactor int16
	Configs           map[string]*string
}

// EnsureTopics creates the given topics idempotently using the Kafka protocol
// CreateTopics request (via kmsg — no extra module dependency), authenticating
// exactly like the producer/consumer (authOpts). Topics that already exist are
// treated as success. This exists because MSK Serverless does not reliably honor
// client-side producer auto-topic-creation — the app, the only component with
// in-VPC access to the private brokers, creates them explicitly on boot when
// KAFKA_ALLOW_AUTO_TOPICS is set.
func EnsureTopics(ctx context.Context, cfg Config, specs []TopicSpec) error {
	if !cfg.Enabled() || len(specs) == 0 {
		return nil
	}
	opts := []kgo.Opt{kgo.SeedBrokers(cfg.Brokers...), kgo.ClientID(cfg.ClientID + "-admin")}
	auth, err := authOpts(cfg)
	if err != nil {
		return err
	}
	opts = append(opts, auth...)
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return fmt.Errorf("eventbus: admin client: %w", err)
	}
	defer cl.Close()

	req := kmsg.NewPtrCreateTopicsRequest()
	req.TimeoutMillis = 15000
	for _, s := range specs {
		rf := s.ReplicationFactor
		if rf == 0 {
			rf = -1 // cluster default (MSK Serverless requirement)
		}
		t := kmsg.NewCreateTopicsRequestTopic()
		t.Topic = s.Name
		t.NumPartitions = s.Partitions
		t.ReplicationFactor = rf
		for k, v := range s.Configs {
			c := kmsg.NewCreateTopicsRequestTopicConfig()
			c.Name = k
			c.Value = v
			t.Configs = append(t.Configs, c)
		}
		req.Topics = append(req.Topics, t)
	}

	resp, err := req.RequestWith(ctx, cl)
	if err != nil {
		return fmt.Errorf("eventbus: CreateTopics request: %w", err)
	}

	var firstErr error
	for _, ct := range resp.Topics {
		cerr := kerr.ErrorForCode(ct.ErrorCode)
		if cerr != nil && !errors.Is(cerr, kerr.TopicAlreadyExists) {
			if firstErr == nil {
				firstErr = fmt.Errorf("create %s: %w", ct.Topic, cerr)
			}
			log.Printf("[eventbus] ensure-topic %s failed: %v", ct.Topic, cerr)
			continue
		}
		log.Printf("[eventbus] ensure-topic %s ok", ct.Topic)
	}
	return firstErr
}
