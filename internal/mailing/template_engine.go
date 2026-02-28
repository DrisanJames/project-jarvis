// Package mailing provides the template engine for dynamic data injection
// using the Liquid template language for enterprise-grade personalization.
package mailing

import (
	"crypto/md5"
	"fmt"
	"html"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/liquid"
)

// RenderMode determines how the template engine handles errors
type RenderMode int

const (
	// RenderModeLax returns empty string for missing vars (production sends)
	RenderModeLax RenderMode = iota
	// RenderModeStrict returns error for missing vars (preview/validation)
	RenderModeStrict
)

// TemplateService handles Liquid template rendering with caching
type TemplateService struct {
	engine *liquid.Engine
	cache  sync.Map // map[string]*liquid.Template
	mu     sync.RWMutex
}

// TemplateValidationError represents a validation issue in a template
type TemplateValidationError struct {
	Variable string `json:"variable"`
	Message  string `json:"message"`
	Line     int    `json:"line,omitempty"`
}

// RenderResult contains the rendered output and any warnings
type RenderResult struct {
	Output   string                    `json:"output"`
	Warnings []TemplateValidationError `json:"warnings,omitempty"`
	Success  bool                      `json:"success"`
}

// NewTemplateService creates a new template service with custom filters
func NewTemplateService() *TemplateService {
	engine := liquid.NewEngine()

	ts := &TemplateService{
		engine: engine,
	}

	ts.registerCustomFilters()

	return ts
}

