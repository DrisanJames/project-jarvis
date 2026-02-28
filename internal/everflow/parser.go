package everflow

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Sub1 format examples:
// FYF_556_1091_01262026_3219015617
// TDIH_407_3926_01262026_3219537162
// 400_3875_01262026_3219015617 (sometimes property code is missing)
// IGN_ (sometimes just prefix)

// ParseSub1 extracts property code, offer ID, date, and mailing ID from sub1
func ParseSub1(sub1 string) (*ParsedSub1, error) {
	if sub1 == "" {
		return nil, fmt.Errorf("empty sub1")
	}

	parsed := &ParsedSub1{Raw: sub1}

	// Split by underscore
	parts := strings.Split(sub1, "_")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid sub1 format, not enough parts: %s", sub1)
	}

	// Check if first part is a known property code
	firstPart := strings.ToUpper(parts[0])
	if _, isProperty := PropertyMapping[firstPart]; isProperty {
		parsed.PropertyCode = firstPart
		parsed.PropertyName = GetPropertyName(firstPart)
		parts = parts[1:] // Remove property code from parts
	} else if isNumeric(parts[0]) {
		// First part is numeric, no property code - might be offer ID
		// Format: 400_3875_01262026_3219015617
		parsed.PropertyCode = ""
		parsed.PropertyName = ""
	} else {
		// Unknown property code, still record it
		parsed.PropertyCode = firstPart
		parsed.PropertyName = firstPart // Use code as name
		parts = parts[1:]
	}

	// Now parse remaining parts: [offerID, unknown, date, mailingID]
	// Or: [unknown, date, mailingID]
	// Or: [offerID, date, mailingID]
	
	if len(parts) >= 1 && isNumeric(parts[0]) {
		parsed.OfferID = parts[0]
	}

	// Find the date part (8 digits in mmddyyyy format)
	dateRegex := regexp.MustCompile(`^\d{8}$`)
	for i, part := range parts {
		if dateRegex.MatchString(part) {
			parsed.Date = part
			// The next part after date should be the mailing ID
			if i+1 < len(parts) {
				parsed.MailingID = parts[i+1]
			}
			break
		}
	}

	// If we didn't find a date, try the last part as mailing ID
	if parsed.MailingID == "" && len(parts) > 0 {
		lastPart := parts[len(parts)-1]
		if isNumeric(lastPart) && len(lastPart) >= 5 {
			parsed.MailingID = lastPart
		}
	}

	return parsed, nil
}

// ParseCampaignName extracts property and offer ID from campaign name
// Format: 02052025_HRO_1944_FidelityLife_OPENERS
// [DATE]_[PROPERTY]_[OFFERID]_[OFFERNAME]_[SEGMENT]
func ParseCampaignName(name string) (date, property, offerID, offerName, segment string, err error) {
	if name == "" {
		return "", "", "", "", "", fmt.Errorf("empty campaign name")
	}

	parts := strings.Split(name, "_")
	if len(parts) < 3 {
		return "", "", "", "", "", fmt.Errorf("invalid campaign name format: %s", name)
	}

	// First part should be date (8 digits)
	dateRegex := regexp.MustCompile(`^\d{8}$`)
	if dateRegex.MatchString(parts[0]) {
		date = parts[0]
		parts = parts[1:]
	}

	// Second part should be property code
	if len(parts) >= 1 {
		property = strings.ToUpper(parts[0])
		parts = parts[1:]
	}

	// Third part should be offer ID (numeric)
	if len(parts) >= 1 && isNumeric(parts[0]) {
		offerID = parts[0]
		parts = parts[1:]
	}

	// Remaining parts are offer name and segment
	if len(parts) >= 1 {
		// Last part is usually segment
		if len(parts) >= 2 {
			offerName = strings.Join(parts[:len(parts)-1], "_")
			segment = parts[len(parts)-1]
		} else {
			offerName = parts[0]
		}
	}

	return date, property, offerID, offerName, segment, nil
}

