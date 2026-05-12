package api

// Send-Day creative resolve — Phase 1e of the Send-Day Planner plan.
//
// Ports the Python helpers `apply_full_pipeline` (may01_link_rewriter.py)
// and `inject_brand_domain` (apr30_brand_footers.py) to Go so the new
// canvas can ship the same mutated HTML the deploy_may12_mature.py
// script ships. Without this port, the canvas would have to either
// re-implement the regex pipeline in TypeScript (drift risk) or POST
// raw HTML and trust the canvas to pre-mutate (also drift risk).
//
// The endpoint accepts the source HTML two ways:
//
//   1. POST body { "raw_html": "..." }            ← preferred
//   2. POST body { "filename": "warby-...html" }  ← attempts disk load
//      from $CREATIVE_DIR (default: ./docs/emails)
//
// Mode 1 always works in ECS where docs/emails isn't packaged; mode 2
// is the local-dev convenience that mirrors the Python script's own
// REPO_ROOT/docs/emails behavior.
//
// Returns:
//   {
//     api_version, filename, family, brand, slot,
//     subject, preheader,
//     html_content,         // post-pipeline; what the wizard's
//                           // variants[0].html_content should carry
//     html_kb,
//     diagnostics: { ... }  // money_link_residual, cratoolpro_count,
//                           // tracking_tagged, has_brand_footer
//   }

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ─── Brand metadata (DB / QF / HT / MH — the 4 mature brands) ────────────────
//
// Port of `apr30_brand_footers.BRAND_DOMAIN`, `_BRAND_LABEL`, and
// `_BRAND_ADDRESS`. Keep brand keys identical to the Python — DB / QF / HT / MH.
// The 11 May-2026 onward brands are intentionally OUT OF SCOPE for the
// initial Send-Day Planner milestone (mature_only).
var matureBrandDomain = map[string]string{
	"DB": "discountblog.com",
	"QF": "quizfiesta.com",
	"HT": "historythinking.com",
	"MH": "myownhealth.net",
}

var matureBrandLabel = map[string]string{
	"DB": "Discount Blog",
	"QF": "Quiz Fiesta",
	"HT": "HistoryThinking",
	"MH": "My Own Health",
}

var matureBrandAddress = map[string]string{
	"DB": "784 S. Clearwater Loop, Ste. R, Post Falls, ID 83854",
	"QF": "1309 Coffeen Avenue STE 1200, Sheridan, WY 82801",
	"HT": "1309 Coffeen Ave STE 1200, Sheridan, WY 82801",
	"MH": "784 S. Clearwater Loop, Ste. R, Post Falls, ID 83854",
}

// matureBrandTemplateCode mirrors deploy_may12_mature.TEMPLATE_CODE so the
// resolver can derive a default filename for a (family, brand, date)
// triple in disk-load mode.
var matureBrandTemplateCode = map[string]string{
	"DB": "db",
	"QF": "qf",
	"HT": "ht",
	"MH": "mh",
}

// ─── Subject + preheader catalog (mirrors deploy_may12_mature) ───────────────
//
// engagerCopy / welcomeCopy port the ENGAGER_FAMILY_COPY and
// WELCOME_FAMILY_COPY tables from deploy_may12_mature.py. The canvas
// uses these so its dry-run output matches the Python script byte-for-byte
// for the same (slot, family) input.
type subjectPreheader struct {
	Subject   string
	Preheader string
}

var engagerFamilyCopy = map[string]subjectPreheader{
	"warby-parker": {
		Subject:   "Last chance: this spring's frames are flying off shelves",
		Preheader: "Limited stock on the 3 shapes we're seeing most this month — see what's left.",
	},
	"quicken-loans": {
		Subject:   "Rates just moved again — your HELOC window is closing",
		Preheader: "What this week's rate shift means for borrowing against your home.",
	},
	"the-capital-wallet": {
		Subject:   "You're losing money this month and don't know it yet",
		Preheader: "Three line items on your bills that are quietly inflating — fix them in 5 minutes.",
	},
	"north-star-loans": {
		Subject:   "Balance transfer offers expire faster than the rate suggests",
		Preheader: "What the promo window really costs once it ends — don't get caught.",
	},
}

var welcomeFamilyCopy = map[string]subjectPreheader{
	"warby-parker": {
		Subject:   "What this season's frames quietly say about you",
		Preheader: "A short look at the shapes and tints showing up most this spring.",
	},
	"quicken-loans": {
		Subject:   "How home equity actually moves when rates do",
		Preheader: "A plain-English breakdown of when borrowing against your home is and isn't worth it.",
	},
	"the-capital-wallet": {
		Subject:   "Where most monthly waste is hiding",
		Preheader: "Three line items worth a five-minute audit before next month's statements.",
	},
	"north-star-loans": {
		Subject:   "Reading the fine print on balance-transfer offers",
		Preheader: "What the introductory rate really costs once the promo window ends.",
	},
}

