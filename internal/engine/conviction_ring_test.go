package engine

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// --- convictionRing as a circular buffer (2026-11-05 CPU audit) ---

func mkConv(i int, v Verdict) Conviction {
	return Conviction{ID: fmt.Sprintf("c%04d", i), Verdict: v, CreatedAt: time.Unix(int64(i), 0),
		Context: MicroContext{HourUTC: i % 24, IP: fmt.Sprintf("10.0.0.%d", i%7), Domain: fmt.Sprintf("d%d.test", i%3)}}
}

func TestConvictionRing_OrderAndOverflow(t *testing.T) {
	r := newConvictionRing(5)
	for i := 0; i < 12; i++ {
		r.append(mkConv(i, VerdictWill))
	}
	if r.len() != 5 {
		t.Fatalf("len=%d want 5", r.len())
	}
	all := r.all()
	for k, want := range []string{"c0007", "c0008", "c0009", "c0010", "c0011"} {
		if all[k].ID != want || r.at(k).ID != want {
			t.Fatalf("slot %d: all=%s at=%s want %s", k, all[k].ID, r.at(k).ID, want)
		}
	}
}

func TestConvictionRing_GrowsLazilyToMax(t *testing.T) {
	r := newConvictionRing(2000)
	if cap(r.buf) >= 2000 {
		t.Fatalf("ring must not pre-allocate max capacity (cap=%d)", cap(r.buf))
	}
	for i := 0; i < 3; i++ {
		r.append(mkConv(i, VerdictWont))
	}
	if r.len() != 3 || r.at(0).ID != "c0000" || r.at(2).ID != "c0002" {
		t.Fatalf("partial ring order wrong: %v", r.all())
	}
	if got := r.byVerdict(VerdictWont); len(got) != 3 {
		t.Fatalf("byVerdict=%d want 3", len(got))
	}
	if got := r.byVerdict(VerdictWill); len(got) != 0 {
		t.Fatalf("byVerdict(will)=%d want 0", len(got))
	}
}

// referenceRecallSimilar is the pre-2026-11-05 implementation, kept here as the
// oracle: copy everything, score by value, stable insertion sort descending.
func referenceRecallSimilar(all []Conviction, query MicroContext, limit int) []ScoredConviction {
	var scored []ScoredConviction
	for _, c := range all {
		if sim := contextSimilarity(query, c.Context); sim > 0.2 {
			scored = append(scored, ScoredConviction{Conviction: c, Similarity: sim})
		}
	}
	for i := 1; i < len(scored); i++ {
		j := i
		for j > 0 && scored[j].Similarity > scored[j-1].Similarity {
			scored[j], scored[j-1] = scored[j-1], scored[j]
			j--
		}
	}
	if limit > 0 && len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

func TestRecallSimilar_MatchesReferenceIncludingTies(t *testing.T) {
	cs := NewConvictionStore(nil)
	cs.maxPerAgent = 50 // force wrap-around so ring order != insertion index
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 130; i++ {
		c := mkConv(i, VerdictWont)
		c.Context.HourUTC = rng.Intn(24)
		c.Context.IP = fmt.Sprintf("10.0.0.%d", rng.Intn(4)) // few values => many exact ties
		c.Context.Domain = fmt.Sprintf("d%d.test", rng.Intn(2))
		cs.ring("gmail", AgentThrottle).append(c)
	}
	all := cs.RecallAll("gmail", AgentThrottle)
	if len(all) != 50 || all[0].ID != "c0080" || all[49].ID != "c0129" {
		t.Fatalf("ring content unexpected: n=%d first=%s last=%s", len(all), all[0].ID, all[len(all)-1].ID)
	}
	for _, limit := range []int{0, 1, 3, 10, 500} {
		for q := 0; q < 20; q++ {
			query := MicroContext{HourUTC: rng.Intn(24), IP: fmt.Sprintf("10.0.0.%d", rng.Intn(4)), Domain: fmt.Sprintf("d%d.test", rng.Intn(2))}
			want := referenceRecallSimilar(all, query, limit)
			got := cs.RecallSimilar("gmail", AgentThrottle, query, limit)
			if len(got) != len(want) {
				t.Fatalf("limit=%d: len got=%d want=%d", limit, len(got), len(want))
			}
			for k := range want {
				if got[k].Conviction.ID != want[k].Conviction.ID || got[k].Similarity != want[k].Similarity {
					t.Fatalf("limit=%d pos=%d: got %s/%.3f want %s/%.3f", limit, k, got[k].Conviction.ID, got[k].Similarity, want[k].Conviction.ID, want[k].Similarity)
				}
			}
		}
	}
}

func TestRecallRecent_NewestNInOrder(t *testing.T) {
	cs := NewConvictionStore(nil)
	cs.maxPerAgent = 10
	for i := 0; i < 25; i++ {
		cs.ring("yahoo", AgentPool).append(mkConv(i, VerdictWill))
	}
	got := cs.RecallRecent("yahoo", AgentPool, 3)
	if len(got) != 3 || got[0].ID != "c0022" || got[2].ID != "c0024" {
		t.Fatalf("RecallRecent(3)=%v", got)
	}
	if n := len(cs.RecallRecent("yahoo", AgentPool, 99)); n != 10 {
		t.Fatalf("RecallRecent(99) len=%d want 10", n)
	}
	if n := len(cs.RecallRecent("yahoo", AgentPool, 0)); n != 0 {
		t.Fatalf("RecallRecent(0) len=%d want 0", n)
	}
	will, wont := cs.Stats("yahoo", AgentPool)
	if will != 10 || wont != 0 {
		t.Fatalf("Stats=%d/%d want 10/0", will, wont)
	}
}
