package worker

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
)

// ValidatorMode controls whether validation failures block sends or only log.
type ValidatorMode int

const (
	ValidatorAudit  ValidatorMode = iota // Log only, never block
	ValidatorEnforce                     // Block sends on hard failures
)

// ValidationSeverity indicates how serious a validation issue is.
type ValidationSeverity string

const (
	SeverityError   ValidationSeverity = "error"   // Blocks send in enforce mode
	SeverityWarning ValidationSeverity = "warning" // Logged, never blocks
)

// ValidationIssue describes a single content quality problem.
type ValidationIssue struct {
	Code     string             `json:"code"`
	Severity ValidationSeverity `json:"severity"`
	Message  string             `json:"message"`
	Field    string             `json:"field,omitempty"`
}

var (
	reUnresolvedLiquid = regexp.MustCompile(`\{\{[^}]*\}\}|\{%[^%]*%\}`)
	reEmptyHref        = regexp.MustCompile(`(?i)href\s*=\s*["']\s*["']`)
	reSiteTokens       = regexp.MustCompile(`\{\{(SUBJECT|PREVIEW_TEXT|ARTICLE_\d|INTRO|CLOSING_LINE|FIRST_NAME|DATE_MMDDYYYY)\}\}`)
	reHiddenPreheader  = regexp.MustCompile(`(?is)<div[^>]*display\s*:\s*none[^>]*>(.*?)</div>`)
	reFillerGlyphs     = regexp.MustCompile(`&#847;|&#8199;|&#65279;|&zwnj;`)
	reUnsubPath        = regexp.MustCompile(`(?i)/track/unsubscribe/`)
	reUnsubLiquid      = regexp.MustCompile(`\{\{\s*system\.unsubscribe_url\s*\}\}`)
)

// GetValidatorMode reads the VALIDATOR_MODE environment variable.
// Returns ValidatorAudit unless explicitly set to "enforce".
func GetValidatorMode() ValidatorMode {
	if strings.EqualFold(os.Getenv("VALIDATOR_MODE"), "enforce") {
		return ValidatorEnforce
	}
	return ValidatorAudit
}

