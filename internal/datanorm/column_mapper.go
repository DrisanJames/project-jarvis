package datanorm

import "strings"

// CanonicalField is a normalized field name used across all import sources.
type CanonicalField string

const (
	FieldEmail             CanonicalField = "email"
	FieldFirstName         CanonicalField = "first_name"
	FieldLastName          CanonicalField = "last_name"
	FieldCity              CanonicalField = "city"
	FieldState             CanonicalField = "state"
	FieldCountry           CanonicalField = "country"
	FieldZip               CanonicalField = "zip"
	FieldPhone             CanonicalField = "phone"
	FieldValidationStatus  CanonicalField = "validation_status"
	FieldDomainGroup       CanonicalField = "domain_group"
	FieldEngagementBehavior CanonicalField = "engagement_behavior"
	FieldRiskLevel         CanonicalField = "risk_level"
	FieldIsRole            CanonicalField = "is_role"
	FieldIsDisposable      CanonicalField = "is_disposable"
	FieldIsBot             CanonicalField = "is_bot"
	FieldSourceSignal      CanonicalField = "source_signal"
	FieldExternalID        CanonicalField = "external_id"
	FieldBounceCategory    CanonicalField = "bounce_category"
	FieldDSNStatus         CanonicalField = "dsn_status"
	FieldDSNDiag           CanonicalField = "dsn_diag"
	FieldSourceIP          CanonicalField = "source_ip"
	FieldVMTA              CanonicalField = "vmta"
	FieldOngageStatus      CanonicalField = "ongage_status"
	FieldUnsubscribed      CanonicalField = "unsubscribed"
	FieldBounced           CanonicalField = "bounced"
)

// columnAliases maps lowercase header names to canonical fields.
// When multiple raw headers mean the same thing, they all map here.
var columnAliases = map[string]CanonicalField{
	// Email
	"email":         FieldEmail,
	"email_address":  FieldEmail,
	"emailaddress":   FieldEmail,
	"e-mail":         FieldEmail,
	"mail":           FieldEmail,
	"plaintextemail": FieldEmail,
	"address":        FieldEmail, // mailgun validation exports
	"rcpt":           FieldEmail, // PMTA accounting logs

	// First name
	"first_name": FieldFirstName,
	"firstname":  FieldFirstName,
	"fname":      FieldFirstName,
	"first":      FieldFirstName,
	"first name": FieldFirstName,

	// Last name
	"last_name": FieldLastName,
	"lastname":  FieldLastName,
	"lname":     FieldLastName,
	"last":      FieldLastName,
	"last name": FieldLastName,

	// Location
	"city":        FieldCity,
	"state":       FieldState,
	"country":     FieldCountry,
	"country_code": FieldCountry,
	"zip":         FieldZip,
	"zipcode":     FieldZip,
	"zip_code":    FieldZip,
	"post code":   FieldZip,
	"postal_code": FieldZip,

	// Phone
	"phone":  FieldPhone,
	"mobile": FieldPhone,
	"fax":    FieldPhone,

	// Validation / verification
	"validationstatus":   FieldValidationStatus,
	"validationstatusid": FieldValidationStatus,
	"result":             FieldValidationStatus, // mailgun: deliverable/undeliverable/risky

	// Domain grouping (ISP classification)
	"emaildomaingroup":   FieldDomainGroup,
	"emaildomaingroupid": FieldDomainGroup,

	// Engagement
	"engagement_behavior": FieldEngagementBehavior,
	"ongage_status":       FieldOngageStatus,

	// Risk
	"risk": FieldRiskLevel,

	// Boolean flags
	"is_role_address":       FieldIsRole,
	"is_disposable_address": FieldIsDisposable,
	"is_bot":                FieldIsBot,

	// Source signal / campaign tag
	"signal": FieldSourceSignal,

	// External subscriber ID
	"subscriberid": FieldExternalID,

	// PMTA bounce fields
	"bouncecat": FieldBounceCategory,
	"dsnstatus": FieldDSNStatus,
	"dsndiag":   FieldDSNDiag,
	"dlvsourceip": FieldSourceIP,
	"vmta":      FieldVMTA,

	// Status flags from legacy exports
	"unsubscribed": FieldUnsubscribed,
	"bounced":      FieldBounced,
}

