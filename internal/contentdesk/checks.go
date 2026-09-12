package contentdesk

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/pkg/moneylink"
)

// BannedRE is agents/content/publish.py:37-41 BANNED, ported verbatim
// (offer/advertiser terms that must never appear in editorial).
var BannedRE = regexp.MustCompile(`(?i)` +
	`national debt relief|metal roofing|empire flooring|jacuzzi|west shore|carshield|` +
	`liberty mutual|tahiti village|a place for mom|3 day blinds|fidelity life|trugreen|` +
	`warby parker|capital wallet|american home warranty|total ?av\b|mutual of omaha|directmeds`)

// moneyHostExtras are the dead-link money hosts the family-lane
// newsletterGuard also refuses (internal/worker/partner_drip_family_lane.go
// newsletterBannedRE). moneylink.Hosts is the live set; the union is what an
// editorial package may never carry. Kept here because contentdesk cannot
// import internal/worker (the worker imports contentdesk).
var moneyHostExtras = []string{"xnonu.com", "jyqye.com"}

// moneyMarkerRE mirrors newsletterBannedRE's non-host markers.
var moneyMarkerRE = regexp.MustCompile(`(?i)everflow|trkclk|/aff/|[?&]aff=`)

var (
	slugRE         = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	trustTextRE    = regexp.MustCompile(`(?i)\b\d(?:\.\d)?\s*(?:out of 5\s*)?stars?\b|\b\d[\d,]*\s+(?:reviews|ratings|testimonials|reviewers)\b|\brated\s+\d`)
	fabricatedKeys = map[string]bool{
		"comments": true, "comment_count": true, "rating": true, "ratings": true,
		"aggregate_rating": true, "aggregaterating": true, "stars": true,
		"testimonials": true, "testimonial": true, "review_count": true,
		"reviews_count": true, "reviewer_count": true, "reviewers": true,
	}
)

// Length limits for package text (characters).
const (
	MaxTitleLen              = 110
	MaxMetaTitleLen          = 60
	MaxMetaDescriptionLen    = 160
	MaxExcerptLen            = 300
	MaxSubjectLen            = 90
	MaxPreheaderLen          = 120
	MaxSlugLen               = 80
	MaxBlockTextLen          = 4000
	MaxSubjects              = 8
	MaxPreheaders            = 8
	DefaultSimhashMaxHamming = 3
)

// OtherArticle is a sibling article on the same site, for near-duplicate
// detection.
type OtherArticle struct {
	ArticleID string
	Body      string
}

// CheckInput is everything the deterministic checks need; no I/O happens in
// RunCodeChecks.
type CheckInput struct {
	Domain            string   // site apex, e.g. "discountblog.com"
	SourceDomains     []string // category allow-list (external links allowed to these)
	Package           Package
	RawOutputs        [][]byte // raw model JSON (draft + package) — scanned for fabricated fields
	Refs              []ClaimRef
	Claims            map[string]Claim // ClaimVersionKey → claim
	Latest            map[string]int   // claim_id → latest version (a ref to an older one is stale)
	Slug              string
	TakenSlugs        []string
	Others            []OtherArticle
	SimhashMaxHamming int
}

// RunCodeChecks runs every deterministic check, in a fixed order.
func RunCodeChecks(in CheckInput) []CheckResult {
	return []CheckResult{
		checkSchema(in.Package),
		checkClaimRefs(in),
		checkNumericSentencesReferenced(in),
		checkCalcs(in),
		checkBannedTerms(in.Package),
		checkMoneyLinks(in.Package),
		checkURLForm(in),
		checkNoRawHTML(in.Package),
		checkSlug(in),
		checkFabricatedTrust(in),
		checkNearDuplicate(in),
		checkLengths(in.Package),
	}
}

func result(name, sev string, details []string) CheckResult {
	return CheckResult{Name: name, Passed: len(details) == 0, Severity: sev, Details: details}
}