// ─── Pipeline helpers (port of may01_link_rewriter.apply_full_pipeline) ──────

const moneyLinkSentinel = " this is the money link"

// cratoolproHrefRE matches cratoolpro money URLs but excludes the
// `integration/` unsub1 URLs that carry their own ?_redir= tokens we must
// NOT mutate. Group 1 = base URL through the path, group 2 = optional
// query string. Mirrors `_CRATOOLPRO_HREF` in the Python.
var cratoolproHrefRE = regexp.MustCompile(
	`(?i)(https?://(?:www\.)?cratoolpro\.com/(?:[A-Za-z0-9_/.\-]+))(\?[^"'\s>]*)?`,
)

// publisherFooterDetectorRE mirrors `_PUBLISHER_FOOTER_DETECTOR`.
var publisherFooterDetectorRE = regexp.MustCompile(
	`(?i)(?:class\s*=\s*["'][^"']*\bpublisher-footer\b|<!--\s*BRAND_PUBLISHER_FOOTER\s*-->)`,
)

// knownBrandAddresses mirrors `_KNOWN_BRAND_ADDRESSES`.
var knownBrandAddresses = []string{
	"784 S. Clearwater Loop, Ste. R, Post Falls, ID 83854",
	"1309 Coffeen Ave STE 1200, Sheridan, WY 82801",
	"1309 Coffeen Avenue STE 1200, Sheridan, WY 82801",
}

func stripMoneyLinkText(html string) string {
	return strings.ReplaceAll(html, moneyLinkSentinel, "")
}

// appendTrackingToCratoolpro adds `?source_id=email&sub1={{subscriber.id}}
// &sub2=<brand.domain>` to every cratoolpro money URL that does NOT
// already carry `source_id=email`. Idempotent — re-running on already-
// tracked HTML is a no-op (matches Python).
func appendTrackingToCratoolpro(html, brand string) (string, error) {
	domain, ok := matureBrandDomain[brand]
	if !ok {
		return html, fmt.Errorf("unknown brand for cratoolpro tracking: %q", brand)
	}
	suffixNoQmark := fmt.Sprintf("source_id=email&sub1={{subscriber.id}}&sub2=%s", domain)

	// We need a path-aware replace because Go regexp ReplaceAllStringFunc
	// gives us each match string but not its capture groups directly. So
	// loop with FindAllStringSubmatchIndex and rebuild the string.
	matches := cratoolproHrefRE.FindAllStringSubmatchIndex(html, -1)
	if len(matches) == 0 {
		return html, nil
	}
	var b strings.Builder
	b.Grow(len(html) + len(matches)*len(suffixNoQmark))
	cursor := 0
	for _, m := range matches {
		// m: [matchStart, matchEnd, g1Start, g1End, g2Start, g2End]
		matchStart, matchEnd := m[0], m[1]
		g1Start, g1End := m[2], m[3]
		g2Start, g2End := m[4], m[5]

		base := html[g1Start:g1End]
		// Skip the unsub1 URLs (these carry ?_redir= tokens). Python uses
		// a negative lookahead in the regex; Go regexp doesn't support
		// lookaheads so we filter post-match.
		if strings.Contains(base, "/integration/") {
			b.WriteString(html[cursor:matchEnd])
			cursor = matchEnd
			continue
		}
		qs := ""
		if g2Start >= 0 && g2End >= 0 {
			qs = html[g2Start:g2End]
		}
		b.WriteString(html[cursor:matchStart])
		switch {
		case qs != "" && strings.Contains(qs, "source_id=email"):
			// Idempotent: already tracked, leave alone.
			b.WriteString(base)
			b.WriteString(qs)
		case qs != "":
			b.WriteString(base)
			b.WriteString(qs)
			joiner := "&"
			if strings.HasSuffix(qs, "&") {
				joiner = ""
			}
			b.WriteString(joiner)
			b.WriteString(suffixNoQmark)
		default:
			b.WriteString(base)
			b.WriteString("?")
			b.WriteString(suffixNoQmark)
		}
		cursor = matchEnd
	}
	b.WriteString(html[cursor:])
	return b.String(), nil
}

// hasExistingBrandFooter mirrors Python `_has_existing_brand_footer`.
func hasExistingBrandFooter(html string) bool {
	if publisherFooterDetectorRE.MatchString(html) {
		return true
	}
	if !strings.Contains(html, "{{ system.unsubscribe_url }}") &&
		!strings.Contains(html, "{{system.unsubscribe_url}}") {
		return false
	}
	for _, addr := range knownBrandAddresses {
		if strings.Contains(html, addr) {
			return true
		}
	}
	return false
}

