package contentdesk

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// System prompts are STABLE per stage (cached); everything per-article goes
// in the user prompt.

const researchSystem = `You are the research desk of a consumer publication. You establish facts for one article; you do not write it.

Rules:
- Use web_search and web_fetch. Only the allowed source domains are reachable; never cite anything else.
- Every sourced_fact must carry the exact source_url you fetched and a VERBATIM passage (at most ~600 characters) that states the fact. Paraphrase goes in "text", never in "passage".
- Record scope for every claim: published_at (source date as printed), effective_at (when the rule/number applies), jurisdiction, population (who it applies to) and conditions (exceptions, thresholds, caveats). Use "" when genuinely not stated.
- Never invent a source, passage, number, date or eligibility rule. If you cannot support something the reader question needs, still emit it with status "insufficient_evidence". If sources disagree, emit status "conflicting" and say why in context.
- type: sourced_fact (stated by a source), calculation (derived arithmetically — give calc {inputs:[{name,value}], formula, result} using only + - * / ^, parentheses and input names), assumption (a stated premise for an example), interpretation (a reading of sources; say which in context).
- key=true for every claim about numbers, eligibility, deadlines, money, health or safety. For key claims give claim_key (snake_case), a neutral "question" that does NOT contain the answer, and "answer": the bare canonical value (a number, date, or yes/no phrase).
- When research is complete, call submit_research exactly once.`

const rederiveSystem = `You are an independent verifier. You receive QUESTIONS only — nobody's answers. Establish each answer yourself from primary sources.

Rules:
- Use web_search and web_fetch; only the allowed source domains are reachable.
- For each question return question_index, a one-sentence "text", the bare canonical "answer" (number, date, or yes/no phrase), the exact source_url, a VERBATIM passage, and scope (published_at, effective_at, jurisdiction, population, conditions).
- status "supported" only when the passage states the answer; otherwise "insufficient_evidence" or "conflicting". Never guess.
- When done, call submit_rederivation exactly once.`

// sentenceRules is shared by the draft and package prompts — it is the
// contract SplitSentences/BlockSentences implement.
const sentenceRules = `Citations — cite with inline markers. Code computes every position, so never write sentence indexes or a claim_refs list:
- Put [[c:<claim_id>@<version>]] right after the words it supports, before the sentence's closing punctuation, e.g. "The APR includes the origination fee [[c:0f8e2a1c-…@1]]." Several markers may follow one sentence; one marker per claim.
- Every factual sentence — anything a reader could check, and every sentence containing a number — carries at least one marker, in body text, items, table rows and captions, and in title, excerpt, meta_description, subjects and preheaders alike.
- A marker's claim_id must be a listed claim with status=supported, with its exact version. If a point has no supported claim, leave it out or say plainly that it could not be confirmed. Never write "N/A" or an empty id.
- A sentence ends at ".", "!" or "?" followed by a space. Avoid abbreviations with periods ("e.g.") inside cited sentences.`

const draftSystem = `You write evidence-bound consumer articles for a brand site. A human editor reviews every word before anything publishes.

Output typed blocks only. Allowed types: lede, section, key_takeaways, worked_example, document_anatomy, stat, comparison_table, steps, faq, pull_quote, callout.
- Length: 700–1,400 words across all blocks, at most 12 blocks, at most 6 faq pairs. Answer the reader's question and stop; depth comes from the claims, not from restating them.
- Text is plain text plus only this inline markdown: **bold**, *italic*, [label](https://…). NO HTML, no tags, no entities.
- comparison_table: "rows" with a header row first, every row the same number of cells (≥2).
- steps / key_takeaways / faq: use "items" (faq items alternate question, answer).
- worked_example: must carry calc {inputs:[{name,value}], formula, result}; the formula uses only + - * / ^, parentheses and input names, and result must equal the formula.
- Cite every factual sentence — anything a reader could check, and every sentence containing a number — with an inline claim marker as the citation rules describe. Reference ONLY claims whose status is "supported". If a point has no supported claim, leave it out or say plainly that it could not be confirmed.
- Keep each claim's scope: never drop its jurisdiction, population, effective date or conditions when you restate it.
- Links to the site itself use exactly https://www.<site domain>/<path> with no trailing slash. External links only to the claim source URLs. No advertiser or brand names, no offers, no affiliate links.
- Never invent ratings, stars, reviews, testimonials, comments, reader counts or quotes.
Unused fields: "" for strings, [] for arrays, null for calc.`

