package datanorm

import (
	"strings"
)

// Classifier determines file classification from filename and header row.
type Classifier struct{}

func NewClassifier() *Classifier {
	return &Classifier{}
}

var suppressionKeywords = []string{"suppress", "unsub", "bounce", "complaint", "block", "blacklist"}
var warmupKeywords = []string{"warmup", "seed", "engaged", "active", "premium"}
var suppressionHeaders = []string{"suppress_reason", "bounce_type", "complaint_type", "unsub_reason"}

// Classify determines the file classification based on filename and CSV header row.
func (c *Classifier) Classify(key string, headerRow []string) Classification {
	keyLower := strings.ToLower(key)

	for _, kw := range suppressionKeywords {
		if strings.Contains(keyLower, kw) {
			return ClassSuppression
		}
	}

	for _, kw := range warmupKeywords {
		if strings.Contains(keyLower, kw) {
			return ClassWarmup
		}
	}

	for _, h := range headerRow {
		hLower := strings.ToLower(strings.TrimSpace(h))
		for _, sh := range suppressionHeaders {
			if hLower == sh {
				return ClassSuppression
			}
		}
	}

	return ClassMailable
}
