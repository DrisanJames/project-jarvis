package datanorm

import (
	"strings"
	"unicode"
)

// NormalizedRecord is the fully normalized output of one CSV row,
// ready for insertion into mailing_subscribers or mailing_global_suppressions.
type NormalizedRecord struct {
	Email              string
	FirstName          string
	LastName           string
	City               string
	State              string
	Country            string
	Zip                string
	Phone              string
	VerificationStatus string  // verified, risky, invalid, unknown
	DomainGroup        string  // google, microsoft, yahoo, apple, other
	QualityScore       float64 // 0.0–1.0 composite score
	IsRole             bool
	IsDisposable       bool
	IsBot              bool
	EngagementBehavior string  // raw normalized behavior label
	SourceSignal       string  // campaign/tag from source system
	ExternalID         string  // external subscriber ID from source

	// Suppression-specific
	BounceCategory string
	DSNStatus      string
	DSNDiag        string
	SourceIP       string
	VMTA           string

	// Anything that doesn't map to a canonical field
	Extra map[string]string

	SourceFile string
}

// NormalizeRecord takes a CSV row, column mapping, and produces a NormalizedRecord.
func NormalizeRecord(row []string, mapping *ColumnMapping, sourceFile string) *NormalizedRecord {
	rec := &NormalizedRecord{
		Extra:      make(map[string]string),
		SourceFile: sourceFile,
	}

	for i, val := range row {
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}

		field, mapped := mapping.FieldMap[i]
		if !mapped {
			rawHeader := ""
			if i < len(mapping.RawNames) {
				rawHeader = strings.TrimSpace(mapping.RawNames[i])
			}
			if rawHeader != "" && !ShouldSkipColumn(rawHeader) {
				rec.Extra[rawHeader] = val
			}
			continue
		}

		switch field {
		case FieldEmail:
			rec.Email = normalizeEmail(val)
		case FieldFirstName:
			rec.FirstName = normalizeName(val)
		case FieldLastName:
			rec.LastName = normalizeName(val)
		case FieldCity:
			rec.City = normalizeName(val)
		case FieldState:
			rec.State = strings.ToUpper(strings.TrimSpace(val))
		case FieldCountry:
			rec.Country = normalizeCountry(val)
		case FieldZip:
			rec.Zip = normalizeZip(val)
		case FieldPhone:
			if rec.Phone == "" {
				rec.Phone = normalizePhone(val)
			}
		case FieldValidationStatus:
			rec.VerificationStatus = normalizeValidationStatus(val)
		case FieldDomainGroup:
			rec.DomainGroup = normalizeDomainGroup(val)
		case FieldEngagementBehavior:
			rec.EngagementBehavior = strings.ToLower(val)
		case FieldOngageStatus:
			rec.EngagementBehavior = strings.ToLower(val)
		case FieldRiskLevel:
			// Stored as raw for quality score calculation
			rec.Extra["_risk"] = strings.ToLower(val)
		case FieldIsRole:
			rec.IsRole = parseBool(val)
		case FieldIsDisposable:
			rec.IsDisposable = parseBool(val)
		case FieldIsBot:
			rec.IsBot = parseBool(val)
		case FieldSourceSignal:
			rec.SourceSignal = val
		case FieldExternalID:
			rec.ExternalID = val
		case FieldBounceCategory:
			rec.BounceCategory = strings.ToLower(val)
		case FieldDSNStatus:
			rec.DSNStatus = val
		case FieldDSNDiag:
			rec.DSNDiag = val
		case FieldSourceIP:
			rec.SourceIP = val
		case FieldVMTA:
			rec.VMTA = val
		case FieldUnsubscribed:
			if parseBool(val) {
				rec.Extra["_unsubscribed"] = "true"
			}
		case FieldBounced:
			if parseBool(val) {
				rec.Extra["_bounced"] = "true"
			}
		}
	}

	rec.QualityScore = computeQualityScore(rec)
	return rec
}

func normalizeEmail(raw string) string {
	email := strings.ToLower(strings.TrimSpace(raw))
	email = strings.Trim(email, "\"'<>")
	return email
}

func normalizeName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	return titleCase(s)
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		runes := []rune(strings.ToLower(w))
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
		}
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

func normalizeCountry(raw string) string {
	v := strings.TrimSpace(raw)
	upper := strings.ToUpper(v)
	if len(upper) == 2 {
		return upper
	}
	switch strings.ToLower(v) {
	case "united states", "usa", "us", "united states of america":
		return "US"
	case "united kingdom", "uk", "gb", "great britain":
		return "GB"
	case "canada", "ca":
		return "CA"
	default:
		return upper
	}
}

