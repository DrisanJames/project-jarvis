package snowflake

import (
	"time"
)

// Config holds Snowflake database configuration
type Config struct {
	Account   string `yaml:"account"`
	User      string `yaml:"user"`
	Password  string `yaml:"password"`
	Database  string `yaml:"database"`
	Schema    string `yaml:"schema"`
	Warehouse string `yaml:"warehouse"`
	Enabled   bool   `yaml:"enabled"`
}

// ParseConnectionString extracts components from the connection string
// Format: scheme=https;ACCOUNT=xxx;HOST=yyy;port=443;USER=zzz;PASSWORD=www;DB=aaa;
func ParseConnectionString(connStr string) Config {
	parts := make(map[string]string)
	
	var current string
	for _, c := range connStr {
		if c == ';' {
			if idx := indexOfChar(current, '='); idx > 0 {
				key := current[:idx]
				value := current[idx+1:]
				parts[key] = value
			}
			current = ""
		} else {
			current += string(c)
		}
	}
	// Handle last part without trailing semicolon
	if current != "" {
		if idx := indexOfChar(current, '='); idx > 0 {
			key := current[:idx]
			value := current[idx+1:]
			parts[key] = value
		}
	}
	
	// Parse database.schema from DB field if present
	db := parts["DB"]
	var database, schema string
	if idx := indexOfChar(db, '.'); idx > 0 {
		database = db[:idx]
		schema = db[idx+1:]
	} else {
		database = db
	}
	
	return Config{
		Account:  parts["ACCOUNT"],
		User:     parts["USER"],
		Password: parts["PASSWORD"],
		Database: database,
		Schema:   schema,
	}
}

func indexOfChar(s string, c rune) int {
	for i, r := range s {
		if r == c {
			return i
		}
	}
	return -1
}

// ValidationStatus represents a validation status with count
type ValidationStatus struct {
	StatusID string `json:"status_id"`
	Count    int64  `json:"count"`
}

// DailyValidationMetrics holds validation metrics for a single day
type DailyValidationMetrics struct {
	Date           string             `json:"date"`
	TotalRecords   int64              `json:"total_records"`
	StatusBreakdown []ValidationStatus `json:"status_breakdown"`
}

// DomainGroupMetrics holds metrics grouped by email domain
type DomainGroupMetrics struct {
	DomainGroup      string `json:"domain_group"`
	DomainGroupShort string `json:"domain_group_short"`
	Count            int64  `json:"count"`
}

// ValidationSummary provides an overview of validation data
type ValidationSummary struct {
	Timestamp           time.Time                `json:"timestamp"`
	TotalRecords        int64                    `json:"total_records"`
	TodayRecords        int64                    `json:"today_records"`
	UniqueStatuses      int                      `json:"unique_statuses"`
	DailyMetrics        []DailyValidationMetrics `json:"daily_metrics"`
	StatusBreakdown     []ValidationStatus       `json:"status_breakdown"`
	DomainGroupBreakdown []DomainGroupMetrics    `json:"domain_group_breakdown"`
}

// SubscriberValidation represents a row from SUBSCRIBER_VALIDATIONS_EO
type SubscriberValidation struct {
	ID                  int64  `json:"id"`
	Email               string `json:"email"`
	ValidationStatusID  string `json:"validation_status_id"`
	CreationDate        string `json:"creation_date"`
	LastValidationDate  string `json:"last_validation_date"`
	EmailDomainGroupID  int64  `json:"email_domain_group_id"`
	EmailDomainGroup    string `json:"email_domain_group"`
	EmailDomainGroupShort string `json:"email_domain_group_short"`
	Filename            string `json:"filename"`
}
