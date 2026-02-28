package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// =============================================================================
// CREATIVE OPTIMIZER SERVICE
// AI-powered creative optimization: HTML-to-text conversion, differentiation,
// scoring, and variant management for mailing creatives.
// =============================================================================

// Precompiled regexp patterns used across handlers
var (
	reHTMLAnchor       = regexp.MustCompile(`(?i)<a\s[^>]*href=["']([^"']*)["'][^>]*>(.*?)</a>`)
	reHTMLListItem     = regexp.MustCompile(`(?i)<li[^>]*>`)
	reHTMLBlockBreak   = regexp.MustCompile(`(?i)<\s*(?:br|p|div)[^>]*/?\s*>`)
	reHTMLClosingBlock = regexp.MustCompile(`(?i)</\s*(?:p|div)\s*>`)
	reHTMLAllTags      = regexp.MustCompile(`<[^>]*>`)
	reMultipleNewlines = regexp.MustCompile(`\n{3,}`)
	reMultipleSpaces   = regexp.MustCompile(`[ \t]+`)

	// Differentiation patterns
	reFontFamily    = regexp.MustCompile(`(?i)(font-family\s*:\s*)([^;}"]+)`)
	rePadding       = regexp.MustCompile(`(?i)(padding\s*:\s*)(\d+)(px)`)
	reMargin        = regexp.MustCompile(`(?i)(margin\s*:\s*)(\d+)(px)`)
	reBgColor       = regexp.MustCompile(`(?i)(background-color\s*:\s*#)([0-9a-fA-F]{6})`)
	reHeadingSize   = regexp.MustCompile(`(?i)(<h[1-6][^>]*style=["'][^"']*)font-size\s*:\s*(\d+)px`)
	reLinkColor     = regexp.MustCompile(`(?i)(<a[^>]*style=["'][^"']*)color\s*:\s*#([0-9a-fA-F]{6})`)
	reTableWidth    = regexp.MustCompile(`(?i)(<table[^>]*)(width=["']\d+["'])`)
	reImgWidth      = regexp.MustCompile(`(?i)(<img[^>]*)(width=["']\d+["'])`)
	reHTMLComment   = regexp.MustCompile(`(?i)<!--[\s\S]*?-->`)

	// Scoring patterns
	reSpamWords     = regexp.MustCompile(`(?i)\b(free|win|guaranteed|act now|limited time|click here|buy now|urgent|exclusive|offer)\b`)
	reImgTags       = regexp.MustCompile(`(?i)<img[^>]*>`)
	reLargeFontSize = regexp.MustCompile(`(?i)font-size\s*:\s*(\d{2,})px`)
	rePersonalize   = regexp.MustCompile(`\{\{(?:name|first_name)\}\}`)
	reCTAButton     = regexp.MustCompile(`(?i)<a[^>]*class=["'][^"']*\b(?:button|btn)\b[^"']*["']`)
	reInlineStyle   = regexp.MustCompile(`(?i)\bstyle=["']`)
	reCSSClass      = regexp.MustCompile(`(?i)\bclass=["']`)
	reViewport      = regexp.MustCompile(`(?i)<meta[^>]*name=["']viewport["']`)
	reImgAlt        = regexp.MustCompile(`(?i)<img[^>]*alt=["'][^"']+["']`)
	reImgTotal      = regexp.MustCompile(`(?i)<img[^>]*>`)
	reAllCaps       = regexp.MustCompile(`\b[A-Z]{5,}\b`)
	reExclamations  = regexp.MustCompile(`!{2,}`)
	reHiddenText    = regexp.MustCompile(`(?i)(display\s*:\s*none|font-size\s*:\s*0)`)
)

// CreativeOptimizerService provides creative optimization handlers.
type CreativeOptimizerService struct {
	db *sql.DB
}

// --- Request / Response Types ---

type htmlToTextRequest struct {
	CreativeID  string `json:"creative_id"`
	HTMLContent string `json:"html_content"`
}

type htmlToTextResponse struct {
	TextContent string `json:"text_content"`
	VariantID   string `json:"variant_id,omitempty"`
	WordCount   int    `json:"word_count"`
	LineCount   int    `json:"line_count"`
}

type differentiateRequest struct {
	CreativeID  string `json:"creative_id"`
	HTMLContent string `json:"html_content"`
	Strategy    string `json:"strategy"` // subtle, moderate, aggressive
}

