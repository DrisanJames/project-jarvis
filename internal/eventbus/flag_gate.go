package eventbus

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// FlagGate is the runtime kill-switch primitive for the event backbone. It
// resolves a single boolean flag from TWO sources, re-checking on an interval so
// a flow can be flipped OFF (or ON) across all tasks WITHOUT a redeploy:
//
//  1. an environment variable (KAFKA_FLAG_<NAME>, also a friendly fallback) —
//     the deploy-time default; and
//  2. an optional Redis key "kafka:flag:<name>" — the runtime override that the
//     plan's per-flow kill-switches use (polled ~15s, flips in <30s fleet-wide).
//
// Precedence: if the Redis key is PRESENT, it wins (runtime override of the
// deploy default). If Redis is absent/unset/errors, the env value is used.
// Default is FALSE (OFF) when neither source provides a value — the
// dark-by-default safety: an unset flag means the flow stays on its legacy path.
//
// FlagGate tolerates a nil *redis.Client (env-only mode), so it is fully
// unit-testable without Redis or Kafka.
type FlagGate struct {
	name     string
	envKey   string
	redisKey string
	rdb      *redis.Client
	interval time.Duration

	// envSource optionally REPLACES the plain os.LookupEnv(envKey) layer. It is
	// used by the "send_route" gate (REQ-090), whose deploy-time default is not
	// one variable but the send-routing predicate (KAFKA_SEND_QUEUE_ALL / _WAVES
	// / _CAMPAIGNS — sendqueue.SendRouteEnvConfigured). It returns (value,
	// present); present=false falls through to the plain env key, then to false.
	// nil (every other gate) = today's behaviour exactly.
	envSource func() (bool, bool)

	// value holds the current resolved flag as an atomic bool (0/1).
	value atomic.Bool

	stop chan struct{}
	done chan struct{}
}

// NewFlagGate builds a gate for the named flag. name is a short identifier such
// as "produce_lake"; the env var checked is KAFKA_FLAG_PRODUCE_LAKE and the
// Redis key is "kafka:flag:produce_lake". rdb may be nil (env-only). interval
// <= 0 uses DefaultFlagPollInterval. The gate evaluates its sources once
// synchronously before returning, so Enabled() is correct immediately; call
// Start to begin background re-checks.
func NewFlagGate(name string, rdb *redis.Client, interval time.Duration) *FlagGate {
	if interval <= 0 {
		interval = DefaultFlagPollInterval
	}
	g := &FlagGate{
		name:     name,
		envKey:   "KAFKA_FLAG_" + strings.ToUpper(name),
		redisKey: "kafka:flag:" + name,
		rdb:      rdb,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	g.value.Store(g.evaluate(context.Background()))
	return g
}

// WithEnvSource replaces the gate's ENV layer with fn, which returns (value,
// present). Redis still wins when its key is present, so the kill-switch
// contract is unchanged: `SET kafka:flag:<name> 0` beats whatever fn says.
// Returns the gate for chaining; it re-evaluates immediately so Enabled() is
// correct before Start(). Passing nil restores the plain env-key layer.
//
// The one caller is the send_route gate (cmd/server/eventbus_wiring.go): its
// deploy-time default is the send-routing env predicate rather than a single
// KAFKA_FLAG_SEND_ROUTE variable, so that "no Redis key" means "env decides,
// exactly as before this gate existed".
func (g *FlagGate) WithEnvSource(fn func() (bool, bool)) *FlagGate {
	g.envSource = fn
	g.value.Store(g.evaluate(context.Background()))
	return g
}

// Enabled returns the current resolved flag. Cheap (atomic load); safe for the
// hot path. Default FALSE when unset.
func (g *FlagGate) Enabled() bool { return g.value.Load() }

// Start launches the background re-check loop. Idempotent only in the sense that
// it should be called once; call Stop to terminate it. Safe to skip entirely in
// tests that only want the synchronous initial evaluation.
func (g *FlagGate) Start() {
	go g.run()
}

// Refresh forces a synchronous re-evaluation of the sources and updates the
// cached value. Exposed for tests and for an on-demand /health probe.
func (g *FlagGate) Refresh(ctx context.Context) bool {
	v := g.evaluate(ctx)
	g.value.Store(v)
	return v
}

func (g *FlagGate) run() {
	defer close(g.done)
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			g.value.Store(g.evaluate(ctx))
			cancel()
		}
	}
}

// evaluate resolves the flag from Redis (override, if present) then env
// (default). Returns false when neither yields a value.
func (g *FlagGate) evaluate(ctx context.Context) bool {
	// Redis override wins when the key exists.
	if g.rdb != nil {
		v, err := g.rdb.Get(ctx, g.redisKey).Result()
		if err == nil {
			// key present: this is the authoritative runtime override
			return truthy(v)
		}
		// err == redis.Nil (key absent) or a transient error => fall through to
		// env. We do NOT treat a Redis outage as "flip on"; the env default (or
		// false) governs, keeping the safe legacy path.
	}
	// Env layer: the custom source when one is installed (send_route), else the
	// plain KAFKA_FLAG_<NAME> variable.
	if g.envSource != nil {
		if v, present := g.envSource(); present {
			return v
		}
	}
	if val, ok := os.LookupEnv(g.envKey); ok {
		return truthy(val)
	}
	return false
}

// Name returns the flag's short identifier ("produce_lake", "send_route"), for
// /health listings and log lines.
func (g *FlagGate) Name() string { return g.name }

// Stop terminates the background loop (bounded wait). Safe to call once.
func (g *FlagGate) Stop() {
	select {
	case <-g.stop:
		// already stopped
		return
	default:
	}
	close(g.stop)
	select {
	case <-g.done:
	case <-time.After(2 * time.Second):
	}
}
