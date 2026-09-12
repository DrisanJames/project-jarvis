package contentdesk

import "strings"

// maxTrimPasses bounds the editorial trim: it repeats while each pass improves
// (live :1135: discountblog's single pass took 5 hard blockers to 2, and the
// re-judged takeaways block raised one more a second pass can cut).
const maxTrimPasses = 5

// trimAcceptable keeps a trim pass unless it makes things worse: no code
// check newly fails and there are no more hard blockers than before. Unlike a
// revise round it need not strictly improve — a cut only removes content, and
// only the changed units are re-judged, so repeated passes converge on 0 hard
// blockers (live :1140: pass 2 came back at 1 hard vs 1, was rejected as "no
// improvement", and the article stalled one blocker short).
func trimAcceptable(prev, cand assessment) bool {
	failedBefore := map[string]bool{}
	for _, c := range prev.code {
		if !c.Passed {
			failedBefore[c.Name] = true
		}
	}
	for _, c := range cand.code {
		if !c.Passed && !failedBefore[c.Name] {
			return false
		}
	}
	return cand.hardBlockers() <= prev.hardBlockers()
}

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
	return trimWhere(a, func(j JudgmentItem) bool {
		return j.Kind == "unreferenced_claim" || (j.Kind == "claim" && (j.Verdict == "unsupported" || j.Verdict == "overstated"))
	})
}

// trimWhere cuts the units of the judgment items pick selects, under the same
// rules as trimFlagged (FAQ pairs, list items, alternates; never the last).
// The editor-cut step uses it for items the managing editor refused.
func trimWhere(a assessment, pick func(JudgmentItem) bool) (draftResult, packageResult, int) {
	cut := map[string]map[int]bool{}
	for _, j := range a.judgment {
		if pick(j) {
			if cut[j.BlockID] == nil {
				cut[j.BlockID] = map[int]bool{}
			}
			cut[j.BlockID][j.SentenceIdx] = true
		}
	}
	var out draftResult
	n := 0
	for _, b := range a.pkg.Blocks {
		// A worked_example the model never gave a calc fails schema (S1), and
		// no revise round added one (live :1145: bestcreditcare held on it
		// after 5 rounds). Its prose stands as a plain section: units and
		// references are unchanged, since BlockSentences ignores the type.
		if b.Type == "worked_example" && b.Calc == nil && (strings.TrimSpace(b.Text) != "" || len(b.Items) > 0) {
			b.Type = "section"
			n++
		}
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

	// Package: title and meta title stay. Flagged excerpt / meta description
	// sentences go through trimHeadline. Flagged subject/preheader alternates
	// are dropped and the survivors' unit ids renumbered.
	pk := packageResult{Title: a.pkg.Title, MetaTitle: a.pkg.MetaTitle}
	var headRefs []ClaimRef
	var hn int
	pk.Excerpt, headRefs, hn = trimHeadline(a, UnitExcerpt, a.pkg.Excerpt, cut)
	pk.ClaimRefs, n = append(pk.ClaimRefs, headRefs...), n+hn
	pk.MetaDescription, headRefs, hn = trimHeadline(a, UnitMetaDescription, a.pkg.MetaDescription, cut)
	pk.ClaimRefs, n = append(pk.ClaimRefs, headRefs...), n+hn
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
		if !IsReservedUnitID(r.BlockID) || r.BlockID == UnitExcerpt || r.BlockID == UnitMetaDescription {
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

// trimHeadline cuts the flagged sentences of an excerpt or meta description.
// When every sentence is flagged the unit falls back to the lede's first
// sentence with no hard finding, carrying that sentence's references — text
// already judged in the body, so no unchecked fact is added. With no such
// sentence the unit is left as is. Live :1144: myownhealth's one-sentence
// excerpt dropped a qualifier ("only nutrients with a Daily Value"), stayed
// overstated through 5 package revisions, and nothing could cut it.
// Returns the text, its references and the number of sentences removed.
func trimHeadline(a assessment, unit, text string, cut map[string]map[int]bool) (string, []ClaimRef, int) {
	var refs []ClaimRef
	for _, r := range a.refs {
		if r.BlockID == unit {
			refs = append(refs, r)
		}
	}
	sents := SplitSentences(text)
	newIdx := map[int]int{}
	var kept []string
	for i, s := range sents {
		if !cut[unit][i] {
			newIdx[i] = len(kept)
			kept = append(kept, s)
		}
	}
	if len(kept) == len(sents) {
		return text, refs, 0
	}
	if len(kept) > 0 {
		var out []ClaimRef
		for _, r := range refs {
			if ni, ok := newIdx[r.SentenceIdx]; ok {
				r.SentenceIdx = ni
				out = append(out, r)
			}
		}
		return strings.Join(kept, " "), out, len(sents) - len(kept)
	}
	s, srefs, ok := ledeFallback(a, cut)
	if !ok {
		return text, refs, 0
	}
	out := make([]ClaimRef, 0, len(srefs))
	for _, r := range srefs {
		r.BlockID, r.SentenceIdx = unit, 0
		out = append(out, r)
	}
	return s, out, len(sents)
}

// ledeFallback is the lede's first text sentence (≥40 chars) with no hard
// finding, and its references.
func ledeFallback(a assessment, cut map[string]map[int]bool) (string, []ClaimRef, bool) {
	for _, b := range a.pkg.Blocks {
		if b.Type != "lede" {
			continue
		}
		h := 0
		if strings.TrimSpace(b.Heading) != "" {
			h = 1
		}
		for i, s := range SplitSentences(b.Text) {
			if cut[b.ID][h+i] || len(s) < 40 {
				continue
			}
			var refs []ClaimRef
			for _, r := range a.refs {
				if r.BlockID == b.ID && r.SentenceIdx == h+i {
					refs = append(refs, r)
				}
			}
			return s, refs, true
		}
		return "", nil, false
	}
	return "", nil, false
}