const packageSystem = `You package an approved-for-review article draft: headline, excerpt, SEO meta and email subject lines/preheaders.
- The title must not promise more than the body delivers; no clickbait, no urgency you cannot support.
- meta_title ≤ 60 characters, meta_description ≤ 160, excerpt ≤ 300, each subject ≤ 90, each preheader ≤ 120. Give 3–5 subjects and 3–5 preheaders.
- Plain text only, no HTML. No advertiser names, offers, ratings or testimonials.
- Any title/excerpt/meta/subject/preheader sentence that states a fact or a number carries an inline marker [[c:<claim_id>@<version>]] for a supported claim, as the citation rules describe.`

const judgeSystem = `You are the standards editor. You check every sentence that restates a claim against that claim's source passage and scope.

For each numbered reference decide:
- supported: the sentence (read with its paragraph) says no more than the passage, within the claim's jurisdiction, population, effective date and conditions.
- overstated: it drops or loosens a qualifier (scope, condition, date, "up to", "may") — name the lost qualifier in lost_qualifier.
- unsupported: the passage does not state it.
Return exactly one item per reference index.

Then add flags for: headline_overpromise (title/excerpt/subject promises more than the body), omitted_exception (an exception in a passage the reader needs but the article omits), low_usefulness (a section only restates the source without helping the reader act), unreferenced_claim (a checkable factual sentence with no reference). Point each flag at block_id and sentence_idx. Be strict; silence is not approval.`

// ── schema helpers (structured outputs: every object closed, all
// properties required) ──

func sStr() map[string]any  { return map[string]any{"type": "string"} }
func sNum() map[string]any  { return map[string]any{"type": "number"} }
func sInt() map[string]any  { return map[string]any{"type": "integer"} }
func sBool() map[string]any { return map[string]any{"type": "boolean"} }
func sEnum(vals ...string) map[string]any {
	return map[string]any{"type": "string", "enum": vals}
}
func sArr(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}
func sNullable(s map[string]any) map[string]any {
	return map[string]any{"anyOf": []any{s, map[string]any{"type": "null"}}}
}
func sObj(props map[string]any) map[string]any {
	req := make([]string, 0, len(props))
	for k := range props {
		req = append(req, k)
	}
	sort.Strings(req)
	return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
}

func calcSchema() map[string]any {
	return sObj(map[string]any{
		"inputs":  sArr(sObj(map[string]any{"name": sStr(), "value": sNum()})),
		"formula": sStr(),
		"result":  sNum(),
	})
}

func claimRefSchema() map[string]any {
	return sObj(map[string]any{"block_id": sStr(), "sentence_idx": sInt(), "claim_id": sStr(), "version": sInt()})
}

func scopeProps(p map[string]any) map[string]any {
	for _, k := range []string{"source_url", "passage", "context", "published_at", "effective_at", "jurisdiction", "population", "conditions"} {
		p[k] = sStr()
	}
	p["status"] = sEnum(ClaimSupported, ClaimInsufficientEvidence, ClaimConflicting)
	return p
}

func researchSchema() map[string]any {
	claim := sObj(scopeProps(map[string]any{
		"text":      sStr(),
		"type":      sEnum(ClaimSourcedFact, ClaimCalculation, ClaimAssumption, ClaimInterpretation),
		"key":       sBool(),
		"claim_key": sStr(),
		"question":  sStr(),
		"answer":    sStr(),
		"calc":      sNullable(calcSchema()),
	}))
	return sObj(map[string]any{"claims": sArr(claim)})
}

func rederiveSchema() map[string]any {
	ans := sObj(scopeProps(map[string]any{"question_index": sInt(), "text": sStr(), "answer": sStr()}))
	return sObj(map[string]any{"answers": sArr(ans)})
}