const (
	inlineFooterDivStyle  = "max-width:600px;margin:24px auto 0 auto;padding:18px 16px;border-top:1px solid #d8d8d8;font-family:Arial,Helvetica,sans-serif;font-size:11px;line-height:1.5;color:#555;text-align:center;"
	inlineFooterLinkStyle = "color:#555;text-decoration:underline;"
)

func buildInlineFooter(brand string) (string, error) {
	name, ok := matureBrandLabel[brand]
	if !ok {
		return "", fmt.Errorf("unknown brand for footer: %q", brand)
	}
	addr := matureBrandAddress[brand]
	domain := matureBrandDomain[brand]
	return fmt.Sprintf(
		`<div style="%s">You received this email because you subscribed to %s to receive offers and promotions.<br />`+
			`If you no longer wish to receive these messages, you can `+
			`<a href="{{ system.unsubscribe_url }}" style="%s">unsubscribe globally here</a>`+
			` or `+
			`<a href="{{ system.preferences_url }}" style="%s">manage your preferences</a>.<br /><br />`+
			`%s &middot; %s<br />`+
			`&copy; {{ system.current_year }} %s. All rights reserved.</div>`,
		inlineFooterDivStyle,
		name,
		inlineFooterLinkStyle,
		inlineFooterLinkStyle,
		name, addr,
		domain,
	), nil
}

// appendBrandPublisherFooter mirrors the Python helper of the same name.
func appendBrandPublisherFooter(html, brand string) (string, error) {
	if _, ok := matureBrandDomain[brand]; !ok {
		return html, fmt.Errorf("unknown brand: %q", brand)
	}
	if hasExistingBrandFooter(html) {
		return html, nil
	}
	footer, err := buildInlineFooter(brand)
	if err != nil {
		return html, err
	}
	if strings.Contains(html, "</body>") {
		return strings.Replace(html, "</body>", footer+"</body>", 1), nil
	}
	return html + footer, nil
}

// injectBrandDomain replaces literal `{{brand.domain}}` tokens with the
// per-brand root domain. Idempotent.
func injectBrandDomain(html, brand string) (string, error) {
	domain, ok := matureBrandDomain[brand]
	if !ok {
		return html, fmt.Errorf("unknown brand for domain substitution: %q", brand)
	}
	return strings.ReplaceAll(html, "{{brand.domain}}", domain), nil
}

// applyFullPipeline runs the Python pipeline order: strip → tracking →
// footer → brand-domain. Output MUST byte-equal the Python output for
// any (html, brand) input the canvas might exercise.
func applyFullPipeline(html, brand string) (string, error) {
	html = stripMoneyLinkText(html)
	out, err := appendTrackingToCratoolpro(html, brand)
	if err != nil {
		return out, err
	}
	out, err = appendBrandPublisherFooter(out, brand)
	if err != nil {
		return out, err
	}
	out, err = injectBrandDomain(out, brand)
	if err != nil {
		return out, err
	}
	return out, nil
}

// ─── Subject/preheader derivation ────────────────────────────────────────────

// deriveCopyForSlotFamily mirrors deploy_may12_mature.derive_copy().
func deriveCopyForSlotFamily(slot, family string) (subjectPreheader, error) {
	switch slot {
	case "welcome-newsletter":
		if cp, ok := welcomeFamilyCopy[family]; ok {
			return cp, nil
		}
	case "eng-w1", "eng-w2", "eng-w3":
		if cp, ok := engagerFamilyCopy[family]; ok {
			return cp, nil
		}
	}
	return subjectPreheader{}, fmt.Errorf("no copy entry for slot=%s family=%s", slot, family)
}

// ─── Handler ─────────────────────────────────────────────────────────────────

type creativeResolveBody struct {
	Slot     string `json:"slot"`
	Brand    string `json:"brand"`
	Date     string `json:"date"`           // YYYY-MM-DD (used to derive filename when raw_html absent)
	Family   string `json:"family"`         // optional; required if raw_html supplied without filename
	RawHTML  string `json:"raw_html"`       // preferred — caller supplies file content directly
	Filename string `json:"filename"`       // optional disk-load mode (default: derive from family/brand/date)
}

