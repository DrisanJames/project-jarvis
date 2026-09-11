package contentdesk

import "strings"

// trimFlagged is the editor's last resort after the revise loop: it cuts body
// text sentences that still carry a hard finding no rewrite cleared, an
// unreferenced_claim flag or an unsupported claim verdict. Cutting an uncited
// or unsupported sentence only removes content; it never adds an unchecked
// fact. The trimmed revision is re-assessed and kept only under
// acceptRevision, and the adversarial second pass still re-judges it.
// Live :1132: across 4 rounds the reviser kept myownhealth's uncited "check
// the serving size at the top of the label", the article's only hard blocker
// with 37/37 references supported.
//
// Only text sentences are cut (never headings, list items, table rows or
// captions), and never a block's last text sentence. Returns the trimmed body
// with its references re-indexed, and how many sentences were cut.
func trimFlagged(a assessment) (draftResult, int) {
	cut := map[string]map[int]bool{}
	for _, j := range a.judgment {
		if IsReservedUnitID(j.BlockID) {
			continue
		}
		if j.Kind == "unreferenced_claim" || (j.Kind == "claim" && j.Verdict == "unsupported") {
			if cut[j.BlockID] == nil {
				cut[j.BlockID] = map[int]bool{}
			}
			cut[j.BlockID][j.SentenceIdx] = true
		}
	}
	var out draftResult
	n := 0
	for _, b := range a.pkg.Blocks {
		h := 0 // a heading is unit 0 (BlockSentences)
		if strings.TrimSpace(b.Heading) != "" {
			h = 1
		}
		ts := SplitSentences(b.Text)
		drop := map[int]bool{}
		var kept []string
		for i, s := range ts {
			if cut[b.ID][h+i] {
				drop[h+i] = true
				continue
			}
			kept = append(kept, s)
		}
		if len(kept) == 0 { // never empty a block's text
			drop = map[int]bool{}
		}
		if len(drop) > 0 {
			b.Text = strings.Join(kept, " ")
			n += len(drop)
		}
		out.Blocks = append(out.Blocks, b)
		for _, r := range a.refs {
			if r.BlockID != b.ID || drop[r.SentenceIdx] {
				continue
			}
			shift := 0
			for u := range drop {
				if u < r.SentenceIdx {
					shift++
				}
			}
			r.SentenceIdx -= shift
			out.ClaimRefs = append(out.ClaimRefs, r)
		}
	}
	return out, n
}