// registerCustomFilters adds domain-specific Liquid filters
func (ts *TemplateService) registerCustomFilters() {
	// ============================================
	// STRING FILTERS
	// ============================================

	// Default value filter: {{ first_name | default: "Friend" }}
	ts.engine.RegisterFilter("default", func(value interface{}, defaultVal string) interface{} {
		if value == nil {
			return defaultVal
		}
		strVal := fmt.Sprintf("%v", value)
		if strVal == "" || strVal == "<nil>" {
			return defaultVal
		}
		return value
	})

	// Capitalize first letter: {{ name | capitalize }}
	ts.engine.RegisterFilter("capitalize", func(s string) string {
		if len(s) == 0 {
			return s
		}
		return strings.ToUpper(string(s[0])) + strings.ToLower(s[1:])
	})

	// Title case: {{ name | titlecase }}
	ts.engine.RegisterFilter("titlecase", func(s string) string {
		return strings.Title(strings.ToLower(s))
	})

	// Truncate with ellipsis: {{ bio | truncate: 50 }}
	ts.engine.RegisterFilter("truncate", func(s string, length int) string {
		if len(s) <= length {
			return s
		}
		if length <= 3 {
			return s[:length]
		}
		return s[:length-3] + "..."
	})

	// URL encode: {{ email | urlencode }}
	ts.engine.RegisterFilter("urlencode", func(s string) string {
		return url.QueryEscape(s)
	})

	// HTML escape (safety): {{ user_input | escape }}
	ts.engine.RegisterFilter("escape", func(s string) string {
		return html.EscapeString(s)
	})

	// ============================================
	// NUMBER FILTERS
	// ============================================

	// Currency formatting: {{ price | currency }}
	ts.engine.RegisterFilter("currency", func(value interface{}) string {
		var f float64
		switch v := value.(type) {
		case float64:
			f = v
		case float32:
			f = float64(v)
		case int:
			f = float64(v)
		case int64:
			f = float64(v)
		case string:
			parsed, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return v
			}
			f = parsed
		default:
			return fmt.Sprintf("%v", value)
		}
		return fmt.Sprintf("$%.2f", f)
	})

	// Number with commas: {{ count | number_with_delimiter }}
	ts.engine.RegisterFilter("number_with_delimiter", func(value interface{}) string {
		var n int64
		switch v := value.(type) {
		case int:
			n = int64(v)
		case int64:
			n = v
		case float64:
			n = int64(v)
		default:
			return fmt.Sprintf("%v", value)
		}

		// Format with commas
		str := fmt.Sprintf("%d", n)
		if n < 0 {
			str = str[1:]
		}

		var result strings.Builder
		for i, c := range str {
			if i > 0 && (len(str)-i)%3 == 0 {
				result.WriteRune(',')
			}
			result.WriteRune(c)
		}

		if n < 0 {
			return "-" + result.String()
		}
		return result.String()
	})

	// Percentage: {{ rate | percentage }}
	ts.engine.RegisterFilter("percentage", func(value interface{}) string {
		var f float64
		switch v := value.(type) {
		case float64:
			f = v
		case float32:
			f = float64(v)
		case int:
			f = float64(v)
		default:
			return fmt.Sprintf("%v", value)
		}
		return fmt.Sprintf("%.1f%%", f)
	})

	// ============================================
	// DATE/TIME FILTERS
	// ============================================

	// Format date: {{ signup_date | date: "%B %d, %Y" }}
	// Note: osteele/liquid has built-in date filter, but we enhance it
	ts.engine.RegisterFilter("relative_time", func(t interface{}) string {
		var timestamp time.Time
		switch v := t.(type) {
		case time.Time:
			timestamp = v
		case *time.Time:
			if v == nil {
				return ""
			}
			timestamp = *v
		case string:
			parsed, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return v
			}
			timestamp = parsed
		default:
			return fmt.Sprintf("%v", t)
		}

		duration := time.Since(timestamp)
		switch {
		case duration < time.Minute:
			return "just now"
		case duration < time.Hour:
			mins := int(duration.Minutes())
			if mins == 1 {
				return "1 minute ago"
			}
			return fmt.Sprintf("%d minutes ago", mins)
		case duration < 24*time.Hour:
			hrs := int(duration.Hours())
			if hrs == 1 {
				return "1 hour ago"
			}
			return fmt.Sprintf("%d hours ago", hrs)
		case duration < 7*24*time.Hour:
			days := int(duration.Hours() / 24)
			if days == 1 {
				return "yesterday"
			}
			return fmt.Sprintf("%d days ago", days)
		case duration < 30*24*time.Hour:
			weeks := int(duration.Hours() / (24 * 7))
			if weeks == 1 {
				return "1 week ago"
			}
			return fmt.Sprintf("%d weeks ago", weeks)
		default:
			months := int(duration.Hours() / (24 * 30))
			if months == 1 {
				return "1 month ago"
			}
			return fmt.Sprintf("%d months ago", months)
		}
	})

	// Format as "time until": {{ expiry_date | time_until }}
	ts.engine.RegisterFilter("time_until", func(t interface{}) string {
		var timestamp time.Time
		switch v := t.(type) {
		case time.Time:
			timestamp = v
		case *time.Time:
			if v == nil {
				return ""
			}
			timestamp = *v
		case string:
			parsed, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return v
			}
			timestamp = parsed
		default:
			return fmt.Sprintf("%v", t)
		}

		duration := time.Until(timestamp)
		if duration < 0 {
			return "expired"
		}

		switch {
		case duration < time.Hour:
			mins := int(duration.Minutes())
			if mins <= 1 {
				return "less than a minute"
			}
			return fmt.Sprintf("%d minutes", mins)
		case duration < 24*time.Hour:
			hrs := int(duration.Hours())
			if hrs == 1 {
				return "1 hour"
			}
			return fmt.Sprintf("%d hours", hrs)
		default:
			days := int(duration.Hours() / 24)
			if days == 1 {
				return "1 day"
			}
			return fmt.Sprintf("%d days", days)
		}
	})

	// ============================================
	// EMAIL-SPECIFIC FILTERS
	// ============================================

	// Gravatar URL: {{ email | gravatar }}
	ts.engine.RegisterFilter("gravatar", func(email string) string {
		email = strings.ToLower(strings.TrimSpace(email))
		hash := md5.Sum([]byte(email))
		return fmt.Sprintf("https://www.gravatar.com/avatar/%x?d=identicon&s=80", hash)
	})

	// Extract domain from email: {{ email | email_domain }}
	ts.engine.RegisterFilter("email_domain", func(email string) string {
		parts := strings.Split(email, "@")
		if len(parts) == 2 {
			return parts[1]
		}
		return ""
	})

	// Mask email for privacy: {{ email | mask_email }}
	ts.engine.RegisterFilter("mask_email", func(email string) string {
		parts := strings.Split(email, "@")
		if len(parts) != 2 {
			return email
		}
		local := parts[0]
		domain := parts[1]

		if len(local) <= 2 {
			return local + "***@" + domain
		}
		return local[:2] + "***@" + domain
	})

	// ============================================
	// CONDITIONAL HELPERS
	// ============================================

	// Check if value is present (not nil/empty): {{ name | present }}
	ts.engine.RegisterFilter("present", func(value interface{}) bool {
		if value == nil {
			return false
		}
		strVal := fmt.Sprintf("%v", value)
		return strVal != "" && strVal != "<nil>" && strVal != "0" && strVal != "false"
	})

	// Check if value is blank: {{ name | blank }}
	ts.engine.RegisterFilter("blank", func(value interface{}) bool {
		if value == nil {
			return true
		}
		strVal := fmt.Sprintf("%v", value)
		return strVal == "" || strVal == "<nil>"
	})
}

