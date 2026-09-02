package eventbus

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// REQ-090: the send-path routing kill switch is a FlagGate named "send_route",
// so flipping it is ONE Redis SET — no task-def revision, no ECS roll, and the
// resolved value is visible on /health.flags.send_route.
//
// Its env layer is not a single KAFKA_FLAG_SEND_ROUTE variable but the
// send-routing predicate (WithEnvSource), so "no Redis key" resolves to exactly
// what the KAFKA_SEND_QUEUE_* envs already say. These tests pin the precedence
// in BOTH directions, because a kill switch that cannot be proven to fire is
// worse than none (platform-work Part 4 item 5).

func newSendRouteTestGate(t *testing.T, rdb *redis.Client, envRoutes bool) *FlagGate {
	t.Helper()
	// A long interval + no Start(): these tests drive evaluation explicitly via
	// the constructor and Refresh, so there is no background goroutine to stop.
	return NewFlagGate("send_route", rdb, time.Hour).
		WithEnvSource(func() (bool, bool) { return envRoutes, true })
}

// Redis key ABSENT → the env layer decides (identical to pre-REQ-090 behaviour).
func TestFlagGate_SendRoute_EnvDecidesWhenRedisAbsent(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	if g := newSendRouteTestGate(t, rdb, true); !g.Enabled() {
		t.Error("no Redis key + env routes ON => gate must be OPEN")
	}
	if g := newSendRouteTestGate(t, rdb, false); g.Enabled() {
		t.Error("no Redis key + env routes OFF => gate must be CLOSED")
	}
}

// THE KILL SWITCH: `SET kafka:flag:send_route 0` beats an env that routes.
func TestFlagGate_SendRoute_RedisZeroWinsOverEnv(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	g := newSendRouteTestGate(t, rdb, true) // env says routing is ON
	if !g.Enabled() {
		t.Fatal("precondition: env-ON gate must start OPEN")
	}

	if err := mr.Set("kafka:flag:send_route", "0"); err != nil {
		t.Fatal(err)
	}
	if g.Refresh(context.Background()) {
		t.Fatal("Redis kafka:flag:send_route=0 must WIN over an env that routes")
	}
	if g.Enabled() {
		t.Error("the refreshed value must be visible to Enabled() (what /health and the guards read)")
	}

	// Deleting the key hands control back to env — no redeploy either way.
	mr.Del("kafka:flag:send_route")
	if !g.Refresh(context.Background()) {
		t.Error("removing the Redis key must restore the env decision")
	}
}

// A Redis "1" does not fabricate routing on its own: the routing predicate ANDs
// the gate with the env, so this gate reporting OPEN with an env that routes
// nothing still routes nothing (proven in sendqueue's
// TestRouteGate_KillSwitchCannotRouteWithoutEnv). Here we only pin that the gate
// itself reports the override.
func TestFlagGate_SendRoute_RedisOneOverridesEnvOff(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	g := newSendRouteTestGate(t, rdb, false)
	if err := mr.Set("kafka:flag:send_route", "1"); err != nil {
		t.Fatal(err)
	}
	if !g.Refresh(context.Background()) {
		t.Error("a present Redis key is the authoritative override in both directions")
	}
}

// Redis DOWN must not flip anything on or off: the env default governs.
func TestFlagGate_SendRoute_RedisOutageFallsBackToEnv(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	// Short dial timeout + no retries: an outage must resolve fast, not stall the
	// gate for seconds on every evaluation.
	rdb := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer rdb.Close()
	mr.Close() // the outage

	if g := newSendRouteTestGate(t, rdb, true); !g.Enabled() {
		t.Error("a Redis outage must fall back to the env value, not close the gate")
	}
	if g := newSendRouteTestGate(t, rdb, false); g.Enabled() {
		t.Error("a Redis outage must fall back to the env value, not open the gate")
	}
}

// Regression guard for the OTHER gates: WithEnvSource is opt-in, so
// produce_lake / produce_ingest / produce_suppress still read KAFKA_FLAG_<NAME>.
func TestFlagGate_PlainEnvKeyUnchanged(t *testing.T) {
	t.Setenv("KAFKA_FLAG_PRODUCE_LAKE", "1")
	g := NewFlagGate("produce_lake", nil, time.Hour) // never Start()ed: nothing to stop
	if !g.Enabled() {
		t.Error("KAFKA_FLAG_PRODUCE_LAKE=1 must still open the produce_lake gate")
	}
	if g.Name() != "produce_lake" {
		t.Errorf("Name() = %q", g.Name())
	}
}
