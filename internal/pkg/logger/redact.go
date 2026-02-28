package logger

import "strings"

// RedactEmail masks an email address for safe logging.
// "john.doe@example.com" → "jo***@example.com"
// Short local parts (≤2 chars) are fully masked: "ab@example.com" → "***@example.com"
func RedactEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return "***@***"
	}
	name := parts[0]
	if len(name) > 2 {
		return name[:2] + "***@" + parts[1]
	}
	return "***@" + parts[1]
}