// Parse compiles a template string and returns any syntax errors
func (ts *TemplateService) Parse(templateStr string) error {
	_, err := ts.engine.ParseString(templateStr)
	return err
}

// Render processes a template with the given context
// Uses caching for repeated renders of the same template
func (ts *TemplateService) Render(cacheKey string, templateStr string, ctx map[string]interface{}) (string, error) {
	// Try cache first
	if cacheKey != "" {
		if cached, ok := ts.cache.Load(cacheKey); ok {
			tpl := cached.(*liquid.Template)
			return tpl.RenderString(ctx)
		}
	}

	// Parse template
	tpl, err := ts.engine.ParseString(templateStr)
	if err != nil {
		log.Printf("TemplateService: Parse error: %v", err)
		return templateStr, err // Return original on parse error
	}

	// Cache if key provided
	if cacheKey != "" {
		ts.cache.Store(cacheKey, tpl)
	}

	// Render
	result, err := tpl.RenderString(ctx)
	if err != nil {
		log.Printf("TemplateService: Render error: %v", err)
		return templateStr, err
	}

	return result, nil
}

// RenderWithMode processes a template with configurable error handling
func (ts *TemplateService) RenderWithMode(templateStr string, ctx map[string]interface{}, mode RenderMode) (*RenderResult, error) {
	result := &RenderResult{
		Success:  true,
		Warnings: []TemplateValidationError{},
	}

	// Validate variables in strict mode
	if mode == RenderModeStrict {
		warnings := ts.ValidateVariables(templateStr, ctx)
		result.Warnings = warnings
		if len(warnings) > 0 {
			result.Success = false
			// Still try to render
		}
	}

	// Render
	output, err := ts.engine.ParseAndRenderString(templateStr, ctx)
	if err != nil {
		if mode == RenderModeStrict {
			return result, err
		}
		// Lax mode: return original template on error
		result.Output = templateStr
		result.Success = false
		log.Printf("TemplateService: Lax mode render warning: %v", err)
		return result, nil
	}

	result.Output = output
	return result, nil
}

