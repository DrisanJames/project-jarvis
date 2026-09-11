package contentdesk

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Citation contract (2026-09-11). The writer cites with inline markers,
// [[c:<claim_id>@<version>]], and code computes each ref's (block_id,
// sentence_idx) with the same splitter the checks and the judge use. When the
// model counted sentences itself, 3 of 4 pilot drafts carried refs to units
// that did not exist, and misaligned refs showed the judge blank sentences
// (myownhealth: 33 fail-closed "no verdict" items). The version is part of
// the draft / package / revise input hashes, so a contract change never
// replays outputs cached under the old one.
const citationContractVersion = "2026-09-11.markers2-supported-only"

// Any [[c:…]] is consumed; only a UUID id is recorded ("N/A" is dropped).
var citationMarkerRE = regexp.MustCompile(`\[\[c:([^\]@]*)(?:@(\d+))?\]\]`)

type markerAt struct {
	claimID string
	version int // 0 = not written
	at      int // rune offset in the stripped text
}

// stripMarkers removes citation markers — and the space left in front of one
// that sat before punctuation, a space or the end — and returns each valid
// marker with its rune offset in the clean text.
func stripMarkers(s string) (string, []markerAt) {
	locs := citationMarkerRE.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return s, nil
	}
	var out []rune
	var ms []markerAt
	prev := 0
	for _, l := range locs {
		out = append(out, []rune(s[prev:l[0]])...)
		next := s[l[1]:]
		if n := len(out); n > 0 && out[n-1] == ' ' && (next == "" || strings.IndexAny(next[:1], ".!?,;:) \n\t") == 0) {
			out = out[:n-1]
		}
		id := strings.ToLower(strings.TrimSpace(s[l[2]:l[3]]))
		if validUUID(id) == nil {
			m := markerAt{claimID: id, at: len(out)}
			if l[4] >= 0 {
				m.version, _ = strconv.Atoi(s[l[4]:l[5]])
			}
			ms = append(ms, m)
		}
		prev = l[1]
	}
	out = append(out, []rune(s[prev:])...)
	return string(out), ms
}

type sentSpan struct{ start, end int } // rune offsets, whitespace-trimmed

// sentenceSpans mirrors SplitSentences exactly (same boundary rule, same
// trimming) but returns where each sentence lies, so a marker offset maps to
// its sentence.
func sentenceSpans(text string) []sentSpan {
	rs := []rune(text)
	var out []sentSpan
	emit := func(a, b int) {
		for a < b && unicode.IsSpace(rs[a]) {
			a++
		}
		for b > a && unicode.IsSpace(rs[b-1]) {
			b--
		}
		if a < b {
			out = append(out, sentSpan{a, b})
		}
	}
	start := 0
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if (c == '.' || c == '!' || c == '?') && (i+1 == len(rs) || rs[i+1] == ' ' || rs[i+1] == '\n' || rs[i+1] == '\t') {
			emit(start, i+1)
			start = i + 1
		}
	}
	if start < len(rs) {
		emit(start, len(rs))
	}
	return out
}

// sentenceOf is the sentence a marker at rune offset o belongs to: the last
// one that starts before o, so a marker written after a sentence's closing
// punctuation attaches to that sentence, not the next. -1 for empty text.
func sentenceOf(spans []sentSpan, o int) int {
	if len(spans) == 0 {
		return -1
	}
	idx := 0
	for i, sp := range spans {
		if sp.start < o {
			idx = i
		}
	}
	return idx
}

func claimVersions(claims []Claim) map[string]int {
	v := map[string]int{}
	for _, c := range claims {
		id := strings.ToLower(c.ClaimID)
		if c.Version > v[id] {
			v[id] = c.Version
		}
	}
	return v
}

func refFor(unit string, si int, m markerAt, versions map[string]int) (ClaimRef, bool) {
	v := m.version
	if v == 0 {
		v = versions[m.claimID]
	}
	if v == 0 || si < 0 {
		return ClaimRef{}, false
	}
	return ClaimRef{BlockID: unit, SentenceIdx: si, ClaimID: m.claimID, Version: v}, true
}