func blockTypeList() []string {
	out := make([]string, 0, len(AllowedBlockTypes))
	for t := range AllowedBlockTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func blockSchema() map[string]any {
	return sObj(map[string]any{
		"id": sStr(), "type": sEnum(blockTypeList()...), "heading": sStr(), "text": sStr(),
		"items": sArr(sStr()), "rows": sArr(sArr(sStr())), "calc": sNullable(calcSchema()), "caption": sStr(),
	})
}

func draftSchema() map[string]any {
	// No claim_refs: the writer cites with inline markers and code computes
	// the refs (citations.go).
	return sObj(map[string]any{"blocks": sArr(blockSchema())})
}

// reviseBlockSchema is one rewritten block (per-block revise).
func reviseBlockSchema() map[string]any {
	return sObj(map[string]any{"block": blockSchema()})
}

func packageSchema() map[string]any {
	return sObj(map[string]any{
		"title": sStr(), "excerpt": sStr(), "meta_title": sStr(), "meta_description": sStr(),
		"subjects": sArr(sStr()), "preheaders": sArr(sStr()),
	})
}

func judgmentSchema() map[string]any {
	item := sObj(map[string]any{
		"ref_index": sInt(), "verdict": sEnum("supported", "overstated", "unsupported"),
		"lost_qualifier": sStr(), "note": sStr(),
	})
	kinds := make([]string, 0, len(judgeFlagKinds))
	for k := range judgeFlagKinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	flag := sObj(map[string]any{"kind": sEnum(kinds...), "block_id": sStr(), "sentence_idx": sInt(), "note": sStr()})
	return sObj(map[string]any{"items": sArr(item), "flags": sArr(flag)})
}

// ── prompt builders ──

func briefBlock(in PipelineInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Site: %s (%s)\n", in.Site.Domain, in.Site.BrandCode)
	fmt.Fprintf(&b, "Format: %s\nCategory: %s\nReader question: %s\n", in.Brief.Format, in.Brief.Category, in.Brief.ReaderQuestion)
	if in.Brief.Angle != "" {
		fmt.Fprintf(&b, "Angle: %s\n", in.Brief.Angle)
	}
	if len(in.Brief.Outline) > 0 && string(in.Brief.Outline) != "[]" {
		fmt.Fprintf(&b, "Outline: %s\n", in.Brief.Outline)
	}
	return b.String()
}

func researchPrompt(in PipelineInput) string {
	return fmt.Sprintf("Today is %s.\n%s\nAllowed sources: %s\n\nEstablish every fact this article needs to answer the reader question. Submit with submit_research.",
		time.Now().UTC().Format("2006-01-02"), briefBlock(in), strings.Join(CategoryAllowlist[in.Brief.Category], ", "))
}

func rederivePrompt(in PipelineInput, keys []researchRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Today is %s.\nCategory: %s\nAllowed sources: %s\n\nQuestions:\n", time.Now().UTC().Format("2006-01-02"),
		in.Brief.Category, strings.Join(CategoryAllowlist[in.Brief.Category], ", "))
	for i, k := range keys {
		fmt.Fprintf(&b, "%d. %s\n", i, k.Question)
	}
	b.WriteString("\nAnswer each by question_index. Submit with submit_rederivation.")
	return b.String()
}

func claimLines(claims []Claim) string {
	var b strings.Builder
	for _, c := range claims {
		fmt.Fprintf(&b, "- claim_id=%s version=%d status=%s type=%s\n  text: %s\n", c.ClaimID, c.Version, c.Status, c.Type, c.Text)
		if c.Passage != "" {
			fmt.Fprintf(&b, "  passage: %q\n  source: %s\n", clip(c.Passage, 600), c.SourceURL)
		}
		if c.Jurisdiction+c.Population+c.Conditions+c.EffectiveAt != "" {
			fmt.Fprintf(&b, "  scope: jurisdiction=%q population=%q conditions=%q effective=%q\n", c.Jurisdiction, c.Population, c.Conditions, c.EffectiveAt)
		}
		if c.Calc != nil {
			cj, _ := json.Marshal(c.Calc)
			fmt.Fprintf(&b, "  calc: %s\n", cj)
		}
	}
	return b.String()
}

func draftPrompt(in PipelineInput, claims []Claim) string {
	voice := string(in.Site.Voice)
	if voice == "" || voice == "{}" {
		voice = "(no voice guide — plain, warm, specific, second person)"
	}
	// Only supported claims are shown. Listing conflicting/insufficient ones
	// (with a "do not cite" status) still got them cited — 2026-09-11 every
	// pilot draft failed claim_refs_resolve on a "conflicting" claim.
	var supported []Claim
	for _, c := range claims {
		if c.Status == ClaimSupported {
			supported = append(supported, c)
		}
	}
	return fmt.Sprintf("%s\nSite voice: %s\nSite links: https://www.%s/<path>\n\n%s\n\nClaims (all supported — cite only these; anything not listed could not be confirmed):\n%s",
		briefBlock(in), voice, strings.TrimPrefix(in.Site.Domain, "www."), sentenceRules, claimLines(supported))
}