// ValidateVariables checks for undefined variables in a template
func (ts *TemplateService) ValidateVariables(templateStr string, ctx map[string]interface{}) []TemplateValidationError {
	var errors []TemplateValidationError

	// Regex to find {{ variable }} patterns
	// Matches: {{ var }}, {{ var | filter }}, {{ var.nested }}
	varPattern := regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_.]*?)(?:\s*\||\s*\}\})`)

	matches := varPattern.FindAllStringSubmatch(templateStr, -1)
	seen := make(map[string]bool)

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		varName := strings.TrimSpace(match[1])

		// Skip duplicates
		if seen[varName] {
			continue
		}
		seen[varName] = true

		// Skip Liquid keywords
		if isLiquidKeyword(varName) {
			continue
		}

		// Check if variable exists in context
		if !ts.variableExists(varName, ctx) {
			errors = append(errors, TemplateValidationError{
				Variable: varName,
				Message:  fmt.Sprintf("Variable '%s' may not be defined for all subscribers", varName),
			})
		}
	}

	return errors
}

// variableExists checks if a variable path exists in the context
func (ts *TemplateService) variableExists(varPath string, ctx map[string]interface{}) bool {
	parts := strings.Split(varPath, ".")

	var current interface{} = ctx
	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			val, ok := v[part]
			if !ok {
				return false
			}
			current = val
		default:
			return false
		}
	}

	return true
}

// ClearCache removes all cached templates
func (ts *TemplateService) ClearCache() {
	ts.cache = sync.Map{}
}

// ClearCacheKey removes a specific cached template
func (ts *TemplateService) ClearCacheKey(key string) {
	ts.cache.Delete(key)
}

// isLiquidKeyword checks if a name is a Liquid control keyword
func isLiquidKeyword(name string) bool {
	keywords := map[string]bool{
		"if": true, "elsif": true, "else": true, "endif": true,
		"unless": true, "endunless": true,
		"case": true, "when": true, "endcase": true,
		"for": true, "endfor": true, "break": true, "continue": true,
		"capture": true, "endcapture": true,
		"comment": true, "endcomment": true,
		"raw": true, "endraw": true,
		"assign": true, "increment": true, "decrement": true,
		"include": true, "render": true,
		"forloop": true, "tablerowloop": true,
		"limit": true, "offset": true, "reversed": true,
		"cols": true, "item": true, "empty": true,
		"true": true, "false": true, "nil": true, "null": true,
		"blank": true, "present": true,
		"and": true, "or": true, "not": true,
		"contains": true, "in": true,
	}
	return keywords[strings.ToLower(name)]
}

// GetAvailableFilters returns a list of all custom filters
func (ts *TemplateService) GetAvailableFilters() []FilterInfo {
	return []FilterInfo{
		// String filters
		{Name: "default", Description: "Provide fallback value", Example: `{{ first_name | default: "Friend" }}`, Category: "string"},
		{Name: "capitalize", Description: "Capitalize first letter", Example: `{{ name | capitalize }}`, Category: "string"},
		{Name: "titlecase", Description: "Title case all words", Example: `{{ name | titlecase }}`, Category: "string"},
		{Name: "truncate", Description: "Truncate with ellipsis", Example: `{{ bio | truncate: 50 }}`, Category: "string"},
		{Name: "urlencode", Description: "URL encode a string", Example: `{{ email | urlencode }}`, Category: "string"},
		{Name: "escape", Description: "HTML escape for safety", Example: `{{ user_input | escape }}`, Category: "string"},

		// Number filters
		{Name: "currency", Description: "Format as currency", Example: `{{ price | currency }}`, Category: "number"},
		{Name: "number_with_delimiter", Description: "Add thousand separators", Example: `{{ count | number_with_delimiter }}`, Category: "number"},
		{Name: "percentage", Description: "Format as percentage", Example: `{{ rate | percentage }}`, Category: "number"},

		// Date filters
		{Name: "date", Description: "Format date (Liquid built-in)", Example: `{{ signup_date | date: "%B %d, %Y" }}`, Category: "date"},
		{Name: "relative_time", Description: "Show as 'X days ago'", Example: `{{ last_login | relative_time }}`, Category: "date"},
		{Name: "time_until", Description: "Show time remaining", Example: `{{ expiry | time_until }}`, Category: "date"},

		// Email filters
		{Name: "gravatar", Description: "Generate Gravatar URL", Example: `{{ email | gravatar }}`, Category: "email"},
		{Name: "email_domain", Description: "Extract email domain", Example: `{{ email | email_domain }}`, Category: "email"},
		{Name: "mask_email", Description: "Mask email for privacy", Example: `{{ email | mask_email }}`, Category: "email"},

		// Boolean filters
		{Name: "present", Description: "Check if value exists", Example: `{% if name | present %}...{% endif %}`, Category: "boolean"},
		{Name: "blank", Description: "Check if value is empty", Example: `{% if name | blank %}...{% endif %}`, Category: "boolean"},
	}
}

// FilterInfo describes a template filter
type FilterInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
	Category    string `json:"category"`
}
