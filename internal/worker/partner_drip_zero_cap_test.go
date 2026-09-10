package worker

import (
	"reflect"
	"testing"
)

// applyZeroCaps is the orchestrator half of the contracted-zero prohibition
// (REQ-118 contract-fulfilment rule 5). The mediator half — which ISPs are in
// the set, and in which modes — is pinned in
// internal/worker/dripsupply/contract_fulfilment_new_test.go.
//
// WP-D measured what its absence cost on 2026-09-08: three domain×microsoft
// cells whose domain contracts promised 0 delivered 6,823 / 4,501 / 2,656. Under
// MODE=canary those lanes were outside the enforcement scope, so their caps came
// from the old chain — which has never read drip_domain_contracts.
func TestApplyZeroCaps(t *testing.T) {
	cases := []struct {
		name  string
		caps  map[string]int
		zeros []string
		want  map[string]int
	}{
		{
			name:  "the prohibition binds on the old chain's caps",
			caps:  map[string]int{"aol": 4000, "microsoft": 2500, "gmail": 1000},
			zeros: []string{"microsoft"},
			want:  map[string]int{"aol": 4000, "microsoft": 0, "gmail": 1000},
		},
		{
			name:  "several at once, and only those",
			caps:  map[string]int{"aol": 4000, "microsoft": 2500, "gmail": 1000},
			zeros: []string{"gmail", "microsoft"},
			want:  map[string]int{"aol": 4000, "microsoft": 0, "gmail": 0},
		},
		{
			// The map's keys are the wave's ISP set. Adding one would WIDEN the
			// wave into a lane it was never planned for — a zero cap on an ISP
			// the wave does not carry is not a licence to carry it.
			name:  "an ISP the wave does not carry is never added",
			caps:  map[string]int{"aol": 4000},
			zeros: []string{"microsoft"},
			want:  map[string]int{"aol": 4000},
		},
		{
			name:  "nothing to zero is a pass-through",
			caps:  map[string]int{"aol": 4000},
			zeros: nil,
			want:  map[string]int{"aol": 4000},
		},
		{
			name:  "an already-zero cap is left alone",
			caps:  map[string]int{"aol": 0},
			zeros: []string{"aol"},
			want:  map[string]int{"aol": 0},
		},
		{
			name:  "a nil cap map survives",
			caps:  nil,
			zeros: []string{"microsoft"},
			want:  map[string]int{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := cloneISPCapMap(c.caps)
			got := applyZeroCaps(c.caps, c.zeros)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("applyZeroCaps(%v, %v) = %v, want %v", c.caps, c.zeros, got, c.want)
			}
			// It must never mutate its input: grantWaveCapacity's caller still
			// holds the chain's map and compares against it.
			if !reflect.DeepEqual(cloneISPCapMap(c.caps), in) {
				t.Errorf("input map was mutated: %v, was %v", c.caps, in)
			}
		})
	}
}

// It can only ever REDUCE. That asymmetry is the entire argument for applying it
// outside the mediator's enforcement scope: the worst a wrong answer can do is
// mail less.
func TestApplyZeroCapsOnlyEverReduces(t *testing.T) {
	caps := map[string]int{"aol": 4000, "microsoft": 2500, "gmail": 0, "yahoo": 17000}
	for _, zeros := range [][]string{
		nil, {"aol"}, {"gmail"}, {"aol", "gmail", "microsoft", "yahoo"}, {"nonexistent"},
	} {
		got := applyZeroCaps(caps, zeros)
		for isp, v := range got {
			if v > caps[isp] {
				t.Errorf("zeros=%v raised %s from %d to %d", zeros, isp, caps[isp], v)
			}
			if v < 0 {
				t.Errorf("zeros=%v drove %s negative (%d)", zeros, isp, v)
			}
		}
		if len(got) != len(caps) {
			t.Errorf("zeros=%v changed the ISP set size from %d to %d", zeros, len(caps), len(got))
		}
	}
}