// ValidateRenderedMessage checks a fully rendered EmailMessage for content
// quality issues. In audit mode, issues are logged but sends are never blocked.
// In enforce mode, error-severity issues cause the send to be rejected.
func ValidateRenderedMessage(msg *EmailMessage, mode ValidatorMode, messageClass string) []ValidationIssue {
	var issues []ValidationIssue

	// 1. Unresolved template tokens in subject
	if matches := reUnresolvedLiquid.FindAllString(msg.Subject, -1); len(matches) > 0 {
		for _, m := range matches {
			if !isApprovedPlatformToken(m) {
				issues = append(issues, ValidationIssue{
					Code:     "unresolved_token_subject",
					Severity: SeverityError,
					Message:  fmt.Sprintf("unresolved token in subject: %s", m),
					Field:    "subject",
				})
			}
		}
	}

	// 2. Unresolved template tokens in HTML
	if matches := reUnresolvedLiquid.FindAllString(msg.HTMLContent, -1); len(matches) > 0 {
		for _, m := range matches {
			if !isApprovedPlatformToken(m) {
				issues = append(issues, ValidationIssue{
					Code:     "unresolved_token_html",
					Severity: SeverityWarning,
					Message:  fmt.Sprintf("unresolved token in HTML: %s", m),
					Field:    "html_content",
				})
			}
		}
	}

	// 3. Site-token leakage (wave generator placeholders that weren't replaced)
	if matches := reSiteTokens.FindAllString(msg.HTMLContent, -1); len(matches) > 0 {
		for _, m := range matches {
			issues = append(issues, ValidationIssue{
				Code:     "site_token_leakage",
				Severity: SeverityError,
				Message:  fmt.Sprintf("site-author token leaked into rendered output: %s", m),
				Field:    "html_content",
			})
		}
	}

	// 4. Empty href attributes
	if reEmptyHref.MatchString(msg.HTMLContent) {
		issues = append(issues, ValidationIssue{
			Code:     "empty_href",
			Severity: SeverityWarning,
			Message:  "empty href=\"\" found in rendered HTML",
			Field:    "html_content",
		})
	}

	// 5. Compliant unsubscribe control (class-aware, intent-aware)
	if messageClass == "promotional" || messageClass == "welcome" {
		hasUnsubControl := reUnsubPath.MatchString(msg.HTMLContent) ||
			reUnsubLiquid.MatchString(msg.HTMLContent)
		if !hasUnsubControl {
			issues = append(issues, ValidationIssue{
				Code:     "missing_unsub_control",
				Severity: SeverityError,
				Message:  "no compliant unsubscribe control found in HTML body for " + messageClass + " message",
				Field:    "html_content",
			})
		}

		if _, ok := msg.Headers["List-Unsubscribe"]; !ok {
			issues = append(issues, ValidationIssue{
				Code:     "missing_list_unsubscribe",
				Severity: SeverityWarning,
				Message:  "missing List-Unsubscribe header for " + messageClass + " message",
				Field:    "headers",
			})
		}
		if _, ok := msg.Headers["List-Unsubscribe-Post"]; !ok {
			issues = append(issues, ValidationIssue{
				Code:     "missing_list_unsubscribe_post",
				Severity: SeverityWarning,
				Message:  "missing List-Unsubscribe-Post header for " + messageClass + " message",
				Field:    "headers",
			})
		}
	}

	// 6. Suspicious hidden preheader content
	if preheaderMatches := reHiddenPreheader.FindAllStringSubmatch(msg.HTMLContent, -1); len(preheaderMatches) > 0 {
		for _, pm := range preheaderMatches {
			if len(pm) > 1 && reFillerGlyphs.MatchString(pm[1]) {
				issues = append(issues, ValidationIssue{
					Code:     "preheader_filler_glyphs",
					Severity: SeverityWarning,
					Message:  "hidden preheader contains filler glyphs (&#847;/&#8199;/&#65279;)",
					Field:    "html_content",
				})
				break
			}
		}
	}

	// 7. Trivial or missing text/plain
	plainLen := len(strings.TrimSpace(msg.TextContent))
	if plainLen == 0 {
		issues = append(issues, ValidationIssue{
			Code:     "missing_text_plain",
			Severity: SeverityWarning,
			Message:  "text/plain part is empty",
			Field:    "text_content",
		})
	} else if plainLen < 50 {
		issues = append(issues, ValidationIssue{
			Code:     "trivial_text_plain",
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("text/plain part is trivially short (%d chars)", plainLen),
			Field:    "text_content",
		})
	}

	// Log in audit mode; log + potentially block in enforce mode
	if len(issues) > 0 {
		logValidationIssues(msg, issues, mode)
	}

	return issues
}

// HasBlockingIssues returns true if any issue has error severity.
func HasBlockingIssues(issues []ValidationIssue) bool {
	for _, issue := range issues {
		if issue.Severity == SeverityError {
			return true
		}
	}
	return false
}