type differentiateResponse struct {
	HTMLContent    string   `json:"html_content"`
	VariantID      string   `json:"variant_id,omitempty"`
	ChangesApplied []string `json:"changes_applied"`
	Strategy       string   `json:"strategy"`
}

type scoreCreativeRequest struct {
	HTMLContent string `json:"html_content"`
	TextContent string `json:"text_content"`
}

type scoreDetail struct {
	Deliverability int `json:"deliverability"`
	Engagement     int `json:"engagement"`
	Compatibility  int `json:"compatibility"`
	SpamRisk       int `json:"spam_risk"`
	Overall        int `json:"overall"`
}

type scoreIssue struct {
	Category string `json:"category"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type scoreCreativeResponse struct {
	Scores          scoreDetail `json:"scores"`
	Issues          []scoreIssue  `json:"issues"`
	Recommendations []string      `json:"recommendations"`
}

type variantSummary struct {
	ID                string    `json:"id"`
	CreativeName      string    `json:"creative_name"`
	Source            string    `json:"source"`
	AIOptimized       bool      `json:"ai_optimized"`
	TextContent       string    `json:"text_content"`
	HTMLContentLength int       `json:"html_content_length"`
	CreatedAt         time.Time `json:"created_at"`
}

type getVariantsResponse struct {
	OriginalID string           `json:"original_id"`
	Variants   []variantSummary `json:"variants"`
}

// =============================================================================
// Route Registration
// =============================================================================

// RegisterCreativeOptimizerRoutes registers creative optimizer routes on the router.
func RegisterCreativeOptimizerRoutes(r chi.Router, db *sql.DB) {
	cos := &CreativeOptimizerService{db: db}
	r.Post("/creative-optimizer/html-to-text", cos.HandleHTMLToText)
	r.Post("/creative-optimizer/differentiate", cos.HandleDifferentiateHTML)
	r.Post("/creative-optimizer/score", cos.HandleScoreCreative)
	r.Get("/creative-optimizer/variants/{creativeId}", cos.HandleGetVariants)
}

// =============================================================================
// Handler 1: HTML-to-Text Conversion
// =============================================================================

// HandleHTMLToText converts HTML creative content to clean plain text.
func (cos *CreativeOptimizerService) HandleHTMLToText(w http.ResponseWriter, r *http.Request) {
	var req htmlToTextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.HTMLContent) == "" {
		respondError(w, http.StatusBadRequest, "html_content is required")
		return
	}

	text := convertHTMLToText(req.HTMLContent)

	resp := htmlToTextResponse{
		TextContent: text,
		WordCount:   countWords(text),
		LineCount:   countLines(text),
	}

	// If creative_id is provided, save as a variant
	if req.CreativeID != "" {
		variantID, err := cos.saveTextVariant(req.CreativeID, text)
		if err != nil {
			log.Printf("ERROR: failed to save text variant for creative %s: %v", req.CreativeID, err)
			respondError(w, http.StatusInternalServerError, "Failed to save text variant")
			return
		}
		resp.VariantID = variantID
	}

	respondJSON(w, http.StatusOK, resp)
}

// convertHTMLToText performs deterministic HTML to plain-text conversion.
func convertHTMLToText(htmlStr string) string {
	if htmlStr == "" {
		return ""
	}

	s := htmlStr

	// 1. Convert <a href="URL">text</a> to "text (URL)"
	s = reHTMLAnchor.ReplaceAllString(s, "$2 ($1)")

	// 2. Convert <li> to bullet points
	s = reHTMLListItem.ReplaceAllString(s, "\n- ")

	// 3. Convert block-level tags to newlines
	s = reHTMLBlockBreak.ReplaceAllString(s, "\n")
	s = reHTMLClosingBlock.ReplaceAllString(s, "\n")

	// 4. Strip all remaining HTML tags
	s = reHTMLAllTags.ReplaceAllString(s, "")

	// 5. Decode HTML entities
	s = html.UnescapeString(s)

	// 6. Collapse multiple spaces on each line
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(reMultipleSpaces.ReplaceAllString(line, " "))
	}
	s = strings.Join(lines, "\n")

	// 7. Collapse multiple blank lines to max 2
	s = reMultipleNewlines.ReplaceAllString(s, "\n\n")

	// 8. Trim leading/trailing whitespace
	s = strings.TrimSpace(s)

	return s
}

// saveTextVariant saves the converted text as a new variant in the creative library.
func (cos *CreativeOptimizerService) saveTextVariant(creativeID, textContent string) (string, error) {
	parentUUID, err := uuid.Parse(creativeID)
	if err != nil {
		return "", fmt.Errorf("invalid creative_id: %w", err)
	}

	// Fetch parent creative for context
	var orgID uuid.UUID
	var offerID, offerName, creativeName string
	err = cos.db.QueryRow(`
		SELECT organization_id, COALESCE(offer_id,''), COALESCE(offer_name,''), COALESCE(creative_name,'')
		FROM mailing_creative_library WHERE id = $1`, parentUUID).
		Scan(&orgID, &offerID, &offerName, &creativeName)
	if err != nil {
		return "", fmt.Errorf("parent creative not found: %w", err)
	}

	variantID := uuid.New()
	now := time.Now()
	_, err = cos.db.Exec(`
		INSERT INTO mailing_creative_library
			(id, organization_id, offer_id, offer_name, creative_name, source, text_content,
			 ai_optimized, variant_of, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,'ai_generated',$6,true,$7,'active',$8,$9)`,
		variantID, orgID, offerID, offerName,
		fmt.Sprintf("%s (text variant)", creativeName),
		textContent, parentUUID, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("insert variant: %w", err)
	}

	return variantID.String(), nil
}

// =============================================================================
// Handler 2: Differentiate HTML
// =============================================================================

// HandleDifferentiateHTML makes HTML look different from the network version
// by applying deterministic transformations based on the selected strategy.
func (cos *CreativeOptimizerService) HandleDifferentiateHTML(w http.ResponseWriter, r *http.Request) {
	var req differentiateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.HTMLContent) == "" {
		respondError(w, http.StatusBadRequest, "html_content is required")
		return
	}
	strategy := strings.ToLower(strings.TrimSpace(req.Strategy))
	if strategy == "" {
		strategy = "subtle"
	}
	if strategy != "subtle" && strategy != "moderate" && strategy != "aggressive" {
		respondError(w, http.StatusBadRequest, "strategy must be subtle, moderate, or aggressive")
		return
	}

	// Deterministic seed based on content length
	seed := len(req.HTMLContent)

	result, changes := differentiateHTML(req.HTMLContent, strategy, seed)

	resp := differentiateResponse{
		HTMLContent:    result,
		ChangesApplied: changes,
		Strategy:       strategy,
	}

	// If creative_id is provided, save as variant
	if req.CreativeID != "" {
		variantID, err := cos.saveHTMLVariant(req.CreativeID, result, strategy)
		if err != nil {
			log.Printf("ERROR: failed to save HTML variant for creative %s: %v", req.CreativeID, err)
			respondError(w, http.StatusInternalServerError, "Failed to save variant")
			return
		}
		resp.VariantID = variantID
	}

	respondJSON(w, http.StatusOK, resp)
}

// differentiateHTML applies deterministic mutations to HTML based on strategy.
func differentiateHTML(htmlStr, strategy string, seed int) (string, []string) {
	s := htmlStr
	var changes []string

	// --- Subtle transformations ---

	// 1. Change font-family list order (rotate first font to end)
	s = reFontFamily.ReplaceAllStringFunc(s, func(match string) string {
		parts := reFontFamily.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		fonts := strings.Split(parts[2], ",")
		if len(fonts) > 1 {
			for i := range fonts {
				fonts[i] = strings.TrimSpace(fonts[i])
			}
			// Rotate: move first font to end
			rotated := append(fonts[1:], fonts[0])
			return parts[1] + strings.Join(rotated, ", ")
		}
		return match
	})
	changes = append(changes, "changed font family order")

	// 2. Adjust padding by +2px (deterministic based on seed)
	offset := 2
	if seed%2 == 0 {
		offset = -2
	}
	s = rePadding.ReplaceAllStringFunc(s, func(match string) string {
		parts := rePadding.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		val := 0
		fmt.Sscanf(parts[2], "%d", &val)
		val += offset
		if val < 0 {
			val = 0
		}
		return fmt.Sprintf("%s%d%s", parts[1], val, parts[3])
	})
	changes = append(changes, "adjusted padding")

	// 3. Adjust margin by ±2px (opposite direction of padding)
	marginOffset := -offset
	s = reMargin.ReplaceAllStringFunc(s, func(match string) string {
		parts := reMargin.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		val := 0
		fmt.Sscanf(parts[2], "%d", &val)
		val += marginOffset
		if val < 0 {
			val = 0
		}
		return fmt.Sprintf("%s%d%s", parts[1], val, parts[3])
	})
	changes = append(changes, "adjusted margin")

	// 4. Shift background-color hex by 5-10 per channel
	s = reBgColor.ReplaceAllStringFunc(s, func(match string) string {
		parts := reBgColor.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		shifted := shiftHexColor(parts[2], seed)
		return parts[1] + shifted
	})
	changes = append(changes, "shifted background colors")

	// 5. Add small footer disclaimer
	disclaimer := fmt.Sprintf(
		`<div style="font-size:10px;color:#999;text-align:center;padding-top:8px;">`+
			`This message was sent in compliance with applicable regulations. Ref: %d</div>`,
		seed%9999+1000)
	if strings.Contains(strings.ToLower(s), "</body>") {
		s = strings.Replace(s, "</body>", disclaimer+"</body>", 1)
		s = strings.Replace(s, "</BODY>", disclaimer+"</BODY>", 1)
	} else {
		s += "\n" + disclaimer
	}
	changes = append(changes, "added footer disclaimer")

	if strategy == "subtle" {
		return s, changes
	}

	// --- Moderate transformations (includes subtle) ---

	// 6. Add wrapper div with unique ID
	wrapperID := fmt.Sprintf("wrap-%d", seed%10000+1000)
	s = fmt.Sprintf(`<div id="%s" style="max-width:100%%;">`, wrapperID) + s + `</div>`
	changes = append(changes, "added wrapper div")

	// 7. Change heading sizes (+1px)
	s = reHeadingSize.ReplaceAllStringFunc(s, func(match string) string {
		parts := reHeadingSize.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		val := 0
		fmt.Sscanf(parts[2], "%d", &val)
		val++
		return fmt.Sprintf("%sfont-size:%dpx", parts[1], val)
	})
	changes = append(changes, "adjusted heading sizes")

	// 8. Modify link colors (shift by 10)
	s = reLinkColor.ReplaceAllStringFunc(s, func(match string) string {
		parts := reLinkColor.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		shifted := shiftHexColor(parts[2], seed+7)
		return fmt.Sprintf("%scolor:#%s", parts[1], shifted)
	})
	changes = append(changes, "modified link colors")

	if strategy == "moderate" {
		return s, changes
	}

	// --- Aggressive transformations (includes moderate) ---

	// 9. Change table widths (+5)
	s = reTableWidth.ReplaceAllStringFunc(s, func(match string) string {
		parts := reTableWidth.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		// Extract numeric value from width="NNN"
		widthStr := parts[2]
		var w int
		fmt.Sscanf(widthStr, `width="%d"`, &w)
		if w == 0 {
			fmt.Sscanf(widthStr, `width='%d'`, &w)
		}
		if w > 0 {
			w += 5
			return fmt.Sprintf(`%swidth="%d"`, parts[1], w)
		}
		return match
	})
	changes = append(changes, "adjusted table widths")

	// 10. Change image widths (-3)
	s = reImgWidth.ReplaceAllStringFunc(s, func(match string) string {
		parts := reImgWidth.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		widthStr := parts[2]
		var iw int
		fmt.Sscanf(widthStr, `width="%d"`, &iw)
		if iw == 0 {
			fmt.Sscanf(widthStr, `width='%d'`, &iw)
		}
		if iw > 3 {
			iw -= 3
			return fmt.Sprintf(`%swidth="%d"`, parts[1], iw)
		}
		return match
	})
	changes = append(changes, "adjusted image widths")

	// 11. Add preheader text
	preheader := fmt.Sprintf(
		`<div style="display:none;font-size:1px;line-height:1px;max-height:0;overflow:hidden;">`+
			`Preview reference %d - optimized content delivery</div>`,
		seed%9999+1000)
	// Insert after <body> if present, otherwise prepend
	bodyIdx := strings.Index(strings.ToLower(s), "<body")
	if bodyIdx >= 0 {
		closeIdx := strings.Index(s[bodyIdx:], ">")
		if closeIdx >= 0 {
			insertAt := bodyIdx + closeIdx + 1
			s = s[:insertAt] + preheader + s[insertAt:]
		}
	} else {
		s = preheader + s
	}
	changes = append(changes, "added preheader text")

	// 12. Remove existing comments and add unique ones
	s = reHTMLComment.ReplaceAllString(s, "")
	uniqueComment := fmt.Sprintf("<!-- optimized variant seed:%d ts:%d -->", seed, time.Now().Unix())
	s = uniqueComment + "\n" + s
	changes = append(changes, "added unique comments")

	return s, changes
}

// shiftHexColor shifts each channel of a 6-char hex color by a deterministic amount.
func shiftHexColor(hexStr string, seed int) string {
	if len(hexStr) != 6 {
		return hexStr
	}
	shift := (seed % 6) + 5 // 5–10 range
	result := make([]byte, 6)
	for i := 0; i < 6; i += 2 {
		hi := hexCharToInt(hexStr[i])
		lo := hexCharToInt(hexStr[i+1])
		val := hi*16 + lo + shift
		if val > 255 {
			val = 255
		}
		if val < 0 {
			val = 0
		}
		result[i] = intToHexChar(val / 16)
		result[i+1] = intToHexChar(val % 16)
	}
	return string(result)
}

func hexCharToInt(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return 0
	}
}

func intToHexChar(v int) byte {
	if v < 10 {
		return byte('0' + v)
	}
	return byte('a' + v - 10)
}

// saveHTMLVariant saves differentiated HTML as a new variant.
func (cos *CreativeOptimizerService) saveHTMLVariant(creativeID, htmlContent, strategy string) (string, error) {
	parentUUID, err := uuid.Parse(creativeID)
	if err != nil {
		return "", fmt.Errorf("invalid creative_id: %w", err)
	}

	var orgID uuid.UUID
	var offerID, offerName, creativeName string
	err = cos.db.QueryRow(`
		SELECT organization_id, COALESCE(offer_id,''), COALESCE(offer_name,''), COALESCE(creative_name,'')
		FROM mailing_creative_library WHERE id = $1`, parentUUID).
		Scan(&orgID, &offerID, &offerName, &creativeName)
	if err != nil {
		return "", fmt.Errorf("parent creative not found: %w", err)
	}

	variantID := uuid.New()
	now := time.Now()
	_, err = cos.db.Exec(`
		INSERT INTO mailing_creative_library
			(id, organization_id, offer_id, offer_name, creative_name, source, html_content,
			 ai_optimized, variant_of, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,'ai_generated',$6,true,$7,'active',$8,$9)`,
		variantID, orgID, offerID, offerName,
		fmt.Sprintf("%s (%s variant)", creativeName, strategy),
		htmlContent, parentUUID, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("insert variant: %w", err)
	}

	return variantID.String(), nil
}

// =============================================================================
// Handler 3: Score Creative
// =============================================================================

// HandleScoreCreative scores a creative on deliverability, engagement,
// compatibility, and spam risk.
func (cos *CreativeOptimizerService) HandleScoreCreative(w http.ResponseWriter, r *http.Request) {
	var req scoreCreativeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.HTMLContent) == "" && strings.TrimSpace(req.TextContent) == "" {
		respondError(w, http.StatusBadRequest, "html_content or text_content is required")
		return
	}

	combined := req.HTMLContent + " " + req.TextContent
	var issues []scoreIssue
	var recommendations []string

	// --- Deliverability (0-100) ---
	deliverability := 100
	spamWordMatches := reSpamWords.FindAllString(combined, -1)
	spamWordCount := len(spamWordMatches)
	if spamWordCount > 0 {
		penalty := spamWordCount * 8
		if penalty > 40 {
			penalty = 40
		}
		deliverability -= penalty
		issues = append(issues, scoreIssue{
			Category: "deliverability",
			Severity: severityByCount(spamWordCount, 2, 5),
			Message:  fmt.Sprintf("Found %d spam trigger word(s): %s", spamWordCount, strings.Join(uniqueStrings(spamWordMatches), ", ")),
		})
		recommendations = append(recommendations, "Reduce spam trigger words to improve deliverability")
	}

	imgCount := len(reImgTags.FindAllString(req.HTMLContent, -1))
	textLen := len(strings.TrimSpace(reHTMLAllTags.ReplaceAllString(req.HTMLContent, "")))
	if imgCount > 0 && textLen > 0 {
		ratio := float64(imgCount*1000) / float64(textLen) // images per 1000 chars
		if ratio > 3.0 {
			deliverability -= 15
			issues = append(issues, scoreIssue{
				Category: "deliverability",
				Severity: "warning",
				Message:  fmt.Sprintf("High image-to-text ratio (%.1f images per 1000 chars)", ratio),
			})
			recommendations = append(recommendations, "Add more text content to balance the image-to-text ratio")
		}
	}

	largeFontMatches := reLargeFontSize.FindAllStringSubmatch(req.HTMLContent, -1)
	largeFontCount := 0
	for _, m := range largeFontMatches {
		if len(m) >= 2 {
			var sz int
			fmt.Sscanf(m[1], "%d", &sz)
			if sz > 28 {
				largeFontCount++
			}
		}
	}
	if largeFontCount > 0 {
		deliverability -= largeFontCount * 5
		issues = append(issues, scoreIssue{
			Category: "deliverability",
			Severity: "info",
			Message:  fmt.Sprintf("Found %d instance(s) of large font sizes (>28px)", largeFontCount),
		})
	}
	if deliverability < 0 {
		deliverability = 0
	}

	// --- Engagement (0-100) ---
	engagement := 60 // base
	if rePersonalize.MatchString(combined) {
		engagement += 15
	} else {
		issues = append(issues, scoreIssue{
			Category: "engagement",
			Severity: "info",
			Message:  "No personalization tokens found (e.g., {{name}}, {{first_name}})",
		})
		recommendations = append(recommendations, "Add personalization tokens like {{first_name}} to boost engagement")
	}

	if reCTAButton.MatchString(req.HTMLContent) {
		engagement += 15
	} else {
		issues = append(issues, scoreIssue{
			Category: "engagement",
			Severity: "warning",
			Message:  "No CTA button found (link with 'button' or 'btn' class)",
		})
		recommendations = append(recommendations, "Add a clear CTA button with a 'button' or 'btn' class")
	}

	// Check for short preheader-like content (first 100 chars of text)
	plainText := convertHTMLToText(req.HTMLContent)
	if len(plainText) > 0 {
		firstLine := strings.SplitN(plainText, "\n", 2)[0]
		if len(firstLine) > 10 && len(firstLine) < 100 {
			engagement += 10
		}
	}
	if engagement > 100 {
		engagement = 100
	}

	// --- Compatibility (0-100) ---
	compatibility := 70 // base

	inlineStyleCount := len(reInlineStyle.FindAllString(req.HTMLContent, -1))
	classCount := len(reCSSClass.FindAllString(req.HTMLContent, -1))
	if inlineStyleCount > 0 && classCount > inlineStyleCount {
		compatibility -= 10
		issues = append(issues, scoreIssue{
			Category: "compatibility",
			Severity: "warning",
			Message:  "More CSS classes than inline styles — some email clients may not support external CSS",
		})
		recommendations = append(recommendations, "Prefer inline styles over CSS classes for better email client compatibility")
	} else if inlineStyleCount > 0 {
		compatibility += 10
	}

	if reViewport.MatchString(req.HTMLContent) {
		compatibility += 10
	} else {
		issues = append(issues, scoreIssue{
			Category: "compatibility",
			Severity: "info",
			Message:  "No responsive meta viewport tag found",
		})
		recommendations = append(recommendations, "Add a meta viewport tag for responsive email rendering")
	}

	totalImgs := len(reImgTotal.FindAllString(req.HTMLContent, -1))
	imgsWithAlt := len(reImgAlt.FindAllString(req.HTMLContent, -1))
	if totalImgs > 0 && imgsWithAlt < totalImgs {
		missing := totalImgs - imgsWithAlt
		compatibility -= missing * 5
		issues = append(issues, scoreIssue{
			Category: "compatibility",
			Severity: "warning",
			Message:  fmt.Sprintf("%d of %d images missing alt text", missing, totalImgs),
		})
		recommendations = append(recommendations, "Add alt text to all images for accessibility and fallback display")
	}
	if compatibility < 0 {
		compatibility = 0
	}
	if compatibility > 100 {
		compatibility = 100
	}

	// --- Spam Risk (0-100, higher = less risky = better) ---
	spamRisk := 100
	// Reuse spam word count from deliverability
	if spamWordCount > 0 {
		penalty := spamWordCount * 10
		if penalty > 50 {
			penalty = 50
		}
		spamRisk -= penalty
	}

	allCapsCount := len(reAllCaps.FindAllString(combined, -1))
	if allCapsCount > 2 {
		spamRisk -= allCapsCount * 5
		issues = append(issues, scoreIssue{
			Category: "spam_risk",
			Severity: "warning",
			Message:  fmt.Sprintf("Found %d ALL CAPS words (5+ consecutive uppercase letters)", allCapsCount),
		})
		recommendations = append(recommendations, "Reduce ALL CAPS text to avoid spam filters")
	}

	exclCount := len(reExclamations.FindAllString(combined, -1))
	if exclCount > 0 {
		spamRisk -= exclCount * 8
		issues = append(issues, scoreIssue{
			Category: "spam_risk",
			Severity: "warning",
			Message:  fmt.Sprintf("Found %d instance(s) of excessive exclamation marks", exclCount),
		})
		recommendations = append(recommendations, "Remove excessive exclamation marks")
	}

	hiddenCount := len(reHiddenText.FindAllString(req.HTMLContent, -1))
	if hiddenCount > 0 {
		spamRisk -= hiddenCount * 15
		issues = append(issues, scoreIssue{
			Category: "spam_risk",
			Severity: "critical",
			Message:  fmt.Sprintf("Found %d instance(s) of hidden text (display:none or font-size:0)", hiddenCount),
		})
		recommendations = append(recommendations, "Remove hidden text elements — they trigger spam filters")
	}
	if spamRisk < 0 {
		spamRisk = 0
	}

	// --- Overall weighted score ---
	overall := int(
		float64(deliverability)*0.30 +
			float64(engagement)*0.25 +
			float64(compatibility)*0.25 +
			float64(spamRisk)*0.20 +
			0.5) // round

	if issues == nil {
		issues = []scoreIssue{}
	}
	if recommendations == nil {
		recommendations = []string{}
	}

	respondJSON(w, http.StatusOK, scoreCreativeResponse{
		Scores: scoreDetail{
			Deliverability: deliverability,
			Engagement:     engagement,
			Compatibility:  compatibility,
			SpamRisk:       spamRisk,
			Overall:        overall,
		},
		Issues:          issues,
		Recommendations: recommendations,
	})
}

// =============================================================================
// Handler 4: Get Variants
// =============================================================================

// HandleGetVariants returns all active variants of a given creative.
func (cos *CreativeOptimizerService) HandleGetVariants(w http.ResponseWriter, r *http.Request) {
	creativeID := chi.URLParam(r, "creativeId")
	if creativeID == "" {
		respondError(w, http.StatusBadRequest, "creativeId is required")
		return
	}

	parentUUID, err := uuid.Parse(creativeID)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid creativeId format")
		return
	}

	rows, err := cos.db.Query(`
		SELECT id, COALESCE(creative_name,''), COALESCE(source,''), COALESCE(ai_optimized,false),
			   COALESCE(text_content,''), COALESCE(LENGTH(html_content),0), created_at
		FROM mailing_creative_library
		WHERE variant_of = $1 AND status = 'active'
		ORDER BY created_at DESC`, parentUUID)
	if err != nil {
		log.Printf("ERROR: failed to query variants for creative %s: %v", creativeID, err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve variants")
		return
	}
	defer rows.Close()

	var variants []variantSummary
	for rows.Next() {
		var v variantSummary
		var fullText string
		if err := rows.Scan(&v.ID, &v.CreativeName, &v.Source, &v.AIOptimized,
			&fullText, &v.HTMLContentLength, &v.CreatedAt); err != nil {
			log.Printf("ERROR: failed to scan variant row: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve variants")
			return
		}
		// Truncate text_content to 200 chars
		if len(fullText) > 200 {
			v.TextContent = fullText[:200] + "..."
		} else {
			v.TextContent = fullText
		}
		variants = append(variants, v)
	}
	if err := rows.Err(); err != nil {
		log.Printf("ERROR: rows iteration error for variants: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve variants")
		return
	}

	if variants == nil {
		variants = []variantSummary{}
	}

	respondJSON(w, http.StatusOK, getVariantsResponse{
		OriginalID: creativeID,
		Variants:   variants,
	})
}

// =============================================================================
// Utility helpers
// =============================================================================

func countWords(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return len(strings.Fields(s))
}

func countLines(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

func severityByCount(count, warnThreshold, critThreshold int) string {
	if count >= critThreshold {
		return "critical"
	}
	if count >= warnThreshold {
		return "warning"
	}
	return "info"
}

func uniqueStrings(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	var result []string
	for _, s := range ss {
		lower := strings.ToLower(s)
		if _, ok := seen[lower]; !ok {
			seen[lower] = struct{}{}
			result = append(result, lower)
		}
	}
	return result
}