func checkSchema(p Package) CheckResult {
	var d []string
	if strings.TrimSpace(p.Title) == "" {
		d = append(d, "title is empty")
	}
	if strings.TrimSpace(p.Excerpt) == "" {
		d = append(d, "excerpt is empty")
	}
	if strings.TrimSpace(p.MetaTitle) == "" {
		d = append(d, "meta_title is empty")
	}
	if strings.TrimSpace(p.MetaDescription) == "" {
		d = append(d, "meta_description is empty")
	}
	if len(p.Blocks) == 0 {
		d = append(d, "no blocks")
	}
	if len(p.Subjects) == 0 {
		d = append(d, "no subjects")
	}
	if len(p.Preheaders) == 0 {
		d = append(d, "no preheaders")
	}
	seen := map[string]bool{}
	for i, b := range p.Blocks {
		where := fmt.Sprintf("block[%d] id=%q", i, b.ID)
		if strings.TrimSpace(b.ID) == "" {
			d = append(d, where+": empty id")
		} else if IsReservedUnitID(b.ID) {
			d = append(d, where+": id collides with a reserved package unit id")
		} else if seen[b.ID] {
			d = append(d, where+": duplicate id")
		}
		seen[b.ID] = true
		if !AllowedBlockTypes[b.Type] {
			d = append(d, fmt.Sprintf("%s: block type %q not allowed", where, b.Type))
			continue
		}
		switch b.Type {
		case "comparison_table":
			if len(b.Rows) < 2 {
				d = append(d, where+": comparison_table needs a header row and at least one data row")
			}
			for ri, r := range b.Rows {
				if len(r) < 2 || (len(b.Rows) > 0 && len(r) != len(b.Rows[0])) {
					d = append(d, fmt.Sprintf("%s: row %d has %d cells (want %d, ≥2)", where, ri, len(r), len(b.Rows[0])))
				}
			}
		case "steps", "key_takeaways", "faq":
			if len(b.Items) == 0 {
				d = append(d, where+": "+b.Type+" needs items")
			}
		case "worked_example":
			if b.Calc == nil {
				d = append(d, where+": worked_example needs calc {inputs, formula, result}")
			}
		default:
			if strings.TrimSpace(b.Text) == "" && len(b.Items) == 0 {
				d = append(d, where+": empty")
			}
		}
	}
	return result("schema", "S1", d)
}

func checkClaimRefs(in CheckInput) CheckResult {
	var d []string
	units := Units(in.Package)
	for i, r := range in.Refs {
		where := fmt.Sprintf("ref[%d] %s#%d → %s@%d", i, r.BlockID, r.SentenceIdx, r.ClaimID, r.Version)
		sents, ok := units[r.BlockID]
		if !ok {
			d = append(d, where+": unit does not exist")
		} else if r.SentenceIdx < 0 || r.SentenceIdx >= len(sents) {
			d = append(d, fmt.Sprintf("%s: sentence_idx out of range (unit has %d)", where, len(sents)))
		}
		c, ok := in.Claims[ClaimVersionKey(r.ClaimID, r.Version)]
		if !ok {
			d = append(d, where+": claim version does not resolve")
			continue
		}
		if c.Status != ClaimSupported {
			d = append(d, fmt.Sprintf("%s: claim status is %q, not supported", where, c.Status))
		}
		if lv := in.Latest[r.ClaimID]; lv > r.Version {
			d = append(d, fmt.Sprintf("%s: stale — version %d exists", where, lv))
		}
	}
	return result("claim_refs_resolve", "S1", d)
}

// codeChecksVersion is part of the code_checks stage's input hash, so a
// changed check never replays a result cached under an old one.
const codeChecksVersion = "2026-09-12.headline-restates"

// numTokenRE finds number tokens ("2", "37" of "37%", "978.52", "1,176").
var numTokenRE = regexp.MustCompile(`\d[\d,.]*\d|\d`)