func packagePrompt(in PipelineInput, d draftResult, claims []Claim) string {
	var body strings.Builder
	for _, bl := range d.Blocks {
		fmt.Fprintf(&body, "[%s:%s] %s\n", bl.ID, bl.Type, BlockParagraph(bl))
	}
	var supported []Claim
	for _, c := range claims {
		if c.Status == ClaimSupported {
			supported = append(supported, c)
		}
	}
	return fmt.Sprintf("%s\n%s\n\nArticle body:\n%s\nSupported claims:\n%s", briefBlock(in), sentenceRules, body.String(), claimLines(supported))
}

// packageRevisePromptVersion is part of the package stage's input hash when
// the package is re-run with findings.
const packageRevisePromptVersion = "2026-09-11.1"

// packageRevisePrompt re-packages with the previous package and the findings
// on its units (title, excerpt, meta, subjects, preheaders). The block reviser
// never touches those units, so without this their findings could not clear.
func packageRevisePrompt(in PipelineInput, d draftResult, claims []Claim, prev Package, fs []reviseFinding) string {
	pj, _ := json.Marshal(map[string]any{"title": prev.Title, "excerpt": prev.Excerpt, "meta_title": prev.MetaTitle,
		"meta_description": prev.MetaDescription, "subjects": prev.Subjects, "preheaders": prev.Preheaders})
	fj, _ := json.Marshal(fs)
	return packagePrompt(in, d, claims) + fmt.Sprintf("\n\nThe previous package and the standards editor's findings on it. Fix each finding at its cause: narrow a headline or excerpt to exactly what the body delivers, cite a number with a marker or cut it, restore a lost qualifier. Keep what has no finding.\n%s\n\nFindings (unit ids: title, excerpt, meta_title, meta_description, subject:N, preheader:N):\n%s", pj, fj)
}

// adjudicateSystem is the managing editor: it decides whether editorial flags
// and heuristic check failures may stand. It is never shown claim verdicts,
// unreferenced factual sentences, or hard check failures — those cannot be
// accepted by anyone short of a new revision.
const adjudicateSystem = `You are the managing editor. The standards editor and the automated checks raised the items below on an article whose facts are already sourced. For each item, decide whether the article may be published with it as it stands.
- Accept an item only if it is mistaken, or immaterial: a careful reader would not be misled or left worse informed by the text as written. The reason must name the text you relied on.
- Do not accept an item because it is small, common practice, or easy to fix later. If a headline promises more than the body delivers, a material exception or limit is missing, or the article does not answer its reader question, do not accept.
- For "has a number but no claim ref": accept a number that is arithmetic on the article's own worked example, a count of the article's own sections, or a restatement of a body sentence that carries a citation. Do not accept a number that states a fact about the world.
- Decide every item. Output only the decisions.`

func adjudicationSchema() map[string]any {
	return sObj(map[string]any{"decisions": sArr(sObj(map[string]any{"id": sStr(), "accept": sBool(), "reason": sStr()}))})
}

func judgePrompt(pkg Package, refs []ClaimRef, claims map[string]Claim) string {
	units := Units(pkg)
	var b strings.Builder
	fmt.Fprintf(&b, "There are %d references, numbered #0 to #%d. Return exactly %d items, one for every ref_index, supported references included. A reference left out is recorded as unsupported and the judgment is re-run.\n\nReferences to judge:\n",
		len(refs), len(refs)-1, len(refs))
	for i, r := range refs {
		sent := ""
		if s := units[r.BlockID]; r.SentenceIdx >= 0 && r.SentenceIdx < len(s) {
			sent = s[r.SentenceIdx]
		}
		c := claims[ClaimVersionKey(r.ClaimID, r.Version)]
		fmt.Fprintf(&b, "\n#%d  unit=%s sentence_idx=%d\n  sentence: %q\n  paragraph: %q\n  claim: %s\n  passage: %q\n  scope: jurisdiction=%q population=%q conditions=%q effective=%q\n",
			i, r.BlockID, r.SentenceIdx, sent, clip(UnitParagraph(pkg, r.BlockID), 1200), c.Text, clip(c.Passage, 800),
			c.Jurisdiction, c.Population, c.Conditions, c.EffectiveAt)
	}
	b.WriteString("\nFull package for flags (unit id: sentences):\n")
	ids := make([]string, 0, len(units))
	for id := range units {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for i, s := range units[id] {
			fmt.Fprintf(&b, "%s#%d: %s\n", id, i, s)
		}
	}
	return b.String()
}
