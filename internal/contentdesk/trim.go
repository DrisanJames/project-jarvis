package contentdesk

import "strings"

// maxTrimPasses bounds the editorial trim: it repeats while each pass improves
// (live :1135: discountblog's single pass took 5 hard blockers to 2, and the
// re-judged takeaways block raised one more a second pass can cut).
const maxTrimPasses = 3

// trimFlagged is the editor's last resort after the revise loop: it cuts
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
//     would orphan its question (live :1134: myownhealth's last hard blocker).
//   - A flagged subject or preheader line is dropped — they are
//     interchangeable alternates (live :1135: discountblog's preheader:1).
//   - A flagged caption sentence is cut; a caption is optional (live :1135:
//     financialcalculate's table caption).
//   - Never a heading, table row, title, excerpt or meta line, and never a
//     block's last text sentence, item or FAQ pair, or the last subject or
//     preheader.
//
// Returns the trimmed body and package with every reference re-indexed, and
// how many units were removed.
func trimFlagged(a assessment) (draftResult, packageResult, int) {
	cut := map[string]map[int]bool{}
	for _, j := range a.judgment {
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

		// Caption sentences follow the rows. A caption is optional, so a
		// flagged one may be cut entirely (live :1135: financialcalculate's
		// table caption asserted an APR difference it never computed).
		cs := SplitSentences(b.Caption)
		cbase := u + len(b.Rows)
		var keptCap []string
		capCut := false
		for i, s := range cs {
			if flagged[cbase+i] {
				removed[cbase+i] = true
				capCut = true
				continue
			}
			keptCap = append(keptCap, s)
		}

		if textCut {
			b.Text = strings.Join(keptText, " ")
		}
		if len(drop) > 0 {
			b.Items = keptItems
		}
		if capCut {
			b.Caption = strings.Join(keptCap, " ")
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

	// Package: title, excerpt and meta stay; flagged subject/preheader
	// alternates are dropped and the survivors' unit ids renumbered.
	pk := packageResult{Title: a.pkg.Title, Excerpt: a.pkg.Excerpt, MetaTitle: a.pkg.MetaTitle, MetaDescription: a.pkg.MetaDescription}
	rename := map[string]string{} // surviving old unit id → new unit id
	keepAlts := func(lines []string, unit func(int) string) []string {
		var keep []int
		for i := range lines {
			if len(cut[unit(i)]) == 0 {
				keep = append(keep, i)
			}
		}
		if len(keep) == 0 { // never drop the last alternate
			for i := range lines {
				keep = append(keep, i)
			}
		}
		kept := make([]string, 0, len(keep))
		for ni, oi := range keep {
			kept = append(kept, lines[oi])
			rename[unit(oi)] = unit(ni)
		}
		n += len(lines) - len(keep)
		return kept
	}
	pk.Subjects = keepAlts(a.pkg.Subjects, SubjectUnit)
	pk.Preheaders = keepAlts(a.pkg.Preheaders, PreheaderUnit)
	for _, r := range a.refs {
		if !IsReservedUnitID(r.BlockID) {
			continue
		}
		if strings.HasPrefix(r.BlockID, unitSubjectPrefix) || strings.HasPrefix(r.BlockID, unitPreheaderPrefix) {
			id, ok := rename[r.BlockID]
			if !ok {
				continue // its line was dropped
			}
			r.BlockID = id
		}
		pk.ClaimRefs = append(pk.ClaimRefs, r)
	}
	return out, pk, n
}