// checkNumericSentencesReferenced — HEURISTIC: a sentence carrying a digit is
// treated as factual and must reference a claim. The judge covers the rest.
// A headline line — a block heading, or the title, excerpt, meta, subject or
// preheader — whose every number already appears in a cited body sentence
// makes no new numeric claim: it restates one (live :1137/:1138: the history
// pilot, 49/49 references supported, was held on "August 2: The parchment is
// signed" and "July 2 vote, July 4 text, August 2 signing"). A number found
// in no cited body sentence still fails, and body sentences are unchanged.
func checkNumericSentencesReferenced(in CheckInput) CheckResult {
	refd := map[string]bool{}
	for _, r := range in.Refs {
		refd[fmt.Sprintf("%s#%d", r.BlockID, r.SentenceIdx)] = true
	}
	units := Units(in.Package)
	headed := map[string]bool{} // block ids whose unit 0 is a heading
	for _, b := range in.Package.Blocks {
		if strings.TrimSpace(b.Heading) != "" {
			headed[b.ID] = true
		}
	}
	isHeadline := func(id string, i int) bool { return IsReservedUnitID(id) || (headed[id] && i == 0) }
	cited := map[string]bool{} // number tokens in cited body sentences
	for id, sents := range units {
		for i, s := range sents {
			if isHeadline(id, i) || !refd[fmt.Sprintf("%s#%d", id, i)] {
				continue
			}
			for _, t := range numTokenRE.FindAllString(s, -1) {
				cited[t] = true
			}
		}
	}
	restatesCited := func(s string) bool {
		toks := numTokenRE.FindAllString(s, -1)
		if len(toks) == 0 {
			return false
		}
		for _, t := range toks {
			if !cited[t] {
				return false
			}
		}
		return true
	}
	ids := make([]string, 0, len(units))
	for id := range units {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var d []string
	for _, id := range ids {
		for i, s := range units[id] {
			if !digitRE.MatchString(s) || refd[fmt.Sprintf("%s#%d", id, i)] {
				continue
			}
			if isHeadline(id, i) && restatesCited(s) {
				continue
			}
			d = append(d, fmt.Sprintf("%s#%d has a number but no claim ref: %q", id, i, clip(s, 100)))
		}
	}
	r := result("numeric_sentences_referenced", "S2", d)
	r.Heuristic = true
	return r
}

func checkCalcs(in CheckInput) CheckResult {
	var d []string
	for _, b := range in.Package.Blocks {
		if b.Calc == nil {
			continue
		}
		if got, ok, err := CalcMatches(*b.Calc); err != nil {
			d = append(d, fmt.Sprintf("block %s: formula %q: %v", b.ID, b.Calc.Formula, err))
		} else if !ok {
			d = append(d, fmt.Sprintf("block %s: %q recomputes to %.4f, stated %.4f", b.ID, b.Calc.Formula, got, b.Calc.Result))
		}
	}
	seen := map[string]bool{}
	for _, r := range in.Refs {
		k := ClaimVersionKey(r.ClaimID, r.Version)
		c, ok := in.Claims[k]
		if !ok || seen[k] || c.Type != ClaimCalculation {
			continue
		}
		seen[k] = true
		if c.Calc == nil {
			d = append(d, fmt.Sprintf("claim %s: calculation claim has no calc", k))
			continue
		}
		if got, ok, err := CalcMatches(*c.Calc); err != nil {
			d = append(d, fmt.Sprintf("claim %s: formula %q: %v", k, c.Calc.Formula, err))
		} else if !ok {
			d = append(d, fmt.Sprintf("claim %s: %q recomputes to %.4f, stated %.4f", k, c.Calc.Formula, got, c.Calc.Result))
		}
	}
	return result("calc_recompute", "S1", d)
}

func checkBannedTerms(p Package) CheckResult {
	var d []string
	for _, s := range AllText(p) {
		if m := BannedRE.FindString(s); m != "" {
			d = append(d, fmt.Sprintf("banned term %q in %q", m, clip(s, 80)))
		}
	}
	return result("banned_terms", "S1", d)
}

func allMoneyHosts() []string {
	return append(append([]string(nil), moneylink.Hosts...), moneyHostExtras...)
}

func checkMoneyLinks(p Package) CheckResult {
	var d []string
	hosts := allMoneyHosts()
	for _, s := range AllText(p) {
		low := strings.ToLower(s)
		for _, h := range hosts {
			if strings.Contains(low, h) {
				d = append(d, fmt.Sprintf("money host %s in %q", h, clip(s, 80)))
			}
		}
		if m := moneyMarkerRE.FindString(s); m != "" {
			d = append(d, fmt.Sprintf("affiliate marker %q in %q", m, clip(s, 80)))
		}
		for _, u := range ExtractURLs(s) {
			if pu, err := url.Parse(u); err == nil && (strings.HasPrefix(pu.Path, "/o/") || strings.Contains(pu.Path, "/o/")) {
				d = append(d, fmt.Sprintf("/o/ offer-gateway link %s", u))
			}
		}
	}
	return result("no_money_links", "S1", d)
}

func hostAllowed(host string, allow []string) bool {
	for _, a := range allow {
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

func checkURLForm(in CheckInput) CheckResult {
	var d []string
	apex := strings.ToLower(strings.TrimPrefix(in.Domain, "www."))
	for _, s := range AllText(in.Package) {
		for _, raw := range ExtractURLs(s) {
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" {
				d = append(d, fmt.Sprintf("unparseable URL %q", raw))
				continue
			}
			host := strings.ToLower(u.Host)
			if u.Scheme != "https" {
				d = append(d, fmt.Sprintf("%s: not https", raw))
			}
			if host == apex || host == "www."+apex {
				if host != "www."+apex {
					d = append(d, fmt.Sprintf("%s: own-site links must be https://www.%s/…", raw, apex))
				}
				if u.Path == "" || u.Path == "/" {
					d = append(d, fmt.Sprintf("%s: own-site link has no path", raw))
				} else if strings.HasSuffix(u.Path, "/") {
					d = append(d, fmt.Sprintf("%s: trailing slash (harvest parity needs none)", raw))
				}
				continue
			}
			if !hostAllowed(host, in.SourceDomains) {
				d = append(d, fmt.Sprintf("%s: external host %s is not on the category allow-list", raw, host))
			}
		}
	}
	return result("url_form", "S1", d)
}

func checkNoRawHTML(p Package) CheckResult {
	var d []string
	for _, s := range AllText(p) {
		if HasRawHTML(s) {
			d = append(d, fmt.Sprintf("raw HTML in %q", clip(s, 80)))
		}
	}
	return result("no_raw_html", "S1", d)
}

func checkSlug(in CheckInput) CheckResult {
	var d []string
	if !slugRE.MatchString(in.Slug) {
		d = append(d, fmt.Sprintf("slug %q is not lowercase-hyphenated", in.Slug))
	}
	if len(in.Slug) > MaxSlugLen {
		d = append(d, fmt.Sprintf("slug is %d chars (max %d)", len(in.Slug), MaxSlugLen))
	}
	for _, t := range in.TakenSlugs {
		if strings.EqualFold(strings.TrimSpace(t), in.Slug) {
			d = append(d, fmt.Sprintf("slug %q already exists on the site", in.Slug))
			break
		}
	}
	return result("slug_unique", "S1", d)
}

func scanKeys(v any, path string, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if fabricatedKeys[strings.ToLower(k)] {
				*out = append(*out, fmt.Sprintf("fabricated-trust field %q at %s", k, path))
			}
			scanKeys(t[k], path+"."+k, out)
		}
	case []any:
		for i, e := range t {
			scanKeys(e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

func checkFabricatedTrust(in CheckInput) CheckResult {
	var d []string
	for i, raw := range in.RawOutputs {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		scanKeys(v, fmt.Sprintf("output[%d]", i), &d)
	}
	for _, s := range AllText(in.Package) {
		if m := trustTextRE.FindString(s); m != "" {
			d = append(d, fmt.Sprintf("trust claim %q in %q", m, clip(s, 80)))
		}
	}
	return result("no_fabricated_trust", "S1", d)
}

func checkNearDuplicate(in CheckInput) CheckResult {
	max := in.SimhashMaxHamming
	if max <= 0 {
		max = DefaultSimhashMaxHamming
	}
	mine := Simhash(PackageBodyText(in.Package))
	var d []string
	for _, o := range in.Others {
		if h := Hamming(mine, Simhash(o.Body)); h <= max {
			d = append(d, fmt.Sprintf("simhash distance %d ≤ %d to article %s", h, max, o.ArticleID))
		}
	}
	r := result("near_duplicate", "S2", d)
	r.Heuristic = true
	return r
}

func checkLengths(p Package) CheckResult {
	var d []string
	lim := func(name, s string, max int) {
		if n := len([]rune(s)); n > max {
			d = append(d, fmt.Sprintf("%s is %d chars (max %d)", name, n, max))
		}
	}
	lim("title", p.Title, MaxTitleLen)
	lim("meta_title", p.MetaTitle, MaxMetaTitleLen)
	lim("meta_description", p.MetaDescription, MaxMetaDescriptionLen)
	lim("excerpt", p.Excerpt, MaxExcerptLen)
	if len(p.Subjects) > MaxSubjects {
		d = append(d, fmt.Sprintf("%d subjects (max %d)", len(p.Subjects), MaxSubjects))
	}
	if len(p.Preheaders) > MaxPreheaders {
		d = append(d, fmt.Sprintf("%d preheaders (max %d)", len(p.Preheaders), MaxPreheaders))
	}
	for i, s := range p.Subjects {
		lim(SubjectUnit(i), s, MaxSubjectLen)
	}
	for i, s := range p.Preheaders {
		lim(PreheaderUnit(i), s, MaxPreheaderLen)
	}
	for _, b := range p.Blocks {
		lim("block "+b.ID, b.Text, MaxBlockTextLen)
	}
	return result("length_limits", "S1", d)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
