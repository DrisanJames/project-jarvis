package engine

import "testing"

func newTestReputationAgent(cs *ConvictionStore) *ReputationAgent {
	return NewReputationAgent(AgentID{ISP: ISPYahoo, AgentType: AgentReputation},
		ISPConfig{
			PoolName:           "yahoo-pool",
			BounceWarnPct:      90,
			BounceActionPct:    95,
			ComplaintWarnPct:   0.05,
			ComplaintActionPct: 0.1,
		}, nil, cs, nil)
}

func complaintSnap(ip string, complaintRate float64) SignalSnapshot {
	return SignalSnapshot{ISP: ISPYahoo, IPMetrics: map[string]IPMetric{
		ip: {IP: ip, ComplaintRate: complaintRate, Score: 60, Sent1h: 500},
	}}
}

func TestReputationConvictionDedup_TwoTicksOneRecord(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestReputationAgent(cs)

	d1 := a.Evaluate(complaintSnap("1.2.3.4", 0.5))
	d2 := a.Evaluate(complaintSnap("1.2.3.4", 0.5))

	if got := countWont(cs, AgentReputation); got != 1 {
		t.Fatalf("WONT convictions after 2 over-threshold ticks = %d, want 1", got)
	}
	if quarantineDecisions(d1) != 1 || quarantineDecisions(d2) != 1 {
		t.Fatalf("quarantine decision must still be emitted every tick: tick1=%d tick2=%d",
			quarantineDecisions(d1), quarantineDecisions(d2))
	}
}

func TestReputationConvictionDedup_RestoreThenDropRecordsAgain(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestReputationAgent(cs)

	a.Evaluate(complaintSnap("1.2.3.4", 0.5))
	a.Evaluate(complaintSnap("1.2.3.4", 0.5))
	a.Evaluate(complaintSnap("1.2.3.4", 0.0)) // restored
	a.Evaluate(complaintSnap("1.2.3.4", 0.5))

	if got := countWont(cs, AgentReputation); got != 2 {
		t.Fatalf("WONT convictions after drop/restore/drop = %d, want 2", got)
	}
}

func TestReputationConvictionDedup_KillSwitchRecordsEveryTick(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "1")
	cs := NewConvictionStore(nil)
	a := newTestReputationAgent(cs)

	for i := 0; i < 3; i++ {
		a.Evaluate(complaintSnap("1.2.3.4", 0.5))
	}
	if got := countWont(cs, AgentReputation); got != 3 {
		t.Fatalf("kill switch: WONT convictions after 3 ticks = %d, want 3", got)
	}
}

const bounceStmt = "I WONT allow IP"

func bounceSnap(ip string, bounceRate float64) SignalSnapshot {
	return SignalSnapshot{ISP: ISPYahoo, IPMetrics: map[string]IPMetric{
		ip: {IP: ip, BounceRate1h: bounceRate, Score: 60, Sent1h: 500},
	}}
}

func TestReputationBounceConvictionDedup_TwoTicksOneRecord(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestReputationAgent(cs)
	d1 := a.Evaluate(bounceSnap("1.2.3.4", 96))
	d2 := a.Evaluate(bounceSnap("1.2.3.4", 96))
	if got := countStmt(cs, AgentReputation, bounceStmt); got != 1 {
		t.Fatalf("bounce WONT after 2 ticks = %d, want 1", got)
	}
	has := func(ds []Decision) bool {
		for _, d := range ds {
			if d.ActionTaken == "deprioritize_ip" {
				return true
			}
		}
		return false
	}
	if !has(d1) || !has(d2) {
		t.Fatalf("bounce decision must be emitted every tick: %v %v", d1, d2)
	}
}

func TestReputationBounceConvictionDedup_RestoreThenDropRecordsAgain(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "")
	cs := NewConvictionStore(nil)
	a := newTestReputationAgent(cs)
	a.Evaluate(bounceSnap("1.2.3.4", 96))
	a.Evaluate(bounceSnap("1.2.3.4", 96))
	a.Evaluate(bounceSnap("1.2.3.4", 1)) // recovered
	a.Evaluate(bounceSnap("1.2.3.4", 96))
	if got := countStmt(cs, AgentReputation, bounceStmt); got != 2 {
		t.Fatalf("bounce WONT after drop/restore/drop = %d, want 2", got)
	}
}

func TestReputationBounceConvictionDedup_KillSwitchRecordsEveryTick(t *testing.T) {
	t.Setenv("ENGINE_CONVICTION_DEDUP_DISABLED", "1")
	cs := NewConvictionStore(nil)
	a := newTestReputationAgent(cs)
	for i := 0; i < 3; i++ {
		a.Evaluate(bounceSnap("1.2.3.4", 96))
	}
	if got := countStmt(cs, AgentReputation, bounceStmt); got != 3 {
		t.Fatalf("kill switch: bounce WONT after 3 ticks = %d, want 3", got)
	}
}
