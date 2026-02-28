package api

import (
	"log"
	"net/http"
	"strings"
)

// =============================================================================
// ERROR SANITIZER
// Ensures internal errors (database details, file paths, stack traces) are
// NEVER leaked to API consumers. All 5xx errors return generic safe messages
// while the full error is logged server-side for debugging.
// =============================================================================

// sanitizedError logs the full internal error and returns a public-safe message.
// Use this whenever a 500-level error would otherwise include err.Error() in the response.
func sanitizedError(code int, internalErr error, publicMsg string) string {
	if internalErr != nil {
		log.Printf("ERROR [%d]: %s: %v", code, publicMsg, internalErr)
	}
	return publicMsg
}

// respondSafeError is a convenience wrapper that logs the internal error and
// sends a sanitized JSON error response to the client.
func respondSafeError(w http.ResponseWriter, code int, internalErr error, publicMsg string) {
	msg := sanitizedError(code, internalErr, publicMsg)
	respondJSON(w, code, map[string]string{"error": msg})
}

// respondSafeHTTPError is a convenience wrapper for handlers using http.Error
// instead of respondError/respondJSON.
func respondSafeHTTPError(w http.ResponseWriter, code int, internalErr error, publicMsg string) {
	if internalErr != nil {
		log.Printf("ERROR [%d]: %s: %v", code, publicMsg, internalErr)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write([]byte(`{"error":"` + publicMsg + `"}`))
}

// safeErrorMessage maps common internal error patterns to public-safe messages.
// For 400-level errors, the original message is typically fine (user input issues).
// For 500-level errors, this returns a generic safe message.
func safeErrorMessage(code int, internalErr error) string {
	if code < 500 {
		// 4xx errors are about user input — usually safe to expose
		if internalErr != nil {
			return internalErr.Error()
		}
		return "Bad request"
	}

	if internalErr == nil {
		return "An internal error occurred"
	}

	errStr := strings.ToLower(internalErr.Error())

	switch {
	case strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "no such host") ||
		strings.Contains(errStr, "dial tcp"):
		return "Service temporarily unavailable"

	case strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "context canceled"):
		return "Request timed out"

	case strings.Contains(errStr, "sql") ||
		strings.Contains(errStr, "pq:") ||
		strings.Contains(errStr, "query") ||
		strings.Contains(errStr, "scan") ||
		strings.Contains(errStr, "transaction") ||
		strings.Contains(errStr, "database"):
		return "A database error occurred"

	case strings.Contains(errStr, "json") ||
		strings.Contains(errStr, "unmarshal") ||
		strings.Contains(errStr, "marshal") ||
		strings.Contains(errStr, "decode") ||
		strings.Contains(errStr, "parse"):
		return "Invalid request format"

	case strings.Contains(errStr, "permission") ||
		strings.Contains(errStr, "access denied"):
		return "Access denied"

	default:
		return "An internal error occurred"
	}
}