func normalizeZip(raw string) string {
	z := strings.TrimSpace(raw)
	// Strip .0 suffix from float-parsed zip codes (e.g., "38824.0" -> "38824")
	if idx := strings.Index(z, "."); idx > 0 {
		z = z[:idx]
	}
	return z
}

func normalizePhone(raw string) string {
	// Keep only digits and leading +
	var b strings.Builder
	for i, r := range raw {
		if r == '+' && i == 0 {
			b.WriteRune(r)
		} else if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeValidationStatus maps vendor-specific statuses to a canonical set.
func normalizeValidationStatus(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "1", "verified", "deliverable", "valid":
		return "verified"
	case "2", "unverifiable", "risky", "unknown", "catch-all", "catch_all":
		return "risky"
	case "3", "invalid", "undeliverable", "do_not_send":
		return "invalid"
	default:
		return "unknown"
	}
}

// normalizeDomainGroup maps vendor domain group labels to lowercase canonical names.
func normalizeDomainGroup(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(v, "google") || v == "5" || v == "gmail":
		return "google"
	case strings.Contains(v, "microsoft") || v == "4" || v == "outlook" || v == "hotmail":
		return "microsoft"
	case strings.Contains(v, "yahoo") || v == "3" || v == "aol":
		return "yahoo"
	case strings.Contains(v, "apple") || v == "icloud":
		return "apple"
	case strings.Contains(v, "att") || strings.Contains(v, "at&t"):
		return "att"
	case strings.Contains(v, "comcast") || strings.Contains(v, "xfinity"):
		return "comcast"
	default:
		if v == "" || v == "0" {
			return ""
		}
		return v
	}
}

func parseBool(raw string) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "true", "1", "yes", "y", "t":
		return true
	default:
		return false
	}
}

// computeQualityScore derives a 0.0–1.0 quality score from all available signals.
func computeQualityScore(rec *NormalizedRecord) float64 {
	score := 0.50 // base: unknown quality

	// Verification status is the strongest signal
	switch rec.VerificationStatus {
	case "verified":
		score = 0.80
	case "risky":
		score = 0.40
	case "invalid":
		score = 0.05
	}

	// Engagement behavior adjusts score
	switch rec.EngagementBehavior {
	case "highly_engaged":
		score = clamp(score+0.15, 0, 1)
	case "engager":
		score = clamp(score+0.10, 0, 1)
	case "active":
		score = clamp(score+0.05, 0, 1)
	case "complainer":
		score = clamp(score-0.30, 0, 1)
	case "disengaged":
		score = clamp(score-0.15, 0, 1)
	}

	// Risk level from validation provider
	if risk, ok := rec.Extra["_risk"]; ok {
		switch risk {
		case "low":
			score = clamp(score+0.05, 0, 1)
		case "medium":
			score = clamp(score-0.05, 0, 1)
		case "high":
			score = clamp(score-0.20, 0, 1)
		}
		delete(rec.Extra, "_risk")
	}

	// Penalty for disposable / role / bot
	if rec.IsDisposable {
		score = clamp(score-0.30, 0, 1)
	}
	if rec.IsRole {
		score = clamp(score-0.15, 0, 1)
	}
	if rec.IsBot {
		score = clamp(score-0.40, 0, 1)
	}

	// Previously bounced or unsubscribed records in source data
	if rec.Extra["_bounced"] == "true" {
		score = clamp(score-0.25, 0, 1)
		delete(rec.Extra, "_bounced")
	}
	if rec.Extra["_unsubscribed"] == "true" {
		score = clamp(score-0.20, 0, 1)
		delete(rec.Extra, "_unsubscribed")
	}

	return score
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// InferDomainGroupFromEmail extracts ISP group from email domain when no explicit column exists.
func InferDomainGroupFromEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at >= len(email)-1 {
		return ""
	}
	domain := strings.ToLower(email[at+1:])

	switch {
	case domain == "gmail.com" || domain == "googlemail.com":
		return "google"
	case domain == "yahoo.com" || domain == "yahoo.co.uk" || domain == "aol.com" ||
		domain == "ymail.com" || domain == "rocketmail.com":
		return "yahoo"
	case domain == "outlook.com" || domain == "hotmail.com" || domain == "live.com" ||
		domain == "msn.com" || domain == "hotmail.co.uk":
		return "microsoft"
	case domain == "icloud.com" || domain == "me.com" || domain == "mac.com":
		return "apple"
	case domain == "att.net" || domain == "sbcglobal.net" || domain == "bellsouth.net":
		return "att"
	case domain == "comcast.net" || domain == "xfinity.com":
		return "comcast"
	case domain == "verizon.net":
		return "verizon"
	case domain == "charter.net" || domain == "spectrum.net":
		return "charter"
	default:
		return ""
	}
}