// ColumnMapping holds the resolved mapping from CSV column indices to canonical fields.
type ColumnMapping struct {
	EmailIdx  int
	FieldMap  map[int]CanonicalField // column index -> canonical field
	RawNames  []string               // original header names
}

// MapColumns takes a raw CSV header row and returns a resolved mapping.
// Returns nil if no email column is found.
func MapColumns(header []string) *ColumnMapping {
	m := &ColumnMapping{
		EmailIdx: -1,
		FieldMap: make(map[int]CanonicalField, len(header)),
		RawNames: header,
	}

	for i, h := range header {
		normalized := strings.ToLower(strings.TrimSpace(h))
		// Remove surrounding quotes
		normalized = strings.Trim(normalized, "\"'")

		if field, ok := columnAliases[normalized]; ok {
			m.FieldMap[i] = field
			if field == FieldEmail {
				m.EmailIdx = i
			}
		}
	}

	// Fallback: scan for any header containing "email" if no exact match
	if m.EmailIdx < 0 {
		for i, h := range header {
			if strings.Contains(strings.ToLower(h), "email") {
				m.FieldMap[i] = FieldEmail
				m.EmailIdx = i
				break
			}
		}
	}

	if m.EmailIdx < 0 {
		return nil
	}

	return m
}

// skipColumns are columns that carry no useful information for normalization.
var skipColumns = map[string]bool{
	"eof":             true,
	"format":          true,
	"fax":             true,
	"timelogged":      true,
	"timequeued":      true,
	"orig":            true,
	"orcpt":           true,
	"dsnaction":       true,
	"dsnmta":          true,
	"srctype":         true,
	"srcmta":          true,
	"dlvtype":         true,
	"dlvdestinationip": true,
	"dlvesmtpavailable": true,
	"dlvsize":         true,
	"jobid":           true,
	"envid":           true,
	"queue":           true,
	"vmtapool":        true,
	"timefirstattempt": true,
	"dsnreportingmta": true,
	"did_you_mean":     true,
	"root_address":     true,
	"reason":           true, // mailgun validation array, redundant with result
}

// ShouldSkipColumn returns true if a column carries no useful normalized value.
func ShouldSkipColumn(headerName string) bool {
	return skipColumns[strings.ToLower(strings.TrimSpace(headerName))]
}

// LooksLikeEmail returns true if the value appears to be an email address.
// Used to detect headerless CSVs where the first row is data, not column names.
func LooksLikeEmail(val string) bool {
	v := strings.TrimSpace(val)
	if len(v) < 5 || len(v) > 254 {
		return false
	}
	at := strings.LastIndex(v, "@")
	if at < 1 || at >= len(v)-1 {
		return false
	}
	domain := v[at+1:]
	return strings.Contains(domain, ".") && len(domain) >= 3
}

// MapColumnsHeaderless builds a ColumnMapping for a CSV with no header row
// by scanning the first data row for a cell that looks like an email address.
// Returns nil if no email-shaped value is found.
func MapColumnsHeaderless(firstRow []string) *ColumnMapping {
	m := &ColumnMapping{
		EmailIdx: -1,
		FieldMap: make(map[int]CanonicalField),
	}
	for i, val := range firstRow {
		val = strings.TrimSpace(val)
		if m.EmailIdx < 0 && LooksLikeEmail(val) {
			m.EmailIdx = i
			m.FieldMap[i] = FieldEmail
		}
	}
	if m.EmailIdx < 0 {
		return nil
	}
	return m
}
