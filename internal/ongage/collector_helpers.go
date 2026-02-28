package ongage

import (
	"regexp"
	"strconv"
	"unicode"
)

// getStringValue safely extracts a string from a ReportRow
func getStringValue(row ReportRow, key string) string {
	if val, ok := row[key]; ok {
		switch v := val.(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case int64:
			return strconv.FormatInt(v, 10)
		case int:
			return strconv.Itoa(v)
		}
	}
	return ""
}

// getInt64Value safely extracts an int64 from a ReportRow
func getInt64Value(row ReportRow, key string) int64 {
	if val, ok := row[key]; ok {
		switch v := val.(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		case string:
			if i, err := strconv.ParseInt(v, 10, 64); err == nil {
				return i
			}
		}
	}
	return 0
}

// getFloat64Value safely extracts a float64 from a ReportRow
func getFloat64Value(row ReportRow, key string) float64 {
	if val, ok := row[key]; ok {
		switch v := val.(type) {
		case float64:
			return v
		case int64:
			return float64(v)
		case int:
			return float64(v)
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

// containsEmoji checks if a string contains emoji characters
func containsEmoji(s string) bool {
	for _, r := range s {
		// Emoticons
		if r >= 0x1F600 && r <= 0x1F64F {
			return true
		}
		// Misc Symbols and Pictographs
		if r >= 0x1F300 && r <= 0x1F5FF {
			return true
		}
		// Transport and Map
		if r >= 0x1F680 && r <= 0x1F6FF {
			return true
		}
		// Misc symbols
		if r >= 0x2600 && r <= 0x26FF {
			return true
		}
		// Dingbats
		if r >= 0x2700 && r <= 0x27BF {
			return true
		}
		// Flags
		if r >= 0x1F1E0 && r <= 0x1F1FF {
			return true
		}
		// Supplemental Symbols and Pictographs
		if r >= 0x1F900 && r <= 0x1F9FF {
			return true
		}
		// Symbols and Pictographs Extended-A
		if r >= 0x1FA00 && r <= 0x1FA6F {
			return true
		}
		// Enclosed Alphanumeric Supplement
		if r >= 0x1F100 && r <= 0x1F1FF {
			return true
		}
		// Regional Indicators
		if r >= 0x1F3FB && r <= 0x1F3FF {
			return true
		}
		// Variation Selectors
		if r == 0xFE0F {
			return true
		}
	}
	return false
}

// containsNumber checks if a string contains numeric characters
func containsNumber(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// containsUrgencyWords checks for common urgency/scarcity words
func containsUrgencyWords(s string) bool {
	urgencyPatterns := []string{
		`(?i)\burgent\b`,
		`(?i)\blast\s+chance\b`,
		`(?i)\blimited\s+time\b`,
		`(?i)\bdon'?t\s+miss\b`,
		`(?i)\bhurry\b`,
		`(?i)\bending\s+soon\b`,
		`(?i)\bact\s+now\b`,
		`(?i)\btoday\s+only\b`,
		`(?i)\bexpires?\b`,
		`(?i)\bfinal\b`,
		`(?i)\bnow\b`,
	}

	for _, pattern := range urgencyPatterns {
		if matched, _ := regexp.MatchString(pattern, s); matched {
			return true
		}
	}
	return false
}
