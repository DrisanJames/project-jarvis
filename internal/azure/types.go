package azure

import (
	"encoding/json"
	"time"
)

// TableEntity represents a row in Azure Table Storage
type TableEntity struct {
	PartitionKey string    `json:"PartitionKey"` // Data Set Code (e.g., "GLB_BR")
	RowKey       string    `json:"RowKey"`       // Unique identifier
	Timestamp    time.Time `json:"Timestamp"`    // When record was created
	ContactData  string    `json:"ContactData"`  // JSON string with contact details
}

// ContactData represents the parsed contact data JSON
type ContactData struct {
	Email       string            `json:"email"`
	FirstName   string            `json:"first_name"`
	LastName    string            `json:"last_name"`
	CustomField ContactCustomData `json:"custom_field"`
}

// ContactCustomData contains partner-specific fields
type ContactCustomData struct {
	DataPartner   string `json:"dataPartner"`
	DataSet       string `json:"dataSet"`
	SourceURL     string `json:"sourceUrl"`
	IPAddress     string `json:"ipAddress"`
	PostalAddress string `json:"postalAddress"`
	City          string `json:"city"`
	State         string `json:"state"`
	ZipCode       string `json:"zipCode"`
	OptInDate     string `json:"opt_in_date"`
}

// ParseContactData parses the ContactData JSON string
func (e *TableEntity) ParseContactData() (*ContactData, error) {
	var data ContactData
	if err := json.Unmarshal([]byte(e.ContactData), &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// DataSetMetrics holds aggregated metrics for a data set
type DataSetMetrics struct {
	DataSetCode   string    `json:"data_set_code"`   // PartitionKey value
	DataPartner   string    `json:"data_partner"`    // From ContactData
	DataSetName   string    `json:"data_set_name"`   // From ContactData.dataSet
	RecordCount   int64     `json:"record_count"`    // Total records
	TodayCount    int64     `json:"today_count"`     // Records added today
	LastTimestamp time.Time `json:"last_timestamp"`  // Most recent record
	HasGap        bool      `json:"has_gap"`         // True if partner gap detected (>24h)
	GapHours      float64   `json:"gap_hours"`       // Hours since last record
}

// SystemHealth represents the overall processor health
type SystemHealth struct {
	Status              string    `json:"status"`               // healthy, critical
	LastHydrationTime   time.Time `json:"last_hydration_time"`  // Most recent record across ALL data sets
	HoursSinceHydration float64   `json:"hours_since_hydration"`
	ProcessorRunning    bool      `json:"processor_running"`    // True if any data set hydrated in last hour
}

// PartnerHealth represents an individual partner's data feed status
type PartnerHealth struct {
	DataPartner   string    `json:"data_partner"`
	DataSetCode   string    `json:"data_set_code"`
	LastTimestamp time.Time `json:"last_timestamp"`
	GapHours      float64   `json:"gap_hours"`
	Status        string    `json:"status"`          // healthy, warning (gap detected)
}

// HistoricalMetrics holds metrics for a date range
type HistoricalMetrics struct {
	DateRange     string            `json:"date_range"`      // "today", "7d", "30d", "365d"
	StartDate     time.Time         `json:"start_date"`
	EndDate       time.Time         `json:"end_date"`
	TotalRecords  int64             `json:"total_records"`
	DailyAverage  float64           `json:"daily_average"`
	DailyCounts   []DailyDataSetCount `json:"daily_counts"`
}

// DataInjectionSummary provides an overview of all data injections
type DataInjectionSummary struct {
	Timestamp        time.Time        `json:"timestamp"`
	TotalRecords     int64            `json:"total_records"`
	TodayRecords     int64            `json:"today_records"`
	DataSetsActive   int              `json:"data_sets_active"`
	DataSetsWithGaps int              `json:"data_sets_with_gaps"`
	DataSetMetrics   []DataSetMetrics `json:"data_set_metrics"`
}

// DailyDataSetCount represents counts for a specific day and data set
type DailyDataSetCount struct {
	Date        string `json:"date"`
	DataSetCode string `json:"data_set_code"`
	Count       int64  `json:"count"`
}

// TableQueryResponse represents the response from Azure Table Storage query
type TableQueryResponse struct {
	Value             []map[string]interface{} `json:"value"`
	ODataNextLink     string                   `json:"odata.nextLink,omitempty"`
	ContinuationToken string                   `json:"-"`
}

// Config holds Azure Table Storage configuration
type Config struct {
	ConnectionString string `yaml:"connection_string"`
	TableName        string `yaml:"table_name"`
	Enabled          bool   `yaml:"enabled"`
	GapThresholdHours int   `yaml:"gap_threshold_hours"` // Hours before flagging a gap
}

// ParseConnectionString extracts components from the connection string
func (c Config) ParseConnectionString() (accountName, accountKey, endpointSuffix string) {
	// Parse: DefaultEndpointsProtocol=https;AccountName=xxx;AccountKey=yyy;EndpointSuffix=zzz
	parts := make(map[string]string)
	for _, part := range splitConnectionString(c.ConnectionString) {
		if idx := indexOf(part, '='); idx > 0 {
			key := part[:idx]
			value := part[idx+1:]
			parts[key] = value
		}
	}
	return parts["AccountName"], parts["AccountKey"], parts["EndpointSuffix"]
}

func splitConnectionString(s string) []string {
	var result []string
	var current string
	for _, c := range s {
		if c == ';' {
			if current != "" {
				result = append(result, current)
			}
			current = ""
		} else {
			current += string(c)
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

func indexOf(s string, c rune) int {
	for i, r := range s {
		if r == c {
			return i
		}
	}
	return -1
}
