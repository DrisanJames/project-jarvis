package smtputil

import "strings"

// BounceType classifies an SMTP failure as hard (permanent) or soft (transient).
type BounceType string

const (
	BounceHard BounceType = "hard"
	BounceSoft BounceType = "soft"
)

// hardCodes are SMTP reply codes that indicate permanent failures.
var hardCodes = []string{"550", "551", "552", "553", "554", "555"}

// ClassifyError examines an SMTP error string and returns the bounce type.
// 5xx responses are hard bounces; everything else (4xx, connection errors,
// timeouts) is treated as a soft bounce. A nil error returns BounceSoft.
func ClassifyError(err error) BounceType {
	if err == nil {
		return BounceSoft
	}
	msg := err.Error()
	for _, code := range hardCodes {
		if strings.Contains(msg, code) {
			return BounceHard
		}
	}
	return BounceSoft
}
