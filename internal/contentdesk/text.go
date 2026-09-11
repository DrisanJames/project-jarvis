package contentdesk

import (
	"regexp"
	"strconv"
	"strings"
)

// Reserved unit ids for package-level text. Blocks may not use these ids.
const (
	UnitTitle           = "title"
	UnitExcerpt         = "excerpt"
	UnitMetaTitle       = "meta_title"
	UnitMetaDescription = "meta_description"
	unitSubjectPrefix   = "subject:"
	unitPreheaderPrefix = "preheader:"
)

// SubjectUnit / PreheaderUnit name the i-th subject / preheader unit.
func SubjectUnit(i int) string   { return unitSubjectPrefix + strconv.Itoa(i) }
func PreheaderUnit(i int) string { return unitPreheaderPrefix + strconv.Itoa(i) }

// IsReservedUnitID reports whether id is a package-level unit id.
func IsReservedUnitID(id string) bool {
	switch id {
	case UnitTitle, UnitExcerpt, UnitMetaTitle, UnitMetaDescription:
		return true
	}
	return strings.HasPrefix(id, unitSubjectPrefix) || strings.HasPrefix(id, unitPreheaderPrefix)
}

// SplitSentences is the ONE sentence splitter: code checks, the judge prompt
// and the review UI's sentence↔passage pairs all index through it, so a
// sentence_idx means the same sentence everywhere. A boundary is . ! or ?
// followed by whitespace (so "3.5%" never splits). Deterministic, not
// linguistic: "e.g. x" splits — the model sees the same indices it is judged on.
func SplitSentences(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var out []string
	start := 0
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if (c == '.' || c == '!' || c == '?') && (i+1 == len(rs) || rs[i+1] == ' ' || rs[i+1] == '\n' || rs[i+1] == '\t') {
			if s := strings.TrimSpace(string(rs[start : i+1])); s != "" {
				out = append(out, s)
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		if s := strings.TrimSpace(string(rs[start:])); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// BlockSentences flattens a block into its indexed sentence list, in this
// fixed order: heading (one unit), text sentences, each item's sentences,
// each table row (cells joined " | "), caption sentences.
func BlockSentences(b Block) []string {
	var out []string
	if h := strings.TrimSpace(b.Heading); h != "" {
		out = append(out, h)
	}
	out = append(out, SplitSentences(b.Text)...)
	for _, it := range b.Items {
		out = append(out, SplitSentences(it)...)
	}
	for _, row := range b.Rows {
		out = append(out, strings.Join(row, " | "))
	}
	out = append(out, SplitSentences(b.Caption)...)
	return out
}

// BlockParagraph is the block's full plain text — the "paragraph" the judge
// reads around a sentence.
func BlockParagraph(b Block) string {
	return strings.Join(BlockSentences(b), " ")
}

// Units maps every unit id (block ids + reserved package ids) to its
// indexed sentence list.
func Units(p Package) map[string][]string {
	u := map[string][]string{}
	if p.Title != "" {
		u[UnitTitle] = []string{strings.TrimSpace(p.Title)}
	}
	if p.MetaTitle != "" {
		u[UnitMetaTitle] = []string{strings.TrimSpace(p.MetaTitle)}
	}
	u[UnitExcerpt] = SplitSentences(p.Excerpt)
	u[UnitMetaDescription] = SplitSentences(p.MetaDescription)
	for i, s := range p.Subjects {
		u[SubjectUnit(i)] = []string{strings.TrimSpace(s)}
	}
	for i, s := range p.Preheaders {
		u[PreheaderUnit(i)] = []string{strings.TrimSpace(s)}
	}
	for _, b := range p.Blocks {
		u[b.ID] = BlockSentences(b)
	}
	return u
}

// UnitParagraph returns the context paragraph for a unit id.
func UnitParagraph(p Package, unitID string) string {
	for _, b := range p.Blocks {
		if b.ID == unitID {
			return BlockParagraph(b)
		}
	}
	return strings.Join(Units(p)[unitID], " ")
}

// AllText is every human-visible string in the package, one per element.
func AllText(p Package) []string {
	out := []string{p.Title, p.Excerpt, p.MetaTitle, p.MetaDescription}
	out = append(out, p.Subjects...)
	out = append(out, p.Preheaders...)
	for _, b := range p.Blocks {
		out = append(out, b.Heading, b.Text, b.Caption)
		out = append(out, b.Items...)
		for _, r := range b.Rows {
			out = append(out, r...)
		}
	}
	if p.HeroImage != nil {
		out = append(out, p.HeroImage.Alt, p.HeroImage.Credit, p.HeroImage.URL)
	}
	return out
}

var (
	// rawHTMLRE: any tag opener, comment, or HTML entity. The inline subset is
	// markdown only.
	rawHTMLRE = regexp.MustCompile(`<\s*/?\s*[a-zA-Z!]|&[a-zA-Z]+;|&#[0-9]+;`)
	mdLinkRE  = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)
	bareURLRE = regexp.MustCompile(`https?://[^\s)\]"'<>]+`)
	digitRE   = regexp.MustCompile(`[0-9]`)
)

// HasRawHTML reports whether s carries raw HTML.
func HasRawHTML(s string) bool { return rawHTMLRE.MatchString(s) }

// ExtractURLs returns every URL in markdown links and bare URLs.
func ExtractURLs(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mdLinkRE.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	for _, m := range bareURLRE.FindAllString(s, -1) {
		m = strings.TrimRight(m, ".,;:!?")
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}