// ValidateTemplateContent checks template content at upload time using the same
// rules as the send-path validator, adapted for pre-render context (tokens
// haven't been resolved yet, so the check focuses on site-author tokens and
// structural issues rather than unresolved platform tokens).
func ValidateTemplateContent(htmlContent, textContent, subject string) []ValidationIssue {
	var issues []ValidationIssue

	// Site-token leakage: non-platform tokens that sites should resolve before upload
	if matches := reSiteTokens.FindAllString(htmlContent, -1); len(matches) > 0 {
		for _, m := range matches {
			issues = append(issues, ValidationIssue{
				Code:     "site_token_in_template",
				Severity: SeverityError,
				Message:  fmt.Sprintf("site-author token found in template HTML: %s (must be resolved before upload)", m),
				Field:    "html_content",
			})
		}
	}
	if matches := reSiteTokens.FindAllString(subject, -1); len(matches) > 0 {
		for _, m := range matches {
			issues = append(issues, ValidationIssue{
				Code:     "site_token_in_subject",
				Severity: SeverityError,
				Message:  fmt.Sprintf("site-author token found in template subject: %s", m),
				Field:    "subject",
			})
		}
	}

	// Empty href attributes
	if reEmptyHref.MatchString(htmlContent) {
		issues = append(issues, ValidationIssue{
			Code:     "empty_href",
			Severity: SeverityWarning,
			Message:  "empty href=\"\" found in template HTML",
			Field:    "html_content",
		})
	}

	// Compliant unsubscribe control (class-aware: check if any unsub mechanism exists)
	hasUnsubControl := reUnsubPath.MatchString(htmlContent) ||
		reUnsubLiquid.MatchString(htmlContent)
	if !hasUnsubControl && htmlContent != "" {
		issues = append(issues, ValidationIssue{
			Code:     "missing_unsub_in_template",
			Severity: SeverityWarning,
			Message:  "no unsubscribe control found in template (expected {{ system.unsubscribe_url }} or /track/unsubscribe/ path)",
			Field:    "html_content",
		})
	}

	// Suspicious hidden preheader
	if preheaderMatches := reHiddenPreheader.FindAllStringSubmatch(htmlContent, -1); len(preheaderMatches) > 0 {
		for _, pm := range preheaderMatches {
			if len(pm) > 1 && reFillerGlyphs.MatchString(pm[1]) {
				issues = append(issues, ValidationIssue{
					Code:     "preheader_filler_in_template",
					Severity: SeverityWarning,
					Message:  "hidden preheader contains filler glyphs — remove before upload",
					Field:    "html_content",
				})
				break
			}
		}
	}

	return issues
}

func logValidationIssues(msg *EmailMessage, issues []ValidationIssue, mode ValidatorMode) {
	modeStr := "audit"
	if mode == ValidatorEnforce {
		modeStr = "enforce"
	}
	issueJSON, _ := json.Marshal(issues)
	log.Printf("[MessageValidator] mode=%s campaign=%s subscriber=%s email=%s issues=%s",
		modeStr, msg.CampaignID, msg.SubscriberID, msg.Email, string(issueJSON))
}

// isApprovedPlatformToken returns true for Liquid tokens that the platform
// is expected to resolve at render time (not site-author tokens).
func isApprovedPlatformToken(token string) bool {
	varName := extractLiquidVarName(token)
	if varName == "" {
		return false
	}

	approved := map[string]bool{
		"first_name": true, "last_name": true, "email": true, "full_name": true,
		"email_local": true, "email_domain": true,
		"system.unsubscribe_url": true, "system.preferences_url": true,
		"system.view_in_browser_url": true, "system.current_date": true,
		"system.current_year": true, "system.current_month": true,
		"system.current_day": true, "system.current_weekday": true,
		"system.current_hour": true, "system.timestamp": true,
		"campaign.name": true, "campaign.subject": true, "campaign.from_name": true,
		"campaign.id": true, "campaignid": true, "campaign_name": true,
		"now": true, "today": true, "year": true,
		"engagement.score": true, "engagement.total_emails": true,
		"engagement.total_opens": true, "engagement.total_clicks": true,
		"subscriber.id": true, "subscriber.status": true,
	}

	if approved[varName] {
		return true
	}

	// Allow nested access under approved prefixes (e.g. custom.*, intel.*, computed.*)
	approvedPrefixes := []string{"system.", "campaign.", "engagement.", "subscriber.", "custom.", "intel.", "computed."}
	for _, pfx := range approvedPrefixes {
		if strings.HasPrefix(varName, pfx) {
			return true
		}
	}

	return false
}

// extractLiquidVarName extracts the variable name from a Liquid token,
// stripping braces, filters, and whitespace.
// "{{ first_name | default: 'there' }}" => "first_name"
// "{{ system.unsubscribe_url }}" => "system.unsubscribe_url"
// "{%  if x %}" => "if" (tag, not a variable — but we don't match these)
func extractLiquidVarName(token string) string {
	s := strings.TrimSpace(token)
	s = strings.TrimPrefix(s, "{{")
	s = strings.TrimPrefix(s, "{%")
	s = strings.TrimSuffix(s, "}}")
	s = strings.TrimSuffix(s, "%}")
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)

	// Strip filter: "first_name | default: 'there'" => "first_name"
	if idx := strings.Index(s, "|"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}

	return s
}