func dedupeRefs(refs []ClaimRef) []ClaimRef {
	seen := map[string]bool{}
	out := make([]ClaimRef, 0, len(refs))
	for _, r := range refs {
		k := r.BlockID + "#" + strconv.Itoa(r.SentenceIdx) + "#" + ClaimVersionKey(r.ClaimID, r.Version)
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// extractBlockRefs strips markers from b in place and returns its refs,
// indexed exactly as BlockSentences lays the block out: heading (one unit),
// text sentences, each item's sentences, one unit per row, caption sentences.
func extractBlockRefs(b *Block, versions map[string]int) []ClaimRef {
	var refs []ClaimRef
	add := func(ms []markerAt, unit int) {
		for _, m := range ms {
			if r, ok := refFor(b.ID, unit, m, versions); ok {
				refs = append(refs, r)
			}
		}
	}
	idx := 0
	clean, ms := stripMarkers(b.Heading)
	b.Heading = clean
	if strings.TrimSpace(clean) != "" {
		add(ms, idx)
		idx++
	}
	multi := func(s *string) {
		clean, ms := stripMarkers(*s)
		*s = clean
		spans := sentenceSpans(clean)
		for _, m := range ms {
			if si := sentenceOf(spans, m.at); si >= 0 {
				add([]markerAt{m}, idx+si)
			}
		}
		idx += len(spans)
	}
	multi(&b.Text)
	for i := range b.Items {
		multi(&b.Items[i])
	}
	for r := range b.Rows {
		var rowMs []markerAt
		for c := range b.Rows[r] {
			clean, ms := stripMarkers(b.Rows[r][c])
			b.Rows[r][c] = clean
			rowMs = append(rowMs, ms...)
		}
		add(rowMs, idx)
		idx++
	}
	multi(&b.Caption)
	return dedupeRefs(refs)
}

// extractDraftRefs strips markers from every block in place and returns the
// draft's refs.
func extractDraftRefs(blocks []Block, claims []Claim) []ClaimRef {
	vs := claimVersions(claims)
	var refs []ClaimRef
	for i := range blocks {
		refs = append(refs, extractBlockRefs(&blocks[i], vs)...)
	}
	return refs
}

// extractPackageRefs strips markers from package text in place and returns
// refs on the reserved package units.
func extractPackageRefs(pk *packageResult, claims []Claim) []ClaimRef {
	vs := claimVersions(claims)
	var refs []ClaimRef
	single := func(unit string, s *string) {
		clean, ms := stripMarkers(*s)
		*s = strings.TrimSpace(clean)
		if *s == "" {
			return
		}
		for _, m := range ms {
			if r, ok := refFor(unit, 0, m, vs); ok {
				refs = append(refs, r)
			}
		}
	}
	multi := func(unit string, s *string) {
		clean, ms := stripMarkers(*s)
		spans := sentenceSpans(clean)
		for _, m := range ms {
			if r, ok := refFor(unit, sentenceOf(spans, m.at), m, vs); ok {
				refs = append(refs, r)
			}
		}
		*s = strings.TrimSpace(clean) // trimming never changes the sentence count
	}
	single(UnitTitle, &pk.Title)
	single(UnitMetaTitle, &pk.MetaTitle)
	multi(UnitExcerpt, &pk.Excerpt)
	multi(UnitMetaDescription, &pk.MetaDescription)
	for i := range pk.Subjects {
		single(SubjectUnit(i), &pk.Subjects[i])
	}
	for i := range pk.Preheaders {
		single(PreheaderUnit(i), &pk.Preheaders[i])
	}
	return dedupeRefs(refs)
}

// enforcePackageLimits trims package text that overruns its code-check limit
// (2026-09-11: 3 of 4 pilot excerpts were 307–310 chars against 300 — an S1
// failure no rewrite fixed). Whole trailing sentences go first, and refs to a
// removed sentence go with them. Measured in bytes, the stricter unit.
func enforcePackageLimits(pk *packageResult) {
	var keepEx, keepMD int
	pk.Excerpt, keepEx = fitSentences(pk.Excerpt, MaxExcerptLen)
	pk.MetaDescription, keepMD = fitSentences(pk.MetaDescription, MaxMetaDescriptionLen)
	pk.Title = fitWords(pk.Title, MaxTitleLen)
	pk.MetaTitle = fitWords(pk.MetaTitle, MaxMetaTitleLen)
	for i := range pk.Subjects {
		pk.Subjects[i] = fitWords(pk.Subjects[i], MaxSubjectLen)
	}
	for i := range pk.Preheaders {
		pk.Preheaders[i] = fitWords(pk.Preheaders[i], MaxPreheaderLen)
	}
	kept := pk.ClaimRefs[:0]
	for _, r := range pk.ClaimRefs {
		if (r.BlockID == UnitExcerpt && r.SentenceIdx >= keepEx) || (r.BlockID == UnitMetaDescription && r.SentenceIdx >= keepMD) {
			continue
		}
		kept = append(kept, r)
	}
	pk.ClaimRefs = kept
}

// fitSentences keeps the longest run of whole leading sentences within max
// bytes and returns the text and how many sentences it kept.
func fitSentences(s string, max int) (string, int) {
	sents := SplitSentences(s)
	if len(s) <= max {
		return s, len(sents)
	}
	var kept []string
	total := 0
	for _, x := range sents {
		n := len(x)
		if len(kept) > 0 {
			n++ // joining space
		}
		if total+n > max {
			break
		}
		kept = append(kept, x)
		total += n
	}
	if len(kept) == 0 && len(sents) > 0 {
		return fitWords(sents[0], max), 1
	}
	return strings.Join(kept, " "), len(kept)
}

// fitWords cuts s at a word boundary so it fits max bytes with a trailing "…".
func fitWords(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ell = "…"
	limit := max - len(ell)
	end, lastSpace := 0, -1
	for i, r := range s {
		if i+utf8.RuneLen(r) > limit {
			break
		}
		if r == ' ' {
			lastSpace = i
		}
		end = i + utf8.RuneLen(r)
	}
	cut := s[:end]
	if lastSpace > 0 {
		cut = s[:lastSpace]
	}
	return strings.TrimRight(cut, " ,;:-") + ell
}

// markedBlocks re-inserts markers so the reviser sees where each claim is
// cited — the inverse of extractBlockRefs at sentence granularity: a unit's
// markers go just before its closing punctuation.
func markedBlocks(blocks []Block, refs []ClaimRef) []Block {
	by := map[string]map[int][]string{}
	for _, r := range refs {
		if by[r.BlockID] == nil {
			by[r.BlockID] = map[int][]string{}
		}
		by[r.BlockID][r.SentenceIdx] = append(by[r.BlockID][r.SentenceIdx], "[[c:"+r.ClaimID+"@"+strconv.Itoa(r.Version)+"]]")
	}
	out := make([]Block, len(blocks))
	for i, b := range blocks {
		m := by[b.ID]
		nb := b
		idx := 0
		if strings.TrimSpace(b.Heading) != "" {
			nb.Heading = withMarkers(b.Heading, m[idx])
			idx++
		}
		multi := func(s string) string {
			sents := SplitSentences(s)
			if len(sents) == 0 {
				return s
			}
			for j := range sents {
				sents[j] = withMarkers(sents[j], m[idx+j])
			}
			idx += len(sents)
			return strings.Join(sents, " ")
		}
		nb.Text = multi(b.Text)
		nb.Items = make([]string, len(b.Items))
		for j, it := range b.Items {
			nb.Items[j] = multi(it)
		}
		nb.Rows = make([][]string, len(b.Rows))
		for r, row := range b.Rows {
			nr := append([]string(nil), row...)
			if len(nr) > 0 && len(m[idx]) > 0 {
				nr[len(nr)-1] = nr[len(nr)-1] + " " + strings.Join(m[idx], "")
			}
			nb.Rows[r] = nr
			idx++
		}
		nb.Caption = multi(b.Caption)
		out[i] = nb
	}
	return out
}

func withMarkers(sent string, ms []string) string {
	if len(ms) == 0 {
		return sent
	}
	mk := strings.Join(ms, "")
	if n := len(sent); n > 0 && strings.ContainsAny(sent[n-1:], ".!?") {
		return sent[:n-1] + " " + mk + sent[n-1:]
	}
	return sent + " " + mk
}
