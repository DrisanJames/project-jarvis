package contentdesk

import (
	"context"
	"strconv"
)

// Incremental judgment (2026-09-11). With full coverage, every revise round
// re-judged the whole article and the judge found a different set of
// overstated sentences each time (:1127: discountblog 7 hard blockers → 9,
// myownhealth 7 → 8), so no round could converge. A reference whose
// sentence, paragraph and cited claim version are unchanged keeps its
// previous verdict; only changed references are judged again. Flags are
// always re-judged over the whole package, and the adversarial second pass
// re-judges everything from scratch.

// carryKey is a reference's judged context: its unit, sentence, the
// paragraph around it and the claim version it cites.
func carryKey(pkg Package, units map[string][]string, blockID string, idx int, claimID string, version int) string {
	s := ""
	if u := units[blockID]; idx >= 0 && idx < len(u) {
		s = u[idx]
	}
	return blockID + "\x00" + s + "\x00" + UnitParagraph(pkg, blockID) + "\x00" + ClaimVersionKey(claimID, version)
}

// carriedVerdicts maps each index of refs whose context is unchanged since
// prev to prev's verdict on it. A fail-closed placeholder never carries.
func carriedVerdicts(prev assessment, cur Package, refs []ClaimRef) map[int]JudgmentItem {
	pu, cu := Units(prev.pkg), Units(cur)
	old := map[string]JudgmentItem{}
	for _, j := range prev.judgment {
		if j.Kind != "claim" || j.Note == failClosedNote {
			continue
		}
		old[carryKey(prev.pkg, pu, j.BlockID, j.SentenceIdx, j.ClaimID, j.Version)] = j
	}
	out := map[int]JudgmentItem{}
	for i, r := range refs {
		if j, ok := old[carryKey(cur, cu, r.BlockID, r.SentenceIdx, r.ClaimID, r.Version)]; ok {
			out[i] = j
		}
	}
	return out
}

// judgeIncremental judges the references not in carried and merges in the
// carried verdicts. With prev (a revise round or the trim), flags carry the
// same way: a unit whose text is unchanged since prev keeps prev's flags, and
// a fresh flag counts only on a unit that changed (live :1133: two
// unreferenced_claim flags appeared on sentences nobody had touched and sank
// the trim). Items come back in refs order, then the flags, numbered j1…
func (p *Pipeline) judgeIncremental(ctx context.Context, in PipelineInput, pkg Package, refs []ClaimRef, claims map[string]Claim, carried map[int]JudgmentItem, prev *assessment) (any, Usage, error) {
	var fresh, kept []ClaimRef
	var freshIdx []int
	for i, r := range refs {
		if _, ok := carried[i]; ok {
			kept = append(kept, r)
			continue
		}
		fresh = append(fresh, r)
		freshIdx = append(freshIdx, i)
	}
	items, u, err := p.judgeCall(ctx, in, judgeSystem, pkg, fresh, claims, kept)
	if err != nil {
		return nil, u, err
	}
	merged := make([]JudgmentItem, len(refs), len(refs)+len(items)-len(fresh))
	for k, i := range freshIdx {
		merged[i] = items[k]
	}
	for i, j := range carried {
		r := refs[i]
		j.BlockID, j.SentenceIdx, j.ClaimID, j.Version = r.BlockID, r.SentenceIdx, r.ClaimID, r.Version
		merged[i] = j
	}
	flags := items[len(fresh):]
	if prev != nil {
		changed := changedUnits(prev.pkg, pkg)
		var keptFlags []JudgmentItem
		for _, f := range flags {
			if changed[f.BlockID] {
				keptFlags = append(keptFlags, f)
			}
		}
		for _, f := range prev.judgment {
			if f.Kind != "claim" && !changed[f.BlockID] {
				keptFlags = append(keptFlags, f)
			}
		}
		flags = keptFlags
	}
	merged = append(merged, flags...)
	for i := range merged {
		merged[i].ID = "j" + strconv.Itoa(i+1)
	}
	return merged, u, nil
}

// changedUnits is the set of unit ids whose text differs between prev and
// cur; a unit present in only one of them counts as changed.
func changedUnits(prev, cur Package) map[string]bool {
	pu, cu := Units(prev), Units(cur)
	out := map[string]bool{}
	for id := range cu {
		if UnitParagraph(prev, id) != UnitParagraph(cur, id) {
			out[id] = true
		}
	}
	for id := range pu {
		if _, ok := cu[id]; !ok {
			out[id] = true
		}
	}
	return out
}
