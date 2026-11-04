package engine

import (
	"testing"
	"time"
)

func newTestPoolAgent(cs *ConvictionStore) *PoolAgent {
	return NewPoolAgent(AgentID{ISP: ISPYahoo, AgentType: AgentPool},
		ISPConfig{PoolName: "yahoo-pool"}, nil, cs, nil)
}

func poolSnap(ip string, score float64) SignalSnapshot {
	return SignalSnapshot{ISP: ISPYahoo, IPMetrics: map[string]IPMetric{
		ip: {IP: ip, Score: score, Sent1h: 500},
	}}
}

func countWont(cs *ConvictionStore, at AgentType) int {
	return len(cs.RecallByVerdict(ISPYahoo, at, VerdictWont))
}

func quarantineDecisions(ds []Decision) int {
	n := 0
	for _, d := range ds {
		if d.ActionTaken == "quarantine_ip" {
			n++
		}
	}
	return n
}

func TestPoolConvictionDedup_TwoTicksOneRecord(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestPoolAgent(cs)

	d1 := a.Evaluate(poolSnap("1.2.3.4", 30))
	d2 := a.Evaluate(poolSnap("1.2.3.4", 30))

	if got := countWont(cs, AgentPool); got != 1 {
		t.Fatalf("WONT convictions after 2 below-threshold ticks = %d, want 1", got)
	}
	if quarantineDecisions(d1) != 1 || quarantineDecisions(d2) != 1 {
		t.Fatalf("quarantine decision must still be emitted every tick: tick1=%d tick2=%d",
			quarantineDecisions(d1), quarantineDecisions(d2))
	}
}

func TestPoolConvictionDedup_RestoreThenDropRecordsAgain(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestPoolAgent(cs)

	a.Evaluate(poolSnap("1.2.3.4", 30)) // drop -> record
	a.Evaluate(poolSnap("1.2.3.4", 30)) // still down -> suppressed
	a.Evaluate(poolSnap("1.2.3.4", 75)) // restored (no WONT, no WILL: 70<=s<85)
	a.Evaluate(poolSnap("1.2.3.4", 30)) // drops again -> new record

	if got := countWont(cs, AgentPool); got != 2 {
		t.Fatalf("WONT convictions after drop/restore/drop = %d, want 2", got)
	}
}

func TestPoolConvictionDedup_KillSwitchRecordsEveryTick(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "1")
	cs := NewConvictionStore(nil)
	a := newTestPoolAgent(cs)

	for i := 0; i < 3; i++ {
		a.Evaluate(poolSnap("1.2.3.4", 30))
	}
	if got := countWont(cs, AgentPool); got != 3 {
		t.Fatalf("kill switch: WONT convictions after 3 ticks = %d, want 3", got)
	}
}

func TestPoolConvictionDedup_PerIP(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestPoolAgent(cs)
	snap := SignalSnapshot{ISP: ISPYahoo, IPMetrics: map[string]IPMetric{
		"1.1.1.1": {Score: 10, Sent1h: 100},
		"2.2.2.2": {Score: 20, Sent1h: 100},
	}}
	a.Evaluate(snap)
	a.Evaluate(snap)
	if got := countWont(cs, AgentPool); got != 2 {
		t.Fatalf("two distinct IPs over 2 ticks = %d WONT, want 2", got)
	}
}

func TestWontQuarantineGate_ReemitsAfterInterval(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	var g wontQuarantineGate
	t0 := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if !g.allow("ip", t0) {
		t.Fatal("first allow must be true")
	}
	if g.allow("ip", t0.Add(59*time.Minute)) {
		t.Fatal("allow inside the interval must be false")
	}
	if !g.allow("ip", t0.Add(wontQuarantineReemitInterval)) {
		t.Fatal("allow at the interval must be true (hourly re-emit)")
	}
}

func countStmt(cs *ConvictionStore, at AgentType, prefix string) int {
	n := 0
	for _, c := range cs.RecallByVerdict(ISPYahoo, at, VerdictWont) {
		if len(c.Statement) >= len(prefix) && c.Statement[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

const reduceStmt = "I WONT allow full volume"

func TestPoolReduceConvictionDedup_TwoTicksOneRecord(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestPoolAgent(cs)
	d1 := a.Evaluate(poolSnap("1.2.3.4", 60))
	d2 := a.Evaluate(poolSnap("1.2.3.4", 60))
	if got := countStmt(cs, AgentPool, reduceStmt); got != 1 {
		t.Fatalf("reduce WONT after 2 ticks = %d, want 1", got)
	}
	if len(d1) != 1 || len(d2) != 1 || d2[0].ActionTaken != "reduce_ip_volume" {
		t.Fatalf("reduce decision must be emitted every tick: %v %v", d1, d2)
	}
}

func TestPoolReduceConvictionDedup_RestoreThenDropRecordsAgain(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestPoolAgent(cs)
	a.Evaluate(poolSnap("1.2.3.4", 60))
	a.Evaluate(poolSnap("1.2.3.4", 60))
	a.Evaluate(poolSnap("1.2.3.4", 75)) // recovered
	a.Evaluate(poolSnap("1.2.3.4", 60))
	if got := countStmt(cs, AgentPool, reduceStmt); got != 2 {
		t.Fatalf("reduce WONT after drop/restore/drop = %d, want 2", got)
	}
}

func TestPoolReduceConvictionDedup_KillSwitchRecordsEveryTick(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "1")
	cs := NewConvictionStore(nil)
	a := newTestPoolAgent(cs)
	for i := 0; i < 3; i++ {
		a.Evaluate(poolSnap("1.2.3.4", 60))
	}
	if got := countStmt(cs, AgentPool, reduceStmt); got != 3 {
		t.Fatalf("kill switch: reduce WONT after 3 ticks = %d, want 3", got)
	}
}

func TestPoolConvictionDedup_KindsDoNotMaskEachOther(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestPoolAgent(cs)
	a.Evaluate(poolSnap("1.2.3.4", 60)) // reduce
	a.Evaluate(poolSnap("1.2.3.4", 30)) // quarantine
	a.Evaluate(poolSnap("1.2.3.4", 60)) // back to reduce: left the reduce state, so new record
	if got := countStmt(cs, AgentPool, "I WONT keep IP"); got != 1 {
		t.Fatalf("quarantine WONT = %d, want 1", got)
	}
	if got := countStmt(cs, AgentPool, reduceStmt); got != 2 {
		t.Fatalf("reduce WONT = %d, want 2", got)
	}
}
