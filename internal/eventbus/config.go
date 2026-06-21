// Package eventbus is the Kafka client foundation for the event backbone
// (see docs/KAFKA_EVENT_BACKBONE_PLAN.md).
//
// Design contract (read before wiring any call site):
//
//   - SHIPS DARK. This package is NOT wired into cmd/ or any existing file. It
//     compiles and unit-tests on its own, but the app does nothing with it until
//     a flag is explicitly flipped. There is no global init and no side effects
//     at import time.
//
//   - DISABLED BY DEFAULT, exactly like internal/analytics.lake_emitter: a
//     Config with no KAFKA_BROKERS is Enabled()==false. Producers/consumers are
//     only constructed from an explicit broker list, so a missing/empty env
//     leaves everything a cheap no-op.
//
//   - NEVER blocks the hot path. The producer-side seam the app will use is the
//     Tap (see tap.go): a fire-and-forget, bounded, drop-and-count buffer that
//     mirrors lake_emitter.go's posture (channel depth 8192, batch/flush,
//     atomic counters). Kafka can never add latency or backpressure to
//     ingestion. If the broker is down or the buffer is full, records are
//     dropped and counted — never blocked on.
//
//   - At-least-once on the consumer side (see consumer.go). Idempotency lives at
//     the sink (the existing lake ON CONFLICT and hub.Suppress() idempotency),
//     not here. Offsets are committed only AFTER a record is handled
//     successfully, so a crash re-delivers rather than loses.
//
//   - Everything is behind interfaces (Producer, DLQ) so the whole package is
//     unit-testable WITHOUT a running Kafka broker. The franz-go client is only
//     constructed by NewKgoProducer / NewConsumer, which take a broker list;
//     tests use FakeProducer and in-memory handlers.
package eventbus

import (
	"os"
	"strings"
	"time"
)

// Config is loaded from the environment. It deliberately mirrors the
// lake-emitter disabled-by-default pattern: with no KAFKA_BROKERS set,
// Enabled() is false and nothing is constructed.
type Config struct {
	// Brokers is the bootstrap broker list. Empty => disabled.
	Brokers []string

	// ClientID identifies this client to the broker (observability/ACLs).
	ClientID string

	// SASLMechanism selects the auth hook. The production target is AWS MSK
	// IAM-SASL via the ECS task role (no static secrets). This is left as a
	// config option and NOT hardcoded: the kgo producer wires the IAM hook only
	// when SASLMechanism=="aws_msk_iam". Empty => no SASL (plaintext/dev).
	//
	// To enable IAM auth in production, set KAFKA_SASL_MECHANISM=aws_msk_iam and
	// supply the franz-go aws.ManagedStreamingIAM SASL option in NewKgoProducer
	// (see the documented hook there). Never put credentials in Config.
	SASLMechanism string

	// TLS enables TLS in transit. MSK requires this with IAM-SASL.
	TLS bool

	// AllowAutoTopics lets the producer create a topic on first publish
	// (KAFKA_ALLOW_AUTO_TOPICS). Useful for bootstrapping/test against MSK
	// Serverless where pre-creating topics from outside the VPC is awkward;
	// production pre-creates topics explicitly and leaves this off.
	AllowAutoTopics bool

	// Group is the consumer group id (consumer side only).
	Group string

	// Topics is the list of topics a consumer subscribes to.
	Topics []string

	// FlagPollInterval is how often a FlagGate re-checks its sources. 0 => 15s.
	FlagPollInterval time.Duration
}

// Environment variable names. Kept as exported constants so wiring code and
// tests agree on spelling.
const (
	EnvBrokers          = "KAFKA_BROKERS"
	EnvClientID         = "KAFKA_CLIENT_ID"
	EnvSASLMechanism    = "KAFKA_SASL_MECHANISM"
	EnvTLS              = "KAFKA_TLS"
	EnvAllowAutoTopics  = "KAFKA_ALLOW_AUTO_TOPICS"
	EnvGroup            = "KAFKA_GROUP"
	EnvTopics           = "KAFKA_TOPICS"
	EnvFlagPollInterval = "KAFKA_FLAG_POLL_INTERVAL"
)

// DefaultFlagPollInterval is the cadence at which a FlagGate re-reads its env
// var and (optional) Redis key, so a kill-switch can flip at runtime without a
// redeploy.
const DefaultFlagPollInterval = 15 * time.Second

// LoadConfig reads Config from the environment. It performs no I/O and never
// fails: an absent KAFKA_BROKERS simply yields a disabled Config.
func LoadConfig() Config {
	c := Config{
		Brokers:       splitlist(os.Getenv(EnvBrokers)),
		ClientID:      os.Getenv(EnvClientID),
		SASLMechanism: strings.TrimSpace(os.Getenv(EnvSASLMechanism)),
		TLS:             truthy(os.Getenv(EnvTLS)),
		AllowAutoTopics: truthy(os.Getenv(EnvAllowAutoTopics)),
		Group:           strings.TrimSpace(os.Getenv(EnvGroup)),
		Topics:        splitlist(os.Getenv(EnvTopics)),
	}
	if c.ClientID == "" {
		c.ClientID = "ignite-eventbus"
	}
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(EnvFlagPollInterval))); err == nil && d > 0 {
		c.FlagPollInterval = d
	}
	return c
}

// Enabled reports whether a broker list is configured. When false, callers must
// treat the bus as off and fall back to their legacy path. This is the same
// disabled-by-default safety the lake emitter relies on.
func (c Config) Enabled() bool { return len(c.Brokers) > 0 }

// pollInterval returns the configured flag poll interval, or the default.
func (c Config) pollInterval() time.Duration {
	if c.FlagPollInterval > 0 {
		return c.FlagPollInterval
	}
	return DefaultFlagPollInterval
}

// splitlist splits a comma/space separated env value into a trimmed,
// empty-filtered slice. An empty input yields a nil slice (not a [""] slice),
// so Enabled() is correctly false.
func splitlist(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// truthy interprets common boolean env spellings.
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "y", "t":
		return true
	default:
		return false
	}
}