func (s *AdvancedMailingService) HandleSendDayCreativeResolve(w http.ResponseWriter, r *http.Request) {
	var body creativeResolveBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionSendDayCreativeResolve,
			"error":       fmt.Sprintf("could not parse JSON body: %v", err),
		})
		return
	}
	body.Brand = strings.ToUpper(strings.TrimSpace(body.Brand))
	body.Slot = strings.ToLower(strings.TrimSpace(body.Slot))
	body.Family = strings.ToLower(strings.TrimSpace(body.Family))

	if _, ok := matureBrandDomain[body.Brand]; !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionSendDayCreativeResolve,
			"error":       fmt.Sprintf("unknown brand %q (mature brands: DB/QF/HT/MH)", body.Brand),
		})
		return
	}
	if body.Slot == "" {
		body.Slot = "welcome-newsletter"
	}
	if _, _, err := windowForSlot(body.Slot); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionSendDayCreativeResolve,
			"error":       err.Error(),
		})
		return
	}

	html := body.RawHTML
	filename := body.Filename
	if html == "" {
		// Disk-load fallback: derive filename if not supplied, then read.
		if filename == "" {
			if body.Family == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"api_version": VersionSendDayCreativeResolve,
					"error":       "either raw_html or (family + date) must be supplied",
				})
				return
			}
			datePrefix := strings.ReplaceAll(body.Date, "-", "")
			if len(datePrefix) == 8 {
				// Convert YYYYMMDD → MMDDYYYY (Python TEMPLATE_DIR pattern).
				datePrefix = datePrefix[4:6] + datePrefix[6:8] + datePrefix[0:4]
			}
			filename = fmt.Sprintf("%s-%s-newsletter-%s.html",
				body.Family, matureBrandTemplateCode[body.Brand], datePrefix)
		}
		dir := os.Getenv("CREATIVE_DIR")
		if dir == "" {
			dir = "./docs/emails"
		}
		path := filepath.Join(dir, filename)
		raw, err := os.ReadFile(path)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"api_version": VersionSendDayCreativeResolve,
				"error":       fmt.Sprintf("creative file not readable at %s: %v — supply raw_html if running in ECS", path, err),
				"filename":    filename,
			})
			return
		}
		html = string(raw)
	}

	mutated, err := applyFullPipeline(html, body.Brand)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionSendDayCreativeResolve,
			"error":       err.Error(),
		})
		return
	}

	// Subject + preheader. If no family is in the body, try to recover it
	// from the filename (the Python script encodes family as the first
	// dash-separated segment of the basename: warby-parker-db-newsletter-...).
	family := body.Family
	if family == "" && filename != "" {
		family = inferFamilyFromFilename(filename)
	}
	var subject, preheader string
	if cp, derr := deriveCopyForSlotFamily(body.Slot, family); derr == nil {
		subject = cp.Subject
		preheader = cp.Preheader
	}

	// Diagnostics for the canvas drawer.
	moneyResid := strings.Count(mutated, moneyLinkSentinel)
	cratoolproUrls := cratoolproHrefRE.FindAllStringSubmatch(mutated, -1)
	tagged := 0
	for _, mm := range cratoolproUrls {
		if len(mm) >= 3 && strings.Contains(mm[2], "source_id=email") {
			tagged++
		}
	}

	writeJSONResponse(w, map[string]interface{}{
		"api_version":  VersionSendDayCreativeResolve,
		"slot":         body.Slot,
		"brand":        body.Brand,
		"family":       family,
		"filename":     filename,
		"subject":      subject,
		"preheader":    preheader,
		"html_content": mutated,
		"html_kb":      float64(len(mutated)) / 1024.0,
		"diagnostics": map[string]interface{}{
			"money_link_text_residual": moneyResid,
			"cratoolpro_money_urls":    len(cratoolproUrls),
			"tracking_tagged":          tagged,
			"tracking_untagged":        len(cratoolproUrls) - tagged,
			"has_brand_footer":         hasExistingBrandFooter(mutated),
		},
	})
}

// inferFamilyFromFilename peels the leading family identifier off filenames
// shaped like `warby-parker-db-newsletter-05122026.html`. Families are
// hyphen-bearing, so we take everything before `-{brandcode}-newsletter-`.
func inferFamilyFromFilename(fn string) string {
	base := filepath.Base(fn)
	for _, code := range []string{"db", "qf", "ht", "mh"} {
		marker := fmt.Sprintf("-%s-newsletter-", code)
		if idx := strings.Index(base, marker); idx > 0 {
			return base[:idx]
		}
	}
	return ""
}

// windowForSlot returns the documented (start anchor offset, window hours)
// for the four mature send-day slots. The values must match
// deploy_may12_mature.SLOT_ANCHORS_UTC + SLOT_WINDOW_HOURS exactly.
func windowForSlot(slot string) (int, int, error) {
	switch slot {
	case "eng-w1":
		return 7, 4, nil
	case "welcome-newsletter":
		return 9, 6, nil
	case "eng-w2":
		return 16, 4, nil
	case "eng-w3":
		return 18, 4, nil
	}
	return 0, 0, fmt.Errorf("unknown slot %q (expected eng-w1 / welcome-newsletter / eng-w2 / eng-w3)", slot)
}