// ParseTimestamp parses Everflow timestamp format
// Examples: "01/27/2026 00:06:13 PST", "2026-01-27 00:00:00"
func ParseTimestamp(ts string) (time.Time, error) {
	if ts == "" {
		return time.Time{}, nil
	}

	// Try various formats
	formats := []string{
		"01/02/2006 15:04:05 MST",
		"01/02/2006 15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
		time.RFC3339,
	}

	for _, format := range formats {
		if t, err := time.Parse(format, ts); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse timestamp: %s", ts)
}

// ParseDate parses date from sub1 format (mmddyyyy)
func ParseDate(dateStr string) (time.Time, error) {
	if len(dateStr) != 8 {
		return time.Time{}, fmt.Errorf("invalid date format: %s", dateStr)
	}

	// Format: mmddyyyy
	month := dateStr[0:2]
	day := dateStr[2:4]
	year := dateStr[4:8]

	formatted := fmt.Sprintf("%s-%s-%s", year, month, day)
	return time.Parse("2006-01-02", formatted)
}

// isNumeric checks if a string contains only digits
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ExtractMailingIDFromSub1 is a convenience function to get just the mailing ID
func ExtractMailingIDFromSub1(sub1 string) string {
	parsed, err := ParseSub1(sub1)
	if err != nil {
		return ""
	}
	return parsed.MailingID
}

// ExtractPropertyFromSub1 is a convenience function to get just the property code
func ExtractPropertyFromSub1(sub1 string) string {
	parsed, err := ParseSub1(sub1)
	if err != nil {
		return ""
	}
	return parsed.PropertyCode
}

// MatchMailingToCampaign attempts to match a mailing ID to campaign data
func MatchMailingToCampaign(mailingID string, campaigns []CampaignInfo) *CampaignInfo {
	for i, c := range campaigns {
		if c.ID == mailingID {
			return &campaigns[i]
		}
	}
	return nil
}

// CampaignInfo holds basic campaign info for matching
type CampaignInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	PropertyCode string `json:"property_code"`
	OfferID      string `json:"offer_id"`
	ScheduleDate string `json:"schedule_date"`
}

// BuildCampaignInfoFromName creates campaign info from name parsing
func BuildCampaignInfoFromName(id, name string) CampaignInfo {
	date, property, offerID, _, _, _ := ParseCampaignName(name)
	return CampaignInfo{
		ID:           id,
		Name:         name,
		PropertyCode: property,
		OfferID:      offerID,
		ScheduleDate: date,
	}
}

// NormalizeSub1 cleans and normalizes a sub1 value
func NormalizeSub1(sub1 string) string {
	// Trim whitespace
	sub1 = strings.TrimSpace(sub1)
	// Convert to uppercase for consistency
	return strings.ToUpper(sub1)
}

// ValidatePropertyCode checks if a property code is valid
func ValidatePropertyCode(code string) bool {
	code = strings.ToUpper(code)
	_, exists := PropertyMapping[code]
	return exists
}

// ========== Sub2 Parsing (Data Partner Attribution) ==========

// Sub2 format examples:
// M77_WIT_   → data set code "M77_WIT", partner prefix "M77" → "Media 77"
// GLB_BR_    → data set code "GLB_BR",  partner prefix "GLB" → "GlobeUSA"
// SCO_FIN_   → data set code "SCO_FIN", partner prefix "SCO" → "Suited Connector"
// a1b2c3d4e5 → Jarvis email hash (10-char hex), skip for data partner analysis

// emailHashRegex matches Jarvis-generated 10-char hex email hashes
var emailHashRegex = regexp.MustCompile(`^[a-f0-9]{10}$`)

// ParsedSub2 holds the result of parsing a sub2 value
type ParsedSub2 struct {
	Raw           string `json:"raw"`
	DataSetCode   string `json:"data_set_code"`   // e.g. "M77_WIT"
	PartnerPrefix string `json:"partner_prefix"`  // e.g. "M77"
	PartnerName   string `json:"partner_name"`    // e.g. "Media 77"
	IsEmailHash   bool   `json:"is_email_hash"`   // true if sub2 is a Jarvis email hash
}

// ParseSub2 extracts a data set code and data partner from sub2.
// Returns nil if sub2 is empty or an email hash.
func ParseSub2(sub2 string) *ParsedSub2 {
	sub2 = strings.TrimSpace(sub2)
	if sub2 == "" {
		return nil
	}

	parsed := &ParsedSub2{Raw: sub2}

	// Detect Jarvis email hashes (10-char lowercase hex)
	if emailHashRegex.MatchString(sub2) {
		parsed.IsEmailHash = true
		return parsed
	}

	// Filter out bogus/template sub2 values that are not real data set codes
	if strings.Contains(sub2, "{{") || strings.Contains(sub2, "}}") {
		return nil // Template variable not substituted
	}
	upper := strings.ToUpper(strings.TrimRight(sub2, "_"))
	if upper == "N/A" || upper == "NA" || upper == "NULL" || upper == "UNDEFINED" || upper == "TEST" || upper == "TESTDATASET" || upper == "WMRY" {
		return nil // Placeholder / test / internal-only values
	}

	// Strip trailing underscore(s) to get the clean data set code
	code := strings.TrimRight(sub2, "_")
	if code == "" {
		return nil
	}
	parsed.DataSetCode = code

	// Resolve partner using the FULL data-set code (checks overrides first, then prefix)
	groupKey, groupName := ResolvePartnerGroup(code)
	parsed.PartnerPrefix = groupKey
	parsed.PartnerName = groupName

	return parsed
}

// ExtractDataPartnerFromSub2 is a convenience function returning partner prefix and name.
func ExtractDataPartnerFromSub2(sub2 string) (prefix, name string) {
	parsed := ParseSub2(sub2)
	if parsed == nil || parsed.IsEmailHash {
		return "", ""
	}
	return parsed.PartnerPrefix, parsed.PartnerName
}
