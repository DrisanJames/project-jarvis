package contentdesk

import "strings"

// trimFlagged is the editor's last resort after the revise loop: it cuts body
// content that still carries a hard claim finding no rewrite cleared — an
// unreferenced_claim flag, or a claim judged unsupported or overstated.
// Cutting only removes content; it never adds an unchecked fact. The trimmed
// revision is re-assessed and kept only under acceptRevision, and the
// adversarial second pass still re-judges it.
//
//   - A flagged text sentence is cut (live :1132: the reviser kept
//     myownhealth's uncited "check the serving size at the top of the label"
//     for 4 rounds).
//   - A flagged sentence inside a list item removes that item; inside an FAQ
//     it removes the whole question/answer pair, since an answer cut alone
//     would orphan its question (live :1134: myownhealth's last hard blocker
//     was an overstated FAQ answer).
//   - Never a heading, table row or caption, and never a block's last text
//     sentence, last item or last FAQ pair.
//
// Returns the trimmed body with its references re-indexed, and how many
// sentences (units) were removed.
func trimFlagged(a assessment) (draftResult, int) {
	cut := map[string]map[int]bool{}
	for _, j := range a.judgment {
		if IsReservedUnitID(j.BlockID) {
			continue
		}
		if j.Kind == "unreferenced_claim" || (j.Kind == "claim" && (j.Verdict == "unsupported" || j.Verdict == "overstated")) {
			if cut[j.BlockID] == nil {
				cut[j.BlockID] = map[int]bool{}
			}
			cut[j.BlockID][j.SentenceIdx] = true
		}
	}
	var out draftResult
	n := 0
	for _, b := range a.pkg.Blocks {
		flagged := cut[b.ID]
		removed := map[int]bool{} // unit indices (BlockSentences order) removed from this block

		// Text sentences: units h .. h+len(ts)-1 (a heading is unit 0).
		h := 0
		if strings.TrimSpace(b.Heading) != "" {
			h = 1
		}
		ts := SplitSentences(b.Text)
		var keptText []string
		for i, s := range ts {
			if flagged[h+i] {
				removed[h+i] = true
				continue
			}
			keptText = append(keptText, s)
		}
		if len(ts) > 0 && len(keptText) == 0 { // never empty a block's text
			for i := range ts {
				delete(removed, h+i)
			}
		}
		textCut := len(removed) > 0

		// Items: each item's sentences follow the text.
		start := make([]int, len(b.Items))
		sents := make([][]string, len(b.Items))
		u := h + len(ts)
		for i, it := range b.Items {
			start[i], sents[i] = u, SplitSentences(it)
			u += len(sents[i])
		}
		itemFlagged := func(i int) bool {
			for k := range sents[i] {
				if flagged[start[i]+k] {
					return true
				}
			}
			return false
		}
		drop := map[int]bool{}
		if b.Type == "faq" {
			pairs, dropped := 0, 0
			for p := 0; p+1 < len(b.Items); p += 2 {
				pairs++
				if itemFlagged(p) || itemFlagged(p+1) {
					drop[p], drop[p+1] = true, true
					dropped++
				}
			}
			if dropped >= pairs { // never remove every pair
				drop = map[int]bool{}
			}
		} else {
			for i := range b.Items {
				if itemFlagged(i) {
					drop[i] = true
				}
			}
			if len(drop) >= len(b.Items) { // never remove every item
				drop = map[int]bool{}
			}
		}
		var keptItems []string
		for i, it := range b.Items {
			if !drop[i] {
				keptItems = append(keptItems, it)
				continue
			}
			for k := range sents[i] {
				removed[start[i]+k] = true
			}
		}

		if textCut {
			b.Text = strings.Join(keptText, " ")
		}
		if len(drop) > 0 {
			b.Items = keptItems
		}
		n += len(removed)
		out.Blocks = append(out.Blocks, b)
		for _, r := range a.refs {
			if r.BlockID != b.ID || removed[r.SentenceIdx] {
				continue
			}
			shift := 0
			for x := range removed {
				if x < r.SentenceIdx {
					shift++
				}
			}
			r.SentenceIdx -= shift
			out.ClaimRefs = append(out.ClaimRefs, r)
		}
	}
	return out, n
}
